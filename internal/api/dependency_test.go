package api_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDependencyAPIRecordsAndReportsBlastRadius(t *testing.T) {
	h := newHarness(t)
	agent := h.f.ActiveAgent(t, "buyer")
	input := map[string]any{
		"from_kind": "agent_version", "from_id": agent.Version.String(),
		"to_kind": "model", "to_name": "gpt-production", "source": "deployment_manifest",
		"confidence": "high", "observed_at": time.Now().Add(-time.Minute).UTC(),
		"expires_at": time.Now().Add(time.Hour).UTC(),
	}
	code, body := h.as("carol", "POST", "/v1/dependencies", input)
	h.want(403, code, body)
	code, body = h.as("erin", "POST", "/v1/dependencies", input)
	h.want(201, code, body)
	id := str(body, "id")
	if _, err := uuid.Parse(id); err != nil {
		t.Fatalf("dependency id: %v", body)
	}
	code, report := h.as("otto", "GET", "/v1/dependencies/blast-radius?kind=model&name=gpt-production", nil)
	h.want(200, code, report)
	confirmed, _ := report["confirmed_agents"].([]any)
	if len(confirmed) != 1 || confirmed[0].(map[string]any)["version_id"] != agent.Version.String() {
		t.Fatalf("report = %v", report)
	}
	code, body = h.as("carol", "GET", "/v1/dependencies/blast-radius?kind=model&name=gpt-production", nil)
	h.want(403, code, body)
	code, body = h.as("otto", "GET", "/v1/dependencies/blast-radius?kind=mcp&id="+uuid.NewString(), nil)
	h.want(404, code, body)
	code, body = h.as("erin", "POST", "/v1/dependencies/"+id+"/revoke", map[string]any{"reason": "retired manifest"})
	h.want(204, code, body)
	code, report = h.as("audra", "GET", "/v1/dependencies/blast-radius?kind=model&name=gpt-production", nil)
	h.want(200, code, report)
	if confirmed, _ := report["confirmed_agents"].([]any); len(confirmed) != 0 {
		t.Fatalf("revoked edge still confirmed: %v", report)
	}
}
