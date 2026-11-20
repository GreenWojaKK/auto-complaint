"""지도 배경에 그릴 행정동 경계를 줄여 만든다. 원본 경계 파일은 행정동 판별의 기준으로 그대로 둔다.

  uv run --project preprocess python -m dllio_preprocess.network.map_context [--boundaries <geojson>] [--out <지역>/build/map-context.json]
"""
from __future__ import annotations

import argparse
import json
from pathlib import Path

from .. import region_dir

BOUNDARIES = region_dir() / 'boundaries.geojson'


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
    p.add_argument('--boundaries', type=Path, default=BOUNDARIES)
    p.add_argument('--out', type=Path, default=region_dir() / 'build' / 'map-context.json')
    a = p.parse_args()
    context = build(json.loads(a.boundaries.read_text(encoding='utf-8')))
    a.out.parent.mkdir(parents=True, exist_ok=True)
    a.out.write_text(json.dumps(context, ensure_ascii=False, separators=(',', ':')), encoding='utf-8')
    print(json.dumps({'areas': len(context)}))


if __name__ == '__main__':
    main()
