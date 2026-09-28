package opensource

import (
	"regexp"
	"strings"
	"testing"
)

func TestLicenseIsApache2(t *testing.T) {
	license := read(t, "LICENSE")
	for _, want := range []string{"Apache License", "Version 2.0, January 2004", "END OF TERMS AND CONDITIONS"} {
		if !strings.Contains(license, want) {
			t.Errorf("LICENSE lacks %q: it is not the Apache License 2.0 text", want)
		}
	}
}

func TestNoticeNamesTheCopyright(t *testing.T) {
	if !strings.Contains(read(t, "NOTICE"), "Copyright 2026 atipongsena") {
		t.Fatal("NOTICE lacks the line Copyright 2026 atipongsena")
	}
}

// goModules lists the module paths of go.mod's require directives.
func goModules(t *testing.T) []string {
	t.Helper()
	var mods []string
	inBlock := false
	for _, line := range strings.Split(read(t, "go.mod"), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "require (":
			inBlock = true
		case inBlock && line == ")":
			inBlock = false
		case inBlock && line != "" && !strings.HasPrefix(line, "//"):
			mods = append(mods, strings.Fields(line)[0])
		case strings.HasPrefix(line, "require "):
			mods = append(mods, strings.Fields(line)[1])
		}
	}
	if len(mods) == 0 {
		t.Fatal("go.mod requires nothing: the parser is wrong")
	}
	return mods
}

var requirement = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]*)==`)

// sidecarPackages lists the pinned packages of the AGT sidecar.
func sidecarPackages(t *testing.T) []string {
	t.Helper()
	var pkgs []string
	for _, line := range strings.Split(read(t, "sidecars/agt-pdp/requirements.txt"), "\n") {
		if m := requirement.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			pkgs = append(pkgs, m[1])
		}
	}
	if len(pkgs) == 0 {
		t.Fatal("the sidecar requires nothing: the parser is wrong")
	}
	return pkgs
}

func TestThirdPartyNoticesCoverDependencies(t *testing.T) {
	notices := read(t, "THIRD_PARTY_NOTICES.md")
	for _, m := range goModules(t) {
		if !strings.Contains(notices, "`"+m+"`") {
			t.Errorf("THIRD_PARTY_NOTICES.md does not list the Go module %s", m)
		}
	}
	for _, p := range sidecarPackages(t) {
		if !strings.Contains(notices, "`"+p+"`") {
			t.Errorf("THIRD_PARTY_NOTICES.md does not list the Python package %s", p)
		}
	}
}
