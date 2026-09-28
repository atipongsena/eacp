package opensource

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// examples/setup.sh writes every key to examples/.env; it must never be
// committed.
func TestExamplesEnvIsIgnored(t *testing.T) {
	for _, f := range []string{"examples/.env", "examples/02-llm-gateway/.venv/pyvenv.cfg"} {
		cmd := exec.Command("git", "check-ignore", "-q", f)
		cmd.Dir = repoRoot(t)
		if err := cmd.Run(); err != nil {
			t.Errorf("%s is not git-ignored", f)
		}
	}
}

func TestEveryExampleHasBothReadmes(t *testing.T) {
	root := repoRoot(t)
	dirs, err := filepath.Glob(filepath.Join(root, "examples", "[0-9]*"))
	if err != nil || len(dirs) == 0 {
		t.Fatal("no numbered examples under examples/")
	}
	for _, d := range append(dirs, filepath.Join(root, "examples")) {
		for _, f := range []string{"README.md", "README.th.md"} {
			if _, err := os.Stat(filepath.Join(d, f)); err != nil {
				t.Errorf("%s has no %s", d, f)
			}
		}
	}
}
