package storage_test

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"eacp/internal/storage"
	"eacp/internal/storage/pgtest"
	"eacp/migrations"
)

const (
	tenantA = "0b6f0c3e-4a8e-4d7a-9a51-0000000000a1"
	tenantB = "0b6f0c3e-4a8e-4d7a-9a51-0000000000b2"
)

// migratedDB returns a fresh database with all migrations applied and two
// tenants seeded under their own tenant contexts, including their audited
// policy pointers.
func migratedDB(t *testing.T) pgtest.DB {
	t.Helper()
	return pgtest.Migrated(t)
}

func appPool(t *testing.T, dsn string, maxConns int32) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = maxConns
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func tenantSlugs(t *testing.T, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) []string {
	t.Helper()
	rows, err := q.Query(context.Background(), "SELECT slug FROM eacp.tenants ORDER BY slug")
	if err != nil {
		t.Fatalf("query tenants: %v", err)
	}
	slugs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	return slugs
}

func TestMigrationsRoundTrip(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	if err := storage.MigrateUp(ctx, db.OwnerDSN); err != nil {
		t.Fatalf("up: %v", err)
	}
	if err := storage.MigrateDownAll(ctx, db.OwnerDSN); err != nil {
		t.Fatalf("down: %v", err)
	}
	if err := storage.MigrateUp(ctx, db.OwnerDSN); err != nil {
		t.Fatalf("up again: %v", err)
	}
	pool := appPool(t, db.AppDSN, 2)
	if err := storage.CheckSchemaVersion(ctx, pool, migrations.Latest()); err != nil {
		t.Fatalf("CheckSchemaVersion after migrate: %v", err)
	}
}

func TestCheckSchemaVersionFailsWhenDatabaseIsBehind(t *testing.T) {
	db := migratedDB(t)
	pool := appPool(t, db.AppDSN, 2)
	if err := storage.CheckSchemaVersion(context.Background(), pool, migrations.Latest()+1); err == nil {
		t.Fatal("CheckSchemaVersion accepted a database older than the binary")
	}
}

func TestAppRolePassesSafetyCheckButSuperuserFails(t *testing.T) {
	db := migratedDB(t)
	ctx := context.Background()
	if err := storage.CheckRoleSafety(ctx, appPool(t, db.AppDSN, 1)); err != nil {
		t.Fatalf("app role rejected: %v", err)
	}
	if err := storage.CheckRoleSafety(ctx, appPool(t, db.AdminDSN, 1)); err == nil {
		t.Fatal("superuser connection passed the role safety check; RLS would be bypassed")
	}
}

// A login role that is a member of a BYPASSRLS role could SET ROLE to it
// later, so membership alone must fail the check.
func TestRoleSafetyRejectsMembershipInBypassRole(t *testing.T) {
	db := migratedDB(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, db.AdminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer admin.Close(ctx)

	bypass, member := db.Name+"_bypass", db.Name+"_member"
	for _, stmt := range []string{
		"CREATE ROLE " + bypass + " NOLOGIN BYPASSRLS",
		"CREATE ROLE " + member + " LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD 'member_pw'",
		"GRANT " + bypass + " TO " + member,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), db.AdminDSN)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), "DROP ROLE IF EXISTS "+member)
		_, _ = c.Exec(context.Background(), "DROP ROLE IF EXISTS "+bypass)
	})

	u, _ := url.Parse(db.AppDSN)
	u.User = url.UserPassword(member, "member_pw")
	if err := storage.CheckRoleSafety(ctx, appPool(t, u.String(), 1)); err == nil {
		t.Fatal("role with membership in a BYPASSRLS role passed the safety check")
	}
}

// The schema owner can ALTER TABLE ... NO FORCE ROW LEVEL SECURITY or
// disable the registry triggers, so services must never run as the owner or
// as any role that can become it.
func TestRoleSafetyRejectsSchemaOwnerAndItsMembers(t *testing.T) {
	db := migratedDB(t)
	ctx := context.Background()
	if err := storage.CheckRoleSafety(ctx, appPool(t, db.OwnerDSN, 1)); err == nil {
		t.Fatal("schema owner passed the role safety check")
	}

	admin, err := pgx.Connect(ctx, db.AdminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer admin.Close(ctx)
	member := db.Name + "_owner_member"
	for _, stmt := range []string{
		"CREATE ROLE " + member + " LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD 'member_pw'",
		"GRANT " + pgtest.OwnerRole + " TO " + member,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), db.AdminDSN)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), "DROP ROLE IF EXISTS "+member)
	})
	u, _ := url.Parse(db.AppDSN)
	u.User = url.UserPassword(member, "member_pw")
	if err := storage.CheckRoleSafety(ctx, appPool(t, u.String(), 1)); err == nil {
		t.Fatal("member of the schema owner role passed the safety check")
	}
}

// A superuser session that SET ROLEs to eacp_app looks safe by current_user
// but can RESET ROLE at any time; session_user must be checked too.
func TestRoleSafetyRejectsSuperuserSessionUsingSetRole(t *testing.T) {
	db := migratedDB(t)
	cfg, err := pgxpool.ParseConfig(db.AdminDSN)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE "+pgtest.AppRole)
		return err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	if err := storage.CheckRoleSafety(context.Background(), pool); err == nil {
		t.Fatal("superuser session with SET ROLE eacp_app passed the safety check")
	}
}

func TestTenantSeesOnlyItsOwnRows(t *testing.T) {
	db := migratedDB(t)
	pool := appPool(t, db.AppDSN, 2)
	var got []string
	err := storage.InTenantTx(context.Background(), pool, tenantA, func(tx pgx.Tx) error {
		got = tenantSlugs(t, tx)
		return nil
	})
	if err != nil {
		t.Fatalf("InTenantTx: %v", err)
	}
	if len(got) != 1 || got[0] != "tenant-a" {
		t.Fatalf("tenant A saw %v, want [tenant-a]", got)
	}
}

func TestMissingTenantContextSeesNothing(t *testing.T) {
	db := migratedDB(t)
	if got := tenantSlugs(t, appPool(t, db.AppDSN, 2)); len(got) != 0 {
		t.Fatalf("query without tenant context saw %v, want no rows (fail closed)", got)
	}
}

func TestTenantContextIsTransactionLocal(t *testing.T) {
	db := migratedDB(t)
	pool := appPool(t, db.AppDSN, 1) // one connection: the next query reuses it
	if err := storage.InTenantTx(context.Background(), pool, tenantA, func(pgx.Tx) error { return nil }); err != nil {
		t.Fatalf("InTenantTx: %v", err)
	}
	if got := tenantSlugs(t, pool); len(got) != 0 {
		t.Fatalf("tenant context leaked past the transaction: saw %v", got)
	}
}

func TestInTenantTxRejectsInvalidTenantIDs(t *testing.T) {
	db := migratedDB(t)
	pool := appPool(t, db.AppDSN, 1)
	for _, id := range []string{"", "not-a-uuid", tenantA + "' OR '1'='1"} {
		called := false
		err := storage.InTenantTx(context.Background(), pool, id, func(pgx.Tx) error { called = true; return nil })
		if err == nil || called {
			t.Fatalf("InTenantTx(%q) err=%v called=%v, want rejection before running fn", id, err, called)
		}
	}
}

func TestInTenantTxRollsBackOnError(t *testing.T) {
	db := migratedDB(t)
	ctx := context.Background()
	pool := appPool(t, db.AppDSN, 1)
	sentinel := errors.New("boom")
	err := storage.InTenantTx(ctx, pool, tenantA, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "CREATE TEMP TABLE rollback_probe (x int)"); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	var exists bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('pg_temp.rollback_probe') IS NOT NULL").Scan(&exists); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if exists {
		t.Fatal("transaction was committed despite fn returning an error")
	}
}

func TestAppRoleCannotCreateTenants(t *testing.T) {
	db := migratedDB(t)
	ctx := context.Background()
	err := storage.InTenantTx(ctx, appPool(t, db.AppDSN, 1), tenantA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO eacp.tenants (id, slug, display_name) VALUES ($1, 'x', 'x')`, tenantA)
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" { // insufficient_privilege
		t.Fatalf("err = %v, want permission denied (42501)", err)
	}
}
