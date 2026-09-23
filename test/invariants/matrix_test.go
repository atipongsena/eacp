// Package invariants checks docs/INVARIANTS.md against MASTER_PLAN §103 and
// the test suite: the Slice A exit criterion (§82) as a test, and the Slice
// B invariants mapped so far.
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

// tags returns the slice tag ([A], [B] or [C]) of every invariant in §103.
func tags(t *testing.T) map[string]string {
	t.Helper()
	plan := read(t, "docs/MASTER_PLAN.md")
	start := strings.Index(plan, "\n# 103. Critical Invariants")
	end := strings.Index(plan, "\n# 104.")
	if start < 0 || end < start {
		t.Fatal("MASTER_PLAN §103 not found")
	}
	out := map[string]string{}
	for _, line := range strings.Split(plan[start:end], "\n") {
		if m := planInvariant.FindStringSubmatch(line); m != nil {
			out[m[1]] = m[2]
		}
	}
	a := 0
	for _, tag := range out {
		if tag == "A" {
			a++
		}
	}
	if a < 10 {
		t.Fatalf("found only %d [A] invariants in §103: %v", a, out)
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

// Every [A] invariant has a section with tests. A [B] invariant may have one
// once its phase lands. A section carries its invariant's §103 tag, and
// every test it names exists.
func TestEverySliceAInvariantHasExistingTests(t *testing.T) {
	plan := tags(t)
	tests := map[string][]string{} // invariant -> "pkg TestName"
	var current string
	for _, line := range strings.Split(read(t, "docs/INVARIANTS.md"), "\n") {
		if m := docSection.FindStringSubmatch(line); m != nil {
			current = m[1]
			switch tag, ok := plan[current]; {
			case !ok:
				t.Errorf("section %s is not an invariant of §103", current)
			case m[2] != tag:
				t.Errorf("section %s is tagged [%s]; §103 tags it [%s]", current, m[2], tag)
			case tag == "C":
				t.Errorf("section %s is a Slice C invariant", current)
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

	var missing []string
	for n, tag := range plan {
		if _, ok := tests[n]; tag == "A" && !ok {
			missing = append(missing, n)
		}
	}
	sort.Strings(missing)
	if len(missing) != 0 {
		t.Errorf("[A] invariants without a section in docs/INVARIANTS.md: %v", missing)
	}
	for n, refs := range tests {
		if len(refs) == 0 {
			t.Errorf("invariant %s has no tests in docs/INVARIANTS.md", n)
		}
		for _, ref := range refs {
			dir, name, _ := strings.Cut(ref, " ")
			if !hasTest(t, dir, name) {
				t.Errorf("invariant %s: %s has no %s", n, dir, name)
			}
		}
	}
}
