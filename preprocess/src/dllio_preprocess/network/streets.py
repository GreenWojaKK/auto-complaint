"""OpenStreetMap에서 보행망 원본을 내려받는다.

  python -m dllio_preprocess.network.streets [--region <지역 폴더>]

<지역>/streets/source.json의 boxes 범위를 받아 <지역>/streets/walk.osm.json.gz에 쓰고 source.json에 받은 시각을 적는다.
범위가 커서 거절당하면 범위를 위아래로 나눠 다시 받는다.
"""
from __future__ import annotations

import argparse
import gzip
import json
import re
from datetime import datetime, timezone
from pathlib import Path

import httpx

from .. import region_dir

ENDPOINT = 'https://api.openstreetmap.org/api/0.6/map.json'
HEADERS = {'User-Agent': 'Dllio-Transit-Research/0.1'}
MAX_SPLITS = 3


def download(client: httpx.Client, box: list[float], elements: dict, depth: int = 0) -> None:
    response = client.get(ENDPOINT, params={'bbox': ','.join(map(str, box))})
    if response.status_code != 200:
        if response.status_code == 400 and re.search('nodes|large|many', response.text, re.I) and depth < MAX_SPLITS:
            middle = (box[1] + box[3]) / 2
            download(client, [box[0], box[1], box[2], middle], elements, depth + 1)
            download(client, [box[0], middle, box[2], box[3]], elements, depth + 1)
            return
        raise RuntimeError(f'OSM HTTP {response.status_code}: {response.text[:180]}')
    found = response.json()['elements']
    for element in found:
        elements[f"{element['type']}{element['id']}"] = element
    print(json.dumps({'box': box, 'elements': len(found)}), flush=True)


def main() -> None:
    p = argparse.ArgumentParser()
    p.add_argument('--region', type=Path, default=None, help='지역 폴더. 비우면 DLLIO_REGION, 그것도 없으면 예제 도시')
    a = p.parse_args()
    out = (a.region or region_dir()) / 'streets'
    boxes = json.loads((out / 'source.json').read_text(encoding='utf-8'))['boxes']
    elements: dict = {}
    with httpx.Client(headers=HEADERS, timeout=None) as client:
        for box in boxes:
            download(client, box, elements)
    text = json.dumps({'elements': list(elements.values())}, ensure_ascii=False, separators=(',', ':'))
    out.mkdir(parents=True, exist_ok=True)
    (out / 'walk.osm.json.gz').write_bytes(gzip.compress(text.encode()))
    source = {'provider': 'OpenStreetMap contributors', 'license': 'ODbL 1.0', 'url': 'https://www.openstreetmap.org/copyright', 'endpoint': ENDPOINT,
              'boxes': boxes, 'fetchedAt': datetime.now(timezone.utc).isoformat()}
    (out / 'source.json').write_text(json.dumps(source, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
    print(json.dumps({'elements': len(elements), 'bytes': len(text)}))


if __name__ == '__main__':
    main()
