package compose

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"dllio/backend/execute"
	"dllio/backend/jsonx"
)

// 조회 결과로 todo마다 화면 블록 후보를 만든다. 모델은 이 후보 중에서 페이지에 둘 블록을 고른다.
//
// 후보는 조회 방법마다 정해진 모양이다. 값은 근거에서 그대로 옮기고, 후보마다 출처 근거 ID와 무엇을 보이는지 한 줄 설명을
// 붙인다. 개편 전후를 함께 조회한 todo는 두 판을 한 표·차트에 나란히 놓는다.
//   - 정류장 출발: 도착지까지 가는 노선 비교 표, 계열별 편수·간격 표, 출발 시각 띠, 계열별 편수 막대
//   - 노선 출발: 노선별 편수·간격 표, 노선 출발 시각 띠, 노선별 편수 막대
//   - 노선 경로: 판마다 지도, 대표 운행 표(기점·종점·정류장 수), 대표 운행 정류장 순서, 원문 정류장을 지나는지,
//     개편 전에만·후에만 지나는 정류장
//   - 여정: 기준 시각별 가장 빨리 닿는 여정과 직행 여정 표, 직행 여정이 있는 기준 시각 수
//   - 구간 소요: 견적 합·실제 운행 기록의 소요

// J는 적힌 그대로 읽은 JSON 객체다.
type J = jsonx.J

// Revisions는 자료 판의 화면 이름이다.
var Revisions = map[string]string{"before": "개편 전", "after": "개편 후"}

// pyText는 값을 Python의 str()처럼 쓴다. 근거의 숫자는 적힌 글자 그대로다.
func pyText(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		if x {
			return "True"
		}
		return "False"
	}
	return fmt.Sprint(v)
}

// value는 빈 값을 —로 쓴다.
func value(v any) string {
	if v == nil {
		return "—"
	}
	return pyText(v)
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case json.Number:
		return jsonx.Num(x) != 0
	case []any:
		return len(x) > 0
	case J:
		return len(x) > 0
	}
	return true
}

func requests(run J) []J {
	out := []J{}
	for _, q := range jsonx.List(run["requests"]) {
		out = append(out, jsonx.Obj(q))
	}
	return out
}

func runs(executed J) []J {
	out := []J{}
	for _, r := range jsonx.List(executed["runs"]) {
		out = append(out, jsonx.Obj(r))
	}
	return out
}

func functionRows(run J, function string, evidence J) ([]J, []string) {
	rows, ids := []J{}, []string{}
	for _, q := range requests(run) {
		if jsonx.Str(q["function"]) == function {
			for _, r := range jsonx.List(jsonx.Obj(jsonx.Obj(evidence[jsonx.Str(q["evidence"])])["data"])["rows"]) {
				rows = append(rows, jsonx.Obj(r))
			}
			ids = append(ids, jsonx.Str(q["evidence"]))
		}
	}
	return rows, ids
}

func revisionPrefix(compare bool, run J) []any {
	if compare {
		return []any{Revisions[jsonx.Str(run["revision"])]}
	}
	return []any{}
}

// seriesTable은 정류장 출발(calls_summary)·노선 출발(route_starts)의 표다. 기준 간격이 있으면 그 간격을 넘는 수를 붙인다.
func seriesTable(todo, executed, evidence J, function string) (J, []string) {
	m := jsonx.Obj(todo["method"])
	all := runs(executed)
	compare := len(all) > 1
	gapColumns := jsonx.Ints(m["gap_columns"])
	hasDestination := function == "calls_summary" && truthy(m["destination"])
	columns := []any{}
	if compare {
		columns = append(columns, "자료")
	}
	if function == "calls_summary" {
		columns = append(columns, "노선", "정류장", "방면")
	} else {
		columns = append(columns, "노선", "출발 정류장")
	}
	columns = append(columns, "운행수")
	if strings.HasPrefix(function, "route_starts") {
		columns = append(columns, "변형")
	}
	columns = append(columns, "평균 간격(분)", "최대 간격(분)", "첫차–막차")
	if hasDestination {
		columns = append(columns, "목적지 도달", "도달 소요 중앙값(분)")
	}
	if len(gapColumns) > 0 {
		columns = append(columns, "간격 수")
		for _, n := range gapColumns {
			columns = append(columns, fmt.Sprintf("%d분 초과", n))
		}
	}
	rows, refs := []any{}, []string{}
	for _, run := range all {
		series, ids := functionRows(run, function, evidence)
		gaps, gapIDs := functionRows(run, "gaps", evidence)
		refs = append(append(refs, ids...), gapIDs...)
		bySeries := map[string]J{}
		for _, g := range gaps {
			bySeries[pyText(g["route_number"])+"\x00"+pyText(g["stop_id"])] = g
		}
		for _, r := range series {
			row := append(revisionPrefix(compare, run), r["route_number"], r["stop_name"])
			count := r["departures"]
			if function == "calls_summary" {
				row = append(row, value(r["headings"]))
				count = r["passes"]
			}
			row = append(row, value(count))
			if strings.HasPrefix(function, "route_starts") {
				row = append(row, value(r["variants"]))
			}
			row = append(row, value(r["mean_gap_min"]), value(r["max_gap_min"]), value(r["first_time"])+"–"+value(r["last_time"]))
			if hasDestination {
				row = append(row, value(r["reached_destination"]), value(r["median_minutes_to_destination"]))
			}
			if len(gapColumns) > 0 {
				g := bySeries[pyText(r["route_number"])+"\x00"+pyText(r["stop_id"])]
				row = append(row, value(g["gaps"]))
				for _, n := range gapColumns {
					row = append(row, value(g[fmt.Sprintf("over_%d", n)]))
				}
			}
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		return nil, refs
	}
	if len(rows) > maxTableRows {
		countAt := 2
		if function == "calls_summary" {
			countAt = 3
		}
		rows = cappedRows(rows, compare, countAt, len(columns))
	}
	return J{"columns": columns, "rows": rows}, refs
}

// maxTableRows는 표 후보의 최대 줄 수다.
const maxTableRows = 40

// cappedRows는 줄이 많은 표에서 운행수가 많은 노선만 남기고, 나머지는 판마다 합계 한 줄로 묶는다. 판별 비교가 되도록 같은 노선은 두 판에 함께 남긴다.
func cappedRows(rows []any, compare bool, countAt, width int) []any {
	head := 0
	if compare {
		head = 1
	}
	groups, order := map[string][]any{}, []string{}
	counts := map[string]int{}
	for _, x := range rows {
		row := x.([]any)
		group := ""
		if compare {
			group = pyText(row[0])
		}
		if _, ok := groups[group]; !ok {
			order = append(order, group)
		}
		groups[group] = append(groups[group], row)
		n, _ := strconv.Atoi(pyText(row[head+countAt]))
		counts[pyText(row[head])] += n
	}
	routes := make([]string, 0, len(counts))
	for r := range counts {
		routes = append(routes, r)
	}
	sort.Slice(routes, func(i, j int) bool {
		if counts[routes[i]] != counts[routes[j]] {
			return counts[routes[i]] > counts[routes[j]]
		}
		return routes[i] < routes[j]
	})
	keep := map[string]bool{}
	for _, r := range routes[:min(len(routes), maxTableRows/len(order)-1)] {
		keep[r] = true
	}
	out := []any{}
	for _, group := range order {
		others, total := map[string]bool{}, 0
		for _, x := range groups[group] {
			row := x.([]any)
			if keep[pyText(row[head])] {
				out = append(out, row)
				continue
			}
			others[pyText(row[head])] = true
			n, _ := strconv.Atoi(pyText(row[head+countAt]))
			total += n
		}
		if len(others) > 0 {
			rest := make([]any, width)
			for i := range rest {
				rest[i] = "—"
			}
			if compare {
				rest[0] = group
			}
			rest[head] = fmt.Sprintf("그 외 %d개 노선", len(others))
			rest[head+countAt] = strconv.Itoa(total)
			out = append(out, rest)
		}
	}
	return out
}

func label(parts ...any) string {
	texts := []string{}
	for _, p := range parts {
		texts = append(texts, pyText(p))
	}
	return strings.Join(texts, " · ")
}

func stripPayload(todo, executed, evidence J, function string) (J, []string) {
	w := jsonx.Obj(jsonx.Obj(todo["method"])["window"])
	if w == nil {
		w = J{"start": "00:00", "end": "24:00"}
	}
	all := runs(executed)
	compare := len(all) > 1
	rows, refs := []any{}, []string{}
	for _, run := range all {
		series, ids := functionRows(run, function, evidence)
		refs = append(refs, ids...)
		for _, r := range series {
			parts := append(revisionPrefix(compare, run), r["route_number"], r["stop_name"])
			if truthy(r["headings"]) {
				parts = append(parts, r["headings"])
			}
			times := []any{}
			for _, t := range strings.Split(jsonx.Str(r["times"]), " · ") {
				if t != "" {
					times = append(times, t)
				}
			}
			rows = append(rows, J{"label": label(parts...), "times": times})
		}
	}
	if len(rows) == 0 || len(rows) > 16 {
		return nil, refs
	}
	end := jsonx.Str(w["end"])
	if end > "24:00" {
		end = "24:00"
	}
	return J{"strip": J{"start": w["start"], "end": end, "markers": []any{}, "rows": rows}}, refs
}

func barsPayload(executed, evidence J, function string) (J, []string) {
	all := runs(executed)
	compare := len(all) > 1
	rows, refs := []any{}, []string{}
	for _, run := range all {
		series, ids := functionRows(run, function, evidence)
		refs = append(refs, ids...)
		for _, r := range series {
			count := r["departures"]
			if function == "calls_summary" {
				count = r["passes"]
			}
			rows = append(rows, J{"label": label(append(revisionPrefix(compare, run), r["route_number"], r["stop_name"])...), "values": []any{count}})
		}
	}
	if len(rows) < 2 || len(rows) > 24 {
		return nil, refs
	}
	return J{"bars": J{"unit": "편", "series": []any{J{"label": "편수", "nullLabel": "없음"}}, "rows": rows, "note": ""}}, refs
}

// compareTable은 목적지까지 가는 노선을 나란히 놓은 표다.
func compareTable(executed, evidence J) (J, []string) {
	all := runs(executed)
	compare := len(all) > 1
	rows, refs := []any{}, []string{}
	for _, run := range all {
		found, ids := functionRows(run, "route_compare", evidence)
		refs = append(refs, ids...)
		for _, r := range found {
			rows = append(rows, append(revisionPrefix(compare, run), r["route_number"], r["fare"], value(r["boarding"]), value(r["trips"]), value(r["mean_gap_min"]),
				value(r["max_gap_min"]), value(r["first_time"])+"–"+value(r["last_time"]), value(r["median_minutes"]), value(r["min_minutes"])+"–"+value(r["max_minutes"])))
		}
	}
	if len(rows) == 0 {
		return nil, refs
	}
	head := []any{}
	if compare {
		head = append(head, "자료")
	}
	return J{"columns": append(head, "노선", "요금", "가장 가까운 승차 정류장", "운행 수", "평균 간격(분)", "최대 간격(분)", "첫차–막차", "소요 중앙값(분)", "소요 범위(분)"), "rows": rows}, refs
}

func block(kind, title, description string, payload any, refs []string) J {
	return J{"kind": kind, "title": title, "description": description, "payload": payload, "evidence_refs": refs}
}

func mapBlocks(executed, evidence J) []J {
	out := []J{}
	all := runs(executed)
	compare := len(all) > 1
	var sequenceRows, orderRows, mentionRows []any
	refs := []string{}
	for _, run := range all {
		name := Revisions[jsonx.Str(run["revision"])]
		prefix := []any{}
		if compare {
			prefix = []any{name}
		}
		for _, q := range requests(run) {
			if jsonx.Str(q["endpoint"]) != "/query" {
				continue
			}
			data := jsonx.Obj(jsonx.Obj(evidence[jsonx.Str(q["evidence"])])["data"])
			if truthy(jsonx.Obj(data["map"])["lines"]) {
				title := "노선 경로"
				if compare {
					title = name + " 노선 경로"
				}
				out = append(out, block("map", title, name+" 자료에서 노선이 실제로 지난 정류장 순서의 선과 원문 정류장 위치", data["map"], []string{jsonx.Str(q["evidence"])}))
			}
		}
		rows, ids := functionRows(run, "sequence", evidence)
		refs = append(refs, ids...)
		for _, r := range rows {
			sequenceRows = append(sequenceRows, append(append([]any{}, prefix...), r["route_number"], r["first_stop"], r["last_stop"], pyText(r["stops"])))
			orderRows = append(orderRows, append(append([]any{}, prefix...), r["route_number"], r["sequence"]))
		}
		rows, ids = functionRows(run, "mentions_on_route", evidence)
		refs = append(refs, ids...)
		for _, r := range rows {
			stops := "지나지 않음"
			if list := jsonx.Strs(r["stops"]); len(list) > 0 {
				stops = strings.Join(list, ", ")
			}
			mentionRows = append(mentionRows, append(append([]any{}, prefix...), r["route_number"], r["mention"], stops))
		}
	}
	head := []any{}
	if compare {
		head = append(head, "자료")
	}
	if len(sequenceRows) > 0 {
		out = append(out, block("table", "대표 운행의 기점·종점", "노선마다 정류장 방문이 가장 많은 온전한 운행 기록의 기점·종점·정류장 수",
			J{"columns": append(append([]any{}, head...), "노선", "기점", "종점", "정류장 수"), "rows": sequenceRows}, refs))
		out = append(out, block("table", "대표 운행의 정류장 순서", "그 운행 기록이 지난 정류장 전체 순서",
			J{"columns": append(append([]any{}, head...), "노선", "정류장 순서"), "rows": orderRows}, refs))
	}
	if len(mentionRows) > 0 {
		out = append(out, block("table", "원문 정류장을 지나는지", "원문이 말한 정류장과 이름이 겹치는 정류장을 노선이 지나는지",
			J{"columns": append(append([]any{}, head...), "노선", "원문 정류장", "지나는 정류장"), "rows": mentionRows}, refs))
	}
	for _, x := range jsonx.List(executed["comparisons"]) {
		c := jsonx.Obj(x)
		if jsonx.Str(c["function"]) != "stop_difference" {
			continue
		}
		row := jsonx.Obj(jsonx.List(jsonx.Obj(jsonx.Obj(evidence[jsonx.Str(c["evidence"])])["data"])["rows"])[0])
		joined := func(key string) string {
			if list := jsonx.Strs(row[key]); len(list) > 0 {
				return strings.Join(list, ", ")
			}
			return "없음"
		}
		out = append(out, block("table", "개편 전후 지나는 정류장 차이", "개편 전에만 지나는 정류장, 개편 후에만 지나는 정류장, 양쪽 모두 지나는 정류장 수",
			J{"columns": []any{"구분", "정류장"}, "rows": []any{[]any{"개편 전에만", joined("only_before")}, []any{"개편 후에만", joined("only_after")},
				[]any{"양쪽 모두", pyText(row["both"]) + "곳"}}}, []string{jsonx.Str(c["evidence"])}))
	}
	return out
}

func journeyBlocks(executed J) []J {
	out := []J{}
	brief := func(j J) string {
		return fmt.Sprintf("%s %s 승차(%s) → %s 도착, 대기 제외 %s분", pyText(j["routes"]), pyText(j["board"]), pyText(j["boarding"]), pyText(j["arrival"]), pyText(j["travelMinutes"]))
	}
	for _, run := range runs(executed) {
		rows, refs := []any{}, []string{}
		hours, directHours := 0, 0
		last := ""
		for _, q := range requests(run) {
			s := jsonx.Obj(q["summary"])
			refs = append(refs, jsonx.Str(q["evidence"]))
			if !truthy(s["earliest"]) {
				continue
			}
			hours++
			if s["direct"] != nil {
				directHours++
			}
			e, d := jsonx.Obj(s["earliest"]), jsonx.Obj(s["direct"])
			key := pyText(e["board"]) + "\x00" + pyText(e["routes"]) + "\x00" + pyText(e["arrival"])
			if key == last {
				continue
			}
			last = key
			earliest := brief(e)
			if truthy(e["transfers"]) {
				earliest += ", 환승 " + pyText(e["transfers"])
			}
			direct := "없음"
			if d != nil {
				direct = brief(d)
			}
			rows = append(rows, []any{jsonx.Obj(q["input"])["time"], earliest, direct})
		}
		if len(rows) > 0 {
			out = append(out, block("table", "기준 시각별 여정", "기준 시각마다 가장 빨리 닿는 여정과 가장 빨리 닿는 직행 여정",
				J{"columns": []any{"기준 시각", "가장 빨리 닿는 여정", "직행 여정"}, "rows": rows}, refs))
		}
		out = append(out, block("metrics", "여정이 있는 기준 시각", "여정이 있는 기준 시각 수와 그중 직행 여정이 있는 수",
			J{"metrics": []any{J{"label": "여정이 있는 기준 시각", "value": fmt.Sprintf("%d개", hours)}, J{"label": "직행 여정이 있는 기준 시각", "value": fmt.Sprintf("%d개", directHours)}}}, refs))
	}
	return out
}

func segmentBlocks(executed J) []J {
	out := []J{}
	for _, run := range runs(executed) {
		for _, q := range requests(run) {
			s := jsonx.Obj(q["summary"])
			metrics := []any{J{"label": "구간 견적 합", "value": pyText(s["totalMinutes"]) + "분"}, J{"label": "지난 정류장", "value": pyText(s["stops"]) + "곳"}}
			if rec := jsonx.Obj(s["recordedTrips"]); len(rec) > 0 {
				metrics = append(metrics, J{"label": "실제 운행 기록", "value": pyText(rec["trips"]) + "회"},
					J{"label": "실제 소요", "value": pyText(rec["lowMinutes"]) + "–" + pyText(rec["highMinutes"]) + "분"})
			}
			out = append(out, block("metrics", "구간 소요", "구간별 소요 견적의 합과 그 노선 운행 기록이 실제로 걸린 시간", J{"metrics": metrics}, []string{jsonx.Str(q["evidence"])}))
		}
	}
	return out
}

// BlockCandidates는 todo 하나의 블록 후보다. id는 '<todo id>.b<번호>'다. route('염포동 → 삼산')를 주면 표·차트 제목 앞에 붙여 쌍을 구별한다.
func BlockCandidates(todo, executed, evidence J, route string) []J {
	m := jsonx.Obj(todo["method"])
	method := jsonx.Str(m["method"])
	out := []J{}
	switch method {
	case "departures", "route_departures":
		function, what := "route_starts", "노선"
		place, district := false, false
		for _, run := range runs(executed) {
			place = place || run["place"] == true
			district = district || run["district"] == true
		}
		stripTitle, here := what+"별 출발 시각", ""
		if method == "departures" {
			function, what = "calls_summary", "정류장 계열"
			stripTitle, here = "정류장에 오는 시각", "정류장에 오는 시각"
			if place {
				what, stripTitle, here = "노선", "장소에 오는 평균 시각", "장소에 오는 평균 시각"
			}
			if m["at_destination"] == true {
				stripTitle, here = "도착지에 오는 시각", "도착지에 오는 시각"
				if place {
					stripTitle, here = "도착지에 오는 평균 시각", "도착지에 오는 평균 시각"
				}
			}
		}
		// 정류장 조회에는 노선 기점 출발을 화면의 전환으로 곁들인다. 지칭을 잘못 이었을 때 담당자가 대조한다.
		plain := J{"method": J{"window": m["window"]}}
		alternate := func(payload J, kind string, refs []string) []string {
			if here == "" {
				return refs
			}
			var other J
			var more []string
			title := "노선별 출발 시각"
			if kind == "table" {
				other, more = seriesTable(plain, executed, evidence, execute.RouteStartsAlternate)
				title = "노선별 편수와 간격"
			} else {
				other, more = stripPayload(plain, executed, evidence, execute.RouteStartsAlternate)
			}
			if other != nil {
				payload["label"], payload["alternate"] = here, J{"label": "노선 기점 출발", "title": title, "payload": other}
				return append(refs, more...)
			}
			return refs
		}
		compared, comparedRefs := compareTable(executed, evidence)
		if compared != nil {
			out = append(out, block("table", "도착지까지 가는 노선 비교", "그 이동에 쓸 수 있는 노선마다 요금 구분, 도착지에 가장 가까운 승차 정류장, 운행 수, 간격, 도착지까지 소요", compared, comparedRefs))
		}
		// 구·군 전체는 버스가 지나는 구간이 길어 한 장소의 평균 시각이 뜻이 없다. 도착지가 있으면 노선 비교 표만, 없으면 노선별 표만 둔다.
		if table, refs := seriesTable(todo, executed, evidence, function); table != nil && len(table["rows"].([]any)) <= 40 && !(district && compared != nil) {
			description := what + "마다 출발 편수, 평균·최대 간격, 첫차·막차"
			if truthy(m["gap_columns"]) {
				description += ", 시민이 말한 수치를 넘는 간격 수"
			}
			out = append(out, block("table", what+"별 편수와 간격", description, table, alternate(table, "table", refs)))
		}
		if strip, refs := stripPayload(todo, executed, evidence, function); strip != nil && !district {
			out = append(out, block("strip", stripTitle, what+"마다 시간대 안의 시각을 시간축에 찍은 것", strip, alternate(strip, "strip", refs)))
		}
		if bars, refs := barsPayload(executed, evidence, function); bars != nil && !district {
			out = append(out, block("bars", what+"별 편수", what+"마다 출발 편수를 막대로 비교", bars, refs))
		}
	case "map":
		out = mapBlocks(executed, evidence)
	case "journeys":
		out = journeyBlocks(executed)
	case "segments":
		out = segmentBlocks(executed)
	}
	id := jsonx.Str(todo["id"])
	// 개편 전후 번호가 다르면 어느 번호를 나란히 놓았는지 제목에 적는다.
	period := ""
	if all := runs(executed); len(all) == 2 && method != "map" {
		before, after := jsonx.Strs(all[0]["routes"]), jsonx.Strs(all[1]["routes"])
		if len(before) > 0 && len(after) > 0 && strings.Join(before, ",") != strings.Join(after, ",") {
			period = " · 개편 전 " + strings.Join(before, "·") + " → 개편 후 " + strings.Join(after, "·")
		}
	}
	for i, b := range out {
		b["id"], b["todo"] = fmt.Sprintf("%s.b%d", id, i+1), id
		if route != "" {
			b["title"] = route + " " + jsonx.Str(b["title"])
		}
		b["title"] = jsonx.Str(b["title"]) + period
	}
	return out
}
