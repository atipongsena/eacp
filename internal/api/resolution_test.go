package api_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/action"
	"github.com/atipongsena/eacp/internal/identity"
	"github.com/atipongsena/eacp/internal/worker"
)

// idempotentWriteSQL is a natively idempotent write with no lookup: its
// unknown outcomes always need a human (T29), and a retry has attempts left.
const idempotentWriteSQL = `INSERT INTO eacp.tool_contracts
	(tenant_id, tool_id, side_effects, idempotency_mode, idempotency_key_field, reconciliation_lookup,
	 reconciliation_consistency, proof_standard, max_attempts, timeout_ms)
	VALUES (eacp.current_tenant_id(), $1, '{REVERSIBLE_WRITE}', 'native', 'Idempotency-Key', 'none', 'none',
	 'none', 3, 200)
	RETURNING id`

// needsHuman submits a ledger.post action with agentKey and drives it to
// NEEDS_HUMAN_RESOLUTION: an ambiguous result, then, once the call has
// settled, the sweeper (T29).
func (h *actionHarness) needsHuman(agentKey, idem string) string {
	h.t.Helper()
	body := actionBody(1)
	body["tool"] = "ledger.post"
	code, queued, _ := h.send(agentKey, "POST", "/v1/actions", map[string]string{"Idempotency-Key": idem}, body)
	h.want(202, code, queued)
	id := uuid.MustParse(str(queued, "id"))
	ctx := context.Background()
	s := worker.NewStore(h.f.App, "w1")
	l, ok, err := s.Claim(ctx, worker.Candidate{TenantID: h.f.Tenant, ActionID: id}, time.Minute)
	if err != nil || !ok {
		h.t.Fatalf("claim = %v %v", ok, err)
	}
	if d, _, err := s.Intent(ctx, l, 2*time.Second); err != nil || d != worker.Dispatched {
		h.t.Fatalf("intent = %s %v", d, err)
	}
	if c, err := s.Complete(ctx, l, worker.Result{Outcome: worker.Ambiguous, ErrorClass: "timeout"}, time.Second); err != nil ||
		c.State != "UNKNOWN_OUTCOME" {
		h.t.Fatalf("complete = %+v %v", c, err)
	}
	v, err := h.actions.Get(ctx, h.f.Tenant, id)
	if err != nil || v.NextReconcileAt == nil {
		h.t.Fatalf("unknown = %+v %v", v, err)
	}
	time.Sleep(time.Until(*v.NextReconcileAt) + 20*time.Millisecond) // until the call has settled
	sw := action.NewSweeper(h.actions)
	if st, err := sw.RunOnce(ctx); err != nil || st.Escalated != 1 {
		h.t.Fatalf("sweep = %+v %v", st, err)
	}
	return id.String()
}

func TestOperatorsResolveActionsOverTheAPI(t *testing.T) {
	h := newActionHarness(t, allowPolicy, action.Limits{})
	tool := h.f.ActiveToolWith(t, "ledger", "post", idempotentWriteSQL)
	agent := h.f.ActiveAgent(t, "poster", tool.Tool)
	agentKey := h.issue(identity.KindAgent, agent.Version, "erin", "rita")

	id := h.needsHuman(agentKey, "k1")
	path := "/v1/actions/" + id
	// The queue and the evidence are for operators and auditors.
	for _, who := range []string{"otto", "audra"} {
		code, body := h.as(who, "GET", "/v1/actions?state=NEEDS_HUMAN_RESOLUTION", nil)
		h.want(200, code, body)
		if list, _ := body["actions"].([]any); len(list) != 1 {
			t.Fatalf("%s queue = %v", who, body)
		}
		code, body = h.as(who, "GET", path+"/evidence", nil)
		h.want(200, code, body)
		attempts, _ := body["attempts"].([]any)
		journal, _ := body["journal"].([]any)
		chain, _ := body["chain"].(map[string]any)
		if len(attempts) != 1 || len(journal) == 0 || chain["verified"] != true {
			t.Fatalf("evidence = %v", body)
		}
	}
	code, body := h.as("carol", "GET", "/v1/actions?state=NEEDS_HUMAN_RESOLUTION", nil)
	h.want(403, code, body)
	code, body = h.as("otto", "GET", "/v1/actions", nil)
	h.want(400, code, body)
	code, body = h.as("otto", "GET", "/v1/actions?state=MAYBE", nil)
	h.want(400, code, body)

	succeed := map[string]any{"outcome": "succeeded", "reason": "found in the ERP",
		"evidence": "ERP search by operation key", "external_reference": "PO-7"}
	code, body = h.as("audra", "POST", path+"/resolutions", succeed) // auditors observe only
	h.want(403, code, body)
	code, body, _ = h.send(agentKey, "POST", path+"/resolutions", nil, succeed)
	h.want(403, code, body)
	code, body = h.as("otto", "POST", path+"/resolutions", map[string]any{"outcome": "succeeded", "reason": "x",
		"evidence": "y"})
	h.want(400, code, body) // a success needs its external reference
	code, body = h.as("otto", "POST", path+"/resolutions", succeed)
	h.want(200, code, body)
	if a, _ := body["action"].(map[string]any); a["state"] != "SUCCEEDED" || a["external_reference"] != "PO-7" {
		t.Fatalf("resolved = %v", body)
	}
	code, body = h.as("otto", "POST", path+"/resolutions", succeed)
	h.want(409, code, body)

	// A retry: proposed (202), confirmed by a second operator (200).
	id = h.needsHuman(agentKey, "k2")
	path = "/v1/actions/" + id
	code, body = h.as("otto", "POST", path+"/resolutions", map[string]any{"outcome": "retry",
		"reason": "the ERP team confirmed nothing arrived"})
	h.want(202, code, body)
	rid := str(body["resolution"].(map[string]any), "id")
	code, body = h.as("otto", "POST", path+"/resolutions/"+rid+"/confirm", map[string]any{"reason": "self"})
	h.want(403, code, body)
	code, body = h.as("opal", "POST", path+"/resolutions/"+uuid.NewString()+"/confirm", map[string]any{"reason": "x"})
	h.want(404, code, body)
	code, body = h.as("opal", "POST", path+"/resolutions/"+rid+"/confirm", map[string]any{"reason": "agreed"})
	h.want(200, code, body)
	if a, _ := body["action"].(map[string]any); a["state"] != "RETRY_WAIT" {
		t.Fatalf("confirmed = %v", body)
	}
}
