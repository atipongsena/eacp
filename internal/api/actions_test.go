package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/action"
	"github.com/atipongsena/eacp/internal/api"
	"github.com/atipongsena/eacp/internal/governance"
	"github.com/atipongsena/eacp/internal/identity"
	"github.com/atipongsena/eacp/internal/registry/registrytest"
	"github.com/atipongsena/eacp/internal/worker"
)

// switchPDP is the local provider, or an outage when down is set.
type switchPDP struct {
	down  atomic.Bool
	calls atomic.Int64
}

func (p *switchPDP) Evaluate(ctx context.Context, req governance.GovernanceRequest) (governance.GovernanceDecision, error) {
	p.calls.Add(1)
	if p.down.Load() {
		return governance.GovernanceDecision{}, errors.New("pdp unreachable")
	}
	return governance.LocalProvider{InstanceID: "api-test"}.Evaluate(ctx, req)
}

type actionHarness struct {
	*harness
	pdp     *switchPDP
	agent   registrytest.Agent
	key     string // the agent's API key
	actions *action.Engine
}

func newActionHarness(t *testing.T, policy string, limits action.Limits) *actionHarness {
	t.Helper()
	pdp := &switchPDP{}
	var engine *action.Engine
	h := newHarness(t, func(f *registrytest.Fixture, s *api.Server) {
		engine = action.New(f.App, action.Options{Provider: pdp, Limits: limits})
		s.WithActions(engine)
	})
	tool := h.f.ActiveTool(t, "erp", "purchase")
	agent := h.f.ActiveAgent(t, "buyer", tool.Tool)
	ah := &actionHarness{harness: h, pdp: pdp, agent: agent, actions: engine,
		key: h.issue(identity.KindAgent, agent.Version, "erin", "rita")}
	ah.activate(policy)
	return ah
}

func (h *actionHarness) activate(policy string) {
	h.t.Helper()
	code, p := h.as("alice", "POST", "/v1/policies", map[string]any{"content": json.RawMessage(policy)})
	h.want(201, code, p)
	code, body := h.as("bob", "POST", "/v1/policies/"+str(p, "id")+"/activate", map[string]any{"reason": "test"})
	h.want(204, code, body)
}

// send issues a request with headers.
func (h *actionHarness) send(key, method, path string, headers map[string]string, body any) (int, map[string]any, http.Header) {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		buf, _ := json.Marshal(body)
		rd = bytes.NewReader(buf)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+key)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, resp.Header
}

func actionBody(amount int) map[string]any {
	return map[string]any{
		"subject": "carol@tenant-a.test", "operation": "purchase", "target": "erp", "tool": "erp.purchase",
		"tool_schema_version": "1", "resource": "po", "payload": map[string]any{"amount": amount, "currency": "THB"},
	}
}

func (h *actionHarness) submit(key string, amount int, query string) (int, map[string]any, http.Header) {
	return h.send(h.key, "POST", "/v1/actions"+query, map[string]string{"Idempotency-Key": key}, actionBody(amount))
}

const (
	allowPolicy    = `{"format_version":1,"rules":[{"id":"all","verdict":"allow","reason":"permitted"}]}`
	denyPolicy     = `{"format_version":1,"rules":[{"id":"all","verdict":"deny","reason":"forbidden"}]}`
	escalatePolicy = `{"format_version":1,"rules":[{"id":"review","verdict":"escalate","reason":"needs review","approval":{"quorum":2,"eligible_roles":["approver"],"ttl_seconds":600}}]}`
)

func TestSubmitActionContract(t *testing.T) {
	h := newActionHarness(t, allowPolicy, action.Limits{})
	code, body, _ := h.send(h.key, "POST", "/v1/actions", nil, actionBody(1))
	h.want(400, code, body) // Idempotency-Key is required
	code, body = h.as("alice", "POST", "/v1/actions", actionBody(1))
	h.want(403, code, body) // agents only

	code, first, _ := h.submit("k1", 2400000, "")
	h.want(202, code, first)
	if first["state"] != "QUEUED" || first["policy_version"] != float64(1) || str(first, "enforced_digest") == "" {
		t.Fatalf("queued action = %v", first)
	}
	code, replay, _ := h.submit("k1", 2400000, "")
	h.want(202, code, replay)
	if replay["id"] != first["id"] {
		t.Fatalf("replay = %v, want %v", replay["id"], first["id"])
	}
	code, body, _ = h.submit("k1", 24000000, "")
	h.want(409, code, body)
	code, body, _ = h.send(h.key, "POST", "/v1/actions", map[string]string{"Idempotency-Key": "k2"},
		map[string]any{"subject": "carol@tenant-a.test"})
	h.want(400, code, body)
}

func TestTerminalDecisionIs200(t *testing.T) {
	h := newActionHarness(t, denyPolicy, action.Limits{})
	code, body, _ := h.submit("k1", 1, "")
	h.want(200, code, body)
	if body["state"] != "DENIED" || str(body, "state_reason") != "forbidden" {
		t.Fatalf("denied = %v", body)
	}
}

func TestGovernanceOutageIs503WithTheAction(t *testing.T) {
	h := newActionHarness(t, allowPolicy, action.Limits{})
	h.pdp.down.Store(true)
	code, body, hdr := h.submit("k1", 1, "")
	h.want(503, code, body)
	if body["state"] != "RECEIVED" || str(body, "action_id") == "" || hdr.Get("Retry-After") == "" {
		t.Fatalf("503 = %v %v", body, hdr)
	}
	h.pdp.down.Store(false)
	code, again, _ := h.submit("k1", 1, "")
	h.want(202, code, again)
	if again["id"] != body["action_id"] || again["state"] != "QUEUED" {
		t.Fatalf("after recovery = %v", again)
	}
}

func TestAdmissionLimitIs429(t *testing.T) {
	h := newActionHarness(t, allowPolicy, action.Limits{MaxQueuedPerTenant: 1})
	code, body, _ := h.submit("k1", 1, "")
	h.want(202, code, body)
	code, body, hdr := h.submit("k2", 1, "")
	h.want(429, code, body)
	if hdr.Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After")
	}
	if body["error"] != "admission_limit" || body["scope"] != "tenant" {
		t.Fatalf("429 body = %v, want admission_limit with scope tenant", body)
	}
}

func TestWaitReturnsWhenTerminalOrAfterTheDeadline(t *testing.T) {
	h := newActionHarness(t, escalatePolicy, action.Limits{})
	code, body, _ := h.submit("k1", 1, "?wait=bad")
	h.want(400, code, body)
	code, body, _ = h.submit("k1", 1, "?wait=2m")
	h.want(400, code, body)
	start := time.Now()
	code, pending, _ := h.submit("k1", 1, "?wait=300ms")
	h.want(202, code, pending)
	if pending["state"] != "PENDING_APPROVAL" || time.Since(start) < 300*time.Millisecond {
		t.Fatalf("wait = %v after %v", pending, time.Since(start))
	}
	// A deny vote during the wait ends it early with the terminal state.
	go func() {
		time.Sleep(200 * time.Millisecond)
		h.as("amy", "POST", "/v1/approvals/"+str(pending, "approval_request_id")+"/votes",
			map[string]any{"decision": "DENY", "reason": "no"})
	}()
	start = time.Now()
	code, done, _ := h.send(h.key, "GET", "/v1/actions/"+str(pending, "id")+"?wait=5s", nil, nil)
	h.want(200, code, done)
	if done["state"] != "DENIED" || time.Since(start) > 4*time.Second {
		t.Fatalf("waited get = %v after %v", done, time.Since(start))
	}
}

func TestActionVisibilityAndCancel(t *testing.T) {
	h := newActionHarness(t, escalatePolicy, action.Limits{})
	_, pending, _ := h.submit("k1", 1, "")
	path := "/v1/actions/" + str(pending, "id")
	other := h.f.ActiveAgent(t, "other", h.f.ActiveTool(t, "crm", "read").Tool)
	otherKey := h.issue(identity.KindAgent, other.Version, "erin", "rita")

	code, body, _ := h.send(h.key, "GET", path, nil, nil)
	h.want(202, code, body)
	code, body, _ = h.send(otherKey, "GET", path, nil, nil)
	h.want(404, code, body)
	for _, who := range []string{"otto", "audra"} {
		code, body = h.as(who, "GET", path, nil)
		h.want(202, code, body)
	}
	code, body = h.as("erin", "GET", path, nil)
	h.want(403, code, body)
	code, body = h.as("otto", "GET", "/v1/actions/"+uuid.NewString(), nil)
	h.want(404, code, body)

	code, body, _ = h.send(otherKey, "POST", path+"/cancel", nil, map[string]any{"reason": "mine"})
	h.want(404, code, body)
	code, body = h.as("erin", "POST", path+"/cancel", map[string]any{"reason": "no"})
	h.want(403, code, body)
	code, body, _ = h.send(h.key, "POST", path+"/cancel", nil, map[string]any{"reason": "user withdrew"})
	h.want(200, code, body)
	if body["state"] != "CANCELLED" {
		t.Fatalf("cancel = %v", body)
	}
	code, body = h.as("otto", "POST", path+"/cancel", map[string]any{"reason": "again"})
	h.want(409, code, body)
}

func TestCancelOfARunningActionIsARequest(t *testing.T) {
	h := newActionHarness(t, allowPolicy, action.Limits{})
	_, queued, _ := h.submit("k1", 1, "")
	id := uuid.MustParse(str(queued, "id"))
	ctx := context.Background()
	s := worker.NewStore(h.f.App, "w1")
	l, ok, err := s.Claim(ctx, worker.Candidate{TenantID: h.f.Tenant, ActionID: id}, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim = %v %v", ok, err)
	}
	if d, _, err := s.Intent(ctx, l, 2*time.Second); err != nil || d != worker.Dispatched {
		t.Fatalf("intent = %s %v", d, err)
	}
	path := "/v1/actions/" + id.String()
	code, body, _ := h.send(h.key, "POST", path+"/cancel", nil, map[string]any{"reason": "user withdrew"})
	h.want(202, code, body)
	if body["state"] != "EXECUTING" || body["cancel_requested_at"] == nil || body["lease_generation"] != float64(1) ||
		body["attempt_count"] != float64(1) {
		t.Fatalf("cancel request = %v", body)
	}
	code, body = h.as("otto", "POST", path+"/cancel", map[string]any{"reason": "again"})
	h.want(409, code, body)
	if _, err := s.Complete(ctx, l, worker.Result{Outcome: worker.Succeeded, ExternalReference: "PO-1"}, time.Second); err != nil {
		t.Fatal(err)
	}
	code, body = h.as("otto", "GET", path, nil)
	h.want(200, code, body)
	if body["state"] != "SUCCEEDED" || body["external_reference"] != "PO-1" {
		t.Fatalf("after the call = %v", body)
	}
}

func TestAgentWaitReleasesItsApprovedAction(t *testing.T) {
	h := newActionHarness(t, escalatePolicy, action.Limits{})
	_, pending, _ := h.submit("k1", 1, "")
	votes := "/v1/approvals/" + str(pending, "approval_request_id") + "/votes"
	for _, who := range []string{"amy", "ben"} {
		code, body := h.as(who, "POST", votes, map[string]any{"decision": "APPROVE", "reason": "ok"})
		h.want(200, code, body)
	}
	// An operator's wait only observes; the agent's wait releases (T10).
	code, seen := h.as("otto", "GET", "/v1/actions/"+str(pending, "id")+"?wait=300ms", nil)
	h.want(202, code, seen)
	if seen["state"] != "AUTHORIZED" {
		t.Fatalf("operator wait = %v", seen)
	}
	code, released, _ := h.send(h.key, "GET", "/v1/actions/"+str(pending, "id")+"?wait=1", nil, nil)
	h.want(202, code, released)
	if released["state"] != "QUEUED" || released["released_at"] == nil {
		t.Fatalf("agent wait = %v", released)
	}
}

func TestWaitDoesNotPollAnUnavailablePDP(t *testing.T) {
	h := newActionHarness(t, escalatePolicy, action.Limits{})
	_, pending, _ := h.submit("k1", 1, "")
	votes := "/v1/approvals/" + str(pending, "approval_request_id") + "/votes"
	for _, who := range []string{"amy", "ben"} {
		code, body := h.as(who, "POST", votes, map[string]any{"decision": "APPROVE", "reason": "ok"})
		h.want(200, code, body)
	}
	h.pdp.down.Store(true)
	before := h.pdp.calls.Load()
	code, body, _ := h.send(h.key, "GET", "/v1/actions/"+str(pending, "id")+"?wait=1", nil, nil)
	h.want(202, code, body)
	if body["state"] != "AUTHORIZED" {
		t.Fatalf("state during outage = %v", body)
	}
	if n := h.pdp.calls.Load() - before; n > 1 {
		t.Fatalf("a 1s wait made %d PDP calls, want at most 1", n)
	}
}
