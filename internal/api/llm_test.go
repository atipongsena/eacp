package api_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/identity"
	"github.com/atipongsena/eacp/internal/llm"
)

func TestLLMModelRoutes(t *testing.T) {
	h := newHarness(t)
	in := map[string]any{"name": "sonnet", "provider": "anthropic", "base_url": "https://api.anthropic.test",
		"upstream_model": "claude-x", "secret_ref": "llm", "max_output_tokens": 4096}
	code, body := h.as("carol", "POST", "/v1/llm-models", in)
	h.want(403, code, body)
	code, model := h.as("erin", "POST", "/v1/llm-models", in)
	h.want(201, code, model)
	if model["timeout_ms"] != float64(600000) || str(model, "id") == "" {
		t.Fatalf("model = %v", model)
	}
	code, body = h.as("erin", "POST", "/v1/llm-models", in)
	h.want(409, code, body)
	code, body = h.as("erin", "POST", "/v1/llm-models", map[string]any{"name": "x", "provider": "gemini",
		"base_url": "https://g.test", "upstream_model": "g", "secret_ref": "llm", "max_output_tokens": 10})
	h.want(400, code, body)
	code, body = h.as("carol", "GET", "/v1/llm-models", nil)
	h.want(200, code, body)
	if list, _ := body["models"].([]any); len(list) != 1 {
		t.Fatalf("models = %v", body)
	}

	a := h.f.ActiveAgent(t, "buyer")
	code, al := h.as("erin", "POST", "/v1/agent-versions/"+a.Version.String()+"/allowlists",
		map[string]any{"tools": []string{}, "models": []string{"sonnet"}})
	h.want(201, code, al)
	code, body = h.as("erin", "POST", "/v1/agent-versions/"+a.Version.String()+"/allowlists",
		map[string]any{"models": []string{"ghost"}})
	h.want(404, code, body)
	code, body = h.as("rita", "POST", "/v1/agent-versions/"+a.Version.String()+"/allowlist",
		map[string]any{"allowlist_id": str(al, "id")})
	h.want(204, code, body)
	code, detail := h.as("carol", "GET", "/v1/agents/buyer", nil)
	h.want(200, code, detail)
	v := detail["versions"].([]any)[0].(map[string]any)
	if models, _ := v["allowed_models"].([]any); len(models) != 1 || models[0] != "sonnet" {
		t.Fatalf("version = %v", v)
	}

	// An operator kills a model.
	code, body = h.as("otto", "POST", "/v1/killswitch", map[string]any{"scope": "model",
		"target_id": str(model, "id"), "killed": true, "reason": "provider incident"})
	h.want(200, code, body)
}

func TestLLMCallsAreReadOnlyForReaders(t *testing.T) {
	h := newHarness(t)
	h.f.LLMModel(t, "sonnet", "anthropic", "https://api.anthropic.test", "claude-x")
	a := h.f.ActiveAgent(t, "buyer")
	adm, err := llm.New(h.f.App).Admit(context.Background(), h.f.Tenant, llm.AdmitRequest{AgentVersionID: a.Version,
		ModelName: "sonnet", Provider: "anthropic", GatewayID: "gw-1", RequestBytes: 10,
		Decision: llm.Decision{ID: uuid.New(), BundleID: uuid.New(), Version: 1, Verdict: "allow"}})
	if err != nil || adm.Denial != "model_not_in_allowlist" {
		t.Fatalf("admit = %+v, %v", adm, err)
	}
	agentKey := h.issue(identity.KindAgent, a.Version, "erin", "rita")
	code, body := h.do(agentKey, "GET", "/v1/llm-calls", nil)
	h.want(403, code, body)
	code, body = h.as("carol", "GET", "/v1/llm-calls", nil)
	h.want(403, code, body)
	for _, who := range []string{"audra", "otto", "alice"} {
		code, body = h.as(who, "GET", "/v1/llm-calls?agent="+a.Agent.String()+"&model=sonnet&state=DENIED", nil)
		h.want(200, code, body)
		if list, _ := body["calls"].([]any); len(list) != 1 {
			t.Fatalf("%s: calls = %v", who, body)
		}
	}
	for _, q := range []string{"state=BOGUS", "limit=0", "limit=1001", "from=yesterday", "agent=x"} {
		code, body = h.as("audra", "GET", "/v1/llm-calls?"+q, nil)
		h.want(400, code, body)
	}
	code, body = h.as("audra", "GET", "/v1/llm-calls/"+adm.CallID.String(), nil)
	h.want(200, code, body)
	if body["state"] != "DENIED" || body["denial"] != "model_not_in_allowlist" {
		t.Fatalf("call = %v", body)
	}
	code, body = h.as("audra", "GET", "/v1/llm-calls/"+uuid.NewString(), nil)
	h.want(404, code, body)
}
