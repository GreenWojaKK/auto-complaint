# 실행과 검토

명령은 저장소 루트에서 실행한다. 단계 결과는 `--out`·`--run`으로 지정한 실행 디렉터리에, 호출 기록은 산출물 디렉터리에 저장한다. 산출물 디렉터리의 기본값은 저장소 밖 `../Dllio-artifact/`이며 `DLLIO_ARTIFACT_DIR`로 변경할 수 있다.

## 입력과 실행 환경

분석 대상 지역은 `DLLIO_REGION`으로 지정한다. 저장소 루트 기준 상대 경로나 절대 경로를 사용할 수 있으며, 기본값은 [examples/solmoe](examples/solmoe/)다. 지역 디렉터리의 `region.json`은 지역 이름과 PostgreSQL database를, `complaints/`는 민원 원문과 접수일을 제공한다. 교통 입력과 전처리는 [TRANSIT_BACKEND](docs/TRANSIT_BACKEND.md#자료-준비)를 따른다.

- `plan`과 `execute`는 지역 설정의 PostgreSQL database를 읽는다. DB 조회 비밀번호는 `secrets/pg-dllio-read.txt`로 제공한다.
- VWorld 장소 검색을 사용할 때는 `VWORLD_API_KEY`, `VWORLD_API_KEY_FILE` 또는 `secrets/vworld.txt`로 키를 제공한다.
- 판정 worker의 Python 실행 파일과 모델 경로는 [MODEL_RUNTIME](docs/MODEL_RUNTIME.md#실행-설정)에 따라 지정한다.
- 생성은 Raon 생성 서버가 받는다. `extract`, `plan`, `compose` 전에 [MODEL_RUNTIME](docs/MODEL_RUNTIME.md#raon-생성-서버)에 따라 `raon/serve.sh`로 서버를 띄운다.
- 전처리와 검토 도구는 [preprocess/pyproject.toml](preprocess/pyproject.toml)의 의존성이 설치된 기존 Python 환경에서 실행한다.

아래 shell 예시의 Python 경로는 실제 사용할 기존 환경의 실행 파일로 바꾼다. `run_dir`은 이번 분석 결과를 저장할 경로다.

```sh
export DLLIO_REGION=examples/solmoe
export DLLIO_PYTHON=/path/to/existing/model-environment/bin/python
export DLLIO_PREPROCESS_PYTHON=/path/to/existing/preprocess-environment/bin/python
export PYTHONPATH="$PWD/preprocess/src${PYTHONPATH:+:$PYTHONPATH}"
run_dir=../Dllio-artifact/review
```

`DLLIO_PREPROCESS_PYTHON`은 아래 명령에서 사용할 전처리·검토용 interpreter 경로다.

## 빌드

판정 worker를 실행할 WSL 환경에서 CLI를 빌드한다. AMD 장치의 kernel도 같은 환경에서 빌드한다.

```sh
go build -o ../Dllio-artifact/bin/dllio ./backend/cmd/dllio
tools/build_kernels.sh ../Dllio-artifact/bin
```

## 분석 실행

```sh
../Dllio-artifact/bin/dllio problems --out "$run_dir" --cases 01,03
../Dllio-artifact/bin/dllio extract --run "$run_dir"
../Dllio-artifact/bin/dllio plan --run "$run_dir"
../Dllio-artifact/bin/dllio execute --run "$run_dir"
../Dllio-artifact/bin/dllio compose --run "$run_dir"
```

단계별 입력·판정·검증·출력은 [ANALYSIS_STAGES](docs/ANALYSIS_STAGES.md)를 따른다.

| 항목 | 동작 |
|---|---|
| `--cases` | 처리할 민원 번호. `problems`에서 생략하면 지역의 전체 민원을, 후속 단계에서 생략하면 해당 단계의 입력 결과가 있는 민원을 처리한다 |
| `--effort` | 생성의 추론 강도. 기본값 `none` |
| `--workers` | 동시에 처리할 민원 수. 기본값은 `compose`가 1, 그 밖의 단계가 5 |
| `compose --out` | 답변과 화면 결과를 별도로 저장할 출력 디렉터리 |
| `DLLIO_CALL_DIR` | 호출별 JSON 기록을 저장할 디렉터리. 기본값은 산출물 디렉터리의 `model-calls/dllio/` |

결과 파일은 `<실행>/<단계>/<번호>.json`이다. 단계 디렉터리는 `l2`, `extract`, `candidates`, `plan`, `execute`, `compose`, `view`다.

## 결과 보기와 검토

Rust 뷰어는 `view` 디렉터리를 첫 인자로 받고 선택적으로 지도 배경 파일을 두 번째 인자로 받는다. 저장소 루트에서 다음과 같이 실행한다. Windows에서 실행할 때는 Windows 형식의 결과 경로를 사용한다.

```sh
cargo run --manifest-path viewer/Cargo.toml -- "$run_dir/view"
```

검토 도구는 앞에서 지정한 interpreter로 실행한다.

```sh
"$DLLIO_PREPROCESS_PYTHON" test/review/review_front.py --run "$run_dir"
```

| 도구 | 기능 |
|---|---|
| [review_front.py](test/review/review_front.py) | 원문, 유형별 확률, 추출, 계획과 조회 결과를 나란히 표시한 검토 문서 생성 |
| [compare_runs.py](test/review/compare_runs.py) | 두 실행의 계획·답변·페이지 비교 |
| [plan_stability.py](test/review/plan_stability.py) | 동일 입력으로 생성한 계획의 일치도 계산 |
| [audit_usage.py](test/review/audit_usage.py) | 호출 기록의 모델·단계·민원별 token 사용량 집계 |

## 웹 검토 화면 빌드

[app/src/web](app/src/web/)은 민원 목록 `/data/cases/index.json`, 민원별 `/data/cases/<번호>.json`, 지도 배경 `/data/map-context.json`을 읽는다. 화면 자료의 연결과 배포 작업은 [DEFERRED](DEFERRED.md#웹-검토-화면의-자료-연결과-배포)에 둔다.

```sh
cd app
npm ci
npm run build
```

## 테스트

다음 명령은 저장소 루트에서 실행한다.

```sh
go test ./test/backend/
"$DLLIO_PREPROCESS_PYTHON" -m unittest discover -s test/preprocess -p 'test_*.py'
"$DLLIO_PREPROCESS_PYTHON" -m unittest discover -s test/review -p 'test_*.py'
```

DB가 실행 중이면 DB 조회 테스트도 실행한다.

## 교통 자료 갱신

원천 자료가 바뀌면 지역 디렉터리를 지정해 연결망·장소·노선 계보를 다시 생성하고 DB에 적재한다. 모듈별 입력과 출력은 [TRANSIT_BACKEND](docs/TRANSIT_BACKEND.md#자료-준비)를 따른다.
