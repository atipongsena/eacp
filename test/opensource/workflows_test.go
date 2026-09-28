package opensource

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	usesLine = regexp.MustCompile(`^\s*(?:-\s*)?uses:\s*(\S+)(.*)$`)
	pinned   = regexp.MustCompile(`@[0-9a-f]{40}$`)
)

// TestWorkflowActionsArePinned: a tag can be moved to other code, a
// commit cannot. Every action a workflow uses names a full commit SHA.
func TestWorkflowActionsArePinned(t *testing.T) {
	dir := filepath.Join(repoRoot(t), ".github", "workflows")
	files, err := filepath.Glob(filepath.Join(dir, "*.yml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows in %s", dir)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			m := usesLine.FindStringSubmatch(line)
			if m == nil || strings.HasPrefix(m[1], "./") {
				continue
			}
			if !pinned.MatchString(m[1]) {
				t.Errorf("%s:%d uses %s, not pinned to a commit SHA", filepath.Base(f), i+1, m[1])
			}
		}
	}
}
