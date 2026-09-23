// Package approval exposes Phase 3 approval transactions. PostgreSQL triggers
// enforce eligibility, separation of duties, quorum and one-time consumption
// even when callers issue SQL outside these helpers.
package approval

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

	"eacp/internal/registry"
	"eacp/internal/storage"
)

var ErrGrantUnavailable = errors.New("approval grant unavailable")

type VoteDecision string

const (
	Approve VoteDecision = "APPROVE"
	Deny    VoteDecision = "DENY"
)

type VoteResult struct {
	ID    uuid.UUID `json:"id"`
	State string    `json:"request_state"`
}

type View struct {
	ID              uuid.UUID       `json:"id"`
	ActionID        uuid.UUID       `json:"action_id"`
	State           string          `json:"state"`
	EnforcedPayload json.RawMessage `json:"enforced_payload"`
	EnforcedDigest  string          `json:"enforced_digest"`
	PolicyVersion   int             `json:"policy_version"`
	RequiredQuorum  int             `json:"required_quorum"`
	ExpiresAt       time.Time       `json:"expires_at"`
}

type QueueItem struct {
	ID             uuid.UUID `json:"id"`
	ActionID       uuid.UUID `json:"action_id"`
	EnforcedDigest string    `json:"enforced_digest"`
	PolicyVersion  int       `json:"policy_version"`
	RequiredQuorum int       `json:"required_quorum"`
	ExpiresAt      time.Time `json:"expires_at"`
}

// NewRequest opens approval for an escalated action. The database derives
// the agent version, tool, subject, lifetime bound, policy and digest from
// the action and its decision evidence.
type NewRequest struct {
	ActionID           uuid.UUID
	DecisionEvidenceID uuid.UUID
	ExpiresAt          time.Time
}

type Service struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// ListEligible is a preview queue. Vote still rechecks every guard under
// locks; a concurrent change may remove a request before the vote arrives.
func (s *Service) ListEligible(ctx context.Context, a registry.Actor) ([]QueueItem, error) {
	if a.TenantID == uuid.Nil || a.PrincipalID == uuid.Nil {
		return nil, &registry.Error{Kind: registry.ErrForbidden, Msg: "no actor"}
	}
	out := make([]QueueItem, 0)
	err := storage.InTenantTx(ctx, s.pool, a.TenantID.String(), func(tx pgx.Tx) error {
		if err := storage.SetActor(ctx, tx, a.PrincipalID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT eacp.assert_role($1, 'approver')`, a.PrincipalID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT r.id, r.action_id, encode(r.enforced_digest, 'hex'),
			r.policy_version, r.required_quorum, r.expires_at
			FROM eacp.approval_requests r
			JOIN eacp.tenant_policy_pointer p ON p.tenant_id = r.tenant_id
			JOIN eacp.policy_bundles b ON b.tenant_id = r.tenant_id AND b.id = r.policy_bundle_id
			WHERE r.state = 'PENDING' AND r.expires_at > now() AND r.not_after > now()
			  AND p.current_version = r.policy_version AND b.revoked_at IS NULL
			  AND $1::uuid <> r.requesting_subject_id
			  AND $1::uuid IS DISTINCT FROM r.owner_principal_id
			  AND NOT ($1::uuid = ANY (r.owner_member_ids_snapshot))
			  AND NOT ($1::uuid = ANY (r.enabling_actor_ids))
			  AND NOT EXISTS (SELECT 1 FROM eacp.group_memberships gm
			    WHERE gm.tenant_id = r.tenant_id AND gm.group_id = r.owner_group_id
			      AND gm.principal_id = $1 AND gm.removed_at IS NULL)
			ORDER BY r.created_at, r.id LIMIT 100`, a.PrincipalID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item QueueItem
			if err := rows.Scan(&item.ID, &item.ActionID, &item.EnforcedDigest,
				&item.PolicyVersion, &item.RequiredQuorum, &item.ExpiresAt); err != nil {
				return err
			}
			out = append(out, item)
		}
		return rows.Err()
	})
	return out, mapErr(err)
}

// GetEligible shows an approver the exact payload and digest they would
// approve. A live role recheck and request/pointer locks protect the read;
// Vote repeats every eligibility check before recording a decision.
func (s *Service) GetEligible(ctx context.Context, a registry.Actor, id uuid.UUID) (View, error) {
	var out View
	if a.TenantID == uuid.Nil || a.PrincipalID == uuid.Nil {
		return out, &registry.Error{Kind: registry.ErrForbidden, Msg: "no actor"}
	}
	err := storage.InTenantTx(ctx, s.pool, a.TenantID.String(), func(tx pgx.Tx) error {
		if err := storage.SetActor(ctx, tx, a.PrincipalID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT eacp.assert_role($1, 'approver')`, a.PrincipalID); err != nil {
			return err
		}
		var payload string
		err := tx.QueryRow(ctx, `SELECT r.id, r.action_id, r.state, e.enforced_payload::text,
			encode(r.enforced_digest, 'hex'), r.policy_version, r.required_quorum, r.expires_at
			FROM eacp.approval_requests r
			JOIN eacp.decision_evidence e ON e.tenant_id = r.tenant_id AND e.id = r.decision_evidence_id
			JOIN eacp.tenant_policy_pointer p ON p.tenant_id = r.tenant_id
			JOIN eacp.policy_bundles b ON b.tenant_id = r.tenant_id AND b.id = r.policy_bundle_id
			WHERE r.id = $1 AND r.state = 'PENDING' AND r.expires_at > now()
			  AND r.not_after > now() AND p.current_version = r.policy_version
			  AND b.revoked_at IS NULL
			  AND $2::uuid <> r.requesting_subject_id
			  AND $2::uuid IS DISTINCT FROM r.owner_principal_id
			  AND NOT ($2::uuid = ANY (r.owner_member_ids_snapshot))
			  AND NOT ($2::uuid = ANY (r.enabling_actor_ids))
			  AND NOT EXISTS (SELECT 1 FROM eacp.group_memberships gm
			    WHERE gm.tenant_id = r.tenant_id AND gm.group_id = r.owner_group_id
			      AND gm.principal_id = $2 AND gm.removed_at IS NULL)
			FOR SHARE OF r, p, b`, id, a.PrincipalID).Scan(&out.ID, &out.ActionID,
			&out.State, &payload, &out.EnforcedDigest, &out.PolicyVersion,
			&out.RequiredQuorum, &out.ExpiresAt)
		out.EnforcedPayload = json.RawMessage(payload)
		return err
	})
	return out, mapErr(err)
}

// CreateRequest runs inside the action's transition transaction (ADR-004
// T4/T11). The database derives all approval binding and eligibility fields
// from the action, its decision evidence and the current registry; callers
// cannot supply them.
func CreateRequest(ctx context.Context, tx pgx.Tx, r NewRequest) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `INSERT INTO eacp.approval_requests
		(tenant_id, action_id, decision_evidence_id, expires_at)
		VALUES (eacp.current_tenant_id(), $1, $2, $3)
		RETURNING id`, r.ActionID, r.DecisionEvidenceID, r.ExpiresAt).Scan(&id)
	return id, mapErr(err)
}

// Vote records a human vote; the vote trigger rechecks the live role, group
// membership and request state under row locks, then changes request state
// and issues a grant if quorum is reached.
func (s *Service) Vote(ctx context.Context, a registry.Actor, requestID uuid.UUID,
	decision VoteDecision, reason string) (VoteResult, error) {
	var out VoteResult
	if a.TenantID == uuid.Nil || a.PrincipalID == uuid.Nil {
		return out, &registry.Error{Kind: registry.ErrForbidden, Msg: "no actor"}
	}
	if decision != Approve && decision != Deny {
		return out, &registry.Error{Kind: registry.ErrInvalid, Msg: "unknown vote decision"}
	}
	err := storage.InTenantTx(ctx, s.pool, a.TenantID.String(), func(tx pgx.Tx) error {
		if err := storage.SetActor(ctx, tx, a.PrincipalID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO eacp.approval_votes
			(tenant_id, request_id, decision, reason)
			VALUES (eacp.current_tenant_id(), $1, $2, $3) RETURNING id`,
			requestID, string(decision), reason).Scan(&out.ID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT state FROM eacp.approval_requests WHERE id = $1`, requestID).Scan(&out.State)
	})
	return out, mapErr(err)
}

// Consume must run in the SAME transaction that queues the action and writes
// its release evidence (ADR-005 §5a R1); a deferred database check rejects
// the commit otherwise. The predicates bind the action, enforced digest and
// policy version; a successful UPDATE can occur only once.
func Consume(ctx context.Context, tx pgx.Tx, actionID uuid.UUID,
	enforcedDigest [32]byte, policyVersion int) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `UPDATE eacp.approval_grants
		SET consumed_at = now(), consumed_by_action_id = $1
		WHERE action_id = $1 AND enforced_digest = $2 AND policy_version = $3
		  AND consumed_at IS NULL AND expires_at > now()
		RETURNING id`, actionID, enforcedDigest[:], policyVersion).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrGrantUnavailable
	}
	return id, mapErr(err)
}

func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return &registry.Error{Kind: registry.ErrNotFound, Msg: "no such approval"}
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
		return fmt.Errorf("approval: %w", err)
	}
	return &registry.Error{Kind: kind, Msg: pgErr.Message}
}
