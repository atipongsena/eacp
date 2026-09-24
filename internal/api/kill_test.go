package api_test

import "testing"

func TestKillAPIRequiresOperatorAndSecondOperatorToResume(t *testing.T) {
	h := newHarness(t)
	a := h.f.ActiveAgent(t, "buyer")
	in := map[string]any{"scope": "agent_version", "target_id": a.Version.String(),
		"killed": true, "reason": "incident containment"}
	code, body := h.as("carol", "POST", "/v1/killswitch", in)
	h.want(403, code, body)
	code, body = h.as("otto", "POST", "/v1/killswitch", in)
	h.want(200, code, body)
	if body["epoch"] != float64(1) || body["killed"] != true {
		t.Fatalf("kill = %v", body)
	}
	if body["reason_code"] != "operator_request" {
		t.Fatalf("AGT reason code = %v", body)
	}
	code, body = h.as("audra", "GET", "/v1/killswitch", nil)
	h.want(200, code, body)
	in["killed"] = false
	in["reason"] = "incident cleared"
	code, body = h.as("otto", "POST", "/v1/killswitch", in)
	h.want(403, code, body)
	code, body = h.as("opal", "POST", "/v1/killswitch", in)
	h.want(200, code, body)
	if body["epoch"] != float64(2) || body["killed"] != false {
		t.Fatalf("resume = %v", body)
	}
	in["reason_code"] = "invented"
	code, body = h.as("opal", "POST", "/v1/killswitch", in)
	h.want(400, code, body)
}
