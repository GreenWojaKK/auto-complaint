"""Route numbers and the earlier numbers they took over, read from the sheet names of the city's weekday timetable workbook.

  uv run --project preprocess python -m dllio_preprocess.network.lineage [--xlsx <지역>/transit/weekday-timetable.xlsx]
      [--out <지역>/transit/lineage.json]

Each sheet is one route. Its name gives the route and, in brackets, the earlier route numbers it took over
('543(구857)', '울주81(구 울주01)') or that it is new ('741(신설)'). Only the sheet names are read; the timetables in the
sheets are not used.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import re
from pathlib import Path

import openpyxl

from .. import ROOT, region_dir


def route_of(title: str) -> dict:
    """'543(구857)' → route 543 taking over 857; '741(신설)' → new; '5001번(리무진)' → 5001 with a note."""
    m = re.match(r'^\s*(.+?)\s*(?:\((.+?)\))?\s*$', title)
    name, bracket = re.sub(r'번$', '', m[1].strip()), (m[2] or '').strip()
    out = {'route': name, 'previous': [], 'created': False, 'note': None, 'sheet': title}
    if bracket.startswith('구'):
        out['previous'] = [re.sub(r'번$', '', p.strip()) for p in re.split(r'[,·/]', bracket[1:]) if p.strip()]
    elif bracket == '신설':
        out['created'] = True
    elif bracket:
        out['note'] = bracket
    return out


def read(path: Path) -> dict:
    book = openpyxl.load_workbook(path, read_only=True)
    names = list(book.sheetnames)
    return {'source': {'file': path.name, 'sha256': hashlib.sha256(path.read_bytes()).hexdigest(), 'sheets': len(names)},
            'lineage': [route_of(n) for n in names]}


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument('--xlsx', type=Path, default=region_dir() / 'transit' / 'weekday-timetable.xlsx')
    parser.add_argument('--out', type=Path, default=region_dir() / 'transit' / 'lineage.json')
    args = parser.parse_args()
    data = read(args.xlsx)
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(json.dumps(data, ensure_ascii=False, indent=1), encoding='utf-8')
    rows = data['lineage']
    print(json.dumps({'sheets': len(rows), 'with_previous': sum(bool(r['previous']) for r in rows), 'created': sum(r['created'] for r in rows)}))


if __name__ == '__main__':
    main()
