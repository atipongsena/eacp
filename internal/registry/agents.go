package registry

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// State is an AgentVersion lifecycle state (ADR-003 §2).
type State string

const (
	StateRegistered  State = "REGISTERED"
	StateActive      State = "ACTIVE"
	StateSuspended   State = "SUSPENDED"
	StateQuarantined State = "QUARANTINED"
	StateRetired     State = "RETIRED"
	StateRevoked     State = "REVOKED"
)

// NewAgent describes an agent to register. Exactly one owner is required.
type NewAgent struct {
	Name             string    `json:"name"`
	DisplayName      string    `json:"display_name"`
	Environment      string    `json:"environment"`
	RiskClass        string    `json:"risk_class"`
	OwnerPrincipalID uuid.UUID `json:"owner_principal_id,omitempty"`
	OwnerGroupID     uuid.UUID `json:"owner_group_id,omitempty"`
}

// Agent is a registered agent. Agents are immutable.
type Agent struct {
	ID               uuid.UUID `json:"id"`
	Name             string    `json:"name"`
	DisplayName      string    `json:"display_name"`
	Environment      string    `json:"environment"`
	RiskClass        string    `json:"risk_class"`
	OwnerPrincipalID uuid.UUID `json:"owner_principal_id,omitzero"`
	OwnerGroupID     uuid.UUID `json:"owner_group_id,omitzero"`
	CreatedAt        time.Time `json:"created_at"`
}

// NewVersion describes an agent version to register.
type NewVersion struct {
	Runtime string `json:"runtime"`
	CodeRef string `json:"code_ref"`
}

// Version is an agent version with its active allowlist resolved.
type Version struct {
	ID                uuid.UUID `json:"id"`
	AgentID           uuid.UUID `json:"agent_id"`
	Number            int       `json:"number"`
	Runtime           string    `json:"runtime"`
	CodeRef           string    `json:"code_ref"`
	State             State     `json:"state"`
	StateReason       string    `json:"state_reason,omitempty"`
	ActiveAllowlistID uuid.UUID `json:"active_allowlist_id,omitzero"`
	AllowedTools      []string  `json:"allowed_tools"` // "connector.tool", sorted
}

// AgentDetail is an agent with all its versions, newest first.
type AgentDetail struct {
	Agent
	Versions []Version `json:"versions"`
}

const agentColumns = `id, name, display_name, environment, risk_class,
	COALESCE(owner_principal_id, '00000000-0000-0000-0000-000000000000'),
	COALESCE(owner_group_id, '00000000-0000-0000-0000-000000000000'), created_at`

func scanAgent(row pgx.Row) (Agent, error) {
	var ag Agent
	err := row.Scan(&ag.ID, &ag.Name, &ag.DisplayName, &ag.Environment, &ag.RiskClass,
		&ag.OwnerPrincipalID, &ag.OwnerGroupID, &ag.CreatedAt)
	return ag, err
}

// RegisterAgent registers an agent (registry_editor).
func (s *Service) RegisterAgent(ctx context.Context, a Actor, n NewAgent) (Agent, error) {
	var ag Agent
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		var err error
		ag, err = Tx{tx}.RegisterAgent(ctx, n)
		return err
	})
	return ag, err
}

// ListAgents lists the tenant's agents by name.
func (s *Service) ListAgents(ctx context.Context, a Actor) ([]Agent, error) {
	var out []Agent
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+agentColumns+` FROM eacp.agents ORDER BY name`)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Agent, error) { return scanAgent(r) })
		return err
	})
	if out == nil {
		out = []Agent{}
	}
	return out, err
}

// GetAgent returns an agent, by id or name, with its versions.
func (s *Service) GetAgent(ctx context.Context, a Actor, idOrName string) (AgentDetail, error) {
	var d AgentDetail
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		var err error
		if id, perr := uuid.Parse(idOrName); perr == nil {
			d.Agent, err = scanAgent(tx.QueryRow(ctx, `SELECT `+agentColumns+` FROM eacp.agents WHERE id = $1`, id))
		} else {
			d.Agent, err = scanAgent(tx.QueryRow(ctx, `SELECT `+agentColumns+` FROM eacp.agents WHERE name = $1`, idOrName))
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT v.id, v.agent_id, v.version, v.runtime, v.code_ref, v.state, COALESCE(v.state_reason, ''),
			       COALESCE(v.active_allowlist_id, '00000000-0000-0000-0000-000000000000'),
			       COALESCE((SELECT array_agg(c.name || '.' || t.name ORDER BY c.name, t.name)
			                 FROM eacp.agent_allowlists al
			                 JOIN eacp.tools t ON t.tenant_id = al.tenant_id AND t.id = ANY (al.tool_ids)
			                 JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
			                 WHERE al.tenant_id = v.tenant_id AND al.id = v.active_allowlist_id), '{}')
			FROM eacp.agent_versions v
			WHERE v.agent_id = $1
			ORDER BY v.version DESC`, d.ID)
		if err != nil {
			return err
		}
		d.Versions, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Version, error) {
			var v Version
			err := r.Scan(&v.ID, &v.AgentID, &v.Number, &v.Runtime, &v.CodeRef, &v.State, &v.StateReason,
				&v.ActiveAllowlistID, &v.AllowedTools)
			return v, err
		})
		return err
	})
	if d.Versions == nil {
		d.Versions = []Version{}
	}
	return d, err
}

// RegisterVersion registers a new version (number assigned by the database)
// in state REGISTERED.
func (s *Service) RegisterVersion(ctx context.Context, a Actor, agentID uuid.UUID, n NewVersion) (Version, error) {
	var v Version
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		var err error
		v, err = Tx{tx}.RegisterVersion(ctx, agentID, n)
		return err
	})
	return v, err
}

// ProposeAllowlist records an immutable allowlist of "connector.tool" refs for
// a version. A second person activates it.
func (s *Service) ProposeAllowlist(ctx context.Context, a Actor, versionID uuid.UUID, tools []string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		var err error
		id, err = Tx{tx}.ProposeAllowlist(ctx, versionID, tools)
		return err
	})
	return id, err
}

// ActivateAllowlist points a version at an allowlist (two-person).
func (s *Service) ActivateAllowlist(ctx context.Context, a Actor, versionID, allowlistID uuid.UUID) error {
	return s.change(ctx, a, func(tx pgx.Tx) error { return Tx{tx}.ActivateAllowlist(ctx, versionID, allowlistID) })
}

// TransitionVersion moves a version through its lifecycle (ADR-003 §2).
func (s *Service) TransitionVersion(ctx context.Context, a Actor, versionID uuid.UUID, to State, reason string) error {
	return s.change(ctx, a, func(tx pgx.Tx) error { return Tx{tx}.TransitionVersion(ctx, versionID, to, reason) })
}
