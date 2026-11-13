package compose

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"dllio/backend/jsonx"
	"dllio/backend/plan"
)

// Methods는 조회의 화면 이름이다.
var Methods = map[string]string{"departures": "정류장 출발", "route_departures": "노선 출발", "journeys": "여정", "segments": "구간 소요", "map": "노선 경로"}

// roles는 이동의 칸과 그 화면 이름이다.
var roles = [][2]string{{"origin", "출발지"}, {"destination", "도착지"}, {"time", "시간대"}, {"values", "수치"}, {"targets", "대상"}}

// Tags는 화면에 보이는 유형 이름이다.
var Tags = map[string]string{"frequency_insufficiency": "운행 횟수·배차 간격", "temporal_fit_gap": "필요한 시각의 운행", "spatial_coverage_gap": "정차·노선이 없는 곳",
	"direct_connectivity_deficit": "직접 가는 노선", "journey_burden": "이동 시간·경로 부담", "schedule_unreliability": "예정과 다른 운행",
	"capacity_shortfall": "차내 혼잡", "information_defect": "운행 안내 정보", "transfer_dependency_gap": "환승"}

func tag(kind string) string {
	if name, ok := Tags[kind]; ok {
		return name
	}
	return kind
}

// Binding은 값 안의 {"$ref": "e_ID/data/…"}를 근거의 값으로 바꾼다. prefix·suffix가 있으면 글로 잇는다.
func Binding(v any, evidence J) (any, error) {
	switch x := v.(type) {
	case J:
		if ref, ok := x["$ref"].(string); ok {
			parts := strings.Split(ref, "/")
			var current any = evidence[parts[0]]
			if current == nil {
				return nil, fmt.Errorf("%s", parts[0])
			}
			for _, p := range parts[1:] {
				switch c := current.(type) {
				case []any:
					i, err := strconv.Atoi(p)
					if err != nil {
						return nil, err
					}
					if i < 0 {
						i += len(c)
					}
					if i < 0 || i >= len(c) {
						return nil, fmt.Errorf("list index out of range")
					}
					current = c[i]
				case J:
					next, ok := c[p]
					if !ok {
						return nil, fmt.Errorf("%s", p)
					}
					current = next
				default:
					return nil, fmt.Errorf("%s", p)
				}
			}
			_, hasPrefix := x["prefix"]
			_, hasSuffix := x["suffix"]
			if hasPrefix || hasSuffix {
				return jsonx.Str(x["prefix"]) + value(current) + jsonx.Str(x["suffix"]), nil
			}
			return current, nil
		}
		out := J{}
		for k, item := range x {
			bound, err := Binding(item, evidence)
			if err != nil {
				return nil, err
			}
			out[k] = bound
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			bound, err := Binding(item, evidence)
			if err != nil {
				return nil, err
			}
			out[i] = bound
		}
		return out, nil
	}
	return v, nil
}

func ids(v any) []string {
	switch x := v.(type) {
	case string:
		if x == "" {
			return nil
		}
		return []string{x}
	case nil:
		return nil
	case []string:
		return x
	}
	return jsonx.Strs(v)
}

func byID(list []any) map[string]J {
	out := map[string]J{}
	for _, x := range list {
		out[jsonx.Str(jsonx.Obj(x)["id"])] = jsonx.Obj(x)
	}
	return out
}

func head(id string) string {
	before, _, _ := strings.Cut(id, ".")
	return before
}

func candidateText(cid string, mentions map[string]J, revision string) string {
	mention := mentions[head(cid)]
	prefix := "«?» → "
	if mention != nil {
		prefix = plan.MentionLabel(mention) + " → "
	}
	var cand J
	for _, c := range jsonx.List(mention["candidates"]) {
		if jsonx.Str(jsonx.Obj(c)["id"]) == cid {
			cand = jsonx.Obj(c)
			break
		}
	}
	if cand == nil {
		return prefix + cid
	}
	if jsonx.Str(cand["kind"]) == "nearby" {
		out := prefix + jsonx.Str(cand["basis"]) + ": " + strings.Join(jsonx.Strs(cand["places"]), "·") + " 주변"
		if stops, ok := jsonx.Obj(cand["stops"])[revision]; ok && stops != nil {
			out += " (정류장 " + pyText(stops) + "곳)"
		}
		return out
	}
	if jsonx.Str(cand["kind"]) == "area" {
		areas := []string{}
		for _, a := range jsonx.List(cand["areas"]) {
			areas = append(areas, jsonx.Str(jsonx.Obj(a)["area"]))
		}
		out := prefix + jsonx.Str(cand["basis"]) + ": " + strings.Join(areas, "·")
		if stops, ok := jsonx.Obj(cand["stops"])[revision]; ok && stops != nil {
			out += " (정류장 " + pyText(stops) + "곳)"
		}
		return out
	}
	return prefix + pyText(cand["name"]) + "(" + pyText(cand["area"]) + ")"
}

// Conditions는 todo의 조회 조건을 [이름, 값] 줄로 쓴다. 모델 입력과 뷰어가 함께 쓴다.
func Conditions(method J, mentions map[string]J, revision string) [][]string {
	kind := jsonx.Str(method["method"])
	rows := [][]string{{"조회", Methods[kind]}}
	if routes := jsonx.Strs(method["routes"]); len(routes) > 0 {
		rows = append(rows, []string{"노선", strings.Join(routes, ", ")})
	} else if kind == "departures" {
		rows = append(rows, []string{"노선", "정류장에 서는 모든 노선"})
	}
	if before := jsonx.Strs(method["before_routes"]); len(before) > 0 {
		rows = append(rows, []string{"개편 전 노선", strings.Join(before, ", ")})
	}
	for _, f := range [][2]string{{"boarding", "승차"}, {"origin", "출발"}, {"destination", "도착"}} {
		if list := ids(method[f[0]]); len(list) > 0 {
			texts := []string{}
			for _, c := range list {
				texts = append(texts, candidateText(c, mentions, revision))
			}
			label := f[1]
			if f[0] == "boarding" && method["at_destination"] == true {
				label = "도착"
			}
			rows = append(rows, []string{label, strings.Join(texts, " / ")})
		}
	}
	if kind == "departures" {
		text := "통과 시각"
		if jsonx.Str(method["time_kind"]) == "departure" {
			text = "출발 간격"
		}
		if method["direct"] == true {
			text += ", 목적지 전 같은 정류장 재통과 제외"
		}
		rows = append(rows, []string{"시각", text})
	}
	if columns := jsonx.Ints(method["gap_columns"]); len(columns) > 0 {
		texts := []string{}
		for _, n := range columns {
			texts = append(texts, fmt.Sprintf("%d분", n))
		}
		rows = append(rows, []string{"기준 수치", strings.Join(texts, ", ")})
	}
	if kind == "journeys" {
		basis := "출발 기준"
		if method["arrive_by"] == true {
			basis = "도착 기한"
		}
		rows = append(rows, []string{"여정 조건", fmt.Sprintf("%s, 환승 %s회 이하, 총 도보 %sm 이하", basis, pyText(method["transfers"]), pyText(method["walk_meters"]))})
	}
	if kind == "map" && truthy(method["stops"]) {
		texts := []string{}
		for _, s := range jsonx.Strs(method["stops"]) {
			if m, ok := mentions[s]; ok {
				texts = append(texts, plan.MentionLabel(m))
			}
		}
		rows = append(rows, []string{"지도 표시", strings.Join(texts, ", ")})
	}
	if w := jsonx.Obj(method["window"]); len(w) > 0 {
		basis := jsonx.Str(w["basis"])
		if m, ok := mentions[jsonx.Str(w["mention"])]; ok && w["mention"] != nil {
			basis += ", «" + jsonx.Str(m["text"]) + "»"
		}
		rows = append(rows, []string{"시간대", fmt.Sprintf("%s–%s (%s) %s", jsonx.Str(w["start"]), jsonx.Str(w["end"]), basis, jsonx.Str(w["reason"]))})
		if days := jsonx.Strs(w["day"]); len(days) > 0 {
			rows = append(rows, []string{"요일", strings.Join(days, "·")})
		}
	}
	return rows
}

// RouteLabel은 조회의 출발·도착 지칭('염포동 → 삼산')이다.
func RouteLabel(method J, mentions map[string]J) string {
	names := func(key string) string {
		texts := []string{}
		for _, i := range ids(method[key]) {
			texts = append(texts, plan.SearchText(mentions[head(i)]))
		}
		return strings.Join(texts, "·")
	}
	start, end := names("boarding"), names("destination")
	if start == "" {
		start = names("origin")
	}
	switch {
	case start != "" && end != "":
		return start + " → " + end
	case start != "":
		return start
	}
	return end
}

// periodNames는 이동과 노선의 개편 표시를 화면에 쓰는 이름이다.
var periodNames = map[string]string{"before": "개편 전", "after": "개편 후", "compare": "개편 전후"}

// mentionText는 추출한 지칭과 그에 붙은 가리키는 곳·prior·노선 계보·적힌 시각이다.
func mentionText(m J) string {
	text := jsonx.Str(m["text"])
	if label := periodNames[jsonx.Str(m["period"])]; label != "" {
		text += " (" + label + ")"
	}
	if refers := jsonx.Obj(m["refersTo"]); jsonx.Str(refers["text"]) != "" {
		text += " → " + jsonx.Str(refers["text"])
		m = refers
	}
	if prior := jsonx.Obj(m["prior"]); len(prior) > 0 {
		areas := []string{}
		for _, a := range jsonx.List(prior["areas"]) {
			areas = append(areas, jsonx.Str(jsonx.Obj(a)["area"]))
		}
		text += " → " + strings.Join(areas, "·")
	}
	for _, x := range jsonx.List(m["routes"]) {
		r := jsonx.Obj(x)
		links := []string{}
		if previous := jsonx.Strs(r["previous"]); len(previous) > 0 {
			links = append(links, "이전 "+strings.Join(previous, ","))
		}
		if later := jsonx.Strs(r["later"]); len(later) > 0 {
			links = append(links, "이어받은 "+strings.Join(later, ","))
		}
		text += " [" + jsonx.Str(r["number"])
		if len(links) > 0 {
			text += " · " + strings.Join(links, " · ")
		}
		text += "]"
	}
	if written := jsonx.Strs(m["written"]); len(written) > 0 {
		text += " [" + strings.Join(written, ", ") + "]"
	}
	return text
}

func shownRows(table any) []any {
	rows := []any{}
	for _, row := range jsonx.List(table) {
		cells := []any{}
		for _, c := range jsonx.List(row) {
			if c == nil {
				cells = append(cells, "—")
			} else {
				cells = append(cells, pyText(c))
			}
		}
		rows = append(rows, cells)
	}
	return rows
}

func shownBlock(b, evidence J) (J, error) {
	bound, err := Binding(b["payload"], evidence)
	if err != nil {
		return nil, err
	}
	payload := jsonx.Obj(bound)
	switch jsonx.Str(b["kind"]) {
	case "table":
		payload["rows"] = shownRows(payload["rows"])
		if alternate := jsonx.Obj(payload["alternate"]); len(alternate) > 0 {
			inner := jsonx.Obj(alternate["payload"])
			inner["rows"] = shownRows(inner["rows"])
		}
	case "metrics":
		metrics := []any{}
		for _, x := range jsonx.List(payload["metrics"]) {
			m := J{}
			for k, v := range jsonx.Obj(x) {
				m[k] = v
			}
			m["value"] = pyText(m["value"])
			metrics = append(metrics, m)
		}
		payload["metrics"] = metrics
	}
	return J{"title": b["title"], "kind": b["kind"], "payload": payload}, nil
}

// ProblemLabel은 유형의 화면 이름이다. 측면을 고른 유형은 고른 측면의 이름이다.
func ProblemLabel(kind string, l2 J) string {
	names := []string{}
	for _, x := range jsonx.List(jsonx.Obj(l2["aspects"])[kind]) {
		if a := jsonx.Obj(x); a["chosen"] == true {
			names = append(names, jsonx.Str(a["label"]))
		}
	}
	if len(names) > 0 {
		return strings.Join(names, "·")
	}
	return tag(kind)
}

// ViewInput은 뷰어 입력을 만드는 데 쓰는 앞 단계 결과다.
type ViewInput struct {
	Case, Source, Received  string
	L2, Extract, Candidates J
	Plan                    J
	Composition             J
	Blocks                  map[string][]J
	Evidence                J
	Model                   any
}

// View는 뷰어 입력이다. 확인할 수 없음으로 답한 todo는 보이지 않는다.
func View(in ViewInput) (J, error) {
	plan := jsonx.Obj(in.Plan["plan"])
	revision := jsonx.Str(in.Candidates["revision"])
	mentions := byID(jsonx.List(in.Candidates["mentions"]))
	answers := map[string]J{}
	for _, a := range jsonx.List(in.Composition["answers"]) {
		answers[jsonx.Str(jsonx.Obj(a)["todo"])] = jsonx.Obj(a)
	}
	blocks := map[string]J{}
	for _, list := range in.Blocks {
		for _, b := range list {
			blocks[jsonx.Str(b["id"])] = b
		}
	}
	pages := jsonx.List(in.Composition["pages"])
	problems := []any{}
	for _, x := range jsonx.List(in.Extract["problems"]) {
		p := jsonx.Obj(x)
		kind := jsonx.Str(p["type"])
		moves := []any{}
		for _, y := range jsonx.List(p["moves"]) {
			mv := jsonx.Obj(y)
			shown := J{}
			for _, r := range roles {
				texts := []any{}
				for _, item := range jsonx.List(mv[r[0]]) {
					texts = append(texts, mentionText(jsonx.Obj(item)))
				}
				shown[r[1]] = texts
			}
			if label := periodNames[jsonx.Str(mv["period"])]; label != "" {
				shown["개편"] = []any{label}
			}
			moves = append(moves, shown)
		}
		problems = append(problems, J{"type": kind, "label": ProblemLabel(kind, in.L2), "probability": jsonx.Obj(in.L2["yes"])[kind], "grounding": p["grounding"], "moves": moves})
	}
	todos := []any{}
	for _, x := range jsonx.List(plan["todos"]) {
		t := jsonx.Obj(x)
		id := jsonx.Str(t["id"])
		answer := answers[id]
		if answer["status"] == "확인할 수 없음" {
			continue
		}
		onPages := []any{}
		for i, p := range pages {
			for _, todo := range jsonx.Strs(jsonx.Obj(p)["todos"]) {
				if todo == id {
					onPages = append(onPages, i)
					break
				}
			}
		}
		todos = append(todos, J{"id": id, "kind": t["kind"], "problem": t["problem"], "problemLabel": tag(jsonx.Str(t["problem"])), "move": t["move"],
			"question": t["question"], "conditions": Conditions(jsonx.Obj(t["method"]), mentions, revision), "comparePeriods": t["compare_periods"],
			"doneWhen": t["done_when"], "status": answer["status"], "answer": answer["text"], "pages": onPages})
	}
	shownPages := []any{}
	for _, x := range pages {
		p := jsonx.Obj(x)
		shown, seen := []any{}, map[string]bool{}
		for _, b := range jsonx.Strs(p["blocks"]) {
			if found, ok := blocks[b]; ok {
				s, err := shownBlock(found, in.Evidence)
				if err != nil {
					return nil, err
				}
				// 같은 값을 되풀이하는 블록(같은 종류·제목·내용)은 페이지에 한 번만 보인다.
				key, err := jsonx.Marshal(s)
				if err != nil {
					return nil, err
				}
				if seen[string(key)] {
					continue
				}
				seen[string(key)] = true
				shown = append(shown, s)
			}
		}
		shownPages = append(shownPages, J{"title": p["title"], "layout": "stack", "todos": p["todos"], "blocks": shown})
	}
	return J{"caseId": in.Case, "createdAt": time.Now().Format(time.RFC3339Nano), "received": in.Received, "revision": Revisions[revision],
		"models": J{"plan": in.Plan["model"], "compose": in.Model}, "source": in.Source, "l1": plan["l1"], "problems": problems, "todos": todos, "pages": shownPages}, nil
}
