// Package conformance loads the ADR-002 §7/§8 reference policy set and
// checks a provider's decision against it. Every governance provider (local,
// the AGT sidecar) must produce the expected outcome for every case; the
// sidecar's image build runs the same file through ACS.
package conformance

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/governance"
)

// File is the reference set. Field order is the on-disk order.
type File struct {
	Comment         string            `json:"comment"`
	Binding         json.RawMessage   `json:"binding"`
	RiskClass       string            `json:"risk_class"`
	SideEffectClass string            `json:"side_effect_class"`
	Policies        map[string]Policy `json:"policies"`
	Cases           []Case            `json:"cases"`
}

type Policy struct {
	ID      uuid.UUID       `json:"id"`
	Version int             `json:"version"`
	Content json.RawMessage `json:"content"`
}

type Case struct {
	Name            string          `json:"name"`
	Policy          string          `json:"policy"`
	Binding         json.RawMessage `json:"binding,omitempty"`
	RiskClass       string          `json:"risk_class,omitempty"`
	SideEffectClass string          `json:"side_effect_class,omitempty"`
	Expect          Expect          `json:"expect"`
	// AGTExpect overrides Expect for the AGT provider: a documented
	// divergence that must fail closed (ADR-002 §8).
	AGTExpect     *Expect `json:"agt_expect,omitempty"`
	AGTDivergence string  `json:"agt_divergence,omitempty"`
}

// Expect is either an error class or a complete decision.
type Expect struct {
	Error           string                          `json:"error,omitempty"`
	Verdict         governance.Verdict              `json:"verdict,omitempty"`
	Reasons         []string                        `json:"reasons,omitempty"`
	EnforcedPayload json.RawMessage                 `json:"enforced_payload,omitempty"`
	Approval        *governance.ApprovalRequirement `json:"approval,omitempty"`
	InputDigest     string                          `json:"input_digest,omitempty"`
	EnforcedDigest  string                          `json:"enforced_digest,omitempty"`
}

func Load(path string) (*File, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var f File
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("conformance: %s: %w", path, err)
	}
	if len(f.Cases) == 0 {
		return nil, errors.New("conformance: no cases")
	}
	return &f, nil
}

// Request builds the governance request of a case: the default binding with
// the case's fields overlaid.
func (f *File) Request(c Case) (governance.GovernanceRequest, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(f.Binding, &fields); err != nil {
		return governance.GovernanceRequest{}, err
	}
	if len(c.Binding) != 0 {
		var over map[string]json.RawMessage
		if err := json.Unmarshal(c.Binding, &over); err != nil {
			return governance.GovernanceRequest{}, err
		}
		for k, v := range over {
			fields[k] = v
		}
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return governance.GovernanceRequest{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var b governance.Binding
	if err := dec.Decode(&b); err != nil {
		return governance.GovernanceRequest{}, fmt.Errorf("conformance: case %s binding: %w", c.Name, err)
	}
	p, ok := f.Policies[c.Policy]
	if !ok {
		return governance.GovernanceRequest{}, fmt.Errorf("conformance: case %s: unknown policy %q", c.Name, c.Policy)
	}
	req := governance.GovernanceRequest{Binding: b, RiskClass: f.RiskClass, SideEffectClass: f.SideEffectClass,
		PolicyBundleID: p.ID, PolicyVersion: p.Version, Policy: p.Content}
	if c.RiskClass != "" {
		req.RiskClass = c.RiskClass
	}
	if c.SideEffectClass != "" {
		req.SideEffectClass = c.SideEffectClass
	}
	return req, nil
}

// Check compares a decision from governance.EvaluateChecked with the
// expected outcome. An expected error of any class requires a transient
// failure: nothing may become executable and nothing may be denied.
func Check(e Expect, d governance.GovernanceDecision, err error) error {
	if e.Error != "" {
		if err == nil {
			return fmt.Errorf("want error %q, got verdict %s %v", e.Error, d.Verdict, d.Reasons)
		}
		if !errors.Is(err, governance.ErrProviderUnavailable) && !errors.Is(err, governance.ErrMalformedDecision) {
			return fmt.Errorf("want a transient failure, got %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("unexpected error: %w", err)
	}
	if d.DigestMismatch {
		return errors.New("provider digests differ from EACP's")
	}
	if d.Verdict != e.Verdict || !slices.Equal(d.Reasons, e.Reasons) {
		return fmt.Errorf("verdict = %s %q, want %s %q", d.Verdict, d.Reasons, e.Verdict, e.Reasons)
	}
	got, err := governance.Canonicalize(d.EnforcedPayload)
	if err != nil {
		return err
	}
	want, err := governance.Canonicalize(e.EnforcedPayload)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("enforced payload = %s, want %s", got, want)
	}
	if (d.Approval == nil) != (e.Approval == nil) ||
		(d.Approval != nil && (d.Approval.Quorum != e.Approval.Quorum || d.Approval.TTLSeconds != e.Approval.TTLSeconds ||
			!slices.Equal(d.Approval.EligibleRoles, e.Approval.EligibleRoles))) {
		return fmt.Errorf("approval = %+v, want %+v", d.Approval, e.Approval)
	}
	if hex.EncodeToString(d.InputDigest[:]) != e.InputDigest || hex.EncodeToString(d.EnforcedDigest[:]) != e.EnforcedDigest {
		return fmt.Errorf("digests = %x / %x, want %s / %s", d.InputDigest, d.EnforcedDigest, e.InputDigest, e.EnforcedDigest)
	}
	return nil
}
