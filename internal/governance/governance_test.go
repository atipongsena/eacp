package governance

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

type fixedProvider struct {
	decision GovernanceDecision
	err      error
}

func (p fixedProvider) Evaluate(context.Context, GovernanceRequest) (GovernanceDecision, error) {
	return p.decision, p.err
}

func TestEvaluateCheckedFailsClosedOnProviderErrorAndIncompleteEvidence(t *testing.T) {
	req := GovernanceRequest{Binding: testBinding(), PolicyBundleID: uuid.New(), PolicyVersion: 1}
	if d, err := EvaluateChecked(context.Background(), fixedProvider{err: errors.New("PDP down")}, req); err == nil || d.Verdict == VerdictAllow {
		t.Fatalf("PDP outage yielded executable decision %+v, %v", d, err)
	}
	if d, err := EvaluateChecked(context.Background(), fixedProvider{decision: GovernanceDecision{Verdict: VerdictAllow}}, req); err == nil || d.Verdict == VerdictAllow {
		t.Fatalf("incomplete evidence yielded executable decision %+v, %v", d, err)
	}
}

func TestEvaluateCheckedDeniesProviderDigestMismatch(t *testing.T) {
	req := GovernanceRequest{Binding: testBinding(), PolicyBundleID: uuid.New(), PolicyVersion: 1,
		Policy: []byte(`{"format_version":1,"rules":[{"id":"a","verdict":"allow","reason":"okay"}]}`)}
	d, err := (LocalProvider{InstanceID: "local-test"}).Evaluate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	d.EnforcedDigest[0] ^= 0xff
	checked, err := EvaluateChecked(context.Background(), fixedProvider{decision: d}, req)
	if err != nil || checked.Verdict != VerdictDeny || len(checked.Reasons) != 1 || checked.Reasons[0] != "digest_mismatch" {
		t.Fatalf("digest mismatch = %+v, err = %v", checked, err)
	}
}

func TestEvaluateCheckedRejectsOutOfRangeApprovalRequirement(t *testing.T) {
	req := GovernanceRequest{Binding: testBinding(), PolicyBundleID: uuid.New(), PolicyVersion: 1,
		Policy: []byte(`{"format_version":1,"rules":[{"id":"a","verdict":"escalate","reason":"review","approval":{"quorum":2,"eligible_roles":["approver"],"ttl_seconds":600}}]}`)}
	d, err := (LocalProvider{InstanceID: "local-test"}).Evaluate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	for _, approval := range []ApprovalRequirement{
		{Quorum: 6, EligibleRoles: []string{"approver"}, TTLSeconds: 600},
		{Quorum: 2, EligibleRoles: []string{"approver"}, TTLSeconds: 86401},
	} {
		d.Approval = &approval
		if got, err := EvaluateChecked(context.Background(), fixedProvider{decision: d}, req); err == nil {
			t.Fatalf("invalid approval %+v yielded %+v", approval, got)
		}
	}
}
