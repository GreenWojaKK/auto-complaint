// Package execute는 계획의 todo마다 정해진 조회를 그 조건 그대로 실행하고 결과를 근거로 남긴다. 모델을 부르지 않는다.
//
//   - departures: 승차 후보의 그 판 정류장에서 calls_summary(계열 요약)와 calls(통과 목록)를 조회한다. 목적지가 있으면 그곳에 닿는
//     노선을 나란히 정리한다. 지역 후보는 그 행정동의 정류장 전체다. 시민이 말한 수치(gap_over)마다 계열별로 그 간격을 넘는
//     출발 간격의 수를 센다.
//   - route_departures: 노선 번호마다 출발(route_starts)을 조회하고, 시민이 말한 수치마다 그 간격을 넘는 수를 센다.
//   - journeys: 출발·도착 장소로 여정을 조회한다. 도착 기한이면 시간대의 끝을 기준 시각으로 한 번, 출발 기준이면 시간대 안의
//     매시 정각을 기준 시각으로 여러 번 조회한다(여정 조회 한 번은 기준 시각에서 4시간 안에 닿는 여정만 찾는다).
//   - segments: 출발·도착 정류장 사이 구간 소요를 조회한다.
//   - map: 노선 경로와 원문 정류장 위치를 조회하고, 노선마다 대표 운행(정류장 방문이 가장 많은 온전한 운행 기록)의 기점·
//     종점·정류장 순서, 노선이 지나는 정류장 이름, 원문 정류장을 지나는지를 정리한다.
//
// compare_periods면 같은 조건을 개편 전·후 자료에 함께 조회하고, 개편 전 자료에서는 before_routes(시민이 말한 개편 전 번호나 판정을 통과한 번호)를 쓴다.
// 노선 경로를 개편 전후로 조회하면 개편 전에만·후에만 지나는 정류장을 정리한다. 시간대에 요일 종류가 있으면 자료 판의 날과
// 맞는지 표시한다(자료 판의 관측일을 달력으로 읽는다). 코드가 정리한 값도 근거 ID를 가진 근거로 남는다.
// 결과는 <run>/execute/<번호>.json에 todo별 조회 입력·근거 ID·요약과 근거 원문으로 쓴다.
package execute

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"dllio/backend/calendar"
	"dllio/backend/jsonx"
	"dllio/backend/repo"
	"dllio/backend/textx"
	"dllio/backend/tools"
)

const gapsCalls = `SELECT route_number, stop_id, min(stop_name) AS stop_name, count(gap_seconds)::int AS gaps, {over},
  round(max(gap_seconds) / 60.0, 1) AS max_gap_min
FROM transit.calls({args}) GROUP BY route_number, stop_id ORDER BY route_number, stop_id`

const gapsStarts = `WITH s AS (
  SELECT p.route_number, p.stop_id, p.stop_name, p.partial, coalesce(p.departure, p.arrival) AS at FROM transit.pass p
  WHERE p.revision = '{rev}' AND p.seq = 0 AND p.shape <> 'back' AND p.route_number = ANY({routes})),
w AS (SELECT s.*, s.at - lag(s.at) OVER (PARTITION BY s.route_number ORDER BY s.at) AS gap
  FROM s WHERE s.at >= {t_from} AND s.at < {t_to}),
a AS (SELECT DISTINCT ON (w.route_number) w.route_number, w.stop_id, w.stop_name FROM w GROUP BY w.route_number, w.stop_id, w.stop_name
  ORDER BY w.route_number, count(*) FILTER (WHERE NOT w.partial) DESC, count(*) DESC, w.stop_id)
SELECT w.route_number, a.stop_id, a.stop_name, count(gap)::int AS gaps, {over},
  round(max(gap) / 60.0, 1) AS max_gap_min
FROM w JOIN a ON a.route_number = w.route_number GROUP BY w.route_number, a.stop_id, a.stop_name ORDER BY w.route_number`

// 정류장이 여러 개인 장소의 통과. 같은 운행 기록에서 방문 순번이 세 번 안쪽으로 이어지면 그 장소를 한 번 지나는 것이고,
// 그 동안의 시각을 평균해 그 장소에 오는 시각으로 쓴다. 노선 번호마다 한 줄이다.
const placePasses = `WITH c AS (SELECT * FROM transit.calls({args})),
b AS (SELECT c.*, CASE WHEN c.seq - lag(c.seq) OVER (PARTITION BY c.trip_id ORDER BY c.seq) <= 3 THEN 0 ELSE 1 END AS starts FROM c),
i AS (SELECT b.*, sum(b.starts) OVER (PARTITION BY b.trip_id ORDER BY b.seq) AS pass FROM b),
p AS (SELECT i.route_number, i.trip_id, i.pass, round(avg(i.at))::int AS at, avg(i.minutes_to_destination) AS minutes
  FROM i GROUP BY i.route_number, i.trip_id, i.pass),
g AS (SELECT p.*, p.at - lag(p.at) OVER (PARTITION BY p.route_number ORDER BY p.at, p.trip_id) AS gap_seconds FROM p)
`

const placeSummary = placePasses + `SELECT route_number, {label} AS stop_id, {label} AS stop_name, '' AS headings, count(*)::int AS passes,
  round(avg(gap_seconds) / 60.0, 1) AS mean_gap_min, round(max(gap_seconds) / 60.0, 1) AS max_gap_min,
  transit.hhmm(min(at)) AS first_time, transit.hhmm(max(at)) AS last_time, string_agg(transit.hhmm(at), ' · ' ORDER BY at) AS times,
  count(minutes)::int AS reached_destination, CASE WHEN {has_destination} THEN (count(*) - count(minutes))::int END AS not_reached_destination,
  round(percentile_cont(0.5) WITHIN GROUP (ORDER BY minutes)::numeric, 1) AS median_minutes_to_destination,
  round(min(minutes), 1) AS min_minutes_to_destination, round(max(minutes), 1) AS max_minutes_to_destination
FROM g GROUP BY route_number ORDER BY route_number`

const placeGaps = placePasses + `SELECT route_number, {label} AS stop_id, {label} AS stop_name, count(gap_seconds)::int AS gaps, {over},
  round(max(gap_seconds) / 60.0, 1) AS max_gap_min
FROM g GROUP BY route_number ORDER BY route_number`

// RouteStartsAlternate는 정류장 조회에 곁들이는 노선 기점 출발의 함수 이름이다. 화면의 전환에만 쓰고 답의 재료로 보이지 않는다.
const RouteStartsAlternate = "route_starts_alternate"

// 목적지까지 가는 노선 비교. 운행 수와 간격은 운행마다 한 번만 센다(한 운행이 승차 지역의 정류장을 여러 번 지나도 부풀지 않는다).
// 소요는 승차 정류장 전체로 재므로, 지역에서 타면 어느 정류장에서 타느냐에 따른 범위가 된다. 요금 구분은 지역 설정(region.json의 fares)이 노선 번호 형식으로 정한다.
const routeCompare = `WITH a AS (
  SELECT route_number, trip_id, at, stop_name, minutes_to_destination FROM transit.calls({args}) WHERE destination_stop IS NOT NULL),
c AS (SELECT DISTINCT ON (route_number, trip_id) route_number, trip_id, at, stop_name FROM a ORDER BY route_number, trip_id, minutes_to_destination, at),
g AS (SELECT c.*, at - lag(at) OVER (PARTITION BY route_number ORDER BY at) AS gap FROM c),
t AS (SELECT route_number, count(*)::int AS trips, round(avg(gap) / 60.0, 1) AS mean_gap_min, round(max(gap) / 60.0, 1) AS max_gap_min,
    transit.hhmm(min(at)) AS first_time, transit.hhmm(max(at)) AS last_time, mode() WITHIN GROUP (ORDER BY stop_name) AS boarding FROM g GROUP BY route_number),
m AS (SELECT route_number, round((percentile_cont(0.5) WITHIN GROUP (ORDER BY minutes_to_destination))::numeric, 1) AS median_minutes,
    min(minutes_to_destination) AS min_minutes, max(minutes_to_destination) AS max_minutes FROM a GROUP BY route_number)
SELECT t.route_number, {fare} AS fare, boarding, trips, mean_gap_min, max_gap_min, first_time, last_time,
  median_minutes, min_minutes, max_minutes
FROM t JOIN m USING (route_number) ORDER BY median_minutes, t.route_number`

const sequence = `WITH t AS (
  SELECT DISTINCT ON (route_number) route_number, trip_id FROM transit.trip
  WHERE revision = '{rev}' AND route_number = ANY({routes}) AND NOT partial ORDER BY route_number, visits DESC, trip_id)
SELECT t.route_number, t.trip_id, count(*)::int AS stops,
  (array_agg(p.stop_name ORDER BY p.seq))[1] AS first_stop, (array_agg(p.stop_name ORDER BY p.seq DESC))[1] AS last_stop,
  string_agg(p.stop_name, ' → ' ORDER BY p.seq) AS sequence
FROM t JOIN transit.pass p ON p.revision = '{rev}' AND p.trip_id = t.trip_id
GROUP BY t.route_number, t.trip_id ORDER BY t.route_number`

const stopNames = `SELECT route_number, count(DISTINCT stop_name)::int AS stop_count, array_agg(DISTINCT stop_name ORDER BY stop_name) AS stop_names
FROM transit.pass WHERE revision = '{rev}' AND route_number = ANY({routes}) GROUP BY route_number ORDER BY route_number`

// J는 JSON 객체다. 계획과 조회 결과를 적힌 그대로 읽는다.
type J = jsonx.J

func fill(template string, values ...string) string {
	return strings.NewReplacer(values...).Replace(template)
}

// Literal은 글 목록의 SQL 배열이다.
func Literal(values []string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = "'" + strings.ReplaceAll(v, "'", "''") + "'"
	}
	return "ARRAY[" + strings.Join(quoted, ",") + "]::text[]"
}

var clockText = regexp.MustCompile(`^(\d{1,2}):(\d{2})$`)

// Minutes는 H:MM 또는 HH:MM 시각의 분이다. 시각이 아니면 거짓이다.
func Minutes(text string) (int, bool) {
	m := clockText.FindStringSubmatch(text)
	if m == nil {
		return 0, false
	}
	h, _ := strconv.Atoi(m[1])
	minute, _ := strconv.Atoi(m[2])
	if minute >= 60 {
		return 0, false
	}
	return h*60 + minute, true
}

// Clock은 분을 HH:MM으로 쓴다.
func Clock(total int) string { return fmt.Sprintf("%02d:%02d", total/60, total%60) }

func seconds(text string) string {
	m, _ := Minutes(text)
	return strconv.Itoa(m * 60)
}

func overColumns(column string, thresholds []int) string {
	parts := make([]string, len(thresholds))
	for i, n := range thresholds {
		parts[i] = fmt.Sprintf("count(*) FILTER (WHERE %s > %d)::int AS over_%d", column, n*60, n)
	}
	return strings.Join(parts, ", ")
}

// round1은 소수 첫째 자리로 반올림한다. 이진수 값의 정확한 십진 표현을 기준으로 한다.
func round1(x float64) float64 {
	y, _ := strconv.ParseFloat(strconv.FormatFloat(x, 'f', 1, 64), 64)
	return y
}

// digest는 값을 키를 정렬한 글로 쓴 SHA-256이다.
func digest(value any) (string, error) {
	raw, err := jsonx.Marshal(value)
	if err != nil {
		return "", err
	}
	parsed, err := jsonx.Parse(raw)
	if err != nil {
		return "", err
	}
	return jsonx.Digest(parsed)
}

// DayCheck은 질문의 요일 종류와 자료 판의 날이 맞는지다. 판에 요일이 수록되지 않았으면 matches는 null이다.
func DayCheck(window J, revision string, days map[string]calendar.RevisionDay) J {
	asked := jsonx.List(window["day"])
	if window == nil || len(asked) == 0 {
		return nil
	}
	info, ok := days[revision]
	check := J{"asked": window["day"], "data": nil, "note": nil, "matches": nil}
	if ok {
		check["note"] = info.Note
		if info.Day != nil {
			check["data"] = *info.Day
			check["matches"] = slices.Contains(jsonx.Strs(window["day"]), *info.Day)
		}
	}
	return check
}

// derived는 코드가 다른 근거에서 정리한 값이다. 입력에 근거 ID를 남겨 출처를 잇는다.
func derived(operation string, sources []string, rows []J) (J, error) {
	record := J{"input": J{"operation": operation, "derived_from": sources}, "data": J{"rows": rows, "total": len(rows)}}
	hash, err := digest(record)
	if err != nil {
		return nil, err
	}
	record["id"] = "e_" + hash[:16]
	return record, nil
}

// RouteNumbers는 그 판에서 쓸 노선 번호다. 계획의 판이 아닌 개편 전 자료에서는 후보 준비가 찾은 개편 전 수록 번호로 바꾼다.
func RouteNumbers(routes []string, revision, planRevision string, mentions []any) []string {
	byNumber := map[string]J{}
	for _, m := range mentions {
		if route := jsonx.Obj(jsonx.Obj(m)["route"]); len(route) > 0 {
			byNumber[jsonx.Str(route["number"])] = route
		}
	}
	out := []string{}
	if revision == planRevision || revision != "before" {
		for _, r := range routes {
			if in, present := byNumber[r]["in_revision"]; !present || in == true {
				out = append(out, r)
			}
		}
		return out
	}
	for _, r := range routes {
		for _, n := range jsonx.Strs(byNumber[r]["before_numbers"]) {
			if !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
	}
	return out
}

// OnRoute는 원문 정류장 지칭과 이름이 겹치는 정류장이다.
func OnRoute(text string, names []string) []string {
	want, out := textx.Squeeze(text), []string{}
	for _, n := range names {
		name := textx.Squeeze(n)
		if textx.Len(name) >= 2 && (strings.Contains(want, name) || strings.Contains(name, want)) {
			out = append(out, n)
		}
	}
	return out
}

func pick(row J, keys ...string) J {
	out := J{}
	for _, k := range keys {
		out[k] = row[k]
	}
	return out
}

func rowsOf(data J) []J {
	out := []J{}
	for _, r := range jsonx.List(data["rows"]) {
		out = append(out, jsonx.Obj(r))
	}
	return out
}

// summary는 검토 문서에 보일 핵심 값이다.
func summary(method string, data J) J {
	rows := rowsOf(data)
	picked := func(keys ...string) []J {
		out := []J{}
		for _, r := range rows {
			out = append(out, pick(r, keys...))
		}
		return out
	}
	switch method {
	case "calls_summary":
		return J{"series": picked("route_number", "stop_id", "stop_name", "headings", "passes", "mean_gap_min", "max_gap_min", "first_time", "last_time",
			"reached_destination", "not_reached_destination", "median_minutes_to_destination"), "total": data["total"]}
	case "route_starts":
		return J{"starts": picked("route_number", "stop_name", "departures", "variants", "mean_gap_min", "max_gap_min", "first_time", "last_time"), "total": data["total"]}
	case "gaps":
		return J{"gaps": data["rows"]}
	case "sequence":
		return J{"routes": picked("route_number", "first_stop", "last_stop", "stops")}
	case "route_compare":
		return J{"routes": data["rows"]}
	case "stop_names":
		return J{"rows": picked("route_number", "stop_count")}
	case "mentions_on_route", "stop_difference":
		return J{"rows": data["rows"]}
	case "journeys":
		brief := func(r J) J {
			var board any
			for _, e := range jsonx.List(r["events"]) {
				if jsonx.Str(jsonx.Obj(e)["label"]) != "도보" {
					board = jsonx.Obj(e)["start"]
					break
				}
			}
			return J{"board": board, "arrival": r["arrival"], "routes": r["routes"], "transfers": r["transfers"], "walkMeters": r["walkMeters"],
				"minutes": r["minutes"], "waitMinutes": r["waitMinutes"], "travelMinutes": round1(jsonx.Num(r["minutes"]) - jsonx.Num(r["waitMinutes"])),
				"boarding": r["boarding"]}
		}
		out := J{"status": data["status"], "journeys": len(rows), "boardingCount": data["boardingCount"], "earliest": nil, "direct": nil}
		if len(rows) > 0 {
			out["earliest"] = brief(rows[0])
		}
		var direct J
		for _, r := range rows {
			if jsonx.Num(r["transfers"]) == 0 && (direct == nil || jsonx.Str(r["arrival"]) < jsonx.Str(direct["arrival"])) {
				direct = r
			}
		}
		if direct != nil {
			out["direct"] = brief(direct)
		}
		return out
	case "segments":
		path := jsonx.Obj(data["path"])
		return J{"totalMinutes": data["totalMinutes"], "recordedTrips": data["recordedTrips"], "steps": len(rows), "routes": path["routes"], "stops": len(jsonx.List(path["stops"]))}
	case "map":
		points := []any{}
		for _, p := range jsonx.List(jsonx.Obj(data["map"])["points"]) {
			points = append(points, jsonx.Obj(p)["name"])
		}
		return J{"routes": data["rows"], "points": points}
	}
	return J{}
}

type runner struct {
	ctx      context.Context
	tools    *tools.Tools
	question string
}

// toolError는 조회가 거절한 이유다. 그 판의 실행에 error로 남기고 다음 판으로 넘어간다.
type toolError struct{ message string }

func (e *toolError) Error() string { return e.message }

func (r *runner) call(endpoint string, input J) (J, error) {
	evidence, err := r.tools.Call(r.ctx, endpoint, input)
	if errors.Is(err, tools.ErrUnknown) {
		return nil, err
	}
	if err != nil {
		return nil, &toolError{endpoint + ": " + err.Error()}
	}
	return tools.Plain(evidence)
}

func (r *runner) sql(run J, function, query, purpose string) (J, error) {
	result, err := r.call("/sql", J{"query": query, "purpose": purpose})
	if err != nil {
		return nil, err
	}
	run["requests"] = append(run["requests"].([]J), J{"endpoint": "/sql", "function": function, "evidence": result["id"], "result": result,
		"summary": summary(function, jsonx.Obj(result["data"]))})
	return result, nil
}

func (r *runner) stopsOf(candidate J, revision string) ([]string, error) {
	if jsonx.Str(candidate["kind"]) == "nearby" {
		return jsonx.Strs(jsonx.Obj(candidate["stop_ids"])[revision]), nil
	}
	var query string
	if jsonx.Str(candidate["kind"]) == "area" {
		areas := []string{}
		for _, a := range jsonx.List(candidate["areas"]) {
			areas = append(areas, jsonx.Str(jsonx.Obj(a)["area"]))
		}
		query = fmt.Sprintf("SELECT stop_id FROM stop WHERE revision = '%s' AND area = ANY(%s) ORDER BY stop_id", revision, Literal(areas))
	} else {
		query = fmt.Sprintf("SELECT stop_id FROM place_stop WHERE place_id = '%s' AND revision = '%s' ORDER BY ord", jsonx.Str(candidate["place_id"]), revision)
	}
	result, err := r.call("/sql", J{"query": query, "purpose": "앞단 후보·조회"})
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, row := range rowsOf(jsonx.Obj(result["data"])) {
		out = append(out, jsonx.Str(row["stop_id"]))
	}
	return out, nil
}

func (r *runner) stops(ids []string, places map[string]J, revision string) ([]string, error) {
	out := []string{}
	for _, id := range ids {
		found, err := r.stopsOf(places[id], revision)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	return out, nil
}

func (r *runner) query(run J, function string, body J) error {
	input := J{"purpose": r.question}
	for k, v := range body {
		input[k] = v
	}
	result, err := r.call("/query", input)
	if err != nil {
		return err
	}
	run["requests"] = append(run["requests"].([]J), J{"endpoint": "/query", "input": body, "evidence": result["id"], "result": result,
		"summary": summary(function, jsonx.Obj(result["data"]))})
	return nil
}

// firstID는 후보 id 목록의 첫 id다. 여정은 출발·도착 후보를 하나씩만 쓴다.
func firstID(v any) string {
	if ids := jsonx.Strs(v); len(ids) > 0 {
		return ids[0]
	}
	return ""
}

func boardingName(ids []string, places map[string]J) string {
	names := []string{}
	for _, id := range ids {
		names = append(names, jsonx.Str(places[id]["name"]))
	}
	return strings.Join(names, " / ")
}

// once는 한 판의 조회다.
func (r *runner) once(m J, run J, revision string, routes []string, places map[string]J, texts map[string]string) error {
	window := jsonx.Obj(m["window"])
	switch jsonx.Str(m["method"]) {
	case "departures":
		boarding, err := r.stops(jsonx.Strs(m["boarding_candidates"]), places, revision)
		if err != nil {
			return err
		}
		destination, err := r.stops(jsonx.Strs(m["destination_candidates"]), places, revision)
		if err != nil {
			return err
		}
		run["stop_counts"] = J{"boarding": len(boarding), "destination": len(destination)}
		for _, id := range jsonx.Strs(m["boarding_candidates"]) {
			if jsonx.Str(places[id]["scope"]) == "district" {
				run["district"] = true
			}
		}
		if len(boarding) == 0 {
			run["note"] = "이 판에 승차 장소의 정류장이 수록되어 있지 않다"
			return nil
		}
		args := fmt.Sprintf("'%s', %s, %s, %s, %s, '%s', %s, %t", revision, Literal(boarding), Literal(routes), seconds(jsonx.Str(window["start"])),
			seconds(jsonx.Str(window["end"])), jsonx.Str(m["time_kind"]), Literal(destination), m["direct"] == true)
		// 정류장이 여러 개인 장소는 정확한 통과 시각이 정해지지 않으므로 한 번 지나는 동안의 평균 시각으로 노선마다 한 줄이다.
		wide := len(boarding) > 1
		label := "'" + strings.ReplaceAll(boardingName(jsonx.Strs(m["boarding_candidates"]), places), "'", "''") + "'"
		summaryQuery := "SELECT * FROM calls_summary(" + args + ")"
		if wide {
			run["place"] = true
			summaryQuery = fill(placeSummary, "{args}", args, "{label}", label, "{has_destination}", fmt.Sprint(len(destination) > 0))
		}
		passes, err := r.sql(run, "calls_summary", summaryQuery, r.question)
		if err != nil {
			return err
		}
		calls, err := r.call("/sql", J{"query": "SELECT * FROM calls(" + args + ")", "purpose": r.question})
		if err != nil {
			return err
		}
		run["requests"] = append(run["requests"].([]J), J{"endpoint": "/sql", "function": "calls", "evidence": calls["id"], "result": calls,
			"summary": J{"passes": jsonx.Obj(calls["data"])["total"]}})
		if len(destination) > 0 {
			if _, err := r.sql(run, "route_compare", fill(routeCompare, "{args}", args, "{fare}", r.tools.Region().FareSQL("t.route_number")), "목적지까지 가는 노선 비교"); err != nil {
				return err
			}
		}
		if over := jsonx.Ints(m["gap_over"]); len(over) > 0 {
			query := fill(gapsCalls, "{over}", overColumns("gap_seconds", over), "{args}", args)
			if wide {
				query = fill(placeGaps, "{over}", overColumns("gap_seconds", over), "{args}", args, "{label}", label)
			}
			if _, err := r.sql(run, "gaps", query, "시민이 말한 수치를 넘는 출발 간격"); err != nil {
				return err
			}
		}
		alternate := routes
		if len(alternate) == 0 {
			for _, row := range rowsOf(jsonx.Obj(passes["data"])) {
				if n := jsonx.Str(row["route_number"]); n != "" && !slices.Contains(alternate, n) {
					alternate = append(alternate, n)
				}
			}
		}
		if len(alternate) > 0 {
			query := fmt.Sprintf("SELECT * FROM route_starts('%s', %s, %s, %s)", revision, Literal(alternate), seconds(jsonx.Str(window["start"])), seconds(jsonx.Str(window["end"])))
			if _, err := r.sql(run, RouteStartsAlternate, query, r.question); err != nil {
				return err
			}
		}
	case "route_departures":
		from, to := seconds(jsonx.Str(window["start"])), seconds(jsonx.Str(window["end"]))
		query := fmt.Sprintf("SELECT * FROM route_starts('%s', %s, %s, %s)", revision, Literal(routes), from, to)
		if _, err := r.sql(run, "route_starts", query, r.question); err != nil {
			return err
		}
		if over := jsonx.Ints(m["gap_over"]); len(over) > 0 {
			query := fill(gapsStarts, "{rev}", revision, "{routes}", Literal(routes), "{t_from}", from, "{t_to}", to, "{over}", overColumns("gap", over))
			if _, err := r.sql(run, "gaps", query, "시민이 말한 수치를 넘는 노선 출발 간격"); err != nil {
				return err
			}
		}
	case "journeys":
		start, _ := Minutes(jsonx.Str(window["start"]))
		end, _ := Minutes(jsonx.Str(window["end"]))
		end = min(end, 24*60-1)
		base := J{"operation": "journeys", "dataset": revision, "origin": places[firstID(m["origin_candidates"])]["place_id"],
			"destination": places[firstID(m["destination_candidates"])]["place_id"], "arrive_by": m["arrive_by"], "start": window["start"], "end": Clock(end),
			"walk_meters": m["walk_meters"], "transfers": m["transfers"], "routes": routes}
		// 여정 조회는 한 기준 시각에서 4시간 안에 닿는 여정을 찾는다. 출발 기준이면 시간대를 매시 기준 시각으로 나눠
		// 시간대 전체의 여정을 본다. 도착 기한이면 시간대의 끝 하나다.
		times := []string{Clock(end)}
		if m["arrive_by"] != true {
			times = []string{}
			for t := start; t < end; t += 60 {
				times = append(times, Clock(t))
			}
		}
		for _, t := range times {
			body := J{"time": t}
			for k, v := range base {
				body[k] = v
			}
			if err := r.query(run, "journeys", body); err != nil {
				return err
			}
		}
	case "segments":
		origin, err := r.stops(jsonx.Strs(m["origin_candidates"]), places, revision)
		if err != nil {
			return err
		}
		destination, err := r.stops(jsonx.Strs(m["destination_candidates"]), places, revision)
		if err != nil {
			return err
		}
		run["stop_counts"] = J{"origin": len(origin), "destination": len(destination)}
		if len(origin) == 0 || len(destination) == 0 {
			run["note"] = "이 판에 출발 또는 도착 장소의 정류장이 수록되어 있지 않다"
			return nil
		}
		body := J{"operation": "segments", "dataset": revision, "routes": routes, "stop_ids": origin, "destination_stop_ids": destination,
			"start": window["start"], "end": window["end"]}
		return r.query(run, "segments", body)
	case "map":
		named := []string{}
		for _, s := range jsonx.Strs(m["map_mentions"]) {
			if text, ok := texts[s]; ok {
				named = append(named, text)
			}
		}
		if err := r.query(run, "map", J{"operation": "map", "dataset": revision, "routes": routes, "mentions": named}); err != nil {
			return err
		}
		if _, err := r.sql(run, "sequence", fill(sequence, "{rev}", revision, "{routes}", Literal(routes)), "노선마다 대표 운행의 정류장 순서"); err != nil {
			return err
		}
		names, err := r.sql(run, "stop_names", fill(stopNames, "{rev}", revision, "{routes}", Literal(routes)), "노선이 지나는 정류장 이름")
		if err != nil {
			return err
		}
		if len(named) > 0 {
			rows := []J{}
			for _, row := range rowsOf(jsonx.Obj(names["data"])) {
				for _, text := range named {
					rows = append(rows, J{"route_number": row["route_number"], "mention": text, "stops": OnRoute(text, jsonx.Strs(row["stop_names"]))})
				}
			}
			record, err := derived("원문 정류장을 지나는지", []string{jsonx.Str(names["id"])}, rows)
			if err != nil {
				return err
			}
			run["requests"] = append(run["requests"].([]J), J{"endpoint": "derived", "function": "mentions_on_route", "evidence": record["id"],
				"result": record, "summary": summary("mentions_on_route", jsonx.Obj(record["data"]))})
		}
	}
	return nil
}

// Todo는 todo 하나를 실행한다.
func Todo(ctx context.Context, t *tools.Tools, todo J, candidates J, days map[string]calendar.RevisionDay) (J, error) {
	m, planRevision := jsonx.Obj(todo["method"]), jsonx.Str(candidates["revision"])
	places, texts := map[string]J{}, map[string]string{}
	for _, x := range jsonx.List(candidates["mentions"]) {
		mention := jsonx.Obj(x)
		for _, c := range jsonx.List(mention["candidates"]) {
			places[jsonx.Str(jsonx.Obj(c)["id"])] = jsonx.Obj(c)
		}
		texts[jsonx.Str(mention["id"])] = jsonx.Str(mention["text"])
	}
	revisions := []string{planRevision}
	switch {
	case jsonx.Str(todo["period"]) == "before":
		revisions = []string{"before"}
	case todo["compare_periods"] == true:
		revisions = []string{"before", "after"}
	}
	r := &runner{ctx, t, jsonx.Str(todo["question"])}
	runs, comparisons := []J{}, []J{}
	for _, revision := range revisions {
		routes := RouteNumbers(jsonx.Strs(m["routes"]), revision, planRevision, jsonx.List(candidates["mentions"]))
		if _, chosen := m["before_routes"]; chosen && revision == "before" && revision != planRevision {
			routes = jsonx.Strs(m["before_routes"])
		}
		run := J{"revision": revision, "routes": routes, "requests": []J{}}
		if day := DayCheck(jsonx.Obj(m["window"]), revision, days); day != nil {
			run["day"] = day
		}
		var err error
		if len(jsonx.List(m["routes"])) > 0 && len(routes) == 0 {
			run["note"] = "이 판에 해당 노선 번호가 수록되어 있지 않다"
		} else {
			err = r.once(m, run, revision, routes, places, texts)
		}
		var refused *toolError
		if errors.As(err, &refused) {
			run["error"] = refused.message
		} else if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	if jsonx.Str(m["method"]) == "map" && len(runs) == 2 {
		type named struct {
			id    string
			names map[string]bool
		}
		var sets []*named
		for _, run := range runs {
			var found *named
			for _, q := range run["requests"].([]J) {
				if q["function"] == "stop_names" {
					found = &named{jsonx.Str(q["evidence"]), map[string]bool{}}
					for _, row := range rowsOf(jsonx.Obj(jsonx.Obj(q["result"])["data"])) {
						for _, n := range jsonx.Strs(row["stop_names"]) {
							found.names[n] = true
						}
					}
					break
				}
			}
			sets = append(sets, found)
		}
		if sets[0] != nil && sets[1] != nil {
			before, after := sets[0].names, sets[1].names
			onlyBefore, onlyAfter, both := []string{}, []string{}, 0
			for n := range before {
				if after[n] {
					both++
				} else {
					onlyBefore = append(onlyBefore, n)
				}
			}
			for n := range after {
				if !before[n] {
					onlyAfter = append(onlyAfter, n)
				}
			}
			sort.Strings(onlyBefore)
			sort.Strings(onlyAfter)
			row := J{"only_before_count": len(onlyBefore), "only_after_count": len(onlyAfter), "both": both, "only_before": onlyBefore, "only_after": onlyAfter}
			record, err := derived("개편 전후 정류장 차이", []string{sets[0].id, sets[1].id}, []J{row})
			if err != nil {
				return nil, err
			}
			comparisons = append(comparisons, J{"function": "stop_difference", "evidence": record["id"], "result": record,
				"summary": summary("stop_difference", jsonx.Obj(record["data"]))})
		}
	}
	out := J{"id": todo["id"], "method": m["method"], "runs": runs}
	if len(comparisons) > 0 {
		out["comparisons"] = comparisons
	}
	return out, nil
}

// Config는 Run의 입력이다.
type Config struct {
	Root, Run string
	Cases     []string
	Tools     *tools.Tools
	Report    func(line string)
}

func readJSON(path string) (J, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	parsed, err := jsonx.Parse(data)
	if err != nil {
		return nil, err
	}
	return jsonx.Obj(parsed), nil
}

// Run은 계획이 있는 민원마다 조회를 실행해 결과 파일을 쓴다. Cases가 비면 전부다. 민원은 차례로 실행한다.
func Run(ctx context.Context, c Config) error {
	cal, err := calendar.Load(c.Root)
	if err != nil {
		return err
	}
	days, err := cal.RevisionDays(c.Root)
	if err != nil {
		return err
	}
	cases := c.Cases
	if len(cases) == 0 {
		if cases, err = repo.Stems(filepath.Join(c.Run, "plan")); err != nil {
			return err
		}
	}
	for _, id := range cases {
		plan, err := readJSON(filepath.Join(c.Run, "plan", id+".json"))
		if err != nil {
			return err
		}
		candidates, err := readJSON(filepath.Join(c.Run, "candidates", id+".json"))
		if err != nil {
			return err
		}
		todos, evidence, errorCount := []J{}, J{}, 0
		var order []string
		keep := func(item J) {
			key := jsonx.Str(item["evidence"])
			if _, seen := evidence[key]; !seen {
				order = append(order, key)
			}
			evidence[key] = item["result"]
			delete(item, "result")
		}
		for _, todo := range jsonx.List(jsonx.Obj(plan["plan"])["todos"]) {
			done, err := Todo(ctx, c.Tools, jsonx.Obj(todo), candidates, days)
			if err != nil {
				return fmt.Errorf("민원 %s: %w", id, err)
			}
			todos = append(todos, done)
		}
		for _, t := range todos {
			for _, run := range t["runs"].([]J) {
				for _, q := range run["requests"].([]J) {
					keep(q)
				}
				if _, failed := run["error"]; failed {
					errorCount++
				}
			}
		}
		for _, t := range todos {
			if comparisons, ok := t["comparisons"].([]J); ok {
				for _, q := range comparisons {
					keep(q)
				}
			}
		}
		planHash, err := jsonx.Digest(plan["plan"])
		if err != nil {
			return err
		}
		ordered := jsonx.Object{}
		for _, key := range order {
			ordered = append(ordered, jsonx.Field{Key: key, Value: evidence[key]})
		}
		record := jsonx.Object{{Key: "id", Value: id}, {Key: "createdAt", Value: time.Now().Format(time.RFC3339Nano)}, {Key: "network", Value: c.Tools.Network()},
			{Key: "planHash", Value: planHash}, {Key: "todos", Value: todos}, {Key: "evidence", Value: ordered}}
		if err := jsonx.WriteFile(filepath.Join(c.Run, "execute", id+".json"), record); err != nil {
			return err
		}
		if c.Report != nil {
			line, err := jsonx.Marshal(jsonx.Object{{Key: "id", Value: id}, {Key: "todos", Value: len(todos)}, {Key: "evidence", Value: len(evidence)}, {Key: "errors", Value: errorCount}})
			if err != nil {
				return err
			}
			c.Report(string(line))
		}
	}
	return nil
}
