// Package store는 PostgreSQL의 transit 스키마(backend/db/schema.sql)를 읽는다.
// 도구 서버는 읽기 전용 계정(dllio_read)으로만 접속하므로 자료를 바꿀 수 없다.
package store

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"dllio/backend/journey"
	"dllio/backend/places"
	"dllio/backend/revisions"
)

// Open은 접속 풀을 연다. 비밀번호는 파일에서 읽어 인증에만 쓴다.
func Open(ctx context.Context, dsn, passwordFile string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	if passwordFile != "" {
		raw, err := os.ReadFile(passwordFile)
		if err != nil {
			return nil, fmt.Errorf("비밀번호 파일을 읽지 못했다: %w", err)
		}
		cfg.ConnConfig.Password = strings.TrimSpace(string(raw))
	}
	return pgxpool.NewWithConfig(ctx, cfg)
}

// Version은 마지막 적재의 network version이다.
func Version(ctx context.Context, db *pgxpool.Pool) (string, error) {
	var v string
	err := db.QueryRow(ctx, `SELECT version FROM transit.load ORDER BY loaded_at DESC LIMIT 1`).Scan(&v)
	return v, err
}

// Revisions는 판을 시간 순서로, 알려진 노선 변경을 적은 순서로 읽는다.
func Revisions(ctx context.Context, db *pgxpool.Pool) ([]revisions.Revision, []revisions.Change, error) {
	rows, err := db.Query(ctx, `
		SELECT id, label, effective_from::text, effective_to::text, observed::text
		FROM transit.revision ORDER BY ord`)
	if err != nil {
		return nil, nil, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (revisions.Revision, error) {
		var v revisions.Revision
		err := r.Scan(&v.ID, &v.Label, &v.EffectiveFrom, &v.EffectiveTo, &v.Observed)
		return v, err
	})
	if err != nil {
		return nil, nil, err
	}
	rows, err = db.Query(ctx, `SELECT route, change, date::text FROM transit.route_change ORDER BY ord`)
	if err != nil {
		return nil, nil, err
	}
	changes, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (revisions.Change, error) {
		var c revisions.Change
		err := r.Scan(&c.Route, &c.Change, &c.Date)
		return c, err
	})
	return list, changes, err
}

// Sheets는 노선 번호와 이전 번호다.
func Sheets(ctx context.Context, db *pgxpool.Pool) ([]revisions.Sheet, error) {
	rows, err := db.Query(ctx, `SELECT route, previous, created, note, sheet FROM transit.route_lineage ORDER BY ord`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (revisions.Sheet, error) {
		var s revisions.Sheet
		err := r.Scan(&s.Route, &s.Previous, &s.Created, &s.Note, &s.Sheet)
		return s, err
	})
}

// Network는 판 하나의 정류장·운행·보행망을 메모리로 읽는다. 배열 순서는 적재한 순서(idx)를 따른다.
func Network(ctx context.Context, db *pgxpool.Pool, rev string) (*journey.Network, error) {
	n := &journey.Network{Snapshot: rev}
	var date *string
	err := db.QueryRow(ctx, `
		SELECT r.label, r.basis, r.data_date::text, r.source_file, r.source_hash, r.network_version, r.max_walk_meters, r.notes,
		       g.hash, g.url, coalesce(to_char(g.fetched_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'), '')
		FROM transit.revision r JOIN transit.street_graph g ON g.id = r.street_graph
		WHERE r.id = $1`, rev).
		Scan(&n.Label, &n.Basis, &date, &n.SourceFile, &n.SourceHash, &n.Version, &n.MaxWalkMeters, &n.Notes,
			&n.Street.Hash, &n.Street.URL, &n.Street.FetchedAt)
	if err != nil {
		return nil, fmt.Errorf("판 %s: %w", rev, err)
	}
	if date != nil {
		n.Date = *date
	}

	rows, err := db.Query(ctx, `
		SELECT stop_id, name, lat, lon, network_place, walk_node, snap_meters
		FROM transit.stop WHERE revision = $1 ORDER BY idx`, rev)
	if err != nil {
		return nil, err
	}
	n.Stops, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (journey.Stop, error) {
		var s journey.Stop
		err := r.Scan(&s.ID, &s.Name, &s.Lat, &s.Lon, &s.Place, &s.WalkNode, &s.SnapMeters)
		return s, err
	})
	if err != nil {
		return nil, err
	}
	index := make(map[string]int, len(n.Stops))
	for i, s := range n.Stops {
		index[s.ID] = i
	}

	rows, err = db.Query(ctx, `
		SELECT name, degree, dominance_radius FROM transit.network_place WHERE revision = $1 ORDER BY idx`, rev)
	if err != nil {
		return nil, err
	}
	n.Places, err = pgx.CollectRows(rows, pgx.RowToStructByPos[journey.Place])
	if err != nil {
		return nil, err
	}

	rows, err = db.Query(ctx, `
		SELECT trip_id, route, route_number, pattern, coalesce(vehicle, '') FROM transit.trip WHERE revision = $1 ORDER BY idx`, rev)
	if err != nil {
		return nil, err
	}
	n.Trips, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (journey.Trip, error) {
		var t journey.Trip
		err := r.Scan(&t.ID, &t.Route, &t.RouteNumber, &t.Pattern, &t.Vehicle)
		return t, err
	})
	if err != nil {
		return nil, err
	}
	tripIndex := make(map[string]int, len(n.Trips))
	for i, t := range n.Trips {
		tripIndex[t.ID] = i
	}
	rows, err = db.Query(ctx, `
		SELECT trip_id, stop_id, coalesce(arrival, -1), coalesce(departure, -1), stop_sequence, source_row
		FROM transit.visit WHERE revision = $1 ORDER BY trip_id, seq`, rev)
	if err != nil {
		return nil, err
	}
	var tripID, stopID string
	var arrival, departure, sequence, sourceRow int
	_, err = pgx.ForEachRow(rows, []any{&tripID, &stopID, &arrival, &departure, &sequence, &sourceRow}, func() error {
		t := &n.Trips[tripIndex[tripID]]
		t.Events = append(t.Events, index[stopID], arrival, departure, sequence, sourceRow)
		return nil
	})
	if err != nil {
		return nil, err
	}

	n.Walking = make([][]journey.Link, len(n.Stops))
	rows, err = db.Query(ctx, `SELECT from_idx, to_idx, meters FROM transit.walking_link WHERE revision = $1 ORDER BY from_idx, ord`, rev)
	if err != nil {
		return nil, err
	}
	var from, to int
	var meters float64
	_, err = pgx.ForEachRow(rows, []any{&from, &to, &meters}, func() error {
		n.Walking[from] = append(n.Walking[from], journey.Link{Stop: to, Meters: meters})
		return nil
	})
	if err != nil {
		return nil, err
	}

	rows, err = db.Query(ctx, `SELECT lat, lon, osm_id FROM transit.street_node WHERE graph = $1 ORDER BY id`, rev)
	if err != nil {
		return nil, err
	}
	n.Street.Nodes, err = pgx.CollectRows(rows, pgx.RowToStructByPos[journey.Node])
	if err != nil {
		return nil, err
	}
	rows, err = db.Query(ctx, `SELECT from_node, to_node, meters FROM transit.street_edge WHERE graph = $1 ORDER BY ord`, rev)
	if err != nil {
		return nil, err
	}
	n.Street.Edges, err = pgx.CollectRows(rows, pgx.RowToStructByPos[journey.Edge])
	return n, err
}

// Places는 장소와 판별 소속 정류장·노선을 장소 목록 순서대로 읽는다.
func Places(ctx context.Context, db *pgxpool.Pool) ([]*places.Place, error) {
	rows, err := db.Query(ctx, `SELECT place_id, name, aliases, lat, lon FROM transit.place ORDER BY ord`)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*places.Place, error) {
		p := &places.Place{Members: map[string][]places.Member{}, Routes: map[string][]string{}}
		err := r.Scan(&p.ID, &p.Name, &p.Aliases, &p.Point[0], &p.Point[1])
		return p, err
	})
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*places.Place, len(list))
	for _, p := range list {
		byID[p.ID] = p
	}
	rows, err = db.Query(ctx, `
		SELECT m.place_id, m.revision, m.stop_id, m.name, m.lat, m.lon
		FROM transit.place_stop m JOIN transit.revision r ON r.id = m.revision
		ORDER BY m.place_id, r.ord, m.ord`)
	if err != nil {
		return nil, err
	}
	var placeID, rev string
	var m places.Member
	_, err = pgx.ForEachRow(rows, []any{&placeID, &rev, &m.ID, &m.Name, &m.Point[0], &m.Point[1]}, func() error {
		p := byID[placeID]
		if len(p.Members[rev]) == 0 {
			p.Datasets = append(p.Datasets, rev)
		}
		p.Members[rev] = append(p.Members[rev], m)
		return nil
	})
	if err != nil {
		return nil, err
	}
	rows, err = db.Query(ctx, `SELECT place_id, revision, route FROM transit.place_route ORDER BY place_id, revision, ord`)
	if err != nil {
		return nil, err
	}
	var route string
	_, err = pgx.ForEachRow(rows, []any{&placeID, &rev, &route}, func() error {
		byID[placeID].Routes[rev] = append(byID[placeID].Routes[rev], route)
		return nil
	})
	return list, err
}
