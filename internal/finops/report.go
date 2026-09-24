package finops

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/registry"
)

// Group-by values of a chargeback.
const (
	ByAgent   = "agent"
	ByTeam    = "team"
	ByAccount = "account"
)

// MaxPeriod bounds a report's period.
const MaxPeriod = 400 * 24 * time.Hour

// UnitSpend is a group's spend in one unit (ADR-025 §4-§5). Total is
// committed + held tool spend + effective LLM spend.
type UnitSpend struct {
	Unit          string      `json:"unit"`
	ToolCommitted json.Number `json:"tool_committed"`
	ToolHeld      json.Number `json:"tool_held"`
	LLMReported   json.Number `json:"llm_reported"`
	LLMBilled     json.Number `json:"llm_billed"`
	LLMEffective  json.Number `json:"llm_effective"`
	Total         json.Number `json:"total"`
}

// ConnectorLine is tool spend through one connector.
type ConnectorLine struct {
	Connector string      `json:"connector"`
	Unit      string      `json:"unit"`
	Committed json.Number `json:"committed"`
	Held      json.Number `json:"held"`
}

// ModelLine is billable LLM usage of one provider and model. Unit is
// empty for unpriced usage. Tokens are the reported (OTel) tokens.
type ModelLine struct {
	Provider string      `json:"provider"`
	Model    string      `json:"model"`
	Unit     string      `json:"unit,omitempty"`
	Reported json.Number `json:"reported"`
	Billed   json.Number `json:"billed"`
	Tokens   int64       `json:"tokens"`
}

// Group is one chargeback group. Kind is agent, group or principal (the
// owning team), account, or unassigned.
type Group struct {
	Kind           string          `json:"kind"`
	ID             *uuid.UUID      `json:"id,omitempty"`
	Name           string          `json:"name"`
	Spend          []UnitSpend     `json:"spend"`
	Tokens         int64           `json:"tokens"`          // billable, reported
	UnpricedTokens int64           `json:"unpriced_tokens"` // billable with no price
	UnbilledTokens int64           `json:"unbilled_tokens"` // agent-level spans, never summed into spend
	Connectors     []ConnectorLine `json:"connectors"`
	Models         []ModelLine     `json:"models"`
}

// Chargeback is a period's spend grouped by agent, team or account.
// Account groups overlap: every ancestor includes its subtree.
type Chargeback struct {
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	GroupBy string    `json:"group_by"`
	Groups  []Group   `json:"groups"`
}

// MonthStart is the start of t's UTC month.
func MonthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// checkPeriod validates [from, to).
func checkPeriod(from, to time.Time) error {
	if from.IsZero() || to.IsZero() || !to.After(from) {
		return invalid("the period needs from < to")
	}
	if to.Sub(from) > MaxPeriod {
		return invalid("the period is at most 400 days")
	}
	return nil
}

// members maps each group to its agents (agent_id) and, where the group is
// unit-specific, the unit its spend counts in (unit, NULL: every unit).
// count_tokens marks the one member row per agent whose tokens count.
func members(groupBy string) string {
	switch groupBy {
	case ByTeam:
		return `SELECT CASE WHEN a.owner_group_id IS NOT NULL THEN 'group' ELSE 'principal' END AS kind,
			COALESCE(a.owner_group_id, a.owner_principal_id) AS gid, COALESCE(g.name, p.name) AS name,
			a.id AS agent_id, NULL::text AS unit, true AS count_tokens
			FROM eacp.agents a
			LEFT JOIN eacp.groups g ON g.id = a.owner_group_id
			LEFT JOIN eacp.principals p ON p.id = a.owner_principal_id`
	case ByAccount:
		// Each agent leaf rolls up to every ancestor. Spend in a unit where
		// the agent has no leaf, and the tokens of an agent with no leaf,
		// are unassigned.
		return `WITH RECURSIVE up(account_id, agent_id, unit) AS (
				SELECT id, agent_id, unit FROM eacp.budget_accounts WHERE agent_id IS NOT NULL
				UNION ALL
				SELECT b.parent_id, up.agent_id, up.unit FROM up
				JOIN eacp.budget_accounts b ON b.id = up.account_id WHERE b.parent_id IS NOT NULL)
			SELECT 'account' AS kind, up.account_id AS gid, b.name, up.agent_id, up.unit, true AS count_tokens
			FROM up JOIN eacp.budget_accounts b ON b.id = up.account_id
			UNION ALL
			SELECT 'unassigned', NULL, 'unassigned', s.agent_id, s.unit, false
			FROM (SELECT DISTINCT agent_id, unit FROM eacp.finops_agent_spend($1, $2)) s
			WHERE NOT EXISTS (SELECT 1 FROM eacp.budget_accounts b WHERE b.agent_id = s.agent_id AND b.unit = s.unit)
			UNION ALL
			SELECT 'unassigned', NULL, 'unassigned', a.id, '', true
			FROM eacp.agents a
			WHERE NOT EXISTS (SELECT 1 FROM eacp.budget_accounts b WHERE b.agent_id = a.id)`
	default:
		return `SELECT 'agent' AS kind, a.id AS gid, a.name, a.id AS agent_id, NULL::text AS unit, true AS count_tokens
			FROM eacp.agents a`
	}
}

type groupKey struct {
	kind string
	id   uuid.UUID
}

// Chargeback reports spend in [from, to) grouped by agent, team or account.
func (s *Service) Chargeback(ctx context.Context, a registry.Actor, from, to time.Time, groupBy string) (Chargeback, error) {
	if groupBy == "" {
		groupBy = ByAgent
	}
	if groupBy != ByAgent && groupBy != ByTeam && groupBy != ByAccount {
		return Chargeback{}, invalid("group_by is agent, team or account")
	}
	if err := checkPeriod(from, to); err != nil {
		return Chargeback{}, err
	}
	out := Chargeback{From: from, To: to, GroupBy: groupBy, Groups: []Group{}}
	index := map[groupKey]int{}
	group := func(kind string, id *uuid.UUID, name string) *Group {
		k := groupKey{kind: kind}
		if id != nil {
			k.id = *id
		}
		i, ok := index[k]
		if !ok {
			i = len(out.Groups)
			index[k] = i
			out.Groups = append(out.Groups, Group{Kind: kind, ID: id, Name: name, Spend: []UnitSpend{},
				Connectors: []ConnectorLine{}, Models: []ModelLine{}})
		}
		return &out.Groups[i]
	}
	m := `WITH m AS (` + members(groupBy) + `) `
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		// Spend per group and unit.
		rows, err := tx.Query(ctx, m+`SELECT m.kind, m.gid, m.name, s.unit,
			trim_scale(sum(s.tool_committed))::text, trim_scale(sum(s.tool_held))::text,
			trim_scale(sum(s.llm_reported))::text, trim_scale(sum(s.llm_billed))::text,
			trim_scale(sum(s.llm_effective))::text,
			trim_scale(sum(s.tool_committed + s.tool_held + s.llm_effective))::text
			FROM m JOIN eacp.finops_agent_spend($1, $2) s ON s.agent_id = m.agent_id AND (m.unit IS NULL OR s.unit = m.unit)
			GROUP BY 1, 2, 3, 4 ORDER BY 3, 1, 4`, from, to)
		if err != nil {
			return err
		}
		var kind, name string
		var id *uuid.UUID
		var u UnitSpend
		var c, h, r, b, e, t string
		if _, err := pgx.ForEachRow(rows, []any{&kind, &id, &name, &u.Unit, &c, &h, &r, &b, &e, &t}, func() error {
			g := group(kind, id, name)
			g.Spend = append(g.Spend, UnitSpend{Unit: u.Unit, ToolCommitted: json.Number(c), ToolHeld: json.Number(h),
				LLMReported: json.Number(r), LLMBilled: json.Number(b), LLMEffective: json.Number(e), Total: json.Number(t)})
			return nil
		}); err != nil {
			return err
		}

		// Reported tokens per group.
		rows, err = tx.Query(ctx, m+`, t AS (
				SELECT agent_id,
				       COALESCE(sum(input_tokens + output_tokens) FILTER (WHERE billable), 0) AS tokens,
				       COALESCE(sum(input_tokens + output_tokens) FILTER (WHERE billable AND cost_amount IS NULL), 0) AS unpriced,
				       COALESCE(sum(input_tokens + output_tokens) FILTER (WHERE NOT billable), 0) AS unbilled
				FROM eacp.usage_records WHERE source = 'otel' AND observed_at >= $1 AND observed_at < $2
				GROUP BY agent_id)
			SELECT m.kind, m.gid, m.name, sum(t.tokens)::bigint, sum(t.unpriced)::bigint, sum(t.unbilled)::bigint
			FROM m JOIN t ON t.agent_id = m.agent_id WHERE m.count_tokens
			GROUP BY 1, 2, 3 ORDER BY 3, 1`, from, to)
		if err != nil {
			return err
		}
		var tokens, unpriced, unbilled int64
		if _, err := pgx.ForEachRow(rows, []any{&kind, &id, &name, &tokens, &unpriced, &unbilled}, func() error {
			g := group(kind, id, name)
			g.Tokens, g.UnpricedTokens, g.UnbilledTokens = tokens, unpriced, unbilled
			return nil
		}); err != nil {
			return err
		}

		// Tool spend per connector.
		rows, err = tx.Query(ctx, m+`, c AS (
				SELECT a.agent_id, c.name AS connector, r.unit,
				       COALESCE(sum(r.committed_amount) FILTER (WHERE r.state = 'COMMITTED'
				                AND r.settled_at >= $1 AND r.settled_at < $2), 0) AS committed,
				       COALESCE(sum(r.amount) FILTER (WHERE r.state = 'ACTIVE'
				                AND r.created_at >= $1 AND r.created_at < $2), 0) AS held
				FROM eacp.budget_reservations r
				JOIN eacp.actions a ON a.id = r.action_id
				JOIN eacp.tools t ON t.id = a.tool_id
				JOIN eacp.connectors c ON c.id = t.connector_id
				WHERE (r.settled_at >= $1 AND r.settled_at < $2) OR (r.state = 'ACTIVE' AND r.created_at >= $1 AND r.created_at < $2)
				GROUP BY 1, 2, 3)
			SELECT m.kind, m.gid, m.name, c.connector, c.unit,
			       trim_scale(sum(c.committed))::text, trim_scale(sum(c.held))::text
			FROM m JOIN c ON c.agent_id = m.agent_id AND (m.unit IS NULL OR c.unit = m.unit)
			GROUP BY 1, 2, 3, 4, 5 HAVING sum(c.committed) + sum(c.held) > 0 ORDER BY 3, 1, 4, 5`, from, to)
		if err != nil {
			return err
		}
		var line ConnectorLine
		if _, err := pgx.ForEachRow(rows, []any{&kind, &id, &name, &line.Connector, &line.Unit, &c, &h}, func() error {
			g := group(kind, id, name)
			g.Connectors = append(g.Connectors, ConnectorLine{Connector: line.Connector, Unit: line.Unit,
				Committed: json.Number(c), Held: json.Number(h)})
			return nil
		}); err != nil {
			return err
		}

		// Billable LLM usage per provider and model. Priced lines follow the
		// group's unit; unpriced lines follow its tokens.
		rows, err = tx.Query(ctx, m+`, l AS (
				SELECT agent_id, provider, COALESCE(model, '') AS model, cost_unit AS unit,
				       COALESCE(sum(cost_amount) FILTER (WHERE source = 'otel'), 0) AS reported,
				       COALESCE(sum(cost_amount) FILTER (WHERE source = 'provider_billing'), 0) AS billed,
				       COALESCE(sum(input_tokens + output_tokens) FILTER (WHERE source = 'otel'), 0) AS tokens
				FROM eacp.usage_records WHERE billable AND observed_at >= $1 AND observed_at < $2
				GROUP BY 1, 2, 3, 4)
			SELECT m.kind, m.gid, m.name, l.provider, l.model, COALESCE(l.unit, ''),
			       trim_scale(sum(l.reported))::text, trim_scale(sum(l.billed))::text, sum(l.tokens)::bigint
			FROM m JOIN l ON l.agent_id = m.agent_id
			 AND ((l.unit IS NULL AND m.count_tokens) OR (l.unit IS NOT NULL AND (m.unit IS NULL OR l.unit = m.unit)))
			GROUP BY 1, 2, 3, 4, 5, 6 ORDER BY 3, 1, 4, 5, 6`, from, to)
		if err != nil {
			return err
		}
		var ml ModelLine
		_, err = pgx.ForEachRow(rows, []any{&kind, &id, &name, &ml.Provider, &ml.Model, &ml.Unit, &r, &b, &ml.Tokens}, func() error {
			g := group(kind, id, name)
			g.Models = append(g.Models, ModelLine{Provider: ml.Provider, Model: ml.Model, Unit: ml.Unit,
				Reported: json.Number(r), Billed: json.Number(b), Tokens: ml.Tokens})
			return nil
		})
		return err
	})
	return out, err
}

// ---------------------------------------------------------------- dashboard

// AgentSpend is one agent's month-to-date spend in a unit.
type AgentSpend struct {
	AgentID uuid.UUID   `json:"agent_id"`
	Name    string      `json:"name"`
	Total   json.Number `json:"total"`
}

// UnitDashboard is the dashboard for one unit.
type UnitDashboard struct {
	Unit        string       `json:"unit"`
	Today       json.Number  `json:"today"`
	MonthToDate json.Number  `json:"month_to_date"`
	TopAgents   []AgentSpend `json:"top_agents"`
}

// Dashboard is the spend dashboard (ADR-025 §9). Days and months are UTC.
type Dashboard struct {
	AsOf                time.Time       `json:"as_of"`
	Units               []UnitDashboard `json:"units"`
	HardBlocksToday     map[string]int  `json:"hard_blocks_today"` // by denial reason
	OpenAlerts          map[string]int  `json:"open_alerts"`       // by kind
	UnpricedTokensToday int64           `json:"unpriced_tokens_today"`
}

// TopAgents is the number of agents per unit on the dashboard.
const TopAgents = 5

// Dashboard reads the spend dashboard in one snapshot.
func (s *Service) Dashboard(ctx context.Context, a registry.Actor) (Dashboard, error) {
	d := Dashboard{Units: []UnitDashboard{}, HardBlocksToday: map[string]int{}, OpenAlerts: map[string]int{}}
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		var month, day time.Time
		if err := tx.QueryRow(ctx, `SELECT now(), date_trunc('month', now(), 'UTC'), date_trunc('day', now(), 'UTC')`).
			Scan(&d.AsOf, &month, &day); err != nil {
			return err
		}
		units := map[string]int{}
		rows, err := tx.Query(ctx, `WITH mtd AS (SELECT * FROM eacp.finops_agent_spend($1, $3)),
			     today AS (SELECT * FROM eacp.finops_agent_spend($2, $3))
			SELECT u.unit,
			       trim_scale(COALESCE((SELECT sum(tool_committed + tool_held + llm_effective) FROM today WHERE today.unit = u.unit), 0))::text,
			       trim_scale(COALESCE((SELECT sum(tool_committed + tool_held + llm_effective) FROM mtd WHERE mtd.unit = u.unit), 0))::text
			FROM (SELECT DISTINCT unit FROM mtd) u ORDER BY 1`, month, day, d.AsOf)
		if err != nil {
			return err
		}
		var ud UnitDashboard
		var today, mtd string
		if _, err := pgx.ForEachRow(rows, []any{&ud.Unit, &today, &mtd}, func() error {
			units[ud.Unit] = len(d.Units)
			d.Units = append(d.Units, UnitDashboard{Unit: ud.Unit, Today: json.Number(today),
				MonthToDate: json.Number(mtd), TopAgents: []AgentSpend{}})
			return nil
		}); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT unit, agent_id, name, total FROM (
				SELECT s.unit, s.agent_id, a.name,
				       trim_scale(s.tool_committed + s.tool_held + s.llm_effective)::text AS total,
				       row_number() OVER (PARTITION BY s.unit
				                          ORDER BY s.tool_committed + s.tool_held + s.llm_effective DESC, a.name) AS n
				FROM eacp.finops_agent_spend($1, $2) s JOIN eacp.agents a ON a.id = s.agent_id
				WHERE s.tool_committed + s.tool_held + s.llm_effective > 0) x
			WHERE n <= $3 ORDER BY unit, n`, month, d.AsOf, TopAgents)
		if err != nil {
			return err
		}
		var unit, total string
		var ag AgentSpend
		if _, err := pgx.ForEachRow(rows, []any{&unit, &ag.AgentID, &ag.Name, &total}, func() error {
			if i, ok := units[unit]; ok {
				d.Units[i].TopAgents = append(d.Units[i].TopAgents, AgentSpend{AgentID: ag.AgentID, Name: ag.Name,
					Total: json.Number(total)})
			}
			return nil
		}); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT state_reason, count(*) FROM eacp.actions
			WHERE state = 'DENIED' AND state_changed_at >= $1
			  AND state_reason IN ('budget_exceeded', 'budget_account_missing', 'budget_cost_invalid')
			GROUP BY 1`, day)
		if err != nil {
			return err
		}
		var reason string
		var n int
		if _, err := pgx.ForEachRow(rows, []any{&reason, &n}, func() error {
			d.HardBlocksToday[reason] = n
			return nil
		}); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT kind, count(*) FROM eacp.finops_alerts WHERE acknowledged_at IS NULL GROUP BY 1`)
		if err != nil {
			return err
		}
		if _, err := pgx.ForEachRow(rows, []any{&reason, &n}, func() error {
			d.OpenAlerts[reason] = n
			return nil
		}); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT COALESCE(sum(input_tokens + output_tokens), 0)::bigint FROM eacp.usage_records
			WHERE billable AND cost_amount IS NULL AND observed_at >= $1`, day).Scan(&d.UnpricedTokensToday)
	})
	return d, err
}
