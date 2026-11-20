"""Walking over the OpenStreetMap streets: the street graph, stops connected to it and the stops each stop reaches on foot.

Ways closed to pedestrians, private access and motorways without a footway are left out; one-way walking rules and the
nodes where ways meet are kept. A stop is connected to the nearest street segment within 35 m: a node at its position,
a node at its projection on the segment, and the connector between them, whose length counts as walking. Projections
on one segment are linked to each other along it, in the directions the segment allows.
"""
from __future__ import annotations

import gzip
import hashlib
import json
import math
from pathlib import Path

import numpy as np
import pandas as pd
from scipy.sparse import csr_matrix
from scipy.sparse.csgraph import dijkstra

SNAP_METERS = 35
WALK_METERS = 1800
CELL = 0.001
CLOSED_HIGHWAYS = {'motorway', 'motorway_link', 'trunk', 'trunk_link', 'construction', 'proposed', 'raceway', 'cycleway'}
OPEN = {'yes', 'designated', 'permissive'}


def meters(a: tuple[float, float], b: tuple[float, float]) -> float:
    p = math.pi / 180
    d_lat, d_lon = (b[0] - a[0]) * p, (b[1] - a[1]) * p
    h = math.sin(d_lat / 2) ** 2 + math.cos(a[0] * p) * math.cos(b[0] * p) * math.sin(d_lon / 2) ** 2
    return 6371000 * 2 * math.asin(min(1, math.sqrt(h)))


def street(path: Path, source: dict) -> dict:
    """The walkable street graph: nodes [lat, lon, osm id] and edges flattened as from, to, metres, way id."""
    raw = path.read_bytes()
    elements = json.loads(gzip.decompress(raw))['elements']
    osm = {e['id']: e for e in elements if e['type'] == 'node'}
    nodes: list[list[float]] = []
    edges: list[float] = []
    index: dict[int, int] = {}

    def node(i: int) -> int:
        if i not in index:
            n = osm.get(i)
            if not n or 'lat' not in n or 'lon' not in n:
                return -1
            index[i] = len(nodes)
            nodes.append([n['lat'], n['lon'], i])
        return index[i]

    def blocked(i: int) -> bool:
        tag = osm.get(i, {}).get('tags')
        return bool(tag) and (tag.get('foot') in ('no', 'private') or tag.get('barrier') in ('wall', 'fence'))

    for way in elements:
        t = way.get('tags') or {}
        if way['type'] != 'way' or not t.get('highway') or not way.get('nodes'):
            continue
        if t['highway'] in CLOSED_HIGHWAYS and t.get('foot') not in OPEN:
            continue
        if t.get('foot') in ('no', 'private') or (t.get('access') in ('no', 'private') and t.get('foot') not in OPEN) or 'foot:conditional' in t:
            continue
        ids = way['nodes']
        for left, right in zip(ids, ids[1:]):
            a, b = node(left), node(right)
            if a < 0 or b < 0 or blocked(left) or blocked(right):
                continue
            d = meters(nodes[a][:2], nodes[b][:2])
            if t.get('oneway:foot') != '-1':
                edges += [a, b, d, way['id']]
            if t.get('oneway:foot') != 'yes':
                edges += [b, a, d, way['id']]
    return {'nodes': nodes, 'edges': edges, 'source': {'url': source['url'], 'fetchedAt': source['fetchedAt'], 'license': source['license']},
            'hash': hashlib.sha256(raw).hexdigest()}


def connect(graph: dict, stops: pd.DataFrame) -> tuple[list[int], list[float | None]]:
    """Adds each stop to the graph at the nearest segment; returns each stop's node (-1 if none) and connector length."""
    nodes, edges = graph['nodes'], graph['edges']
    buckets: dict[tuple[int, int], list[int]] = {}
    directions = {(edges[i], edges[i + 1], edges[i + 3]) for i in range(0, len(edges), 4)}
    for i in range(0, len(edges), 4):
        a, b = nodes[int(edges[i])], nodes[int(edges[i + 1])]
        for y in range(math.floor(min(a[0], b[0]) / CELL), math.floor(max(a[0], b[0]) / CELL) + 1):
            for x in range(math.floor(min(a[1], b[1]) / CELL), math.floor(max(a[1], b[1]) / CELL) + 1):
                buckets.setdefault((y, x), []).append(i)
    projections: dict[tuple[int, int, int], dict] = {}
    walk_node, snap = [-1] * len(stops), [None] * len(stops)
    for k, (lat, lon) in enumerate(zip(stops['lat'].to_numpy(float), stops['lon'].to_numpy(float))):
        if not (math.isfinite(lat) and math.isfinite(lon)):
            continue
        y, x, cos = math.floor(lat / CELL), math.floor(lon / CELL), math.cos(lat * math.pi / 180)
        candidates = {e for dy in (-1, 0, 1) for dx in (-1, 0, 1) for e in buckets.get((y + dy, x + dx), [])}
        best = None
        for e in sorted(candidates):
            a, b = nodes[int(edges[e])], nodes[int(edges[e + 1])]
            ax, ay, bx, by = (a[1] - lon) * cos, a[0] - lat, (b[1] - lon) * cos, b[0] - lat
            dx, dy = bx - ax, by - ay
            t = max(0.0, min(1.0, -(ax * dx + ay * dy) / ((dx * dx + dy * dy) or 1)))
            p = (a[0] + (b[0] - a[0]) * t, a[1] + (b[1] - a[1]) * t)
            d = meters((lat, lon), p)
            if d <= SNAP_METERS and (best is None or d < best[2]):
                best = (e, t, d, p)
        if best is None:
            continue
        e, t, d, p = best
        a, b, length, way = int(edges[e]), int(edges[e + 1]), edges[e + 2], int(edges[e + 3])
        on = len(nodes)
        nodes.append([p[0], p[1], 0])
        at = len(nodes)
        nodes.append([lat, lon, 0])
        walk_node[k], snap[k] = at, d
        edges += [at, on, d, 0, on, at, d, 0]
        low, high = min(a, b), max(a, b)
        group = projections.setdefault((low, high, way), {'length': length, 'points': [(0.0, low), (1.0, high)]})
        group['points'].append((t if a == low else 1 - t, on))
    for (low, high, way), g in projections.items():
        points = sorted(g['points'], key=lambda v: v[0])
        for (ta, na), (tb, nb) in zip(points, points[1:]):
            d = (tb - ta) * g['length']
            if (low, high, way) in directions:
                edges += [na, nb, d, way]
            if (high, low, way) in directions:
                edges += [nb, na, d, way]
    return walk_node, snap


def links(graph: dict, walk_node: list[int]) -> list[list[dict]]:
    """For each stop, itself and every stop within the walking limit, nearest first, in whole metres."""
    e = np.asarray(graph['edges'], dtype=float).reshape(-1, 4)
    frame = pd.DataFrame({'a': e[:, 0].astype(int), 'b': e[:, 1].astype(int), 'd': np.maximum(e[:, 2], 1e-9)})
    frame = frame.groupby(['a', 'b'], as_index=False)['d'].min()
    n = len(graph['nodes'])
    matrix = csr_matrix((frame['d'], (frame['a'], frame['b'])), shape=(n, n))
    stop_of = {node: i for i, node in enumerate(walk_node) if node >= 0}
    targets = np.array(sorted(stop_of))
    out = [[{'stop': i, 'meters': 0}] for i in range(len(walk_node))]
    sources = [i for i, node in enumerate(walk_node) if node >= 0]
    for start in range(0, len(sources), 32):
        chunk = sources[start:start + 32]
        distance = dijkstra(matrix, directed=True, indices=[walk_node[i] for i in chunk], limit=WALK_METERS)[:, targets]
        for row, i in zip(distance, chunk):
            found = [(math.ceil(round(d, 6)), stop_of[int(node)]) for node, d in zip(targets, row) if np.isfinite(d) and stop_of[int(node)] != i]
            out[i] += [{'stop': s, 'meters': m} for m, s in sorted(found)]
    return out
