# Phase 23b — Kubernetes deployment Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A hardened Helm chart for EACP that keeps the compose boundary, a render test that pins it, and a real 2-node minikube run (Slice A demo + a disruption test) that proves it.

**Architecture:** `deployments/helm/eacp` renders api/worker/pdp Deployments, a migrate hook Job, PDBs, NetworkPolicies and optional HPAs; values only name Secrets. `test/helm` renders it with pinned Helm and checks the YAML. `deployments/k8s/dev` holds dev-only PostgreSQL/NATS/Fake ERP/Fake MCP/agent with their own ingress policies and ExternalName aliases. `test/demo` gets a `platform` interface (compose | k8s) so the Slice A demo runs on the cluster, plus `TestKubernetesDisruption`. `scripts/k8s-e2e.sh` drives it.

**Tech Stack:** Helm v4.3.0 (`.tools/helm.exe`), minikube v1.38.1 (docker driver, Calico), kubectl, Go 1.27, `go.yaml.in/yaml/v3`.

**Spec:** `docs/superpowers/specs/2026-09-26-kubernetes-deployment-design.md`

## Global Constraints

- The chart never renders a secret value; values only name Secrets (spec §2.2).
- Wrong or dangerous values fail `helm template` (spec §2.3).
- No RBAC objects, no SA token mounted (spec §2.4).
- PostgreSQL and NATS are external to the chart (spec §2.5).
- Every pod: runAsNonRoot 65532, readOnlyRootFilesystem, drop ALL, no privilege escalation, seccomp RuntimeDefault, `/tmp` emptyDir (spec §3.1).
- Worker grace ≥ shutdownDelay + shutdownTimeout + maxCallSeconds; defaults 10s/15s/30s, grace 60 (spec §3.1).
- PDB `maxUnavailable: 1`; rolling `maxUnavailable: 0, maxSurge: 1` (spec §3.1).
- HPAs off by default (spec §3.4).
- Compose demo behaviour unchanged (spec §3.6).
- Commits as the user; no Co-Authored-By trailer. Tests with `-race` and `EACP_TEST_ADMIN_DSN`.

## Review Focus

1. `helm upgrade` of an installed release must re-run migrations before new pods start and never run the Job with the app role — Task 1 test (hook annotations, owner secret only in the Job).
2. A values file that gives the API a connector secret, or puts a DSN with a password into `env`, must fail rendering — Task 1 bad-value fixtures.
3. A rolling update while agents submit must not drop requests (drain delay + `maxUnavailable: 0`) — Task 5 `TestKubernetesDisruption` (health polling).
4. A drained node must not take all API or PDP pods at once — Task 5 (PDB honoured: availability polling during `kubectl drain`).
5. An agent pod must not reach the worker, PDP, PostgreSQL, NATS or the ERP even by IP — Task 5 probes (by Service name and by pod IP for the worker).

---

### Task 1: Chart workloads, secret custody and hardening (render-tested)

**Files:**
- Create: `test/helm/helm_test.go`, `test/helm/workloads_test.go`
- Create: `deployments/helm/eacp/Chart.yaml`, `values.yaml`, `templates/_helpers.tpl`, `templates/validate.yaml`, `templates/serviceaccount.yaml`, `templates/api.yaml`, `templates/worker.yaml`, `templates/pdp.yaml`, `templates/migrate.yaml`, `deployments/helm/eacp/.helmignore`
- Create: `deployments/k8s/e2e-values.yaml`

**Interfaces:**
- Produces: `render(t, extraArgs...) []object`, `renderErr(extraArgs...) (string, error)`, `helmPath(t) string`, `find(objs, kind, name) object`, `podSpec(obj) map[string]any` in `test/helm`; chart object names `<release>-api`, `<release>-worker`, `<release>-pdp`, `<release>-migrate`; labels `app.kubernetes.io/component` ∈ {api, worker, pdp, migrate}.

- [ ] **Step 1: Write the harness and the failing tests**

`test/helm/helm_test.go`:

```go
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
```

`test/helm/workloads_test.go`:

```go
package helm

import (
	"fmt"
	"strings"
	"testing"
)

var workloads = []struct{ kind, name, component string }{
	{"Deployment", "eacp-api", "api"}, {"Deployment", "eacp-worker", "worker"},
	{"Deployment", "eacp-pdp", "pdp"}, {"Job", "eacp-migrate", "migrate"},
}

func TestEveryPodIsHardened(t *testing.T) {
	objs := render(t)
	for _, w := range workloads {
		o := find(t, objs, w.kind, w.name)
		spec := podSpec(o)
		if get(o, "spec", "template", "metadata", "labels", "app.kubernetes.io/component") != w.component {
			t.Errorf("%s: component label missing", w.name)
		}
		if spec["automountServiceAccountToken"] != false || spec["enableServiceLinks"] != false {
			t.Errorf("%s: service account token or service links not disabled", w.name)
		}
		psc := get(spec, "securityContext")
		if get(psc, "runAsNonRoot") != true || get(psc, "runAsUser") != 65532 || get(psc, "runAsGroup") != 65532 ||
			get(psc, "seccompProfile", "type") != "RuntimeDefault" {
			t.Errorf("%s: pod securityContext = %v", w.name, psc)
		}
		tmp := false
		for _, v := range list(spec, "volumes") {
			if get(v, "name") == "tmp" && get(v, "emptyDir") != nil {
				tmp = true
			}
		}
		if !tmp {
			t.Errorf("%s: no /tmp emptyDir", w.name)
		}
		for _, c := range list(spec, "containers") {
			sc := get(c, "securityContext")
			caps := fmt.Sprint(get(sc, "capabilities", "drop"))
			if get(sc, "allowPrivilegeEscalation") != false || get(sc, "readOnlyRootFilesystem") != true || caps != "[ALL]" {
				t.Errorf("%s/%v: container securityContext = %v", w.name, get(c, "name"), sc)
			}
			if get(c, "resources", "limits", "memory") == nil || get(c, "resources", "requests", "cpu") == nil {
				t.Errorf("%s: resources not set", w.name)
			}
			if w.kind == "Deployment" && (get(c, "readinessProbe") == nil || get(c, "livenessProbe") == nil ||
				get(c, "startupProbe") == nil) {
				t.Errorf("%s: probes missing", w.name)
			}
		}
	}
	if len(all(objs, "Role"))+len(all(objs, "ClusterRole"))+len(all(objs, "RoleBinding"))+len(all(objs, "ClusterRoleBinding")) != 0 {
		t.Error("the chart renders RBAC objects")
	}
	sa := find(t, objs, "ServiceAccount", "eacp")
	if sa["automountServiceAccountToken"] != false {
		t.Error("service account mounts its token")
	}
}

// secretRefs lists every Secret name a pod spec references, with the keys
// it projects ("" for a whole-secret reference).
func secretRefs(spec map[string]any) map[string][]string {
	refs := map[string][]string{}
	for _, v := range list(spec, "volumes") {
		if n, ok := get(v, "secret", "secretName").(string); ok {
			for _, it := range list(v, "secret", "items") {
				refs[n] = append(refs[n], fmt.Sprint(get(it, "key")))
			}
			if len(list(v, "secret", "items")) == 0 {
				refs[n] = append(refs[n], "")
			}
		}
		for _, src := range list(v, "projected", "sources") {
			if n, ok := get(src, "secret", "name").(string); ok {
				for _, it := range list(src, "secret", "items") {
					refs[n] = append(refs[n], fmt.Sprint(get(it, "key")))
				}
			}
		}
	}
	for _, c := range list(spec, "containers") {
		for _, e := range list(c, "env") {
			if n, ok := get(e, "valueFrom", "secretKeyRef", "name").(string); ok {
				refs[n] = append(refs[n], fmt.Sprint(get(e, "valueFrom", "secretKeyRef", "key")))
			}
		}
		if len(list(c, "envFrom")) > 0 {
			refs["<envFrom>"] = append(refs["<envFrom>"], "")
		}
	}
	return refs
}

func TestSecretsReachOnlyTheirPods(t *testing.T) {
	objs := render(t)
	want := map[string]map[string]string{ // component -> secret -> keys
		"api":     {"eacp-db-app": "url", "eacp-pdp-tls": "ca.pem client-key.pem client.pem", "eacp-nats-relay": "url"},
		"worker":  {"eacp-db-app": "url", "eacp-connector-secrets": "connector-secrets.json", "eacp-nats-worker": "url"},
		"pdp":     {"eacp-pdp-tls": "ca.pem server-key.pem server.pem"},
		"migrate": {"eacp-db-owner": "url"},
	}
	for _, w := range workloads {
		refs := secretRefs(podSpec(find(t, objs, w.kind, w.name)))
		got := map[string]string{}
		for n, keys := range refs {
			got[n] = strings.Join(sortedUnique(keys), " ")
		}
		if fmt.Sprint(got) != fmt.Sprint(want[w.component]) {
			t.Errorf("%s references secrets %v, want %v", w.name, got, want[w.component])
		}
	}
}

func sortedUnique(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func TestNoSecretValueIsRendered(t *testing.T) {
	out, err := renderErr(t)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"kind: Secret", "postgres://", "nats://", "dev-only-", "BEGIN "} {
		if strings.Contains(out, bad) {
			t.Errorf("rendered manifests contain %q", bad)
		}
	}
}

func TestMigrationsRunAsAHookWithTheOwnerRole(t *testing.T) {
	job := find(t, render(t), "Job", "eacp-migrate")
	ann := get(job, "metadata", "annotations")
	if get(ann, "helm.sh/hook") != "pre-install,pre-upgrade" ||
		get(ann, "helm.sh/hook-delete-policy") != "before-hook-creation,hook-succeeded" {
		t.Fatalf("migrate hook annotations = %v", ann)
	}
	c := list(podSpec(job), "containers")[0]
	if fmt.Sprint(get(c, "args")) != "[migrate up]" || get(c, "command") == nil {
		t.Fatalf("migrate container = %v %v", get(c, "command"), get(c, "args"))
	}
	if get(job, "spec", "backoffLimit") == nil || podSpec(job)["restartPolicy"] != "Never" {
		t.Fatal("migrate Job must bound retries and never restart in place")
	}
}

func TestWorkerIdentityComesFromThePodName(t *testing.T) {
	objs := render(t)
	for name, key := range map[string]string{"eacp-worker": "EACP_WORKER_ID", "eacp-pdp": "AGT_PDP_INSTANCE_ID"} {
		found := false
		for _, e := range list(list(podSpec(find(t, objs, "Deployment", name)), "containers")[0], "env") {
			if get(e, "name") == key && get(e, "valueFrom", "fieldRef", "fieldPath") == "metadata.name" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: %s is not the pod name", name, key)
		}
	}
}

func TestDangerousValuesAreRefused(t *testing.T) {
	for set, msg := range map[string]string{
		"api.env.EACP_CONNECTOR_SECRETS_FILE=/x":       "connector secrets",
		"pdp.env.EACP_CONNECTOR_SECRETS_FILE=/x":       "connector secrets",
		"api.env.EACP_DATABASE_URL=postgres://a:b@c/d": "EACP_DATABASE_URL",
		"worker.env.EACP_NATS_URL=nats://u:p@h:4222":   "password",
		"database.appSecret=":                          "database.appSecret",
		"database.ownerSecret=":                        "database.ownerSecret",
		"pdp.tlsSecret=":                               "pdp.tlsSecret",
		"worker.connectorSecrets=":                     "worker.connectorSecrets",
		"api.replicas=0":                               "replicas",
		"governance.provider=local":                    "governance.allowLocal",
		"governance.provider=other":                    "governance.provider",
	} {
		out, err := renderErr(t, "--set", set)
		if err == nil || !strings.Contains(out, msg) {
			t.Errorf("--set %s: err = %v, output lacks %q:\n%s", set, err, msg, firstLines(out))
		}
	}
}

func firstLines(s string) string {
	lines := strings.SplitN(s, "\n", 4)
	return strings.Join(lines[:min(3, len(lines))], "\n")
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `EACP_HELM_REQUIRED=1 go test ./test/helm`
Expected: FAIL (`helm lint` / `helm template`: chart path not found).

- [ ] **Step 3: Implement the chart**

`deployments/helm/eacp/Chart.yaml`:

```yaml
apiVersion: v2
name: eacp
description: Enterprise Agent Control Plane - controlplane-api, execution-worker and the AGT sidecar PDP (ADR-029).
type: application
version: 0.23.0
appVersion: "0.23.0"
kubeVersion: ">=1.30.0-0"
```

`deployments/helm/eacp/.helmignore`:

```text
*.md
.git/
```

`deployments/helm/eacp/values.yaml`:

```yaml
# EACP Helm values (ADR-029 Rev 1.1, docs/KUBERNETES.md).
# The chart never holds a secret value: every *Secret value below NAMES an
# existing Kubernetes Secret with fixed keys. PostgreSQL and NATS are
# external. Install into a dedicated namespace: the default-deny
# NetworkPolicy covers every pod in it.

image:
  repository: eacp
  tag: ""            # defaults to the chart appVersion
  pullPolicy: IfNotPresent

governance:
  provider: microsoft-agt   # or local (tests only, with allowLocal)
  allowLocal: false

database:
  appSecret: ""      # Secret with key "url": the eacp_app DSN (services)
  ownerSecret: ""    # Secret with key "url": the eacp_owner DSN (migrate Job only)
  peers: []          # NetworkPolicy peers for PostgreSQL egress
  port: 5432

nats:
  enabled: true
  relaySecret: ""    # Secret with key "url" (controlplane-api relay user)
  workerSecret: ""   # Secret with key "url" (execution-worker user)
  peers: []
  port: 4222

otel:
  peers: []          # optional egress for the OTLP exporter
  ports: []

shutdown:
  delay: 10s         # EACP_SHUTDOWN_DELAY: keep serving, not ready
  timeout: 15s       # EACP_SHUTDOWN_TIMEOUT: graceful HTTP shutdown

api:
  replicas: 2
  terminationGracePeriodSeconds: 30
  service:
    type: ClusterIP
  ingress:
    from: []         # NetworkPolicy peers allowed to reach the API on 8080; [] = any source
  env: {}            # extra non-secret EACP_* settings
  resources:
    requests: {cpu: 100m, memory: 128Mi}
    limits: {cpu: "1", memory: 512Mi}

worker:
  replicas: 2
  terminationGracePeriodSeconds: 60
  maxCallSeconds: 30            # the longest connector call timeout you allow
  connectorSecrets: ""          # Secret with key "connector-secrets.json" (worker only)
  connectorEgress: []           # NetworkPolicy egress rules to enterprise systems (peers + ports)
  allowNoConnectorEgress: false
  env: {}
  resources:
    requests: {cpu: 100m, memory: 128Mi}
    limits: {cpu: "1", memory: 512Mi}

pdp:
  replicas: 2
  image:
    repository: eacp-agt-pdp
    tag: ""
    pullPolicy: IfNotPresent
  tlsSecret: ""      # Secret with ca.pem, client.pem, client-key.pem, server.pem, server-key.pem
  env: {}
  resources:
    requests: {cpu: 100m, memory: 192Mi}
    limits: {cpu: "1", memory: 768Mi}

migrate:
  backoffLimit: 3
  resources:
    requests: {cpu: 50m, memory: 64Mi}
    limits: {cpu: 500m, memory: 256Mi}

autoscaling:
  enabled: false     # CPU HPAs; queue-depth scaling is deferred (ADR-029 Rev 1.1)
  api: {minReplicas: 2, maxReplicas: 6, targetCPUUtilizationPercentage: 70}
  worker: {minReplicas: 2, maxReplicas: 10, targetCPUUtilizationPercentage: 70}
```

`deployments/helm/eacp/templates/_helpers.tpl`:

```yaml
{{- define "eacp.name" -}}{{ .Release.Name }}{{- end -}}

{{- define "eacp.labels" -}}
app.kubernetes.io/name: eacp
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/version: {{ .root.Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .root.Release.Service }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{- define "eacp.selector" -}}
app.kubernetes.io/name: eacp
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{- define "eacp.image" -}}{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}{{- end -}}
{{- define "eacp.pdpImage" -}}{{ .Values.pdp.image.repository }}:{{ .Values.pdp.image.tag | default .Chart.AppVersion }}{{- end -}}

{{- define "eacp.podSecurity" -}}
automountServiceAccountToken: false
enableServiceLinks: false
serviceAccountName: {{ .Release.Name }}
securityContext:
  runAsNonRoot: true
  runAsUser: 65532
  runAsGroup: 65532
  fsGroup: 65532
  seccompProfile:
    type: RuntimeDefault
{{- end -}}

{{- define "eacp.containerSecurity" -}}
securityContext:
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  capabilities:
    drop: [ALL]
{{- end -}}

{{- define "eacp.spread" -}}
topologySpreadConstraints:
  - maxSkew: 1
    topologyKey: kubernetes.io/hostname
    whenUnsatisfiable: ScheduleAnyway
    labelSelector:
      matchLabels:
        {{- include "eacp.selector" . | nindent 8 }}
{{- end -}}

{{- /* Extra env from a values map; the validation template refuses secrets. */ -}}
{{- define "eacp.extraEnv" -}}
{{- range $k, $v := . }}
- name: {{ $k }}
  value: {{ $v | quote }}
{{- end }}
{{- end -}}

{{- define "eacp.shutdownEnv" -}}
- name: EACP_SHUTDOWN_DELAY
  value: {{ .Values.shutdown.delay | quote }}
- name: EACP_SHUTDOWN_TIMEOUT
  value: {{ .Values.shutdown.timeout | quote }}
{{- end -}}

{{- define "eacp.seconds" -}}{{ trimSuffix "s" . | int }}{{- end -}}
```

`deployments/helm/eacp/templates/validate.yaml`:

```yaml
{{- /* Fail closed on dangerous or incomplete values (ADR-029 Rev 1.1). Renders nothing. */ -}}
{{- $v := .Values -}}
{{- range $name := list "database.appSecret" "database.ownerSecret" "pdp.tlsSecret" "worker.connectorSecrets" -}}
{{- $parts := splitList "." $name -}}
{{- $val := index (index $v (first $parts)) (last $parts) -}}
{{- if not $val -}}{{- fail (printf "%s must name an existing Secret" $name) -}}{{- end -}}
{{- end -}}
{{- if $v.nats.enabled -}}
{{- if or (not $v.nats.relaySecret) (not $v.nats.workerSecret) -}}{{- fail "nats.relaySecret and nats.workerSecret must name existing Secrets (or set nats.enabled=false)" -}}{{- end -}}
{{- end -}}
{{- range $c := list "api" "worker" "pdp" -}}
{{- $comp := index $v $c -}}
{{- if lt (int $comp.replicas) 1 -}}{{- fail (printf "%s.replicas must be at least 1" $c) -}}{{- end -}}
{{- range $k, $val := $comp.env -}}
{{- if eq $k "EACP_CONNECTOR_SECRETS_FILE" -}}{{- fail (printf "%s.env: connector secrets belong to the worker's connectorSecrets only (ADR-001)" $c) -}}{{- end -}}
{{- if eq $k "EACP_DATABASE_URL" -}}{{- fail (printf "%s.env: EACP_DATABASE_URL comes from database.appSecret, never from values" $c) -}}{{- end -}}
{{- if regexMatch "://[^/@:]+:[^/@]+@" (toString $val) -}}{{- fail (printf "%s.env.%s: a URL with a password belongs in a Secret" $c $k) -}}{{- end -}}
{{- end -}}
{{- end -}}
{{- if not (has $v.governance.provider (list "microsoft-agt" "local")) -}}{{- fail "governance.provider must be microsoft-agt or local" -}}{{- end -}}
{{- if and (eq $v.governance.provider "local") (not $v.governance.allowLocal) -}}{{- fail "governance.provider=local needs governance.allowLocal=true (tests only)" -}}{{- end -}}
{{- $delay := int (include "eacp.seconds" $v.shutdown.delay) -}}
{{- $timeout := int (include "eacp.seconds" $v.shutdown.timeout) -}}
{{- if lt (int $v.api.terminationGracePeriodSeconds) (add $delay $timeout) -}}{{- fail (printf "api.terminationGracePeriodSeconds must be at least shutdown.delay + shutdown.timeout (%d)" (add $delay $timeout)) -}}{{- end -}}
{{- if lt (int $v.worker.terminationGracePeriodSeconds) (add $delay $timeout (int $v.worker.maxCallSeconds)) -}}{{- fail (printf "worker.terminationGracePeriodSeconds must be at least shutdown.delay + shutdown.timeout + worker.maxCallSeconds (%d)" (add $delay $timeout (int $v.worker.maxCallSeconds))) -}}{{- end -}}
{{- if and (not $v.worker.connectorEgress) (not $v.worker.allowNoConnectorEgress) -}}{{- fail "worker.connectorEgress is empty: list the enterprise systems the worker may reach, or set worker.allowNoConnectorEgress=true" -}}{{- end -}}
```

(`shutdown.delay`/`timeout` must be whole seconds like `10s`; the helper strips the `s`.)

`deployments/helm/eacp/templates/serviceaccount.yaml`:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{ .Release.Name }}
  labels:
    {{- include "eacp.labels" (dict "root" . "component" "serviceaccount") | nindent 4 }}
automountServiceAccountToken: false
```

`deployments/helm/eacp/templates/api.yaml`:

```yaml
{{- $c := dict "root" . "component" "api" -}}
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Release.Name }}-api
  labels:
    {{- include "eacp.labels" $c | nindent 4 }}
spec:
  {{- if not .Values.autoscaling.enabled }}
  replicas: {{ .Values.api.replicas }}
  {{- end }}
  strategy:
    type: RollingUpdate
    rollingUpdate: {maxUnavailable: 0, maxSurge: 1}
  selector:
    matchLabels:
      {{- include "eacp.selector" $c | nindent 6 }}
  template:
    metadata:
      labels:
        {{- include "eacp.labels" $c | nindent 8 }}
    spec:
      {{- include "eacp.podSecurity" . | nindent 6 }}
      terminationGracePeriodSeconds: {{ .Values.api.terminationGracePeriodSeconds }}
      {{- include "eacp.spread" $c | nindent 6 }}
      containers:
        - name: controlplane-api
          image: {{ include "eacp.image" . }}
          imagePullPolicy: {{ .Values.image.pullPolicy }}
          command: ["/controlplane-api"]
          ports:
            - {name: http, containerPort: 8080}
          env:
            - {name: EACP_HTTP_ADDR, value: ":8080"}
            - {name: EACP_LOG_FORMAT, value: json}
            - name: EACP_DATABASE_URL
              valueFrom: {secretKeyRef: {name: {{ .Values.database.appSecret }}, key: url}}
            - {name: EACP_GOVERNANCE_PROVIDER, value: {{ .Values.governance.provider | quote }}}
            - {name: EACP_AGT_PDP_URL, value: "https://{{ .Release.Name }}-pdp:8443"}
            - {name: EACP_AGT_PDP_CA_FILE, value: /pki/ca.pem}
            - {name: EACP_AGT_PDP_CERT_FILE, value: /pki/client.pem}
            - {name: EACP_AGT_PDP_KEY_FILE, value: /pki/client-key.pem}
            {{- if .Values.nats.enabled }}
            - name: EACP_NATS_URL
              valueFrom: {secretKeyRef: {name: {{ .Values.nats.relaySecret }}, key: url}}
            {{- end }}
            {{- include "eacp.shutdownEnv" . | nindent 12 }}
            {{- include "eacp.extraEnv" .Values.api.env | nindent 12 }}
          readinessProbe: {httpGet: {path: /readyz, port: http}, periodSeconds: 5, failureThreshold: 2}
          livenessProbe: {httpGet: {path: /healthz, port: http}, periodSeconds: 10, failureThreshold: 3}
          startupProbe: {httpGet: {path: /healthz, port: http}, periodSeconds: 2, failureThreshold: 60}
          resources:
            {{- toYaml .Values.api.resources | nindent 12 }}
          {{- include "eacp.containerSecurity" . | nindent 10 }}
          volumeMounts:
            - {name: tmp, mountPath: /tmp}
            - {name: pki, mountPath: /pki, readOnly: true}
      volumes:
        - {name: tmp, emptyDir: {}}
        - name: pki
          projected:
            sources:
              - secret:
                  name: {{ .Values.pdp.tlsSecret }}
                  items:
                    - {key: ca.pem, path: ca.pem}
                    - {key: client.pem, path: client.pem}
                    - {key: client-key.pem, path: client-key.pem}
---
apiVersion: v1
kind: Service
metadata:
  name: {{ .Release.Name }}-api
  labels:
    {{- include "eacp.labels" $c | nindent 4 }}
spec:
  type: {{ .Values.api.service.type }}
  selector:
    {{- include "eacp.selector" $c | nindent 4 }}
  ports:
    - {name: http, port: 8080, targetPort: http}
```

`deployments/helm/eacp/templates/worker.yaml`:

```yaml
{{- $c := dict "root" . "component" "worker" -}}
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Release.Name }}-worker
  labels:
    {{- include "eacp.labels" $c | nindent 4 }}
spec:
  {{- if not .Values.autoscaling.enabled }}
  replicas: {{ .Values.worker.replicas }}
  {{- end }}
  strategy:
    type: RollingUpdate
    rollingUpdate: {maxUnavailable: 0, maxSurge: 1}
  selector:
    matchLabels:
      {{- include "eacp.selector" $c | nindent 6 }}
  template:
    metadata:
      labels:
        {{- include "eacp.labels" $c | nindent 8 }}
    spec:
      {{- include "eacp.podSecurity" . | nindent 6 }}
      terminationGracePeriodSeconds: {{ .Values.worker.terminationGracePeriodSeconds }}
      {{- include "eacp.spread" $c | nindent 6 }}
      containers:
        - name: execution-worker
          image: {{ include "eacp.image" . }}
          imagePullPolicy: {{ .Values.image.pullPolicy }}
          command: ["/execution-worker"]
          ports:
            - {name: http, containerPort: 8081}
          env:
            - {name: EACP_HTTP_ADDR, value: ":8081"}
            - {name: EACP_LOG_FORMAT, value: json}
            - name: EACP_WORKER_ID
              valueFrom: {fieldRef: {fieldPath: metadata.name}}
            - name: EACP_DATABASE_URL
              valueFrom: {secretKeyRef: {name: {{ .Values.database.appSecret }}, key: url}}
            - {name: EACP_CONNECTOR_SECRETS_FILE, value: /run/secrets/eacp/connector-secrets.json}
            {{- if .Values.nats.enabled }}
            - name: EACP_NATS_URL
              valueFrom: {secretKeyRef: {name: {{ .Values.nats.workerSecret }}, key: url}}
            {{- end }}
            {{- include "eacp.shutdownEnv" . | nindent 12 }}
            {{- include "eacp.extraEnv" .Values.worker.env | nindent 12 }}
          readinessProbe: {httpGet: {path: /readyz, port: http}, periodSeconds: 5, failureThreshold: 2}
          livenessProbe: {httpGet: {path: /healthz, port: http}, periodSeconds: 10, failureThreshold: 3}
          startupProbe: {httpGet: {path: /healthz, port: http}, periodSeconds: 2, failureThreshold: 60}
          resources:
            {{- toYaml .Values.worker.resources | nindent 12 }}
          {{- include "eacp.containerSecurity" . | nindent 10 }}
          volumeMounts:
            - {name: tmp, mountPath: /tmp}
            - {name: connector-secrets, mountPath: /run/secrets/eacp, readOnly: true}
      volumes:
        - {name: tmp, emptyDir: {}}
        - name: connector-secrets
          secret:
            secretName: {{ .Values.worker.connectorSecrets }}
            defaultMode: 0400
            items:
              - {key: connector-secrets.json, path: connector-secrets.json}
```

`deployments/helm/eacp/templates/pdp.yaml`:

```yaml
{{- $c := dict "root" . "component" "pdp" -}}
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Release.Name }}-pdp
  labels:
    {{- include "eacp.labels" $c | nindent 4 }}
spec:
  replicas: {{ .Values.pdp.replicas }}
  strategy:
    type: RollingUpdate
    rollingUpdate: {maxUnavailable: 0, maxSurge: 1}
  selector:
    matchLabels:
      {{- include "eacp.selector" $c | nindent 6 }}
  template:
    metadata:
      labels:
        {{- include "eacp.labels" $c | nindent 8 }}
    spec:
      {{- include "eacp.podSecurity" . | nindent 6 }}
      terminationGracePeriodSeconds: 30
      {{- include "eacp.spread" $c | nindent 6 }}
      containers:
        - name: agt-pdp
          image: {{ include "eacp.pdpImage" . }}
          imagePullPolicy: {{ .Values.pdp.image.pullPolicy }}
          ports:
            - {name: mtls, containerPort: 8443}
          env:
            - {name: AGT_PDP_LISTEN, value: "0.0.0.0:8443"}
            - name: AGT_PDP_INSTANCE_ID
              valueFrom: {fieldRef: {fieldPath: metadata.name}}
            - {name: AGT_PDP_TLS_CERT_FILE, value: /pki/server.pem}
            - {name: AGT_PDP_TLS_KEY_FILE, value: /pki/server-key.pem}
            - {name: AGT_PDP_TLS_CLIENT_CA_FILE, value: /pki/ca.pem}
            {{- include "eacp.extraEnv" .Values.pdp.env | nindent 12 }}
          # Its HTTP health needs a client certificate, so the probes are TCP.
          readinessProbe: {tcpSocket: {port: mtls}, periodSeconds: 5}
          livenessProbe: {tcpSocket: {port: mtls}, periodSeconds: 10}
          startupProbe: {tcpSocket: {port: mtls}, periodSeconds: 2, failureThreshold: 60}
          resources:
            {{- toYaml .Values.pdp.resources | nindent 12 }}
          {{- include "eacp.containerSecurity" . | nindent 10 }}
          volumeMounts:
            - {name: tmp, mountPath: /tmp}
            - {name: pki, mountPath: /pki, readOnly: true}
      volumes:
        - {name: tmp, emptyDir: {}}
        - name: pki
          projected:
            sources:
              - secret:
                  name: {{ .Values.pdp.tlsSecret }}
                  items:
                    - {key: ca.pem, path: ca.pem}
                    - {key: server.pem, path: server.pem}
                    - {key: server-key.pem, path: server-key.pem}
---
apiVersion: v1
kind: Service
metadata:
  name: {{ .Release.Name }}-pdp
  labels:
    {{- include "eacp.labels" $c | nindent 4 }}
spec:
  selector:
    {{- include "eacp.selector" $c | nindent 4 }}
  ports:
    - {name: mtls, port: 8443, targetPort: mtls}
```

`deployments/helm/eacp/templates/migrate.yaml`:

```yaml
{{- $c := dict "root" . "component" "migrate" -}}
apiVersion: batch/v1
kind: Job
metadata:
  name: {{ .Release.Name }}-migrate
  labels:
    {{- include "eacp.labels" $c | nindent 4 }}
  annotations:
    # Before any pod of the new release: services refuse an older schema.
    helm.sh/hook: pre-install,pre-upgrade
    helm.sh/hook-weight: "0"
    helm.sh/hook-delete-policy: before-hook-creation,hook-succeeded
spec:
  backoffLimit: {{ .Values.migrate.backoffLimit }}
  template:
    metadata:
      labels:
        {{- include "eacp.labels" $c | nindent 8 }}
    spec:
      {{- include "eacp.podSecurity" . | nindent 6 }}
      restartPolicy: Never
      containers:
        - name: migrate
          image: {{ include "eacp.image" . }}
          imagePullPolicy: {{ .Values.image.pullPolicy }}
          command: ["/eacpctl"]
          args: ["migrate", "up"]
          env:
            - {name: EACP_LOG_FORMAT, value: json}
            - name: EACP_DATABASE_URL
              valueFrom: {secretKeyRef: {name: {{ .Values.database.ownerSecret }}, key: url}}
          resources:
            {{- toYaml .Values.migrate.resources | nindent 12 }}
          {{- include "eacp.containerSecurity" . | nindent 10 }}
          volumeMounts:
            - {name: tmp, mountPath: /tmp}
      volumes:
        - {name: tmp, emptyDir: {}}
```

Note: the pre-install hook runs before the ServiceAccount exists. So the Job sets `serviceAccountName` only when not a hook? Ruling during execution if `helm install` fails: the migrate Job gets `serviceAccountName: default` with `automountServiceAccountToken: false` (a hook cannot use a chart-created SA on first install). Put this directly: in `migrate.yaml` override after the include with a local `eacp.podSecurityNoSA` variant — implement `eacp.podSecurity` taking a `sa` flag: `{{- include "eacp.podSecurity" (dict "root" . "sa" false) }}` for the Job and `(dict "root" . "sa" true)` elsewhere; the helper emits `serviceAccountName: {{ .root.Release.Name }}` only when `.sa`. (Update `_helpers.tpl` and all four callers accordingly — the test checks only `automountServiceAccountToken`.)

`deployments/k8s/e2e-values.yaml`:

```yaml
# Values for the minikube e2e (scripts/k8s-e2e.sh) and the chart render tests.
# DEVELOPMENT ONLY: the Secrets named here are created by the script from
# deployments/docker/secrets and eacpctl pdp-dev-certs.
image:
  repository: eacp
  tag: dev
  pullPolicy: Never
pdp:
  image: {repository: eacp-agt-pdp, tag: dev, pullPolicy: Never}
  tlsSecret: eacp-pdp-tls
database:
  appSecret: eacp-db-app
  ownerSecret: eacp-db-owner
  peers:
    - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: eacp-deps}}
      podSelector: {matchLabels: {app: postgres}}
nats:
  relaySecret: eacp-nats-relay
  workerSecret: eacp-nats-worker
  peers:
    - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: eacp-deps}}
      podSelector: {matchLabels: {app: nats}}
api:
  service: {type: NodePort}
worker:
  connectorSecrets: eacp-connector-secrets
  env:
    EACP_WORKER_LEASE: 10s          # the demo's timings (deployments/demo/compose.demo.yml)
    EACP_RECONCILE_MAX_ATTEMPTS: "3"
  connectorEgress:
    - to:
        - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: eacp-deps}}
          podSelector: {matchLabels: {app: fakeerp}}
      ports: [{protocol: TCP, port: 8090}]
    - to:
        - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: eacp-deps}}
          podSelector: {matchLabels: {app: fakemcp}}
      ports: [{protocol: TCP, port: 8091}]
```

(`EACP_WORKER_LEASE` must be an accepted setting: verify with `grep -n EACP_WORKER_LEASE internal/config/config.go`; if the key differs, use the real key — a ruling.)

- [ ] **Step 4: Run to verify it passes**

Run: `EACP_HELM_REQUIRED=1 go test -count=1 ./test/helm`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add deployments/helm deployments/k8s/e2e-values.yaml test/helm
git commit -m "feat(deploy): Helm chart workloads - hardened pods, secrets by reference, migration hook (ADR-029 Rev 1.1)"
```

---

### Task 2: NetworkPolicies

**Files:**
- Create: `deployments/helm/eacp/templates/networkpolicy.yaml`, `test/helm/network_test.go`

**Interfaces:**
- Consumes: Task 1 helpers and labels.
- Produces: NetworkPolicies `eacp-default-deny`, `eacp-dns`, `eacp-api`, `eacp-worker`, `eacp-pdp`, `eacp-migrate`.

- [ ] **Step 1: Write the failing test** — `test/helm/network_test.go`:

```go
package helm

import (
	"fmt"
	"strings"
	"testing"
)

func policy(t *testing.T, objs []object, name string) object {
	t.Helper()
	return find(t, objs, "NetworkPolicy", name)
}

func types(p object) string { return fmt.Sprint(list(p, "spec", "policyTypes")) }

func component(p object) any {
	return get(p, "spec", "podSelector", "matchLabels", "app.kubernetes.io/component")
}

func TestTheNamespaceDeniesEverythingByDefault(t *testing.T) {
	p := policy(t, render(t), "eacp-default-deny")
	if len(get(p, "spec", "podSelector").(map[string]any)) != 0 || types(p) != "[Ingress Egress]" ||
		get(p, "spec", "ingress") != nil || get(p, "spec", "egress") != nil {
		t.Fatalf("default deny = %v", p["spec"])
	}
}

func TestChartPodsResolveNamesOnly(t *testing.T) {
	p := policy(t, render(t), "eacp-dns")
	s := fmt.Sprint(p["spec"])
	if !strings.Contains(s, "k8s-app:kube-dns") || !strings.Contains(s, "port:53") || types(p) != "[Egress]" {
		t.Fatalf("dns policy = %v", s)
	}
}

func TestTheAPIIsTheFrontDoorAndReachesOnlyItsDependencies(t *testing.T) {
	p := policy(t, render(t), "eacp-api")
	if component(p) != "api" || types(p) != "[Ingress Egress]" {
		t.Fatalf("api policy = %v", p["spec"])
	}
	in := list(p, "spec", "ingress")
	if len(in) != 1 || fmt.Sprint(get(in[0], "ports")) != "[map[port:8080 protocol:TCP]]" || get(in[0], "from") != nil {
		t.Fatalf("api ingress = %v", in)
	}
	eg := fmt.Sprint(list(p, "spec", "egress"))
	for _, want := range []string{"app:postgres", "port:5432", "app:nats", "port:4222",
		"app.kubernetes.io/component:pdp", "port:8443"} {
		if !strings.Contains(eg, want) {
			t.Errorf("api egress lacks %s: %s", want, eg)
		}
	}
	if strings.Contains(eg, "fakeerp") || strings.Contains(eg, "8090") {
		t.Errorf("the API may reach the ERP: %s", eg)
	}
}

func TestOnlyTheWorkerReachesEnterpriseSystems(t *testing.T) {
	objs := render(t)
	w := policy(t, objs, "eacp-worker")
	if component(w) != "worker" || get(w, "spec", "ingress") != nil || types(w) != "[Ingress Egress]" {
		t.Fatalf("worker policy = %v", w["spec"])
	}
	eg := fmt.Sprint(list(w, "spec", "egress"))
	for _, want := range []string{"app:fakeerp", "port:8090", "app:fakemcp", "port:8091", "app:postgres", "app:nats"} {
		if !strings.Contains(eg, want) {
			t.Errorf("worker egress lacks %s: %s", want, eg)
		}
	}
	for _, name := range []string{"eacp-api", "eacp-pdp", "eacp-migrate", "eacp-dns"} {
		if s := fmt.Sprint(policy(t, objs, name)["spec"]); strings.Contains(s, "fakeerp") || strings.Contains(s, "8090") {
			t.Errorf("%s reaches the ERP: %s", name, s)
		}
	}
}

func TestOnlyTheAPIReachesThePDP(t *testing.T) {
	p := policy(t, render(t), "eacp-pdp")
	in := list(p, "spec", "ingress")
	if component(p) != "pdp" || types(p) != "[Ingress Egress]" || get(p, "spec", "egress") != nil || len(in) != 1 ||
		fmt.Sprint(get(in[0], "from")) != "[map[podSelector:map[matchLabels:map[app.kubernetes.io/component:api app.kubernetes.io/instance:eacp app.kubernetes.io/name:eacp]]]]" ||
		fmt.Sprint(get(in[0], "ports")) != "[map[port:8443 protocol:TCP]]" {
		t.Fatalf("pdp policy = %v", p["spec"])
	}
}

func TestMigrationsReachOnlyPostgres(t *testing.T) {
	p := policy(t, render(t), "eacp-migrate")
	eg := list(p, "spec", "egress")
	if component(p) != "migrate" || len(eg) != 1 || !strings.Contains(fmt.Sprint(eg), "app:postgres") {
		t.Fatalf("migrate policy = %v", p["spec"])
	}
}

func TestAPIIngressCanBeNarrowed(t *testing.T) {
	p := policy(t, render(t, "--set-json", `api.ingress.from=[{"namespaceSelector":{"matchLabels":{"team":"agents"}}}]`), "eacp-api")
	if s := fmt.Sprint(list(p, "spec", "ingress")); !strings.Contains(s, "team:agents") {
		t.Fatalf("narrowed ingress = %s", s)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `EACP_HELM_REQUIRED=1 go test -count=1 ./test/helm -run 'Namespace|DNS|Front|Enterprise|PDP|Migrations|Narrowed'`
Expected: FAIL (`no NetworkPolicy eacp-default-deny rendered`).

- [ ] **Step 3: Implement** — `deployments/helm/eacp/templates/networkpolicy.yaml`:

```yaml
{{- $r := .Release.Name -}}
{{- $v := .Values -}}
# Every pod in the release namespace: nothing in, nothing out, unless allowed below.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: {{ $r }}-default-deny
  labels:
    {{- include "eacp.labels" (dict "root" . "component" "network") | nindent 4 }}
spec:
  podSelector: {}
  policyTypes: [Ingress, Egress]
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: {{ $r }}-dns
  labels:
    {{- include "eacp.labels" (dict "root" . "component" "network") | nindent 4 }}
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: eacp
      app.kubernetes.io/instance: {{ $r }}
  policyTypes: [Egress]
  egress:
    - to:
        - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: kube-system}}
          podSelector: {matchLabels: {k8s-app: kube-dns}}
      ports:
        - {protocol: UDP, port: 53}
        - {protocol: TCP, port: 53}
---
# The authenticated front door (ADR-001 §3): agents and operators reach only this.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: {{ $r }}-api
  labels:
    {{- include "eacp.labels" (dict "root" . "component" "network") | nindent 4 }}
spec:
  podSelector:
    matchLabels:
      {{- include "eacp.selector" (dict "root" . "component" "api") | nindent 6 }}
  policyTypes: [Ingress, Egress]
  ingress:
    - {{- with $v.api.ingress.from }}
      from:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      ports:
        - {protocol: TCP, port: 8080}
  egress:
    - to:
        {{- toYaml $v.database.peers | nindent 8 }}
      ports: [{protocol: TCP, port: {{ $v.database.port }}}]
    {{- if $v.nats.enabled }}
    - to:
        {{- toYaml $v.nats.peers | nindent 8 }}
      ports: [{protocol: TCP, port: {{ $v.nats.port }}}]
    {{- end }}
    - to:
        - podSelector:
            matchLabels:
              {{- include "eacp.selector" (dict "root" . "component" "pdp") | nindent 14 }}
      ports: [{protocol: TCP, port: 8443}]
    {{- with $v.otel.peers }}
    - to:
        {{- toYaml . | nindent 8 }}
      {{- with $v.otel.ports }}
      ports:
        {{- toYaml . | nindent 8 }}
      {{- end }}
    {{- end }}
---
# Only the worker holds connector credentials and reaches enterprise systems.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: {{ $r }}-worker
  labels:
    {{- include "eacp.labels" (dict "root" . "component" "network") | nindent 4 }}
spec:
  podSelector:
    matchLabels:
      {{- include "eacp.selector" (dict "root" . "component" "worker") | nindent 6 }}
  policyTypes: [Ingress, Egress]
  egress:
    - to:
        {{- toYaml $v.database.peers | nindent 8 }}
      ports: [{protocol: TCP, port: {{ $v.database.port }}}]
    {{- if $v.nats.enabled }}
    - to:
        {{- toYaml $v.nats.peers | nindent 8 }}
      ports: [{protocol: TCP, port: {{ $v.nats.port }}}]
    {{- end }}
    {{- with $v.worker.connectorEgress }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
    {{- with $v.otel.peers }}
    - to:
        {{- toYaml . | nindent 8 }}
      {{- with $v.otel.ports }}
      ports:
        {{- toYaml . | nindent 8 }}
      {{- end }}
    {{- end }}
---
# The PDP answers the API only, over mutual TLS, and calls nothing.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: {{ $r }}-pdp
  labels:
    {{- include "eacp.labels" (dict "root" . "component" "network") | nindent 4 }}
spec:
  podSelector:
    matchLabels:
      {{- include "eacp.selector" (dict "root" . "component" "pdp") | nindent 6 }}
  policyTypes: [Ingress, Egress]
  ingress:
    - from:
        - podSelector:
            matchLabels:
              {{- include "eacp.selector" (dict "root" . "component" "api") | nindent 14 }}
      ports: [{protocol: TCP, port: 8443}]
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: {{ $r }}-migrate
  labels:
    {{- include "eacp.labels" (dict "root" . "component" "network") | nindent 4 }}
spec:
  podSelector:
    matchLabels:
      {{- include "eacp.selector" (dict "root" . "component" "migrate") | nindent 6 }}
  policyTypes: [Ingress, Egress]
  egress:
    - to:
        {{- toYaml $v.database.peers | nindent 8 }}
      ports: [{protocol: TCP, port: {{ $v.database.port }}}]
```

Also add to `validate.yaml`: `{{- if not $v.database.peers -}}{{- fail "database.peers must select PostgreSQL for NetworkPolicy egress" -}}{{- end -}}` and the same for `nats.peers` when `nats.enabled`, with fixtures `database.peers=null` → "database.peers" in `TestDangerousValuesAreRefused` (add the map entries `"database.peers=null": "database.peers"`).

- [ ] **Step 4: Run to verify it passes**

Run: `EACP_HELM_REQUIRED=1 go test -count=1 ./test/helm`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add deployments/helm test/helm
git commit -m "feat(deploy): NetworkPolicies mirroring the compose networks (ADR-001 §3, ADR-029 Rev 1.1)"
```

---

### Task 3: Availability: disruption budgets, rollout, grace, autoscaling

**Files:**
- Create: `deployments/helm/eacp/templates/pdb.yaml`, `templates/hpa.yaml`, `test/helm/availability_test.go`

- [ ] **Step 1: Write the failing test** — `test/helm/availability_test.go`:

```go
package helm

import (
	"fmt"
	"strings"
	"testing"
)

func TestDisruptionBudgetsKeepAllButOne(t *testing.T) {
	objs := render(t)
	for _, c := range []string{"api", "worker", "pdp"} {
		p := find(t, objs, "PodDisruptionBudget", "eacp-"+c)
		if get(p, "spec", "maxUnavailable") != 1 ||
			get(p, "spec", "selector", "matchLabels", "app.kubernetes.io/component") != c {
			t.Errorf("pdb %s = %v", c, p["spec"])
		}
	}
}

func TestRolloutsNeverDropBelowTheReplicas(t *testing.T) {
	objs := render(t)
	for _, c := range []string{"api", "worker", "pdp"} {
		d := find(t, objs, "Deployment", "eacp-"+c)
		if fmt.Sprint(get(d, "spec", "strategy")) != "map[rollingUpdate:map[maxSurge:1 maxUnavailable:0] type:RollingUpdate]" {
			t.Errorf("%s strategy = %v", c, get(d, "spec", "strategy"))
		}
		if get(d, "spec", "replicas") != 2 {
			t.Errorf("%s replicas = %v", c, get(d, "spec", "replicas"))
		}
		if s := fmt.Sprint(get(podSpec(d), "topologySpreadConstraints")); !strings.Contains(s, "kubernetes.io/hostname") {
			t.Errorf("%s is not spread across nodes: %s", c, s)
		}
	}
	for c, grace := range map[string]int{"api": 30, "worker": 60} {
		if g := podSpec(find(t, objs, "Deployment", "eacp-"+c))["terminationGracePeriodSeconds"]; g != grace {
			t.Errorf("%s grace = %v, want %d", c, g, grace)
		}
	}
}

func TestGraceMustCoverTheDrain(t *testing.T) {
	for set, msg := range map[string]string{
		"worker.terminationGracePeriodSeconds=54": "worker.terminationGracePeriodSeconds must be at least",
		"api.terminationGracePeriodSeconds=24":    "api.terminationGracePeriodSeconds must be at least",
		"worker.maxCallSeconds=120":               "worker.terminationGracePeriodSeconds must be at least",
	} {
		if out, err := renderErr(t, "--set", set); err == nil || !strings.Contains(out, msg) {
			t.Errorf("--set %s: err = %v, output lacks %q", set, err, msg)
		}
	}
	if _, err := renderErr(t, "--set", "worker.terminationGracePeriodSeconds=55"); err != nil {
		t.Errorf("grace exactly delay+timeout+call refused: %v", err)
	}
}

func TestAutoscalingIsOffByDefault(t *testing.T) {
	if hpas := all(render(t), "HorizontalPodAutoscaler"); len(hpas) != 0 {
		t.Fatalf("HPAs rendered by default: %d", len(hpas))
	}
	objs := render(t, "--set", "autoscaling.enabled=true")
	for _, c := range []string{"api", "worker"} {
		h := find(t, objs, "HorizontalPodAutoscaler", "eacp-"+c)
		if get(h, "spec", "scaleTargetRef", "name") != "eacp-"+c || get(h, "spec", "minReplicas") != 2 ||
			!strings.Contains(fmt.Sprint(get(h, "spec", "metrics")), "cpu") {
			t.Errorf("hpa %s = %v", c, h["spec"])
		}
		if get(find(t, objs, "Deployment", "eacp-"+c), "spec", "replicas") != nil {
			t.Errorf("%s sets replicas while an HPA owns them", c)
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `EACP_HELM_REQUIRED=1 go test -count=1 ./test/helm -run 'Budgets|Rollouts|Grace|Autoscaling'`
Expected: FAIL (`no PodDisruptionBudget eacp-api rendered`, `no HorizontalPodAutoscaler`). The grace test may already pass (Task 1 validation) — that is fine; it pins it.

- [ ] **Step 3: Implement**

`templates/pdb.yaml`:

```yaml
{{- range $c := list "api" "worker" "pdp" }}
---
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: {{ $.Release.Name }}-{{ $c }}
  labels:
    {{- include "eacp.labels" (dict "root" $ "component" $c) | nindent 4 }}
spec:
  maxUnavailable: 1
  selector:
    matchLabels:
      {{- include "eacp.selector" (dict "root" $ "component" $c) | nindent 6 }}
{{- end }}
```

`templates/hpa.yaml`:

```yaml
{{- if .Values.autoscaling.enabled }}
{{- range $c := list "api" "worker" }}
{{- $a := index $.Values.autoscaling $c }}
---
# CPU only: queue-depth scaling is deferred (ADR-029 Rev 1.1).
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: {{ $.Release.Name }}-{{ $c }}
  labels:
    {{- include "eacp.labels" (dict "root" $ "component" $c) | nindent 4 }}
spec:
  scaleTargetRef: {apiVersion: apps/v1, kind: Deployment, name: {{ $.Release.Name }}-{{ $c }}}
  minReplicas: {{ $a.minReplicas }}
  maxReplicas: {{ $a.maxReplicas }}
  metrics:
    - type: Resource
      resource:
        name: cpu
        target: {type: Utilization, averageUtilization: {{ $a.targetCPUUtilizationPercentage }}}
{{- end }}
{{- end }}
```

- [ ] **Step 4: Run to verify it passes**

Run: `EACP_HELM_REQUIRED=1 go test -count=1 ./test/helm`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add deployments/helm test/helm
git commit -m "feat(deploy): disruption budgets, safe rollouts, drain-covering grace, optional CPU autoscaling"
```

---

### Task 4: Dev dependencies, e2e script, first cluster install

**Files:**
- Create: `deployments/k8s/dev/namespaces.yaml`, `postgres.yaml`, `nats.yaml`, `fakes.yaml`, `agent.yaml`, `aliases.yaml`, `policies.yaml`
- Create: `scripts/k8s-e2e.sh`
- Modify: `deployments/docker/secrets/connector-secrets.dev.json` (tenant `…e2`)

- [ ] **Step 1: Write the manifests.** Namespaces `eacp` (labels `pod-security.kubernetes.io/enforce: restricted`, `pod-security.kubernetes.io/enforce-version: latest`), `eacp-deps`, `agents`. In `eacp-deps`: `postgres` StatefulSet (image `postgres:18-alpine`, `imagePullPolicy: Never`, env from Secret `postgres-bootstrap` keys `POSTGRES_PASSWORD`, `EACP_OWNER_PASSWORD`, `EACP_APP_PASSWORD`, initdb ConfigMap `postgres-initdb` = content of `deployments/docker/postgres/initdb/01-roles.sh` (created by the script with `--from-file`), a 1 Gi PVC, `pg_isready` exec probes, Service `postgres` 5432); `nats` Deployment (`nats:2.15.0-alpine`, config from ConfigMap `nats-config` created by the script from `deployments/docker/nats/nats.conf`, emptyDir `/data`, Service `nats` 4222); `fakeerp` and `fakemcp` Deployments (image `eacp:dev`, commands `/fakeerp`, `/fakemcp`, env as in docker-compose.yml, tokens from Secrets `fakeerp-token`/`fakemcp-token` mounted at `/run/secrets/…`, emptyDir data dirs, Services `fakeerp` 8090 and `fakemcp` 8091, labels `app: fakeerp|fakemcp`). In `agents`: `agent` Deployment (`busybox:1.37`, `sleep infinity`, restricted-compatible securityContext). `aliases.yaml`: ExternalName Services `fakeerp`, `fakemcp` in `eacp` and `agents` → `fakeerp.eacp-deps.svc.cluster.local` etc., `controlplane-api` in `agents` → `eacp-api.eacp.svc.cluster.local`. `policies.yaml` in `eacp-deps`: default deny ingress; postgres ingress from namespace `eacp` pods with component in (api, worker, migrate) on 5432; nats from api, worker on 4222; fakeerp/fakemcp from worker on 8090/8091. Every dev pod: non-root where the image allows (postgres runs as its image user 70 with `fsGroup: 70`; nats as 1000), `DEVELOPMENT ONLY` comments. Exact YAML is written at execution; `kubectl apply --dry-run=server` must accept it (Step 3).

- [ ] **Step 2: Write `scripts/k8s-e2e.sh`** doing spec §3.6 steps 1–7:

```bash
#!/usr/bin/env bash
# Phase 23b end-to-end run on a 2-node minikube cluster with Calico
# (docs/KUBERNETES.md, ADR-029 Rev 1.1). DEVELOPMENT ONLY.
#
#   scripts/k8s-e2e.sh           full run, then delete the profile
#   KEEP=1 scripts/k8s-e2e.sh    leave the cluster running
#   TESTS=TestSliceADemo ...     choose the Go tests (default: both)
set -euo pipefail
cd "$(dirname "$0")/.."
export MSYS_NO_PATHCONV=1
PROFILE=${PROFILE:-eacp-e2e}
HELM=${HELM:-.tools/helm}
[ -x "$HELM" ] || HELM=.tools/helm.exe
[ -x "$HELM" ] || HELM=helm
k() { kubectl --context "$PROFILE" "$@"; }

minikube status -p "$PROFILE" >/dev/null 2>&1 ||
	minikube start -p "$PROFILE" --nodes 2 --cni calico --driver docker --cpus 3 --memory 2800

echo "==> Building images and loading them into $PROFILE"
python3 deployments/docker/secrets/prepare_fakeerp_token.py 2>/dev/null || python deployments/docker/secrets/prepare_fakeerp_token.py
docker build -q -t eacp:dev -f deployments/docker/Dockerfile .
docker build -q -t eacp-agt-pdp:dev -f sidecars/agt-pdp/Dockerfile --target runtime .
for img in eacp:dev eacp-agt-pdp:dev postgres:18-alpine nats:2.15.0-alpine busybox:1.37; do
	docker image inspect "$img" >/dev/null 2>&1 || docker pull -q "$img"
	minikube -p "$PROFILE" image load "$img"
done

echo "==> Namespaces, dev secrets and dependencies"
k apply -f deployments/k8s/dev/namespaces.yaml
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
docker run --rm -v "$(cd "$work" && pwd -W 2>/dev/null || pwd):/out" --user "$(id -u 2>/dev/null || echo 0)" eacp:dev \
	/eacpctl pdp-dev-certs --dir /out --name eacp-pdp --name eacp-pdp.eacp.svc --name eacp-pdp.eacp.svc.cluster.local
secret() { k -n "$1" create secret generic "$2" "${@:3}" --dry-run=client -o yaml | k apply -f -; }
secret eacp eacp-pdp-tls --from-file="$work/ca.pem" --from-file="$work/client.pem" --from-file="$work/client-key.pem" \
	--from-file="$work/server.pem" --from-file="$work/server-key.pem"
secret eacp eacp-db-app --from-literal=url='postgres://eacp_app:eacp_app_dev@postgres.eacp-deps.svc:5432/eacp?sslmode=disable'
secret eacp eacp-db-owner --from-literal=url='postgres://eacp_owner:eacp_owner_dev@postgres.eacp-deps.svc:5432/eacp?sslmode=disable'
secret eacp eacp-nats-relay --from-literal=url='nats://relay:relay_dev@nats.eacp-deps.svc:4222'
secret eacp eacp-nats-worker --from-literal=url='nats://worker:worker_dev@nats.eacp-deps.svc:4222'
secret eacp eacp-connector-secrets --from-file=connector-secrets.json=deployments/docker/secrets/connector-secrets.dev.json
secret eacp-deps postgres-bootstrap --from-literal=POSTGRES_PASSWORD=postgres \
	--from-literal=EACP_OWNER_PASSWORD=eacp_owner_dev --from-literal=EACP_APP_PASSWORD=eacp_app_dev
secret eacp-deps fakeerp-token --from-file=token=deployments/docker/secrets/fakeerp-token.dev
secret eacp-deps fakemcp-token --from-file=token=deployments/docker/secrets/fakemcp-token.dev
k -n eacp-deps create configmap postgres-initdb --from-file=deployments/docker/postgres/initdb/01-roles.sh \
	--dry-run=client -o yaml | k apply -f -
k -n eacp-deps create configmap nats-config --from-file=nats.conf=deployments/docker/nats/nats.conf \
	--dry-run=client -o yaml | k apply -f -
k apply -f deployments/k8s/dev/
k -n eacp-deps rollout status statefulset/postgres --timeout=300s
for d in nats fakeerp fakemcp; do k -n eacp-deps rollout status "deploy/$d" --timeout=300s; done
k -n agents rollout status deploy/agent --timeout=300s

echo "==> helm upgrade --install eacp"
"$HELM" upgrade --install eacp deployments/helm/eacp -n eacp -f deployments/k8s/e2e-values.yaml \
	--kube-context "$PROFILE" --wait --timeout 10m

echo "==> Tunnel to the API"
minikube -p "$PROFILE" service eacp-api -n eacp --url > "$work/api-url" 2>"$work/tunnel.log" &
tunnel=$!
trap 'kill $tunnel 2>/dev/null || true; rm -rf "$work"' EXIT
for _ in $(seq 60); do grep -q '^http' "$work/api-url" 2>/dev/null && break; sleep 1; done
api=$(grep -m1 '^http' "$work/api-url")
echo "    API at $api"

status=0
EACP_DEMO=1 EACP_DEMO_PLATFORM=k8s EACP_DEMO_API="$api" EACP_DEMO_KUBE_CONTEXT="$PROFILE" \
	go test -count=1 -v -timeout 40m -run "${TESTS:-TestSliceADemo|TestKubernetesDisruption}" ./test/demo || status=$?

if [ "${KEEP:-}" = 1 ]; then
	echo "==> Cluster $PROFILE left running (API $api while this shell's tunnel lived)"
else
	minikube delete -p "$PROFILE"
fi
exit "$status"
```

(Adjust at execution: `pwd -W` gives a Windows path for docker volume mounts in Git Bash; the certs container writes as nonroot, so the mount dir must be writable — if not, run with `--user 0` a ruling; the PDP cert file names must match `ca.pem`, `client.pem`, `client-key.pem`, `server.pem`, `server-key.pem`.)

Add tenant `00000000-0000-4000-8000-0000000000e2` bound to `fakeerp` (host `fakeerp:8090`, same value) to `connector-secrets.dev.json`.

- [ ] **Step 3: Bring the cluster up (no Go tests yet)**

Run: `TESTS=NONE KEEP=1 bash scripts/k8s-e2e.sh`
Expected: images load, dependencies roll out, `helm upgrade --install … --wait` succeeds (migrate hook completes; api, worker, pdp Ready under the restricted Pod Security Standard); `go test -run NONE` passes trivially. Then `kubectl --context eacp-e2e -n eacp get pods -o wide` shows 2 api, 2 worker, 2 pdp spread over 2 nodes. Any failure here is a finding: fix the chart test-first when the render test can express it (add the assertion to `test/helm` first), otherwise ledger the manifest fix.

- [ ] **Step 4: Commit**

```bash
git add deployments/k8s scripts/k8s-e2e.sh deployments/docker/secrets/connector-secrets.dev.json deployments/helm test/helm
git commit -m "feat(deploy): dev dependencies and the minikube e2e script; the chart installs under the restricted Pod Security Standard"
```

---

### Task 5: The demo on Kubernetes and the disruption test

**Files:**
- Create: `test/demo/platform_test.go` (interface + compose + k8s implementations)
- Create: `test/demo/k8s_test.go` (`TestKubernetesDisruption`)
- Modify: `test/demo/demo_test.go`, `test/demo/slice_c_test.go` (use `d.p` instead of compose calls)

**Interfaces:**
- Produces:

```go
type platform interface {
	name() string
	eacpctl(args ...string) (string, error)       // eacpctl with the owner DSN
	restart(services ...string)                   // compose service names
	kill(service string)                          // SIGKILL every process of it
	stop(service string)
	start(service string)                         // after kill or stop; waits until ready
	agent(args ...string) (string, error)         // run in the stand-in agent
	postgres(args ...string) (string, error)      // run in the PostgreSQL container
	logs() string                                 // every EACP and dependency log
	erpAudit(token string) ([]byte, error)        // GET fakeerp /v1/audit with the ERP credential
	copyToFakeMCP(local, remote string) error     // Slice C only
}
```

Compose service names are the vocabulary: `controlplane-api`, `execution-worker`, `agt-pdp`, `postgres`, `nats`, `fakeerp`, `fakemcp`. The k8s implementation maps them to `deploy/eacp-api`, `deploy/eacp-worker`, `deploy/eacp-pdp` (namespace `eacp`), `statefulset/postgres`, `deploy/nats`, `deploy/fakeerp`, `deploy/fakemcp` (namespace `eacp-deps`).

- [ ] **Step 1: Refactor (compose behaviour unchanged).** Move `compose`, `composeErr` into `composePlatform` in `platform_test.go`; `newDemo` picks `k8sPlatform` when `EACP_DEMO_PLATFORM=k8s` (else compose). Replace each call site:

| Old | New |
|---|---|
| `d.compose("restart", a, b, c)` | `d.p.restart(a, b, c)` |
| `d.compose("kill", s)` / `("start", s)` / `("stop", s)` | `d.p.kill(s)` / `d.p.start(s)` / `d.p.stop(s)` |
| `d.composeErr("run","--rm","migrate","/eacpctl", args...)` | `d.p.eacpctl(args...)` |
| `d.composeErr("exec","-T","agent", args...)` | `d.p.agent(args...)` |
| `d.compose("exec","-T","postgres", args...)` | `d.p.postgres(args...)` (fatal on error at the call site) |
| `d.compose("logs", …)` | `d.p.logs()` |
| `docker run … busybox wget …/v1/audit` | `d.p.erpAudit(d.token)` |
| `d.compose("cp", rel, "fakemcp:/data/tools.json")` | `d.p.copyToFakeMCP(rel, "/data/tools.json")` |

`TestSliceCDemo` starts with `if d.p.name() == "k8s" { t.Skip("Slice C copies a file into the distroless Fake MCP pod; run it on compose (scripts/demo.sh)") }`.

k8s implementation essentials (`kubectl --context $EACP_DEMO_KUBE_CONTEXT`):
- `eacpctl`: `kubectl -n eacp run eacpctl-<random> --rm -i --restart=Never --image eacp:dev --image-pull-policy Never --labels app.kubernetes.io/name=eacp,app.kubernetes.io/instance=eacp,app.kubernetes.io/component=migrate --overrides <JSON>` where the overrides give the restricted securityContext, `automountServiceAccountToken: false`, command `/eacpctl` + args, and `EACP_DATABASE_URL` from Secret `eacp-db-owner`.
- `restart`: `rollout restart` each (postgres: `delete pod postgres-0`), then `rollout status --timeout=5m` each.
- `kill`: `delete pod -l app.kubernetes.io/component=<c> --grace-period=0 --force` (postgres/nats/fakes: `-l app=<name>` in `eacp-deps`); `start` after `kill`: `rollout status`.
- `stop`: remember replicas (`get deploy -o jsonpath={.spec.replicas}`), `scale --replicas=0`, wait until no pod remains; `start`: scale back, `rollout status`.
- `agent`: `kubectl -n agents exec deploy/agent -- args…`.
- `postgres`: `kubectl -n eacp-deps exec postgres-0 -- args…`.
- `logs`: `kubectl logs --all-containers --prefix --tail=-1 -l app.kubernetes.io/instance=eacp -n eacp` plus the four dependency deployments.
- `erpAudit`: `kubectl -n eacp-deps port-forward svc/fakeerp 0:8090` (reads the chosen port from its first output line; port-forward is not subject to NetworkPolicy), GET with the bearer token, stop the forward.
- `copyToFakeMCP`: returns an error ("not supported on k8s").

Verify compose is unchanged: `bash scripts/demo.sh` (both demos) must pass before the k8s run. Expected: PASS.

- [ ] **Step 2: Write `TestKubernetesDisruption`** in `test/demo/k8s_test.go`:
  - skip unless `EACP_DEMO=1` and platform k8s;
  - `d := newDemo(t, "00000000-0000-4000-8000-0000000000e2")`, `d.tenantWithCast("globex", "Globex", <same cast as Slice A>)`, the Slice A policy (allow `target: erp`), `d.register()`;
  - start a prober goroutine: `GET d.api+"/healthz"` every 100 ms with a 2 s timeout, counting failures, until stopped;
  - submit 30 actions (`erp.create_po`; payloads alternate `{}` and `{"scenario":"slow_response","delay_ms":1500}` so calls are in flight) in the background, 200 ms apart;
  - meanwhile: `kubectl rollout restart deploy/eacp-api` + status; `kubectl scale deploy/eacp-worker --replicas=3` + status; `kubectl drain <second node> --ignore-daemonsets --delete-emptydir-data --timeout=5m`, then `kubectl uncordon`;
  - `d.until(id, "SUCCEEDED")` for all; `d.onePO(id)` for all;
  - prober failures == 0 (log the count and duration);
  - isolation: from `d.p.agent`: `wget` `http://controlplane-api:8080/healthz` succeeds (positive control); `nc -z -w 3` fails for `eacp-pdp.eacp.svc 8443`, `postgres.eacp-deps.svc 5432`, `nats.eacp-deps.svc 4222`, `fakeerp 8090`, and a worker pod IP on 8081 (IP from `kubectl get pod -l …component=worker -o jsonpath`); the positive nc control `nc -z -w 3 controlplane-api 8080` succeeds;
  - pod security: `kubectl -n eacp get pods -o json`: every container `readOnlyRootFilesystem: true` and pod `runAsNonRoot: true`; namespace label `pod-security.kubernetes.io/enforce=restricted`;
  - restore workers to 2.

- [ ] **Step 3: Run the real e2e**

Run: `KEEP=1 bash scripts/k8s-e2e.sh`
Expected: `TestSliceADemo` PASS on k8s, `TestKubernetesDisruption` PASS. A failure is a finding: debug systematically; a chart defect is fixed test-first in `test/helm` where expressible; a demo/platform defect is fixed in `test/demo`; any change to the demo's assertions for k8s needs a ledgered ruling (never weaken what compose asserts).

- [ ] **Step 4: Commit**

```bash
git add test/demo deployments scripts
git commit -m "test(demo): the Slice A demo and a disruption run on a 2-node minikube cluster"
```

---

### Task 6: Documentation

**Files:**
- Create: `docs/KUBERNETES.md`
- Modify: `docs/adr/ADR-029-high-availability.md` (Rev 1.1 section + assumptions), `docs/adr/README.md` (Rev), `docs/MASTER_PLAN.md` §95 status, `AGENTS.md` (status, rule, commands), `README.md`, `docs/INVARIANTS.md` (the k8s disruption test is gated like the demo: mention in the Phase 23 paragraph only).

- [ ] **Step 1:** `docs/KUBERNETES.md`: prerequisites (pinned Helm: download URL, sha256, `.tools/`), the Secrets and their keys, values that shape the boundary, install commands, upgrade (migration hook), what the NetworkPolicies allow, PDB/rollout/grace rules, autoscaling stance, the e2e (`scripts/k8s-e2e.sh`, resources, `KEEP`, `TESTS`), and what is out of scope.
- [ ] **Step 2:** ADR-029 Rev 1.1: a "Kubernetes (Phase 23b)" decision section (chart shape, secret custody, NetworkPolicies, availability, autoscaling deferred, the proof) and new rows in the assumptions table (spec §5).
- [ ] **Step 3:** MASTER_PLAN §95 status → 23b delivered; AGENTS.md status line, a rule bullet ("The Helm chart (ADR-029 Rev 1.1) never renders a secret value, keeps the compose boundary with NetworkPolicies and refuses dangerous values at render time; change it only with `test/helm` (EACP_HELM_REQUIRED=1) and rerun `scripts/k8s-e2e.sh`."), commands (`EACP_HELM_REQUIRED=1 go test ./test/helm`, `scripts/k8s-e2e.sh`), layout lines (`deployments/helm`, `deployments/k8s`, `test/helm`); README Phase 23 section extended.
- [ ] **Step 4: Verify and commit**

Run: `go vet ./... && EACP_HELM_REQUIRED=1 go test -race ./test/helm ./test/invariants ./test/demo`
Expected: PASS (demo tests skip without `EACP_DEMO`).

```bash
git add docs AGENTS.md README.md
git commit -m "docs: Kubernetes deployment guide; ADR-029 Rev 1.1; Phase 23 delivered"
```
