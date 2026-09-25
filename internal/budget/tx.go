package budget

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/registry"
)

// Tx makes budget writes in a caller's transaction, which must already have
// its tenant and actor set. Service uses it, and a bundle change set
// (ADR-026) uses it, so the budget triggers decide both the same way. Errors
// are raw database errors, except an amount that fails validation.
type Tx struct{ pgx.Tx }

// CreateAccount inserts an empty account; its limit starts at zero.
func (t Tx) CreateAccount(ctx context.Context, n NewAccount) (uuid.UUID, error) {
	var id uuid.UUID
	err := t.QueryRow(ctx, `INSERT INTO eacp.budget_accounts (tenant_id, name, unit, parent_id, agent_id)
		VALUES (eacp.current_tenant_id(), $1, $2, $3, $4) RETURNING id`,
		n.Name, n.Unit, n.ParentID, n.AgentID).Scan(&id)
	return id, err
}

// ChangeLimit records a limit change: a decrease applies at once, an
// increase stays PROPOSED until a different admin applies it.
func (t Tx) ChangeLimit(ctx context.Context, account uuid.UUID, limit, reason string) (LimitChange, error) {
	if !amountPattern.MatchString(limit) {
		return LimitChange{}, &registry.Error{Kind: registry.ErrInvalid,
			Msg: "limit must be a non-negative decimal below 10^15 with at most 6 decimals"}
	}
	return scanChange(t.QueryRow(ctx, `INSERT INTO eacp.budget_limit_changes (tenant_id, account_id, new_limit, reason)
		VALUES (eacp.current_tenant_id(), $1, $2::numeric, $3) RETURNING `+changeColumns, account, limit, reason))
}

// DecideLimitChange applies or rejects an open increase.
func (t Tx) DecideLimitChange(ctx context.Context, change uuid.UUID, apply bool, reason string) (LimitChange, error) {
	state := "REJECTED"
	if apply {
		state = "APPLIED"
	}
	return scanChange(t.QueryRow(ctx, `UPDATE eacp.budget_limit_changes SET state = $2, decision_reason = $3
		WHERE id = $1 RETURNING `+changeColumns, change, state, reason))
}
