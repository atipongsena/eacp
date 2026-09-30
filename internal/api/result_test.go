package api_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/action"
	"github.com/atipongsena/eacp/internal/identity"
	"github.com/atipongsena/eacp/internal/storage"
)

// keepContractSQL is a read-only contract for tool $1 that keeps a
// success's output for five minutes (ADR-034).
const keepContractSQL = `INSERT INTO eacp.tool_contracts
	(tenant_id, tool_id, side_effects, idempotency_mode, reconciliation_lookup,
	 reconciliation_consistency, proof_standard, max_attempts, result_retention_seconds)
	VALUES (eacp.current_tenant_id(), $1, '{READ_ONLY}', 'none', 'none', 'none', 'none', 1, 300)
	RETURNING id`

// settle does what the worker does for queued action id: lease, intent and
// a success whose output (or withheld reason) is recorded, as w1.
func settle(t *testing.T, h *actionHarness, id uuid.UUID, output, withheld any) {
	t.Helper()
	ctx := context.Background()
	err := storage.InTenantTx(ctx, h.f.App, h.f.Tenant.String(), func(tx pgx.Tx) error {
		if err := storage.SetWorker(ctx, tx, "w1", 1); err != nil {
			return err
		}
		for _, sql := range []string{
			`UPDATE eacp.actions SET state = 'LEASED', lease_generation = lease_generation + 1, worker_id = 'w1',
				leased_until = now() + interval '1 minute' WHERE id = $1`,
			`UPDATE eacp.actions SET state = 'EXECUTING', attempt_count = attempt_count + 1,
				leased_until = now() + interval '1 minute' WHERE id = $1`,
			`UPDATE eacp.action_attempts SET outcome = 'succeeded', external_reference = 'R-1'
				WHERE action_id = $1 AND lease_generation = 1`,
		} {
			if _, err := tx.Exec(ctx, sql, id); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `SELECT eacp.action_result_record($1, $2, $3)`, id, output, withheld); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE eacp.actions SET state = 'SUCCEEDED', state_reason = 'succeeded' WHERE id = $1`, id)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// keeper is an agent allowed hr.balance, whose contract keeps output, with
// its key; it submits and settles one action and returns its id.
func keeper(t *testing.T, h *actionHarness) (string, func(output, withheld any) uuid.UUID) {
	t.Helper()
	tool := h.f.ActiveToolWith(t, "hr", "balance", keepContractSQL)
	agent := h.f.ActiveAgent(t, "hr-bot", tool.Tool)
	key := h.issue(identity.KindAgent, agent.Version, "erin", "rita")
	return key, func(output, withheld any) uuid.UUID {
		t.Helper()
		body := actionBody(1)
		body["tool"], body["target"] = "hr.balance", "hr"
		code, got, _ := h.send(key, "POST", "/v1/actions", map[string]string{"Idempotency-Key": uuid.NewString()}, body)
		h.want(202, code, got)
		id := uuid.MustParse(str(got, "id"))
		settle(t, h, id, output, withheld)
		return id
	}
}

func TestTheCallingAgentReadsItsResult(t *testing.T) {
	h := newActionHarness(t, allowPolicy, action.Limits{})
	key, run := keeper(t, h)
	id := run(`{"days":12}`, nil)
	path := "/v1/actions/" + id.String() + "/result"

	code, body, hdr := h.send(key, "GET", path, nil, nil)
	h.want(200, code, body)
	out, _ := body["output"].(map[string]any)
	if body["action_id"] != id.String() || out["days"] != float64(12) || len(str(body, "sha256")) != 64 ||
		body["bytes"] != float64(11) || str(body, "expires_at") == "" || hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("result = %v (%v)", body, hdr)
	}

	// Another agent, a principal and an action without a result see nothing.
	code, body, _ = h.send(h.key, "GET", path, nil, nil)
	h.want(404, code, body)
	if body["error"] != "result_not_available" {
		t.Fatalf("other agent = %v", body)
	}
	for _, who := range []string{"otto", "audra", "alice"} {
		code, body = h.as(who, "GET", path, nil)
		h.want(403, code, body)
	}
	_, queued, _ := h.submit("k1", 1, "")
	code, body, _ = h.send(h.key, "GET", "/v1/actions/"+str(queued, "id")+"/result", nil, nil)
	h.want(404, code, body)
	code, body, _ = h.send(key, "GET", "/v1/actions/not-a-uuid/result", nil, nil)
	h.want(400, code, body)

	// Operators see that a result exists, never its content.
	code, ev := h.as("otto", "GET", "/v1/actions/"+id.String()+"/evidence", nil)
	h.want(200, code, ev)
	meta, _ := ev["result"].(map[string]any)
	if meta == nil || meta["sha256"] != str(body0(t, h, key, path), "sha256") || meta["bytes"] != float64(11) {
		t.Fatalf("evidence result = %v", ev["result"])
	}
	if _, ok := meta["output"]; ok {
		t.Fatal("evidence carries the output")
	}

	// Once expired nothing is served.
	if err := storage.InTenantTx(context.Background(), h.f.Owner, h.f.Tenant.String(), func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `UPDATE eacp.action_results SET expires_at = now() - interval '1 second'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	code, body, _ = h.send(key, "GET", path, nil, nil)
	h.want(404, code, body)
}

// body0 reads path as key and returns the 200 body.
func body0(t *testing.T, h *actionHarness, key, path string) map[string]any {
	t.Helper()
	code, body, _ := h.send(key, "GET", path, nil, nil)
	h.want(200, code, body)
	return body
}

func TestAWithheldResultSaysWhy(t *testing.T) {
	h := newActionHarness(t, allowPolicy, action.Limits{})
	key, run := keeper(t, h)
	id := run(nil, "too_large")
	code, body, _ := h.send(key, "GET", "/v1/actions/"+id.String()+"/result", nil, nil)
	h.want(409, code, body)
	if body["error"] != "result_withheld" || body["reason"] != "too_large" || strings.Contains(str(body, "detail"), "{") {
		t.Fatalf("withheld = %v", body)
	}
	code, ev := h.as("audra", "GET", "/v1/actions/"+id.String()+"/evidence", nil)
	h.want(200, code, ev)
	if meta, _ := ev["result"].(map[string]any); meta == nil || meta["withheld"] != "too_large" {
		t.Fatalf("evidence result = %v", ev["result"])
	}
}
