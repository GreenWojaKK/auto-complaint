// Package journey는 한 자료 판의 정류장·운행·보행망을 메모리에 두고 여정을 탐색한다.
// 자료는 PostgreSQL에서 읽는다(store 패키지).
package journey

import "sync"

// Point는 [위도, 경도]다.
type Point [2]float64

type Stop struct {
	ID         string
	Name       string
	Lat, Lon   float64
	Place      int // network_place 순번
	WalkNode   int // 보행망 노드 번호, 연결되지 않았으면 -1
	SnapMeters *float64
}

// Trip의 Events는 방문마다 [정류장 순번, 도착, 출발, 원본 순번, 원본 행] 다섯 값을 이어 붙인 것이다. 모르는 시각은 -1.
type Trip struct {
	ID          string
	Route       string
	RouteNumber string
	Pattern     string
	Vehicle     string
	Events      []int
}

type Place struct {
	Name            string
	Degree          int
	DominanceRadius float64
}

type Link struct {
	Stop   int
	Meters float64
}

type Node struct {
	Lat, Lon float64
	OsmID    int64
}

type Edge struct {
	From, To int
	Meters   float64
}

type Street struct {
	Hash      string
	URL       string
	FetchedAt string
	Nodes     []Node
	Edges     []Edge
}

// Network는 자료 판 하나다. Snapshot은 판 ID(before, after)다.
type Network struct {
	Snapshot      string
	Label         string
	Basis         string // planned 또는 observed
	Date          string
	SourceFile    string
	SourceHash    string
	Version       string
	MaxWalkMeters int
	Notes         []string
	Stops         []Stop
	Trips         []Trip
	Places        []Place
	Walking       [][]Link
	Street        Street

	once      sync.Once // 보행망 인접 목록은 처음 탐색할 때 한 번 만든다.
	adj, radj [][]arc
}
