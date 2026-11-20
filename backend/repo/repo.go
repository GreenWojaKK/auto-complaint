// Package repo는 저장소와 산출물 폴더, 지역 폴더의 경로와 민원 원문을 읽는 규칙을 둔다.
package repo

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// DefaultRegion은 DLLIO_REGION이 없을 때 쓰는 지역 폴더다. Git에서 제외된다.
const DefaultRegion = "private/ulsan"

// RegionDir는 지역 폴더다. 민원 원문, 지역 사전, 교통 자료 판과 원천 자료, 행정동 경계, 전처리 결과를 둔다.
// 환경 변수 DLLIO_REGION(저장소 루트 기준 상대 경로 또는 절대 경로)이 가리키고, 없으면 DefaultRegion이다.
func RegionDir(root string) string {
	dir := strings.TrimSpace(os.Getenv("DLLIO_REGION"))
	if dir == "" {
		dir = DefaultRegion
	}
	if filepath.IsAbs(dir) {
		return dir
	}
	return filepath.Join(root, dir)
}

// Root는 작업 폴더에서 위로 올라가며 go.mod가 있는 저장소 루트를 찾는다.
func Root() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("저장소 루트(go.mod)를 찾지 못했다")
		}
		dir = parent
	}
}

// Artifacts는 실행 결과와 호출 기록을 두는 저장소 밖 폴더다.
func Artifacts(root string) string {
	if dir := os.Getenv("DLLIO_ARTIFACT_DIR"); dir != "" {
		return dir
	}
	return filepath.Join(filepath.Dir(root), "Dllio-artifact")
}

// ReadText는 파일을 읽어 줄바꿈을 \n으로 맞춘다.
func ReadText(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(string(data)), nil
}

var heading = regexp.MustCompile(`^# [^\n]+\n`)

// SourceText는 민원 원문에서 맨 위 제목 줄을 뺀 글이다.
func SourceText(root, id string) (string, error) {
	text, err := ReadText(filepath.Join(RegionDir(root), "complaints", id+".md"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(heading.ReplaceAllString(text, "")), nil
}

// Stems는 폴더 안 JSON 파일의 확장자를 뺀 이름을 순서대로 돌려준다.
func Stems(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			out = append(out, strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	sort.Strings(out)
	return out, nil
}

// ForEach는 항목마다 work를 동시에 최대 workers개 실행한다. 모두 끝난 뒤 처음 나온 실패를 돌려준다.
func ForEach(items []string, workers int, work func(string) error) error {
	slots := make(chan struct{}, max(workers, 1))
	failures := make(chan error, len(items))
	for _, item := range items {
		go func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			failures <- work(item)
		}()
	}
	var first error
	for range items {
		if err := <-failures; err != nil && first == nil {
			first = err
		}
	}
	return first
}

var caseFile = regexp.MustCompile(`^[0-9][0-9]\.md$`)

// Cases는 민원 번호를 순서대로 돌려준다.
func Cases(root string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(RegionDir(root), "complaints"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if caseFile.MatchString(e.Name()) {
			out = append(out, strings.TrimSuffix(e.Name(), ".md"))
		}
	}
	sort.Strings(out)
	return out, nil
}
