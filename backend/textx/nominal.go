package textx

import (
	"regexp"
	"strings"
)

const (
	hangulBase  = 0xAC00
	hangulLast  = 0xD7A3
	finalsCount = 28
	finalNieun  = 4  // ㄴ
	finalRieul  = 8  // ㄹ
	finalMieum  = 16 // ㅁ
	finalBieup  = 17 // ㅂ
	finalRm     = 10 // ㄻ
)

// irregularD는 ㄷ 받침이 명사형에서 ㄹ로 바뀌는 음절이다(듣는다 → 들음, 걷는다 → 걸음).
var irregularD = map[rune]bool{'듣': true, '걷': true, '싣': true}

// irregularB는 ㅂ 받침이 명사형에서 '움'으로 바뀌는 음절이다(어렵다 → 어려움).
var irregularB = map[rune]bool{'롭': true, '럽': true, '렵': true, '깝': true, '겁': true, '쉽': true, '밉': true, '덥': true, '춥': true, '곱': true, '맵': true, '볍': true}

func syllable(r rune) (int, bool) {
	if r < hangulBase || r > hangulLast {
		return 0, false
	}
	return int(r-hangulBase) % finalsCount, true
}

func withFinal(r rune, final int) rune {
	return r - rune(int(r-hangulBase)%finalsCount) + rune(final)
}

// nounOfStem은 용언 어간을 명사형(-ㅁ/-음)으로 바꾼다.
func nounOfStem(stem string) string {
	runes := []rune(stem)
	if len(runes) == 0 {
		return stem
	}
	last := runes[len(runes)-1]
	final, ok := syllable(last)
	if !ok {
		return stem
	}
	head := string(runes[:len(runes)-1])
	switch {
	case final == 0:
		return head + string(withFinal(last, finalMieum))
	case final == finalRieul:
		return head + string(withFinal(last, finalRm))
	case final == finalBieup && irregularB[last]:
		return head + string(withFinal(last, 0)) + "움"
	case final == 7 && irregularD[last]: // ㄷ
		return head + string(withFinal(last, finalRieul)) + "음"
	default:
		return stem + "음"
	}
}

// nounOfPredicate는 '~다'로 끝나는 서술어 하나(공백 없는 어절)를 명사형으로 바꾼다. 바꿀 수 없으면 그대로 돌려준다.
func nounOfPredicate(word string) string {
	runes := []rune(word)
	if len(runes) < 2 || runes[len(runes)-1] != '다' {
		return word
	}
	body := runes[:len(runes)-1]
	if len(body) >= 1 && body[len(body)-1] == '는' && len(body) >= 2 {
		// 겪는다 → 겪음, 않는다 → 않음
		return nounOfStem(string(body[:len(body)-1]))
	}
	last := body[len(body)-1]
	final, ok := syllable(last)
	if !ok {
		return word
	}
	if final == finalNieun && len(body) >= 2 {
		// 한다 → 함, 된다 → 됨, 기다린다 → 기다림
		return string(body[:len(body)-1]) + string(withFinal(last, finalMieum))
	}
	if final == finalNieun && len(body) == 1 && (last == '한' || last == '된' || last == '탄' || last == '간' || last == '온' || last == '준') {
		return string(withFinal(last, finalMieum))
	}
	if last == '이' && len(body) >= 1 {
		// 기본이다 → 기본임
		return string(body[:len(body)-1]) + "임"
	}
	return nounOfStem(string(body))
}

var (
	// 모델이 입력의 '이동'이라는 이름을 옮겨 "~하는 이동이 있다", "~인 이동을 한다"처럼 쓴 꼬리.
	bareMove = regexp.MustCompile(`\s*(이동|이용 환경|상황)(이|을|를)\s*(있다|있음|있습니다|한다|함|합니다|겪는다|겪음|겪습니다)\.?$`)
	sentence = regexp.MustCompile(`[.。]\s*$`)
)

// Nominal은 상황 문장을 명사형으로 맺는다. "~하는 이동이 있다"처럼 이동이 있다는 꼬리만 붙은 문장은 그 앞의 서술을 명사형으로 끝내고,
// '~다'로 끝나는 문장은 마지막 서술어를 명사형('~함', '~됨', '~있음')으로 바꾼다. 바꿀 수 없는 문장은 마침표만 뗀다.
func Nominal(text string) string {
	text = strings.TrimSpace(sentence.ReplaceAllString(strings.TrimSpace(text), ""))
	if loc := bareMove.FindStringIndex(text); loc != nil {
		head := strings.TrimSpace(text[:loc[0]])
		words := strings.Fields(head)
		if len(words) > 0 {
			last := []rune(words[len(words)-1])
			switch tail := last[len(last)-1]; {
			case tail == '는' && len(last) >= 2:
				// 이용하는 → 이용함, 겪는 → 겪음
				words[len(words)-1] = nounOfStem(string(last[:len(last)-1]))
				return strings.Join(words, " ")
			case tail == '인' && len(last) >= 2:
				// 기본인 → 기본임
				words[len(words)-1] = string(last[:len(last)-1]) + "임"
				return strings.Join(words, " ")
			default:
				if final, ok := syllable(tail); ok && final == finalNieun && len(last) >= 2 {
					// 감축된 → 감축됨, 줄어든 → 줄어듬
					last[len(last)-1] = withFinal(tail, finalMieum)
					words[len(words)-1] = string(last)
					return strings.Join(words, " ")
				}
			}
		}
	}
	words := strings.Fields(text)
	if len(words) == 0 {
		return text
	}
	words[len(words)-1] = nounOfPredicate(words[len(words)-1])
	return strings.Join(words, " ")
}
