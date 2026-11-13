"""Join a census boundary Shapefile to its matching code sheet and export GeoJSON.

Example (run from the repository root):
  python -X utf8 tools/build_admin_geojson.py --shapefile PATH.shp --codes PATH.xlsx --sido 울산광역시 --output private/ulsan/boundaries.geojson

Dependencies: openpyxl, pyshp, pyproj, shapely. The source workbook and boundary are read-only.
"""

import argparse
from collections import Counter
from datetime import datetime, timezone
import hashlib
import importlib.metadata
import json
from pathlib import Path
import re
import sys

ROOT = Path(__file__).resolve().parents[1]

import numpy as np
import openpyxl
from pyproj import CRS, Transformer
import shapefile
from shapely import get_coordinates
from shapely.geometry import MultiPolygon, mapping, shape
from shapely.geometry.polygon import orient
from shapely.ops import transform
from shapely.validation import explain_validity


def require(condition, message):
    if not condition:
        raise ValueError(message)


def code_part(value, width):
    if isinstance(value, float) and value.is_integer():
        value = int(value)
    text = str(value).strip()
    require(text.isdigit() and len(text) <= width, f'Invalid code segment: {value!r}')
    return text.zfill(width)


def load_codes(path, sheet, sido):
    wb = openpyxl.load_workbook(path, read_only=True, data_only=True)
    try:
        ws = wb[sheet]
        header = next(ws.iter_rows(min_row=2, max_row=2, values_only=True))
        require(tuple(header[:6]) == ('시도코드', '시도명칭', '시군구코드', '시군구명칭', '읍면동코드', '읍면동명칭'),
                'Unexpected code-sheet columns')
        result = {}
        for row_number, row in enumerate(ws.iter_rows(min_row=3, values_only=True), 3):
            if row[1] != sido:
                continue
            require(all(value is not None for value in row[:6]), f'Missing code/name at row {row_number}')
            code = code_part(row[0], 2) + code_part(row[2], 3) + code_part(row[4], 3)
            require(code not in result, f'Duplicate workbook code: {code}')
            result[code] = {'sidonm': row[1], 'sggnm': row[3], 'dongnm': row[5], 'source_row': row_number}
        require(result, f'No rows for {sido}')
        return result
    finally:
        wb.close()


def digest(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def parts(geometry):
    return [geometry] if geometry.geom_type == 'Polygon' else list(geometry.geoms)


def structure(geometry):
    polygons = parts(geometry)
    return {'polygons': len(polygons), 'holes': sum(len(p.interiors) for p in polygons),
            'vertices': len(get_coordinates(geometry))}


def write_json(path, value, pretty=False):
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + '.tmp')
    temporary.write_text(json.dumps(value, ensure_ascii=False, allow_nan=False,
                                   indent=2 if pretty else None,
                                   separators=None if pretty else (',', ':')) + '\n', encoding='utf-8')
    temporary.replace(path)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--shapefile', type=Path, required=True)
    parser.add_argument('--codes', type=Path, required=True)
    parser.add_argument('--sheet', default='2025년 6월')
    parser.add_argument('--sido', required=True)
    parser.add_argument('--output', type=Path, required=True, help='지역 폴더의 boundaries.geojson')
    args = parser.parse_args()
    shp = args.shapefile.resolve()
    source_files = [shp.with_suffix(suffix) for suffix in ('.shp', '.shx', '.dbf', '.prj', '.cpg')]
    require(all(p.is_file() for p in source_files), 'Missing Shapefile component')
    codes = load_codes(args.codes, args.sheet, args.sido)
    prefixes = {code[:2] for code in codes}
    require(len(prefixes) == 1, 'Selected province has multiple census prefixes')
    encoding = shp.with_suffix('.cpg').read_text(encoding='ascii').strip()
    encoding = 'cp949' if encoding == '949' else encoding
    source_crs = CRS.from_wkt(shp.with_suffix('.prj').read_text(encoding='utf-8'))
    to_lonlat = Transformer.from_crs(source_crs, CRS.from_epsg(4326), always_xy=True)
    to_source = Transformer.from_crs(CRS.from_epsg(4326), source_crs, always_xy=True)
    records = {}
    dates = set()
    checks = []
    with shapefile.Reader(str(shp), encoding=encoding) as reader:
        for record in reader.iterRecords():
            attrs = record.as_dict()
            code = str(attrs['ADM_CD']).strip()
            if code[:2] not in prefixes:
                continue
            require(code in codes, f'Boundary code absent from selected sheet: {code}')
            require(code not in records, f'Duplicate boundary code: {code}')
            names = codes[code]
            require(attrs['ADM_NM'].strip() == names['dongnm'], f'Name mismatch for {code}')
            base_date = str(attrs['BASE_DATE']).strip()
            date = datetime.strptime(base_date, '%Y%m%d').date()
            require(re.sub(r'\s+', '', args.sheet) == f'{date.year}년{date.month}월',
                    f'Sheet and boundary dates differ: {args.sheet} / {base_date}')
            dates.add(base_date)
            source_geometry = shape(reader.shape(record.oid).__geo_interface__)
            require(source_geometry.geom_type in ('Polygon', 'MultiPolygon'), f'Non-polygon: {code}')
            require(not source_geometry.is_empty and source_geometry.is_valid,
                    f'Invalid source geometry {code}: {explain_validity(source_geometry)}')
            projected = transform(to_lonlat.transform, source_geometry)
            geometry = MultiPolygon([orient(p, sign=1.0) for p in parts(projected)])
            require(not geometry.is_empty and geometry.is_valid,
                    f'Invalid transformed geometry {code}: {explain_validity(geometry)}')
            before, after = structure(source_geometry), structure(geometry)
            require(before == after, f'Changed polygon/hole/vertex count: {code}')
            xy = get_coordinates(source_geometry)
            lon, lat = to_lonlat.transform(xy[:, 0], xy[:, 1], errcheck=True)
            require(np.isfinite(lon).all() and np.isfinite(lat).all(), f'Non-finite coordinate: {code}')
            require((np.abs(lon) <= 180).all() and (np.abs(lat) <= 90).all(), f'Out-of-range coordinate: {code}')
            x_back, y_back = to_source.transform(lon, lat, errcheck=True)
            max_error = float(np.hypot(x_back - xy[:, 0], y_back - xy[:, 1]).max())
            require(max_error < 0.001, f'Projection round-trip error exceeds 1 mm: {code}')
            full_name = ' '.join(names[key] for key in ('sidonm', 'sggnm', 'dongnm'))
            records[code] = {
                'type': 'Feature', 'id': code, 'bbox': list(geometry.bounds),
                'properties': {'adm_cd': code, 'adm_nm': full_name,
                               'sidonm': names['sidonm'], 'sggnm': names['sggnm'],
                               'dongnm': names['dongnm'], 'base_date': date.isoformat(),
                               'code_system': 'census'},
                'geometry': mapping(geometry),
            }
            checks.append({'adm_cd': code, 'name': full_name, 'workbook_row': names['source_row'],
                           'source_shape_index': record.oid, **after, 'round_trip_max_error_m': max_error})
    require(set(records) == set(codes), f'Missing boundary codes: {sorted(set(codes) - set(records))}')
    require(len(dates) == 1, f'Multiple boundary dates: {dates}')
    features = [records[code] for code in sorted(records)]
    bounds = np.array([feature['bbox'] for feature in features])
    bbox = [float(bounds[:, 0].min()), float(bounds[:, 1].min()),
            float(bounds[:, 2].max()), float(bounds[:, 3].max())]
    collection = {'type': 'FeatureCollection', 'name': args.output.stem, 'bbox': bbox, 'features': features}
    write_json(args.output, collection)
    # Read the actual serialized geometry back once, including GeoJSON ring winding.
    saved = json.loads(args.output.read_text(encoding='utf-8'))
    require(len(saved['features']) == len(codes), 'Serialized feature count changed')
    for feature in saved['features']:
        geometry = shape(feature['geometry'])
        require(geometry.is_valid, f'Invalid serialized geometry: {feature["id"]}')
        require(all(p.exterior.is_ccw and all(not r.is_ccw for r in p.interiors)
                    for p in geometry.geoms), f'Incorrect ring orientation: {feature["id"]}')
    metadata = {
        'generated_at': datetime.now(timezone.utc).isoformat(),
        'output': args.output.name, 'output_sha256': digest(args.output),
        'base_date': features[0]['properties']['base_date'], 'sido': args.sido,
        'feature_count': len(features), 'district_counts': dict(Counter(f['properties']['sggnm'] for f in features)),
        'source_crs': {'authority': source_crs.to_authority(), 'wkt': source_crs.to_wkt()},
        'output_coordinates': 'WGS84 longitude, latitude (GeoJSON / OGC:CRS84 axis order)',
        'encoding': {'source_dbf': encoding, 'output': 'utf-8'},
        'geometry_processing': 'Reproject and orient rings; wrap Polygon as MultiPolygon; no simplification or repair.',
        'code_join': {'sheet': args.sheet, 'key': '시도코드(2) + 시군구코드(3) + 읍면동코드(3) = ADM_CD',
                     'matched': len(features), 'missing': 0, 'duplicate': 0, 'name_mismatch': 0},
        'input_files': [{'path': str(p.resolve()), 'sha256': digest(p)} for p in [*source_files, args.codes]],
        'tool_sha256': digest(Path(__file__)),
        'libraries': {name: importlib.metadata.version(name) for name in ('openpyxl', 'pyshp', 'pyproj', 'shapely', 'numpy')},
        'bbox': bbox, 'geometry_checks': sorted(checks, key=lambda row: row['adm_cd']),
        'validation': {'valid_geometries': len(features), 'preserved_polygons_holes_vertices': True,
                       'geojson_ring_orientation': True,
                       'max_round_trip_error_m': max(row['round_trip_max_error_m'] for row in checks)},
    }
    write_json(args.output.with_suffix('.metadata.json'), metadata, pretty=True)
    print(json.dumps({'output': str(args.output), 'features': len(features), 'bbox': bbox,
                      'valid': len(features), 'district_counts': metadata['district_counts'],
                      'bytes': args.output.stat().st_size}, ensure_ascii=False))


if __name__ == '__main__':
    main()
