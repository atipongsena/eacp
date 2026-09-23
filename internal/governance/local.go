package governance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
)

// LocalProvider evaluates a versioned, ordered JSON rule bundle. It has no
// database connection or mutable state and cannot issue approval grants.
type LocalProvider struct{ InstanceID string }

type localBundle struct {
	FormatVersion int         `json:"format_version"`
	Rules         []localRule `json:"rules"`
}

type localRule struct {
	ID       string                     `json:"id"`
	Match    localMatch                 `json:"match"`
	Verdict  Verdict                    `json:"verdict"`
	Reason   string                     `json:"reason"`
	Set      map[string]json.RawMessage `json:"set,omitempty"`
	Approval *ApprovalRequirement       `json:"approval,omitempty"`
}

type localMatch struct {
	Subject         string `json:"subject,omitempty"`
	Operation       string `json:"operation,omitempty"`
	Target          string `json:"target,omitempty"`
	Tool            string `json:"tool,omitempty"`
	RiskClass       string `json:"risk_class,omitempty"`
	SideEffectClass string `json:"side_effect_class,omitempty"`
}

// ValidatePolicy rejects a bundle that might yield an incomplete decision.
// The same validation runs before policy insertion and during evaluation.
func ValidatePolicy(raw json.RawMessage) error {
	_, err := parseBundle(raw)
	return err
}

func parseBundle(raw json.RawMessage) (localBundle, error) {
	var bundle localBundle
	canonical, err := canonicalize(raw)
	if err != nil {
		return bundle, fmt.Errorf("governance: invalid policy JSON: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(canonical))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&bundle); err != nil {
		return bundle, fmt.Errorf("governance: invalid policy: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return bundle, errors.New("governance: trailing policy data")
	}
	if bundle.FormatVersion != 1 || len(bundle.Rules) == 0 {
		return bundle, errors.New("governance: unsupported or empty policy bundle")
	}
	seen := make(map[string]bool, len(bundle.Rules))
	for _, r := range bundle.Rules {
		if r.ID == "" || seen[r.ID] || strings.TrimSpace(r.Reason) == "" {
			return bundle, errors.New("governance: duplicate rule or missing id/reason")
		}
		seen[r.ID] = true
		switch r.Verdict {
		case VerdictAllow, VerdictWarn, VerdictDeny:
			if len(r.Set) != 0 || r.Approval != nil {
				return bundle, errors.New("governance: verdict cannot transform or require approval")
			}
		case VerdictTransform:
			if len(r.Set) == 0 || r.Approval != nil {
				return bundle, errors.New("governance: transform requires replacements only")
			}
		case VerdictEscalate:
			if r.Approval == nil || r.Approval.Quorum < 1 || r.Approval.Quorum > 5 ||
				r.Approval.TTLSeconds < 1 || r.Approval.TTLSeconds > 86400 ||
				len(r.Approval.EligibleRoles) != 1 || r.Approval.EligibleRoles[0] != "approver" {
				return bundle, errors.New("governance: invalid approval requirement")
			}
		default:
			return bundle, errors.New("governance: unknown verdict")
		}
		for k, v := range r.Set {
			if k == "" {
				return bundle, errors.New("governance: blank replacement key")
			}
			if _, err := canonicalize(v); err != nil {
				return bundle, fmt.Errorf("governance: replacement %q: %w", k, err)
			}
			var value any
			if err := json.Unmarshal(v, &value); err != nil || !policyValueSafe(value) {
				return bundle, fmt.Errorf("governance: replacement %q exceeds the interoperable numeric range", k)
			}
		}
	}
	return bundle, nil
}

// PostgreSQL jsonb expands exponent notation when rendering a stored policy.
// Keep replacement numbers within the I-JSON interoperable range so a policy
// validated before insertion remains valid when loaded for evaluation.
func policyValueSafe(v any) bool {
	switch x := v.(type) {
	case float64:
		return !math.IsInf(x, 0) && !math.IsNaN(x) && math.Abs(x) <= 9007199254740992
	case []any:
		for _, item := range x {
			if !policyValueSafe(item) {
				return false
			}
		}
	case map[string]any:
		for _, item := range x {
			if !policyValueSafe(item) {
				return false
			}
		}
	}
	return true
}

func (p LocalProvider) Evaluate(ctx context.Context, req GovernanceRequest) (GovernanceDecision, error) {
	if err := ctx.Err(); err != nil {
		return GovernanceDecision{}, err
	}
	if p.InstanceID == "" || req.PolicyBundleID == uuid.Nil || req.PolicyVersion < 1 {
		return GovernanceDecision{}, errors.New("governance: incomplete provider or policy identity")
	}
	bundle, err := parseBundle(req.Policy)
	if err != nil {
		return GovernanceDecision{}, err
	}
	enforced, err := canonicalize(req.Binding.Payload)
	if err != nil {
		return GovernanceDecision{}, fmt.Errorf("governance: submitted payload: %w", err)
	}
	verdict, reason := VerdictDeny, "no_matching_rule"
	var approval *ApprovalRequirement
	for _, rule := range bundle.Rules {
		if !rule.Match.matches(req) {
			continue
		}
		verdict, reason, approval = rule.Verdict, rule.Reason, rule.Approval
		if len(rule.Set) != 0 {
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(enforced, &payload); err != nil || payload == nil {
				return GovernanceDecision{}, errors.New("governance: replacements require an object payload")
			}
			for k, v := range rule.Set {
				payload[k] = v
			}
			raw, err := json.Marshal(payload)
			if err != nil {
				return GovernanceDecision{}, err
			}
			enforced, err = canonicalize(raw)
			if err != nil {
				return GovernanceDecision{}, err
			}
		}
		break
	}
	inputDigest, enforcedDigest, err := Digests(req.Binding, enforced)
	if err != nil {
		return GovernanceDecision{}, err
	}
	return GovernanceDecision{
		Verdict: verdict, EnforcedPayload: enforced,
		InputDigest: inputDigest, EnforcedDigest: enforcedDigest,
		PolicyBundleID: req.PolicyBundleID, PolicyVersion: req.PolicyVersion,
		Provider: "local", ProviderInstanceID: p.InstanceID,
		DecisionID: uuid.New(), Reasons: []string{reason}, Approval: approval,
		EvaluatedAt: time.Now().UTC(),
	}, nil
}

func (m localMatch) matches(req GovernanceRequest) bool {
	return (m.Subject == "" || m.Subject == req.Binding.Subject) &&
		(m.Operation == "" || m.Operation == req.Binding.Operation) &&
		(m.Target == "" || m.Target == req.Binding.Target) &&
		(m.Tool == "" || m.Tool == req.Binding.Tool) &&
		(m.RiskClass == "" || m.RiskClass == req.RiskClass) &&
		(m.SideEffectClass == "" || m.SideEffectClass == req.SideEffectClass)
}
