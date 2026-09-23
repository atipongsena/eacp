// Package registry manages principals, roles, groups, credentials, agents,
// versions, allowlists, connectors, tools and contracts (ADR-003), and
// answers the capability check used by later phases.
//
// PostgreSQL enforces every registry rule and writes the audit journal
// (migration 00003); this package runs each change as one transaction with
// the tenant and actor set, and maps database rejections to the error kinds
// below.
package registry

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"eacp/internal/storage"
)

// Error kinds. Every error returned by Service matches exactly one of them
// with errors.Is, or is an unexpected infrastructure error.
var (
	ErrForbidden = errors.New("forbidden")
	ErrConflict  = errors.New("conflict")
	ErrNotFound  = errors.New("not found")
	ErrInvalid   = errors.New("invalid")
)

// Error carries the kind and a message safe to show to the caller.
type Error struct {
	Kind error
	Msg  string
}

func (e *Error) Error() string { return fmt.Sprintf("registry: %v: %s", e.Kind, e.Msg) }
func (e *Error) Unwrap() error { return e.Kind }

func newErr(kind error, format string, args ...any) error {
	return &Error{Kind: kind, Msg: fmt.Sprintf(format, args...)}
}

// mapErr translates the SQLSTATEs raised by the registry schema.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	var re *Error
	if errors.As(err, &re) {
		return err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return newErr(ErrNotFound, "no such object")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	msg := pgErr.Message
	if pgErr.ConstraintName != "" && pgErr.Code != "23514" {
		msg += " (" + pgErr.ConstraintName + ")"
	}
	switch pgErr.Code {
	case "42501":
		return &Error{ErrForbidden, msg}
	case "55000", "23505":
		return &Error{ErrConflict, msg}
	case "23503":
		return &Error{ErrNotFound, msg}
	case "23514", "23502", "22P02", "22023", "22001", "22007", "22008":
		return &Error{ErrInvalid, msg}
	}
	return err
}

// Actor is the authenticated principal performing a call.
type Actor struct {
	TenantID    uuid.UUID
	PrincipalID uuid.UUID
}

// Service is the registry API over a pool connected as the application role.
type Service struct {
	pool *pgxpool.Pool
}

// New returns a Service.
func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// change runs fn as actor in one transaction. The database authorises every
// write from the actor and appends the audit event for each changed row in
// the same transaction (migration 00003, eacp.audit_row_change), so a change
// and its audit record commit or fail together.
func (s *Service) change(ctx context.Context, a Actor, fn func(pgx.Tx) error) error {
	if a.TenantID == uuid.Nil || a.PrincipalID == uuid.Nil {
		return newErr(ErrForbidden, "no actor")
	}
	err := storage.InTenantTx(ctx, s.pool, a.TenantID.String(), func(tx pgx.Tx) error {
		if err := storage.SetActor(ctx, tx, a.PrincipalID); err != nil {
			return err
		}
		return fn(tx)
	})
	return mapErr(err)
}

// read runs fn in the actor's tenant.
func (s *Service) read(ctx context.Context, a Actor, fn func(pgx.Tx) error) error {
	if a.TenantID == uuid.Nil {
		return newErr(ErrForbidden, "no tenant")
	}
	return mapErr(storage.InTenantTx(ctx, s.pool, a.TenantID.String(), fn))
}

// execOne runs an UPDATE that must hit exactly one row.
func execOne(ctx context.Context, tx pgx.Tx, sql string, args ...any) error {
	tag, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return newErr(ErrNotFound, "no such object")
	}
	return nil
}

func nullID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(i int) any {
	if i == 0 {
		return nil
	}
	return i
}
