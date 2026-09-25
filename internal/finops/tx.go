package finops

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Tx makes rate-card and soft-limit writes in a caller's transaction, which
// must already have its tenant and actor set. Service uses it, and a bundle
// change set (ADR-026) uses it, so the FinOps triggers decide both the same
// way. Errors are raw database errors, except an amount that fails
// validation.
type Tx struct{ pgx.Tx }

// AddPrice adds a price, effective now unless a later effective_from is
// given; the price trigger refuses a backdated one.
func (t Tx) AddPrice(ctx context.Context, n NewPrice) (Price, error) {
	for _, v := range []*string{&n.InputPerMTok, n.CachedPerMTok, &n.OutputPerMTok} {
		if v != nil && !amountPattern.MatchString(*v) {
			return Price{}, invalid("prices must be non-negative decimals below 10^15 with at most 6 decimals")
		}
	}
	return scanPrice(t.QueryRow(ctx, `INSERT INTO eacp.model_prices (tenant_id, provider, model, unit,
		input_per_mtok, cached_input_per_mtok, output_per_mtok, effective_from, reason)
		VALUES (eacp.current_tenant_id(), $1, $2, $3, $4::numeric, $5::numeric, $6::numeric, COALESCE($7, now()), $8)
		RETURNING `+priceColumns,
		n.Provider, n.Model, n.Unit, n.InputPerMTok, n.CachedPerMTok, n.OutputPerMTok, n.EffectiveFrom, n.Reason))
}

// SetSoftLimit sets or clears (limit nil) an account's soft limit.
func (t Tx) SetSoftLimit(ctx context.Context, account uuid.UUID, limit *string, reason string) error {
	if limit != nil && !amountPattern.MatchString(*limit) {
		return invalid("monthly_limit must be a positive decimal with at most 6 decimals")
	}
	_, err := t.Exec(ctx, `INSERT INTO eacp.budget_soft_limits (tenant_id, account_id, monthly_limit, reason)
		VALUES (eacp.current_tenant_id(), $1, $2::numeric, $3)
		ON CONFLICT (tenant_id, account_id) DO UPDATE SET monthly_limit = EXCLUDED.monthly_limit,
		reason = EXCLUDED.reason`, account, limit, reason)
	return err
}
