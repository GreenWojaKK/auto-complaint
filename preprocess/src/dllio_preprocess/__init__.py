"""DLLIO 교통 자료 전처리. GTFS·시간표·장소 자료로 연결망, 장소 목록, 노선 계보, 지도 배경을 만들고 PostgreSQL에 싣는다.

민원 분석 단계는 backend/의 Go가 실행한다.
"""
import os
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]


def region_dir() -> Path:
    """지역 폴더. 환경 변수 DLLIO_REGION(저장소 루트 기준 상대 경로 또는 절대 경로)이 가리키고, 없으면 private/ulsan이다."""
    return ROOT / (os.environ.get('DLLIO_REGION', '').strip() or 'private/ulsan')
