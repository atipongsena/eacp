package opensource

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const modulePath = "github.com/atipongsena/eacp"

func TestModulePath(t *testing.T) {
	for _, line := range strings.Split(read(t, "go.mod"), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "module" {
			if f[1] != modulePath {
				t.Fatalf("go.mod declares module %q, want %q", f[1], modulePath)
			}
			return
		}
	}
	t.Fatal("go.mod has no module line")
}

// The old module path "eacp" must not survive in a file a build reads.
// Specs, plans and reviews are historical records and keep their paths.
var oldImport = regexp.MustCompile(`(^|[^/\w.-])eacp/(internal|cmd|integrations|test|sidecars)/`)

func TestNoOldModulePath(t *testing.T) {
	root := repoRoot(t)
	for _, f := range trackedFiles(t) {
		if strings.HasPrefix(f, "docs/superpowers/") || strings.HasPrefix(f, "docs/reviews/") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if err != nil || !isText(raw) {
			continue
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if oldImport.MatchString(line) {
				t.Errorf("%s:%d names the old module path: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}
