"""한국 공휴일 표를 data/priors/holidays.json으로 만든다. 대체공휴일과 음력 명절을 포함한 holidays 패키지의 한국 달력이다.

  preprocess/.venv/Scripts/python.exe tools/build_holidays.py [--from 2000] [--to 2050]

Go의 달력(backend/calendar)이 이 표로 날짜의 공휴일 이름과 요일 종류를 정한다.
"""
import argparse
import json
from importlib.metadata import version
from pathlib import Path

import holidays

ROOT = Path(__file__).resolve().parents[1]


def main() -> None:
    p = argparse.ArgumentParser()
    p.add_argument('--from', dest='start', type=int, default=2000)
    p.add_argument('--to', dest='end', type=int, default=2050)
    a = p.parse_args()
    days = holidays.country_holidays('KR', years=range(a.start, a.end + 1), language='ko')
    table = {d.isoformat(): days.get(d) for d in sorted(days)}
    out = {'source': f'holidays {version("holidays")} KR ko', 'years': [a.start, a.end], 'holidays': table}
    path = ROOT / 'data' / 'priors' / 'holidays.json'
    path.write_text(json.dumps(out, ensure_ascii=False, indent=1) + '\n', encoding='utf-8')
    print(json.dumps({'path': str(path.relative_to(ROOT)), 'days': len(table)}, ensure_ascii=False))


if __name__ == '__main__':
    main()
