// Package calendar는 날짜의 요일·공휴일과 요일 종류(평일·토요일·일요일·공휴일)를 정한다.
// 공휴일은 tools/build_holidays.py가 만든 data/priors/holidays.json의 한국 달력이다.
package calendar

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"dllio/backend/repo"
	"dllio/backend/textx"
)

const weekdays = "월화수목금토일"

// Kinds는 요일 종류를 정해진 순서로 둔다.
var Kinds = []string{"평일", "토요일", "일요일·공휴일"}

// Calendar는 공휴일 표다.
type Calendar struct {
	holidays map[string]string
	first    int
	last     int
}

// Load는 공휴일 표를 읽는다.
func Load(root string) (*Calendar, error) {
	text, err := repo.ReadText(filepath.Join(root, "data", "priors", "holidays.json"))
	if err != nil {
		return nil, err
	}
	var file struct {
		Years    [2]int            `json:"years"`
		Holidays map[string]string `json:"holidays"`
	}
	if err := json.Unmarshal([]byte(text), &file); err != nil {
		return nil, err
	}
	return &Calendar{file.Holidays, file.Years[0], file.Years[1]}, nil
}

// Day는 날짜 하나의 요일과 공휴일이다.
type Day struct {
	Date    string  `json:"date"`
	Weekday string  `json:"weekday"`
	Holiday *string `json:"holiday"`
	Kind    string  `json:"kind"`
	Label   string  `json:"label"`
}

func weekday(t time.Time) string {
	return string([]rune(weekdays)[(int(t.Weekday())+6)%7])
}

// Info는 날짜(YYYY-MM-DD)의 요일, 공휴일 이름, 요일 종류다.
func (c *Calendar) Info(date string) (Day, error) {
	t, err := time.Parse(time.DateOnly, date)
	if err != nil {
		return Day{}, err
	}
	return c.InfoOf(t)
}

// InfoOf는 날짜의 요일, 공휴일 이름, 요일 종류다.
func (c *Calendar) InfoOf(t time.Time) (Day, error) {
	if t.Year() < c.first || t.Year() > c.last {
		return Day{}, fmt.Errorf("공휴일 표의 범위(%d~%d년) 밖의 날짜: %s", c.first, c.last, t.Format(time.DateOnly))
	}
	date, name := t.Format(time.DateOnly), weekday(t)
	day := Day{Date: date, Weekday: name + "요일", Label: date + "(" + name + ")", Kind: "평일"}
	if holiday, ok := c.holidays[date]; ok {
		day.Holiday = &holiday
		day.Label += " " + holiday
	}
	switch {
	case day.Holiday != nil || t.Weekday() == time.Sunday:
		day.Kind = "일요일·공휴일"
	case t.Weekday() == time.Saturday:
		day.Kind = "토요일"
	}
	return day, nil
}

var (
	relative = regexp.MustCompile(`(이번|지난|저번|다음)[` + textx.Space + `]*(?:주[` + textx.Space + `]*)?([월화수목금토일])요일`)
	excluded = regexp.MustCompile(`^[` + textx.Space + `]*(?:제외|빼고|빼면|말고)`)
	near     = []struct {
		word   string
		offset int
	}{{"그제", -2}, {"그저께", -2}, {"어제", -1}, {"오늘", 0}, {"내일", 1}}
)

// said는 words 중 하나가 나오고 '제외' 같은 말로 빠지지 않았는지다. '주말제외'는 주말이 아니다.
func said(text string, words ...string) bool {
	for i := range text {
		for _, w := range words {
			if strings.HasPrefix(text[i:], w) && !excluded.MatchString(text[i+len(w):]) {
				return true
			}
		}
	}
	return false
}

// weekdayKinds는 '월요일' 같은 요일 말마다의 요일 종류다. 뒤에 '제외' 같은 말이 붙은 것은 뺀다.
func weekdayKinds(text string) []string {
	out := []string{}
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if strings.ContainsRune(weekdays, r) && strings.HasPrefix(text[i+size:], "요일") && !excluded.MatchString(text[i+size+len("요일"):]) {
			switch r {
			case '토':
				out = append(out, "토요일")
			case '일':
				out = append(out, "일요일·공휴일")
			default:
				out = append(out, "평일")
			}
			i += size + len("요일")
			continue
		}
		i += size
	}
	return out
}

// DatedMention은 접수일을 기준으로 날짜를 풀 수 있는 표현이다.
type DatedMention struct {
	Text string `json:"text"`
	Day
}

// Mentions는 원문 표현의 요일 종류와 접수일 기준으로 풀린 날짜다.
type Mentions struct {
	Kinds []string       `json:"kinds"`
	Dates []DatedMention `json:"dates"`
}

// MentionsIn은 원문 표현의 요일 종류와 접수일(received) 기준으로 풀린 날짜다. 요일 표현이 없으면 nil이다.
func (c *Calendar) MentionsIn(text, received string) (*Mentions, error) {
	base, err := time.Parse(time.DateOnly, received)
	if err != nil {
		return nil, err
	}
	kinds, dates := []string{}, []DatedMention{}
	for _, m := range relative.FindAllStringSubmatch(text, -1) {
		monday := base.AddDate(0, 0, -((int(base.Weekday()) + 6) % 7))
		offset := map[string]int{"이번": 0, "지난": -7, "저번": -7, "다음": 7}[m[1]]
		day, err := c.InfoOf(monday.AddDate(0, 0, offset+slices.Index([]rune(weekdays), []rune(m[2])[0])))
		if err != nil {
			return nil, err
		}
		dates = append(dates, DatedMention{m[1] + " " + m[2] + "요일", day})
	}
	for _, n := range near {
		if strings.Contains(text, n.word) {
			day, err := c.InfoOf(base.AddDate(0, 0, n.offset))
			if err != nil {
				return nil, err
			}
			dates = append(dates, DatedMention{n.word, day})
		}
	}
	kinds = append(kinds, weekdayKinds(text)...)
	if said(text, "주말") {
		kinds = append(kinds, "토요일", "일요일·공휴일")
	}
	if said(text, "평일", "주중") {
		kinds = append(kinds, "평일")
	}
	if said(text, "공휴일", "휴일", "명절") {
		kinds = append(kinds, "일요일·공휴일")
	}
	for _, d := range dates {
		kinds = append(kinds, d.Kind)
	}
	if len(kinds) == 0 {
		return nil, nil
	}
	ordered := []string{}
	for _, k := range Kinds {
		if slices.Contains(kinds, k) {
			ordered = append(ordered, k)
		}
	}
	return &Mentions{ordered, dates}, nil
}

// RevisionDay는 자료 판의 요일 종류다. 관측일이 없는 판은 Day가 nil이다.
type RevisionDay struct {
	Day  *string `json:"day"`
	Note string  `json:"note"`
}

// RevisionDays는 자료 판마다 그 날의 요일 종류다. 관측일이 있는 판은 달력으로 읽고, 관측일이 없는 제공 시간표는 요일을 모른다.
func (c *Calendar) RevisionDays(root string) (map[string]RevisionDay, error) {
	text, err := repo.ReadText(filepath.Join(repo.RegionDir(root), "transit", "revisions.json"))
	if err != nil {
		return nil, err
	}
	var file struct {
		Revisions []struct {
			ID       string  `json:"id"`
			Observed *string `json:"observed"`
		} `json:"revisions"`
	}
	if err := json.Unmarshal([]byte(text), &file); err != nil {
		return nil, err
	}
	out := map[string]RevisionDay{}
	for _, r := range file.Revisions {
		if r.Observed == nil || *r.Observed == "" {
			out[r.ID] = RevisionDay{nil, "적용 요일이 수록되지 않은 시간표"}
			continue
		}
		info, err := c.Info(*r.Observed)
		if err != nil {
			return nil, err
		}
		kind := info.Kind
		out[r.ID] = RevisionDay{&kind, info.Label}
	}
	return out, nil
}
