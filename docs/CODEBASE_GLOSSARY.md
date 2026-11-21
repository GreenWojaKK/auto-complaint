# 용어집

개념마다 뜻과, 그 개념이 문서·코드·단계 사이 JSON·지시문에서 불리는 이름을 적는다. 이름이 여럿인 개념은 지금 쓰이는 이름을 모두 적는다. 이름을 새로 쓰거나 바꿀 때는 이 문서를 먼저 고친다. 동작은 [ANALYSIS_STAGES](ANALYSIS_STAGES.md), 호출 형식은 [MODEL_RUNTIME](MODEL_RUNTIME.md)이 정한다.

## 민원과 읽기 수준

### 원문
시민이 쓴 민원 전체 글이다. 모든 판정과 생성의 근거다.
- JSON: `source`(view), 원문 hash `sourceHash`(l2)

### 문제 유형 (L2)
여러 민원에 공통으로 적용되는 교통 문제의 종류다. 정의와 경계는 [L2_CATALOG](L2_CATALOG.md)와 `prompts/l2/catalog.json`에 둔다. 한 민원에 여러 유형이 함께 있을 수 있다.
- 코드: `problems.Entry`, 유형 id 문자열
- JSON: `types`(l2, extract), `problems[].type`(extract), `problem`(plan의 칸·todo, 유형 id 문자열), `problemLabel`(view)
- 문제 번호(extract `problems` 목록의 순서)는 `problem_index`다(candidates의 지칭, 이동 연결, plan의 칸).

### 측면
한 유형 안에서 조회 여부가 갈릴 때 나누는 하위 문제다(예: 환승의 연결과 규칙).
- 코드: `problems.Aspect`
- JSON: `aspects`(l2), `noQueryAspects`

### 조회 대상 여부
그 유형·측면을 교통 자료로 조회하는지다. 조회하지 않는 유형은 근거와 L1에만 남는다.
- catalog: `query`(불리언)
- JSON: `noQuery`, `noQueryAspects`(조회하지 않는 유형·측면 목록)
- 헷갈리기 쉬운 것: plan의 `not_checkable`은 조회 대상인 유형인데 실행할 조회가 없는 칸의 목록이다. 조회 실행의 `/query`, SQL 문장과도 이름이 겹친다.

### L1
행정 검토자가 읽는 민원 요약이다. 칸은 한줄요약, 상황, 요청, 문의이며 칸마다의 쓰는 규칙은 계획 지시(`prompts/analysis/plan.md`)와 출력 형식이 정한다.
- 코드: `plan.L1`, 모델 출력은 `plan.L1Draft`
- JSON: `l1.headline`, `l1.situation`, `l1.requests`, `l1.inquiries`(시민의 문의). 요청·문의 항목의 원문 발췌는 `quote`다.

## 추출

### 유형별 불편 요약
유형마다 그 민원이 그 문제를 어떤 처지로 말하는지 적은 한 문장이다. 추출의 첫 호출이 쓰고, 유형별 이동은 이 문장을 범위로 쓴다.
- 문서: 유형별 사정
- 코드: `extract.TypeSummaries`, `extract.TypeSummary`, 지역 변수 `summaries`
- JSON: 모델 출력 `summaries[].summary`, 추출 결과 `problems[].summary`
- 지시문: `prompts/l2/summary.md`
- 유형별 이동과 근거 문장을 받는 출력 형식은 `extract.TypeMoves`다.

### 근거 문장과 원문 발췌
근거 문장은 그 유형의 문제를 말하는 원문 문장이다. 원문 발췌는 지칭이나 항목이 나온 원문 구절이다.
- JSON: 근거 문장 `problems[].grounding`(extract), `grounding`(plan todo). 원문 발췌 `quote`(지칭, 지시 표현 해석), `period_quote`(이동의 시기 근거), `quote`(L1 요청·문의)

### 이동
문제가 걸린 시민의 이동 하나다. 출발지, 도착지, 시간대, 수치, 대상(노선·정류장·장소), 개편 전후 시기로 이루어진다.
- 코드: `extract.Move`
- JSON: `problems[].moves[]`(`about`, `origin`, `destination`, `time`, `values`, `targets`, `period`), 이동 번호 `move`(candidates의 지칭, plan의 칸·todo)
- 헷갈리기 쉬운 것: `plan.MoveLink.Move`는 이동이 아니라 이동에 속한 지칭 목록과 연결 후보의 짝이다.

### 지칭
원문에 쓰인 장소·노선·정류장·시간·수치 표현 하나다.
- 코드: `extract.PlaceMention`, `TimeMention`, `ValueMention`, `TargetMention`
- JSON: candidates `mentions[]`(`id` m1…, `text`, `role`, `quote`, `move`, `problem_index`)
- 지칭 id(m1)와 후보 id(m1.c1)는 다른 id 공간이다. 지칭 id를 담는 칸은 이름이 `_mentions`로, 후보 id를 담는 칸은 `_candidates`로 끝난다.

### 지시 표현
"여기", "우리 동네"처럼 원문 안의 다른 곳을 가리키는 장소 지칭이다. 추출이 가리키는 원문 지칭을 한 번 더 뽑는다.
- 코드: `extract.Referent`, `extract.Deictic`
- JSON: `referents`(extract)

### 지역 사전 값
지칭에 붙이는 사전 정보다. 장소 지칭에는 행정동 묶음, 시간 표현에는 시간대 범위, 노선에는 이전 번호를 붙인다.
- 코드: `extract.Priors`, `Place.Prior`, `Target.Prior`
- JSON: `priors`(extract의 사전 hash), 시간 지칭의 `range`(사전 항목 객체 `{key, ranges, basis}`)
- 헷갈리기 쉬운 것: plan·compose 입력의 `prior`는 사전 값이 아니라 지역 폴더 `prior.md`의 지역 설명 글이다.

## 계획

### 후보
지칭이 가리킬 수 있는 교통 자료의 대상이다. 종류는 장소(정류장 묶음), 지역(행정동), 구·군, 노선 등이다.
- JSON: candidates `mentions[].candidates[]`(`id` m1.c1…, `kind`, `name`, `stop_counts`(자료 판별 정류장 수), `stop_ids`, `fit`), 판정에서 떨어진 후보 `rejected`
- 헷갈리기 쉬운 것: compose의 블록 후보와 이름이 같다. 실행 단계의 `places` 맵은 장소가 아니라 후보를 후보 id로 찾는 맵이다.

### 장소와 정류장 묶음
장소는 원문 지칭을 정류장에 잇는 대상(별칭, 소속 정류장, 노선)이다. 정류장 묶음은 이름이 같아 서로 이어지는 정류장들이며 여정 탐색의 승차 후보 표기에 쓴다.
- 장소: `places.Place`, 표 `transit.place`
- 정류장 묶음: `journey.Place`, 표 `transit.network_place`, 열 `stop.network_place`(묶음 번호)

### 칸
문제별 이동 하나에서 조회 조건을 모으는 단위다.
- JSON: plan `slots[]`(`id` s1…, `mentions`, `move`, `problem`(유형 id), `problem_index`(extract 문제 번호), `period`)

### 조회 선택지
코드가 이동의 지칭으로 만든, 조건까지 정해진 조회 하나다. 판정 모델이 실행할지 정한다.
- 코드: `plan.Built.Options`
- JSON: plan `options[]`(`id` q1…, `method`(조회 방법), `routes`, `boarding_mentions`·`origin_mentions`·`destination_mentions`·`map_mentions`(지칭 id), `window`, `gap_over`, `gap_columns`, `fit`(실행 판정의 예 확률), `slots`)
- 헷갈리기 쉬운 것: `fitOptions`는 조회 선택지가 아니라 판정의 예/아니오 보기다.

### todo (계획의 질문 항목)
실행할 조회 하나에 대해 행정 검토자가 자료에서 볼 질문이다. 언어 모델이 질문을 쓰고, 코드가 todo로 푼다.
- 문서: todo, 확인할 일. 뷰어: 확인 계획
- 코드: 모델 출력 `plan.PlanChoice`, 항목 `plan.Check`
- JSON: 모델 출력 `choice.checks[]`(`option`, `kind`(현황·요구 검토), `question`, `done_when`, `window`), 확정 `plan.todos[]`(`id` t1…, `option`, `method`(조회 조건 묶음; 장소는 `boarding_candidates`·`origin_candidates`·`destination_candidates`의 후보 id, 지도 표시는 `map_mentions`의 지칭 id), `question`, `done_when`, `kind`, `problem`, `move`, `period`, `compare_periods`, `grounding`)
- 헷갈리기 쉬운 것: 코드의 `Check`는 이 항목이고, `matchi.Spec.Check`·`IssuesCheck`는 출력을 검사하는 함수다. plan 기록의 `plan` 키 안에 다시 `plan`(L1과 todo)이 있다.

### 판정 기록
판정 모델을 부른 기록이다. 모델 이름, 기준값, 요청과 응답을 담으며 판정 결과 자체는 후보·선택지·연결의 필드에 있다.
- JSON: `placeDecisions`, `mapDecisions`, `linkDecisions`, `routeDecisions`(candidates), `fitDecisions`(plan)
- 헷갈리기 쉬운 것: compose의 `decisions`는 언어 모델이 쓴 판단 메모다.

### 예 확률
판정 문항의 '예' 확률이다. 코드는 기준값 이상이면 고른다.
- JSON: `yes`(l2, 유형별 확률 맵), `probability`(view), `fit`·`areaFit`(후보), `fit`(조회 선택지)
- 헷갈리기 쉬운 것: catalog의 `fit`은 확률이 아니라 조회 판정 문항에 넣는 유형 명제 글이다.

## 시간과 시기

### 시간대
조회할 하루 중의 시간 범위다.
- JSON: 추출의 시간 지칭 `time[]`, 조회 선택지의 `window`(`start`, `end`, `arrive_by`, `ask`, `day`, `basis`)
- 코드: `plan.TimeContexts`(조회 시간대 목록), 모델이 정한 시간대 `plan.Range`

### 개편 전후 시기
이동이나 조회가 개편 전, 개편 후, 개편 전후 비교 중 무엇의 이야기인지다.
- JSON: `period`(`before`·`after`·`compare`·`none`; 추출 이동, 칸, 선택지, todo), `compare_periods`(todo), `before_routes`(개편 전 노선)

### 자료 판
교통 자료의 한 판(개편 전, 개편 후)이다. 판마다 기간, 관측일, 근거 종류가 있다.
- 문서: 판, 자료 판
- 코드·JSON: `revision`(candidates, execute, 표 `transit.revision`), `dataset`(조회 도구 입력), `Snapshot`(`journey.Network`), `Datasets`(`places.Place`), 판 표기 `revisions.Choice`
- 헷갈리기 쉬운 것: 개편 전후 구분이 `revision`과 `period` 두 이름으로 나뉜다. execute의 `network`는 연결망이 아니라 자료 출처 문자열이다.

### 기준 수치
시민이 말한 분·시간 수치에서 뽑아, 그 간격을 넘는 운행 간격을 세는 기준이다.
- JSON: `gap_over`(민원 전체의 기준 수치), `gap_columns`(그 이동의 기준 수치), 조회 결과의 `over_N` 열

## 조회 실행과 답

### 실행 디렉터리
분석 한 번의 단계 결과를 담는 폴더다(`--out`, `--run`).
- 코드: `Config.Run`
- 헷갈리기 쉬운 것: execute의 `runs`는 todo 하나를 자료 판마다 실행한 조회 묶음이다.

### 조회 실행 기록
todo 하나를 자료 판마다 실행한 기록과 요청이다.
- JSON: execute `todos[].runs[]`(`revision`, `routes`, `stop_counts`(역할별 정류장 수), `place`, `requests`), 요청의 조회 이름 `requests[].function`
- 헷갈리기 쉬운 것: `function`은 DB 함수 이름만이 아니라 실행 단계가 붙인 조회 이름(gaps, sequence 등)도 담는다. plan todo의 `method`는 조건 묶음이고, execute todo의 `method`는 조회 방법 문자열이다.

### 근거
조회 하나의 입력, 값, 출처다. 답 속 수치와 블록은 근거 경로로 근거에 이어진다.
- JSON: execute `evidence`(`e_ID` → `input`, `data`, `provenance`), 근거 경로 `e_ID/data/rows/0/열`, 답의 `evidence_values[]`(`ref`, `text`), 블록의 `evidence_refs`
- 추출 이동의 `values`는 시민이 말한 수치이고, 답의 `evidence_values`는 조회 값에서 고른 수치다.

### 운행 방문
운행 하나가 정류장에 한 번 들른 것이다.
- 표·함수: `transit.visit`(행), `transit.pass`(이름을 붙인 뷰), `calls`(시각 범위로 거른 함수 결과)
- 헷갈리기 쉬운 것: 실행 단계의 `placePasses`의 통과는 여러 방문을 묶은 장소 통과 한 번이다.

### 블록 후보
조회 방법마다 정해진 모양으로 코드가 근거 값에서 만든 표·차트·지도다. 언어 모델이 페이지에 보일 것을 고른다.
- JSON: compose `candidates`(todo id → 블록 목록, `id` t1.b1…, `kind`, `title`, `payload`, `evidence_refs`)
- 헷갈리기 쉬운 것: candidates 단계의 후보, compose 입력의 `Inputs.Candidates`(candidates 단계 결과)와 이름이 같다.

### 답과 페이지
todo마다 상태(답함, 확인할 수 없음)와 답 문장, 화면 페이지마다 보일 todo·블록·제목이다.
- JSON: compose `composition.answers[]`(`todo`, `status`, `text`, `evidence_values`), `composition.pages[]`(`title`, `todos`, `blocks`)
