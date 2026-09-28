package api_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/action"
	"github.com/atipongsena/eacp/internal/identity"
	"github.com/atipongsena/eacp/internal/storage"
	"github.com/atipongsena/eacp/internal/storage/pgtest"
)

// principalIn bootstraps a principal with roles in tenant (the owner's
// break-glass path, as for a new tenant) and returns its API key.
func (h *harness) principalIn(tenant, name string, roles ...string) string {
	h.t.Helper()
	ctx := context.Background()
	credID := uuid.New()
	key, hash, err := identity.NewKey(identity.KindPrincipal, uuid.MustParse(tenant), credID)
	if err != nil {
		h.t.Fatal(err)
	}
	err = storage.InTenantTx(ctx, h.f.Owner, tenant, func(tx pgx.Tx) error {
		var p uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO eacp.principals (tenant_id, kind, name, subject, display_name)
			VALUES (eacp.current_tenant_id(), 'human', $1, $1 || '@b.test', $1) RETURNING id`, name).Scan(&p); err != nil {
			return err
		}
		for _, r := range roles {
			if _, err := tx.Exec(ctx, `INSERT INTO eacp.role_grants (tenant_id, principal_id, role, approved_at)
				VALUES (eacp.current_tenant_id(), $1, $2, now())`, p, r); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO eacp.credentials (tenant_id, id, kind, principal_id, secret_hash, expires_at, approved_at)
			VALUES (eacp.current_tenant_id(), $1, 'pk', $2, $3, now() + interval '1 day', now())`, credID, p, hash)
		return err
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return key
}

// Invariant 8 over the action API: an operator and an auditor of tenant B,
// holding exactly the roles that would let them act in tenant A, cannot
// see, list, reconstruct, resolve or cancel tenant A's action. Every answer
// is the one for an action that does not exist.
func TestActionsOfOtherTenantsAreNotFound(t *testing.T) {
	h := newActionHarness(t, allowPolicy, action.Limits{})
	tool := h.f.ActiveToolWith(t, "ledger", "post", idempotentWriteSQL)
	agent := h.f.ActiveAgent(t, "poster", tool.Tool)
	agentKey := h.issue(identity.KindAgent, agent.Version, "erin", "rita")
	id := h.needsHuman(agentKey, "k1")
	path := "/v1/actions/" + id
	eve := h.principalIn(pgtest.TenantB, "eve", "operator", "auditor")
	missing := "/v1/actions/" + uuid.NewString()
	code, before, _ := h.send(eve, "GET", "/v1/audit/verify", nil, nil)
	h.want(200, code, before)

	for _, p := range []string{path, missing} {
		for _, req := range []struct {
			method, path string
			body         any
		}{
			{"GET", p, nil},
			{"GET", p + "/evidence", nil},
			{"POST", p + "/cancel", map[string]any{"reason": "b"}},
			{"POST", p + "/resolutions", map[string]any{"outcome": "failed", "reason": "b", "evidence": "b"}},
			{"POST", p + "/resolutions/" + uuid.NewString() + "/confirm", map[string]any{"reason": "b"}},
		} {
			code, body, _ := h.send(eve, req.method, req.path, nil, req.body)
			h.want(404, code, body)
		}
	}
	code, list, _ := h.send(eve, "GET", "/v1/actions?state=NEEDS_HUMAN_RESOLUTION", nil, nil)
	h.want(200, code, list)
	if items, _ := list["actions"].([]any); len(items) != 0 {
		t.Fatalf("tenant B lists %v", list)
	}
	// Tenant B's journal records nothing about any of it.
	code, after, _ := h.send(eve, "GET", "/v1/audit/verify", nil, nil)
	h.want(200, code, after)
	if after["valid"] != true || after["count"] != before["count"] {
		t.Fatalf("tenant B's journal went from %v to %v", before, after)
	}
	// Nothing changed in tenant A.
	code, got := h.as("otto", "GET", path, nil)
	h.want(202, code, got) // not terminal
	if got["state"] != "NEEDS_HUMAN_RESOLUTION" || got["cancel_requested_at"] != nil {
		t.Fatalf("tenant A action = %v", got)
	}
}
