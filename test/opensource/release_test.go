package opensource

import (
	"regexp"
	"strings"
	"testing"
)

// releaseCommands are the commands a deployment runs. The fake services
// (fakeerp, fakemcp, fakea2a, fakellm) exist for tests and demos only:
// SECURITY.md says they are not shipped, so no release artefact holds one.
var releaseCommands = []string{"controlplane-api", "execution-worker", "llm-gateway", "eacpctl"}

func TestReleaseArchivesShipNoFakeService(t *testing.T) {
	m := regexp.MustCompile(`(?m)^commands="([^"]*)"`).FindStringSubmatch(read(t, "scripts/ci/release-binaries.sh"))
	if m == nil {
		t.Fatal("scripts/ci/release-binaries.sh has no commands= line")
	}
	if got := strings.Fields(m[1]); strings.Join(got, " ") != strings.Join(releaseCommands, " ") {
		t.Fatalf("release archives hold %v, want %v", got, releaseCommands)
	}
}

func TestReleaseImageShipsNoFakeService(t *testing.T) {
	// The release stage copies each release command by name, and nothing else
	// from the build stage.
	stage := dockerStage(t, read(t, "deployments/docker/Dockerfile"), "release")
	if strings.Contains(stage, "fake") {
		t.Errorf("the release stage mentions a fake service:\n%s", stage)
	}
	if regexp.MustCompile(`(?m)^COPY\s+--from=build\s+/out/\s`).MatchString(stage) {
		t.Errorf("the release stage copies the whole build output:\n%s", stage)
	}
	for _, c := range releaseCommands {
		if !strings.Contains(stage, "/out/"+c+" ") && !strings.Contains(stage, "/out/"+c+"\n") {
			t.Errorf("the release stage does not copy %s", c)
		}
	}

	// The workflow pushes that stage, not the compose image.
	wf := read(t, ".github/workflows/release.yml")
	i := strings.Index(wf, "file: deployments/docker/Dockerfile")
	if i < 0 {
		t.Fatal("release.yml does not build deployments/docker/Dockerfile")
	}
	step := wf[i:]
	if j := strings.Index(step, "- uses:"); j >= 0 {
		step = step[:j]
	}
	if !regexp.MustCompile(`(?m)^\s+target:\s*release\s*$`).MatchString(step) {
		t.Errorf("release.yml builds deployments/docker/Dockerfile without target: release:\n%s", step)
	}
}

// dockerStage returns the instructions of the named build stage, from its
// FROM up to the next FROM, without comment lines.
func dockerStage(t *testing.T, dockerfile, name string) string {
	t.Helper()
	from := regexp.MustCompile(`(?im)^FROM\s+\S+(?:\s+AS\s+(\S+))?\s*$`)
	locs := from.FindAllStringSubmatchIndex(dockerfile, -1)
	for k, loc := range locs {
		if loc[2] < 0 || !strings.EqualFold(dockerfile[loc[2]:loc[3]], name) {
			continue
		}
		end := len(dockerfile)
		if k+1 < len(locs) {
			end = locs[k+1][0]
		}
		var lines []string
		for _, l := range strings.Split(dockerfile[loc[0]:end], "\n") {
			if !strings.HasPrefix(strings.TrimSpace(l), "#") {
				lines = append(lines, l)
			}
		}
		return strings.Join(lines, "\n")
	}
	t.Fatalf("the Dockerfile has no stage %q", name)
	return ""
}
