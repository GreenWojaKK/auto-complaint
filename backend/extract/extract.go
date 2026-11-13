// Package extract는 1단계가 고른 문제 유형마다 원문에서 출발지·도착지, 시간대, 대상을 뽑고 prior를 붙인다.
//
// 언어 모델을 두 번에 나누어 부른다. 먼저 민원마다 한 번 유형별 사정(유형마다 원문이 말하는 이용 사정 한 문장)을 쓰게 하고,
// 이어서 유형 하나씩 그 사정을 범위로 이동과 근거를 묻는다. 언어 모델은 원문에 쓰인 지칭과 근거 문장만 쓴다. 원문에 글자 그대로 없는 값은 코드가 버리고,
// 지칭이 나온 원문 문장은 코드가 원문에서 찾아 붙인다(Settle). 조회하지 않는 유형은 근거만 묻고 이동을 비운다.
// 지시 표현(여기, 거기, 이곳 같은 말)인 장소 지칭은 언어 모델에게 그것이 가리키는 원문 지칭을 한 번 더 뽑게 한다(referent.go).
// 그 뒤 코드가 prior를 붙인다.
//   - 장소: 지역 폴더의 priors/areas.json에 있는 지칭("시내")이면 그 행정동 목록을 붙인다.
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
	"unicode"
	"unicode/utf8"

	"dllio/backend/jsonx"
	"dllio/backend/matchi"
	"dllio/backend/problems"
	"dllio/backend/repo"
	"dllio/backend/textx"
)

// PlaceMention은 원문에 쓰인 장소 지칭이다. 모델은 지칭(Text)만 쓰고, 지칭이 나온 원문 문장(Quote)은 Settle이 붙인다.
type PlaceMention struct {
	Text  string `json:"text" desc:"원문에 쓰인 장소 지칭 그대로. 예: 시내, 북구 양정동, 태화강역"`
	Quote string `json:"-"`
}

// TimeMention은 원문에 쓰인 시각·시간대 표현이다.
type TimeMention struct {
	Text  string `json:"text" desc:"원문에 쓰인 시각·시간대 표현 그대로. 예: 출근 시간, 등하교 시간, 6시반, 막차"`
	Quote string `json:"-"`
}

// ValueMention은 시민이 말한 수치다.
type ValueMention struct {
	Text  string `json:"text" desc:"시민이 말한 수치 그대로. 예: 104분, 20-30분 대기, 1개로 줄어, 7대"`
	Quote string `json:"-"`
}

// TargetMention은 원문에 쓰인 노선·정류장·장소 지칭이다.
type TargetMention struct {
	Text   string `json:"text" desc:"원문에 쓰인 노선 번호·정류장·장소 지칭 그대로"`
	Kind   string `json:"kind" desc:"route: 노선, stop: 정류장, place: 장소·지역" enum:"route,stop,place"`
	Quote  string `json:"-"`
	Period string `json:"period" desc:"노선을 어느 시기의 번호로 말했는지. before: 개편 전 노선, after: 개편 뒤 노선, none: 말하지 않음이거나 노선이 아닌 대상" enum:"before,after,none"`
}

// Move는 문제가 걸린 이동 하나다.
type Move struct {
	About       string          `json:"about" desc:"이 이동을 한 문장으로 쓴다: 이용자가 어디서 어디로 언제 무엇을 타고 다니는지와 이번 유형의 불편. 원문이 말한 만큼만 쓴다"`
	Origin      []PlaceMention  `json:"origin" desc:"이용자가 출발하는 곳" max:"3"`
	Destination []PlaceMention  `json:"destination" desc:"이용자가 가려는 곳" max:"3"`
	Time        []TimeMention   `json:"time" desc:"이 이동이 걸린 하루 중의 때(시각·시간대). 시간의 길이는 넣지 않는다" max:"4"`
	Values      []ValueMention  `json:"values" desc:"시민이 말한 수치: 배차 간격, 대기, 소요 시간, 편수·대수" max:"6"`
	Targets     []TargetMention `json:"targets" desc:"이 이동의 대상인 노선·정류장·장소" max:"8"`
	Period      string          `json:"period" desc:"before: 개편 전의 이동을 말함, after: 개편 뒤의 이동을 말함, compare: 개편 전과 지금을 비교하거나 개편으로 바뀌었다고 말함, none: 개편을 말하지 않음" enum:"before,after,compare,none"`
	PeriodQuote string          `json:"period_quote" desc:"period의 근거가 된 원문 문장의 발췌. none이면 빈 글"`
}

// Problem은 고른 유형 하나의 추출 결과다. Reading은 모델이 이동을 칸에 옮기기 전에 쓴, 이 문제가 걸린 이용 사정이다.
type Problem struct {
	Type      string   `json:"type"`
	Reading   string   `json:"reading"`
	Grounding []string `json:"grounding"`
	Moves     []Move   `json:"moves"`
}

// Extraction은 민원 하나의 추출 결과다. 유형마다 하나다.
type Extraction struct {
	Problems []Problem `json:"problems"`
}

// TypeShare는 고른 유형 하나에 대해 원문이 말하는 이용 사정이다.
type TypeShare struct {
	Type    string `json:"type" desc:"받은 유형 id 그대로"`
	Reading string `json:"reading" desc:"원문이 이 유형의 문제를 어떤 이용 사정으로 말하는지 한 문장. 수치·시각은 그것이 말하는 불편의 유형에만 쓴다"`
}

// Shares는 민원 하나에서 고른 유형들의 이용 사정이다. 유형마다 하나다. 모델이 유형들을 함께 보며 원문의 사실을 유형에 나눈다.
type Shares struct {
	Readings []TypeShare `json:"readings" max:"12"`
}

// TypeReading은 조회하는 유형 하나에 대한 모델의 출력 형식이다. 모델은 받은 이용 사정의 이동을 칸에 옮기고, 근거 문장을 고른다.
type TypeReading struct {
	Moves     []Move   `json:"moves" desc:"이번 유형의 사정에 나오는 이동. 출발지·도착지나 노선별 사정이 다르면 나눈다" max:"4"`
	Grounding []string `json:"grounding" desc:"이번 유형의 문제를 말하는 원문 문장의 발췌. 원문 그대로" max:"12"`
}

// TypeGrounding은 조회하지 않는 유형 하나에 대한 모델의 출력 형식이다.
type TypeGrounding struct {
	Grounding []string `json:"grounding" desc:"이번 유형의 문제를 말하는 원문 문장의 발췌. 원문 그대로" max:"20"`
}

var sentenceEnd = regexp.MustCompile(`[.?!…]+[)\]"'”’]*[` + textx.Space + `]+|\n+`)

// Sentences는 원문을 문장으로 나눈다. 줄바꿈과, 마침표·물음표·느낌표 뒤의 공백에서 끊는다.
func Sentences(source string) []string {
	out, start := []string{}, 0
	add := func(piece string) {
		if piece = strings.TrimSpace(piece); piece != "" {
			out = append(out, piece)
		}
	}
	for _, m := range sentenceEnd.FindAllStringIndex(source, -1) {
		add(source[start:m[1]])
		start = m[1]
	}
	add(source[start:])
	return out
}

// quoteOf는 지칭이 나온 원문 문장이다. 여러 문장에 나오면 같은 이동의 다른 지칭이 가장 많이 나온 문장, 그다음은 앞선 문장이다.
// 지칭이 문장 경계에 걸치면 그 지칭이 나온 줄이다.
func quoteOf(text string, others []string, sentences, lines []string) string {
	want := textx.Squeeze(text)
	for _, units := range [][]string{sentences, lines} {
		best, most := "", -1
		for _, unit := range units {
			flat := textx.Squeeze(unit)
			if !strings.Contains(flat, want) {
				continue
			}
			count := 0
			for _, other := range others {
				if other != want && strings.Contains(flat, other) {
					count++
				}
			}
			if count > most {
				best, most = unit, count
			}
		}
		if most >= 0 {
			return best
		}
	}
	return text
}

// settled는 원문에 글자 그대로 있는 지칭만 남긴다. 같은 칸에 같은 지칭이 거듭 나오면 한 번만 남긴다.
func settled[T any](items []T, text func(*T) *string, in func(string) bool, drop func(string)) []T {
	out, seen := []T{}, map[string]bool{}
	for i := range items {
		value := *text(&items[i])
		switch flat := textx.Squeeze(value); {
		case !in(value):
			drop(value)
		case !seen[flat]:
			seen[flat] = true
			out = append(out, items[i])
		}
	}
	return out
}

// Settle은 모델이 쓴 유형 하나의 결과를 원문에 맞춘다. 같은 문장으로 적은 이동은 하나로 모으고, 원문에 글자 그대로 없는 근거·지칭은
// 버리고, 남은 지칭마다 그 지칭이 나온 원문 문장을 Quote로 붙인다. 출발지·도착지와 같은 장소 대상은 빼고, 원문에 없는 시기 근거는
// 비운다. 원문에 없어 버린 값을 돌려준다.
func Settle(p *Problem, source string) []string {
	flat := textx.Squeeze(source)
	in := func(text string) bool {
		squeezed := textx.Squeeze(text)
		return squeezed != "" && strings.Contains(flat, squeezed)
	}
	dropped := []string{}
	grounding := []string{}
	for _, g := range p.Grounding {
		if in(g) {
			grounding = append(grounding, g)
		} else {
			dropped = append(dropped, p.Type+" grounding «"+g+"»")
		}
	}
	p.Grounding = grounding
	sentences, lines := Sentences(source), strings.Split(source, "\n")
	// 같은 문장으로 적은 이동은 같은 이동이다. 뒤의 것의 지칭을 앞의 것에 모은다.
	merged, first := []Move{}, map[string]int{}
	for _, m := range p.Moves {
		about := textx.Squeeze(m.About)
		if i, seen := first[about]; seen && about != "" {
			into := &merged[i]
			into.Origin, into.Destination = append(into.Origin, m.Origin...), append(into.Destination, m.Destination...)
			into.Time, into.Values, into.Targets = append(into.Time, m.Time...), append(into.Values, m.Values...), append(into.Targets, m.Targets...)
			continue
		}
		first[about] = len(merged)
		merged = append(merged, m)
	}
	p.Moves = merged
	for i := range p.Moves {
		m := &p.Moves[i]
		drop := func(where string) func(string) {
			return func(text string) { dropped = append(dropped, p.Type+" "+where+" «"+text+"»") }
		}
		m.Origin = settled(m.Origin, func(x *PlaceMention) *string { return &x.Text }, in, drop("origin"))
		m.Destination = settled(m.Destination, func(x *PlaceMention) *string { return &x.Text }, in, drop("destination"))
		// 출발지와 같은 지칭은 도착지가 아니다(돌아가는 길을 출발지·도착지 모두 그 정류장으로 쓴 경우).
		starts := map[string]bool{}
		for _, x := range m.Origin {
			starts[textx.Squeeze(x.Text)] = true
		}
		arrivals := []PlaceMention{}
		for _, x := range m.Destination {
			if starts[textx.Squeeze(x.Text)] {
				drop("destination")(x.Text)
				continue
			}
			arrivals = append(arrivals, x)
		}
		m.Destination = arrivals
		m.Time = settled(m.Time, func(x *TimeMention) *string { return &x.Text }, in, drop("time"))
		m.Values = settled(m.Values, func(x *ValueMention) *string { return &x.Text }, in, drop("values"))
		// 숫자만 있는 값(노선 번호)과 수를 말하지 않는 값은 수치가 아니다.
		values := []ValueMention{}
		for _, v := range m.Values {
			if strings.TrimFunc(v.Text, func(r rune) bool { return unicode.IsDigit(r) || unicode.IsSpace(r) || unicode.IsPunct(r) }) == "" || !countWord.MatchString(v.Text) {
				drop("values")(v.Text)
				continue
			}
			values = append(values, v)
		}
		m.Values = values
		m.Targets = settled(m.Targets, func(x *TargetMention) *string { return &x.Text }, in, drop("targets"))
		// 출발지·도착지로 이미 넣은 지칭은 대상에서 뺀다.
		ends := map[string]bool{}
		for _, x := range append(append([]PlaceMention{}, m.Origin...), m.Destination...) {
			ends[textx.Squeeze(x.Text)] = true
		}
		targets := []TargetMention{}
		for _, x := range m.Targets {
			if x.Kind == "route" || !ends[textx.Squeeze(x.Text)] {
				targets = append(targets, x)
			}
		}
		m.Targets = targets
		others := []string{}
		each := func(visit func(text string, quote *string)) {
			for j := range m.Origin {
				visit(m.Origin[j].Text, &m.Origin[j].Quote)
			}
			for j := range m.Destination {
				visit(m.Destination[j].Text, &m.Destination[j].Quote)
			}
			for j := range m.Time {
				visit(m.Time[j].Text, &m.Time[j].Quote)
			}
			for j := range m.Values {
				visit(m.Values[j].Text, &m.Values[j].Quote)
			}
			for j := range m.Targets {
				visit(m.Targets[j].Text, &m.Targets[j].Quote)
			}
		}
		each(func(text string, _ *string) { others = append(others, textx.Squeeze(text)) })
		each(func(text string, quote *string) { *quote = quoteOf(text, others, sentences, lines) })
		// 노선의 기점·종점·회차지로 말한 장소는 이용자의 출발지·도착지가 아니다.
		notRouteEnds := func(list []PlaceMention, where string) []PlaceMention {
			kept := []PlaceMention{}
			for _, x := range list {
				if RouteEnd(x.Text, x.Quote) {
					drop(where)(x.Text)
					continue
				}
				kept = append(kept, x)
			}
			return kept
		}
		m.Origin, m.Destination = notRouteEnds(m.Origin, "origin"), notRouteEnds(m.Destination, "destination")
		if m.Period == "" {
			m.Period = "none"
		}
		if m.Period == "none" || !in(m.PeriodQuote) {
			m.PeriodQuote = ""
		}
	}
	return dropped
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
	About       string   `json:"about,omitempty"`
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
	Reading   string         `json:"reading,omitempty"`
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
				move := ResolvedMove{About: strings.TrimSpace(m.About), Origin: places(m.Origin), Destination: places(m.Destination), Time: []Time{}, Values: []Value{}, Targets: []Target{}, Period: m.Period, PeriodQuote: m.PeriodQuote}
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
		result = append(result, ResolvedProblem{problem.Type, problem.Reading, problem.Grounding, moves})
	}
	return result
}

// Request는 유형 하나를 묻는 입력이다. 이 민원에서 고른 유형 전부와, 그 가운데 이번에 다룰 유형을 적는다.
// ShareRequest는 유형마다 이용 사정을 쓰게 하는 요청의 입력이다.
func ShareRequest(source string, types []string, catalog map[string]problems.Entry, parts []problems.NoQueryAspect) string {
	return strings.TrimSuffix(Request(source, types, "", catalog, parts, nil), "\n\n이번에 다룰 유형: ")
}

// Request는 유형 하나의 이동을 쓰게 하는 요청의 입력이다. shares가 있으면 유형마다의 이용 사정을 함께 준다.
func Request(source string, types []string, target string, catalog map[string]problems.Entry, parts []problems.NoQueryAspect, shares map[string]string) string {
	lines := []string{"원문:", source, "", "이 민원에서 고른 문제 유형:"}
	for _, t := range types {
		e := catalog[t]
		line := "- " + t + ": " + e.Proposition
		if e.Includes != "" {
			line += " " + e.Includes
		}
		lines = append(lines, line)
	}
	if len(parts) > 0 {
		lines = append(lines, "", "조회하지 않는 측면:")
		for _, p := range parts {
			lines = append(lines, "- "+p.Type+": "+p.Statement)
		}
	}
	if len(shares) > 0 {
		lines = append(lines, "", "이번 유형의 사정: "+shares[target], "", "다른 유형이 다루는 사정:")
		for _, t := range types {
			if t != target && shares[t] != "" {
				lines = append(lines, "- "+t+": "+shares[t])
			}
		}
	}
	lines = append(lines, "", "이번에 다룰 유형: "+target)
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
	Dropped        []string                 `json:"dropped"`
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
	groundingPrompt, err := repo.ReadText(filepath.Join(c.Root, "prompts", "l2", "grounding.md"))
	if err != nil {
		return err
	}
	referentPrompt, err := repo.ReadText(filepath.Join(c.Root, "prompts", "l2", "referent.md"))
	if err != nil {
		return err
	}
	sharePrompt, err := repo.ReadText(filepath.Join(c.Root, "prompts", "l2", "share.md"))
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
			L2Hash: l2Hash, Priors: hashes, Prompt: promptHash, Problems: []ResolvedProblem{}, Dropped: []string{}, Referents: []ReferentRecord{}, Messages: []matchi.Message{}}
		if len(stage1.Types) > 0 {
			// 유형마다 따로 묻는다. 요청은 함께 보내고 결과는 유형 순서로 모은다.
			index := map[string]int{}
			for i, t := range stage1.Types {
				index[t] = i
			}
			out := &Extraction{Problems: make([]Problem, len(stage1.Types))}
			messages, dropped := make([][]matchi.Message, len(stage1.Types)), make([][]string, len(stage1.Types))
			scope := matchi.WithScope(ctx, id, "extract")
			// 먼저 유형들을 함께 보며 유형마다 이용 사정을 한 문장씩 쓴다. 유형별 요청은 그 사정을 범위로 받는다.
			shared, shareMessages, err := matchi.Write(scope, c.Writer, matchi.Spec[Shares]{Name: "Shares", Instructions: sharePrompt,
				Input: ShareRequest(source, stage1.Types, catalog, stage1.NoQueryAspects), Enums: map[string][]string{"TypeShare.type": stage1.Types},
				Check: func(s *Shares) string { return shareIssues(s, stage1.Types) }})
			if err != nil {
				return fmt.Errorf("민원 %s: 유형별 사정: %w", id, err)
			}
			record.Messages = append(record.Messages, shareMessages...)
			shares := map[string]string{}
			// 같은 유형을 여러 항목에 나누어 쓰면 한 사정으로 잇는다.
			for _, s := range shared.Readings {
				if reading := strings.TrimSpace(s.Reading); reading != "" && shares[s.Type] == "" {
					shares[s.Type] = reading
				} else if reading != "" {
					shares[s.Type] += " " + reading
				}
			}
			err = repo.ForEach(stage1.Types, len(stage1.Types), func(t string) error {
				i, input := index[t], Request(source, stage1.Types, t, catalog, stage1.NoQueryAspects, shares)
				problem := Problem{Type: t}
				var err error
				if slices.Contains(stage1.NoQuery, t) {
					var reading *TypeGrounding
					if reading, messages[i], err = matchi.Write(scope, c.Writer, matchi.Spec[TypeGrounding]{Name: "Grounding", Instructions: groundingPrompt, Input: input}); err == nil {
						problem.Grounding = reading.Grounding
					}
				} else {
					var reading *TypeReading
					if reading, messages[i], err = matchi.Write(scope, c.Writer, matchi.Spec[TypeReading]{Name: "Reading", Instructions: prompt, Input: input}); err == nil {
						problem.Reading, problem.Grounding, problem.Moves = shares[t], reading.Grounding, reading.Moves
					}
				}
				if err != nil {
					return fmt.Errorf("유형 %s: %w", t, err)
				}
				dropped[i] = Settle(&problem, source)
				priors.FillTimes(&problem, source)
				out.Problems[i] = problem
				return nil
			})
			if err != nil {
				return fmt.Errorf("민원 %s: %w", id, err)
			}
			referents, resolved, err := resolveReferents(matchi.WithScope(ctx, id, "referent"), c.Writer, referentPrompt, source, out, stage1.NoQuery)
			if err != nil {
				return fmt.Errorf("민원 %s: %w", id, err)
			}
			record.Problems, record.Referents = priors.WithPriors(out, stage1.NoQuery, resolved), referents
			for i := range stage1.Types {
				record.Messages = append(record.Messages, messages[i]...)
				record.Dropped = append(record.Dropped, dropped[i]...)
			}
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

// countWord는 수를 말하는 표현이다. 숫자와, 수를 세는 우리말(몇, 여러, 한 번, 두 대 등)이다.
var countWord = regexp.MustCompile(`[0-9０-９]|몇|여러|하나|둘|셋|넷|다섯|여섯|일곱|여덟|아홉|스무|서른|마흔|절반|반시간|시간 반|한\s?(번|대|시간|편|명|정거장|정류장|노선)|[두세네]\s?(번|대|시간|편|명|정거장|정류장|노선)|열\s?(번|대|편)`)

// FillTimes는 시간대 칸이 빈 이동에서, 모델이 about에 쓴 시간대 표현(시간대 사전의 표현, "6시반" 같은 시각)이 원문에 그대로 있으면
// 그 표현을 시간대 칸에 옮긴다. about에 쓴 것을 칸에 옮기는 일은 원문 대조로 정해지므로 코드가 한다.
func (p *Priors) FillTimes(problem *Problem, source string) {
	flat := textx.Squeeze(source)
	sentences, lines := Sentences(source), strings.Split(source, "\n")
	for i := range problem.Moves {
		m := &problem.Moves[i]
		if len(m.Time) > 0 || strings.TrimSpace(m.About) == "" {
			continue
		}
		about, seen := textx.Squeeze(m.About), map[string]bool{}
		add := func(text string) {
			if key := textx.Squeeze(text); key != "" && !seen[key] && strings.Contains(flat, key) {
				seen[key] = true
				m.Time = append(m.Time, TimeMention{Text: text, Quote: quoteOf(text, nil, sentences, lines)})
			}
		}
		for _, entry := range p.Times {
			if key := textx.Squeeze(entry.Key); key != "" && strings.Contains(about, key) {
				add(entry.Key)
			}
		}
		for _, loc := range clock.FindAllStringIndex(m.About, -1) {
			if rest := m.About[loc[1]:]; strings.HasPrefix(rest, "간") {
				continue
			}
			add(m.About[loc[0]:loc[1]])
		}
	}
}

var (
	// 지칭 바로 뒤에 붙어 그곳이 노선의 끝점임을 말하는 표현.
	routeEndAfter = regexp.MustCompile(`^(을|를)?(기점|종점|회차|반환)`)
	// 노선이 주어인 문장: "543버스의 경우", "543번은".
	routeSubject = regexp.MustCompile(`[0-9]+(번|버스)(의경우|는|은|이|가)`)
)

// RouteEnd는 원문 문장에서 그 장소를 노선의 기점·종점·회차지로 말했는지다. 지칭 바로 뒤에 "기점", "종점", "회차", "반환"이 오거나,
// 노선이 주어인 문장에서 "~에서 출발하여"로 나온 장소다.
func RouteEnd(text, quote string) bool {
	flat, name := textx.Squeeze(quote), textx.Squeeze(text)
	at := strings.Index(flat, name)
	if name == "" || at < 0 {
		return false
	}
	rest := flat[at+len(name):]
	if routeEndAfter.MatchString(rest) {
		return true
	}
	return (strings.HasPrefix(rest, "에서출발하여") || strings.HasPrefix(rest, "에서출발해")) && routeSubject.MatchString(flat[:at])
}

// shareIssues는 유형별 사정에서 빠진 유형을 고칠 내용으로 돌려준다. 거듭 쓴 유형은 이어 붙이므로 고치게 하지 않는다.
func shareIssues(s *Shares, types []string) string {
	count := map[string]int{}
	for _, r := range s.Readings {
		if strings.TrimSpace(r.Reading) != "" {
			count[r.Type]++
		}
	}
	issues := []string{}
	for _, t := range types {
		if count[t] == 0 {
			issues = append(issues, t+"의 reading을 쓴다")
		}
	}
	return strings.Join(issues, "\n")
}

func fileHash(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return jsonx.SHA(string(data)), nil
}
