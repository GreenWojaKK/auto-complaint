"""자료 판들의 교통 자료에서 장소 catalogue를 만든다.

  python -m dllio_preprocess.network.catalogue [--region <지역 폴더>]

<지역>/build/journey의 판마다 정류장 묶음(place)을 읽어 한 장소로 합치고 <지역>/build/catalogue.json에 쓴다. 정류장 번호 끝
다섯 자리가 같고 30m 안이면 같은 장소이고, 아니면 이름이 같고 250m 안에서 가장 가까운 장소에 붙인다. 어느 쪽도 아니면 새
장소다. 이름은 나중 판의 자료를 따른다.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import math
import re
from pathlib import Path

from .. import region_config, region_dir, revisions

CODE = re.compile(r'\d{5}$')


def sha(text: str) -> str:
    return hashlib.sha256(text.encode()).hexdigest()


def normalize(name: str) -> str:
    return re.sub(r'\s|정류소|정류장', '', re.sub(r'\([^)]*\)', '', name))


def meters(a: list[float], b: list[float]) -> float:
    p = math.pi / 180
    d_lat, d_lon = (b[0] - a[0]) * p, (b[1] - a[1]) * p
    h = math.sin(d_lat / 2) ** 2 + math.cos(a[0] * p) * math.cos(b[0] * p) * math.sin(d_lon / 2) ** 2
    return 6371000 * 2 * math.asin(min(1, math.sqrt(h)))


def compact(value) -> str:
    return json.dumps(value, ensure_ascii=False, separators=(',', ':'))


def coordinate(value: float) -> str:
    """좌표를 장소 id에 넣는 글자. 정수로 떨어지는 값은 소수점 없이 쓴다."""
    return str(int(value)) if value == int(value) else repr(value)


def build(networks: dict[str, dict], coverage: str = '') -> dict:
    catalogue = {'version': '', 'datasets': [], 'places': []}
    places = catalogue['places']
    by_code: dict[str, list[tuple[dict, list[float]]]] = {}
    by_name: dict[str, list[dict]] = {}
    for order, (snapshot, net) in enumerate(networks.items()):
        catalogue['datasets'].append({
            'id': snapshot, 'label': '제공 시간표 · 적용 기간 미수록' if net['basis'] == 'planned' else f"{net.get('date') or '수록일 미상'} 관측 운행",
            'basis': net['basis'], 'date': net.get('date'), 'validFrom': None, 'validTo': None, 'sourceHash': net['sourceHash'],
            'networkVersion': net['version'], 'region': coverage, 'routes': sorted({t['route'] for t in net['trips']})})
        stop_routes: dict[int, set[str]] = {}
        for trip in net['trips']:
            for index in trip['events'][::5]:
                stop_routes.setdefault(index, set()).add(trip['route'])
        for np_ in net['places']:
            anchor = net['stops'][np_['stops'][0]]
            point = [anchor['lat'], anchor['lon']]
            if not all(isinstance(x, (int, float)) and math.isfinite(x) for x in point):
                continue
            name = normalize(np_['name'])
            code = CODE.search(anchor['id'])
            matched = {id(p): p for p, at in by_code.get(code[0], []) if meters(at, point) < 30} if code else {}
            if len(matched) == 1:
                place = next(iter(matched.values()))
            else:
                near = [p for p in by_name.get(name, []) if meters(p['point'], point) < 250]
                place = min(near, key=lambda p: meters(p['point'], point), default=None)
            if place is None:
                key = f"{name}:{coordinate(point[0])},{coordinate(point[1])}"
                place = {'id': 'place_' + sha(key)[:12], 'name': name, 'aliases': [], 'point': point, 'members': {}, 'routes': {}}
                places.append(place)
                by_name.setdefault(name, []).append(place)
            place['aliases'] = list(dict.fromkeys([*place['aliases'], place['name'], name]))
            if order > 0 and place['name'] != name:
                by_name[normalize(place['name'])].remove(place)
                place['name'] = name
                by_name.setdefault(name, []).append(place)
            members = [{'id': net['stops'][i]['id'], 'name': net['stops'][i]['name'], 'point': [net['stops'][i]['lat'], net['stops'][i]['lon']]} for i in np_['stops']]
            place['members'][snapshot] = [*place['members'].get(snapshot, []), *members]
            for member in members:
                if c := CODE.search(member['id']):
                    by_code.setdefault(c[0], []).append((place, member['point']))
            place['routes'][snapshot] = sorted({*place['routes'].get(snapshot, []), *(r for i in np_['stops'] for r in stop_routes.get(i, ()))})
    catalogue['version'] = sha(compact(catalogue))
    return catalogue


def main() -> None:
    p = argparse.ArgumentParser()
    p.add_argument('--region', type=Path, default=None, help='지역 폴더. 비우면 DLLIO_REGION, 그것도 없으면 예제 도시')
    a = p.parse_args()
    region = a.region or region_dir()
    journey, out = region / 'build' / 'journey', region / 'build' / 'catalogue.json'
    networks = {r['id']: json.loads((journey / f"{r['id']}.json").read_text(encoding='utf-8')) for r in revisions(region)['revisions']}
    catalogue = build(networks, region_config(region).get('coverage', ''))
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(compact(catalogue), encoding='utf-8')
    print(json.dumps({'places': len(catalogue['places']), 'version': catalogue['version'],
                      'datasets': [{'id': d['id'], 'routes': len(d['routes'])} for d in catalogue['datasets']]}, ensure_ascii=False))


if __name__ == '__main__':
    main()
