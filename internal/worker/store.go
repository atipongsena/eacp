package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atipongsena/eacp/internal/storage"
)

// ErrLeaseLost: the worker no longer holds the lease at its generation (it
// expired and was reclaimed, or the action moved on). A stale worker must
// not dispatch; a result it still holds is recorded as late evidence.
var ErrLeaseLost = errors.New("worker: lease lost")

// Store performs one worker's fenced transitions (migration 00006). Every
// write binds the worker id and lease generation, and PostgreSQL rejects a
// write whose generation is not the action's current one.
type Store struct {
	pool *pgxpool.Pool
	id   string
}

// NewStore returns the store of worker id over pool (the application role).
func NewStore(pool *pgxpool.Pool, id string) *Store { return &Store{pool: pool, id: id} }

// Candidate is a claimable action.
type Candidate struct{ TenantID, ActionID uuid.UUID }

// Lease is a claimed action at a lease generation (the fencing token).
// Capacity names its capacity group ("group:<name>" or "connector:<id>")
// for the worker's bulkhead; Claim sets it.
type Lease struct {
	TenantID, ActionID uuid.UUID
	Generation         int64
	Capacity           string
}

// Skip is a key the claim hint leaves out for one tenant: a capacity group
// ("group:<name>" or "connector:<id>") whose bulkhead is full, or a
// connector ("connector:<id>") whose breaker is open (ADR-022 §2).
type Skip struct {
	TenantID uuid.UUID `json:"tenant_id"`
	Key      string    `json:"key"`
}

func (l Lease) String() string { return fmt.Sprintf("%s@%d", l.ActionID, l.Generation) }

func retryable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "40P01")
}

// fenced maps a rejected worker write to ErrLeaseLost.
func fenced(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "42501" || pgErr.Code == "55000") {
		return fmt.Errorf("%w: %s", ErrLeaseLost, pgErr.Message)
	}
	return err
}

// inTx runs fn in tenant as this worker at generation gen (0: read only,
// no actor), retrying serialization failures and deadlocks.
func (s *Store) inTx(ctx context.Context, tenant uuid.UUID, gen int64, fn func(pgx.Tx) error) error {
	for attempt := 0; ; attempt++ {
		err := storage.InTenantTx(ctx, s.pool, tenant.String(), func(tx pgx.Tx) error {
			if gen > 0 {
				if err := storage.SetWorker(ctx, tx, s.id, gen); err != nil {
					return err
				}
			}
			return fn(tx)
		})
		if attempt < 3 && retryable(err) {
			continue
		}
		return err
	}
}

// Claimable lists up to limit eligible QUEUED actions in PostgreSQL fair
// order, leaving out open circuits and the keys in skip. The protocol,
// credential, capacity and circuit checks are hints; Claim rechecks the
// authoritative state under the action lock.
func (s *Store) Claimable(ctx context.Context, protocols []string, bindings []Binding, limit int, skip []Skip) ([]Candidate, error) {
	if len(protocols) == 0 || len(bindings) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(bindings)
	if err != nil {
		return nil, err
	}
	if skip == nil {
		skip = []Skip{}
	}
	sk, err := json.Marshal(skip)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT tenant_id, action_id FROM eacp.claimable_actions($1, $2::jsonb, $3, $4::jsonb)`,
		protocols, string(b), limit, string(sk))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Candidate, error) {
		var c Candidate
		err := r.Scan(&c.TenantID, &c.ActionID)
		return c, err
	})
}

// Claim leases c for lease (T14, FOR UPDATE SKIP LOCKED). ok is false when
// another worker holds the row, it is no longer QUEUED, its connector
// capacity is occupied or its connector's circuit is open.
func (s *Store) Claim(ctx context.Context, c Candidate, lease time.Duration) (l Lease, ok bool, err error) {
	err = storage.InTenantTx(ctx, s.pool, c.TenantID.String(), func(tx pgx.Tx) error {
		var state string
		var gen int64
		err := tx.QueryRow(ctx, `SELECT state, lease_generation FROM eacp.actions
			WHERE id = $1 FOR UPDATE SKIP LOCKED`, c.ActionID).Scan(&state, &gen)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && state != "QUEUED") {
			return nil
		}
		if err != nil {
			return err
		}
		if err := storage.SetWorker(ctx, tx, s.id, gen+1); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE eacp.actions SET state = 'LEASED', lease_generation = $2,
			worker_id = $3, leased_until = now() + make_interval(secs => $4)
			WHERE id = $1 AND state = 'QUEUED' AND lease_generation = $2 - 1`,
			c.ActionID, gen+1, s.id, lease.Seconds())
		if err != nil {
			return err
		}
		ok = tag.RowsAffected() == 1
		l = Lease{TenantID: c.TenantID, ActionID: c.ActionID, Generation: gen + 1}
		if !ok {
			return nil
		}
		return tx.QueryRow(ctx, `SELECT COALESCE('group:' || k.concurrency_group, 'connector:' || t.connector_id::text)
			FROM eacp.actions a
			JOIN eacp.tool_contracts k ON k.tenant_id = a.tenant_id AND k.id = a.connector_contract_id
			JOIN eacp.tools t ON t.tenant_id = k.tenant_id AND t.id = k.tool_id
			WHERE a.id = $1`, c.ActionID).Scan(&l.Capacity)
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "53300" {
		return Lease{}, false, nil
	}
	if err != nil || !ok {
		return Lease{}, false, err
	}
	return l, true, nil
}

// Job is everything a leased action's execution needs, read under the lease.
type Job struct {
	Lease
	AgentID, AgentVersionID                   uuid.UUID
	Subject, Operation, Target, Tool          string
	ToolSchemaVersion, Resource, OperationKey string
	InputPayload, EnforcedPayload             json.RawMessage
	EnforcedDigest                            []byte
	Attempts                                  int // dispatch intents so far
	ConnectorID                               uuid.UUID
	Protocol, Endpoint, SecretRef             string
	Contract                                  Contract
	CallTimeout                               time.Duration // eacp.call_timeout of the pinned contract
	// RemoteName is the server's exact tool name and Definition the certified
	// canonical definition of the contract's definition_id (MCP tools only,
	// ADR-032); both are empty for other protocols.
	RemoteName, Definition string
}

// Load reads the leased action with its pinned contract and connector.
func (s *Store) Load(ctx context.Context, l Lease) (Job, error) {
	j := Job{Lease: l}
	err := s.inTx(ctx, l.TenantID, 0, func(tx pgx.Tx) error {
		var state string
		var gen int64
		var input, enforced string
		var callSecs float64
		err := tx.QueryRow(ctx, `SELECT a.state, a.lease_generation, a.agent_id, a.agent_version_id, a.subject,
			a.operation, a.target, a.tool, a.tool_schema_version, a.resource, a.operation_key,
			a.input_payload::text, a.enforced_payload::text, a.enforced_digest, a.attempt_count,
			c.id, c.protocol, c.endpoint, c.secret_ref, k.version, k.side_effects, k.idempotency_mode,
			COALESCE(k.idempotency_key_field, ''), COALESCE(k.correlation_field, ''),
			k.no_effect_errors, k.max_attempts,
			extract(epoch FROM eacp.call_timeout(a.connector_contract_id))::float8,
			COALESCE(t.remote_name, ''), COALESCE(d.definition, '')
			FROM eacp.actions a
			JOIN eacp.tool_contracts k ON k.tenant_id = a.tenant_id AND k.id = a.connector_contract_id
			JOIN eacp.tools t ON t.tenant_id = a.tenant_id AND t.id = a.tool_id
			JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
			LEFT JOIN eacp.tool_definitions d ON d.tenant_id = k.tenant_id AND d.id = k.definition_id
			WHERE a.id = $1`, l.ActionID).Scan(&state, &gen, &j.AgentID, &j.AgentVersionID, &j.Subject,
			&j.Operation, &j.Target, &j.Tool, &j.ToolSchemaVersion, &j.Resource, &j.OperationKey,
			&input, &enforced, &j.EnforcedDigest, &j.Attempts, &j.ConnectorID, &j.Protocol, &j.Endpoint, &j.SecretRef,
			&j.Contract.Version, &j.Contract.SideEffects, &j.Contract.IdempotencyMode,
			&j.Contract.IdempotencyKeyField, &j.Contract.CorrelationField, &j.Contract.NoEffectErrors,
			&j.Contract.MaxAttempts, &callSecs, &j.RemoteName, &j.Definition)
		if err != nil {
			return err
		}
		if state != "LEASED" || gen != l.Generation {
			return ErrLeaseLost
		}
		j.CallTimeout = time.Duration(callSecs * float64(time.Second))
		j.InputPayload, j.EnforcedPayload = json.RawMessage(input), json.RawMessage(enforced)
		return nil
	})
	return j, err
}

// Decision is what the dispatch-intent transaction did.
type Decision string

const (
	Dispatched  Decision = "dispatched"   // T16: the call may be made
	Reauthorize Decision = "reauthorize"  // T16a: policy changed
	Denied      Decision = "denied"       // T16b: revoked before dispatch
	Expired     Decision = "expired"      // T18: not_after passed
	CircuitOpen Decision = "circuit_open" // T17: the connector's circuit is open
	Killed      Decision = "killed"       // T17: a kill scope withheld dispatch
)

// Dispatch is a recorded dispatch intent.
type Dispatch struct {
	Attempt int
	Timeout time.Duration // call budget from the pinned contract, by the database
}

// Intent records the fenced dispatch intent (T16) in its own transaction,
// with the registry and policy read FOR SHARE. When dispatch is not
// permitted it makes T16a, T16b, T18 or, while the connector's circuit is
// open (ADR-022 §3), T17 instead. Only Dispatched allows the external call.
func (s *Store) Intent(ctx context.Context, l Lease, leaseAfterCall time.Duration) (Decision, Dispatch, error) {
	var d Dispatch
	var decision Decision
	err := s.inTx(ctx, l.TenantID, l.Generation, func(tx pgx.Tx) error {
		var state string
		var gen int64
		var expired bool
		var drift, circuit *string
		var timeout float64
		err := tx.QueryRow(ctx, `SELECT a.state, a.lease_generation, a.not_after <= now(), eacp.dispatch_drift(a),
			extract(epoch FROM eacp.call_timeout(a.connector_contract_id))::float8,
			(SELECT eacp.connector_circuit_state(t.tenant_id, t.connector_id) FROM eacp.tools t
			 WHERE t.tenant_id = a.tenant_id AND t.id = a.tool_id)
			FROM eacp.actions a WHERE a.id = $1 FOR UPDATE`, l.ActionID).Scan(&state, &gen, &expired, &drift, &timeout,
			&circuit)
		if err != nil {
			return err
		}
		if state != "LEASED" || gen != l.Generation {
			return ErrLeaseLost
		}
		// Serialize even an absent kill row with set_kill. The action row is
		// already locked; this same advisory lock is taken by the T16 trigger.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('eacp.kill:' || eacp.current_tenant_id()::text, 0))`); err != nil {
			return err
		}
		var killed bool
		if err := tx.QueryRow(ctx, `SELECT eacp.action_killed(a) FROM eacp.actions a WHERE id = $1`, l.ActionID).Scan(&killed); err != nil {
			return err
		}
		move := func(sql string, args ...any) error {
			_, err := tx.Exec(ctx, sql, append([]any{l.ActionID}, args...)...)
			return fenced(err)
		}
		switch {
		case expired:
			decision = Expired
			return move(`UPDATE eacp.actions SET state = 'EXPIRED', state_reason = 'expired before dispatch' WHERE id = $1`)
		case drift != nil && *drift == "policy_changed":
			decision = Reauthorize
			return move(`UPDATE eacp.actions SET state = 'AUTHORIZED', state_reason = 'policy changed before dispatch' WHERE id = $1`)
		case drift != nil:
			decision = Denied
			return move(`UPDATE eacp.actions SET state = 'DENIED', state_reason = $2 WHERE id = $1`, *drift)
		case killed:
			decision = Killed
			return move(`UPDATE eacp.actions SET state = 'QUEUED', state_reason = 'kill scope active' WHERE id = $1`)
		case circuit != nil:
			decision = CircuitOpen
			return move(`UPDATE eacp.actions SET state = 'QUEUED', state_reason = 'connector circuit open' WHERE id = $1`)
		}
		decision = Dispatched
		d.Timeout = time.Duration(timeout * float64(time.Second))
		if err := move(`UPDATE eacp.actions SET state = 'EXECUTING', attempt_count = attempt_count + 1,
			leased_until = now() + make_interval(secs => $2) WHERE id = $1`,
			(d.Timeout + leaseAfterCall).Seconds()); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT attempt_no FROM eacp.action_attempts
			WHERE action_id = $1 AND lease_generation = $2`, l.ActionID, l.Generation).Scan(&d.Attempt)
	})
	return decision, d, err
}

// CheckKill is the post-intent, pre-call check. An epoch change is treated
// conservatively even if an operator already resumed the scope.
func (s *Store) CheckKill(ctx context.Context, l Lease) (bool, error) {
	var killed bool
	err := s.inTx(ctx, l.TenantID, 0, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT eacp.action_killed(a) OR a.dispatch_kill_epoch <
			COALESCE((SELECT epoch FROM eacp.kill_tenant_epochs WHERE tenant_id = a.tenant_id), 0)
			FROM eacp.actions a WHERE a.id = $1 AND a.state = 'EXECUTING' AND a.lease_generation = $2`,
			l.ActionID, l.Generation).Scan(&killed)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return true, ErrLeaseLost
	}
	return killed, err
}

// Deny denies a leased action before dispatch (T16b) for a reason the
// database accepts without a registry revocation: enforced_digest_mismatch.
func (s *Store) Deny(ctx context.Context, l Lease, reason string) error {
	return s.inTx(ctx, l.TenantID, l.Generation, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE eacp.actions SET state = 'DENIED', state_reason = $2
			WHERE id = $1 AND state = 'LEASED' AND lease_generation = $3`, l.ActionID, reason, l.Generation)
		return fenced(err)
	})
}

// Release returns a leased action to QUEUED before any dispatch intent
// (T17, voluntary).
func (s *Store) Release(ctx context.Context, l Lease, reason string) error {
	return s.inTx(ctx, l.TenantID, l.Generation, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE eacp.actions SET state = 'QUEUED', state_reason = $2
			WHERE id = $1 AND state = 'LEASED' AND lease_generation = $3`, l.ActionID, reason, l.Generation)
		return fenced(err)
	})
}

// Heartbeat extends a live lease to at least now()+lease and reports
// whether a cancellation has been requested. An expired or lost lease is
// ErrLeaseLost: the sweeper owns it now.
func (s *Store) Heartbeat(ctx context.Context, l Lease, lease time.Duration) (cancelRequested bool, err error) {
	err = s.inTx(ctx, l.TenantID, l.Generation, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `UPDATE eacp.actions a
			SET leased_until = greatest(leased_until, now() + make_interval(secs => $2))
			WHERE id = $1 AND lease_generation = $3 AND state IN ('LEASED', 'EXECUTING')
			RETURNING cancel_requested_at IS NOT NULL OR eacp.action_killed(a) OR
			  (a.state = 'EXECUTING' AND a.dispatch_kill_epoch < COALESCE(
			   (SELECT epoch FROM eacp.kill_tenant_epochs WHERE tenant_id = a.tenant_id), 0))`,
			l.ActionID, lease.Seconds(), l.Generation).Scan(&cancelRequested)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLeaseLost
		}
		return fenced(err)
	})
	return cancelRequested, err
}

// Completion is what the result transaction did.
type Completion struct {
	State string // the action's new state; "" for a late result
	Late  bool   // recorded as late-result evidence without a state change
}

// Complete records the classified result of attempt l in one transaction:
// the attempt's outcome and, if the lease still holds, the fenced
// transition it drives (T19-T22a). Otherwise the result is late evidence.
// backoff schedules a retry when the contract and state permit one.
func (s *Store) Complete(ctx context.Context, l Lease, r Result, backoff time.Duration) (Completion, error) {
	var c Completion
	err := s.inTx(ctx, l.TenantID, l.Generation, func(tx pgx.Tx) error {
		var state string
		var gen int64
		var budgetLeft, cancelled, alive, readOnly, killChanged bool
		err := tx.QueryRow(ctx, `SELECT a.state, a.lease_generation, a.cancel_requested_at IS NOT NULL,
			a.not_after > now(), k.side_effects = ARRAY['READ_ONLY'] AND k.revoked_at IS NULL
			FROM eacp.actions a
			JOIN eacp.tool_contracts k ON k.tenant_id = a.tenant_id AND k.id = a.connector_contract_id
			WHERE a.id = $1 FOR UPDATE OF a`, l.ActionID).Scan(&state, &gen, &cancelled, &alive, &readOnly)
		if err != nil {
			return err
		}
		// Lock after the action row, as T16 does. A concurrent kill that
		// commits first must be visible before a result is classified.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('eacp.kill:' || eacp.current_tenant_id()::text, 0))`); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT eacp.action_killed(a) OR a.dispatch_kill_epoch < COALESCE(
			(SELECT epoch FROM eacp.kill_tenant_epochs WHERE tenant_id = a.tenant_id), 0)
			FROM eacp.actions a WHERE a.id = $1`, l.ActionID).Scan(&killChanged); err != nil {
			return err
		}
		if killChanged {
			// The kill makes the outcome unknown; a reference the connector
			// proved still tells the human where to look (ADR-030 §5).
			remote := r.RemoteReference
			if r.Outcome == Succeeded && remoteReferencePattern.MatchString(r.ExternalReference) {
				remote = r.ExternalReference
			}
			r = Result{Outcome: Ambiguous, ErrorClass: "kill_interrupted", RemoteReference: remote}
		}
		if err := tx.QueryRow(ctx, `UPDATE eacp.action_attempts
			SET outcome = $3, external_reference = NULLIF($4, ''), error_class = NULLIF($5, ''),
			    remote_reference = NULLIF($6, '')
			WHERE action_id = $1 AND lease_generation = $2 RETURNING late`,
			l.ActionID, l.Generation, string(r.Outcome), r.ExternalReference, r.ErrorClass,
			r.RemoteReference).Scan(&c.Late); err != nil {
			return fenced(err)
		}
		if c.Late {
			return nil
		}
		// The retry budget (ADR-022 §5), asked of the database that enforces it.
		if err := tx.QueryRow(ctx, `SELECT eacp.retry_budget_exhausted(a, k) IS NULL
			FROM eacp.actions a
			JOIN eacp.tool_contracts k ON k.tenant_id = a.tenant_id AND k.id = a.connector_contract_id
			WHERE a.id = $1`, l.ActionID).Scan(&budgetLeft); err != nil {
			return err
		}
		retry := budgetLeft && !cancelled && !killChanged && alive
		var reason string
		switch {
		case killChanged:
			c.State, reason = "UNKNOWN_OUTCOME", "kill during execution"
		case r.Outcome == Succeeded:
			c.State, reason = "SUCCEEDED", "succeeded"
		case r.Outcome == NoEffect && retry:
			c.State, reason = "RETRY_WAIT", "no effect: "+r.ErrorClass
		case r.Outcome == NoEffect:
			c.State, reason = "FAILED", "no effect: "+r.ErrorClass
		case readOnly && retry:
			c.State, reason = "RETRY_WAIT", "ambiguous read"
		case cancelled:
			c.State, reason = "UNKNOWN_OUTCOME", "cancelled during the call"
		default:
			c.State, reason = "UNKNOWN_OUTCOME", "ambiguous result"
		}
		var next any
		if c.State == "RETRY_WAIT" {
			next = max(backoff, 100*time.Millisecond).Seconds()
		}
		_, err = tx.Exec(ctx, `UPDATE eacp.actions SET state = $2, state_reason = $3,
			next_attempt_at = now() + make_interval(secs => $4::float8)
			WHERE id = $1 AND state = 'EXECUTING' AND lease_generation = $5`,
			l.ActionID, c.State, reason, next, l.Generation)
		return fenced(err)
	})
	if err != nil {
		return Completion{}, err
	}
	return c, nil
}

// TripCircuit opens the shared circuit of connector for cooldown (at most
// 10 minutes, and never earlier than it already is) after this worker's
// breaker opened on failed calls such as the one under lease l (ADR-022
// §3). The database journals it.
func (s *Store) TripCircuit(ctx context.Context, l Lease, connector uuid.UUID, cooldown time.Duration, reason string) error {
	return s.inTx(ctx, l.TenantID, l.Generation, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE eacp.connector_circuits
			SET open_until = now() + make_interval(secs => $2), reason = $3 WHERE connector_id = $1`,
			connector, min(cooldown, maxBreakerCooldown).Seconds(), reason)
		return err
	})
}
