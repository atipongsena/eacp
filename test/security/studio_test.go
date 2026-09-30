package security

import (
	"encoding/json"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// runtimeContainer creates (never starts) the agent-runtime container of
// profile studio and returns its inspection: a plain `up` does not start
// the runtime before its key exists (ADR-033 Rev 1.2).
func runtimeContainer(t *testing.T) (env []string, mounts []struct {
	Destination string
	RW          bool
}, networks []string) {
	t.Helper()
	if out, err := exec.Command("docker", "compose", "--profile", "studio", "create", "agent-runtime").CombinedOutput(); err != nil {
		t.Fatalf("create agent-runtime: %v\n%s", err, out)
	}
	id, err := exec.Command("docker", "compose", "--profile", "studio", "ps", "-a", "-q", "agent-runtime").Output()
	if err != nil || strings.TrimSpace(string(id)) == "" {
		t.Fatalf("no agent-runtime container (err=%v)", err)
	}
	out, err := exec.Command("docker", "inspect", strings.TrimSpace(string(id))).Output()
	if err != nil {
		t.Fatalf("docker inspect agent-runtime: %v", err)
	}
	var info []struct {
		Config struct{ Env []string }
		Mounts []struct {
			Destination string
			RW          bool
		}
		NetworkSettings struct{ Networks map[string]any }
	}
	if err := json.Unmarshal(out, &info); err != nil || len(info) != 1 {
		t.Fatalf("inspect: %v", err)
	}
	for n := range info[0].NetworkSettings.Networks {
		networks = append(networks, n)
	}
	slices.Sort(networks)
	return info[0].Config.Env, info[0].Mounts, networks
}

// ADR-033 Rev 1.2: the runtime reaches EACP through the API only. It sits
// on the agents network (the API and nothing that holds data or
// credentials), has no database URL and holds no connector or provider
// secret; its master and keys are a read-only mount.
func TestTheRuntimeReachesOnlyTheAPI(t *testing.T) {
	requireCompose(t)
	env, mounts, networks := runtimeContainer(t)
	if !slices.Equal(networks, []string{"eacp_agents"}) {
		t.Fatalf("agent-runtime networks = %v, want [eacp_agents]", networks)
	}
	all := strings.Join(env, "\n")
	for _, want := range []string{"EACP_API_URL=http://controlplane-api:8080", "EACP_STUDIO_MASTER_FILE=/run/studio/master",
		"EACP_RUNTIME_KEY_FILE=/run/studio/keys"} {
		if !strings.Contains(all, want) {
			t.Errorf("agent-runtime env lacks %s: %s", want, all)
		}
	}
	for _, banned := range []string{"EACP_DATABASE_URL", "CONNECTOR_SECRETS", "LLM_SECRETS", "EACP_NATS_URL"} {
		if strings.Contains(all, banned) {
			t.Errorf("agent-runtime has %s", banned)
		}
	}
	var dests []string
	for _, m := range mounts {
		dests = append(dests, m.Destination)
		if m.Destination == "/run/studio" && m.RW {
			t.Error("the runtime's master and keys are mounted writable")
		}
	}
	if !slices.Equal(dests, []string{"/run/studio"}) {
		t.Fatalf("agent-runtime mounts = %v, want only /run/studio", dests)
	}
	// The agents network reaches neither PostgreSQL nor the MCP servers
	// (the stand-in agent shares it: TestAgentCannotReachPostgres and below).
	for _, url := range []string{"http://fakemcp-hr:8091/healthz", "http://fakeerp:8090/healthz"} {
		if out, err := fromAgent("wget", "-q", "-T", "3", "-O", "-", url); err == nil {
			t.Fatalf("the agents network reached %s: %q", url, out)
		}
	}
}

// ADR-033 invariant 5: no other service is given the Studio master.
func TestOnlyTheRuntimeHoldsTheStudioMaster(t *testing.T) {
	requireCompose(t)
	for _, service := range []string{"controlplane-api", "execution-worker", "llm-gateway", "fakeerp", "fakemcp",
		"fakemcp-hr", "fakea2a", "fakellm", "agent", "postgres"} {
		env, mounts := inspect(t, service)
		if strings.Contains(env, "STUDIO_MASTER") || strings.Contains(mounts, "/run/studio") {
			t.Errorf("%s holds the Studio master (env=%s mounts=%s)", service, env, mounts)
		}
	}
}

// The HR MCP server has its own verifier, mounted into it only; a caller on
// the worker-only network without the token gets nothing.
func TestFakeMCPHRRejectsUnauthenticatedCalls(t *testing.T) {
	requireCompose(t)
	if _, mounts := inspect(t, "fakemcp-hr"); !strings.Contains(mounts, "/run/secrets/fakemcp_hr_token") {
		t.Fatalf("fakemcp-hr has no credential mount: %s", mounts)
	}
	for _, service := range []string{"controlplane-api", "execution-worker", "fakemcp", "agent", "postgres"} {
		if _, mounts := inspect(t, service); strings.Contains(mounts, "fakemcp_hr_token") {
			t.Errorf("%s has the HR MCP verifier mount: %s", service, mounts)
		}
	}
	out, err := exec.Command("docker", "run", "--rm", "--network", "eacp_erp", "busybox:1.37", "wget", "-q", "-T", "3",
		"-O", "-", "--header", "Content-Type: application/json", "--post-data", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		"http://fakemcp-hr:8091/mcp").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "401") {
		t.Fatalf("unauthenticated fakemcp-hr call (err=%v out=%q)", err, out)
	}
}
