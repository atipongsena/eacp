package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"eacp/internal/approval"
	"eacp/internal/governance"
	"eacp/internal/registry"
	"eacp/internal/storage"
)

// Limits are the static admission limits of Slice A (MASTER_PLAN §26):
// the number of QUEUED actions per tenant and across all tenants.
type Limits struct {
	MaxQueuedPerTenant int64
	MaxQueuedGlobal    int64
}

// DefaultLimits apply when Options.Limits leaves a limit at zero. Admission
// is never unbounded.
var DefaultLimits = Limits{MaxQueuedPerTenant: 1000, MaxQueuedGlobal: 10000}

// Reservation identifies what a budget reservation is for.
type Reservation struct {
	TenantID, ActionID, AgentID uuid.UUID
	Tool                        string
}

// Budget reserves budget inside the release transaction (MASTER_PLAN §47).
// An error aborts the release; the action stays AUTHORIZED and no grant is
// consumed. Hard budgets arrive in Slice B (Phase 11).
type Budget interface {
	Reserve(ctx context.Context, tx pgx.Tx, r Reservation) error
}

// NoBudget is the Slice A budget hook: it reserves nothing.
type NoBudget struct{}

func (NoBudget) Reserve(context.Context, pgx.Tx, Reservation) error { return nil }

// Options configure an Engine.
type Options struct {
	// Provider is the governance PDP. Nil means unavailable (fail closed).
	Provider governance.GovernanceProvider
	Budget   Budget
	Limits   Limits
	Log      *slog.Logger
	// EvaluationTimeout bounds one PDP call (default 5s). A timeout is an
	// unavailable PDP.
	EvaluationTimeout time.Duration

	// Test hooks: BeforeRelease runs between the release revalidation (R0)
	// and the release transaction (R1); InRelease runs inside R1 just before
	// the QUEUED transition, and its error aborts the release.
	BeforeRelease func(context.Context)
	InRelease     func(context.Context, pgx.Tx) error
}

// Engine runs action transitions against PostgreSQL.
type Engine struct {
	pool *pgxpool.Pool
	o    Options
}

// New returns an Engine over pool (connected as the application role).
func New(pool *pgxpool.Pool, o Options) *Engine {
	if o.Budget == nil {
		o.Budget = NoBudget{}
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if o.EvaluationTimeout <= 0 {
		o.EvaluationTimeout = 5 * time.Second
	}
	if o.Limits.MaxQueuedPerTenant <= 0 {
		o.Limits.MaxQueuedPerTenant = DefaultLimits.MaxQueuedPerTenant
	}
	if o.Limits.MaxQueuedGlobal <= 0 {
		o.Limits.MaxQueuedGlobal = DefaultLimits.MaxQueuedGlobal
	}
	return &Engine{pool: pool, o: o}
}

// Submission is an agent's request to perform an action (ADR-005 §2 binding).
type Submission struct {
	IdempotencyKey    string
	Subject           string
	Operation         string
	Target            string
	Tool              string // "connector.tool"
	ToolSchemaVersion string
	Resource          string
	Payload           json.RawMessage
	// Lifetime sets not_after = now() + Lifetime by the database clock
	// (default 1 hour, at most 24 hours).
	Lifetime time.Duration
	// Traceparent is the W3C trace context of the submitting request.
	Traceparent string
}

const (
	defaultLifetime = time.Hour
	maxLifetime     = 24 * time.Hour
	maxPayload      = 1 << 20
	stepLimit       = 8
	staleRetries    = 3
)

var (
	keyPattern         = regexp.MustCompile(`^[!-~]{1,255}$`)
	traceparentPattern = regexp.MustCompile(`^[0-9a-f]{2}-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$`)
)

func invalid(format string, args ...any) error {
	return &registry.Error{Kind: registry.ErrInvalid, Msg: fmt.Sprintf(format, args...)}
}

// validate returns the canonical payload and lifetime, or an ErrInvalid.
func (s Submission) validate() ([]byte, time.Duration, error) {
	if !keyPattern.MatchString(s.IdempotencyKey) {
		return nil, 0, invalid("Idempotency-Key must be 1-255 visible ASCII characters")
	}
	fields := []struct {
		name, value string
		max         int
	}{
		{"subject", s.Subject, 320}, {"operation", s.Operation, 256}, {"target", s.Target, 256},
		{"tool", s.Tool, 128}, {"tool_schema_version", s.ToolSchemaVersion, 64}, {"resource", s.Resource, 1024},
	}
	for _, f := range fields {
		if strings.TrimSpace(f.value) == "" || len(f.value) > f.max || !utf8.ValidString(f.value) {
			return nil, 0, invalid("%s is required (at most %d bytes of UTF-8)", f.name, f.max)
		}
	}
	if len(s.Payload) == 0 || len(s.Payload) > maxPayload {
		return nil, 0, invalid("payload is required (at most %d bytes)", maxPayload)
	}
	canonical, err := governance.Canonicalize(s.Payload)
	if err != nil {
		return nil, 0, invalid("payload: %v", err)
	}
	lifetime := s.Lifetime
	if lifetime == 0 {
		lifetime = defaultLifetime
	}
	if lifetime < time.Second || lifetime > maxLifetime {
		return nil, 0, invalid("lifetime must be between 1s and 24h")
	}
	return canonical, lifetime, nil
}

// inTx runs fn in the actor's tenant with the actor bound, retrying
// serialization failures and deadlocks (the whole transaction rolls back).
func (e *Engine) inTx(ctx context.Context, a Actor, fn func(pgx.Tx) error) error {
	for attempt := 0; ; attempt++ {
		err := storage.InTenantTx(ctx, e.pool, a.TenantID.String(), func(tx pgx.Tx) error {
			if err := a.bind(ctx, tx); err != nil {
				return err
			}
			return fn(tx)
		})
		if attempt < 3 && retryable(err) {
			continue
		}
		return err
	}
}

// Submit persists an agent's action (T1), or finds the action already
// submitted under the same Idempotency-Key, and advances it as far as it
// can go without a human: governance, then the release boundary.
//
// A replay with a different input digest is ErrIdempotencyConflict. A new
// submission over an admission limit is ErrAdmission and creates nothing.
// If governance is unavailable the action stays RECEIVED and the view is
// returned with ErrGovernanceUnavailable; resubmitting the same key
// re-evaluates it (T2a).
func (e *Engine) Submit(ctx context.Context, a Actor, s Submission) (View, error) {
	if !a.isAgent() || a.AgentID == uuid.Nil || a.TenantID == uuid.Nil {
		return View{}, &registry.Error{Kind: registry.ErrForbidden, Msg: "only agents submit actions"}
	}
	canonical, lifetime, err := s.validate()
	if err != nil {
		return View{}, err
	}
	input, _, err := governance.Digests(governance.Binding{
		TenantID: a.TenantID, AgentID: a.AgentID, AgentVersionID: a.AgentVersionID, Subject: s.Subject,
		Operation: s.Operation, Target: s.Target, Tool: s.Tool, ToolSchemaVersion: s.ToolSchemaVersion,
		Resource: s.Resource, Payload: canonical,
	}, canonical)
	if err != nil {
		return View{}, invalid("%v", err)
	}
	traceparent := s.Traceparent
	if !traceparentPattern.MatchString(traceparent) {
		traceparent = ""
	}

	var id uuid.UUID
	err = e.inTx(ctx, a, func(tx pgx.Tx) error {
		existing := func() (bool, error) {
			var digest []byte
			err := tx.QueryRow(ctx, `SELECT id, input_digest FROM eacp.actions
				WHERE agent_id = $1 AND idempotency_key = $2`, a.AgentID, s.IdempotencyKey).Scan(&id, &digest)
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			if !bytes.Equal(digest, input[:]) {
				return true, ErrIdempotencyConflict
			}
			return true, nil
		}
		if found, err := existing(); found || err != nil {
			return err
		}
		if err := e.admit(ctx, tx); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `INSERT INTO eacp.actions
			(tenant_id, agent_version_id, idempotency_key, subject, operation, target, tool,
			 tool_schema_version, resource, input_payload, input_digest, not_after, traceparent)
			VALUES (eacp.current_tenant_id(), $1, $2, $3, $4, $5, $6, $7, $8, $9::json, $10,
			        now() + make_interval(secs => $11), NULLIF($12, ''))
			ON CONFLICT (tenant_id, agent_id, idempotency_key) DO NOTHING
			RETURNING id`,
			a.AgentVersionID, s.IdempotencyKey, s.Subject, s.Operation, s.Target, s.Tool,
			s.ToolSchemaVersion, s.Resource, string(canonical), input[:], lifetime.Seconds(), traceparent).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			// A concurrent submission with the same key committed first.
			found, err := existing()
			if err == nil && !found {
				err = errors.New("action: idempotency key conflict without a visible action")
			}
			return err
		}
		return err
	})
	if err != nil {
		return View{}, mapErr(err)
	}
	return e.Advance(ctx, a, id)
}

// admit applies the static admission limits to released, unfinished
// actions (QUEUED to RETRY_WAIT). Counts are advisory under concurrency:
// concurrent submissions may overshoot by their number.
func (e *Engine) admit(ctx context.Context, tx pgx.Tx) error {
	var tenant, global int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM eacp.actions
		WHERE state IN ('QUEUED', 'LEASED', 'EXECUTING', 'RETRY_WAIT')`).Scan(&tenant); err != nil {
		return err
	}
	if tenant >= e.o.Limits.MaxQueuedPerTenant {
		return ErrAdmission
	}
	if err := tx.QueryRow(ctx, `SELECT eacp.global_queued_count()`).Scan(&global); err != nil {
		return err
	}
	if global >= e.o.Limits.MaxQueuedGlobal {
		return ErrAdmission
	}
	return nil
}

// Get returns an action of tenant.
func (e *Engine) Get(ctx context.Context, tenant, id uuid.UUID) (View, error) {
	var r row
	err := storage.InTenantTx(ctx, e.pool, tenant.String(), func(tx pgx.Tx) error {
		var err error
		r, err = load(ctx, tx, id, false)
		return err
	})
	if err != nil {
		return View{}, mapErr(err)
	}
	return r.view(), nil
}

// Read returns an action as actor may see it: an agent sees only its own
// agent's actions (another agent's action is ErrNotFound). Callers check a
// principal's role themselves.
func (e *Engine) Read(ctx context.Context, a Actor, id uuid.UUID) (View, error) {
	r, err := e.read(ctx, a, id)
	if err != nil {
		return View{}, err
	}
	return r.view(), nil
}

// read loads an action for actor; an agent sees only its own agent's actions.
func (e *Engine) read(ctx context.Context, a Actor, id uuid.UUID) (row, error) {
	var r row
	err := storage.InTenantTx(ctx, e.pool, a.TenantID.String(), func(tx pgx.Tx) error {
		var err error
		r, err = load(ctx, tx, id, false)
		return err
	})
	if err == nil && a.isAgent() && r.AgentID != a.AgentID {
		err = pgx.ErrNoRows
	}
	return r, mapErr(err)
}

// Advance moves an action forward without a human: RECEIVED is evaluated
// (T2-T5) and AUTHORIZED goes through the release boundary (T10-T13). It
// stops at the first state that needs something else (an approval, a
// worker, an operator) and returns the latest view with any error.
func (e *Engine) Advance(ctx context.Context, a Actor, id uuid.UUID) (View, error) {
	r, err := e.read(ctx, a, id)
	if err != nil {
		return View{}, err
	}
	for range stepLimit {
		var stepErr error
		switch r.State {
		case "RECEIVED":
			stepErr = e.evaluate(ctx, a, r)
		case "AUTHORIZED":
			stepErr = e.release(ctx, a, r)
		default:
			return r.view(), nil
		}
		next, err := e.read(ctx, a, id)
		if err != nil {
			return r.view(), err
		}
		if stepErr != nil {
			return next.view(), mapErr(stepErr)
		}
		if next.State == r.State {
			return next.view(), nil
		}
		r = next
	}
	return r.view(), nil
}

// snapshot is the registry and policy state a decision was made against.
type snapshot struct {
	policyID      uuid.UUID
	policyVersion int
	policy        json.RawMessage // nil: no usable policy is active
	riskClass     string
	sideEffects   string
	contractID    uuid.UUID
	allowlistID   uuid.UUID
}

func (s snapshot) same(o snapshot) bool {
	return s.policyID == o.policyID && s.policyVersion == o.policyVersion &&
		s.contractID == o.contractID && s.allowlistID == o.allowlistID
}

// inputs reads, under the caller's action lock, everything a decision
// depends on. Registry rows and the policy pointer are read FOR SHARE, so
// in a transition transaction they cannot change before commit (ADR-004
// principle 7). A non-empty denial is a deterministic T2/T12 reason.
func (e *Engine) inputs(ctx context.Context, tx pgx.Tx, r row) (snapshot, string, error) {
	var s snapshot
	if r.SubjectPrincipalID == nil {
		return s, "subject_invalid", nil
	}
	err := tx.QueryRow(ctx, `SELECT 1 FROM eacp.principals
		WHERE id = $1 AND kind = 'human' AND disabled_at IS NULL FOR SHARE`, *r.SubjectPrincipalID).Scan(new(int))
	if errors.Is(err, pgx.ErrNoRows) {
		return s, "subject_invalid", nil
	}
	if err != nil {
		return s, "", err
	}
	grant, denial, err := registry.CheckCapability(ctx, tx, r.AgentVersionID, r.Tool)
	if err != nil || denial != "" {
		return s, string(denial), err
	}
	if r.ToolID == nil || grant.ToolID != *r.ToolID {
		return s, string(registry.DenyUnknownTool), nil
	}
	s.contractID = grant.ContractID
	if err := tx.QueryRow(ctx, `SELECT a.risk_class, v.active_allowlist_id,
			array_to_string(c.side_effects, ',')
		FROM eacp.agent_versions v
		JOIN eacp.agents a ON a.tenant_id = v.tenant_id AND a.id = v.agent_id
		JOIN eacp.tool_contracts c ON c.tenant_id = v.tenant_id AND c.id = $2
		WHERE v.id = $1`, r.AgentVersionID, grant.ContractID).
		Scan(&s.riskClass, &s.allowlistID, &s.sideEffects); err != nil {
		return s, "", err
	}
	var content string
	err = tx.QueryRow(ctx, `SELECT b.id, b.version, b.content::text
		FROM eacp.tenant_policy_pointer p
		JOIN eacp.policy_bundles b ON b.tenant_id = p.tenant_id AND b.id = p.current_bundle_id
		WHERE p.tenant_id = eacp.current_tenant_id() AND b.revoked_at IS NULL
		FOR SHARE OF p, b`).Scan(&s.policyID, &s.policyVersion, &content)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return s, "", nil // no usable policy: governance is unavailable
	case err != nil:
		return s, "", err
	}
	s.policy = json.RawMessage(content)
	return s, "", nil
}

// decide calls the PDP with no transaction open.
func (e *Engine) decide(ctx context.Context, a Actor, r row, s snapshot) (governance.GovernanceDecision, error) {
	log := e.o.Log.With("tenant", a.TenantID.String(), "action", r.ID.String())
	if s.policy == nil {
		log.WarnContext(ctx, "no active policy; action not evaluated")
		return governance.GovernanceDecision{}, fmt.Errorf("%w: no active policy", ErrGovernanceUnavailable)
	}
	if e.o.Provider == nil {
		return governance.GovernanceDecision{}, fmt.Errorf("%w: no provider configured", ErrGovernanceUnavailable)
	}
	ctx, cancel := context.WithTimeout(ctx, e.o.EvaluationTimeout)
	defer cancel()
	d, err := governance.EvaluateChecked(ctx, e.o.Provider, governance.GovernanceRequest{
		Binding: r.binding(a.TenantID), RiskClass: s.riskClass, SideEffectClass: s.sideEffects,
		PolicyBundleID: s.policyID, PolicyVersion: s.policyVersion, Policy: s.policy,
	})
	if err != nil {
		if errors.Is(err, governance.ErrMalformedDecision) {
			alert(ctx, log, "governance.malformed_decision", err)
		} else {
			log.WarnContext(ctx, "governance unavailable", "err", err)
		}
		return d, fmt.Errorf("%w: %w", ErrGovernanceUnavailable, err)
	}
	if d.DigestMismatch {
		alert(ctx, log, "governance.digest_mismatch", errors.New("provider digests differ from EACP's"))
	}
	return d, nil
}

// alert emits a security signal (ADR-002 §4, §5).
func alert(ctx context.Context, log *slog.Logger, kind string, err error) {
	log.ErrorContext(ctx, "security alert", "alert", kind, "err", err)
}

// reason is a decision's reasons as a state reason.
func reason(d governance.GovernanceDecision) string {
	if d.DigestMismatch {
		return "digest_mismatch"
	}
	r := strings.Join(d.Reasons, "; ")
	for len(r) > 1024 {
		_, size := utf8.DecodeLastRuneInString(r)
		r = r[:len(r)-size]
	}
	return r
}

// approvalExpiry is the request lifetime: the policy TTL from evaluation,
// capped by the action's not_after (ADR-005 §8).
func approvalExpiry(r row, d governance.GovernanceDecision) time.Time {
	exp := d.EvaluatedAt.Truncate(time.Microsecond).Add(time.Duration(d.Approval.TTLSeconds) * time.Second)
	if r.NotAfter.Before(exp) {
		return r.NotAfter
	}
	return exp
}

// move is a compare-and-set transition that must hit the locked row.
func move(ctx context.Context, tx pgx.Tx, sql string, args ...any) error {
	tag, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("action: transition lost its compare-and-set")
	}
	return nil
}

func deny(ctx context.Context, tx pgx.Tx, r row, why string, evidence *uuid.UUID) error {
	return move(ctx, tx, `UPDATE eacp.actions SET state = 'DENIED', state_reason = $3,
		decision_evidence_id = COALESCE($4, decision_evidence_id)
		WHERE id = $1 AND state = $2`, r.ID, r.State, why, evidence)
}

func expire(ctx context.Context, tx pgx.Tx, r row) error {
	return move(ctx, tx, `UPDATE eacp.actions SET state = 'EXPIRED', state_reason = 'not_after passed'
		WHERE id = $1 AND state = $2`, r.ID, r.State)
}

// locked runs one transition transaction on the locked action. It returns
// done=true when fn is not needed: the action left want, or it expired or
// was denied on the registry, both decided here.
func (e *Engine) locked(ctx context.Context, a Actor, id uuid.UUID, want string,
	fn func(tx pgx.Tx, r row, s snapshot) error) error {
	return e.inTx(ctx, a, func(tx pgx.Tx) error {
		r, err := load(ctx, tx, id, true)
		if err != nil || r.State != want {
			return err
		}
		if r.Expired {
			return expire(ctx, tx, r) // T5 / T13
		}
		s, denial, err := e.inputs(ctx, tx, r)
		if err != nil {
			return err
		}
		if denial != "" {
			return deny(ctx, tx, r, denial, nil) // T2 / T12, no PDP consulted
		}
		return fn(tx, r, s)
	})
}

var errStale = errors.New("action: decision inputs changed; re-evaluate")

// evaluate is T2-T5 for a RECEIVED action.
func (e *Engine) evaluate(ctx context.Context, a Actor, r row) error {
	for range staleRetries {
		var snap snapshot
		var ready bool
		err := e.locked(ctx, a, r.ID, "RECEIVED", func(_ pgx.Tx, _ row, s snapshot) error {
			snap, ready = s, true
			return nil
		})
		if err != nil || !ready {
			return err
		}
		d, err := e.decide(ctx, a, r, snap)
		if err != nil {
			return err
		}
		err = e.locked(ctx, a, r.ID, "RECEIVED", func(tx pgx.Tx, cur row, now snapshot) error {
			if !now.same(snap) {
				return errStale
			}
			evidence, err := governance.RecordDecision(ctx, tx, cur.ID, d)
			if err != nil {
				return err
			}
			switch d.Verdict {
			case governance.VerdictDeny:
				return deny(ctx, tx, cur, reason(d), &evidence) // T2
			case governance.VerdictEscalate:
				enforced, err := governance.Canonicalize(d.EnforcedPayload)
				if err != nil {
					return err
				}
				request, err := approval.CreateRequest(ctx, tx, approval.NewRequest{
					ActionID: cur.ID, DecisionEvidenceID: evidence, ExpiresAt: approvalExpiry(cur, d),
				})
				if err != nil {
					return err
				}
				return move(ctx, tx, `UPDATE eacp.actions SET state = 'PENDING_APPROVAL', state_reason = $2,
					decision_evidence_id = $3, enforced_payload = $4::json, approval_request_id = $5
					WHERE id = $1 AND state = 'RECEIVED'`, cur.ID, reason(d), evidence, string(enforced), request) // T4
			default:
				enforced, err := governance.Canonicalize(d.EnforcedPayload)
				if err != nil {
					return err
				}
				return move(ctx, tx, `UPDATE eacp.actions SET state = 'AUTHORIZED', state_reason = $2,
					decision_evidence_id = $3, enforced_payload = $4::json
					WHERE id = $1 AND state = 'RECEIVED'`, cur.ID, reason(d), evidence, string(enforced)) // T3
			}
		})
		if !errors.Is(err, errStale) {
			return err
		}
	}
	return fmt.Errorf("%w: policy or registry kept changing during evaluation", ErrGovernanceUnavailable)
}

// release is the atomic release boundary for an AUTHORIZED action (ADR-005
// §5a): R0 revalidates with no transaction open, then R1 re-reads every
// input under lock and either releases (T10), asks for re-approval (T11),
// denies (T12) or expires (T13), all in one transaction.
func (e *Engine) release(ctx context.Context, a Actor, r row) error {
	for range staleRetries {
		var snap snapshot
		var ready bool
		err := e.locked(ctx, a, r.ID, "AUTHORIZED", func(_ pgx.Tx, _ row, s snapshot) error {
			snap, ready = s, true
			return nil
		})
		if err != nil || !ready {
			return err
		}
		d, err := e.decide(ctx, a, r, snap) // R0; on failure the action stays AUTHORIZED
		if err != nil {
			return err
		}
		if e.o.BeforeRelease != nil {
			e.o.BeforeRelease(ctx)
		}
		err = e.locked(ctx, a, r.ID, "AUTHORIZED", func(tx pgx.Tx, cur row, now snapshot) error {
			if !now.same(snap) {
				return errStale // the pointer or registry moved after R0
			}
			return e.releaseLocked(ctx, tx, a, cur, d, now)
		})
		if !errors.Is(err, errStale) {
			return err
		}
	}
	return fmt.Errorf("%w: policy or registry kept changing during release", ErrGovernanceUnavailable)
}

// approvalRows are the action's current request and its unconsumed grant.
type approvalRows struct {
	id                      uuid.UUID
	state                   string
	policyID                uuid.UUID
	policyVersion           int
	allowlistID, contractID uuid.UUID
	expired                 bool
	grantID                 *uuid.UUID
	grantExpired            bool
}

func (q *approvalRows) live() bool { return q != nil && (q.state == "PENDING" || q.state == "GRANTED") }

// currentApproval locks the action's current request and unconsumed grant,
// before any audited write (lock order, migration 00005).
func currentApproval(ctx context.Context, tx pgx.Tx, r row) (*approvalRows, error) {
	if r.ApprovalRequestID == nil {
		return nil, nil
	}
	q := &approvalRows{id: *r.ApprovalRequestID}
	if err := tx.QueryRow(ctx, `SELECT state, policy_bundle_id, policy_version, active_allowlist_id,
			active_contract_id, expires_at <= now() OR not_after <= now()
		FROM eacp.approval_requests WHERE id = $1 FOR UPDATE`, q.id).
		Scan(&q.state, &q.policyID, &q.policyVersion, &q.allowlistID, &q.contractID, &q.expired); err != nil {
		return nil, err
	}
	var expired bool
	var grant uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id, expires_at <= now() FROM eacp.approval_grants
		WHERE request_id = $1 AND consumed_at IS NULL FOR UPDATE`, q.id).Scan(&grant, &expired)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return nil, err
	default:
		q.grantID, q.grantExpired = &grant, expired
	}
	return q, nil
}

// voidApproval voids a live request whose policy or dependencies changed.
func voidApproval(ctx context.Context, tx pgx.Tx, q *approvalRows) error {
	if !q.live() {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE eacp.approval_requests SET state = 'VOIDED' WHERE id = $1`, q.id); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE eacp.approval_grants SET expires_at = now()
		WHERE request_id = $1 AND consumed_at IS NULL AND expires_at > now()`, q.id)
	return err
}

func (e *Engine) releaseLocked(ctx context.Context, tx pgx.Tx, a Actor, r row,
	d governance.GovernanceDecision, s snapshot) error {
	q, err := currentApproval(ctx, tx, r)
	if err != nil {
		return err
	}
	record := func() (uuid.UUID, error) { return governance.RecordDecision(ctx, tx, r.ID, d) }

	if d.Verdict == governance.VerdictDeny || d.DigestMismatch || !bytes.Equal(d.EnforcedDigest[:], r.EnforcedDigest) {
		why := reason(d)
		if d.Verdict != governance.VerdictDeny && !d.DigestMismatch {
			why = "digest_changed" // the approved or authorized payload is not what would run now
		}
		evidence, err := record()
		if err != nil {
			return err
		}
		return deny(ctx, tx, r, why, &evidence) // T12
	}

	if d.Verdict == governance.VerdictEscalate {
		usable := q != nil && q.state == "GRANTED" && q.policyID == d.PolicyBundleID &&
			q.policyVersion == d.PolicyVersion && q.allowlistID == s.allowlistID && q.contractID == s.contractID
		if usable && (q.expired || q.grantID == nil || q.grantExpired) {
			// The approval expired before release: the action expires with
			// it (ADR-005 default; the request cascade applies T13).
			return move(ctx, tx, `UPDATE eacp.approval_requests SET state = 'EXPIRED' WHERE id = $1`, q.id)
		}
		if !usable {
			// T11: the approval was for another policy version or registry
			// state (or never obtained): void it and ask again.
			if err := voidApproval(ctx, tx, q); err != nil {
				return err
			}
			evidence, err := record()
			if err != nil {
				return err
			}
			request, err := approval.CreateRequest(ctx, tx, approval.NewRequest{
				ActionID: r.ID, DecisionEvidenceID: evidence, ExpiresAt: approvalExpiry(r, d),
			})
			if err != nil {
				return err
			}
			return move(ctx, tx, `UPDATE eacp.actions SET state = 'PENDING_APPROVAL', state_reason = $2,
				decision_evidence_id = $3, approval_request_id = $4
				WHERE id = $1 AND state = 'AUTHORIZED'`, r.ID, reason(d), evidence, request)
		}
		evidence, err := record()
		if err != nil {
			return err
		}
		if _, err := approval.Consume(ctx, tx, r.ID, d.EnforcedDigest, d.PolicyVersion); err != nil {
			return err
		}
		return e.queue(ctx, tx, a, r, evidence, d, s)
	}

	// allow, warn, transform: any approval state left from an earlier
	// policy version is void (ADR-005 §5a).
	if err := voidApproval(ctx, tx, q); err != nil {
		return err
	}
	evidence, err := record()
	if err != nil {
		return err
	}
	return e.queue(ctx, tx, a, r, evidence, d, s)
}

// queue reserves budget and makes the action executable (T10), pinning the
// policy version and connector contract.
func (e *Engine) queue(ctx context.Context, tx pgx.Tx, a Actor, r row, evidence uuid.UUID,
	d governance.GovernanceDecision, s snapshot) error {
	if err := e.o.Budget.Reserve(ctx, tx, Reservation{
		TenantID: a.TenantID, ActionID: r.ID, AgentID: r.AgentID, Tool: r.Tool,
	}); err != nil {
		return err
	}
	if e.o.InRelease != nil {
		if err := e.o.InRelease(ctx, tx); err != nil {
			return err
		}
	}
	return move(ctx, tx, `UPDATE eacp.actions SET state = 'QUEUED', state_reason = $2,
		decision_evidence_id = $3, connector_contract_id = $4
		WHERE id = $1 AND state = 'AUTHORIZED'`, r.ID, reason(d), evidence, s.contractID)
}

// Cancel cancels an action (ADR-004 T9, T13, T15, T18). Before any
// dispatch intent the action becomes CANCELLED; once a dispatch intent
// exists (EXECUTING, RETRY_WAIT) only a cancel request is recorded: the
// worker cancels the in-flight call and the sweeper fails a waiting retry
// (ADR-004 Rev 2.3). It never consults governance, so it works while the
// PDP is down (ADR-002 §6). The database allows the submitting agent, the
// subject and operators, and voids outstanding approval state.
func (e *Engine) Cancel(ctx context.Context, a Actor, id uuid.UUID, why string) (View, error) {
	if strings.TrimSpace(why) == "" {
		return View{}, invalid("a cancellation requires a reason")
	}
	err := e.inTx(ctx, a, func(tx pgx.Tx) error {
		r, err := load(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if a.isAgent() && r.AgentID != a.AgentID {
			return pgx.ErrNoRows
		}
		switch r.State {
		case "PENDING_APPROVAL", "AUTHORIZED", "QUEUED", "LEASED":
			return move(ctx, tx, `UPDATE eacp.actions SET state = 'CANCELLED', state_reason = $3
				WHERE id = $1 AND state = $2`, r.ID, r.State, why)
		case "EXECUTING", "RETRY_WAIT":
			if r.CancelRequestedAt != nil {
				return &registry.Error{Kind: registry.ErrConflict, Msg: "cancellation was already requested"}
			}
			return move(ctx, tx, `UPDATE eacp.actions SET cancel_requested_at = now(), cancel_reason = $3
				WHERE id = $1 AND state = $2`, r.ID, r.State, why)
		}
		return &registry.Error{Kind: registry.ErrConflict, Msg: "an action in state " + r.State + " cannot be cancelled"}
	})
	if err != nil {
		return View{}, mapErr(err)
	}
	return e.Get(ctx, a.TenantID, id)
}
