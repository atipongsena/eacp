// Package storage owns PostgreSQL access: connection pools, tenant-scoped
// transactions, safety checks and schema migrations.
//
// PostgreSQL is the single source of truth for execution state (ADR-014),
// and tenant isolation is enforced by Row-Level Security (ADR-021).
package storage

import (
	"context"
	"fmt"
	"strconv"

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

// CheckRoleSafety fails if the connection can bypass Row-Level Security or
// the registry triggers, now or later: the session user or current user is a
// superuser, has BYPASSRLS, owns the eacp schema (and so could ALTER its
// tables), or is a member of any such role (and could SET ROLE to it).
// Services call it at startup and refuse to run, because such a connection
// would silently disable tenant isolation and the database-enforced rules.
func CheckRoleSafety(ctx context.Context, pool *pgxpool.Pool) error {
	var sessionUser, currentUser string
	var unsafe []string
	err := pool.QueryRow(ctx, `
		SELECT session_user, current_user,
		       COALESCE(array_agg(r.rolname ORDER BY r.rolname)
		                FILTER (WHERE r.rolname IS NOT NULL), '{}')
		FROM (SELECT 1) AS one
		LEFT JOIN pg_roles r
		  ON (r.rolsuper OR r.rolbypassrls
		      OR r.oid = (SELECT nspowner FROM pg_namespace WHERE nspname = 'eacp'))
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

// InTenantReadTx is InTenantTx with a read-only REPEATABLE READ transaction:
// every statement in fn sees the same snapshot. Use it for checks that read
// several related rows and must not see a half-committed picture (for
// example audit.Verify).
func InTenantReadTx(ctx context.Context, pool *pgxpool.Pool, tenantID string, fn func(pgx.Tx) error) error {
	id, err := uuid.Parse(tenantID)
	if err != nil || id == uuid.Nil {
		return fmt.Errorf("storage: invalid tenant id %q", tenantID)
	}
	opts := pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
	return pgx.BeginTxFunc(ctx, pool, opts, func(tx pgx.Tx) error {
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

// SetActor records the principal performing the transaction's changes. It is
// transaction-local, like the tenant context. Registry triggers read it
// (eacp.current_actor_id()) to attribute and authorise every change, instead
// of trusting *_by columns written by the client (ADR-003 §8).
func SetActor(ctx context.Context, tx pgx.Tx, actorID uuid.UUID) error {
	if actorID == uuid.Nil {
		return fmt.Errorf("storage: nil actor id")
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.actor_id', $1, true)`, actorID.String()); err != nil {
		return fmt.Errorf("storage: set actor: %w", err)
	}
	return nil
}

// SetAgent records that an authenticated agent version performs the
// transaction's action changes (migration 00005, eacp.actor_context). Call it
// only after the agent's API key has been authenticated. It never grants
// registry or policy rights: eacp.actor() still requires a principal.
func SetAgent(ctx context.Context, tx pgx.Tx, agentVersionID uuid.UUID) error {
	if agentVersionID == uuid.Nil {
		return fmt.Errorf("storage: nil agent version id")
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.agent_version_id', $1, true)`, agentVersionID.String()); err != nil {
		return fmt.Errorf("storage: set agent: %w", err)
	}
	return nil
}

// SetSystem records that a named control-plane component (for example the
// sweeper) performs the transaction's action changes.
func SetSystem(ctx context.Context, tx pgx.Tx, component string) error {
	if component == "" {
		return fmt.Errorf("storage: empty system component")
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.system_actor', $1, true)`, component); err != nil {
		return fmt.Errorf("storage: set system actor: %w", err)
	}
	return nil
}

// SetWorker records that execution worker workerID, holding (or claiming)
// lease generation generation, performs the transaction's action changes.
// PostgreSQL rejects any worker write whose generation is not the action's
// current one (migration 00006), so a stale worker cannot commit.
func SetWorker(ctx context.Context, tx pgx.Tx, workerID string, generation int64) error {
	if workerID == "" || generation < 1 {
		return fmt.Errorf("storage: worker id and a positive lease generation are required")
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.system_actor', 'worker', true),
		set_config('app.worker_id', $1, true), set_config('app.lease_generation', $2, true)`,
		workerID, strconv.FormatInt(generation, 10)); err != nil {
		return fmt.Errorf("storage: set worker: %w", err)
	}
	return nil
}

// SetReconciler records that reconciler reconcilerID, holding (or claiming)
// reconciler lease generation generation, performs the transaction's action
// changes (migration 00007). Like a worker, a reconciler whose generation
// is not the action's current one cannot commit.
func SetReconciler(ctx context.Context, tx pgx.Tx, reconcilerID string, generation int64) error {
	if reconcilerID == "" || generation < 1 {
		return fmt.Errorf("storage: reconciler id and a positive lease generation are required")
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.system_actor', 'reconciler', true),
		set_config('app.worker_id', $1, true), set_config('app.lease_generation', $2, true)`,
		reconcilerID, strconv.FormatInt(generation, 10)); err != nil {
		return fmt.Errorf("storage: set reconciler: %w", err)
	}
	return nil
}

// SetScanner records that MCP scanner scannerID (an execution worker),
// holding (or claiming) scan lease generation generation, performs the
// transaction's registry changes (migration 00014, ADR-023). PostgreSQL
// refuses a record from a scanner that no longer holds that lease, and
// eacp.actor_context() rejects the scanner, so it can never change an action.
func SetScanner(ctx context.Context, tx pgx.Tx, scannerID string, generation int64) error {
	if scannerID == "" || generation < 1 {
		return fmt.Errorf("storage: scanner id and a positive lease generation are required")
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.system_actor', 'scanner', true),
		set_config('app.worker_id', $1, true), set_config('app.lease_generation', $2, true)`,
		scannerID, strconv.FormatInt(generation, 10)); err != nil {
		return fmt.Errorf("storage: set scanner: %w", err)
	}
	return nil
}

// SetTraceparent carries a W3C traceparent into the transaction so the
// outbox trigger can attach it to events. An empty value is a no-op.
func SetTraceparent(ctx context.Context, tx pgx.Tx, traceparent string) error {
	if traceparent == "" {
		return nil
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.traceparent', $1, true)`, traceparent); err != nil {
		return fmt.Errorf("storage: set traceparent: %w", err)
	}
	return nil
}
