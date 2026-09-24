// Package finops is Agent FinOps (ADR-025, MASTER_PLAN §45-§48 and §92):
// LLM usage ingest, a rate card, chargeback, soft budgets, a spend
// dashboard and alerts.
//
// PostgreSQL enforces every rule (migration 00018): OTel usage is bound to
// the agent key that sent it and priced by a trigger; billing lines are an
// admin's import; prices only take effect forward; alerts are raised only
// by the finops evaluator and acknowledged once. Nothing here blocks an
// action: only ADR-012 hard limits do.
package finops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"eacp/internal/registry"
	"eacp/internal/storage"
)

// SystemActor is the component that raises alerts (storage.SetSystem).
const SystemActor = "finops"

// MaxBillingLines bounds one billing import.
const MaxBillingLines = 10000

// Service is the FinOps API over a pool connected as the application role.
type Service struct{ pool *pgxpool.Pool }

// New returns a Service.
func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// amountPattern is a non-negative decimal below 10^15 with at most six
// decimals: the numeric(21,6) columns store it exactly.
var amountPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,14})(\.[0-9]{1,6})?$`)

func invalid(format string, args ...any) error {
	return &registry.Error{Kind: registry.ErrInvalid, Msg: fmt.Sprintf(format, args...)}
}

// ------------------------------------------------------------------ prices

// Price is a rate-card entry: prices per million tokens.
type Price struct {
	ID            uuid.UUID    `json:"id"`
	Provider      string       `json:"provider"`
	Model         string       `json:"model"`
	Unit          string       `json:"unit"`
	InputPerMTok  json.Number  `json:"input_per_mtok"`
	CachedPerMTok *json.Number `json:"cached_input_per_mtok,omitempty"`
	OutputPerMTok json.Number  `json:"output_per_mtok"`
	EffectiveFrom time.Time    `json:"effective_from"`
	Reason        string       `json:"reason"`
	CreatedBy     *uuid.UUID   `json:"created_by,omitempty"`
	CreatedAt     time.Time    `json:"created_at"`
}

// NewPrice adds a price. Cached input defaults to the input price.
// EffectiveFrom defaults to now and may not be earlier.
type NewPrice struct {
	Provider      string     `json:"provider"`
	Model         string     `json:"model"`
	Unit          string     `json:"unit"`
	InputPerMTok  string     `json:"input_per_mtok"`
	CachedPerMTok *string    `json:"cached_input_per_mtok,omitempty"`
	OutputPerMTok string     `json:"output_per_mtok"`
	EffectiveFrom *time.Time `json:"effective_from,omitempty"`
	Reason        string     `json:"reason"`
}

const priceColumns = `id, provider, model, unit, trim_scale(input_per_mtok)::text,
	trim_scale(cached_input_per_mtok)::text, trim_scale(output_per_mtok)::text, effective_from, reason,
	created_by, created_at`

func scanPrice(r pgx.Row) (Price, error) {
	var p Price
	var in, out string
	var cached *string
	err := r.Scan(&p.ID, &p.Provider, &p.Model, &p.Unit, &in, &cached, &out, &p.EffectiveFrom, &p.Reason,
		&p.CreatedBy, &p.CreatedAt)
	p.InputPerMTok, p.OutputPerMTok = json.Number(in), json.Number(out)
	if cached != nil {
		n := json.Number(*cached)
		p.CachedPerMTok = &n
	}
	return p, err
}

// AddPrice adds a rate-card entry (admin, journaled).
func (s *Service) AddPrice(ctx context.Context, a registry.Actor, n NewPrice) (Price, error) {
	for _, v := range []*string{&n.InputPerMTok, n.CachedPerMTok, &n.OutputPerMTok} {
		if v != nil && !amountPattern.MatchString(*v) {
			return Price{}, invalid("prices must be non-negative decimals below 10^15 with at most 6 decimals")
		}
	}
	var p Price
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		var err error
		p, err = scanPrice(tx.QueryRow(ctx, `INSERT INTO eacp.model_prices (tenant_id, provider, model, unit,
			input_per_mtok, cached_input_per_mtok, output_per_mtok, effective_from, reason)
			VALUES (eacp.current_tenant_id(), $1, $2, $3, $4::numeric, $5::numeric, $6::numeric, COALESCE($7, now()), $8)
			RETURNING `+priceColumns,
			n.Provider, n.Model, n.Unit, n.InputPerMTok, n.CachedPerMTok, n.OutputPerMTok, n.EffectiveFrom, n.Reason))
		return err
	})
	return p, err
}

// Prices returns the rate card, newest first.
func (s *Service) Prices(ctx context.Context, a registry.Actor) ([]Price, error) {
	out := []Price{}
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+priceColumns+` FROM eacp.model_prices
			ORDER BY provider, model, effective_from DESC`)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Price, error) { return scanPrice(r) })
		return err
	})
	return out, err
}

// ------------------------------------------------------------------- usage

// Ingest is the outcome of recording an agent's spans.
type Ingest struct {
	Accepted   int // new records
	Duplicates int // spans already recorded (an exporter's retry)
	Rejected   int // spans PostgreSQL refused (for example, outside the window)
	Message    string
}

// RecordSpans records usage spans for the agent version whose key sent
// them. Each span is its own savepoint: a span PostgreSQL refuses is
// counted and the rest are kept. An authorization failure refuses all.
func (s *Service) RecordSpans(ctx context.Context, tenant, version uuid.UUID, spans []Span) (Ingest, error) {
	var in Ingest
	if tenant == uuid.Nil || version == uuid.Nil {
		return in, &registry.Error{Kind: registry.ErrForbidden, Msg: "no agent"}
	}
	err := storage.InTenantTx(ctx, s.pool, tenant.String(), func(tx pgx.Tx) error {
		in = Ingest{}
		if err := storage.SetAgent(ctx, tx, version); err != nil {
			return err
		}
		for _, sp := range spans {
			var model *string
			if sp.Model != "" {
				model = &sp.Model
			}
			sub, err := tx.Begin(ctx)
			if err != nil {
				return err
			}
			// agent_id is a placeholder: the guard binds the key's agent.
			tag, err := sub.Exec(ctx, `INSERT INTO eacp.usage_records (tenant_id, source, agent_id, trace_id, span_id,
				provider, operation, model, input_tokens, cache_read_tokens, output_tokens, observed_at)
				VALUES (eacp.current_tenant_id(), 'otel', $1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
				ON CONFLICT (tenant_id, trace_id, span_id) WHERE source = 'otel' DO NOTHING`,
				uuid.Nil, sp.TraceID, sp.SpanID, sp.Provider, sp.Operation, model,
				sp.InputTokens, sp.CacheReadTokens, sp.OutputTokens, sp.ObservedAt)
			var pgErr *pgconn.PgError
			switch {
			case err == nil:
				if err := sub.Commit(ctx); err != nil {
					return err
				}
				if tag.RowsAffected() == 1 {
					in.Accepted++
				} else {
					in.Duplicates++
				}
			case errors.As(err, &pgErr) && pgErr.Code[:2] == "23":
				if err := sub.Rollback(ctx); err != nil {
					return err
				}
				if in.Rejected == 0 {
					in.Message = fmt.Sprintf("span %s/%s: %s", sp.TraceID, sp.SpanID, pgErr.Message)
				}
				in.Rejected++
			default:
				return err
			}
		}
		return nil
	})
	return in, mapErr(err)
}

// BillingLine is one line of a provider billing export.
type BillingLine struct {
	ExternalID      string    `json:"external_id"`
	AgentID         uuid.UUID `json:"agent_id"`
	Provider        string    `json:"provider"`
	Operation       string    `json:"operation,omitempty"` // default chat
	Model           string    `json:"model,omitempty"`
	InputTokens     int64     `json:"input_tokens"`
	CacheReadTokens int64     `json:"cache_read_tokens"`
	OutputTokens    int64     `json:"output_tokens"`
	Cost            string    `json:"cost"`
	Unit            string    `json:"unit"`
	ObservedAt      time.Time `json:"observed_at"`
}

// Import is the outcome of a billing import.
type Import struct {
	Inserted   int `json:"inserted"`
	Duplicates int `json:"duplicates"` // external ids already imported; the first import stands
}

// ImportBilling records provider billing lines (admin), all or none.
func (s *Service) ImportBilling(ctx context.Context, a registry.Actor, lines []BillingLine) (Import, error) {
	var out Import
	if len(lines) == 0 || len(lines) > MaxBillingLines {
		return out, invalid("a billing import has 1 to %d lines", MaxBillingLines)
	}
	for i, l := range lines {
		if !amountPattern.MatchString(l.Cost) {
			return out, invalid("line %d: cost must be a non-negative decimal with at most 6 decimals", i)
		}
		if l.ObservedAt.IsZero() {
			return out, invalid("line %d: observed_at is required", i)
		}
	}
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		out = Import{}
		for _, l := range lines {
			op := l.Operation
			if op == "" {
				op = "chat"
			}
			var model *string
			if l.Model != "" {
				model = &l.Model
			}
			tag, err := tx.Exec(ctx, `INSERT INTO eacp.usage_records (tenant_id, source, agent_id, external_id,
				provider, operation, model, input_tokens, cache_read_tokens, output_tokens, observed_at,
				cost_amount, cost_unit)
				VALUES (eacp.current_tenant_id(), 'provider_billing', $1, $2, $3, $4, $5, $6, $7, $8, $9, $10::numeric, $11)
				ON CONFLICT (tenant_id, external_id) WHERE source = 'provider_billing' DO NOTHING`,
				l.AgentID, l.ExternalID, l.Provider, op, model, l.InputTokens, l.CacheReadTokens, l.OutputTokens,
				l.ObservedAt, l.Cost, l.Unit)
			if err != nil {
				return fmt.Errorf("line %s: %w", l.ExternalID, err)
			}
			if tag.RowsAffected() == 1 {
				out.Inserted++
			} else {
				out.Duplicates++
			}
		}
		return nil
	})
	return out, err
}

// Usage is one usage record.
type Usage struct {
	ID              uuid.UUID    `json:"id"`
	Source          string       `json:"source"`
	AgentID         uuid.UUID    `json:"agent_id"`
	AgentVersionID  *uuid.UUID   `json:"agent_version_id,omitempty"`
	TraceID         *string      `json:"trace_id,omitempty"`
	SpanID          *string      `json:"span_id,omitempty"`
	ExternalID      *string      `json:"external_id,omitempty"`
	Provider        string       `json:"provider"`
	Operation       string       `json:"operation"`
	Model           *string      `json:"model,omitempty"`
	InputTokens     int64        `json:"input_tokens"`
	CacheReadTokens int64        `json:"cache_read_tokens"`
	OutputTokens    int64        `json:"output_tokens"`
	Billable        bool         `json:"billable"`
	ObservedAt      time.Time    `json:"observed_at"`
	ReceivedAt      time.Time    `json:"received_at"`
	PriceID         *uuid.UUID   `json:"price_id,omitempty"`
	Cost            *json.Number `json:"cost,omitempty"` // absent: unpriced
	Unit            *string      `json:"unit,omitempty"`
}

// UsageFilter selects usage records in [From, To), newest first.
type UsageFilter struct {
	AgentID  *uuid.UUID
	From, To time.Time
	Limit    int // default 100, at most 1000
}

// Usage lists usage records.
func (s *Service) Usage(ctx context.Context, a registry.Actor, f UsageFilter) ([]Usage, error) {
	if f.Limit <= 0 {
		f.Limit = 100
	}
	if f.Limit > 1000 {
		return nil, invalid("limit is at most 1000")
	}
	out := []Usage{}
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, source, agent_id, agent_version_id, trace_id, span_id, external_id,
			provider, operation, model, input_tokens, cache_read_tokens, output_tokens, billable, observed_at,
			received_at, price_id, trim_scale(cost_amount)::text, cost_unit
			FROM eacp.usage_records
			WHERE ($1::uuid IS NULL OR agent_id = $1) AND observed_at >= $2 AND observed_at < $3
			ORDER BY observed_at DESC, id LIMIT $4`, f.AgentID, f.From, f.To, f.Limit)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Usage, error) {
			var u Usage
			var cost *string
			err := r.Scan(&u.ID, &u.Source, &u.AgentID, &u.AgentVersionID, &u.TraceID, &u.SpanID, &u.ExternalID,
				&u.Provider, &u.Operation, &u.Model, &u.InputTokens, &u.CacheReadTokens, &u.OutputTokens,
				&u.Billable, &u.ObservedAt, &u.ReceivedAt, &u.PriceID, &cost, &u.Unit)
			if cost != nil {
				n := json.Number(*cost)
				u.Cost = &n
			}
			return u, err
		})
		return err
	})
	return out, err
}

// ------------------------------------------------------------- soft limits

// SoftLimit is an account's monthly (UTC) soft limit with its
// month-to-date spend over the account's subtree.
type SoftLimit struct {
	AccountID    uuid.UUID    `json:"account_id"`
	AccountName  string       `json:"account_name"`
	Unit         string       `json:"unit"`
	MonthlyLimit *json.Number `json:"monthly_limit,omitempty"` // absent: cleared
	MonthToDate  json.Number  `json:"month_to_date"`
	Reason       string       `json:"reason"`
	SetBy        *uuid.UUID   `json:"set_by,omitempty"`
	SetAt        time.Time    `json:"set_at"`
}

// SetSoftLimit sets or clears (limit nil) an account's soft limit (admin,
// journaled). It never blocks an action.
func (s *Service) SetSoftLimit(ctx context.Context, a registry.Actor, account uuid.UUID, limit *string, reason string) (SoftLimit, error) {
	if limit != nil && !amountPattern.MatchString(*limit) {
		return SoftLimit{}, invalid("monthly_limit must be a positive decimal with at most 6 decimals")
	}
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO eacp.budget_soft_limits (tenant_id, account_id, monthly_limit, reason)
			VALUES (eacp.current_tenant_id(), $1, $2::numeric, $3)
			ON CONFLICT (tenant_id, account_id) DO UPDATE SET monthly_limit = EXCLUDED.monthly_limit,
			reason = EXCLUDED.reason`, account, limit, reason)
		return err
	})
	if err != nil {
		return SoftLimit{}, err
	}
	all, err := s.SoftLimits(ctx, a)
	for _, l := range all {
		if l.AccountID == account {
			return l, nil
		}
	}
	if err == nil {
		err = &registry.Error{Kind: registry.ErrNotFound, Msg: "no such soft limit"}
	}
	return SoftLimit{}, err
}

// SoftLimits returns every soft limit with its month-to-date spend.
func (s *Service) SoftLimits(ctx context.Context, a registry.Actor) ([]SoftLimit, error) {
	out := []SoftLimit{}
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT l.account_id, b.name, b.unit, trim_scale(l.monthly_limit)::text,
			trim_scale(eacp.finops_account_spend(l.account_id, date_trunc('month', now(), 'UTC'), now()))::text,
			l.reason, l.set_by, l.set_at
			FROM eacp.budget_soft_limits l JOIN eacp.budget_accounts b ON b.id = l.account_id
			ORDER BY b.name`)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (SoftLimit, error) {
			var l SoftLimit
			var limit *string
			var mtd string
			err := r.Scan(&l.AccountID, &l.AccountName, &l.Unit, &limit, &mtd, &l.Reason, &l.SetBy, &l.SetAt)
			l.MonthToDate = json.Number(mtd)
			if limit != nil {
				n := json.Number(*limit)
				l.MonthlyLimit = &n
			}
			return l, err
		})
		return err
	})
	return out, err
}

// ------------------------------------------------------------------ alerts

// Alert is a FinOps alert.
type Alert struct {
	ID             uuid.UUID       `json:"id"`
	Kind           string          `json:"kind"`
	SubjectType    string          `json:"subject_type"`
	SubjectID      uuid.UUID       `json:"subject_id"`
	Unit           string          `json:"unit,omitempty"`
	PeriodStart    time.Time       `json:"period_start"`
	Observed       *json.Number    `json:"observed,omitempty"`
	Threshold      *json.Number    `json:"threshold,omitempty"`
	Detail         json.RawMessage `json:"detail"`
	CreatedAt      time.Time       `json:"created_at"`
	AcknowledgedBy *uuid.UUID      `json:"acknowledged_by,omitempty"`
	AcknowledgedAt *time.Time      `json:"acknowledged_at,omitempty"`
	AckReason      *string         `json:"ack_reason,omitempty"`
}

const alertColumns = `id, kind, subject_type, subject_id, unit, period_start, trim_scale(observed)::text,
	trim_scale(threshold)::text, detail, created_at, acknowledged_by, acknowledged_at, ack_reason`

func scanAlert(r pgx.Row) (Alert, error) {
	var x Alert
	var observed, threshold *string
	err := r.Scan(&x.ID, &x.Kind, &x.SubjectType, &x.SubjectID, &x.Unit, &x.PeriodStart, &observed, &threshold,
		&x.Detail, &x.CreatedAt, &x.AcknowledgedBy, &x.AcknowledgedAt, &x.AckReason)
	if observed != nil {
		n := json.Number(*observed)
		x.Observed = &n
	}
	if threshold != nil {
		n := json.Number(*threshold)
		x.Threshold = &n
	}
	return x, err
}

// Alerts lists alerts, newest first; open limits them to unacknowledged.
func (s *Service) Alerts(ctx context.Context, a registry.Actor, open bool, limit int) ([]Alert, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		return nil, invalid("limit is at most 1000")
	}
	out := []Alert{}
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+alertColumns+` FROM eacp.finops_alerts
			WHERE NOT $1 OR acknowledged_at IS NULL ORDER BY created_at DESC, id LIMIT $2`, open, limit)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Alert, error) { return scanAlert(r) })
		return err
	})
	return out, err
}

// Acknowledge acknowledges an alert once (operator or admin, journaled).
func (s *Service) Acknowledge(ctx context.Context, a registry.Actor, id uuid.UUID, reason string) (Alert, error) {
	var x Alert
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		var err error
		x, err = scanAlert(tx.QueryRow(ctx, `UPDATE eacp.finops_alerts SET ack_reason = $2, acknowledged_at = now()
			WHERE id = $1 RETURNING `+alertColumns, id, reason))
		return err
	})
	return x, err
}

// --------------------------------------------------------------- evaluator

// Evaluate runs the alert evaluator for one tenant and returns the number
// of alerts raised.
func (s *Service) Evaluate(ctx context.Context, tenant uuid.UUID) (int, error) {
	var n int
	err := storage.InTenantTx(ctx, s.pool, tenant.String(), func(tx pgx.Tx) error {
		if err := storage.SetSystem(ctx, tx, SystemActor); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT eacp.finops_evaluate()`).Scan(&n)
	})
	return n, err
}

// EvaluateAll evaluates every tenant the reviewed hint eacp.finops_tenants()
// lists. A tenant's failure does not stop the others; the first is returned.
func (s *Service) EvaluateAll(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT tenant_id FROM eacp.finops_tenants()`)
	if err != nil {
		return 0, err
	}
	tenants, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return 0, err
	}
	total := 0
	var first error
	for _, t := range tenants {
		n, err := s.Evaluate(ctx, t)
		total += n
		if err != nil && first == nil {
			first = fmt.Errorf("finops: evaluate tenant %s: %w", t, err)
		}
	}
	return total, first
}

// Run evaluates every interval until ctx ends.
func (s *Service) Run(ctx context.Context, interval time.Duration, log *slog.Logger) {
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		n, err := s.EvaluateAll(ctx)
		if err != nil && ctx.Err() == nil {
			log.ErrorContext(ctx, "finops evaluation failed", "err", err)
		} else if n > 0 {
			log.InfoContext(ctx, "finops alerts raised", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ---------------------------------------------------------------- plumbing

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
		return &registry.Error{Kind: registry.ErrNotFound, Msg: "no such finops object"}
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
	case "23514", "23502", "22P02", "22023", "22001", "22003", "22007", "22008":
		kind = registry.ErrInvalid
	default:
		return fmt.Errorf("finops: %w", err)
	}
	return &registry.Error{Kind: kind, Msg: pgErr.Message}
}
