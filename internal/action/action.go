// Package action runs the Action API up to the atomic release boundary:
// submission with idempotency and admission (ADR-004 T1), governance
// evaluation (T2-T5), approval outcomes (T6-T9, driven by the approval rows),
// release, re-approval and denial at release (T10-T13) and pre-dispatch
// expiry and cancellation (T13, T15). Workers, leases and dispatch are
// Phase 5.
//
// The governance provider is only called with no transaction open (ADR-005
// §5a). Every transition is a compare-and-set on the locked action row, and
// migration 00005 re-checks each guard in PostgreSQL, attributes the actor,
// journals the change and writes the outbox row in the same transaction.
package action

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"eacp/internal/governance"
	"eacp/internal/registry"
	"eacp/internal/storage"
)

// Errors returned by the Engine, besides *registry.Error kinds.
var (
	// ErrIdempotencyConflict: the key was used for a request with a
	// different input digest (HTTP 409).
	ErrIdempotencyConflict = errors.New("action: idempotency key reused for a different request")
	// ErrAdmission: a static admission limit is reached; no action was
	// created (HTTP 429, MASTER_PLAN §26).
	ErrAdmission = errors.New("action: admission limit reached")
	// ErrGovernanceUnavailable: no complete decision could be obtained. The
	// action keeps its state and nothing becomes executable (ADR-002 §6).
	ErrGovernanceUnavailable = errors.New("action: governance unavailable")
)

// Actor performs an engine call. Exactly one identity is set.
type Actor struct {
	TenantID       uuid.UUID
	PrincipalID    uuid.UUID
	AgentID        uuid.UUID
	AgentVersionID uuid.UUID
	System         string
}

// Agent is an authenticated agent version.
func Agent(tenant, agent, version uuid.UUID) Actor {
	return Actor{TenantID: tenant, AgentID: agent, AgentVersionID: version}
}

// Principal is an authenticated principal (an operator or the subject).
func Principal(tenant, principal uuid.UUID) Actor {
	return Actor{TenantID: tenant, PrincipalID: principal}
}

// System is a named control-plane component, e.g. "sweeper".
func System(tenant uuid.UUID, component string) Actor {
	return Actor{TenantID: tenant, System: component}
}

func (a Actor) isAgent() bool { return a.AgentVersionID != uuid.Nil }

// bind sets the transaction's single actor (migration 00005, actor_context).
func (a Actor) bind(ctx context.Context, tx pgx.Tx) error {
	switch {
	case a.AgentVersionID != uuid.Nil:
		return storage.SetAgent(ctx, tx, a.AgentVersionID)
	case a.PrincipalID != uuid.Nil:
		return storage.SetActor(ctx, tx, a.PrincipalID)
	case a.System != "":
		return storage.SetSystem(ctx, tx, a.System)
	}
	return &registry.Error{Kind: registry.ErrForbidden, Msg: "no actor"}
}

// View is an action as the API shows it. Payloads are RFC 8785 text.
type View struct {
	ID                       uuid.UUID       `json:"id"`
	State                    string          `json:"state"`
	StateReason              string          `json:"state_reason,omitempty"`
	AgentID                  uuid.UUID       `json:"agent_id"`
	AgentVersionID           uuid.UUID       `json:"agent_version_id"`
	IdempotencyKey           string          `json:"idempotency_key"`
	OperationKey             string          `json:"operation_key"`
	Subject                  string          `json:"subject"`
	Operation                string          `json:"operation"`
	Target                   string          `json:"target"`
	Tool                     string          `json:"tool"`
	ToolSchemaVersion        string          `json:"tool_schema_version"`
	Resource                 string          `json:"resource"`
	InputDigest              string          `json:"input_digest"`
	EnforcedDigest           string          `json:"enforced_digest,omitempty"`
	EnforcedPayload          json.RawMessage `json:"enforced_payload,omitempty"`
	PolicyVersion            *int            `json:"policy_version,omitempty"`
	ApprovalRequestID        *uuid.UUID      `json:"approval_request_id,omitempty"`
	ConnectorContractVersion *int            `json:"connector_contract_version,omitempty"`
	NotAfter                 time.Time       `json:"not_after"`
	CreatedAt                time.Time       `json:"created_at"`
	StateChangedAt           time.Time       `json:"state_changed_at"`
	ReleasedAt               *time.Time      `json:"released_at,omitempty"`
}

// Terminal reports whether the action can no longer change (ADR-004).
func (v View) Terminal() bool { return terminal(v.State) }

func terminal(state string) bool {
	switch state {
	case "SUCCEEDED", "FAILED", "DENIED", "CANCELLED", "EXPIRED":
		return true
	}
	return false
}

// row is the stored action.
type row struct {
	ID, AgentID, AgentVersionID                uuid.UUID
	IdempotencyKey, Subject, Operation, Target string
	Tool, ToolSchemaVersion, Resource          string
	SubjectPrincipalID, ToolID                 *uuid.UUID
	InputPayload                               string
	InputDigest                                []byte
	NotAfter                                   time.Time
	Expired                                    bool // not_after has passed, by the database clock
	OperationKey, State, StateReason           string
	DecisionEvidenceID, PolicyBundleID         *uuid.UUID
	PolicyVersion                              *int
	EnforcedPayload                            *string
	EnforcedDigest                             []byte
	ApprovalRequestID, ConnectorContractID     *uuid.UUID
	ConnectorContractVersion                   *int
	ReleasedAt                                 *time.Time
	CreatedAt, StateChangedAt                  time.Time
}

const rowColumns = `id, agent_id, agent_version_id, idempotency_key, subject, operation, target,
	tool, tool_schema_version, resource, subject_principal_id, tool_id, input_payload::text,
	input_digest, not_after, not_after <= now(), operation_key, state, COALESCE(state_reason, ''),
	decision_evidence_id, policy_bundle_id, policy_version, enforced_payload::text, enforced_digest,
	approval_request_id, connector_contract_id, connector_contract_version, released_at,
	created_at, state_changed_at`

func scanRow(r pgx.Row) (row, error) {
	var x row
	err := r.Scan(&x.ID, &x.AgentID, &x.AgentVersionID, &x.IdempotencyKey, &x.Subject, &x.Operation,
		&x.Target, &x.Tool, &x.ToolSchemaVersion, &x.Resource, &x.SubjectPrincipalID, &x.ToolID,
		&x.InputPayload, &x.InputDigest, &x.NotAfter, &x.Expired, &x.OperationKey, &x.State,
		&x.StateReason, &x.DecisionEvidenceID, &x.PolicyBundleID, &x.PolicyVersion, &x.EnforcedPayload,
		&x.EnforcedDigest, &x.ApprovalRequestID, &x.ConnectorContractID, &x.ConnectorContractVersion,
		&x.ReleasedAt, &x.CreatedAt, &x.StateChangedAt)
	return x, err
}

// load reads an action, locking it FOR UPDATE when lock is set: the first
// lock of every transaction that changes an action (migration 00005).
func load(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (row, error) {
	q := `SELECT ` + rowColumns + ` FROM eacp.actions WHERE id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	return scanRow(tx.QueryRow(ctx, q, id))
}

func (r row) view() View {
	v := View{
		ID: r.ID, State: r.State, StateReason: r.StateReason, AgentID: r.AgentID,
		AgentVersionID: r.AgentVersionID, IdempotencyKey: r.IdempotencyKey, OperationKey: r.OperationKey,
		Subject: r.Subject, Operation: r.Operation, Target: r.Target, Tool: r.Tool,
		ToolSchemaVersion: r.ToolSchemaVersion, Resource: r.Resource,
		InputDigest: hex.EncodeToString(r.InputDigest), PolicyVersion: r.PolicyVersion,
		ApprovalRequestID: r.ApprovalRequestID, ConnectorContractVersion: r.ConnectorContractVersion,
		NotAfter: r.NotAfter, CreatedAt: r.CreatedAt, StateChangedAt: r.StateChangedAt, ReleasedAt: r.ReleasedAt,
	}
	if r.EnforcedPayload != nil {
		v.EnforcedPayload = json.RawMessage(*r.EnforcedPayload)
		v.EnforcedDigest = hex.EncodeToString(r.EnforcedDigest)
	}
	return v
}

// binding is the action's identity for governance (ADR-005 §2).
func (r row) binding(tenant uuid.UUID) governance.Binding {
	return governance.Binding{
		TenantID: tenant, AgentID: r.AgentID, AgentVersionID: r.AgentVersionID, Subject: r.Subject,
		Operation: r.Operation, Target: r.Target, Tool: r.Tool, ToolSchemaVersion: r.ToolSchemaVersion,
		Resource: r.Resource, Payload: json.RawMessage(r.InputPayload),
	}
}

// retryable reports serialization failures and deadlocks, which roll the
// whole transaction back and may simply be retried.
func retryable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "40P01")
}

// mapErr translates database rejections into registry error kinds.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	var re *registry.Error
	if errors.As(err, &re) || errors.Is(err, ErrIdempotencyConflict) || errors.Is(err, ErrAdmission) ||
		errors.Is(err, ErrGovernanceUnavailable) {
		return err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return &registry.Error{Kind: registry.ErrNotFound, Msg: "no such action"}
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
	case "23514", "23502", "22P02", "22023", "22001", "22007", "22008":
		kind = registry.ErrInvalid
	default:
		return fmt.Errorf("action: %w", err)
	}
	return &registry.Error{Kind: kind, Msg: pgErr.Message}
}
