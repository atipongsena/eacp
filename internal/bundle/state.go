package bundle

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/registry"
)

// State is the registry as one plan sees it, read in one snapshot.
type State struct {
	Managed          map[string]uuid.UUID      // this bundle's addresses
	ManagedElsewhere map[uuid.UUID]string      // object -> the other bundle managing it
	Connectors       map[string]ConnectorState // by name
	Agents           map[string]AgentState     // by name
	Principals       map[string]uuid.UUID      // enabled principals by name
	Groups           map[string]uuid.UUID      // by name
	AddressElsewhere map[string]string         // address -> the other bundle managing it
	TenantID         uuid.UUID                 // the tenant planned
	People           map[string]PrincipalState // every principal by name, disabled ones too
	GroupRows        map[string]GroupState     // by name
	Policy           PolicyState               // the active policy version, if any
	Budgets          map[string]BudgetState    // by name
	Prices           map[string]PriceState     // the price in effect, by provider + " " + model
	PriceKeys        map[uuid.UUID]string      // provider + " " + model of every managed price row, any bundle
}

// ConnectorState is a registered connector and its tools by name.
type ConnectorState struct {
	ID                            uuid.UUID
	Protocol, Endpoint, SecretRef string
	Tools                         map[string]ToolState
}

// ToolState is a tool and its active contract. ActiveContract is nil when
// the tool has none or it is revoked.
type ToolState struct {
	ID               uuid.UUID
	DefinitionID     *uuid.UUID
	Quarantined      bool
	ActiveContractID *uuid.UUID
	ActiveContract   *registry.Contract
}

// AgentState is an agent and its versions, newest first.
type AgentState struct {
	ID                                  uuid.UUID
	DisplayName, Environment, RiskClass string
	OwnerPrincipalID, OwnerGroupID      uuid.UUID
	Versions                            []VersionState
}

// VersionState is a version and its active allowlist, sorted.
type VersionState struct {
	ID               uuid.UUID
	Number           int
	Runtime, CodeRef string
	State            registry.State
	AllowedTools     []string
}

func loadState(ctx context.Context, tx pgx.Tx, bundle string) (State, error) {
	st := State{Managed: map[string]uuid.UUID{}, ManagedElsewhere: map[uuid.UUID]string{},
		Connectors: map[string]ConnectorState{}, Agents: map[string]AgentState{},
		Principals: map[string]uuid.UUID{}, Groups: map[string]uuid.UUID{},
		AddressElsewhere: map[string]string{}, People: map[string]PrincipalState{}, GroupRows: map[string]GroupState{},
		Budgets: map[string]BudgetState{}, Prices: map[string]PriceState{}, PriceKeys: map[uuid.UUID]string{}}

	rows, err := tx.Query(ctx, `SELECT br.address, br.object_id, b.name FROM eacp.bundle_resources br
		JOIN eacp.bundles b ON b.tenant_id = br.tenant_id AND b.id = br.bundle_id`)
	if err != nil {
		return st, err
	}
	var addr, owner string
	var id uuid.UUID
	if _, err := pgx.ForEachRow(rows, []any{&addr, &id, &owner}, func() error {
		if owner == bundle {
			st.Managed[addr] = id
		} else {
			st.ManagedElsewhere[id] = owner
			st.AddressElsewhere[addr] = owner
		}
		return nil
	}); err != nil {
		return st, err
	}

	rows, err = tx.Query(ctx, `SELECT id, name, protocol, endpoint, secret_ref FROM eacp.connectors`)
	if err != nil {
		return st, err
	}
	var c ConnectorState
	var name string
	if _, err := pgx.ForEachRow(rows, []any{&c.ID, &name, &c.Protocol, &c.Endpoint, &c.SecretRef}, func() error {
		c.Tools = map[string]ToolState{}
		st.Connectors[name] = c
		return nil
	}); err != nil {
		return st, err
	}

	rows, err = tx.Query(ctx, `
		SELECT t.id, c.name, t.name, t.definition_id, t.quarantined_at IS NOT NULL, t.active_contract_id,
		       ct.id IS NOT NULL AND ct.revoked_at IS NULL,
		       COALESCE(ct.side_effects, '{}'), COALESCE(ct.idempotency_mode, ''),
		       COALESCE(ct.idempotency_key_field, ''), COALESCE(ct.correlation_field, ''),
		       COALESCE(ct.reconciliation_lookup, ''), COALESCE(ct.reconciliation_consistency, ''),
		       COALESCE(ct.proof_standard, ''), COALESCE(ct.no_effect_errors, '{}'), COALESCE(ct.max_attempts, 0),
		       COALESCE(ct.timeout_ms, 0), COALESCE(ct.concurrency_group, ''), COALESCE(ct.max_inflight, 0),
		       COALESCE(ct.schedule_priority, 0), COALESCE(ct.data_sensitivity, ''), COALESCE(ct.cost_unit, ''),
		       COALESCE(ct.cost_fixed::text, '0'), COALESCE(ct.cost_amount_field, ''),
		       COALESCE(ct.cost_unit_field, ''), COALESCE(ct.max_queued, 0),
		       COALESCE(ct.retry_max_elapsed_ms, 0), COALESCE(ct.retry_max_cost::text, ''), ct.definition_id
		FROM eacp.tools t
		JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
		LEFT JOIN eacp.tool_contracts ct ON ct.tenant_id = t.tenant_id AND ct.id = t.active_contract_id`)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var ts ToolState
		var conn, tool, costFixed, retryCost string
		var live bool
		var ct registry.Contract
		if err := rows.Scan(&ts.ID, &conn, &tool, &ts.DefinitionID, &ts.Quarantined, &ts.ActiveContractID, &live,
			&ct.SideEffects, &ct.IdempotencyMode, &ct.IdempotencyKeyField, &ct.CorrelationField,
			&ct.ReconciliationLookup, &ct.ReconciliationConsistency, &ct.ProofStandard, &ct.NoEffectErrors,
			&ct.MaxAttempts, &ct.TimeoutMS, &ct.ConcurrencyGroup, &ct.MaxInflight, &ct.SchedulePriority,
			&ct.DataSensitivity, &ct.CostUnit, &costFixed, &ct.CostAmountField, &ct.CostUnitField, &ct.MaxQueued,
			&ct.RetryMaxElapsedMS, &retryCost, &ct.DefinitionID); err != nil {
			rows.Close()
			return st, err
		}
		if live {
			ct.CostFixed, ct.RetryMaxCost = json.Number(costFixed), json.Number(retryCost)
			ts.ActiveContract = &ct
		}
		st.Connectors[conn].Tools[tool] = ts
	}
	if err := rows.Err(); err != nil {
		return st, err
	}

	rows, err = tx.Query(ctx, `SELECT id, name, display_name, environment, risk_class,
		COALESCE(owner_principal_id, '00000000-0000-0000-0000-000000000000'),
		COALESCE(owner_group_id, '00000000-0000-0000-0000-000000000000') FROM eacp.agents`)
	if err != nil {
		return st, err
	}
	var ag AgentState
	byID := map[uuid.UUID]string{}
	if _, err := pgx.ForEachRow(rows, []any{&ag.ID, &name, &ag.DisplayName, &ag.Environment, &ag.RiskClass,
		&ag.OwnerPrincipalID, &ag.OwnerGroupID}, func() error {
		st.Agents[name] = ag
		byID[ag.ID] = name
		return nil
	}); err != nil {
		return st, err
	}

	rows, err = tx.Query(ctx, `
		SELECT v.agent_id, v.id, v.version, v.runtime, v.code_ref, v.state,
		       COALESCE((SELECT array_agg(c.name || '.' || t.name) FROM eacp.agent_allowlists al
		                 JOIN eacp.tools t ON t.tenant_id = al.tenant_id AND t.id = ANY (al.tool_ids)
		                 JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
		                 WHERE al.tenant_id = v.tenant_id AND al.id = v.active_allowlist_id), '{}')
		FROM eacp.agent_versions v ORDER BY v.agent_id, v.version DESC`)
	if err != nil {
		return st, err
	}
	var agentID uuid.UUID
	var v VersionState
	if _, err := pgx.ForEachRow(rows, []any{&agentID, &v.ID, &v.Number, &v.Runtime, &v.CodeRef, &v.State,
		&v.AllowedTools}, func() error {
		n := byID[agentID]
		a := st.Agents[n]
		x := v
		x.AllowedTools = slices.Clone(v.AllowedTools)
		slices.Sort(x.AllowedTools)
		a.Versions = append(a.Versions, x)
		st.Agents[n] = a
		return nil
	}); err != nil {
		return st, err
	}

	rows, err = tx.Query(ctx, `SELECT name, id FROM eacp.principals WHERE disabled_at IS NULL`)
	if err != nil {
		return st, err
	}
	if _, err := pgx.ForEachRow(rows, []any{&name, &id}, func() error { st.Principals[name] = id; return nil }); err != nil {
		return st, err
	}
	rows, err = tx.Query(ctx, `SELECT name, id FROM eacp.groups`)
	if err != nil {
		return st, err
	}
	if _, err := pgx.ForEachRow(rows, []any{&name, &id}, func() error { st.Groups[name] = id; return nil }); err != nil {
		return st, err
	}
	return st, loadGovernance(ctx, tx, &st)
}
