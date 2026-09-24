package worker_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/registry/registrytest"
	"eacp/internal/storage"
	"eacp/internal/storage/pgtest"
	"eacp/internal/worker"
)

// Raw SQL for the shared connector circuit (migration 00013, ADR-022 §3).
const (
	tripSQL = `UPDATE eacp.connector_circuits
		SET open_until = now() + make_interval(secs => $2), reason = $3 WHERE connector_id = $1`
	switchSQL = `UPDATE eacp.connector_circuits SET disabled = $2, reason = $3 WHERE connector_id = $1`
)

func (s schema) circuitCandidates(t *testing.T) map[uuid.UUID]bool {
	t.Helper()
	store := worker.NewStore(s.f.App, "circuit-test")
	var bindings []worker.Binding
	for _, ref := range []string{"erp", "ledger", "bank"} {
		bindings = append(bindings, worker.Binding{TenantID: uuid.MustParse(pgtest.TenantA), Ref: ref, Host: "fakeerp:8090"})
	}
	got, err := store.Claimable(context.Background(), []string{"http"}, bindings, 100, nil)
	must(t, err)
	out := map[uuid.UUID]bool{}
	for _, c := range got {
		out[c.ActionID] = true
	}
	return out
}

func TestOpenCircuitRefusesClaimAndDispatch(t *testing.T) {
	s := newSchema(t)
	// Leased before the circuit opened: the dispatch intent is refused.
	leased := s.leased(t, "erp.lookup", 60)
	queued := s.queued(t, "erp.lookup")
	other := s.queued(t, "bank.pay")
	must(t, s.f.ExecWorker("w9", 1, tripSQL, s.read.Connector, 1, "5 consecutive failures"))

	wantCode(t, s.f.ExecWorker("w1", 1, intentSQL, leased, 60), "55000")
	wantCode(t, s.f.ExecWorker("w2", 1, claimSQL, queued, "w2", 60), "53300")
	if c := s.circuitCandidates(t); c[queued] || !c[other] {
		t.Fatalf("hint while open = %v: want the open connector hidden and the other one listed", c)
	}
	// Other connectors are unaffected (invariant 9).
	must(t, s.f.ExecWorker("w2", 1, claimSQL, other, "w2", 60))
	// The lease holder may still release its lease (T17).
	must(t, s.f.ExecWorker("w1", 1, `UPDATE eacp.actions SET state = 'QUEUED', state_reason = 'connector circuit open'
		WHERE id = $1`, leased))

	// Once open_until passes, claims and dispatch resume.
	time.Sleep(1100 * time.Millisecond)
	if c := s.circuitCandidates(t); !c[queued] {
		t.Fatal("hint still hides the connector after its circuit expired")
	}
	must(t, s.f.ExecWorker("w2", 1, claimSQL, queued, "w2", 60))
	must(t, s.f.ExecWorker("w2", 1, intentSQL, queued, 60))

	// An operator's disable holds until an operator enables the connector.
	must(t, s.f.Exec("otto", switchSQL, s.read.Connector, true, "maintenance window"))
	wantCode(t, s.f.ExecWorker("w2", 2, claimSQL, leased, "w2", 60), "53300")
	must(t, s.f.Exec("opal", switchSQL, s.read.Connector, false, "maintenance over"))
	must(t, s.f.ExecWorker("w2", 2, claimSQL, leased, "w2", 60))
}

func TestCircuitChangesAreGuardedAndJournaled(t *testing.T) {
	s := newSchema(t)
	conn := s.read.Connector
	// Only a worker opens a circuit, and only an operator disables or enables.
	wantCode(t, s.f.ExecAgent(s.agent.Version, tripSQL, conn, 30, "x"), "42501")
	wantCode(t, s.f.Exec("otto", tripSQL, conn, 30, "x"), "42501")
	wantCode(t, s.f.ExecSystem("sweeper", tripSQL, conn, 30, "x"), "42501")
	wantCode(t, s.f.Exec("carol", switchSQL, conn, true, "x"), "42501")
	wantCode(t, s.f.ExecWorker("w1", 1, switchSQL, conn, true, "x"), "42501")
	// A reason is required, and a worker opens for at most 10 minutes.
	wantCode(t, s.f.ExecWorker("w1", 1, tripSQL, conn, 30, " "), "23514")
	wantCode(t, s.f.Exec("otto", switchSQL, conn, true, ""), "23514")
	wantCode(t, s.f.ExecWorker("w1", 1, tripSQL, conn, 601, "x"), "23514")
	wantCode(t, s.f.ExecWorker("w1", 1, tripSQL, conn, 0, "x"), "23514")

	must(t, s.f.ExecWorker("w1", 1, tripSQL, conn, 300, "5 consecutive failures"))
	// A second worker never moves the circuit earlier.
	must(t, s.f.ExecWorker("w2", 1, tripSQL, conn, 5, "2 consecutive failures"))
	var remaining float64
	var by string
	ctx := context.Background()
	read := func() {
		t.Helper()
		must(t, storage.InTenantTx(ctx, s.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT COALESCE(extract(epoch FROM open_until - now()), 0)::float8,
				COALESCE(changed_by_worker, '') FROM eacp.connector_circuits WHERE connector_id = $1`, conn).Scan(&remaining, &by)
		}))
	}
	read()
	if remaining < 250 || by != "w2" {
		t.Fatalf("open for %.0fs by %q, want about 300s by w2", remaining, by)
	}
	// Stamped columns and deletion are the database's.
	wantCode(t, s.f.ExecWorker("w1", 1, `UPDATE eacp.connector_circuits SET changed_by_worker = 'forged', reason = 'x'
		WHERE connector_id = $1`, conn), "55000")
	wantCode(t, s.f.ExecWorker("w1", 1, `DELETE FROM eacp.connector_circuits WHERE connector_id = $1`, conn), "42501")
	// Rows are created with their connector, never by hand.
	wantCode(t, s.f.Exec("otto", `INSERT INTO eacp.connector_circuits (tenant_id, connector_id, reason)
		VALUES (eacp.current_tenant_id(), $1, 'x')`, uuid.New()), "42501")
	// A worker cannot clear an operator's disable, and enabling clears a trip.
	must(t, s.f.Exec("otto", switchSQL, conn, true, "vendor incident"))
	wantCode(t, s.f.ExecWorker("w1", 1, `UPDATE eacp.connector_circuits SET disabled = false, reason = 'x'
		WHERE connector_id = $1`, conn), "42501")
	must(t, s.f.Exec("opal", switchSQL, conn, false, "vendor recovered"))
	read()
	if remaining != 0 {
		t.Fatalf("enabling left the circuit open for %.0fs", remaining)
	}

	journal := s.count(t, `SELECT count(*) FROM eacp.audit_events
		WHERE convert_from(payload, 'UTF8')::jsonb #>> '{subject,id}' = $1::text
		  AND convert_from(payload, 'UTF8')::jsonb ->> 'action' LIKE 'connector.circuit_%'`, conn)
	if journal != 4 {
		t.Fatalf("circuit journal entries = %d, want 4 (two trips, disable, enable)", journal)
	}
	// Tenant isolation: tenant B sees nothing of tenant A's circuits.
	s.f.ForTenant(t, pgtest.TenantB)
	var n int
	must(t, storage.InTenantTx(ctx, s.f.App, pgtest.TenantB, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM eacp.connector_circuits`).Scan(&n)
	}))
	if n != 0 {
		t.Fatalf("tenant B sees %d of tenant A's circuits", n)
	}
}

// retryContractSQL is a natively idempotent write with ten attempts, a
// certified "refused" no-effect and a one-second retry time budget.
const retryContractSQL = `INSERT INTO eacp.tool_contracts
	(tenant_id, tool_id, side_effects, idempotency_mode, idempotency_key_field, reconciliation_lookup,
	 reconciliation_consistency, proof_standard, no_effect_errors, max_attempts, timeout_ms, retry_max_elapsed_ms)
	VALUES (eacp.current_tenant_id(), $1, '{REVERSIBLE_WRITE}', 'native', 'Idempotency-Key',
	 'by_operation_key', 'strong', 'authoritative', '{refused}', 10, 5000, 1000)
	RETURNING id`

func TestRetryBudgetBoundsElapsedTime(t *testing.T) {
	f := registrytest.New(t)
	tool := f.ActiveToolWith(t, "bank", "pay", retryContractSQL)
	agent := f.ActiveAgent(t, "buyer", tool.Tool)
	f.ActivatePolicy(t, registrytest.AllowPolicy)
	s := schema{f: f, agent: agent}
	soon := 100 * time.Millisecond

	id := s.executing(t, "bank.pay")
	must(t, s.finish("w1", 1, id, "no_effect", "", "refused", "RETRY_WAIT", &soon))
	time.Sleep(150 * time.Millisecond)
	requeue := `UPDATE eacp.actions SET state = 'QUEUED', state_reason = 'retry' WHERE id = $1`
	fail := `UPDATE eacp.actions SET state = 'FAILED', state_reason = 'retry budget exhausted: elapsed' WHERE id = $1`
	// Within the budget: T27 is refused and T25 re-queues.
	wantCode(t, s.f.ExecSystem("sweeper", fail, id), "55000")
	must(t, s.f.ExecSystem("sweeper", requeue, id))
	must(t, s.f.ExecWorker("w1", 2, claimSQL, id, "w1", 60))
	must(t, s.f.ExecWorker("w1", 2, intentSQL, id, 60))
	// Past the budget, measured from the first dispatch intent: no retry
	// (T20) is granted, and the definitive no-effect fails (T21).
	time.Sleep(1100 * time.Millisecond)
	wantCode(t, s.finish("w1", 2, id, "no_effect", "", "refused", "RETRY_WAIT", &soon), "55000")
	must(t, s.finish("w1", 2, id, "no_effect", "", "refused", "FAILED", nil))

	// A retry already waiting when the budget runs out is failed (T27),
	// never re-queued (T25).
	waiting := s.executing(t, "bank.pay")
	must(t, s.finish("w1", 1, waiting, "no_effect", "", "refused", "RETRY_WAIT", &soon))
	time.Sleep(1100 * time.Millisecond)
	wantCode(t, s.f.ExecSystem("sweeper", requeue, waiting), "55000")
	must(t, s.f.ExecSystem("sweeper", fail, waiting))

	// A contract cannot declare a retry cost without a budget unit.
	_, err := f.TryID("erin", `INSERT INTO eacp.tool_contracts
		(tenant_id, tool_id, side_effects, idempotency_mode, reconciliation_lookup,
		 reconciliation_consistency, proof_standard, max_attempts, retry_max_cost)
		VALUES (eacp.current_tenant_id(), $1, '{READ_ONLY}', 'none', 'none', 'none', 'none', 3, 10)
		RETURNING id`, tool.Tool)
	wantCode(t, err, "23514")
}

// T16 reads the circuit row FOR SHARE and holds it until it commits, so an
// operator's disable waits for a dispatch intent in flight: no dispatch
// intent commits after a disable does. A dispatch intent whose snapshot
// predates the disable fails to serialize instead of dispatching past it.
func TestDisableSerializesWithAStaleDispatchIntent(t *testing.T) {
	s := newSchema(t)
	ctx := context.Background()
	inFlight := s.leased(t, "erp.lookup", 60)
	a, err := s.f.App.Begin(ctx)
	must(t, err)
	defer a.Rollback(ctx)
	_, err = a.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, pgtest.TenantA)
	must(t, err)
	must(t, storage.SetWorker(ctx, a, "w1", 1))
	_, err = a.Exec(ctx, intentSQL, inFlight, 60)
	must(t, err)
	err = storage.InTenantTx(ctx, s.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT 1 FROM eacp.connector_circuits WHERE connector_id = $1 FOR UPDATE NOWAIT`,
			s.read.Connector)
		return err
	})
	wantCode(t, err, "55P03") // the uncommitted dispatch intent holds the circuit row
	must(t, a.Rollback(ctx))

	id := s.leased(t, "erp.lookup", 60)
	tx, err := s.f.App.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	must(t, err)
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, pgtest.TenantA)
	must(t, err)
	must(t, storage.SetWorker(ctx, tx, "w1", 1))
	var open bool
	must(t, tx.QueryRow(ctx, `SELECT disabled FROM eacp.connector_circuits WHERE connector_id = $1`,
		s.read.Connector).Scan(&open)) // the snapshot is taken here
	must(t, s.f.Exec("otto", switchSQL, s.read.Connector, true, "vendor incident"))
	_, err = tx.Exec(ctx, intentSQL, id, 60)
	if err == nil {
		err = tx.Commit(ctx)
	}
	wantCode(t, err, "40001")
	if st := s.state(t, id); st != "LEASED" {
		t.Fatalf("state = %s, want LEASED (no dispatch intent)", st)
	}
}
