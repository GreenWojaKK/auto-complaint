package journey

import "math"

// Meters는 두 지점 사이의 대원 거리(m)다.
func Meters(a, b Point) float64 {
	p := math.Pi / 180
	dLat, dLon := (b[0]-a[0])*p, (b[1]-a[1])*p
	sLat, sLon := math.Sin(dLat/2), math.Sin(dLon/2)
	h := sLat*sLat + math.Cos(a[0]*p)*math.Cos(b[0]*p)*sLon*sLon
	return 6371000 * 2 * math.Asin(math.Min(1, math.Sqrt(h)))
}

type arc struct {
	to     int
	meters float64
}

// Adjacency는 보행망의 노드별 나가는 간선이다. 간선은 저장된 순서를 지킨다.
func Adjacency(s *Street) [][]arc {
	out := make([][]arc, len(s.Nodes))
	for _, e := range s.Edges {
		out[e.From] = append(out[e.From], arc{e.To, e.Meters})
	}
	return out
}

func reverse(adj [][]arc) [][]arc {
	out := make([][]arc, len(adj))
	for u, edges := range adj {
		for _, e := range edges {
			out[e.to] = append(out[e.to], arc{u, e.meters})
		}
	}
	return out
}

// 최소 힙. container/heap 대신 직접 둔 것은 같은 거리에서 꺼내는 순서를 router.ts의 MinHeap과 맞추기 위해서다.
type entry struct {
	key   float64
	value int
}
type minHeap []entry

func (h minHeap) Len() int { return len(h) }
func (h *minHeap) push(key float64, value int) {
	row := entry{key, value}
	i := len(*h)
	*h = append(*h, row)
	for i > 0 {
		parent := (i - 1) >> 1
		if (*h)[parent].key <= key {
			break
		}
		(*h)[i] = (*h)[parent]
		i = parent
	}
	(*h)[i] = row
}
func (h *minHeap) pop() entry {
	rows := *h
	first, last := rows[0], rows[len(rows)-1]
	rows = rows[:len(rows)-1]
	if len(rows) > 0 {
		i := 0
		for i*2+1 < len(rows) {
			c := i*2 + 1
			if c+1 < len(rows) && rows[c+1].key < rows[c].key {
				c++
			}
			if rows[c].key >= last.key {
				break
			}
			rows[i] = rows[c]
			i = c
		}
		rows[i] = last
	}
	*h = rows
	return first
}

// Walk는 한 노드에서 limit 미터 안에 닿는 노드까지의 거리와 직전 노드다.
type Walk struct {
	Distance map[int]float64
	Previous map[int]int
}

func WalkDistances(adj [][]arc, start int, limit float64) Walk {
	w := Walk{map[int]float64{}, map[int]int{}}
	if start < 0 {
		return w
	}
	q := &minHeap{}
	w.Distance[start] = 0
	q.push(0, start)
	for q.Len() > 0 {
		top := q.pop()
		d, u := top.key, top.value
		if d != w.Distance[u] {
			continue
		}
		for _, e := range adj[u] {
			next := d + e.meters
			old, seen := w.Distance[e.to]
			if next > limit || (seen && next >= old) {
				continue
			}
			w.Distance[e.to] = next
			w.Previous[e.to] = u
			q.push(next, e.to)
		}
	}
	return w
}

// WalkGeometry는 start에서 end까지 보행 경로의 좌표다. 이어지지 않으면 빈 목록이다.
func WalkGeometry(s *Street, previous map[int]int, start, end int) []Point {
	ids := []int{end}
	for v := end; v != start; {
		p, ok := previous[v]
		if !ok {
			return []Point{}
		}
		ids = append(ids, p)
		v = p
	}
	out := make([]Point, 0, len(ids))
	for i := len(ids) - 1; i >= 0; i-- {
		n := s.Nodes[ids[i]]
		out = append(out, Point{n.Lat, n.Lon})
	}
	return out
}

// NearestNode는 maxDistance 안에서 가장 가까운 OSM 노드다(정류장 연결용 노드는 제외한다).
func NearestNode(s *Street, p Point, maxDistance float64) (int, float64) {
	best, dist := -1, maxDistance
	for i, n := range s.Nodes {
		if n.OsmID == 0 {
			continue
		}
		if d := Meters(p, Point{n.Lat, n.Lon}); d < dist {
			dist, best = d, i
		}
	}
	return best, dist
}
