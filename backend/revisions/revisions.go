// Package revisions는 날짜에 맞는 자료 판을 고르고, 판의 날짜 뒤에 알려진 노선 변경과 노선 번호의 이전 번호를 찾는다.
// 같은 규칙이 PostgreSQL 함수 transit.revision_at에도 있다.
package revisions

import (
	"strings"

	"dllio/backend/places"
)

type Revision struct {
	ID            string  `json:"id"`
	Label         string  `json:"label"`
	EffectiveFrom *string `json:"effectiveFrom"`
	EffectiveTo   *string `json:"effectiveTo"`
	Observed      *string `json:"observed"`
}

type Change struct {
	Route  string  `json:"route"`
	Change string  `json:"change"`
	Date   *string `json:"date"`
}

// Choice는 결과에 붙이는 판 표기다.
type Choice struct {
	ID       string  `json:"id"`
	Label    string  `json:"label"`
	Observed *string `json:"observed"`
}

func (r Revision) Choice() Choice { return Choice{r.ID, r.Label, r.Observed} }

func (r Revision) covers(day string) bool {
	return (r.EffectiveFrom == nil || *r.EffectiveFrom <= day) && (r.EffectiveTo == nil || day <= *r.EffectiveTo)
}

func (r Revision) known() bool { return r.EffectiveFrom != nil || r.EffectiveTo != nil }

// At은 그날 쓸 판이다. 그날을 덮는 판 중 기간이 정해진 판이 하나면 그 판, 아니면 덮는 판 중 가장 나중 판, 덮는 판이
// 없으면 가장 나중 판이다. 판은 시간 순서로 주어진다.
func At(list []Revision, day string) Revision {
	var covering, decided []Revision
	for _, r := range list {
		if r.covers(day) {
			covering = append(covering, r)
			if r.known() {
				decided = append(decided, r)
			}
		}
	}
	switch {
	case len(decided) == 1:
		return decided[0]
	case len(covering) > 0:
		return covering[len(covering)-1]
	}
	return list[len(list)-1]
}

// ChangesAfter는 판이 보여주는 날(관측일, 없으면 유효 시작일) 뒤에 질의한 노선에 알려진 변경이다. 날짜를 모르는 변경도 넣는다.
func ChangesAfter(list []Revision, changes []Change, revisionID string, routes []string) []Change {
	var since *string
	for _, r := range list {
		if r.ID == revisionID {
			since = r.Observed
			if since == nil {
				since = r.EffectiveFrom
			}
		}
	}
	out := []Change{}
	for _, c := range changes {
		if contains(routes, c.Route) && (c.Date == nil || since == nil || *c.Date > *since) {
			out = append(out, c)
		}
	}
	return out
}

// Sheet는 시간표 파일 시트 이름이 적은 노선 번호와 이전 번호다.
type Sheet struct {
	Route    string   `json:"route"`
	Previous []string `json:"previous"`
	Created  bool     `json:"created"`
	Note     *string  `json:"note"`
	Sheet    string   `json:"sheet"`
}

// Lineage는 한 번호와 이어진 번호다. Later는 그 번호를 이전 번호로 적은 지금 노선이다.
type Lineage struct {
	Route     string   `json:"route"`
	Previous  []string `json:"previous"`
	Created   bool     `json:"created"`
	Later     []string `json:"later"`
	InDataset []string `json:"inDataset"`
}

func LineageOf(sheets []Sheet, route string) Lineage {
	base := strings.TrimSuffix(places.RouteBase(route), "번")
	l := Lineage{Route: base, Previous: []string{}, Later: []string{}, InDataset: []string{}}
	for _, s := range sheets {
		if s.Route == base {
			for _, p := range s.Previous {
				if !contains(l.Previous, p) {
					l.Previous = append(l.Previous, p)
				}
			}
			l.Created = l.Created || s.Created
		}
		if contains(s.Previous, base) && !contains(l.Later, s.Route) {
			l.Later = append(l.Later, s.Route)
		}
	}
	return l
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
