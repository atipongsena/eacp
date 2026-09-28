package bench

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The collector runs read-only queries on a superuser pool of the bench
// stack's PostgreSQL (it bypasses RLS to read the journal of one tenant).

// ReadTimelines returns one Timeline per id, in the order of ids. An id
// with no journal rows has no transitions (it counts as open).
func ReadTimelines(ctx context.Context, db *pgxpool.Pool, tenant uuid.UUID, ids []uuid.UUID) ([]Timeline, error) {
	keys := make([]string, len(ids))
	byID := make(map[string]*Timeline, len(ids))
	out := make([]Timeline, len(ids))
	for i, id := range ids {
		keys[i] = id.String()
		out[i].ActionID = keys[i]
		byID[keys[i]] = &out[i]
	}
	rows, err := db.Query(ctx, `
		SELECT p->'subject'->>'id', COALESCE(p->'data'->>'from', ''), p->'data'->>'to', e.recorded_at
		FROM eacp.audit_events e
		CROSS JOIN LATERAL (SELECT convert_from(e.payload, 'UTF8')::jsonb AS p) j
		WHERE e.tenant_id = $1
		  AND p->>'action' IN ('action.received', 'action.transition')
		  AND p->'subject'->>'type' = 'action'
		  AND p->'subject'->>'id' = ANY($2::text[])
		ORDER BY e.seq`, tenant, keys)
	if err != nil {
		return nil, fmt.Errorf("read transitions: %w", err)
	}
	for rows.Next() {
		var id string
		var tr Transition
		if err := rows.Scan(&id, &tr.From, &tr.To, &tr.At); err != nil {
			rows.Close()
			return nil, err
		}
		tl := byID[id]
		tl.Transitions = append(tl.Transitions, tr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read transitions: %w", err)
	}
	rows, err = db.Query(ctx, `
		SELECT action_id::text, dispatched_at, completed_at FROM eacp.action_attempts
		WHERE tenant_id = $1 AND attempt_no = 1 AND action_id = ANY($2::uuid[])`, tenant, ids)
	if err != nil {
		return nil, fmt.Errorf("read attempts: %w", err)
	}
	for rows.Next() {
		var id string
		var dispatched time.Time
		var completed *time.Time
		if err := rows.Scan(&id, &dispatched, &completed); err != nil {
			rows.Close()
			return nil, err
		}
		byID[id].Dispatched, byID[id].Completed = &dispatched, completed
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read attempts: %w", err)
	}
	return out, nil
}

// OpenCount is how many of ids are not in a terminal state.
func OpenCount(ctx context.Context, db *pgxpool.Pool, tenant uuid.UUID, ids []uuid.UUID) (int, error) {
	var n int
	err := db.QueryRow(ctx, `SELECT count(*) FROM eacp.actions
		WHERE tenant_id = $1 AND id = ANY($2::uuid[]) AND state <> ALL($3::text[])`, tenant, ids, TerminalStates).Scan(&n)
	return n, err
}

// TenantOpen is how many of the tenant's actions are not in a terminal
// state: a backlog a step would otherwise inherit.
func TenantOpen(ctx context.Context, db *pgxpool.Pool, tenant uuid.UUID) (int, error) {
	var n int
	err := db.QueryRow(ctx, `SELECT count(*) FROM eacp.actions
		WHERE tenant_id = $1 AND state <> ALL($2::text[])`, tenant, TerminalStates).Scan(&n)
	return n, err
}

// DuplicateKeys counts (agent, idempotency key) pairs with more than one
// action created since since. The unique constraint keeps it 0; the check
// guards a regression.
func DuplicateKeys(ctx context.Context, db *pgxpool.Pool, tenant uuid.UUID, since time.Time) (int, error) {
	var n int
	err := db.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM eacp.actions
		WHERE tenant_id = $1 AND created_at >= $2
		GROUP BY agent_id, idempotency_key HAVING count(*) > 1) d`, tenant, since).Scan(&n)
	return n, err
}

// DBSample is one reading of the database's counters.
type DBSample struct {
	At        time.Time
	Commits   int64
	BlksHit   int64
	BlksRead  int64
	Active    int
	LockWaits int
	SizeBytes int64
}

// ReadDB reads the current database's counters and activity.
func ReadDB(ctx context.Context, db *pgxpool.Pool) (DBSample, error) {
	var s DBSample
	err := db.QueryRow(ctx, `
		SELECT clock_timestamp(), d.xact_commit, d.blks_hit, d.blks_read, pg_database_size(d.datname),
		       (SELECT count(*) FROM pg_stat_activity a WHERE a.datname = d.datname AND a.state = 'active'),
		       (SELECT count(*) FROM pg_stat_activity a WHERE a.datname = d.datname AND a.wait_event_type = 'Lock')
		FROM pg_stat_database d WHERE d.datname = current_database()`).
		Scan(&s.At, &s.Commits, &s.BlksHit, &s.BlksRead, &s.SizeBytes, &s.Active, &s.LockWaits)
	return s, err
}

// ReadFunctionStats returns eacp.<name>'s cumulative calls and total time
// (pg_stat_user_functions, which needs track_functions). No row reads as 0.
func ReadFunctionStats(ctx context.Context, db *pgxpool.Pool, name string) (calls int64, totalMs float64, err error) {
	err = db.QueryRow(ctx, `SELECT COALESCE(sum(calls), 0)::bigint, COALESCE(sum(total_time), 0)::float8
		FROM pg_stat_user_functions WHERE schemaname = 'eacp' AND funcname = $1`, name).Scan(&calls, &totalMs)
	return calls, totalMs, err
}

// ReadLLMCalls returns admission-to-settlement times of the tenant's LLM
// calls created in [since, until) and settled.
func ReadLLMCalls(ctx context.Context, db *pgxpool.Pool, tenant uuid.UUID, since, until time.Time) ([]time.Duration, error) {
	rows, err := db.Query(ctx, `
		SELECT (extract(epoch FROM settled_at - created_at) * 1000000)::bigint FROM eacp.llm_calls
		WHERE tenant_id = $1 AND created_at >= $2 AND created_at < $3 AND settled_at IS NOT NULL`, tenant, since, until)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []time.Duration
	for rows.Next() {
		var us int64
		if err := rows.Scan(&us); err != nil {
			return nil, err
		}
		out = append(out, time.Duration(us)*time.Microsecond)
	}
	return out, rows.Err()
}
