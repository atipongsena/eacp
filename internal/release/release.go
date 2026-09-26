// Package release runs agent releases (ADR-018): evaluation evidence,
// research-grade replay, a shadow that cannot execute, a canary cohort,
// promotion and rollback.
//
// PostgreSQL enforces every rule (migration 00019): the stages and their
// gates, separation of duties, the second ACTIVE version, the cohort at
// T2/T10/T16, the guardrails and the automatic rollback. This package reads
// and writes rows, calls the PDP for a candidate's proposal with no
// transaction open (ADR-005 §5a), and runs the evaluator.
package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"eacp/internal/governance"
	"eacp/internal/registry"
	"eacp/internal/storage"
)

// SystemActor is the component that rolls back breached canaries.
const SystemActor = "release"

// Denial codes the engine gives a candidate's actions (ADR-018 §4).
const (
	DenyCanaryCohort       = "canary_cohort"
	DenyReleaseNotInCanary = "release_not_in_canary"
)

// ErrGovernanceUnavailable: the PDP gave no usable decision for a proposal,
// so nothing was recorded. The caller may retry.
var ErrGovernanceUnavailable = errors.New("release: governance unavailable")

// Options configures a Service.
type Options struct {
	Provider          governance.GovernanceProvider
	EvaluationTimeout time.Duration // default 5 s
	Log               *slog.Logger
}

// Service reads and changes releases.
type Service struct {
	pool *pgxpool.Pool
	o    Options
}

// New returns a Service.
func New(pool *pgxpool.Pool, o Options) *Service {
	if o.EvaluationTimeout <= 0 {
		o.EvaluationTimeout = 5 * time.Second
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &Service{pool: pool, o: o}
}

// Agent is an authenticated agent key.
type Agent struct {
	TenantID, AgentID, VersionID uuid.UUID
}

func invalid(format string, args ...any) error {
	return &registry.Error{Kind: registry.ErrInvalid, Msg: fmt.Sprintf(format, args...)}
}

// ------------------------------------------------------------- releases

// Plan is a release plan. Unset fields take the database defaults.
type Plan struct {
	RequiredSuites     []string     `json:"required_suites"`
	MinReplayCases     *int         `json:"min_replay_cases,omitempty"`
	MinReplayAgreement *json.Number `json:"min_replay_agreement,omitempty"`
	MinShadowCases     *int         `json:"min_shadow_cases,omitempty"`
	MinShadowAgreement *json.Number `json:"min_shadow_agreement,omitempty"`
	CanarySteps        []int        `json:"canary_steps,omitempty"` // basis points
	MinCanaryActions   *int         `json:"min_canary_actions,omitempty"`
	MaxDenialIncrease  *json.Number `json:"max_denial_increase,omitempty"`
	MaxFailureIncrease *json.Number `json:"max_failure_increase,omitempty"`
	MaxUnknownIncrease *json.Number `json:"max_unknown_increase,omitempty"`
	MaxLatencyRatio    *json.Number `json:"max_latency_ratio,omitempty"`
	MaxCostRatio       *json.Number `json:"max_cost_ratio,omitempty"`
}

// OpenRequest opens a release of a candidate version.
type OpenRequest struct {
	ID                 uuid.UUID `json:"id,omitzero"` // optional; the database generates one
	CandidateVersionID uuid.UUID `json:"candidate_version_id"`
	Reason             string    `json:"reason"`
	Plan
}

// Release is a release row.
type Release struct {
	ID                 uuid.UUID       `json:"id"`
	AgentID            uuid.UUID       `json:"agent_id"`
	StableVersionID    uuid.UUID       `json:"stable_version_id"`
	CandidateVersionID uuid.UUID       `json:"candidate_version_id"`
	State              string          `json:"state"`
	RequiredSuites     []string        `json:"required_suites"`
	MinReplayCases     int             `json:"min_replay_cases"`
	MinReplayAgreement json.Number     `json:"min_replay_agreement"`
	MinShadowCases     int             `json:"min_shadow_cases"`
	MinShadowAgreement json.Number     `json:"min_shadow_agreement"`
	CanarySteps        []int           `json:"canary_steps"`
	MinCanaryActions   int             `json:"min_canary_actions"`
	MaxDenialIncrease  json.Number     `json:"max_denial_increase"`
	MaxFailureIncrease json.Number     `json:"max_failure_increase"`
	MaxUnknownIncrease json.Number     `json:"max_unknown_increase"`
	MaxLatencyRatio    json.Number     `json:"max_latency_ratio"`
	MaxCostRatio       json.Number     `json:"max_cost_ratio"`
	Reason             string          `json:"reason"`
	CanaryBP           *int            `json:"canary_bp,omitempty"`
	StageStartedAt     time.Time       `json:"stage_started_at"`
	CreatedBy          uuid.UUID       `json:"created_by"`
	CreatedAt          time.Time       `json:"created_at"`
	ChangedBy          *uuid.UUID      `json:"changed_by,omitempty"`
	ChangedAt          *time.Time      `json:"changed_at,omitempty"`
	ChangeReason       string          `json:"change_reason,omitempty"`
	Breaches           json.RawMessage `json:"breaches,omitempty"`
}

const releaseColumns = `id, agent_id, stable_version_id, candidate_version_id, state, required_suites,
	min_replay_cases, trim_scale(min_replay_agreement)::text, min_shadow_cases,
	trim_scale(min_shadow_agreement)::text, canary_steps, min_canary_actions,
	trim_scale(max_denial_increase)::text, trim_scale(max_failure_increase)::text,
	trim_scale(max_unknown_increase)::text, trim_scale(max_latency_ratio)::text, trim_scale(max_cost_ratio)::text,
	reason, canary_bp, stage_started_at, created_by, created_at, changed_by, changed_at,
	COALESCE(change_reason, ''), breaches`

func scanRelease(r pgx.Row) (Release, error) {
	var x Release
	var steps []int32
	var bp *int32
	var numbers [7]string
	err := r.Scan(&x.ID, &x.AgentID, &x.StableVersionID, &x.CandidateVersionID, &x.State, &x.RequiredSuites,
		&x.MinReplayCases, &numbers[0], &x.MinShadowCases, &numbers[1], &steps, &x.MinCanaryActions,
		&numbers[2], &numbers[3], &numbers[4], &numbers[5], &numbers[6],
		&x.Reason, &bp, &x.StageStartedAt, &x.CreatedBy, &x.CreatedAt, &x.ChangedBy, &x.ChangedAt,
		&x.ChangeReason, &x.Breaches)
	x.MinReplayAgreement, x.MinShadowAgreement = json.Number(numbers[0]), json.Number(numbers[1])
	x.MaxDenialIncrease, x.MaxFailureIncrease = json.Number(numbers[2]), json.Number(numbers[3])
	x.MaxUnknownIncrease, x.MaxLatencyRatio, x.MaxCostRatio = json.Number(numbers[4]), json.Number(numbers[5]), json.Number(numbers[6])
	for _, s := range steps {
		x.CanarySteps = append(x.CanarySteps, int(s))
	}
	if bp != nil {
		n := int(*bp)
		x.CanaryBP = &n
	}
	return x, err
}

// decimal is a plain non-negative decimal: the plan's rates and ratios.
var decimal = regexp.MustCompile(`^(0|[1-9][0-9]{0,5})(\.[0-9]{1,6})?$`)

// Open opens a release (registry_editor or registry_approver). The
// database reads the stable version and checks the plan.
func (s *Service) Open(ctx context.Context, a registry.Actor, in OpenRequest) (Release, error) {
	if in.CandidateVersionID == uuid.Nil {
		return Release{}, invalid("candidate_version_id is required")
	}
	cols := []string{"tenant_id", "candidate_version_id", "required_suites", "reason"}
	vals := []string{"eacp.current_tenant_id()", "$1", "$2", "$3"}
	args := []any{in.CandidateVersionID, in.RequiredSuites, in.Reason}
	add := func(col, cast string, v any) {
		args = append(args, v)
		cols = append(cols, col)
		vals = append(vals, fmt.Sprintf("$%d%s", len(args), cast))
	}
	if in.ID != uuid.Nil {
		add("id", "", in.ID)
	}
	if in.RequiredSuites == nil {
		args[1] = []string{}
	}
	for _, n := range []struct {
		col string
		v   *int
	}{{"min_replay_cases", in.MinReplayCases}, {"min_shadow_cases", in.MinShadowCases},
		{"min_canary_actions", in.MinCanaryActions}} {
		if n.v != nil {
			add(n.col, "", *n.v)
		}
	}
	for _, n := range []struct {
		col string
		v   *json.Number
	}{{"min_replay_agreement", in.MinReplayAgreement}, {"min_shadow_agreement", in.MinShadowAgreement},
		{"max_denial_increase", in.MaxDenialIncrease}, {"max_failure_increase", in.MaxFailureIncrease},
		{"max_unknown_increase", in.MaxUnknownIncrease}, {"max_latency_ratio", in.MaxLatencyRatio},
		{"max_cost_ratio", in.MaxCostRatio}} {
		if n.v != nil {
			if !decimal.MatchString(string(*n.v)) {
				return Release{}, invalid("%s must be a plain decimal", n.col)
			}
			add(n.col, "::numeric", string(*n.v))
		}
	}
	if in.CanarySteps != nil {
		add("canary_steps", "", in.CanarySteps)
	}
	var r Release
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		var err error
		r, err = scanRelease(tx.QueryRow(ctx, `INSERT INTO eacp.agent_releases (`+strings.Join(cols, ", ")+`)
			VALUES (`+strings.Join(vals, ", ")+`) RETURNING `+releaseColumns, args...))
		return err
	})
	return r, err
}

// Filter narrows List.
type Filter struct {
	AgentID uuid.UUID
	State   string
}

// List returns releases, newest first.
func (s *Service) List(ctx context.Context, a registry.Actor, f Filter) ([]Release, error) {
	out := []Release{}
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+releaseColumns+` FROM eacp.agent_releases
			WHERE ($1::uuid IS NULL OR agent_id = $1) AND ($2 = '' OR state = $2)
			ORDER BY created_at DESC, id LIMIT 500`, nullID(f.AgentID), f.State)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Release, error) { return scanRelease(r) })
		return err
	})
	return out, err
}

// Evaluation is a recorded evaluation result.
type Evaluation struct {
	ID            uuid.UUID `json:"id"`
	Suite         string    `json:"suite"`
	Score         string    `json:"score"`
	Threshold     string    `json:"threshold"`
	Passed        bool      `json:"passed"`
	DatasetDigest string    `json:"dataset_digest"`
	EvidenceRef   string    `json:"evidence_ref"`
	RecordedBy    uuid.UUID `json:"recorded_by"`
	RecordedAt    time.Time `json:"recorded_at"`
}

const evaluationColumns = `id, suite, trim_scale(score)::text, trim_scale(threshold)::text, passed,
	dataset_digest, evidence_ref, recorded_by, recorded_at`

func scanEvaluation(r pgx.Row) (Evaluation, error) {
	var e Evaluation
	err := r.Scan(&e.ID, &e.Suite, &e.Score, &e.Threshold, &e.Passed, &e.DatasetDigest, &e.EvidenceRef,
		&e.RecordedBy, &e.RecordedAt)
	return e, err
}

// Detail is a release with the evidence of every stage: its evaluations
// and the suites that have not passed, replay and shadow summaries, and
// the canary report of the current step (CANARY only).
type Detail struct {
	Release
	Evaluations   []Evaluation    `json:"evaluations"`
	FailingSuites []string        `json:"failing_suites"`
	Replay        json.RawMessage `json:"replay"`
	Shadow        json.RawMessage `json:"shadow"`
	Canary        json.RawMessage `json:"canary,omitempty"`
}

// Get returns a release's detail.
func (s *Service) Get(ctx context.Context, a registry.Actor, id uuid.UUID) (Detail, error) {
	var d Detail
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		var err error
		if d.Release, err = scanRelease(tx.QueryRow(ctx, `SELECT `+releaseColumns+`
			FROM eacp.agent_releases WHERE id = $1`, id)); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+evaluationColumns+` FROM eacp.agent_release_evaluations
			WHERE release_id = $1 ORDER BY seq`, id)
		if err != nil {
			return err
		}
		if d.Evaluations, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Evaluation, error) {
			return scanEvaluation(r)
		}); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT eacp.release_failing_suites(r),
			eacp.release_observation_summary(r.id, 'replay'), eacp.release_observation_summary(r.id, 'shadow'),
			eacp.release_canary_report(r.id)
			FROM eacp.agent_releases r WHERE r.id = $1`, id).Scan(&d.FailingSuites, &d.Replay, &d.Shadow, &d.Canary)
	})
	if d.Evaluations == nil {
		d.Evaluations = []Evaluation{}
	}
	return d, err
}

// EvaluationInput is one evaluation result, attested by its recorder.
type EvaluationInput struct {
	Suite         string `json:"suite"`
	Score         string `json:"score"`
	Threshold     string `json:"threshold"`
	DatasetDigest string `json:"dataset_digest"` // SHA-256, lowercase hex
	EvidenceRef   string `json:"evidence_ref"`
}

var score = regexp.MustCompile(`^-?(0|[1-9][0-9]{0,5})(\.[0-9]{1,6})?$`)

// RecordEvaluation records an evaluation result (registry_editor or
// registry_approver) while the release is EVALUATING or SHADOW.
func (s *Service) RecordEvaluation(ctx context.Context, a registry.Actor, release uuid.UUID, in EvaluationInput) (Evaluation, error) {
	if !score.MatchString(in.Score) || !score.MatchString(in.Threshold) {
		return Evaluation{}, invalid("score and threshold are decimals with at most 6 integer and 6 fractional digits")
	}
	var e Evaluation
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		var err error
		e, err = scanEvaluation(tx.QueryRow(ctx, `INSERT INTO eacp.agent_release_evaluations
			(tenant_id, release_id, suite, score, threshold, dataset_digest, evidence_ref)
			VALUES (eacp.current_tenant_id(), $1, $2, $3::numeric, $4::numeric, $5, $6) RETURNING `+evaluationColumns,
			release, in.Suite, in.Score, in.Threshold, in.DatasetDigest, in.EvidenceRef))
		return err
	})
	return e, err
}

// Advance moves a release forward one stage, as a registry_approver who is
// a second person (ADR-018 §5): EVALUATING to SHADOW, SHADOW to CANARY at
// the first step, CANARY to its next step, and from the last step to
// PROMOTED. From names the state (and, in CANARY, the step) the caller
// reviewed; a release that moved since is a conflict.
type Advance struct {
	From         string `json:"from"`
	FromCanaryBP *int   `json:"from_canary_bp,omitempty"`
	Reason       string `json:"reason"`
}

// Advance applies an Advance.
func (s *Service) Advance(ctx context.Context, a registry.Actor, id uuid.UUID, in Advance) (Release, error) {
	if in.From == "CANARY" && in.FromCanaryBP == nil {
		return Release{}, invalid("from_canary_bp is required to advance a canary")
	}
	var r Release
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		var state string
		var bp *int32
		var steps []int32
		if err := tx.QueryRow(ctx, `SELECT state, canary_bp, canary_steps FROM eacp.agent_releases
			WHERE id = $1 FOR UPDATE`, id).Scan(&state, &bp, &steps); err != nil {
			return err
		}
		if state != in.From || (state == "CANARY" && int(*bp) != *in.FromCanaryBP) {
			at := state
			if bp != nil {
				at = fmt.Sprintf("%s at %d basis points", state, *bp)
			}
			return &registry.Error{Kind: registry.ErrConflict, Msg: "the release is " + at + ", not " + in.From}
		}
		next, nextBP := "", any(nil)
		switch state {
		case "EVALUATING":
			next = "SHADOW"
		case "SHADOW":
			next, nextBP = "CANARY", steps[0]
		case "CANARY":
			next, nextBP = "PROMOTED", *bp
			for i, st := range steps {
				if st == *bp && i+1 < len(steps) {
					next, nextBP = "CANARY", steps[i+1]
				}
			}
		default:
			return &registry.Error{Kind: registry.ErrConflict, Msg: "release is " + state + " (terminal)"}
		}
		var err error
		r, err = scanRelease(tx.QueryRow(ctx, `UPDATE eacp.agent_releases SET state = $2, canary_bp = $3,
			change_reason = $4 WHERE id = $1 RETURNING `+releaseColumns, id, next, nextBP, in.Reason))
		return err
	})
	return r, err
}

// Rollback closes an open release (operator or registry_approver) and
// suspends an ACTIVE candidate in the same transaction.
func (s *Service) Rollback(ctx context.Context, a registry.Actor, id uuid.UUID, reason string) (Release, error) {
	var r Release
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		var err error
		r, err = scanRelease(tx.QueryRow(ctx, `UPDATE eacp.agent_releases SET state = 'ROLLED_BACK',
			change_reason = $2 WHERE id = $1 RETURNING `+releaseColumns, id, reason))
		return err
	})
	return r, err
}

// -------------------------------------------------------- observations

// Proposal is what the candidate would do for a reference request.
type Proposal struct {
	Kind              string          `json:"kind"` // replay or shadow
	ReferenceActionID uuid.UUID       `json:"reference_action_id"`
	Subject           string          `json:"subject"`
	Operation         string          `json:"operation"`
	Target            string          `json:"target"`
	Tool              string          `json:"tool"`
	ToolSchemaVersion string          `json:"tool_schema_version"`
	Resource          string          `json:"resource"`
	Payload           json.RawMessage `json:"payload"`
}

// Observation is a recorded proposal with the database's comparison.
// Recorded is the reference's recorded outcome, the replay response.
type Observation struct {
	ID                uuid.UUID       `json:"id"`
	ReleaseID         uuid.UUID       `json:"release_id"`
	Kind              string          `json:"kind"`
	ReferenceActionID uuid.UUID       `json:"reference_action_id"`
	Tool              string          `json:"tool"`
	CapabilityDenial  *string         `json:"capability_denial,omitempty"`
	Verdict           *string         `json:"verdict,omitempty"`
	PolicyVersion     *int            `json:"policy_version,omitempty"`
	CandidateOutcome  string          `json:"candidate_outcome"`
	ReferenceOutcome  *string         `json:"reference_outcome,omitempty"`
	ReferenceState    string          `json:"reference_state"`
	ToolMatch         bool            `json:"tool_match"`
	OutcomeMatch      bool            `json:"outcome_match"`
	PayloadMatch      bool            `json:"payload_match"`
	Agrees            bool            `json:"agrees"`
	Recorded          json.RawMessage `json:"recorded,omitempty"`
	RecordedAt        time.Time       `json:"recorded_at"`
}

func (p Proposal) validate() ([]byte, error) {
	if p.Kind != "replay" && p.Kind != "shadow" {
		return nil, invalid("kind is replay or shadow")
	}
	if p.ReferenceActionID == uuid.Nil {
		return nil, invalid("reference_action_id is required")
	}
	for _, f := range []struct {
		name, value string
		max         int
	}{
		{"subject", p.Subject, 320}, {"operation", p.Operation, 256}, {"target", p.Target, 256},
		{"tool", p.Tool, 128}, {"tool_schema_version", p.ToolSchemaVersion, 64}, {"resource", p.Resource, 1024},
	} {
		if strings.TrimSpace(f.value) == "" || len(f.value) > f.max || !utf8.ValidString(f.value) {
			return nil, invalid("%s is required (at most %d bytes of UTF-8)", f.name, f.max)
		}
	}
	if len(p.Payload) == 0 || len(p.Payload) > 1<<20 {
		return nil, invalid("payload is required (at most 1 MiB)")
	}
	canonical, err := governance.Canonicalize(p.Payload)
	if err != nil {
		return nil, invalid("payload: %v", err)
	}
	return canonical, nil
}

// Observe records the candidate's proposal. When the candidate's own
// allowlist permits the tool, the PDP decides it first, with no
// transaction open; the database checks the pairing and compares.
func (s *Service) Observe(ctx context.Context, a Agent, p Proposal) (Observation, error) {
	if a.TenantID == uuid.Nil || a.AgentID == uuid.Nil || a.VersionID == uuid.Nil {
		return Observation{}, &registry.Error{Kind: registry.ErrForbidden, Msg: "only agents observe"}
	}
	canonical, err := p.validate()
	if err != nil {
		return Observation{}, err
	}

	// What the decision depends on, read under the candidate's key.
	var denial, sideEffects, policy *string
	var riskClass string
	var policyID *uuid.UUID
	var policyVersion *int
	err = s.agentTx(ctx, a, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT 1 FROM eacp.agent_releases
			WHERE candidate_version_id = $1 AND state IN ('EVALUATING', 'SHADOW', 'CANARY')`, a.VersionID).
			Scan(new(int)); errors.Is(err, pgx.ErrNoRows) {
			return &registry.Error{Kind: registry.ErrForbidden, Msg: "only the candidate of an open release observes"}
		} else if err != nil {
			return err
		}
		// The same capability rule the database applies to the observation.
		return tx.QueryRow(ctx, `WITH tool AS (
				SELECT t.id, t.active_contract_id FROM eacp.tools t
				JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
				WHERE c.name || '.' || t.name = $2)
			SELECT eacp.allowlist_tool_denial(v.active_allowlist_id, (SELECT id FROM tool)), ag.risk_class,
			       (SELECT array_to_string(ct.side_effects, ',') FROM eacp.tool_contracts ct
			        WHERE ct.id = (SELECT active_contract_id FROM tool)),
			       b.id, b.version, b.content::text
			FROM eacp.agent_versions v
			JOIN eacp.agents ag ON ag.tenant_id = v.tenant_id AND ag.id = v.agent_id
			LEFT JOIN eacp.tenant_policy_pointer pp ON pp.tenant_id = v.tenant_id
			LEFT JOIN eacp.policy_bundles b ON b.tenant_id = pp.tenant_id AND b.id = pp.current_bundle_id
			     AND b.revoked_at IS NULL
			WHERE v.id = $1`, a.VersionID, p.Tool).
			Scan(&denial, &riskClass, &sideEffects, &policyID, &policyVersion, &policy)
	})
	if err != nil {
		return Observation{}, err
	}

	var verdict *string
	if denial == nil {
		if policy == nil || s.o.Provider == nil {
			return Observation{}, fmt.Errorf("%w: no active policy or provider", ErrGovernanceUnavailable)
		}
		effects := ""
		if sideEffects != nil {
			effects = *sideEffects
		}
		dctx, cancel := context.WithTimeout(ctx, s.o.EvaluationTimeout)
		d, err := governance.EvaluateChecked(dctx, s.o.Provider, governance.GovernanceRequest{
			Binding: governance.Binding{TenantID: a.TenantID, AgentID: a.AgentID, AgentVersionID: a.VersionID,
				Subject: p.Subject, Operation: p.Operation, Target: p.Target, Tool: p.Tool,
				ToolSchemaVersion: p.ToolSchemaVersion, Resource: p.Resource, Payload: canonical},
			RiskClass: riskClass, SideEffectClass: effects,
			PolicyBundleID: *policyID, PolicyVersion: *policyVersion, Policy: json.RawMessage(*policy),
		})
		cancel()
		if err != nil {
			s.o.Log.WarnContext(ctx, "governance unavailable for a release observation", "err", err)
			return Observation{}, fmt.Errorf("%w: %w", ErrGovernanceUnavailable, err)
		}
		v := string(d.Verdict)
		if d.DigestMismatch {
			v = string(governance.VerdictDeny)
		}
		verdict = &v
	} else {
		policyVersion = nil
	}

	var o Observation
	err = s.agentTx(ctx, a, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO eacp.agent_release_observations
			(tenant_id, kind, reference_action_id, subject, operation, target, tool, tool_schema_version,
			 resource, input_payload, verdict, policy_version)
			VALUES (eacp.current_tenant_id(), $1, $2, $3, $4, $5, $6, $7, $8, $9::json, $10, $11)
			RETURNING id, release_id, kind, reference_action_id, tool, capability_denial, verdict, policy_version,
			          candidate_outcome, reference_outcome, reference_state, tool_match, outcome_match,
			          payload_match, agrees, recorded, recorded_at`,
			p.Kind, p.ReferenceActionID, p.Subject, p.Operation, p.Target, p.Tool, p.ToolSchemaVersion,
			p.Resource, string(canonical), verdict, policyVersion).
			Scan(&o.ID, &o.ReleaseID, &o.Kind, &o.ReferenceActionID, &o.Tool, &o.CapabilityDenial, &o.Verdict,
				&o.PolicyVersion, &o.CandidateOutcome, &o.ReferenceOutcome, &o.ReferenceState, &o.ToolMatch,
				&o.OutcomeMatch, &o.PayloadMatch, &o.Agrees, &o.Recorded, &o.RecordedAt)
	})
	return o, err
}

// ---------------------------------------------------------------- route

// Route is the version that serves a subject: the canary candidate inside
// its cohort, otherwise the stable version. Admission is still checked on
// every action; the route only helps the runtime choose.
type Route struct {
	VersionID uuid.UUID  `json:"version_id"`
	Version   int        `json:"version"`
	Cohort    string     `json:"cohort"` // canary or stable
	ReleaseID *uuid.UUID `json:"release_id,omitempty"`
	CanaryBP  *int       `json:"canary_bp,omitempty"`
}

// Route returns the version of a's agent that serves subject.
func (s *Service) Route(ctx context.Context, a Agent, subject string) (Route, error) {
	if a.TenantID == uuid.Nil || a.AgentID == uuid.Nil {
		return Route{}, &registry.Error{Kind: registry.ErrForbidden, Msg: "only agents route"}
	}
	if strings.TrimSpace(subject) == "" || len(subject) > 320 {
		return Route{}, invalid("subject is required (at most 320 bytes)")
	}
	var r Route
	err := mapErr(storage.InTenantReadTx(ctx, s.pool, a.TenantID.String(), func(tx pgx.Tx) error {
		var version *uuid.UUID
		var candidate *uuid.UUID
		var bp *int32
		if err := tx.QueryRow(ctx, `WITH p AS (
				SELECT id FROM eacp.principals WHERE subject = $2 AND kind = 'human' AND disabled_at IS NULL)
			SELECT eacp.release_route($1, (SELECT id FROM p)), r.id, r.candidate_version_id, r.canary_bp
			FROM (SELECT 1) one
			LEFT JOIN eacp.agent_releases r ON r.agent_id = $1 AND r.state = 'CANARY'`,
			a.AgentID, subject).Scan(&version, &r.ReleaseID, &candidate, &bp); err != nil {
			return err
		}
		if version == nil {
			return &registry.Error{Kind: registry.ErrNotFound, Msg: "the agent has no ACTIVE version"}
		}
		r.VersionID, r.Cohort = *version, "stable"
		if candidate != nil && *candidate == *version {
			r.Cohort = "canary"
		}
		if bp != nil {
			n := int(*bp)
			r.CanaryBP = &n
		}
		return tx.QueryRow(ctx, `SELECT version FROM eacp.agent_versions WHERE id = $1`, r.VersionID).Scan(&r.Version)
	}))
	return r, err
}

// ------------------------------------------------------------ evaluator

// Evaluate rolls back the tenant's breached canaries and returns how many.
// It skips the tenant (0, nil) while another replica is evaluating it (ADR-029).
func (s *Service) Evaluate(ctx context.Context, tenant uuid.UUID) (int, error) {
	var n int
	err := storage.InTenantTx(ctx, s.pool, tenant.String(), func(tx pgx.Tx) error {
		if err := storage.SetSystem(ctx, tx, SystemActor); err != nil {
			return err
		}
		if got, err := storage.TryLoopLock(ctx, tx, "release"); err != nil || !got {
			return err
		}
		return tx.QueryRow(ctx, `SELECT eacp.release_evaluate()`).Scan(&n)
	})
	return n, err
}

// EvaluateAll evaluates every tenant the reviewed hint eacp.release_tenants()
// lists. A tenant's failure does not stop the others; the first is returned.
func (s *Service) EvaluateAll(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT tenant_id FROM eacp.release_tenants()`)
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
			first = fmt.Errorf("release: evaluate tenant %s: %w", t, err)
		}
	}
	return total, first
}

// Run evaluates every interval until ctx ends.
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		n, err := s.EvaluateAll(ctx)
		if err != nil && ctx.Err() == nil {
			s.o.Log.ErrorContext(ctx, "release evaluation failed", "err", err)
		} else if n > 0 {
			s.o.Log.WarnContext(ctx, "canaries rolled back on a guardrail breach", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ------------------------------------------------------------- plumbing

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

func (s *Service) agentTx(ctx context.Context, a Agent, fn func(pgx.Tx) error) error {
	return mapErr(storage.InTenantTx(ctx, s.pool, a.TenantID.String(), func(tx pgx.Tx) error {
		if err := storage.SetAgent(ctx, tx, a.VersionID); err != nil {
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

func nullID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

func mapErr(err error) error {
	if err == nil {
		return nil
	}
	var re *registry.Error
	if errors.As(err, &re) || errors.Is(err, ErrGovernanceUnavailable) {
		return err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return &registry.Error{Kind: registry.ErrNotFound, Msg: "no such release"}
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
	case "23514", "23502", "22P02", "22023", "22001", "22003", "22007", "22008", "2202E":
		kind = registry.ErrInvalid
	default:
		return err
	}
	return &registry.Error{Kind: kind, Msg: pgErr.Message}
}
