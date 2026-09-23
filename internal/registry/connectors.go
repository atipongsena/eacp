package registry

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// NewConnector describes a connector. SecretRef names a secret the execution
// worker resolves together with the tenant id; it is never a secret value.
type NewConnector struct {
	Name      string `json:"name"`
	Protocol  string `json:"protocol"`
	Endpoint  string `json:"endpoint"`
	SecretRef string `json:"secret_ref"`
}

// Connector is a registered connector and the names of its tools.
type Connector struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Protocol  string    `json:"protocol"`
	Endpoint  string    `json:"endpoint"`
	SecretRef string    `json:"secret_ref"`
	Tools     []string  `json:"tools"`
}

// Contract is an operator-declared connector contract (MASTER_PLAN §31,
// ADR-003 §4). The database validates every field.
type Contract struct {
	SideEffects               []string `json:"side_effects"`
	IdempotencyMode           string   `json:"idempotency_mode"`
	IdempotencyKeyField       string   `json:"idempotency_key_field,omitempty"`
	CorrelationField          string   `json:"correlation_field,omitempty"`
	ReconciliationLookup      string   `json:"reconciliation_lookup"`
	ReconciliationConsistency string   `json:"reconciliation_consistency"`
	ProofStandard             string   `json:"proof_standard"`
	NoEffectErrors            []string `json:"no_effect_errors,omitempty"`
	MaxAttempts               int      `json:"max_attempts"`
	TimeoutMS                 int      `json:"timeout_ms,omitempty"`
	ConcurrencyGroup          string   `json:"concurrency_group,omitempty"`
	MaxInflight               int      `json:"max_inflight,omitempty"`
	SchedulePriority          int      `json:"schedule_priority,omitempty"`
	DataSensitivity           string   `json:"data_sensitivity,omitempty"`

	// Cost (ADR-012 §1). Without CostUnit the tool is not budgeted. A call
	// costs CostFixed plus the enforced payload's CostAmountField, and
	// CostUnitField (if set) must name CostUnit.
	CostUnit        string      `json:"cost_unit,omitempty"`
	CostFixed       json.Number `json:"cost_fixed,omitempty"`
	CostAmountField string      `json:"cost_amount_field,omitempty"`
	CostUnitField   string      `json:"cost_unit_field,omitempty"`
}

// RegisterConnector registers an immutable connector (registry_editor).
func (s *Service) RegisterConnector(ctx context.Context, a Actor, n NewConnector) (Connector, error) {
	c := Connector{Name: n.Name, Protocol: n.Protocol, Endpoint: n.Endpoint, SecretRef: n.SecretRef, Tools: []string{}}
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO eacp.connectors (tenant_id, name, protocol, endpoint, secret_ref)
			VALUES (eacp.current_tenant_id(), $1, $2, $3, $4) RETURNING id`,
			n.Name, n.Protocol, n.Endpoint, n.SecretRef).Scan(&c.ID)
		return err
	})
	return c, err
}

// ListConnectors lists connectors with their tool names.
func (s *Service) ListConnectors(ctx context.Context, a Actor) ([]Connector, error) {
	var out []Connector
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT c.id, c.name, c.protocol, c.endpoint, c.secret_ref,
			       COALESCE(array_agg(t.name ORDER BY t.name) FILTER (WHERE t.id IS NOT NULL), '{}')
			FROM eacp.connectors c
			LEFT JOIN eacp.tools t ON t.tenant_id = c.tenant_id AND t.connector_id = c.id
			GROUP BY c.id, c.name, c.protocol, c.endpoint, c.secret_ref
			ORDER BY c.name`)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Connector, error) {
			var c Connector
			err := r.Scan(&c.ID, &c.Name, &c.Protocol, &c.Endpoint, &c.SecretRef, &c.Tools)
			return c, err
		})
		return err
	})
	if out == nil {
		out = []Connector{}
	}
	return out, err
}

// RegisterTool registers a tool on a connector. It cannot execute until a
// contract is activated for it.
func (s *Service) RegisterTool(ctx context.Context, a Actor, connectorID uuid.UUID, name string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO eacp.tools (tenant_id, connector_id, name)
			VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, connectorID, name).Scan(&id)
		return err
	})
	return id, err
}

// ProposeContract records an immutable contract version for a tool.
func (s *Service) ProposeContract(ctx context.Context, a Actor, toolID uuid.UUID, c Contract) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		noEffect := c.NoEffectErrors
		if noEffect == nil {
			noEffect = []string{}
		}
		err := tx.QueryRow(ctx, `
			INSERT INTO eacp.tool_contracts
			    (tenant_id, tool_id, side_effects, idempotency_mode, idempotency_key_field, correlation_field,
			     reconciliation_lookup, reconciliation_consistency, proof_standard, no_effect_errors,
			     max_attempts, timeout_ms, concurrency_group, max_inflight, data_sensitivity, schedule_priority,
			     cost_unit, cost_fixed, cost_amount_field, cost_unit_field)
			VALUES (eacp.current_tenant_id(), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
			        $15, $16, COALESCE($17::numeric, 0), $18, $19)
			RETURNING id`,
			toolID, c.SideEffects, c.IdempotencyMode, nullStr(c.IdempotencyKeyField), nullStr(c.CorrelationField),
			c.ReconciliationLookup, c.ReconciliationConsistency, c.ProofStandard, noEffect,
			c.MaxAttempts, nullInt(c.TimeoutMS), nullStr(c.ConcurrencyGroup), nullInt(c.MaxInflight),
			nullStr(c.DataSensitivity), c.SchedulePriority, nullStr(c.CostUnit), nullStr(string(c.CostFixed)),
			nullStr(c.CostAmountField), nullStr(c.CostUnitField)).Scan(&id)
		return err
	})
	return id, err
}

// ActivateContract makes a contract the tool's active one (two-person).
func (s *Service) ActivateContract(ctx context.Context, a Actor, toolID, contractID uuid.UUID) error {
	return s.change(ctx, a, func(tx pgx.Tx) error {
		err := execOne(ctx, tx, `UPDATE eacp.tools SET active_contract_id = $2 WHERE id = $1`, toolID, contractID)
		return err
	})
}

// RevokeContract permanently revokes a contract; its tool stops being
// executable until another contract is activated.
func (s *Service) RevokeContract(ctx context.Context, a Actor, contractID uuid.UUID, reason string) error {
	return s.change(ctx, a, func(tx pgx.Tx) error {
		err := execOne(ctx, tx, `UPDATE eacp.tool_contracts SET revoked_at = now(), revoke_reason = $2 WHERE id = $1`,
			contractID, reason)
		return err
	})
}

// splitToolRef parses "connector.tool".
func splitToolRef(ref string) (connector, tool string, ok bool) {
	connector, tool, ok = strings.Cut(ref, ".")
	return connector, tool, ok && connector != "" && tool != "" && !strings.Contains(tool, ".")
}

func resolveTool(ctx context.Context, tx pgx.Tx, ref string) (uuid.UUID, error) {
	conn, tool, ok := splitToolRef(ref)
	if !ok {
		return uuid.Nil, newErr(ErrInvalid, "tool reference %q is not connector.tool", ref)
	}
	var id uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT t.id FROM eacp.tools t
		JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
		WHERE c.name = $1 AND t.name = $2`, conn, tool).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, newErr(ErrNotFound, "unknown tool %q", ref)
	}
	return id, err
}
