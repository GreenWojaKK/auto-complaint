# 모델 호출과 실행

분석 단계는 [`backend/matchi`](../backend/matchi/)를 통해 판정과 생성을 요청한다. 판정은 코드가 구성한 문항의 보기별 확률을 반환하고, 생성은 지정한 출력 형식에 맞는 텍스트를 반환한다. 조회 선택지의 구성과 도구 실행은 분석 단계의 코드가 담당한다.

## 구성 요소와 수명

| 구성 요소 | 역할 |
|---|---|
| `backend/matchi` | 판정·생성 요청 구성, 출력 검증, 재시도와 호출 기록 |
| `backend/worker`의 Go client | 판정 worker(Python 프로세스) 시작·종료, 요청 전송과 ID별 응답 전달 |
| 판정 worker | 판정 모델 로딩과 판정 실행 |
| Device kernel | ROCm 장치에서 packed INT4 가중치의 행렬곱 실행 |
| Raon 생성 서버 | 생성 요청을 받는 별도 프로세스. 아래 "Raon 생성 서버"를 따른다 |

판정과 생성은 서로 다른 모델이 맡는다. 판정은 d3-ultra-INT4의 판정 readout이 하고, 생성은 Raon-1-mini-preview가 한다.

| 실행 경로 | 분석 단계에서의 용도 |
|---|---|
| 판정 | 유형·측면 분류, 장소 대응 판정(정류장 이름 검색이나 지도 검색으로 찾은 후보가 원문의 장소 지칭을 가리키는지), 이동 연결, 개편 전 노선 선택, 조회 실행 여부 판정 |
| 생성 | 유형별 사정과 이동 추출, 지시 표현 해석, L1과 조회 질문 작성, 답변과 화면 구성 |

단계 명령은 판정이 처음 필요할 때 판정 worker를 시작하고 명령이 끝나면 worker도 끝낸다. Python 프로그램 `backbone.py`는 `dllio` 실행 파일에 포함된다. Raon 생성 서버는 단계 명령과 따로 `raon/serve.sh`로 띄운다.

## 실행 설정

| 환경변수 | 의미 | 기본값 |
|---|---|---|
| `DLLIO_PYTHON` | 판정 worker를 실행할 기존 Python 환경의 실행 파일 | `python3` |
| `DLLIO_MODELS` | 모델 디렉터리 | `~/models` |
| `DLLIO_DEVICE` | 판정 모델을 로드할 PyTorch device | `cuda:0` |
| `DLLIO_KERNELS` | Device kernel 라이브러리 | 실행 파일 옆의 `libdllio_kernels.so` |

모델 디렉터리의 `d3-ultra-INT4`에 `vllm-sr/d3-ultra-INT4`의 모델 파일과 실행 모듈을 둔다. 기존 Python 환경에는 모델의 `requirements.txt`에 명시된 의존성이 필요하다.

[`kernels/int4_linear.hip`](../kernels/int4_linear.hip)은 지원하는 INT4 연산을 packed 가중치에서 직접 계산한다. [빌드 스크립트](../tools/build_kernels.sh)로 ROCm 라이브러리를 생성한다. 라이브러리가 없으면 checkpoint의 linear layer를 사용한다.

## 판정 worker 프로토콜

Go client와 판정 worker는 stdin/stdout으로 JSONL 메시지를 교환한다. Worker의 첫 메시지는 `ready: true`, 모델 이름 `model`, 장치 이름 `device`, packed INT4 kernel 사용 여부 `packed_linear`를 포함한다. Worker는 stdin이 닫히면 끝난다.

### 요청 envelope

| 필드 | 형식 | 의미 |
|---|---|---|
| `id` | 정수 | Go client가 부여하는 요청 식별자 |
| `op` | 문자열 | `decide`. 다른 값은 오류로 응답한다 |
| `request` | 객체 | 판정 요청 본문 |

### 응답 envelope

| 필드 | 형식 | 의미 |
|---|---|---|
| `id` | 정수 | 요청에 대응하는 식별자 |
| `reply` | 객체 | 성공한 요청의 응답 본문 |
| `error` | 문자열 | 실패한 요청의 오류 |

여러 민원의 요청을 동시에 제출할 수 있다. Worker는 요청을 받은 순서로 하나씩 실행하고, Go client가 `id`로 해당 호출에 응답을 전달한다. 한 요청이 실패해도 worker는 다음 요청을 받는다. 장치 메모리 부족으로 실패한 요청은 장치 cache를 비운 뒤 오류로 응답한다.

## 판정 요청

[`decision.go`](../backend/matchi/decision.go)는 System One 형식으로 판정을 요청한다.

| 필드 | 내용 |
|---|---|
| `model` | 판정 모델 식별자 |
| `state` | 민원 원문 |
| `questions` | 문항 식별자로 구분한 질문 객체 |
| `questions.<id>.type` | `choice` |
| `questions.<id>.criteria` | `yes`·`no` 보기와 각 보기의 판정 기준 |
| `questions.<id>.instructions` | `field`, `question`, `goal`, `background`를 포함하는 판정 지시 |

유형·측면 판정은 해당 단계의 문항 전체를 한 요청으로 보낸다. 장소 대응·이동 연결·개편 전 노선·조회 판정은 문항을 최대 24개씩 묶는다. 요청이 판정 모델의 입력 한도를 넘으면(`request_too_long`) 묶음을 반으로 나눠 다시 묻는다.

응답의 `answers`는 문항별 `choice`, `confidence`, `probabilities`를 포함한다. Client는 요청한 문항과 보기의 존재, 확률의 유효 범위와 합을 검사한다. 확률 합의 허용 오차는 `1e-6 + 보기 수 × 0.005`다. 분석 단계는 `yes` 확률이 기준값 0.5 이상이면 긍정 판정으로 처리한다.

HTTP 판정 API에 연결하지 못했거나 서버 오류(5xx)를 받으면 5초, 20초, 60초를 기다려 세 번까지 다시 보낸다. 그 밖의 전송 실패나 응답 검사 실패는 호출 오류를 반환한다. 장소 후보의 경계 확률 재판정처럼 입력을 바꿔 다시 묻는 조건은 [분석 단계](ANALYSIS_STAGES.md)가 정한다.

## 생성 요청

[`writer.go`](../backend/matchi/writer.go)의 `Write`는 지시문, 입력, 출력 schema와 단계별 검증 함수를 받아 Responses 형식의 요청을 생성 서버의 `<URL>/responses`로 보낸다.

| 요청 항목 | 처리 |
|---|---|
| `instructions` | 생성 지시문 |
| `input` | 순서가 있는 대화 메시지 |
| `text.format` | `type: json_schema`, 출력 이름, schema와 `strict: true` |
| `reasoning.effort` | 추론 설정. 단계 명령의 `--effort`로 지정하며 기본값은 `none` |
| `max_output_tokens` | 호출 하나의 출력 token 상한. `DLLIO_LANGUAGE_MAX_OUTPUT`으로 정하고, 지정하지 않으면 24000이다 |

### 출력 schema

[`schema.go`](../backend/matchi/schema.go)는 Go 구조체에서 JSON schema를 생성한다. 필드 설명은 `desc`, 고정 선택지는 `enum`, 배열 길이는 `min`·`max`, 글자 수 상한은 `maxlen` 태그로 지정한다. 포인터 필드는 `null`을 허용한다. 민원별로 달라지는 선택지(예: 그 민원에서 고른 유형 id)는 호출 시 전달한다.

## 출력 검증과 재시도

생성 응답은 JSON parsing, schema 검증, 단계별 검증 함수 순서로 검사한다. 모든 검사를 통과한 값을 호출자에게 반환한다. 단계별 검증 함수는 검사 전에 모델 출력을 원문·조회 값과 대조해 정리할 수 있다(정리 규칙은 [분석 단계](ANALYSIS_STAGES.md)가 정한다).

| 구분 | 조건 | 처리와 상한 |
|---|---|---|
| 생성 결과 재작성 | JSON·schema·단계 검증 실패 | 최초 생성 이후 기본 최대 3회 재작성, `compose`는 최대 4회 재작성 |
| 출력 상한 도달 | 추론 없이 본문이 출력 상한까지 이어진 응답 | 줄여 쓰라는 고칠 내용을 붙여 재작성. 재작성 횟수에 든다 |
| 추론의 출력 상한 도달 | 추론 token을 쓴 응답이 상한에서 끝남 | 재작성하지 않고 호출 오류 반환 |
| HTTP 생성 요청 재전송 | 연결·응답 읽기 실패 또는 재시도 가능한 HTTP 응답 | 최초 전송 이후 최대 2회 재전송 |

재작성은 직전 답을 대화에 잇지 않고, 최초 입력 뒤에 아래 안내와 최신 검증 실패 이유를 붙인 새 요청으로 보낸다. 직전 답을 대화에 이으면 모델이 같은 답을 되풀이하기 때문이다.

> 같은 요청을 처음부터 다시 작성한다. 앞서 작성한 답은 다음 검사를 통과하지 못했다.

HTTP 재전송은 `x-should-retry` 헤더가 있으면 그 값을 따르고, 없으면 HTTP 408·409·429·5xx에 적용한다. 대기시간은 응답의 retry header 또는 backoff를 사용한다. 잔액 소진 오류는 즉시 실행을 종료한다.

## Endpoint 설정

설정은 [`secret.go`](../backend/matchi/secret.go)가 읽는다. 환경변수가 없으면 판정은 판정 worker가, 생성은 이 컴퓨터의 Raon 생성 서버(`http://127.0.0.1:18080/v1`, 모델 `raon-r9700`)가 받는다. 다른 곳을 쓸 때는 역할별로 지정한다.

| 역할 | Base URL | 요청 경로 | 모델 이름 | 인증 키 또는 키 파일 |
|---|---|---|---|---|
| 판정 | `DLLIO_DECISION_URL` | `<URL>/v1/systemone` | `DLLIO_DECISION_MODEL` | `DLLIO_DECISION_KEY`, `DLLIO_DECISION_KEY_FILE` |
| 생성 | `DLLIO_LANGUAGE_URL` | `<URL>/responses` | `DLLIO_LANGUAGE_MODEL` | `DLLIO_LANGUAGE_KEY`, `DLLIO_LANGUAGE_KEY_FILE` |

키는 직접 지정한 환경변수 값을 우선하고, 값이 없으면 지정된 키 파일을 읽는다. 키가 있으면 `Authorization` 헤더에 쓰고, 없으면 헤더를 보내지 않는다.

`DLLIO_LANGUAGE_MAX_OUTPUT`은 생성 호출 하나의 출력 token 상한이다. 생성 서버가 추론 token을 출력에 포함하면 이 상한은 추론과 답변 본문에 함께 적용된다.

## Raon 생성 서버

[`raon/`](../raon/)은 Raon-1-mini-preview를 WSL의 AMD 장치(RDNA4)에서 생성 서버로 실행한다. 서버는 생성 역할만 맡는다. 판정 역할은 판정 worker나 `DLLIO_DECISION_URL`의 endpoint가 맡는다.

| 파일 | 역할 |
|---|---|
| `serve.sh` | rdna4-mxfp6a8 저장소의 `raon/env.sh`를 source해 엔진 환경을 준비하고 서버를 시작한다 |
| `serve.py` | 엔진을 한 프로세스에 로드하고 loopback `/v1/responses`를 제공한다. 엔진은 main thread에서 실행하고, 처리 중인 요청들을 한 batch로 생성한다. 새 요청은 다음 engine step에 합류한다 |

모든 요청이 token 하나씩 생성하는 decode step은 시작할 때 capture한 장치 graph를 재생한다(vLLM `FULL_DECODE_ONLY`). Graph는 요청 1·2·4·8개처럼 `RAON_SEQS`까지의 2의 거듭제곱과 `RAON_SEQS`에 대해 만들고, 그 사이의 요청 수는 바로 위 크기의 graph로 실행한다. 입력 token을 받는 step은 graph 없이 실행한다. Graph를 쓰면 decode step에서 host가 실행을 거는 시간이 사라져 step 시간이 장치의 계산 시간이 된다. 시작할 때 graph capture에 약 30초가 더 걸린다.

엔진 환경은 rdna4-mxfp6a8 저장소(Dllio 옆의 `../rdna4-mxfp6a8`, `RDNA4_MXFP6A8`으로 바꾼다)가 정한다. 그 저장소의 `raon/env.sh`가 KRAFTON vLLM 포크의 고정 커밋에 패치를 적용해 실행 트리를 조립하고, routed expert와 MLA prefill attention을 그 저장소의 RDNA4 kernel로 계산하게 한다. 조립과 kernel의 구성은 그 저장소의 README를 따른다.

Checkpoint는 routed expert 가중치를 MXFP6 E2M3 packed 바이트로 저장한 `Raon-1-mini-preview-MXFP6-E2M3`다. Routed expert의 두 곱은 입력 활성값을 FP8(E4M3)로 반올림해 곱한다(W6A8). 그 밖의 연산, 입력 embedding과 vocabulary head는 BF16이다.

| 환경변수 | 의미 | 기본값 |
|---|---|---|
| `RAON_PYTHON` | vLLM ROCm 환경의 Python 실행 파일 | `~/.venvs/vllm-rocm10/bin/python` |
| `RDNA4_MXFP6A8` | rdna4-mxfp6a8 저장소의 위치 | Dllio 옆의 `../rdna4-mxfp6a8` |
| `RAON_RUNTIME` | 실행 트리(`fork/`)와 wheel(`wheel/vllm-*.whl`)을 두는 WSL 폴더 | `~/.local/share/rdna4-mxfp6a8/raon` |
| `RAON_MODEL_PATH` | Checkpoint 폴더 | `DLLIO_MODELS` 아래 `Raon-1-mini-preview-MXFP6-E2M3` |
| `RAON_CONTEXT` | 입력과 출력을 합한 context 길이(token). 모델이 지원하는 길이는 262,144다 | 65536 |
| `RAON_SEQS` | 함께 생성하는 요청 수의 상한 | 8 |
| `RAON_KV_BYTES` | KV cache 크기(byte). MLA 7개 층이 token당 약 8KiB를 쓴다 | 1610612736 |
| `RAON_BATCH_TOKENS` | 엔진의 한 step이 받는 token 수의 상한 | 2048 |
| `RAON_PORT` | Loopback port | 18080 |

서버는 준비가 끝나면 `raon-r9700 ready on 127.0.0.1:<port>`를 출력한다. 기본값은 31.7GiB 장치(Radeon AI PRO R9700)에서 가중치와 KV cache를 둔 뒤 prefill 작업 메모리가 남도록 정한 값이다. KV cache가 차면 엔진이 요청을 대기시킨다. `GET /stats`는 처리 중·대기 중 요청 수와 장치 메모리의 현재·최대 할당량을 반환한다.

| 요청 항목 | 처리 |
|---|---|
| `model` | `raon-r9700` |
| `instructions`, `input` | System 메시지와 대화 메시지. 텍스트만 받는다 |
| `text.format` | `text` 또는 `json_schema`. Schema는 system 메시지 끝에 붙여 모델이 필드 설명을 읽게 하고, structured output 제약으로도 적용한다 |
| `reasoning.effort` | 생략하거나 빈 값, `none`, `minimal`이면 thinking을 끄고 그 밖의 값이면 켠다. Thinking을 켜면 추론 종료 token 뒤의 본문만 응답 텍스트로 반환하고, 추론 token 수를 `usage.output_tokens_details.reasoning_tokens`에 적는다. Schema 제약은 추론이 끝난 뒤의 본문에 적용한다 |
| `max_output_tokens` | 출력 token 상한. 기본값 256. 입력을 뺀 context의 남은 길이보다 크면 남은 길이를 적용한다. 추론 token도 이 상한에 포함된다. 상한에 닿으면 응답의 `status`가 `incomplete`다 |
| `temperature`, `top_p`, `top_k`, `presence_penalty`, `seed` | Sampling 설정. 생략하면 thinking을 끈 요청은 greedy decoding(`temperature` 0)이고, thinking을 켠 요청은 모델 카드의 권장값(`temperature` 1.0, `top_p` 0.95, `top_k` 20, `presence_penalty` 1.5)이다. Greedy decoding에서는 추론이 끝나지 않고 출력 상한까지 이어지는 요청이 많다 |
| `stream`, `store`, `tools` | 지원하지 않는다 |

요청 본문은 4MiB 이하다. 입력이 context를 넘는 요청은 HTTP 400, engine step이 실패한 요청은 HTTP 500으로 응답하고 서버는 다음 요청을 계속 받는다. 응답을 기다리던 client가 연결을 닫으면 그 요청의 생성을 중단한다.

분석 단계는 환경변수 없이 이 서버에 생성을 요청한다. 서버를 먼저 `raon/serve.sh`로 띄우고 준비 완료 줄이 나온 뒤 단계 명령을 실행한다.

## 호출 기록

[`calllog.go`](../backend/matchi/calllog.go)는 호출마다 JSON 파일 하나를 `DLLIO_CALL_DIR`에 저장한다. 기본 경로는 산출물 디렉터리(`DLLIO_ARTIFACT_DIR`, 기본값 `../Dllio-artifact/`)의 `model-calls/dllio/`다.

| 항목 | 내용 |
|---|---|
| `scope` | 민원 식별자 `case`와 단계 `phase` |
| `model`, `settings` | 호출 모델과 실행 설정 |
| `startedAt`, `finishedAt` | 호출 시작·종료 시각 |
| `request`, `requestHash`, `messageCount` | 요청 본문, 본문의 hash와 입력 메시지 수 |
| `requestParameters` | 빈 객체로 기록하는 필드 |
| `status` | 시작 시 `pending`, 완료 시 `complete` 또는 `failed` |
| `response` | 성공한 호출의 응답 메시지 |
| `error` | 실패한 호출의 오류 |

`response`에는 `model_name`, `provider_response_id`, `timestamp`, `usage`, `parts`를 기록한다. `usage`는 입력·출력·cache 읽기·cache 쓰기·추론 token 수를 각각 `input_tokens`, `output_tokens`, `cache_read_tokens`, `cache_write_tokens`, `output_reasoning_tokens`로 저장한다.

단계 결과 파일의 `messages`는 요청과 응답을 순서대로 저장한다. `parts[].part_kind`는 최초 입력 `user-prompt`, 응답 본문 `text`, 재작성 요청의 검증 실패 이유 `retry-prompt`를 구분한다.

검토 도구 [audit_usage.py](../test/review/audit_usage.py), [compare_runs.py](../test/review/compare_runs.py), [plan_stability.py](../test/review/plan_stability.py)는 이 기록을 읽는다.

## 생성 런타임의 성능 비교

같은 민원 묶음으로 기준 구현과 변경 구현을 비교한다. 입력 token ID, 배치 크기, 요청당 출력 token 수와 생성 설정을 고정한다. 여러 변경을 조사할 때는 각 변경을 같은 기준에 개별 적용한다.

1. 병목이 의심되는 호출에서 스택과 호출 횟수를 표본으로 수집한다. GPU 실행은 입력 처리(prefill)와 다음 token 생성(decode)으로 나눠 재고, 호스트 호출 시간과 GPU 스트림 경과시간을 각각 기록한다.
2. 각 구현을 본측정과 같은 입력과 배치로 워밍업한다. 상세 추적을 끄고 실행 순서를 교대한다. GPU를 동기화한 뒤 단조 시계로 시간을 재기 시작하고, 마지막 응답의 GPU 작업이 끝나면 측정을 마친다.
3. 처리량은 반복 전체의 생성 token 수를 총 경과시간으로 나눈다. 요청별 생성 token ID를 기준과 대조하고, 다르면 처음 달라진 위치를 기록한다. 커널 단독 실행시간과 같은 변경을 적용한 전체 민원 묶음의 처리시간을 함께 비교한다.
