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
}

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
		return GovernanceDecision{}, fmt.Errorf("governance: provider unavailable: %w", err)
	}
	if d.PolicyBundleID != req.PolicyBundleID || d.PolicyVersion != req.PolicyVersion ||
		d.PolicyBundleID == uuid.Nil || d.PolicyVersion < 1 || d.Provider == "" ||
		d.ProviderInstanceID == "" || d.DecisionID == uuid.Nil || d.EvaluatedAt.IsZero() ||
		len(d.Reasons) == 0 || len(d.EnforcedPayload) == 0 {
		return GovernanceDecision{}, errors.New("governance: incomplete provider decision")
	}
	switch d.Verdict {
	case VerdictAllow, VerdictWarn, VerdictDeny, VerdictTransform:
		if d.Approval != nil {
			return GovernanceDecision{}, errors.New("governance: unexpected approval requirement")
		}
	case VerdictEscalate:
		if d.Approval == nil || d.Approval.Quorum < 1 || d.Approval.Quorum > 5 ||
			d.Approval.TTLSeconds < 1 || d.Approval.TTLSeconds > 86400 ||
			len(d.Approval.EligibleRoles) != 1 || d.Approval.EligibleRoles[0] != "approver" {
			return GovernanceDecision{}, errors.New("governance: incomplete approval requirement")
		}
	default:
		return GovernanceDecision{}, errors.New("governance: unknown verdict")
	}
	input, enforced, err := Digests(req.Binding, d.EnforcedPayload)
	if err != nil {
		return GovernanceDecision{}, fmt.Errorf("governance: invalid provider payload: %w", err)
	}
	if d.InputDigest != input || d.EnforcedDigest != enforced {
		d.Verdict = VerdictDeny
		d.Reasons = []string{"digest_mismatch"}
		d.Approval = nil
		d.InputDigest, d.EnforcedDigest = input, enforced
	}
	return d, nil
}
