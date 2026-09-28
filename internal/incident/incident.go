// Package incident is the Agent SOC's incident domain and read model
// (ADR-027). An incident observes and never decides: PostgreSQL opens
// automatic incidents from signals it already holds, and operators
// acknowledge, assign, annotate, link and resolve them under the database's
// rules. Every event is journaled.
package incident

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atipongsena/eacp/internal/registry"
	"github.com/atipongsena/eacp/internal/storage"
)

// Incident is an incident and, from Get, its timeline.
type Incident struct {
	ID               uuid.UUID       `json:"id"`
	Kind             string          `json:"kind"`
	SourceKey        *string         `json:"source_key,omitempty"`
	Severity         string          `json:"severity"`
	State            string          `json:"state"`
	Title            string          `json:"title"`
	SubjectType      *string         `json:"subject_type,omitempty"`
	SubjectID        *uuid.UUID      `json:"subject_id,omitempty"`
	Detail           json.RawMessage `json:"detail"`
	Affected         json.RawMessage `json:"affected"`
	OpenedBy         *uuid.UUID      `json:"opened_by,omitempty"`
	OpenedAt         time.Time       `json:"opened_at"`
	AcknowledgedBy   *uuid.UUID      `json:"acknowledged_by,omitempty"`
	AcknowledgedAt   *time.Time      `json:"acknowledged_at,omitempty"`
	AckReason        *string         `json:"ack_reason,omitempty"`
	AssigneeID       *uuid.UUID      `json:"assignee_id,omitempty"`
	ResolvedBy       *uuid.UUID      `json:"resolved_by,omitempty"`
	ResolvedAt       *time.Time      `json:"resolved_at,omitempty"`
	Resolution       *string         `json:"resolution,omitempty"`
	ResolutionReason *string         `json:"resolution_reason,omitempty"`
	Events           []Event         `json:"events,omitempty"`
}

// Event is one entry of an incident's timeline.
type Event struct {
	Seq      int        `json:"seq"`
	Kind     string     `json:"kind"`
	ActorID  *uuid.UUID `json:"actor_id,omitempty"`
	At       time.Time  `json:"at"`
	Note     *string    `json:"note,omitempty"`
	LinkKind *string    `json:"link_kind,omitempty"`
	LinkID   *uuid.UUID `json:"link_id,omitempty"`
}

// NewIncident is a manual incident.
type NewIncident struct {
	Title       string     `json:"title"`
	Severity    string     `json:"severity"`
	SubjectType string     `json:"subject_type,omitempty"`
	SubjectID   *uuid.UUID `json:"subject_id,omitempty"`
	Reason      string     `json:"reason"`
}

// Filter narrows List; empty fields match everything.
type Filter struct {
	State, Severity, Kind string
	Limit                 int
}

// Service works incidents over a pool connected as the application role.
type Service struct{ pool *pgxpool.Pool }

// New returns a Service.
func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

const columns = `id, kind, source_key, severity, state, title, subject_type, subject_id, detail::text, affected::text,
	opened_by, opened_at, acknowledged_by, acknowledged_at, ack_reason, assignee_id, resolved_by, resolved_at,
	resolution, resolution_reason`

func scan(r pgx.Row) (Incident, error) {
	var i Incident
	var detail, affected string
	err := r.Scan(&i.ID, &i.Kind, &i.SourceKey, &i.Severity, &i.State, &i.Title, &i.SubjectType, &i.SubjectID,
		&detail, &affected, &i.OpenedBy, &i.OpenedAt, &i.AcknowledgedBy, &i.AcknowledgedAt, &i.AckReason,
		&i.AssigneeID, &i.ResolvedBy, &i.ResolvedAt, &i.Resolution, &i.ResolutionReason)
	i.Detail, i.Affected = json.RawMessage(detail), json.RawMessage(affected)
	return i, err
}

// Open opens a manual incident (operator or admin, with a reason).
func (s *Service) Open(ctx context.Context, a registry.Actor, n NewIncident) (Incident, error) {
	var id uuid.UUID
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO eacp.incidents (tenant_id, kind, severity, title, subject_type, subject_id, detail)
			VALUES (eacp.current_tenant_id(), 'manual', $1, $2, NULLIF($3, ''), $4, jsonb_build_object('reason', $5::text))
			RETURNING id`, n.Severity, n.Title, n.SubjectType, n.SubjectID, n.Reason).Scan(&id)
	})
	if err != nil {
		return Incident{}, err
	}
	return s.Get(ctx, a, id)
}

// Acknowledge takes an OPEN incident, with a reason.
func (s *Service) Acknowledge(ctx context.Context, a registry.Actor, id uuid.UUID, reason string) (Incident, error) {
	return s.update(ctx, a, id, `UPDATE eacp.incidents SET state = 'ACKNOWLEDGED', ack_reason = $2 WHERE id = $1`, reason)
}

// Assign sets (or, with nil, clears) the assignee of an unresolved incident.
func (s *Service) Assign(ctx context.Context, a registry.Actor, id uuid.UUID, assignee *uuid.UUID) (Incident, error) {
	return s.update(ctx, a, id, `UPDATE eacp.incidents SET assignee_id = $2 WHERE id = $1`, assignee)
}

// Resolve closes an ACKNOWLEDGED incident with a code and a reason; a
// critical one by someone other than its acknowledger.
func (s *Service) Resolve(ctx context.Context, a registry.Actor, id uuid.UUID, resolution, reason string) (Incident, error) {
	return s.update(ctx, a, id, `UPDATE eacp.incidents SET state = 'RESOLVED', resolution = $2, resolution_reason = $3
		WHERE id = $1`, resolution, reason)
}

// Note adds a note to the timeline.
func (s *Service) Note(ctx context.Context, a registry.Actor, id uuid.UUID, text string) (Incident, error) {
	return s.event(ctx, a, id, `INSERT INTO eacp.incident_events (tenant_id, incident_id, kind, note)
		VALUES (eacp.current_tenant_id(), $1, 'note', $2)`, text)
}

// Link records the evidence of a response (a kill, an action, a release…).
func (s *Service) Link(ctx context.Context, a registry.Actor, id uuid.UUID, kind string, target uuid.UUID) (Incident, error) {
	return s.event(ctx, a, id, `INSERT INTO eacp.incident_events (tenant_id, incident_id, kind, link_kind, link_id)
		VALUES (eacp.current_tenant_id(), $1, 'linked', $2, $3)`, kind, target)
}

// Get returns an incident and its timeline.
func (s *Service) Get(ctx context.Context, a registry.Actor, id uuid.UUID) (Incident, error) {
	var out Incident
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		var err error
		if out, err = scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM eacp.incidents WHERE id = $1`, id)); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT seq, kind, actor_id, at, note, link_kind, link_id FROM eacp.incident_events
			WHERE incident_id = $1 ORDER BY seq`, id)
		if err != nil {
			return err
		}
		out.Events, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Event, error) {
			var e Event
			err := r.Scan(&e.Seq, &e.Kind, &e.ActorID, &e.At, &e.Note, &e.LinkKind, &e.LinkID)
			return e, err
		})
		return err
	})
	return out, err
}

// List returns incidents, most severe and newest first (without timelines).
func (s *Service) List(ctx context.Context, a registry.Actor, f Filter) ([]Incident, error) {
	if f.Limit == 0 {
		f.Limit = 100
	}
	if f.Limit < 1 || f.Limit > 500 {
		return nil, &registry.Error{Kind: registry.ErrInvalid, Msg: "limit must be between 1 and 500"}
	}
	out := []Incident{}
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+columns+` FROM eacp.incidents
			WHERE ($1 = '' OR state = $1) AND ($2 = '' OR severity = $2) AND ($3 = '' OR kind = $3)
			ORDER BY CASE severity WHEN 'critical' THEN 0 WHEN 'high' THEN 1 WHEN 'medium' THEN 2 ELSE 3 END,
			         opened_at DESC, id
			LIMIT $4`, f.State, f.Severity, f.Kind, f.Limit)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Incident, error) { return scan(r) })
		return err
	})
	return out, err
}

func (s *Service) update(ctx context.Context, a registry.Actor, id uuid.UUID, sql string, args ...any) (Incident, error) {
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, sql, append([]any{id}, args...)...)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return &registry.Error{Kind: registry.ErrNotFound, Msg: "no such incident"}
		}
		return nil
	})
	if err != nil {
		return Incident{}, err
	}
	return s.Get(ctx, a, id)
}

func (s *Service) event(ctx context.Context, a registry.Actor, id uuid.UUID, sql string, args ...any) (Incident, error) {
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, append([]any{id}, args...)...)
		return err
	})
	if err != nil {
		return Incident{}, err
	}
	return s.Get(ctx, a, id)
}

// change runs fn as the principal in one transaction; the database
// authorises every write and journals it in the same transaction.
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

func classify(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return &registry.Error{Kind: registry.ErrNotFound, Msg: "no such incident"}
	}
	var p *pgconn.PgError
	if !errors.As(err, &p) {
		return err
	}
	switch p.Code {
	case "42501":
		return &registry.Error{Kind: registry.ErrForbidden, Msg: p.Message}
	case "23514", "22P02", "23502":
		return &registry.Error{Kind: registry.ErrInvalid, Msg: p.Message}
	case "23503":
		return &registry.Error{Kind: registry.ErrNotFound, Msg: p.Message}
	case "55000", "23505":
		return &registry.Error{Kind: registry.ErrConflict, Msg: p.Message}
	default:
		return err
	}
}
