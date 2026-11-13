"""DLLIO 교통 자료 전처리. 지역 폴더의 원천 자료로 연결망, 장소 목록, 지도 배경을 만들고 PostgreSQL에 싣는다.

지역 폴더는 환경 변수 DLLIO_REGION(저장소 루트 기준 상대 경로 또는 절대 경로)이 가리키고, 없으면 예제 도시
examples/solmoe다. 민원 분석 단계는 backend/의 Go가 실행한다.
"""
import json
import os
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]
DEFAULT_REGION = ROOT / 'examples' / 'solmoe'


def region_dir() -> Path:
    value = os.environ.get('DLLIO_REGION', '').strip()
    if not value:
        return DEFAULT_REGION
    path = Path(value)
    return path if path.is_absolute() else ROOT / path


def region_config(region: Path | None = None) -> dict:
    return json.loads(((region or region_dir()) / 'region.json').read_text(encoding='utf-8'))


def revisions(region: Path | None = None) -> dict:
    return json.loads(((region or region_dir()) / 'transit' / 'revisions.json').read_text(encoding='utf-8'))


def shown(path: Path) -> str:
    """기록에 남기는 경로. 저장소 안이면 루트 기준 상대 경로다."""
    try:
        return path.resolve().relative_to(ROOT).as_posix()
    except ValueError:
        return path.as_posix()
