package registry

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Tx makes registry writes in a caller's transaction, which must already
// have its tenant and actor set (storage.InTenantTx, storage.SetActor).
// Service uses it for its one-transaction changes, and a bundle change set
// (ADR-026) uses it so that each step is exactly the write the API makes:
// the registry triggers decide both the same way. Errors are the raw
// database errors; map them with MapErr.
type Tx struct{ pgx.Tx }

// MapErr maps a registry database error to the error kinds of this package.
func MapErr(err error) error { return mapErr(err) }

// RegisterConnector inserts an immutable connector.
func (t Tx) RegisterConnector(ctx context.Context, n NewConnector) (Connector, error) {
	c := Connector{Name: n.Name, Protocol: n.Protocol, Endpoint: n.Endpoint, SecretRef: n.SecretRef, Tools: []string{}}
	err := t.QueryRow(ctx, `
		INSERT INTO eacp.connectors (tenant_id, name, protocol, endpoint, secret_ref)
		VALUES (eacp.current_tenant_id(), $1, $2, $3, $4) RETURNING id`,
		n.Name, n.Protocol, n.Endpoint, n.SecretRef).Scan(&c.ID)
	return c, err
}

// RegisterTool inserts a tool on a connector.
func (t Tx) RegisterTool(ctx context.Context, connectorID uuid.UUID, name string) (uuid.UUID, error) {
	var id uuid.UUID
	err := t.QueryRow(ctx, `INSERT INTO eacp.tools (tenant_id, connector_id, name)
		VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, connectorID, name).Scan(&id)
	return id, err
}

// ProposeContract inserts an immutable contract version for a tool.
func (t Tx) ProposeContract(ctx context.Context, toolID uuid.UUID, c Contract) (uuid.UUID, error) {
	noEffect := c.NoEffectErrors
	if noEffect == nil {
		noEffect = []string{}
	}
	var id uuid.UUID
	err := t.QueryRow(ctx, `
		INSERT INTO eacp.tool_contracts
		    (tenant_id, tool_id, side_effects, idempotency_mode, idempotency_key_field, correlation_field,
		     reconciliation_lookup, reconciliation_consistency, proof_standard, no_effect_errors,
		     max_attempts, timeout_ms, concurrency_group, max_inflight, data_sensitivity, schedule_priority,
		     cost_unit, cost_fixed, cost_amount_field, cost_unit_field,
		     max_queued, retry_max_elapsed_ms, retry_max_cost, definition_id)
		VALUES (eacp.current_tenant_id(), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
		        $15, $16, COALESCE($17::numeric, 0), $18, $19, $20, $21, $22::numeric, $23)
		RETURNING id`,
		toolID, c.SideEffects, c.IdempotencyMode, nullStr(c.IdempotencyKeyField), nullStr(c.CorrelationField),
		c.ReconciliationLookup, c.ReconciliationConsistency, c.ProofStandard, noEffect,
		c.MaxAttempts, nullInt(c.TimeoutMS), nullStr(c.ConcurrencyGroup), nullInt(c.MaxInflight),
		nullStr(c.DataSensitivity), c.SchedulePriority, nullStr(c.CostUnit), nullStr(string(c.CostFixed)),
		nullStr(c.CostAmountField), nullStr(c.CostUnitField),
		nullInt(c.MaxQueued), nullInt(c.RetryMaxElapsedMS), nullStr(string(c.RetryMaxCost)), c.DefinitionID).Scan(&id)
	return id, err
}

// ActivateContract makes a contract the tool's active one (two-person).
func (t Tx) ActivateContract(ctx context.Context, toolID, contractID uuid.UUID) error {
	return execOne(ctx, t.Tx, `UPDATE eacp.tools SET active_contract_id = $2 WHERE id = $1`, toolID, contractID)
}

// RevokeContract permanently revokes a contract.
func (t Tx) RevokeContract(ctx context.Context, contractID uuid.UUID, reason string) error {
	return execOne(ctx, t.Tx, `UPDATE eacp.tool_contracts SET revoked_at = now(), revoke_reason = $2 WHERE id = $1`,
		contractID, reason)
}

// RegisterAgent inserts an immutable agent.
func (t Tx) RegisterAgent(ctx context.Context, n NewAgent) (Agent, error) {
	return scanAgent(t.QueryRow(ctx, `
		INSERT INTO eacp.agents (tenant_id, name, display_name, environment, risk_class, owner_principal_id, owner_group_id)
		VALUES (eacp.current_tenant_id(), $1, $2, $3, $4, $5, $6)
		RETURNING `+agentColumns,
		n.Name, n.DisplayName, n.Environment, n.RiskClass, nullID(n.OwnerPrincipalID), nullID(n.OwnerGroupID)))
}

// RegisterVersion inserts a REGISTERED version (the database numbers it).
func (t Tx) RegisterVersion(ctx context.Context, agentID uuid.UUID, n NewVersion) (Version, error) {
	v := Version{AgentID: agentID, Runtime: n.Runtime, CodeRef: n.CodeRef, AllowedTools: []string{}}
	err := t.QueryRow(ctx, `
		INSERT INTO eacp.agent_versions (tenant_id, agent_id, runtime, code_ref)
		VALUES (eacp.current_tenant_id(), $1, $2, $3) RETURNING id, version, state`,
		agentID, n.Runtime, n.CodeRef).Scan(&v.ID, &v.Number, &v.State)
	return v, err
}

// ProposeAllowlist inserts an immutable allowlist of "connector.tool" refs.
func (t Tx) ProposeAllowlist(ctx context.Context, versionID uuid.UUID, tools []string) (uuid.UUID, error) {
	ids := make([]uuid.UUID, 0, len(tools))
	for _, ref := range tools {
		toolID, err := resolveTool(ctx, t.Tx, ref)
		if err != nil {
			return uuid.Nil, err
		}
		ids = append(ids, toolID)
	}
	var id uuid.UUID
	err := t.QueryRow(ctx, `
		INSERT INTO eacp.agent_allowlists (tenant_id, agent_version_id, tool_ids)
		VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, versionID, ids).Scan(&id)
	return id, err
}

// ActivateAllowlist points a version at an allowlist (two-person).
func (t Tx) ActivateAllowlist(ctx context.Context, versionID, allowlistID uuid.UUID) error {
	return execOne(ctx, t.Tx, `UPDATE eacp.agent_versions SET active_allowlist_id = $2 WHERE id = $1`,
		versionID, allowlistID)
}

// TransitionVersion moves a version through its lifecycle (ADR-003 §2).
func (t Tx) TransitionVersion(ctx context.Context, versionID uuid.UUID, to State, reason string) error {
	var from State
	if err := t.QueryRow(ctx, `SELECT state FROM eacp.agent_versions WHERE id = $1`, versionID).Scan(&from); err != nil {
		return err
	}
	if from == to {
		return newErr(ErrConflict, "version is already %s", to)
	}
	return execOne(ctx, t.Tx, `UPDATE eacp.agent_versions SET state = $2, state_reason = $3 WHERE id = $1`,
		versionID, string(to), reason)
}

// TransitionVersionFrom moves a version only while it is still in from. A
// version another transaction moved meanwhile, even one committed while this
// statement waited for the row, is a conflict (ADR-026).
func (t Tx) TransitionVersionFrom(ctx context.Context, versionID uuid.UUID, from, to State, reason string) error {
	tag, err := t.Exec(ctx, `UPDATE eacp.agent_versions SET state = $2, state_reason = $3 WHERE id = $1 AND state = $4`,
		versionID, string(to), reason, string(from))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return newErr(ErrConflict, "version is no longer %s", from)
	}
	return nil
}

// CreatePrincipal inserts a principal; it holds no roles until two admins
// grant one. A human's subject is canonicalised to lower case.
func (t Tx) CreatePrincipal(ctx context.Context, p NewPrincipal) (Principal, error) {
	out := Principal{Kind: p.Kind, Name: p.Name, DisplayName: p.DisplayName,
		Subject: strings.ToLower(strings.TrimSpace(p.Subject))}
	err := t.QueryRow(ctx, `
		INSERT INTO eacp.principals (tenant_id, kind, name, subject, display_name)
		VALUES (eacp.current_tenant_id(), $1, $2, $3, $4) RETURNING id`,
		out.Kind, out.Name, nullStr(out.Subject), out.DisplayName).Scan(&out.ID)
	return out, err
}

// ProposeRole proposes granting role to a principal; a second admin approves.
func (t Tx) ProposeRole(ctx context.Context, principalID uuid.UUID, role string) (uuid.UUID, error) {
	var id uuid.UUID
	err := t.QueryRow(ctx, `INSERT INTO eacp.role_grants (tenant_id, principal_id, role)
		VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, principalID, role).Scan(&id)
	return id, err
}

// ApproveRole makes a proposed grant effective.
func (t Tx) ApproveRole(ctx context.Context, grantID uuid.UUID) error {
	return execOne(ctx, t.Tx, `UPDATE eacp.role_grants SET approved_at = now() WHERE id = $1`, grantID)
}

// RevokeRole permanently revokes a grant.
func (t Tx) RevokeRole(ctx context.Context, grantID uuid.UUID, reason string) error {
	return execOne(ctx, t.Tx, `UPDATE eacp.role_grants SET revoked_at = now(), revoke_reason = $2 WHERE id = $1`,
		grantID, reason)
}

// CreateGroup inserts a group with its scheduler claim quantum.
func (t Tx) CreateGroup(ctx context.Context, name, displayName string, weight int) (uuid.UUID, error) {
	var id uuid.UUID
	err := t.QueryRow(ctx, `INSERT INTO eacp.groups (tenant_id, name, display_name, schedule_weight)
		VALUES (eacp.current_tenant_id(), $1, $2, $3) RETURNING id`, name, displayName, weight).Scan(&id)
	return id, err
}

// AddMember adds a principal to a group and returns the membership id.
func (t Tx) AddMember(ctx context.Context, groupID, principalID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := t.QueryRow(ctx, `INSERT INTO eacp.group_memberships (tenant_id, group_id, principal_id)
		VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, groupID, principalID).Scan(&id)
	return id, err
}

// RemoveMember closes a membership.
func (t Tx) RemoveMember(ctx context.Context, membershipID uuid.UUID, reason string) error {
	return execOne(ctx, t.Tx, `UPDATE eacp.group_memberships SET removed_at = now(), remove_reason = $2 WHERE id = $1`,
		membershipID, reason)
}
