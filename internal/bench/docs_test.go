package bench

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// MASTER_PLAN §105: a published figure exists only with a raw result behind
// it. docs/BENCHMARKS.md's generated block is exactly the report of the
// committed baselines, and the README quotes only figures from it.

func baselineReport(t *testing.T) string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "..", "docs", "benchmarks", "baseline-*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no docs/benchmarks/baseline-*.json: %v", err)
	}
	slices.Sort(paths)
	var in []Input
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		in = append(in, Input{Name: filepath.Base(p), Raw: raw})
	}
	block, err := Report(in)
	if err != nil {
		t.Fatal(err)
	}
	return block
}

func benchmarksDoc(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "BENCHMARKS.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestBenchmarksDocMatchesBaseline(t *testing.T) {
	want := baselineReport(t)
	doc := benchmarksDoc(t)
	i, j := strings.Index(doc, GeneratedBegin), strings.Index(doc, GeneratedEnd)
	if i < 0 || j < i {
		t.Fatal("docs/BENCHMARKS.md has no generated block")
	}
	if got := doc[i : j+len(GeneratedEnd)]; got != want {
		t.Fatal("docs/BENCHMARKS.md's generated block is not the report of docs/benchmarks/baseline-*.json: " +
			"run go run ./cmd/eacp-bench report --out docs/BENCHMARKS.md docs/benchmarks/baseline-*.json")
	}
}

var performanceFigure = regexp.MustCompile(`(\d[\d,.]*)\s*(?:actions/s|req/s|rps|ms|/s)\b`)

func TestReadmeNumbersComeFromTheBaseline(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	// A figure must appear in docs/BENCHMARKS.md with its unit: a bare number
	// matches too much (an override of 100000 is not 100000 actions/s).
	space := regexp.MustCompile(`\s+`)
	doc := space.ReplaceAllString(benchmarksDoc(t), " ")
	for _, m := range performanceFigure.FindAllString(string(raw), -1) {
		if figure := space.ReplaceAllString(m, " "); !strings.Contains(doc, figure) {
			t.Errorf("README quotes %q, which docs/BENCHMARKS.md does not state", figure)
		}
	}
}
