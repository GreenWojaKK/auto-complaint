// Package extract는 1단계가 고른 문제 유형마다 원문에서 출발지·도착지, 시간대, 대상을 뽑고 prior를 붙인다.
//
// 언어 모델은 원문에 쓰인 지칭과 근거 문장만 뽑는다. 원문에 없는 값은 다시 쓰게 한다. 조회하지 않는 유형은 근거만 남기고 이동을 비운다.
// 지시 표현(여기, 거기, 이곳 같은 말)인 장소 지칭은 언어 모델에게 그것이 가리키는 원문 지칭을 한 번 더 뽑게 한다(referent.go).
// 그 뒤 코드가 prior를 붙인다.
//   - 장소: data/priors/areas.json의 지칭("시내")이면 그 행정동 목록을 붙인다.
//   - 시간대: 원문에 적힌 시각(6시반 → 6:30)을 읽고, data/priors/times.json에 있는 표현이면 그 범위를 붙인다. 사전에 없는 표현의 범위는
//     비워 두고 다음 단계(계획)가 정한다.
//   - 노선: 원문의 노선 지칭에서 노선 번호를 읽고, 시간표 파일 시트 이름이 적은 이전 번호·이어받은 번호를 붙인다.
//
// 결과는 <run>/extract/<번호>.json에 쓴다.
package extract

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"dllio/backend/jsonx"
	"dllio/backend/matchi"
	"dllio/backend/problems"
	"dllio/backend/repo"
	"dllio/backend/textx"
)

// PlaceMention은 원문에 쓰인 장소 지칭이다.
type PlaceMention struct {
	Text  string `json:"text" desc:"원문에 쓰인 장소 지칭 그대로. 예: 시내, 북구 양정동, 태화강역"`
	Quote string `json:"quote" desc:"이 지칭이 나온 원문 문장의 발췌. 원문 그대로"`
}

// TimeMention은 원문에 쓰인 시각·시간대 표현이다.
type TimeMention struct {
	Text  string `json:"text" desc:"원문에 쓰인 시각·시간대 표현 그대로. 예: 출근 시간, 등하교 시간, 6시반, 막차"`
	Quote string `json:"quote" desc:"이 표현이 나온 원문 문장의 발췌. 원문 그대로"`
}

// ValueMention은 시민이 말한 수치다.
type ValueMention struct {
	Text  string `json:"text" desc:"시민이 말한 수치 그대로. 예: 104분, 20-30분 대기, 1개로 줄어, 7대"`
	Quote string `json:"quote" desc:"이 수치가 나온 원문 문장의 발췌. 원문 그대로"`
}

// TargetMention은 원문에 쓰인 노선·정류장·장소 지칭이다.
type TargetMention struct {
	Text   string `json:"text" desc:"원문에 쓰인 노선 번호·정류장·장소 지칭 그대로"`
	Kind   string `json:"kind" desc:"route: 노선, stop: 정류장, place: 장소·지역" enum:"route,stop,place"`
	Quote  string `json:"quote" desc:"이 지칭이 나온 원문 문장의 발췌. 원문 그대로"`
	Period string `json:"period" desc:"노선을 어느 시기의 번호로 말했는지. before: 개편 전 노선, after: 개편 뒤 노선, none: 말하지 않음이거나 노선이 아닌 대상" enum:"before,after,none"`
}

// Move는 문제가 걸린 이동 하나다.
type Move struct {
	Origin      []PlaceMention  `json:"origin" desc:"이용자가 출발하는 곳"`
	Destination []PlaceMention  `json:"destination" desc:"이용자가 가려는 곳"`
	Time        []TimeMention   `json:"time" desc:"이 이동이 걸린 하루 중의 때(시각·시간대). 시간의 길이는 넣지 않는다"`
	Values      []ValueMention  `json:"values" desc:"시민이 말한 수치: 배차 간격, 대기, 소요 시간, 편수·대수"`
	Targets     []TargetMention `json:"targets" desc:"이 이동의 대상인 노선·정류장·장소"`
	Period      string          `json:"period" desc:"before: 개편 전의 이동을 말함, after: 개편 뒤의 이동을 말함, compare: 개편 전과 지금을 비교하거나 개편으로 바뀌었다고 말함, none: 개편을 말하지 않음" enum:"before,after,compare,none"`
	PeriodQuote string          `json:"period_quote" desc:"period의 근거가 된 원문 문장의 발췌. none이면 빈 글"`
}

// Problem은 고른 유형 하나의 추출 결과다.
type Problem struct {
	Type      string   `json:"type" desc:"이 항목이 다루는 문제 유형"`
	Grounding []string `json:"grounding" desc:"이 문제를 말하는 원문 문장의 발췌. 원문 그대로"`
	Moves     []Move   `json:"moves" desc:"이 문제가 걸린 이동. 출발지·도착지나 노선별 사정이 다르면 나눈다"`
}

// Extraction은 모델의 출력 형식이다.
type Extraction struct {
	Problems []Problem `json:"problems" desc:"고른 문제 유형마다 하나"`
}

// Issues는 원문에 없는 값과 다루지 않은 유형을 찾는다.
func Issues(out *Extraction, source string, types []string) []string {
	flat, found := textx.Squeeze(source), []string{}
	counts := map[string]int{}
	for _, p := range out.Problems {
		counts[p.Type]++
	}
	for _, t := range types {
		if counts[t] == 0 {
			found = append(found, "유형 "+t+"를 다루지 않았다")
		}
	}
	for _, t := range types {
		if counts[t] > 1 {
			found = append(found, fmt.Sprintf("유형 %s가 %d번 나온다. 유형마다 항목 하나에 moves로 나눈다", t, counts[t]))
		}
	}
	for _, p := range out.Problems {
		for _, g := range p.Grounding {
			if !strings.Contains(flat, textx.Squeeze(g)) {
				found = append(found, p.Type+" grounding «"+g+"»")
			}
		}
		check := func(where, text, quote string) {
			if !strings.Contains(flat, textx.Squeeze(text)) || !strings.Contains(flat, textx.Squeeze(quote)) {
				found = append(found, p.Type+" "+where+" «"+text+"» / «"+quote+"»")
			} else if !strings.Contains(textx.Squeeze(quote), textx.Squeeze(text)) {
				found = append(found, p.Type+" "+where+" «"+text+"»가 그 quote 안에 없다")
			}
		}
		for _, m := range p.Moves {
			for _, x := range m.Origin {
				check("origin", x.Text, x.Quote)
			}
			for _, x := range m.Destination {
				check("destination", x.Text, x.Quote)
			}
			for _, x := range m.Time {
				check("time", x.Text, x.Quote)
			}
			for _, x := range m.Values {
				check("values", x.Text, x.Quote)
			}
			for _, x := range m.Targets {
				check("targets", x.Text, x.Quote)
			}
			if m.Period != "" && m.Period != "none" && (textx.Squeeze(m.PeriodQuote) == "" || !strings.Contains(flat, textx.Squeeze(m.PeriodQuote))) {
				found = append(found, p.Type+" period_quote «"+m.PeriodQuote+"»")
			}
		}
	}
	return found
}

// Check는 Issues를 모델에게 돌려줄 글로 바꾼다.
func Check(source string, types []string) func(*Extraction) string {
	return func(out *Extraction) string {
		found := Issues(out, source, types)
		if len(found) == 0 {
			return ""
		}
		return "다음 값은 원문에 그대로 있지 않거나 빠졌다. 원문에 쓰인 그대로 고치거나 뺀다: " + strings.Join(found, "; ")
	}
}

// Priors는 data/priors와 시간표 시트에서 읽은 사전이다.
type Priors struct {
	Areas  []AreaEntry
	Times  []TimeEntry
	Sheets []Sheet
}

// AreaEntry는 지칭 하나의 행정동 목록이다.
type AreaEntry struct {
	Key     string
	Aliases []string
	Areas   json.RawMessage
}

// TimeEntry는 시간대 표현 하나의 범위다.
type TimeEntry struct {
	Key    string
	Ranges json.RawMessage
	Narrow []NarrowRange
}

// NarrowRange는 표현이 나온 문장에 그 말이 있으면 대신 쓰는 범위다.
type NarrowRange struct {
	Word   string          `json:"word"`
	Ranges json.RawMessage `json:"ranges"`
}

// Sheet는 시간표 파일의 시트 하나다.
type Sheet struct {
	Route    string   `json:"route"`
	Previous []string `json:"previous"`
}

// LoadPriors는 사전 파일을 읽는다. 지칭과 표현은 파일에 적힌 순서를 지킨다.
func LoadPriors(root string) (*Priors, error) {
	out := &Priors{}
	areas, err := repo.ReadText(filepath.Join(repo.RegionDir(root), "priors", "areas.json"))
	if err != nil {
		return nil, err
	}
	fields, err := jsonx.Fields([]byte(areas))
	if err != nil {
		return nil, err
	}
	for _, f := range fields {
		var entry struct {
			Aliases []string        `json:"aliases"`
			Areas   json.RawMessage `json:"areas"`
		}
		if err := json.Unmarshal(f.Value, &entry); err != nil {
			return nil, err
		}
		out.Areas = append(out.Areas, AreaEntry{f.Key, entry.Aliases, entry.Areas})
	}
	times, err := repo.ReadText(filepath.Join(root, "data", "priors", "times.json"))
	if err != nil {
		return nil, err
	}
	var file struct {
		Expressions json.RawMessage `json:"expressions"`
	}
	if err := json.Unmarshal([]byte(times), &file); err != nil {
		return nil, err
	}
	fields, err = jsonx.Fields(file.Expressions)
	if err != nil {
		return nil, err
	}
	for _, f := range fields {
		var entry struct {
			Ranges json.RawMessage `json:"ranges"`
			Narrow []NarrowRange   `json:"narrow"`
		}
		if err := json.Unmarshal(f.Value, &entry); err != nil {
			return nil, err
		}
		out.Times = append(out.Times, TimeEntry{f.Key, entry.Ranges, entry.Narrow})
	}
	sort.SliceStable(out.Times, func(i, j int) bool {
		return textx.Len(textx.Squeeze(out.Times[i].Key)) > textx.Len(textx.Squeeze(out.Times[j].Key))
	})
	lineage, err := repo.ReadText(filepath.Join(repo.RegionDir(root), "transit", "lineage.json"))
	if err != nil {
		return nil, err
	}
	var sheets struct {
		Lineage []Sheet `json:"lineage"`
	}
	if err := json.Unmarshal([]byte(lineage), &sheets); err != nil {
		return nil, err
	}
	out.Sheets = sheets.Lineage
	return out, nil
}

var josa = regexp.MustCompile(`(으로|로|에서|에|까지|쪽|방면|방향)$`)

// AreaPrior는 지칭이 사전에 있는 지역이면 그 지역의 행정동 목록을 돌려준다.
func (p *Priors) AreaPrior(text string) *AreaMatch {
	name := josa.ReplaceAllString(textx.Squeeze(text), "")
	for _, entry := range p.Areas {
		for _, alias := range append([]string{entry.Key}, entry.Aliases...) {
			if name == josa.ReplaceAllString(textx.Squeeze(alias), "") {
				return &AreaMatch{entry.Key, entry.Areas, "사전"}
			}
		}
	}
	return nil
}

// TimePrior는 표현에 사전의 말이 들어 있으면 그 범위들을 돌려준다. 긴 말을 먼저 본다('새벽 출근' → '출근').
func (p *Priors) TimePrior(text string) *TimeMatch {
	return p.TimeIn(text, "")
}

// TimeIn은 표현의 범위다. 사전이 문장의 말로 좁히는 표현이면 그 표현이 나온 문장에서 고른다('아침 8시, … 출근하는 시간대'는 아침 범위).
func (p *Priors) TimeIn(text, quote string) *TimeMatch {
	name, sentence := textx.Squeeze(text), textx.Squeeze(quote)
	for _, entry := range p.Times {
		if strings.Contains(name, textx.Squeeze(entry.Key)) {
			for _, n := range entry.Narrow {
				if strings.Contains(sentence, n.Word) {
					return &TimeMatch{entry.Key, n.Ranges, "사전"}
				}
			}
			return &TimeMatch{entry.Key, entry.Ranges, "사전"}
		}
	}
	return nil
}

var route = regexp.MustCompile(`(울주|동구|남구|북구|중구|순환)[` + textx.Space + `]*|(` + textx.Digit + `{5,})|(` + textx.Digit + `{1,4})|([^` + textx.Digit + `,·` + textx.Space + `]+)`)

// RouteNumbers는 노선 지칭의 노선 번호다. '543버스' → 543, '순환 31,32번' → 순환31·순환32, '울주4번' → 울주04.
func RouteNumbers(text string) []string {
	out, prefix := []string{}, ""
	for _, m := range route.FindAllStringSubmatch(text, -1) {
		switch {
		case m[1] != "":
			prefix = m[1]
		case m[3] != "":
			if prefix != "" {
				out = append(out, prefix+strings.Repeat("0", max(2-textx.Len(m[3]), 0))+m[3])
			} else if textx.Len(m[3]) >= 2 {
				out = append(out, m[3])
			}
		default:
			prefix = ""
		}
	}
	unique := []string{}
	for _, n := range out {
		if !slices.Contains(unique, n) {
			unique = append(unique, n)
		}
	}
	return unique
}

var clock = regexp.MustCompile(`(` + textx.Digit + `{1,2})[` + textx.Space + `]*:[` + textx.Space + `]*(` + textx.Digit + `{2})|(` + textx.Digit + `{1,2})[` + textx.Space + `]*(시)[` + textx.Space + `]*(?:(반)|(` + textx.Digit + `{1,2})[` + textx.Space + `]*분)?`)

// WrittenTimes는 표현에 적힌 시각을 적힌 그대로 H:MM으로 읽는다. 오전·오후는 정하지 않는다. '시간'의 시는 시각이 아니다.
func WrittenTimes(text string) []string {
	out := []string{}
	for position := 0; position < len(text); {
		m := clock.FindStringSubmatchIndex(text[position:])
		if m == nil {
			break
		}
		group := func(i int) string {
			if m[2*i] < 0 {
				return ""
			}
			return text[position+m[2*i] : position+m[2*i+1]]
		}
		if m[8] >= 0 && strings.HasPrefix(text[position+m[9]:], "간") {
			_, size := utf8.DecodeRuneInString(text[position+m[0]:])
			position += m[0] + size
			continue
		}
		if group(1) != "" {
			out = append(out, fmt.Sprintf("%d:%s", textx.Atoi(group(1)), group(2)))
		} else {
			minute := textx.Atoi(group(6))
			if group(5) != "" {
				minute = 30
			}
			out = append(out, fmt.Sprintf("%d:%02d", textx.Atoi(group(3)), minute))
		}
		position += m[1]
	}
	return out
}

// RouteLineage는 노선 번호와 그 이전·이어받은 번호다.
type RouteLineage struct {
	Number   string   `json:"number"`
	Previous []string `json:"previous"`
	Later    []string `json:"later"`
}

func (p *Priors) lineage(number string) RouteLineage {
	previous, later := []string{}, []string{}
	for _, s := range p.Sheets {
		if s.Route == number {
			for _, n := range s.Previous {
				if !slices.Contains(previous, n) {
					previous = append(previous, n)
				}
			}
		}
		if slices.Contains(s.Previous, number) && !slices.Contains(later, s.Route) {
			later = append(later, s.Route)
		}
	}
	sort.Strings(previous)
	sort.Strings(later)
	return RouteLineage{number, previous, later}
}

// AreaMatch는 장소 지칭에 붙은 행정동 목록이다.
type AreaMatch struct {
	Key   string          `json:"key"`
	Areas json.RawMessage `json:"areas"`
	Basis string          `json:"basis"`
}

// TimeMatch는 시간대 표현에 붙은 범위다.
type TimeMatch struct {
	Key    string          `json:"key"`
	Ranges json.RawMessage `json:"ranges"`
	Basis  string          `json:"basis"`
}

// Place는 prior를 붙인 장소 지칭이다. 지시 표현이면 RefersTo가 그것이 가리키는 원문 지칭이다.
type Place struct {
	Text     string         `json:"text"`
	Quote    string         `json:"quote"`
	Prior    *AreaMatch     `json:"prior,omitempty"`
	RefersTo *ResolvedPlace `json:"refersTo,omitempty"`
}

// ResolvedPlace는 지시 표현이 가리키는 원문 지칭과 그 prior다.
type ResolvedPlace struct {
	Text  string     `json:"text"`
	Quote string     `json:"quote"`
	Prior *AreaMatch `json:"prior,omitempty"`
}

// Time은 적힌 시각과 범위를 붙인 시간대 표현이다.
type Time struct {
	Text    string     `json:"text"`
	Quote   string     `json:"quote"`
	Written []string   `json:"written"`
	Range   *TimeMatch `json:"range"`
}

// Value는 시민이 말한 수치다.
type Value struct {
	Text  string `json:"text"`
	Quote string `json:"quote"`
}

// Target은 노선 번호나 prior를 붙인 대상이다.
type Target struct {
	Text     string          `json:"text"`
	Kind     string          `json:"kind"`
	Quote    string          `json:"quote"`
	Period   string          `json:"period,omitempty"`
	Routes   *[]RouteLineage `json:"routes,omitempty"`
	Prior    *AreaMatch      `json:"prior,omitempty"`
	RefersTo *ResolvedPlace  `json:"refersTo,omitempty"`
}

// ResolvedMove는 prior를 붙인 이동이다.
type ResolvedMove struct {
	Origin      []Place  `json:"origin"`
	Destination []Place  `json:"destination"`
	Time        []Time   `json:"time"`
	Values      []Value  `json:"values"`
	Targets     []Target `json:"targets"`
	Period      string   `json:"period"`
	PeriodQuote string   `json:"period_quote"`
}

// ResolvedProblem은 prior를 붙인 유형 하나의 결과다.
type ResolvedProblem struct {
	Type      string         `json:"type"`
	Grounding []string       `json:"grounding"`
	Moves     []ResolvedMove `json:"moves"`
}

// WithPriors는 모델이 뽑은 값에 prior를 붙인다. 지시 표현에는 그것이 가리키는 지칭(referents)을 붙인다. 조회하지 않는 유형은 이동을 비운다.
func (p *Priors) WithPriors(out *Extraction, noQuery []string, referents map[string]*Referent) []ResolvedProblem {
	refers := func(text, quote string) *ResolvedPlace {
		r := referents[referentKey(text, quote)]
		if r == nil {
			return nil
		}
		return &ResolvedPlace{r.Text, r.Quote, p.AreaPrior(r.Text)}
	}
	places := func(items []PlaceMention) []Place {
		resolved := []Place{}
		for _, m := range items {
			resolved = append(resolved, Place{m.Text, m.Quote, p.AreaPrior(m.Text), refers(m.Text, m.Quote)})
		}
		return resolved
	}
	result := []ResolvedProblem{}
	for _, problem := range out.Problems {
		moves := []ResolvedMove{}
		if !slices.Contains(noQuery, problem.Type) {
			for _, m := range problem.Moves {
				move := ResolvedMove{Origin: places(m.Origin), Destination: places(m.Destination), Time: []Time{}, Values: []Value{}, Targets: []Target{}, Period: m.Period, PeriodQuote: m.PeriodQuote}
				for _, t := range m.Time {
					move.Time = append(move.Time, Time{t.Text, t.Quote, WrittenTimes(t.Text), p.TimeIn(t.Text, t.Quote)})
				}
				for _, v := range m.Values {
					move.Values = append(move.Values, Value(v))
				}
				for _, t := range m.Targets {
					target := Target{Text: t.Text, Kind: t.Kind, Quote: t.Quote}
					if t.Kind == "route" {
						target.Period = t.Period
						routes := []RouteLineage{}
						for _, number := range RouteNumbers(t.Text) {
							routes = append(routes, p.lineage(number))
						}
						target.Routes = &routes
					} else {
						target.Prior, target.RefersTo = p.AreaPrior(t.Text), refers(t.Text, t.Quote)
					}
					move.Targets = append(move.Targets, target)
				}
				moves = append(moves, move)
			}
		}
		result = append(result, ResolvedProblem{problem.Type, problem.Grounding, moves})
	}
	return result
}

// Request는 모델에게 보내는 입력이다.
func Request(source string, types []string, catalog map[string]problems.Entry, noQuery []string, parts []problems.NoQueryAspect) string {
	lines := []string{"원문:", source, "", "고른 문제 유형:"}
	for _, t := range types {
		e := catalog[t]
		line := "- " + t + ": " + e.Proposition
		if e.Includes != "" {
			line += " " + e.Includes
		}
		if slices.Contains(noQuery, t) {
			line += " (moves를 비운다)"
		}
		lines = append(lines, line)
	}
	if len(parts) > 0 {
		lines = append(lines, "", "조회하지 않는 측면:")
		for _, p := range parts {
			lines = append(lines, "- "+p.Type+": "+p.Statement)
		}
	}
	return strings.Join(lines, "\n")
}

// Hashes는 결과를 만든 사전 파일의 hash다.
type Hashes struct {
	Areas   string `json:"areas"`
	Times   string `json:"times"`
	Lineage string `json:"lineage"`
}

// Record는 <run>/extract/<번호>.json의 내용이다.
type Record struct {
	ID             string                   `json:"id"`
	CreatedAt      string                   `json:"createdAt"`
	Model          string                   `json:"model"`
	Effort         string                   `json:"effort"`
	Types          []string                 `json:"types"`
	NoQuery        []string                 `json:"noQuery"`
	NoQueryAspects []problems.NoQueryAspect `json:"noQueryAspects"`
	L2Hash         string                   `json:"l2Hash"`
	Priors         Hashes                   `json:"priors"`
	Prompt         string                   `json:"prompt"`
	Problems       []ResolvedProblem        `json:"problems"`
	Referents      []ReferentRecord         `json:"referents"`
	Messages       []matchi.Message         `json:"messages"`
}

// Config는 Run의 입력이다.
type Config struct {
	Root, Run, ModelName string
	Cases                []string
	Workers              int
	Writer               *matchi.Writer
	Report               func(line string)
}

// Run은 1단계 결과가 있는 민원마다 추출해 결과 파일을 쓴다. Cases가 비면 전부다.
func Run(ctx context.Context, c Config) error {
	promptPath := filepath.Join(c.Root, "prompts", "l2", "extract.md")
	prompt, err := repo.ReadText(promptPath)
	if err != nil {
		return err
	}
	referentPrompt, err := repo.ReadText(filepath.Join(c.Root, "prompts", "l2", "referent.md"))
	if err != nil {
		return err
	}
	priors, err := LoadPriors(c.Root)
	if err != nil {
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
	hashes := Hashes{}
	for _, f := range []struct {
		out  *string
		path string
	}{
		{&hashes.Areas, filepath.Join(repo.RegionDir(c.Root), "priors", "areas.json")},
		{&hashes.Times, filepath.Join(c.Root, "data", "priors", "times.json")},
		{&hashes.Lineage, filepath.Join(repo.RegionDir(c.Root), "transit", "lineage.json")},
	} {
		if *f.out, err = fileHash(f.path); err != nil {
			return err
		}
	}
	promptHash, err := fileHash(promptPath)
	if err != nil {
		return err
	}
	cases := c.Cases
	if len(cases) == 0 {
		if cases, err = repo.Stems(filepath.Join(c.Run, "l2")); err != nil {
			return err
		}
	}
	return repo.ForEach(cases, c.Workers, func(id string) error {
		data, err := os.ReadFile(filepath.Join(c.Run, "l2", id+".json"))
		if err != nil {
			return err
		}
		var stage1 struct {
			Types          []string                 `json:"types"`
			NoQuery        []string                 `json:"noQuery"`
			NoQueryAspects []problems.NoQueryAspect `json:"noQueryAspects"`
		}
		if err := json.Unmarshal(data, &stage1); err != nil {
			return err
		}
		parsed, err := jsonx.Parse(data)
		if err != nil {
			return err
		}
		l2Hash, err := jsonx.Digest(parsed)
		if err != nil {
			return err
		}
		source, err := repo.SourceText(c.Root, id)
		if err != nil {
			return err
		}
		record := Record{ID: id, Model: c.ModelName, Effort: c.Writer.Effort, Types: stage1.Types, NoQuery: stage1.NoQuery, NoQueryAspects: stage1.NoQueryAspects,
			L2Hash: l2Hash, Priors: hashes, Prompt: promptHash, Problems: []ResolvedProblem{}, Referents: []ReferentRecord{}, Messages: []matchi.Message{}}
		if len(stage1.Types) > 0 {
			out, messages, err := matchi.Write(matchi.WithScope(ctx, id, "extract"), c.Writer, matchi.Spec[Extraction]{
				Name: "Extraction", Instructions: prompt, Input: Request(source, stage1.Types, catalog, stage1.NoQuery, stage1.NoQueryAspects),
				Enums: map[string][]string{"Problem.type": stage1.Types}, Check: Check(source, stage1.Types)})
			if err != nil {
				return fmt.Errorf("민원 %s: %w", id, err)
			}
			referents, resolved, err := resolveReferents(matchi.WithScope(ctx, id, "referent"), c.Writer, referentPrompt, source, out, stage1.NoQuery)
			if err != nil {
				return fmt.Errorf("민원 %s: %w", id, err)
			}
			record.Problems, record.Referents, record.Messages = priors.WithPriors(out, stage1.NoQuery, resolved), referents, messages
		}
		record.CreatedAt = time.Now().Format(time.RFC3339Nano)
		if err := jsonx.WriteFile(filepath.Join(c.Run, "extract", id+".json"), record); err != nil {
			return err
		}
		if c.Report != nil {
			kinds := []string{}
			for _, p := range record.Problems {
				kinds = append(kinds, p.Type)
			}
			line, err := jsonx.Marshal(jsonx.Object{{Key: "id", Value: id}, {Key: "problems", Value: kinds}})
			if err != nil {
				return err
			}
			c.Report(string(line))
		}
		return nil
	})
}

func fileHash(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return jsonx.SHA(string(data)), nil
}
