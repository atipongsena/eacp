// Package storage owns PostgreSQL access: connection pools, tenant-scoped
// transactions, safety checks and schema migrations.
//
// PostgreSQL is the single source of truth for execution state (ADR-014),
// and tenant isolation is enforced by Row-Level Security (ADR-021).
package storage

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"eacp/migrations"
)

// Open creates a connection pool and verifies connectivity.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("storage: ping: %w", err)
	}
	return pool, nil
}

// CheckRoleSafety fails if the connection can bypass Row-Level Security,
// now or later: the session user or current user is a superuser or has
// BYPASSRLS, or either is a member of any such role (and could SET ROLE to
// it). Services call it at startup and refuse to run, because such a
// connection would silently disable tenant isolation.
func CheckRoleSafety(ctx context.Context, pool *pgxpool.Pool) error {
	var sessionUser, currentUser string
	var unsafe []string
	err := pool.QueryRow(ctx, `
		SELECT session_user, current_user,
		       COALESCE(array_agg(r.rolname ORDER BY r.rolname)
		                FILTER (WHERE r.rolname IS NOT NULL), '{}')
		FROM (SELECT 1) AS one
		LEFT JOIN pg_roles r
		  ON (r.rolsuper OR r.rolbypassrls)
		 AND (pg_has_role(session_user, r.oid, 'MEMBER')
		   OR pg_has_role(current_user, r.oid, 'MEMBER'))
		GROUP BY session_user, current_user`,
	).Scan(&sessionUser, &currentUser, &unsafe)
	if err != nil {
		return fmt.Errorf("storage: role safety check: %w", err)
	}
	if len(unsafe) > 0 {
		return fmt.Errorf("storage: session_user %q / current_user %q can bypass row-level security via roles %v; refusing to run",
			sessionUser, currentUser, unsafe)
	}
	return nil
}

// CheckSchemaVersion fails unless the database has at least migration
// version want applied. A newer database is accepted (rolling deploys).
func CheckSchemaVersion(ctx context.Context, pool *pgxpool.Pool, want int64) error {
	var got int64
	err := pool.QueryRow(ctx,
		`SELECT COALESCE(max(version_id), 0) FROM public.goose_db_version WHERE is_applied`,
	).Scan(&got)
	if err != nil {
		return fmt.Errorf("storage: schema version: %w", err)
	}
	if got < want {
		return fmt.Errorf("storage: schema version %d is behind required %d", got, want)
	}
	return nil
}

// InTenantTx runs fn in a transaction whose Row-Level Security context is
// tenantID. The setting is transaction-local (set_config(..., true)), so it
// cannot leak to later work on the same pooled connection. The transaction
// is committed only if fn returns nil.
func InTenantTx(ctx context.Context, pool *pgxpool.Pool, tenantID string, fn func(pgx.Tx) error) error {
	id, err := uuid.Parse(tenantID)
	if err != nil || id == uuid.Nil {
		return fmt.Errorf("storage: invalid tenant id %q", tenantID)
	}
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, id.String()); err != nil {
			return fmt.Errorf("storage: set tenant context: %w", err)
		}
		return fn(tx)
	})
}

// MigrateUp applies all pending migrations. It must run as the schema owner.
// Concurrent runs are serialised with a PostgreSQL advisory lock.
func MigrateUp(ctx context.Context, ownerDSN string) error {
	return withProvider(ctx, ownerDSN, func(p *goose.Provider) error {
		_, err := p.Up(ctx)
		return err
	})
}

// MigrateDownAll rolls back every migration. Intended for tests and local
// development only.
func MigrateDownAll(ctx context.Context, ownerDSN string) error {
	return withProvider(ctx, ownerDSN, func(p *goose.Provider) error {
		_, err := p.DownTo(ctx, 0)
		return err
	})
}

// MigrationStatus reports the current and latest embedded versions.
func MigrationStatus(ctx context.Context, ownerDSN string) (current, latest int64, err error) {
	err = withProvider(ctx, ownerDSN, func(p *goose.Provider) error {
		current, err = p.GetDBVersion(ctx)
		return err
	})
	return current, migrations.Latest(), err
}

func withProvider(ctx context.Context, dsn string, fn func(*goose.Provider) error) error {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("storage: parse dsn: %w", err)
	}
	db := stdlib.OpenDB(*cfg)
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("storage: connect for migrations: %w", err)
	}

	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("storage: migration lock: %w", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS, goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("storage: migration provider: %w", err)
	}
	if err := fn(p); err != nil {
		return fmt.Errorf("storage: migrate: %w", err)
	}
	return nil
}
