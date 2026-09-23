package api_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/api"
	"eacp/internal/identity"
	"eacp/internal/registry/registrytest"
	"eacp/internal/storage"
	"eacp/internal/storage/pgtest"
)

type harness struct {
	t    *testing.T
	f    *registrytest.Fixture
	srv  *httptest.Server
	logs *bytes.Buffer
	keys map[string]string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	f := registrytest.New(t)
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	mux := http.NewServeMux()
	api.New(f.App, log).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	h := &harness{t: t, f: f, srv: srv, logs: logs, keys: map[string]string{}}
	for _, who := range []string{"alice", "bob", "erin", "rita", "ravi", "otto", "audra", "carol", "amy", "ben", "cy"} {
		// Two admins: neither may approve their own proposal nor their own key.
		proposer, approver := "alice", "bob"
		if who == "bob" {
			proposer, approver = "bob", "alice"
		}
		h.keys[who] = h.issue(identity.KindPrincipal, f.P[who], proposer, approver)
	}
	return h
}

func TestPolicyAPICreatesAndActivatesWithTwoAdmins(t *testing.T) {
	h := newHarness(t)
	content := json.RawMessage(`{"format_version":1,"rules":[{"id":"read","match":{"operation":"read"},"verdict":"allow","reason":"permitted"}]}`)
	code, policy := h.as("alice", "POST", "/v1/policies", map[string]any{"content": content})
	h.want(201, code, policy)
	id := str(policy, "id")
	if id == "" || policy["version"] != float64(1) {
		t.Fatalf("policy = %v", policy)
	}
	code, body := h.as("alice", "POST", "/v1/policies/"+id+"/activate", map[string]any{"reason": "self"})
	h.want(403, code, body)
	code, body = h.as("bob", "POST", "/v1/policies/"+id+"/activate", map[string]any{"reason": "reviewed"})
	h.want(204, code, body)
	code, current := h.as("alice", "GET", "/v1/policies/current", nil)
	h.want(200, code, current)
	if str(current, "id") != id {
		t.Fatalf("current policy = %v", current)
	}
	code, body = h.as("amy", "POST", "/v1/policies", map[string]any{"content": content})
	h.want(403, code, body)
}

func TestApprovalAPIShowsEnforcedPayloadAndRecordsVotes(t *testing.T) {
	h := newHarness(t)
	tool := h.f.ActiveTool(t, "erp", "purchase")
	agent := h.f.ActiveAgent(t, "buyer", tool.Tool)
	content := json.RawMessage(`{"format_version":1,"rules":[{"id":"high","verdict":"escalate","reason":"high risk","approval":{"quorum":2,"eligible_roles":["approver"],"ttl_seconds":600}}]}`)
	code, policy := h.as("alice", "POST", "/v1/policies", map[string]any{"content": content})
	h.want(201, code, policy)
	policyID := uuid.MustParse(str(policy, "id"))
	code, body := h.as("bob", "POST", "/v1/policies/"+policyID.String()+"/activate", map[string]any{"reason": "reviewed"})
	h.want(204, code, body)
	actionID := uuid.New()
	evidence := h.f.ID(t, "carol", `INSERT INTO eacp.decision_evidence
		(tenant_id, action_id, policy_bundle_id, policy_version, provider, provider_instance_id,
		 decision_id, verdict, reasons, input_digest, enforced_digest, enforced_payload,
		 required_quorum, eligible_roles, approval_ttl_seconds, evaluated_at)
		VALUES (eacp.current_tenant_id(), $1, $2, 1, 'local', 'local-test', $3, 'escalate',
		 ARRAY['high risk'], decode(repeat('11', 32), 'hex'), decode(repeat('22', 32), 'hex'),
		 '{"amount":1000000,"currency":"THB"}'::jsonb, 2, ARRAY['approver'], 600, now())
		RETURNING id`, actionID, policyID, uuid.New())
	request := h.f.ID(t, "carol", `INSERT INTO eacp.approval_requests
		(tenant_id, action_id, agent_version_id, tool_id, requesting_subject_id,
		 decision_evidence_id, not_after, expires_at)
		VALUES (eacp.current_tenant_id(), $1, $2, $3, $4, $5,
		 now() + interval '1 hour', now() + interval '9 minutes') RETURNING id`,
		actionID, agent.Version, tool.Tool, h.f.P["carol"], evidence)
	path := "/v1/approvals/" + request.String()
	code, queue := h.as("amy", "GET", "/v1/approvals", nil)
	h.want(200, code, queue)
	items, _ := queue["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["id"] != request.String() {
		t.Fatalf("approval queue = %v", queue)
	}
	code, detail := h.as("amy", "GET", path, nil)
	h.want(200, code, detail)
	payload, _ := detail["enforced_payload"].(map[string]any)
	if payload["amount"] != float64(1000000) || str(detail, "enforced_digest") != strings.Repeat("22", 32) {
		t.Fatalf("approval detail = %v", detail)
	}
	code, body = h.as("carol", "GET", path, nil)
	h.want(403, code, body)
	code, result := h.as("amy", "POST", path+"/votes", map[string]any{"decision": "APPROVE", "reason": "reviewed"})
	h.want(200, code, result)
	if result["request_state"] != "PENDING" {
		t.Fatalf("first vote = %v", result)
	}
	code, result = h.as("ben", "POST", path+"/votes", map[string]any{"decision": "APPROVE", "reason": "reviewed"})
	h.want(200, code, result)
	if result["request_state"] != "GRANTED" {
		t.Fatalf("second vote = %v", result)
	}
}

// issue registers a holder-generated key via SQL (see ADR-003 §5).
func (h *harness) issue(kind identity.Kind, subject uuid.UUID, proposer, approver string) string {
	h.t.Helper()
	credID := uuid.New()
	key, hash, _ := identity.NewKey(kind, h.f.Tenant, credID)
	var principal, version any
	if kind == identity.KindPrincipal {
		principal = subject
	} else {
		version = subject
	}
	h.f.ID(h.t, proposer, `INSERT INTO eacp.credentials
		(tenant_id, id, kind, principal_id, agent_version_id, secret_hash, expires_at)
		VALUES (eacp.current_tenant_id(), $1, $2, $3, $4, $5, now() + interval '30 days') RETURNING id`,
		credID, string(kind), principal, version, hash)
	if err := h.f.Exec(approver, `UPDATE eacp.credentials SET approved_at = now() WHERE id = $1`, credID); err != nil {
		h.t.Fatalf("approve credential: %v", err)
	}
	return key
}

func (h *harness) do(key, method, path string, body any) (int, map[string]any) {
	h.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		buf, _ := json.Marshal(b)
		rd = bytes.NewReader(buf)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, rd)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(raw) > 0 && raw[0] == '{' {
		json.Unmarshal(raw, &out)
	} else if len(raw) > 0 {
		out = map[string]any{"_raw": string(raw)}
	}
	return resp.StatusCode, out
}

func (h *harness) as(who, method, path string, body any) (int, map[string]any) {
	return h.do(h.keys[who], method, path, body)
}

func (h *harness) want(code int, gotCode int, body map[string]any) {
	h.t.Helper()
	if gotCode != code {
		h.t.Fatalf("status %d (body %v), want %d", gotCode, body, code)
	}
}

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func TestUnauthenticatedRequestsGetIdenticalAnswers(t *testing.T) {
	h := newHarness(t)
	wrongSecret, _, _ := identity.NewKey(identity.KindPrincipal, h.f.Tenant, uuid.New())
	var bodies []string
	for _, key := range []string{"", "garbage", wrongSecret} {
		req, _ := http.NewRequest("GET", h.srv.URL+"/v1/agents", nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") == "" {
			t.Fatalf("key %q: status %d", key, resp.StatusCode)
		}
		bodies = append(bodies, string(b))
	}
	if bodies[0] != bodies[1] || bodies[1] != bodies[2] {
		t.Fatalf("401 bodies differ: %q", bodies)
	}
	if strings.Contains(h.logs.String(), wrongSecret) {
		t.Fatal("the presented key was logged")
	}
	if !strings.Contains(h.logs.String(), "unknown_credential") {
		t.Fatal("the failure reason was not logged")
	}
}

func TestRoleIsCheckedBeforeTheDatabase(t *testing.T) {
	h := newHarness(t)
	code, body := h.as("carol", "POST", "/v1/agents", map[string]any{
		"name": "x1", "display_name": "x", "environment": "production", "risk_class": "low",
		"owner_principal_id": h.f.P["carol"]})
	h.want(403, code, body)
}

func TestAgentKeysCannotUseOperatorRoutesAndViceVersa(t *testing.T) {
	h := newHarness(t)
	tool := h.f.ActiveTool(t, "erp", "read")
	a := h.f.ActiveAgent(t, "a1", tool.Tool)
	agentKey := h.issue(identity.KindAgent, a.Version, "erin", "rita")
	code, body := h.do(agentKey, "GET", "/v1/agents", nil)
	h.want(403, code, body)
	code, body = h.as("alice", "GET", "/v1/agent/self", nil)
	h.want(403, code, body)
}

func TestEndToEndRegistrationThroughTheAPI(t *testing.T) {
	h := newHarness(t)
	P := h.f.P

	code, conn := h.as("erin", "POST", "/v1/connectors", map[string]any{
		"name": "erp", "protocol": "http", "endpoint": "http://fakeerp:8090", "secret_ref": "erp"})
	h.want(201, code, conn)
	code, tool := h.as("erin", "POST", "/v1/connectors/"+str(conn, "id")+"/tools", map[string]any{"name": "create_po"})
	h.want(201, code, tool)
	code, contract := h.as("erin", "POST", "/v1/tools/"+str(tool, "id")+"/contracts", map[string]any{
		"side_effects": []string{"IRREVERSIBLE_WRITE", "FINANCIAL"}, "idempotency_mode": "native",
		"idempotency_key_field": "Idempotency-Key", "correlation_field": "external_reference",
		"reconciliation_lookup": "by_operation_key", "reconciliation_consistency": "strong",
		"proof_standard": "authoritative", "max_attempts": 3})
	h.want(201, code, contract)
	code, body := h.as("erin", "POST", "/v1/tools/"+str(tool, "id")+"/contract", map[string]any{"contract_id": str(contract, "id")})
	h.want(403, code, body) // editors cannot activate
	code, body = h.as("rita", "POST", "/v1/tools/"+str(tool, "id")+"/contract", map[string]any{"contract_id": str(contract, "id")})
	h.want(204, code, body)

	code, agent := h.as("erin", "POST", "/v1/agents", map[string]any{
		"name": "procurement-bot", "display_name": "Procurement bot", "environment": "production",
		"risk_class": "high", "owner_principal_id": P["carol"]})
	h.want(201, code, agent)
	code, version := h.as("erin", "POST", "/v1/agents/"+str(agent, "id")+"/versions", map[string]any{"runtime": "python", "code_ref": "git:abc"})
	h.want(201, code, version)
	vid := str(version, "id")
	code, al := h.as("erin", "POST", "/v1/agent-versions/"+vid+"/allowlists", map[string]any{"tools": []string{"erp.create_po"}})
	h.want(201, code, al)
	code, body = h.as("rita", "POST", "/v1/agent-versions/"+vid+"/allowlist", map[string]any{"allowlist_id": str(al, "id")})
	h.want(204, code, body)
	code, body = h.as("ravi", "POST", "/v1/agent-versions/"+vid+"/transitions", map[string]any{"to": "ACTIVE", "reason": "go live"})
	h.want(204, code, body)

	code, detail := h.as("carol", "GET", "/v1/agents/procurement-bot", nil)
	h.want(200, code, detail)
	versions, _ := detail["versions"].([]any)
	if len(versions) != 1 || versions[0].(map[string]any)["state"] != "ACTIVE" {
		t.Fatalf("detail = %v", detail)
	}

	// The agent runtime authenticates with a key bound to this version.
	agentKey := h.issue(identity.KindAgent, uuid.MustParse(vid), "erin", "rita")
	code, self := h.do(agentKey, "GET", "/v1/agent/self", nil)
	h.want(200, code, self)
	if self["state"] != "ACTIVE" || self["agent_version_id"] != vid {
		t.Fatalf("self = %v", self)
	}
	code, cap := h.do(agentKey, "POST", "/v1/agent/capability-check", map[string]any{"tool": "erp.create_po"})
	h.want(200, code, cap)
	if cap["allowed"] != true || cap["contract_id"] != str(contract, "id") {
		t.Fatalf("capability = %v", cap)
	}
	code, cap = h.do(agentKey, "POST", "/v1/agent/capability-check", map[string]any{"tool": "erp.delete_po"})
	h.want(200, code, cap)
	if cap["allowed"] != false || cap["denial"] != "unknown_tool" {
		t.Fatalf("capability = %v", cap)
	}

	// Containment: one operator, immediate effect.
	code, body = h.as("otto", "POST", "/v1/agent-versions/"+vid+"/transitions", map[string]any{"to": "SUSPENDED", "reason": "incident"})
	h.want(204, code, body)
	code, cap = h.do(agentKey, "POST", "/v1/agent/capability-check", map[string]any{"tool": "erp.create_po"})
	if code != 200 || cap["allowed"] != false || cap["denial"] != "agent_version_not_active" {
		t.Fatalf("after suspension: %d %v", code, cap)
	}

	// Every change is on the verified audit chain.
	code, v := h.as("audra", "GET", "/v1/audit/verify", nil)
	h.want(200, code, v)
	if v["count"].(float64) < 10 {
		t.Fatalf("audit verify = %v", v)
	}
	code, body = h.as("erin", "GET", "/v1/audit/verify", nil)
	h.want(403, code, body)
}

func TestErrorMapping(t *testing.T) {
	h := newHarness(t)
	agent := map[string]any{"name": "dup", "display_name": "d", "environment": "production", "risk_class": "low",
		"owner_principal_id": h.f.P["carol"]}
	code, body := h.as("erin", "POST", "/v1/agents", agent)
	h.want(201, code, body)
	code, body = h.as("erin", "POST", "/v1/agents", agent)
	h.want(409, code, body)
	code, body = h.as("erin", "POST", "/v1/agents", `{"name": "x", "surprise": true}`)
	h.want(400, code, body)
	code, body = h.as("erin", "POST", "/v1/agents", `{not json`)
	h.want(400, code, body)
	code, body = h.as("erin", "POST", "/v1/agents", map[string]any{"name": "no-owner", "display_name": "d",
		"environment": "production", "risk_class": "low"})
	h.want(400, code, body)
	code, body = h.as("erin", "GET", "/v1/agents/"+uuid.NewString(), nil)
	h.want(404, code, body)
	code, body = h.as("erin", "POST", "/v1/agents/not-a-uuid/versions", map[string]any{"runtime": "go", "code_ref": "x"})
	h.want(400, code, body)
}

func TestOtherTenantsResourcesAreNotFound(t *testing.T) {
	h := newHarness(t)
	tool := h.f.ActiveTool(t, "erp", "read")
	a := h.f.ActiveAgent(t, "a1", tool.Tool)

	// Bootstrap a reader in tenant B via the owner path and give it a key.
	ctx := context.Background()
	tenantB := uuid.MustParse(pgtest.TenantB)
	credID := uuid.New()
	key, hash, _ := identity.NewKey(identity.KindPrincipal, tenantB, credID)
	err := storage.InTenantTx(ctx, h.f.Owner, pgtest.TenantB, func(tx pgx.Tx) error {
		var p uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO eacp.principals (tenant_id, kind, name, subject, display_name)
			VALUES (eacp.current_tenant_id(), 'human', 'eve', 'eve@b.test', 'Eve') RETURNING id`).Scan(&p); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO eacp.credentials (tenant_id, id, kind, principal_id, secret_hash, expires_at, approved_at)
			VALUES (eacp.current_tenant_id(), $1, 'pk', $2, $3, now() + interval '1 day', now())`, credID, p, hash)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	code, body := h.do(key, "GET", "/v1/agents/"+a.Agent.String(), nil)
	h.want(404, code, body)
	code, list := h.do(key, "GET", "/v1/agents", nil)
	h.want(200, code, list)
	if items, _ := list["agents"].([]any); len(items) != 0 {
		t.Fatalf("tenant B lists %d agents", len(items))
	}
}

func TestPrincipalManagementThroughTheAPI(t *testing.T) {
	h := newHarness(t)
	code, p := h.as("alice", "POST", "/v1/principals", map[string]any{"kind": "human", "name": "dave",
		"subject": "dave@tenant-a.test", "display_name": "Dave"})
	h.want(201, code, p)
	code, g := h.as("alice", "POST", "/v1/role-grants", map[string]any{"principal_id": str(p, "id"), "role": "auditor"})
	h.want(201, code, g)
	code, body := h.as("alice", "POST", "/v1/role-grants/"+str(g, "id")+"/approve", nil)
	h.want(403, code, body)
	code, body = h.as("bob", "POST", "/v1/role-grants/"+str(g, "id")+"/approve", nil)
	h.want(204, code, body)

	// Bring your own key: dave generates it, alice registers the hash, bob approves.
	credID := uuid.New()
	key, hash, _ := identity.NewKey(identity.KindPrincipal, h.f.Tenant, credID)
	code, body = h.as("alice", "POST", "/v1/credentials", map[string]any{"id": credID, "kind": "pk",
		"principal_id": str(p, "id"), "hash": hex.EncodeToString(hash), "expires_in_days": 30})
	h.want(201, code, body)
	code, body = h.as("bob", "POST", "/v1/credentials/"+credID.String()+"/approve", nil)
	h.want(204, code, body)
	code, me := h.do(key, "GET", "/v1/me", nil)
	h.want(200, code, me)
	if me["principal_id"] != str(p, "id") {
		t.Fatalf("me = %v", me)
	}
}
