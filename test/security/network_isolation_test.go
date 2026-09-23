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

// inspect returns a running compose service's environment and mount targets.
func inspect(t *testing.T, service string) (env, mounts string) {
	t.Helper()
	id, err := exec.Command("docker", "compose", "ps", "-q", service).Output()
	if err != nil || strings.TrimSpace(string(id)) == "" {
		t.Fatalf("service %s is not running (err=%v)", service, err)
	}
	out, err := exec.Command("docker", "inspect", "--format", "{{json .Config.Env}}\n{{range .Mounts}}{{.Destination}} {{end}}",
		strings.TrimSpace(string(id))).Output()
	if err != nil {
		t.Fatalf("docker inspect %s: %v", service, err)
	}
	env, mounts, _ = strings.Cut(string(out), "\n")
	return env, mounts
}

// ADR-001 §3: connector credentials are mounted into the execution worker
// and nowhere else.
func TestOnlyTheWorkerHoldsConnectorSecrets(t *testing.T) {
	requireCompose(t)
	env, mounts := inspect(t, "execution-worker")
	if !strings.Contains(env, "EACP_CONNECTOR_SECRETS_FILE=") || !strings.Contains(mounts, "/run/secrets/connector_secrets") {
		t.Fatalf("worker has no connector secrets (env=%s mounts=%s); the negative checks would be meaningless", env, mounts)
	}
	for _, service := range []string{"controlplane-api", "fakeerp", "agent", "postgres"} {
		env, mounts := inspect(t, service)
		if strings.Contains(env, "CONNECTOR_SECRETS") || strings.Contains(mounts, "connector_secrets") {
			t.Errorf("%s holds connector secrets (env=%s mounts=%s)", service, env, mounts)
		}
	}
	logs, err := exec.Command("docker", "compose", "logs", "--no-color", "execution-worker").CombinedOutput()
	if err != nil || !strings.Contains(string(logs), `"bindings":1`) {
		t.Fatalf("worker did not load its credentials (err=%v): %s", err, logs)
	}
	if strings.Contains(string(logs), "dev-only-fakeerp-token") {
		t.Fatal("worker logged a connector secret")
	}
}

// The ERP has its own verifier token, shared only with the worker. No other
// service receives that mount or the worker's connector-secret manifest.
func TestFakeERPCredentialMountsAreLimitedToWorkerAndERP(t *testing.T) {
	requireCompose(t)
	for _, service := range []string{"execution-worker", "fakeerp"} {
		_, mounts := inspect(t, service)
		if !strings.Contains(mounts, "/run/secrets/fakeerp_token") {
			t.Errorf("%s has no ERP credential mount: %s", service, mounts)
		}
	}
	for _, service := range []string{"controlplane-api", "agent", "postgres"} {
		_, mounts := inspect(t, service)
		if strings.Contains(mounts, "fakeerp_token") {
			t.Errorf("%s has the ERP credential mount: %s", service, mounts)
		}
	}
}

// A caller with ERP network access but no worker credential still cannot
// execute a privileged operation. This also proves the ERP's auth gate is live.
func TestFakeERPRejectsUnauthenticatedPrivilegedCall(t *testing.T) {
	requireCompose(t)
	out, err := exec.Command("docker", "run", "--rm", "--network", "eacp_erp", "busybox:1.37",
		"wget", "-S", "-T", "3", "-O", "-", "--post-data={}", "http://fakeerp:8090/v1/execute").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "401 Unauthorized") {
		t.Fatalf("unauthenticated ERP call was not rejected with 401 (err=%v out=%q)", err, out)
	}
}
