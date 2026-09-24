package fleet

import (
	"context"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/registry"
	"eacp/internal/storage"
)

// Health values (ADR-024 §5). They are observations, never decisions.
const (
	HealthOK        = "ok"
	HealthDegraded  = "degraded"
	HealthContained = "contained"
)

// DefaultWindow and MaxWindow bound the recent-action counts.
const (
	DefaultWindow = 24 * time.Hour
	MaxWindow     = 7 * 24 * time.Hour
)

// Filter narrows the fleet view. Window is the period of RecentActions.
type Filter struct {
	Environment  string
	RiskClass    string
	OwnerGroupID uuid.UUID
	Health       string
	Window       time.Duration
}

func (f *Filter) validate() error {
	if f.Environment != "" && !oneOf(f.Environment, environments) {
		return invalid("unknown environment %q", f.Environment)
	}
	if f.RiskClass != "" && !oneOf(f.RiskClass, riskClasses) {
		return invalid("unknown risk class %q", f.RiskClass)
	}
	if f.Health != "" && !oneOf(f.Health, []string{HealthOK, HealthDegraded, HealthContained}) {
		return invalid("unknown health %q", f.Health)
	}
	if f.Window == 0 {
		f.Window = DefaultWindow
	}
	if f.Window < 0 || f.Window > MaxWindow {
		return invalid("window must be positive and at most %s", MaxWindow)
	}
	return nil
}

// VersionRef names an agent version.
type VersionRef struct {
	ID     uuid.UUID `json:"id"`
	Number int       `json:"number"`
}

// KillRef is an active kill scope that matches the agent's active version.
type KillRef struct {
	Scope    string    `json:"scope"`
	TargetID uuid.UUID `json:"target_id"`
}

// ToolIssue is an allowlisted tool the database would not execute, with the
// capability check's reason.
type ToolIssue struct {
	Tool   string `json:"tool"`
	Reason string `json:"reason"`
}

// CircuitIssue is an allowlisted connector whose circuit is open or disabled.
type CircuitIssue struct {
	Connector string `json:"connector"`
	State     string `json:"state"`
}

// AgentStatus is one agent in the fleet view.
type AgentStatus struct {
	registry.Agent
	ActiveVersion   *VersionRef    `json:"active_version"`
	VersionsByState map[string]int `json:"versions_by_state"`
	OwnerKnown      bool           `json:"owner_known"`
	Health          string         `json:"health"`
	Reasons         []string       `json:"reasons"`
	Kills           []KillRef      `json:"kills"`
	Drift           []ToolIssue    `json:"capability_drift"`
	Circuits        []CircuitIssue `json:"circuits"`
	OpenActions     map[string]int `json:"open_actions"`
	RecentActions   map[string]int `json:"recent_actions"`

	tools, connectors []uuid.UUID
}

// Summary is the fleet dashboard (MASTER_PLAN §38).
type Summary struct {
	AsOf            time.Time      `json:"as_of"`
	Window          string         `json:"window"`
	Agents          int            `json:"agents"`
	ByEnvironment   map[string]int `json:"by_environment"`
	ByHealth        map[string]int `json:"by_health"`
	Quarantined     int            `json:"quarantined"` // no ACTIVE version, at least one QUARANTINED
	UnknownOwner    int            `json:"unknown_owner"`
	CapabilityDrift int            `json:"capability_drift"`
	CircuitIssues   int            `json:"circuit_issues"`
	ActiveKills     int            `json:"active_kills"` // active kill scopes in the tenant
	OpenActions     map[string]int `json:"open_actions"`
	RecentActions   map[string]int `json:"recent_actions"`
	// Coverage says what the view can know: it observes registry, kill,
	// circuit and action rows; it cannot see agents' own runtimes.
	Coverage string `json:"coverage"`
}

var terminal = []string{"SUCCEEDED", "FAILED", "DENIED", "CANCELLED", "EXPIRED"}

// Agents returns the fleet view, ordered by agent name.
func (s *Service) Agents(ctx context.Context, a registry.Actor, f Filter) ([]AgentStatus, error) {
	if err := f.validate(); err != nil {
		return nil, err
	}
	var out []AgentStatus
	err := storage.InTenantReadTx(ctx, s.pool, a.TenantID.String(), func(tx pgx.Tx) error {
		var err error
		out, _, err = view(ctx, tx, f)
		return err
	})
	if err != nil {
		return nil, classify(err)
	}
	return out, nil
}

// Health returns the fleet summary over the agents f selects.
func (s *Service) Health(ctx context.Context, a registry.Actor, f Filter) (Summary, error) {
	if err := f.validate(); err != nil {
		return Summary{}, err
	}
	sum := Summary{Window: f.Window.String(), ByEnvironment: map[string]int{}, ByHealth: map[string]int{},
		OpenActions: map[string]int{}, RecentActions: map[string]int{}, Coverage: "observed"}
	err := storage.InTenantReadTx(ctx, s.pool, a.TenantID.String(), func(tx pgx.Tx) error {
		agents, kills, err := view(ctx, tx, f)
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&sum.AsOf); err != nil {
			return err
		}
		sum.ActiveKills = kills
		sum.Agents = len(agents)
		for _, g := range agents {
			sum.ByEnvironment[g.Environment]++
			sum.ByHealth[g.Health]++
			if g.ActiveVersion == nil && g.VersionsByState["QUARANTINED"] > 0 {
				sum.Quarantined++
			}
			if !g.OwnerKnown {
				sum.UnknownOwner++
			}
			if len(g.Drift) > 0 {
				sum.CapabilityDrift++
			}
			if len(g.Circuits) > 0 {
				sum.CircuitIssues++
			}
			for k, n := range g.OpenActions {
				sum.OpenActions[k] += n
			}
			for k, n := range g.RecentActions {
				sum.RecentActions[k] += n
			}
		}
		return nil
	})
	if err != nil {
		return Summary{}, classify(err)
	}
	return sum, nil
}

// view reads one snapshot. It repeats eacp.action_capability_denial's tool
// rules without its row locks (a read-only transaction cannot take them);
// TestFleetDriftAgreesWithTheCapabilityCheck keeps the two in step.
func view(ctx context.Context, tx pgx.Tx, f Filter) ([]AgentStatus, int, error) {
	rows, err := tx.Query(ctx, `
		SELECT a.id, a.name, a.display_name, a.environment, a.risk_class,
		       COALESCE(a.owner_principal_id, '00000000-0000-0000-0000-000000000000'),
		       COALESCE(a.owner_group_id, '00000000-0000-0000-0000-000000000000'), a.created_at,
		       v.id, v.version,
		       CASE WHEN a.owner_principal_id IS NOT NULL THEN EXISTS (
		                SELECT 1 FROM eacp.principals p
		                WHERE p.tenant_id = a.tenant_id AND p.id = a.owner_principal_id AND p.disabled_at IS NULL)
		            ELSE EXISTS (
		                SELECT 1 FROM eacp.group_memberships m
		                JOIN eacp.principals p ON p.tenant_id = m.tenant_id AND p.id = m.principal_id
		                WHERE m.tenant_id = a.tenant_id AND m.group_id = a.owner_group_id
		                  AND m.removed_at IS NULL AND p.disabled_at IS NULL) END,
		       COALESCE((SELECT jsonb_object_agg(state, n) FROM (
		                SELECT state, count(*) AS n FROM eacp.agent_versions x
		                WHERE x.tenant_id = a.tenant_id AND x.agent_id = a.id GROUP BY state) s), '{}')
		FROM eacp.agents a
		LEFT JOIN eacp.agent_versions v ON v.tenant_id = a.tenant_id AND v.agent_id = a.id AND v.state = 'ACTIVE'
		WHERE ($1 = '' OR a.environment = $1) AND ($2 = '' OR a.risk_class = $2)
		  AND ($3::uuid IS NULL OR a.owner_group_id = $3)
		ORDER BY a.name`, f.Environment, f.RiskClass, nullID(f.OwnerGroupID))
	if err != nil {
		return nil, 0, err
	}
	var agents []AgentStatus
	index := map[uuid.UUID]int{}
	var g AgentStatus
	var vid *uuid.UUID
	var vnum *int
	_, err = pgx.ForEachRow(rows, []any{&g.ID, &g.Name, &g.DisplayName, &g.Environment, &g.RiskClass,
		&g.OwnerPrincipalID, &g.OwnerGroupID, &g.CreatedAt, &vid, &vnum, &g.OwnerKnown, &g.VersionsByState}, func() error {
		st := g
		st.ActiveVersion = nil
		if vid != nil {
			st.ActiveVersion = &VersionRef{ID: *vid, Number: *vnum}
		}
		st.Reasons, st.Kills, st.Drift, st.Circuits = []string{}, []KillRef{}, []ToolIssue{}, []CircuitIssue{}
		st.OpenActions, st.RecentActions = map[string]int{}, map[string]int{}
		index[st.ID] = len(agents)
		agents = append(agents, st)
		g.VersionsByState = nil
		return nil
	})
	if err != nil {
		return nil, 0, err
	}

	// Allowlisted tools of each ACTIVE version, with the capability check's
	// tool rules (same order as eacp.action_capability_denial) and circuits.
	rows, err = tx.Query(ctx, `
		SELECT v.agent_id, t.id, c.id, c.name || '.' || t.name, c.name,
		       CASE WHEN t.quarantined_at IS NOT NULL THEN 'tool_quarantined'
		            WHEN t.active_contract_id IS NULL THEN 'no_active_contract'
		            WHEN ct.revoked_at IS NOT NULL THEN 'contract_revoked'
		            WHEN ct.fingerprint IS DISTINCT FROM eacp.tool_fingerprint(t.tenant_id, t.id)
		                 THEN 'contract_fingerprint_mismatch' END,
		       eacp.connector_circuit_state(c.tenant_id, c.id)
		FROM eacp.agent_versions v
		JOIN eacp.agent_allowlists al ON al.tenant_id = v.tenant_id AND al.id = v.active_allowlist_id
		JOIN eacp.tools t ON t.tenant_id = al.tenant_id AND t.id = ANY (al.tool_ids)
		JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
		LEFT JOIN eacp.tool_contracts ct ON ct.tenant_id = t.tenant_id AND ct.id = t.active_contract_id
		WHERE v.state = 'ACTIVE'
		ORDER BY 4`)
	if err != nil {
		return nil, 0, err
	}
	var agent, tool, conn uuid.UUID
	var ref, connName string
	var denial, circuit *string
	_, err = pgx.ForEachRow(rows, []any{&agent, &tool, &conn, &ref, &connName, &denial, &circuit}, func() error {
		i, ok := index[agent]
		if !ok {
			return nil
		}
		st := &agents[i]
		st.tools = append(st.tools, tool)
		if !slices.Contains(st.connectors, conn) {
			st.connectors = append(st.connectors, conn)
			if circuit != nil {
				st.Circuits = append(st.Circuits, CircuitIssue{Connector: connName, State: *circuit})
			}
		}
		if denial != nil {
			st.Drift = append(st.Drift, ToolIssue{Tool: ref, Reason: *denial})
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}

	// Active kill scopes, matched as eacp.action_killed would match an
	// action of the ACTIVE version (the team is the owning group).
	rows, err = tx.Query(ctx, `SELECT scope, target_id, eacp.current_tenant_id() FROM eacp.kill_states
		WHERE killed ORDER BY scope, target_id`)
	if err != nil {
		return nil, 0, err
	}
	var kills int
	var scope string
	var target, tenant uuid.UUID
	_, err = pgx.ForEachRow(rows, []any{&scope, &target, &tenant}, func() error {
		kills++
		for i := range agents {
			st := &agents[i]
			var match bool
			switch scope {
			case "tenant":
				match = target == tenant
			case "team":
				match = st.OwnerGroupID != uuid.Nil && target == st.OwnerGroupID
			case "agent":
				match = target == st.ID
			case "agent_version":
				match = st.ActiveVersion != nil && target == st.ActiveVersion.ID
			case "tool":
				match = slices.Contains(st.tools, target)
			case "connector":
				match = slices.Contains(st.connectors, target)
			}
			if match {
				st.Kills = append(st.Kills, KillRef{Scope: scope, TargetID: target})
			}
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}

	// Open actions by state, and terminal ones in the window.
	rows, err = tx.Query(ctx, `
		SELECT agent_id, state, count(*)::integer, state = ANY ($1)
		FROM eacp.actions
		WHERE NOT (state = ANY ($1)) OR state_changed_at >= now() - make_interval(secs => $2)
		GROUP BY agent_id, state`, terminal, f.Window.Seconds())
	if err != nil {
		return nil, 0, err
	}
	var state string
	var n int
	var done bool
	_, err = pgx.ForEachRow(rows, []any{&agent, &state, &n, &done}, func() error {
		i, ok := index[agent]
		if !ok {
			return nil
		}
		if done {
			agents[i].RecentActions[state] = n
		} else {
			agents[i].OpenActions[state] = n
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}

	out := agents[:0]
	for _, st := range agents {
		st.Health, st.Reasons = health(st)
		if f.Health == "" || f.Health == st.Health {
			out = append(out, st)
		}
	}
	if out == nil {
		out = []AgentStatus{}
	}
	return out, kills, nil
}

// health classifies an agent (ADR-024 §5) and lists every reason.
func health(st AgentStatus) (string, []string) {
	reasons := []string{}
	add := func(cond bool, r string) {
		if cond {
			reasons = append(reasons, r)
		}
	}
	add(st.ActiveVersion == nil, "no_active_version")
	add(len(st.Kills) > 0, "kill_active")
	add(!st.OwnerKnown, "owner_unknown")
	add(len(st.Drift) > 0, "capability_drift")
	add(len(st.Circuits) > 0, "circuit_open")
	add(st.OpenActions["UNKNOWN_OUTCOME"]+st.OpenActions["RECONCILING"] > 0, "unknown_outcome")
	add(st.OpenActions["NEEDS_HUMAN_RESOLUTION"] > 0, "needs_human_resolution")
	switch {
	case st.ActiveVersion == nil || len(st.Kills) > 0:
		return HealthContained, reasons
	case len(reasons) > 0:
		return HealthDegraded, reasons
	default:
		return HealthOK, reasons
	}
}
