// Package fleet applies fleet operations and reports the fleet view
// (ADR-024).
//
// An operation (pause, resume, quarantine, release, rollback) is a set of
// ADR-003 lifecycle transitions that PostgreSQL makes atomically (migration
// 00017): inserting a target performs its transition under the version
// guard, and the operation is journaled at commit. This package only
// selects the targets. The fleet view is a read-only observation; it never
// grants, withholds or triggers anything.
package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"eacp/internal/registry"
	"eacp/internal/storage"
)

// MaxTargets is the most versions one operation may change (ADR-024 §3).
const MaxTargets = 500

// Selector chooses agents for pause and quarantine, and the one agent of a
// rollback. Filters combine with AND. An empty selector selects nothing:
// every agent needs All.
type Selector struct {
	AgentIDs     []uuid.UUID `json:"agent_ids,omitempty"`
	Agents       []string    `json:"agents,omitempty"` // slugs
	Environment  string      `json:"environment,omitempty"`
	RiskClass    string      `json:"risk_class,omitempty"`
	OwnerGroupID uuid.UUID   `json:"owner_group_id,omitzero"`
	Tool         string      `json:"tool,omitempty"` // "connector.tool" on the active allowlist
	All          bool        `json:"all,omitempty"`
}

func (s Selector) explicit() int { return len(s.AgentIDs) + len(s.Agents) }

func (s Selector) filtered() bool {
	return s.Environment != "" || s.RiskClass != "" || s.OwnerGroupID != uuid.Nil || s.Tool != ""
}

func (s Selector) empty() bool { return !s.All && s.explicit() == 0 && !s.filtered() }

// Request is one fleet operation.
type Request struct {
	Kind     string   `json:"kind"`
	Selector Selector `json:"selector"`
	// SourceOperationID is the pause a resume undoes, or the quarantine a
	// release undoes. They select nothing else.
	SourceOperationID uuid.UUID `json:"source_operation_id,omitzero"`
	// ToVersionID names the rollback target; by default it is the newest
	// SUSPENDED version older than the one being replaced.
	ToVersionID uuid.UUID `json:"to_version_id,omitzero"`
	Reason      string    `json:"reason"`
	DryRun      bool      `json:"dry_run,omitempty"`
}

// Target is one version an operation changes. From is the state PostgreSQL
// read when it made the transition (the planned state in a dry run).
type Target struct {
	AgentID   uuid.UUID `json:"agent_id"`
	AgentName string    `json:"agent_name"`
	VersionID uuid.UUID `json:"version_id"`
	Version   int       `json:"version"`
	From      string    `json:"from"`
	To        string    `json:"to"`
}

// Skip is a selected agent the operation leaves unchanged.
type Skip struct {
	AgentID   uuid.UUID `json:"agent_id"`
	AgentName string    `json:"agent_name"`
	Reason    string    `json:"reason"`
}

// Operation is an applied operation, or the plan of a dry run (no ID).
type Operation struct {
	ID                uuid.UUID       `json:"id,omitzero"`
	Kind              string          `json:"kind"`
	DryRun            bool            `json:"dry_run,omitempty"`
	Reason            string          `json:"reason"`
	Selector          json.RawMessage `json:"selector"`
	SourceOperationID uuid.UUID       `json:"source_operation_id,omitzero"`
	CreatedBy         uuid.UUID       `json:"created_by,omitzero"`
	CreatedAt         time.Time       `json:"created_at,omitzero"`
	Targets           []Target        `json:"targets"`
	Skipped           []Skip          `json:"skipped"`
}

// Service runs fleet operations over a pool connected as the application role.
type Service struct{ pool *pgxpool.Pool }

// New returns a Service.
func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

func invalid(format string, args ...any) error {
	return &registry.Error{Kind: registry.ErrInvalid, Msg: fmt.Sprintf(format, args...)}
}

func conflict(format string, args ...any) error {
	return &registry.Error{Kind: registry.ErrConflict, Msg: fmt.Sprintf(format, args...)}
}

func notFound(format string, args ...any) error {
	return &registry.Error{Kind: registry.ErrNotFound, Msg: fmt.Sprintf(format, args...)}
}

var (
	environments = []string{"development", "staging", "production"}
	riskClasses  = []string{"low", "medium", "high", "critical"}
)

func oneOf(v string, set []string) bool {
	for _, s := range set {
		if v == s {
			return true
		}
	}
	return false
}

func (r Request) validate() error {
	if strings.TrimSpace(r.Reason) == "" || len(r.Reason) > 1024 {
		return invalid("a fleet operation needs a reason of at most 1024 bytes")
	}
	s := r.Selector
	if s.Environment != "" && !oneOf(s.Environment, environments) {
		return invalid("unknown environment %q", s.Environment)
	}
	if s.RiskClass != "" && !oneOf(s.RiskClass, riskClasses) {
		return invalid("unknown risk class %q", s.RiskClass)
	}
	if s.Tool != "" {
		if conn, tool, ok := strings.Cut(s.Tool, "."); !ok || conn == "" || tool == "" {
			return invalid("tool must be \"connector.tool\"")
		}
	}
	switch r.Kind {
	case "pause", "quarantine":
		if r.SourceOperationID != uuid.Nil || r.ToVersionID != uuid.Nil {
			return invalid("a %s takes a selector only", r.Kind)
		}
		if s.All && (s.explicit() > 0 || s.filtered()) {
			return invalid("all selects every agent; do not combine it with other criteria")
		}
		if s.empty() {
			return invalid("select agents, or every agent with all")
		}
	case "resume", "release":
		if !s.empty() || s.All || r.ToVersionID != uuid.Nil {
			return invalid("a %s undoes one operation and takes no selector", r.Kind)
		}
		if r.SourceOperationID == uuid.Nil {
			return invalid("a %s needs source_operation_id", r.Kind)
		}
	case "rollback":
		if r.SourceOperationID != uuid.Nil || s.All || s.filtered() || s.explicit() != 1 {
			return invalid("a rollback names exactly one agent")
		}
	default:
		return invalid("unknown fleet operation %q", r.Kind)
	}
	return nil
}

// errDryRun rolls back a dry run's transaction.
var errDryRun = errors.New("fleet: dry run")

// Apply applies r atomically, or with DryRun returns its plan. Every
// transition is authorized by PostgreSQL; one refusal fails the operation.
func (s *Service) Apply(ctx context.Context, a registry.Actor, r Request) (Operation, error) {
	if a.TenantID == uuid.Nil || a.PrincipalID == uuid.Nil {
		return Operation{}, &registry.Error{Kind: registry.ErrForbidden, Msg: "no actor"}
	}
	if err := r.validate(); err != nil {
		return Operation{}, err
	}
	record := map[string]any{"selector": r.Selector}
	if r.ToVersionID != uuid.Nil {
		record["to_version_id"] = r.ToVersionID
	}
	sel, err := json.Marshal(record)
	if err != nil {
		return Operation{}, err
	}
	op := Operation{Kind: r.Kind, DryRun: r.DryRun, Reason: r.Reason, Selector: sel,
		SourceOperationID: r.SourceOperationID}
	err = storage.InTenantTx(ctx, s.pool, a.TenantID.String(), func(tx pgx.Tx) error {
		if err := storage.SetActor(ctx, tx, a.PrincipalID); err != nil {
			return err
		}
		var err error
		if op.Targets, op.Skipped, err = plan(ctx, tx, r); err != nil {
			return err
		}
		op.Targets, op.Skipped = nonNil(op.Targets), nonNil(op.Skipped)
		if len(op.Targets) > MaxTargets {
			return invalid("%d versions selected; an operation changes at most %d", len(op.Targets), MaxTargets)
		}
		if r.DryRun {
			return errDryRun
		}
		if len(op.Targets) == 0 {
			return conflict("nothing to change")
		}
		if err := tx.QueryRow(ctx, `INSERT INTO eacp.fleet_operations
			(tenant_id, kind, selector, reason, source_operation_id)
			VALUES (eacp.current_tenant_id(), $1, $2, $3, $4) RETURNING id, created_by, created_at`,
			r.Kind, sel, r.Reason, nullID(r.SourceOperationID)).Scan(&op.ID, &op.CreatedBy, &op.CreatedAt); err != nil {
			return err
		}
		for i := range op.Targets {
			if err := tx.QueryRow(ctx, `INSERT INTO eacp.fleet_operation_targets
				(tenant_id, operation_id, version_id, to_state)
				VALUES (eacp.current_tenant_id(), $1, $2, $3) RETURNING from_state`,
				op.ID, op.Targets[i].VersionID, op.Targets[i].To).Scan(&op.Targets[i].From); err != nil {
				return err
			}
		}
		return nil
	})
	if errors.Is(err, errDryRun) {
		return op, nil
	}
	if err != nil {
		return Operation{}, classify(err)
	}
	return op, nil
}

// plan selects the targets of r, locking their versions in a stable order.
func plan(ctx context.Context, tx pgx.Tx, r Request) ([]Target, []Skip, error) {
	switch r.Kind {
	case "pause", "quarantine":
		return planSelected(ctx, tx, r)
	case "resume", "release":
		return planUndo(ctx, tx, r)
	default:
		return planRollback(ctx, tx, r)
	}
}

type agentRef struct {
	ID   uuid.UUID
	Name string
}

// selectAgents resolves s. Explicitly named agents must exist; those the
// filters exclude are skipped.
func selectAgents(ctx context.Context, tx pgx.Tx, s Selector) ([]agentRef, []Skip, error) {
	ids, names := s.AgentIDs, s.Agents
	if ids == nil {
		ids = []uuid.UUID{}
	}
	if names == nil {
		names = []string{}
	}
	var named []agentRef
	if s.explicit() > 0 {
		rows, err := tx.Query(ctx, `SELECT id, name FROM eacp.agents WHERE id = ANY ($1) OR name = ANY ($2)`, ids, names)
		if err != nil {
			return nil, nil, err
		}
		named, err = pgx.CollectRows(rows, pgx.RowToStructByPos[agentRef])
		if err != nil {
			return nil, nil, err
		}
		for _, id := range ids {
			if !hasAgent(named, func(a agentRef) bool { return a.ID == id }) {
				return nil, nil, notFound("no agent %s", id)
			}
		}
		for _, n := range names {
			if !hasAgent(named, func(a agentRef) bool { return a.Name == n }) {
				return nil, nil, notFound("no agent %q", n)
			}
		}
	}
	rows, err := tx.Query(ctx, `
		SELECT a.id, a.name FROM eacp.agents a
		WHERE ($1 OR ((cardinality($2::uuid[]) = 0 AND cardinality($3::text[]) = 0)
		              OR a.id = ANY ($2) OR a.name = ANY ($3)))
		  AND ($4 = '' OR a.environment = $4)
		  AND ($5 = '' OR a.risk_class = $5)
		  AND ($6::uuid IS NULL OR a.owner_group_id = $6)
		  AND ($7 = '' OR EXISTS (
		      SELECT 1 FROM eacp.agent_versions v
		      JOIN eacp.agent_allowlists al ON al.tenant_id = v.tenant_id AND al.id = v.active_allowlist_id
		      JOIN eacp.tools t ON t.tenant_id = al.tenant_id AND t.id = ANY (al.tool_ids)
		      JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
		      WHERE v.tenant_id = a.tenant_id AND v.agent_id = a.id AND v.state = 'ACTIVE'
		        AND c.name || '.' || t.name = $7))
		ORDER BY a.name`,
		s.All, ids, names, s.Environment, s.RiskClass, nullID(s.OwnerGroupID), s.Tool)
	if err != nil {
		return nil, nil, err
	}
	agents, err := pgx.CollectRows(rows, pgx.RowToStructByPos[agentRef])
	if err != nil {
		return nil, nil, err
	}
	var skipped []Skip
	for _, n := range named {
		if !hasAgent(agents, func(a agentRef) bool { return a.ID == n.ID }) {
			skipped = append(skipped, Skip{AgentID: n.ID, AgentName: n.Name, Reason: "does not match the other criteria"})
		}
	}
	return agents, skipped, nil
}

func hasAgent(list []agentRef, match func(agentRef) bool) bool {
	for _, a := range list {
		if match(a) {
			return true
		}
	}
	return false
}

func planSelected(ctx context.Context, tx pgx.Tx, r Request) ([]Target, []Skip, error) {
	agents, skipped, err := selectAgents(ctx, tx, r.Selector)
	if err != nil {
		return nil, nil, err
	}
	from, to, why := []string{"ACTIVE"}, "SUSPENDED", "no ACTIVE version"
	if r.Kind == "quarantine" {
		from, to, why = []string{"REGISTERED", "ACTIVE", "SUSPENDED"}, "QUARANTINED", "no version to quarantine"
	}
	ids := make([]uuid.UUID, len(agents))
	for i, a := range agents {
		ids[i] = a.ID
	}
	rows, err := tx.Query(ctx, `
		SELECT v.agent_id, a.name, v.id, v.version, v.state, $3::text
		FROM eacp.agent_versions v
		JOIN eacp.agents a ON a.tenant_id = v.tenant_id AND a.id = v.agent_id
		WHERE v.agent_id = ANY ($1) AND v.state = ANY ($2)
		ORDER BY v.id
		FOR UPDATE OF v`, ids, from, to)
	if err != nil {
		return nil, nil, err
	}
	targets, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Target])
	if err != nil {
		return nil, nil, err
	}
	for _, a := range agents {
		found := false
		for _, t := range targets {
			found = found || t.AgentID == a.ID
		}
		if !found {
			skipped = append(skipped, Skip{AgentID: a.ID, AgentName: a.Name, Reason: why})
		}
	}
	return targets, skipped, nil
}

func planUndo(ctx context.Context, tx pgx.Tx, r Request) ([]Target, []Skip, error) {
	want, from, to := "pause", "SUSPENDED", "ACTIVE"
	if r.Kind == "release" {
		want, from, to = "quarantine", "QUARANTINED", "SUSPENDED"
	}
	var kind string
	err := tx.QueryRow(ctx, `SELECT kind FROM eacp.fleet_operations WHERE id = $1`, r.SourceOperationID).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, notFound("no fleet operation %s", r.SourceOperationID)
	}
	if err != nil {
		return nil, nil, err
	}
	if kind != want {
		return nil, nil, invalid("a %s undoes a %s, not a %s", r.Kind, want, kind)
	}
	rows, err := tx.Query(ctx, `
		SELECT t.agent_id, a.name, v.id, v.version, v.state,
		       EXISTS (SELECT 1 FROM eacp.agent_versions o
		               WHERE o.tenant_id = v.tenant_id AND o.agent_id = v.agent_id AND o.state = 'ACTIVE')
		FROM eacp.fleet_operation_targets t
		JOIN eacp.agent_versions v ON v.tenant_id = t.tenant_id AND v.id = t.version_id
		JOIN eacp.agents a ON a.tenant_id = t.tenant_id AND a.id = t.agent_id
		WHERE t.operation_id = $1 AND t.to_state = $2
		ORDER BY v.id
		FOR UPDATE OF v`, r.SourceOperationID, from)
	if err != nil {
		return nil, nil, err
	}
	var targets []Target
	var skipped []Skip
	var t Target
	var otherActive bool
	_, err = pgx.ForEachRow(rows, []any{&t.AgentID, &t.AgentName, &t.VersionID, &t.Version, &t.From, &otherActive}, func() error {
		switch {
		case t.From != from:
			skipped = append(skipped, Skip{AgentID: t.AgentID, AgentName: t.AgentName, Reason: "version is no longer " + from})
		case to == "ACTIVE" && otherActive:
			skipped = append(skipped, Skip{AgentID: t.AgentID, AgentName: t.AgentName, Reason: "another version is ACTIVE"})
		default:
			t.To = to
			targets = append(targets, t)
		}
		return nil
	})
	return targets, skipped, err
}

func planRollback(ctx context.Context, tx pgx.Tx, r Request) ([]Target, []Skip, error) {
	agents, _, err := selectAgents(ctx, tx, r.Selector)
	if err != nil {
		return nil, nil, err
	}
	agent := agents[0] // the one explicitly named agent, which exists
	type version struct {
		ID        uuid.UUID
		Number    int
		State     string
		Allowlist bool
	}
	rows, err := tx.Query(ctx, `SELECT id, version, state, active_allowlist_id IS NOT NULL
		FROM eacp.agent_versions WHERE agent_id = $1 ORDER BY id FOR UPDATE`, agent.ID)
	if err != nil {
		return nil, nil, err
	}
	versions, err := pgx.CollectRows(rows, pgx.RowToStructByPos[version])
	if err != nil {
		return nil, nil, err
	}
	var current *version
	newest := 0
	for i, v := range versions {
		if v.State == "ACTIVE" {
			current = &versions[i]
		}
		newest = max(newest, v.Number)
	}
	bound := newest
	if current != nil {
		bound = current.Number
	}
	var to *version
	for i, v := range versions {
		switch {
		case r.ToVersionID != uuid.Nil:
			if v.ID == r.ToVersionID {
				to = &versions[i]
			}
		case v.State == "SUSPENDED" && v.Allowlist && v.Number < bound && (to == nil || v.Number > to.Number):
			to = &versions[i]
		}
	}
	switch {
	case r.ToVersionID != uuid.Nil && to == nil:
		return nil, nil, notFound("version %s is not a version of %s", r.ToVersionID, agent.Name)
	case to == nil:
		return nil, nil, conflict("%s has no SUSPENDED version older than version %d to roll back to", agent.Name, bound)
	case to.State != "SUSPENDED":
		return nil, nil, conflict("version %d is %s; a rollback activates a SUSPENDED version", to.Number, to.State)
	case to.Number >= bound:
		return nil, nil, conflict("version %d is not older than version %d", to.Number, bound)
	}
	var targets []Target
	if current != nil {
		targets = append(targets, Target{AgentID: agent.ID, AgentName: agent.Name, VersionID: current.ID,
			Version: current.Number, From: current.State, To: "SUSPENDED"})
	}
	targets = append(targets, Target{AgentID: agent.ID, AgentName: agent.Name, VersionID: to.ID,
		Version: to.Number, From: to.State, To: "ACTIVE"})
	return targets, nil, nil
}

// Operation returns a recorded operation with its targets.
func (s *Service) Operation(ctx context.Context, a registry.Actor, id uuid.UUID) (Operation, error) {
	op := Operation{Targets: []Target{}, Skipped: []Skip{}}
	err := storage.InTenantReadTx(ctx, s.pool, a.TenantID.String(), func(tx pgx.Tx) error {
		var source *uuid.UUID
		err := tx.QueryRow(ctx, `SELECT id, kind, reason, selector, source_operation_id, created_by, created_at
			FROM eacp.fleet_operations WHERE id = $1`, id).
			Scan(&op.ID, &op.Kind, &op.Reason, &op.Selector, &source, &op.CreatedBy, &op.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return notFound("no fleet operation %s", id)
		}
		if err != nil {
			return err
		}
		if source != nil {
			op.SourceOperationID = *source
		}
		rows, err := tx.Query(ctx, `SELECT t.agent_id, a.name, t.version_id, v.version, t.from_state, t.to_state
			FROM eacp.fleet_operation_targets t
			JOIN eacp.agents a ON a.tenant_id = t.tenant_id AND a.id = t.agent_id
			JOIN eacp.agent_versions v ON v.tenant_id = t.tenant_id AND v.id = t.version_id
			WHERE t.operation_id = $1 ORDER BY t.ordinal`, id)
		if err != nil {
			return err
		}
		op.Targets, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Target])
		return err
	})
	if err != nil {
		return Operation{}, classify(err)
	}
	return op, nil
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func nullID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

// classify maps the SQLSTATEs of the fleet and version guards.
func classify(err error) error {
	var re *registry.Error
	if err == nil || errors.As(err, &re) {
		return err
	}
	var p *pgconn.PgError
	if !errors.As(err, &p) {
		return err
	}
	switch p.Code {
	case "42501":
		return &registry.Error{Kind: registry.ErrForbidden, Msg: p.Message}
	case "23514", "22P02":
		return &registry.Error{Kind: registry.ErrInvalid, Msg: p.Message}
	case "23503":
		return &registry.Error{Kind: registry.ErrNotFound, Msg: p.Message}
	case "55000", "23505", "40001", "40P01":
		return &registry.Error{Kind: registry.ErrConflict, Msg: p.Message}
	default:
		return err
	}
}
