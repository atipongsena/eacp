package fleet_test

// Schema-level tests (ADR-024 §3): every fleet rule is enforced by
// PostgreSQL, so these tests issue raw SQL as the application role.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

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

	opSQL = `INSERT INTO eacp.fleet_operations (tenant_id, kind, selector, reason, source_operation_id)
		VALUES (eacp.current_tenant_id(), $1, '{"test":true}', $2, $3) RETURNING id`
	targetSQL = `INSERT INTO eacp.fleet_operation_targets (tenant_id, operation_id, version_id, to_state)
		VALUES (eacp.current_tenant_id(), $1, $2, $3)`
)

func wantState(t *testing.T, err error, codes ...string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("err = %v, want SQLSTATE %v", err, codes)
	}
	for _, c := range codes {
		if pgErr.Code == c {
			return
		}
	}
	t.Fatalf("SQLSTATE %s (%s), want %v", pgErr.Code, pgErr.Message, codes)
}

type target struct {
	version uuid.UUID
	to      string
}

// apply runs one fleet operation in one transaction as actor and returns
// its id. A nil source is stored as NULL.
func apply(f *registrytest.Fixture, actor, kind string, source *uuid.UUID, targets ...target) (uuid.UUID, error) {
	ctx := context.Background()
	var id uuid.UUID
	err := storage.InTenantTx(ctx, f.App, f.Tenant.String(), func(tx pgx.Tx) error {
		if err := storage.SetActor(ctx, tx, f.P[actor]); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, opSQL, kind, "incident 42", source).Scan(&id); err != nil {
			return err
		}
		for _, tg := range targets {
			if _, err := tx.Exec(ctx, targetSQL, id, tg.version, tg.to); err != nil {
				return err
			}
		}
		return nil
	})
	return id, err
}

func mustApply(t *testing.T, f *registrytest.Fixture, actor, kind string, source *uuid.UUID, targets ...target) uuid.UUID {
	t.Helper()
	id, err := apply(f, actor, kind, source, targets...)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func versionState(t *testing.T, f *registrytest.Fixture, v uuid.UUID) (state, reason string) {
	t.Helper()
	ctx := context.Background()
	if err := storage.InTenantTx(ctx, f.App, f.Tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state, COALESCE(state_reason, '') FROM eacp.agent_versions WHERE id = $1`, v).
			Scan(&state, &reason)
	}); err != nil {
		t.Fatal(err)
	}
	return state, reason
}

// lastAudit returns the newest audit payload of the fixture tenant.
func lastAudit(t *testing.T, f *registrytest.Fixture) map[string]any {
	t.Helper()
	ctx := context.Background()
	var raw []byte
	if err := storage.InTenantTx(ctx, f.App, f.Tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT payload FROM eacp.audit_events ORDER BY seq DESC LIMIT 1`).Scan(&raw)
	}); err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// newVersion registers another version of agent with an allowlist authored
// by editor and activated by rita. It stays REGISTERED.
func newVersion(t *testing.T, f *registrytest.Fixture, agent uuid.UUID, editor string) uuid.UUID {
	t.Helper()
	v := f.ID(t, "erin", `INSERT INTO eacp.agent_versions (tenant_id, agent_id, runtime, code_ref)
		VALUES (eacp.current_tenant_id(), $1, 'python', 'git:next') RETURNING id`, agent)
	al := f.ID(t, editor, `INSERT INTO eacp.agent_allowlists (tenant_id, agent_version_id, tool_ids)
		VALUES (eacp.current_tenant_id(), $1, '{}') RETURNING id`, v)
	approver := "rita"
	if editor == "rita" {
		approver = "ravi"
	}
	if err := f.Exec(approver, `UPDATE eacp.agent_versions SET active_allowlist_id = $1 WHERE id = $2`, al, v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestPauseTargetRecordsDatabaseStateAndIsJournaled(t *testing.T) {
	f := registrytest.New(t)
	a := f.ActiveAgent(t, "buyer")
	op := mustApply(t, f, "otto", "pause", nil, target{a.Version, "SUSPENDED"})

	state, reason := versionState(t, f, a.Version)
	if state != "SUSPENDED" || reason != "fleet pause "+op.String()+": incident 42" {
		t.Fatalf("version after pause = %s %q", state, reason)
	}
	ctx := context.Background()
	var from, agent string
	var ordinal int
	if err := storage.InTenantTx(ctx, f.App, f.Tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT from_state, agent_id::text, ordinal FROM eacp.fleet_operation_targets
			WHERE operation_id = $1 AND version_id = $2`, op, a.Version).Scan(&from, &agent, &ordinal)
	}); err != nil {
		t.Fatal(err)
	}
	if from != "ACTIVE" || agent != a.Agent.String() || ordinal != 1 {
		t.Fatalf("target = %s %s %d", from, agent, ordinal)
	}
	// The operation's journal entry is the last one, after the transition.
	p := lastAudit(t, f)
	data, _ := p["data"].(map[string]any)
	targets, _ := data["targets"].([]any)
	if p["action"] != "fleet.pause" || p["reason"] != "incident 42" || len(targets) != 1 {
		t.Fatalf("fleet audit = %v", p)
	}
	actor, _ := p["actor"].(map[string]any)
	if actor["id"] != f.P["otto"].String() {
		t.Fatalf("fleet audit actor = %v", actor)
	}
}

func TestTargetsBelongToTheCreatingTransactionAndActor(t *testing.T) {
	f := registrytest.New(t)
	a := f.ActiveAgent(t, "buyer")
	op := mustApply(t, f, "otto", "pause", nil, target{a.Version, "SUSPENDED"})
	b := f.ActiveAgent(t, "seller")
	// A later transaction, even by the same operator, cannot extend it.
	wantState(t, f.Exec("otto", targetSQL, op, b.Version, "SUSPENDED"), sqlBadState)
	wantState(t, f.Exec("opal", targetSQL, op, b.Version, "SUSPENDED"), sqlBadState)
	if state, _ := versionState(t, f, b.Version); state != "ACTIVE" {
		t.Fatalf("foreign target changed the version: %s", state)
	}
}

func TestEmptyOperationIsRejectedAtCommit(t *testing.T) {
	f := registrytest.New(t)
	_, err := apply(f, "otto", "pause", nil)
	wantState(t, err, sqlCheck)
}

func TestOperationNeedsAPrincipalAndAReason(t *testing.T) {
	f := registrytest.New(t)
	a := f.ActiveAgent(t, "buyer")
	wantState(t, f.Exec("", opSQL, "pause", "x", nil), sqlForbidden)
	wantState(t, f.Exec("otto", opSQL, "pause", "  ", nil), sqlCheck)
	wantState(t, f.Exec("ravi", opSQL, "restart", "x", nil), sqlCheck)
	// A role the transition needs is checked early and again by the version guard.
	_, err := apply(f, "carol", "pause", nil, target{a.Version, "SUSPENDED"})
	wantState(t, err, sqlForbidden)
	_, err = apply(f, "otto", "resume", nil)
	wantState(t, err, sqlForbidden)
}

func TestKindAllowsOnlyItsTransition(t *testing.T) {
	f := registrytest.New(t)
	a := f.ActiveAgent(t, "buyer")
	for _, tc := range []struct {
		kind, to string
	}{
		{"pause", "QUARANTINED"}, {"pause", "RETIRED"}, {"quarantine", "SUSPENDED"}, {"rollback", "QUARANTINED"},
	} {
		_, err := apply(f, "ravi", tc.kind, nil, target{a.Version, tc.to})
		wantState(t, err, sqlBadState)
	}
	// Pausing a version that is not ACTIVE is refused, not skipped.
	mustApply(t, f, "otto", "pause", nil, target{a.Version, "SUSPENDED"})
	_, err := apply(f, "otto", "pause", nil, target{a.Version, "SUSPENDED"})
	wantState(t, err, sqlBadState)
	// The same version twice in one operation.
	b := f.ActiveAgent(t, "seller")
	_, err = apply(f, "otto", "pause", nil, target{b.Version, "SUSPENDED"}, target{b.Version, "SUSPENDED"})
	wantState(t, err, sqlBadState, "23505")
	if state, _ := versionState(t, f, b.Version); state != "ACTIVE" {
		t.Fatalf("a refused operation changed a version: %s", state)
	}
}

func TestResumeOnlyWhatThePauseSuspended(t *testing.T) {
	f := registrytest.New(t)
	a := f.ActiveAgent(t, "buyer")
	b := f.ActiveAgent(t, "seller")
	pause := mustApply(t, f, "otto", "pause", nil, target{a.Version, "SUSPENDED"})
	if err := f.Exec("otto", `UPDATE eacp.agent_versions SET state = 'SUSPENDED', state_reason = 'manual' WHERE id = $1`, b.Version); err != nil {
		t.Fatal(err)
	}
	_, err := apply(f, "ravi", "resume", nil, target{a.Version, "ACTIVE"})
	wantState(t, err, sqlCheck) // resume needs its pause
	_, err = apply(f, "ravi", "resume", &pause, target{b.Version, "ACTIVE"})
	wantState(t, err, sqlBadState) // b was not suspended by that pause
	quarantine := mustApply(t, f, "otto", "quarantine", nil, target{b.Version, "QUARANTINED"})
	_, err = apply(f, "ravi", "resume", &quarantine, target{a.Version, "ACTIVE"})
	wantState(t, err, sqlCheck) // the source must be a pause
	_, err = apply(f, "otto", "pause", &pause, target{a.Version, "SUSPENDED"})
	wantState(t, err, sqlCheck) // only resume and release take a source
	mustApply(t, f, "ravi", "resume", &pause, target{a.Version, "ACTIVE"})
	if state, _ := versionState(t, f, a.Version); state != "ACTIVE" {
		t.Fatalf("resumed version = %s", state)
	}
}

// The version guard still decides who may grant: a fleet resume cannot do
// what a single transition could not (ADR-003 §2).
func TestFleetResumeKeepsVersionSeparationOfDuties(t *testing.T) {
	f := registrytest.New(t)
	a := f.NewAgent(t, "buyer")
	al := f.ID(t, "rita", `INSERT INTO eacp.agent_allowlists (tenant_id, agent_version_id, tool_ids)
		VALUES (eacp.current_tenant_id(), $1, '{}') RETURNING id`, a.Version)
	if err := f.Exec("ravi", `UPDATE eacp.agent_versions SET active_allowlist_id = $1 WHERE id = $2`, al, a.Version); err != nil {
		t.Fatal(err)
	}
	if err := f.Exec("ravi", `UPDATE eacp.agent_versions SET state = 'ACTIVE', state_reason = 'go live' WHERE id = $1`, a.Version); err != nil {
		t.Fatal(err)
	}
	pause := mustApply(t, f, "otto", "pause", nil, target{a.Version, "SUSPENDED"})
	_, err := apply(f, "rita", "resume", &pause, target{a.Version, "ACTIVE"}) // rita wrote the allowlist
	wantState(t, err, sqlForbidden)
	mustApply(t, f, "ravi", "resume", &pause, target{a.Version, "ACTIVE"})
}

func TestQuarantineAndReleaseByAnotherApprover(t *testing.T) {
	f := registrytest.New(t)
	a := f.ActiveAgent(t, "buyer")
	older := newVersion(t, f, a.Agent, "erin") // REGISTERED sibling
	q := mustApply(t, f, "rita", "quarantine", nil, target{a.Version, "QUARANTINED"}, target{older, "QUARANTINED"})
	for _, v := range []uuid.UUID{a.Version, older} {
		if state, _ := versionState(t, f, v); state != "QUARANTINED" {
			t.Fatalf("quarantined version = %s", state)
		}
	}
	_, err := apply(f, "rita", "release", &q, target{a.Version, "SUSPENDED"})
	wantState(t, err, sqlForbidden) // the quarantiner cannot release
	_, err = apply(f, "ravi", "release", &q, target{a.Version, "ACTIVE"})
	wantState(t, err, sqlBadState) // release never activates
	mustApply(t, f, "ravi", "release", &q, target{a.Version, "SUSPENDED"})
	_, err = apply(f, "ravi", "release", &q, target{a.Version, "SUSPENDED"})
	wantState(t, err, sqlBadState) // no longer quarantined
}

// rollbackAgent returns an agent whose v1 is SUSPENDED and v2 ACTIVE.
func rollbackAgent(t *testing.T, f *registrytest.Fixture, name string) (registrytest.Agent, uuid.UUID) {
	t.Helper()
	a := f.ActiveAgent(t, name)
	if err := f.Exec("otto", `UPDATE eacp.agent_versions SET state = 'SUSPENDED', state_reason = 'upgrade' WHERE id = $1`, a.Version); err != nil {
		t.Fatal(err)
	}
	v2 := newVersion(t, f, a.Agent, "erin")
	if err := f.Exec("ravi", `UPDATE eacp.agent_versions SET state = 'ACTIVE', state_reason = 'upgrade' WHERE id = $1`, v2); err != nil {
		t.Fatal(err)
	}
	return a, v2
}

func TestRollbackActivatesAnOlderVersionOfOneAgent(t *testing.T) {
	f := registrytest.New(t)
	a, v2 := rollbackAgent(t, f, "buyer")
	_, b2 := rollbackAgent(t, f, "seller")

	// The newest version is not a rollback target.
	_, err := apply(f, "ravi", "rollback", nil, target{a.Version, "ACTIVE"})
	wantState(t, err, sqlBadState, "23505") // v2 is still ACTIVE
	_, err = apply(f, "ravi", "rollback", nil, target{v2, "SUSPENDED"}, target{v2, "ACTIVE"})
	wantState(t, err, sqlBadState, "23505")
	// One agent per rollback.
	_, err = apply(f, "ravi", "rollback", nil, target{v2, "SUSPENDED"}, target{b2, "SUSPENDED"})
	wantState(t, err, sqlBadState)
	// A rollback must activate a version.
	_, err = apply(f, "ravi", "rollback", nil, target{v2, "SUSPENDED"})
	wantState(t, err, sqlCheck)
	// Operators contain; only a registry approver grants.
	_, err = apply(f, "otto", "rollback", nil, target{v2, "SUSPENDED"}, target{a.Version, "ACTIVE"})
	wantState(t, err, sqlForbidden)

	mustApply(t, f, "ravi", "rollback", nil, target{v2, "SUSPENDED"}, target{a.Version, "ACTIVE"})
	if s, _ := versionState(t, f, a.Version); s != "ACTIVE" {
		t.Fatalf("rolled back version = %s", s)
	}
	if s, _ := versionState(t, f, v2); s != "SUSPENDED" {
		t.Fatalf("replaced version = %s", s)
	}
	// Rolling "forward" is not a rollback: v2 is newer than the active v1.
	_, err = apply(f, "ravi", "rollback", nil, target{a.Version, "SUSPENDED"}, target{v2, "ACTIVE"})
	wantState(t, err, sqlBadState)
}

func TestFleetTablesAreInsertOnly(t *testing.T) {
	f := registrytest.New(t)
	a := f.ActiveAgent(t, "buyer")
	op := mustApply(t, f, "otto", "pause", nil, target{a.Version, "SUSPENDED"})
	wantState(t, f.Exec("otto", `UPDATE eacp.fleet_operations SET reason = 'x' WHERE id = $1`, op), sqlForbidden)
	wantState(t, f.Exec("otto", `DELETE FROM eacp.fleet_operations WHERE id = $1`, op), sqlForbidden)
	wantState(t, f.Exec("otto", `UPDATE eacp.fleet_operation_targets SET to_state = 'ACTIVE' WHERE operation_id = $1`, op), sqlForbidden)
	wantState(t, f.Exec("otto", `DELETE FROM eacp.fleet_operation_targets WHERE operation_id = $1`, op), sqlForbidden)
}

func TestFleetTargetMustBeATenantVersion(t *testing.T) {
	f := registrytest.New(t)
	other := f.ForTenant(t, pgtest.TenantB)
	foreign := other.ActiveAgent(t, "foreign")
	_, err := apply(f, "otto", "pause", nil, target{foreign.Version, "SUSPENDED"})
	wantState(t, err, sqlForeignKey)
	if s, _ := versionState(t, other, foreign.Version); s != "ACTIVE" {
		t.Fatalf("foreign version = %s", s)
	}
}

func TestOperationChangesAtMost500Versions(t *testing.T) {
	f := registrytest.New(t)
	a := f.NewAgent(t, "big")
	if err := f.Exec("erin", `INSERT INTO eacp.agent_versions (tenant_id, agent_id, runtime, code_ref)
		SELECT eacp.current_tenant_id(), $1, 'python', 'git:' || g FROM generate_series(1, 500) g`, a.Agent); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	err := storage.InTenantTx(ctx, f.App, f.Tenant.String(), func(tx pgx.Tx) error {
		if err := storage.SetActor(ctx, tx, f.P["otto"]); err != nil {
			return err
		}
		var op uuid.UUID
		if err := tx.QueryRow(ctx, opSQL, "quarantine", "too big", nil).Scan(&op); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO eacp.fleet_operation_targets (tenant_id, operation_id, version_id, to_state)
			SELECT eacp.current_tenant_id(), $1, id, 'QUARANTINED' FROM eacp.agent_versions
			WHERE agent_id = $2 ORDER BY id`, op, a.Agent)
		return err
	})
	wantState(t, err, sqlCheck)
}

// Rebinding the actor inside the transaction does not let another
// principal extend the operation.
func TestTargetsMustBeAddedByTheOperationsActor(t *testing.T) {
	f := registrytest.New(t)
	a := f.ActiveAgent(t, "buyer")
	ctx := context.Background()
	err := storage.InTenantTx(ctx, f.App, f.Tenant.String(), func(tx pgx.Tx) error {
		if err := storage.SetActor(ctx, tx, f.P["otto"]); err != nil {
			return err
		}
		var op uuid.UUID
		if err := tx.QueryRow(ctx, opSQL, "pause", "x", nil).Scan(&op); err != nil {
			return err
		}
		if err := storage.SetActor(ctx, tx, f.P["opal"]); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, targetSQL, op, a.Version, "SUSPENDED")
		return err
	})
	wantState(t, err, sqlBadState)
}

// Each kind allows only its own pair, even where the version guard would
// accept another transition (a release, a retirement or a revocation).
func TestKindCannotBorrowAnotherLegalTransition(t *testing.T) {
	f := registrytest.New(t)
	a := f.ActiveAgent(t, "buyer")
	q := mustApply(t, f, "otto", "quarantine", nil, target{a.Version, "QUARANTINED"})
	// ravi may release (QUARANTINED -> SUSPENDED), but not under "pause".
	_, err := apply(f, "ravi", "pause", nil, target{a.Version, "SUSPENDED"})
	wantState(t, err, sqlBadState)
	for _, to := range []string{"RETIRED", "REVOKED"} {
		_, err := apply(f, "ravi", "release", &q, target{a.Version, to})
		wantState(t, err, sqlBadState)
	}
	if s, _ := versionState(t, f, a.Version); s != "QUARANTINED" {
		t.Fatalf("version = %s", s)
	}
}

// Only the one-agent rule stops a rollback from suspending one agent and
// activating another's older version.
func TestRollbackCannotSpanTwoAgents(t *testing.T) {
	f := registrytest.New(t)
	_, v2 := rollbackAgent(t, f, "buyer")
	b, b2 := rollbackAgent(t, f, "seller")
	if err := f.Exec("otto", `UPDATE eacp.agent_versions SET state = 'SUSPENDED', state_reason = 'x' WHERE id = $1`, b2); err != nil {
		t.Fatal(err)
	}
	_, err := apply(f, "ravi", "rollback", nil, target{v2, "SUSPENDED"}, target{b.Version, "ACTIVE"})
	wantState(t, err, sqlBadState)
	if s, _ := versionState(t, f, b.Version); s != "SUSPENDED" {
		t.Fatalf("other agent's version = %s", s)
	}
}
