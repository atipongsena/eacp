package worker_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"eacp/internal/action"
	"eacp/internal/approval"
	"eacp/internal/identity"
	"eacp/internal/registry"
	"eacp/internal/storage"
	"eacp/internal/storage/pgtest"
)

// Invariant 8: after a complete flow in tenant A (registry, policy,
// governance, approval, execution, reconciliation, operator resolution,
// journal and outbox), neither tenant B nor a session without a tenant sees
// or changes a single row of any table.
func TestAnotherTenantSeesAndChangesNothingAfterAFullFlow(t *testing.T) {
	v := newERPEnvWith(t, reviewPolicy)
	ctx := context.Background()
	approve := func(a action.View) {
		t.Helper()
		for _, who := range []string{"amy", "ben"} {
			if _, err := approval.New(v.f.App).Vote(ctx, registry.Actor{TenantID: v.f.Tenant, PrincipalID: v.f.P[who]},
				*a.ApprovalRequestID, approval.Approve, "reviewed"); err != nil {
				t.Fatal(err)
			}
		}
	}
	found := v.submitTo("PENDING_APPROVAL", "create_po", map[string]any{"scenario": "execute_then_timeout", "delay_ms": 300})
	hidden := v.submitTo("PENDING_APPROVAL", "create_po_eventual", map[string]any{"scenario": "execute_then_timeout",
		"delay_ms": 300, "visibility_delay_ms": 5000})
	approve(found)
	approve(hidden)
	v.sweep()
	v.runWorker(2)
	if got, _ := v.reconcile(v.reconciler(1, 100*time.Millisecond), found.ID); got.State != "SUCCEEDED" {
		t.Fatalf("found = %s", got.State)
	}
	if got, _ := v.reconcile(v.reconciler(1, 100*time.Millisecond), hidden.ID); got.State != "NEEDS_HUMAN_RESOLUTION" {
		t.Fatalf("hidden = %s", got.State)
	}
	if _, _, err := v.e.Resolve(ctx, action.Principal(v.f.Tenant, v.f.P["otto"]), hidden.ID, action.Resolution{
		Outcome: "succeeded", Reason: "ERP back office shows the PO", Evidence: "ERP search",
		ExternalReference: "PO-" + hidden.ID.String()}); err != nil {
		t.Fatal(err)
	}

	// Registry records the action flow does not touch: a group with a member
	// and an approved principal key.
	g := v.f.ID(t, "alice", `INSERT INTO eacp.groups (tenant_id, name, display_name)
		VALUES (eacp.current_tenant_id(), 'buyers', 'Buyers') RETURNING id`)
	v.f.ID(t, "alice", `INSERT INTO eacp.group_memberships (tenant_id, group_id, principal_id)
		VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, g, v.f.P["carol"])
	credID := uuid.New()
	_, hash, err := identity.NewKey(identity.KindPrincipal, v.f.Tenant, credID)
	if err != nil {
		t.Fatal(err)
	}
	v.f.ID(t, "alice", `INSERT INTO eacp.credentials (tenant_id, id, kind, principal_id, secret_hash, expires_at)
		VALUES (eacp.current_tenant_id(), $1, 'pk', $2, $3, now() + interval '1 day') RETURNING id`, credID, v.f.P["carol"], hash)
	if err := v.f.Exec("bob", `UPDATE eacp.credentials SET approved_at = now() WHERE id = $1`, credID); err != nil {
		t.Fatal(err)
	}

	admin, err := pgx.Connect(ctx, v.f.DB.AdminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	rows, err := admin.Query(ctx, `SELECT relname FROM pg_class
		WHERE relnamespace = 'eacp'::regnamespace AND relkind IN ('r', 'p') ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	// ref names table and the column holding its tenant.
	ref := func(table string) (name, key string) {
		if table == "tenants" {
			return "eacp.tenants", "id"
		}
		return "eacp." + pgx.Identifier{table}.Sanitize(), "tenant_id"
	}
	ofA := " = '" + pgtest.TenantA + "'"
	// count counts tenant A's rows of table as seen from tenant ("" for none).
	count := func(tenant, table string) (n int) {
		t.Helper()
		name, key := ref(table)
		sql := "SELECT count(*) FROM " + name + " WHERE " + key + ofA
		if tenant == "" {
			if err := v.f.App.QueryRow(ctx, sql).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		if err := storage.InTenantTx(ctx, v.f.App, tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, sql).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// change runs sql as tenant B and returns the rows it changed; a refusal
	// by privileges or policy changes nothing.
	change := func(sql string) int64 {
		t.Helper()
		var n int64
		err := storage.InTenantTx(ctx, v.f.App, pgtest.TenantB, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, sql)
			n = tag.RowsAffected()
			return err
		})
		var pg *pgconn.PgError
		if err != nil && !(errors.As(err, &pg) && pg.Code == "42501") {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}

	var empty []string
	for _, table := range tables {
		inA := count(pgtest.TenantA, table)
		if inA == 0 {
			empty = append(empty, table)
		}
		if inB, none := count(pgtest.TenantB, table), count("", table); inB != 0 || none != 0 {
			t.Errorf("%s: tenant A sees %d rows, tenant B %d, no tenant %d", table, inA, inB, none)
		}
		name, key := ref(table)
		if n := change("UPDATE " + name + " SET " + key + " = " + key + " WHERE " + key + ofA); n != 0 {
			t.Errorf("tenant B updated %d rows of %s", n, table)
		}
		if n := change("DELETE FROM " + name + " WHERE " + key + ofA); n != 0 {
			t.Errorf("tenant B deleted %d rows of %s", n, table)
		}
		if after := count(pgtest.TenantA, table); after != inA {
			t.Errorf("%s: tenant A had %d rows, now %d", table, inA, after)
		}
	}
	// The flow touched every table, so the sweep above is not vacuous.
	if len(empty) != 0 {
		t.Fatalf("tables without tenant A rows: %v", empty)
	}
}
