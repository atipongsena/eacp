package api_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"

	"eacp/internal/storage"
	"eacp/internal/storage/pgtest"
)

const bundleDoc = `{
 "connectors": {"ledger": {"protocol": "http", "endpoint": "http://fakeerp:8090", "secret_ref": "ledger",
   "tools": {"post_entry": {"contract": {"side_effects": ["READ_ONLY"], "idempotency_mode": "none",
     "reconciliation_lookup": "none", "reconciliation_consistency": "none", "proof_standard": "none",
     "max_attempts": 3}}}}},
 "agents": {"ledger-bot": {"display_name": "Ledger bot", "environment": "production", "risk_class": "high",
   "owner": {"principal": "carol"}, "version": {"runtime": "python", "code_ref": "git:aaa111"},
   "allowlist": ["ledger.post_entry"], "state": "ACTIVE"}}}`

func TestChangeSetAPI(t *testing.T) {
	h := newHarness(t)
	plan := map[string]any{"bundle": "ledger", "desired": json.RawMessage(bundleDoc)}

	code, body := h.as("carol", "POST", "/v1/change-sets", plan)
	h.want(403, code, body)
	code, body = h.as("erin", "POST", "/v1/change-sets", map[string]any{"bundle": "ledger", "desired": json.RawMessage(bundleDoc), "dry_run": true})
	h.want(200, code, body)
	if body["id"] != nil || len(body["steps"].([]any)) != 9 {
		t.Fatalf("dry run = %v", body)
	}
	code, body = h.as("erin", "POST", "/v1/change-sets", plan)
	h.want(201, code, body)
	id, _ := body["id"].(string)

	code, body = h.as("erin", "POST", "/v1/change-sets/"+id+"/submit", nil)
	h.want(200, code, body)
	if body["state"] != "SUBMITTED" {
		t.Fatalf("submit = %v", body)
	}
	code, body = h.as("erin", "POST", "/v1/change-sets/"+id+"/approve", nil)
	h.want(403, code, body) // not an approver (and the submitter)
	code, body = h.as("rita", "POST", "/v1/change-sets/"+id+"/approve", nil)
	h.want(200, code, body)
	if body["state"] != "APPLIED" {
		t.Fatalf("approve = %v", body)
	}

	code, body = h.as("audra", "GET", "/v1/change-sets?bundle=ledger", nil)
	h.want(200, code, body)
	if cs, _ := body["change_sets"].([]any); len(cs) != 1 {
		t.Fatalf("list = %v", body)
	}
	code, body = h.as("audra", "GET", "/v1/bundles", nil)
	h.want(200, code, body)
	code, body = h.as("audra", "GET", "/v1/bundles/ledger/drift", nil)
	h.want(200, code, body)
	if entries, _ := body["entries"].([]any); len(entries) != 4 {
		t.Fatalf("drift = %v", body)
	}
	code, body = h.as("audra", "GET", "/v1/bundles/nope/drift", nil)
	h.want(404, code, body)
}

func TestChangeSetErrorCodes(t *testing.T) {
	h := newHarness(t)
	plan := map[string]any{"bundle": "ledger", "desired": json.RawMessage(bundleDoc)}

	bad := map[string]any{"bundle": "ledger", "desired": json.RawMessage(`{"agents": {"x": {"password": "p"}}}`)}
	code, body := h.as("erin", "POST", "/v1/change-sets", bad)
	h.want(400, code, body)

	blocked := map[string]any{"bundle": "ledger",
		"desired": json.RawMessage(`{"agents": {"bot": {"display_name": "B", "environment": "production",
			"risk_class": "low", "owner": {"principal": "nobody"}, "version": {"runtime": "go", "code_ref": "g:1"}}}}`)}
	code, body = h.as("erin", "POST", "/v1/change-sets", blocked)
	h.want(422, code, body)
	if body["error"] != "plan_blocked" || body["findings"] == nil {
		t.Fatalf("blocked = %v", body)
	}

	// rita also writes the registry, so she can submit and then try to approve.
	if err := storage.InTenantTx(context.Background(), h.f.Owner, h.f.Tenant.String(), func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `INSERT INTO eacp.role_grants (tenant_id, principal_id, role, approved_at)
			VALUES (eacp.current_tenant_id(), $1, 'registry_editor', now())`, h.f.P["rita"])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	code, body = h.as("rita", "POST", "/v1/change-sets", plan)
	h.want(201, code, body)
	id := body["id"].(string)
	code, body = h.as("rita", "POST", "/v1/change-sets/"+id+"/submit", nil)
	h.want(200, code, body)
	code, body = h.as("rita", "POST", "/v1/change-sets/"+id+"/approve", nil)
	h.want(403, code, body)
	if body["error"] != "same_principal" {
		t.Fatalf("self-approval = %v", body)
	}
	code, body = h.as("erin", "POST", "/v1/change-sets", plan)
	h.want(409, code, body)
	if body["error"] != "change_set_open" {
		t.Fatalf("open = %v", body)
	}
	code, body = h.as("erin", "POST", "/v1/change-sets/"+id+"/reject", map[string]any{"reason": "redo"})
	h.want(200, code, body)
	code, body = h.as("erin", "POST", "/v1/change-sets/not-a-uuid/submit", nil)
	h.want(400, code, body)
}

func TestChangeSetsOfOtherTenantsAreNotFound(t *testing.T) {
	h := newHarness(t)
	code, body := h.as("erin", "POST", "/v1/change-sets", map[string]any{"bundle": "ledger",
		"desired": json.RawMessage(bundleDoc)})
	h.want(201, code, body)
	id := body["id"].(string)
	stranger := h.principalIn(pgtest.TenantB, "eve", "registry_approver", "auditor")
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/change-sets/" + id}, {"POST", "/v1/change-sets/" + id + "/submit"},
		{"POST", "/v1/change-sets/" + id + "/approve"}, {"GET", "/v1/bundles/ledger/drift"},
	} {
		code, body := h.do(stranger, c.method, c.path, nil)
		h.want(404, code, body)
	}
	code, body = h.do(stranger, "GET", "/v1/change-sets", nil)
	h.want(200, code, body)
	if cs, _ := body["change_sets"].([]any); len(cs) != 0 {
		t.Fatalf("tenant B lists %v", cs)
	}
}
