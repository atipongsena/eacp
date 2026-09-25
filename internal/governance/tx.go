package governance

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/registry"
)

// Tx makes policy writes in a caller's transaction, which must already have
// its tenant and actor set. Store uses it, and a bundle change set (ADR-026)
// uses it, so the policy triggers decide both the same way. Errors are raw
// database errors, except content that fails validation.
type Tx struct{ pgx.Tx }

// CreatePolicy validates the exact local bundle, then stores an immutable
// version. PostgreSQL assigns the version under the tenant pointer lock.
func (t Tx) CreatePolicy(ctx context.Context, content json.RawMessage) (Policy, error) {
	out := Policy{Content: content}
	if err := ValidatePolicy(content); err != nil {
		return out, &registry.Error{Kind: registry.ErrInvalid, Msg: err.Error()}
	}
	err := t.QueryRow(ctx, `INSERT INTO eacp.policy_bundles (tenant_id, content)
		VALUES (eacp.current_tenant_id(), $1::jsonb) RETURNING id, version`, content).Scan(&out.ID, &out.Version)
	return out, err
}

// ActivatePolicy points the tenant at policy id. The pointer trigger needs an
// admin who did not author it and a higher version.
func (t Tx) ActivatePolicy(ctx context.Context, id uuid.UUID, reason string) error {
	tag, err := t.Exec(ctx, `UPDATE eacp.tenant_policy_pointer
		SET current_bundle_id = $1, activation_reason = $2
		WHERE tenant_id = eacp.current_tenant_id()`, id, reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return nil
}
