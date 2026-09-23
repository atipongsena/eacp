// Package governance defines the provider-neutral governance decision and
// action binding used by the local Slice A policy provider (ADR-002).
package governance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type Verdict string

const (
	VerdictAllow     Verdict = "allow"
	VerdictWarn      Verdict = "warn"
	VerdictDeny      Verdict = "deny"
	VerdictEscalate  Verdict = "escalate"
	VerdictTransform Verdict = "transform"
)

type ApprovalRequirement struct {
	Quorum        int      `json:"quorum"`
	EligibleRoles []string `json:"eligible_roles"`
	TTLSeconds    int      `json:"ttl_seconds"`
}

type GovernanceRequest struct {
	Binding         Binding
	RiskClass       string
	SideEffectClass string
	PolicyBundleID  uuid.UUID
	PolicyVersion   int
	Policy          json.RawMessage
}

type GovernanceDecision struct {
	Verdict            Verdict
	EnforcedPayload    json.RawMessage
	InputDigest        [32]byte
	EnforcedDigest     [32]byte
	PolicyBundleID     uuid.UUID
	PolicyVersion      int
	Provider           string
	ProviderInstanceID string
	DecisionID         uuid.UUID
	Reasons            []string
	Approval           *ApprovalRequirement
	EvaluatedAt        time.Time
	// DigestMismatch is set by EvaluateChecked when the provider's digests
	// differ from EACP's: a security signal that also denies the action.
	DigestMismatch bool
}

// EvaluateChecked failures. Both are transient for the caller: nothing
// becomes executable (ADR-002 §6). A malformed decision also raises an alert.
var (
	ErrProviderUnavailable = errors.New("governance: provider unavailable")
	ErrMalformedDecision   = errors.New("governance: malformed provider decision")
)

// GovernanceProvider makes a pure decision. The caller owns evidence and
// approval storage; provider implementations never mutate either.
type GovernanceProvider interface {
	Evaluate(context.Context, GovernanceRequest) (GovernanceDecision, error)
}

// EvaluateChecked treats an unavailable or incomplete PDP as a transient
// failure. A returned digest mismatch is a deterministic denial: no payload
// can become executable from that response.
func EvaluateChecked(ctx context.Context, provider GovernanceProvider, req GovernanceRequest) (GovernanceDecision, error) {
	d, err := provider.Evaluate(ctx, req)
	if err != nil {
		return GovernanceDecision{}, fmt.Errorf("%w: %w", ErrProviderUnavailable, err)
	}
	if d.PolicyBundleID != req.PolicyBundleID || d.PolicyVersion != req.PolicyVersion ||
		d.PolicyBundleID == uuid.Nil || d.PolicyVersion < 1 || d.Provider == "" ||
		d.ProviderInstanceID == "" || d.DecisionID == uuid.Nil || d.EvaluatedAt.IsZero() ||
		len(d.Reasons) == 0 || len(d.EnforcedPayload) == 0 {
		return GovernanceDecision{}, fmt.Errorf("%w: incomplete provider decision", ErrMalformedDecision)
	}
	switch d.Verdict {
	case VerdictAllow, VerdictWarn, VerdictDeny, VerdictTransform:
		if d.Approval != nil {
			return GovernanceDecision{}, fmt.Errorf("%w: unexpected approval requirement", ErrMalformedDecision)
		}
	case VerdictEscalate:
		if d.Approval == nil || d.Approval.Quorum < 1 || d.Approval.Quorum > 5 ||
			d.Approval.TTLSeconds < 1 || d.Approval.TTLSeconds > 86400 ||
			len(d.Approval.EligibleRoles) != 1 || d.Approval.EligibleRoles[0] != "approver" {
			return GovernanceDecision{}, fmt.Errorf("%w: incomplete approval requirement", ErrMalformedDecision)
		}
	default:
		return GovernanceDecision{}, fmt.Errorf("%w: unknown verdict", ErrMalformedDecision)
	}
	input, enforced, err := Digests(req.Binding, d.EnforcedPayload)
	if err != nil {
		return GovernanceDecision{}, fmt.Errorf("%w: invalid provider payload: %w", ErrMalformedDecision, err)
	}
	if d.InputDigest != input || d.EnforcedDigest != enforced {
		d.Verdict = VerdictDeny
		d.Reasons = []string{"digest_mismatch"}
		d.Approval = nil
		d.InputDigest, d.EnforcedDigest = input, enforced
		d.DigestMismatch = true
	}
	return d, nil
}
