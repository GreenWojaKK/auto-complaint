"""Stops and runs of one snapshot, read from the raw Parquet files into one form.

Before is the provided timetable: a trip_id is one run, stop_sequence its order, and times are 'H:MM:SS' that pass 24
for service after midnight. After is one day of vehicle terminal records. Its rows are fragmented, so a run is restored
from one vehicle's records of one pattern in arrival order, cut where the stop order starts again; a restored run is a
round trip. Times are seconds from the midnight of the service day, so service after midnight counts past 24 hours. A
departure that is missing, a placeholder or earlier than the arrival is -1; the arrival stays as evidence.
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
# Patterns of the Yangsan routes, recorded only up to the city boundary.
BOUNDARY_CLIPPED = ('388',)


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


def read_stops(path: Path) -> pd.DataFrame:
    raw = pd.read_parquet(path)
    return pd.DataFrame({'id': raw['stop_id'].astype(str).str.replace(r'\.0$', '', regex=True), 'name': raw['stop_name'].astype(str),
                         'lat': pd.to_numeric(raw['stop_lat'], errors='coerce'), 'lon': pd.to_numeric(raw['stop_lon'], errors='coerce')})


def read(data: Path, snapshot: str) -> Snapshot:
    source_file = f'ulsan_route_schedule_{snapshot}.parquet'
    raw = pd.read_parquet(data / source_file)
    stops = read_stops(data / f'ulsan_stops_{snapshot}.parquet')
    rows = len(raw)
    raw = raw.assign(row=np.arange(rows))
    if snapshot == 'before':
        raw = raw.dropna(subset=['trip_id'])
        frame = pd.DataFrame({'run': raw['trip_id'], 'route': raw['route_name'], 'pattern': raw['route_id'], 'vehicle': None,
                              'stop_id': raw['stop_id'].astype(str), 'name': raw['stop_name'], 'lat': np.nan, 'lon': np.nan,
                              'sequence': raw['stop_sequence'].astype(int), 'arrival': parsed(raw['arrival_time'], hms),
                              'departure': parsed(raw['departure_time'], hms), 'row': raw['row']})
        order = ['run', 'sequence']
    else:
        day = datetime.strptime(raw.iloc[0, 0], '%Y%m%d')
        frame = pd.DataFrame({'date': raw.iloc[:, 0], 'vehicle': raw['OBE_ID'], 'route': raw['route_name'], 'pattern': raw['route_id'],
                              'stop_id': raw['stop_id'].astype(str).str.replace(r'\.0$', '', regex=True), 'name': raw['stop_name'],
                              'lat': pd.to_numeric(raw.iloc[:, 10], errors='coerce'), 'lon': pd.to_numeric(raw.iloc[:, 11], errors='coerce'),
                              'sequence': raw.iloc[:, 4].astype(int), 'arrival': parsed(raw['arrival_time'], lambda v: kr_time(v, day)),
                              'departure': parsed(raw['departure_time'], lambda v: kr_time(v, day)), 'row': raw['row']})
        # Restored as in BusToSubway s01: one vehicle's records of one pattern in arrival order, cut where the order restarts.
        frame = frame.sort_values(['date', 'vehicle', 'pattern', 'arrival', 'sequence'], kind='mergesort')
        group = [frame['date'], frame['vehicle'], frame['pattern']]
        previous = frame.groupby(group, sort=False)['sequence'].shift()
        lap = (previous.isna() | (frame['sequence'] <= previous)).groupby(group).cumsum()
        frame['run'] = frame['pattern'] + ':' + frame['vehicle'] + ':run' + lap.astype(str)
        order = None
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
    if order:
        frame = frame.sort_values(order, kind='mergesort')
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
    trips['partial'] = (first['sequence'] > events['sequence'].min()).to_numpy() if snapshot == 'after' else False
    trips['crosses_midnight'] = (last['arrival'] >= 86400).to_numpy()
    trips['boundary_clipped'] = trips['pattern'].str.startswith(BOUNDARY_CLIPPED) if snapshot == 'after' else False
    return Snapshot(snapshot, source_file, stops, trips, events, rows, excluded)
