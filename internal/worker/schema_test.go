package worker_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"eacp/internal/registry/registrytest"
	"eacp/internal/storage"
	"eacp/internal/storage/pgtest"
)

// Raw SQL for the worker moves of migration 00006, run as eacp_app.
const (
	claimSQL = `UPDATE eacp.actions SET state = 'LEASED', lease_generation = lease_generation + 1,
		worker_id = $2, leased_until = now() + make_interval(secs => $3) WHERE id = $1`
	intentSQL = `UPDATE eacp.actions SET state = 'EXECUTING', attempt_count = attempt_count + 1,
		leased_until = now() + make_interval(secs => $2) WHERE id = $1`
	heartbeatSQL = `UPDATE eacp.actions SET leased_until = now() + make_interval(secs => $2) WHERE id = $1`
	completeSQL  = `UPDATE eacp.action_attempts SET outcome = $3, external_reference = NULLIF($4, ''),
		error_class = NULLIF($5, '') WHERE action_id = $1 AND lease_generation = $2`
	finishSQL = `UPDATE eacp.actions SET state = $2, state_reason = $3, next_attempt_at = $4 WHERE id = $1`
)

type schema struct {
	f                 *registrytest.Fixture
	agent             registrytest.Agent
	read, write, idem registrytest.Tooling
	policy            uuid.UUID
}

func newSchema(t *testing.T) schema {
	t.Helper()
	f := registrytest.New(t)
	s := schema{f: f}
	s.read = f.ActiveTool(t, "erp", "lookup")
	s.write = f.ActiveToolWith(t, "ledger", "post", registrytest.WriteContractSQL)
	s.idem = f.ActiveToolWith(t, "bank", "pay", registrytest.IdempotentContractSQL)
	s.agent = f.ActiveAgent(t, "buyer", s.read.Tool, s.write.Tool, s.idem.Tool)
	s.policy = f.ActivatePolicy(t, registrytest.AllowPolicy)
	return s
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("error = %v, want SQLSTATE %s", err, code)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// inWorker runs fn in tenant A as worker at generation gen.
func (s schema) inWorker(worker string, gen int64, fn func(context.Context, pgx.Tx) error) error {
	ctx := context.Background()
	return storage.InTenantTx(ctx, s.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		if err := storage.SetWorker(ctx, tx, worker, gen); err != nil {
			return err
		}
		return fn(ctx, tx)
	})
}

func (s schema) row(t *testing.T, action uuid.UUID, cols string, dest ...any) {
	t.Helper()
	ctx := context.Background()
	must(t, storage.InTenantTx(ctx, s.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT `+cols+` FROM eacp.actions WHERE id = $1`, action).Scan(dest...)
	}))
}

func (s schema) state(t *testing.T, action uuid.UUID) string {
	t.Helper()
	var st string
	s.row(t, action, "state", &st)
	return st
}

func (s schema) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	ctx := context.Background()
	var n int
	must(t, storage.InTenantTx(ctx, s.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&n)
	}))
	return n
}

// queued returns a QUEUED action on tool ("erp.lookup", "ledger.post", "bank.pay").
func (s schema) queued(t *testing.T, tool string) uuid.UUID {
	t.Helper()
	return s.f.QueuedAction(t, s.agent.Version, "carol", tool)
}

// leased claims a queued action as w1 at generation 1 for leaseSecs.
func (s schema) leased(t *testing.T, tool string, leaseSecs float64) uuid.UUID {
	t.Helper()
	id := s.queued(t, tool)
	must(t, s.f.ExecWorker("w1", 1, claimSQL, id, "w1", leaseSecs))
	return id
}

// executing records the dispatch intent of a w1 generation 1 lease.
func (s schema) executing(t *testing.T, tool string) uuid.UUID {
	t.Helper()
	id := s.leased(t, tool, 60)
	must(t, s.f.ExecWorker("w1", 1, intentSQL, id, 60))
	return id
}

// expire ends an action's lifetime now, as the schema owner with the guard
// disabled: the guard never lets not_after move.
func (s schema) expire(t *testing.T, id uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	must(t, storage.InTenantTx(ctx, s.f.Owner, pgtest.TenantA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `ALTER TABLE eacp.actions DISABLE TRIGGER actions_guard;
			UPDATE eacp.actions SET not_after = now() - interval '1 second' WHERE id = '`+id.String()+`';
			ALTER TABLE eacp.actions ENABLE TRIGGER actions_guard`)
		return err
	}))
}

// finish completes attempt gen and moves the action in one transaction.
func (s schema) finish(worker string, gen int64, id uuid.UUID, outcome, ref, class, to string, next *time.Duration) error {
	return s.inWorker(worker, gen, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM eacp.actions WHERE id = $1 FOR UPDATE`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, completeSQL, id, gen, outcome, ref, class); err != nil {
			return err
		}
		if to == "" {
			return nil
		}
		var at any
		if next != nil {
			at = time.Now().Add(*next)
		}
		_, err := tx.Exec(ctx, finishSQL, id, to, "result", at)
		return err
	})
}

func TestClaimTakesTheNextGenerationForTheClaimingWorker(t *testing.T) {
	s := newSchema(t)
	id := s.queued(t, "erp.lookup")
	wantCode(t, s.f.ExecAgent(s.agent.Version, claimSQL, id, "w1", 30), "42501")
	wantCode(t, s.f.ExecSystem("sweeper", claimSQL, id, "w1", 30), "42501")
	wantCode(t, s.f.ExecWorker("w1", 2, claimSQL, id, "w1", 30), "42501") // not the next generation
	wantCode(t, s.f.ExecWorker("w1", 1, claimSQL, id, "w2", 30), "42501") // for another worker
	wantCode(t, s.f.ExecWorker("w1", 1, claimSQL, id, "w1", 700), "23514")
	// Self-consistent but skipped or reused generations are refused too.
	claimAt := `UPDATE eacp.actions SET state = 'LEASED', lease_generation = $2, worker_id = 'w1',
		leased_until = now() + interval '30 seconds' WHERE id = $1`
	wantCode(t, s.f.ExecWorker("w1", 2, claimAt, id, 2), "42501")
	must(t, s.f.ExecWorker("w1", 1, claimSQL, id, "w1", 30))
	must(t, s.f.ExecWorker("w1", 1, `UPDATE eacp.actions SET state = 'QUEUED', state_reason = 'released'
		WHERE id = $1`, id))
	wantCode(t, s.f.ExecWorker("w1", 1, claimAt, id, 1), "42501") // generation 1 again
	must(t, s.f.ExecWorker("w1", 2, claimSQL, id, "w1", 30))
	var gen int64
	var worker string
	var leased, heartbeat bool
	s.row(t, id, "lease_generation, worker_id, leased_until > now(), heartbeat_at IS NOT NULL", &gen, &worker, &leased, &heartbeat)
	if gen != 2 || worker != "w1" || !leased || !heartbeat {
		t.Fatalf("lease = %d %s %v %v", gen, worker, leased, heartbeat)
	}
	// A second claim of the same generation finds no QUEUED action.
	wantCode(t, s.f.ExecWorker("w2", 2, claimSQL, id, "w2", 30), "55000")
	// Worker moves are never governance moves.
	other := s.f.ReceivedAction(t, s.agent.Version, "carol", "erp.lookup")
	wantCode(t, s.f.ExecWorker("w1", 1, `UPDATE eacp.actions SET state = 'DENIED', state_reason = 'x'
		WHERE id = $1`, other), "42501")
}

func TestHeartbeatExtendsOnlyTheHoldersLiveLease(t *testing.T) {
	s := newSchema(t)
	id := s.leased(t, "erp.lookup", 1)
	must(t, s.f.ExecWorker("w1", 1, heartbeatSQL, id, 1))
	wantCode(t, s.f.ExecWorker("w2", 1, heartbeatSQL, id, 30), "42501")
	wantCode(t, s.f.ExecWorker("w1", 2, heartbeatSQL, id, 30), "42501")
	wantCode(t, s.f.ExecWorker("w1", 1, `UPDATE eacp.actions SET leased_until = now() + interval '5 seconds',
		attempt_count = 1 WHERE id = $1`, id), "55000")
	if n := s.count(t, `SELECT count(*) FROM eacp.audit_events WHERE convert_from(payload, 'UTF8')::jsonb
		->'subject'->>'id' = $1::text`, id); n != 4 {
		// received, authorized, queued, leased: heartbeats are not journaled.
		t.Fatalf("journal entries = %d, want 4", n)
	}
	time.Sleep(1100 * time.Millisecond)
	wantCode(t, s.f.ExecWorker("w1", 1, heartbeatSQL, id, 30), "55000") // expired: the sweeper's now
}

func TestDispatchIntentCreatesTheAttemptAndMustOutliveTheCall(t *testing.T) {
	s := newSchema(t)
	id := s.leased(t, "erp.lookup", 60)
	// The fixture contract has no timeout: 30s default plus a 1s margin.
	wantCode(t, s.f.ExecWorker("w1", 1, intentSQL, id, 20), "23514")
	wantCode(t, s.f.ExecWorker("w2", 1, intentSQL, id, 60), "42501")
	wantCode(t, s.f.ExecWorker("w1", 1, `UPDATE eacp.actions SET state = 'EXECUTING', attempt_count = 2,
		leased_until = now() + interval '60 seconds' WHERE id = $1`, id), "55000")
	must(t, s.f.ExecWorker("w1", 1, intentSQL, id, 60))

	// A lapsed lease that the sweeper has not reclaimed yet is not dispatched.
	lapsed := s.leased(t, "erp.lookup", 1)
	time.Sleep(1100 * time.Millisecond)
	wantCode(t, s.f.ExecWorker("w1", 1, intentSQL, lapsed, 60), "55000")
	// Nor is an action whose lifetime ended after its claim (only the schema
	// owner can shorten one, bypassing the guard).
	expired := s.leased(t, "erp.lookup", 60)
	s.expire(t, expired)
	wantCode(t, s.f.ExecWorker("w1", 1, intentSQL, expired, 60), "55000")
	queued := s.queued(t, "erp.lookup")
	s.expire(t, queued)
	wantCode(t, s.f.ExecWorker("w1", 1, claimSQL, queued, "w1", 30), "55000")

	var n, attempt int
	var gen int64
	var worker, key string
	var deadline bool
	ctx := context.Background()
	must(t, storage.InTenantTx(ctx, s.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) OVER (), attempt_no, lease_generation, worker_id, operation_key,
			call_deadline BETWEEN dispatched_at + interval '29 seconds' AND dispatched_at + interval '31 seconds'
			FROM eacp.action_attempts WHERE action_id = $1`, id).Scan(&n, &attempt, &gen, &worker, &key, &deadline)
	}))
	if n != 1 || attempt != 1 || gen != 1 || worker != "w1" || !strings.HasSuffix(key, id.String()) || !deadline {
		t.Fatalf("attempt = %d %d %d %s %s %v", n, attempt, gen, worker, key, deadline)
	}
	// Attempts are created only by their dispatch intent, and are immutable.
	wantCode(t, s.f.ExecWorker("w1", 1, `INSERT INTO eacp.action_attempts (tenant_id, action_id, attempt_no, lease_generation)
		VALUES (eacp.current_tenant_id(), $1, 2, 1)`, id), "55000")
	wantCode(t, s.f.ExecWorker("w1", 1, `UPDATE eacp.action_attempts SET dispatched_at = now() WHERE action_id = $1`, id), "42501")
	wantCode(t, s.f.ExecWorker("w1", 1, `DELETE FROM eacp.action_attempts WHERE action_id = $1`, id), "42501")
	var others int
	must(t, storage.InTenantTx(ctx, s.f.App, pgtest.TenantB, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM eacp.action_attempts`).Scan(&others)
	}))
	if others != 0 {
		t.Fatalf("tenant B sees %d attempts", others)
	}
}

func TestDispatchIntentRefusesDriftAndRevocation(t *testing.T) {
	s := newSchema(t)
	moved := s.leased(t, "erp.lookup", 60)
	revoked := s.leased(t, "ledger.post", 60)
	clean := s.leased(t, "bank.pay", 60)
	// No drift: T16a and T16b have nothing to act on (except a digest mismatch).
	wantCode(t, s.f.ExecWorker("w1", 1, `UPDATE eacp.actions SET state = 'AUTHORIZED', state_reason = 'x' WHERE id = $1`, clean), "55000")
	wantCode(t, s.f.ExecWorker("w1", 1, `UPDATE eacp.actions SET state = 'DENIED', state_reason = 'x' WHERE id = $1`, clean), "55000")
	must(t, s.f.ExecWorker("w1", 1, `UPDATE eacp.actions SET state = 'DENIED', state_reason = 'enforced_digest_mismatch'
		WHERE id = $1`, clean))

	must(t, s.f.Exec("otto", `UPDATE eacp.tool_contracts SET revoked_at = now(), revoke_reason = 'incident'
		WHERE id = $1`, s.write.Contract))
	s.f.ActivatePolicy(t, `{"format_version":1,"rules":[{"id":"all","verdict":"allow","reason":"v2"}]}`)
	wantCode(t, s.f.ExecWorker("w1", 1, intentSQL, moved, 60), "55000")
	wantCode(t, s.f.ExecWorker("w1", 1, intentSQL, revoked, 60), "55000")
	// Revocation wins over policy drift.
	wantCode(t, s.f.ExecWorker("w1", 1, `UPDATE eacp.actions SET state = 'AUTHORIZED', state_reason = 'x' WHERE id = $1`, revoked), "55000")
	must(t, s.f.ExecWorker("w1", 1, `UPDATE eacp.actions SET state = 'DENIED', state_reason = 'x' WHERE id = $1`, revoked))
	must(t, s.f.ExecWorker("w1", 1, `UPDATE eacp.actions SET state = 'AUTHORIZED', state_reason = 'policy changed'
		WHERE id = $1`, moved))
	var reason string
	var leased bool
	s.row(t, revoked, "state_reason, leased_until IS NOT NULL", &reason, &leased)
	if reason != "revoked_before_dispatch: contract_revoked" || leased {
		t.Fatalf("T16b = %q leased=%v", reason, leased)
	}
	if st := s.state(t, moved); st != "AUTHORIZED" {
		t.Fatalf("T16a state = %s", st)
	}
}

func TestResultCommitsAreFencedAndNeedTheAttemptOutcome(t *testing.T) {
	s := newSchema(t)
	id := s.executing(t, "ledger.post")
	wantCode(t, s.f.ExecWorker("w1", 1, finishSQL, id, "SUCCEEDED", "x", nil), "55000")
	wantCode(t, s.finish("w1", 1, id, "succeeded", "PO-1", "", "", nil), "55000") // result without transition
	wantCode(t, s.finish("w1", 1, id, "succeeded", "PO-1", "", "FAILED", nil), "55000")
	wantCode(t, s.finish("w2", 1, id, "succeeded", "PO-1", "", "SUCCEEDED", nil), "42501")
	wantCode(t, s.finish("w1", 1, id, "no_effect", "", "timeout", "FAILED", nil), "23514") // not certified
	must(t, s.finish("w1", 1, id, "succeeded", "PO-1", "", "SUCCEEDED", nil))
	var ref string
	s.row(t, id, "external_reference", &ref)
	if s.state(t, id) != "SUCCEEDED" || ref != "PO-1" {
		t.Fatalf("result = %s %s", s.state(t, id), ref)
	}
	wantCode(t, s.finish("w1", 1, id, "ambiguous", "", "", "", nil), "55000") // completes once

	amb := s.executing(t, "ledger.post")
	must(t, s.finish("w1", 1, amb, "ambiguous", "", "timeout", "UNKNOWN_OUTCOME", nil))
	noEffect := s.executing(t, "bank.pay")
	wantCode(t, s.finish("w1", 1, noEffect, "no_effect", "", "validation", "SUCCEEDED", nil), "55000")
	must(t, s.finish("w1", 1, noEffect, "no_effect", "", "validation", "FAILED", nil))
}

func TestRetriesFollowTheContract(t *testing.T) {
	s := newSchema(t)
	soon := 200 * time.Millisecond
	// A write with one attempt never retries.
	w := s.executing(t, "ledger.post")
	wantCode(t, s.finish("w1", 1, w, "ambiguous", "", "", "RETRY_WAIT", &soon), "55000")
	// A certified no-effect of an idempotent write retries (T20).
	idem := s.executing(t, "bank.pay")
	late := 2 * time.Hour
	wantCode(t, s.finish("w1", 1, idem, "no_effect", "", "refused", "RETRY_WAIT", &late), "23514")
	wantCode(t, s.finish("w1", 1, idem, "ambiguous", "", "", "RETRY_WAIT", &soon), "55000") // T22a is read-only
	must(t, s.finish("w1", 1, idem, "no_effect", "", "refused", "RETRY_WAIT", &soon))
	// An ambiguous read retries (T22a).
	read := s.executing(t, "erp.lookup")
	must(t, s.finish("w1", 1, read, "ambiguous", "", "", "RETRY_WAIT", &soon))
	for _, id := range []uuid.UUID{idem, read} {
		if st := s.state(t, id); st != "RETRY_WAIT" {
			t.Fatalf("state = %s", st)
		}
	}
	// T25 requeues when due, under the pinned policy, as the sweeper only.
	requeue := `UPDATE eacp.actions SET state = 'QUEUED', state_reason = 'retry' WHERE id = $1`
	wantCode(t, s.f.ExecSystem("sweeper", requeue, idem), "55000")
	time.Sleep(300 * time.Millisecond)
	wantCode(t, s.f.ExecWorker("w1", 1, requeue, idem), "42501")
	wantCode(t, s.f.ExecAgent(s.agent.Version, requeue, idem), "42501")
	wantCode(t, s.f.ExecAgent(s.agent.Version, `UPDATE eacp.actions SET state = 'FAILED', state_reason = 'x'
		WHERE id = $1`, idem), "42501")
	must(t, s.f.ExecSystem("sweeper", requeue, idem))
	if n := s.count(t, `SELECT count(*) FROM eacp.outbox_events WHERE aggregate_id = $1 AND topic = 'action.queued'`, idem); n != 2 {
		t.Fatalf("outbox rows = %d, want 2 (release and requeue)", n)
	}
	// The next claim is generation 2; generation 1 is stale everywhere.
	must(t, s.f.ExecWorker("w2", 2, claimSQL, idem, "w2", 60))
	wantCode(t, s.f.ExecWorker("w1", 1, intentSQL, idem, 60), "42501")
	must(t, s.f.ExecWorker("w2", 2, intentSQL, idem, 60))
	// A policy change sends a due retry back through release (T26), and a
	// cancelled retry fails (T27).
	s.f.ActivatePolicy(t, `{"format_version":1,"rules":[{"id":"all","verdict":"allow","reason":"v2"}]}`)
	wantCode(t, s.f.ExecSystem("sweeper", requeue, read), "55000")
	wantCode(t, s.f.ExecSystem("sweeper", `UPDATE eacp.actions SET state = 'FAILED', state_reason = 'x' WHERE id = $1`, read), "55000")
	must(t, s.f.ExecSystem("sweeper", `UPDATE eacp.actions SET state = 'AUTHORIZED', state_reason = 'policy changed' WHERE id = $1`, read))
}

func TestSweeperReclaimsExpiredLeases(t *testing.T) {
	s := newSchema(t)
	release := `UPDATE eacp.actions SET state = 'QUEUED', state_reason = 'lease expired' WHERE id = $1`
	unknown := `UPDATE eacp.actions SET state = 'UNKNOWN_OUTCOME', state_reason = 'lease expired' WHERE id = $1`
	retry := `UPDATE eacp.actions SET state = 'RETRY_WAIT', state_reason = 'lease expired' WHERE id = $1`

	leased := s.leased(t, "ledger.post", 1)
	wantCode(t, s.f.ExecSystem("sweeper", release, leased), "55000")
	// The holder may release voluntarily; an agent may not.
	wantCode(t, s.f.ExecAgent(s.agent.Version, release, leased), "42501")

	w := s.executing(t, "ledger.post")
	r := s.executing(t, "erp.lookup")
	// Shorten both leases through heartbeats, then let them lapse.
	must(t, s.f.ExecWorker("w1", 1, heartbeatSQL, w, 1))
	must(t, s.f.ExecWorker("w1", 1, heartbeatSQL, r, 1))
	wantCode(t, s.f.ExecSystem("sweeper", unknown, w), "55000")
	time.Sleep(1100 * time.Millisecond)
	must(t, s.f.ExecSystem("sweeper", release, leased))
	wantCode(t, s.f.ExecSystem("sweeper", retry, w), "55000") // T24 is read-only in Slice A
	must(t, s.f.ExecSystem("sweeper", unknown, w))
	must(t, s.f.ExecSystem("sweeper", retry, r))
	var gen int64
	s.row(t, leased, "lease_generation", &gen)
	if s.state(t, leased) != "QUEUED" || gen != 1 || s.state(t, w) != "UNKNOWN_OUTCOME" || s.state(t, r) != "RETRY_WAIT" {
		t.Fatalf("states = %s %s %s", s.state(t, leased), s.state(t, w), s.state(t, r))
	}
	// The worker that lost the lease records its result as late evidence,
	// and nobody else can write evidence into its attempt.
	wantCode(t, s.finish("w2", 1, w, "succeeded", "PO-X", "", "", nil), "42501")
	wantCode(t, s.f.ExecWorker("w1", 2, `UPDATE eacp.action_attempts SET outcome = 'succeeded',
		external_reference = 'PO-X' WHERE action_id = $1 AND lease_generation = 1`, w), "42501")
	must(t, s.finish("w1", 1, w, "succeeded", "PO-9", "", "", nil))
	var isLate bool
	ctx := context.Background()
	must(t, storage.InTenantTx(ctx, s.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT late FROM eacp.action_attempts WHERE action_id = $1`, w).Scan(&isLate)
	}))
	if !isLate || s.state(t, w) != "UNKNOWN_OUTCOME" {
		t.Fatalf("late = %v, state = %s", isLate, s.state(t, w))
	}
	if n := s.count(t, `SELECT count(*) FROM eacp.audit_events WHERE convert_from(payload, 'UTF8')::jsonb->>'action'
		= 'action.late_result' AND convert_from(payload, 'UTF8')::jsonb->'subject'->>'id' = $1::text`, w); n != 1 {
		t.Fatalf("late-result journal entries = %d", n)
	}
}

func TestCancelAfterReleaseFollowsTheDispatchIntent(t *testing.T) {
	s := newSchema(t)
	cancel := `UPDATE eacp.actions SET state = 'CANCELLED', state_reason = 'withdrawn' WHERE id = $1`
	request := `UPDATE eacp.actions SET cancel_requested_at = now(), cancel_reason = 'withdrawn' WHERE id = $1`

	leased := s.leased(t, "ledger.post", 60)
	wantCode(t, s.f.ExecWorker("w1", 1, cancel, leased), "42501")         // the system never cancels
	wantCode(t, s.f.ExecAgent(s.agent.Version, request, leased), "55000") // LEASED is cancelled outright
	must(t, s.f.ExecAgent(s.agent.Version, cancel, leased))
	wantCode(t, s.f.ExecWorker("w1", 1, intentSQL, leased, 60), "55000") // nothing left to dispatch

	exec := s.executing(t, "ledger.post")
	wantCode(t, s.f.ExecAgent(s.agent.Version, cancel, exec), "55000") // no cancel after dispatch intent
	wantCode(t, s.f.Exec("erin", request, exec), "42501")
	must(t, s.f.Exec("carol", request, exec)) // the subject
	var requested bool
	s.row(t, exec, "cancel_requested_at IS NOT NULL", &requested)
	if !requested || s.state(t, exec) != "EXECUTING" {
		t.Fatalf("cancel request = %v, state %s", requested, s.state(t, exec))
	}
	must(t, s.f.ExecWorker("w1", 1, heartbeatSQL, exec, 60))            // heartbeats continue
	wantCode(t, s.f.ExecAgent(s.agent.Version, request, exec), "55000") // requested once

	// A cancelled retry is not retried: T27 fails it.
	read := s.executing(t, "erp.lookup")
	soon := 30 * time.Minute
	must(t, s.finish("w1", 1, read, "ambiguous", "", "", "RETRY_WAIT", &soon))
	must(t, s.f.Exec("otto", request, read))
	must(t, s.f.ExecSystem("sweeper", `UPDATE eacp.actions SET state = 'FAILED', state_reason = 'cancelled' WHERE id = $1`, read))
	if n := s.count(t, `SELECT count(*) FROM eacp.audit_events WHERE convert_from(payload, 'UTF8')::jsonb->>'action'
		= 'action.cancel_requested'`); n != 2 {
		t.Fatalf("cancel requests journaled = %d", n)
	}
}

func TestClaimableActionsMatchProtocolCredentialAndOrder(t *testing.T) {
	s := newSchema(t)
	first := s.queued(t, "erp.lookup")
	second := s.queued(t, "erp.lookup")
	s.queued(t, "ledger.post")
	ctx := context.Background()
	claimable := func(protocols []string, bindings string) []uuid.UUID {
		var ids []uuid.UUID
		rows, err := s.f.App.Query(ctx, `SELECT action_id FROM eacp.claimable_actions($1, $2::jsonb, 10, NULL)`, protocols, bindings)
		must(t, err)
		ids, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		must(t, err)
		return ids
	}
	erp := `[{"tenant_id":"` + pgtest.TenantA + `","secret_ref":"erp","host":"fakeerp:8090"}]`
	if got := claimable([]string{"http"}, erp); len(got) != 2 || got[0] != first || got[1] != second {
		t.Fatalf("claimable = %v, want [%v %v]", got, first, second)
	}
	for name, c := range map[string]struct {
		protocols []string
		bindings  string
	}{
		"protocol":    {[]string{"grpc"}, erp},
		"host":        {[]string{"http"}, strings.Replace(erp, "fakeerp:8090", "evil:8090", 1)},
		"tenant":      {[]string{"http"}, strings.Replace(erp, pgtest.TenantA, pgtest.TenantB, 1)},
		"no bindings": {[]string{"http"}, `[]`},
	} {
		if got := claimable(c.protocols, c.bindings); len(got) != 0 {
			t.Errorf("%s mismatch still claimable: %v", name, got)
		}
	}
	// Admission counts released, unfinished actions (QUEUED to RETRY_WAIT).
	must(t, s.f.ExecWorker("w1", 1, claimSQL, first, "w1", 60))
	var n int64
	must(t, s.f.App.QueryRow(ctx, `SELECT eacp.global_queued_count()`).Scan(&n))
	if n != 3 {
		t.Fatalf("global count = %d, want 3", n)
	}
}
