// Package segments는 운행 기록이 연달아 지난 두 정류장 사이 시간을 모아 구간별 소요를 낸다.
// 한 구간은 앞 정류장 도착부터 다음 정류장 도착까지라 정차 시간을 포함하고,
// 시각 없이 지난 정류장은 건너뛰어 다음 시각이 있는 정류장까지를 한 구간으로 둔다. 어떤 운행도 연달아 지나지 않은
// 두 정류장은 직선 거리와 같은 자료의 정류장 간 평균 속도로 잰다. 같은 표본은 PostgreSQL의 segment_sample에도 있다.
package segments

import (
	"math"
	"sort"
	"strconv"
	"strings"

	"dllio/backend/journey"
	"dllio/backend/places"
)

type sample struct{ seconds, clock int }

type Table struct {
	pairs           map[string][]sample
	metersPerSecond float64 // 0이면 속도를 잴 표본이 없다
	basis           string
}

func key(a, b string) string { return a + ">" + b }

func timed(e []int, i int) (arrival, departure int) {
	arrival, departure = e[i+1], e[i+2]
	if arrival < 0 {
		arrival = e[i+2]
	}
	if departure < 0 {
		departure = e[i+1]
	}
	return
}

// Meters는 segments.ts의 거리 계산(지구 반지름 6,371,008.8m)과 같다.
func Meters(a, b journey.Stop) float64 {
	rad := math.Pi / 180
	dLat, dLon := (b.Lat-a.Lat)*rad, (b.Lon-a.Lon)*rad
	sLat, sLon := math.Sin(dLat/2), math.Sin(dLon/2)
	h := sLat*sLat + math.Cos(a.Lat*rad)*math.Cos(b.Lat*rad)*sLon*sLon
	return 2 * 6371008.8 * math.Asin(math.Sqrt(h))
}

func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	p := float64(len(sorted)-1) * q
	lo, hi := int(math.Floor(p)), int(math.Ceil(p))
	return sorted[lo] + (sorted[hi]-sorted[lo])*(p-float64(lo))
}

func minutes(seconds float64) float64 { return math.Floor(seconds/6+0.5) / 10 }

// NewTable은 자료 판의 모든 운행 기록에서 구간 표본을 모은다.
func NewTable(n *journey.Network) *Table {
	t := &Table{pairs: map[string][]sample{}, basis: n.Basis}
	var speeds []float64
	for _, trip := range n.Trips {
		e, last := trip.Events, -1
		for i := 0; i < len(e); i += 5 {
			arrival, _ := timed(e, i)
			if arrival < 0 {
				continue
			}
			if last >= 0 && e[last] != e[i] {
				before, _ := timed(e, last)
				if seconds := arrival - before; seconds >= 0 {
					a, b := n.Stops[e[last]], n.Stops[e[i]]
					k := key(a.ID, b.ID)
					t.pairs[k] = append(t.pairs[k], sample{seconds, before})
					if seconds > 0 {
						speeds = append(speeds, Meters(a, b)/float64(seconds))
					}
				}
			}
			last = i
		}
	}
	sort.Float64s(speeds)
	if len(speeds) > 0 {
		t.metersPerSecond = quantile(speeds, 0.5)
	}
	return t
}

// Step은 두 정류장 사이 한 구간의 견적이다.
type Step struct {
	FromID      string   `json:"fromId"`
	From        string   `json:"from"`
	ToID        string   `json:"toId"`
	To          string   `json:"to"`
	Minutes     *float64 `json:"minutes"`
	LowMinutes  *float64 `json:"lowMinutes"`
	HighMinutes *float64 `json:"highMinutes"`
	Samples     int      `json:"samples"`
	Source      string   `json:"source"`
	Window      *string  `json:"window"`
	Meters      *int     `json:"meters"`
}

func ptr[T any](v T) *T { return &v }

// step: 요청 시간대에 표본이 세 건 이상이면 그 시간대 값만, 아니면 하루 전체 값을 쓴다.
func (t *Table) step(n *journey.Network, from, to, start, end int) Step {
	a, b := n.Stops[from], n.Stops[to]
	all := t.pairs[key(a.ID, b.ID)]
	var inWindow []sample
	for _, s := range all {
		if s.clock >= start && s.clock < end {
			inWindow = append(inWindow, s)
		}
	}
	use, window := all, "하루 전체"
	if len(inWindow) >= 3 {
		use, window = inWindow, "요청 시간대"
	}
	out := Step{FromID: a.ID, From: a.Name, ToID: b.ID, To: b.Name}
	if len(use) > 0 {
		v := make([]float64, len(use))
		for i, s := range use {
			v[i] = float64(s.seconds)
		}
		sort.Float64s(v)
		out.Minutes, out.LowMinutes, out.HighMinutes = ptr(minutes(quantile(v, .5))), ptr(minutes(quantile(v, .25))), ptr(minutes(quantile(v, .75)))
		out.Samples, out.Window = len(use), &window
		out.Source = "제공 시간표"
		if t.basis == "observed" {
			out.Source = "관측 운행 기록"
		}
		return out
	}
	m := Meters(a, b)
	if t.metersPerSecond > 0 {
		out.Minutes = ptr(minutes(m / t.metersPerSecond))
	}
	out.Source, out.Meters = "직선 거리와 같은 자료의 정류장 간 평균 속도", ptr(int(math.Floor(m+0.5)))
	return out
}

// Estimate는 정류장 순서 전체의 견적이다. 이웃한 두 정류장의 기록을 쓰고, 없으면 기록이 연달아 지난 더 먼 정류장까지의
// 구간(최대 reach개 건너)을, 그것도 없으면 직선 거리를 쓴다.
func (t *Table) Estimate(n *journey.Network, path []int, start, end int) ([]Step, *float64) {
	const reach = 8
	type best struct{ distance, steps, prev int }
	b := make([]*best, len(path))
	b[0] = &best{0, 0, -1}
	for j := 0; j < len(path)-1; j++ {
		here := b[j]
		if here == nil {
			continue
		}
		for k := j + 1; k < min(len(path), j+1+reach); k++ {
			_, known := t.pairs[key(n.Stops[path[j]].ID, n.Stops[path[k]].ID)]
			if !known && k > j+1 {
				continue
			}
			next := &best{here.distance, here.steps + 1, j}
			if !known {
				next.distance++
			}
			if old := b[k]; old == nil || next.distance < old.distance || next.distance == old.distance && next.steps > old.steps {
				b[k] = next
			}
		}
	}
	var cuts []int
	for k := len(path) - 1; k >= 0; k = b[k].prev {
		cuts = append([]int{k}, cuts...)
	}
	steps := make([]Step, 0, len(cuts))
	total, known := 0.0, true
	for i := 1; i < len(cuts); i++ {
		s := t.step(n, path[cuts[i-1]], path[cuts[i]], start, end)
		steps = append(steps, s)
		if s.Minutes == nil {
			known = false
		} else {
			total += *s.Minutes
		}
	}
	if !known {
		return steps, nil
	}
	return steps, ptr(math.Floor(total*10+0.5) / 10)
}

// Path는 노선 기록에서 출발 정류장 뒤에 도착 정류장을 지나는 정류장 순서다.
type Path struct {
	Stops  []int
	Routes []string
	Trips  int   // 출발 시각이 요청 시간대에 든 운행 수
	Direct []int // 그 운행들이 실제로 걸린 초
}

// RecordedPaths는 가장 많은 운행이 지난 순서부터 돌려준다.
func RecordedPaths(n *journey.Network, routes []string, origins, destinations map[string]bool, start, end int) []*Path {
	wanted := map[string]bool{}
	for _, r := range routes {
		wanted[places.RouteBase(r)] = true
	}
	found := map[string]*Path{}
	var order []string
	for _, trip := range n.Trips {
		route := places.RouteBase(trip.Route)
		if len(wanted) > 0 && !wanted[route] {
			continue
		}
		e := trip.Events
		for i := 0; i < len(e); i += 5 {
			if !origins[n.Stops[e[i]].ID] {
				continue
			}
			k := i + 5
			for k < len(e) && !destinations[n.Stops[e[k]].ID] && !origins[n.Stops[e[k]].ID] {
				k += 5
			}
			if k >= len(e) || !destinations[n.Stops[e[k]].ID] {
				continue
			}
			var stops []string
			var idx []int
			for j := i; j <= k; j += 5 {
				stops = append(stops, strconv.Itoa(e[j]))
				idx = append(idx, e[j])
			}
			pk := strings.Join(stops, ",")
			p := found[pk]
			if p == nil {
				p = &Path{Stops: idx}
				found[pk] = p
				order = append(order, pk)
			}
			if !contains(p.Routes, route) {
				p.Routes = append(p.Routes, route)
			}
			_, leave := timed(e, i)
			arrive, _ := timed(e, k)
			if leave >= start && leave < end {
				p.Trips++
				if leave >= 0 && arrive >= leave {
					p.Direct = append(p.Direct, arrive-leave)
				}
			}
		}
	}
	out := make([]*Path, len(order))
	for i, k := range order {
		out[i] = found[k]
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Trips != out[j].Trips {
			return out[i].Trips > out[j].Trips
		}
		return len(out[i].Stops) < len(out[j].Stops)
	})
	return out
}

type Summary struct {
	Trips       int     `json:"trips"`
	Minutes     float64 `json:"minutes"`
	LowMinutes  float64 `json:"lowMinutes"`
	HighMinutes float64 `json:"highMinutes"`
}

// DirectSummary는 실제 운행 소요의 중앙값과 사분위다.
func DirectSummary(direct []int) *Summary {
	if len(direct) == 0 {
		return nil
	}
	v := make([]float64, len(direct))
	for i, d := range direct {
		v[i] = float64(d)
	}
	sort.Float64s(v)
	return &Summary{len(v), minutes(quantile(v, .5)), minutes(quantile(v, .25)), minutes(quantile(v, .75))}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
