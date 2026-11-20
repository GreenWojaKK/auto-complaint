# 실행과 검토

명령은 저장소 루트에서 실행한다. 분석 결과는 저장소 밖 `../Dllio-artifact/<실행 이름>/`에 쓴다.

## 준비

- 민원 원문과 교통 자료는 지역 폴더(`DLLIO_REGION`, 기본 `private/ulsan`)에서 읽는다.
- PostgreSQL의 `dllio` 데이터베이스를 켠다. 계획(`plan`)과 조회 실행(`execute`)이 읽는다. 유형 판정, 추출, 답 쓰기는 DB를 쓰지 않는다.
- 키는 `secrets/vllm-001.txt`(판정 모델), `secrets/oai-001.txt`(언어 모델), `secrets/pg-dllio-read.txt`(DB 읽기 계정), `secrets/vworld.txt`(VWorld 장소 검색)다.
  판정·언어 모델 키는 환경 변수 `VLLM_SR_API_KEY`·`OPENAI_API_KEY`나 파일 경로를 담은 `*_FILE`로도 넘긴다.
- 명령을 빌드한다: `go build -o ../Dllio-artifact/bin/dllio.exe ./backend/cmd/dllio`

## 분석 실행

```powershell
$Run = "../Dllio-artifact/<실행 이름>"
dllio problems --out $Run --cases 01,03
dllio extract --run $Run
dllio plan --run $Run
dllio execute --run $Run
dllio compose --run $Run
```

단계마다 `<실행>/<단계>/<번호>.json`을 쓴다. 폴더는 `l2`, `extract`, `candidates`, `plan`, `execute`, `compose`, `view`다.
`--cases`를 비우면 앞 단계 결과가 있는 민원 전부를 처리한다. `--effort`는 언어 모델의 추론 강도(기본 `high`),
`--workers`는 동시에 처리할 민원 수(기본 5)다. `compose --out <다른 폴더>`는 같은 계획으로 쓴 답을 다른 폴더에 둬 비교할 때 쓴다.
모델 호출은 호출마다 파일 하나로 `DLLIO_CALL_DIR`(기본 `../Dllio-artifact/model-calls/dllio/`)에 남는다.

단계의 입력·판정·검사·출력은 [ANALYSIS_STAGES](docs/ANALYSIS_STAGES.md)를 따른다.

## 결과 보기와 검토

- 뷰어: `main` 브랜치에서 `cargo run --manifest-path viewer/Cargo.toml -- <실행>/view`
- 검토 도구는 `preprocess/.venv/Scripts/python.exe`로 실행한다.

| 도구 | 하는 일 |
|---|---|
| `test/review/review_front.py --run <실행>` | 원문, 유형별 확률, 추출, 계획, 조회 결과를 나란히 놓은 검토 문서 |
| `test/review/compare_runs.py` | 두 실행의 계획과 답·페이지 비교 |
| `test/review/plan_stability.py --runs <실행…> --cases …` | 같은 입력으로 여러 번 쓴 계획의 일치도 |
| `test/review/audit_usage.py --logs <호출 기록 폴더>` | 모델·단계·민원별 토큰 사용량 합산 |

지역 폴더의 `complaints/abstract/<번호>.md`는 사람이 원문 대신 읽는 민원 요약이다. 분석의 입력이나 채점 기준이 아니다.

## 시험

```powershell
go test ./test/backend/
preprocess/.venv/Scripts/python.exe -m unittest discover -s test/preprocess -p 'test_*.py'
preprocess/.venv/Scripts/python.exe -m unittest discover -s test/review -p 'test_*.py'
```

DB가 켜져 있으면 DB를 읽는 Go 시험도 함께 돈다.

## 교통 자료 갱신

원천 자료가 바뀌면 Python 전처리로 연결망·장소·노선 계보를 다시 만들고 DB에 싣는다. 순서와 입력은
[TRANSIT_BACKEND](docs/TRANSIT_BACKEND.md#자료-준비)를 따른다.
