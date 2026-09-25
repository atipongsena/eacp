package bundle_test

// Schema-level tests (ADR-026 §5): every change-set rule is enforced by
// PostgreSQL, so these tests issue raw SQL as the application role.

import (
	"context"
	"crypto/sha256"
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
	sqlForbidden = "42501"
	sqlBadState  = "55000"
	sqlCheck     = "23514"
	sqlStale     = "40001"
	sqlUnique    = "23505"
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

func ok(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// inTx runs fn as actor in one transaction of the fixture tenant.
func inTx(f *registrytest.Fixture, actor string, fn func(ctx context.Context, tx pgx.Tx) error) error {
	ctx := context.Background()
	return storage.InTenantTx(ctx, f.App, f.Tenant.String(), func(tx pgx.Tx) error {
		if err := storage.SetActor(ctx, tx, f.P[actor]); err != nil {
			return err
		}
		return fn(ctx, tx)
	})
}

type rawStep struct{ address, op, stage string }
type rawRef struct {
	kind string
	id   uuid.UUID
}

// plan records and seals a PLANNED change set of bundle with steps and refs
// in one transaction as actor.
func plan(f *registrytest.Fixture, actor, bundle string, steps []rawStep, refs []rawRef) (uuid.UUID, error) {
	id := uuid.New()
	err := inTx(f, actor, func(ctx context.Context, tx pgx.Tx) error {
		if err := planIn(ctx, tx, id, bundle, steps, refs); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT eacp.change_set_seal($1)`, id)
		return err
	})
	return id, err
}

func planIn(ctx context.Context, tx pgx.Tx, id uuid.UUID, bundle string, steps []rawStep, refs []rawRef) error {
	if _, err := tx.Exec(ctx, `INSERT INTO eacp.bundles (tenant_id, name) VALUES (eacp.current_tenant_id(), $1)
		ON CONFLICT (tenant_id, name) DO NOTHING`, bundle); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO eacp.change_sets (tenant_id, id, bundle_id, state, desired, prune)
		SELECT eacp.current_tenant_id(), $1, id, 'PLANNED', '{"test": true}', false
		FROM eacp.bundles WHERE name = $2`, id, bundle); err != nil {
		return err
	}
	for i, s := range steps {
		if _, err := tx.Exec(ctx, `INSERT INTO eacp.change_set_steps
			(tenant_id, change_set_id, ordinal, address, op, stage, payload)
			VALUES (eacp.current_tenant_id(), $1, $2, $3, $4, $5, '{}')`, id, i+1, s.address, s.op, s.stage); err != nil {
			return err
		}
	}
	for _, r := range refs {
		if _, err := tx.Exec(ctx, `INSERT INTO eacp.change_set_refs (tenant_id, change_set_id, kind, object_id)
			VALUES (eacp.current_tenant_id(), $1, $2, $3)`, id, r.kind, r.id); err != nil {
			return err
		}
	}
	return nil
}

// runSteps marks each listed step (by ordinal) as run with a fresh object id.
func runSteps(ctx context.Context, tx pgx.Tx, id uuid.UUID, ordinals ...int) error {
	for _, o := range ordinals {
		if _, err := tx.Exec(ctx, `UPDATE eacp.change_set_steps SET object_id = $3
			WHERE change_set_id = $1 AND ordinal = $2`, id, o, uuid.New()); err != nil {
			return err
		}
	}
	return nil
}

func setState(ctx context.Context, tx pgx.Tx, id uuid.UUID, state string) error {
	_, err := tx.Exec(ctx, `UPDATE eacp.change_sets SET state = $2 WHERE id = $1`, id, state)
	return err
}

func twoStage() []rawStep {
	return []rawStep{{"connector.ledger", "create", "submit"}, {"tool.ledger.post", "create", "submit"},
		{"contract.ledger.post", "activate", "approve"}}
}

func auditCount(t *testing.T, f *registrytest.Fixture, action string) int {
	t.Helper()
	var n int
	ok(t, storage.InTenantTx(context.Background(), f.Owner, f.Tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM eacp.audit_events
			WHERE convert_from(payload, 'UTF8')::jsonb->>'action' = $1`, action).Scan(&n)
	}))
	return n
}

func TestPlanningNeedsARegistryRoleAndIsSealed(t *testing.T) {
	f := registrytest.New(t)
	_, err := plan(f, "carol", "ledger", twoStage(), nil)
	wantState(t, err, sqlForbidden)
	_, err = plan(f, "otto", "ledger", twoStage(), nil)
	wantState(t, err, sqlForbidden)

	id, err := plan(f, "erin", "ledger", twoStage(), nil)
	ok(t, err)
	var desired, base []byte
	ok(t, inTx(f, "erin", func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT desired_digest, base_digest FROM eacp.change_sets WHERE id = $1`, id).
			Scan(&desired, &base)
	}))
	want := sha256.Sum256([]byte(`{"test": true}`))
	if string(desired) != string(want[:]) || len(base) != 32 {
		t.Fatalf("desired %x (want %x), base %x", desired, want, base)
	}

	// A plan with no steps, or one not sealed, does not commit.
	_, err = plan(f, "rita", "empty", nil, nil)
	wantState(t, err, sqlCheck)
	err = inTx(f, "erin", func(ctx context.Context, tx pgx.Tx) error {
		return planIn(ctx, tx, uuid.New(), "unsealed", twoStage(), nil)
	})
	wantState(t, err, sqlCheck)
}

func TestAPlanIsImmutableOnceSealed(t *testing.T) {
	f := registrytest.New(t)
	id, err := plan(f, "erin", "ledger", twoStage(), nil)
	ok(t, err)
	for _, sql := range []string{
		`UPDATE eacp.change_sets SET desired = '{"other": 1}' WHERE id = $1`,
		`UPDATE eacp.change_sets SET base_digest = sha256('x') WHERE id = $1`,
		`INSERT INTO eacp.change_set_steps (tenant_id, change_set_id, ordinal, address, op, stage, payload)
		 VALUES (eacp.current_tenant_id(), $1, 9, 'agent.late', 'create', 'submit', '{}')`,
		`INSERT INTO eacp.change_set_refs (tenant_id, change_set_id, kind, object_id)
		 VALUES (eacp.current_tenant_id(), $1, 'agent', gen_random_uuid())`,
		`SELECT eacp.change_set_seal($1)`,
	} {
		wantState(t, f.Exec("erin", sql, id), sqlBadState)
	}
}

func TestOneOpenChangeSetPerBundle(t *testing.T) {
	f := registrytest.New(t)
	first, err := plan(f, "erin", "ledger", twoStage(), nil)
	ok(t, err)
	_, err = plan(f, "erin", "ledger", twoStage(), nil)
	wantState(t, err, sqlUnique)
	wantState(t, f.Exec("erin", `UPDATE eacp.change_sets SET state = 'SUPERSEDED' WHERE id = $1`, first), sqlCheck)
	ok(t, f.Exec("erin", `UPDATE eacp.change_sets SET state = 'SUPERSEDED', close_reason = 'replanned' WHERE id = $1`, first))
	_, err = plan(f, "erin", "ledger", twoStage(), nil)
	ok(t, err)
	if n := auditCount(t, f, "change_set.superseded"); n != 1 {
		t.Fatalf("superseded events = %d", n)
	}
}

func TestSubmitChecksTheDigestAndRunsStepsOnceInOrder(t *testing.T) {
	f := registrytest.New(t)
	tool := f.ActiveTool(t, "erp", "po")
	id, err := plan(f, "erin", "ledger", twoStage(), []rawRef{{"connector", tool.Connector}})
	ok(t, err)

	// A registry change to a ref makes the plan stale.
	ok(t, f.Exec("erin", `INSERT INTO eacp.tools (tenant_id, connector_id, name)
		VALUES (eacp.current_tenant_id(), $1, 'extra')`, tool.Connector))
	err = inTx(f, "erin", func(ctx context.Context, tx pgx.Tx) error { return setState(ctx, tx, id, "SUBMITTED") })
	wantState(t, err, sqlStale)

	ok(t, f.Exec("erin", `UPDATE eacp.change_sets SET state = 'REJECTED', close_reason = 'stale' WHERE id = $1`, id))
	id, err = plan(f, "erin", "ledger", twoStage(), []rawRef{{"connector", tool.Connector}})
	ok(t, err)

	// Steps run in order, once, only in the submitting transaction.
	err = inTx(f, "erin", func(ctx context.Context, tx pgx.Tx) error {
		if err := setState(ctx, tx, id, "SUBMITTED"); err != nil {
			return err
		}
		return runSteps(ctx, tx, id, 2)
	})
	wantState(t, err, sqlBadState)
	err = inTx(f, "erin", func(ctx context.Context, tx pgx.Tx) error {
		if err := setState(ctx, tx, id, "SUBMITTED"); err != nil {
			return err
		}
		return runSteps(ctx, tx, id, 1) // step 2 never runs: the commit check refuses
	})
	wantState(t, err, sqlCheck)
	err = inTx(f, "erin", func(ctx context.Context, tx pgx.Tx) error {
		if err := setState(ctx, tx, id, "SUBMITTED"); err != nil {
			return err
		}
		if err := runSteps(ctx, tx, id, 1, 2); err != nil {
			return err
		}
		return runSteps(ctx, tx, id, 1)
	})
	wantState(t, err, sqlBadState)
	ok(t, inTx(f, "erin", func(ctx context.Context, tx pgx.Tx) error {
		if err := setState(ctx, tx, id, "SUBMITTED"); err != nil {
			return err
		}
		if err := runSteps(ctx, tx, id, 1, 2); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT eacp.change_set_seal($1)`, id)
		return err
	}))
	// After commit, nobody runs a step of it again, and approve steps wait for approval.
	wantState(t, f.Exec("erin", `UPDATE eacp.change_set_steps SET object_id = gen_random_uuid()
		WHERE change_set_id = $1 AND ordinal = 3`, id), sqlBadState)
	if n := auditCount(t, f, "change_set.submitted"); n != 1 {
		t.Fatalf("submitted events = %d", n)
	}
}

func TestApprovalIsASecondPersonAgainstTheSealedDigest(t *testing.T) {
	f := registrytest.New(t)
	agent := f.NewAgent(t, "buyer")
	submit := func(actor string) uuid.UUID {
		id, err := plan(f, actor, "b-"+actor, twoStage(), []rawRef{{"version", agent.Version}})
		ok(t, err)
		ok(t, inTx(f, actor, func(ctx context.Context, tx pgx.Tx) error {
			if err := setState(ctx, tx, id, "SUBMITTED"); err != nil {
				return err
			}
			if err := runSteps(ctx, tx, id, 1, 2); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `SELECT eacp.change_set_seal($1)`, id)
			return err
		}))
		return id
	}
	approve := func(actor string, id uuid.UUID) error {
		return inTx(f, actor, func(ctx context.Context, tx pgx.Tx) error {
			if err := setState(ctx, tx, id, "APPLIED"); err != nil {
				return err
			}
			return runSteps(ctx, tx, id, 3)
		})
	}

	id := submit("rita")
	wantState(t, approve("rita", id), sqlForbidden) // the submitter
	wantState(t, approve("erin", id), sqlForbidden) // no approver role
	ok(t, approve("ravi", id))
	if n := auditCount(t, f, "change_set.applied"); n != 1 {
		t.Fatalf("applied events = %d", n)
	}

	// The digest sealed at submission covers the refs: a change makes it stale.
	id = submit("erin")
	ok(t, f.Exec("otto", `UPDATE eacp.agent_versions SET state = 'QUARANTINED', state_reason = 'incident' WHERE id = $1`,
		agent.Version))
	wantState(t, approve("rita", id), sqlStale)
}

func TestDirectApplyOnlyWithoutApproveSteps(t *testing.T) {
	f := registrytest.New(t)
	single := []rawStep{{"connector.ledger", "create", "submit"}}
	id, err := plan(f, "erin", "one", single, nil)
	ok(t, err)
	err = inTx(f, "erin", func(ctx context.Context, tx pgx.Tx) error { return setState(ctx, tx, id, "SUBMITTED") })
	wantState(t, err, sqlBadState)
	ok(t, inTx(f, "erin", func(ctx context.Context, tx pgx.Tx) error {
		if err := setState(ctx, tx, id, "APPLIED"); err != nil {
			return err
		}
		return runSteps(ctx, tx, id, 1)
	}))

	id, err = plan(f, "erin", "two", twoStage(), nil)
	ok(t, err)
	err = inTx(f, "erin", func(ctx context.Context, tx pgx.Tx) error { return setState(ctx, tx, id, "APPLIED") })
	wantState(t, err, sqlBadState)
}

func TestClosedChangeSetsAreTerminalAndRejectionNeedsAReason(t *testing.T) {
	f := registrytest.New(t)
	id, err := plan(f, "erin", "ledger", twoStage(), nil)
	ok(t, err)
	wantState(t, f.Exec("erin", `UPDATE eacp.change_sets SET state = 'REJECTED' WHERE id = $1`, id), sqlCheck)
	ok(t, f.Exec("rita", `UPDATE eacp.change_sets SET state = 'REJECTED', close_reason = 'not now' WHERE id = $1`, id))
	for _, to := range []string{"PLANNED", "SUBMITTED", "APPLIED", "SUPERSEDED"} {
		wantState(t, f.Exec("erin", `UPDATE eacp.change_sets SET state = $2 WHERE id = $1`, id, to), sqlBadState)
	}
	if n := auditCount(t, f, "change_set.rejected"); n != 1 {
		t.Fatalf("rejected events = %d", n)
	}
}

func TestBundleResourcesAreWrittenOnlyByTheExecutingChangeSet(t *testing.T) {
	f := registrytest.New(t)
	conn := f.ActiveTool(t, "erp", "po").Connector
	steps := []rawStep{{"connector.erp", "import", "submit"}}
	id, err := plan(f, "erin", "erp", steps, []rawRef{{"connector", conn}})
	ok(t, err)
	manage := `INSERT INTO eacp.bundle_resources (tenant_id, bundle_id, address, kind, object_id, change_set_id)
		SELECT eacp.current_tenant_id(), bundle_id, 'connector.erp', 'connector', $2, id
		FROM eacp.change_sets WHERE id = $1`
	wantState(t, f.Exec("erin", manage, id, conn), sqlBadState) // not executing

	// importAndManage applies change set cs (its one import step produces
	// conn) and tries to manage object as connector.erp, in one transaction.
	importAndManage := func(cs, object uuid.UUID) error {
		return inTx(f, "erin", func(ctx context.Context, tx pgx.Tx) error {
			if err := setState(ctx, tx, cs, "APPLIED"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE eacp.change_set_steps SET object_id = $2
				WHERE change_set_id = $1 AND ordinal = 1`, cs, conn); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, manage, cs, object)
			return err
		})
	}
	wantState(t, importAndManage(id, uuid.New()), sqlBadState) // no step produced that object
	ok(t, importAndManage(id, conn))

	// An object belongs to at most one bundle.
	other, err := plan(f, "erin", "erp-other", steps, []rawRef{{"connector", conn}})
	ok(t, err)
	wantState(t, importAndManage(other, conn), sqlUnique)
}

func TestChangeSetsAreTenantIsolated(t *testing.T) {
	f := registrytest.New(t)
	other := f.ForTenant(t, pgtest.TenantB)
	_, err := plan(f, "erin", "ledger", twoStage(), nil)
	ok(t, err)
	var n int
	ok(t, inTx(other, "erin", func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM eacp.change_sets) + (SELECT count(*) FROM eacp.bundles)
			+ (SELECT count(*) FROM eacp.change_set_steps)`).Scan(&n)
	}))
	if n != 0 {
		t.Fatalf("tenant B sees %d change-set rows", n)
	}
	_, err = plan(other, "erin", "ledger", twoStage(), nil) // same bundle name, other tenant
	ok(t, err)
}
