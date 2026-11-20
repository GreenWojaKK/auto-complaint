"""Loads the prepared transit data into PostgreSQL (backend/db/schema.sql).

  uv run --project preprocess python -m dllio_preprocess.network.load [--dsn postgresql://postgres@localhost:5432/dllio]

The schema is recreated, then the journey networks, the place catalogue, the route numbers' earlier numbers and the
revisions with their known route changes are copied in as they are. Nothing is judged here: routes stay as recorded
with their route number beside them, each run carries how its stops line up with the longest run of its number
(round_trip), and missing times stay NULL. The password comes from the PostgreSQL password
file (pgpass), never from the command line.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
from pathlib import Path

import psycopg

from .. import ROOT, region_dir
from . import round_trip

DSN = os.environ.get('DLLIO_DATABASE_URL', 'postgresql://postgres@localhost:5432/dllio')


def route_number(route: str) -> str:
    """'857(덕하차고지 방면)' → '857': the route name without its last bracket."""
    return re.sub(r'\s*\([^()]*\)\s*$', '', route).strip()


def in_ring(ring: list, lon: float, lat: float) -> bool:
    inside, j = False, len(ring) - 1
    for i in range(len(ring)):
        (xi, yi), (xj, yj) = ring[i][:2], ring[j][:2]
        if (yi > lat) != (yj > lat) and lon < (xj - xi) * (lat - yi) / (yj - yi) + xi:
            inside = not inside
        j = i
    return inside


def area_reader(path: Path):
    """행정동 GeoJSON에서 지점이 놓인 '구 동'을 찾는 함수. 고리는 [경도, 위도]이고 구멍은 뺀다."""
    areas = []
    for f in json.loads(path.read_text(encoding='utf-8'))['features']:
        g = f['geometry']
        polygons = [g['coordinates']] if g['type'] == 'Polygon' else g['coordinates']
        areas.append((f['properties']['sggnm'] + ' ' + f['properties']['dongnm'], polygons))

    def area_of(lat: float, lon: float) -> str | None:
        for name, polygons in areas:
            for polygon in polygons:
                if in_ring(polygon[0], lon, lat) and not any(in_ring(hole, lon, lat) for hole in polygon[1:]):
                    return name
        return None
    return area_of


def sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def time_or_null(value: int) -> int | None:
    return None if value < 0 else value


def copy(cur, table: str, columns: list[str], rows) -> int:
    n = 0
    with cur.copy(f'COPY transit.{table} ({", ".join(columns)}) FROM STDIN') as out:
        for row in rows:
            out.write_row(row)
            n += 1
    return n


def load(dsn: str, journey: Path, catalogue: Path, lineage: Path, revisions: Path, schema: Path, boundaries: Path) -> dict:
    manifest = json.loads((journey / 'manifest.json').read_text(encoding='utf-8'))
    order = json.loads(revisions.read_text(encoding='utf-8'))
    nets = {r['id']: json.loads((journey / (r['id'] + '.json')).read_text(encoding='utf-8')) for r in order['revisions']}
    places = json.loads(catalogue.read_text(encoding='utf-8'))['places']
    sheets = json.loads(lineage.read_text(encoding='utf-8'))['lineage']
    area_of = area_reader(boundaries)
    counts = {}
    with psycopg.connect(dsn, autocommit=False) as conn, conn.cursor() as cur:
        cur.execute(schema.read_text(encoding='utf-8'))
        # Each network carries its own street graph: the stop connectors differ even when the OSM source is the same.
        for rev, net in nets.items():
            street, src = net['street'], net['street']['source']
            cur.execute('INSERT INTO transit.street_graph VALUES (%s, %s, %s, %s, %s)', (rev, street['hash'], src['url'], src.get('fetchedAt'), src.get('license')))
            counts[rev + ':street_node'] = copy(cur, 'street_node', ['graph', 'id', 'lat', 'lon', 'osm_id'],
                                                ((rev, i, n[0], n[1], n[2]) for i, n in enumerate(street['nodes'])))
            e = street['edges']
            counts[rev + ':street_edge'] = copy(cur, 'street_edge', ['graph', 'ord', 'from_node', 'to_node', 'meters', 'osm_way'],
                                                ((rev, k // 4, e[k], e[k + 1], e[k + 2], e[k + 3]) for k in range(0, len(e), 4)))
        for ord_, r in enumerate(order['revisions']):
            net = nets[r['id']]
            cur.execute('INSERT INTO transit.revision VALUES (%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)',
                        (r['id'], ord_, r['label'], net['basis'], r['effectiveFrom'], r['effectiveTo'], r['observed'], net['date'],
                         net['sourceFile'], net['sourceHash'], net['version'], r['id'], net['maxWalkMeters'],
                         net['notes'], json.dumps(net['diagnostics'])))
            rev, stops = r['id'], net['stops']
            counts[rev + ':stop'] = copy(cur, 'stop', ['revision', 'stop_id', 'idx', 'name', 'lat', 'lon', 'network_place', 'walk_node', 'snap_meters', 'area'],
                                         ((rev, s['id'], i, s['name'], s['lat'], s['lon'], s['place'], s['walkNode'], s['snapMeters'], area_of(s['lat'], s['lon']))
                                          for i, s in enumerate(stops)))
            counts[rev + ':network_place'] = copy(cur, 'network_place', ['revision', 'idx', 'name', 'degree', 'dominance_radius', 'whole_component'],
                                                  ((rev, i, p['name'], p['degree'], p['dominanceRadius'], p['wholeComponent'])
                                                   for i, p in enumerate(net['places'])))
            facts = round_trip.relate(net['trips'], stops, route_number)
            counts[rev + ':trip'] = copy(cur, 'trip', ['revision', 'trip_id', 'idx', 'route', 'route_number', 'pattern', 'vehicle', 'visits',
                                                        'partial', 'crosses_midnight', 'boundary_clipped',
                                                        'reference_trip', 'reference_start', 'reference_share', 'shape'],
                                         ((rev, t['id'], i, t['route'], route_number(t['route']), t['pattern'], t.get('vehicle'),
                                           len(t['events']) // 5, bool(t.get('partial')), bool(t.get('crossesMidnight')), bool(t.get('boundaryClipped')),
                                           facts[t['id']]['reference_trip'], facts[t['id']]['reference_start'], facts[t['id']]['reference_share'],
                                           facts[t['id']]['shape'])
                                          for i, t in enumerate(net['trips'])))
            counts[rev + ':visit'] = copy(cur, 'visit', ['revision', 'trip_id', 'seq', 'stop_id', 'arrival', 'departure', 'stop_sequence', 'source_row'],
                                          ((rev, t['id'], k // 5, stops[t['events'][k]]['id'], time_or_null(t['events'][k + 1]),
                                            time_or_null(t['events'][k + 2]), t['events'][k + 3], t['events'][k + 4])
                                           for t in net['trips'] for k in range(0, len(t['events']), 5)))
            counts[rev + ':walking_link'] = copy(cur, 'walking_link', ['revision', 'from_idx', 'ord', 'to_idx', 'meters'],
                                                 ((rev, i, j, link['stop'], link['meters'])
                                                  for i, links in enumerate(net['walking']) for j, link in enumerate(links)))
        counts['place'] = copy(cur, 'place', ['place_id', 'ord', 'name', 'aliases', 'lat', 'lon'],
                               ((p['id'], i, p['name'], p.get('aliases') or [], p['point'][0], p['point'][1]) for i, p in enumerate(places)))
        counts['place_stop'] = copy(cur, 'place_stop', ['place_id', 'revision', 'ord', 'stop_id', 'name', 'lat', 'lon'],
                                    ((p['id'], rev, j, m['id'], m['name'], m['point'][0], m['point'][1])
                                     for p in places for rev, members in p['members'].items() for j, m in enumerate(members or [])))
        counts['place_route'] = copy(cur, 'place_route', ['place_id', 'revision', 'ord', 'route'],
                                     ((p['id'], rev, j, route) for p in places for rev, routes in p['routes'].items() for j, route in enumerate(routes or [])))
        counts['route_lineage'] = copy(cur, 'route_lineage', ['sheet', 'ord', 'route', 'previous', 'created', 'note'],
                                       ((s['sheet'], i, s['route'], s['previous'], s['created'], s['note']) for i, s in enumerate(sheets)))
        counts['route_change'] = copy(cur, 'route_change', ['ord', 'route', 'change', 'date'],
                                      ((i, c['route'], c['change'], c['date']) for i, c in enumerate(order['changes'])))
        sources = {str(p.relative_to(ROOT)).replace('\\', '/'): sha256(p) for p in
                   [journey / 'manifest.json', *(journey / (r + '.json') for r in nets), catalogue, lineage, revisions, schema, boundaries]}
        cur.execute('INSERT INTO transit.load (version, sources) VALUES (%s, %s)', (manifest['version'], json.dumps(sources)))
        cur.execute('REFRESH MATERIALIZED VIEW transit.segment_sample')
        cur.execute('SELECT count(*) FROM transit.segment_sample')
        counts['segment_sample'] = cur.fetchone()[0]
        conn.commit()
    with psycopg.connect(dsn, autocommit=True) as conn:
        conn.execute('ANALYZE')
    return {'version': manifest['version'], 'rows': counts}


def main() -> None:
    p = argparse.ArgumentParser()
    p.add_argument('--dsn', default=DSN)
    p.add_argument('--journey', type=Path, default=region_dir() / 'build' / 'journey')
    p.add_argument('--catalogue', type=Path, default=region_dir() / 'build' / 'catalogue.json')
    p.add_argument('--lineage', type=Path, default=region_dir() / 'transit' / 'lineage.json')
    p.add_argument('--revisions', type=Path, default=ROOT / 'data' / 'transit' / 'revisions.json')
    p.add_argument('--schema', type=Path, default=ROOT / 'backend' / 'db' / 'schema.sql')
    p.add_argument('--boundaries', type=Path, default=ROOT / 'data' / 'boundaries' / 'hangjeongdong_울산광역시_20250630.geojson')
    a = p.parse_args()
    print(json.dumps(load(a.dsn, a.journey, a.catalogue, a.lineage, a.revisions, a.schema, a.boundaries), ensure_ascii=False, indent=1))


if __name__ == '__main__':
    main()
