package budget_test

// Schema-level tests: PostgreSQL enforces every budget rule (migration
// 00011, ADR-012), so these issue raw SQL as the application role.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"eacp/internal/registry/registrytest"
	"eacp/internal/storage"
	"eacp/internal/storage/pgtest"
)

const (
	sqlForbidden  = "42501"
	sqlBadState   = "55000"
	sqlCheck      = "23514"
	sqlForeignKey = "23503"
	sqlUnique     = "23505"

	accountSQL = `INSERT INTO eacp.budget_accounts (tenant_id, name, unit, parent_id, agent_id)
		VALUES (eacp.current_tenant_id(), $1, $2, $3, $4) RETURNING id`
	limitSQL = `INSERT INTO eacp.budget_limit_changes (tenant_id, account_id, new_limit, reason)
		VALUES (eacp.current_tenant_id(), $1, $2::numeric, 'needed') RETURNING id`
	decideSQL = `UPDATE eacp.budget_limit_changes SET state = $2, decision_reason = $3 WHERE id = $1`
)

func wantState(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("err = %v, want SQLSTATE %s", err, code)
	}
}

func ok(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// world is a tenant with a costed erp.purchase tool (the payload's amount,
// in THB), an agent allowed to call it and an allow-all policy.
type world struct {
	f     *registrytest.Fixture
	tool  registrytest.Tooling
	agent registrytest.Agent
}

func newWorld(t *testing.T) world {
	t.Helper()
	f := registrytest.New(t)
	tool := f.ActiveToolWith(t, "erp", "purchase", registrytest.CostedContractSQL)
	agent := f.ActiveAgent(t, "buyer", tool.Tool)
	f.ActivatePolicy(t, registrytest.AllowPolicy)
	return world{f: f, tool: tool, agent: agent}
}

// text reads one value as text in tenant A.
func (w world) text(t *testing.T, sql string, args ...any) string {
	t.Helper()
	var s *string
	err := storage.InTenantTx(context.Background(), w.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), sql, args...).Scan(&s)
	})
	if err != nil {
		t.Fatal(err)
	}
	if s == nil {
		return "<nil>"
	}
	return *s
}

// counters is an account's limit, allocated, reserved and committed.
func (w world) counters(t *testing.T, account uuid.UUID) string {
	t.Helper()
	return w.text(t, `SELECT concat_ws(' ', trim_scale(hard_limit), trim_scale(allocated), trim_scale(reserved),
		trim_scale(committed)) FROM eacp.budget_accounts WHERE id = $1`, account)
}

// authorized submits an action for the fixture payload (1000000 THB) and
// authorizes it (T3) as the agent.
func (w world) authorized(t *testing.T) uuid.UUID {
	t.Helper()
	id := w.f.ReceivedAction(t, w.agent.Version, "carol", "erp.purchase")
	ev := w.f.AgentID(t, w.agent.Version, registrytest.AllowEvidenceSQL, id)
	ok(t, w.f.ExecAgent(w.agent.Version, `UPDATE eacp.actions SET state = 'AUTHORIZED', state_reason = 'ok',
		decision_evidence_id = $2, enforced_payload = $3 WHERE id = $1`, id, ev, registrytest.ActionPayload))
	return id
}

// release is R1 in raw SQL as the agent, in the engine's lock order: the
// action, the budget step (unless skipBudget) before any journal write,
// then a revalidation and T10 when the budget allows it. It returns the
// budget status.
func (w world) release(t *testing.T, id uuid.UUID, skipBudget bool) (string, error) {
	return w.releaseAs(w.agent.Version, id, skipBudget)
}

func (w world) releaseAs(version, id uuid.UUID, skipBudget bool) (string, error) {
	ctx := context.Background()
	status := "skipped"
	err := storage.InTenantTx(ctx, w.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		if err := storage.SetAgent(ctx, tx, version); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM eacp.actions WHERE id = $1 FOR UPDATE`, id); err != nil {
			return err
		}
		if !skipBudget {
			if err := tx.QueryRow(ctx, `SELECT eacp.budget_reserve($1, t.active_contract_id)
				FROM eacp.actions a JOIN eacp.tools t ON t.tenant_id = a.tenant_id AND t.id = a.tool_id
				WHERE a.id = $1`, id).Scan(&status); err != nil {
				return err
			}
			if status != "reserved" && status != "unbudgeted" {
				return nil
			}
		}
		var rev uuid.UUID
		if err := tx.QueryRow(ctx, registrytest.ReleaseEvidenceSQL, id, "allow").Scan(&rev); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, registrytest.QueueSQL, id, rev)
		return err
	})
	return status, err
}

func TestContractsDeclareACostOrNone(t *testing.T) {
	f := registrytest.New(t)
	tool := f.ActiveTool(t, "erp", "purchase")
	insert := func(cols, vals string) error {
		_, err := f.TryID("erin", `INSERT INTO eacp.tool_contracts (tenant_id, tool_id, side_effects, idempotency_mode,
			reconciliation_lookup, reconciliation_consistency, proof_standard, max_attempts`+cols+`)
			VALUES (eacp.current_tenant_id(), $1, '{FINANCIAL}', 'none', 'none', 'none', 'none', 1`+vals+`) RETURNING id`,
			tool.Tool)
		return err
	}
	for name, c := range map[string][2]string{
		"a cost without a unit":     {", cost_fixed", ", 5"},
		"a unit without a cost":     {", cost_unit", ", 'THB'"},
		"a lower-case unit":         {", cost_unit, cost_fixed", ", 'thb', 5"},
		"a blank amount field":      {", cost_unit, cost_amount_field", ", 'THB', ''"},
		"a negative fixed cost":     {", cost_unit, cost_fixed", ", 'THB', -1"},
		"a unit field without unit": {", cost_unit_field", ", 'currency'"},
	} {
		if err := insert(c[0], c[1]); err == nil {
			t.Fatalf("%s was accepted", name)
		} else {
			wantState(t, err, sqlCheck)
		}
	}
	ok(t, insert(", cost_unit, cost_fixed", ", 'TOOL_CALLS', 1"))

	costed := f.ID(t, "erin", `INSERT INTO eacp.tool_contracts (tenant_id, tool_id, side_effects, idempotency_mode,
		reconciliation_lookup, reconciliation_consistency, proof_standard, max_attempts,
		cost_unit, cost_fixed, cost_amount_field, cost_unit_field)
		VALUES (eacp.current_tenant_id(), $1, '{FINANCIAL}', 'none', 'none', 'none', 'none', 1,
		'THB', 2.5, 'amount', 'currency') RETURNING id`, tool.Tool)
	w := world{f: f}
	for payload, want := range map[string]string{
		`{"amount":10,"currency":"THB"}`:        "12.5",
		`{"amount":0.25,"currency":"THB"}`:      "2.75",
		`{"amount":1e3,"currency":"THB"}`:       "1002.5",
		`{"amount":10,"currency":"USD"}`:        "<nil>",
		`{"amount":10}`:                         "<nil>",
		`{"amount":"10","currency":"THB"}`:      "<nil>",
		`{"amount":-1,"currency":"THB"}`:        "<nil>",
		`{"amount":1.0000001,"currency":"THB"}`: "<nil>",
		`{"amount":1e15,"currency":"THB"}`:      "<nil>",
		`{"currency":"THB"}`:                    "<nil>",
		`[1,2]`:                                 "<nil>",
	} {
		got := w.text(t, `SELECT trim_scale(eacp.action_cost(c, $2::jsonb))::text FROM eacp.tool_contracts c
			WHERE id = $1`, costed, payload)
		if got != want {
			t.Errorf("cost of %s = %s, want %s", payload, got, want)
		}
	}
}

func TestAccountsStartEmptyAndOnlyTheirTriggersMoveThem(t *testing.T) {
	w := newWorld(t)
	f := w.f
	_, err := f.TryID("erin", accountSQL, "ops", "THB", nil, nil)
	wantState(t, err, sqlForbidden)
	_, err = f.TryID("alice", `INSERT INTO eacp.budget_accounts (tenant_id, name, unit, hard_limit)
		VALUES (eacp.current_tenant_id(), 'rich', 'THB', 1000) RETURNING id`)
	wantState(t, err, sqlCheck)

	root := f.ID(t, "alice", accountSQL, "ops", "THB", nil, nil)
	leaf := f.ID(t, "alice", accountSQL, "buyer", "THB", root, w.agent.Agent)
	if by := w.text(t, `SELECT created_by::text FROM eacp.budget_accounts WHERE id = $1`, leaf); by != f.P["alice"].String() {
		t.Fatalf("created_by = %s", by)
	}
	_, err = f.TryID("alice", accountSQL, "under-agent", "THB", leaf, nil)
	wantState(t, err, sqlBadState)
	_, err = f.TryID("alice", accountSQL, "dollars", "USD", root, nil)
	wantState(t, err, sqlForeignKey)
	_, err = f.TryID("alice", accountSQL, "buyer-again", "THB", nil, w.agent.Agent)
	wantState(t, err, sqlUnique)
	ok(t, f.Exec("alice", accountSQL, "buyer-usd", "USD", nil, w.agent.Agent))

	// Counters and limits move only through reservations and limit changes.
	wantState(t, f.Exec("alice", `UPDATE eacp.budget_accounts SET hard_limit = 100 WHERE id = $1`, root), sqlForbidden)
	wantState(t, f.Exec("alice", `UPDATE eacp.budget_accounts SET reserved = 0 WHERE id = $1`, leaf), sqlForbidden)
	wantState(t, f.Exec("alice", `UPDATE eacp.budget_accounts SET name = 'x' WHERE id = $1`, leaf), sqlForbidden)
	wantState(t, f.Exec("alice", `DELETE FROM eacp.budget_accounts WHERE id = $1`, leaf), sqlForbidden)
	if n := w.text(t, `SELECT count(*)::text FROM eacp.audit_events
		WHERE convert_from(payload, 'UTF8')::jsonb ->> 'action' = 'budget_accounts.insert'`); n != "3" {
		t.Fatalf("journaled account creations = %s", n)
	}
}

func TestRaisingALimitIsTwoPersonAndLoweringIsNot(t *testing.T) {
	w := newWorld(t)
	f := w.f
	acct := f.ID(t, "alice", accountSQL, "ops", "THB", nil, nil)
	_, err := f.TryID("erin", limitSQL, acct, "100")
	wantState(t, err, sqlForbidden)
	_, err = f.TryID("alice", `INSERT INTO eacp.budget_limit_changes (tenant_id, account_id, new_limit, reason)
		VALUES (eacp.current_tenant_id(), $1, 100, ' ') RETURNING id`, acct)
	wantState(t, err, sqlCheck)
	_, err = f.TryID("alice", `INSERT INTO eacp.budget_limit_changes (tenant_id, account_id, new_limit, reason, state)
		VALUES (eacp.current_tenant_id(), $1, 100, 'x', 'APPLIED') RETURNING id`, acct)
	wantState(t, err, sqlCheck)

	raise := f.ID(t, "alice", limitSQL, acct, "100")
	if st := w.text(t, `SELECT state || ' ' || trim_scale(old_limit) FROM eacp.budget_limit_changes WHERE id = $1`, raise); st != "PROPOSED 0" {
		t.Fatalf("raise = %s", st)
	}
	if c := w.counters(t, acct); c != "0 0 0 0" {
		t.Fatalf("a proposal changed the account: %s", c)
	}
	_, err = f.TryID("bob", limitSQL, acct, "200")
	wantState(t, err, sqlUnique) // one open proposal per account
	wantState(t, f.Exec("alice", decideSQL, raise, "APPLIED", "mine"), sqlForbidden)
	wantState(t, f.Exec("erin", decideSQL, raise, "APPLIED", "ok"), sqlForbidden)
	wantState(t, f.Exec("bob", decideSQL, raise, "APPLIED", ""), sqlCheck)
	ok(t, f.Exec("bob", decideSQL, raise, "APPLIED", "agreed"))
	if c := w.counters(t, acct); c != "100 0 0 0" {
		t.Fatalf("after approval = %s", c)
	}
	wantState(t, f.Exec("bob", decideSQL, raise, "REJECTED", "changed my mind"), sqlBadState)

	// One admin lowers at once.
	lower := f.ID(t, "alice", limitSQL, acct, "40")
	if st := w.text(t, `SELECT state || ' ' || decided_by FROM eacp.budget_limit_changes WHERE id = $1`, lower); st != "APPLIED "+f.P["alice"].String() {
		t.Fatalf("lower = %s", st)
	}
	// An approval is for "from X to Y": it fails once the limit moved.
	stale := f.ID(t, "alice", limitSQL, acct, "500")
	ok(t, f.Exec("bob", limitSQL, acct, "30"))
	wantState(t, f.Exec("bob", decideSQL, stale, "APPLIED", "agreed"), sqlBadState)
	ok(t, f.Exec("alice", decideSQL, stale, "REJECTED", "superseded"))
	wantState(t, f.Exec("alice", `UPDATE eacp.budget_limit_changes SET new_limit = 1 WHERE id = $1`, stale), sqlForbidden)
	if c := w.counters(t, acct); c != "30 0 0 0" {
		t.Fatalf("final = %s", c)
	}
	got := w.text(t, `SELECT string_agg(convert_from(payload, 'UTF8')::jsonb ->> 'action', ',' ORDER BY seq)
		FROM eacp.audit_events WHERE convert_from(payload, 'UTF8')::jsonb ->> 'action' LIKE 'budget.limit%'`)
	if got != "budget.limit_proposed,budget.limit_applied,budget.limit_applied,budget.limit_proposed,budget.limit_applied,budget.limit_rejected" {
		t.Fatalf("journal = %s", got)
	}
}

func TestEscrowBoundsChildrenByTheirParent(t *testing.T) {
	w := newWorld(t)
	f := w.f
	root := f.ID(t, "alice", accountSQL, "ops", "THB", nil, nil)
	a := f.ID(t, "alice", accountSQL, "team-a", "THB", root, nil)
	b := f.ID(t, "alice", accountSQL, "team-b", "THB", root, nil)
	f.SetLimit(t, root, "100")
	f.SetLimit(t, a, "60")
	if c := w.counters(t, root); c != "100 60 0 0" {
		t.Fatalf("root after escrow = %s", c)
	}
	over := f.ID(t, "alice", limitSQL, b, "50")
	wantState(t, f.Exec("bob", decideSQL, over, "APPLIED", "agreed"), sqlBadState)
	ok(t, f.Exec("alice", decideSQL, over, "REJECTED", "no room"))
	f.SetLimit(t, b, "40")
	_, err := f.TryID("alice", limitSQL, root, "90")
	wantState(t, err, sqlBadState) // below what the children hold
	f.SetLimit(t, a, "10")
	if c := w.counters(t, root); c != "100 50 0 0" {
		t.Fatalf("root after a child's decrease = %s", c)
	}
}

func TestReservationsAreMadeOnlyByTheReleaseForTheActionsCost(t *testing.T) {
	w := newWorld(t)
	f := w.f
	id := w.authorized(t)

	// No account: the release is denied by the engine; raw T10 is refused.
	if st, err := w.release(t, id, false); err != nil || st != "no_account" {
		t.Fatalf("release without an account = %s %v", st, err)
	}
	leaf := f.FundAgent(t, w.agent.Agent, "THB", "2500000")
	_, err := w.release(t, id, true)
	wantState(t, err, sqlBadState)

	// Rows are made only for the action's exact cost, by its release actor.
	insert := `INSERT INTO eacp.budget_reservations (tenant_id, account_id, unit, action_id, contract_id, amount, expires_at)
		VALUES (eacp.current_tenant_id(), $1, 'THB', $2, $3, $4::numeric, now()) RETURNING id`
	_, err = f.TryAgentID(w.agent.Version, insert, leaf, id, w.tool.Contract, "1")
	wantState(t, err, sqlBadState)
	_, err = f.TryID("alice", insert, leaf, id, w.tool.Contract, "1000000")
	wantState(t, err, sqlForbidden)
	other := f.ActiveAgent(t, "other", w.tool.Tool)
	_, err = f.TryAgentID(other.Version, insert, leaf, id, w.tool.Contract, "1000000")
	wantState(t, err, sqlForbidden)

	if st, err := w.release(t, id, false); err != nil || st != "reserved" {
		t.Fatalf("release = %s %v", st, err)
	}
	if c := w.counters(t, leaf); c != "2500000 0 1000000 0" {
		t.Fatalf("leaf = %s", c)
	}
	r := w.text(t, `SELECT r.id::text FROM eacp.budget_reservations r JOIN eacp.actions a ON a.id = r.action_id
		WHERE r.action_id = $1 AND r.state = 'ACTIVE' AND r.expires_at = a.not_after AND r.amount = 1000000`, id)
	if r == "<nil>" {
		t.Fatal("no ACTIVE reservation with the action's TTL")
	}
	// Settlement follows the action; the amount and identity never change.
	wantState(t, f.ExecAgent(w.agent.Version, `UPDATE eacp.budget_reservations SET state = 'COMMITTED',
		settle_reason = 'x' WHERE id = $1`, r), sqlBadState)
	wantState(t, f.ExecAgent(w.agent.Version, `UPDATE eacp.budget_reservations SET state = 'RELEASED',
		settle_reason = 'x' WHERE id = $1`, r), sqlBadState)
	wantState(t, f.ExecAgent(w.agent.Version, `UPDATE eacp.budget_reservations SET folded_at = now() WHERE id = $1`, r), sqlCheck)
	wantState(t, f.ExecAgent(w.agent.Version, `UPDATE eacp.budget_reservations SET amount = 0 WHERE id = $1`, r), sqlForbidden)
	wantState(t, f.ExecAgent(w.agent.Version, `DELETE FROM eacp.budget_reservations WHERE id = $1`, r), sqlForbidden)

	// Cancelling the QUEUED action releases the reservation in its transaction;
	// the next reservation on the leaf folds it.
	ok(t, f.ExecAgent(w.agent.Version, `UPDATE eacp.actions SET state = 'CANCELLED', state_reason = 'not needed'
		WHERE id = $1`, id))
	if st := w.text(t, `SELECT state || ' ' || settle_reason || ' ' || trim_scale(committed_amount)
		FROM eacp.budget_reservations WHERE id = $1`, r); st != "RELEASED cancelled 0" {
		t.Fatalf("after cancel = %s", st)
	}
	if c := w.counters(t, leaf); c != "2500000 0 1000000 0" {
		t.Fatalf("settlement touched the account: %s", c)
	}
	next := w.authorized(t)
	if st, err := w.release(t, next, false); err != nil || st != "reserved" {
		t.Fatalf("next release = %s %v", st, err)
	}
	if c := w.counters(t, leaf); c != "2500000 0 1000000 0" {
		t.Fatalf("after the fold = %s", c)
	}
	got := w.text(t, `SELECT string_agg(convert_from(payload, 'UTF8')::jsonb ->> 'action', ',' ORDER BY seq)
		FROM eacp.audit_events WHERE convert_from(payload, 'UTF8')::jsonb ->> 'action' LIKE 'budget.%'
		  AND convert_from(payload, 'UTF8')::jsonb ->> 'action' NOT LIKE 'budget.limit%'`)
	if got != "budget.reserved,budget.released,budget.reserved" {
		t.Fatalf("journal = %s", got)
	}
}

func TestReserveReportsWhyItCannotReserve(t *testing.T) {
	w := newWorld(t)
	f := w.f
	leaf := f.FundAgent(t, w.agent.Agent, "THB", "1500000")
	first := w.authorized(t)
	if st, err := w.release(t, first, false); err != nil || st != "reserved" {
		t.Fatalf("first = %s %v", st, err)
	}
	second := w.authorized(t)
	if st, err := w.release(t, second, false); err != nil || st != "exceeded" {
		t.Fatalf("second = %s %v", st, err)
	}
	if c := w.counters(t, leaf); c != "1500000 0 1000000 0" {
		t.Fatalf("leaf = %s", c)
	}
	// The account CHECK is the backstop: a correctly priced reservation made
	// by hand still cannot oversubscribe the leaf.
	_, err := f.TryAgentID(w.agent.Version, `INSERT INTO eacp.budget_reservations
		(tenant_id, account_id, unit, action_id, contract_id, amount, expires_at)
		VALUES (eacp.current_tenant_id(), $1, 'THB', $2, $3, 1000000, now()) RETURNING id`, leaf, second, w.tool.Contract)
	wantState(t, err, sqlCheck)

	// A contract whose unit field the payload does not match.
	usd := f.ID(t, "erin", `INSERT INTO eacp.tool_contracts (tenant_id, tool_id, side_effects, idempotency_mode,
		reconciliation_lookup, reconciliation_consistency, proof_standard, max_attempts,
		cost_unit, cost_amount_field, cost_unit_field)
		VALUES (eacp.current_tenant_id(), $1, '{FINANCIAL}', 'none', 'none', 'none', 'none', 1,
		'USD', 'amount', 'currency') RETURNING id`, w.tool.Tool)
	ok(t, f.Exec("rita", `UPDATE eacp.tools SET active_contract_id = $1 WHERE id = $2`, usd, w.tool.Tool))
	if st, err := w.release(t, second, false); err != nil || st != "invalid_cost" {
		t.Fatalf("usd = %s %v", st, err)
	}
	// An unbudgeted contract releases without a reservation.
	free := f.ID(t, "erin", registrytest.SafeContractSQL, w.tool.Tool)
	ok(t, f.Exec("ravi", `UPDATE eacp.tools SET active_contract_id = $1 WHERE id = $2`, free, w.tool.Tool))
	if st, err := w.release(t, second, false); err != nil || st != "unbudgeted" {
		t.Fatalf("free = %s %v", st, err)
	}
	if n := w.text(t, `SELECT count(*)::text FROM eacp.budget_reservations WHERE action_id = $1`, second); n != "0" {
		t.Fatalf("reservations of an unbudgeted release = %s", n)
	}
}

func TestBudgetRowsAreTenantIsolated(t *testing.T) {
	w := newWorld(t)
	f := w.f
	leaf := f.FundAgent(t, w.agent.Agent, "THB", "5000000")
	id := w.authorized(t)
	if st, err := w.release(t, id, false); err != nil || st != "reserved" {
		t.Fatalf("release = %s %v", st, err)
	}
	ctx := context.Background()
	err := storage.InTenantTx(ctx, f.App, pgtest.TenantB, func(tx pgx.Tx) error {
		for _, table := range []string{"budget_accounts", "budget_reservations", "budget_limit_changes"} {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM eacp.`+table).Scan(&n); err != nil {
				return err
			}
			if n != 0 {
				t.Errorf("tenant B sees %d %s rows", n, table)
			}
		}
		// A limit change naming tenant A's account finds nothing.
		_, err := tx.Exec(ctx, `INSERT INTO eacp.budget_limit_changes (tenant_id, account_id, new_limit, reason)
			VALUES (eacp.current_tenant_id(), $1, 1, 'x')`, leaf)
		return err
	})
	if err == nil {
		t.Fatal("tenant B changed tenant A's limit")
	}
}

func TestALeafReservationDoesNotWaitForItsParent(t *testing.T) {
	w := newWorld(t)
	f := w.f
	root := f.ID(t, "alice", accountSQL, "ops", "THB", nil, nil)
	leaf := f.ID(t, "alice", accountSQL, "buyer", "THB", root, w.agent.Agent)
	f.SetLimit(t, root, "10000000")
	f.SetLimit(t, leaf, "5000000")
	id := w.authorized(t)

	// Another transaction holds the parent row (as a limit change on a
	// sibling would) until the release is done.
	ctx := context.Background()
	holder, err := f.Owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck
	if _, err := holder.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, pgtest.TenantA); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `SELECT 1 FROM eacp.budget_accounts WHERE id = $1 FOR UPDATE`, root); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := w.release(t, id, false)
		done <- err
	}()
	select {
	case err := <-done:
		ok(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the leaf reservation waited for its parent")
	}
	if c := w.counters(t, leaf); c != "5000000 0 1000000 0" {
		t.Fatalf("leaf = %s", c)
	}
}

// Releases (leaf, then journal), settlements (journal only) and limit
// changes (parent, leaf, then journal) on one account tree never deadlock,
// and the account never ends oversubscribed. Raw SQL: nothing retries.
func TestReleasesSettlementsAndLimitChangesDoNotDeadlock(t *testing.T) {
	w := newWorld(t)
	f := w.f
	root := f.ID(t, "alice", accountSQL, "ops", "THB", nil, nil)
	leaf := f.ID(t, "alice", accountSQL, "buyer", "THB", root, w.agent.Agent)
	f.SetLimit(t, root, "100000000")
	f.SetLimit(t, leaf, "20000000")
	const n = 24
	ids := make([]uuid.UUID, n)
	for i := range ids {
		ids[i] = w.authorized(t)
	}

	var mu sync.Mutex
	var failures []string
	record := func(what string, err error) {
		var pgErr *pgconn.PgError
		if err == nil || (errors.As(err, &pgErr) && (pgErr.Code == sqlBadState || pgErr.Code == sqlUnique)) {
			return // an expected refusal (a limit below usage, an open proposal)
		}
		mu.Lock()
		defer mu.Unlock()
		failures = append(failures, what+": "+err.Error())
	}
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Go(func() {
			st, err := w.release(t, id, false)
			record("release", err)
			if err == nil && st == "reserved" {
				record("cancel", f.ExecAgent(w.agent.Version, `UPDATE eacp.actions SET state = 'CANCELLED',
					state_reason = 'load test' WHERE id = $1`, id))
			}
		})
	}
	for i := range 6 {
		wg.Go(func() {
			account := leaf
			if i%2 == 1 {
				account = root
			}
			for j := range 4 {
				limit := fmt.Sprintf("%d", 20000000+(i*4+j)*100000)
				change, err := f.TryID("alice", limitSQL, account, limit)
				record("propose", err)
				if err == nil {
					if err := f.Exec("bob", decideSQL, change, "APPLIED", "load test"); err != nil {
						record("approve", err) // stale: the limit moved since the proposal
						record("reject", f.Exec("alice", decideSQL, change, "REJECTED", "stale"))
					}
				}
				_, err = f.TryID("bob", limitSQL, account, "19000000")
				record("lower", err)
			}
		})
	}
	wg.Wait()
	for _, msg := range failures {
		t.Error(msg)
	}
	// Every action was released then cancelled: after a fold, nothing is
	// reserved and the counters match the rows.
	extra := w.authorized(t)
	f.SetLimit(t, root, "100000000")
	f.SetLimit(t, leaf, "20000000")
	if st, err := w.release(t, extra, false); err != nil || st != "reserved" {
		t.Fatalf("final release = %s %v", st, err)
	}
	if got := w.text(t, `SELECT (b.reserved = (SELECT COALESCE(sum(amount), 0) FROM eacp.budget_reservations
			WHERE account_id = b.id AND folded_at IS NULL))::text
		FROM eacp.budget_accounts b WHERE b.id = $1`, leaf); got != "true" {
		t.Fatal("reserved does not match the unfolded reservations")
	}
	if c := w.counters(t, leaf); c != "20000000 0 1000000 0" {
		t.Fatalf("leaf = %s", c)
	}
}

// ADR-012 §5: settling never locks the account, so a transaction that has
// already taken the journal head (an operator resolution, a reconciliation)
// can settle while a release holds the leaf, and they cannot deadlock.
func TestSettlementNeverWaitsForTheAccount(t *testing.T) {
	w := newWorld(t)
	f := w.f
	leaf := f.FundAgent(t, w.agent.Agent, "THB", "5000000")
	id := w.authorized(t)
	if st, err := w.release(t, id, false); err != nil || st != "reserved" {
		t.Fatalf("release = %s %v", st, err)
	}
	ctx := context.Background()
	holder, err := f.Owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck
	if _, err := holder.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, pgtest.TenantA); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `SELECT 1 FROM eacp.budget_accounts WHERE id = $1 FOR UPDATE`, leaf); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- f.ExecAgent(w.agent.Version, `UPDATE eacp.actions SET state = 'CANCELLED', state_reason = 'not needed'
			WHERE id = $1`, id)
	}()
	select {
	case err := <-done:
		ok(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("settling waited for the account row")
	}
	if st := w.text(t, `SELECT state FROM eacp.budget_reservations WHERE action_id = $1`, id); st != "RELEASED" {
		t.Fatalf("reservation = %s", st)
	}
}
