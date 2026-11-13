"""Stops grouped into places, as in BusToSubway s02: the name is the blocking key, not the merge key.

Stops whose cleaned names are equal and that lie within 150 m of each other, directly or through a chain of such
stops, are one place; stops of different names are never merged. A place's degree is the number of places next to it
on some pattern, and its dominance radius the number of steps over which no neighbour has a larger degree.
"""
from __future__ import annotations

import re

import numpy as np
import pandas as pd

LINKAGE_METERS = 150.0


def clean(name: str) -> str:
    """The name without bracketed parts, a trailing comma clause or spaces."""
    return re.sub(r'\s', '', re.sub(r'\([^)]*\)|,[^,]*$', '', name))


def meters(lat1, lon1, lat2, lon2):
    p = np.pi / 180
    a = np.sin((lat2 - lat1) * p / 2) ** 2 + np.cos(lat1 * p) * np.cos(lat2 * p) * np.sin((lon2 - lon1) * p / 2) ** 2
    return 6371000 * 2 * np.arcsin(np.minimum(1, np.sqrt(a)))


def group(stops: pd.DataFrame) -> np.ndarray:
    """The place index of every stop; places are numbered in the order of their first stop."""
    parent = np.arange(len(stops))

    def find(i: int) -> int:
        while parent[i] != i:
            parent[i] = parent[parent[i]]
            i = parent[i]
        return i

    lat, lon = stops['lat'].to_numpy(float), stops['lon'].to_numpy(float)
    for members in stops.groupby(stops['name'].map(clean)).indices.values():
        if len(members) < 2:
            continue
        d = meters(lat[members][:, None], lon[members][:, None], lat[members][None, :], lon[members][None, :])
        for a, b in zip(*np.nonzero(np.triu(d <= LINKAGE_METERS, k=1))):
            ra, rb = find(members[a]), find(members[b])
            if ra != rb:
                parent[max(ra, rb)] = min(ra, rb)
    roots = np.array([find(i) for i in range(len(stops))])
    _, place = np.unique(roots, return_inverse=True)
    return place


def describe(stops: pd.DataFrame, place: np.ndarray, orders: dict[str, list[int | None]]) -> list[dict]:
    """Places with their stops, degree and dominance radius; `orders` are the stop orders of the patterns."""
    out = [{'name': '', 'stops': [], 'degree': 0, 'dominanceRadius': 0, 'wholeComponent': False} for _ in range(place.max() + 1)]
    for i, p in enumerate(place):
        if not out[p]['stops']:
            out[p]['name'] = stops['name'].iat[i]
        out[p]['stops'].append(i)
    graph = [set() for _ in out]
    for order in orders.values():
        for left, right in zip(order, order[1:]):
            if left is None or right is None:
                continue
            a, b = place[left], place[right]
            if a != b:
                graph[a].add(b)
                graph[b].add(a)
    for i, p in enumerate(out):
        p['degree'] = len(graph[i])
    for i, p in enumerate(out):
        seen, frontier, radius = {i}, [i], 0
        while frontier:
            step = sorted({m for n in frontier for m in graph[n] if m not in seen})
            if not step:
                p['wholeComponent'] = True
                break
            if any(out[n]['degree'] > p['degree'] for n in step):
                break
            radius += 1
            seen.update(step)
            frontier = step
        p['dominanceRadius'] = radius
    return out
