"""Writes the transit network of each snapshot for the app.

  uv run --project preprocess python -m dllio_preprocess.network.build [--data <지역>/transit] [--out <지역>/build/journey]

정류장·운행·실제 방문과 원본 행, 장소·보행 연결을 저장한다.
"""
from __future__ import annotations

import argparse
import copy
import hashlib
import json
from pathlib import Path

import numpy as np

from .. import ROOT, region_dir
from . import places, source, walking

LABELS = {'before': '개편 전 제공 시간표', 'after': '2025.05.06 관측 운행'}
NOTES = {'before': '제공 시간표에 운행일·적용 기간이 없어 특정 날짜의 실제 운행으로 해석하지 않습니다.',
         'after': '같은 차량·노선의 시간 순서와 정류장 방문 순번으로 관측 운행을 연결했습니다. 모화 구간은 수록되어 있지 않습니다.'}
WALK_NOTE = '보행은 현재 OpenStreetMap 도로·보행망 기준이며, 정류장과 도로 사이의 짧은 접근 연결을 포함합니다.'


def sha(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def network(data: Path, snapshot: str, base: dict, version: str) -> dict:
    snap = source.read(data, snapshot)
    place = places.group(snap.stops)
    graph = copy.deepcopy(base)
    walk_node, snap_meters = walking.connect(graph, snap.stops)
    stops = [{'id': r.id, 'name': r.name, 'lat': float(r.lat), 'lon': float(r.lon), 'place': int(place[i]), 'walkNode': walk_node[i],
              'snapMeters': snap_meters[i]} for i, r in enumerate(snap.stops.itertuples())]
    trips = []
    for (t, events), row in zip(snap.events.groupby('trip', sort=True), snap.trips.itertuples()):
        assert t == row.Index
        flat = events[['stop', 'arrival', 'departure', 'sequence', 'row']].to_numpy(np.int64).ravel().tolist()
        trip = {'id': row.id, 'route': row.route, 'pattern': row.pattern}
        if snapshot == 'after':
            trip['vehicle'] = row.vehicle
        trip['events'] = flat
        for flag, key in (('partial', 'partial'), ('crosses_midnight', 'crossesMidnight'), ('boundary_clipped', 'boundaryClipped')):
            if getattr(row, flag):
                trip[key] = True
        trips.append(trip)
    # 장소 연결에만 패턴별 순서를 사용하며, 충돌한 순번의 양쪽 연결을 끊는다.
    ev = snap.events.assign(pattern=snap.trips['pattern'].to_numpy()[snap.events['trip'].to_numpy()])
    orders = {}
    for pid, group in ev.groupby('pattern'):
        orders[str(pid)] = [int(v.iloc[0]) if v.nunique() == 1 else None
                            for _, v in group.groupby('sequence')['stop']]
    links = walking.links(graph, walk_node)
    raw = (data / snap.source_file).read_bytes()
    return {'version': version, 'snapshot': snapshot, 'label': LABELS[snapshot], 'basis': 'planned' if snapshot == 'before' else 'observed',
            'date': '2025-05-06' if snapshot == 'after' else None, 'sourceFile': snap.source_file, 'sourceHash': sha(raw),
            'stops': stops, 'trips': trips, 'places': places.describe(snap.stops, place, orders),
            'street': graph, 'walking': links, 'maxWalkMeters': walking.WALK_METERS,
            'diagnostics': {'sourceRows': snap.rows, 'excluded': snap.excluded, 'unknownDepartures': int((snap.events['departure'] < 0).sum()),
                            'tripGroups': len(trips), 'partialTrips': int(snap.trips['partial'].sum()),
                            'streetConnectedStops': sum(n >= 0 for n in walk_node)},
            'notes': [NOTES[snapshot], WALK_NOTE]}


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument('--data', type=Path, default=region_dir() / 'transit')
    parser.add_argument('--streets', type=Path, default=region_dir() / 'streets')
    parser.add_argument('--out', type=Path, default=region_dir() / 'build' / 'journey')
    parser.add_argument('--provenance', type=Path, default=region_dir() / 'build' / 'journey-provenance.json')
    args = parser.parse_args()
    street_source = json.loads((args.streets / 'source.json').read_text(encoding='utf-8'))
    base = walking.street(args.streets / 'walk.osm.json.gz', street_source)
    code = b''.join(p.read_bytes() for p in sorted(Path(__file__).parent.glob('*.py')))
    version = 'journey-network-3:' + sha(code + (ROOT / 'backend' / 'journey' / 'router.go').read_bytes())[:16]
    args.out.mkdir(parents=True, exist_ok=True)
    manifests = []
    hashes = {}
    for snapshot in ('before', 'after'):
        net = network(args.data, snapshot, base, version)
        (args.out / f'{snapshot}.json').write_text(json.dumps(net, ensure_ascii=False, separators=(',', ':')), encoding='utf-8')
        hashes[snapshot] = sha(json.dumps(net, ensure_ascii=False, sort_keys=True).encode())
        manifest = {'snapshot': snapshot, 'sourceHash': net['sourceHash'], 'stops': len(net['stops']), 'trips': len(net['trips']),
                    'walkLinks': sum(len(v) for v in net['walking']), **net['diagnostics']}
        manifests.append(manifest)
        print(json.dumps(manifest, ensure_ascii=False))
    (args.out / 'manifest.json').write_text(json.dumps({'version': version, 'snapshots': hashes}, indent=2), encoding='utf-8')
    args.provenance.write_text(json.dumps({'street': {**street_source, 'sha256': base['hash']}, 'snapshots': manifests}, ensure_ascii=False, indent=2) + '\n',
                               encoding='utf-8')


if __name__ == '__main__':
    main()
