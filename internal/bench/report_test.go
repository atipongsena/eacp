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

// A step whose figures could not be collected shows why, never zeros.
func TestFailedStepIsNotZero(t *testing.T) {
	row := actionRow(100, Step{Offered: 50, Incomplete: true, NA: map[string]string{"timelines": "connection refused"}})
	if !strings.Contains(row, "failed (timelines: connection refused)") {
		t.Fatalf("row = %s", row)
	}
	for _, cell := range strings.Split(strings.Trim(row, "| "), "|")[2:] {
		if c := strings.TrimSpace(cell); c == "0" || c == "0.0" {
			t.Fatalf("a failed step renders a zero cell: %s", row)
		}
	}
	if got := dupCell(Step{NA: map[string]string{"duplicate_keys": "timeout"}}); got != "n/a (timeout)" {
		t.Fatalf("an unread duplicate-key count renders %q", got)
	}
}

// An incomplete row says what its latencies leave out, and a step that
// began with an earlier backlog says so.
func TestIncompleteStatesItsOpenActions(t *testing.T) {
	if got := statusCell(Step{Incomplete: true, Open: 711}); got != "incomplete: 711 still open; latencies cover finished actions only" {
		t.Fatalf("status = %q", got)
	}
	if got := statusCell(Step{PreexistingOpen: 12}); got != "ok; began with 12 earlier actions open" {
		t.Fatalf("status = %q", got)
	}
}

// The maximum sustainable rate names the rung that failed, so a reader sees
// the bound it is.
func TestMaxSustainableNamesTheFailingRung(t *testing.T) {
	steps := []Step{{Offered: 25}, {Offered: 30, Saturated: []string{"throughput"}}}
	if got := maxCell(steps, 25); got != "25 (saturated at 30)" {
		t.Fatalf("maxCell = %q", got)
	}
	if got := maxCell([]Step{{Offered: 25, Incomplete: true}}, 0); got != "none (incomplete at 25)" {
		t.Fatalf("maxCell = %q", got)
	}
	if got := maxCell([]Step{{Offered: 25}, {Offered: 50}}, 50); got != "50 (highest rung)" {
		t.Fatalf("maxCell = %q", got)
	}
}
