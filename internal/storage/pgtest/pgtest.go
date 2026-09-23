// Package pgtest provisions throwaway PostgreSQL databases for integration
// tests. Tests are skipped unless EACP_TEST_ADMIN_DSN points at a superuser
// connection (for example the docker-compose postgres service).
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"eacp/internal/storage"
)

// Role names. Roles are cluster-wide, so tests share them with the local
// docker-compose environment; their passwords default to the compose
// development values and can be overridden with EACP_TEST_OWNER_PASSWORD and
// EACP_TEST_APP_PASSWORD.
const (
	OwnerRole = "eacp_owner"
	AppRole   = "eacp_app"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// DB is a freshly created, empty database.
type DB struct {
	Name     string
	AdminDSN string // superuser; bypasses RLS — for seeding and assertions only
	OwnerDSN string // schema owner; runs migrations
	AppDSN   string // application role: NOSUPERUSER NOBYPASSRLS
}

// New creates a database owned by OwnerRole and drops it when t finishes.
func New(t testing.TB) DB {
	t.Helper()
	adminDSN := os.Getenv("EACP_TEST_ADMIN_DSN")
	if adminDSN == "" {
		t.Skip("EACP_TEST_ADMIN_DSN not set; skipping PostgreSQL integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("pgtest: connect admin: %v", err)
	}
	defer admin.Close(ctx)

	ownerPassword := envOr("EACP_TEST_OWNER_PASSWORD", "eacp_owner_dev")
	appPassword := envOr("EACP_TEST_APP_PASSWORD", "eacp_app_dev")
	if err := ensureRoles(ctx, admin, ownerPassword, appPassword); err != nil {
		t.Fatalf("pgtest: ensure roles: %v", err)
	}

	name := "eacp_test_" + randomSuffix(t)
	if _, err := admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s OWNER %s", name, OwnerRole)); err != nil {
		t.Fatalf("pgtest: create database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c, err := pgx.Connect(ctx, adminDSN)
		if err != nil {
			t.Errorf("pgtest: cleanup connect: %v", err)
			return
		}
		defer c.Close(ctx)
		if _, err := c.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", name)); err != nil {
			t.Errorf("pgtest: drop database %s: %v", name, err)
		}
	})

	return DB{
		Name:     name,
		AdminDSN: withDatabase(t, adminDSN, name, "", ""),
		OwnerDSN: withDatabase(t, adminDSN, name, OwnerRole, ownerPassword),
		AppDSN:   withDatabase(t, adminDSN, name, AppRole, appPassword),
	}
}

// ensureRoles creates the cluster-wide roles once. Test packages run in
// parallel, so creation is serialised with an advisory lock.
func ensureRoles(ctx context.Context, admin *pgx.Conn, ownerPassword, appPassword string) error {
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock(hashtext('eacp_pgtest_roles'))"); err != nil {
		return err
	}
	defer admin.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock(hashtext('eacp_pgtest_roles'))")

	for _, r := range []struct{ name, password string }{{OwnerRole, ownerPassword}, {AppRole, appPassword}} {
		var exists bool
		if err := admin.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", r.name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		stmt := fmt.Sprintf("CREATE ROLE %s LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE PASSWORD %s",
			r.name, quoteLiteral(r.password))
		if _, err := admin.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// quoteLiteral renders s as a SQL string literal (utility statements such as
// CREATE ROLE cannot take bind parameters).
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func withDatabase(t testing.TB, dsn, db, user, password string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("pgtest: EACP_TEST_ADMIN_DSN must be a URL: %v", err)
	}
	u.Path = "/" + db
	if user != "" {
		u.User = url.UserPassword(user, password)
	}
	return u.String()
}

func randomSuffix(t testing.TB) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("pgtest: random: %v", err)
	}
	return hex.EncodeToString(b)
}

// Seeded tenant ids used by Migrated.
const (
	TenantA = "0b6f0c3e-4a8e-4d7a-9a51-0000000000a1"
	TenantB = "0b6f0c3e-4a8e-4d7a-9a51-0000000000b2"
)

// Migrated returns a fresh database with every migration applied (as the
// schema owner) and tenants TenantA and TenantB seeded by the superuser.
func Migrated(t testing.TB) DB {
	t.Helper()
	db := New(t)
	ctx := context.Background()
	if err := storage.MigrateUp(ctx, db.OwnerDSN); err != nil {
		t.Fatalf("pgtest: migrate: %v", err)
	}
	admin, err := pgx.Connect(ctx, db.AdminDSN)
	if err != nil {
		t.Fatalf("pgtest: connect admin: %v", err)
	}
	defer admin.Close(ctx)
	for _, tenant := range []struct{ id, slug, name string }{
		{TenantA, "tenant-a", "Tenant A"},
		{TenantB, "tenant-b", "Tenant B"},
	} {
		err := pgx.BeginFunc(ctx, admin, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant.id); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO eacp.tenants (id, slug, display_name) VALUES ($1, $2, $3)`,
				tenant.id, tenant.slug, tenant.name)
			return err
		})
		if err != nil {
			t.Fatalf("pgtest: seed tenant %s: %v", tenant.slug, err)
		}
	}
	return db
}

// Pool opens a pool on dsn that is closed when t finishes.
func Pool(t testing.TB, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgtest: pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
