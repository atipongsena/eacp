// Package security holds end-to-end security tests against the docker-compose
// environment. They are skipped unless EACP_COMPOSE_TEST=1 and the stack is
// running (`docker compose up -d`).
package security

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
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

// ADR-001 §3/§3a: nor to the MCP server of the Slice C demo.
func TestAgentCannotReachFakeMCP(t *testing.T) {
	requireCompose(t)
	out, err := fromAgent("wget", "-q", "-T", "3", "-O", "-", "http://fakemcp:8091/healthz")
	if err == nil {
		t.Fatalf("agent reached fakemcp directly: %q", out)
	}
}

// ADR-030: nor to the remote A2A agent the worker delegates to.
func TestAgentCannotReachFakeA2A(t *testing.T) {
	requireCompose(t)
	out, err := fromAgent("wget", "-q", "-T", "3", "-O", "-", "http://fakea2a:8092/healthz")
	if err == nil {
		t.Fatalf("agent reached fakea2a directly: %q", out)
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

// Positive control for the MCP server, on the same worker-only network.
func TestERPNetworkCanReachFakeMCP(t *testing.T) {
	requireCompose(t)
	out, err := exec.Command("docker", "run", "--rm", "--network", "eacp_erp", "busybox:1.37",
		"wget", "-q", "-T", "3", "-O", "-", "http://fakemcp:8091/healthz").CombinedOutput()
	if err != nil || !strings.Contains(string(out), `"ok"`) {
		t.Fatalf("erp network could not reach fakemcp (err=%v out=%q)", err, out)
	}
}

// Positive control for the A2A agent, on the same worker-only network.
func TestERPNetworkCanReachFakeA2A(t *testing.T) {
	requireCompose(t)
	out, err := exec.Command("docker", "run", "--rm", "--network", "eacp_erp", "busybox:1.37",
		"wget", "-q", "-T", "3", "-O", "-", "http://fakea2a:8092/healthz").CombinedOutput()
	if err != nil || !strings.Contains(string(out), `"ok"`) {
		t.Fatalf("erp network could not reach fakea2a (err=%v out=%q)", err, out)
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
	for _, service := range []string{"controlplane-api", "llm-gateway", "fakeerp", "fakemcp", "fakea2a", "fakellm", "agent",
		"postgres"} {
		env, mounts := inspect(t, service)
		if strings.Contains(env, "CONNECTOR_SECRETS") || strings.Contains(mounts, "connector_secrets") {
			t.Errorf("%s holds connector secrets (env=%s mounts=%s)", service, env, mounts)
		}
	}
	logs, err := exec.Command("docker", "compose", "logs", "--no-color", "execution-worker").CombinedOutput()
	if err != nil || !manifestBindings(t, "connector-secrets.dev.json").Match(logs) {
		t.Fatalf("worker did not load its credentials (err=%v): %s", err, logs)
	}
	if strings.Contains(string(logs), "dev-only-fakeerp-token") || strings.Contains(string(logs), "dev-only-fakemcp-token") ||
		strings.Contains(string(logs), "dev-only-fakea2a-token") ||
		strings.Contains(string(logs), "dev-only-fakeerp-oauth-client-secret") {
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
	for _, service := range []string{"controlplane-api", "agent", "postgres", "fakemcp", "fakea2a"} {
		_, mounts := inspect(t, service)
		if strings.Contains(mounts, "fakeerp_token") {
			t.Errorf("%s has the ERP credential mount: %s", service, mounts)
		}
	}
}

// The MCP server's verifier is mounted into the MCP server only; the worker
// holds the token through its connector-secret manifest.
func TestFakeMCPCredentialMountIsLimitedToTheServer(t *testing.T) {
	requireCompose(t)
	if _, mounts := inspect(t, "fakemcp"); !strings.Contains(mounts, "/run/secrets/fakemcp_token") {
		t.Fatalf("fakemcp has no credential mount: %s", mounts)
	}
	for _, service := range []string{"controlplane-api", "execution-worker", "fakeerp", "agent", "postgres"} {
		if _, mounts := inspect(t, service); strings.Contains(mounts, "fakemcp_token") {
			t.Errorf("%s has the MCP verifier mount: %s", service, mounts)
		}
	}
}

// The A2A agent's verifier is mounted into the agent only; the worker holds
// the token through its connector-secret manifest.
func TestFakeA2ACredentialMountIsLimitedToTheAgent(t *testing.T) {
	requireCompose(t)
	if _, mounts := inspect(t, "fakea2a"); !strings.Contains(mounts, "/run/secrets/fakea2a_token") {
		t.Fatalf("fakea2a has no credential mount: %s", mounts)
	}
	for _, service := range []string{"controlplane-api", "execution-worker", "fakeerp", "fakemcp", "agent", "postgres"} {
		if _, mounts := inspect(t, service); strings.Contains(mounts, "fakea2a_token") {
			t.Errorf("%s has the A2A verifier mount: %s", service, mounts)
		}
	}
}

// A caller on the worker network without the token cannot read the A2A
// agent's card or send it a message.
func TestFakeA2ARejectsUnauthenticatedCalls(t *testing.T) {
	requireCompose(t)
	for _, path := range []string{"/.well-known/agent-card.json", "/v1/audit"} {
		out, err := exec.Command("docker", "run", "--rm", "--network", "eacp_erp", "busybox:1.37",
			"wget", "-q", "-T", "3", "-O", "-", "http://fakea2a:8092"+path).CombinedOutput()
		if err == nil || !strings.Contains(string(out), "401") {
			t.Fatalf("unauthenticated %s (err=%v out=%q)", path, err, out)
		}
	}
}

// A caller on the worker network without the token cannot list the tools.
func TestFakeMCPRejectsUnauthenticatedListing(t *testing.T) {
	requireCompose(t)
	out, err := exec.Command("docker", "run", "--rm", "--network", "eacp_erp", "busybox:1.37",
		"wget", "-S", "-T", "3", "-O", "-", "--post-data={}", "http://fakemcp:8091/mcp").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "401 Unauthorized") {
		t.Fatalf("unauthenticated MCP call was not rejected with 401 (err=%v out=%q)", err, out)
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

// ADR-019: the agent cannot mint a token: the token endpoint lives on the
// ERP, which the agent has no route to.
func TestAgentCannotReachTheTokenEndpoint(t *testing.T) {
	requireCompose(t)
	out, err := fromAgent("wget", "-q", "-T", "3", "-O", "-", "--post-data", "grant_type=client_credentials",
		"http://fakeerp:8090/oauth/token")
	if err == nil {
		t.Fatalf("agent reached the token endpoint: %q", out)
	}
}

// ADR-019: the OAuth client secret the token endpoint verifies is mounted
// into Fake ERP only; the worker holds it through its connector manifest.
func TestTheOAuthClientSecretIsMountedOnlyIntoTheERP(t *testing.T) {
	requireCompose(t)
	if _, mounts := inspect(t, "fakeerp"); !strings.Contains(mounts, "/run/secrets/fakeerp_oauth_client") {
		t.Fatalf("fakeerp has no OAuth client mount: %s", mounts)
	}
	for _, service := range []string{"controlplane-api", "execution-worker", "fakemcp", "agent", "postgres"} {
		if _, mounts := inspect(t, service); strings.Contains(mounts, "fakeerp_oauth_client") {
			t.Errorf("%s has the OAuth client mount: %s", service, mounts)
		}
	}
}

// A caller on the ERP network without the client secret gets no token.
func TestTheTokenEndpointRejectsAnUnknownClient(t *testing.T) {
	requireCompose(t)
	out, err := exec.Command("docker", "run", "--rm", "--network", "eacp_erp", "busybox:1.37",
		"wget", "-S", "-T", "3", "-O", "-", "--post-data=grant_type=client_credentials",
		"http://fakeerp:8090/oauth/token").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "401 Unauthorized") {
		t.Fatalf("an unauthenticated token request was not rejected with 401 (err=%v out=%q)", err, out)
	}
}

// volumeMounts returns a running compose service's named volumes by the
// compose volume name (the project prefix removed) and their mount targets.
func volumeMounts(t *testing.T, service string) map[string]string {
	t.Helper()
	id, err := exec.Command("docker", "compose", "ps", "-q", service).Output()
	if err != nil || strings.TrimSpace(string(id)) == "" {
		t.Fatalf("service %s is not running (err=%v)", service, err)
	}
	out, err := exec.Command("docker", "inspect", "--format",
		`{{range .Mounts}}{{if eq .Type "volume"}}{{.Name}}={{.Destination}} {{end}}{{end}}`,
		strings.TrimSpace(string(id))).Output()
	if err != nil {
		t.Fatalf("docker inspect %s: %v", service, err)
	}
	volumes := map[string]string{}
	for _, f := range strings.Fields(string(out)) {
		name, dest, _ := strings.Cut(f, "=")
		if i := strings.Index(name, "_"); i >= 0 {
			name = name[i+1:]
		}
		volumes[name] = dest
	}
	return volumes
}

// ADR-019 Rev 1.2: the worker's private_key_jwt signing key is mounted into
// the worker only; the Fake ERP gets the public JWKS and never the key.
func TestTheClientKeyIsMountedOnlyIntoTheWorker(t *testing.T) {
	requireCompose(t)
	if got := volumeMounts(t, "execution-worker")["client_key"]; got != "/run/secrets/eacp-client-key" {
		t.Fatalf("the worker mounts client_key at %q", got)
	}
	erp := volumeMounts(t, "fakeerp")
	if erp["client_jwks"] == "" {
		t.Fatalf("fakeerp has no client_jwks mount: %v", erp)
	}
	if _, ok := erp["client_key"]; ok {
		t.Fatal("fakeerp mounts the worker's signing key")
	}
	if _, ok := volumeMounts(t, "execution-worker")["client_jwks"]; ok {
		t.Error("the worker mounts the relying party's JWKS")
	}
	for _, service := range []string{"controlplane-api", "fakemcp", "agent", "postgres"} {
		v := volumeMounts(t, service)
		for _, name := range []string{"client_key", "client_jwks"} {
			if _, ok := v[name]; ok {
				t.Errorf("%s mounts %s", service, name)
			}
		}
	}
}

// ADR-019 Rev 1.3: the agent has no route to Vault.
func TestAgentCannotReachVault(t *testing.T) {
	requireCompose(t)
	out, err := exec.Command("docker", "run", "--rm", "--network", "eacp_vault", "busybox:1.37",
		"wget", "-q", "-T", "3", "-O", "-", "http://vault:8200/v1/sys/health").CombinedOutput()
	if err != nil || !strings.Contains(string(out), `"initialized":true`) {
		t.Fatalf("the vault network could not reach Vault (err=%v out=%q); the probe is broken", err, out)
	}
	if out, err := fromAgent("wget", "-q", "-T", "3", "-O", "-", "http://vault:8200/v1/sys/health"); err == nil {
		t.Fatalf("agent reached Vault: %q", out)
	}
}

// ADR-019 Rev 1.3: the worker's AppRole login files are mounted read-only
// into the worker and no other running service.
func TestTheVaultAppRoleIsMountedOnlyIntoTheWorker(t *testing.T) {
	requireCompose(t)
	if got := volumeMounts(t, "execution-worker")["vault_approle"]; got != "/run/secrets/eacp-vault-approle" {
		t.Fatalf("the worker mounts vault_approle at %q", got)
	}
	id, err := exec.Command("docker", "compose", "ps", "-q", "execution-worker").Output()
	if err != nil {
		t.Fatal(err)
	}
	rw, err := exec.Command("docker", "inspect", "--format",
		`{{range .Mounts}}{{if eq .Destination "/run/secrets/eacp-vault-approle"}}{{.RW}}{{end}}{{end}}`,
		strings.TrimSpace(string(id))).Output()
	if err != nil || strings.TrimSpace(string(rw)) != "false" {
		t.Fatalf("the worker's AppRole mount is not read-only (err=%v rw=%q)", err, rw)
	}
	for _, service := range []string{"controlplane-api", "fakeerp", "fakemcp", "agent", "postgres", "vault"} {
		if _, ok := volumeMounts(t, service)["vault_approle"]; ok {
			t.Errorf("%s mounts vault_approle", service)
		}
	}
}

// ADR-019 Rev 1.3: only Vault, its one-shot init and the worker join the
// vault network.
func TestOnlyTheWorkerSharesTheVaultNetwork(t *testing.T) {
	requireCompose(t)
	on := networkMembers(t, "vault")
	if want := []string{"execution-worker", "vault", "vault-init"}; !slices.Equal(on, want) {
		t.Fatalf("services on the vault network: %v, want %v", on, want)
	}
}

// networkMembers lists, sorted, the compose services on network, which must
// be internal.
func networkMembers(t *testing.T, network string) []string {
	t.Helper()
	out, err := exec.Command("docker", "compose", "config", "--format", "json").Output()
	if err != nil {
		t.Fatalf("docker compose config: %v", err)
	}
	var config struct {
		Services map[string]struct {
			Networks map[string]any `json:"networks"`
		} `json:"services"`
		Networks map[string]struct {
			Internal bool `json:"internal"`
		} `json:"networks"`
	}
	if err := json.Unmarshal(out, &config); err != nil {
		t.Fatal(err)
	}
	if !config.Networks[network].Internal {
		t.Errorf("the %s network is not internal", network)
	}
	var on []string
	for name, s := range config.Services {
		if _, ok := s.Networks[network]; ok {
			on = append(on, name)
		}
	}
	slices.Sort(on)
	return on
}

// manifestBindings matches the "bindings":N a service logs once it has
// loaded every entry of the development manifest compose mounts into it.
func manifestBindings(t *testing.T, name string) *regexp.Regexp {
	t.Helper()
	raw, err := os.ReadFile("../../deployments/docker/secrets/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Secrets []json.RawMessage `json:"secrets"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil || len(manifest.Secrets) == 0 {
		t.Fatalf("%s: %d entries (%v)", name, len(manifest.Secrets), err)
	}
	return regexp.MustCompile(fmt.Sprintf(`"bindings":%d\D`, len(manifest.Secrets)))
}
