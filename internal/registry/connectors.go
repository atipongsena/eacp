package registry

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

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
	// DefinitionID pins the reviewed definition of a discovered MCP tool; it
	// must be the tool's current one (ADR-023 §6). HTTP tools pin none.
	DefinitionID *uuid.UUID `json:"definition_id,omitempty"`

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

	// Backpressure and retry budget (ADR-022 §1, §5). MaxQueued bounds the
	// released, unfinished actions of the tool's capacity group. A retry is
	// granted only within RetryMaxElapsedMS of the first dispatch intent,
	// and while the cost of retries stays within RetryMaxCost (in CostUnit).
	MaxQueued         int         `json:"max_queued,omitempty"`
	RetryMaxElapsedMS int         `json:"retry_max_elapsed_ms,omitempty"`
	RetryMaxCost      json.Number `json:"retry_max_cost,omitempty"`

	// Cost (ADR-012 §1). Without CostUnit the tool is not budgeted. A call
	// costs CostFixed plus the enforced payload's CostAmountField, and
	// CostUnitField (if set) must name CostUnit.
	CostUnit        string      `json:"cost_unit,omitempty"`
	CostFixed       json.Number `json:"cost_fixed,omitempty"`
	CostAmountField string      `json:"cost_amount_field,omitempty"`
	CostUnitField   string      `json:"cost_unit_field,omitempty"`

	// ResultRetentionSeconds keeps a success's output for the calling agent
	// that long (60 to 86 400; ADR-034). Zero keeps none.
	ResultRetentionSeconds int `json:"result_retention_seconds,omitempty"`
}

// RegisterConnector registers an immutable connector (registry_editor).
func (s *Service) RegisterConnector(ctx context.Context, a Actor, n NewConnector) (Connector, error) {
	var c Connector
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		var err error
		c, err = Tx{tx}.RegisterConnector(ctx, n)
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
		var err error
		id, err = Tx{tx}.RegisterTool(ctx, connectorID, name)
		return err
	})
	return id, err
}

// ProposeContract records an immutable contract version for a tool.
func (s *Service) ProposeContract(ctx context.Context, a Actor, toolID uuid.UUID, c Contract) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		var err error
		id, err = Tx{tx}.ProposeContract(ctx, toolID, c)
		return err
	})
	return id, err
}

// ActivateContract makes a contract the tool's active one (two-person).
func (s *Service) ActivateContract(ctx context.Context, a Actor, toolID, contractID uuid.UUID) error {
	return s.change(ctx, a, func(tx pgx.Tx) error { return Tx{tx}.ActivateContract(ctx, toolID, contractID) })
}

// RevokeContract permanently revokes a contract; its tool stops being
// executable until another contract is activated.
func (s *Service) RevokeContract(ctx context.Context, a Actor, contractID uuid.UUID, reason string) error {
	return s.change(ctx, a, func(tx pgx.Tx) error { return Tx{tx}.RevokeContract(ctx, contractID, reason) })
}

// Circuit is a connector's shared circuit (ADR-022 §3). Open is true while
// it is disabled or a worker's breaker keeps it open (OpenUntil ahead):
// no action on the connector is then claimed or dispatched.
type Circuit struct {
	ConnectorID     uuid.UUID  `json:"connector_id"`
	Open            bool       `json:"open"`
	Disabled        bool       `json:"disabled"`
	OpenUntil       *time.Time `json:"open_until,omitempty"`
	Reason          string     `json:"reason"`
	ChangedAt       time.Time  `json:"changed_at"`
	ChangedBy       *uuid.UUID `json:"changed_by,omitempty"`
	ChangedByWorker string     `json:"changed_by_worker,omitempty"`
}

const circuitColumns = `connector_id, disabled OR COALESCE(open_until > now(), false), disabled,
	CASE WHEN open_until > now() THEN open_until END, reason, changed_at, changed_by, COALESCE(changed_by_worker, '')`

func scanCircuit(r pgx.Row) (Circuit, error) {
	var c Circuit
	err := r.Scan(&c.ConnectorID, &c.Open, &c.Disabled, &c.OpenUntil, &c.Reason, &c.ChangedAt, &c.ChangedBy,
		&c.ChangedByWorker)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, newErr(ErrNotFound, "no such connector")
	}
	return c, err
}

// ConnectorCircuit returns a connector's shared circuit.
func (s *Service) ConnectorCircuit(ctx context.Context, a Actor, connectorID uuid.UUID) (Circuit, error) {
	var c Circuit
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		var err error
		c, err = scanCircuit(tx.QueryRow(ctx, `SELECT `+circuitColumns+`
			FROM eacp.connector_circuits WHERE connector_id = $1`, connectorID))
		return err
	})
	return c, err
}

// SetConnectorDisabled disables a connector's dispatch until it is enabled
// again, or enables it, which also clears a worker's automatic trip
// (operator, reason required, journaled). Calls already dispatched are not
// affected: they finish, or their outcome is unknown and reconciled.
func (s *Service) SetConnectorDisabled(ctx context.Context, a Actor, connectorID uuid.UUID, disabled bool, reason string) (Circuit, error) {
	var c Circuit
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		var err error
		c, err = scanCircuit(tx.QueryRow(ctx, `UPDATE eacp.connector_circuits SET disabled = $2, reason = $3
			WHERE connector_id = $1 RETURNING `+circuitColumns, connectorID, disabled, reason))
		return err
	})
	return c, err
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
