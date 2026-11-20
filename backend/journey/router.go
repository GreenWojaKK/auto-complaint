package journey

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// 여정 탐색. 직통과 한 번 환승하는 운행 조합을 열거하고,
// 도착(또는 출발)·도보·환승 횟수에서 다른 후보보다 나쁘지 않은 후보와 원문 노선·넓은 도보 후보를 고른다.

// Endpoint는 여정의 출발지 또는 목적지다.
type Endpoint struct {
	Label  string
	Point  Point
	Source string
}

// Request는 탐색 조건이다. DepartureStart·DepartureEnd·AccessLimitMeters는 비워 두면 쓰지 않는다.
type Request struct {
	Time                  string
	ArriveBy              bool
	MaxWalkMeters         int
	MaxTransfers          int
	WalkSpeed             float64
	TransferMarginSeconds int
	FocusRoutes           []string
	DepartureStart        *int
	DepartureEnd          *int
	AccessLimitMeters     *int
	CollectBoardings      bool
}

type Leg struct {
	Kind            string  `json:"kind"`
	Route           string  `json:"route,omitempty"`
	TripID          string  `json:"tripId,omitempty"`
	From            string  `json:"from"`
	To              string  `json:"to"`
	FromID          string  `json:"fromId,omitempty"`
	ToID            string  `json:"toId,omitempty"`
	Start           int     `json:"start"`
	End             int     `json:"end"`
	FromSequence    int     `json:"fromSequence,omitempty"`
	ToSequence      int     `json:"toSequence,omitempty"`
	Meters          float64 `json:"meters,omitempty"`
	Geometry        []Point `json:"geometry"`
	SourceRows      []int   `json:"sourceRows,omitempty"`
	SourceFile      string  `json:"sourceFile,omitempty"`
	Source          string  `json:"source,omitempty"`
	Basis           string  `json:"basis,omitempty"`
	ConnectorMeters float64 `json:"connectorMeters,omitempty"`
}

type Journey struct {
	ID                   string  `json:"id"`
	Role                 string  `json:"role"`
	Departure            int     `json:"departure"`
	Arrival              int     `json:"arrival"`
	DurationSeconds      int     `json:"durationSeconds"`
	WalkMeters           float64 `json:"walkMeters"`
	WalkingSeconds       int     `json:"walkingSeconds"`
	RidingSeconds        int     `json:"ridingSeconds"`
	WaitingSeconds       int     `json:"waitingSeconds"`
	Transfers            int     `json:"transfers"`
	ArrivalMarginSeconds *int    `json:"arrivalMarginSeconds"`
	Legs                 []Leg   `json:"legs"`
	Signature            string  `json:"signature"`
}

type Result struct {
	Status        string    `json:"status"` // ok, no_journey, coverage_gap
	Journeys      []Journey `json:"journeys"`
	BoardingCount int       `json:"boardingCount"`
	From, To      int       `json:"-"`
}

var hhmm = regexp.MustCompile(`^(?:[01]\d|2[0-3]):[0-5]\d$`)

// TimeSeconds는 'HH:MM'을 0시부터의 초로 바꾼다.
func TimeSeconds(text string) (int, error) {
	if !hhmm.MatchString(text) {
		return 0, fmt.Errorf("시각을 HH:MM으로 입력해 주세요")
	}
	h, _ := strconv.Atoi(text[:2])
	m, _ := strconv.Atoi(text[3:])
	return h*3600 + m*60, nil
}

// LabelTime은 초를 'HH:MM'으로 쓴다.
func LabelTime(t int) string {
	return fmt.Sprintf("%02d:%02d", floorDiv(t, 3600), floorDiv(t%3600, 60))
}

func floorDiv(a, b int) int { return int(math.Floor(float64(a) / float64(b))) }

func ceilInt(x float64) int { return int(math.Ceil(x)) }

type ride struct{ trip, from, to int }

type plan struct {
	rides        []ride
	access       float64
	egress       float64
	transferWalk float64
	departure    int
	arrival      int
	walk         float64
	duration     int
	signature    string
}

type anchorPoint struct {
	node              int
	meters, connector float64
}

type visitRef struct{ trip, offset int }
type ending struct {
	offset int
	walk   float64
}
type reachable struct {
	stop   int
	meters float64
}

// orderedPlans는 넣은 순서를 지키는 지도다(router.ts의 Map과 같다).
type orderedPlans[K comparable] struct {
	keys  []K
	byKey map[K]*plan
}

func newOrdered[K comparable]() *orderedPlans[K] { return &orderedPlans[K]{byKey: map[K]*plan{}} }
func (o *orderedPlans[K]) get(k K) *plan         { return o.byKey[k] }
func (o *orderedPlans[K]) set(k K, p *plan) {
	if _, ok := o.byKey[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.byKey[k] = p
}
func (o *orderedPlans[K]) values() []*plan {
	out := make([]*plan, len(o.keys))
	for i, k := range o.keys {
		out[i] = o.byKey[k]
	}
	return out
}

type enumeration struct {
	plans            []*plan
	combinations     int
	count            int
	earliest, latest int
	bestBoarding     *orderedPlans[int]
	directByBoarding *orderedPlans[int]
	byDeparture      *orderedPlans[string]
	mentioned        *plan
	direct           *plan
}

func (n *Network) adjacency() ([][]arc, [][]arc) {
	n.once.Do(func() {
		n.adj = Adjacency(&n.Street)
		n.radj = reverse(n.adj)
	})
	return n.adj, n.radj
}

// Analyze는 출발지에서 목적지까지의 여정을 찾는다.
func (n *Network) Analyze(origin, destination Endpoint, req Request) (*Result, error) {
	t, err := TimeSeconds(req.Time)
	if err != nil {
		return nil, err
	}
	adj, radj := n.adjacency()
	arriveBy := req.ArriveBy
	from, to := t, t+4*3600
	if arriveBy {
		from, to = max(0, t-4*3600), t
	}
	if req.DepartureStart != nil {
		from = *req.DepartureStart
	}
	stops, trips, speed := n.Stops, n.Trips, req.WalkSpeed

	anchor := func(p Point) anchorPoint {
		for _, s := range stops {
			if s.WalkNode >= 0 && Meters(p, Point{s.Lat, s.Lon}) < 3 {
				c := 0.0
				if s.SnapMeters != nil {
					c = *s.SnapMeters
				}
				return anchorPoint{s.WalkNode, 0, c}
			}
		}
		node, d := NearestNode(&n.Street, p, 50)
		return anchorPoint{node, d, 0}
	}
	o, d := anchor(origin.Point), anchor(destination.Point)
	maxWalk := float64(req.MaxWalkMeters)
	originWalk, destinationWalk := WalkDistances(adj, o.node, maxWalk), WalkDistances(radj, d.node, maxWalk)

	accessLimit := maxWalk
	if req.AccessLimitMeters != nil {
		accessLimit = math.Min(maxWalk, float64(*req.AccessLimitMeters))
	}
	var access []reachable
	egress := map[int]float64{}
	for i, s := range stops {
		if a, ok := originWalk.Distance[s.WalkNode]; ok && a+o.meters <= accessLimit {
			access = append(access, reachable{i, math.Ceil(a + o.meters)})
		}
		if b, ok := destinationWalk.Distance[s.WalkNode]; ok && b+d.meters <= maxWalk {
			egress[i] = math.Ceil(b + d.meters)
		}
	}

	entries := make([][]visitRef, len(stops))
	endings := make([][]ending, len(trips))
	for ti := range trips {
		e := trips[ti].Events
		for k := 0; k < len(e); k += 5 {
			if e[k+2] >= from && e[k+2] <= to {
				entries[e[k]] = append(entries[e[k]], visitRef{ti, k})
			}
			if w, ok := egress[e[k]]; ok && e[k+1] <= to {
				endings[ti] = append(endings[ti], ending{k, w})
			}
		}
	}
	for _, list := range entries {
		sort.SliceStable(list, func(a, b int) bool {
			return trips[list[a].trip].Events[list[a].offset+2] < trips[list[b].trip].Events[list[b].offset+2]
		})
	}
	endsAfter := func(trip, offset int) bool {
		for _, e := range endings[trip] {
			if e.offset > offset {
				return true
			}
		}
		return false
	}
	finalEntries := make([][]visitRef, len(stops))
	for i, list := range entries {
		for _, r := range list {
			if endsAfter(r.trip, r.offset) {
				finalEntries[i] = append(finalEntries[i], r)
			}
		}
	}

	dominates := func(a, b *plan) bool {
		if !(a.walk <= b.walk && len(a.rides) <= len(b.rides)) {
			return false
		}
		if arriveBy {
			if !(a.departure >= b.departure && a.duration <= b.duration) {
				return false
			}
			return a.walk < b.walk || len(a.rides) < len(b.rides) || a.departure > b.departure || a.duration < b.duration
		}
		if !(a.arrival <= b.arrival) {
			return false
		}
		return a.walk < b.walk || len(a.rides) < len(b.rides) || a.arrival < b.arrival
	}
	focus := func(route string) bool {
		for _, r := range req.FocusRoutes {
			if route == r || strings.HasPrefix(route, r+"(") {
				return true
			}
		}
		return false
	}
	signature := func(rides []ride) string {
		parts := make([]string, len(rides))
		for i, r := range rides {
			parts[i] = fmt.Sprintf("%s:%d:%d", trips[r.trip].ID, r.from, r.to)
		}
		return strings.Join(parts, "|")
	}

	enumerate := func(budget float64) *enumeration {
		out := &enumeration{earliest: math.MaxInt, latest: math.MinInt, bestBoarding: newOrdered[int](),
			directByBoarding: newOrdered[int](), byDeparture: newOrdered[string]()}
		add := func(rides []ride, accessM, egressM, transferWalk float64) {
			out.combinations++
			first, last := rides[0], rides[len(rides)-1]
			a, b := trips[first.trip].Events, trips[last.trip].Events
			walk := accessM + egressM + transferWalk
			if walk > budget {
				return
			}
			departure := from
			if arriveBy {
				departure = a[first.from+2] - ceilInt(accessM/speed)
			}
			arrival := b[last.to+1] + ceilInt(egressM/speed)
			if departure < from || arrival > to || arrival < departure {
				return
			}
			out.count++
			out.earliest = min(out.earliest, arrival)
			out.latest = max(out.latest, departure)
			p := &plan{rides: rides, access: accessM, egress: egressM, transferWalk: transferWalk, departure: departure,
				arrival: arrival, walk: walk, duration: arrival - departure}
			better := func(old *plan) bool {
				if old == nil {
					return true
				}
				if arriveBy {
					return p.departure > old.departure || p.departure == old.departure && p.duration < old.duration
				}
				return p.arrival < old.arrival || p.arrival == old.arrival && p.walk < old.walk
			}
			boardingStop := a[first.from]
			if req.CollectBoardings {
				key := fmt.Sprintf("%d:%d", first.trip, first.from)
				if better(out.byDeparture.get(key)) {
					out.byDeparture.set(key, p)
				}
			}
			if better(out.bestBoarding.get(boardingStop)) {
				out.bestBoarding.set(boardingStop, p)
			}
			if len(rides) == 1 {
				if better(out.direct) {
					out.direct = p
				}
				if better(out.directByBoarding.get(boardingStop)) {
					out.directByBoarding.set(boardingStop, p)
				}
			}
			if focus(trips[first.trip].Route) && better(out.mentioned) {
				out.mentioned = p
			}
			p.signature = signature(rides)
			for _, f := range out.plans {
				if dominates(f, p) {
					return
				}
			}
			for _, f := range out.plans {
				if f.signature == p.signature {
					return
				}
			}
			kept := out.plans[:0]
			for _, f := range out.plans {
				if !dominates(p, f) {
					kept = append(kept, f)
				}
			}
			out.plans = append(kept, p)
		}
		end := math.Inf(1)
		if req.DepartureEnd != nil {
			end = float64(*req.DepartureEnd)
		}
		for _, origin := range access {
			accessM := origin.meters
			if accessM > budget {
				continue
			}
			ready := from + ceilInt(accessM/speed)
			for _, first := range entries[origin.stop] {
				e := trips[first.trip].Events
				depart := e[first.offset+2]
				if depart < ready || float64(depart) >= end {
					continue
				}
				for _, last := range endings[first.trip] {
					if last.offset > first.offset {
						add([]ride{{first.trip, first.offset, last.offset}}, accessM, last.walk, 0)
					}
				}
				if req.MaxTransfers == 0 {
					continue
				}
				for alight := first.offset + 5; alight < len(e); alight += 5 {
					at := e[alight+1]
					if at >= to {
						break
					}
					for _, link := range n.Walking[e[alight]] {
						if link.Meters+accessM > budget {
							break
						}
						readyTransfer := at + ceilInt(link.Meters/speed) + req.TransferMarginSeconds
						for _, second := range finalEntries[link.Stop] {
							if second.trip == first.trip {
								continue
							}
							s := trips[second.trip].Events
							if s[second.offset+2] < readyTransfer {
								continue
							}
							for _, last := range endings[second.trip] {
								if last.offset > second.offset {
									add([]ride{{first.trip, first.offset, alight}, {second.trip, second.offset, last.offset}}, accessM, last.walk, link.Meters)
								}
							}
						}
					}
				}
			}
		}
		return out
	}

	full := enumerate(maxWalk)
	order := func(a, b *plan) bool {
		if arriveBy {
			if a.departure != b.departure {
				return a.departure > b.departure
			}
			return a.walk < b.walk
		}
		if a.arrival != b.arrival {
			return a.arrival < b.arrival
		}
		return a.walk < b.walk
	}
	sorted := append([]*plan(nil), full.plans...)
	sort.SliceStable(sorted, func(i, j int) bool { return order(sorted[i], sorted[j]) })
	var frontier []*plan
	for _, p := range sorted {
		dominated := false
		for _, f := range frontier {
			if dominates(f, p) {
				dominated = true
				break
			}
		}
		if dominated {
			continue
		}
		kept := frontier[:0]
		for _, f := range frontier {
			if !dominates(p, f) {
				kept = append(kept, f)
			}
		}
		frontier = append(kept, p)
	}
	unique := newOrdered[string]()
	for _, p := range frontier {
		patterns := make([]string, len(p.rides))
		for i, r := range p.rides {
			patterns[i] = trips[r.trip].Pattern
		}
		key := strings.Join(patterns, "|") + ":" + strconv.Itoa(trips[p.rides[0].trip].Events[p.rides[0].from])
		if unique.get(key) == nil {
			unique.set(key, p)
		}
	}
	type pick struct {
		plan *plan
		role string
	}
	var selected []pick
	for i, p := range unique.values() {
		if i >= 7 {
			break
		}
		selected = append(selected, pick{p, "efficient"})
	}
	has := func(p *plan) bool {
		for _, s := range selected {
			if s.plan.signature == p.signature {
				return true
			}
		}
		return false
	}
	if full.direct != nil && !has(full.direct) {
		selected = append(selected, pick{full.direct, "efficient"})
	}
	if full.mentioned != nil && !has(full.mentioned) {
		selected = append(selected, pick{full.mentioned, "mentioned_route"})
	}
	var extra []*plan
	for _, p := range append(full.directByBoarding.values(), full.bestBoarding.values()...) {
		if p.access > 200 {
			extra = append(extra, p)
		}
	}
	sort.SliceStable(extra, func(i, j int) bool {
		a, b := extra[i], extra[j]
		if len(a.rides) != len(b.rides) {
			return len(a.rides) < len(b.rides)
		}
		return order(a, b)
	})
	for _, p := range extra {
		if len(selected) >= 12 {
			break
		}
		if !has(p) {
			selected = append(selected, pick{p, "walking_alternative"})
		}
	}

	streetPath := func(a, b int, limit float64) []Point {
		if a == b {
			return []Point{}
		}
		found := WalkDistances(adj, a, limit+2)
		return WalkGeometry(&n.Street, found.Previous, a, b)
	}
	snap := func(s Stop) float64 {
		if s.SnapMeters == nil {
			return 0
		}
		return *s.SnapMeters
	}
	toJourney := func(p *plan, role, id string) Journey {
		var legs []Leg
		cursor := p.departure
		walking := func(fromName, toName string, distance float64, geometry []Point, connector float64) {
			if distance <= 0 {
				return
			}
			duration := ceilInt(distance / speed)
			legs = append(legs, Leg{Kind: "walk", From: fromName, To: toName, Start: cursor, End: cursor + duration, Meters: distance,
				Geometry: geometry, Source: n.Street.URL, ConnectorMeters: connector})
			cursor += duration
		}
		first := p.rides[0]
		firstStop := stops[trips[first.trip].Events[first.from]]
		connector := 0.0
		if o.node != firstStop.WalkNode {
			connector = snap(firstStop) + o.meters + o.connector
		}
		walking(origin.Label, firstStop.Name, p.access,
			append([]Point{origin.Point}, WalkGeometry(&n.Street, originWalk.Previous, o.node, firstStop.WalkNode)...), connector)
		for i, r := range p.rides {
			trip := trips[r.trip]
			e := trip.Events
			a, b := stops[e[r.from]], stops[e[r.to]]
			if i > 0 {
				prev := p.rides[i-1]
				lastStop := stops[trips[prev.trip].Events[prev.to]]
				walking(lastStop.Name, a.Name, p.transferWalk, streetPath(lastStop.WalkNode, a.WalkNode, p.transferWalk), snap(lastStop)+snap(a))
			}
			var geometry []Point
			var rows []int
			for j := r.from; j <= r.to; j += 5 {
				s := stops[e[j]]
				geometry = append(geometry, Point{s.Lat, s.Lon})
				rows = append(rows, e[j+4])
			}
			legs = append(legs, Leg{Kind: "ride", Route: trip.Route, TripID: trip.ID, From: a.Name, To: b.Name, FromID: a.ID, ToID: b.ID,
				Start: e[r.from+2], End: e[r.to+1], FromSequence: e[r.from+3], ToSequence: e[r.to+3], Geometry: geometry,
				SourceRows: rows, SourceFile: n.SourceFile, Basis: n.Basis})
			cursor = e[r.to+1]
		}
		last := p.rides[len(p.rides)-1]
		lastStop := stops[trips[last.trip].Events[last.to]]
		back := WalkGeometry(&n.Street, destinationWalk.Previous, d.node, lastStop.WalkNode)
		for i, j := 0, len(back)-1; i < j; i, j = i+1, j-1 {
			back[i], back[j] = back[j], back[i]
		}
		connector = 0
		if d.node != lastStop.WalkNode {
			connector = snap(lastStop) + d.meters + d.connector
		}
		walking(lastStop.Name, destination.Label, p.egress, append(back, destination.Point), connector)
		walkingSeconds, ridingSeconds := 0, 0
		for _, l := range legs {
			if l.Kind == "walk" {
				walkingSeconds += l.End - l.Start
			} else {
				ridingSeconds += l.End - l.Start
			}
		}
		var margin *int
		if arriveBy {
			m := t - p.arrival
			margin = &m
		}
		return Journey{ID: id, Role: role, Departure: p.departure, Arrival: p.arrival, DurationSeconds: p.duration, WalkMeters: p.walk,
			WalkingSeconds: walkingSeconds, RidingSeconds: ridingSeconds, WaitingSeconds: p.duration - walkingSeconds - ridingSeconds,
			Transfers: len(p.rides) - 1, ArrivalMarginSeconds: margin, Legs: legs, Signature: p.signature}
	}

	result := &Result{From: from, To: to, Journeys: []Journey{}}
	for i, s := range selected {
		result.Journeys = append(result.Journeys, toJourney(s.plan, s.role, "journey-"+strconv.Itoa(i)))
	}
	if req.CollectBoardings {
		result.BoardingCount = len(full.byDeparture.keys)
	}
	switch {
	case len(result.Journeys) > 0:
		result.Status = "ok"
	case len(access) == 0 || len(egress) == 0:
		result.Status = "coverage_gap"
	default:
		result.Status = "no_journey"
	}
	return result, nil
}
