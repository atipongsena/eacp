package bundle_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/atipongsena/eacp/internal/registry/registrytest"
)

const allowPolicy = `{"format_version":1,"rules":[{"id":"all","verdict":"allow","reason":"permitted"}]}`

func TestPhase21StepKindsAndTheSetOp(t *testing.T) {
	f := registrytest.New(t)
	steps := []rawStep{{"principal.dana", "create", "submit"}, {"group.ops", "create", "submit"},
		{"member.ops.dana", "create", "submit"}, {"role.dana.auditor", "propose", "submit"},
		{"policy.tenant", "create", "submit"}, {"budget.ops", "set", "submit"}, {"price.gpt", "create", "submit"},
		{"role.dana.auditor", "activate", "approve"}}
	_, err := plan(f, "alice", "people", steps, []rawRef{{"principal", f.P["carol"]}, {"policy", f.Tenant},
		{"member", uuid.New()}, {"role", uuid.New()}, {"budget", uuid.New()}, {"price", uuid.New()}})
	ok(t, err)
	_, err = plan(f, "alice", "bad-kind", []rawStep{{"secret.x", "create", "submit"}}, nil)
	wantState(t, err, sqlCheck)
	_, err = plan(f, "alice", "bad-op", []rawStep{{"budget.x", "delete", "submit"}}, nil)
	wantState(t, err, sqlCheck)
	_, err = plan(f, "alice", "bad-ref", []rawStep{{"budget.x", "set", "submit"}}, []rawRef{{"secret", uuid.New()}})
	wantState(t, err, sqlCheck)
}

func TestOnlyOneBundleOwnsTheTenantPolicy(t *testing.T) {
	f := registrytest.New(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, f.DB.AdminDSN)
	ok(t, err)
	defer admin.Close(ctx)
	// Triggers and foreign keys off: only the index is under test.
	_, err = admin.Exec(ctx, `SET session_replication_role = replica`)
	ok(t, err)
	insert := func() error {
		_, err := admin.Exec(ctx, `INSERT INTO eacp.bundle_resources
			(tenant_id, bundle_id, address, kind, object_id, change_set_id, managed_at)
			VALUES ($1, $2, 'policy.tenant', 'policy', $3, $4, now())`, f.Tenant, uuid.New(), uuid.New(), uuid.New())
		return err
	}
	ok(t, insert())
	wantState(t, insert(), sqlUnique)
}

func TestAStageThatRevokedAnAdminCannotLeaveFewerThanTwo(t *testing.T) {
	f := registrytest.New(t)
	steps := []rawStep{{"principal.alice", "import", "submit"}, {"role.alice.admin", "revoke", "approve"}}
	id, err := plan(f, "alice", "admins", steps, nil)
	ok(t, err)
	ok(t, inTx(f, "alice", func(ctx context.Context, tx pgx.Tx) error {
		if err := setState(ctx, tx, id, "SUBMITTED"); err != nil {
			return err
		}
		if err := runSteps(ctx, tx, id, 1); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT eacp.change_set_seal($1)`, id)
		return err
	}))
	approve := func() error {
		return inTx(f, "bob", func(ctx context.Context, tx pgx.Tx) error {
			if err := setState(ctx, tx, id, "APPLIED"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE eacp.role_grants SET revoked_at = now(), revoke_reason = 'prune'
				WHERE principal_id = $1 AND role = 'admin' AND revoked_at IS NULL`, f.P["alice"]); err != nil {
				return err
			}
			return runSteps(ctx, tx, id, 2)
		})
	}
	err = approve()
	wantState(t, err, sqlCheck)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Hint != "admin_floor" {
		t.Fatalf("admin floor error = %v", err)
	}
	// With a third admin, the same stage commits and two remain.
	grant := f.ID(t, "alice", `INSERT INTO eacp.role_grants (tenant_id, principal_id, role)
		VALUES (eacp.current_tenant_id(), $1, 'admin') RETURNING id`, f.P["carol"])
	ok(t, f.Exec("bob", `UPDATE eacp.role_grants SET approved_at = now() WHERE id = $1`, grant))
	ok(t, approve())
}

func refRow(t *testing.T, f *registrytest.Fixture, kind string, id uuid.UUID) string {
	t.Helper()
	var s string
	ok(t, inTx(f, "alice", func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT eacp.change_set_ref_row($1, $2)`, kind, id).Scan(&s)
	}))
	return s
}

func TestABudgetRefIgnoresItsCounters(t *testing.T) {
	f := registrytest.New(t)
	raise := func(acct uuid.UUID, limit string) {
		change := f.ID(t, "alice", `INSERT INTO eacp.budget_limit_changes (tenant_id, account_id, new_limit, reason)
			VALUES (eacp.current_tenant_id(), $1, $2::numeric, 'fund') RETURNING id`, acct, limit)
		ok(t, f.Exec("bob", `UPDATE eacp.budget_limit_changes SET state = 'APPLIED', decision_reason = 'ok'
			WHERE id = $1`, change))
	}
	root := f.ID(t, "alice", `INSERT INTO eacp.budget_accounts (tenant_id, name, unit)
		VALUES (eacp.current_tenant_id(), 'root', 'USD') RETURNING id`)
	raise(root, "100")
	before := refRow(t, f, "budget", root)
	team := f.ID(t, "alice", `INSERT INTO eacp.budget_accounts (tenant_id, name, unit, parent_id)
		VALUES (eacp.current_tenant_id(), 'team', 'USD', $1) RETURNING id`, root)
	raise(team, "40") // escrowed from root: root's allocated counter moves
	if after := refRow(t, f, "budget", root); after != before {
		t.Fatalf("a counter changed the ref row:\n%s\n%s", before, after)
	}
	f.ID(t, "alice", `INSERT INTO eacp.budget_limit_changes (tenant_id, account_id, new_limit, reason)
		VALUES (eacp.current_tenant_id(), $1, 50, 'trim') RETURNING id`, root)
	if refRow(t, f, "budget", root) == before {
		t.Fatal("a limit change must change the ref row")
	}
}

func TestAPolicyRefFollowsTheTenantPointer(t *testing.T) {
	f := registrytest.New(t)
	if refRow(t, f, "policy", uuid.New()) != "absent" {
		t.Fatal("an unknown policy is absent")
	}
	before := refRow(t, f, "policy", f.Tenant)
	if before == "absent" {
		t.Fatal("the tenant's policy ref exists before any policy")
	}
	pol := f.ID(t, "alice", `INSERT INTO eacp.policy_bundles (tenant_id, content)
		VALUES (eacp.current_tenant_id(), $1::jsonb) RETURNING id`, allowPolicy)
	ok(t, f.Exec("bob", `UPDATE eacp.tenant_policy_pointer SET current_bundle_id = $1, activation_reason = 'go'
		WHERE tenant_id = eacp.current_tenant_id()`, pol))
	if refRow(t, f, "policy", f.Tenant) == before {
		t.Fatal("activating a policy must change the tenant's policy ref")
	}
	if refRow(t, f, "policy", pol) == "absent" {
		t.Fatal("a policy version is present")
	}
}
