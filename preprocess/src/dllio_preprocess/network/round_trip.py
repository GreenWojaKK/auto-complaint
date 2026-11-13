"""같은 노선 번호의 운행 기록이 서로 어떤 관계인지 정류장 순서에서 계산한다.

번호마다 정류장을 가장 많이 지난 온전한 기록을 기준 기록으로 두고, 기록마다 기준 기록의 어디에서 어디까지 순서대로 겹치는지 구한다.
같은 정류장 이름을 네 번째 방문마다 한 번 이상 다시 지나면 왕복 기록이고, 앞쪽과 뒤쪽에 함께 나오는 정류장 이름이 가장 많아지는 곳이 되돌아오는 지점이다.
그렇지 않으면 편도 기록이므로 그 역순을 이어 왕복으로 보고, 편도의 끝이 되돌아오는 지점이다. 양방향 정류장 이름은 서로 조금씩
다르므로, 편도 기록과 시작·끝이 뒤바뀐 기록은 겹치는 비율과 상관없이 올 때 반쪽으로 본다.

- reference_start: 기록의 첫 겹침이 기준 기록의 몇 번째 정류장인지(0부터). 같은 정류장을 왕복으로 두 번 지나면 뒤쪽을 고른다.
- reference_share: 기록의 정류장 중 기준 기록과 순서대로 겹치는 비율.
- shape: 기록의 모양. loop는 되돌아오는 지점을 지나는 왕복 운행(앞 정류장을 놓친 기록 포함), out은 되돌아오는 지점까지만 가는 갈 때 반쪽,
  back은 되돌아오는 지점에서 시작하는 올 때 반쪽, variant는 겹치는 비율이 CONTAINED 미만이라 기준 기록에 들어가지 않는 변형 운행이다.
"""
from __future__ import annotations

import re
from typing import Callable

CONTAINED = 0.9


def clean(name: str) -> str:
    """괄호 내용, 공백, 끝의 기점·종점을 뺀 정류장 이름. '율리공영차고지종점'과 '율리공영차고지'는 같다."""
    name = re.sub(r'\s', '', re.sub(r'\([^)]*\)', '', name))
    found = re.match(r'^(.{2,})(기점|종점)$', name)
    return found[1] if found else name


def fold(names: list[str]) -> int:
    """앞쪽과 뒤쪽에 함께 나오는 정류장 이름이 가장 많아지는 위치. 갈 때와 올 때 정류장이 조금 달라도 찾는다. 같은 값이 이어지면 그 가운데다."""
    shared = [len(set(names[:i]) & set(names[i + 1:])) for i in range(len(names))]
    best = [i for i, value in enumerate(shared) if value == max(shared)]
    return best[(len(best) - 1) // 2]


def align(record: tuple[str, ...], reference: tuple[str, ...]) -> tuple[int, int, int]:
    """(순서대로 겹치는 정류장 수, 첫 겹침 위치, 마지막 겹침 위치). 겹침 수가 같으면 가장 뒤에서 시작하는 위치를 고른다."""
    n, m = len(record), len(reference)
    longest = [[0] * (m + 1) for _ in range(n + 1)]
    for i in range(n - 1, -1, -1):
        for j in range(m - 1, -1, -1):
            longest[i][j] = longest[i + 1][j + 1] + 1 if record[i] == reference[j] else max(longest[i + 1][j], longest[i][j + 1])
    total = longest[0][0]
    if total == 0:
        return 0, 0, 0
    first = next(i for i in range(n) if longest[i][0] > longest[i + 1][0])
    start = max(j for j in range(m) if record[first] == reference[j] and longest[first + 1][j + 1] + 1 == longest[first][0])
    i, j, end = first, start, start
    while i < n and j < m and longest[i][j] > 0:
        if record[i] == reference[j] and longest[i][j] == longest[i + 1][j + 1] + 1:
            end = j
            i, j = i + 1, j + 1
        elif longest[i + 1][j] >= longest[i][j + 1]:
            i += 1
        else:
            j += 1
    return total, start, end


def route_shape(reference: list[int], stops: list[dict]) -> tuple[tuple[str, ...], int, bool]:
    """(왕복으로 편 기준 기록의 이름, 되돌아오는 지점의 첫 위치, 기준 기록이 편도인지)."""
    names = [clean(stops[s]['name']) for s in reference]
    if (len(names) - len(set(names))) * 4 < len(names):
        return tuple(names + names[-2::-1]), names.index(names[-1]), True
    return tuple(names), fold(names), False


def same_end(a: str, b: str) -> bool:
    """끝 정류장 이름이 같거나 한쪽이 다른 쪽을 포함한다('테크노파크'와 '울산테크노파크')."""
    return a in b or b in a


def shape_of(share: float, start: int, end: int, turn: int, opposite: bool) -> str:
    if opposite:
        return 'back'
    if share < CONTAINED:
        return 'variant'
    if start >= turn and start > 0:
        return 'back'
    return 'out' if end <= turn + 1 else 'loop'


def relate(trips: list[dict], stops: list[dict], number_of: Callable[[str], str]) -> dict[str, dict]:
    """운행 기록 id마다 기준 기록과의 관계."""
    groups: dict[str, list[tuple[dict, list[int]]]] = {}
    for trip in trips:
        groups.setdefault(number_of(trip['route']), []).append((trip, trip['events'][::5]))
    facts = {}
    for records in groups.values():
        reference, path = max(records, key=lambda r: (not r[0].get('partial'), len(r[1])))
        names, turn, one_way = route_shape(path, stops)
        head, tail = clean(stops[path[0]]['name']), clean(stops[path[-1]]['name'])
        seen: dict[tuple[str, ...], tuple[int, int, int]] = {}
        for trip, visited in records:
            sequence = tuple(clean(stops[s]['name']) for s in visited)
            if sequence not in seen:
                seen[sequence] = align(sequence, names)
            matched, start, end = seen[sequence]
            share = matched / len(sequence)
            opposite = one_way and same_end(sequence[0], tail) and same_end(sequence[-1], head)
            facts[trip['id']] = {'reference_trip': reference['id'], 'reference_start': start, 'reference_share': round(share, 4),
                                 'shape': shape_of(share, start, end, turn, opposite)}
    return facts
