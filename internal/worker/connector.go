package worker

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
)

// Outcome is a connector's classification of one call (ADR-004 "Definitive
// vs ambiguous results").
type Outcome string

const (
	// Succeeded: a definitive success carrying an external reference.
	Succeeded Outcome = "succeeded"
	// NoEffect: a definitive error the pinned contract certifies as having
	// no side effect (its no_effect_errors).
	NoEffect Outcome = "no_effect"
	// Ambiguous: anything else. The effect may have happened.
	Ambiguous Outcome = "ambiguous"
)

// Contract is the pinned connector contract facts a call needs.
type Contract struct {
	Version             int
	SideEffects         []string
	IdempotencyMode     string // native, correlation_only or none
	IdempotencyKeyField string
	CorrelationField    string
	NoEffectErrors      []string
	MaxAttempts         int
}

// Call is one dispatch of an action, made only after its dispatch intent
// committed. OperationKey is stable across attempts (ADR-004 "Operation
// identity"); Generation is the lease fencing token, for targets that
// support conditional writes. Payload is the enforced payload (RFC 8785).
type Call struct {
	TenantID     uuid.UUID
	ActionID     uuid.UUID
	OperationKey string
	Attempt      int
	Generation   int64
	Tool         string // connector.tool
	Endpoint     string
	Payload      json.RawMessage
	Contract     Contract
	Secret       Secret
}

// Result is a connector's classified result. ErrorClass names a
// definitive error; the worker downgrades a NoEffect whose class the
// contract does not certify, or a Succeeded without ExternalReference, to
// Ambiguous.
type Result struct {
	Outcome           Outcome
	ExternalReference string
	ErrorClass        string
}

// LookupCall asks a connector for evidence about the stable operation key.
// Only the execution worker supplies the credential; reconciliation owns
// interpretation of positive and negative evidence (ADR-004 §20.2).
type LookupCall struct {
	TenantID     uuid.UUID
	OperationKey string
	Endpoint     string
	Secret       Secret
}

// LookupStatus is what a lookup by operation key saw. Only the reconciler
// interprets it, under the pinned contract's proof standard (ADR-004 §20.2).
type LookupStatus string

const (
	// LookupFound: exactly one record carries the operation key.
	LookupFound LookupStatus = "found"
	// LookupAbsent: the target certified that no record carries the key.
	// Proof of no effect only under an AUTHORITATIVE contract.
	LookupAbsent LookupStatus = "absent"
	// LookupConflict: more than one record carries the key.
	LookupConflict LookupStatus = "conflict"
	// LookupUnknown: anything else, including errors and malformed answers.
	LookupUnknown LookupStatus = "unknown"
)

type LookupResult struct {
	Status            LookupStatus
	ExternalReference string
}

// Connector performs calls for one protocol. Execute must respect ctx: the
// worker sets its deadline inside the lease and cancels it when the lease
// is lost or a cancellation is requested. Lookup only reports evidence; the
// Reconciler decides whether absence proves no effect under the pinned
// contract.
type Connector interface {
	Execute(ctx context.Context, c Call) Result
	Lookup(ctx context.Context, c LookupCall) LookupResult
}
