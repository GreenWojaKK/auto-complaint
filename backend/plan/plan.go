// Package plan은 추출한 지칭마다 후보를 붙이고 이동마다 조회 선택지를 만든 뒤(후보 준비), 판정 모델이 실행할 조회를 고르고
// 언어 모델이 L1과 조회마다 질문을 쓴다(계획).
//
// 후보 준비는 교통 조회 도구와 PostgreSQL에서 다음을 붙인다.
//   - 장소 지칭: 장소 검색 상위 후보(행정동, 그곳에 서는 노선). 지역 prior("시내")가 붙은 지칭은 그 행정동의 정류장 지역 후보.
//     판정 모델이 장소 검색 후보마다 정류장 이름과 좌표를 보고 그 지칭의 후보가 될 수 있는지 판정하고, 아니면 뺀다.
//     판정이 애매한 후보만 행정동까지 주고 다시 묻는다.
//   - 노선 번호: 접수일의 자료 판에 그 번호가 있는지, 개편 전 자료에 어떤 번호로 수록되어 있는지.
//   - 시간 지칭: 원문에 적힌 시각, 시간 사전의 범위, 달력의 요일 종류.
//
// 시간대나 노선이 비어 있는 이동은 판정 모델이 같은 민원의 다른 이동에 적힌 시간대·노선이 그 이동의 것인지 판정한다(links.go).
// 조회 선택지도 코드가 만든다. 문제의 이동마다 칸을 두고, 그 이동의 노선·장소·시간대로 가능한 조회를 조건까지 정해 둔다.
// 판정 모델이 선택지마다 실행할지 판정한다. 예 확률이 기준값 이상인 선택지가 실행할 조회다. 후보가 여럿이면 코드가 판정 점수가 가장 높은 후보를 쓴다.
// 계획은 언어 모델 호출 한 번이다. 모델은 L1을 쓰고, 실행할 조회마다 질문을 쓴다.
// 코드는 실행할 조회를 모두 한 번씩 다뤘는지 검사해 어긋나면 다시 쓰게 하고, 고른 선택지를 todo로 푼다.
// 결과는 <run>/candidates/<번호>.json과 <run>/plan/<번호>.json에 쓴다.
package plan

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"dllio/backend/calendar"
	"dllio/backend/execute"
	"dllio/backend/extract"
	"dllio/backend/jsonx"
	"dllio/backend/matchi"
	"dllio/backend/problems"
	"dllio/backend/repo"
	"dllio/backend/tools"
)

// L1은 민원의 요약이다.
type L1 struct {
	Headline  string   `json:"headline"`
	Situation []string `json:"situation" min:"1" max:"4"`
	Requests  []string `json:"requests"`
	Replies   []string `json:"replies"`
}

// Range는 모델이 정한 시간대다.
type Range struct {
	Start  string `json:"start" desc:"HH:MM"`
	End    string `json:"end" desc:"HH:MM"`
	Reason string `json:"reason"`
}

// Check는 실행할 조회 하나의 질문이다.
type Check struct {
	Option   string `json:"option" desc:"실행할 조회 id(q…)"`
	Kind     string `json:"kind" enum:"현황,요구 검토"`
	Question string `json:"question"`
	DoneWhen string `json:"done_when"`
	Window   *Range `json:"window" desc:"범위를 정할 수 있다고 표시된 조회에서 범위를 정할 때만. 아니면 null"`
}

// PlanChoice는 모델의 출력 형식이다.
type PlanChoice struct {
	L1     L1      `json:"l1"`
	Checks []Check `json:"checks"`
}

// picked는 지칭마다 정류장 조회와 여정에 쓸 후보다. 동네 이름인 지칭은 정류장 조회에서 그 지역 전체다. 장소 후보가 여럿이면 판정 점수가 가장 높은 후보다.
func picked(b *Built, cands J) (map[string]string, map[string]string) {
	stops, places := map[string]string{}, map[string]string{}
	revision := jsonx.Str(cands["revision"])
	score := func(c J) float64 {
		if v, ok := c["areaFit"]; ok {
			return jsonx.Num(v)
		}
		if v, ok := c["fit"]; ok {
			return jsonx.Num(v)
		}
		return 0
	}
	for _, m := range mentionsOf(cands) {
		id := jsonx.Str(m["id"])
		if b.Canon[id] != id {
			continue
		}
		var areas []J
		var best J
		for _, c := range usable(m, revision) {
			if kind := jsonx.Str(c["kind"]); kind == "area" || kind == "nearby" {
				areas = append(areas, c)
			} else if best == nil || score(c) > score(best) {
				best = c
			}
		}
		if best != nil {
			places[id] = jsonx.Str(best["id"])
		}
		if len(areas) > 0 {
			stops[id] = jsonx.Str(areas[0]["id"])
		} else if best != nil {
			stops[id] = jsonx.Str(best["id"])
		}
	}
	return stops, places
}

// PlanIssues는 실행할 조회마다 항목이 하나씩 있는지, 정할 수 있는 조회의 시간대 범위가 맞는지 본다. 시간대가 정해진 조회에 쓴 범위는 코드가 무시한다.
func PlanIssues(choice *PlanChoice, b *Built) []string {
	options := map[string]J{}
	selected := SelectedOptions(b)
	for _, o := range selected {
		options[jsonx.Str(o["id"])] = o
	}
	found, written := []string{}, []string{}
	for _, c := range choice.Checks {
		o, ok := options[c.Option]
		if !ok {
			found = append(found, c.Option+"은 실행할 조회가 아니다")
			continue
		}
		for _, w := range written {
			if w == c.Option {
				found = append(found, c.Option+"을 두 번 썼다")
				break
			}
		}
		written = append(written, c.Option)
		if c.Window != nil && jsonx.Obj(o["window"])["ask"] == true {
			start, okStart := execute.Minutes(c.Window.Start)
			end, okEnd := execute.Minutes(c.Window.End)
			if !okStart || !okEnd || start >= end {
				found = append(found, fmt.Sprintf("%s: 시간대 %s–%s가 HH:MM 순서가 아니다", c.Option, c.Window.Start, c.Window.End))
			}
		}
	}
	for _, o := range selected {
		id, done := jsonx.Str(o["id"]), false
		for _, w := range written {
			done = done || w == id
		}
		if !done {
			found = append(found, id+"의 질문을 쓴다")
		}
	}
	return found
}

func checkNumber(c Check) int {
	var n int
	fmt.Sscanf(strings.TrimPrefix(c.Option, "q"), "%d", &n)
	return n
}

// Assemble은 실행할 조회를 todo로 푼다. todo 순서는 선택지 순서다. 실행할 조회가 없는 칸은 실행 기록에 남긴다.
func Assemble(choice *PlanChoice, b *Built, cands, ext J) J {
	selected := SelectedOptions(b)
	options, slots, byID := map[string]J{}, map[string]J{}, map[string]J{}
	for _, o := range selected {
		options[jsonx.Str(o["id"])] = o
	}
	for _, s := range b.Slots {
		slots[jsonx.Str(s["id"])] = s
	}
	for _, m := range mentionsOf(cands) {
		byID[jsonx.Str(m["id"])] = m
	}
	stops, places := picked(b, cands)
	each := func(ids []string, table map[string]string) []string {
		out := []string{}
		for _, i := range ids {
			out = append(out, table[i])
		}
		return out
	}
	checks := append([]Check{}, choice.Checks...)
	sort.SliceStable(checks, func(i, j int) bool { return checkNumber(checks[i]) < checkNumber(checks[j]) })
	todos := []J{}
	for _, c := range checks {
		o := options[c.Option]
		optionSlots := stringsOf(o["slots"])
		slot := slots[optionSlots[0]]
		var window J
		if w := jsonx.Obj(o["window"]); w != nil {
			window = J{}
			for k, v := range w {
				window[k] = v
			}
			if w["ask"] == true && c.Window != nil {
				window["start"], window["end"], window["reason"] = c.Window.Start, c.Window.End, c.Window.Reason
			}
		}
		var method J
		switch kind := jsonx.Str(o["method"]); kind {
		case "departures":
			method = J{"method": kind, "boarding": each(stringsOf(o["boarding"]), stops), "destination": each(stringsOf(o["destination"]), stops), "routes": o["routes"],
				"window": window, "time_kind": "departure", "direct": len(stringsOf(o["destination"])) > 0, "gap_over": o["gap_over"], "gap_columns": o["gap_columns"]}
			if o["at_destination"] == true {
				method["at_destination"], method["time_kind"] = true, "arrival"
			}
		case "route_departures":
			method = J{"method": kind, "routes": o["routes"], "window": window, "gap_over": o["gap_over"], "gap_columns": o["gap_columns"]}
		case "journeys":
			method = J{"method": kind, "origin": places[jsonx.Str(o["origin"])], "destination": places[jsonx.Str(o["destination"])], "routes": o["routes"],
				"window": window, "arrive_by": window["arrive_by"], "walk_meters": 600, "transfers": 1}
		case "segments":
			method = J{"method": kind, "origin": each(stringsOf(o["origin"]), stops), "destination": each(stringsOf(o["destination"]), stops), "routes": o["routes"], "window": window}
		default:
			method = J{"method": kind, "routes": o["routes"], "stops": o["stops"]}
		}
		if p := jsonx.Str(o["period"]); p == "compare" || p == "before" {
			method["before_routes"] = o["before_routes"]
		}
		quotes := []string{}
		for _, s := range optionSlots {
			for _, m := range stringsOf(slots[s]["mentions"]) {
				quotes = uniqueAppend(quotes, jsonx.Str(byID[m]["quote"]))
			}
		}
		var grounding any = quotes
		if len(quotes) == 0 {
			grounding = jsonx.Obj(jsonx.List(ext["problems"])[int(jsonx.Num(slot["problem_index"]))-1])["grounding"]
		}
		todos = append(todos, J{"id": fmt.Sprintf("t%d", len(todos)+1), "option": c.Option, "kind": c.Kind, "problem": slot["problem"], "move": slot["move"],
			"question": c.Question, "method": method, "period": o["period"], "compare_periods": o["period"] == "compare", "done_when": c.DoneWhen, "grounding": grounding})
	}
	covered, made := map[string]bool{}, map[string]bool{}
	for _, o := range selected {
		for _, s := range stringsOf(o["slots"]) {
			covered[s] = true
		}
	}
	for _, o := range b.Options {
		for _, s := range stringsOf(o["slots"]) {
			made[s] = true
		}
	}
	notCheckable := []J{}
	for _, s := range b.Slots {
		id := jsonx.Str(s["id"])
		if covered[id] {
			continue
		}
		reason := "이 이동으로 만들 수 있는 조회가 없음"
		if made[id] {
			reason = "판정을 통과한 조회가 없음"
		}
		notCheckable = append(notCheckable, J{"problem": s["problem"], "move": s["move"], "reason": reason})
	}
	return J{"l1": choice.L1, "todos": todos, "not_checkable": notCheckable}
}

func quoted(values []any) string {
	parts := []string{}
	for _, v := range values {
		parts = append(parts, "«"+jsonx.Str(v)+"»")
	}
	return strings.Join(parts, " / ")
}

// Request는 계획 모델에게 보내는 입력이다.
func Request(source string, day calendar.Day, cands, ext J, catalog map[string]problems.Entry, prior string, b *Built) string {
	lines := []string{fmt.Sprintf("민원 접수일: %s (조회 자료 판: %s)", day.Label, jsonx.Str(cands["revision"])), "", "원문:", source, "", "지역 prior:", prior, "", "조회가 돌려주는 값:"}
	for _, k := range methodOrder {
		lines = append(lines, "- "+MethodNames[k]+": "+MethodFacts[k])
	}
	lines = append(lines, "- 모든 조회: "+MethodLimits, "", "문제와 이동:")
	byID, every := map[string]J{}, map[string]J{}
	for _, m := range mentionsOf(cands) {
		byID[jsonx.Str(m["id"])] = m
		for _, c := range candidatesOf(m) {
			every[jsonx.Str(c["id"])] = c
		}
	}
	stops, places := picked(b, cands)
	chosen := func(o J) map[string]string {
		table := stops
		if o["method"] == "journeys" {
			table = places
		}
		out := map[string]string{}
		for i, c := range table {
			out[i] = jsonx.Str(every[c]["name"])
		}
		return out
	}
	selected := SelectedOptions(b)
	problemsList := jsonx.List(ext["problems"])
	for _, slot := range b.Slots {
		problem := jsonx.Obj(problemsList[int(jsonx.Num(slot["problem_index"]))-1])
		kind := jsonx.Str(slot["problem"])
		lines = append(lines, fmt.Sprintf("\n[%s] %s 이동 %v (개편 표시: %s): %s", jsonx.Str(slot["id"]), kind, slot["move"], jsonx.Str(slot["period"]), catalog[kind].Proposition))
		lines = append(lines, "근거: "+quoted(jsonx.List(problem["grounding"])))
		for _, i := range stringsOf(slot["mentions"]) {
			m := byID[i]
			text := fmt.Sprintf("  %s %s", jsonx.Str(m["role"]), MentionLabel(m))
			if r := jsonx.Obj(m["route"]); r != nil {
				text += fmt.Sprintf(" (적힌 그대로 «%s», 이 판에 있음=%s, 개편 전 수록 번호=%s)", jsonx.Str(m["written_as"]), formatBool(r["in_revision"] == true), pyList(stringsOf(r["before_numbers"])))
			}
			lines = append(lines, text)
		}
		var here []J
		for _, o := range selected {
			for _, s := range stringsOf(o["slots"]) {
				if s == jsonx.Str(slot["id"]) {
					here = append(here, o)
					break
				}
			}
		}
		if len(here) > 0 {
			lines = append(lines, "  실행할 조회:")
		} else {
			lines = append(lines, "  실행할 조회 없음")
		}
		for _, o := range here {
			lines = append(lines, "    "+OptionLabel(o, byID, chosen(o)))
		}
	}
	if noQuery := jsonx.Strs(ext["noQuery"]); len(noQuery) > 0 {
		grounding := map[string][]any{}
		for _, p := range problemsList {
			grounding[jsonx.Str(jsonx.Obj(p)["type"])] = jsonx.List(jsonx.Obj(p)["grounding"])
		}
		lines = append(lines, "", "조회하지 않는 문제(칸이 없다):")
		for _, t := range noQuery {
			lines = append(lines, "- "+t+": "+catalog[t].Proposition, "  근거: "+quoted(grounding[t]))
		}
	}
	if parts := jsonx.List(ext["noQueryAspects"]); len(parts) > 0 {
		lines = append(lines, "", "조회하지 않는 측면:")
		for _, p := range parts {
			lines = append(lines, "- "+jsonx.Str(jsonx.Obj(p)["type"])+": "+jsonx.Str(jsonx.Obj(p)["statement"]))
		}
	}
	return strings.Join(lines, "\n")
}

// IssuesCheck는 PlanIssues를 모델에게 돌려줄 글로 바꾼다.
func IssuesCheck(b *Built) func(*PlanChoice) string {
	return func(choice *PlanChoice) string {
		if found := PlanIssues(choice, b); len(found) > 0 {
			return "쓴 것을 고친다: " + strings.Join(found, "; ")
		}
		return ""
	}
}

// Config는 Run의 입력이다.
type Config struct {
	Root, Run, ModelName, DecisionName string
	Cases                              []string
	Workers                            int
	Tools                              *tools.Tools
	Decider                            *matchi.Decider
	Writer                             *matchi.Writer
	Map                                *MapSearch
	Report                             func(line string)
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

// Run은 추출 결과가 있는 민원마다 후보를 준비하고 계획을 써 결과 파일을 쓴다. Cases가 비면 전부다.
func Run(ctx context.Context, c Config) error {
	prompt, err := repo.ReadText(filepath.Join(c.Root, "prompts", "analysis", "plan.md"))
	if err != nil {
		return err
	}
	prior, err := repo.ReadText(filepath.Join(c.Root, "prompts", "analysis", "ulsan.md"))
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
	entries, _, err := problems.LoadCatalog(filepath.Join(c.Root, "prompts", "l2", "catalog.json"))
	if err != nil {
		return err
	}
	catalog := map[string]problems.Entry{}
	for _, e := range entries {
		catalog[e.Type] = e
	}
	cal, err := calendar.Load(c.Root)
	if err != nil {
		return err
	}
	priors, err := extract.LoadPriors(c.Root)
	if err != nil {
		return err
	}
	cases := c.Cases
	if len(cases) == 0 {
		if cases, err = repo.Stems(filepath.Join(c.Run, "extract")); err != nil {
			return err
		}
	}
	return repo.ForEach(cases, c.Workers, func(id string) error {
		if err := one(ctx, c, id, prompt, prior, received[id], catalog, cal, priors); err != nil {
			return fmt.Errorf("민원 %s: %w", id, err)
		}
		return nil
	})
}

func one(ctx context.Context, c Config, id, prompt, prior, received string, catalog map[string]problems.Entry, cal *calendar.Calendar, priors *extract.Priors) error {
	ext, err := readJSON(filepath.Join(c.Run, "extract", id+".json"))
	if err != nil {
		return err
	}
	source, err := repo.SourceText(c.Root, id)
	if err != nil {
		return err
	}
	cands, err := Candidates(ctx, c.Tools, cal, priors, id, ext, received)
	if err != nil {
		return err
	}
	placeMessages, err := judgePlaces(matchi.WithScope(ctx, id, "candidates"), lookup{ctx, c.Tools}, c.Decider, cands, source)
	if err != nil {
		return err
	}
	cands["placeDecisions"] = J{"model": c.DecisionName, "threshold": placeThreshold, "messages": placeMessages}
	if c.Map != nil {
		mapMessages, err := judgeMapPlaces(matchi.WithScope(ctx, id, "map-places"), lookup{ctx, c.Tools}, c.Decider, c.Map, cands, source)
		if err != nil {
			return err
		}
		cands["mapDecisions"] = J{"model": c.DecisionName, "threshold": mapThreshold, "messages": mapMessages}
	}
	linkMessages, err := judgeLinks(matchi.WithScope(ctx, id, "move-links"), c.Decider, cands, ext, catalog, source)
	if err != nil {
		return err
	}
	cands["linkDecisions"] = J{"model": c.DecisionName, "threshold": linkThreshold, "messages": linkMessages}
	routeMessages, err := judgeRoutes(matchi.WithScope(ctx, id, "route-before"), lookup{ctx, c.Tools}, c.Decider, cands, ext, source)
	if err != nil {
		return err
	}
	cands["routeDecisions"] = J{"model": c.DecisionName, "threshold": routeThreshold, "messages": routeMessages}
	if err := jsonx.WriteFile(filepath.Join(c.Run, "candidates", id+".json"), orderedCandidates(cands)); err != nil {
		return err
	}
	built, err := BuildOptions(cands, ext)
	if err != nil {
		return err
	}
	fitMessages, err := scoreOptions(matchi.WithScope(ctx, id, "option-fit"), c.Decider, built, cands, catalog, source)
	if err != nil {
		return err
	}
	day, err := cal.Info(received)
	if err != nil {
		return err
	}
	choice, messages, err := matchi.Write(matchi.WithScope(ctx, id, "plan"), c.Writer, matchi.Spec[PlanChoice]{
		Name: "PlanChoice", Instructions: prompt, Input: Request(source, day, cands, ext, catalog, prior, built), Check: IssuesCheck(built)})
	if err != nil {
		return err
	}
	plan := Assemble(choice, built, cands, ext)
	candidatesHash, err := digest(cands)
	if err != nil {
		return err
	}
	extractHash, err := jsonx.Digest(ext)
	if err != nil {
		return err
	}
	record := jsonx.Object{{Key: "id", Value: id}, {Key: "createdAt", Value: time.Now().Format(time.RFC3339Nano)}, {Key: "model", Value: c.ModelName},
		{Key: "effort", Value: c.Writer.Effort}, {Key: "revision", Value: cands["revision"]}, {Key: "received", Value: received},
		{Key: "candidatesHash", Value: candidatesHash}, {Key: "extractHash", Value: extractHash}, {Key: "prompt", Value: jsonx.SHA(prompt)},
		{Key: "slots", Value: built.Slots}, {Key: "options", Value: built.Options}, {Key: "choice", Value: choice}, {Key: "plan", Value: plan},
		{Key: "fitDecisions", Value: J{"model": c.DecisionName, "messages": fitMessages}}, {Key: "messages", Value: messages}}
	if err := jsonx.WriteFile(filepath.Join(c.Run, "plan", id+".json"), record); err != nil {
		return err
	}
	if c.Report != nil {
		todos, unchecked := []string{}, []string{}
		for _, t := range plan["todos"].([]J) {
			todos = append(todos, fmt.Sprintf("%s:%s:%s", t["id"], t["option"], jsonx.Obj(t["method"])["method"]))
		}
		for _, n := range plan["not_checkable"].([]J) {
			unchecked = append(unchecked, jsonx.Str(n["problem"]))
		}
		line, err := jsonx.Marshal(jsonx.Object{{Key: "id", Value: id}, {Key: "options", Value: len(built.Options)}, {Key: "todos", Value: todos}, {Key: "not_checkable", Value: unchecked}})
		if err != nil {
			return err
		}
		c.Report(string(line))
	}
	return nil
}

// orderedCandidates는 후보 준비 결과를 읽기 좋은 순서로 쓴다.
func orderedCandidates(cands J) jsonx.Object {
	out := jsonx.Object{}
	for _, k := range []string{"id", "received", "receivedDay", "revision", "mentions", "lineageCount", "extractHash", "placeDecisions", "mapDecisions", "moveLinks", "linkDecisions", "routeDecisions"} {
		out = append(out, jsonx.Field{Key: k, Value: cands[k]})
	}
	return out
}
