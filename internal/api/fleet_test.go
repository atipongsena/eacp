package api_test

import "testing"

func TestFleetAPI(t *testing.T) {
	h := newHarness(t)
	h.f.ActiveAgent(t, "buyer")
	pause := map[string]any{"kind": "pause", "selector": map[string]any{"agents": []string{"buyer"}},
		"reason": "incident 9", "dry_run": true}

	code, body := h.as("carol", "POST", "/v1/fleet/operations", pause)
	h.want(403, code, body)
	code, body = h.as("otto", "POST", "/v1/fleet/operations", pause)
	h.want(200, code, body)
	if targets, _ := body["targets"].([]any); len(targets) != 1 || body["id"] != nil {
		t.Fatalf("dry run = %v", body)
	}
	delete(pause, "dry_run")
	code, body = h.as("otto", "POST", "/v1/fleet/operations", pause)
	h.want(201, code, body)
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("pause = %v", body)
	}
	code, body = h.as("otto", "POST", "/v1/fleet/operations", pause)
	h.want(409, code, body) // nothing left to pause

	code, body = h.as("audra", "GET", "/v1/fleet/operations/"+id, nil)
	h.want(200, code, body)
	if body["kind"] != "pause" {
		t.Fatalf("operation = %v", body)
	}
	code, body = h.as("audra", "GET", "/v1/fleet/agents?health=contained", nil)
	h.want(200, code, body)
	if agents, _ := body["agents"].([]any); len(agents) != 1 {
		t.Fatalf("contained agents = %v", body)
	}
	code, body = h.as("otto", "GET", "/v1/fleet/health?window=1h", nil)
	h.want(200, code, body)
	if body["agents"] != float64(1) || body["window"] != "1h0m0s" {
		t.Fatalf("health = %v", body)
	}
	code, body = h.as("otto", "GET", "/v1/fleet/health?window=soon", nil)
	h.want(400, code, body)
	code, body = h.as("carol", "GET", "/v1/fleet/health", nil)
	h.want(403, code, body)

	// An operator contains; only a registry approver grants.
	resume := map[string]any{"kind": "resume", "source_operation_id": id, "reason": "cleared"}
	code, body = h.as("otto", "POST", "/v1/fleet/operations", resume)
	h.want(403, code, body)
	code, body = h.as("ravi", "POST", "/v1/fleet/operations", resume)
	h.want(201, code, body)
	code, body = h.as("ravi", "POST", "/v1/fleet/operations", map[string]any{"kind": "pause", "reason": "x"})
	h.want(400, code, body)
}
