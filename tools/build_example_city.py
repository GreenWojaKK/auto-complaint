"""예제 도시 솔뫼시의 교통 자료를 만든다. 실제 도시가 아니며, 분석 흐름의 갈림길을 한 번씩 지나도록 짠 가상 자료다.

  preprocess/.venv/Scripts/python.exe tools/build_example_city.py

examples/solmoe/에 행정동 경계, 격자 보행망, 개편 전 제공 시간표와 개편 후 하루 관측 운행, 노선의 이전 번호를 쓴다.
민원, 지역 사전(priors/areas.json), 지역 지식(prior.md), 지역 설정(region.json), 자료 판(transit/revisions.json)은 손으로 쓴
파일이다. 같은 코드는 언제나 같은 파일을 만든다.

자료에 넣은 갈림길은 다음과 같다.

- 개편으로 번호가 바뀐 노선(15 → 101, 27 → 207)과 한 번호를 둘이 이어받은 노선(33 → 210·211)
- 개편으로 경유지가 빠진 노선(102: 다솜아파트·다솜1동주민센터)
- 같은 정류장을 한 운행에서 두 번 지나는 순환 노선(110: 솔뫼역)
- 절반이 솔뫼역에서 되돌아가는 단축 운행이 섞인 노선(105)
- 번호가 네 자리인 요금 2배 노선(1500)과 개편 뒤 새로 생긴 출퇴근 노선(301)
- 길 건너 마주 보는 같은 이름의 정류장, 이름 일부가 겹치는 정류장(한빛사거리·한빛아파트·한빛도서관)
- 개편 전 이름이 다른 정류장(시청앞 → 시청)과 개편 뒤에 생긴 정류장(솔밭산업단지정문)
- 관측 기록에서 빠진 출발 시각, 빠진 방문, 앞 정류장을 놓친 운행, 도착 시각이 없어 빠지는 기록
"""
from __future__ import annotations

import csv
import gzip
import hashlib
import json
import math
import random
from pathlib import Path

from dllio_preprocess.network.lineage import route_of

ROOT = Path(__file__).resolve().parents[1]
OUT = ROOT / 'examples' / 'solmoe'

# 격자 도로. 열 i는 동서 방향 위치, 행 j는 남북 방향 위치다. 행정동 경계가 도로 위에 놓이지 않게 원점을 비켜 둔다.
LAT0, LON0, DLAT, DLON = 36.5812, 130.8615, 0.0025, 0.003
COLS, ROWS = 27, 17
LON_EDGES = [130.857, 130.88, 130.90, 130.92, 130.943]
LAT_EDGES = [36.578, 36.60, 36.624]
DONGS = [  # 구, 동, 경계 열 번호(서→동), 행 번호(남→북)
    ('서구', '다솜2동', 0, 0), ('서구', '가온동', 1, 0), ('서구', '미리내동', 0, 1), ('서구', '다솜1동', 1, 1),
    ('동구', '한빛동', 2, 0), ('동구', '너울동', 3, 0), ('동구', '바람골동', 2, 1), ('동구', '솔밭동', 3, 1),
]

# 정류장 자리: 이름 → (열, 행, 도로 방향, 한쪽에만 서는지). 'h'는 동서 도로, 'v'는 남북 도로 위다.
# 양쪽에 서는 자리는 길 건너 마주 보는 두 정류장이고, 갈 때(+)와 올 때(-) 다른 쪽에 선다.
STATIONS = {
    '미리내차고지': (1, 13, 'h', True), '미리내마을': (2, 11, 'h', False), '미리내초등학교': (4, 11, 'h', False),
    '다솜아파트': (8, 12, 'v', False), '다솜1동주민센터': (10, 9, 'h', False), '강변공원': (11, 11, 'h', False),
    '다솜2동행정복지센터': (3, 4, 'h', True), '다솜시장': (5, 6, 'h', False),
    '중앙시장': (8, 6, 'h', False), '시청': (11, 6, 'h', False), '가온사거리': (12, 3, 'v', False), '가온초등학교': (9, 2, 'h', False),
    '한빛사거리': (14, 6, 'h', False), '한빛아파트': (17, 6, 'h', False), '솔뫼역': (16, 4, 'v', False), '한빛도서관': (18, 2, 'h', False),
    '너울시장': (22, 4, 'h', False), '너울항': (25, 3, 'h', True),
    '바람골초등학교': (14, 10, 'h', False), '바람골시장': (16, 11, 'v', False), '바람골종점': (17, 14, 'h', True),
    '솔밭삼거리': (21, 9, 'h', False), '솔밭산업단지정문': (24, 10, 'v', False), '솔밭산업단지': (23, 12, 'h', True),
}
RENAMED_BEFORE = {'시청': '시청앞'}
NEW_AFTER = {'솔밭산업단지정문'}

OBSERVED_DAY = '20250415'
PEAKS = [(7 * 3600, 9 * 3600), (17 * 3600, 19 * 3600)]


def grid(i: float, j: float) -> tuple[float, float]:
    return LAT0 + DLAT * j, LON0 + DLON * i


def meters(a: tuple[float, float], b: tuple[float, float]) -> float:
    p = math.pi / 180
    h = math.sin((b[0] - a[0]) * p / 2) ** 2 + math.cos(a[0] * p) * math.cos(b[0] * p) * math.sin((b[1] - a[1]) * p / 2) ** 2
    return 6371000 * 2 * math.asin(min(1, math.sqrt(h)))


def stops() -> dict[tuple[str, str], dict]:
    """정류장 자리와 쪽마다 정류장 하나. 정류장 번호는 다섯 자리이고 개편 전 자료는 앞에 BS_SM_을 붙인다."""
    out, code = {}, 31001
    for name, (i, j, axis, single) in STATIONS.items():
        lat, lon = grid(i, j)
        for side in ('+',) if single else ('+', '-'):
            sign = 1 if side == '+' else -1
            point = (lat + sign * 0.00011, lon + 0.0012) if axis == 'h' else (lat + 0.001, lon + sign * 0.00014)
            out[(name, side)] = {'code': str(code), 'name': name, 'lat': round(point[0], 6), 'lon': round(point[1], 6)}
            code += 1
    return out


def round_trip(stations: list[str]) -> list[tuple[str, str]]:
    """갈 때 +쪽, 올 때 -쪽 정류장에 서고 첫 정류장으로 돌아오는 왕복 운행."""
    return [(s, '+') for s in stations] + [(s, '-') for s in reversed(stations[:-1])]


def visit_stop(all_stops: dict, station: str, side: str) -> dict:
    return all_stops.get((station, side)) or all_stops[(station, '+')]


# 노선: 번호, 방면, 정류장 순서(갈 때), 배차 간격(분), 첫 출발, 마지막 출발, 속도 배율
BEFORE = [
    ('15', '너울항', round_trip(['미리내차고지', '미리내마을', '미리내초등학교', '다솜시장', '중앙시장', '시청', '한빛사거리', '한빛아파트', '솔뫼역', '너울시장', '너울항']), 15, '05:40', '23:30', 1.0),
    ('102', '솔뫼역', round_trip(['미리내차고지', '미리내초등학교', '다솜아파트', '다솜1동주민센터', '강변공원', '시청', '가온사거리', '솔뫼역']), 20, '06:00', '22:00', 1.0),
    ('105', '솔밭산업단지', round_trip(['다솜2동행정복지센터', '다솜시장', '중앙시장', '시청', '한빛사거리', '솔뫼역', '솔밭삼거리', '솔밭산업단지']), 30, '06:00', '21:30', 1.0),
    ('110', '솔뫼역', [('바람골종점', '+'), ('바람골시장', '+'), ('솔뫼역', '+'), ('한빛도서관', '+'), ('가온사거리', '+'), ('시청', '+'), ('한빛사거리', '+'), ('솔뫼역', '+'), ('바람골시장', '-'), ('바람골종점', '+')], 40, '06:20', '21:40', 1.0),
    ('27', '너울항', round_trip(['다솜2동행정복지센터', '가온초등학교', '가온사거리', '솔뫼역', '한빛도서관', '너울시장', '너울항']), 20, '06:00', '22:00', 1.0),
    ('33', '바람골종점', round_trip(['너울항', '너울시장', '솔뫼역', '바람골초등학교', '바람골시장', '바람골종점']), 15, '05:50', '22:20', 1.0),
    ('1500', '미리내차고지', round_trip(['솔밭산업단지', '솔밭삼거리', '솔뫼역', '시청', '미리내차고지']), 20, '06:00', '22:00', 1.5),
]
AFTER = [
    ('101', '너울항', BEFORE[0][2], 20, '06:00', '22:00', 1.0),
    ('102', '솔뫼역', round_trip(['미리내차고지', '미리내초등학교', '강변공원', '시청', '가온사거리', '솔뫼역']), 25, '06:00', '22:00', 1.0),
    ('105', '솔밭산업단지', BEFORE[2][2], 60, '06:00', '21:00', 1.0),
    ('105', '솔뫼역', round_trip(['다솜2동행정복지센터', '다솜시장', '중앙시장', '시청', '한빛사거리', '솔뫼역']), 60, '06:30', '21:30', 1.0),
    ('110', '솔뫼역', BEFORE[3][2], 40, '06:20', '21:40', 1.0),
    ('207', '너울항', BEFORE[4][2], 35, '06:00', '21:45', 1.0),
    ('210', '솔뫼역', round_trip(['너울항', '너울시장', '솔뫼역']), 30, '06:00', '22:00', 1.0),
    ('211', '바람골종점', round_trip(['솔뫼역', '바람골초등학교', '바람골시장', '바람골종점']), 30, '06:00', '22:00', 1.0),
    ('301', '솔밭산업단지', round_trip(['바람골종점', '바람골시장', '솔밭삼거리', '솔밭산업단지정문', '솔밭산업단지']), 40, '06:50', '08:50', 1.0),
    ('301', '바람골종점', round_trip(['솔밭산업단지', '솔밭삼거리', '바람골시장', '바람골종점']), 30, '17:30', '19:00', 1.0),
    ('1500', '미리내차고지', BEFORE[6][2], 20, '06:00', '22:00', 1.5),
]
SHEETS = ['101(구15)', '102', '105', '110', '207(구27)', '210(구33)', '211(구33)', '301(신설)', '1500']


def seconds(clock: str) -> int:
    h, m = clock.split(':')
    return int(h) * 3600 + int(m) * 60


def hms(value: int | None) -> str:
    return '' if value is None else f'{value // 3600}:{value % 3600 // 60:02d}:{value % 60:02d}'


def legs(pattern: list[tuple[str, str]], all_stops: dict) -> list[float]:
    """정류장 사이 도로 거리(직선 거리의 1.35배)."""
    points = [visit_stop(all_stops, s, side) for s, side in pattern]
    return [meters((a['lat'], a['lon']), (b['lat'], b['lon'])) * 1.35 for a, b in zip(points, points[1:])]


def planned(all_stops: dict) -> list[dict]:
    rows = []
    for number, heading, pattern, headway, first, last, pace in BEFORE:
        route_id = f'BR_SM_{number}'
        distance = legs(pattern, all_stops)
        for k, start in enumerate(range(seconds(first), seconds(last) + 1, headway * 60)):
            at = start
            for sequence, (station, side) in enumerate(pattern, 1):
                stop = visit_stop(all_stops, station, side)
                if sequence > 1:
                    at += round(distance[sequence - 2] / (5.5 * pace)) + 20
                dwell = 0 if sequence in (1, len(pattern)) else 10
                rows.append({'trip_id': f'{route_id}_T{k + 1:03d}', 'route_name': f'{number}({heading} 방면)', 'route_id': route_id,
                             'stop_id': 'BS_SM_' + stop['code'], 'stop_name': RENAMED_BEFORE.get(station, station), 'stop_sequence': sequence,
                             'arrival_time': hms(at), 'departure_time': hms(at + dwell)})
                at += dwell
    return rows


def observed(all_stops: dict, rng: random.Random) -> list[dict]:
    rows = []
    for number, heading, pattern, headway, first, last, pace in AFTER:
        route_id = f'SM{number}' + ('' if heading == AFTER_HEADINGS[number] else '-2')
        distance = legs(pattern, all_stops)
        free: list[int] = []  # 차량마다 다음 운행을 시작할 수 있는 시각
        for start in range(seconds(first), seconds(last) + 1, headway * 60):
            vehicle = next((v for v, t in enumerate(free) if t <= start), None)
            if vehicle is None:
                vehicle = len(free)
                free.append(0)
            at = start + max(-60, round(rng.gauss(40, 70)))
            trip = []
            for sequence, (station, side) in enumerate(pattern, 1):
                if sequence > 1:
                    slow = 1.3 if any(a <= at < b for a, b in PEAKS) else 1.0
                    at += round(distance[sequence - 2] / (6.0 * pace) * slow * rng.uniform(0.9, 1.25)) + 15
                dwell = 0 if sequence == len(pattern) else rng.randint(15, 45)
                trip.append((sequence, station, side, at, at + dwell))
                at += dwell
            free[vehicle] = at + 300
            # 단말 기록의 빈 곳: 앞 정류장을 놓친 운행, 빠진 방문, 빠진 출발 시각, 도착 시각이 없는 기록
            if rng.random() < 0.04:
                trip = trip[2:]
            for sequence, station, side, arrival, departure in trip:
                if 1 < sequence < len(pattern) and rng.random() < 0.01:
                    continue
                stop = visit_stop(all_stops, station, side)
                missing = rng.random()
                rows.append({'date': OBSERVED_DAY, 'vehicle': f"V{route_id.removeprefix('SM')}-{vehicle + 1:02d}", 'route_name': f'{number}({heading} 방면)',
                             'route_id': route_id, 'stop_id': stop['code'], 'stop_name': station, 'stop_lat': stop['lat'], 'stop_lon': stop['lon'],
                             'stop_sequence': sequence, 'arrival_time': '' if missing < 0.003 else hms(arrival),
                             'departure_time': '' if missing < 0.025 else hms(departure)})
    # 단말 기록은 차량·시각 순서로 쌓이고 운행 번호가 없다.
    rows.sort(key=lambda r: (r['vehicle'], r['route_id'], r['arrival_time'].zfill(8), r['stop_sequence']))
    return rows


AFTER_HEADINGS = {number: heading for number, heading, *_ in reversed(AFTER)}


def write_csv(path: Path, rows: list[dict]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open('w', encoding='utf-8', newline='') as f:
        writer = csv.DictWriter(f, fieldnames=list(rows[0]), lineterminator='\n')
        writer.writeheader()
        writer.writerows(rows)


def stop_rows(all_stops: dict, before: bool) -> list[dict]:
    rows = []
    for (station, _), s in all_stops.items():
        if before and station in NEW_AFTER:
            continue
        rows.append({'stop_id': ('BS_SM_' if before else '') + s['code'], 'stop_name': RENAMED_BEFORE.get(station, station) if before else station,
                     'stop_lat': s['lat'], 'stop_lon': s['lon']})
    return rows


def boundaries() -> dict:
    features = []
    for gu, dong, col, row in DONGS:
        west, east, south, north = LON_EDGES[col], LON_EDGES[col + 1], LAT_EDGES[row], LAT_EDGES[row + 1]
        ring = [[west, south], [east, south], [east, north], [west, north], [west, south]]
        features.append({'type': 'Feature', 'properties': {'adm_nm': f'솔뫼시 {gu} {dong}', 'sidonm': '솔뫼시', 'sggnm': gu, 'dongnm': dong},
                         'geometry': {'type': 'MultiPolygon', 'coordinates': [[ring]]}})
    return {'type': 'FeatureCollection', 'name': 'solmoe-boundaries', 'features': features}


def streets() -> dict:
    """격자 도로를 OpenStreetMap 형식으로. 행 6과 11, 열 11과 16은 큰길이다."""
    elements, ids = [], {}
    for j in range(ROWS):
        for i in range(COLS):
            ids[(i, j)] = len(ids) + 1
            lat, lon = grid(i, j)
            elements.append({'type': 'node', 'id': ids[(i, j)], 'lat': round(lat, 6), 'lon': round(lon, 6)})
    way = 100000
    for j in range(ROWS):
        way += 1
        elements.append({'type': 'way', 'id': way, 'nodes': [ids[(i, j)] for i in range(COLS)], 'tags': {'highway': 'secondary' if j in (6, 11) else 'residential'}})
    for i in range(COLS):
        way += 1
        elements.append({'type': 'way', 'id': way, 'nodes': [ids[(i, j)] for j in range(ROWS)], 'tags': {'highway': 'secondary' if i in (11, 16) else 'residential'}})
    return {'elements': elements}


def main() -> None:
    rng = random.Random(20250415)
    all_stops = stops()
    write_csv(OUT / 'transit' / 'before' / 'stops.csv', stop_rows(all_stops, True))
    write_csv(OUT / 'transit' / 'before' / 'visits.csv', planned(all_stops))
    write_csv(OUT / 'transit' / 'after' / 'stops.csv', stop_rows(all_stops, False))
    write_csv(OUT / 'transit' / 'after' / 'visits.csv', observed(all_stops, rng))
    sheets = '\n'.join(SHEETS).encode()
    lineage = {'source': {'file': 'tools/build_example_city.py', 'sha256': hashlib.sha256(sheets).hexdigest(), 'sheets': len(SHEETS)},
               'lineage': [route_of(s) for s in SHEETS]}
    (OUT / 'transit' / 'lineage.json').write_text(json.dumps(lineage, ensure_ascii=False, indent=1) + '\n', encoding='utf-8', newline='\n')
    (OUT / 'boundaries.geojson').write_text(json.dumps(boundaries(), ensure_ascii=False) + '\n', encoding='utf-8', newline='\n')
    (OUT / 'streets').mkdir(parents=True, exist_ok=True)
    text = json.dumps(streets(), separators=(',', ':')).encode()
    (OUT / 'streets' / 'walk.osm.json.gz').write_bytes(gzip.compress(text, mtime=0))
    source = {'provider': '예제 도시 생성기', 'license': 'CC0-1.0', 'url': 'tools/build_example_city.py', 'endpoint': None,
              'boxes': [[LON_EDGES[0], LAT_EDGES[0], LON_EDGES[-1], LAT_EDGES[-1]]], 'fetchedAt': '2025-04-15T00:00:00+09:00',
              'note': '보행은 예제 도시의 격자 도로망 기준이며, 정류장과 도로 사이의 짧은 접근 연결을 포함합니다.'}
    (OUT / 'streets' / 'source.json').write_text(json.dumps(source, ensure_ascii=False, indent=2) + '\n', encoding='utf-8', newline='\n')
    print(json.dumps({'stops': len(all_stops), 'out': str(OUT.relative_to(ROOT))}, ensure_ascii=False))


if __name__ == '__main__':
    main()
