package worker

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/storage"
)

// inReconcileTx runs fn in tenant as this store's reconciler at generation
// gen (migration 00007), retrying serialization failures and deadlocks.
func (s *Store) inReconcileTx(ctx context.Context, l Lease, fn func(pgx.Tx) error) error {
	for attempt := 0; ; attempt++ {
		err := storage.InTenantTx(ctx, s.pool, l.TenantID.String(), func(tx pgx.Tx) error {
			if err := storage.SetReconciler(ctx, tx, s.id, l.Generation); err != nil {
				return err
			}
			return fn(tx)
		})
		if attempt < 3 && retryable(err) {
			continue
		}
		return err
	}
}

// Reconcilable lists up to limit UNKNOWN_OUTCOME actions due for
// reconciliation that this reconciler can look up: a pinned contract with a
// lookup by operation key and a proof standard, a protocol it implements
// and a credential it holds (a hint; ClaimReconcile decides).
func (s *Store) Reconcilable(ctx context.Context, protocols []string, bindings []Binding, limit int) ([]Candidate, error) {
	if len(protocols) == 0 || len(bindings) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(bindings)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT tenant_id, action_id FROM eacp.reconcilable_actions($1, $2::jsonb, $3)`,
		protocols, string(b), limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Candidate, error) {
		var c Candidate
		err := r.Scan(&c.TenantID, &c.ActionID)
		return c, err
	})
}

// ClaimReconcile takes the reconciler lease on a due UNKNOWN_OUTCOME
// action (T28, FOR UPDATE SKIP LOCKED): the next lease generation, which
// fences out every earlier worker and reconciler.
func (s *Store) ClaimReconcile(ctx context.Context, c Candidate, lease time.Duration) (l Lease, ok bool, err error) {
	err = storage.InTenantTx(ctx, s.pool, c.TenantID.String(), func(tx pgx.Tx) error {
		var state string
		var gen int64
		var due bool
		err := tx.QueryRow(ctx, `SELECT state, lease_generation, COALESCE(next_reconcile_at, '-infinity') <= now()
			FROM eacp.actions WHERE id = $1 FOR UPDATE SKIP LOCKED`, c.ActionID).Scan(&state, &gen, &due)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (state != "UNKNOWN_OUTCOME" || !due)) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := storage.SetReconciler(ctx, tx, s.id, gen+1); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE eacp.actions SET state = 'RECONCILING', lease_generation = $2,
			worker_id = $3, leased_until = now() + make_interval(secs => $4)
			WHERE id = $1 AND state = 'UNKNOWN_OUTCOME' AND lease_generation = $2 - 1`,
			c.ActionID, gen+1, s.id, lease.Seconds())
		if err != nil {
			return err
		}
		ok = tag.RowsAffected() == 1
		l = Lease{TenantID: c.TenantID, ActionID: c.ActionID, Generation: gen + 1}
		return nil
	})
	if err != nil || !ok {
		return Lease{}, false, err
	}
	return l, true, nil
}

// reconcileJob is what reconciling a leased action needs, read under the
// reconciler lease.
type reconcileJob struct {
	Lease
	OperationKey, Protocol, Endpoint, SecretRef string
	Timeout                                     time.Duration // the contract's call budget
	Attempts                                    int           // reconcile attempts so far
	Facts                                       reconcileFacts
	age                                         time.Duration
}

// loadReconcile reads the action under reconciler lease l with its pinned
// contract, connector, and every success an attempt reported.
func (s *Store) loadReconcile(ctx context.Context, l Lease) (reconcileJob, error) {
	j := reconcileJob{Lease: l}
	err := storage.InTenantTx(ctx, s.pool, l.TenantID.String(), func(tx pgx.Tx) error {
		var state string
		var gen int64
		var timeout, age float64
		err := tx.QueryRow(ctx, `SELECT a.state, a.lease_generation, a.operation_key, c.protocol, c.endpoint,
			c.secret_ref, extract(epoch FROM eacp.call_timeout(a.connector_contract_id))::float8,
			a.reconcile_attempts, extract(epoch FROM now() - COALESCE(a.outcome_unknown_at, a.state_changed_at))::float8,
			k.proof_standard, COALESCE(eacp.outcome_settled_at(a) <= now(), true),
			a.cancel_requested_at IS NULL AND a.not_after > now() AND eacp.retry_budget_exhausted(a, k) IS NULL,
			ARRAY(SELECT x.external_reference FROM eacp.action_attempts x
			      WHERE x.action_id = a.id AND x.outcome = 'succeeded' ORDER BY x.attempt_no)
			FROM eacp.actions a
			JOIN eacp.tool_contracts k ON k.tenant_id = a.tenant_id AND k.id = a.connector_contract_id
			JOIN eacp.tools t ON t.tenant_id = a.tenant_id AND t.id = a.tool_id
			JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
			WHERE a.id = $1`, l.ActionID).Scan(&state, &gen, &j.OperationKey, &j.Protocol, &j.Endpoint,
			&j.SecretRef, &timeout, &j.Attempts, &age, &j.Facts.Proof, &j.Facts.Settled, &j.Facts.RetryAllowed,
			&j.Facts.Reported)
		if err != nil {
			return err
		}
		if state != "RECONCILING" || gen != l.Generation {
			return ErrLeaseLost
		}
		j.Timeout = time.Duration(timeout * float64(time.Second))
		j.age = time.Duration(age * float64(time.Second))
		return nil
	})
	return j, err
}

// reconciled records verdict v of the check made under reconciler lease l
// and makes the move it drives, in one transaction (T30-T34). next is the
// delay before a retry (T31) or the next reconciliation (T33).
func (s *Store) reconciled(ctx context.Context, l Lease, v verdict, next time.Duration) error {
	v.Reason = clip(v.Reason)
	return s.inReconcileTx(ctx, l, func(tx pgx.Tx) error {
		var state string
		var gen int64
		err := tx.QueryRow(ctx, `SELECT state, lease_generation FROM eacp.actions WHERE id = $1 FOR UPDATE`,
			l.ActionID).Scan(&state, &gen)
		if err != nil {
			return err
		}
		if state != "RECONCILING" || gen != l.Generation {
			return ErrLeaseLost
		}
		if _, err := tx.Exec(ctx, `INSERT INTO eacp.reconciliation_checks
			(tenant_id, action_id, lease_generation, result, external_reference)
			VALUES (eacp.current_tenant_id(), $1, $2, $3, NULLIF($4, ''))`,
			l.ActionID, l.Generation, v.Check, v.ExternalReference); err != nil {
			return fenced(err)
		}
		var tag interface{ RowsAffected() int64 }
		switch v.To {
		case "RETRY_WAIT":
			tag, err = tx.Exec(ctx, `UPDATE eacp.actions SET state = 'RETRY_WAIT', state_reason = $2,
				next_attempt_at = now() + make_interval(secs => $3)
				WHERE id = $1 AND state = 'RECONCILING' AND lease_generation = $4`,
				l.ActionID, v.Reason, next.Seconds(), l.Generation)
		case "UNKNOWN_OUTCOME":
			tag, err = tx.Exec(ctx, `UPDATE eacp.actions SET state = 'UNKNOWN_OUTCOME', state_reason = $2,
				reconcile_attempts = reconcile_attempts + 1, next_reconcile_at = now() + make_interval(secs => $3)
				WHERE id = $1 AND state = 'RECONCILING' AND lease_generation = $4`,
				l.ActionID, v.Reason, next.Seconds(), l.Generation)
		default:
			tag, err = tx.Exec(ctx, `UPDATE eacp.actions SET state = $2, state_reason = $3
				WHERE id = $1 AND state = 'RECONCILING' AND lease_generation = $4`,
				l.ActionID, v.To, v.Reason, l.Generation)
		}
		if err != nil {
			return fenced(err)
		}
		if tag.RowsAffected() != 1 {
			return ErrLeaseLost
		}
		return nil
	})
}
