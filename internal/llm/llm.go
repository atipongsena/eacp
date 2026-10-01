// Package llm is the Go store over the LLM-call ledger (ADR-031, migration
// 00024). PostgreSQL decides everything: eacp.llm_admit admits or denies a
// call for an authenticated agent and reserves its budget, eacp.llm_settle
// prices and settles it for the gateway, and eacp.llm_sweep abandons calls
// still open after their deadline. This package only binds the actor and
// carries the values.
package llm

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atipongsena/eacp/internal/registry"
	"github.com/atipongsena/eacp/internal/storage"
)

// Settlement outcomes (ADR-031 §3.5). The gateway reports the first four;
// only the sweeper abandons a call.
const (
	OutcomeSucceeded     = "succeeded"
	OutcomeProviderError = "provider_error"
	OutcomeUsageUnknown  = "usage_unknown"
	OutcomeKilled        = "killed"
	OutcomeAbandoned     = "abandoned"
)

// The system actors that settle calls.
const (
	gatewayActor = "llm_gateway"
	sweeperActor = "llm_sweeper"
)

// Decision is the PDP's decision on a call, evaluated with no transaction
// open. Denial is the gateway's denial code for a verdict it does not admit
// (policy_denied, approval_unsupported, transform_unsupported), or "".
type Decision struct {
	ID, BundleID uuid.UUID
	Version      int
	Verdict      string
	InputDigest  [32]byte
	Denial       string
}

// AdmitRequest describes a call as the gateway received it. MaxOutputTokens
// is the requested output cap, or 0 when the request states none.
type AdmitRequest struct {
	AgentVersionID                                   uuid.UUID
	ModelName, Provider, Subject, TraceID, GatewayID string
	Stream                                           bool
	RequestBytes, MaxOutputTokens                    int64
	Decision                                         Decision
	StudioIntentID                                   uuid.UUID
	StudioRuntimeID                                  string
	StudioGeneration                                 int64
}

// Model is what an admitted call may be sent to.
type Model struct {
	ID                                uuid.UUID
	UpstreamModel, BaseURL, SecretRef string
	Timeout                           time.Duration
	MaxOutputTokens                   int64
}

// Admission is eacp.llm_admit's answer. A denied call has a Denial and a
// ledger row, and nothing else.
type Admission struct {
	CallID   uuid.UUID
	Denial   string
	Model    Model
	Deadline time.Time
	Studio   *StudioBinding
}

// StudioBinding is the immutable output contract for one admitted node.
type StudioBinding struct {
	RunID        uuid.UUID       `json:"run_id"`
	Index        int             `json:"index"`
	OutputSchema json.RawMessage `json:"output_schema"`
}

// Usage is the provider's token usage as the gateway read it. Known is false
// when the response carried none, or the gateway could not read it.
type Usage struct {
	Input, CacheRead, CacheWrite, Output int64
	Known                                bool
}

// Settlement is how a call ended.
type Settlement struct {
	Outcome        string
	ProviderStatus int
	Usage          Usage
	StudioOutput   json.RawMessage
	StudioFailure  string
}

// Call is one ledger row. Amounts are PostgreSQL's numeric text.
type Call struct {
	ID                 uuid.UUID  `json:"id"`
	AgentID            uuid.UUID  `json:"agent_id"`
	AgentVersionID     uuid.UUID  `json:"agent_version_id"`
	ModelID            *uuid.UUID `json:"model_id,omitempty"`
	ModelName          string     `json:"model"`
	Provider           string     `json:"provider"`
	SubjectPrincipalID *uuid.UUID `json:"subject_principal_id,omitempty"`
	Stream             bool       `json:"stream"`
	TraceID            string     `json:"trace_id,omitempty"`
	RequestBytes       int64      `json:"request_bytes"`
	MaxOutputTokens    *int64     `json:"max_output_tokens,omitempty"`
	DecisionID         *uuid.UUID `json:"decision_id,omitempty"`
	Verdict            string     `json:"verdict,omitempty"`
	State              string     `json:"state"`
	Denial             string     `json:"denial,omitempty"`
	Outcome            string     `json:"outcome,omitempty"`
	ProviderStatus     *int       `json:"provider_status,omitempty"`
	InputTokens        *int64     `json:"input_tokens,omitempty"`
	CacheReadTokens    *int64     `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens   *int64     `json:"cache_write_tokens,omitempty"`
	OutputTokens       *int64     `json:"output_tokens,omitempty"`
	CostAmount         *string    `json:"cost_amount,omitempty"`
	CostUnit           string     `json:"cost_unit,omitempty"`
	CommittedAmount    *string    `json:"committed_amount,omitempty"`
	GatewayID          string     `json:"gateway_id"`
	CreatedAt          time.Time  `json:"created_at"`
	Deadline           *time.Time `json:"deadline,omitempty"`
	SettledAt          *time.Time `json:"settled_at,omitempty"`
}

// Filter narrows List. Zero values match everything; Limit defaults to 100
// and is at most 1000.
type Filter struct {
	AgentID  uuid.UUID
	Model    string
	State    string
	From, To time.Time
	Limit    int
}

// Store runs the ledger's functions.
type Store struct{ pool *pgxpool.Pool }

// New returns a store over pool, which must connect as eacp_app.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Admit asks eacp.llm_admit to admit r for its agent version. A denial is an
// answer, not an error.
func (s *Store) Admit(ctx context.Context, tenant uuid.UUID, r AdmitRequest) (Admission, error) {
	d := map[string]any{"verdict": r.Decision.Verdict, "denial": r.Decision.Denial}
	if r.Decision.ID != uuid.Nil {
		d["id"] = r.Decision.ID
		d["input_digest"] = hex.EncodeToString(r.Decision.InputDigest[:])
	}
	if r.Decision.BundleID != uuid.Nil {
		d["bundle_id"] = r.Decision.BundleID
	}
	if r.Decision.Version > 0 {
		d["version"] = r.Decision.Version
	}
	in := map[string]any{
		"model_name": r.ModelName, "provider": r.Provider, "subject": r.Subject, "stream": r.Stream,
		"request_bytes": r.RequestBytes, "max_output_tokens": nil, "trace_id": r.TraceID, "gateway_id": r.GatewayID,
		"decision": d,
	}
	if r.MaxOutputTokens > 0 {
		in["max_output_tokens"] = r.MaxOutputTokens
	}
	if r.StudioIntentID != uuid.Nil {
		in["studio_intent_id"], in["studio_runtime_id"], in["studio_generation"] = r.StudioIntentID, r.StudioRuntimeID, r.StudioGeneration
	}
	body, err := json.Marshal(in)
	if err != nil {
		return Admission{}, err
	}
	var out struct {
		CallID          uuid.UUID      `json:"call_id"`
		Denial          string         `json:"denial"`
		ModelID         uuid.UUID      `json:"model_id"`
		UpstreamModel   string         `json:"upstream_model"`
		BaseURL         string         `json:"base_url"`
		SecretRef       string         `json:"secret_ref"`
		TimeoutMS       int64          `json:"timeout_ms"`
		MaxOutputTokens int64          `json:"max_output_tokens"`
		Deadline        time.Time      `json:"deadline"`
		Studio          *StudioBinding `json:"studio"`
	}
	err = storage.InTenantTx(ctx, s.pool, tenant.String(), func(tx pgx.Tx) error {
		if err := storage.SetAgent(ctx, tx, r.AgentVersionID); err != nil {
			return err
		}
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT eacp.llm_admit($1::jsonb)`, body).Scan(&raw); err != nil {
			return err
		}
		return json.Unmarshal(raw, &out)
	})
	if err != nil {
		return Admission{}, fmt.Errorf("llm: admit: %w", err)
	}
	a := Admission{CallID: out.CallID, Denial: out.Denial, Studio: out.Studio}
	if out.Denial == "" {
		a.Model = Model{ID: out.ModelID, UpstreamModel: out.UpstreamModel, BaseURL: out.BaseURL, SecretRef: out.SecretRef,
			Timeout: time.Duration(out.TimeoutMS) * time.Millisecond, MaxOutputTokens: out.MaxOutputTokens}
		a.Deadline = out.Deadline
	}
	return a, nil
}

// Settle settles call as the gateway. PostgreSQL prices known usage from the
// price pinned at admission and settles the reservation.
func (s *Store) Settle(ctx context.Context, tenant, call uuid.UUID, st Settlement) error {
	switch st.Outcome {
	case OutcomeSucceeded, OutcomeProviderError, OutcomeUsageUnknown, OutcomeKilled:
	default:
		return fmt.Errorf("llm: the gateway cannot settle a call as %q", st.Outcome)
	}
	var status *int
	if st.ProviderStatus > 0 {
		status = &st.ProviderStatus
	}
	u := st.Usage
	err := storage.InTenantTx(ctx, s.pool, tenant.String(), func(tx pgx.Tx) error {
		if err := storage.SetSystem(ctx, tx, gatewayActor); err != nil {
			return err
		}
		var output, failure *string
		if len(st.StudioOutput) > 0 {
			value := string(st.StudioOutput)
			output = &value
		}
		if st.StudioFailure != "" {
			failure = &st.StudioFailure
		}
		_, err := tx.Exec(ctx, `SELECT eacp.studio_llm_settle($1, $2, $3, $4, $5, $6, $7, $8,$9::jsonb,$10)`,
			call, st.Outcome, status, u.Input, u.CacheRead, u.CacheWrite, u.Output, u.Known, output, failure)
		return err
	})
	if err != nil {
		return fmt.Errorf("llm: settle %s: %w", call, err)
	}
	return nil
}

// Killed reports whether a kill scope now stops call.
func (s *Store) Killed(ctx context.Context, tenant, call uuid.UUID) (bool, error) {
	var killed *bool
	err := storage.InTenantTx(ctx, s.pool, tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT eacp.llm_call_killed($1)`, call).Scan(&killed)
	})
	if err != nil {
		return false, fmt.Errorf("llm: kill check: %w", err)
	}
	if killed == nil {
		return false, &registry.Error{Kind: registry.ErrNotFound, Msg: "no such LLM call"}
	}
	return *killed, nil
}

// KillEpoch is the tenant's kill epoch, which moves with every kill change.
func (s *Store) KillEpoch(ctx context.Context, tenant uuid.UUID) (int64, error) {
	var epoch int64
	err := storage.InTenantTx(ctx, s.pool, tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COALESCE((SELECT epoch FROM eacp.kill_tenant_epochs
			WHERE tenant_id = eacp.current_tenant_id()), 0)`).Scan(&epoch)
	})
	if err != nil {
		return 0, fmt.Errorf("llm: kill epoch: %w", err)
	}
	return epoch, nil
}

// SweepAll abandons every tenant's overdue calls as the sweeper and returns
// how many it settled. Replicas may run it at once: each row moves once.
func (s *Store) SweepAll(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT eacp.llm_sweep_tenants()`)
	if err != nil {
		return 0, fmt.Errorf("llm: sweep tenants: %w", err)
	}
	tenants, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return 0, fmt.Errorf("llm: sweep tenants: %w", err)
	}
	total := 0
	var errs []error
	for _, t := range tenants {
		var n int
		err := storage.InTenantTx(ctx, s.pool, t.String(), func(tx pgx.Tx) error {
			if err := storage.SetSystem(ctx, tx, sweeperActor); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT eacp.llm_sweep()`).Scan(&n)
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("llm: sweep tenant %s: %w", t, err))
			continue
		}
		total += n
	}
	return total, errors.Join(errs...)
}

const callColumns = `id, agent_id, agent_version_id, model_id, model_name, provider, subject_principal_id, stream,
	COALESCE(trace_id, ''), request_bytes, max_output_tokens, decision_id, COALESCE(verdict, ''), state,
	COALESCE(denial, ''), COALESCE(outcome, ''), provider_status, input_tokens, cache_read_tokens, cache_write_tokens,
	output_tokens, cost_amount::text, COALESCE(cost_unit, ''), committed_amount::text, gateway_id, created_at,
	deadline, settled_at`

func scanCall(row pgx.CollectableRow) (Call, error) {
	var c Call
	err := row.Scan(&c.ID, &c.AgentID, &c.AgentVersionID, &c.ModelID, &c.ModelName, &c.Provider,
		&c.SubjectPrincipalID, &c.Stream, &c.TraceID, &c.RequestBytes, &c.MaxOutputTokens, &c.DecisionID, &c.Verdict,
		&c.State, &c.Denial, &c.Outcome, &c.ProviderStatus, &c.InputTokens, &c.CacheReadTokens, &c.CacheWriteTokens,
		&c.OutputTokens, &c.CostAmount, &c.CostUnit, &c.CommittedAmount, &c.GatewayID, &c.CreatedAt, &c.Deadline,
		&c.SettledAt)
	return c, err
}

// List returns the tenant's calls matching f, newest first.
func (s *Store) List(ctx context.Context, tenant uuid.UUID, f Filter) ([]Call, error) {
	switch f.State {
	case "", "DENIED", "ADMITTED", "SETTLED":
	default:
		return nil, &registry.Error{Kind: registry.ErrInvalid, Msg: "state must be DENIED, ADMITTED or SETTLED"}
	}
	if f.Limit <= 0 {
		f.Limit = 100
	}
	f.Limit = min(f.Limit, 1000)
	var agent *uuid.UUID
	if f.AgentID != uuid.Nil {
		agent = &f.AgentID
	}
	var from, to *time.Time
	if !f.From.IsZero() {
		from = &f.From
	}
	if !f.To.IsZero() {
		to = &f.To
	}
	var calls []Call
	err := storage.InTenantReadTx(ctx, s.pool, tenant.String(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+callColumns+` FROM eacp.llm_calls
			WHERE ($1::uuid IS NULL OR agent_id = $1) AND ($2 = '' OR model_name = $2) AND ($3 = '' OR state = $3)
			  AND ($4::timestamptz IS NULL OR created_at >= $4) AND ($5::timestamptz IS NULL OR created_at < $5)
			ORDER BY created_at DESC, id DESC LIMIT $6`, agent, f.Model, f.State, from, to, f.Limit)
		if err != nil {
			return err
		}
		calls, err = pgx.CollectRows(rows, scanCall)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("llm: list: %w", err)
	}
	return calls, nil
}

// Get returns one call of the tenant.
func (s *Store) Get(ctx context.Context, tenant, id uuid.UUID) (Call, error) {
	var c Call
	err := storage.InTenantReadTx(ctx, s.pool, tenant.String(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+callColumns+` FROM eacp.llm_calls WHERE id = $1`, id)
		if err != nil {
			return err
		}
		c, err = pgx.CollectExactlyOneRow(rows, scanCall)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Call{}, &registry.Error{Kind: registry.ErrNotFound, Msg: "no such LLM call"}
	}
	if err != nil {
		return Call{}, fmt.Errorf("llm: get: %w", err)
	}
	return c, nil
}

// AgentRisk is the agent's risk class, for the PDP request.
func (s *Store) AgentRisk(ctx context.Context, tenant, agent uuid.UUID) (string, error) {
	var risk string
	err := storage.InTenantTx(ctx, s.pool, tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT risk_class FROM eacp.agents WHERE id = $1`, agent).Scan(&risk)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", &registry.Error{Kind: registry.ErrNotFound, Msg: "no such agent"}
	}
	if err != nil {
		return "", fmt.Errorf("llm: agent risk: %w", err)
	}
	return risk, nil
}
