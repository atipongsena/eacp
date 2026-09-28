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
// committed baselines, and the README (both languages) quotes only figures
// from it.

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

// resultTables is the generated block without its Configuration section:
// the measured tables a published figure may be quoted from. (A setting
// such as 100000 is not a result.)
func resultTables(t *testing.T) string {
	t.Helper()
	doc := benchmarksDoc(t)
	i, j := strings.Index(doc, GeneratedBegin), strings.Index(doc, GeneratedEnd)
	if i < 0 || j < i {
		t.Fatal("docs/BENCHMARKS.md has no generated block")
	}
	block := doc[i:j]
	c := strings.Index(block, "## Configuration")
	if c < 0 {
		t.Fatal("the generated block has no Configuration section")
	}
	end := strings.Index(block[c+1:], "\n## ")
	if end < 0 {
		t.Fatal("the Configuration section is the last one")
	}
	return block[:c] + block[c+1+end:]
}

// inTables reports whether n appears in the result tables as a whole number
// token (not as part of a longer number).
func inTables(tables, n string) bool {
	return regexp.MustCompile(`(^|[^\d.])` + regexp.QuoteMeta(n) + `([^\d.]|$)`).MatchString(tables)
}

// performanceFigure is a number followed by a rate or a latency unit.
var performanceFigure = regexp.MustCompile(`(\d[\d,.]*\d|\d)\s*(?:[A-Za-z]*/s|ms|rps)\b`)

func TestReadmeNumbersComeFromTheBaseline(t *testing.T) {
	tables := resultTables(t)
	for _, name := range []string{"README.md", "README.th.md"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", name))
		if err != nil {
			t.Fatal(err)
		}
		found := 0
		for _, m := range performanceFigure.FindAllStringSubmatch(string(raw), -1) {
			found++
			if n := strings.ReplaceAll(m[1], ",", ""); !inTables(tables, n) {
				t.Errorf("%s quotes %q, but %s is in no result table of docs/BENCHMARKS.md", name, m[0], n)
			}
		}
		if found == 0 {
			t.Fatalf("%s quotes no benchmark figure: the guard would check nothing", name)
		}
	}
}

// The hand-written Findings below the generated block add no figure of
// their own: every measured-looking number is a table cell.
func TestFindingsQuoteTheTables(t *testing.T) {
	doc := benchmarksDoc(t)
	i := strings.Index(doc, "## Findings")
	if i < 0 {
		t.Fatal("docs/BENCHMARKS.md has no Findings section")
	}
	tables := resultTables(t)
	// Decimals, and integers with a unit, are measurements; bare integers
	// such as agent counts, rates of the ladder or "13 API calls" are
	// checked only when they carry a unit.
	figure := regexp.MustCompile(`\d[\d,]*\.\d+|\d[\d,]*\s*(?:ms|%|[A-Za-z]*/s)\b`)
	for _, m := range figure.FindAllString(doc[i:], -1) {
		n := strings.ReplaceAll(regexp.MustCompile(`[\d,.]*\d`).FindString(m), ",", "")
		if !inTables(tables, n) {
			t.Errorf("the Findings state %q, but %s is in no result table", m, n)
		}
	}
}
