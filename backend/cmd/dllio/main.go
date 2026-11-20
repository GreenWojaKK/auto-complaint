// dllio는 민원 분석 단계를 실행한다. 저장소 안 어디서든 실행할 수 있다.
//
//	dllio problems --out <출력 경로> [--cases 01,02] [--threshold 0.5] [--workers 5] [--catalog prompts/l2/catalog.json]
//	dllio extract --run <1단계 출력 경로> [--cases 01,02] [--effort high] [--workers 5]
//	dllio plan --run <앞 단계 출력 경로> [--cases 01,02] [--effort high] [--workers 5]
//	dllio execute --run <앞 단계 출력 경로> [--cases 01,02]
//	dllio compose --run <앞 단계 출력 경로> [--out <결과 경로>] [--cases 01,02] [--effort high]
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
	"dllio/backend/repo"
	"dllio/backend/tools"
)

const (
	decisionURL   = "https://decision.vllm-sr.ai"
	decisionModel = "vllm-sr/d3-ultra"
	decisionName  = "d3-ultra"
	languageURL   = "https://api.openai.com/v1"
	languageModel = "gpt-6-luna"
	languageName  = "luna"
)

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

func decider(root string) (*matchi.Decider, error) {
	key, err := matchi.Secret(root, "VLLM_SR_API_KEY")
	if err != nil {
		return nil, err
	}
	return &matchi.Decider{URL: decisionURL, Model: decisionModel, Key: key, Log: matchi.NewLogger(repo.Artifacts(root))}, nil
}

func writer(root, effort string) (*matchi.Writer, error) {
	key, err := matchi.Secret(root, "OPENAI_API_KEY")
	if err != nil {
		return nil, err
	}
	return &matchi.Writer{URL: languageURL, Model: languageModel, Effort: effort, Key: key, Log: matchi.NewLogger(repo.Artifacts(root))}, nil
}

func runExtract(args []string) error {
	root, err := repo.Root()
	if err != nil {
		return err
	}
	f := flag.NewFlagSet("extract", flag.ExitOnError)
	run := f.String("run", "", "1단계 출력 경로")
	effort := f.String("effort", "high", "언어 모델의 추론 강도")
	cases := f.String("cases", "", "민원 번호. 비우면 1단계 결과 전부")
	workers := f.Int("workers", 5, "동시에 처리할 민원 수")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *run == "" {
		return fmt.Errorf("--run을 지정한다")
	}
	w, err := writer(root, *effort)
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

func loadTools(root string) (*tools.Tools, error) {
	return tools.Load(context.Background(), tools.Options{DSN: "postgresql://dllio_read@localhost:5432/dllio",
		PasswordFile: filepath.Join(root, "secrets", "pg-dllio-read.txt"),
		Boundaries:   filepath.Join(repo.RegionDir(root), "boundaries.geojson")})
}

func runPlan(args []string) error {
	root, err := repo.Root()
	if err != nil {
		return err
	}
	f := flag.NewFlagSet("plan", flag.ExitOnError)
	run := f.String("run", "", "앞 단계 출력 경로")
	effort := f.String("effort", "high", "언어 모델의 추론 강도")
	cases := f.String("cases", "", "민원 번호. 비우면 추출 결과가 있는 민원 전부")
	workers := f.Int("workers", 5, "동시에 처리할 민원 수")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *run == "" {
		return fmt.Errorf("--run을 지정한다")
	}
	d, err := decider(root)
	if err != nil {
		return err
	}
	w, err := writer(root, *effort)
	if err != nil {
		return err
	}
	mapKey, err := matchi.Secret(root, "VWORLD_API_KEY")
	if err != nil {
		return err
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
	return plan.Run(context.Background(), plan.Config{Root: root, Run: *run, ModelName: languageName, DecisionName: decisionName, Cases: list, Workers: *workers,
		Tools: t, Decider: d, Writer: w, Map: &plan.MapSearch{URL: "https://api.vworld.kr/req/search", Key: mapKey}, Report: func(line string) { fmt.Println(line) }})
}

func runCompose(args []string) error {
	root, err := repo.Root()
	if err != nil {
		return err
	}
	f := flag.NewFlagSet("compose", flag.ExitOnError)
	run := f.String("run", "", "앞 단계 출력 경로")
	out := f.String("out", "", "compose·view를 쓸 경로. 비우면 --run. 같은 계획으로 모델을 비교할 때 쓴다")
	effort := f.String("effort", "high", "언어 모델의 추론 강도")
	cases := f.String("cases", "", "민원 번호. 비우면 계획이 있는 민원 전부")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *run == "" {
		return fmt.Errorf("--run을 지정한다")
	}
	w, err := writer(root, *effort)
	if err != nil {
		return err
	}
	var list []string
	if *cases != "" {
		list = strings.Split(*cases, ",")
	}
	return compose.Run(context.Background(), compose.Config{Root: root, Run: *run, Out: *out, ModelName: languageName, Cases: list, Writer: w,
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
	d, err := decider(root)
	if err != nil {
		return err
	}
	return problems.Run(context.Background(), problems.Config{
		Root: root, Out: *out, ModelName: decisionName, CatalogPath: *catalog, Threshold: *threshold, Cases: list, Workers: *workers, Decider: d,
		Report: func(line string) { fmt.Println(line) }})
}
