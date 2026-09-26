// Package helm renders deployments/helm/eacp with the pinned Helm and checks
// the manifests (ADR-029 Rev 1.1). It skips without Helm unless
// EACP_HELM_REQUIRED=1 makes it fail.
package helm

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type object = map[string]any

func root(t *testing.T) string {
	t.Helper()
	r, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// helmPath is the pinned .tools/helm, else helm on PATH.
func helmPath(t *testing.T) string {
	t.Helper()
	name := "helm"
	if runtime.GOOS == "windows" {
		name = "helm.exe"
	}
	if p := filepath.Join(root(t), ".tools", name); fileExists(p) {
		return p
	}
	if p, err := exec.LookPath("helm"); err == nil {
		return p
	}
	if os.Getenv("EACP_HELM_REQUIRED") == "1" {
		t.Fatal("helm is required (EACP_HELM_REQUIRED=1) but neither .tools/helm nor helm on PATH exists")
	}
	t.Skip("helm not found: the chart tests did not run (see docs/KUBERNETES.md; EACP_HELM_REQUIRED=1 fails instead)")
	return ""
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// e2eArgs renders with the e2e values file, the chart's reference input.
func e2eArgs(t *testing.T) []string {
	return []string{"-f", filepath.Join(root(t), "deployments", "k8s", "e2e-values.yaml")}
}

func runHelm(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(helmPath(t), args...)
	cmd.Dir = root(t)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err := cmd.Run()
	if err != nil {
		return stderr.String() + out.String(), err
	}
	return out.String(), nil
}

// renderErr renders release "eacp" in namespace "eacp" with the e2e values
// and extra arguments.
func renderErr(t *testing.T, extra ...string) (string, error) {
	t.Helper()
	args := append([]string{"template", "eacp", filepath.Join("deployments", "helm", "eacp"), "-n", "eacp"}, e2eArgs(t)...)
	return runHelm(t, append(args, extra...)...)
}

func render(t *testing.T, extra ...string) []object {
	t.Helper()
	out, err := renderErr(t, extra...)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	var objs []object
	dec := yaml.NewDecoder(strings.NewReader(out))
	for {
		var o object
		err := dec.Decode(&o)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("parse rendered YAML: %v", err)
		}
		if o != nil {
			objs = append(objs, o)
		}
	}
	return objs
}

func get(o any, path ...string) any {
	for _, p := range path {
		m, ok := o.(map[string]any)
		if !ok {
			return nil
		}
		o = m[p]
	}
	return o
}

func list(o any, path ...string) []any { l, _ := get(o, path...).([]any); return l }

func find(t *testing.T, objs []object, kind, name string) object {
	t.Helper()
	for _, o := range objs {
		if o["kind"] == kind && get(o, "metadata", "name") == name {
			return o
		}
	}
	t.Fatalf("no %s %s rendered", kind, name)
	return nil
}

func all(objs []object, kind string) []object {
	var out []object
	for _, o := range objs {
		if o["kind"] == kind {
			out = append(out, o)
		}
	}
	return out
}

// podSpec returns the pod template spec of a Deployment or Job.
func podSpec(o object) map[string]any {
	s, _ := get(o, "spec", "template", "spec").(map[string]any)
	return s
}

func TestChartLintsStrictly(t *testing.T) {
	args := append([]string{"lint", "--strict", filepath.Join("deployments", "helm", "eacp")}, e2eArgs(t)...)
	if out, err := runHelm(t, args...); err != nil {
		t.Fatalf("helm lint: %v\n%s", err, out)
	}
}
