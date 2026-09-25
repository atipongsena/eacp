package incident

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"eacp/internal/registry"
)

// Summary is the Agent SOC dashboard (MASTER_PLAN §55): counters read in
// one tenant snapshot. It observes and never decides.
type Summary struct {
	AsOf      time.Time       `json:"as_of"`
	Agents    AgentCounts     `json:"agents"`
	Security  SecurityCounts  `json:"security"`
	Execution ExecutionCounts `json:"execution"`
	FinOps    FinOpsCounts    `json:"finops"`
}

type AgentCounts struct {
	Registered    int           `json:"registered"`
	Production    int           `json:"production"`
	HighRisk      int           `json:"high_risk"`
	OwnerDisabled int           `json:"owner_disabled"`
	Versions      VersionCounts `json:"versions"`
}

type VersionCounts struct {
	Active      int `json:"active"`
	Suspended   int `json:"suspended"`
	Quarantined int `json:"quarantined"`
}

type SecurityCounts struct {
	OpenIncidents    map[string]int `json:"open_incidents"`
	Unacknowledged   int            `json:"unacknowledged"`
	QuarantinedTools int            `json:"quarantined_tools"`
	ActiveKills      int            `json:"active_kills"`
	OpenCircuits     int            `json:"open_circuits"`
	DisabledCircuits int            `json:"disabled_circuits"`
	PendingApprovals int            `json:"pending_approvals"`
}

type ExecutionCounts struct {
	Queued         int `json:"queued"`
	Running        int `json:"running"`
	RetryWait      int `json:"retry_wait"`
	UnknownOutcome int `json:"unknown_outcome"`
	NeedsHuman     int `json:"needs_human"`
}

type FinOpsCounts struct {
	OpenAlerts int          `json:"open_alerts"`
	SpendToday []UnitAmount `json:"spend_today"`
}

// UnitAmount is spend in one unit; today's spend is what the FinOps
// dashboard counts (committed and held tool cost plus effective LLM cost).
type UnitAmount struct {
	Unit   string      `json:"unit"`
	Amount json.Number `json:"amount"`
}

const summarySQL = `SELECT jsonb_build_object(
	'as_of', now(),
	'agents', (SELECT jsonb_build_object(
		'registered', count(*),
		'production', count(*) FILTER (WHERE ag.environment = 'production'),
		'high_risk', count(*) FILTER (WHERE ag.risk_class IN ('high', 'critical')),
		'owner_disabled', count(*) FILTER (WHERE EXISTS (SELECT 1 FROM eacp.principals p
			WHERE p.tenant_id = ag.tenant_id AND p.id = ag.owner_principal_id AND p.disabled_at IS NOT NULL)),
		'versions', (SELECT jsonb_build_object(
			'active', count(*) FILTER (WHERE v.state = 'ACTIVE'),
			'suspended', count(*) FILTER (WHERE v.state = 'SUSPENDED'),
			'quarantined', count(*) FILTER (WHERE v.state = 'QUARANTINED')) FROM eacp.agent_versions v))
		FROM eacp.agents ag),
	'security', jsonb_build_object(
		'open_incidents', (SELECT jsonb_build_object(
			'critical', count(*) FILTER (WHERE severity = 'critical'), 'high', count(*) FILTER (WHERE severity = 'high'),
			'medium', count(*) FILTER (WHERE severity = 'medium'), 'low', count(*) FILTER (WHERE severity = 'low'))
			FROM eacp.incidents WHERE state <> 'RESOLVED'),
		'unacknowledged', (SELECT count(*) FROM eacp.incidents WHERE state = 'OPEN'),
		'quarantined_tools', (SELECT count(*) FROM eacp.tools WHERE quarantined_at IS NOT NULL),
		'active_kills', (SELECT count(*) FROM eacp.kill_states WHERE killed),
		'open_circuits', (SELECT count(*) FROM eacp.connector_circuits WHERE open_until > now() AND NOT disabled),
		'disabled_circuits', (SELECT count(*) FROM eacp.connector_circuits WHERE disabled),
		'pending_approvals', (SELECT count(*) FROM eacp.approval_requests WHERE state = 'PENDING')),
	'execution', (SELECT jsonb_build_object(
		'queued', count(*) FILTER (WHERE state = 'QUEUED'),
		'running', count(*) FILTER (WHERE state IN ('LEASED', 'EXECUTING')),
		'retry_wait', count(*) FILTER (WHERE state = 'RETRY_WAIT'),
		'unknown_outcome', count(*) FILTER (WHERE state IN ('UNKNOWN_OUTCOME', 'RECONCILING')),
		'needs_human', count(*) FILTER (WHERE state = 'NEEDS_HUMAN_RESOLUTION'))
		FROM eacp.actions WHERE state NOT IN ('SUCCEEDED', 'FAILED', 'DENIED', 'CANCELLED', 'EXPIRED')),
	'finops', jsonb_build_object(
		'open_alerts', (SELECT count(*) FROM eacp.finops_alerts WHERE acknowledged_at IS NULL),
		'spend_today', COALESCE((SELECT jsonb_agg(jsonb_build_object('unit', s.unit, 'amount', s.amount) ORDER BY s.unit)
			FROM (SELECT unit, trim_scale(sum(tool_committed + tool_held + llm_effective)) AS amount
			        FROM eacp.finops_agent_spend(date_trunc('day', now(), 'UTC'), now()) GROUP BY unit) s), '[]'::jsonb))
)::text`

// Summary reads the SOC counters of the actor's tenant.
func (s *Service) Summary(ctx context.Context, a registry.Actor) (Summary, error) {
	var out Summary
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		var raw string
		if err := tx.QueryRow(ctx, summarySQL).Scan(&raw); err != nil {
			return err
		}
		return json.Unmarshal([]byte(raw), &out)
	})
	return out, err
}
