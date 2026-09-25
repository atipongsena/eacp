package incident

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/storage"
)

// Evaluate runs eacp.incident_evaluate() for tenant as the incident system
// actor and returns the number of incidents it opened.
func (s *Service) Evaluate(ctx context.Context, tenant uuid.UUID) (int, error) {
	var n int
	err := storage.InTenantTx(ctx, s.pool, tenant.String(), func(tx pgx.Tx) error {
		if err := storage.SetSystem(ctx, tx, "incident"); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT eacp.incident_evaluate()`).Scan(&n)
	})
	return n, err
}

// EvaluateAll evaluates every tenant with a current signal.
func (s *Service) EvaluateAll(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT tenant_id FROM eacp.incident_tenants()`)
	if err != nil {
		return 0, err
	}
	tenants, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return 0, err
	}
	total := 0
	var first error
	for _, t := range tenants {
		n, err := s.Evaluate(ctx, t)
		total += n
		if err != nil && first == nil {
			first = fmt.Errorf("incident: evaluate tenant %s: %w", t, err)
		}
	}
	return total, first
}

// Run evaluates every interval until ctx ends.
func (s *Service) Run(ctx context.Context, interval time.Duration, log *slog.Logger) {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		n, err := s.EvaluateAll(ctx)
		if err != nil && ctx.Err() == nil {
			log.ErrorContext(ctx, "incident evaluation failed", "err", err)
		} else if n > 0 {
			log.InfoContext(ctx, "incidents opened", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
