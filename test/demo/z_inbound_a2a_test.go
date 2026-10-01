package demo

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"

	"github.com/atipongsena/eacp/internal/identity"
)

const tenantInbound = "00000000-0000-4000-8000-0000000000f6"

type inboundTransport struct {
	key  string
	base http.RoundTripper
}

func (tr inboundTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	copy := r.Clone(r.Context())
	copy.Header.Set("Authorization", "Bearer "+tr.key)
	return tr.base.RoundTrip(copy)
}

// TestInboundA2ADemo exercises the published API with the pinned upstream
// client and actual execution worker on compose and Kubernetes (ADR-030 Rev 1.1).
func TestInboundA2ADemo(t *testing.T) {
	d := newDemo(t, tenantInbound)
	d.subject = "carol@inbound.test"
	d.step("I0. Discover the opt-in A2A 1.0 JSON-RPC interface")
	cardBody := d.must(200, "", "GET", "/.well-known/agent-card.json", nil)
	encoded, _ := json.Marshal(cardBody)
	var card a2a.AgentCard
	if err := json.Unmarshal(encoded, &card); err != nil {
		t.Fatal(err)
	}
	if len(card.SupportedInterfaces) != 1 || card.SupportedInterfaces[0].ProtocolVersion != "1.0" || card.Capabilities.Streaming || card.Capabilities.PushNotifications {
		t.Fatal("unsupported card capabilities")
	}
	// A host-side test uses the forwarded API origin; discovery still advertises
	// its explicitly configured endpoint rather than trusting the request Host.
	card.SupportedInterfaces[0].URL = d.api + "/a2a"

	d.step("I1. Register a remote caller with ordinary approved registry capability")
	d.tenantWithCast("inbound", "Inbound A2A", []member{{"erin", "registry_editor"}, {"rita", "registry_approver"}, {"ravi", "registry_approver"}, {"otto", "operator"}, {"amy", "approver"}, {"ben", "approver"}, {"carol", "approver"}, {"audra", "auditor"}})
	policy := d.must(201, "alice", "POST", "/v1/policies", map[string]any{"content": json.RawMessage(`{"format_version":1,"rules":[{"id":"high-value","match":{"operation":"purchase_high_value"},"verdict":"escalate","reason":"two approvers","approval":{"quorum":2,"eligible_roles":["approver"],"ttl_seconds":3600}},{"id":"allow","verdict":"allow","reason":"permitted"}]}`)})
	d.must(204, "bob", "POST", "/v1/policies/"+policy["id"].(string)+"/activate", map[string]any{"reason": "reviewed"})
	d.register()
	client := func(who string) *a2aclient.Client {
		t.Helper()
		c, err := a2aclient.NewFromCard(context.Background(), &card, a2aclient.WithJSONRPCTransport(&http.Client{Timeout: 10 * time.Second, Transport: inboundTransport{d.keys[who], http.DefaultTransport}}))
		if err != nil {
			t.Fatalf("reference client construction: %T", err)
		}
		return c
	}
	caller := client("agent")
	send := func(c *a2aclient.Client, mid, operation, tool string, amount int) *a2a.Task {
		t.Helper()
		msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewDataPart(map[string]any{"subject": d.subject, "operation": operation, "target": "erp", "tool": tool, "tool_schema_version": "1", "resource": "po", "payload": map[string]any{"amount": amount, "currency": "THB"}}))
		msg.ID = mid
		got, err := c.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: msg, Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
		if err != nil {
			t.Fatalf("SendMessage: %T", err)
		}
		task, ok := got.(*a2a.Task)
		if !ok {
			t.Fatal("delegation did not return a Task")
		}
		return task
	}
	get := func(c *a2aclient.Client, id a2a.TaskID) *a2a.Task {
		t.Helper()
		task, err := c.GetTask(context.Background(), &a2a.GetTaskRequest{ID: id})
		if err != nil {
			t.Fatalf("GetTask: %T", err)
		}
		if len(task.History) != 0 {
			t.Fatal("inbound exposed message history")
		}
		return task
	}

	d.step("I2. SendMessage replay survives an API restart and produces one purchase order")
	routine := send(caller, "inbound-routine", "purchase", "erp.create_po", 123)
	d.until(string(routine.ID), "SUCCEEDED")
	d.p.restart("controlplane-api")
	d.ready()
	if replay := send(caller, "inbound-routine", "purchase", "erp.create_po", 123); replay.ID != routine.ID {
		t.Fatal("replay started a second action")
	}
	if get(caller, routine.ID).Status.State != a2a.TaskStateCompleted {
		t.Fatal("completed action not projected")
	}
	d.onePO(string(routine.ID))
	changed := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewDataPart(map[string]any{"subject": d.subject, "operation": "purchase", "target": "erp", "tool": "erp.create_po", "tool_schema_version": "1", "resource": "po", "payload": map[string]any{"amount": 124, "currency": "THB"}}))
	changed.ID = "inbound-routine"
	if _, err := caller.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: changed, Config: &a2a.SendMessageConfig{ReturnImmediately: true}}); err == nil {
		t.Fatal("changed replay accepted")
	}

	d.step("I3. Ordinary two-person approval remains authoritative")
	high := send(caller, "inbound-high", "purchase_high_value", "erp.create_po", 2400000)
	d.until(string(high.ID), "PENDING_APPROVAL")
	if get(caller, high.ID).Status.State != a2a.TaskStateWorking {
		t.Fatal("approval projected as final")
	}
	request := d.action(string(high.ID))["approval_request_id"].(string)
	d.vote("amy", request, "PENDING")
	d.vote("ben", request, "GRANTED")
	d.until(string(high.ID), "SUCCEEDED")
	d.onePO(string(high.ID))

	d.step("I4. Capability, identity and task ownership are enforced")
	denied := send(caller, "inbound-denied", "purchase", "erp.cancel_po", 1)
	if denied.Status.State != a2a.TaskStateRejected {
		t.Fatal("capability did not deny inbound delegation")
	}
	if _, err := client("otto").GetTask(context.Background(), &a2a.GetTaskRequest{ID: routine.ID}); err == nil {
		t.Fatal("principal key accessed inbound task")
	}
	other := d.must(201, "erin", "POST", "/v1/agents", map[string]any{"name": "other-caller", "display_name": "Other caller", "environment": "production", "risk_class": "low", "owner_principal_id": d.ids["carol"]})
	version := d.must(201, "erin", "POST", "/v1/agents/"+other["id"].(string)+"/versions", map[string]any{"runtime": "go", "code_ref": "git:inbound-demo"})
	vid := version["id"].(string)
	d.certifyLeaveTool()
	al := d.must(201, "erin", "POST", "/v1/agent-versions/"+vid+"/allowlists", map[string]any{"tools": []string{"erp.create_po", "hr-mcp.get_leave_balance"}})
	d.must(204, "rita", "POST", "/v1/agent-versions/"+vid+"/allowlist", map[string]any{"allowlist_id": al["id"]})
	d.must(204, "ravi", "POST", "/v1/agent-versions/"+vid+"/transitions", map[string]any{"to": "ACTIVE", "reason": "go live"})
	cred, hash := d.newKey("other-agent", identity.KindAgent)
	d.must(201, "erin", "POST", "/v1/credentials", map[string]any{"id": cred, "kind": "ak", "agent_version_id": vid, "hash": hash, "expires_in_days": 1})
	d.must(204, "rita", "POST", "/v1/credentials/"+cred.String()+"/approve", nil)
	if _, err := client("other-agent").GetTask(context.Background(), &a2a.GetTaskRequest{ID: routine.ID}); err == nil {
		t.Fatal("another agent accessed task")
	}
	d.step("I4b. Return retained MCP output only to the calling agent")
	privateCaller := client("other-agent")
	privateMessage := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewDataPart(map[string]any{"subject": d.subject, "operation": "lookup", "target": "hr", "tool": "hr-mcp.get_leave_balance", "tool_schema_version": "1", "resource": "leave_balance", "payload": map[string]any{"employee_id": "E-1"}}))
	privateMessage.ID = "inbound-private-result"
	privateResult, err := privateCaller.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: privateMessage, Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
	if err != nil {
		t.Fatalf("private result submission: %T", err)
	}
	privateTask := privateResult.(*a2a.Task)
	deadline := time.Now().Add(90 * time.Second)
	for privateTask.Status.State != a2a.TaskStateCompleted {
		if time.Now().After(deadline) {
			t.Fatal("retained-result task did not complete")
		}
		time.Sleep(250 * time.Millisecond)
		privateTask = get(privateCaller, privateTask.ID)
	}
	if len(privateTask.Artifacts) != 1 {
		t.Fatal("private retained artifact missing")
	}
	output := privateTask.Artifacts[0].Parts[0].Data().(map[string]any)
	if output["structuredContent"].(map[string]any)["days"] != float64(12) {
		t.Fatal("wrong retained result")
	}
	if _, err := caller.GetTask(context.Background(), &a2a.GetTaskRequest{ID: privateTask.ID}); err == nil {
		t.Fatal("private task/result leaked to another agent")
	}

	d.step("I5. CancelTask cancels only a not-dispatched action; kill withholds work")
	cancel := send(caller, "inbound-cancel", "purchase_high_value", "erp.create_po", 2400000)
	ended, err := caller.CancelTask(context.Background(), &a2a.CancelTaskRequest{ID: cancel.ID})
	if err != nil || ended.Status.State != a2a.TaskStateCanceled {
		t.Fatal("pending task cancellation failed")
	}
	self := d.must(200, "agent", "GET", "/v1/agent/self", nil)
	d.must(200, "otto", "POST", "/v1/killswitch", map[string]any{"scope": "agent_version", "target_id": self["agent_version_id"], "killed": true, "reason": "inbound containment"})
	killed := send(caller, "inbound-killed", "purchase", "erp.create_po", 1)
	if killed.Status.State != a2a.TaskStateWorking {
		t.Fatal("kill invented a final action decision")
	}
	if got := d.action(string(killed.ID)); got["state"] != "QUEUED" || got["attempt_count"] != nil {
		t.Fatal("killed action dispatched")
	}
	if got := d.committedPOs()[d.action(string(killed.ID))["operation_key"].(string)]; got != 0 {
		t.Fatal("kill permitted an external effect")
	}

	d.step("I6. Scan service logs and database for credentials; verify the audit chain")
	d.secretScan()
	logs := d.p.logs()
	for _, key := range d.keys {
		if strings.Contains(logs, key) {
			t.Fatal("caller key appeared in service logs")
		}
	}
	if strings.Contains(logs, "inbound-private-result") {
		t.Fatal("request content logged")
	}
	d.evidence(string(high.ID))
	d.step("Inbound A2A demo complete")
}
