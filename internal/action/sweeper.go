package action

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/storage"
)

// SweeperComponent is the system actor name the sweeper runs as.
const SweeperComponent = "sweeper"

// Sweeper is the control plane's background progress loop (ADR-004 actor
// SWP): it expires overdue actions and approvals (T5, T8, T13, T15, T18),
// reclaims expired leases (T17, T23, T24) and reconciler leases (T33),
// routes unknown outcomes that no lookup can resolve (T29, T29a) or that
// reconciliation could not resolve in time (T34), schedules retries
// (T25-T27), re-evaluates actions left RECEIVED by a PDP outage (T2a) and
// releases approved actions (T10-T13). Every step is an ordinary compare-and-set
// transition on the locked row, so the sweeper, workers and API requests
// may race harmlessly on the same action.
type Sweeper struct {
	e *Engine
	// Batch bounds the actions handled per tenant and pass (default 100).
	Batch int
	// Grace leaves recently changed RECEIVED and AUTHORIZED actions to the
	// request that is advancing them (default 2s).
	Grace time.Duration
	// ReconcileMaxAge is how long an outcome may stay unknown before a
	// human must resolve it (T34, default 1h). It also covers connectors no
	// reconciler serves.
	ReconcileMaxAge time.Duration
}

// NewSweeper returns a Sweeper for e.
func NewSweeper(e *Engine) *Sweeper {
	return &Sweeper{e: e, Batch: 100, Grace: 2 * time.Second, ReconcileMaxAge: time.Hour}
}

// Stats counts what one pass did.
type Stats struct {
	Tenants, Expired, Reclaimed, Retried, Advanced int
	// Escalated counts unknown outcomes sent to a human (T29, T34).
	Escalated int
}

// RunOnce makes one pass over every tenant with open actions.
func (s *Sweeper) RunOnce(ctx context.Context) (Stats, error) {
	var st Stats
	var after *uuid.UUID
	for {
		var tenants []uuid.UUID
		rows, err := s.e.pool.Query(ctx, `SELECT tenant_id FROM eacp.tenants_with_open_actions($1, 100)`, after)
		if err == nil {
			tenants, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		}
		if err != nil {
			return st, err
		}
		for _, t := range tenants {
			if err := s.sweepTenant(ctx, t, &st); err != nil {
				if ctx.Err() != nil {
					return st, ctx.Err()
				}
				s.e.o.Log.ErrorContext(ctx, "sweep failed", "tenant", t.String(), "err", err)
			}
			st.Tenants++
		}
		if len(tenants) < 100 {
			return st, nil
		}
		after = &tenants[len(tenants)-1]
	}
}

// Run sweeps every interval until ctx is cancelled.
func (s *Sweeper) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := s.RunOnce(ctx); err != nil && ctx.Err() == nil {
			s.e.o.Log.ErrorContext(ctx, "sweep pass failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Sweeper) ids(ctx context.Context, tenant uuid.UUID, sql string, args ...any) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := storage.InTenantTx(ctx, s.e.pool, tenant.String(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		ids, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		return err
	})
	return ids, err
}

func (s *Sweeper) sweepTenant(ctx context.Context, tenant uuid.UUID, st *Stats) error {
	a := System(tenant, SweeperComponent)

	// Overdue actions before any dispatch intent: T5, T13, T15, T18.
	due, err := s.ids(ctx, tenant, `SELECT id FROM eacp.actions
		WHERE state IN ('RECEIVED', 'AUTHORIZED', 'QUEUED', 'LEASED') AND not_after <= now()
		ORDER BY not_after LIMIT $1`, s.Batch)
	if err != nil {
		return err
	}
	for _, id := range due {
		err := s.e.inTx(ctx, a, func(tx pgx.Tx) error {
			r, err := load(ctx, tx, id, true)
			if err != nil || !r.Expired || (r.State != "RECEIVED" && r.State != "AUTHORIZED" &&
				r.State != "QUEUED" && r.State != "LEASED") {
				return err
			}
			return expire(ctx, tx, r)
		})
		if err != nil {
			return err
		}
		st.Expired++
	}

	// Expired approvals decide their action through the request cascade
	// (T8, and T13 for a granted approval that was never released).
	lapsed, err := s.ids(ctx, tenant, `SELECT a.id FROM eacp.actions a
		JOIN eacp.approval_requests r ON r.tenant_id = a.tenant_id AND r.id = a.approval_request_id
		WHERE a.state IN ('PENDING_APPROVAL', 'AUTHORIZED') AND r.state IN ('PENDING', 'GRANTED')
		  AND (r.expires_at <= now() OR r.not_after <= now())
		ORDER BY r.expires_at LIMIT $1`, s.Batch)
	if err != nil {
		return err
	}
	for _, id := range lapsed {
		err := s.e.inTx(ctx, a, func(tx pgx.Tx) error {
			r, err := load(ctx, tx, id, true) // the action first: lock order
			if err != nil || r.ApprovalRequestID == nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE eacp.approval_requests SET state = 'EXPIRED'
				WHERE id = $1 AND state IN ('PENDING', 'GRANTED')
				  AND (expires_at <= now() OR not_after <= now())`, *r.ApprovalRequestID)
			return err
		})
		if err != nil {
			return err
		}
		st.Expired++
	}

	// Expired leases (§22): before a dispatch intent the action is safely
	// re-queued (T17); after one the effect may have happened, so it is
	// UNKNOWN_OUTCOME (T23) unless the contract is an unrevoked READ_ONLY
	// (T24, narrowed in ADR-004 Rev 2.3). A lapsed reconciler lease decided
	// nothing: the outcome is still unknown (T33).
	lapsedLeases, err := s.ids(ctx, tenant, `SELECT id FROM eacp.actions
		WHERE state IN ('LEASED', 'EXECUTING', 'RECONCILING') AND leased_until <= now()
		ORDER BY leased_until LIMIT $1`, s.Batch)
	if err != nil {
		return err
	}
	for _, id := range lapsedLeases {
		err := s.e.inTx(ctx, a, func(tx pgx.Tx) error {
			var state string
			var expired, readOnly bool
			err := tx.QueryRow(ctx, `SELECT a.state, a.leased_until <= now(),
				k.side_effects = ARRAY['READ_ONLY'] AND k.revoked_at IS NULL
				FROM eacp.actions a
				JOIN eacp.tool_contracts k ON k.tenant_id = a.tenant_id AND k.id = a.connector_contract_id
				WHERE a.id = $1 FOR UPDATE OF a`, id).Scan(&state, &expired, &readOnly)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && (!expired || (state != "LEASED" &&
				state != "EXECUTING" && state != "RECONCILING"))) {
				return nil
			}
			if err != nil {
				return err
			}
			if state == "RECONCILING" {
				return move(ctx, tx, `UPDATE eacp.actions SET state = 'UNKNOWN_OUTCOME',
					state_reason = 'reconciler lease expired', reconcile_attempts = reconcile_attempts + 1,
					next_reconcile_at = now() WHERE id = $1 AND state = $2`, id, state)
			}
			to, why := "QUEUED", "lease expired before dispatch"
			if state == "EXECUTING" && readOnly {
				to, why = "RETRY_WAIT", "lease expired during a read"
			} else if state == "EXECUTING" {
				to, why = "UNKNOWN_OUTCOME", "lease expired during the call"
			}
			return move(ctx, tx, `UPDATE eacp.actions SET state = $3, state_reason = $4
				WHERE id = $1 AND state = $2`, id, state, to, why)
		})
		if err != nil {
			return err
		}
		st.Reclaimed++
	}

	// Unknown outcomes (§20) that no lookup can prove anything about go to a
	// human (T29), unknown reads are retried (T29a), and reconciliation that
	// has taken too long gives up to a human (T34). A human sees an outcome
	// only once every call has settled. Lookups are the reconciler's, in the
	// execution worker.
	unknown, err := s.ids(ctx, tenant, `SELECT a.id FROM eacp.actions a
		JOIN eacp.tool_contracts k ON k.tenant_id = a.tenant_id AND k.id = a.connector_contract_id
		WHERE a.state = 'UNKNOWN_OUTCOME'
		  AND (k.side_effects = ARRAY['READ_ONLY'] OR k.reconciliation_lookup = 'none'
		       OR k.proof_standard = 'none' OR k.revoked_at IS NOT NULL
		       OR COALESCE(a.outcome_unknown_at, a.state_changed_at) <= now() - make_interval(secs => $2))
		  AND ((k.side_effects = ARRAY['READ_ONLY'] AND k.revoked_at IS NULL)
		       OR COALESCE(eacp.outcome_settled_at(a), '-infinity') <= now())
		ORDER BY a.state_changed_at LIMIT $1`, s.Batch, s.ReconcileMaxAge.Seconds())
	if err != nil {
		return err
	}
	for _, id := range unknown {
		escalated := false
		err := s.e.inTx(ctx, a, func(tx pgx.Tx) error {
			var state, proof, lookup string
			var read, revoked, old bool
			err := tx.QueryRow(ctx, `SELECT a.state, k.side_effects = ARRAY['READ_ONLY'], k.revoked_at IS NOT NULL,
				k.proof_standard, k.reconciliation_lookup,
				COALESCE(a.outcome_unknown_at, a.state_changed_at) <= now() - make_interval(secs => $2)
				FROM eacp.actions a
				JOIN eacp.tool_contracts k ON k.tenant_id = a.tenant_id AND k.id = a.connector_contract_id
				WHERE a.id = $1 FOR UPDATE OF a`, id, s.ReconcileMaxAge.Seconds()).Scan(&state, &read, &revoked,
				&proof, &lookup, &old)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && state != "UNKNOWN_OUTCOME") {
				return nil
			}
			if err != nil {
				return err
			}
			var to, why string
			switch {
			case read && !revoked:
				to, why = "RETRY_WAIT", "unknown outcome of a read"
			case revoked:
				to, why = "NEEDS_HUMAN_RESOLUTION", "no reconciliation proof: contract revoked"
			case lookup == "none" || proof == "none":
				to, why = "NEEDS_HUMAN_RESOLUTION", "no reconciliation proof: proof standard none"
			case old:
				to, why = "NEEDS_HUMAN_RESOLUTION", "reconciliation time exhausted"
			default:
				return nil
			}
			escalated = to == "NEEDS_HUMAN_RESOLUTION"
			return move(ctx, tx, `UPDATE eacp.actions SET state = $2, state_reason = $3
				WHERE id = $1 AND state = 'UNKNOWN_OUTCOME'`, id, to, why)
		})
		if err != nil {
			return err
		}
		if escalated {
			st.Escalated++
		} else {
			st.Retried++
		}
	}

	// Retries (T25-T27): a due retry is re-queued under the pinned policy or
	// sent back through the release boundary under a new one; a cancelled,
	// expired or exhausted one fails.
	retries, err := s.ids(ctx, tenant, `SELECT id FROM eacp.actions
		WHERE state = 'RETRY_WAIT'
		  AND (next_attempt_at <= now() OR cancel_requested_at IS NOT NULL OR not_after <= now())
		ORDER BY next_attempt_at LIMIT $1`, s.Batch)
	if err != nil {
		return err
	}
	for _, id := range retries {
		err := s.e.inTx(ctx, a, func(tx pgx.Tx) error {
			var state string
			var due, cancelled, expired, exhausted, samePolicy bool
			err := tx.QueryRow(ctx, `SELECT a.state, a.next_attempt_at <= now(), a.cancel_requested_at IS NOT NULL,
				a.not_after <= now(), a.attempt_count >= k.max_attempts,
				p.current_bundle_id = a.policy_bundle_id AND p.current_version = a.policy_version
				FROM eacp.actions a
				JOIN eacp.tool_contracts k ON k.tenant_id = a.tenant_id AND k.id = a.connector_contract_id
				JOIN eacp.tenant_policy_pointer p ON p.tenant_id = a.tenant_id
				WHERE a.id = $1 FOR UPDATE OF a`, id).Scan(&state, &due, &cancelled, &expired, &exhausted, &samePolicy)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && state != "RETRY_WAIT") {
				return nil
			}
			if err != nil {
				return err
			}
			var to, why string
			switch {
			case cancelled:
				to, why = "FAILED", "cancelled"
			case expired:
				to, why = "FAILED", "expired"
			case exhausted:
				to, why = "FAILED", "retry budget exhausted"
			case !due:
				return nil
			case samePolicy:
				to, why = "QUEUED", "retry"
			default:
				to, why = "AUTHORIZED", "policy changed before retry"
			}
			return move(ctx, tx, `UPDATE eacp.actions SET state = $2, state_reason = $3
				WHERE id = $1 AND state = 'RETRY_WAIT'`, id, to, why)
		})
		if err != nil {
			return err
		}
		st.Retried++
	}

	// Pending governance work: T2a retries and releases after approval.
	open, err := s.ids(ctx, tenant, `SELECT id FROM eacp.actions
		WHERE state IN ('RECEIVED', 'AUTHORIZED') AND not_after > now()
		  AND state_changed_at <= now() - make_interval(secs => $2)
		ORDER BY state_changed_at LIMIT $1`, s.Batch, s.Grace.Seconds())
	if err != nil {
		return err
	}
	for _, id := range open {
		if _, err := s.e.Advance(ctx, a, id); err != nil && !errors.Is(err, ErrGovernanceUnavailable) {
			s.e.o.Log.WarnContext(ctx, "sweeper could not advance action",
				"tenant", tenant.String(), "action", id.String(), "err", err)
		}
		st.Advanced++
	}
	return nil
}
