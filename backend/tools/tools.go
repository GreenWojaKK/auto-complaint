// Package tools는 교통 조회 도구다. PostgreSQL의 transit 스키마를 읽기 전용 계정으로 읽는다.
//
// 조회는 /search(장소 검색), /place(장소 정보), /catalogue, /query(journeys·map·segments), /sql(읽기 전용 SELECT)이다.
// 모든 결과는 입력과 결과, 자료 판·버전을 묶은 evidence ID를 가진다.
package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"dllio/backend/journey"
	"dllio/backend/places"
	"dllio/backend/region"
	"dllio/backend/revisions"
	"dllio/backend/segments"
	"dllio/backend/sqltool"
	"dllio/backend/store"
)

// Options는 Load의 입력이다. DSN이 비면 지역의 database에 조회 계정으로 접속한다. 행정동 경계는 지역 폴더의 boundaries.geojson이다.
type Options struct {
	Region            *region.Region
	DSN, PasswordFile string
}

// Tools는 적재된 교통 자료와 그 위의 조회다.
type Tools struct {
	region    *region.Region
	db        *pgxpool.Pool
	version   string
	order     []string
	nets      map[string]*journey.Network
	segments  map[string]*segments.Table
	routeSets map[string]map[string]bool
	catalogue *places.Catalogue
	revisions []revisions.Revision
	changes   []revisions.Change
	sheets    []revisions.Sheet
}

// ErrUnknown은 없는 조회 이름이다.
var ErrUnknown = errors.New("unknown tool")

// Load는 DB에서 자료 판, 연결망, 장소 목록을 읽는다.
func Load(ctx context.Context, o Options) (*Tools, error) {
	dsn := o.DSN
	if dsn == "" {
		dsn = o.Region.DSN()
	}
	db, err := store.Open(ctx, dsn, o.PasswordFile)
	if err != nil {
		return nil, err
	}
	s := &Tools{region: o.Region, db: db, nets: map[string]*journey.Network{}, segments: map[string]*segments.Table{}, routeSets: map[string]map[string]bool{}}
	if s.version, err = store.Version(ctx, db); err != nil {
		return nil, err
	}
	if s.revisions, s.changes, err = store.Revisions(ctx, db); err != nil {
		return nil, err
	}
	if s.sheets, err = store.Sheets(ctx, db); err != nil {
		return nil, err
	}
	for _, r := range s.revisions {
		n, err := store.Network(ctx, db, r.ID)
		if err != nil {
			return nil, err
		}
		s.order = append(s.order, r.ID)
		s.nets[r.ID], s.segments[r.ID], s.routeSets[r.ID] = n, segments.NewTable(n), map[string]bool{}
		for _, t := range n.Trips {
			s.routeSets[r.ID][t.RouteNumber] = true
		}
	}
	list, err := store.Places(ctx, db)
	if err != nil {
		return nil, err
	}
	areas, err := places.LoadAreas(o.Region.Path("boundaries.geojson"))
	if err != nil {
		return nil, err
	}
	s.catalogue = places.NewCatalogue(list, areas)
	return s, nil
}

// Close는 DB 연결을 닫는다.
func (s *Tools) Close() { s.db.Close() }

// Version은 적재된 자료의 버전이다.
func (s *Tools) Version() string { return s.version }

// Health는 자료 버전과 판마다의 연결망 버전이다.
func (s *Tools) Health() map[string]any {
	versions := map[string]string{}
	for id, n := range s.nets {
		versions[id] = n.Version
	}
	return map[string]any{"ok": true, "network": s.Network(), "versions": versions}
}

// Network는 결과 기록에 남기는 자료 이름과 버전이다.
func (s *Tools) Network() string { return "postgresql:" + s.region.Database + " " + s.version }

// Region은 이 자료의 지역이다.
func (s *Tools) Region() *region.Region { return s.region }

// Body는 조회 입력이다. 입력을 그대로 결과에 되돌려주기 위해 지도로 읽고, 필요한 값만 꺼낸다.
type Body map[string]any

func (b Body) str(k, d string) string {
	if v, ok := b[k].(string); ok {
		return v
	}
	return d
}
func (b Body) num(k string, d float64) float64 {
	if v, ok := b[k].(float64); ok {
		return v
	}
	return d
}
func (b Body) flag(k string) bool { v, _ := b[k].(bool); return v }
func (b Body) list(k string) []string {
	out := []string{}
	if v, ok := b[k].([]any); ok {
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

var clock = regexp.MustCompile(`^\d{2}:\d{2}$`)

func seconds(s string) (int, error) {
	if !clock.MatchString(s) {
		return 0, fmt.Errorf("HH:MM 시각이 필요합니다.")
	}
	h, _ := strconv.Atoi(s[:2])
	m, _ := strconv.Atoi(s[3:])
	return h*3600 + m*60, nil
}

// Evidence는 조회 결과다.
type Evidence struct {
	ID         string         `json:"id"`
	Input      Body           `json:"input"`
	Data       any            `json:"data"`
	Provenance map[string]any `json:"provenance"`
}

// Call은 조회 하나를 실행한다. 입력은 JSON으로 한 번 옮겨 HTTP로 받은 입력과 같은 값으로 읽는다.
func (s *Tools) Call(ctx context.Context, endpoint string, input any) (*Evidence, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	b := Body{}
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, err
	}
	return s.Run(ctx, endpoint, b)
}

// Run은 이미 읽은 입력으로 조회 하나를 실행한다.
func (s *Tools) Run(ctx context.Context, endpoint string, b Body) (*Evidence, error) {
	var choice *revisions.Choice
	if endpoint == "/query" {
		var rev revisions.Revision
		named := false
		for _, v := range s.revisions {
			if v.ID == b.str("dataset", "") {
				rev, named = v, true
			}
		}
		switch {
		case named:
		case b.str("date", "") != "":
			rev = revisions.At(s.revisions, b.str("date", ""))
		default:
			rev = s.revisions[len(s.revisions)-1]
		}
		c := rev.Choice()
		choice = &c
		b["dataset"] = rev.ID
	}
	var data any
	var err error
	switch endpoint {
	case "/search":
		data = map[string]any{"places": s.catalogue.Search(b.str("query", ""), b.list("routes"), int(b.num("limit", 12)))}
	case "/place":
		data = map[string]any{"place": s.catalogue.Info(b.str("id", ""), b.list("routes"), 600, 8)}
	case "/catalogue":
		data = s.catalogueData()
	case "/query":
		data, err = s.query(b)
	case "/sql":
		data, err = sqltool.Run(ctx, s.db, b.str("query", ""), 2000)
	default:
		return nil, ErrUnknown
	}
	if err != nil {
		return nil, err
	}
	provenance := map[string]any{"networkVersion": s.version}
	if endpoint == "/sql" {
		provenance["database"] = "dllio/transit"
	} else {
		n := s.nets[b.str("dataset", s.order[len(s.order)-1])]
		if n != nil {
			provenance["networkVersion"], provenance["sourceHash"] = n.Version, n.SourceHash
		}
	}
	// 질의한 노선 번호의 이전 번호 대응은 모든 결과에, 판 이후 변경은 판을 고른 조회에 붙인다.
	dataset := b.str("dataset", s.order[len(s.order)-1])
	queried := []string{}
	add := func(r string) {
		if r = places.RouteBase(r); !slices.Contains(queried, r) {
			queried = append(queried, r)
		}
	}
	for _, r := range b.list("routes") {
		add(r)
	}
	if m, ok := data.(map[string]any); ok {
		if rows, ok := m["rows"].([]map[string]any); ok {
			for _, row := range rows {
				if v, ok := row["route"].(string); ok && v != "" {
					add(v)
				}
			}
		}
	}
	var lineage []revisions.Lineage
	changed := append([]string{}, queried...)
	for _, q := range queried {
		l := revisions.LineageOf(s.sheets, q)
		if len(l.Previous) == 0 && len(l.Later) == 0 && !l.Created {
			continue
		}
		for _, x := range append(append([]string{}, l.Previous...), l.Later...) {
			if s.routeSets[dataset][x] {
				l.InDataset = append(l.InDataset, x)
			}
		}
		for _, x := range l.Later {
			if !slices.Contains(changed, x) {
				changed = append(changed, x)
			}
		}
		lineage = append(lineage, l)
	}
	if choice != nil {
		provenance["revision"] = choice
		provenance["laterChanges"] = revisions.ChangesAfter(s.revisions, s.changes, choice.ID, changed)
	}
	if len(lineage) > 0 {
		provenance["lineage"] = lineage
	}
	canonical, err := json.Marshal(map[string]any{"endpoint": endpoint, "input": b, "data": data, "provenance": provenance})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(canonical)
	return &Evidence{"e_" + hex.EncodeToString(sum[:])[:16], b, data, provenance}, nil
}

// Plain은 결과를 JSON 값(지도·목록·json.Number)으로 옮긴다. 숫자는 결과에 적힌 글자 그대로 둔다.
func Plain(value any) (map[string]any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var out map[string]any
	return out, d.Decode(&out)
}

func (s *Tools) catalogueData() map[string]any {
	var datasets []map[string]any
	for _, id := range s.order {
		n := s.nets[id]
		routes := []string{}
		for r := range s.routeSets[id] {
			routes = append(routes, r)
		}
		sort.Strings(routes)
		var date any
		if n.Date != "" {
			date = n.Date
		}
		datasets = append(datasets, map[string]any{"id": id, "version": n.Version, "basis": n.Basis, "date": date,
			"holiday": id == "after", "coverageNotes": n.Notes, "routes": routes})
	}
	lineage := []revisions.Sheet{}
	for _, sh := range s.sheets {
		if len(sh.Previous) > 0 || sh.Created {
			lineage = append(lineage, sh)
		}
	}
	return map[string]any{"datasets": datasets, "revisions": s.revisions, "changes": s.changes, "lineage": lineage}
}

// stopSet은 stop_ids와 장소의 그 판 소속 정류장이다.
func (s *Tools) stopSet(n *journey.Network, ids, placeIDs []string) map[string]bool {
	out := map[string]bool{}
	for _, id := range ids {
		out[id] = true
	}
	for _, id := range placeIDs {
		if p := s.catalogue.Find(id); p != nil {
			for _, m := range p.Members[n.Snapshot] {
				out[m.ID] = true
			}
		}
	}
	return out
}

func (s *Tools) query(b Body) (any, error) {
	n := s.nets[b.str("dataset", "")]
	if n == nil {
		return nil, fmt.Errorf("Unknown dataset")
	}
	routes := []string{}
	for _, r := range b.list("routes") {
		if base := places.RouteBase(r); !slices.Contains(routes, base) {
			routes = append(routes, base)
		}
	}
	start, err := seconds(b.str("start", "00:00"))
	if err != nil {
		return nil, err
	}
	end, err := seconds(b.str("end", "24:00"))
	if err != nil {
		return nil, err
	}
	switch b.str("operation", "") {
	case "journeys":
		return s.journeys(n, b, start, end)
	case "map":
		return s.routeMap(n, b, routes)
	case "segments":
		return s.segmentEstimate(n, b, routes, start, end)
	}
	return nil, fmt.Errorf("Unknown operation")
}

func jsRound1(x float64) float64 { return math.Floor(x*10+0.5) / 10 }

func (s *Tools) journeys(n *journey.Network, b Body, start, end int) (any, error) {
	point := func(id string) (journey.Endpoint, error) {
		p := s.catalogue.Find(id)
		if p == nil {
			return journey.Endpoint{}, fmt.Errorf("Unknown place %s", id)
		}
		pt := p.Point
		if m := p.Members[n.Snapshot]; len(m) > 0 {
			pt = m[0].Point
		}
		return journey.Endpoint{Label: p.Name, Point: pt, Source: id}, nil
	}
	origin, err := point(b.str("origin", ""))
	if err != nil {
		return nil, err
	}
	destination, err := point(b.str("destination", ""))
	if err != nil {
		return nil, err
	}
	t := b.str("time", b.str("start", "08:00"))
	departureStart := start
	if !b.flag("arrive_by") {
		at, err := seconds(t)
		if err != nil {
			return nil, err
		}
		departureStart = max(start, at)
	}
	res, err := n.Analyze(origin, destination, journey.Request{Time: t, ArriveBy: b.flag("arrive_by"),
		MaxWalkMeters: int(b.num("walk_meters", 600)), MaxTransfers: int(b.num("transfers", 1)), WalkSpeed: 1.2,
		TransferMarginSeconds: 90, FocusRoutes: b.list("routes"), DepartureStart: &departureStart, DepartureEnd: &end, CollectBoardings: true})
	if err != nil {
		return nil, err
	}
	rows := []map[string]any{}
	for _, j := range res.Journeys {
		var boarding any
		var rideRoutes []string
		events, lines, refs := []map[string]any{}, []map[string]any{}, []string{}
		for _, l := range j.Legs {
			label := "도보"
			if l.Kind == "ride" {
				label = l.Route + "번"
				rideRoutes = append(rideRoutes, l.Route)
				if boarding == nil {
					boarding = l.From
				}
				for _, row := range l.SourceRows {
					refs = append(refs, l.SourceFile+"#row="+strconv.Itoa(row))
				}
			} else {
				refs = append(refs, l.Source)
			}
			lineLabel := "도보"
			if l.Kind == "ride" {
				lineLabel = l.Route
			}
			events = append(events, map[string]any{"start": journey.LabelTime(l.Start), "end": journey.LabelTime(l.End), "label": label, "detail": l.From + " → " + l.To})
			lines = append(lines, map[string]any{"label": lineLabel, "mode": l.Kind, "points": l.Geometry})
		}
		rows = append(rows, map[string]any{"departure": journey.LabelTime(j.Departure), "arrival": journey.LabelTime(j.Arrival),
			"minutes": jsRound1(float64(j.DurationSeconds) / 60), "walkMeters": j.WalkMeters, "waitMinutes": jsRound1(float64(j.WaitingSeconds) / 60),
			"transfers": j.Transfers, "boarding": boarding, "routes": strings.Join(rideRoutes, " → "), "events": events,
			"map": map[string]any{"lines": lines, "points": []map[string]any{
				{"label": origin.Label, "name": origin.Label, "point": origin.Point},
				{"label": destination.Label, "name": destination.Label, "point": destination.Point}}},
			"sourceRefs": refs})
	}
	return map[string]any{"status": res.Status, "rows": rows, "boardingCount": res.BoardingCount}, nil
}

var colors = []string{"#126af4", "#10a171", "#e8590c", "#9c36b5"}

// routeMap은 노선마다 운행 기록이 실제로 지난 정류장 순서를 선으로, 원문이 말한 정류장을 점으로 돌려준다.
func (s *Tools) routeMap(n *journey.Network, b Body, routes []string) (any, error) {
	if len(routes) == 0 {
		return nil, fmt.Errorf("경로 조회에는 노선 번호를 지정한다. 전체 목록은 catalogue를 사용한다.")
	}
	wanted := map[string]bool{}
	for _, r := range routes {
		wanted[r] = true
	}
	var chosen []string
	groups := map[string][]*journey.Trip{}
	for i := range n.Trips {
		t := &n.Trips[i]
		if !wanted[t.RouteNumber] {
			continue
		}
		if _, ok := groups[t.RouteNumber]; !ok {
			chosen = append(chosen, t.RouteNumber)
		}
		groups[t.RouteNumber] = append(groups[t.RouteNumber], t)
	}
	mentions := b.list("mentions")
	mentioned := func(stopName string) bool {
		name := places.Normalize(stopName)
		for _, m := range mentions {
			if m == name || len([]rune(name)) >= 3 && strings.Contains(m, name) {
				return true
			}
		}
		return false
	}
	lines, points, legend, rows := []map[string]any{}, []map[string]any{}, []map[string]any{}, []map[string]any{}
	marked := map[string]bool{}
	for i, route := range chosen {
		color := colors[i%len(colors)]
		var stopOrder []int
		stops := map[int]bool{}
		paths := map[string]bool{}
		patterns := map[string]bool{}
		vehicles := map[string]bool{}
		visits := 0
		for _, t := range groups[route] {
			var order []int
			var key strings.Builder
			for k := 0; k < len(t.Events); k += 5 {
				stop := t.Events[k]
				if !stops[stop] {
					stops[stop] = true
					stopOrder = append(stopOrder, stop)
				}
				order = append(order, stop)
				fmt.Fprintf(&key, "%d,", stop)
				visits++
			}
			patterns[t.Pattern] = true
			if t.Vehicle != "" {
				vehicles[t.Vehicle] = true
			}
			if len(order) >= 2 && !paths[key.String()] {
				paths[key.String()] = true
				pts := make([]journey.Point, len(order))
				for j, stop := range order {
					pts[j] = journey.Point{n.Stops[stop].Lat, n.Stops[stop].Lon}
				}
				lines = append(lines, map[string]any{"label": route, "mode": "ride", "color": color, "points": pts})
			}
		}
		for _, stop := range stopOrder {
			name := cleanName(n.Stops[stop].Name)
			if !mentioned(name) || marked[name] {
				continue
			}
			marked[name] = true
			points = append(points, map[string]any{"label": name, "name": name, "point": journey.Point{n.Stops[stop].Lat, n.Stops[stop].Lon}, "kind": "mention"})
		}
		sourcePatterns := []string{}
		for p := range patterns {
			sourcePatterns = append(sourcePatterns, p)
		}
		sort.Strings(sourcePatterns)
		legend = append(legend, map[string]any{"label": route, "color": color})
		rows = append(rows, map[string]any{"route": route, "trips": len(groups[route]), "stops": len(stops), "visits": visits,
			"vehicleCount": len(vehicles), "sourcePatterns": sourcePatterns})
	}
	return map[string]any{"measurement": "route_map", "scope": map[string]any{"dataset": n.Snapshot, "routes": chosen}, "rows": rows,
		"map": map[string]any{"lines": lines, "points": points, "legend": legend}}, nil
}

var (
	parens   = regexp.MustCompile(`\([^)]*\)`)
	terminal = regexp.MustCompile(`^(.{2,})(?:기점|종점)$`)
)

func cleanName(name string) string {
	return terminal.ReplaceAllString(strings.TrimSpace(parens.ReplaceAllString(name, "")), "$1")
}

func (s *Tools) segmentEstimate(n *journey.Network, b Body, routes []string, start, end int) (any, error) {
	index := map[string]int{}
	for i, st := range n.Stops {
		index[st.ID] = i
	}
	var path []int
	var recorded map[string]any
	var actual any
	if ids := b.list("path_stop_ids"); len(ids) > 0 {
		for _, id := range ids {
			i, ok := index[id]
			if !ok {
				return nil, fmt.Errorf("Unknown stop %s", id)
			}
			path = append(path, i)
		}
	} else {
		origins := s.stopSet(n, b.list("stop_ids"), b.list("place_ids"))
		destinations := s.stopSet(n, b.list("destination_stop_ids"), b.list("destination_place_ids"))
		if len(origins) == 0 || len(destinations) == 0 {
			return nil, fmt.Errorf("segments에는 path_stop_ids, 또는 stop_ids/place_ids와 destination_stop_ids/destination_place_ids가 필요하다.")
		}
		found := segments.RecordedPaths(n, routes, origins, destinations, start, end)
		if len(found) == 0 {
			return nil, fmt.Errorf("지정한 노선의 운행 기록 중 출발 정류장 뒤에 도착 정류장을 지나는 운행이 없다.")
		}
		path = found[0].Stops
		recorded = map[string]any{"routes": found[0].Routes, "tripsInWindow": found[0].Trips, "otherStopOrders": len(found) - 1}
		if sum := segments.DirectSummary(found[0].Direct); sum != nil {
			actual = sum
		}
	}
	steps, total := s.segments[n.Snapshot].Estimate(n, path, start, end)
	pathStops := make([]map[string]string, len(path))
	for i, p := range path {
		pathStops[i] = map[string]string{"id": n.Stops[p].ID, "name": n.Stops[p].Name}
	}
	pathView := map[string]any{"stops": pathStops}
	for k, v := range recorded {
		pathView[k] = v
	}
	return map[string]any{"measurement": "segment_estimate",
		"scope": map[string]any{"dataset": n.Snapshot, "basis": n.Basis, "start": b.str("start", "00:00"), "end": b.str("end", "24:00"), "routes": routes},
		"path":  pathView, "rows": steps, "totalMinutes": total, "recordedTrips": actual}, nil
}
