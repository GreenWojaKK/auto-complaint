"""Single-process vLLM endpoint for Dllio's stateless text Responses requests.

The engine runs on the main thread and batches the requests that are in flight: a request joins the running batch at
the next engine step. HTTP threads only parse a request, hand it to the engine and wait for its reply.

RAON_MODEL_PATH names the checkpoint folder; without it the folder is Raon-1-mini-preview-MXFP6-E2M3 under the model
directory (DLLIO_MODELS, ~/models unless set). RAON_CONTEXT is the context length in tokens, RAON_SEQS the number of
requests generated together, RAON_KV_BYTES the KV cache size, RAON_BATCH_TOKENS the tokens one engine step takes in,
RAON_PORT the loopback port.

A step in which every request generates one token replays a device graph captured at start for 1, 2, 4, 8, ... rows up
to RAON_SEQS; the host then issues the whole model forward as one launch. Steps that take in prompt tokens run eagerly.
"""

import gc
import json
import os
import queue
import select
import socket
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import torch
from vllm import LLM, SamplingParams
from vllm.sampling_params import StructuredOutputsParams

MODEL = "raon-r9700"
CHECKPOINT = "Raon-1-mini-preview-MXFP6-E2M3"
THINK_END = "</think>"
BODY_LIMIT = 4 * 1024 * 1024
SAMPLING = ("temperature", "top_p", "top_k", "presence_penalty", "seed")
# Greedy reasoning tends not to end. With thinking on, the model card's sampling is the default.
THINKING_SAMPLING = {"temperature": 1.0, "top_p": 0.95, "top_k": 20, "presence_penalty": 1.5}
SCHEMA_NOTE = "아래 JSON schema에 맞는 JSON 객체 하나만 출력한다. 설명, 머리말, 코드 블록 표시를 붙이지 않는다."


def checkpoint():
    path = os.getenv("RAON_MODEL_PATH")
    if not path:
        path = Path(os.getenv("DLLIO_MODELS") or Path.home() / "models") / CHECKPOINT
    path = Path(path).expanduser()
    if not (path / "model.safetensors.index.json").is_file():
        raise FileNotFoundError(f"Missing Raon checkpoint index in {path}")
    return str(path.resolve(strict=True))


def text_content(content):
    if isinstance(content, str):
        return content
    if isinstance(content, list) and all(
        isinstance(part, dict)
        and part.get("type") in {"input_text", "output_text", "text"}
        and isinstance(part.get("text"), str)
        for part in content
    ):
        return "\n".join(part["text"] for part in content)
    raise ValueError("Message content must contain text")


class Job:
    """One request: the chat messages and generation settings read from its body, then its reply or error."""

    def __init__(self, request):
        if not isinstance(request, dict) or request.get("model") != MODEL:
            raise ValueError(f"model must be {MODEL}")
        if request.get("stream") or request.get("store") or request.get("tools"):
            raise ValueError("Only stateless, non-streaming text requests are supported")
        reasoning = request.get("reasoning") or {}
        if not isinstance(reasoning, dict):
            raise ValueError("reasoning must be an object")
        self.thinking = reasoning.get("effort") not in (None, "", "none", "minimal")

        self.messages = []
        instructions = request.get("instructions")
        if instructions:
            self.messages.append({"role": "system", "content": text_content(instructions)})
        inputs = request.get("input")
        if isinstance(inputs, str):
            inputs = [{"role": "user", "content": inputs}]
        if not isinstance(inputs, list) or not inputs:
            raise ValueError("input must contain at least one text message")
        for item in inputs:
            if not isinstance(item, dict) or item.get("role") not in {"user", "assistant", "system"}:
                raise ValueError("Text messages require user, assistant, or system roles")
            self.messages.append({"role": item["role"], "content": text_content(item.get("content"))})

        self.max_tokens = request.get("max_output_tokens", 256)
        if type(self.max_tokens) is not int or self.max_tokens < 1:
            raise ValueError("max_output_tokens must be a positive integer")
        text = request.get("text") or {}
        output_format = (text.get("format") if isinstance(text, dict) else None) or {"type": "text"}
        if not isinstance(output_format, dict) or output_format.get("type") not in {"text", "json_schema"}:
            raise ValueError("text.format must describe text or a JSON schema")
        self.schema = None
        if output_format["type"] == "json_schema":
            self.schema = output_format.get("schema")
            if not isinstance(self.schema, dict):
                raise ValueError("text.format.schema must be a JSON object")
            # The model reads the schema with its field descriptions; the decoding constraint alone shows it none.
            note = f"{SCHEMA_NOTE}\n{json.dumps(self.schema, ensure_ascii=False)}"
            if self.messages[0]["role"] == "system":
                self.messages[0]["content"] += "\n\n" + note
            else:
                self.messages.insert(0, {"role": "system", "content": note})
        self.sampling = {name: request[name] for name in SAMPLING if request.get(name) is not None}
        if any(type(value) not in (int, float) for value in self.sampling.values()):
            raise ValueError("Sampling settings must be numbers")
        self.done = threading.Event()
        self.reply = self.error = None
        self.cancelled = False


class Engine:
    def __init__(self):
        self.device = torch.cuda.get_device_name(0)
        torch.cuda.mem_get_info(0)
        torch.full((), 42.0, device="cuda:0").item()
        self.context = int(os.getenv("RAON_CONTEXT", "65536"))
        seqs = int(os.getenv("RAON_SEQS", "8"))
        sizes = sorted({1 << n for n in range(seqs.bit_length()) if 1 << n <= seqs} | {seqs})
        # The fork refuses to start when vLLM resolves another graph mode than the one named here.
        os.environ["RAON_EXPECT_CUDAGRAPH_MODE"] = "FULL_DECODE_ONLY"
        self.llm = LLM(
            model=checkpoint(),
            trust_remote_code=True,
            dtype="bfloat16",
            enforce_eager=False,
            compilation_config={"mode": 0, "cudagraph_mode": "FULL_DECODE_ONLY", "cudagraph_capture_sizes": sizes},
            max_model_len=self.context,
            max_num_batched_tokens=int(os.getenv("RAON_BATCH_TOKENS", "2048")),
            max_num_seqs=seqs,
            kv_cache_memory_bytes=int(os.getenv("RAON_KV_BYTES", "1610612736")),
            gpu_memory_utilization=0.95,
            enable_prefix_caching=False,
            attention_backend="TRITON_MLA",
            # A JSON schema constrains the answer only: with thinking on it applies after the reasoning ends.
            structured_outputs_config={"reasoning_parser": "raon_1"},
        )
        self.tokenizer = self.llm.get_tokenizer()
        self.think_end = self.tokenizer.get_vocab()[THINK_END]
        self.jobs = queue.Queue()
        self.running = {}
        self.llm.chat(
            [{"role": "user", "content": "Answer with one word: What is the capital of South Korea?"}],
            SamplingParams(temperature=0, max_tokens=4), use_tqdm=False,
            chat_template_kwargs={"enable_thinking": False},
        )

    def submit(self, request, gone):
        """Runs a request to its reply. gone() tells whether the caller left; a request nobody waits for is aborted."""
        job = Job(request)
        self.jobs.put(job)
        while not job.done.wait(1.0):
            if gone():
                job.cancelled = True
                return None
        if job.error is not None:
            raise job.error
        return job.reply

    def admit(self, job):
        if job.cancelled:
            return
        try:
            template = {"enable_thinking": job.thinking}
            prompt = self.tokenizer.apply_chat_template(
                job.messages, tokenize=True, add_generation_prompt=True, **template)
            room = self.context - len(prompt)
            if room < 1:
                raise ValueError(f"The input takes {len(prompt)} tokens of the {self.context}-token context")
            params = SamplingParams(
                **{**(THINKING_SAMPLING if job.thinking else {"temperature": 0}), **job.sampling},
                max_tokens=min(job.max_tokens, room),
                structured_outputs=StructuredOutputsParams(json=job.schema) if job.schema is not None else None,
            )
            # The engine returns its internal id (the id results carry, a dash, a random suffix).
            job.engine_id, = self.llm.enqueue_chat(
                job.messages, params, use_tqdm=False, chat_template_kwargs=template)
            self.running[job.engine_id.rsplit("-", 1)[0]] = job
        except Exception as error:  # A request the engine refuses fails alone.
            job.error = error if isinstance(error, (TypeError, ValueError)) else ValueError(str(error))
            job.done.set()

    def finish(self, result):
        job = self.running.pop(result.request_id, None)
        if job is None:  # Aborted after its caller left.
            return
        generation = result.outputs[0]
        tokens = list(generation.token_ids)
        reasoning = 0
        text = generation.text
        if job.thinking:
            # Everything before the end-of-reasoning token is reasoning. An output cut off inside it has no answer.
            reasoning = tokens.index(self.think_end) + 1 if self.think_end in tokens else len(tokens)
            text = self.tokenizer.decode(tokens[reasoning:], skip_special_tokens=True).lstrip("\n")
        truncated = generation.finish_reason == "length"
        status = "incomplete" if truncated else "completed"
        job.reply = {
            "id": f"resp_{uuid.uuid4().hex}",
            "object": "response",
            "created_at": int(time.time()),
            "model": MODEL,
            "status": status,
            "incomplete_details": {"reason": "max_output_tokens"} if truncated else None,
            "output": [{
                "id": f"msg_{uuid.uuid4().hex}",
                "type": "message",
                "role": "assistant",
                "status": status,
                "content": [{"type": "output_text", "text": text, "annotations": []}],
            }],
            "usage": {
                "input_tokens": len(result.prompt_token_ids),
                "output_tokens": len(tokens),
                "input_tokens_details": {"cached_tokens": result.num_cached_tokens or 0},
                "output_tokens_details": {"reasoning_tokens": reasoning},
            },
        }
        job.done.set()

    def run(self):
        engine = self.llm.llm_engine
        while True:
            try:
                self.admit(self.jobs.get(block=not engine.has_unfinished_requests()))
                while True:
                    self.admit(self.jobs.get_nowait())
            except queue.Empty:
                pass
            for key, job in list(self.running.items()):
                if job.cancelled:
                    engine.abort_request([job.engine_id], internal=True)
                    del self.running[key]
            failure = None
            try:
                for result in engine.step():
                    if result.finished:
                        self.finish(result)
            except Exception as error:  # A failed step ends the requests that were in it.
                failure = f"{type(error).__name__}: {error}"
            if failure is not None:
                # The exception is gone here, so the tensors its frames held can be released.
                engine.abort_request([job.engine_id for job in self.running.values()], internal=True)
                for job in self.running.values():
                    job.error = RuntimeError(failure)
                    job.done.set()
                self.running.clear()
                gc.collect()
                torch.cuda.empty_cache()

    def stats(self):
        free, total = torch.cuda.mem_get_info(0)
        return {
            "running": len(self.running), "waiting": self.jobs.qsize(), "context": self.context,
            "allocated_bytes": torch.cuda.memory_allocated(0), "peak_bytes": torch.cuda.max_memory_allocated(0),
            "free_bytes": free, "total_bytes": total,
        }


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def send_json(self, status, data):
        body = json.dumps(data, ensure_ascii=False).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def gone(self):
        """Whether the client closed the connection while its request is being generated."""
        try:
            readable, _, _ = select.select([self.connection], [], [], 0)
            return bool(readable) and self.connection.recv(1, socket.MSG_PEEK) == b""
        except OSError:
            return True

    def do_GET(self):
        if self.path == "/health":
            self.send_json(200, {"status": "ok"})
        elif self.path == "/stats":
            self.send_json(200, self.server.engine.stats())
        elif self.path == "/v1/models":
            self.send_json(200, {"object": "list", "data": [{"id": MODEL, "object": "model"}]})
        else:
            self.send_json(404, {"error": {"message": "Unknown endpoint"}})

    def do_POST(self):
        if self.path != "/v1/responses":
            self.send_json(404, {"error": {"message": "Unknown endpoint"}})
            return
        try:
            length = int(self.headers.get("Content-Length", "0"))
            if not 0 < length <= BODY_LIMIT:
                raise ValueError(f"Request body must be between 1 and {BODY_LIMIT} bytes")
            reply = self.server.engine.submit(json.loads(self.rfile.read(length)), self.gone)
            if reply is None:
                return
        except (TypeError, ValueError) as exc:
            self.send_json(400, {"error": {"message": str(exc)}})
            return
        except RuntimeError as exc:
            self.send_json(500, {"error": {"message": str(exc)}})
            return
        self.send_json(200, reply)


def main():
    engine = Engine()
    port = int(os.getenv("RAON_PORT", "18080"))
    server = ThreadingHTTPServer(("127.0.0.1", port), Handler)
    server.daemon_threads = True
    server.engine = engine
    threading.Thread(target=server.serve_forever, daemon=True).start()
    print(f"{MODEL} ready on 127.0.0.1:{port} ({engine.device})", flush=True)
    engine.run()


if __name__ == "__main__":
    main()
