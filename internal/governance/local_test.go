package governance

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func testBinding() Binding {
	return Binding{
		TenantID:       uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		AgentID:        uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		AgentVersionID: uuid.MustParse("33333333-3333-3333-3333-333333333333"),
		Subject:        "buyer@example.test", Operation: "purchase", Target: "erp",
		Tool: "erp.create_order", ToolSchemaVersion: "1", Resource: "orders",
		Payload: json.RawMessage(`{"amount":2400000,"currency":"THB"}`),
	}
}

func TestLocalProviderReturnsEveryVerdict(t *testing.T) {
	for _, verdict := range []string{"allow", "warn", "deny", "escalate", "transform"} {
		t.Run(verdict, func(t *testing.T) {
			rule := `{"id":"r1","match":{"operation":"purchase"},"verdict":"` + verdict + `","reason":"policy r1"`
			if verdict == "escalate" {
				rule += `,"approval":{"quorum":2,"eligible_roles":["approver"],"ttl_seconds":600}`
			}
			if verdict == "transform" {
				rule += `,"set":{"amount":1000000}`
			}
			rule += `}`
			req := GovernanceRequest{Binding: testBinding(), RiskClass: "high",
				PolicyBundleID: uuid.MustParse("44444444-4444-4444-4444-444444444444"), PolicyVersion: 1,
				Policy: json.RawMessage(`{"format_version":1,"rules":[` + rule + `]}`)}
			d, err := (LocalProvider{InstanceID: "local-test"}).Evaluate(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if string(d.Verdict) != verdict || d.PolicyVersion != 1 || d.ProviderInstanceID != "local-test" || d.DecisionID == uuid.Nil {
				t.Fatalf("incomplete decision: %+v", d)
			}
			if len(d.Reasons) != 1 || d.Reasons[0] != "policy r1" {
				t.Fatalf("reasons = %v", d.Reasons)
			}
			if verdict == "escalate" && (d.Approval == nil || d.Approval.Quorum != 2) {
				t.Fatalf("approval = %+v", d.Approval)
			}
			if verdict == "transform" && d.InputDigest == d.EnforcedDigest {
				t.Fatal("transform did not change the enforced digest")
			}
		})
	}
}

func TestLocalProviderTransformsBeforeApproval(t *testing.T) {
	req := GovernanceRequest{Binding: testBinding(), RiskClass: "high",
		PolicyBundleID: uuid.MustParse("44444444-4444-4444-4444-444444444444"), PolicyVersion: 1,
		Policy: json.RawMessage(`{"format_version":1,"rules":[{"id":"cap","match":{"risk_class":"high"},"verdict":"escalate","reason":"capped purchase needs approval","set":{"amount":1000000},"approval":{"quorum":2,"eligible_roles":["approver"],"ttl_seconds":600}}]}`)}
	d, err := (LocalProvider{InstanceID: "local-test"}).Evaluate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if d.Verdict != VerdictEscalate || string(d.EnforcedPayload) != `{"amount":1000000,"currency":"THB"}` {
		t.Fatalf("decision = %+v", d)
	}
	in, enforced, err := Digests(req.Binding, d.EnforcedPayload)
	if err != nil || d.InputDigest != in || d.EnforcedDigest != enforced || in == enforced {
		t.Fatalf("decision digest does not bind the enforced payload: %v", err)
	}
}

func TestLocalProviderFirstMatchAndDefaultDeny(t *testing.T) {
	req := GovernanceRequest{Binding: testBinding(),
		PolicyBundleID: uuid.MustParse("44444444-4444-4444-4444-444444444444"), PolicyVersion: 1,
		Policy: json.RawMessage(`{"format_version":1,"rules":[{"id":"first","match":{"operation":"purchase"},"verdict":"warn","reason":"first"},{"id":"second","match":{"operation":"purchase"},"verdict":"allow","reason":"second"}]}`)}
	d, err := (LocalProvider{InstanceID: "local-test"}).Evaluate(context.Background(), req)
	if err != nil || d.Verdict != VerdictWarn || d.Reasons[0] != "first" {
		t.Fatalf("first matching rule: %+v, %v", d, err)
	}
	req.Binding.Operation = "unknown"
	d, err = (LocalProvider{InstanceID: "local-test"}).Evaluate(context.Background(), req)
	if err != nil || d.Verdict != VerdictDeny || d.Reasons[0] != "no_matching_rule" {
		t.Fatalf("unmatched rule must deny: %+v, %v", d, err)
	}
}

func TestLocalProviderRejectsMalformedPolicy(t *testing.T) {
	for _, policy := range []string{
		`{"format_version":1,"rules":[{"id":"a","verdict":"allow","reason":"x"},{"id":"a","verdict":"deny","reason":"x"}]}`,
		`{"format_version":1,"rules":[{"id":"x","verdict":"escalate","reason":"x"}]}`,
		`{"format_version":1,"rules":[{"id":"x","verdict":"transform","reason":"x"}]}`,
		`{"format_version":1,"rules":[{"id":"x","verdict":"allow","reason":"x","set":{"amount":1}}]}`,
		`{"format_version":1,"rules":[{"id":"x","verdict":"allow","reason":"x","unknown":true}]}`,
		`{"format_version":1,"format_version":1,"rules":[]}`,
		`{"format_version":1,"rules":[{"id":"x","verdict":"transform","reason":"x","set":{"amount":1e21}}]}`,
	} {
		req := GovernanceRequest{Binding: testBinding(),
			PolicyBundleID: uuid.MustParse("44444444-4444-4444-4444-444444444444"), PolicyVersion: 1,
			Policy: json.RawMessage(policy)}
		if d, err := (LocalProvider{InstanceID: "local-test"}).Evaluate(context.Background(), req); err == nil {
			t.Errorf("policy %s yielded executable decision %+v", policy, d)
		}
	}
}
