// Package places는 원문의 지칭을 장소 후보로 잇는다.
// 장소는 판별 소속 정류장과 그 정류장에 서는 노선을 가지며, 후보를 가르는 사실(행정동, 노선, 주변 장소)을 함께 준다.
package places

import (
	"encoding/json"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"

	"dllio/backend/journey"
)

type Member struct {
	ID    string        `json:"id"`
	Name  string        `json:"name"`
	Point journey.Point `json:"point"`
}

// Place는 장소 하나. Datasets는 소속 정류장이 있는 판을 판 순서로 담는다.
type Place struct {
	ID       string
	Name     string
	Aliases  []string
	Point    journey.Point
	Datasets []string
	Members  map[string][]Member
	Routes   map[string][]string
}

var (
	parens     = regexp.MustCompile(`\([^)]*\)`)
	stopWords  = regexp.MustCompile(`\s|정류소|정류장`)
	lastParen  = regexp.MustCompile(`\s*\([^()]*\)\s*$`)
	areaWords  = regexp.MustCompile(`시내|주변|일대|아파트`)
	tagoCode   = regexp.MustCompile(`^BS_TAGO_USB\d+$`)
	fiveDigits = regexp.MustCompile(`^\d{5}$`)
	dotted     = regexp.MustCompile(`[.·]`)
)

// Normalize는 괄호 내용과 공백, '정류소'·'정류장'을 뺀 이름이다.
func Normalize(name string) string {
	return stopWords.ReplaceAllString(parens.ReplaceAllString(name, ""), "")
}

// RouteBase는 노선명에서 마지막 괄호를 뺀 노선 번호다.
func RouteBase(route string) string {
	return strings.TrimSpace(lastParen.ReplaceAllString(route, ""))
}

func length(s string) int { return utf8.RuneCountInString(s) }

// StopCodes는 글에 적힌 다섯 자리 정류장 번호다(앞뒤에 숫자가 붙지 않은 것).
func StopCodes(text string) []string {
	var out []string
	seen := map[string]bool{}
	r := []rune(text)
	for i := 0; i < len(r); {
		if r[i] < '0' || r[i] > '9' {
			i++
			continue
		}
		j := i
		for j < len(r) && r[j] >= '0' && r[j] <= '9' {
			j++
		}
		if j-i == 5 && !seen[string(r[i:j])] {
			seen[string(r[i:j])] = true
			out = append(out, string(r[i:j]))
		}
		i = j
	}
	return out
}

func memberCode(id string) string {
	switch {
	case fiveDigits.MatchString(id):
		return id
	case tagoCode.MatchString(id):
		return id[len(id)-5:]
	}
	return ""
}

// Codes는 장소 소속 정류장의 공개 번호다.
func (p *Place) Codes() []string {
	out := []string{}
	seen := map[string]bool{}
	for _, d := range p.Datasets {
		for _, m := range p.Members[d] {
			if c := memberCode(m.ID); c != "" && !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	return out
}

// Catalogue는 장소 목록과 행정동 경계다.
type Catalogue struct {
	Places []*Place
	Areas  []Area
	byID   map[string]*Place
}

func NewCatalogue(places []*Place, areas []Area) *Catalogue {
	c := &Catalogue{Places: places, Areas: areas, byID: map[string]*Place{}}
	for _, p := range places {
		c.byID[p.ID] = p
	}
	return c
}

func (c *Catalogue) Find(id string) *Place { return c.byID[id] }

// sharedFragments는 지칭의 이어진 두 글자 묶음 중 이름에도 있는 수와 전체 수다.
func sharedFragments(mention, name string) (int, int) {
	pairs := func(text string) map[string]bool {
		r, out := []rune(text), map[string]bool{}
		for i := 0; i+1 < len(r); i++ {
			out[string(r[i:i+2])] = true
		}
		return out
	}
	a, b, shared := pairs(mention), pairs(name), 0
	for p := range a {
		if b[p] {
			shared++
		}
	}
	return shared, len(a)
}

// Match는 지칭이 가리킬 수 있는 장소다. 정류장 번호가 있으면 번호로, 없으면 이름과 별칭으로 찾고 원문 노선이 서는 곳을 앞에 둔다.
func (c *Catalogue) Match(text string, routeNames []string, limit int) []*Place {
	if codes := StopCodes(text); len(codes) > 0 {
		var coded []*Place
		for _, p := range c.Places {
			for _, code := range p.Codes() {
				if contains(codes, code) {
					coded = append(coded, p)
					break
				}
			}
		}
		if len(coded) > 0 {
			sort.SliceStable(coded, func(i, j int) bool { return coded[i].ID < coded[j].ID })
			return head(coded, limit)
		}
	}
	mention := Normalize(text)
	score := func(p *Place) int {
		name := Normalize(p.Name)
		value := 0
		for _, n := range append([]string{p.Name}, p.Aliases...) {
			alias, v := Normalize(n), 0
			switch {
			case alias == mention:
				v = 120
			case length(alias) >= 2 && strings.Contains(mention, alias):
				v = 75 + min(length(alias), 25)
			case length(mention) >= 2 && strings.Contains(alias, mention):
				v = 65
			}
			value = max(value, v)
		}
		if value == 0 {
			for _, w := range areaWords.Split(mention, -1) {
				if length(w) >= 3 && strings.Contains(name, w) {
					value = 45
					break
				}
			}
		}
		// 줄여 말한 이름('현대차명촌문')은 통째로 겹치지 않으므로, 이어진 두 글자 묶음이 절반 이상 겹치면 후보로 둔다. 맞는 곳인지는 판정 모델이 고른다.
		if value == 0 {
			if shared, total := sharedFragments(mention, name); shared >= 2 && shared*2 >= total {
				value = 20 + min(shared*5, 20)
			}
		}
		if value > 0 && p.servesAny(routeNames) {
			value += 10
		}
		return value
	}
	type ranked struct {
		p     *Place
		score int
	}
	var list []ranked
	for _, p := range c.Places {
		if s := score(p); s > 0 {
			list = append(list, ranked{p, s})
		}
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].score != list[j].score {
			return list[i].score > list[j].score
		}
		return list[i].p.ID < list[j].p.ID
	})
	// 더 적은 판에 수록된 장소는, 같은 이름으로 그 판을 모두 포함해 더 많은 판에 수록된 장소에 자리를 내준다.
	covered := func(p, q *Place) bool {
		if q == p || Normalize(q.Name) != Normalize(p.Name) || len(q.Datasets) <= len(p.Datasets) {
			return false
		}
		for _, d := range p.Datasets {
			if !contains(q.Datasets, d) {
				return false
			}
		}
		return true
	}
	var out []*Place
	for _, r := range list {
		hidden := false
		for _, other := range list {
			if covered(r.p, other.p) {
				hidden = true
				break
			}
		}
		if !hidden {
			out = append(out, r.p)
		}
	}
	return head(out, limit)
}

func (p *Place) servesAny(routeNames []string) bool {
	for _, d := range p.Datasets {
		for _, r := range p.Routes[d] {
			if contains(routeNames, RouteBase(r)) {
				return true
			}
		}
	}
	return false
}

// Cluster는 이름이 같거나 서로 포함하는 radius 미터 안의 장소 묶음이다(통도사 · 통도사.신평).
func (c *Catalogue) Cluster(place *Place, radius float64) []*Place {
	name := dotted.ReplaceAllString(Normalize(place.Name), "")
	var out []*Place
	for _, p := range c.Places {
		if p.ID == place.ID {
			out = append(out, p)
			continue
		}
		other := dotted.ReplaceAllString(Normalize(p.Name), "")
		if min(length(name), length(other)) >= 2 && (strings.Contains(name, other) || strings.Contains(other, name)) &&
			journey.Meters(p.Point, place.Point) <= radius {
			out = append(out, p)
		}
	}
	return out
}

// Summary는 후보를 가르는 사실이다.
type Summary struct {
	ID          string              `json:"id"`
	Name        string              `json:"name"`
	Aliases     []string            `json:"aliases"`
	Area        *string             `json:"area"`
	Datasets    []string            `json:"datasets"`
	Routes      map[string][]string `json:"routes"`
	NamedRoutes []string            `json:"namedRoutes"`
	Codes       []string            `json:"codes"`
	Point       journey.Point       `json:"point"`
}

func (c *Catalogue) Summary(p *Place, named []string) Summary {
	routes := map[string][]string{}
	for _, d := range p.Datasets {
		routes[d] = bases(p.Routes[d])
	}
	for d, list := range p.Routes {
		if _, ok := routes[d]; !ok {
			routes[d] = bases(list)
		}
	}
	aliases := []string{}
	for _, a := range p.Aliases {
		if a != p.Name {
			aliases = append(aliases, a)
		}
	}
	namedRoutes := []string{}
	for _, r := range named {
		for _, list := range routes {
			if contains(list, r) {
				namedRoutes = append(namedRoutes, r)
				break
			}
		}
	}
	return Summary{ID: p.ID, Name: p.Name, Aliases: aliases, Area: AreaOf(c.Areas, p.Point), Datasets: append([]string{}, p.Datasets...),
		Routes: routes, NamedRoutes: namedRoutes, Codes: p.Codes(), Point: p.Point}
}

func (c *Catalogue) Search(query string, named []string, limit int) []Summary {
	out := []Summary{}
	for _, p := range c.Match(query, named, limit) {
		out = append(out, c.Summary(p, named))
	}
	return out
}

type Nearby struct {
	Name   string `json:"name"`
	Meters int    `json:"meters"`
}

type Info struct {
	Summary
	Nearby  []Nearby `json:"nearby"`
	Cluster []string `json:"cluster"`
}

// Info는 장소의 사실과 주변 장소(이름마다 가장 가까운 하나)다.
func (c *Catalogue) Info(id string, named []string, radius float64, count int) *Info {
	place := c.Find(id)
	if place == nil {
		return nil
	}
	type near struct {
		p *Place
		d float64
	}
	var around []near
	for _, p := range c.Places {
		if d := journey.Meters(p.Point, place.Point); p.ID != id && d <= radius {
			around = append(around, near{p, d})
		}
	}
	sort.SliceStable(around, func(i, j int) bool { return around[i].d < around[j].d })
	seen := map[string]bool{Normalize(place.Name): true}
	nearby := []Nearby{}
	for _, v := range around {
		name := Normalize(v.p.Name)
		if seen[name] {
			continue
		}
		seen[name] = true
		nearby = append(nearby, Nearby{v.p.Name, int(jsRound(v.d))})
		if len(nearby) >= count {
			break
		}
	}
	cluster := []string{}
	for _, p := range c.Cluster(place, 1000) {
		if p.ID != id {
			cluster = append(cluster, p.ID)
		}
	}
	return &Info{c.Summary(place, named), nearby, cluster}
}

func bases(routes []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, r := range routes {
		if b := RouteBase(r); !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	// collate.Collator는 내부 버퍼를 써서 동시에 쓸 수 없다. 요청마다 따로 만든다.
	korean := collate.New(language.Korean, collate.Numeric)
	sort.SliceStable(out, func(i, j int) bool { return korean.CompareString(out[i], out[j]) < 0 })
	return out
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func head[T any](list []T, n int) []T {
	if len(list) > n {
		return list[:n]
	}
	return list
}

// jsRound는 JavaScript Math.round와 같다(0.5는 위로).
func jsRound(x float64) float64 { return math.Floor(x + 0.5) }

// Area는 행정동 하나의 경계다. GeoJSON의 고리는 [경도, 위도]다.
type Area struct {
	Sgg, Dong string
	Polygons  [][][][2]float64
}

// LoadAreas는 행정동 GeoJSON을 읽는다.
func LoadAreas(path string) ([]Area, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var g struct {
		Features []struct {
			Properties struct {
				Sgg  string `json:"sggnm"`
				Dong string `json:"dongnm"`
			} `json:"properties"`
			Geometry struct {
				Type        string          `json:"type"`
				Coordinates json.RawMessage `json:"coordinates"`
			} `json:"geometry"`
		} `json:"features"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		return nil, err
	}
	var out []Area
	for _, f := range g.Features {
		a := Area{Sgg: f.Properties.Sgg, Dong: f.Properties.Dong}
		if f.Geometry.Type == "Polygon" {
			var poly [][][2]float64
			if err := json.Unmarshal(f.Geometry.Coordinates, &poly); err != nil {
				return nil, err
			}
			a.Polygons = [][][][2]float64{poly}
		} else if err := json.Unmarshal(f.Geometry.Coordinates, &a.Polygons); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func inRing(ring [][2]float64, lon, lat float64) bool {
	inside := false
	for i, j := 0, len(ring)-1; i < len(ring); j, i = i, i+1 {
		xi, yi, xj, yj := ring[i][0], ring[i][1], ring[j][0], ring[j][1]
		if (yi > lat) != (yj > lat) && lon < (xj-xi)*(lat-yi)/(yj-yi)+xi {
			inside = !inside
		}
	}
	return inside
}

// AreaOf는 지점이 놓인 구와 동이다. 경계 밖이면 nil이다.
func AreaOf(areas []Area, p journey.Point) *string {
	lat, lon := p[0], p[1]
	for _, a := range areas {
		for _, poly := range a.Polygons {
			if !inRing(poly[0], lon, lat) {
				continue
			}
			inHole := false
			for _, hole := range poly[1:] {
				if inRing(hole, lon, lat) {
					inHole = true
					break
				}
			}
			if !inHole {
				s := a.Sgg + " " + a.Dong
				return &s
			}
		}
	}
	return nil
}
