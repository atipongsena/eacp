package storage_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atipongsena/eacp/internal/storage"
	"github.com/atipongsena/eacp/internal/storage/pgtest"
	"github.com/atipongsena/eacp/migrations"
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

// catalog renders every schema object a migration may create or replace,
// so that a Down section can be compared with the schema it must restore.
func catalog(t *testing.T, dsn string) string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var out string
	err = conn.QueryRow(ctx, `SELECT concat_ws(E'\n----\n',
		(SELECT string_agg(p.oid::regprocedure::text || E'\n' || pg_get_functiondef(p.oid) || coalesce(p.proacl::text, ''),
		        E'\n' ORDER BY p.oid::regprocedure::text)
		   FROM pg_proc p WHERE p.pronamespace = 'eacp'::regnamespace),
		(SELECT string_agg(concat_ws(' ', table_name, column_name, data_type, is_nullable, column_default),
		        E'\n' ORDER BY table_name, column_name)
		   FROM information_schema.columns WHERE table_schema = 'eacp'),
		(SELECT string_agg(conrelid::regclass::text || ' ' || conname || ' ' || pg_get_constraintdef(oid),
		        E'\n' ORDER BY conrelid::regclass::text, conname)
		   FROM pg_constraint WHERE connamespace = 'eacp'::regnamespace),
		(SELECT string_agg(indexdef, E'\n' ORDER BY indexname) FROM pg_indexes WHERE schemaname = 'eacp'),
		(SELECT string_agg(tgrelid::regclass::text || ' ' || pg_get_triggerdef(oid) || ' ' || tgenabled::text,
		        E'\n' ORDER BY tgrelid::regclass::text, tgname)
		   FROM pg_trigger WHERE NOT tgisinternal AND tgrelid::regclass::text LIKE 'eacp.%'),
		(SELECT string_agg(concat_ws(' ', tablename, policyname, permissive, cmd, roles::text, qual, with_check),
		        E'\n' ORDER BY tablename, policyname)
		   FROM pg_policies WHERE schemaname = 'eacp'),
		(SELECT string_agg(relname || ' ' || relrowsecurity || ' ' || relforcerowsecurity || ' ' || coalesce(relacl::text, ''),
		        E'\n' ORDER BY relname)
		   FROM pg_class WHERE relnamespace = 'eacp'::regnamespace AND relkind IN ('r', 'v', 'S')),
		(SELECT string_agg(concat_ws(' ', grantee, table_name, column_name, privilege_type),
		        E'\n' ORDER BY grantee, table_name, column_name, privilege_type)
		   FROM information_schema.column_privileges WHERE table_schema = 'eacp'))`).Scan(&out)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Each Down section restores exactly the schema of the previous version.
func TestEveryDownMigrationRestoresThePreviousSchema(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	for v := int64(2); v <= migrations.Latest(); v++ { // 1 → 0 is TestMigrationsRoundTrip
		if err := storage.MigrateTo(ctx, db.OwnerDSN, v-1); err != nil {
			t.Fatalf("to %d: %v", v-1, err)
		}
		before := catalog(t, db.AdminDSN)
		if err := storage.MigrateTo(ctx, db.OwnerDSN, v); err != nil {
			t.Fatalf("up to %d: %v", v, err)
		}
		if err := storage.MigrateTo(ctx, db.OwnerDSN, v-1); err != nil {
			t.Fatalf("down from %d: %v", v, err)
		}
		if after := catalog(t, db.AdminDSN); after != before {
			t.Errorf("migration %d: Down does not restore version %d:\n%s", v, v-1, firstDiff(before, after))
		}
		if err := storage.MigrateTo(ctx, db.OwnerDSN, v); err != nil {
			t.Fatalf("re-up to %d: %v", v, err)
		}
	}
}

func firstDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := range max(len(al), len(bl)) {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			return fmt.Sprintf("line %d\n  before: %s\n  after:  %s", i+1, x, y)
		}
	}
	return ""
}

// InTenantSnapshotTx reads one snapshot and may write: a row committed by
// another transaction after its first statement stays invisible.
func TestInTenantSnapshotTxReadsOneSnapshotAndWrites(t *testing.T) {
	db := pgtest.Migrated(t)
	ctx := context.Background()
	app := pgtest.Pool(t, db.AppDSN)
	owner := pgtest.Pool(t, db.OwnerDSN)
	err := storage.InTenantSnapshotTx(ctx, app, pgtest.TenantA, func(tx pgx.Tx) error {
		var before int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM eacp.principals`).Scan(&before); err != nil {
			return err
		}
		if err := storage.InTenantTx(ctx, owner, pgtest.TenantA, func(o pgx.Tx) error {
			_, err := o.Exec(ctx, `INSERT INTO eacp.principals (tenant_id, kind, name, display_name)
				VALUES (eacp.current_tenant_id(), 'service', 'late-svc', 'late')`)
			return err
		}); err != nil {
			return err
		}
		var after int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM eacp.principals`).Scan(&after); err != nil {
			return err
		}
		if after != before {
			t.Errorf("snapshot saw %d then %d principals", before, after)
		}
		var readOnly string
		if err := tx.QueryRow(ctx, `SHOW transaction_read_only`).Scan(&readOnly); err != nil {
			return err
		}
		if readOnly != "off" {
			t.Errorf("transaction_read_only = %s", readOnly)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
