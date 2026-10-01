// Package studio is Agent Studio's store (Phase 27a-1, ADR-033): saving
// agent definitions and deciding their capability requests. PostgreSQL
// validates every definition, derives its capability and enforces every rule
// (migration 00027, eacp.studio_save and eacp.studio_decide); this package
// canonicalizes the definition, calls those functions and reads the rows,
// computing each version's status from them.
package studio

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atipongsena/eacp/internal/governance"
	"github.com/atipongsena/eacp/internal/registry"
	"github.com/atipongsena/eacp/internal/storage"
)

// Service is the Studio store over a pool connected as the application role.
type Service struct{ pool *pgxpool.Pool }

// New returns a Service.
func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// NewAgent is a new Studio agent and the definition of its version 1.
type NewAgent struct {
	Name         string          `json:"name"`
	DisplayName  string          `json:"display_name"`
	Description  string          `json:"description"`
	DepartmentID uuid.UUID       `json:"department_id"`
	Definition   json.RawMessage `json:"definition"`
}

// Version is a Studio version with its computed status. Status names the
// stage (spec 27a-1 section 3.7) and WaitingOn the role that acts next.
type Version struct {
	ID             uuid.UUID       `json:"id"`
	AgentID        uuid.UUID       `json:"agent_id"`
	AgentName      string          `json:"agent_name"`
	Version        int             `json:"version"`
	State          string          `json:"state"`
	Status         string          `json:"status"`
	WaitingOn      string          `json:"waiting_on,omitempty"`
	Digest         string          `json:"digest"`
	Capability     []string        `json:"capability"`
	Models         []string        `json:"models"`
	Definition     json.RawMessage `json:"definition,omitempty"`
	CreatedBy      uuid.UUID       `json:"created_by"`
	CreatedAt      time.Time       `json:"created_at"`
	Decision       *string         `json:"decision,omitempty"`
	DecidedBy      *uuid.UUID      `json:"decided_by,omitempty"`
	DecidedAt      *time.Time      `json:"decided_at,omitempty"`
	DecisionReason *string         `json:"decision_reason,omitempty"`
}

// Agent is a Studio agent with its latest version.
type Agent struct {
	ID           uuid.UUID `json:"id"`
	Name         string    `json:"name"`
	DisplayName  string    `json:"display_name"`
	Description  string    `json:"description"`
	DepartmentID uuid.UUID `json:"department_id"`
	OwnerID      uuid.UUID `json:"owner_id"`
	CreatedAt    time.Time `json:"created_at"`
	Latest       Version   `json:"latest"`
}

// Tool is one tool of a request, described for its approver.
type Tool struct {
	Ref         string   `json:"ref"`
	Protocol    string   `json:"protocol"`
	SideEffects []string `json:"side_effects"`
	PlainWords  string   `json:"in_plain_words"`
}

// Request is an undecided version and the tools it asks for.
type Request struct {
	Version
	DepartmentID uuid.UUID `json:"department_id"`
	Tools        []Tool    `json:"tools"`
}

// Save creates a Studio agent and its version 1 as a.
func (s *Service) Save(ctx context.Context, a registry.Actor, in NewAgent) (Version, error) {
	def, err := canonical(in.Definition)
	if err != nil {
		return Version{}, err
	}
	var id uuid.UUID
	err = s.change(ctx, a, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT eacp.studio_save(NULL, $1, $2, $3, $4, $5)`,
			in.Name, in.DisplayName, in.Description, nullID(in.DepartmentID), def).Scan(&id)
	})
	if err != nil {
		return Version{}, err
	}
	return s.Version(ctx, a, id, true)
}

// AddVersion saves the next version of the caller's Studio agent.
func (s *Service) AddVersion(ctx context.Context, a registry.Actor, agent uuid.UUID, definition json.RawMessage) (Version, error) {
	return s.addVersion(ctx, a, agent, definition, uuid.Nil)
}

// AddVersionChecked refuses an edit whose base version was replaced.
func (s *Service) AddVersionChecked(ctx context.Context, a registry.Actor, agent uuid.UUID, definition json.RawMessage, expected uuid.UUID) (Version, error) {
	return s.addVersion(ctx, a, agent, definition, expected)
}

func (s *Service) addVersion(ctx context.Context, a registry.Actor, agent uuid.UUID, definition json.RawMessage, expected uuid.UUID) (Version, error) {
	def, err := canonical(definition)
	if err != nil {
		return Version{}, err
	}
	var id uuid.UUID
	err = s.change(ctx, a, func(tx pgx.Tx) error {
		var schema struct {
			Version int `json:"schema_version"`
		}
		if err := json.Unmarshal(definition, &schema); err != nil {
			return err
		}
		if expected != uuid.Nil || schema.Version == 2 {
			return tx.QueryRow(ctx, `SELECT eacp.studio_save_checked($1,NULL,NULL,NULL,NULL,$2,$3)`, agent, def, nullID(expected)).Scan(&id)
		}
		return tx.QueryRow(ctx, `SELECT eacp.studio_save($1, NULL, NULL, NULL, NULL, $2)`, agent, def).Scan(&id)
	})
	if err != nil {
		return Version{}, err
	}
	return s.Version(ctx, a, id, true)
}

// Decide approves or rejects version's capability request as a.
func (s *Service) Decide(ctx context.Context, a registry.Actor, version uuid.UUID, approve bool, reason string) (Version, error) {
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT eacp.studio_decide($1, $2, $3)`, version, approve, reason)
		return err
	})
	if err != nil {
		return Version{}, err
	}
	return s.Version(ctx, a, version, true)
}

// versionSQL selects a Studio version and computes its status from the rows.
const versionSQL = `SELECT s.id, s.agent_id, g.name, v.version, v.state,
	CASE
		WHEN s.decision IS NULL AND v.state = 'REGISTERED' THEN 'waiting_for_approval'
		WHEN s.decision = 'rejected' THEN 'rejected'
		WHEN s.decision = 'approved' AND v.state = 'ACTIVE' THEN
			CASE WHEN c.approved THEN 'ready' ELSE 'waiting_for_credential' END
		WHEN v.state = 'RETIRED' AND EXISTS (
			SELECT 1 FROM eacp.studio_versions n JOIN eacp.agent_versions nv ON nv.tenant_id = n.tenant_id AND nv.id = n.id
			WHERE n.tenant_id = s.tenant_id AND n.agent_id = s.agent_id AND n.decision = 'approved' AND nv.version > v.version)
			THEN 'replaced'
		ELSE lower(v.state)
	END,
	CASE
		WHEN s.decision IS NULL AND v.state = 'REGISTERED' THEN 'registry_approver'
		WHEN s.decision = 'approved' AND v.state = 'ACTIVE' AND NOT c.approved THEN
			CASE WHEN c.pending THEN 'registry_approver' ELSE 'studio_runtime' END
		ELSE ''
	END,
	s.digest, s.capability, ARRAY(SELECT m.name FROM eacp.llm_models m WHERE m.tenant_id=s.tenant_id AND m.id=ANY(s.model_capability) ORDER BY m.name), s.definition, s.created_by, s.created_at,
	s.decision, s.decided_by, s.decided_at, s.decision_reason
	FROM eacp.studio_versions s
	JOIN eacp.agent_versions v ON v.tenant_id = s.tenant_id AND v.id = s.id
	JOIN eacp.agents g ON g.tenant_id = s.tenant_id AND g.id = s.agent_id
	CROSS JOIN LATERAL (SELECT
		EXISTS (SELECT 1 FROM eacp.credentials k WHERE k.tenant_id = s.tenant_id AND k.agent_version_id = s.id
			AND k.approved_at IS NOT NULL AND k.revoked_at IS NULL AND k.expires_at > now()) AS approved,
		EXISTS (SELECT 1 FROM eacp.credentials k WHERE k.tenant_id = s.tenant_id AND k.agent_version_id = s.id
			AND k.approved_at IS NULL AND k.revoked_at IS NULL AND k.expires_at > now()) AS pending) c`

func scanVersion(row pgx.Row) (Version, error) {
	var v Version
	var def string
	err := row.Scan(&v.ID, &v.AgentID, &v.AgentName, &v.Version, &v.State, &v.Status, &v.WaitingOn,
		&v.Digest, &v.Capability, &v.Models, &def, &v.CreatedBy, &v.CreatedAt,
		&v.Decision, &v.DecidedBy, &v.DecidedAt, &v.DecisionReason)
	v.Definition = json.RawMessage(def)
	return v, err
}

// Version returns version id. Unless all, only a's own versions are visible;
// any other is not found.
func (s *Service) Version(ctx context.Context, a registry.Actor, id uuid.UUID, all bool) (Version, error) {
	var v Version
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		var err error
		v, err = scanVersion(tx.QueryRow(ctx, versionSQL+` WHERE s.id = $1 AND ($2 OR g.owner_principal_id = $3)`,
			id, all, a.PrincipalID))
		return err
	})
	return v, err
}

// Agents lists Studio agents with their latest version: a's own, or all.
func (s *Service) Agents(ctx context.Context, a registry.Actor, all bool) ([]Agent, error) {
	out := []Agent{}
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT g.id, g.name, g.display_name, sa.description, sa.department_group_id,
				g.owner_principal_id, g.created_at, latest.id
			FROM eacp.studio_agents sa
			JOIN eacp.agents g ON g.tenant_id = sa.tenant_id AND g.id = sa.id
			CROSS JOIN LATERAL (SELECT v.id FROM eacp.agent_versions v
				WHERE v.tenant_id = sa.tenant_id AND v.agent_id = sa.id ORDER BY v.version DESC LIMIT 1) latest
			WHERE $1 OR g.owner_principal_id = $2
			ORDER BY g.name`, all, a.PrincipalID)
		if err != nil {
			return err
		}
		ids := []uuid.UUID{}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Agent, error) {
			var g Agent
			var latest uuid.UUID
			err := r.Scan(&g.ID, &g.Name, &g.DisplayName, &g.Description, &g.DepartmentID, &g.OwnerID, &g.CreatedAt, &latest)
			ids = append(ids, latest)
			return g, err
		})
		if err != nil {
			return err
		}
		for i := range out {
			if out[i].Latest, err = scanVersion(tx.QueryRow(ctx, versionSQL+` WHERE s.id = $1`, ids[i])); err != nil {
				return err
			}
			out[i].Latest.Definition = nil
		}
		return nil
	})
	return out, err
}

// Requests lists the undecided versions, oldest first, with each tool's
// protocol and contract side effects in plain words.
func (s *Service) Requests(ctx context.Context, a registry.Actor) ([]Request, error) {
	out := []Request{}
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, versionSQL+` WHERE s.decision IS NULL AND v.state = 'REGISTERED' ORDER BY s.created_at, s.id`)
		if err != nil {
			return err
		}
		versions, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Version, error) { return scanVersion(r) })
		if err != nil {
			return err
		}
		for _, v := range versions {
			r := Request{Version: v, Tools: []Tool{}}
			if err := tx.QueryRow(ctx, `SELECT department_group_id FROM eacp.studio_agents WHERE id = $1`,
				v.AgentID).Scan(&r.DepartmentID); err != nil {
				return err
			}
			var err error
			if r.Tools, err = toolsOf(ctx, tx, v.Capability); err != nil {
				return err
			}
			out = append(out, r)
		}
		return nil
	})
	return out, err
}

// plain describes each contract side effect for an approver.
var plain = map[string]string{
	"READ_ONLY":              "reads data and changes nothing",
	"REVERSIBLE_WRITE":       "changes data in a way that can be undone",
	"IRREVERSIBLE_WRITE":     "changes data in a way that cannot be undone",
	"EXTERNAL_COMMUNICATION": "sends messages outside the system",
	"FINANCIAL":              "moves money or makes a financial commitment",
	"ADMINISTRATIVE":         "changes accounts, permissions or settings",
}

// PlainWords describes side effects in one sentence. A tool without an
// active contract cannot run.
func PlainWords(effects []string) string {
	if len(effects) == 0 {
		return "has no active contract, so it cannot run"
	}
	out := ""
	for i, e := range effects {
		p, ok := plain[e]
		if !ok {
			p = e
		}
		if i > 0 {
			out += "; "
		}
		out += p
	}
	return out
}

func canonical(def json.RawMessage) (string, error) {
	if len(def) == 0 {
		return "", &registry.Error{Kind: registry.ErrInvalid, Msg: "definition is required"}
	}
	out, err := governance.Canonicalize(def)
	if err != nil {
		return "", &registry.Error{Kind: registry.ErrInvalid, Msg: "definition: " + err.Error()}
	}
	return string(out), nil
}

func nullID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

func (s *Service) change(ctx context.Context, a registry.Actor, fn func(pgx.Tx) error) error {
	if a.TenantID == uuid.Nil || a.PrincipalID == uuid.Nil {
		return &registry.Error{Kind: registry.ErrForbidden, Msg: "no actor"}
	}
	return classify(storage.InTenantTx(ctx, s.pool, a.TenantID.String(), func(tx pgx.Tx) error {
		if err := storage.SetActor(ctx, tx, a.PrincipalID); err != nil {
			return err
		}
		return fn(tx)
	}))
}

func (s *Service) read(ctx context.Context, a registry.Actor, fn func(pgx.Tx) error) error {
	if a.TenantID == uuid.Nil {
		return &registry.Error{Kind: registry.ErrForbidden, Msg: "no tenant"}
	}
	return classify(storage.InTenantReadTx(ctx, s.pool, a.TenantID.String(), fn))
}

// classify maps the SQLSTATEs of migration 00027 as the registry does.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var re *registry.Error
	if errors.As(err, &re) {
		return err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return &registry.Error{Kind: registry.ErrNotFound, Msg: "no such Studio agent or version"}
	}
	var p *pgconn.PgError
	if !errors.As(err, &p) {
		return err
	}
	switch p.Code {
	case "42501":
		return &registry.Error{Kind: registry.ErrForbidden, Msg: p.Message}
	case "55000", "23505":
		return &registry.Error{Kind: registry.ErrConflict, Msg: p.Message}
	case "23503":
		return &registry.Error{Kind: registry.ErrNotFound, Msg: p.Message}
	case "23514", "23502", "22P02", "22023", "22001":
		return &registry.Error{Kind: registry.ErrInvalid, Msg: p.Message}
	}
	return err
}
