package bench

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/report.golden")

func testInputs(t *testing.T) []Input {
	t.Helper()
	var in []Input
	for _, name := range []string{"results-agt.json", "results-local.json"} {
		raw, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		in = append(in, Input{Name: name, Raw: raw})
	}
	return in
}

func TestReportGolden(t *testing.T) {
	got, err := Report(testInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "report.golden")
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("report differs from %s (run with -update if intended):\n%s", golden, got)
	}
	for _, must := range []string{"incomplete", "saturated (throughput, p99)", "n/a (docker: not found)", GeneratedBegin, GeneratedEnd} {
		if !strings.Contains(got, must) {
			t.Errorf("report lacks %q", must)
		}
	}
}

func TestReportIsDeterministic(t *testing.T) {
	a, err := Report(testInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	in := testInputs(t)
	in[0], in[1] = in[1], in[0]
	b, err := Report(in)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("the report depends on input order")
	}
}

func TestSpliceKeepsTheText(t *testing.T) {
	old := GeneratedBegin + "\nold\n" + GeneratedEnd
	block := GeneratedBegin + "\nnew\n" + GeneratedEnd
	if got, want := Splice("A\n"+old+"\nFindings", block), "A\n"+block+"\nFindings"; got != want {
		t.Fatalf("Splice = %q, want %q", got, want)
	}
	if got := Splice("no markers", block); got != block+"\n" {
		t.Fatalf("Splice without markers = %q", got)
	}
}

// A measure that could not be taken is n/a in its cell, never a zero.
func TestNotAvailableIsNeverZero(t *testing.T) {
	if got := summaryCell(Summary{}); got != "n/a" {
		t.Errorf("an empty summary renders %q", got)
	}
	if got := resourceCell(Resource{NA: "docker: not found"}); got != "n/a (docker: not found)" {
		t.Errorf("a failed resource renders %q", got)
	}
	if got := budgetCell(Step{NA: map[string]string{"budget_reserve": "no reservations"}}); got != "n/a (no reservations)" {
		t.Errorf("a missing budget mean renders %q", got)
	}
	if got := ratesCell(Rates{NA: "fewer than two NATS samples"}, "in_msgs_per_s"); got != "n/a (fewer than two NATS samples)" {
		t.Errorf("failed rates render %q", got)
	}
	if got := summaryCell(Summary{Count: 3, P50Ms: 1, P95Ms: 2.25, P99Ms: 3}); got != "1.0 / 2.2 / 3.0" {
		t.Errorf("a summary renders %q", got)
	}
}
