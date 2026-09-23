package action_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"eacp/internal/action"
	"eacp/internal/registry"
	"eacp/internal/registry/registrytest"
	"eacp/internal/worker"
)

// Contracts with a 200 ms call budget, so an unknown outcome settles fast.
const (
	// noProofSQL: an irreversible write with no lookup (proof NONE).
	noProofSQL = `INSERT INTO eacp.tool_contracts
		(tenant_id, tool_id, side_effects, idempotency_mode, reconciliation_lookup,
		 reconciliation_consistency, proof_standard, max_attempts, timeout_ms)
		VALUES (eacp.current_tenant_id(), $1, '{IRREVERSIBLE_WRITE}', 'none', 'none', 'none', 'none', 1, 200)
		RETURNING id`
	// bestEffortSQL: an irreversible write with an eventually consistent lookup.
	bestEffortSQL = `INSERT INTO eacp.tool_contracts
		(tenant_id, tool_id, side_effects, idempotency_mode, reconciliation_lookup,
		 reconciliation_consistency, proof_standard, max_attempts, timeout_ms)
		VALUES (eacp.current_tenant_id(), $1, '{IRREVERSIBLE_WRITE}', 'none', 'by_operation_key', 'eventual',
		 'best_effort', 1, 200)
		RETURNING id`
	// authoritativeSQL: a natively idempotent write with a strong lookup.
	authoritativeSQL = `INSERT INTO eacp.tool_contracts
		(tenant_id, tool_id, side_effects, idempotency_mode, idempotency_key_field, reconciliation_lookup,
		 reconciliation_consistency, proof_standard, max_attempts, timeout_ms)
		VALUES (eacp.current_tenant_id(), $1, '{REVERSIBLE_WRITE}', 'native', 'Idempotency-Key',
		 'by_operation_key', 'strong', 'authoritative', 3, 200)
		RETURNING id`
)

// reconcileEnv adds write tools with the contracts above to env.
type reconcileEnv struct {
	env
	tools map[string]registrytest.Tooling
}

func newReconcileEnv(t *testing.T) reconcileEnv {
	t.Helper()
	v := reconcileEnv{env: newEnv(t, allowAll), tools: map[string]registrytest.Tooling{}}
	v.tools["erp.purchase"] = v.tool
	v.tools["ledger.none"] = v.f.ActiveToolWith(t, "ledger", "none", noProofSQL)
	v.tools["crm.best"] = v.f.ActiveToolWith(t, "crm", "best", bestEffortSQL)
	v.tools["bank.auth"] = v.f.ActiveToolWith(t, "bank", "auth", authoritativeSQL)
	ids := []uuid.UUID{}
	for _, tl := range v.tools {
		ids = append(ids, tl.Tool)
	}
	v.agent = v.f.ActiveAgent(t, "reconciled", ids...)
	return v
}

// unknown submits an action on tool and leaves it UNKNOWN_OUTCOME after an
// ambiguous result (T22).
func (v reconcileEnv) unknown(key, tool string) action.View {
	v.t.Helper()
	s := v.submission(key)
	s.Tool = tool
	got, err := v.e.Submit(context.Background(), v.actor(), s)
	if err != nil || got.State != "QUEUED" {
		v.t.Fatalf("submit %s = %+v %v", tool, got, err)
	}
	st, l := v.dispatch(got.ID, "w1")
	if to := v.complete(st, l, worker.Result{Outcome: worker.Ambiguous, ErrorClass: "timeout"}); to != "UNKNOWN_OUTCOME" {
		v.t.Fatalf("%s after an ambiguous result = %s", tool, to)
	}
	return v.get(got.ID)
}

// settle waits until every call of a has settled: the earliest time it
// may be reconciled or handed to a human.
func (v reconcileEnv) settle(a action.View) {
	v.t.Helper()
	if a.NextReconcileAt == nil {
		v.t.Fatalf("%s has no reconciliation time", a.ID)
	}
	time.Sleep(time.Until(*a.NextReconcileAt) + 20*time.Millisecond)
}

func (v reconcileEnv) sweepWith(fn func(*action.Sweeper)) action.Stats {
	v.t.Helper()
	s := action.NewSweeper(v.e)
	s.Grace = 0
	fn(s)
	st, err := s.RunOnce(context.Background())
	if err != nil {
		v.t.Fatal(err)
	}
	return st
}

func (v reconcileEnv) operator(name string) action.Actor {
	return action.Principal(v.f.Tenant, v.f.P[name])
}

func TestUnknownOutcomeIsScheduledAndShownWithItsEvidence(t *testing.T) {
	v := newReconcileEnv(t)
	got := v.unknown("k1", "crm.best")
	if got.NextReconcileAt == nil || got.ReconcileAttempts != 0 {
		t.Fatalf("unknown = %+v", got)
	}
	ev, err := v.e.Evidence(context.Background(), v.f.Tenant, got.ID)
	if err != nil || len(ev.Attempts) != 1 || ev.Attempts[0].Outcome != "ambiguous" || ev.Attempts[0].OperationKey != got.OperationKey {
		t.Fatalf("evidence = %+v %v", ev, err)
	}
	list, err := v.e.List(context.Background(), v.f.Tenant, "UNKNOWN_OUTCOME", 10)
	if err != nil || len(list) != 1 || list[0].ID != got.ID {
		t.Fatalf("list = %+v %v", list, err)
	}
	if _, err := v.e.List(context.Background(), v.f.Tenant, "NOPE", 10); !errors.Is(err, registry.ErrInvalid) {
		t.Fatalf("list of an unknown state = %v", err)
	}
}

func TestSweeperRoutesUnknownOutcomesWithoutProof(t *testing.T) {
	v := newReconcileEnv(t)
	none := v.unknown("k1", "ledger.none")
	best := v.unknown("k2", "crm.best")
	auth := v.unknown("k3", "bank.auth")
	// An unknown read cannot stay unknown: T29a retries it, and a read that
	// was cancelled during its call then fails (T27).
	read := v.submission("k4")
	r, err := v.e.Submit(context.Background(), v.actor(), read)
	if err != nil {
		t.Fatal(err)
	}
	st, l := v.dispatch(r.ID, "w1")
	if _, err := v.e.Cancel(context.Background(), v.actor(), r.ID, "withdrawn"); err != nil {
		t.Fatal(err)
	}
	v.complete(st, l, worker.Result{Outcome: worker.Ambiguous, ErrorClass: "timeout"})

	// Nobody is asked to resolve an outcome before its calls have settled:
	// a late result may still arrive.
	stats := v.sweepWith(func(s *action.Sweeper) { s.ReconcileMaxAge = time.Millisecond })
	if stats.Escalated != 0 || v.get(none.ID).State != "UNKNOWN_OUTCOME" {
		t.Fatalf("escalated before the call settled: %+v", stats)
	}
	if got := v.get(r.ID); got.State != "FAILED" || got.StateReason != "cancelled" {
		t.Fatalf("unknown read = %s (%s)", got.State, got.StateReason)
	}
	v.settle(auth)

	stats = v.sweepWith(func(*action.Sweeper) {})
	if stats.Escalated != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if got := v.get(none.ID); got.State != "NEEDS_HUMAN_RESOLUTION" || got.StateReason != "no reconciliation proof: proof standard none" {
		t.Fatalf("proof NONE = %s (%s)", got.State, got.StateReason)
	}
	// Lookups are the reconciler's: the sweeper leaves them alone...
	if v.get(best.ID).State != "UNKNOWN_OUTCOME" || v.get(auth.ID).State != "UNKNOWN_OUTCOME" {
		t.Fatal("the sweeper took an action with a usable lookup")
	}
	if n := v.count(`SELECT count(*) FROM eacp.audit_events
		WHERE convert_from(payload, 'UTF8')::jsonb->'subject'->>'id' = $1::text
		  AND convert_from(payload, 'UTF8')::jsonb->'data'->>'from' = 'UNKNOWN_OUTCOME'
		  AND convert_from(payload, 'UTF8')::jsonb->'data'->>'to' = 'RETRY_WAIT'`, r.ID); n != 1 {
		t.Fatalf("T29a journal entries = %d", n)
	}
	// ...until reconciliation has taken too long (T34).
	v.sweepWith(func(s *action.Sweeper) { s.ReconcileMaxAge = time.Millisecond })
	for _, id := range []uuid.UUID{best.ID, auth.ID} {
		if got := v.get(id); got.State != "NEEDS_HUMAN_RESOLUTION" || got.StateReason != "reconciliation time exhausted" {
			t.Fatalf("exhausted = %s (%s)", got.State, got.StateReason)
		}
	}
}

func TestSweeperReturnsALapsedReconcilerLease(t *testing.T) {
	v := newReconcileEnv(t)
	got := v.unknown("k1", "crm.best")
	time.Sleep(time.Until(*got.NextReconcileAt) + 50*time.Millisecond)
	rec := worker.NewStore(v.f.App, "r1")
	l, ok, err := rec.ClaimReconcile(context.Background(), worker.Candidate{TenantID: v.f.Tenant, ActionID: got.ID}, time.Second)
	if err != nil || !ok || l.Generation != 2 || v.get(got.ID).State != "RECONCILING" {
		t.Fatalf("claim = %v %v %+v", ok, err, l)
	}
	if st := v.sweep(); st.Reclaimed != 0 {
		t.Fatalf("a live reconciler lease was reclaimed: %+v", st)
	}
	time.Sleep(1100 * time.Millisecond)
	if st := v.sweep(); st.Reclaimed != 1 {
		t.Fatalf("stats = %+v", st)
	}
	after := v.get(got.ID)
	if after.State != "UNKNOWN_OUTCOME" || after.ReconcileAttempts != 1 || after.StateReason != "reconciler lease expired" {
		t.Fatalf("T33 = %+v", after)
	}
}

func TestOperatorsResolveWhatNeedsAHuman(t *testing.T) {
	v := newReconcileEnv(t)
	ctx := context.Background()
	escalate := func(key, tool string) action.View {
		t.Helper()
		got := v.unknown(key, tool)
		v.settle(got)
		v.sweepWith(func(s *action.Sweeper) { s.ReconcileMaxAge = time.Millisecond })
		if got = v.get(got.ID); got.State != "NEEDS_HUMAN_RESOLUTION" {
			t.Fatalf("%s = %s", key, got.State)
		}
		return got
	}

	ok := escalate("k1", "ledger.none")
	for name, who := range map[string]action.Actor{
		"agent": v.actor(), "subject": v.operator("carol"), "non-operator": v.operator("erin"),
	} {
		_, _, err := v.e.Resolve(ctx, who, ok.ID, action.Resolution{Outcome: "succeeded", Reason: "seen",
			Evidence: "ERP screen", ExternalReference: "PO-1"})
		if !errors.Is(err, registry.ErrForbidden) {
			t.Fatalf("%s resolved: %v", name, err)
		}
	}
	for _, bad := range []action.Resolution{
		{Outcome: "maybe", Reason: "x", Evidence: "y"},
		{Outcome: "succeeded", Reason: "seen", Evidence: "ERP screen"},
		{Outcome: "failed", Reason: "not there"},
	} {
		if _, _, err := v.e.Resolve(ctx, v.operator("otto"), ok.ID, bad); !errors.Is(err, registry.ErrInvalid) {
			t.Fatalf("resolution %+v = %v, want ErrInvalid", bad, err)
		}
	}
	got, res, err := v.e.Resolve(ctx, v.operator("otto"), ok.ID, action.Resolution{Outcome: "succeeded",
		Reason: "found in the ERP", Evidence: "ERP search by operation key", ExternalReference: "PO-7"})
	if err != nil || got.State != "SUCCEEDED" || got.ExternalReference != "PO-7" || res.State != "APPLIED" ||
		res.DecidedBy == nil || *res.DecidedBy != v.f.P["otto"] {
		t.Fatalf("resolve = %+v %+v %v", got, res, err)
	}

	// A retry is two-person and keeps the operation key.
	retry := escalate("k2", "bank.auth")
	got, proposal, err := v.e.Resolve(ctx, v.operator("otto"), retry.ID, action.Resolution{Outcome: "retry",
		Reason: "the ERP team confirmed nothing arrived"})
	if err != nil || got.State != "NEEDS_HUMAN_RESOLUTION" || proposal.State != "PROPOSED" {
		t.Fatalf("propose = %+v %+v %v", got, proposal, err)
	}
	if _, _, err := v.e.DecideResolution(ctx, v.operator("otto"), retry.ID, proposal.ID, true, "self"); !errors.Is(err, registry.ErrForbidden) {
		t.Fatalf("self-confirmation = %v", err)
	}
	if _, _, err := v.e.DecideResolution(ctx, v.operator("opal"), retry.ID, uuid.New(), true, "x"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("unknown resolution = %v", err)
	}
	got, res, err = v.e.DecideResolution(ctx, v.operator("opal"), retry.ID, proposal.ID, true, "agreed")
	if err != nil || got.State != "RETRY_WAIT" || res.State != "APPLIED" {
		t.Fatalf("confirm = %+v %+v %v", got, res, err)
	}
	v.sweep()
	st, l := v.dispatch(retry.ID, "w2")
	if l.Generation != 2 || v.complete(st, l, worker.Result{Outcome: worker.Succeeded, ExternalReference: "PO-8"}) != "SUCCEEDED" {
		t.Fatalf("retried generation %d", l.Generation)
	}
	if got := v.get(retry.ID); got.OperationKey != retry.OperationKey || got.AttemptCount != 2 {
		t.Fatalf("after retry = %+v", got)
	}

	// Withdrawing a proposal, then failing the action.
	fail := escalate("k3", "bank.auth")
	_, p, err := v.e.Resolve(ctx, v.operator("otto"), fail.ID, action.Resolution{Outcome: "retry", Reason: "maybe"})
	if err != nil {
		t.Fatal(err)
	}
	if _, res, err := v.e.DecideResolution(ctx, v.operator("otto"), fail.ID, p.ID, false, "changed my mind"); err != nil || res.State != "WITHDRAWN" {
		t.Fatalf("withdraw = %+v %v", res, err)
	}
	got, _, err = v.e.Resolve(ctx, v.operator("opal"), fail.ID, action.Resolution{Outcome: "failed",
		Reason: "not in the ERP", Evidence: "ERP audit export"})
	if err != nil || got.State != "FAILED" {
		t.Fatalf("fail = %+v %v", got, err)
	}
	ev, err := v.e.Evidence(ctx, v.f.Tenant, fail.ID)
	if err != nil || len(ev.Resolutions) != 2 || ev.Resolutions[0].State != "WITHDRAWN" || ev.Resolutions[1].Outcome != "failed" {
		b, _ := json.Marshal(ev)
		t.Fatalf("evidence = %s %v", b, err)
	}
	if _, _, err := v.e.Resolve(ctx, v.operator("opal"), fail.ID, action.Resolution{Outcome: "failed",
		Reason: "again", Evidence: "x"}); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("resolving a terminal action = %v", err)
	}
}
