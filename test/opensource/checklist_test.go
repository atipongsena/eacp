package opensource

import (
	"strings"
	"testing"
)

// checklist is MASTER_PLAN §112's list. A public repository holds only
// tracked files, so an untracked file on this machine does not count.
var checklist = []string{
	"README.md", "README.th.md",
	"docs/ARCHITECTURE.md", "docs/ARCHITECTURE.th.md",
	"docs/adr/",
	"docs/security/THREAT_MODEL.md", "docs/security/THREAT_MODEL.th.md",
	"docker-compose.yml",
	"examples/README.md", "examples/README.th.md",
	".github/workflows/ci.yml",
	"docs/BENCHMARKS.md",
	"CONTRIBUTING.md", "CONTRIBUTING.th.md",
	"LICENSE", "NOTICE", "THIRD_PARTY_NOTICES.md",
	".github/ISSUE_TEMPLATE/",
	"docs/RELEASING.md", "docs/RELEASING.th.md",
	"CHANGELOG.md",
	"SECURITY.md", "SECURITY.th.md",
	"CODE_OF_CONDUCT.md",
}

func TestChecklistFilesExist(t *testing.T) {
	tracked := trackedFiles(t)
	for _, want := range checklist {
		found := false
		for _, f := range tracked {
			if f == want || (strings.HasSuffix(want, "/") && strings.HasPrefix(f, want)) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s is on the §112 checklist but is not tracked", want)
		}
	}
	// Each example's README pair is checked by TestEveryExampleHasBothReadmes.
}
