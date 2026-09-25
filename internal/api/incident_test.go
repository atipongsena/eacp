package api_test

import (
	"testing"
)

func TestIncidentsThroughTheAPI(t *testing.T) {
	h := newHarness(t)
	code, body := h.as("carol", "POST", "/v1/incidents", map[string]any{"title": "x", "severity": "low", "reason": "r"})
	h.want(403, code, body)
	code, body = h.as("otto", "POST", "/v1/incidents", map[string]any{"title": "odd purchases",
		"severity": "critical", "reason": "reported by finance"})
	h.want(201, code, body)
	id, _ := body["id"].(string)
	code, body = h.as("otto", "POST", "/v1/incidents/"+id+"/resolve", map[string]any{"resolution": "contained", "reason": "done"})
	h.want(409, code, body)
	code, body = h.as("otto", "POST", "/v1/incidents/"+id+"/notes", map[string]any{"text": "checking"})
	h.want(201, code, body)
	code, body = h.as("otto", "POST", "/v1/incidents/"+id+"/assign", map[string]any{"assignee_id": h.f.P["opal"].String()})
	h.want(200, code, body)
	code, body = h.as("otto", "POST", "/v1/incidents/"+id+"/acknowledge", map[string]any{"reason": "mine"})
	h.want(200, code, body)
	code, body = h.as("otto", "POST", "/v1/incidents/"+id+"/resolve", map[string]any{"resolution": "contained", "reason": "done"})
	h.want(403, code, body)
	code, body = h.as("opal", "POST", "/v1/incidents/"+id+"/resolve", map[string]any{"resolution": "contained", "reason": "done"})
	h.want(200, code, body)
	if body["state"] != "RESOLVED" || len(body["events"].([]any)) != 5 {
		t.Fatalf("resolved = %v", body)
	}
	code, body = h.as("audra", "GET", "/v1/incidents?state=RESOLVED", nil)
	h.want(200, code, body)
	if list, _ := body["incidents"].([]any); len(list) != 1 {
		t.Fatalf("list = %v", body)
	}
	code, body = h.as("audra", "GET", "/v1/incidents/"+id, nil)
	h.want(200, code, body)
	code, body = h.as("audra", "GET", "/v1/incidents?limit=0", nil)
	h.want(400, code, body)
	code, body = h.as("audra", "POST", "/v1/incidents/"+id+"/notes", map[string]any{"text": "auditors read"})
	h.want(403, code, body)
	code, body = h.as("carol", "GET", "/v1/soc/summary", nil)
	h.want(403, code, body)
	code, body = h.as("audra", "GET", "/v1/soc/summary", nil)
	h.want(200, code, body)
	if sec, _ := body["security"].(map[string]any); sec == nil || sec["open_incidents"] == nil {
		t.Fatalf("summary = %v", body)
	}
}
