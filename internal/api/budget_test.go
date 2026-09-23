package api_test

import (
	"testing"

	"github.com/google/uuid"

	"eacp/internal/action"
	"eacp/internal/api"
	"eacp/internal/governance"
	"eacp/internal/identity"
	"eacp/internal/registry/registrytest"
)

func TestBudgetsThroughTheAPI(t *testing.T) {
	h := newHarness(t, func(f *registrytest.Fixture, s *api.Server) {
		s.WithActions(action.New(f.App, action.Options{Provider: governance.LocalProvider{InstanceID: "api-test"}}))
	})
	ah := &actionHarness{harness: h}

	// A costed contract: the payload's amount in THB (ADR-012 §1).
	code, conn := h.as("erin", "POST", "/v1/connectors", map[string]any{
		"name": "erp", "protocol": "http", "endpoint": "http://fakeerp:8090", "secret_ref": "erp"})
	h.want(201, code, conn)
	code, tool := h.as("erin", "POST", "/v1/connectors/"+str(conn, "id")+"/tools", map[string]any{"name": "purchase"})
	h.want(201, code, tool)
	contract := map[string]any{"side_effects": []string{"FINANCIAL"}, "idempotency_mode": "none",
		"reconciliation_lookup": "none", "reconciliation_consistency": "none", "proof_standard": "none",
		"max_attempts": 1, "cost_unit": "THB", "cost_amount_field": "amount", "cost_unit_field": "currency"}
	code, body := h.as("erin", "POST", "/v1/tools/"+str(tool, "id")+"/contracts", map[string]any{
		"side_effects": []string{"FINANCIAL"}, "idempotency_mode": "none", "reconciliation_lookup": "none",
		"reconciliation_consistency": "none", "proof_standard": "none", "max_attempts": 1, "cost_fixed": 5})
	h.want(400, code, body) // a cost needs a unit
	code, c := h.as("erin", "POST", "/v1/tools/"+str(tool, "id")+"/contracts", contract)
	h.want(201, code, c)
	code, body = h.as("rita", "POST", "/v1/tools/"+str(tool, "id")+"/contract", map[string]any{"contract_id": str(c, "id")})
	h.want(204, code, body)
	agent := h.f.ActiveAgent(t, "buyer", uuid.MustParse(str(tool, "id")))
	ah.key = h.issue(identity.KindAgent, agent.Version, "erin", "rita")
	ah.activate(allowPolicy)

	// Without an account the release is denied, fail-closed.
	code, got, _ := ah.submit("k0", 1000, "")
	if code != 200 || got["state"] != "DENIED" || got["state_reason"] != action.BudgetAccountMissing {
		t.Fatalf("without an account: %d %v", code, got)
	}

	code, body = h.as("erin", "POST", "/v1/budgets", map[string]any{"name": "buyer", "unit": "THB", "agent_id": agent.Agent})
	h.want(403, code, body)
	code, acct := h.as("alice", "POST", "/v1/budgets", map[string]any{"name": "buyer", "unit": "THB", "agent_id": agent.Agent})
	h.want(201, code, acct)
	if acct["hard_limit"] != float64(0) {
		t.Fatalf("new account = %v", acct)
	}
	path := "/v1/budgets/" + str(acct, "id")
	for _, bad := range []string{"-1", "abc", "1.0000001", "1000000000000000"} {
		code, body = h.as("alice", "POST", path+"/limit", map[string]any{"limit": bad, "reason": "x"})
		h.want(400, code, body)
	}
	code, raise := h.as("alice", "POST", path+"/limit", map[string]any{"limit": "5000000", "reason": "Q4 purchasing"})
	h.want(201, code, raise)
	if raise["state"] != "PROPOSED" {
		t.Fatalf("raise = %v", raise)
	}
	decide := "/v1/budget-limit-changes/" + str(raise, "id")
	code, body = h.as("alice", "POST", decide+"/approve", map[string]any{"reason": "mine"})
	h.want(403, code, body) // two-person
	code, body = h.as("otto", "POST", decide+"/approve", map[string]any{"reason": "ok"})
	h.want(403, code, body)
	code, body = h.as("bob", "POST", decide+"/approve", map[string]any{"reason": "agreed"})
	h.want(200, code, body)
	if body["state"] != "APPLIED" {
		t.Fatalf("approval = %v", body)
	}

	// Two purchases of 2 400 000 fit in 5 000 000; the third is denied.
	for i, want := range []string{"QUEUED", "QUEUED", "DENIED"} {
		code, got, _ := ah.submit("k"+string(rune('1'+i)), 2400000, "")
		if code != 200 && code != 202 || got["state"] != want {
			t.Fatalf("purchase %d: %d %v", i+1, code, got)
		}
		if want == "DENIED" && got["state_reason"] != action.BudgetExceeded {
			t.Fatalf("denial = %v", got)
		}
	}
	code, view := h.as("audra", "GET", path, nil)
	h.want(200, code, view)
	if view["reserved"] != float64(4800000) || view["available"] != float64(200000) || view["hard_limit"] != float64(5000000) {
		t.Fatalf("account = %v", view)
	}
	code, list := h.as("otto", "GET", "/v1/budgets", nil)
	h.want(200, code, list)
	if l, _ := list["budgets"].([]any); len(l) != 1 {
		t.Fatalf("list = %v", list)
	}
	code, body = h.as("carol", "GET", "/v1/budgets", nil)
	h.want(403, code, body)

	// One admin lowers at once, but never below what is reserved.
	code, body = h.as("alice", "POST", path+"/limit", map[string]any{"limit": "4000000", "reason": "cut"})
	h.want(409, code, body)
	code, body = h.as("alice", "POST", path+"/limit", map[string]any{"limit": "4800000", "reason": "cut"})
	h.want(201, code, body)
	if body["state"] != "APPLIED" {
		t.Fatalf("decrease = %v", body)
	}
}
