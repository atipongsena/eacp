package bundle

import (
	"bytes"
	"encoding/json"

	"github.com/atipongsena/eacp/internal/governance"
)

// policy plans the tenant policy: a new version when the declared content
// differs from the active one (compared canonically), activated by the
// approver. One bundle per tenant owns it.
func (p *planner) policy() {
	if p.doc.Policy == nil {
		return
	}
	if other := p.st.AddressElsewhere[PolicyAddress]; other != "" {
		p.find(PolicyAddress, KindUnmanaged, "the tenant policy is managed by bundle %s", other)
		return
	}
	cur := p.st.Policy
	if cur.ID != nil {
		if _, managed := p.managed[PolicyAddress]; !managed {
			p.find(PolicyAddress, KindUnmanaged, "policy version %d (%s) is active and not managed by this bundle: add an import",
				cur.Version, *cur.ID)
			return
		}
		p.ref("policy", *cur.ID)
		if samePolicy(cur.Content, p.doc.Policy.Content) {
			return
		}
	} else {
		p.ref("policy", p.st.TenantID)
	}
	n := p.add(StageSubmit, PolicyAddress, OpCreate, Payload{Policy: p.doc.Policy.Content})
	p.add(StageApprove, PolicyAddress, OpActivate, Payload{ProposalStep: n, Reason: p.reason})
}

// samePolicy compares two policies as canonical JSON.
func samePolicy(a, b json.RawMessage) bool {
	ca, errA := governance.Canonicalize(a)
	cb, errB := governance.Canonicalize(b)
	return errA == nil && errB == nil && bytes.Equal(ca, cb)
}
