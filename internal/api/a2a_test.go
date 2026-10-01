package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/action"
	"github.com/atipongsena/eacp/internal/api"
	"github.com/atipongsena/eacp/internal/identity"
	"github.com/atipongsena/eacp/internal/registry/registrytest"
	"github.com/atipongsena/eacp/internal/storage"
	"github.com/atipongsena/eacp/internal/storage/pgtest"
	"github.com/atipongsena/eacp/internal/worker"
)

func newA2AHarness(t *testing.T, policy string, limits action.Limits) *actionHarness {
	t.Helper()
	pdp := &switchPDP{}
	var engine *action.Engine
	h := newHarness(t, func(f *registrytest.Fixture, s *api.Server) {
		engine = action.New(f.App, action.Options{Provider: pdp, Limits: limits})
		s.WithActions(engine).WithA2A("https://eacp.example.test/a2a")
	})
	tool := h.f.ActiveTool(t, "erp", "purchase")
	agent := h.f.ActiveAgent(t, "buyer", tool.Tool)
	ah := &actionHarness{harness: h, pdp: pdp, agent: agent, actions: engine,
		key: h.issue(identity.KindAgent, agent.Version, "erin", "rita")}
	ah.activate(policy)
	return ah
}

func a2aMessage(id string, data any) map[string]any {
	return map[string]any{"message": map[string]any{"messageId": id, "role": "ROLE_USER",
		"parts": []any{map[string]any{"data": data, "mediaType": "application/json"}}},
		"configuration": map[string]any{"returnImmediately": true}}
}

func a2aRPC(method string, params any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": "rpc-1", "method": method, "params": params}
}

func (h *actionHarness) a2aCall(key, method string, params any) (int, map[string]any) {
	h.t.Helper()
	code, body, _ := h.send(key, "POST", "/a2a", map[string]string{"A2A-Version": "1.0", "Content-Type": "application/json"}, a2aRPC(method, params))
	return code, body
}

func a2aTask(t *testing.T, body map[string]any, send bool) map[string]any {
	t.Helper()
	result, ok := body["result"].(map[string]any)
	if !ok {
		t.Fatalf("missing RPC result: %v", body)
	}
	if send {
		result, ok = result["task"].(map[string]any)
		if !ok {
			t.Fatalf("missing SendMessage task: %v", body)
		}
	}
	return result
}

func a2aState(task map[string]any) string {
	status, _ := task["status"].(map[string]any)
	return str(status, "state")
}

func a2aError(t *testing.T, body map[string]any, want int) {
	t.Helper()
	err, _ := body["error"].(map[string]any)
	if err["code"] != float64(want) || body["jsonrpc"] != "2.0" {
		t.Fatalf("RPC error = %v, want code %d", body, want)
	}
}

func TestA2ADiscoveryIsStaticAndOptIn(t *testing.T) {
	h := newA2AHarness(t, allowPolicy, action.Limits{})
	code, card, hdr := h.send("", "GET", "/.well-known/agent-card.json", nil, nil)
	h.want(200, code, card)
	interfaces, _ := card["supportedInterfaces"].([]any)
	if len(interfaces) != 1 || interfaces[0].(map[string]any)["url"] != "https://eacp.example.test/a2a" ||
		interfaces[0].(map[string]any)["protocolVersion"] != "1.0" || hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("card = %v", card)
	}
	capabilities := card["capabilities"].(map[string]any)
	if capabilities["streaming"] != false || capabilities["pushNotifications"] != false || card["securitySchemes"] == nil {
		t.Fatalf("capabilities/auth = %v", card)
	}
	raw, _ := json.Marshal(card)
	if strings.Contains(string(raw), h.f.Tenant.String()) || strings.Contains(string(raw), "buyer") || strings.Contains(string(raw), "erp.purchase") {
		t.Fatal("discovery disclosed tenant registry")
	}
	disabled := newActionHarness(t, allowPolicy, action.Limits{})
	for _, path := range []string{"/.well-known/agent-card.json", "/a2a"} {
		code, _, _ := disabled.send(disabled.key, "GET", path, nil, nil)
		if code != 404 {
			t.Fatalf("disabled route %s status = %d", path, code)
		}
	}
}

func TestA2AReusesGovernedActionsAndIdempotency(t *testing.T) {
	h := newA2AHarness(t, allowPolicy, action.Limits{})
	params := a2aMessage("message-1", actionBody(123))
	code, body := h.a2aCall(h.key, "SendMessage", params)
	h.want(200, code, body)
	task := a2aTask(t, body, true)
	id := uuid.MustParse(str(task, "id"))
	if a2aState(task) != "TASK_STATE_WORKING" || task["contextId"] != id.String() || task["history"] != nil {
		t.Fatalf("task = %v", task)
	}
	v, err := h.actions.Read(context.Background(), action.Agent(h.f.Tenant, h.agent.Agent, h.agent.Version), id)
	if err != nil || v.State != "QUEUED" || !strings.HasPrefix(v.IdempotencyKey, "a2a:") || v.NotAfter.Sub(v.CreatedAt) != time.Hour {
		t.Fatalf("action projection state/deadline = %s, %v", v.State, err)
	}
	_, replay := h.a2aCall(h.key, "SendMessage", params)
	if a2aTask(t, replay, true)["id"] != id.String() {
		t.Fatal("replay enqueued a second action")
	}
	_, conflict := h.a2aCall(h.key, "SendMessage", a2aMessage("message-1", actionBody(456)))
	a2aError(t, conflict, -32000)
	_, read := h.a2aCall(h.key, "GetTask", map[string]any{"id": id.String()})
	if a2aTask(t, read, false)["id"] != id.String() {
		t.Fatal("GetTask is not the action")
	}
	_, canceled := h.a2aCall(h.key, "CancelTask", map[string]any{"id": id.String()})
	if a2aState(a2aTask(t, canceled, false)) != "TASK_STATE_CANCELED" {
		t.Fatal("queued task was not canceled")
	}
	_, repeated := h.a2aCall(h.key, "CancelTask", map[string]any{"id": id.String()})
	if a2aState(a2aTask(t, repeated, false)) != "TASK_STATE_CANCELED" {
		t.Fatal("cancellation replay is inconsistent")
	}
}

func TestA2AAuthenticationAndTaskOwnership(t *testing.T) {
	h := newA2AHarness(t, allowPolicy, action.Limits{})
	for _, tc := range []struct {
		key    string
		status int
	}{{"", 401}, {h.keys["alice"], 403}, {h.keys["otto"], 403}} {
		code, body := h.a2aCall(tc.key, "SendMessage", a2aMessage("m", actionBody(1)))
		h.want(tc.status, code, body)
	}
	_, body := h.a2aCall(h.key, "SendMessage", a2aMessage("m", actionBody(1)))
	id := str(a2aTask(t, body, true), "id")
	tool := h.f.ActiveTool(t, "other", "purchase")
	other := h.f.ActiveAgent(t, "other-buyer", tool.Tool)
	otherKey := h.issue(identity.KindAgent, other.Version, "erin", "rita")
	for _, method := range []string{"GetTask", "CancelTask"} {
		for _, taskID := range []string{id, uuid.NewString()} {
			_, denied := h.a2aCall(otherKey, method, map[string]any{"id": taskID})
			a2aError(t, denied, -32001)
		}
	}
	cred, _ := identity.ParseKey(h.key)
	if err := h.f.Exec("otto", `UPDATE eacp.credentials SET revoked_at = now(), revoke_reason = 'test revocation' WHERE id = $1`, cred.CredentialID); err != nil {
		t.Fatal(err)
	}
	code, body := h.a2aCall(h.key, "GetTask", map[string]any{"id": id})
	h.want(401, code, body)
	if strings.Contains(h.logs.String(), h.key) {
		t.Fatal("authentication logged a key")
	}
}

func TestA2ADecisionsOutageAndAdmission(t *testing.T) {
	for _, tc := range []struct{ policy, state string }{{denyPolicy, "TASK_STATE_REJECTED"}, {escalatePolicy, "TASK_STATE_WORKING"}} {
		h := newA2AHarness(t, tc.policy, action.Limits{})
		_, body := h.a2aCall(h.key, "SendMessage", a2aMessage("m", actionBody(1)))
		if a2aState(a2aTask(t, body, true)) != tc.state {
			t.Fatalf("decision task = %v", body)
		}
	}
	h := newA2AHarness(t, allowPolicy, action.Limits{MaxQueuedPerTenant: 1})
	h.pdp.down.Store(true)
	_, body := h.a2aCall(h.key, "SendMessage", a2aMessage("m", actionBody(1)))
	task := a2aTask(t, body, true)
	if a2aState(task) != "TASK_STATE_SUBMITTED" {
		t.Fatalf("outage lost committed task: %v", body)
	}
	h.pdp.down.Store(false)
	_, recovered := h.a2aCall(h.key, "SendMessage", a2aMessage("m", actionBody(1)))
	if a2aTask(t, recovered, true)["id"] != task["id"] {
		t.Fatal("outage recovery enqueued again")
	}
	_, limited := h.a2aCall(h.key, "SendMessage", a2aMessage("other", actionBody(1)))
	a2aError(t, limited, -32000)
	if limited["result"] != nil {
		t.Fatal("admission refusal manufactured a task")
	}
}

func TestA2ARejectsUnsupportedAndAmbiguousRequests(t *testing.T) {
	h := newA2AHarness(t, allowPolicy, action.Limits{})
	changes := []func(map[string]any){
		func(p map[string]any) { p["tenant"] = h.f.Tenant.String() },
		func(p map[string]any) { p["configuration"].(map[string]any)["returnImmediately"] = false },
		func(p map[string]any) { p["configuration"].(map[string]any)["historyLength"] = 1 },
		func(p map[string]any) {
			p["configuration"].(map[string]any)["taskPushNotificationConfig"] = map[string]any{"url": "https://example.test"}
		},
		func(p map[string]any) {
			p["configuration"].(map[string]any)["acceptedOutputModes"] = []string{"text/plain"}
		},
		func(p map[string]any) { p["message"].(map[string]any)["contextId"] = "conversation" },
		func(p map[string]any) { p["message"].(map[string]any)["taskId"] = uuid.NewString() },
		func(p map[string]any) { p["message"].(map[string]any)["role"] = "ROLE_AGENT" },
		func(p map[string]any) { p["message"].(map[string]any)["messageId"] = "" },
		func(p map[string]any) {
			p["message"].(map[string]any)["parts"] = []any{map[string]any{"text": "buy something"}}
		},
		func(p map[string]any) {
			p["message"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"] = "ambiguous"
		},
		func(p map[string]any) {
			p["message"].(map[string]any)["parts"].([]any)[0].(map[string]any)["data"].(map[string]any)["lifetime_seconds"] = 7200
		},
	}
	for i, change := range changes {
		p := a2aMessage("m", actionBody(1))
		change(p)
		_, body := h.a2aCall(h.key, "SendMessage", p)
		if body["error"] == nil || body["result"] != nil {
			t.Fatalf("unsupported case %d accepted: %v", i, body)
		}
	}
	for _, method := range []string{"SendStreamingMessage", "SubscribeToTask", "ListTasks", "CreateTaskPushNotificationConfig"} {
		_, body := h.a2aCall(h.key, method, map[string]any{})
		if body["error"] == nil {
			t.Fatalf("unsupported %s accepted", method)
		}
	}
	code, body, _ := h.send(h.key, "POST", "/a2a", map[string]string{"A2A-Version": "0.3"}, a2aRPC("SendMessage", a2aMessage("m", actionBody(1))))
	h.want(200, code, body)
	a2aError(t, body, -32009)
	for _, raw := range []string{`{"jsonrpc":"2.0","id":1,"id":2,"method":"GetTask","params":{}}`,
		`{"jsonrpc":"2.0","method":"GetTask","params":{}}`, `[]`, `{`,
		`{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{}} {}`, strings.Repeat(" ", (1<<20)+1)} {
		req, _ := http.NewRequest("POST", h.srv.URL+"/a2a", strings.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+h.key)
		req.Header.Set("A2A-Version", "1.0")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var reply map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&reply)
		resp.Body.Close()
		if reply["error"] == nil {
			t.Fatal("invalid envelope accepted")
		}
	}
}

func TestA2AInFlightCancellationAndUncertainty(t *testing.T) {
	h := newA2AHarness(t, allowPolicy, action.Limits{})
	_, body := h.a2aCall(h.key, "SendMessage", a2aMessage("m", actionBody(1)))
	id := uuid.MustParse(str(a2aTask(t, body, true), "id"))
	store := worker.NewStore(h.f.App, "w1")
	lease, ok, err := store.Claim(context.Background(), worker.Candidate{TenantID: h.f.Tenant, ActionID: id}, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim = %v, error = %v", ok, err)
	}
	if dispatched, _, err := store.Intent(context.Background(), lease, 2*time.Second); err != nil || dispatched != worker.Dispatched {
		t.Fatalf("intent = %s, error = %v", dispatched, err)
	}
	_, canceled := h.a2aCall(h.key, "CancelTask", map[string]any{"id": id.String()})
	a2aError(t, canceled, -32002)
	v, err := h.actions.Read(context.Background(), action.Agent(h.f.Tenant, h.agent.Agent, h.agent.Version), id)
	if err != nil || v.CancelRequestedAt == nil || v.State != "EXECUTING" {
		t.Fatal("cancel did not preserve dispatch uncertainty")
	}
	if _, err := store.Complete(context.Background(), lease, worker.Result{Outcome: worker.Ambiguous, ErrorClass: "transport"}, time.Second); err != nil {
		t.Fatal(err)
	}
	_, seen := h.a2aCall(h.key, "GetTask", map[string]any{"id": id.String()})
	if a2aState(a2aTask(t, seen, false)) != "TASK_STATE_WORKING" {
		t.Fatal("unknown outcome reported as final")
	}
}

type a2aBearerTransport struct {
	base http.RoundTripper
	key  string
}

func (t a2aBearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	copy := r.Clone(r.Context())
	copy.Header.Set("Authorization", "Bearer "+t.key)
	return t.base.RoundTrip(copy)
}

func TestA2AReferenceClientAndPrivateResults(t *testing.T) {
	h := newA2AHarness(t, allowPolicy, action.Limits{})
	key, run := keeper(t, h)
	id := run(`{"private_marker":"caller-only"}`, nil)
	resp, err := http.Get(h.srv.URL + "/.well-known/agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	var card a2a.AgentCard
	err = json.NewDecoder(resp.Body).Decode(&card)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	card.SupportedInterfaces[0].URL = h.srv.URL + "/a2a"
	client, err := a2aclient.NewFromCard(context.Background(), &card, a2aclient.WithJSONRPCTransport(&http.Client{Transport: a2aBearerTransport{http.DefaultTransport, key}}))
	if err != nil {
		t.Fatal(err)
	}
	task, err := client.GetTask(context.Background(), &a2a.GetTaskRequest{ID: a2a.TaskID(id.String())})
	if err != nil || task.Status.State != a2a.TaskStateCompleted || len(task.Artifacts) != 1 || len(task.History) != 0 {
		t.Fatalf("reference result: %v", err)
	}
	out := task.Artifacts[0].Parts[0].Data().(map[string]any)
	if out["private_marker"] != "caller-only" {
		t.Fatal("private artifact missing")
	}
	p := actionBody(1)
	p["tool"], p["target"] = "hr.balance", "hr"
	message := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewDataPart(p))
	message.ID = "reference-message"
	result, err := client.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: message, Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
	if err != nil {
		t.Fatal(err)
	}
	started := result.(*a2a.Task)
	if started.Status.State != a2a.TaskStateWorking {
		t.Fatal("reference SendMessage failed")
	}
	ended, err := client.CancelTask(context.Background(), &a2a.CancelTaskRequest{ID: started.ID})
	if err != nil || ended.Status.State != a2a.TaskStateCanceled {
		t.Fatalf("reference CancelTask: %v", err)
	}
	if strings.Contains(h.logs.String(), "private_marker") || strings.Contains(h.logs.String(), key) {
		t.Fatal("content/key logged")
	}
	if err := storage.InTenantTx(context.Background(), h.f.Owner, h.f.Tenant.String(), func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `UPDATE eacp.action_results SET expires_at = now() - interval '1 second'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	task, err = client.GetTask(context.Background(), &a2a.GetTaskRequest{ID: a2a.TaskID(id.String())})
	if err != nil || task.Status.State != a2a.TaskStateCompleted || len(task.Artifacts) != 0 {
		t.Fatal("expired result changed success or leaked content")
	}
	// Check the response itself has neither historical input nor retained expired output.
	_, body := h.a2aCall(key, "GetTask", map[string]any{"id": id.String()})
	raw, _ := json.Marshal(body)
	if bytes.Contains(raw, []byte("caller-only")) || bytes.Contains(raw, []byte("payload")) {
		t.Fatal("private content retained in task metadata")
	}
}

func TestA2ACrossTenantAndCoreContainment(t *testing.T) {
	h := newA2AHarness(t, allowPolicy, action.Limits{})
	_, body := h.a2aCall(h.key, "SendMessage", a2aMessage("original", actionBody(1)))
	id := str(a2aTask(t, body, true), "id")
	other := h.f.ForTenant(t, pgtest.TenantB)
	tool := other.ActiveTool(t, "erp", "purchase")
	agent := other.ActiveAgent(t, "buyer", tool.Tool)
	issuer := &harness{t: t, f: other}
	key := issuer.issue(identity.KindAgent, agent.Version, "erin", "rita")
	for _, method := range []string{"GetTask", "CancelTask"} {
		_, denied := h.a2aCall(key, method, map[string]any{"id": id})
		a2aError(t, denied, -32001)
	}
	code, kill := h.as("otto", "POST", "/v1/killswitch", map[string]any{"scope": "agent_version", "target_id": h.agent.Version.String(), "killed": true, "reason": "containment"})
	h.want(200, code, kill)
	_, denied := h.a2aCall(h.key, "SendMessage", a2aMessage("killed", actionBody(1)))
	if a2aState(a2aTask(t, denied, true)) != "TASK_STATE_WORKING" {
		t.Fatal("kill containment must not invent a final decision")
	}
	v, err := h.actions.Read(context.Background(), action.Agent(h.f.Tenant, h.agent.Agent, h.agent.Version), uuid.MustParse(str(a2aTask(t, denied, true), "id")))
	if err != nil || v.State != "QUEUED" || v.AttemptCount != 0 {
		t.Fatal("killed delegation dispatched")
	}
	store := worker.NewStore(h.f.App, "w1")
	if _, ok, err := store.Claim(context.Background(), worker.Candidate{TenantID: h.f.Tenant, ActionID: v.ID}, time.Minute); err != nil || ok {
		t.Fatalf("killed claim = %v, %v", ok, err)
	}
}

func TestA2AUsesTheHardBudgetAndNonActiveLifecycle(t *testing.T) {
	h := newA2AHarness(t, allowPolicy, action.Limits{})
	tool := h.f.ActiveToolWith(t, "costed", "purchase", registrytest.CostedContractSQL)
	agent := h.f.ActiveAgent(t, "costed-buyer", tool.Tool)
	key := h.issue(identity.KindAgent, agent.Version, "erin", "rita")
	h.f.FundAgent(t, agent.Agent, "THB", "10")
	data := actionBody(11)
	data["tool"], data["target"] = "costed.purchase", "costed"
	_, denied := h.a2aCall(key, "SendMessage", a2aMessage("budget", data))
	task := a2aTask(t, denied, true)
	v, err := h.actions.Read(context.Background(), action.Agent(h.f.Tenant, agent.Agent, agent.Version), uuid.MustParse(str(task, "id")))
	if err != nil || v.StateReason != action.BudgetExceeded || a2aState(task) != "TASK_STATE_REJECTED" || v.AttemptCount != 0 {
		t.Fatal("A2A bypassed hard budget")
	}
	if err := h.f.Exec("otto", `UPDATE eacp.agent_versions SET state = 'SUSPENDED', state_reason = 'containment' WHERE id = $1`, agent.Version); err != nil {
		t.Fatal(err)
	}
	data["payload"] = map[string]any{"amount": 1, "currency": "THB"}
	_, suspended := h.a2aCall(key, "SendMessage", a2aMessage("suspended", data))
	if a2aState(a2aTask(t, suspended, true)) != "TASK_STATE_REJECTED" {
		t.Fatal("non-active version dispatched through A2A")
	}
}
