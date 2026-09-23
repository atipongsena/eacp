package governance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"eacp/internal/registry"
	"eacp/internal/storage"
)

type Policy struct {
	ID      uuid.UUID       `json:"id"`
	Version int             `json:"version"`
	Content json.RawMessage `json:"content"`
}

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// CreatePolicy validates the exact local bundle before storing an immutable
// version. PostgreSQL assigns the version under the tenant pointer lock.
func (s *Store) CreatePolicy(ctx context.Context, actor registry.Actor, content json.RawMessage) (Policy, error) {
	var out Policy
	if actor.TenantID == uuid.Nil || actor.PrincipalID == uuid.Nil {
		return out, &registry.Error{Kind: registry.ErrForbidden, Msg: "no actor"}
	}
	if err := ValidatePolicy(content); err != nil {
		return out, &registry.Error{Kind: registry.ErrInvalid, Msg: err.Error()}
	}
	err := storage.InTenantTx(ctx, s.pool, actor.TenantID.String(), func(tx pgx.Tx) error {
		if err := storage.SetActor(ctx, tx, actor.PrincipalID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO eacp.policy_bundles (tenant_id, content)
			VALUES (eacp.current_tenant_id(), $1::jsonb) RETURNING id, version`, content).Scan(&out.ID, &out.Version)
	})
	out.Content = content
	return out, storeErr(err)
}

func (s *Store) ActivatePolicy(ctx context.Context, actor registry.Actor, id uuid.UUID, reason string) error {
	if actor.TenantID == uuid.Nil || actor.PrincipalID == uuid.Nil {
		return &registry.Error{Kind: registry.ErrForbidden, Msg: "no actor"}
	}
	err := storage.InTenantTx(ctx, s.pool, actor.TenantID.String(), func(tx pgx.Tx) error {
		if err := storage.SetActor(ctx, tx, actor.PrincipalID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE eacp.tenant_policy_pointer
			SET current_bundle_id = $1, activation_reason = $2
			WHERE tenant_id = eacp.current_tenant_id()`, id, reason)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return pgx.ErrNoRows
		}
		return nil
	})
	return storeErr(err)
}

// CurrentPolicy reads the active pointer under FOR SHARE. A missing or
// revoked policy is unavailable, which keeps evaluation fail closed.
func (s *Store) CurrentPolicy(ctx context.Context, tenantID uuid.UUID) (Policy, error) {
	var out Policy
	if tenantID == uuid.Nil {
		return out, &registry.Error{Kind: registry.ErrInvalid, Msg: "tenant required"}
	}
	err := storage.InTenantTx(ctx, s.pool, tenantID.String(), func(tx pgx.Tx) error {
		var content string
		err := tx.QueryRow(ctx, `SELECT b.id, b.version, b.content::text
			FROM eacp.tenant_policy_pointer p
			JOIN eacp.policy_bundles b ON b.tenant_id = p.tenant_id AND b.id = p.current_bundle_id
			WHERE p.tenant_id = eacp.current_tenant_id() AND b.revoked_at IS NULL
			FOR SHARE OF p, b`).Scan(&out.ID, &out.Version, &content)
		out.Content = json.RawMessage(content)
		return err
	})
	return out, storeErr(err)
}

// RecordDecision must run in the action transaction. The evidence record is
// immutable and its insertion must succeed before any action transition.
func RecordDecision(ctx context.Context, tx pgx.Tx, actionID uuid.UUID, d GovernanceDecision) (uuid.UUID, error) {
	if actionID == uuid.Nil || d.PolicyBundleID == uuid.Nil || d.DecisionID == uuid.Nil ||
		d.Provider == "" || d.ProviderInstanceID == "" || len(d.Reasons) == 0 ||
		d.EvaluatedAt.IsZero() || len(d.EnforcedPayload) == 0 {
		return uuid.Nil, &registry.Error{Kind: registry.ErrInvalid, Msg: "incomplete decision evidence"}
	}
	var quorum, ttl any
	var roles any
	if d.Approval != nil {
		quorum, ttl, roles = d.Approval.Quorum, d.Approval.TTLSeconds, d.Approval.EligibleRoles
	}
	var id uuid.UUID
	err := tx.QueryRow(ctx, `INSERT INTO eacp.decision_evidence
		(tenant_id, action_id, policy_bundle_id, policy_version, provider, provider_instance_id,
		 decision_id, verdict, reasons, input_digest, enforced_digest, enforced_payload,
		 required_quorum, eligible_roles, approval_ttl_seconds, evaluated_at)
		VALUES (eacp.current_tenant_id(), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
		        $11::jsonb, $12, $13, $14, $15) RETURNING id`,
		actionID, d.PolicyBundleID, d.PolicyVersion, d.Provider, d.ProviderInstanceID,
		d.DecisionID, string(d.Verdict), d.Reasons, d.InputDigest[:], d.EnforcedDigest[:],
		d.EnforcedPayload, quorum, roles, ttl, d.EvaluatedAt).Scan(&id)
	return id, storeErr(err)
}

func storeErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return &registry.Error{Kind: registry.ErrNotFound, Msg: "no such policy"}
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	var kind error
	switch pgErr.Code {
	case "42501":
		kind = registry.ErrForbidden
	case "55000", "23505":
		kind = registry.ErrConflict
	case "23503":
		kind = registry.ErrNotFound
	case "23514", "23502", "22P02", "22023", "22001", "22007", "22008":
		kind = registry.ErrInvalid
	default:
		return fmt.Errorf("governance: %w", err)
	}
	return &registry.Error{Kind: kind, Msg: pgErr.Message}
}
