"""Stops and runs of one revision, read from its raw files into one form.

Each revision in <region>/transit/revisions.json names its files and their format under `source`. The general format
`visits` is a visit file with one row per stop visit, CSV or Parquet:

  trip_id                   one run; empty for observed records, whose runs are restored
  date, vehicle             service day and vehicle of an observed record
  route_name, route_id      route name (the number with an optional last bracket) and pattern
  stop_id, stop_name        the stop
  stop_lat, stop_lon        optional; a stop the stop file lacks is added from them
  stop_sequence             the stop's order in its pattern
  arrival_time, departure_time
                            'H:MM:SS' from the midnight of the service day, past 24 after midnight; empty if unknown

and a stop file of stop_id, stop_name, stop_lat, stop_lon. `ulsan-planned` and `ulsan-observed` read the raw Parquet of
the Ulsan timetable and of its vehicle terminal records.

A run without a trip_id is restored from one vehicle's records of one pattern in arrival order, cut where the stop order
starts again; a restored run is a round trip, and one whose first record comes after the pattern's first stop is partial.
Times are seconds from the midnight of the service day, so service after midnight counts past 24 hours. A departure that
is missing, a placeholder or earlier than the arrival is -1; the arrival stays as evidence. Visits without an arrival or
without a located stop are left out and counted.
"""
from __future__ import annotations

import re
from dataclasses import dataclass
from datetime import datetime
from pathlib import Path

import numpy as np
import pandas as pd

KR_TIME = re.compile(r'^(\d{4})-(\d{2})-(\d{2}) (오전|오후) (\d{1,2}):(\d{2}):(\d{2})$')
HMS = re.compile(r'^(\d{1,2}):([0-5]\d):([0-5]\d)$')


@dataclass
class Snapshot:
    id: str
    source_file: str
    stops: pd.DataFrame
    """id, name, lat, lon; the row number is the stop index."""
    trips: pd.DataFrame
    """id, route, pattern, vehicle, partial, crosses_midnight, boundary_clipped; the row number is the trip index."""
    events: pd.DataFrame
    """trip, stop, arrival, departure, sequence, row: stop visits in visiting order within each trip."""
    rows: int
    excluded: int
    restored: bool


def hms(value: str) -> int:
    m = HMS.match(str(value).strip())
    return -1 if not m else int(m[1]) * 3600 + int(m[2]) * 60 + int(m[3])


def kr_time(value: str, day: datetime) -> int:
    m = KR_TIME.match(str(value).strip())
    if not m:
        return -1
    hour = int(m[5]) % 12 + (12 if m[4] == '오후' else 0)
    at = datetime(int(m[1]), int(m[2]), int(m[3]), hour, int(m[6]), int(m[7]))
    return int((at - day).total_seconds())


def parsed(values: pd.Series, parse) -> np.ndarray:
    table = {v: parse(v) for v in values.dropna().unique()}
    return values.map(table).fillna(-1).astype('int64').to_numpy()


def table(path: Path) -> pd.DataFrame:
    if path.suffix == '.parquet':
        return pd.read_parquet(path)
    return pd.read_csv(path, dtype=str)


def read_stops(path: Path) -> pd.DataFrame:
    raw = table(path)
    return pd.DataFrame({'id': raw['stop_id'].astype(str).str.replace(r'\.0$', '', regex=True), 'name': raw['stop_name'].astype(str),
                         'lat': pd.to_numeric(raw['stop_lat'], errors='coerce'), 'lon': pd.to_numeric(raw['stop_lon'], errors='coerce')})


def visits(raw: pd.DataFrame) -> pd.DataFrame:
    """The general visit file."""
    empty = pd.Series(np.nan, index=raw.index, dtype=object)
    column = lambda name: raw[name].replace('', np.nan) if name in raw else empty
    return pd.DataFrame({'run': column('trip_id'), 'date': column('date'), 'vehicle': column('vehicle'), 'route': raw['route_name'],
                         'pattern': raw['route_id'].astype(str), 'stop_id': raw['stop_id'].astype(str).str.replace(r'\.0$', '', regex=True),
                         'name': raw['stop_name'], 'lat': pd.to_numeric(column('stop_lat'), errors='coerce'),
                         'lon': pd.to_numeric(column('stop_lon'), errors='coerce'), 'sequence': raw['stop_sequence'].astype(int),
                         'arrival': parsed(column('arrival_time'), hms), 'departure': parsed(column('departure_time'), hms), 'row': raw['row']})


def ulsan_planned(raw: pd.DataFrame) -> pd.DataFrame:
    """The Ulsan timetable: a trip_id is one run, stop_sequence its order, times 'H:MM:SS'."""
    raw = raw.dropna(subset=['trip_id'])
    return pd.DataFrame({'run': raw['trip_id'], 'date': None, 'vehicle': None, 'route': raw['route_name'], 'pattern': raw['route_id'],
                         'stop_id': raw['stop_id'].astype(str), 'name': raw['stop_name'], 'lat': np.nan, 'lon': np.nan,
                         'sequence': raw['stop_sequence'].astype(int), 'arrival': parsed(raw['arrival_time'], hms),
                         'departure': parsed(raw['departure_time'], hms), 'row': raw['row']})


def ulsan_observed(raw: pd.DataFrame) -> pd.DataFrame:
    """One day of Ulsan terminal records: Korean column names read by position, times '2025-05-06 오전 8:02:48'."""
    day = datetime.strptime(raw.iloc[0, 0], '%Y%m%d')
    return pd.DataFrame({'run': None, 'date': raw.iloc[:, 0], 'vehicle': raw['OBE_ID'], 'route': raw['route_name'], 'pattern': raw['route_id'],
                         'stop_id': raw['stop_id'].astype(str).str.replace(r'\.0$', '', regex=True), 'name': raw['stop_name'],
                         'lat': pd.to_numeric(raw.iloc[:, 10], errors='coerce'), 'lon': pd.to_numeric(raw.iloc[:, 11], errors='coerce'),
                         'sequence': raw.iloc[:, 4].astype(int), 'arrival': parsed(raw['arrival_time'], lambda v: kr_time(v, day)),
                         'departure': parsed(raw['departure_time'], lambda v: kr_time(v, day)), 'row': raw['row']})


READERS = {'visits': visits, 'ulsan-planned': ulsan_planned, 'ulsan-observed': ulsan_observed}


def read(transit: Path, revision: dict) -> Snapshot:
    source = revision['source']
    source_file = source['visits']
    raw = table(transit / source_file)
    stops = read_stops(transit / source['stops'])
    rows = len(raw)
    raw = raw.assign(row=np.arange(rows))
    frame = READERS[source['format']](raw)
    named = frame['run'].notna()
    if named.any() and not named.all():
        raise ValueError(f'{source_file}: trip_id is given for some visits only')
    restored = not named.any()
    if restored:
        # One vehicle's records of one pattern in arrival order, cut where the order restarts.
        frame['date'] = frame['date'].fillna('')
        frame = frame.sort_values(['date', 'vehicle', 'pattern', 'arrival', 'sequence'], kind='mergesort')
        group = [frame['date'], frame['vehicle'], frame['pattern']]
        previous = frame.groupby(group, sort=False)['sequence'].shift()
        lap = (previous.isna() | (frame['sequence'] <= previous)).groupby(group).cumsum()
        frame['run'] = frame['pattern'] + ':' + frame['vehicle'] + ':run' + lap.astype(str)
        if frame['date'].nunique() > 1:
            frame['run'] = frame['date'] + ':' + frame['run']
    # A stop the stop file lacks is added from the record itself when the record carries a position.
    known = set(stops['id'])
    extra = frame.loc[~frame['stop_id'].isin(known) & frame['lat'].notna() & frame['lon'].notna()].drop_duplicates('stop_id')
    stops = pd.concat([stops, pd.DataFrame({'id': extra['stop_id'], 'name': extra['name'].astype(str), 'lat': extra['lat'], 'lon': extra['lon']})],
                      ignore_index=True)
    index = pd.Series(np.arange(len(stops)), index=stops['id'])
    frame['stop'] = frame['stop_id'].map(index)
    located = stops['lat'].notna() & stops['lon'].notna()
    keep = frame['stop'].notna() & (frame['arrival'] >= 0)
    keep &= located.reindex(frame['stop'].fillna(-1).astype(int)).fillna(False).to_numpy()
    excluded = rows - int(keep.sum())
    frame = frame.loc[keep].copy()
    frame['stop'] = frame['stop'].astype(int)
    if not restored:
        frame = frame.sort_values(['run', 'sequence'], kind='mergesort')
    frame.loc[frame['departure'] < frame['arrival'], 'departure'] = -1
    runs = frame.drop_duplicates('run')
    trips = pd.DataFrame({'id': runs['run'].to_numpy(), 'route': runs['route'].fillna('').astype(str).to_numpy(),
                          'pattern': runs['pattern'].astype(str).to_numpy(), 'vehicle': runs['vehicle'].to_numpy()})
    trip_index = pd.Series(np.arange(len(trips)), index=trips['id'])
    frame['trip'] = frame['run'].map(trip_index)
    frame = frame.sort_values('trip', kind='mergesort')
    events = frame[['trip', 'stop', 'arrival', 'departure', 'sequence', 'row']].reset_index(drop=True)
    first = events.groupby('trip').first()
    last = events.groupby('trip').last()
    trips['partial'] = (first['sequence'] > events['sequence'].min()).to_numpy() if restored else False
    trips['crosses_midnight'] = (last['arrival'] >= 86400).to_numpy()
    clipped = tuple(source.get('boundaryClipped') or ())
    trips['boundary_clipped'] = trips['pattern'].str.startswith(clipped) if clipped else False
    return Snapshot(revision['id'], source_file, stops, trips, events, rows, excluded, restored)
