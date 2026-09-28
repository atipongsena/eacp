package governance_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/atipongsena/eacp/internal/registry/registrytest"
	"github.com/atipongsena/eacp/internal/storage"
	"github.com/atipongsena/eacp/internal/storage/pgtest"
	"github.com/atipongsena/eacp/migrations"
)

const simplePolicy = `{"format_version":1,"rules":[{"id":"allow-read","match":{"operation":"read"},"verdict":"allow","reason":"read permitted"}]}`

func sqlState(t *testing.T, err error, want string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != want {
		t.Fatalf("error = %v, want SQLSTATE %s", err, want)
	}
}

func TestPolicyActivationIsTwoPersonAndMonotonic(t *testing.T) {
	f := registrytest.New(t)
	insert := `INSERT INTO eacp.policy_bundles (tenant_id, content)
		VALUES (eacp.current_tenant_id(), $1::jsonb) RETURNING id`
	_, err := f.TryID("carol", insert, simplePolicy)
	sqlState(t, err, "42501")
	v1 := f.ID(t, "alice", insert, simplePolicy)
	activate := `UPDATE eacp.tenant_policy_pointer
		SET current_bundle_id = $1, activation_reason = 'deploy policy'
		WHERE tenant_id = eacp.current_tenant_id()`
	sqlState(t, f.Exec("alice", activate, v1), "42501")
	if err := f.Exec("bob", activate, v1); err != nil {
		t.Fatal(err)
	}
	sqlState(t, f.Exec("bob", activate, v1), "55000")
	v2 := f.ID(t, "bob", insert, simplePolicy)
	sqlState(t, f.Exec("bob", activate, v2), "42501")
	if err := f.Exec("alice", activate, v2); err != nil {
		t.Fatal(err)
	}
	sqlState(t, f.Exec("bob", activate, v1), "55000")
	var version int
	err = storage.InTenantTx(context.Background(), f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT current_version FROM eacp.tenant_policy_pointer
			WHERE tenant_id = eacp.current_tenant_id() FOR SHARE`).Scan(&version)
	})
	if err != nil || version != 2 {
		t.Fatalf("pointer version = %d, err = %v", version, err)
	}
}

func TestPolicyBundleImmutableExceptPermanentRevocation(t *testing.T) {
	f := registrytest.New(t)
	id := f.ID(t, "alice", `INSERT INTO eacp.policy_bundles (tenant_id, content)
		VALUES (eacp.current_tenant_id(), $1::jsonb) RETURNING id`, simplePolicy)
	sqlState(t, f.Exec("alice", `UPDATE eacp.policy_bundles SET content = '{}'::jsonb WHERE id = $1`, id), "42501")
	sqlState(t, f.Exec("alice", `DELETE FROM eacp.policy_bundles WHERE id = $1`, id), "42501")
	if err := f.Exec("bob", `UPDATE eacp.policy_bundles SET revoked_at = now(), revoke_reason = 'unsafe' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	sqlState(t, f.Exec("bob", `UPDATE eacp.policy_bundles SET revoked_at = NULL WHERE id = $1`, id), "55000")
	sqlState(t, f.Exec("bob", `UPDATE eacp.tenant_policy_pointer SET current_bundle_id = $1,
		activation_reason = 'activate revoked' WHERE tenant_id = eacp.current_tenant_id()`, id), "55000")
}

func TestRawSQLRejectsPoliciesTheLocalProviderCannotParse(t *testing.T) {
	f := registrytest.New(t)
	insert := `INSERT INTO eacp.policy_bundles (tenant_id, content)
		VALUES (eacp.current_tenant_id(), $1::jsonb) RETURNING id`
	for _, policy := range []string{
		`{"format_version":1,"rules":[{"id":"r","reason":"x","verdict":"allow","match":{"unknown":"x"}}]}`,
		`{"format_version":1,"rules":[{"id":"r","reason":"x","verdict":"escalate","approval":{"quorum":1,"ttl_seconds":60,"eligible_roles":["approver"],"unknown":true}}]}`,
		`{"format_version":1,"rules":[{"id":"r","reason":"x","verdict":"transform","set":{"":1}}]}`,
		`{"format_version":1,"rules":[{"id":"r","reason":"x","verdict":"transform","set":{"amount":1e21}}]}`,
	} {
		_, err := f.TryID("alice", insert, policy)
		sqlState(t, err, "23514")
	}
}

func TestPolicyPointerTenantIsolation(t *testing.T) {
	f := registrytest.New(t)
	var count int
	err := storage.InTenantTx(context.Background(), f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM eacp.tenant_policy_pointer WHERE tenant_id = $1`,
			uuid.MustParse(pgtest.TenantB)).Scan(&count)
	})
	if err != nil || count != 0 {
		t.Fatalf("cross-tenant pointer count = %d, err = %v", count, err)
	}
}

func TestExistingTenantPolicyPointerBackfillIsAudited(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(db.OwnerDSN)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB := stdlib.OpenDB(*cfg)
	defer sqlDB.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 3); err != nil {
		t.Fatal(err)
	}
	owner, err := pgx.Connect(ctx, db.OwnerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(ctx)
	if err := pgx.BeginFunc(ctx, owner, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, pgtest.TenantA); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO eacp.tenants (id, slug, display_name)
			VALUES ($1, 'legacy', 'Legacy')`, pgtest.TenantA)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	app := pgtest.Pool(t, db.AppDSN)
	if err := storage.InTenantTx(ctx, app, pgtest.TenantA, func(tx pgx.Tx) error {
		var pointers, events int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM eacp.tenant_policy_pointer`).Scan(&pointers); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM eacp.audit_events
			WHERE convert_from(payload, 'UTF8') LIKE '%tenant_policy_pointer.insert%'`).Scan(&events); err != nil {
			return err
		}
		if pointers != 1 || events != 1 {
			t.Errorf("backfill pointers/events = %d/%d, want 1/1", pointers, events)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
