package extract

import (
	"context"
	"regexp"
	"slices"
	"strings"

	"dllio/backend/matchi"
	"dllio/backend/textx"
)

// 지시 표현(여기, 거기, 이곳, 우리 동네, 집 같은 말)인 장소 지칭은 장소 검색에 걸리지 않는다. 추출 뒤 그런 지칭마다 언어 모델에게
// 그 표현이 가리키는 장소를 원문에서 한 번 더 뽑게 한다. 답은 원문에 쓰인 지칭 하나이거나, 원문 안에서 정할 수 없으면 빈 글이다.

var deictic = regexp.MustCompile(`^(여기|거기|저기|이곳|그곳|저곳|이쪽|그쪽|저쪽|(이|우리|저희)?동네|(우리|저희)?집)(에서는|에서|에는|에|으로|로|까지|부터|는|은|도|서)?$`)

// Deictic은 지칭이 지시 표현인지다.
func Deictic(text string) bool { return deictic.MatchString(textx.Squeeze(text)) }

// Referent는 지시 표현이 가리키는 원문 지칭이다.
type Referent struct {
	Text  string `json:"text" desc:"지시 표현이 가리키는 장소의 지칭. 원문에 쓰인 그대로 하나. 원문 안에서 정할 수 없으면 빈 글"`
	Quote string `json:"quote" desc:"그 지칭이 나온 원문 문장의 발췌. 원문 그대로. text가 비면 빈 글"`
}

// ReferentCheck는 답이 원문에 그대로 있는 지칭인지 본다.
func ReferentCheck(source string) func(*Referent) string {
	flat := textx.Squeeze(source)
	return func(r *Referent) string {
		text, quote := textx.Squeeze(r.Text), textx.Squeeze(r.Quote)
		switch {
		case text == "" && quote == "":
			return ""
		case text == "" || quote == "":
			return "text와 quote는 함께 쓰거나 함께 비운다"
		case !strings.Contains(flat, text) || !strings.Contains(flat, quote):
			return "«" + r.Text + "» / «" + r.Quote + "»가 원문에 그대로 있지 않다. 원문에 쓰인 그대로 쓰거나 비운다"
		case !strings.Contains(quote, text):
			return "«" + r.Text + "»가 그 quote 안에 없다"
		case Deictic(r.Text):
			return "«" + r.Text + "»도 지시 표현이다. 그것이 가리키는 장소의 지칭을 쓰거나 비운다"
		}
		return ""
	}
}

// ReferentRequest는 지시 표현 하나를 풀게 하는 입력이다.
func ReferentRequest(source, text, quote string) string {
	return strings.Join([]string{"원문:", source, "", "지시 표현: «" + text + "»", "나온 문장: «" + quote + "»"}, "\n")
}

// ReferentRecord는 지시 표현 하나를 푼 기록이다.
type ReferentRecord struct {
	Text     string           `json:"text"`
	Quote    string           `json:"quote"`
	RefersTo *Referent        `json:"refersTo"`
	Messages []matchi.Message `json:"messages"`
}

func referentKey(text, quote string) string { return text + "\x00" + quote }

// deicticMentions는 조회하는 유형의 이동에서 지시 표현인 장소 지칭을 나온 순서대로 돌려준다. 글자와 문장이 같으면 한 번이다.
func deicticMentions(out *Extraction, noQuery []string) [][2]string {
	var found [][2]string
	seen := map[string]bool{}
	add := func(text, quote string) {
		if Deictic(text) && !seen[referentKey(text, quote)] {
			seen[referentKey(text, quote)] = true
			found = append(found, [2]string{text, quote})
		}
	}
	for _, p := range out.Problems {
		if slices.Contains(noQuery, p.Type) {
			continue
		}
		for _, m := range p.Moves {
			for _, x := range append(append([]PlaceMention{}, m.Origin...), m.Destination...) {
				add(x.Text, x.Quote)
			}
			for _, x := range m.Targets {
				if x.Kind != "route" {
					add(x.Text, x.Quote)
				}
			}
		}
	}
	return found
}

// resolveReferents는 지시 표현마다 가리키는 원문 지칭을 묻는다. 원문 안에서 정할 수 없는 표현은 RefersTo가 nil이다.
func resolveReferents(ctx context.Context, w *matchi.Writer, prompt, source string, out *Extraction, noQuery []string) ([]ReferentRecord, map[string]*Referent, error) {
	records, resolved := []ReferentRecord{}, map[string]*Referent{}
	for _, m := range deicticMentions(out, noQuery) {
		answer, messages, err := matchi.Write(ctx, w, matchi.Spec[Referent]{Name: "Referent", Instructions: prompt,
			Input: ReferentRequest(source, m[0], m[1]), Check: ReferentCheck(source)})
		if err != nil {
			return nil, nil, err
		}
		var refers *Referent
		if strings.TrimSpace(answer.Text) != "" {
			refers = answer
		}
		resolved[referentKey(m[0], m[1])] = refers
		records = append(records, ReferentRecord{m[0], m[1], refers, messages})
	}
	return records, resolved, nil
}
