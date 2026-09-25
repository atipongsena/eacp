package bundle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"eacp/internal/registry"
	"eacp/internal/storage"
)

// Error codes the API reports for change-set failures.
const (
	CodePlanBlocked   = "plan_blocked"
	CodeStale         = "change_set_stale"
	CodeOpen          = "change_set_open"
	CodeSamePrincipal = "same_principal"
	CodeStepFailed    = "step_failed"
)

// Error is a change-set failure with a stable code. Err, when set, is the
// registry error a failing step returned.
type Error struct {
	Code     string    `json:"error"`
	Msg      string    `json:"detail"`
	Address  string    `json:"address,omitempty"`
	Findings []Finding `json:"findings,omitempty"`
	Err      error     `json:"-"`
}

func (e *Error) Error() string { return "bundle: " + e.Code + ": " + e.Msg }
func (e *Error) Unwrap() error { return e.Err }

func invalid(format string, args ...any) error {
	return &registry.Error{Kind: registry.ErrInvalid, Msg: fmt.Sprintf(format, args...)}
}

func conflict(format string, args ...any) error {
	return &registry.Error{Kind: registry.ErrConflict, Msg: fmt.Sprintf(format, args...)}
}

// classify maps database errors: a serialization failure or a stale digest
// (40001) means the registry moved on, and the bundle must be planned again.
func classify(err error) error {
	var be *Error
	var re *registry.Error
	if err == nil || errors.As(err, &be) || errors.As(err, &re) {
		return err
	}
	var p *pgconn.PgError
	if errors.As(err, &p) && (p.Code == "40001" || p.Code == "40P01") {
		return &Error{Code: CodeStale, Msg: p.Message + ": plan the bundle again"}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return &registry.Error{Kind: registry.ErrNotFound, Msg: "no such change set"}
	}
	return registry.MapErr(err)
}

// Service plans and applies change sets over a pool connected as the
// application role.
type Service struct{ pool *pgxpool.Pool }

// New returns a Service.
func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// Request asks for a plan of a bundle's desired state.
type Request struct {
	Bundle  string          `json:"bundle"`
	Desired json.RawMessage `json:"desired"`
	Prune   bool            `json:"prune,omitempty"`
	DryRun  bool            `json:"dry_run,omitempty"`
}

// ChangeSet is a recorded change set or, without an ID, a plan that was not
// recorded (a dry run, or nothing to change).
type ChangeSet struct {
	ID            uuid.UUID  `json:"id,omitzero"`
	Bundle        string     `json:"bundle"`
	State         string     `json:"state,omitempty"`
	Prune         bool       `json:"prune"`
	DryRun        bool       `json:"dry_run,omitempty"`
	DesiredDigest string     `json:"desired_digest,omitempty"`
	BaseDigest    string     `json:"base_digest,omitempty"`
	SealedDigest  string     `json:"sealed_digest,omitempty"`
	PlannedBy     uuid.UUID  `json:"planned_by,omitzero"`
	PlannedAt     *time.Time `json:"planned_at,omitempty"`
	SubmittedBy   *uuid.UUID `json:"submitted_by,omitempty"`
	SubmittedAt   *time.Time `json:"submitted_at,omitempty"`
	ApprovedBy    *uuid.UUID `json:"approved_by,omitempty"`
	ApprovedAt    *time.Time `json:"approved_at,omitempty"`
	ClosedBy      *uuid.UUID `json:"closed_by,omitempty"`
	ClosedAt      *time.Time `json:"closed_at,omitempty"`
	CloseReason   string     `json:"close_reason,omitempty"`
	Steps         []Step     `json:"steps"`
	Findings      []Finding  `json:"findings,omitempty"`
}

// BundleInfo is a bundle, how many objects it manages and its latest
// applied change set.
type BundleInfo struct {
	Name        string     `json:"name"`
	Managed     int        `json:"managed"`
	LastApplied *uuid.UUID `json:"last_applied,omitempty"`
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

// Plan diffs the desired document against the registry in one snapshot. It
// records a change set unless the request is a dry run, the plan has a
// blocking finding, or there is nothing to change. It never writes a
// registry object.
func (s *Service) Plan(ctx context.Context, a registry.Actor, req Request) (ChangeSet, error) {
	if a.TenantID == uuid.Nil || a.PrincipalID == uuid.Nil {
		return ChangeSet{}, &registry.Error{Kind: registry.ErrForbidden, Msg: "no actor"}
	}
	if !slugRE.MatchString(req.Bundle) {
		return ChangeSet{}, invalid("bundle names match %s", slugRE)
	}
	doc, err := Decode(req.Desired)
	if err != nil {
		return ChangeSet{}, invalid("%v", err)
	}
	canonical, err := json.Marshal(doc)
	if err != nil {
		return ChangeSet{}, err
	}
	out := ChangeSet{Bundle: req.Bundle, Prune: req.Prune, DryRun: req.DryRun, Steps: []Step{},
		Findings: Validate(doc, req.Desired)}
	if blocked(out.Findings) {
		return out, &Error{Code: CodePlanBlocked, Msg: "the bundle is invalid", Findings: out.Findings}
	}
	id := uuid.New()
	recorded := false
	err = storage.InTenantSnapshotTx(ctx, s.pool, a.TenantID.String(), func(tx pgx.Tx) error {
		if err := storage.SetActor(ctx, tx, a.PrincipalID); err != nil {
			return err
		}
		st, err := loadState(ctx, tx, req.Bundle)
		if err != nil {
			return err
		}
		d := diff(req.Bundle, id, doc, st, req.Prune)
		out.Steps, out.Findings = d.Steps, append(out.Findings, d.Findings...)
		if blocked(out.Findings) {
			return &Error{Code: CodePlanBlocked, Msg: "the plan has blocking findings", Findings: out.Findings}
		}
		if req.DryRun || len(d.Steps) == 0 {
			return nil
		}
		recorded = true
		return record(ctx, tx, id, req, canonical, d)
	})
	if err != nil {
		return out, classify(err)
	}
	if !recorded {
		return out, nil
	}
	cs, err := s.Get(ctx, a, id)
	cs.Findings = out.Findings
	return cs, err
}

// record writes the change set, its steps and refs, and seals it. An open
// PLANNED change set of the bundle is superseded; a SUBMITTED one blocks.
func record(ctx context.Context, tx pgx.Tx, id uuid.UUID, req Request, desired []byte, d Diff) error {
	var open uuid.UUID
	var state string
	err := tx.QueryRow(ctx, `SELECT cs.id, cs.state FROM eacp.change_sets cs
		JOIN eacp.bundles b ON b.tenant_id = cs.tenant_id AND b.id = cs.bundle_id
		WHERE b.name = $1 AND cs.state IN ('PLANNED', 'SUBMITTED') FOR UPDATE OF cs`, req.Bundle).Scan(&open, &state)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return err
	case state == "SUBMITTED":
		return &Error{Code: CodeOpen, Msg: fmt.Sprintf(
			"change set %s of bundle %s awaits approval: approve or reject it first", open, req.Bundle)}
	default:
		if _, err := tx.Exec(ctx, `UPDATE eacp.change_sets SET state = 'SUPERSEDED', close_reason = $2 WHERE id = $1`,
			open, "superseded by change set "+id.String()); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO eacp.bundles (tenant_id, name) VALUES (eacp.current_tenant_id(), $1)
		ON CONFLICT (tenant_id, name) DO NOTHING`, req.Bundle); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO eacp.change_sets (tenant_id, id, bundle_id, state, desired, prune)
		SELECT eacp.current_tenant_id(), $1, id, 'PLANNED', $3::jsonb, $4 FROM eacp.bundles WHERE name = $2`,
		id, req.Bundle, string(desired), req.Prune); err != nil {
		return err
	}
	for _, st := range d.Steps {
		payload, err := json.Marshal(st.Payload)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO eacp.change_set_steps
			(tenant_id, change_set_id, ordinal, address, op, stage, payload)
			VALUES (eacp.current_tenant_id(), $1, $2, $3, $4, $5, $6::jsonb)`,
			id, st.Ordinal, st.Address, st.Op, st.Stage, string(payload)); err != nil {
			return err
		}
	}
	for _, r := range d.Refs {
		if _, err := tx.Exec(ctx, `INSERT INTO eacp.change_set_refs (tenant_id, change_set_id, kind, object_id)
			VALUES (eacp.current_tenant_id(), $1, $2, $3)`, id, r.Kind, r.ID); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `SELECT eacp.change_set_seal($1)`, id)
	return err
}

const changeSetColumns = `cs.id, b.name, cs.state, cs.prune, encode(cs.desired_digest, 'hex'),
	COALESCE(encode(cs.base_digest, 'hex'), ''), COALESCE(encode(cs.sealed_digest, 'hex'), ''),
	cs.planned_by, cs.planned_at, cs.submitted_by, cs.submitted_at, cs.approved_by, cs.approved_at,
	cs.closed_by, cs.closed_at, COALESCE(cs.close_reason, '')
	FROM eacp.change_sets cs JOIN eacp.bundles b ON b.tenant_id = cs.tenant_id AND b.id = cs.bundle_id`

func scanChangeSet(r pgx.Row) (ChangeSet, error) {
	c := ChangeSet{Steps: []Step{}}
	var plannedAt time.Time
	err := r.Scan(&c.ID, &c.Bundle, &c.State, &c.Prune, &c.DesiredDigest, &c.BaseDigest, &c.SealedDigest,
		&c.PlannedBy, &plannedAt, &c.SubmittedBy, &c.SubmittedAt, &c.ApprovedBy, &c.ApprovedAt,
		&c.ClosedBy, &c.ClosedAt, &c.CloseReason)
	c.PlannedAt = &plannedAt
	return c, err
}

func loadSteps(ctx context.Context, tx pgx.Tx, id uuid.UUID) ([]Step, error) {
	rows, err := tx.Query(ctx, `SELECT ordinal, address, op, stage, payload, object_id
		FROM eacp.change_set_steps WHERE change_set_id = $1 ORDER BY ordinal`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Step, error) {
		var s Step
		var payload []byte
		if err := r.Scan(&s.Ordinal, &s.Address, &s.Op, &s.Stage, &payload, &s.ObjectID); err != nil {
			return s, err
		}
		return s, json.Unmarshal(payload, &s.Payload)
	})
}

// Get returns a change set with its steps.
func (s *Service) Get(ctx context.Context, a registry.Actor, id uuid.UUID) (ChangeSet, error) {
	var c ChangeSet
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		var err error
		if c, err = scanChangeSet(tx.QueryRow(ctx, `SELECT `+changeSetColumns+` WHERE cs.id = $1`, id)); err != nil {
			return err
		}
		c.Steps, err = loadSteps(ctx, tx, id)
		return err
	})
	return c, err
}

// List returns change sets, newest first, optionally of one bundle, without
// their steps.
func (s *Service) List(ctx context.Context, a registry.Actor, bundle string, limit int) ([]ChangeSet, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	out := []ChangeSet{}
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+changeSetColumns+`
			WHERE $1 = '' OR b.name = $1 ORDER BY cs.planned_at DESC LIMIT $2`, bundle, limit)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (ChangeSet, error) { return scanChangeSet(r) })
		return err
	})
	return out, err
}

// Bundles lists the tenant's bundles.
func (s *Service) Bundles(ctx context.Context, a registry.Actor) ([]BundleInfo, error) {
	out := []BundleInfo{}
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT b.name,
			(SELECT count(*) FROM eacp.bundle_resources br WHERE br.tenant_id = b.tenant_id AND br.bundle_id = b.id),
			(SELECT cs.id FROM eacp.change_sets cs WHERE cs.tenant_id = b.tenant_id AND cs.bundle_id = b.id
			   AND cs.state = 'APPLIED' ORDER BY COALESCE(cs.approved_at, cs.submitted_at) DESC LIMIT 1)
			FROM eacp.bundles b ORDER BY b.name`)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (BundleInfo, error) {
			var b BundleInfo
			return b, r.Scan(&b.Name, &b.Managed, &b.LastApplied)
		})
		return err
	})
	return out, err
}
