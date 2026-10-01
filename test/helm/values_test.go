package helm

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// envDefs returns every value the first container of o defines for name
// (Kubernetes keeps the last of duplicates, so there must be exactly one).
func envDefs(t *testing.T, o object, name string) []string {
	t.Helper()
	var out []string
	for _, e := range list(list(podSpec(o), "containers")[0], "env") {
		if get(e, "name") == name {
			out = append(out, fmt.Sprint(get(e, "value")))
		}
	}
	return out
}

func defaults(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root(t), "deployments", "helm", "eacp", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// ADR-014 §6 requires tls:// NATS outside development and test; the binary
// defaults EACP_ENV to development, so the chart must set it.
func TestTheEnvironmentIsProductionUnlessSet(t *testing.T) {
	if env := defaults(t)["environment"]; env != "production" {
		t.Fatalf("values.yaml environment = %v, want production", env)
	}
	for set, want := range map[string]string{"": "development" /* e2e values */, "environment=staging": "staging"} {
		var extra []string
		if set != "" {
			// A staging fixture cannot retain the development-only HTTP ingress.
			extra = []string{"--set", set, "--set", "api.a2aPublicURL=https://eacp.example.test/a2a"}
		}
		objs := render(t, extra...)
		for _, name := range []string{"eacp-api", "eacp-worker"} {
			if got := envDefs(t, find(t, objs, "Deployment", name), "EACP_ENV"); fmt.Sprint(got) != "["+want+"]" {
				t.Errorf("%s with %q: EACP_ENV = %v, want %s", name, set, got, want)
			}
		}
	}
}

// A private CA for NATS or PostgreSQL is mounted only where that
// dependency is used, and only when named.
func TestDependencyCAsReachOnlyTheirClients(t *testing.T) {
	objs := render(t, "--set", "nats.caSecret=eacp-nats-ca", "--set", "database.caSecret=eacp-db-ca")
	want := map[string]string{"api": "eacp-db-ca eacp-nats-ca", "worker": "eacp-db-ca eacp-nats-ca",
		"migrate": "eacp-db-ca", "pdp": ""}
	for _, w := range workloads {
		var got []string
		for n, keys := range secretRefs(podSpec(find(t, objs, w.kind, w.name))) {
			if strings.HasSuffix(n, "-ca") {
				if fmt.Sprint(keys) != "[ca.pem]" {
					t.Errorf("%s mounts %s keys %v, want [ca.pem]", w.name, n, keys)
				}
				got = append(got, n)
			}
		}
		if s := strings.Join(sortedUnique(got), " "); s != want[w.component] {
			t.Errorf("%s mounts CAs %q, want %q", w.name, s, want[w.component])
		}
	}
	for _, name := range []string{"eacp-api", "eacp-worker"} {
		if got := envDefs(t, find(t, objs, "Deployment", name), "EACP_NATS_CA_FILE"); fmt.Sprint(got) != "[/run/secrets/eacp-nats/ca.pem]" {
			t.Errorf("%s: EACP_NATS_CA_FILE = %v", name, got)
		}
	}
	for _, w := range workloads {
		if strings.Contains(fmt.Sprint(secretRefs(podSpec(find(t, render(t), w.kind, w.name)))), "-ca:") {
			t.Errorf("%s mounts a dependency CA that was not named", w.name)
		}
	}
}

// The default grace covers the longest call PostgreSQL allows
// (eacp.call_timeout caps timeout_ms at 300 000).
func TestTheDefaultGraceCoversTheLongestCall(t *testing.T) {
	sql, err := os.ReadFile(filepath.Join(root(t), "migrations", "00006_execution.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sql), "300000)") {
		t.Fatal("eacp.call_timeout no longer caps calls at 300 s: update worker.maxCallSeconds")
	}
	w, _ := defaults(t)["worker"].(map[string]any)
	if w["maxCallSeconds"] != 300 {
		t.Fatalf("worker.maxCallSeconds default = %v, want 300", w["maxCallSeconds"])
	}
	if _, err := renderErr(t); err != nil {
		t.Fatalf("the default grace does not pass the chart's own rule: %v", err)
	}
}

// Values the chart cannot check as durations or integers, and env entries
// that would override what the chart sets, fail rendering.
func TestAmbiguousValuesAreRefused(t *testing.T) {
	for _, c := range []struct{ flag, set, msg string }{
		{"--set", "environment=prod", "environment must be"},
		{"--set", "shutdown.timeout=2m", "shutdown.timeout must be"},
		{"--set", "shutdown.timeout=0s", "shutdown.timeout must be"},
		{"--set", "shutdown.delay=1m", "shutdown.delay must be"},
		{"--set", "shutdown.delay=61s", "shutdown.delay must be"},
		{"--set-string", "worker.maxCallSeconds=120s", "worker.maxCallSeconds must be"},
		{"--set", "worker.maxCallSeconds=-100", "worker.maxCallSeconds must be"},
		{"--set", "worker.maxCallSeconds=0", "worker.maxCallSeconds must be"},
		{"--set", "api.env.EACP_SHUTDOWN_DELAY=60s", "set by the chart"},
		{"--set", "api.env.EACP_GOVERNANCE_PROVIDER=local", "set by the chart"},
		{"--set", "api.env.EACP_AGT_PDP_URL=", "set by the chart"},
		{"--set", "api.env.EACP_HTTP_ADDR=:9090", "set by the chart"},
		{"--set", "worker.env.EACP_WORKER_ID=w", "set by the chart"},
		{"--set", "worker.env.EACP_ENV=development", "set by the chart"},
		{"--set", "worker.env.EACP_NATS_CA_FILE=/x", "set by the chart"},
		{"--set", "worker.env.EACP_LOG_FORMAT=text", "set by the chart"},
		{"--set", "pdp.env.AGT_PDP_LISTEN=0.0.0.0:1", "set by the chart"},
		{"--set", "worker.env.EACP_OTEL_ENDPOINT=nats://s3cr3t@nats:4222", "credentials"},
		{"--set", "worker.env.EACP_X=postgres://:pw@db/x", "credentials"},
		{"--set", "api.env.EACP_X.nested=1", "must be a string"},
	} {
		out, err := renderErr(t, c.flag, c.set)
		if err == nil || !strings.Contains(out, c.msg) {
			t.Errorf("%s %s: err = %v, output lacks %q:\n%s", c.flag, c.set, err, c.msg, firstLines(out))
		}
	}
}
