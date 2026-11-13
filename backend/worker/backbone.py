"""Dllio 모델 worker: d3-ultra-INT4 한 벌을 장치에 올려 판정 요청을 받는다.

판정은 checkpoint의 `system_one`으로 답한다. 쓰기는 이 worker가 받지 않는다. 쓰기 모델은 따로 띄운 생성 서버다.

주고받는 방식: stdin으로 한 줄에 JSON 하나를 받고 stdout으로 답한다. 첫 답은 준비가 끝났음을 알린다. stdin이 닫히면 끝난다.
요청은 받는 대로 쌓이고, 받은 순서로 실행해 요청의 id를 달고 답한다.

- `{"id", "op": "decide", "request"}`: 판정 요청. request는 `state`와 `questions`다. 답은 `{"id", "reply"}`이고 reply는 `system_one`의 응답이다.
- 실패한 요청의 답은 `{"id", "error"}`다.

인자: 모델 폴더(`d3-ultra-INT4`가 든 곳), 장치, 장치 kernel 라이브러리(없어도 된다).
"""
import ctypes
import json
import os
import queue
import sys
import threading

# 라이브러리가 찍는 글이 답 줄에 섞이지 않게 한다.
replies = os.fdopen(os.dup(1), 'w', encoding='utf-8', buffering=1)
os.dup2(2, 1)
sending = threading.Lock()

# WSL의 ROCm은 expandable segments의 할당을 거절한다. torch를 읽기 전에 정한다.
os.environ.setdefault('PYTORCH_CUDA_ALLOC_CONF', 'expandable_segments:False')

import torch  # noqa: E402

# 판정 한 번이 state의 키와 값을 문항 줄마다 복사하는 데 쓰는 장치 메모리.
QUESTION_PASS_BYTES = 1 << 30
# int4 층이 packed 가중치에서 바로 곱하는 입력 행 수의 상한. 더 많으면 가중치를 풀어 곱하는 쪽이 빠르다.
PACKED_ROWS = 32


def release_page_cache(*paths: str) -> None:
    """checkpoint의 page cache를 비운다. WSL에서는 이것이 Windows commit 한도에 잡힌다."""
    if not hasattr(os, 'posix_fadvise'):
        return
    for path in paths:
        descriptor = os.open(path, os.O_RDONLY)
        os.posix_fadvise(descriptor, 0, 0, os.POSIX_FADV_DONTNEED)
        os.close(descriptor)


def install_packed_linear(path: str) -> None:
    """int4 층이 decode step처럼 행이 적은 입력을 packed 가중치에서 바로 곱하게 한다."""
    multiply = ctypes.CDLL(path).dplus_int4_linear
    multiply.argtypes = [ctypes.c_void_p] * 5 + [ctypes.c_int] * 4
    multiply.restype = ctypes.c_int
    layers = sys.modules['d3q.kernels'].Int4Linear
    unpack_and_multiply = layers.forward

    def forward(layer, x):
        rows = x.numel() // layer.in_features
        if rows > PACKED_ROWS or x.dtype != torch.bfloat16 or layer.bias is not None:
            return unpack_and_multiply(layer, x)
        flat = x.reshape(rows, layer.in_features).contiguous()
        y = torch.empty(rows, layer.out_features, dtype=torch.bfloat16, device=x.device)
        zero = layer.weight_zero_point
        code = multiply(layer.weight_packed.data_ptr(), layer.weight_scale.data_ptr(), None if zero is None else zero.data_ptr(),
                        flat.data_ptr(), y.data_ptr(), layer.out_features, layer.in_features, layer.group_size, rows)
        if code:  # kernel이 받지 않는 층이나 행 수.
            return unpack_and_multiply(layer, x)
        return y.view(*x.shape[:-1], layer.out_features)

    layers.forward = forward


def send(message: dict) -> None:
    with sending:
        replies.write(json.dumps(message, ensure_ascii=False) + '\n')


class Job:
    """요청 하나. 끝나면 요청의 id를 단 답을 보낸다."""

    def __init__(self, identifier, kind: str, payload: dict):
        self.identifier, self.kind, self.payload = identifier, kind, payload

    def finish(self, reply: dict) -> None:
        send({'id': self.identifier, 'reply': reply})

    def fail(self, error: Exception) -> None:
        send({'id': self.identifier, 'error': f'{type(error).__name__}: {error}'[:500]})


class Backbone:
    """장치에 올린 d3-ultra-INT4 한 벌."""

    def __init__(self, models: str, device: str, kernels: str = ''):
        model_path = os.path.join(models, 'd3-ultra-INT4')
        self.name = os.path.basename(model_path)
        self.device = torch.device(device)
        sys.path.insert(0, model_path)
        import transformers.modeling_utils as loader
        from d3q.load import load_d3

        # loader가 양자화 전 크기를 먼저 잡으려 하면 ROCm이 거절한다. packed 가중치는 들어간다.
        loader.caching_allocator_warmup = lambda *args, **kwargs: None
        self.model = load_d3(model_path, device=device)
        self.tokenizer = self.model.tokenizer
        self.text = self.model.backbone.language_model
        config = self.text.config
        # 판정의 문항 묶음이 state의 키와 값을 줄마다 복사하는 양을 이 장치에 맞게 줄인다. 답은 같다.
        per_key = 2 * config.num_attention_heads * config.head_dim * 2
        sys.modules['d3_runtime'].STATE_ONCE_KEYS = QUESTION_PASS_BYTES // per_key
        release_page_cache(os.path.join(model_path, 'model.safetensors'), os.path.join(model_path, 'readout.safetensors'))
        self.packed_linear = bool(kernels) and os.path.exists(kernels) and self.device.type == 'cuda'
        if self.packed_linear:
            install_packed_linear(kernels)

    @torch.inference_mode()
    def decide(self, request: dict) -> dict:
        return self.model.system_one(state=request.get('state'), questions=request['questions'], images=None)


class Engine(threading.Thread):
    """장치를 혼자 쓰는 실행 루프. 판정 요청을 받은 순서로 실행한다."""

    def __init__(self, backbone: Backbone):
        super().__init__(daemon=True, name='engine')
        self.backbone = backbone
        self.jobs: queue.Queue[Job] = queue.Queue()

    def run(self) -> None:
        while True:
            job = self.jobs.get()
            try:
                job.finish(self.backbone.decide(job.payload))
            except Exception as error:  # 한 요청의 실패가 worker를 끝내지 않는다.
                if isinstance(error, torch.OutOfMemoryError) and self.backbone.device.type == 'cuda':
                    torch.cuda.empty_cache()
                job.fail(error)


def main() -> None:
    models, device = sys.argv[1], sys.argv[2]
    kernels = sys.argv[3] if len(sys.argv) > 3 else ''
    backbone = Backbone(models, device, kernels)
    engine = Engine(backbone)
    name = torch.cuda.get_device_name(backbone.device) if backbone.device.type == 'cuda' else 'cpu'
    send({'ready': True, 'model': backbone.name, 'device': name, 'packed_linear': backbone.packed_linear})
    engine.start()
    for line in sys.stdin:
        if not line.strip():
            continue
        request = None
        try:
            request = json.loads(line)
            if request['op'] != 'decide':
                raise ValueError(f"unknown operation {request['op']!r}")
            engine.jobs.put(Job(request.get('id'), request['op'], request['request']))
        except Exception as error:  # 읽지 못한 요청이 worker를 끝내지 않는다.
            send({'id': request.get('id') if isinstance(request, dict) else None, 'error': f'{type(error).__name__}: {error}'[:500]})


if __name__ == '__main__':
    main()
