package security

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// ADR-031: agents reach the LLM gateway, which is how they reach a model.
func TestAgentReachesTheGateway(t *testing.T) {
	requireCompose(t)
	out, err := fromAgent("wget", "-q", "-T", "3", "-O", "-", "http://llm-gateway:8083/healthz")
	if err != nil || !strings.Contains(out, `"ok"`) {
		t.Fatalf("agent could not reach llm-gateway (err=%v out=%q)", err, out)
	}
	// Without an EACP key the gateway answers 401 and forwards nothing.
	out, err = fromAgent("wget", "-q", "-T", "3", "-O", "-", "--header", "Content-Type: application/json",
		"--post-data", `{"model":"sonnet","max_tokens":1,"messages":[]}`, "http://llm-gateway:8083/v1/messages")
	if err == nil || !strings.Contains(out, "401") {
		t.Fatalf("unauthenticated gateway call (err=%v out=%q)", err, out)
	}
}

// ADR-031: an agent has no route to a provider; only the gateway does.
func TestAgentCannotReachFakeLLM(t *testing.T) {
	requireCompose(t)
	out, err := fromAgent("wget", "-q", "-T", "3", "-O", "-", "http://fakellm:8093/healthz")
	if err == nil {
		t.Fatalf("agent reached fakellm directly: %q — the gateway can be bypassed", out)
	}
}

// Positive control for the provider, on the gateway-only network.
func TestLLMNetworkCanReachFakeLLM(t *testing.T) {
	requireCompose(t)
	out, err := exec.Command("docker", "run", "--rm", "--network", "eacp_llm", "busybox:1.37",
		"wget", "-q", "-T", "3", "-O", "-", "http://fakellm:8093/healthz").CombinedOutput()
	if err != nil || !strings.Contains(string(out), `"ok"`) {
		t.Fatalf("llm network could not reach fakellm (err=%v out=%q)", err, out)
	}
	on := networkMembers(t, "llm")
	if want := []string{"fakellm", "llm-gateway"}; !slices.Equal(on, want) {
		t.Fatalf("services on the llm network: %v, want %v", on, want)
	}
}

// ADR-031: provider credentials are mounted into the gateway and nowhere
// else; the provider's verifier into the provider only.
func TestOnlyTheGatewayHoldsProviderSecrets(t *testing.T) {
	requireCompose(t)
	env, mounts := inspect(t, "llm-gateway")
	if !strings.Contains(env, "EACP_LLM_SECRETS_FILE=") || !strings.Contains(mounts, "/run/secrets/llm_secrets") {
		t.Fatalf("gateway has no provider secrets (env=%s mounts=%s); the negative checks would be meaningless", env, mounts)
	}
	if strings.Contains(env, "CONNECTOR_SECRETS") || strings.Contains(mounts, "connector_secrets") ||
		strings.Contains(mounts, "fakellm_key") {
		t.Fatalf("gateway holds more than provider credentials (env=%s mounts=%s)", env, mounts)
	}
	if _, mounts := inspect(t, "fakellm"); !strings.Contains(mounts, "/run/secrets/fakellm_key") {
		t.Fatalf("fakellm has no key mount: %s", mounts)
	}
	for _, service := range []string{"controlplane-api", "execution-worker", "fakeerp", "fakemcp", "fakea2a", "fakellm",
		"agent", "postgres"} {
		env, mounts := inspect(t, service)
		if strings.Contains(env, "LLM_SECRETS") || strings.Contains(mounts, "llm_secrets") {
			t.Errorf("%s holds provider secrets (env=%s mounts=%s)", service, env, mounts)
		}
		if service != "fakellm" && strings.Contains(mounts, "fakellm_key") {
			t.Errorf("%s has the provider's key mount: %s", service, mounts)
		}
	}
	logs, err := exec.Command("docker", "compose", "logs", "--no-color", "llm-gateway", "fakellm").CombinedOutput()
	if err != nil || !manifestBindings(t, "llm-secrets.dev.json").Match(logs) {
		t.Fatalf("gateway did not load its credential (err=%v): %s", err, logs)
	}
	if strings.Contains(string(logs), "dev-only-fakellm-key") {
		t.Fatal("a provider key was logged")
	}
}

// A caller on the provider network without the key gets nothing.
func TestFakeLLMRejectsUnauthenticatedCalls(t *testing.T) {
	requireCompose(t)
	for _, args := range [][]string{
		{"--header", "Content-Type: application/json", "--post-data", `{"model":"m","max_tokens":1,"messages":[]}`,
			"http://fakellm:8093/v1/messages"},
		{"--header", "Content-Type: application/json", "--post-data", `{"model":"m","messages":[]}`,
			"http://fakellm:8093/v1/chat/completions"},
		{"http://fakellm:8093/v1/audit"},
	} {
		cmd := append([]string{"run", "--rm", "--network", "eacp_llm", "busybox:1.37", "wget", "-q", "-T", "3", "-O", "-"}, args...)
		out, err := exec.Command("docker", cmd...).CombinedOutput()
		if err == nil || !strings.Contains(string(out), "401") {
			t.Fatalf("unauthenticated %v (err=%v out=%q)", args, err, out)
		}
	}
}
