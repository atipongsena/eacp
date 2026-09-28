package demo

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/atipongsena/eacp/internal/identity"
)

// tenantD is the A2A demo's tenant: the local connector-secrets manifest
// binds the Fake A2A agent's token to it (deployments/docker/secrets).
const tenantD = "00000000-0000-4000-8000-0000000000ab"

const a2aEndpoint = "http://fakea2a:8092/a2a"

// a2aCard is the Fake A2A agent's card after its "vendor release": a new
// skill that pays invoices.
const a2aCard = `{"name":"Fake Procurement Agent","description":"Raises purchase orders in the fake ERP",
 "version":"1.1.0","supportedInterfaces":[{"url":"` + a2aEndpoint + `","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],
 "capabilities":{"streaming":false,"pushNotifications":false},"defaultInputModes":["text/plain","application/json"],
 "defaultOutputModes":["text/plain"],"skills":[
 {"id":"purchase","name":"Purchase","description":"Raise a purchase order","tags":["erp","procurement"]},
 {"id":"pay","name":"Pay","description":"Pay any invoice it is sent","tags":["erp","payments"]}]}`

type a2aAuditEntry struct {
	Method    string `json:"method"`
	MessageID string `json:"message_id"`
	TaskID    string `json:"task_id"`
	State     string `json:"state"`
}

// TestA2ADemo shows Phase 25a (ADR-030): an agent delegates to a remote A2A
// agent through EACP, which discovers and certifies the remote agent's card,
// sends each delegation once, contains an unfinished task and quarantines the
// remote agent when its card changes.
func TestA2ADemo(t *testing.T) {
	d := newDemo(t, tenantD)
	c, ok := d.p.(*composePlatform)
	if !ok {
		t.Skip("the A2A demo copies a card into the distroless Fake A2A container; run it on compose (scripts/demo.sh)")
	}
	d.reader = "audra"
	tokenBytes, err := os.ReadFile(filepath.Join(d.root, "deployments", "docker", "secrets", "fakea2a-token.dev"))
	if err != nil {
		t.Fatalf("reading the A2A credential: %v", err)
	}
	token := strings.TrimSpace(string(tokenBytes))

	d.step("D0. Bootstrap tenant Initech; a policy allows delegating procurement")
	d.tenantWithCast("initech", "Initech", []member{
		{"erin", "registry_editor"}, {"rita", "registry_approver"}, {"ravi", "registry_approver"},
		{"otto", "operator"}, {"carol", "approver"}, {"audra", "auditor"},
	})
	policy := d.must(201, "alice", "POST", "/v1/policies", map[string]any{"content": json.RawMessage(`{
		"format_version": 1, "rules": [
		{"id": "procurement", "match": {"target": "procurement"}, "verdict": "allow", "reason": "routine procurement"}]}`)})
	d.must(204, "bob", "POST", "/v1/policies/"+policy["id"].(string)+"/activate", map[string]any{"reason": "reviewed"})

	d.step("D1. Register the remote agent; the worker's scanner discovers one tool, delegate, from its card")
	conn := d.must(201, "erin", "POST", "/v1/connectors", map[string]any{"name": "procurement", "protocol": "a2a",
		"endpoint": a2aEndpoint, "secret_ref": "fakea2a"})
	connID := conn["id"].(string)
	if code, body := d.call("erin", "POST", "/v1/connectors/"+connID+"/tools", map[string]any{"name": "delegate"}); code != 409 {
		d.t.Fatalf("an A2A tool was declared by hand: %d %v", code, body)
	}
	tool := d.waitTool(connID, func(t map[string]any) bool { return t["definition"] != nil })
	def := tool["definition"].(map[string]any)
	scan := d.must(200, "audra", "GET", "/v1/connectors/"+connID+"/mcp", nil)["last_scan"].(map[string]any)
	if tool["name"] != "delegate" || scan["protocol_version"] != "1.0" {
		d.t.Fatalf("discovered %v over %v", tool["name"], scan["protocol_version"])
	}
	d.logf("erin registers procurement (A2A, %s); erin cannot declare its tool; the scanner discovers %v "+
		"(definition #%v over A2A %v, fingerprint %.16s…)", a2aEndpoint, tool["name"], def["seq"], scan["protocol_version"],
		def["fingerprint"])

	d.step("D2. Certify delegate (sent at most once, no lookup, a rejection proves no effect); give it to buyer")
	path := "/v1/tools/" + tool["id"].(string)
	contract := d.must(201, "erin", "POST", path+"/contracts", map[string]any{"definition_id": def["id"],
		"side_effects": []string{"IRREVERSIBLE_WRITE", "FINANCIAL"}, "idempotency_mode": "none",
		"reconciliation_lookup": "none", "reconciliation_consistency": "none", "proof_standard": "none",
		"no_effect_errors": []string{"a2a_rejected"}, "max_attempts": 1, "timeout_ms": 5000})
	d.must(204, "rita", "POST", path+"/contract", map[string]any{"contract_id": contract["id"]})
	team := d.must(201, "alice", "POST", "/v1/groups", map[string]any{"name": "procurement", "display_name": "procurement"})
	agent := d.must(201, "erin", "POST", "/v1/agents", map[string]any{"name": "buyer", "display_name": "buyer",
		"environment": "production", "risk_class": "high", "owner_group_id": team["id"]})
	version := d.must(201, "erin", "POST", "/v1/agents/"+agent["id"].(string)+"/versions",
		map[string]any{"runtime": "python", "code_ref": "git:a2a-demo"})
	vid := version["id"].(string)
	al := d.must(201, "erin", "POST", "/v1/agent-versions/"+vid+"/allowlists",
		map[string]any{"tools": []string{"procurement.delegate"}})
	d.must(204, "rita", "POST", "/v1/agent-versions/"+vid+"/allowlist", map[string]any{"allowlist_id": al["id"]})
	d.must(204, "ravi", "POST", "/v1/agent-versions/"+vid+"/transitions", map[string]any{"to": "ACTIVE", "reason": "go live"})
	cred, hash := d.newKey("buyer", identity.KindAgent)
	d.must(201, "erin", "POST", "/v1/credentials", map[string]any{"id": cred, "kind": identity.KindAgent,
		"agent_version_id": vid, "hash": hash, "expires_in_days": 1})
	d.must(204, "rita", "POST", "/v1/credentials/"+cred.String()+"/approve", nil)
	d.logf("erin proposes and rita activates the contract, pinned to definition #%v; buyer may use procurement.delegate",
		def["seq"])

	audit := func() []a2aAuditEntry {
		d.t.Helper()
		out, err := exec.Command("docker", "run", "--rm", "--network", c.project+"_erp", "busybox:1.37", "wget", "-q",
			"-O-", "--header", "Authorization: Bearer "+token, "http://fakea2a:8092/v1/audit").Output()
		if err != nil {
			d.t.Fatalf("reading the Fake A2A audit: %v", err)
		}
		var entries []a2aAuditEntry
		if err := json.Unmarshal(out, &entries); err != nil {
			d.t.Fatalf("Fake A2A audit: %v", err)
		}
		return entries
	}
	count := func(method, messageID, taskID string) int {
		n := 0
		for _, e := range audit() {
			if e.Method == method && (messageID == "" || e.MessageID == messageID) && (taskID == "" || e.TaskID == taskID) {
				n++
			}
		}
		return n
	}

	d.step("D3. buyer delegates: a task that completes, one that asks for input, one the agent rejects")
	done := d.delegateA2A("a2a-1", "")
	d.until(done, "SUCCEEDED")
	ref, _ := d.action(done)["external_reference"].(string)
	if !strings.HasPrefix(ref, "task-") || count("SendMessage", done, "") != 1 || count("SendMessage", done, ref) != 1 {
		d.t.Fatalf("completed delegation: reference %q, audit %+v", ref, audit())
	}
	d.logf("SUCCEEDED with the remote task %s; the agent received exactly one message, whose id is the action id", ref)

	input := d.delegateA2A("a2a-2", "input_required")
	d.until(input, "UNKNOWN_OUTCOME", "NEEDS_HUMAN_RESOLUTION")
	ev := d.must(200, "audra", "GET", "/v1/actions/"+input+"/evidence", nil)
	attempt := ev["attempts"].([]any)[0].(map[string]any)
	remote, _ := attempt["remote_reference"].(string)
	if attempt["error_class"] != "a2a_input_required" || remote == "" || count("CancelTask", "", remote) != 1 {
		d.t.Fatalf("input required: attempt %v, audit %+v", attempt, audit())
	}
	d.logf("the agent wants input EACP never gives: %v (%v), remote task %s in the evidence, cancelled once",
		d.action(input)["state"], attempt["error_class"], remote)

	rejected := d.delegateA2A("a2a-3", "reject")
	d.until(rejected, "FAILED")
	d.logf("the agent rejects the task: FAILED, a certified no-effect (a2a_rejected)")

	d.step("D4. The remote agent's card gains a skill: the rescan quarantines delegate")
	dir, err := os.MkdirTemp(filepath.Join(d.root, "test", "demo"), ".card-")
	if err != nil {
		d.t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "card.json")
	if err := os.WriteFile(file, []byte(a2aCard), 0o644); err != nil {
		d.t.Fatal(err)
	}
	rel, err := filepath.Rel(d.root, file)
	if err != nil {
		d.t.Fatal(err)
	}
	if out, err := c.run("cp", filepath.ToSlash(rel), "fakea2a:/tmp/card.json"); err != nil {
		d.t.Fatalf("replacing the Fake A2A card: %v: %s", err, out)
	}
	d.must(200, "otto", "POST", "/v1/connectors/"+connID+"/mcp/scan", map[string]any{"reason": "vendor released 1.1.0"})
	q := d.waitTool(connID, func(t map[string]any) bool { return t["quarantined_at"] != nil })
	denied := d.delegateA2A("a2a-4", "")
	d.until(denied, "DENIED")
	if reason := d.action(denied)["state_reason"]; reason != "tool_quarantined" {
		d.t.Fatalf("delegation after the card changed = %v", reason)
	}
	d.logf("delegate quarantined (%v); buyer's next delegation is DENIED tool_quarantined", q["quarantine_reason"])

	d.step("D5. Search responses, logs and the database for the A2A credential")
	d.mu.Lock()
	responses := strings.Join(d.responses, "\n")
	d.mu.Unlock()
	dump, err := d.p.postgres("pg_dump", "-U", "postgres", "--data-only", "eacp")
	if err != nil {
		d.t.Fatalf("pg_dump: %v", err)
	}
	for where, text := range map[string]string{"an API response": responses, "a service log": d.p.logs(), "the database": dump} {
		if strings.Contains(text, token) {
			d.t.Fatalf("the A2A credential appears in %s", where)
		}
	}
	d.logf("no A2A credential in %d responses, the service logs or a %d-byte database dump", len(d.responses), len(dump))
	d.step("A2A demo complete")
}

// delegateA2A has buyer delegate to the remote agent; scenario picks the
// Fake A2A agent's behaviour.
func (d *demo) delegateA2A(idem, scenario string) string {
	d.t.Helper()
	payload := map[string]any{"text": "Buy 10 laptops for the Bangkok office"}
	if scenario != "" {
		payload["data"] = map[string]any{"scenario": scenario}
	}
	code, body := d.call("buyer", "POST", "/v1/actions?wait=2s", map[string]any{"subject": "carol@initech.test",
		"operation": "delegate", "target": "procurement", "tool": "procurement.delegate", "tool_schema_version": "1",
		"resource": "purchase", "payload": payload}, "Idempotency-Key", idem)
	if code != 200 && code != 202 {
		d.t.Fatalf("delegate %s = %d %v", idem, code, body)
	}
	id, _ := body["id"].(string)
	if id == "" {
		d.t.Fatalf("delegate %s: no action id in %v", idem, body)
	}
	return id
}
