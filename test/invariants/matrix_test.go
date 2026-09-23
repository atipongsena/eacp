// Package invariants checks docs/INVARIANTS.md against MASTER_PLAN §103 and
// the test suite: the Slice A exit criterion (§82) as a test.
package invariants

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var (
	planInvariant = regexp.MustCompile(`^\s*(\d+)\. \[([ABC])\]`)
	docSection    = regexp.MustCompile(`^## (\d+) \[([ABC])\]`)
	docTest       = regexp.MustCompile("^- `([a-z/]+)` (Test\\w+)")
)

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", path))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// sliceA returns the numbers of the invariants tagged [A] in §103.
func sliceA(t *testing.T) []string {
	t.Helper()
	plan := read(t, "docs/MASTER_PLAN.md")
	start := strings.Index(plan, "\n# 103. Critical Invariants")
	end := strings.Index(plan, "\n# 104.")
	if start < 0 || end < start {
		t.Fatal("MASTER_PLAN §103 not found")
	}
	var out []string
	for _, line := range strings.Split(plan[start:end], "\n") {
		if m := planInvariant.FindStringSubmatch(line); m != nil && m[2] == "A" {
			out = append(out, m[1])
		}
	}
	if len(out) < 10 {
		t.Fatalf("found only %d [A] invariants in §103: %v", len(out), out)
	}
	return out
}

// hasTest reports whether a _test.go file in dir declares func name.
func hasTest(t *testing.T, dir, name string) bool {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", dir, "*_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	decl := "func " + name + "(t *testing.T)"
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), decl) {
			return true
		}
	}
	return false
}

func TestEverySliceAInvariantHasExistingTests(t *testing.T) {
	tests := map[string][]string{} // invariant -> "pkg TestName"
	var current string
	for _, line := range strings.Split(read(t, "docs/INVARIANTS.md"), "\n") {
		if m := docSection.FindStringSubmatch(line); m != nil {
			current = m[1]
			if m[2] != "A" {
				t.Errorf("section %s is tagged [%s]; this map covers Slice A", m[1], m[2])
			}
			if _, dup := tests[current]; dup {
				t.Errorf("invariant %s has two sections", current)
			}
			tests[current] = nil
			continue
		}
		if m := docTest.FindStringSubmatch(line); m != nil {
			if current == "" {
				t.Fatalf("test outside a section: %s", line)
			}
			tests[current] = append(tests[current], m[1]+" "+m[2])
		}
	}

	want := sliceA(t)
	for _, n := range want {
		refs, ok := tests[n]
		if !ok || len(refs) == 0 {
			t.Errorf("invariant %s [A] has no tests in docs/INVARIANTS.md", n)
		}
		for _, ref := range refs {
			dir, name, _ := strings.Cut(ref, " ")
			if !hasTest(t, dir, name) {
				t.Errorf("invariant %s: %s has no %s", n, dir, name)
			}
		}
	}
	var extra []string
	for n := range tests {
		if !contains(want, n) {
			extra = append(extra, n)
		}
	}
	sort.Strings(extra)
	if len(extra) != 0 {
		t.Errorf("sections for invariants that are not [A] in §103: %v", extra)
	}
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
