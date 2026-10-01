package api_test

import (
	"encoding/json"
	"testing"
)

func TestStudioCheckedVersionAPIKeepsBothTabsFromOverwriting(t *testing.T) {
	h, hr := studioHarness(t)
	code, v := h.as("stella", "POST", "/v1/studio/agents", newStudioAgent("two-tabs", hr, studioDefinition))
	h.want(201, code, v)
	body := map[string]any{"definition": json.RawMessage(studioDefinition), "expected_version_id": str(v, "id")}
	path := "/v1/studio/agents/" + str(v, "agent_id") + "/versions"
	code, result := h.as("stella", "POST", path, body)
	h.want(201, code, result)
	code, result = h.as("stella", "POST", path, body)
	h.want(409, code, result)
}

func TestStudioModelCapabilityIsVisibleToItsAuthorAndApprover(t *testing.T) {
	h, hr := studioHarness(t)
	h.f.LLMModel(t, "triage", "openai", "https://llm.example.test", "triage-upstream")
	def := `{"schema_version":2,"kind":"agent","inputs":{},"limits":{"timeout_seconds":120,"max_output_tokens":100},"steps":[{"id":"classify","kind":"llm","model":"triage","instruction":"Return JSON only.","input":{},"max_output_tokens":100,"output_schema":{"type":"object","properties":{"eligible":{"type":"boolean"}},"required":["eligible"],"additionalProperties":false},"next":"answer"},{"id":"answer","kind":"respond","text":"{{steps.classify.output.eligible}}"}]}`
	code, v := h.as("stella", "POST", "/v1/studio/agents", newStudioAgent("model-agent", hr, def))
	h.want(201, code, v)
	models, _ := v["models"].([]any)
	if len(models) != 1 || models[0] != "triage" {
		t.Fatalf("models=%v", v["models"])
	}
	code, requests := h.as("rita", "GET", "/v1/studio/requests", nil)
	h.want(200, code, requests)
	r := requests["requests"].([]any)[0].(map[string]any)
	if len(r["models"].([]any)) != 1 {
		t.Fatal("approver cannot see models")
	}
}
