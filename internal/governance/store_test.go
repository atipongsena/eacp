package governance_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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
	tool := f.ActiveTool(t, "erp", "purchase")
	agent := f.ActiveAgent(t, "buyer", tool.Tool)
	b := governance.Binding{TenantID: f.Tenant, AgentID: agent.Agent, AgentVersionID: agent.Version,
		Subject: "carol@tenant-a.test", Operation: "purchase", Target: "erp", Tool: "erp.purchase",
		ToolSchemaVersion: "1", Resource: "orders", Payload: json.RawMessage(`{"amount":2400000}`)}
	d, err := governance.EvaluateChecked(context.Background(), governance.LocalProvider{InstanceID: "local-test"},
		governance.GovernanceRequest{Binding: b, RiskClass: "high",
			PolicyBundleID: p.ID, PolicyVersion: p.Version, Policy: p.Content})
	if err != nil || d.Verdict != governance.VerdictEscalate {
		t.Fatalf("decision = %+v, err = %v", d, err)
	}
	actionID := f.AgentID(t, agent.Version, `INSERT INTO eacp.actions
		(tenant_id, agent_version_id, idempotency_key, subject, operation, target, tool,
		 tool_schema_version, resource, input_payload, input_digest, not_after)
		VALUES (eacp.current_tenant_id(), $1, 'k1', 'carol@tenant-a.test', 'purchase', 'erp',
		 'erp.purchase', '1', 'orders', '{"amount":2400000}', $2, now() + interval '1 hour')
		RETURNING id`, agent.Version, d.InputDigest[:])
	var evidenceID uuid.UUID
	err = storage.InTenantTx(context.Background(), f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		if err := storage.SetAgent(context.Background(), tx, agent.Version); err != nil {
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

// ADR-002 §8: an AGT decision persists its provider evidence. The column
// accepts only a JSON object; Go bounds it to 4 KiB canonical, and raw SQL
// is held to 8 KiB of PostgreSQL's (spaced) rendering.
func TestRecordDecisionPersistsBoundedProviderEvidence(t *testing.T) {
	f := registrytest.New(t)
	s := governance.NewStore(f.App)
	alice := registry.Actor{TenantID: f.Tenant, PrincipalID: f.P["alice"]}
	p, err := s.CreatePolicy(context.Background(), alice, json.RawMessage(simplePolicy))
	if err != nil {
		t.Fatal(err)
	}
	tool := f.ActiveTool(t, "erp", "purchase")
	agent := f.ActiveAgent(t, "buyer", tool.Tool)
	b := governance.Binding{TenantID: f.Tenant, AgentID: agent.Agent, AgentVersionID: agent.Version,
		Subject: "carol@tenant-a.test", Operation: "purchase", Target: "erp", Tool: "erp.purchase",
		ToolSchemaVersion: "1", Resource: "orders", Payload: json.RawMessage(`{"amount":1}`)}
	d, err := governance.EvaluateChecked(context.Background(), governance.LocalProvider{InstanceID: "local-test"},
		governance.GovernanceRequest{Binding: b, RiskClass: "high", PolicyBundleID: p.ID, PolicyVersion: p.Version, Policy: p.Content})
	if err != nil {
		t.Fatal(err)
	}
	actionID := f.AgentID(t, agent.Version, `INSERT INTO eacp.actions
		(tenant_id, agent_version_id, idempotency_key, subject, operation, target, tool,
		 tool_schema_version, resource, input_payload, input_digest, not_after)
		VALUES (eacp.current_tenant_id(), $1, 'k1', 'carol@tenant-a.test', 'purchase', 'erp',
		 'erp.purchase', '1', 'orders', '{"amount":1}', $2, now() + interval '1 hour')
		RETURNING id`, agent.Version, d.InputDigest[:])
	inTx := func(fn func(tx pgx.Tx) error) error {
		return storage.InTenantTx(context.Background(), f.App, pgtest.TenantA, func(tx pgx.Tx) error {
			if err := storage.SetAgent(context.Background(), tx, agent.Version); err != nil {
				return err
			}
			return fn(tx)
		})
	}
	for _, bad := range []string{`[]`, `"x"`, `{"a":"` + strings.Repeat("x", 8200) + `"}`} {
		err := inTx(func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), `INSERT INTO eacp.decision_evidence
				(tenant_id, action_id, policy_bundle_id, policy_version, provider, provider_instance_id,
				 decision_id, verdict, reasons, input_digest, enforced_digest, enforced_payload,
				 evaluated_at, provider_evidence)
				VALUES (eacp.current_tenant_id(), $1, $2, $3, 'microsoft-agt', 'pdp-1', gen_random_uuid(), 'allow',
				 ARRAY['ok'], $4, $4, '{}', now(), $5::jsonb)`, actionID, p.ID, p.Version, d.InputDigest[:], bad)
			return err
		})
		sqlState(t, err, "23514")
	}
	d.Provider = "microsoft-agt"
	d.ProviderEvidence = json.RawMessage(`{"acs_action_identity":"sha256:ab","rule_id":"a"}`)
	var evidenceID uuid.UUID
	if err := inTx(func(tx pgx.Tx) error {
		var err error
		evidenceID, err = governance.RecordDecision(context.Background(), tx, actionID, d)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var stored *string
	if err := inTx(func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT provider_evidence::text FROM eacp.decision_evidence
			WHERE id = $1`, evidenceID).Scan(&stored)
	}); err != nil || stored == nil || *stored != `{"rule_id": "a", "acs_action_identity": "sha256:ab"}` {
		t.Fatalf("stored provider evidence = %v, err = %v", stored, err)
	}
}
