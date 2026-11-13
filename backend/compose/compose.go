// Package compose는 todo마다 조회 결과를 읽어 답을 쓰고, 코드가 만든 블록 후보 중에서 페이지에 둘 블록을 고른다.
//
// 조회 실행(execute)이 계획의 조건 그대로 조회하고 필요한 값을 정리해 두므로, 모델은 도구를 부르지 않는다. 모델(기본 Raon
// 생성 서버)은 민원마다 한 번 원문, L1, todo(질문·조건·답한 것으로 볼 결과)와 todo별 조회 값(근거 경로와 값), 블록 후보를 받아
// 다음을 고른다.
//   - todo마다 상태(답함·확인할 수 없음)와 답 문장. 문장 속 수치는 받은 근거 경로로 연결한다.
//   - 페이지: 함께 보일 todo, 그 todo들의 블록 후보 중 보일 것과 순서, 제목.
//
// 코드는 다음을 검사하고 어긋나면 다시 쓰게 한다.
//   - 모든 todo에 답이 하나씩 있다. 수치는 그 todo의 근거 경로를 가리키고 값이 같다. 질문이 기준으로 삼은 원문의 수치는 근거
//     없이 쓸 수 있다.
//   - 자료의 날이 질문의 요일과 다르거나 요일이 수록되지 않은 todo를 답함으로 두면 답에 그 사실을 쓴다.
//   - 페이지는 답한 todo를 하나 이상 가리키고, 블록은 그 페이지 todo들의 후보다.
//
// 결과는 <out>/compose/<번호>.json(블록 후보·답·페이지·모델 대화)과 <out>/view/<번호>.json(뷰어 입력)이다. todo가 없는
// 민원은 모델을 부르지 않는다.
package compose

import (
	"context"
	"encoding/json"
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
	"dllio/backend/execute"
	"dllio/backend/jsonx"
	"dllio/backend/matchi"
	"dllio/backend/repo"
	"dllio/backend/textx"
)

// ValueReference는 답 속 수치 하나의 근거 경로다. 모델은 경로를 먼저 적고 그 값을 문장에 쓸 표현을 적는다.
type ValueReference struct {
	Ref  string `json:"ref" desc:"답에 쓸 값의 evidence 경로. e_ID/data/rows/0/count 형식"`
	Text string `json:"text" desc:"그 값을 문장에 쓸 정확한 수치 표현. 예: 12회, 07:50, 32.1분"`
}

// Answer는 todo 하나의 답이다. 모델은 필드 순서대로 쓴다: 답에 쓸 자료 값을 먼저 고르고, 그 값으로 문장을 쓴다.
type Answer struct {
	Todo   string           `json:"todo" desc:"todo id"`
	Status string           `json:"status" enum:"답함,확인할 수 없음"`
	Values []ValueReference `json:"values" desc:"답에 쓸 자료 수치를 조회 값에서 고른 것. 수치마다 근거 경로와 문장에 쓸 표현" max:"40"`
	Text   string           `json:"text" desc:"행정 검토자가 읽을 답. 한두 문장, ~습니다. 자료 수치는 values에 적은 표현만 쓴다" maxlen:"800"`
}

// Page는 함께 보일 todo와 블록이다.
type Page struct {
	Title  string   `json:"title"`
	Todos  []string `json:"todos" desc:"이 페이지가 답을 보여 주는 todo id" max:"16"`
	Blocks []string `json:"blocks" desc:"보일 블록 후보 id(t1.b1 …), 보일 순서대로" max:"10"`
}

// Composition은 모델의 출력 형식이다.
type Composition struct {
	Answers   []Answer `json:"answers" max:"16"`
	Pages     []Page   `json:"pages" max:"6"`
	Decisions []string `json:"decisions" desc:"결과를 읽으며 정한 해석 한 줄씩" max:"6"`
}

const (
	// answerLength는 답 문장의 글자 수 상한이다(Answer.Text의 maxlen과 같다).
	answerLength = 800
	retries      = 4
	maxWait      = 60
)

// factKeys는 모델에게 보이는 조회 값이다. 근거마다 행 경로와 이 열들을 보인다.
var factKeys = map[string][]string{
	"calls_summary": {"route_number", "stop_id", "stop_name", "headings", "passes", "mean_gap_min", "max_gap_min", "first_time", "last_time",
		"times", "reached_destination", "median_minutes_to_destination"},
	"route_starts":      {"route_number", "stop_id", "stop_name", "departures", "variants", "mean_gap_min", "max_gap_min", "first_time", "last_time", "times"},
	"sequence":          {"route_number", "first_stop", "last_stop", "stops", "sequence"},
	"stop_names":        {"route_number", "stop_count"},
	"mentions_on_route": {"route_number", "mention", "stops"},
	"stop_difference":   {"only_before_count", "only_after_count", "both", "only_before", "only_after"},
	"map":               {"route", "stops", "trips", "vehicleCount"},
	"journeys":          {"departure", "arrival", "minutes", "waitMinutes", "walkMeters", "routes", "transfers", "boarding"},
}

// pyRepr는 값을 Python repr처럼 쓴다. 목록 안의 글은 따옴표로 감싼다.
func pyRepr(v any) string {
	switch x := v.(type) {
	case string:
		quote := "'"
		if strings.Contains(x, "'") && !strings.Contains(x, `"`) {
			quote = `"`
		}
		var b strings.Builder
		b.WriteString(quote)
		for _, r := range x {
			switch {
			case r == '\\':
				b.WriteString(`\\`)
			case string(r) == quote:
				b.WriteString(`\` + quote)
			case r == '\n':
				b.WriteString(`\n`)
			case r == '\r':
				b.WriteString(`\r`)
			case r == '\t':
				b.WriteString(`\t`)
			case r < 0x20 || r == 0x7f:
				fmt.Fprintf(&b, `\x%02x`, r)
			default:
				b.WriteRune(r)
			}
		}
		b.WriteString(quote)
		return b.String()
	case []any:
		parts := make([]string, len(x))
		for i, item := range x {
			parts[i] = pyRepr(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []string:
		parts := make([]string, len(x))
		for i, item := range x {
			parts[i] = pyRepr(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case jsonx.Object:
		parts := make([]string, len(x))
		for i, f := range x {
			parts[i] = pyRepr(f.Key) + ": " + pyRepr(f.Value)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return pyText(v)
}

// pyStr는 값을 Python의 str()처럼 쓴다. 글은 그대로, 목록과 객체는 repr로 쓴다.
func pyStr(v any) string {
	switch v.(type) {
	case []any, []string, jsonx.Object:
		return pyRepr(v)
	}
	return pyText(v)
}

func orderedRows(data jsonx.Object) []jsonx.Object {
	out := []jsonx.Object{}
	for _, r := range jsonx.List(data.Get("rows")) {
		row, _ := r.(jsonx.Object)
		out = append(out, row)
	}
	return out
}

func asObject(v any) jsonx.Object {
	o, _ := v.(jsonx.Object)
	return o
}

// factLines는 근거 하나의 값을 '경로: 열=값' 줄로 쓴다. record는 키 순서를 지켜 읽은 근거다.
func factLines(function string, record jsonx.Object, compared bool) []string {
	data := asObject(record.Get("data"))
	base := pyText(record.Get("id")) + "/data"
	keys, known := factKeys[function]
	rowLine := func(i int, row jsonx.Object, names []string) string {
		parts := []string{}
		if names == nil {
			for _, f := range row {
				parts = append(parts, f.Key+"="+pyStr(f.Value))
			}
		} else {
			for _, k := range names {
				if row.Has(k) {
					parts = append(parts, k+"="+pyStr(row.Get(k)))
				}
			}
		}
		return fmt.Sprintf("      %s/rows/%d: %s", base, i, strings.Join(parts, ", "))
	}
	switch {
	case function == "calls":
		return []string{fmt.Sprintf("      %s/total = %s (승차 정류장들을 지난 횟수의 합. 한 운행이 여러 정류장을 지나면 여러 번 센다. 운행 편수가 아니다)", base, pyStr(data.Get("total")))}
	case function == "segments":
		lines := []string{fmt.Sprintf("      %s/totalMinutes = %s", base, pyStr(data.Get("totalMinutes")))}
		for _, f := range asObject(data.Get("recordedTrips")) {
			switch f.Value.(type) {
			case []any, jsonx.Object:
				continue
			}
			lines = append(lines, fmt.Sprintf("      %s/recordedTrips/%s = %s", base, f.Key, pyStr(f.Value)))
		}
		return lines
	case function == "journeys":
		rows := orderedRows(data)
		if len(rows) == 0 {
			return []string{fmt.Sprintf("      %s: %s (4시간 안에 닿는 여정 없음)", base, pyStr(data.Get("status")))}
		}
		show := []int{0}
		best := -1
		for i, r := range rows {
			if jsonx.Num(r.Get("transfers")) == 0 && (best < 0 || pyText(r.Get("arrival")) < pyText(rows[best].Get("arrival"))) {
				best = i
			}
		}
		if best > 0 {
			show = append(show, best)
		}
		// 기준 시각이 첫차보다 한참 이르면 소요가 첫차까지의 대기로 부푼다. 그런 여정은 답의 재료로 보이지 않는다.
		kept := []int{}
		for _, i := range show {
			if jsonx.Num(rows[i].Get("waitMinutes")) <= maxWait {
				kept = append(kept, i)
			}
		}
		if len(kept) == 0 {
			return []string{fmt.Sprintf("      %s: 이 기준 시각에는 %d분 넘게 기다려야 탈 수 있음", base, maxWait)}
		}
		lines := []string{}
		for _, i := range kept {
			lines = append(lines, rowLine(i, rows[i], keys))
		}
		return lines
	case function == "calls_summary" && compared:
		return []string{fmt.Sprintf("      %s: 계열 %s개는 노선 비교 값으로 읽는다", base, pyStr(data.Get("total")))}
	case function == "gaps" || function == "route_compare":
		lines := []string{}
		for i, r := range orderedRows(data) {
			lines = append(lines, rowLine(i, r, nil))
		}
		return lines
	case !known:
		return []string{}
	}
	lines := []string{}
	for i, r := range orderedRows(data) {
		lines = append(lines, rowLine(i, r, keys))
	}
	if len(lines) == 0 && data.Has("rows") {
		// 조회는 실행됐고 그 조건·시간대에 해당하는 운행이 없다. 모델이 자료가 없는 것과 운행이 0인 것을 구별하도록 적는다.
		lines = append(lines, fmt.Sprintf("      %s/rows: 행 없음(운행 0편)", base))
	}
	return lines
}

func runLabel(run J) string {
	stops := jsonx.Obj(run["stops"])
	revision := jsonx.Str(run["revision"])
	name, ok := Revisions[revision]
	if !ok {
		name = revision
	}
	parts := []string{name}
	if routes := jsonx.Strs(run["routes"]); len(routes) > 0 {
		parts = append(parts, "노선 "+strings.Join(routes, ", "))
	}
	for _, f := range [][2]string{{"boarding", "승차"}, {"origin", "출발"}, {"destination", "도착"}} {
		if count, ok := stops[f[0]]; ok {
			parts = append(parts, fmt.Sprintf("%s 정류장 %s곳", f[1], pyText(count)))
		}
	}
	return strings.Join(parts, " · ")
}

func dayText(day J) string {
	asked := strings.Join(jsonx.Strs(day["asked"]), "·")
	note := pyText(day["note"])
	switch day["matches"] {
	case nil:
		return fmt.Sprintf("질문의 요일은 %s이고, 이 자료는 %s이다", asked, note)
	case true:
		return fmt.Sprintf("질문의 요일(%s)과 자료의 날(%s)이 맞는다", asked, note)
	}
	return fmt.Sprintf("질문의 요일은 %s이고, 자료의 날은 %s라 요일이 다르다", asked, note)
}

func dayUnmatched(executedTodo J) bool {
	for _, run := range runs(executedTodo) {
		if day := jsonx.Obj(run["day"]); len(day) > 0 && day["matches"] != true {
			return true
		}
	}
	return false
}

// Inputs는 민원 하나의 답 쓰기 입력이다.
type Inputs struct {
	Source, Received, Prior, DataDay string
	Plan, Candidates, Executed       J
	Ordered                          map[string]jsonx.Object
	Blocks                           map[string][]J
}

// Request는 답 쓰기 모델에게 보내는 입력이다.
func Request(in Inputs) string {
	mentions := byID(jsonx.List(in.Candidates["mentions"]))
	revision := jsonx.Str(in.Candidates["revision"])
	executedTodos := map[string]J{}
	for _, t := range jsonx.List(in.Executed["todos"]) {
		executedTodos[jsonx.Str(jsonx.Obj(t)["id"])] = jsonx.Obj(t)
	}
	l1 := jsonx.Obj(in.Plan["l1"])
	lines := []string{fmt.Sprintf("민원 접수일: %s (조회 자료 판: %s)", in.Received, Revisions[revision]), "개편 후 자료의 날: " + in.DataDay, "", "원문:", in.Source, "", "L1:",
		"- 한줄요약: " + jsonx.Str(l1["headline"])}
	for _, s := range jsonx.Strs(l1["situation"]) {
		lines = append(lines, "- 상황: "+s)
	}
	for _, s := range jsonx.Strs(l1["requests"]) {
		lines = append(lines, "- 요청: "+s)
	}
	for _, s := range jsonx.Strs(l1["replies"]) {
		lines = append(lines, "- 문의: "+s)
	}
	lines = append(lines, "", "자료로 살펴보지 않는 문제:")
	unchecked := jsonx.List(in.Plan["not_checkable"])
	for _, n := range unchecked {
		lines = append(lines, fmt.Sprintf("- [%s] %s", jsonx.Str(jsonx.Obj(n)["problem"]), jsonx.Str(jsonx.Obj(n)["reason"])))
	}
	if len(unchecked) == 0 {
		lines = append(lines, "- 없음")
	}
	for _, x := range jsonx.List(in.Plan["todos"]) {
		t := jsonx.Obj(x)
		id := jsonx.Str(t["id"])
		done := executedTodos[id]
		head := fmt.Sprintf("\n[%s] %s · %s", id, jsonx.Str(t["kind"]), jsonx.Str(t["problem"]))
		if truthy(t["move"]) {
			head += " 이동 " + pyText(t["move"])
		}
		if t["compare_periods"] == true {
			head += " · 개편 전후 비교"
		}
		lines = append(lines, head, "질문: "+jsonx.Str(t["question"]))
		for _, row := range Conditions(jsonx.Obj(t["method"]), mentions, revision) {
			lines = append(lines, row[0]+": "+row[1])
		}
		grounding := []string{}
		for _, g := range jsonx.Strs(t["grounding"]) {
			grounding = append(grounding, "«"+g+"»")
		}
		lines = append(lines, "답한 것으로 볼 결과: "+jsonx.Str(t["done_when"]), "근거 발췌: "+strings.Join(grounding, " / "), "조회 값:")
		for _, r := range runs(done) {
			lines = append(lines, "  "+runLabel(r))
			if day := jsonx.Obj(r["day"]); len(day) > 0 {
				lines = append(lines, "    요일: "+dayText(day))
			}
			if note := jsonx.Str(r["note"]); note != "" {
				lines = append(lines, "    "+note)
			} else if failure := jsonx.Str(r["error"]); failure != "" {
				lines = append(lines, "    실행 실패: "+failure)
			}
			compared := false
			for _, q := range requests(r) {
				compared = compared || jsonx.Str(q["function"]) == "route_compare"
			}
			for _, q := range requests(r) {
				name := jsonx.Str(q["function"])
				if name == "" {
					name = jsonx.Str(jsonx.Obj(q["input"])["operation"])
				}
				if name == execute.RouteStartsAlternate {
					continue
				}
				evidence := jsonx.Str(q["evidence"])
				lines = append(lines, "    "+name+" "+evidence)
				lines = append(lines, factLines(name, in.Ordered[evidence], compared)...)
			}
		}
		for _, y := range jsonx.List(done["comparisons"]) {
			c := jsonx.Obj(y)
			lines = append(lines, "  개편 전후 비교 "+jsonx.Str(c["function"])+" "+jsonx.Str(c["evidence"]))
			lines = append(lines, factLines(jsonx.Str(c["function"]), in.Ordered[jsonx.Str(c["evidence"])], false)...)
		}
		lines = append(lines, "블록 후보:")
		if len(in.Blocks[id]) == 0 {
			lines = append(lines, "  없음")
		}
		for _, b := range in.Blocks[id] {
			lines = append(lines, fmt.Sprintf("  %s [%s] %s — %s", jsonx.Str(b["id"]), jsonx.Str(b["kind"]), jsonx.Str(b["title"]), jsonx.Str(b["description"])))
		}
	}
	lines = append(lines, "", "지역 prior:", in.Prior)
	return strings.Join(lines, "\n")
}

// Numeric은 답 문장에서 근거가 필요한 수치 표현이다.
var Numeric = regexp.MustCompile(textx.Digit + `{1,2}:` + textx.Digit + `{2}|` + textx.Digit + `+(?:\.` + textx.Digit + `+)?[` + textx.Space + `]*(?:분|시간|편|회|대|km|m|%)`)

var number = regexp.MustCompile(textx.Digit + `+(?:\.` + textx.Digit + `+)?`)

// internalPhrases는 제품 화면에 쓰지 않는 내부 해명이다.
var internalPhrases = []string{"대체공휴일", "GTFS", "원문에 없는 항목", "원문에 없는 정보", "노선 특정 없음", "분석할 수 없습니다"}

var dayWords = regexp.MustCompile(`요일|공휴일|평일|토요일|일요일|주말`)

func asciiNumber(text string) float64 {
	var b strings.Builder
	for _, r := range text {
		if r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteString(strconv.Itoa(textx.Atoi(string(r))))
		}
	}
	f, _ := strconv.ParseFloat(b.String(), 64)
	return f
}

// ValueErrors는 문장의 편수·분·거리·시각마다 values의 근거 경로가 있고 그 값이 문장의 값과 같은지 본다.
// quoted는 원문에 있어 근거 없이 옮길 수 있는 수치다.
func ValueErrors(text string, values []ValueReference, evidence J, quoted map[string]bool) []string {
	errors := []string{}
	declared := map[string]bool{}
	for _, v := range values {
		declared[v.Text] = true
	}
	for _, numeric := range Numeric.FindAllString(text, -1) {
		if !declared[numeric] && !quoted[textx.Squeeze(numeric)] {
			errors = append(errors, "문장의 수치 표현에 values 근거를 연결한다: "+numeric)
		}
	}
	for _, v := range values {
		real, err := Binding(J{"$ref": v.Ref}, evidence)
		if err != nil {
			errors = append(errors, "values ref가 존재하지 않는다: "+v.Ref)
			continue
		}
		switch x := real.(type) {
		case json.Number, bool:
			found := number.FindString(v.Text)
			want := jsonx.Num(x)
			if b, ok := x.(bool); ok {
				want = 0
				if b {
					want = 1
				}
			}
			if found == "" || asciiNumber(found) != want {
				errors = append(errors, "문장 수치와 근거 불일치: "+v.Text+" / "+pyText(real))
			}
		case string:
			if !strings.Contains(textx.Squeeze(x), textx.Squeeze(v.Text)) && !strings.Contains(v.Text, x) {
				errors = append(errors, "시각/문자 값과 근거 불일치: "+v.Text)
			}
		}
	}
	return errors
}

// cell은 조회 값의 칸 하나다.
type cell struct {
	path  string
	value any
}

// cells는 값 아래의 칸을 경로 순서로 모은다.
func cells(path string, value any, out *[]cell) {
	switch x := value.(type) {
	case J:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			cells(path+"/"+k, x[k], out)
		}
	case []any:
		for i, item := range x {
			cells(path+"/"+strconv.Itoa(i), item, out)
		}
	case json.Number, string:
		*out = append(*out, cell{path, x})
	}
}

// bindValues는 답 문장의 수치 가운데 values에 없는 것을 그 todo의 조회 값에서 찾아 values에 잇는다. 값이 같은 칸이 여럿이면 경로 순서로
// 첫 칸이 근거다. 시각은 그 시각이 적힌 글 칸(시각 하나 또는 시각 목록)을 근거로 삼는다. 조회 값에 없는 수치는 그대로 두어, 검사가 다시 쓰게 한다.
func bindValues(a *Answer, evidence J, owner map[string]map[string]bool, quoted map[string]bool) {
	declared := map[string]bool{}
	for _, v := range a.Values {
		declared[v.Text] = true
	}
	var owned []cell
	loaded := false
	for _, numeric := range Numeric.FindAllString(a.Text, -1) {
		if declared[numeric] || quoted[textx.Squeeze(numeric)] {
			continue
		}
		if !loaded {
			loaded = true
			ids := []string{}
			for id := range evidence {
				if owner[id][a.Todo] {
					ids = append(ids, id)
				}
			}
			sort.Strings(ids)
			for _, id := range ids {
				cells(id+"/data", jsonx.Obj(evidence[id])["data"], &owned)
			}
		}
		for _, c := range owned {
			same := false
			switch x := c.value.(type) {
			case json.Number:
				found := number.FindString(numeric)
				same = !strings.Contains(numeric, ":") && found != "" && asciiNumber(found) == jsonx.Num(x)
			case string:
				same = strings.Contains(numeric, ":") && strings.Contains(textx.Squeeze(x), textx.Squeeze(numeric))
			}
			if same {
				a.Values = append(a.Values, ValueReference{Ref: c.path, Text: numeric})
				declared[numeric] = true
				break
			}
		}
	}
}

// Owners는 근거 ID마다 그 근거가 속한 todo id들이다.
func Owners(executed J) map[string]map[string]bool {
	owner := map[string]map[string]bool{}
	add := func(evidence, todo string) {
		if owner[evidence] == nil {
			owner[evidence] = map[string]bool{}
		}
		owner[evidence][todo] = true
	}
	for _, x := range jsonx.List(executed["todos"]) {
		t := jsonx.Obj(x)
		for _, r := range runs(t) {
			for _, q := range requests(r) {
				add(jsonx.Str(q["evidence"]), jsonx.Str(t["id"]))
			}
		}
		for _, c := range jsonx.List(t["comparisons"]) {
			add(jsonx.Str(jsonx.Obj(c)["evidence"]), jsonx.Str(t["id"]))
		}
	}
	return owner
}

func sortedSet(set map[string]bool) []string {
	out := []string{}
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var overColumn = regexp.MustCompile(`"over_([0-9]+)"`)

// Issues는 답과 페이지가 검사를 통과하지 못한 곳이다. source는 원문과 todo의 질문이다. 그 안의 수치는 근거 없이 답에 쓸 수 있다.
// 답 문장의 수치 가운데 values에 없는 것은 먼저 그 todo의 조회 값에서 찾아 잇는다(bindValues).
func Issues(out *Composition, executed J, blocks map[string][]J, owner map[string]map[string]bool, source string) []string {
	todos, unmatched := []string{}, map[string]bool{}
	for _, x := range jsonx.List(executed["todos"]) {
		t := jsonx.Obj(x)
		todos = append(todos, jsonx.Str(t["id"]))
		if dayUnmatched(t) {
			unmatched[jsonx.Str(t["id"])] = true
		}
	}
	errors := []string{}
	// 질문이 기준으로 삼은 원문의 수치(60분을 넘는 간격)는 근거 없이 쓸 수 있다. 자료에서 읽은 값만 근거 경로가 필요하다.
	quoted := map[string]bool{}
	for _, x := range Numeric.FindAllString(source, -1) {
		quoted[textx.Squeeze(x)] = true
	}
	// 조회 값의 over_N 열은 원문 수치에서 온 기준(N분)이므로 그 기준값도 근거 없이 쓸 수 있다.
	if data, err := json.Marshal(executed["evidence"]); err == nil {
		for _, m := range overColumn.FindAllSubmatch(data, -1) {
			quoted[string(m[1])+"분"] = true
		}
	}
	given := []string{}
	for _, a := range out.Answers {
		given = append(given, a.Todo)
	}
	missing := []string{}
	for _, t := range todos {
		if !slices.Contains(given, t) {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		errors = append(errors, "답이 없는 todo: "+pyRepr(missing))
	}
	unknown, twice := map[string]bool{}, map[string]bool{}
	counts := map[string]int{}
	for _, t := range given {
		counts[t]++
		if !slices.Contains(todos, t) {
			unknown[t] = true
		}
		if counts[t] > 1 {
			twice[t] = true
		}
	}
	if len(unknown) > 0 {
		errors = append(errors, "받지 않은 todo: "+pyRepr(sortedSet(unknown)))
	}
	if len(twice) > 0 {
		errors = append(errors, "답이 둘 이상인 todo: "+pyRepr(sortedSet(twice)))
	}
	answered := map[string]bool{}
	for _, a := range out.Answers {
		if a.Status == "답함" {
			answered[a.Todo] = true
		}
	}
	evidence := jsonx.Obj(executed["evidence"])
	for i := range out.Answers {
		bindValues(&out.Answers[i], evidence, owner, quoted)
	}
	for _, a := range out.Answers {
		foreign := map[string]bool{}
		for _, v := range a.Values {
			if e := strings.SplitN(v.Ref, "/", 2)[0]; !owner[e][a.Todo] {
				foreign[e] = true
			}
		}
		if len(foreign) > 0 {
			errors = append(errors, a.Todo+": 이 todo의 조회 값이 아니다: "+pyRepr(sortedSet(foreign)))
		}
		for _, e := range ValueErrors(a.Text, a.Values, evidence, quoted) {
			errors = append(errors, a.Todo+": "+e)
		}
		if textx.Len(a.Text) >= answerLength {
			errors = append(errors, a.Todo+": 답이 길이 상한에서 잘렸다. 질문에 답하는 값만 한두 문장으로 쓴다")
		}
		if unmatched[a.Todo] && a.Status == "답함" && !dayWords.MatchString(a.Text) {
			errors = append(errors, a.Todo+": 자료의 날이 질문의 요일과 다르거나 요일이 수록되지 않았다. 답에 그 사실을 쓰거나 확인할 수 없음으로 둔다")
		}
	}
	for i, p := range out.Pages {
		n := i + 1
		pointsAtAnswer := false
		for _, t := range p.Todos {
			pointsAtAnswer = pointsAtAnswer || answered[t]
		}
		if !pointsAtAnswer {
			errors = append(errors, fmt.Sprintf("페이지 %d: 답한 todo를 하나 이상 가리킨다 (지금 %s)", n, pyRepr(p.Todos)))
		}
		var notGiven []string
		allowed := map[string]bool{}
		for _, t := range p.Todos {
			if !slices.Contains(todos, t) {
				notGiven = append(notGiven, t)
			}
			for _, b := range blocks[t] {
				allowed[jsonx.Str(b["id"])] = true
			}
		}
		if len(notGiven) > 0 {
			errors = append(errors, fmt.Sprintf("페이지 %d: 받지 않은 todo %s", n, pyRepr(notGiven)))
		}
		if len(p.Blocks) == 0 {
			errors = append(errors, fmt.Sprintf("페이지 %d: 보일 블록 후보가 필요하다", n))
		}
		var outside []string
		seen := map[string]bool{}
		repeated := false
		for _, b := range p.Blocks {
			if !allowed[b] {
				outside = append(outside, b)
			}
			repeated = repeated || seen[b]
			seen[b] = true
		}
		if len(outside) > 0 {
			errors = append(errors, fmt.Sprintf("페이지 %d: 이 페이지 todo의 블록 후보가 아니다: %s", n, pyRepr(outside)))
		}
		if repeated {
			errors = append(errors, fmt.Sprintf("페이지 %d: 같은 블록을 두 번 두었다", n))
		}
	}
	visible := []string{}
	for _, a := range out.Answers {
		visible = append(visible, a.Text)
	}
	for _, p := range out.Pages {
		visible = append(visible, p.Title)
	}
	for _, phrase := range internalPhrases {
		if strings.Contains(source, phrase) {
			continue
		}
		for _, s := range visible {
			if strings.Contains(s, phrase) {
				errors = append(errors, "제품 표현에 내부 해명을 추가하지 않는다: "+phrase)
				break
			}
		}
	}
	return errors
}

// Check는 Issues를 모델에게 돌려줄 글로 바꾼다. 한 번에 15개까지 보인다.
// cannotAnswer는 답 문장이 질문에 답하지 못한다고 말하는 표현이다.
var cannotAnswer = regexp.MustCompile(`답할 수 없|확인할 수 없|답하지 못|알 수 없습니다`)

// ZeroAnswers는 todo마다 조회에 행이 없을 때의 답이다. 시기(개편 전·후), 노선, 시간대를 계획의 조회 조건과 실행 기록에서 읽어
// "개편 후 1715번의 07:20~08:20 운행은 0편입니다."처럼 쓴다.
func ZeroAnswers(plan, executed J) map[string]string {
	periods := map[string][]string{}
	for _, x := range jsonx.List(executed["todos"]) {
		t := jsonx.Obj(x)
		for _, r := range jsonx.List(t["runs"]) {
			label := map[string]string{"before": "개편 전", "after": "개편 후"}[jsonx.Str(jsonx.Obj(r)["revision"])]
			if label != "" && !slices.Contains(periods[jsonx.Str(t["id"])], label) {
				periods[jsonx.Str(t["id"])] = append(periods[jsonx.Str(t["id"])], label)
			}
		}
	}
	out := map[string]string{}
	for _, x := range jsonx.List(plan["todos"]) {
		t := jsonx.Obj(x)
		id, method := jsonx.Str(t["id"]), jsonx.Obj(t["method"])
		parts := []string{}
		if p := periods[id]; len(p) == 1 {
			parts = append(parts, p[0])
		} else if len(p) > 1 {
			parts = append(parts, "개편 전후 모두")
		}
		routes := []string{}
		for _, r := range jsonx.List(method["routes"]) {
			if s := jsonx.Str(r); s != "" {
				routes = append(routes, s)
			}
		}
		subject := "조회한 정류장의"
		if len(routes) > 0 {
			subject = strings.Join(routes, "·") + "번의"
		}
		parts = append(parts, subject)
		window := jsonx.Obj(method["window"])
		// 시각은 질문에 적힌 것만 쓴다. 질문에 없는 시각은 근거 없이 쓸 수 없다.
		question := jsonx.Str(t["question"])
		switch start, end := jsonx.Str(window["start"]), jsonx.Str(window["end"]); {
		case start == "" || end == "" || start == "00:00" && end == "24:00":
			parts = append(parts, "하루")
		case strings.Contains(question, start) && strings.Contains(question, end):
			parts = append(parts, start+"~"+end)
		default:
			parts = append(parts, "조회한 시간대")
		}
		out[id] = strings.Join(parts, " ") + " 운행은 0편입니다."
	}
	return out
}

// allEmpty는 todo의 조회 값이 하나 이상 있고 모두 행이 없는 표인지다.
func allEmpty(todo string, evidence J, owner map[string]map[string]bool) bool {
	found := false
	for id, todos := range owner {
		if !todos[todo] {
			continue
		}
		data := jsonx.Obj(jsonx.Obj(evidence[id])["data"])
		rows, ok := data["rows"].([]any)
		if !ok || len(rows) > 0 {
			return false
		}
		found = true
	}
	return found
}

// copiedLabel은 모델이 입력의 표지(완료 조건의 이름, 상태 값)를 답 문장 앞에 옮겨 쓴 머리다.
var copiedLabel = regexp.MustCompile(`^\s*(답한 것으로 볼 결과|답함입니다|답함|확인할 수 없음입니다|확인할 수 없음|답)\s*[:：.]\s*`)

// answerSentence는 답 문장의 한 문장이다. 마침표·물음표·느낌표 뒤에 공백이나 글의 끝이 올 때 끊으므로 21.1분의 소수점에서는 끊지 않는다.
var answerSentence = regexp.MustCompile(`(?s).*?[.?!](?:\s+|$)|.+$`)

// judgesCitizen은 답 문장이 시민의 말을 자료로 맞다·틀리다 따지는 꼴이다("민원 내용과 달리", "시민 말과 달리", "민원인은 …했으나").
var judgesCitizen = regexp.MustCompile(`(민원|시민|원문)[^.]{0,20}(와|과) (달리|같은|일치)|민원 내용(과|대로|처럼)|(라는|다는) (민원|시민)|^민원인은|시민의 [^.]{0,30}불편은|민원인이 말한[^.]{0,30}(아니|않)|라고 쓰지 않`)

// Unhedged는 답에서 완료 조건을 옮겨 쓴 문장("…이 나오면 이 질문에 답한 것이다"), '다만'으로 시작하는 덧붙임 문장, 시민의 말을
// 자료로 판정하는 문장을 뺀다.
// 남는 문장이 없으면 그대로 돌려준다.
func Unhedged(text string) string {
	kept := []string{}
	for _, s := range answerSentence.FindAllString(text, -1) {
		trimmed := strings.TrimSpace(s)
		if trimmed == "" || strings.Contains(trimmed, "답한 것이") || strings.Contains(trimmed, "답한 것으로 본") || strings.HasPrefix(trimmed, "다만") ||
			judgesCitizen.MatchString(trimmed) {
			continue
		}
		kept = append(kept, trimmed)
	}
	if len(kept) == 0 {
		return strings.TrimSpace(text)
	}
	return strings.Join(kept, " ")
}

// zero는 todo마다 그 todo의 조회에 행이 없을 때 쓸 답이다(ZeroAnswers).
func Check(executed J, blocks map[string][]J, source string, zero ...map[string]string) func(*Composition) string {
	owner := Owners(executed)
	return func(out *Composition) string {
		evidence := jsonx.Obj(executed["evidence"])
		// 같은 todo에 거듭 쓴 답은 첫 답만 남긴다.
		answered, answers := map[string]bool{}, []Answer{}
		for _, a := range out.Answers {
			if !answered[a.Todo] {
				answered[a.Todo] = true
				answers = append(answers, a)
			}
		}
		out.Answers = answers
		for i := range out.Answers {
			out.Answers[i].Text = Unhedged(copiedLabel.ReplaceAllString(out.Answers[i].Text, ""))
			// 없는 칸, 문장의 값과 다른 값의 칸, 다른 todo의 조회 값을 가리킨 근거 경로는 지운다. 문장의 수치는 그 todo의 조회 값에서
			// 같은 값의 칸으로 다시 잇는다.
			kept := []ValueReference{}
			for _, v := range out.Answers[i].Values {
				if owner[strings.SplitN(v.Ref, "/", 2)[0]][out.Answers[i].Todo] && len(ValueErrors("", []ValueReference{v}, evidence, nil)) == 0 {
					kept = append(kept, v)
				}
			}
			out.Answers[i].Values = kept
			// 답할 수 없다고 쓴 답은 '확인할 수 없음'이다.
			if out.Answers[i].Status == "답함" && cannotAnswer.MatchString(out.Answers[i].Text) {
				out.Answers[i].Status = "확인할 수 없음"
				out.Answers[i].Values = []ValueReference{}
			}
		}
		// 확인할 수 없음으로 둔 todo라도 그 todo의 조회가 모두 실행됐고 행이 없으면 답은 운행 0편이다.
		for i := range out.Answers {
			if out.Answers[i].Status == "답함" || !allEmpty(out.Answers[i].Todo, evidence, owner) {
				continue
			}
			out.Answers[i].Status = "답함"
			out.Answers[i].Text = "조회한 노선·정류장·시간대에 해당하는 운행은 0편입니다."
			if len(zero) > 0 && zero[0][out.Answers[i].Todo] != "" {
				out.Answers[i].Text = zero[0][out.Answers[i].Todo]
			}
			out.Answers[i].Values = []ValueReference{}
		}
		// 블록을 하나도 고르지 않은 페이지는 보일 것이 없으므로 뺀다(조회 값이 비어 블록 후보가 없는 todo의 페이지).
		withBlocks := []Page{}
		for _, p := range out.Pages {
			if len(p.Blocks) > 0 {
				withBlocks = append(withBlocks, p)
			}
		}
		out.Pages = withBlocks
		// 확인할 수 없음인 todo는 페이지에 보이지 않는다. 그 todo와 그 블록을 페이지에서 빼고, 남는 todo가 없는 페이지는 뺀다.
		unanswered := map[string]bool{}
		for _, a := range out.Answers {
			if a.Status != "답함" {
				unanswered[a.Todo] = true
			}
		}
		if len(unanswered) > 0 {
			pages := []Page{}
			for _, p := range out.Pages {
				todos, shown := []string{}, []string{}
				for _, id := range p.Todos {
					if !unanswered[id] {
						todos = append(todos, id)
					}
				}
				for _, b := range p.Blocks {
					if owner, _, ok := strings.Cut(b, "."); !ok || !unanswered[owner] {
						shown = append(shown, b)
					}
				}
				if len(todos) > 0 && len(shown) > 0 {
					p.Todos, p.Blocks = todos, shown
					pages = append(pages, p)
				}
			}
			out.Pages = pages
		}
		found := Issues(out, executed, blocks, owner, source)
		if len(found) == 0 {
			return ""
		}
		return "답과 페이지를 고친다:\n" + strings.Join(found[:min(len(found), 15)], "\n")
	}
}

// Config는 Run의 입력이다.
type Config struct {
	Root, Run, Out, ModelName string
	Cases                     []string
	Workers                   int
	Writer                    *matchi.Writer
	Report                    func(line string)
}

func readStage(run, stage, id string) (J, []byte, error) {
	data, err := os.ReadFile(filepath.Join(run, stage, id+".json"))
	if err != nil {
		return nil, nil, err
	}
	parsed, err := jsonx.Parse(data)
	if err != nil {
		return nil, nil, err
	}
	return jsonx.Obj(parsed), data, nil
}

// Run은 민원마다 답을 쓰고 뷰어 입력을 만든다. Out이 비면 Run 폴더에 쓴다. 민원은 Workers개씩 함께 처리하고, 0이면 차례로 처리한다.
func Run(ctx context.Context, c Config) error {
	prompt, err := repo.ReadText(filepath.Join(c.Root, "prompts", "analysis", "compose.md"))
	if err != nil {
		return err
	}
	prior, err := repo.ReadText(filepath.Join(repo.RegionDir(c.Root), "prior.md"))
	if err != nil {
		return err
	}
	datesText, err := repo.ReadText(filepath.Join(repo.RegionDir(c.Root), "complaints", "dates.json"))
	if err != nil {
		return err
	}
	var received map[string]string
	if err := json.Unmarshal([]byte(datesText), &received); err != nil {
		return err
	}
	cal, err := calendar.Load(c.Root)
	if err != nil {
		return err
	}
	days, err := cal.RevisionDays(c.Root)
	if err != nil {
		return err
	}
	out := c.Out
	if out == "" {
		out = c.Run
	}
	cases := c.Cases
	if len(cases) == 0 {
		if cases, err = repo.Stems(filepath.Join(c.Run, "plan")); err != nil {
			return err
		}
	}
	// 민원마다 따로 쓰고, 실패한 민원은 그 민원의 기록 줄을 남긴다. 오류로는 첫 실패를 돌려준다.
	return repo.ForEach(cases, c.Workers, func(id string) error {
		if err := one(ctx, c, out, id, prompt, prior, received[id], days["after"].Note); err != nil {
			if c.Report != nil {
				if line, merr := jsonx.Marshal(jsonx.Object{{Key: "id", Value: id}, {Key: "error", Value: err.Error()}}); merr == nil {
					c.Report(string(line))
				}
			}
			return fmt.Errorf("민원 %s: %w", id, err)
		}
		return nil
	})
}

func one(ctx context.Context, c Config, out, id, prompt, prior, received, dataDay string) error {
	planRecord, _, err := readStage(c.Run, "plan", id)
	if err != nil {
		return err
	}
	cands, _, err := readStage(c.Run, "candidates", id)
	if err != nil {
		return err
	}
	executed, raw, err := readStage(c.Run, "execute", id)
	if err != nil {
		return err
	}
	ordered, err := jsonx.ParseOrdered(raw)
	if err != nil {
		return err
	}
	orderedEvidence := map[string]jsonx.Object{}
	for _, f := range asObject(asObject(ordered).Get("evidence")) {
		orderedEvidence[f.Key] = asObject(f.Value)
	}
	source, err := repo.SourceText(c.Root, id)
	if err != nil {
		return err
	}
	plan := jsonx.Obj(planRecord["plan"])
	todos := map[string]J{}
	// 질문과 그 완료 조건에 적힌 수치(여정의 4시간 한도 같은 조회 조건)는 원문의 수치처럼 근거 없이 답에 쓸 수 있다.
	asked := source
	for _, x := range jsonx.List(plan["todos"]) {
		todos[jsonx.Str(jsonx.Obj(x)["id"])] = jsonx.Obj(x)
		asked += "\n" + jsonx.Str(jsonx.Obj(x)["question"]) + "\n" + jsonx.Str(jsonx.Obj(x)["done_when"])
	}
	mentions := byID(jsonx.List(cands["mentions"]))
	evidence := jsonx.Obj(executed["evidence"])
	blocks, blockOrder := map[string][]J{}, jsonx.Object{}
	for _, x := range jsonx.List(executed["todos"]) {
		done := jsonx.Obj(x)
		todo := todos[jsonx.Str(done["id"])]
		list := BlockCandidates(todo, done, evidence, RouteLabel(jsonx.Obj(todo["method"]), mentions))
		blocks[jsonx.Str(done["id"])] = list
		blockOrder = append(blockOrder, jsonx.Field{Key: jsonx.Str(done["id"]), Value: list})
	}
	planHash, err := jsonx.Digest(plan)
	if err != nil {
		return err
	}
	executeHash, err := jsonx.Digest(executed["todos"])
	if err != nil {
		return err
	}
	record := jsonx.Object{{Key: "id", Value: id}, {Key: "createdAt", Value: time.Now().Format(time.RFC3339Nano)}, {Key: "planHash", Value: planHash},
		{Key: "executeHash", Value: executeHash}, {Key: "prompt", Value: jsonx.SHA(prompt)}, {Key: "candidates", Value: blockOrder}}
	var composition any = J{"answers": []any{}, "pages": []any{}, "decisions": []any{}}
	var model any
	if len(todos) > 0 {
		input := Request(Inputs{Source: source, Received: received, Prior: prior, DataDay: dataDay, Plan: plan, Candidates: cands, Executed: executed,
			Ordered: orderedEvidence, Blocks: blocks})
		result, messages, err := matchi.Write(matchi.WithScope(ctx, id, "compose"), c.Writer, matchi.Spec[Composition]{
			Name: "Composition", Instructions: prompt, Input: input, Check: Check(executed, blocks, asked, ZeroAnswers(plan, executed)), Retries: retries})
		if err != nil {
			return err
		}
		model = c.ModelName
		if composition, err = jsonx.Generic(result); err != nil {
			return err
		}
		record = append(record, jsonx.Field{Key: "model", Value: model}, jsonx.Field{Key: "effort", Value: c.Writer.Effort}, jsonx.Field{Key: "messages", Value: messages})
	} else {
		record = append(record, jsonx.Field{Key: "model", Value: nil})
	}
	record = append(record, jsonx.Field{Key: "composition", Value: composition})
	if err := jsonx.WriteFile(filepath.Join(out, "compose", id+".json"), record); err != nil {
		return err
	}
	l2, _, err := readStage(c.Run, "l2", id)
	if err != nil {
		return err
	}
	ext, _, err := readStage(c.Run, "extract", id)
	if err != nil {
		return err
	}
	shown, err := View(ViewInput{Case: id, Source: source, Received: received, L2: l2, Extract: ext, Candidates: cands, Plan: planRecord,
		Composition: jsonx.Obj(composition), Blocks: blocks, Evidence: evidence, Model: model})
	if err != nil {
		return err
	}
	if err := jsonx.WriteFile(filepath.Join(out, "view", id+".json"), shown); err != nil {
		return err
	}
	if c.Report != nil {
		answers, pages := jsonx.Object{}, []string{}
		for _, a := range jsonx.List(jsonx.Obj(composition)["answers"]) {
			answers = append(answers, jsonx.Field{Key: jsonx.Str(jsonx.Obj(a)["todo"]), Value: jsonx.Obj(a)["status"]})
		}
		for _, p := range jsonx.List(jsonx.Obj(composition)["pages"]) {
			pages = append(pages, fmt.Sprintf("%s %s", jsonx.Str(jsonx.Obj(p)["title"]), pyRepr(jsonx.List(jsonx.Obj(p)["blocks"]))))
		}
		line, err := jsonx.Marshal(jsonx.Object{{Key: "id", Value: id}, {Key: "answers", Value: answers}, {Key: "pages", Value: pages}})
		if err != nil {
			return err
		}
		c.Report(string(line))
	}
	return nil
}
