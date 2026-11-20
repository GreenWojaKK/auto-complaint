# 들리오의 구조

## 표현 수준

- **원문**: 시민이 쓴 전체 글이다. 모든 판정과 생성의 근거다.
- **L2**: 여러 민원에 공통으로 적용되는 교통 문제 유형이다. 유형의 정의와 경계는 [L2 catalog](L2_CATALOG.md)에 둔다. 한 민원에 여러 유형이 함께 있을 수 있다.
- **이동**: 유형마다 원문이 말한 출발지·도착지, 시간대, 시민이 말한 수치, 대상(노선·정류장·장소)이다.
- **L1**: 행정 검토자가 읽는 요약(한줄요약, 상황, 요청, 문의)이다. L2와 이동을 뼈대로 쓴다.

민원 읽기는 추상에서 구체로 진행한다. L2를 먼저 정하고, 이동과 L1은 그 L2를 기반으로 쓴다. 표현 규칙은 [READING_LEVELS](READING_LEVELS.md)를 따른다.

## 단계와 담당

| 단계 | 담당 | 출력 |
|---|---|---|
| `problems` | 판정 모델이 유형과 측면을 판정 | `l2` |
| `extract` | 언어 모델이 이동을 뽑고 지시 표현을 풀며, 코드가 prior를 붙임 | `extract` |
| `plan` | 코드가 후보와 조회 선택지를 만들고, 판정 모델이 장소 후보, 이동에 빠진 시간대·노선, 실행할 조회를 고르며, 언어 모델이 L1과 질문을 씀 | `candidates`, `plan` |
| `execute` | 코드가 조회를 실행 | `execute` |
| `compose` | 코드가 블록 후보를 만들고, 언어 모델이 답과 페이지를 씀 | `compose`, `view` |

모델이 고르는 일과 쓰는 일을 나눈다. 판정 모델(`d3-ultra`)은 코드가 만든 예/아니오 문항에 확률로 답하고, 코드는 기준값 0.5로 고른다.
언어 모델(`gpt-6-luna` high)은 코드가 정한 출력 형식에 맞춰 쓰고, 코드가 검사해 어긋나면 이유를 붙여 다시 쓰게 한다.
모델은 도구를 부르지 않는다. 조회의 종류와 조건은 코드가 선택지로 만들고, 판정 모델이 그중 실행할 것을 고른다.
단계별 규칙은 [ANALYSIS_STAGES](ANALYSIS_STAGES.md)를 따른다.

## 원칙

- 원문에 없는 요구·조건·시간대·방향을 더하지 않는다. 모델이 쓴 지칭과 근거 문장은 원문에 그대로 있어야 한다.
- 민원 번호로 분석 목적·대상·조회·결과 문장을 정하지 않는다. 민원 번호는 입력과 실행의 식별자다.
- 민원에서 조회로 가는 연결은 완벽할 필요가 없다. 문맥으로 확실한 범위에서 지칭을 자료에 잇고(지역 이름, 예전 노선 번호, 지시 표현),
  그렇지 않은 지칭은 조회하지 않는다. 원문이 말하지 않은 출발지·도착지를 진술에서 거꾸로 추정하지 않는다.
- 교통 자료로 볼 수 없는 문제(안내 정보, 환승 규칙)는 조회하지 않고 유형과 L1에만 남긴다. 조회가 없는 민원은 정상 결과다.
- 혼잡은 승객 수 자료가 없어도 승객이 몰리는 노선·시간대의 운행 현황을 조회한다. 답은 운행만 쓰고 혼잡 여부는 말하지 않는다.
- 답은 질문의 대상·조건에서 자료가 보여 주는 운행을 쓴다. 시민의 말이 맞는지 판정하지 않는다.
- 화면에는 자료의 한계나 내부 처리 설명을 쓰지 않는다. 답하지 못한 질문과 살펴보지 않은 이유는 실행 기록에만 둔다.

## 코드 경로

| 경로 | 역할 |
|---|---|
| [`backend/cmd/dllio`](../backend/cmd/dllio/main.go) | 단계 명령 |
| [`backend/problems`](../backend/problems/problems.go) | 유형·측면 판정 |
| [`backend/extract`](../backend/extract/extract.go) | 이동 추출, 지시 표현 풀기, prior |
| [`backend/plan`](../backend/plan/plan.go) | 후보 준비, 장소 판정, 조회 선택지와 판정, 계획 |
| [`backend/execute`](../backend/execute/execute.go) | 조회 실행 |
| [`backend/compose`](../backend/compose/compose.go) | 블록 후보, 답 검사, 뷰어 입력 |
| [`backend/matchi`](../backend/matchi/writer.go) | 판정·생성 호출, 출력 형식과 검사, 호출 기록 |
| [`backend/tools`](../backend/tools/tools.go) | 교통 조회 도구(SQL, 여정, 노선 경로, 구간 소요, 장소 검색) |
| `backend/journey`, `segments`, `places`, `revisions`, `store`, `sqltool` | 교통 계산과 자료 읽기 |
| [`backend/calendar`](../backend/calendar/calendar.go) | 요일·공휴일, 원문의 요일 표현 |
| `backend/jsonx`, `textx`, `repo` | JSON, 글자 규칙, 경로 |
| [`preprocess/src/dllio_preprocess/network`](../preprocess/src/dllio_preprocess/network/) | 교통 자료 전처리와 적재 |

교통 자료와 계산은 [TRANSIT_BACKEND](TRANSIT_BACKEND.md)를 따른다.

## 화면

뷰어는 `main` 브랜치의 `viewer/`에 있다. 뷰어는 `view` 폴더를 읽어 민원 목록, 한줄요약, 원문, L1, 문제 유형과 이동, 답한 질문과 조회 조건,
페이지(표·지도·출발 시각 띠·막대·지표)를 한 창에 보인다.

뷰어는 민원 01~40을 모두 다룬다.

## 배포 범위

지자체의 민원 DB와 교통 backend(운행기록·시간표 시스템)는 공개 시간표·관측 운행·OSM 보행망으로 대신 구현한다.
교통 계산과 저장은 고객 시스템으로 바뀔 수 있는 경계로 둔다.
