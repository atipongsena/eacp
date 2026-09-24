package registry

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Denial is why an agent version may not call a tool. Capability denials
// happen before governance is consulted (MASTER_PLAN §76).
type Denial string

const (
	DenyVersionNotActive    Denial = "agent_version_not_active"
	DenyUnknownTool         Denial = "unknown_tool"
	DenyNotInAllowlist      Denial = "tool_not_in_allowlist"
	DenyToolQuarantined     Denial = "tool_quarantined"
	DenyNoContract          Denial = "no_active_contract"
	DenyContractRevoked     Denial = "contract_revoked"
	DenyFingerprintMismatch Denial = "contract_fingerprint_mismatch"
)

// Grant is a passed capability check: the tool and the contract version the
// action must pin (ADR-004 principle 6).
type Grant struct {
	ToolID          uuid.UUID
	ContractID      uuid.UUID
	ContractVersion int
}

// CheckCapability decides whether agent version versionID may call toolRef
// ("connector.tool"). It runs inside the caller's tenant transaction and
// reads the version, tool and contract rows FOR SHARE, so a concurrent
// suspension, allowlist change or contract revocation waits for the caller's
// transaction to end (ADR-004 principle 7).
//
// Passing is necessary, not sufficient: the release boundary also evaluates
// governance under the locked tenant policy pointer (ADR-005 §5a).
func CheckCapability(ctx context.Context, tx pgx.Tx, versionID uuid.UUID, toolRef string) (Grant, Denial, error) {
	var state string
	var allowlist *uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT state, active_allowlist_id FROM eacp.agent_versions WHERE id = $1 FOR SHARE`, versionID,
	).Scan(&state, &allowlist)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Grant{}, DenyVersionNotActive, nil
	case err != nil:
		return Grant{}, "", err
	case state != string(StateActive) || allowlist == nil:
		return Grant{}, DenyVersionNotActive, nil
	}

	conn, tool, ok := splitToolRef(toolRef)
	if !ok {
		return Grant{}, DenyUnknownTool, nil
	}
	var g Grant
	var contract *uuid.UUID
	var quarantined bool
	err = tx.QueryRow(ctx, `
		SELECT t.id, t.active_contract_id, t.quarantined_at IS NOT NULL
		FROM eacp.tools t
		JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
		WHERE c.name = $1 AND t.name = $2
		FOR SHARE OF t`, conn, tool).Scan(&g.ToolID, &contract, &quarantined)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Grant{}, DenyUnknownTool, nil
	case err != nil:
		return Grant{}, "", err
	}

	var allowed bool
	err = tx.QueryRow(ctx, `SELECT $2::uuid = ANY (tool_ids) FROM eacp.agent_allowlists WHERE id = $1`,
		*allowlist, g.ToolID).Scan(&allowed)
	if err != nil {
		return Grant{}, "", err
	}
	if !allowed {
		return Grant{}, DenyNotInAllowlist, nil
	}
	if quarantined {
		return Grant{}, DenyToolQuarantined, nil
	}
	if contract == nil {
		return Grant{}, DenyNoContract, nil
	}

	var revoked, fingerprintOK bool
	err = tx.QueryRow(ctx, `
		SELECT revoked_at IS NOT NULL,
		       COALESCE(fingerprint = eacp.tool_fingerprint(tenant_id, tool_id), false),
		       version
		FROM eacp.tool_contracts WHERE id = $1 FOR SHARE`, *contract,
	).Scan(&revoked, &fingerprintOK, &g.ContractVersion)
	if err != nil {
		return Grant{}, "", err
	}
	switch {
	case revoked:
		return Grant{}, DenyContractRevoked, nil
	case !fingerprintOK:
		return Grant{}, DenyFingerprintMismatch, nil
	}
	g.ContractID = *contract
	return g, "", nil
}
