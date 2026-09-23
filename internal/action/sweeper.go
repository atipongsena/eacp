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
// SWP): it expires overdue actions and approvals (T5, T8, T13, T15),
// re-evaluates actions left RECEIVED by a PDP outage (T2a) and releases
// approved actions (T10-T13). Every step is an ordinary engine transition,
// so the sweeper and API requests may race harmlessly on the same action.
type Sweeper struct {
	e *Engine
	// Batch bounds the actions handled per tenant and pass (default 100).
	Batch int
	// Grace leaves recently changed RECEIVED and AUTHORIZED actions to the
	// request that is advancing them (default 2s).
	Grace time.Duration
}

// NewSweeper returns a Sweeper for e.
func NewSweeper(e *Engine) *Sweeper {
	return &Sweeper{e: e, Batch: 100, Grace: 2 * time.Second}
}

// Stats counts what one pass did.
type Stats struct {
	Tenants, Expired, Advanced int
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

	// Overdue actions without an outstanding approval: T5, T13, T15.
	due, err := s.ids(ctx, tenant, `SELECT id FROM eacp.actions
		WHERE state IN ('RECEIVED', 'AUTHORIZED', 'QUEUED') AND not_after <= now()
		ORDER BY not_after LIMIT $1`, s.Batch)
	if err != nil {
		return err
	}
	for _, id := range due {
		err := s.e.inTx(ctx, a, func(tx pgx.Tx) error {
			r, err := load(ctx, tx, id, true)
			if err != nil || !r.Expired || (r.State != "RECEIVED" && r.State != "AUTHORIZED" && r.State != "QUEUED") {
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
