// Package budget manages hard budget accounts and their limits (ADR-012,
// MASTER_PLAN §45-§47, §57).
//
// PostgreSQL enforces every rule (migration 00011): accounts are created
// empty by an admin; lowering a limit applies at once while raising one is
// two-person; a child's limit is escrowed from its parent; and no account
// can be stored with allocated + reserved + committed above its limit.
// Reservations are not made here: the release transaction reserves
// (eacp.budget_reserve) and the action's state change settles.
package budget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"eacp/internal/registry"
	"eacp/internal/storage"
)

// Account is a budget account. Reserved counts ACTIVE reservations and
// Committed every committed reservation; Available is what a new
// reservation on this account may take (a leaf) or what may still be
// escrowed to children (an inner account).
type Account struct {
	ID        uuid.UUID   `json:"id"`
	Name      string      `json:"name"`
	Unit      string      `json:"unit"`
	ParentID  *uuid.UUID  `json:"parent_id,omitempty"`
	AgentID   *uuid.UUID  `json:"agent_id,omitempty"`
	HardLimit json.Number `json:"hard_limit"`
	Allocated json.Number `json:"allocated"`
	Reserved  json.Number `json:"reserved"`
	Committed json.Number `json:"committed"`
	Available json.Number `json:"available"`
	CreatedBy uuid.UUID   `json:"created_by"`
	CreatedAt time.Time   `json:"created_at"`
	// Proposal is the open limit increase, if any.
	Proposal *LimitChange `json:"open_proposal,omitempty"`
}

// NewAccount describes an account. AgentID makes it the agent's leaf in
// Unit; ParentID escrows its limit from the parent (same unit).
type NewAccount struct {
	Name     string     `json:"name"`
	Unit     string     `json:"unit"`
	ParentID *uuid.UUID `json:"parent_id,omitempty"`
	AgentID  *uuid.UUID `json:"agent_id,omitempty"`
}

// LimitChange is a request to change an account's hard limit.
type LimitChange struct {
	ID             uuid.UUID   `json:"id"`
	AccountID      uuid.UUID   `json:"account_id"`
	OldLimit       json.Number `json:"old_limit"`
	NewLimit       json.Number `json:"new_limit"`
	State          string      `json:"state"`
	Reason         string      `json:"reason"`
	DecisionReason string      `json:"decision_reason,omitempty"`
	ProposedBy     uuid.UUID   `json:"proposed_by"`
	ProposedAt     time.Time   `json:"proposed_at"`
	DecidedBy      *uuid.UUID  `json:"decided_by,omitempty"`
	DecidedAt      *time.Time  `json:"decided_at,omitempty"`
}

// Service is the budget API over a pool connected as the application role.
type Service struct{ pool *pgxpool.Pool }

// New returns a Service.
func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// amountPattern is a non-negative decimal below 10^15 with at most six
// decimals: the numeric(21,6) columns store it exactly.
var amountPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,14})(\.[0-9]{1,6})?$`)

// CreateAccount creates an empty account (admin). Its limit starts at zero.
func (s *Service) CreateAccount(ctx context.Context, a registry.Actor, n NewAccount) (Account, error) {
	var id uuid.UUID
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		var err error
		id, err = Tx{tx}.CreateAccount(ctx, n)
		return err
	})
	if err != nil {
		return Account{}, err
	}
	return s.Get(ctx, a, id)
}

// ChangeLimit sets an account's hard limit (admin). A decrease applies at
// once; an increase is PROPOSED until a different admin approves it.
func (s *Service) ChangeLimit(ctx context.Context, a registry.Actor, account uuid.UUID, limit, reason string) (LimitChange, error) {
	var c LimitChange
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		var err error
		c, err = Tx{tx}.ChangeLimit(ctx, account, limit, reason)
		return err
	})
	return c, err
}

// DecideLimitChange approves (apply) or rejects an open increase (admin).
// Approval needs an admin other than the proposer, and the limit must still
// be the one the proposal started from.
func (s *Service) DecideLimitChange(ctx context.Context, a registry.Actor, change uuid.UUID, apply bool, reason string) (LimitChange, error) {
	var c LimitChange
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		var err error
		c, err = Tx{tx}.DecideLimitChange(ctx, change, apply, reason)
		return err
	})
	return c, err
}

// List returns every account of the tenant, roots first.
func (s *Service) List(ctx context.Context, a registry.Actor) ([]Account, error) {
	out := []Account{}
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, accountQuery+` ORDER BY b.parent_id NULLS FIRST, b.name`)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Account, error) { return scanAccount(r) })
		return err
	})
	return out, err
}

// Get returns one account with its open proposal.
func (s *Service) Get(ctx context.Context, a registry.Actor, id uuid.UUID) (Account, error) {
	var acct Account
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		var err error
		if acct, err = scanAccount(tx.QueryRow(ctx, accountQuery+` WHERE b.id = $1`, id)); err != nil {
			return err
		}
		c, err := scanChange(tx.QueryRow(ctx, `SELECT `+changeColumns+` FROM eacp.budget_limit_changes
			WHERE account_id = $1 AND state = 'PROPOSED'`, id))
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return err
		default:
			acct.Proposal = &c
		}
		return nil
	})
	return acct, err
}

// accountQuery shows the counters as of now: the stored counters include
// settled reservations that the next reservation or limit change folds in.
const accountQuery = `
	WITH u AS (
		SELECT account_id,
		       COALESCE(sum(amount) FILTER (WHERE state = 'ACTIVE'), 0) AS active,
		       COALESCE(sum(amount) FILTER (WHERE state <> 'ACTIVE' AND folded_at IS NULL), 0) AS settled,
		       COALESCE(sum(committed_amount) FILTER (WHERE state <> 'ACTIVE' AND folded_at IS NULL), 0) AS newly
		FROM eacp.budget_reservations GROUP BY account_id)
	SELECT b.id, b.name, b.unit, b.parent_id, b.agent_id,
	       trim_scale(b.hard_limit)::text, trim_scale(b.allocated)::text,
	       trim_scale(b.reserved - COALESCE(u.settled, 0))::text,
	       trim_scale(b.committed + COALESCE(u.newly, 0))::text,
	       trim_scale(b.hard_limit - b.allocated - (b.reserved - COALESCE(u.settled, 0))
	                  - (b.committed + COALESCE(u.newly, 0)))::text,
	       b.created_by, b.created_at
	FROM eacp.budget_accounts b LEFT JOIN u ON u.account_id = b.id`

func scanAccount(r pgx.Row) (Account, error) {
	var x Account
	var limit, allocated, reserved, committed, available string
	var by *uuid.UUID
	err := r.Scan(&x.ID, &x.Name, &x.Unit, &x.ParentID, &x.AgentID, &limit, &allocated, &reserved, &committed,
		&available, &by, &x.CreatedAt)
	x.HardLimit, x.Allocated, x.Reserved = json.Number(limit), json.Number(allocated), json.Number(reserved)
	x.Committed, x.Available = json.Number(committed), json.Number(available)
	if by != nil {
		x.CreatedBy = *by
	}
	return x, err
}

const changeColumns = `id, account_id, trim_scale(old_limit)::text, trim_scale(new_limit)::text, state, reason,
	COALESCE(decision_reason, ''), proposed_by, proposed_at, decided_by, decided_at`

func scanChange(r pgx.Row) (LimitChange, error) {
	var c LimitChange
	var oldLimit, newLimit string
	var by *uuid.UUID
	err := r.Scan(&c.ID, &c.AccountID, &oldLimit, &newLimit, &c.State, &c.Reason, &c.DecisionReason, &by,
		&c.ProposedAt, &c.DecidedBy, &c.DecidedAt)
	c.OldLimit, c.NewLimit = json.Number(oldLimit), json.Number(newLimit)
	if by != nil {
		c.ProposedBy = *by
	}
	return c, err
}

// change runs fn as the principal in one transaction; the database
// authorises every write and journals it in the same transaction.
func (s *Service) change(ctx context.Context, a registry.Actor, fn func(pgx.Tx) error) error {
	if a.TenantID == uuid.Nil || a.PrincipalID == uuid.Nil {
		return &registry.Error{Kind: registry.ErrForbidden, Msg: "no actor"}
	}
	return mapErr(storage.InTenantTx(ctx, s.pool, a.TenantID.String(), func(tx pgx.Tx) error {
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
	return mapErr(storage.InTenantReadTx(ctx, s.pool, a.TenantID.String(), fn))
}

func mapErr(err error) error {
	if err == nil {
		return nil
	}
	var re *registry.Error
	if errors.As(err, &re) {
		return err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return &registry.Error{Kind: registry.ErrNotFound, Msg: "no such budget object"}
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	var kind error
	switch pgErr.Code {
	case "42501":
		kind = registry.ErrForbidden
	case "55000", "23505":
		kind = registry.ErrConflict
	case "23503":
		kind = registry.ErrNotFound
	case "23514", "23502", "22P02", "22023", "22001", "22003":
		kind = registry.ErrInvalid
	default:
		return fmt.Errorf("budget: %w", err)
	}
	return &registry.Error{Kind: kind, Msg: pgErr.Message}
}
