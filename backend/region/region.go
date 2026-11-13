// Package region은 지역 폴더의 region.json을 읽는다. 지역 이름, 교통 자료를 실은 database, 지도 검색 범위, 요금 구분처럼
// 코드가 지역마다 달리 쓰는 값을 둔다. 지역 폴더의 위치는 repo.RegionDir가 정한다.
//
// 지역 폴더의 구성은 다음과 같다.
//
//	region.json              이 파일
//	prior.md                 계획·답 쓰기 모델에 주는 지역 지식
//	priors/areas.json        여러 행정동을 묶어 부르는 지칭("시내")
//	complaints/<번호>.md      민원 원문, complaints/dates.json은 접수일
//	transit/revisions.json   자료 판과 판마다의 원천 파일, transit/lineage.json은 노선의 이전 번호
//	streets/                 보행망 원본과 출처
//	boundaries.geojson       행정동 경계(properties의 sggnm·dongnm)
//	build/                   전처리 결과(연결망, 장소 목록, 지도 배경)
package region

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"dllio/backend/repo"
)

// Fare는 노선 번호 형식으로 정하는 요금 구분이다.
type Fare struct {
	Pattern string `json:"pattern"`
	Label   string `json:"label"`
}

// Region은 지역 폴더와 그 설정이다.
type Region struct {
	Dir string `json:"-"`
	// Name은 지역 이름이다. 지칭 앞에 붙은 이 이름은 떼고 읽는다("울산 동구" → "동구").
	Name string `json:"name"`
	// Aliases는 지역 전체를 가리키는 다른 이름이다. 지역 이름과 함께 지도에서 찾지 않는다.
	Aliases []string `json:"aliases"`
	// Database는 이 지역의 교통 자료를 실은 PostgreSQL database다.
	Database string `json:"database"`
	// MapBox는 VWorld 장소 검색 범위(최소 경도, 최소 위도, 최대 경도, 최대 위도)다. 비우면 지도 검색을 하지 않는다.
	MapBox string `json:"mapBox"`
	// Fares는 노선 번호가 형식에 맞으면 붙이는 요금 구분이고, FareOther는 어느 형식에도 맞지 않는 노선의 구분이다.
	Fares     []Fare `json:"fares"`
	FareOther string `json:"fareOther"`
}

// Load는 지역 폴더(repo.RegionDir)의 region.json을 읽는다.
func Load(root string) (*Region, error) {
	return LoadDir(repo.RegionDir(root))
}

// LoadDir는 주어진 지역 폴더의 region.json을 읽는다.
func LoadDir(dir string) (*Region, error) {
	text, err := repo.ReadText(filepath.Join(dir, "region.json"))
	if err != nil {
		return nil, fmt.Errorf("지역 설정을 읽지 못했다: %w", err)
	}
	r := &Region{Dir: dir}
	if err := json.Unmarshal([]byte(text), r); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, "region.json"), err)
	}
	if r.Database == "" {
		return nil, fmt.Errorf("%s에 database가 없다", filepath.Join(dir, "region.json"))
	}
	return r, nil
}

// Path는 지역 폴더 안의 경로다.
func (r *Region) Path(elem ...string) string {
	return filepath.Join(append([]string{r.Dir}, elem...)...)
}

// Names는 지역 이름과 다른 이름이다.
func (r *Region) Names() []string {
	if r == nil || r.Name == "" {
		return nil
	}
	return append([]string{r.Name}, r.Aliases...)
}

// Outside는 경계 밖 지점의 행정동 표기다.
func (r *Region) Outside() string {
	if r == nil || r.Name == "" {
		return "경계 밖"
	}
	return r.Name + " 밖"
}

// DSN은 조회 계정(dllio_read)으로 이 지역 database에 접속하는 주소다. 환경 변수 DLLIO_READ_DSN이 있으면 그것을 쓴다.
func (r *Region) DSN() string {
	if dsn := strings.TrimSpace(os.Getenv("DLLIO_READ_DSN")); dsn != "" {
		return dsn
	}
	return "postgresql://dllio_read@localhost:5432/" + r.Database
}

// FareSQL은 노선 번호 열(column)에서 요금 구분을 고르는 SQL 식이다.
func (r *Region) FareSQL(column string) string {
	other := "일반"
	if r != nil && r.FareOther != "" {
		other = r.FareOther
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	if r == nil || len(r.Fares) == 0 {
		return quote(other)
	}
	cases := []string{}
	for _, f := range r.Fares {
		cases = append(cases, "WHEN "+column+" ~ "+quote(f.Pattern)+" THEN "+quote(f.Label))
	}
	return "CASE " + strings.Join(cases, " ") + " ELSE " + quote(other) + " END"
}
