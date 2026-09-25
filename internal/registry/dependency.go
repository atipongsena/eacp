package registry

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/storage"
)

// DependencyInput is an observed relationship that is not already held in
// the capability registry. The database validates both tenant-local ends.
type DependencyInput struct {
	FromKind   string    `json:"from_kind"`
	FromID     uuid.UUID `json:"from_id"`
	ToKind     string    `json:"to_kind"`
	ToID       uuid.UUID `json:"to_id,omitempty"`
	ToName     string    `json:"to_name,omitempty"`
	Source     string    `json:"source"`
	Confidence string    `json:"confidence"`
	ObservedAt time.Time `json:"observed_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

type DependencyEdge struct {
	ID uuid.UUID `json:"id"`
	DependencyInput
}

type DependencyTarget struct {
	Kind string    `json:"kind"`
	ID   uuid.UUID `json:"id,omitempty"`
	Name string    `json:"name,omitempty"`
}

type DependencyImpact struct {
	AgentID     uuid.UUID `json:"agent_id"`
	VersionID   uuid.UUID `json:"version_id"`
	AgentName   string    `json:"agent_name"`
	Version     int       `json:"version"`
	Environment string    `json:"environment"`
	Team        string    `json:"team"`
	Owner       string    `json:"owner_principal,omitempty"`
}

type BlastRadiusReport struct {
	Target           DependencyTarget   `json:"target"`
	Confirmed        []DependencyImpact `json:"confirmed_agents"`
	Possible         []DependencyImpact `json:"possible_agents"`
	AffectedTeams    []string           `json:"affected_teams"`
	DataClasses      []string           `json:"data_classes"`
	RecentActions24h int                `json:"recent_actions_24h"`
	Coverage         string             `json:"coverage"`
}

// RecordDependency records immutable, journaled evidence. It never grants a
// capability; the active allowlist and contract remain the authority.
func (s *Service) RecordDependency(ctx context.Context, a Actor, in DependencyInput) (DependencyEdge, error) {
	var out DependencyEdge
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		out.DependencyInput = in
		return tx.QueryRow(ctx, `INSERT INTO eacp.dependency_edges
			(tenant_id, from_kind, from_id, to_kind, to_id, to_name, source, confidence, observed_at, expires_at)
			VALUES (eacp.current_tenant_id(), $1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
			in.FromKind, in.FromID, in.ToKind, nullID(in.ToID), nullStr(in.ToName),
			in.Source, in.Confidence, in.ObservedAt, in.ExpiresAt).Scan(&out.ID)
	})
	return out, err
}

func (s *Service) RevokeDependency(ctx context.Context, a Actor, id uuid.UUID, reason string) error {
	return s.change(ctx, a, func(tx pgx.Tx) error {
		return execOne(ctx, tx, `UPDATE eacp.dependency_edges SET revoked_at = now(), revoke_reason = $2
			WHERE id = $1 AND revoked_at IS NULL`, id, reason)
	})
}

func graphKey(kind string, id uuid.UUID, name string) string {
	if id != uuid.Nil {
		return kind + ":" + id.String()
	}
	return kind + ":" + name
}

// BlastRadius uses a single tenant transaction. Fresh, high-confidence
// evidence makes a confirmed path. Every stale, low-confidence or unknown
// edge widens the possible set, even if its target is not known. Absence of
// a declared edge is never reported as proof of no dependency.
func (s *Service) BlastRadius(ctx context.Context, a Actor, target DependencyTarget) (BlastRadiusReport, error) {
	report := BlastRadiusReport{Target: target, Confirmed: []DependencyImpact{}, Possible: []DependencyImpact{},
		AffectedTeams: []string{}, DataClasses: []string{}, Coverage: "observed_only"}
	if target.Kind != "mcp" && target.Kind != "tool" && target.Kind != "agent_version" &&
		target.Kind != "model" && target.Kind != "system" {
		return report, newErr(ErrInvalid, "unknown dependency target kind")
	}
	if (target.Kind == "model" || target.Kind == "system") != (target.ID == uuid.Nil && target.Name != "") {
		return report, newErr(ErrInvalid, "target requires an id or name for its kind")
	}
	if (target.Kind == "mcp" || target.Kind == "tool" || target.Kind == "agent_version") &&
		(target.ID == uuid.Nil || target.Name != "") {
		return report, newErr(ErrInvalid, "target requires an id")
	}
	if a.TenantID == uuid.Nil {
		return report, newErr(ErrForbidden, "no tenant")
	}
	err := storage.InTenantReadTx(ctx, s.pool, a.TenantID.String(), func(tx pgx.Tx) error {
		impacts := map[string]DependencyImpact{}
		rows, err := tx.Query(ctx, `SELECT v.id, v.agent_id, ag.name, v.version, ag.environment,
			COALESCE(g.name, ''), COALESCE(p.name, '')
			FROM eacp.agent_versions v JOIN eacp.agents ag ON ag.tenant_id = v.tenant_id AND ag.id = v.agent_id
			LEFT JOIN eacp.groups g ON g.tenant_id = ag.tenant_id AND g.id = ag.owner_group_id
			LEFT JOIN eacp.principals p ON p.tenant_id = ag.tenant_id AND p.id = ag.owner_principal_id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var v DependencyImpact
			if err := rows.Scan(&v.VersionID, &v.AgentID, &v.AgentName, &v.Version, &v.Environment, &v.Team, &v.Owner); err != nil {
				rows.Close()
				return err
			}
			impacts[graphKey("agent_version", v.VersionID, "")] = v
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		key := graphKey(target.Kind, target.ID, target.Name)
		if target.ID != uuid.Nil {
			var exists bool
			var sql string
			switch target.Kind {
			case "mcp":
				sql = `SELECT EXISTS (SELECT 1 FROM eacp.connectors WHERE id = $1 AND protocol = 'mcp')`
			case "tool":
				sql = `SELECT EXISTS (SELECT 1 FROM eacp.tools WHERE id = $1)`
			case "agent_version":
				sql = `SELECT EXISTS (SELECT 1 FROM eacp.agent_versions WHERE id = $1)`
			}
			if err := tx.QueryRow(ctx, sql, target.ID).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return newErr(ErrNotFound, "no such dependency target")
			}
		}
		// PostgreSQL expands both reachable sets with recursive CTEs
		// (eacp.blast_radius_versions, shared with the incident evaluator). A
		// stale, low-confidence or unknown edge seeds possible impact
		// regardless of where its target was last reported.
		rows, err = tx.Query(ctx, `SELECT version_id, confirmed, possible FROM eacp.blast_radius_versions(ARRAY[$1::text])`, key)
		if err != nil {
			return err
		}
		confirmed := map[string]bool{}
		possible := map[string]bool{}
		for rows.Next() {
			var id uuid.UUID
			var yes, maybe bool
			if err := rows.Scan(&id, &yes, &maybe); err != nil {
				rows.Close()
				return err
			}
			k := graphKey("agent_version", id, "")
			confirmed[k] = yes
			possible[k] = maybe
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		ids := []uuid.UUID{}
		teams := map[string]bool{}
		for k, impact := range impacts {
			if confirmed[k] {
				report.Confirmed = append(report.Confirmed, impact)
			} else if possible[k] {
				report.Possible = append(report.Possible, impact)
			} else {
				continue
			}
			ids = append(ids, impact.VersionID)
			if impact.Team != "" {
				teams[impact.Team] = true
			}
		}
		for team := range teams {
			report.AffectedTeams = append(report.AffectedTeams, team)
		}
		sort.Strings(report.AffectedTeams)
		sort.Slice(report.Confirmed, func(i, j int) bool { return report.Confirmed[i].AgentName < report.Confirmed[j].AgentName })
		sort.Slice(report.Possible, func(i, j int) bool { return report.Possible[i].AgentName < report.Possible[j].AgentName })
		if len(ids) == 0 {
			return nil
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM eacp.actions a JOIN eacp.agent_versions v
			ON v.tenant_id = a.tenant_id AND v.id = a.agent_version_id
			JOIN eacp.agents ag ON ag.tenant_id = v.tenant_id AND ag.id = v.agent_id
			WHERE a.agent_version_id = ANY($1) AND ag.environment = 'production'
			AND a.created_at >= now() - interval '24 hours'`, ids).Scan(&report.RecentActions24h); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT DISTINCT tc.data_sensitivity FROM eacp.agent_versions v
			JOIN eacp.agent_allowlists al ON al.tenant_id = v.tenant_id AND al.id = v.active_allowlist_id
			JOIN eacp.tools t ON t.tenant_id = al.tenant_id AND t.id = ANY(al.tool_ids)
			JOIN eacp.tool_contracts tc ON tc.tenant_id = t.tenant_id AND tc.id = t.active_contract_id
			WHERE v.id = ANY($1) AND tc.data_sensitivity IS NOT NULL ORDER BY 1`, ids)
		if err != nil {
			return err
		}
		for rows.Next() {
			var sensitivity string
			if err := rows.Scan(&sensitivity); err != nil {
				rows.Close()
				return err
			}
			report.DataClasses = append(report.DataClasses, sensitivity)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		return nil
	})
	return report, mapErr(err)
}
