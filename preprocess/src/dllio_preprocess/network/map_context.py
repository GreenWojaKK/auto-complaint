"""지도 배경에 그릴 행정동 경계를 줄여 만든다. 원본 경계 파일은 행정동 판별의 기준으로 그대로 둔다.

  python -m dllio_preprocess.network.map_context [--region <지역 폴더>]

<지역>/boundaries.geojson을 읽어 <지역>/build/map-context.json에 쓴다.
"""
from __future__ import annotations

import argparse
import json
from pathlib import Path

from .. import region_dir


def simplify(ring: list[list[float]], points: int = 160) -> list[list[float]]:
    """고리를 약 points개 점으로 줄이고 좌표를 소수 다섯 자리로 자른다. 닫힌 고리를 유지한다."""
    stride = max(1, len(ring) // points)
    out = [[round(x, 5) for x in point] for point in ring[::stride]]
    if out[0] != out[-1]:
        out.append(out[0])
    return out


def build(geo: dict) -> list[dict]:
    return [{'name': f['properties']['dongnm'], 'polygons': [[simplify(ring) for ring in polygon] for polygon in f['geometry']['coordinates']]}
            for f in geo['features']]


def main() -> None:
    p = argparse.ArgumentParser()
    p.add_argument('--region', type=Path, default=None, help='지역 폴더. 비우면 DLLIO_REGION, 그것도 없으면 예제 도시')
    a = p.parse_args()
    region = a.region or region_dir()
    out = region / 'build' / 'map-context.json'
    context = build(json.loads((region / 'boundaries.geojson').read_text(encoding='utf-8')))
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(context, ensure_ascii=False, separators=(',', ':')), encoding='utf-8')
    print(json.dumps({'areas': len(context)}))


if __name__ == '__main__':
    main()
