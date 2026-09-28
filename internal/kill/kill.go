// Package kill provides the operator API for PostgreSQL execution kills.
// The database validates scope, role, separation of duties, epoch and audit.
package kill

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

type Change struct {
	Scope      string    `json:"scope"`
	TargetID   uuid.UUID `json:"target_id"`
	Killed     bool      `json:"killed"`
	ReasonCode string    `json:"reason_code"`
	Reason     string    `json:"reason"`
}

type State struct {
	ID uuid.UUID `json:"id"`
	Change
	Epoch     int64     `json:"epoch"`
	ChangedBy uuid.UUID `json:"changed_by"`
	ChangedAt time.Time `json:"changed_at"`
}

type Service struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

const stateColumns = `id, scope, target_id, killed, reason_code, reason, epoch, changed_by, changed_at`

func scan(row pgx.Row) (State, error) {
	var s State
	err := row.Scan(&s.ID, &s.Scope, &s.TargetID, &s.Killed, &s.ReasonCode, &s.Reason, &s.Epoch, &s.ChangedBy, &s.ChangedAt)
	return s, err
}

func (s *Service) Set(ctx context.Context, a registry.Actor, c Change) (State, error) {
	var out State
	if c.ReasonCode == "" {
		c.ReasonCode = "operator_request"
	}
	err := storage.InTenantTx(ctx, s.pool, a.TenantID.String(), func(tx pgx.Tx) error {
		if err := storage.SetActor(ctx, tx, a.PrincipalID); err != nil {
			return err
		}
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT eacp.set_kill($1, $2, $3, $4, $5)`,
			c.Scope, c.TargetID, c.Killed, c.Reason, c.ReasonCode).Scan(&raw); err != nil {
			return err
		}
		return json.Unmarshal(raw, &out)
	})
	return out, classify(err)
}

func (s *Service) List(ctx context.Context, a registry.Actor) ([]State, error) {
	out := []State{}
	err := storage.InTenantReadTx(ctx, s.pool, a.TenantID.String(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+stateColumns+` FROM eacp.kill_states ORDER BY scope, target_id`)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (State, error) { return scan(r) })
		return err
	})
	return out, err
}

func classify(err error) error {
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
	case "55000":
		return &registry.Error{Kind: registry.ErrConflict, Msg: p.Message}
	default:
		return err
	}
}
