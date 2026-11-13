// dllio는 민원 분석 단계를 실행한다. 저장소 안 어디서든 실행할 수 있다. 민원과 교통 자료는 지역 폴더(환경 변수 DLLIO_REGION,
// 없으면 예제 도시 examples/solmoe)에서 읽는다. 판정은 이 실행이 띄우는 모델 worker(backend/worker)가, 쓰기는 따로 띄운 Raon 생성
// 서버(raon/serve.sh)가 받는다. 다른 주소는 DLLIO_DECISION_*, DLLIO_LANGUAGE_*로 정한다.
//
//	dllio problems --out <출력 경로> [--cases 01,02] [--threshold 0.5] [--workers 5] [--catalog prompts/l2/catalog.json]
//	dllio extract --run <1단계 출력 경로> [--cases 01,02] [--effort none] [--workers 5]
//	dllio plan --run <앞 단계 출력 경로> [--cases 01,02] [--effort none] [--workers 5]
//	dllio execute --run <앞 단계 출력 경로> [--cases 01,02]
//	dllio compose --run <앞 단계 출력 경로> [--out <결과 경로>] [--cases 01,02] [--effort none] [--workers 1]
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"dllio/backend/compose"
	"dllio/backend/execute"
	"dllio/backend/extract"
	"dllio/backend/matchi"
	"dllio/backend/plan"
	"dllio/backend/problems"
	"dllio/backend/region"
	"dllio/backend/repo"
	"dllio/backend/tools"
	"dllio/backend/worker"
)

// local은 이 실행이 띄운 판정 모델 worker다. 처음 쓸 때 모델을 올린다.
var local *worker.Client

// localWorker는 모델 worker를 한 번만 만든다.
func localWorker() *worker.Client {
	if local == nil {
		local = worker.FromEnvironment()
	}
	return local
}

func main() {
	if len(os.Args) < 2 {
		fail(fmt.Errorf("사용법: dllio problems|extract|plan|execute|compose ..."))
	}
	var err error
	switch os.Args[1] {
	case "problems":
		err = runProblems(os.Args[2:])
	case "extract":
		err = runExtract(os.Args[2:])
	case "plan":
		err = runPlan(os.Args[2:])
	case "execute":
		err = runExecute(os.Args[2:])
	case "compose":
		err = runCompose(os.Args[2:])
	default:
		err = fmt.Errorf("알 수 없는 단계: %s", os.Args[1])
	}
	if local != nil {
		local.Close()
	}
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func caseList(root, cases string) ([]string, error) {
	if cases != "" {
		return strings.Split(cases, ","), nil
	}
	return repo.Cases(root)
}

// decider는 판정 모델의 연결(matchi.Decision)과 결과에 남길 이름이다.
func decider(root string) (*matchi.Decider, string, error) {
	e, err := matchi.Decision()
	if err != nil {
		return nil, "", err
	}
	d := &matchi.Decider{URL: e.URL, Model: e.Model, Key: e.Key, Log: matchi.NewLogger(repo.Artifacts(root))}
	if e.URL == "" {
		d.Local = localWorker()
	}
	return d, e.Name, nil
}

// writer는 언어 모델의 연결(matchi.Language)과 결과에 남길 이름이다. 검사를 통과하지 못한 답은 대화에 잇지 않고 새 요청으로 다시 쓰게 한다.
func writer(root, effort string) (*matchi.Writer, string, error) {
	e, err := matchi.Language()
	if err != nil {
		return nil, "", err
	}
	return &matchi.Writer{URL: e.URL, Model: e.Model, Effort: effort, Key: e.Key, MaxOutput: e.OutputLimit, Fresh: true,
		Log: matchi.NewLogger(repo.Artifacts(root))}, e.Name, nil
}

func runExtract(args []string) error {
	root, err := repo.Root()
	if err != nil {
		return err
	}
	f := flag.NewFlagSet("extract", flag.ExitOnError)
	run := f.String("run", "", "1단계 출력 경로")
	effort := f.String("effort", "none", "쓰기의 추론 강도. none이면 추론하지 않는다")
	cases := f.String("cases", "", "민원 번호. 비우면 1단계 결과 전부")
	workers := f.Int("workers", 5, "동시에 처리할 민원 수")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *run == "" {
		return fmt.Errorf("--run을 지정한다")
	}
	w, languageName, err := writer(root, *effort)
	if err != nil {
		return err
	}
	var list []string
	if *cases != "" {
		list = strings.Split(*cases, ",")
	}
	return extract.Run(context.Background(), extract.Config{Root: root, Run: *run, ModelName: languageName, Cases: list, Workers: *workers, Writer: w,
		Report: func(line string) { fmt.Println(line) }})
}

// loadTools는 지역(DLLIO_REGION)의 교통 자료를 읽는다. 조회 계정의 비밀번호는 secrets/pg-dllio-read.txt다.
func loadTools(root string) (*tools.Tools, error) {
	r, err := region.Load(root)
	if err != nil {
		return nil, err
	}
	return tools.Load(context.Background(), tools.Options{Region: r, PasswordFile: filepath.Join(root, "secrets", "pg-dllio-read.txt")})
}

func runPlan(args []string) error {
	root, err := repo.Root()
	if err != nil {
		return err
	}
	f := flag.NewFlagSet("plan", flag.ExitOnError)
	run := f.String("run", "", "앞 단계 출력 경로")
	effort := f.String("effort", "none", "쓰기의 추론 강도. none이면 추론하지 않는다")
	cases := f.String("cases", "", "민원 번호. 비우면 추출 결과가 있는 민원 전부")
	workers := f.Int("workers", 5, "동시에 처리할 민원 수")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *run == "" {
		return fmt.Errorf("--run을 지정한다")
	}
	d, decisionName, err := decider(root)
	if err != nil {
		return err
	}
	w, languageName, err := writer(root, *effort)
	if err != nil {
		return err
	}
	t, err := loadTools(root)
	if err != nil {
		return err
	}
	defer t.Close()
	// 지역에 지도 검색 범위가 있을 때만 VWorld로 장소를 찾는다.
	var search *plan.MapSearch
	if box := t.Region().MapBox; box != "" {
		mapKey, err := matchi.Secret(root, "VWORLD_API_KEY")
		if err != nil {
			return err
		}
		search = &plan.MapSearch{URL: "https://api.vworld.kr/req/search", Key: mapKey, Box: box}
	}
	var list []string
	if *cases != "" {
		list = strings.Split(*cases, ",")
	}
	return plan.Run(context.Background(), plan.Config{Root: root, Run: *run, ModelName: languageName, DecisionName: decisionName, Cases: list, Workers: *workers,
		Tools: t, Decider: d, Writer: w, Map: search, Report: func(line string) { fmt.Println(line) }})
}

func runCompose(args []string) error {
	root, err := repo.Root()
	if err != nil {
		return err
	}
	f := flag.NewFlagSet("compose", flag.ExitOnError)
	run := f.String("run", "", "앞 단계 출력 경로")
	out := f.String("out", "", "compose·view를 쓸 경로. 비우면 --run. 같은 계획으로 모델을 비교할 때 쓴다")
	effort := f.String("effort", "none", "쓰기의 추론 강도. none이면 추론하지 않는다")
	cases := f.String("cases", "", "민원 번호. 비우면 계획이 있는 민원 전부")
	workers := f.Int("workers", 1, "동시에 처리할 민원 수")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *run == "" {
		return fmt.Errorf("--run을 지정한다")
	}
	w, languageName, err := writer(root, *effort)
	if err != nil {
		return err
	}
	var list []string
	if *cases != "" {
		list = strings.Split(*cases, ",")
	}
	return compose.Run(context.Background(), compose.Config{Root: root, Run: *run, Out: *out, ModelName: languageName, Cases: list, Workers: *workers, Writer: w,
		Report: func(line string) { fmt.Println(line) }})
}

func runExecute(args []string) error {
	root, err := repo.Root()
	if err != nil {
		return err
	}
	f := flag.NewFlagSet("execute", flag.ExitOnError)
	run := f.String("run", "", "앞 단계 출력 경로")
	cases := f.String("cases", "", "민원 번호. 비우면 계획이 있는 민원 전부")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *run == "" {
		return fmt.Errorf("--run을 지정한다")
	}
	t, err := loadTools(root)
	if err != nil {
		return err
	}
	defer t.Close()
	var list []string
	if *cases != "" {
		list = strings.Split(*cases, ",")
	}
	return execute.Run(context.Background(), execute.Config{Root: root, Run: *run, Cases: list, Tools: t, Report: func(line string) { fmt.Println(line) }})
}

func runProblems(args []string) error {
	root, err := repo.Root()
	if err != nil {
		return err
	}
	f := flag.NewFlagSet("problems", flag.ExitOnError)
	out := f.String("out", "", "출력 경로")
	threshold := f.Float64("threshold", 0.5, "예 확률 기준값")
	cases := f.String("cases", "", "민원 번호. 비우면 전부")
	catalog := f.String("catalog", filepath.Join(root, "prompts", "l2", "catalog.json"), "catalog 파일")
	workers := f.Int("workers", 5, "동시에 판정할 민원 수")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return fmt.Errorf("--out을 지정한다")
	}
	list, err := caseList(root, *cases)
	if err != nil {
		return err
	}
	d, decisionName, err := decider(root)
	if err != nil {
		return err
	}
	return problems.Run(context.Background(), problems.Config{
		Root: root, Out: *out, ModelName: decisionName, CatalogPath: *catalog, Threshold: *threshold, Cases: list, Workers: *workers, Decider: d,
		Report: func(line string) { fmt.Println(line) }})
}
