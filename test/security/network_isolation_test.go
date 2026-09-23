// Package security holds end-to-end security tests against the docker-compose
// environment. They are skipped unless EACP_COMPOSE_TEST=1 and the stack is
// running (`docker compose up -d`).
package security

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func requireCompose(t *testing.T) {
	t.Helper()
	if os.Getenv("EACP_COMPOSE_TEST") != "1" {
		t.Skip("EACP_COMPOSE_TEST!=1; skipping docker-compose security test")
	}
}

// fromAgent runs a command inside the stand-in agent runtime container.
func fromAgent(args ...string) (string, error) {
	cmd := exec.Command("docker", append([]string{"compose", "exec", "-T", "agent"}, args...)...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// Positive control: without it, the negative tests could pass simply because
// the probe itself is broken.
func TestAgentCanReachControlPlaneAPI(t *testing.T) {
	requireCompose(t)
	out, err := fromAgent("wget", "-q", "-T", "3", "-O", "-", "http://controlplane-api:8080/healthz")
	if err != nil || !strings.Contains(out, `"ok"`) {
		t.Fatalf("agent could not reach controlplane-api (err=%v out=%q); probe is broken", err, out)
	}
}

// ADR-001 §3/§3a: the agent runtime has no network path to the ERP.
func TestAgentCannotReachFakeERP(t *testing.T) {
	requireCompose(t)
	out, err := fromAgent("wget", "-q", "-T", "3", "-O", "-", "http://fakeerp:8090/healthz")
	if err == nil {
		t.Fatalf("agent reached fakeerp directly: %q — control plane can be bypassed", out)
	}
}

// Positive control for the nc probe: the same invocation must succeed against
// a port the agent is allowed to reach.
func TestAgentNcProbeWorks(t *testing.T) {
	requireCompose(t)
	if out, err := fromAgent("nc", "-z", "-w", "3", "controlplane-api", "8080"); err != nil {
		t.Fatalf("nc probe failed against a reachable port (err=%v out=%q); postgres test would be meaningless", err, out)
	}
}

// Agents must not reach the database either (all state changes go through the API).
func TestAgentCannotReachPostgres(t *testing.T) {
	requireCompose(t)
	out, err := fromAgent("nc", "-z", "-w", "3", "postgres", "5432")
	if err == nil {
		t.Fatalf("agent reached postgres directly: %q", out)
	}
}

// Positive control for the ERP side: a container on the erp network (where the
// worker lives) can reach fakeerp, so the negative test above is meaningful.
func TestERPNetworkCanReachFakeERP(t *testing.T) {
	requireCompose(t)
	out, err := exec.Command("docker", "run", "--rm", "--network", "eacp_erp", "busybox:1.37",
		"wget", "-q", "-T", "3", "-O", "-", "http://fakeerp:8090/healthz").CombinedOutput()
	if err != nil || !strings.Contains(string(out), `"ok"`) {
		t.Fatalf("erp network could not reach fakeerp (err=%v out=%q)", err, out)
	}
}
