package governance_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/governance"
	"eacp/internal/registry"
	"eacp/internal/registry/registrytest"
	"eacp/internal/storage"
	"eacp/internal/storage/pgtest"
)

func TestPolicyStoreCreatesAndActivatesVersion(t *testing.T) {
	f := registrytest.New(t)
	s := governance.NewStore(f.App)
	alice := registry.Actor{TenantID: f.Tenant, PrincipalID: f.P["alice"]}
	bob := registry.Actor{TenantID: f.Tenant, PrincipalID: f.P["bob"]}
	if _, err := s.CreatePolicy(context.Background(), alice, json.RawMessage(`{"rules":[]}`)); !errors.Is(err, registry.ErrInvalid) {
		t.Fatalf("invalid policy = %v, want ErrInvalid", err)
	}
	p, err := s.CreatePolicy(context.Background(), alice, json.RawMessage(simplePolicy))
	if err != nil || p.ID == uuid.Nil || p.Version != 1 {
		t.Fatalf("created policy = %+v, err = %v", p, err)
	}
	if err := s.ActivatePolicy(context.Background(), alice, p.ID, "self activation"); !errors.Is(err, registry.ErrForbidden) {
		t.Fatalf("self activation = %v, want ErrForbidden", err)
	}
	if err := s.ActivatePolicy(context.Background(), bob, p.ID, "reviewed"); err != nil {
		t.Fatal(err)
	}
	current, err := s.CurrentPolicy(context.Background(), f.Tenant)
	if err != nil || current.ID != p.ID || current.Version != 1 {
		t.Fatalf("current policy = %+v, err = %v", current, err)
	}
}

func TestRecordDecisionPersistsEnforcedPayloadAndBothDigests(t *testing.T) {
	f := registrytest.New(t)
	s := governance.NewStore(f.App)
	alice := registry.Actor{TenantID: f.Tenant, PrincipalID: f.P["alice"]}
	bob := registry.Actor{TenantID: f.Tenant, PrincipalID: f.P["bob"]}
	policy := json.RawMessage(`{"format_version":1,"rules":[{"id":"cap","match":{"risk_class":"high"},"verdict":"escalate","reason":"review capped amount","set":{"amount":1000000},"approval":{"quorum":2,"eligible_roles":["approver"],"ttl_seconds":600}}]}`)
	p, err := s.CreatePolicy(context.Background(), alice, policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ActivatePolicy(context.Background(), bob, p.ID, "reviewed"); err != nil {
		t.Fatal(err)
	}
	b := governance.Binding{TenantID: f.Tenant, AgentID: uuid.New(), AgentVersionID: uuid.New(),
		Subject: "carol@tenant-a.test", Operation: "purchase", Target: "erp", Tool: "erp.purchase",
		ToolSchemaVersion: "1", Resource: "orders", Payload: json.RawMessage(`{"amount":2400000}`)}
	d, err := governance.EvaluateChecked(context.Background(), governance.LocalProvider{InstanceID: "local-test"},
		governance.GovernanceRequest{Binding: b, RiskClass: "high",
			PolicyBundleID: p.ID, PolicyVersion: p.Version, Policy: p.Content})
	if err != nil || d.Verdict != governance.VerdictEscalate {
		t.Fatalf("decision = %+v, err = %v", d, err)
	}
	actionID := uuid.New()
	var evidenceID uuid.UUID
	err = storage.InTenantTx(context.Background(), f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		if err := storage.SetActor(context.Background(), tx, f.P["carol"]); err != nil {
			return err
		}
		var err error
		evidenceID, err = governance.RecordDecision(context.Background(), tx, actionID, d)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var payload string
	var input, enforced []byte
	err = storage.InTenantTx(context.Background(), f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT enforced_payload::text, input_digest, enforced_digest
			FROM eacp.decision_evidence WHERE id = $1`, evidenceID).Scan(&payload, &input, &enforced)
	})
	if err != nil || payload != `{"amount": 1000000}` || string(input) != string(d.InputDigest[:]) ||
		string(enforced) != string(d.EnforcedDigest[:]) {
		t.Fatalf("evidence payload = %s, digests = %x/%x, err = %v", payload, input, enforced, err)
	}
}
