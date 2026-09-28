package identity

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atipongsena/eacp/internal/storage"
)

// ErrUnauthenticated matches every authentication failure.
var ErrUnauthenticated = errors.New("identity: authentication failed")

// AuthError is an authentication failure. Reason is a code for logs only;
// callers must answer every failure identically (a generic 401).
type AuthError struct {
	Reason string
}

func (e *AuthError) Error() string        { return ErrUnauthenticated.Error() }
func (e *AuthError) Is(target error) bool { return target == ErrUnauthenticated }

func fail(reason string) error { return &AuthError{Reason: reason} }

// Caller is an authenticated principal or agent runtime.
type Caller struct {
	Kind         Kind
	TenantID     uuid.UUID
	CredentialID uuid.UUID

	// Principal keys.
	PrincipalID uuid.UUID
	Roles       []string // effective (approved, unrevoked) roles, sorted

	// Agent keys. An agent authenticates in any non-terminal state but may
	// act only when AgentState is ACTIVE (ADR-003 §5).
	AgentID        uuid.UUID
	AgentVersionID uuid.UUID
	AgentState     string
}

// HasRole reports whether the caller holds any of roles.
func (c Caller) HasRole(roles ...string) bool {
	for _, r := range roles {
		if slices.Contains(c.Roles, r) {
			return true
		}
	}
	return false
}

// Authenticate resolves an API key to a caller. The tenant id embedded in the
// key sets the Row-Level Security context before the credential is read, so
// a key can only ever match a credential of its own tenant.
func Authenticate(ctx context.Context, pool *pgxpool.Pool, key string) (Caller, error) {
	parsed, err := ParseKey(key)
	if err != nil {
		return Caller{}, fail("malformed")
	}
	c := Caller{Kind: parsed.Kind, TenantID: parsed.TenantID, CredentialID: parsed.CredentialID}

	err = storage.InTenantTx(ctx, pool, parsed.TenantID.String(), func(tx pgx.Tx) error {
		var (
			kind                            string
			hash                            []byte
			approved, revoked, expired      bool
			principalID, versionID, agentID *uuid.UUID
			principalDisabled               *bool
			state                           *string
		)
		err := tx.QueryRow(ctx, `
			SELECT c.kind, c.secret_hash,
			       c.approved_at IS NOT NULL, c.revoked_at IS NOT NULL, c.expires_at <= now(),
			       c.principal_id, p.disabled_at IS NOT NULL,
			       c.agent_version_id, v.agent_id, v.state
			FROM eacp.credentials c
			LEFT JOIN eacp.principals p ON p.tenant_id = c.tenant_id AND p.id = c.principal_id
			LEFT JOIN eacp.agent_versions v ON v.tenant_id = c.tenant_id AND v.id = c.agent_version_id
			WHERE c.id = $1`, parsed.CredentialID,
		).Scan(&kind, &hash, &approved, &revoked, &expired,
			&principalID, &principalDisabled, &versionID, &agentID, &state)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return fail("unknown_credential")
		case err != nil:
			return fmt.Errorf("identity: read credential: %w", err)
		}

		// Prove possession of the secret before revealing anything else.
		if !parsed.Matches(hash) {
			return fail("secret_mismatch")
		}
		switch {
		case Kind(kind) != parsed.Kind:
			return fail("kind_mismatch")
		case !approved:
			return fail("unapproved")
		case revoked:
			return fail("revoked")
		case expired:
			return fail("expired")
		}

		if parsed.Kind == KindPrincipal {
			if principalDisabled == nil || *principalDisabled {
				return fail("principal_disabled")
			}
			c.PrincipalID = *principalID
			rows, err := tx.Query(ctx, `
				SELECT role FROM eacp.role_grants
				WHERE principal_id = $1 AND approved_at IS NOT NULL AND revoked_at IS NULL
				ORDER BY role`, c.PrincipalID)
			if err != nil {
				return fmt.Errorf("identity: read roles: %w", err)
			}
			c.Roles, err = pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return fmt.Errorf("identity: read roles: %w", err)
			}
			return nil
		}

		if state == nil || *state == "RETIRED" || *state == "REVOKED" {
			return fail("agent_version_terminal")
		}
		c.AgentVersionID, c.AgentID, c.AgentState = *versionID, *agentID, *state
		return nil
	})
	if err != nil {
		return Caller{}, err
	}
	return c, nil
}
