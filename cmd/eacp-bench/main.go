// Command eacp-bench is the load benchmark (MASTER_PLAN §104, §105). It runs
// against the isolated bench stack that scripts/bench.sh starts:
//
//	eacp-bench run --pdp local|microsoft-agt [--quick] [--erp-delay-ms N] ...
//	eacp-bench report [--out docs/BENCHMARKS.md] results.json...
//
// run registers a tenant and agents, sends open-loop load, reads per-stage
// timings from PostgreSQL and writes one results file. Every key it creates
// lives only in this process; the results writer refuses a file holding
// one. report renders results files as markdown (internal/bench.Report).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"eacp/internal/bench"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = runCmd(os.Args[2:])
	case "report":
		err = reportCmd(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "eacp-bench:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: eacp-bench run --pdp local|microsoft-agt [flags] | eacp-bench report [--out file] results.json...")
	os.Exit(2)
}

func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	o := options{}
	fs.StringVar(&o.pdp, "pdp", "", "the API's PDP: local or microsoft-agt (set by scripts/bench.sh)")
	fs.BoolVar(&o.quick, "quick", false, "N=100, two action steps, one LLM step, short windows")
	fs.IntVar(&o.erpDelayMS, "erp-delay-ms", 0, "the Fake ERP's delay_ms per call (0-5000)")
	fs.StringVar(&o.api, "api", "http://127.0.0.1:28080", "control plane API")
	fs.StringVar(&o.gateway, "gateway", "http://127.0.0.1:28083", "LLM gateway")
	fs.StringVar(&o.dsn, "dsn", "postgres://postgres:postgres@127.0.0.1:55434/eacp?sslmode=disable",
		"the bench PostgreSQL (read-only queries)")
	fs.StringVar(&o.project, "project", "eacp-bench", "compose project")
	files := fs.String("compose-files", "docker-compose.yml,deployments/bench/compose.bench.yml", "compose files, comma-separated")
	fs.StringVar(&o.out, "out", "bench-results", "results directory")
	_ = fs.Parse(args)
	if o.pdp != "local" && o.pdp != "microsoft-agt" {
		return fmt.Errorf("--pdp must be local or microsoft-agt")
	}
	if o.erpDelayMS < 0 || o.erpDelayMS > 5000 {
		return fmt.Errorf("--erp-delay-ms must be in [0, 5000]")
	}
	o.composeFiles = strings.Split(*files, ",")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	path, err := run(ctx, o)
	if err != nil {
		return err
	}
	fmt.Println("results:", path)
	return nil
}

func reportCmd(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	out := fs.String("out", "", "rewrite the generated block of this markdown file (default: print)")
	_ = fs.Parse(args)
	if fs.NArg() == 0 {
		return fmt.Errorf("report needs at least one results file")
	}
	var inputs []bench.Input
	for _, p := range fs.Args() {
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		inputs = append(inputs, bench.Input{Name: filepath.Base(p), Raw: raw})
	}
	block, err := bench.Report(inputs)
	if err != nil {
		return err
	}
	if *out == "" {
		fmt.Println(block)
		return nil
	}
	doc, err := os.ReadFile(*out)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.WriteFile(*out, []byte(bench.Splice(string(doc), block)), 0o644)
}
