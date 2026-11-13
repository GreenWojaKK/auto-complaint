"""Writes the transit network of each revision for the app.

  python -m dllio_preprocess.network.build [--region <지역 폴더>] [--out <결과 폴더>]

지역 폴더의 transit/revisions.json에 적힌 판마다 원천 자료를 읽어 정류장·운행·실제 방문과 원본 행, 장소·보행 연결을
<지역>/build/journey/<판>.json에 쓴다. 보행망은 <지역>/streets/walk.osm.json.gz다.
"""
from __future__ import annotations

import argparse
import copy
import hashlib
import json
from pathlib import Path

import numpy as np

from .. import ROOT, region_dir, revisions
from . import places, source, walking

WALK_NOTE = '보행은 현재 OpenStreetMap 도로·보행망 기준이며, 정류장과 도로 사이의 짧은 접근 연결을 포함합니다.'


def sha(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def network(transit: Path, revision: dict, base: dict, version: str, walk_note: str) -> dict:
    snap = source.read(transit, revision)
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
        if snap.restored:
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
    raw = (transit / snap.source_file).read_bytes()
    return {'version': version, 'snapshot': revision['id'], 'label': revision['label'], 'basis': revision['basis'],
            'date': revision.get('observed'), 'sourceFile': snap.source_file, 'sourceHash': sha(raw),
            'stops': stops, 'trips': trips, 'places': places.describe(snap.stops, place, orders),
            'street': graph, 'walking': links, 'maxWalkMeters': walking.WALK_METERS,
            'diagnostics': {'sourceRows': snap.rows, 'excluded': snap.excluded, 'unknownDepartures': int((snap.events['departure'] < 0).sum()),
                            'tripGroups': len(trips), 'partialTrips': int(snap.trips['partial'].sum()),
                            'streetConnectedStops': sum(n >= 0 for n in walk_node)},
            'notes': [*revision.get('notes', []), walk_note]}


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument('--region', type=Path, default=None, help='지역 폴더. 비우면 DLLIO_REGION, 그것도 없으면 예제 도시')
    parser.add_argument('--out', type=Path, default=None, help='결과 폴더. 비우면 <지역>/build')
    args = parser.parse_args()
    region = args.region or region_dir()
    build = args.out or region / 'build'
    street_source = json.loads((region / 'streets' / 'source.json').read_text(encoding='utf-8'))
    base = walking.street(region / 'streets' / 'walk.osm.json.gz', street_source)
    code = b''.join(p.read_bytes() for p in sorted(Path(__file__).parent.glob('*.py')))
    version = 'journey-network-3:' + sha(code + (ROOT / 'backend' / 'journey' / 'router.go').read_bytes())[:16]
    out = build / 'journey'
    out.mkdir(parents=True, exist_ok=True)
    manifests = []
    hashes = {}
    for revision in revisions(region)['revisions']:
        net = network(region / 'transit', revision, base, version, street_source.get('note', WALK_NOTE))
        (out / f"{revision['id']}.json").write_text(json.dumps(net, ensure_ascii=False, separators=(',', ':')), encoding='utf-8')
        hashes[revision['id']] = sha(json.dumps(net, ensure_ascii=False, sort_keys=True).encode())
        manifest = {'snapshot': revision['id'], 'sourceHash': net['sourceHash'], 'stops': len(net['stops']), 'trips': len(net['trips']),
                    'walkLinks': sum(len(v) for v in net['walking']), **net['diagnostics']}
        manifests.append(manifest)
        print(json.dumps(manifest, ensure_ascii=False))
    (out / 'manifest.json').write_text(json.dumps({'version': version, 'snapshots': hashes}, indent=2), encoding='utf-8')
    (build / 'journey-provenance.json').write_text(
        json.dumps({'street': {**street_source, 'sha256': base['hash']}, 'snapshots': manifests}, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')


if __name__ == '__main__':
    main()
