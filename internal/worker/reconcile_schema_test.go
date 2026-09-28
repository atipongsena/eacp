package worker_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/registry/registrytest"
	"github.com/atipongsena/eacp/internal/storage"
	"github.com/atipongsena/eacp/internal/storage/pgtest"
)

// Raw SQL for the reconciliation and resolution moves of migration 00007,
// run as eacp_app (ADR-004 T28-T37, T29a).
const (
	reconClaimSQL = `UPDATE eacp.actions SET state = 'RECONCILING', lease_generation = lease_generation + 1,
		worker_id = $2, leased_until = now() + make_interval(secs => $3) WHERE id = $1`
	checkSQL = `INSERT INTO eacp.reconciliation_checks (tenant_id, action_id, lease_generation, result, external_reference)
		VALUES (eacp.current_tenant_id(), $1, $2, $3, NULLIF($4, ''))`
	moveSQL  = `UPDATE eacp.actions SET state = $2, state_reason = $3 WHERE id = $1`
	stillSQL = `UPDATE eacp.actions SET state = 'UNKNOWN_OUTCOME', state_reason = 'still unknown',
		reconcile_attempts = reconcile_attempts + $2, next_reconcile_at = now() + make_interval(secs => $3) WHERE id = $1`
	retryAfterSQL = `UPDATE eacp.actions SET state = 'RETRY_WAIT', state_reason = 'authoritative absence',
		next_attempt_at = now() + interval '1 second' WHERE id = $1`
	resolveSQL = `INSERT INTO eacp.action_resolutions (tenant_id, action_id, outcome, reason, evidence, external_reference)
		VALUES (eacp.current_tenant_id(), $1, $2, $3, NULLIF($4, ''), NULLIF($5, '')) RETURNING id`
	confirmSQL  = `UPDATE eacp.action_resolutions SET state = 'APPLIED', decision_reason = $2 WHERE id = $1`
	withdrawSQL = `UPDATE eacp.action_resolutions SET state = 'WITHDRAWN', decision_reason = $2 WHERE id = $1`
)

// inReconciler runs fn in tenant A as reconciler rec at generation gen.
func (s schema) inReconciler(rec string, gen int64, fn func(context.Context, pgx.Tx) error) error {
	ctx := context.Background()
	return storage.InTenantTx(ctx, s.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		if err := storage.SetReconciler(ctx, tx, rec, gen); err != nil {
			return err
		}
		return fn(ctx, tx)
	})
}

// execReconciler runs one statement as reconciler rec at generation gen.
func (s schema) execReconciler(rec string, gen int64, sql string, args ...any) error {
	return s.inReconciler(rec, gen, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	})
}

// reconcile records check result (and ref) at gen and runs move, locking
// the action first, all in one reconciler transaction.
func (s schema) reconcile(rec string, gen int64, id uuid.UUID, result, ref, move string, args ...any) error {
	return s.inReconciler(rec, gen, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM eacp.actions WHERE id = $1 FOR UPDATE`, id); err != nil {
			return err
		}
		if result != "" {
			if _, err := tx.Exec(ctx, checkSQL, id, gen, result, ref); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, move, append([]any{id}, args...)...)
		return err
	})
}

// owner runs sql as the schema owner with the action and attempt guards
// disabled, to move clocks the guards never let move.
func (s schema) owner(t *testing.T, sql string) {
	t.Helper()
	ctx := context.Background()
	must(t, storage.InTenantTx(ctx, s.f.Owner, pgtest.TenantA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `ALTER TABLE eacp.actions DISABLE TRIGGER actions_guard;
			ALTER TABLE eacp.action_attempts DISABLE TRIGGER action_attempts_guard;
			`+sql+`;
			SET CONSTRAINTS ALL IMMEDIATE;
			ALTER TABLE eacp.action_attempts ENABLE TRIGGER action_attempts_guard;
			ALTER TABLE eacp.actions ENABLE TRIGGER actions_guard`)
		return err
	}))
}

// settle makes an unknown action due for reconciliation and its attempts'
// call deadlines long past.
func (s schema) settle(t *testing.T, id uuid.UUID) {
	t.Helper()
	s.owner(t, `UPDATE eacp.action_attempts SET call_deadline = now() - interval '1 hour'
		WHERE action_id = '`+id.String()+`';
		UPDATE eacp.actions SET next_reconcile_at = now() - interval '1 second'
		WHERE id = '`+id.String()+`' AND state = 'UNKNOWN_OUTCOME'`)
}

// unknown returns an action on tool left UNKNOWN_OUTCOME by an ambiguous w1
// result at generation 1 (T22), settled and due for reconciliation.
func (s schema) unknown(t *testing.T, tool string) uuid.UUID {
	t.Helper()
	id := s.executing(t, tool)
	must(t, s.finish("w1", 1, id, "ambiguous", "", "", "UNKNOWN_OUTCOME", nil))
	s.settle(t, id)
	return id
}

// needsHuman returns an action on tool moved to NEEDS_HUMAN_RESOLUTION by
// the sweeper.
func (s schema) needsHuman(t *testing.T, tool string) uuid.UUID {
	t.Helper()
	id := s.unknown(t, tool)
	must(t, s.f.ExecSystem("sweeper", moveSQL, id, "NEEDS_HUMAN_RESOLUTION", "no proof"))
	return id
}

func (s schema) journal(t *testing.T, id uuid.UUID, kind string) int {
	t.Helper()
	return s.count(t, `SELECT count(*) FROM eacp.audit_events WHERE convert_from(payload, 'UTF8')::jsonb->>'action' = $2
		AND convert_from(payload, 'UTF8')::jsonb->'subject'->>'id' = $1::text`, id, kind)
}

func newReconcileSchema(t *testing.T) (schema, registrytest.Tooling) {
	t.Helper()
	s := newSchema(t)
	none := s.f.ActiveToolWith(t, "fast", "post", fastWriteContractSQL)
	s.agent = s.f.ActiveAgent(t, "reconciled", s.read.Tool, s.write.Tool, s.idem.Tool, none.Tool)
	return s, none
}

func TestEnteringUnknownOutcomeSchedulesReconciliationAfterTheCallSettles(t *testing.T) {
	s, _ := newReconcileSchema(t)
	id := s.executing(t, "bank.pay")
	must(t, s.finish("w1", 1, id, "ambiguous", "", "", "UNKNOWN_OUTCOME", nil))
	var settled, attempts, since bool
	// The next reconciliation waits for the call deadline plus one call
	// budget, so a request still in flight cannot race a negative lookup.
	s.row(t, id, `next_reconcile_at >= (SELECT call_deadline FROM eacp.action_attempts WHERE action_id = $1)
		+ interval '5 seconds', reconcile_attempts = 0, outcome_unknown_at IS NOT NULL`, &settled, &attempts, &since)
	if !settled || !attempts || !since {
		t.Fatalf("settled=%v attempts=%v since=%v", settled, attempts, since)
	}
	wantCode(t, s.execReconciler("r1", 2, reconClaimSQL, id, "r1", 30), "55000") // not due yet
}

func TestReconcilerClaimTakesTheNextGenerationAndNeedsALookup(t *testing.T) {
	s, _ := newReconcileSchema(t)
	id := s.unknown(t, "bank.pay")
	wantCode(t, s.f.ExecSystem("sweeper", reconClaimSQL, id, "r1", 30), "42501")
	wantCode(t, s.f.ExecWorker("r1", 2, reconClaimSQL, id, "r1", 30), "42501")
	wantCode(t, s.f.ExecAgent(s.agent.Version, reconClaimSQL, id, "r1", 30), "42501")
	wantCode(t, s.f.Exec("otto", reconClaimSQL, id, "r1", 30), "42501")
	wantCode(t, s.execReconciler("r1", 3, reconClaimSQL, id, "r1", 30), "42501")  // not the next generation
	wantCode(t, s.execReconciler("r1", 2, reconClaimSQL, id, "r2", 30), "42501")  // not the claiming reconciler
	wantCode(t, s.execReconciler("r1", 2, reconClaimSQL, id, "r1", 900), "23514") // at most 10 minutes
	must(t, s.execReconciler("r1", 2, reconClaimSQL, id, "r1", 30))
	var worker string
	var gen int64
	var cleared bool
	s.row(t, id, "worker_id, lease_generation, next_reconcile_at IS NULL", &worker, &gen, &cleared)
	if s.state(t, id) != "RECONCILING" || worker != "r1" || gen != 2 || !cleared {
		t.Fatalf("claim = %s %s %d %v", s.state(t, id), worker, gen, cleared)
	}
	// A reconciling action belongs to its reconciler: the worker that held
	// generation 1 cannot heartbeat or record anything but late evidence.
	wantCode(t, s.f.ExecWorker("w1", 1, heartbeatSQL, id, 30), "55000")

	// No lookup, no proof or a READ_ONLY contract is never reconciled by lookup.
	for _, tool := range []string{"fast.post", "erp.lookup"} {
		other := s.unknown(t, tool)
		wantCode(t, s.execReconciler("r1", 2, reconClaimSQL, other, "r1", 30), "55000")
	}
}

func TestPositiveEvidenceSucceedsUnlessALateResultContradictsIt(t *testing.T) {
	s, _ := newReconcileSchema(t)
	id := s.unknown(t, "bank.pay")
	must(t, s.execReconciler("r1", 2, reconClaimSQL, id, "r1", 30))
	wantCode(t, s.reconcile("r1", 2, id, "", "", moveSQL, "SUCCEEDED", "found"), "55000")       // no check
	wantCode(t, s.reconcile("r1", 2, id, "absent", "", moveSQL, "SUCCEEDED", "found"), "55000") // wrong result
	wantCode(t, s.reconcile("r1", 2, id, "found", "", moveSQL, "SUCCEEDED", "found"), "23514")  // found needs a reference
	wantCode(t, s.reconcile("r2", 2, id, "found", "PO-1", moveSQL, "SUCCEEDED", "found"), "42501")
	must(t, s.reconcile("r1", 2, id, "found", "PO-1", moveSQL, "SUCCEEDED", "found"))
	var ref string
	s.row(t, id, "external_reference", &ref)
	if s.state(t, id) != "SUCCEEDED" || ref != "PO-1" {
		t.Fatalf("state %s ref %q", s.state(t, id), ref)
	}

	// A worker's late success is evidence too: a different reference is a
	// conflict, never a success.
	late := s.executing(t, "bank.pay")
	must(t, s.f.ExecWorker("w1", 1, heartbeatSQL, late, 1))
	time.Sleep(1100 * time.Millisecond)
	must(t, s.f.ExecSystem("sweeper", moveSQL, late, "UNKNOWN_OUTCOME", "lease expired during the call"))
	must(t, s.finish("w1", 1, late, "succeeded", "PO-A", "", "", nil))
	s.settle(t, late)
	must(t, s.execReconciler("r1", 2, reconClaimSQL, late, "r1", 30))
	wantCode(t, s.reconcile("r1", 2, late, "found", "PO-B", moveSQL, "SUCCEEDED", "found"), "55000")
	must(t, s.reconcile("r1", 2, late, "found", "PO-A", moveSQL, "SUCCEEDED", "found"))
}

func TestNegativeEvidenceNeedsAnAuthoritativeContract(t *testing.T) {
	s, _ := newReconcileSchema(t)
	// BEST_EFFORT: "not found" is still unknown (invariant 13).
	be := s.unknown(t, "ledger.post")
	must(t, s.execReconciler("r1", 2, reconClaimSQL, be, "r1", 30))
	wantCode(t, s.reconcile("r1", 2, be, "absent", "", retryAfterSQL), "55000")
	wantCode(t, s.reconcile("r1", 2, be, "absent", "", moveSQL, "FAILED", "absent"), "55000")
	must(t, s.reconcile("r1", 2, be, "absent", "", stillSQL, 1, 60))

	// AUTHORITATIVE: absence permits a retry with the same operation key,
	// but only once every call deadline has settled.
	auth := s.executing(t, "bank.pay")
	must(t, s.finish("w1", 1, auth, "ambiguous", "", "", "UNKNOWN_OUTCOME", nil))
	s.owner(t, `UPDATE eacp.actions SET next_reconcile_at = now() WHERE id = '`+auth.String()+`'`)
	must(t, s.execReconciler("r1", 2, reconClaimSQL, auth, "r1", 30))
	wantCode(t, s.reconcile("r1", 2, auth, "absent", "", retryAfterSQL), "55000") // deadline not settled
	s.settle(t, auth)
	wantCode(t, s.reconcile("r1", 2, auth, "unknown", "", retryAfterSQL), "55000")              // needs absence
	wantCode(t, s.reconcile("r1", 2, auth, "absent", "", moveSQL, "FAILED", "absent"), "55000") // retry is allowed
	must(t, s.reconcile("r1", 2, auth, "absent", "", retryAfterSQL))
	var attempts int
	s.row(t, auth, "attempt_count", &attempts)
	if s.state(t, auth) != "RETRY_WAIT" || attempts != 1 {
		t.Fatalf("authoritative absence: %s after %d attempts", s.state(t, auth), attempts)
	}

	// A cancel request makes authoritative absence final: FAILED, not retry.
	cancelled := s.executing(t, "bank.pay")
	must(t, s.f.Exec("carol", `UPDATE eacp.actions SET cancel_requested_at = now(), cancel_reason = 'no longer needed'
		WHERE id = $1`, cancelled))
	must(t, s.finish("w1", 1, cancelled, "ambiguous", "", "", "UNKNOWN_OUTCOME", nil))
	s.settle(t, cancelled)
	must(t, s.execReconciler("r1", 2, reconClaimSQL, cancelled, "r1", 30))
	wantCode(t, s.reconcile("r1", 2, cancelled, "absent", "", retryAfterSQL), "55000")
	must(t, s.reconcile("r1", 2, cancelled, "absent", "", moveSQL, "FAILED", "not executed"))

	// A late success contradicts absence: neither a retry nor FAILED.
	late := s.executing(t, "bank.pay")
	must(t, s.f.ExecWorker("w1", 1, heartbeatSQL, late, 1))
	time.Sleep(1100 * time.Millisecond)
	must(t, s.f.ExecSystem("sweeper", moveSQL, late, "UNKNOWN_OUTCOME", "lease expired during the call"))
	must(t, s.finish("w1", 1, late, "succeeded", "PO-L", "", "", nil))
	s.settle(t, late)
	must(t, s.execReconciler("r1", 2, reconClaimSQL, late, "r1", 30))
	wantCode(t, s.reconcile("r1", 2, late, "absent", "", retryAfterSQL), "55000")
	must(t, s.reconcile("r1", 2, late, "absent", "", moveSQL, "NEEDS_HUMAN_RESOLUTION", "conflict"))
}

func TestStillUnknownBacksOffAndStaleReconcilersAreFenced(t *testing.T) {
	s, _ := newReconcileSchema(t)
	id := s.unknown(t, "ledger.post")
	must(t, s.execReconciler("r1", 2, reconClaimSQL, id, "r1", 30))
	wantCode(t, s.reconcile("r1", 2, id, "unknown", "", stillSQL, 0, 60), "55000")   // counts the attempt
	wantCode(t, s.reconcile("r1", 2, id, "unknown", "", stillSQL, 1, 7200), "23514") // within the hour
	wantCode(t, s.f.ExecSystem("sweeper", stillSQL, id, 1, 60), "55000")             // the lease is live
	must(t, s.reconcile("r1", 2, id, "unknown", "", stillSQL, 1, 60))
	var n int
	s.row(t, id, "reconcile_attempts", &n)
	if s.state(t, id) != "UNKNOWN_OUTCOME" || n != 1 {
		t.Fatalf("still unknown = %s %d", s.state(t, id), n)
	}

	// The next claim fences the previous reconciler out.
	s.settle(t, id)
	must(t, s.execReconciler("r2", 3, reconClaimSQL, id, "r2", 1))
	wantCode(t, s.reconcile("r1", 2, id, "found", "PO-1", moveSQL, "SUCCEEDED", "found"), "42501")
	wantCode(t, s.reconcile("r2", 2, id, "found", "PO-1", moveSQL, "SUCCEEDED", "found"), "42501")
	// A lapsed reconciler lease returns the action to UNKNOWN_OUTCOME (T33).
	time.Sleep(1100 * time.Millisecond)
	must(t, s.f.ExecSystem("sweeper", stillSQL, id, 1, 0))
	s.row(t, id, "reconcile_attempts", &n)
	if s.state(t, id) != "UNKNOWN_OUTCOME" || n != 2 {
		t.Fatalf("lapsed reconciler lease = %s %d", s.state(t, id), n)
	}
	wantCode(t, s.reconcile("r2", 3, id, "found", "PO-1", moveSQL, "SUCCEEDED", "found"), "42501")
}

func TestUnknownOutcomesWithoutProofGoToAHumanAndReadsRetry(t *testing.T) {
	s, _ := newReconcileSchema(t)
	none := s.unknown(t, "fast.post") // proof NONE: T29
	wantCode(t, s.f.ExecAgent(s.agent.Version, moveSQL, none, "NEEDS_HUMAN_RESOLUTION", "x"), "42501")
	wantCode(t, s.f.ExecWorker("w1", 1, moveSQL, none, "NEEDS_HUMAN_RESOLUTION", "x"), "42501")
	wantCode(t, s.f.ExecSystem("sweeper", moveSQL, none, "NEEDS_HUMAN_RESOLUTION", ""), "23514")
	wantCode(t, s.f.ExecSystem("sweeper", moveSQL, none, "RETRY_WAIT", "x"), "55000") // not a read
	must(t, s.f.ExecSystem("sweeper", moveSQL, none, "NEEDS_HUMAN_RESOLUTION", "no proof standard"))

	// Nobody is asked to resolve an outcome before every call has settled:
	// neither T29 nor T34 from UNKNOWN_OUTCOME.
	for _, tool := range []string{"fast.post", "bank.pay"} {
		early := s.executing(t, tool)
		must(t, s.finish("w1", 1, early, "ambiguous", "", "", "UNKNOWN_OUTCOME", nil))
		wantCode(t, s.f.ExecSystem("sweeper", moveSQL, early, "NEEDS_HUMAN_RESOLUTION", "too soon"), "55000")
		s.settle(t, early)
		must(t, s.f.ExecSystem("sweeper", moveSQL, early, "NEEDS_HUMAN_RESOLUTION", "settled"))
	}

	// READ_ONLY: T29a retries, and never waits for a human.
	read := s.unknown(t, "erp.lookup")
	wantCode(t, s.f.ExecSystem("sweeper", moveSQL, read, "NEEDS_HUMAN_RESOLUTION", "x"), "55000")
	must(t, s.f.ExecSystem("sweeper", moveSQL, read, "RETRY_WAIT", "ambiguous read"))
	var due bool
	s.row(t, read, "next_attempt_at <= now()", &due)
	if !due {
		t.Fatal("T29a schedules the retry now")
	}

	// A revoked contract is no longer trusted as proof: T29.
	rev := s.unknown(t, "ledger.post")
	must(t, s.f.Exec("otto", `UPDATE eacp.tool_contracts SET revoked_at = now(), revoke_reason = 'unsafe'
		WHERE id = $1`, s.write.Contract))
	wantCode(t, s.execReconciler("r1", 2, reconClaimSQL, rev, "r1", 30), "55000")
	must(t, s.f.ExecSystem("sweeper", moveSQL, rev, "NEEDS_HUMAN_RESOLUTION", "contract revoked"))

	// Exhaustion (T34), from UNKNOWN_OUTCOME by the sweeper and from
	// RECONCILING by its reconciler with a recorded check.
	idem := s.unknown(t, "bank.pay")
	must(t, s.f.ExecSystem("sweeper", moveSQL, idem, "NEEDS_HUMAN_RESOLUTION", "reconciliation time exhausted"))
	conflict := s.unknown(t, "bank.pay")
	must(t, s.execReconciler("r1", 2, reconClaimSQL, conflict, "r1", 30))
	wantCode(t, s.reconcile("r1", 2, conflict, "", "", moveSQL, "NEEDS_HUMAN_RESOLUTION", "conflict"), "55000")
	must(t, s.reconcile("r1", 2, conflict, "conflict", "", moveSQL, "NEEDS_HUMAN_RESOLUTION", "conflict"))

	// NEEDS_HUMAN_RESOLUTION has no automated exit.
	for _, to := range []string{"SUCCEEDED", "FAILED", "RETRY_WAIT"} {
		wantCode(t, s.f.ExecSystem("sweeper", moveSQL, none, to, "x"), "42501")
		wantCode(t, s.f.ExecAgent(s.agent.Version, moveSQL, none, to, "x"), "42501")
		wantCode(t, s.f.ExecWorker("w1", 1, moveSQL, none, to, "x"), "42501")
		wantCode(t, s.execReconciler("r1", 1, moveSQL, none, to, "x"), "42501")
	}
	wantCode(t, s.f.ExecSystem("sweeper", moveSQL, none, "UNKNOWN_OUTCOME", "x"), "55000") // no such edge
	// Nor does an operator move it without a resolution record.
	wantCode(t, s.f.Exec("otto", moveSQL, none, "FAILED", "x"), "55000")
}

func TestHumanResolutionIsSeparatedJournaledAndTwoPersonForRetry(t *testing.T) {
	s, _ := newReconcileSchema(t)
	ok := s.needsHuman(t, "fast.post")
	for _, who := range []string{"carol", "erin", "amy"} { // subject and agent owner, non-operators
		_, err := s.f.TryID(who, resolveSQL, ok, "succeeded", "confirmed in ERP", "ERP screen", "PO-7")
		wantCode(t, err, "42501")
	}
	_, err := s.f.TryID("otto", resolveSQL, ok, "succeeded", "confirmed in ERP", "ERP screen", "")
	wantCode(t, err, "23514") // a success needs its external reference
	_, err = s.f.TryID("otto", resolveSQL, ok, "succeeded", "confirmed in ERP", "", "PO-7")
	wantCode(t, err, "23514") // and evidence
	_, err = s.f.TryID("otto", resolveSQL, ok, "succeeded", "", "ERP screen", "PO-7")
	wantCode(t, err, "23514") // and a reason
	s.f.ID(t, "otto", resolveSQL, ok, "succeeded", "confirmed in ERP", "ERP screen", "PO-7")
	var ref, actor string
	s.row(t, ok, "external_reference, state_actor_kind", &ref, &actor)
	if s.state(t, ok) != "SUCCEEDED" || ref != "PO-7" || actor != "principal" {
		t.Fatalf("resolved = %s %q %s", s.state(t, ok), ref, actor)
	}
	if s.journal(t, ok, "action.resolution") != 1 {
		t.Fatal("resolution not journaled")
	}
	_, err = s.f.TryID("opal", resolveSQL, ok, "failed", "again", "x", "")
	wantCode(t, err, "55000") // only while it needs a human

	failed := s.needsHuman(t, "fast.post")
	_, err = s.f.TryID("otto", resolveSQL, failed, "failed", "not in ERP", "", "")
	wantCode(t, err, "23514")
	s.f.ID(t, "otto", resolveSQL, failed, "failed", "not in ERP", "ERP audit export shows no PO", "")
	if s.state(t, failed) != "FAILED" {
		t.Fatalf("failed resolution = %s", s.state(t, failed))
	}

	// A retry needs attempts left: fast.post allows one attempt.
	_, err = s.f.TryID("otto", resolveSQL, s.needsHuman(t, "fast.post"), "retry", "try again", "", "")
	wantCode(t, err, "55000")

	// A retry is proposed by one operator and confirmed by another.
	retry := s.needsHuman(t, "bank.pay")
	proposal := s.f.ID(t, "otto", resolveSQL, retry, "retry", "ERP confirmed nothing arrived", "", "")
	if s.state(t, retry) != "NEEDS_HUMAN_RESOLUTION" {
		t.Fatal("a proposal alone must not move the action")
	}
	_, err = s.f.TryID("opal", resolveSQL, retry, "retry", "second proposal", "", "")
	wantCode(t, err, "23505") // one live proposal
	wantCode(t, s.f.Exec("otto", confirmSQL, proposal, "self"), "42501")
	wantCode(t, s.f.Exec("carol", confirmSQL, proposal, "subject"), "42501")
	wantCode(t, s.f.Exec("opal", confirmSQL, proposal, ""), "23514")
	must(t, s.f.Exec("opal", confirmSQL, proposal, "agreed"))
	var due bool
	var key string
	s.row(t, retry, "next_attempt_at <= now(), operation_key", &due, &key)
	if s.state(t, retry) != "RETRY_WAIT" || !due || key == "" {
		t.Fatalf("confirmed retry = %s %v", s.state(t, retry), due)
	}
	wantCode(t, s.f.Exec("opal", confirmSQL, proposal, "again"), "55000")

	// A withdrawn proposal frees the slot; a resolution voids a live one.
	other := s.needsHuman(t, "bank.pay")
	p1 := s.f.ID(t, "otto", resolveSQL, other, "retry", "maybe", "", "")
	wantCode(t, s.f.Exec("opal", withdrawSQL, p1, ""), "23514")
	must(t, s.f.Exec("opal", withdrawSQL, p1, "found the PO after all"))
	p2 := s.f.ID(t, "otto", resolveSQL, other, "retry", "maybe", "", "")
	s.f.ID(t, "opal", resolveSQL, other, "succeeded", "found it", "ERP search", "PO-9")
	if n := s.count(t, `SELECT count(*) FROM eacp.action_resolutions WHERE id = $1 AND state = 'VOIDED'`, p2); n != 1 {
		t.Fatal("the live proposal was not voided")
	}
	wantCode(t, s.f.Exec("opal", confirmSQL, p2, "late"), "55000")
}
