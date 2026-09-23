package registry

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/identity"
)

// NewPrincipal describes a principal to create. Humans need a subject (IdP
// subject or email), which is canonicalised to lower case.
type NewPrincipal struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Subject     string `json:"subject,omitempty"`
	DisplayName string `json:"display_name"`
}

// Principal is a human or service identity.
type Principal struct {
	ID          uuid.UUID `json:"id"`
	Kind        string    `json:"kind"`
	Name        string    `json:"name"`
	Subject     string    `json:"subject,omitempty"`
	DisplayName string    `json:"display_name"`
}

// CreatePrincipal adds a principal. It holds no roles until two admins grant one.
func (s *Service) CreatePrincipal(ctx context.Context, a Actor, p NewPrincipal) (Principal, error) {
	out := Principal{Kind: p.Kind, Name: p.Name, DisplayName: p.DisplayName,
		Subject: strings.ToLower(strings.TrimSpace(p.Subject))}
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO eacp.principals (tenant_id, kind, name, subject, display_name)
			VALUES (eacp.current_tenant_id(), $1, $2, $3, $4) RETURNING id`,
			out.Kind, out.Name, nullStr(out.Subject), out.DisplayName).Scan(&out.ID)
		return err
	})
	return out, err
}

// DisablePrincipal permanently disables a principal.
func (s *Service) DisablePrincipal(ctx context.Context, a Actor, id uuid.UUID, reason string) error {
	return s.change(ctx, a, func(tx pgx.Tx) error {
		err := execOne(ctx, tx, `UPDATE eacp.principals SET disabled_at = now(), disable_reason = $2 WHERE id = $1`, id, reason)
		return err
	})
}

// ProposeRole proposes granting role to a principal; a second admin approves.
func (s *Service) ProposeRole(ctx context.Context, a Actor, principalID uuid.UUID, role string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO eacp.role_grants (tenant_id, principal_id, role)
			VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, principalID, role).Scan(&id)
		return err
	})
	return id, err
}

// ApproveRole makes a proposed grant effective.
func (s *Service) ApproveRole(ctx context.Context, a Actor, grantID uuid.UUID) error {
	return s.change(ctx, a, func(tx pgx.Tx) error {
		err := execOne(ctx, tx, `UPDATE eacp.role_grants SET approved_at = now() WHERE id = $1`, grantID)
		return err
	})
}

// RevokeRole permanently revokes a grant.
func (s *Service) RevokeRole(ctx context.Context, a Actor, grantID uuid.UUID, reason string) error {
	return s.change(ctx, a, func(tx pgx.Tx) error {
		err := execOne(ctx, tx, `UPDATE eacp.role_grants SET revoked_at = now(), revoke_reason = $2 WHERE id = $1`, grantID, reason)
		return err
	})
}

// CreateGroup adds a group.
func (s *Service) CreateGroup(ctx context.Context, a Actor, name, displayName string) (uuid.UUID, error) {
	return s.CreateGroupWeighted(ctx, a, name, displayName, 1)
}

// CreateGroupWeighted adds a group with its scheduler claim quantum.
func (s *Service) CreateGroupWeighted(ctx context.Context, a Actor, name, displayName string, weight int) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO eacp.groups (tenant_id, name, display_name, schedule_weight)
			VALUES (eacp.current_tenant_id(), $1, $2, $3) RETURNING id`, name, displayName, weight).Scan(&id)
		return err
	})
	return id, err
}

// AddMember adds a principal to a group and returns the membership id.
func (s *Service) AddMember(ctx context.Context, a Actor, groupID, principalID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO eacp.group_memberships (tenant_id, group_id, principal_id)
			VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, groupID, principalID).Scan(&id)
		return err
	})
	return id, err
}

// RemoveMember closes a membership.
func (s *Service) RemoveMember(ctx context.Context, a Actor, membershipID uuid.UUID, reason string) error {
	return s.change(ctx, a, func(tx pgx.Tx) error {
		err := execOne(ctx, tx, `UPDATE eacp.group_memberships SET removed_at = now(), remove_reason = $2 WHERE id = $1`,
			membershipID, reason)
		return err
	})
}

// NewCredential registers a key its holder generated (bring your own key):
// only the credential id and the SHA-256 of the secret are ever sent.
type NewCredential struct {
	ID             uuid.UUID     `json:"id"`
	Kind           identity.Kind `json:"kind"`
	PrincipalID    uuid.UUID     `json:"principal_id,omitempty"`
	AgentVersionID uuid.UUID     `json:"agent_version_id,omitempty"`
	Hash           []byte        `json:"hash"`
	ExpiresAt      time.Time     `json:"expires_at"`
}

// ProposeCredential registers a credential hash; a second person approves.
func (s *Service) ProposeCredential(ctx context.Context, a Actor, c NewCredential) error {
	if c.ID == uuid.Nil {
		return newErr(ErrInvalid, "credential id required")
	}
	return s.change(ctx, a, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO eacp.credentials (tenant_id, id, kind, principal_id, agent_version_id, secret_hash, expires_at)
			VALUES (eacp.current_tenant_id(), $1, $2, $3, $4, $5, $6)`,
			c.ID, string(c.Kind), nullID(c.PrincipalID), nullID(c.AgentVersionID), c.Hash, c.ExpiresAt)
		return err
	})
}

// ApproveCredential makes a proposed credential usable.
func (s *Service) ApproveCredential(ctx context.Context, a Actor, id uuid.UUID) error {
	return s.change(ctx, a, func(tx pgx.Tx) error {
		err := execOne(ctx, tx, `UPDATE eacp.credentials SET approved_at = now() WHERE id = $1`, id)
		return err
	})
}

// RevokeCredential permanently revokes a credential.
func (s *Service) RevokeCredential(ctx context.Context, a Actor, id uuid.UUID, reason string) error {
	return s.change(ctx, a, func(tx pgx.Tx) error {
		err := execOne(ctx, tx, `UPDATE eacp.credentials SET revoked_at = now(), revoke_reason = $2 WHERE id = $1`, id, reason)
		return err
	})
}
