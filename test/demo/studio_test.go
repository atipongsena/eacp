package demo

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/identity"
	"github.com/atipongsena/eacp/internal/studioruntime"
)

// tenantStudio is the Studio demo's tenant; the local connector-secrets manifest
// binds the HR MCP server's credential to it.
const tenantStudio = "00000000-0000-4000-8000-0000000000f5"

// leaveAnswer is the answer stella reads for E-1 (fakemcp-hr: 12 days).
const leaveAnswer = "You have 12 days of leave left."

// TestStudioDemo is the Agent Studio thin slice through the API (ADR-033,
// Phase 27a-3a): an HR employee's agent, approved by a second person, runs
// in agent-runtime with a key derived from a master only the runtime holds,
// and every side effect goes through the action path. Phase 27b adds the
// Hub: an HR lead publishes the agent to HR, a colleague runs it, and a
// finance author's copy starts with no permission. Phase 28 adds the run
// kill scope: an operator kills one run and its step never runs.
func TestStudioDemo(t *testing.T) {
	d := newDemo(t, tenantStudio)

	d.step("S0. Bootstrap tenant Wonka: people, the HR department and the runtime's own principal")
	d.tenantWithCast("wonka", "Wonka", []member{
		{"erin", "registry_editor"}, {"rita", "registry_approver"}, {"otto", "operator"}, {"opal", "operator"}, {"audra", "auditor"},
		{"stella", "studio_author"}, {"carol", "approver"}, {"hana", "approver"}, {"lena", "approver"},
		{"finn", "studio_author"},
	})
	hr := d.must(201, "alice", "POST", "/v1/groups", map[string]any{"name": "hr", "display_name": "HR"})
	finance := d.must(201, "alice", "POST", "/v1/groups", map[string]any{"name": "finance", "display_name": "Finance"})
	for who, g := range map[string]map[string]any{"stella": hr, "hana": hr, "lena": hr, "finn": finance} {
		d.must(201, "alice", "POST", "/v1/groups/"+g["id"].(string)+"/members",
			map[string]any{"principal_id": d.ids[who], "lead": who == "lena"})
	}
	d.logf("stella (studio_author) and hana are in HR, led by lena; finn (studio_author) is in finance; carol is in neither")
	d.runtimePrincipal()
	policy := d.must(201, "alice", "POST", "/v1/policies", map[string]any{"content": json.RawMessage(`{"format_version": 1,
		"rules": [{"id": "hr-lookups", "match": {"target": "hr"}, "verdict": "allow", "reason": "read-only HR lookup"}]}`)})
	d.must(204, "bob", "POST", "/v1/policies/"+policy["id"].(string)+"/activate", map[string]any{"reason": "reviewed"})
	d.logf("policy v%v active: read-only HR lookups are allowed", policy["version"])

	d.step("S1. Register the HR MCP server; the worker discovers get_leave_balance; erin and rita certify it")
	leaveTool := d.certifyLeaveTool()

	d.step("S2. Start agent-runtime with a fresh master secret and its own key")
	master := d.startRuntime()

	d.step("S3. stella saves the leave-balance template; rita approves it; the runtime proposes its key")
	def, err := os.ReadFile(filepath.Join("testdata", "leave-balance.json"))
	if err != nil {
		t.Fatal(err)
	}
	saved := d.must(201, "stella", "POST", "/v1/studio/agents", map[string]any{"name": "leave-bot",
		"display_name": "Leave balance", "description": "Tells an employee how many days of leave they have left.",
		"department_id": hr["id"], "definition": json.RawMessage(def)})
	agent, version := saved["agent_id"].(string), saved["id"].(string)
	d.logf("saved version %v: %v, waiting on %v; capability %v (PostgreSQL derived it from the steps)",
		saved["version"], saved["status"], saved["waiting_on"], saved["capability"])
	code, body := d.call("stella", "POST", "/v1/studio/versions/"+version+"/approve", map[string]any{"reason": "mine"})
	d.logf("stella cannot approve her own agent: HTTP %d %v", code, body["error"])
	if code == 200 {
		t.Fatal("the author approved her own agent")
	}
	d.must(200, "rita", "POST", "/v1/studio/versions/"+version+"/approve", map[string]any{"reason": "reads leave balances only"})
	d.logf("rita approves: the version is ACTIVE with exactly that allowlist")
	early := d.runAs("stella", agent, "E-1")
	d.waitRun(early, "FAILED", "credential_pending")
	d.logf("a run before its key is approved fails closed: credential_pending, and no action was sent")
	key := d.approveRuntimeKey(version)

	d.step("S4. stella runs it; carol cannot; rita sees the run but not the answer")
	run := d.runAs("stella", agent, "E-1")
	got := d.waitRun(run, "SUCCEEDED", "")
	if got["answer"] != leaveAnswer {
		t.Fatalf("answer = %v", got["answer"])
	}
	step := got["steps"].([]any)[0].(map[string]any)
	d.logf("stella reads %q; step %v was action %.8s (%v)", got["answer"], step["step_id"], step["action_id"], step["action_state"])
	action := d.must(200, "audra", "GET", "/v1/actions/"+step["action_id"].(string), nil)
	if action["agent_version_id"] != version || action["subject"] != "stella@wonka.test" || action["tool"] != "hr-mcp.get_leave_balance" {
		t.Fatalf("the step's action = %v", action)
	}
	d.logf("the action is the version's own, for stella, through hr-mcp.get_leave_balance: policy, budget and kill scopes applied to it")
	code, body = d.call("carol", "POST", "/v1/studio/agents/"+agent+"/runs", map[string]any{"inputs": map[string]string{"employee_id": "E-2"}})
	if code != 403 {
		t.Fatalf("carol ran an HR agent: %d %v", code, body)
	}
	d.logf("carol, outside HR, cannot run it: HTTP %d", code)
	seen := d.must(200, "rita", "GET", "/v1/studio/runs/"+run, nil)
	if _, ok := seen["answer"]; ok || seen["state"] != "SUCCEEDED" {
		t.Fatalf("rita sees %v", seen)
	}
	d.logf("rita sees the run and its steps, never its answer")
	calls := d.hrCalls()
	if len(calls) != 1 || calls[0].Tool != "get_leave_balance" {
		t.Fatalf("fakemcp-hr calls = %+v", calls)
	}
	d.logf("fakemcp-hr answered one call, logged as a tool name and a time only")

	d.step("S5. An unknown employee: the tool reports an error, and the run fails closed")
	failed := d.waitRun(d.runAs("stella", agent, "ERR"), "FAILED", "action_failed")
	errStep := d.must(200, "audra", "GET", "/v1/actions/"+failed["steps"].([]any)[0].(map[string]any)["action_id"].(string), nil)
	if errStep["state"] != "FAILED" {
		t.Fatalf("the ERR step's action = %v", errStep)
	}
	d.logf("the tool answered isError (not certified as no effect, so ambiguous); a READ_ONLY call has no effect to be unsure of, "+
		"so with its one attempt spent the action is FAILED (%v) and the run FAILED action_failed, with no answer", errStep["state_reason"])
	if _, ok := failed["answer"]; ok {
		t.Fatalf("a failed run has an answer: %v", failed)
	}

	d.step("S6. The Hub: lena, HR's lead, publishes leave-bot to HR; hana runs it; finn's copy starts unapproved")
	code, body = d.call("hana", "POST", "/v1/studio/agents/"+agent+"/runs", map[string]any{"inputs": map[string]string{"employee_id": "E-1"}})
	if code != 403 {
		t.Fatalf("hana ran an unpublished agent: %d %v", code, body)
	}
	d.logf("before it is published only its owner runs it: hana, in HR, gets HTTP %d", code)
	proposal := d.must(201, "stella", "POST", "/v1/studio/agents/"+agent+"/listing", map[string]any{"version_id": version,
		"scope": "DEPARTMENT", "tags": []string{"leave", "hr"}, "note": "for everyone in HR"})
	decide := "/v1/studio/listing-proposals/" + proposal["id"].(string) + "/approve"
	code, body = d.call("stella", "POST", decide, map[string]any{"reason": "mine"})
	if code != 409 {
		t.Fatalf("stella published her own agent: %d %v", code, body)
	}
	code, body = d.call("rita", "POST", decide, map[string]any{"reason": "looks fine"})
	if code != 403 {
		t.Fatalf("a registry approver published to a department: %d %v", code, body)
	}
	d.must(200, "lena", "POST", decide, map[string]any{"reason": "useful for HR"})
	d.logf("stella proposes it to HR: she cannot publish it herself (HTTP 409), nor can rita (HTTP 403); lena, HR's lead, publishes it")
	hub := d.must(200, "hana", "GET", "/v1/studio/hub", nil)
	listings := hub["listings"].([]any)
	if len(listings) != 1 || listings[0].(map[string]any)["runnable"] != true {
		t.Fatalf("hana's Hub = %v", hub)
	}
	listing := listings[0].(map[string]any)["id"].(string)
	colleague := d.waitRunAs("hana", d.runAs("hana", agent, "E-1"), "SUCCEEDED", "")
	if colleague["answer"] != leaveAnswer {
		t.Fatalf("hana's answer = %v", colleague["answer"])
	}
	hanaStep := d.must(200, "audra", "GET", "/v1/actions/"+colleague["steps"].([]any)[0].(map[string]any)["action_id"].(string), nil)
	if hanaStep["agent_version_id"] != version || hanaStep["subject"] != "hana@wonka.test" {
		t.Fatalf("hana's step = %v", hanaStep)
	}
	d.logf("hana finds it in the Hub and runs it as herself: the same version, her own subject, her own answer")
	if got := d.must(200, "finn", "GET", "/v1/studio/hub", nil)["listings"].([]any); len(got) != 0 {
		t.Fatalf("finn sees HR's listing: %v", got)
	}
	org := d.must(201, "stella", "POST", "/v1/studio/agents/"+agent+"/listing", map[string]any{"version_id": version,
		"scope": "ORG", "tags": []string{"leave", "hr"}})
	code, body = d.call("lena", "POST", "/v1/studio/listing-proposals/"+org["id"].(string)+"/approve", map[string]any{"reason": "wider"})
	if code != 403 {
		t.Fatalf("a department lead published to the organisation: %d %v", code, body)
	}
	d.must(200, "rita", "POST", "/v1/studio/listing-proposals/"+org["id"].(string)+"/approve", map[string]any{"reason": "useful for everyone"})
	d.logf("finance saw nothing until rita (not lena, HTTP %d) published it to the organisation", code)
	clone := d.must(201, "finn", "POST", "/v1/studio/listings/"+listing+"/clone", map[string]any{"name": "finance-leave-bot",
		"display_name": "Leave balance (finance)", "department_id": finance["id"]})
	if clone["status"] != "waiting_for_approval" {
		t.Fatalf("clone = %v", clone)
	}
	code, body = d.call("finn", "POST", "/v1/studio/agents/"+clone["agent_id"].(string)+"/runs", map[string]any{"inputs": map[string]string{"employee_id": "E-1"}})
	if code != 409 {
		t.Fatalf("finn ran an unapproved copy: %d %v", code, body)
	}
	d.logf("finn copies it into finance: the copy is %v and cannot run (HTTP %d); it carries no permission", clone["status"], code)

	d.step("S7. otto kills one run: PostgreSQL fails it killed, and its step never runs")
	sent := len(d.hrCalls())
	d.must(200, "otto", "POST", "/v1/killswitch", map[string]any{"scope": "tool", "target_id": leaveTool, "killed": true,
		"reason": "hold HR lookups (demo)"})
	held := d.runAs("stella", agent, "E-1")
	heldAction := d.waitStep(held)
	d.logf("with get_leave_balance killed, the run's step (action %.8s) waits %v and the run keeps running",
		heldAction, d.actionState(heldAction))
	d.must(200, "otto", "POST", "/v1/killswitch", map[string]any{"scope": "run", "target_id": held, "killed": true,
		"reason": "wrong employee (demo)"})
	d.waitRun(held, "FAILED", "killed")
	d.logf("otto kills that run: at its next heartbeat PostgreSQL fails it killed, and the runtime stops without finishing it")
	for _, k := range [][2]string{{"run", held}, {"tool", leaveTool}} {
		d.must(200, "opal", "POST", "/v1/killswitch", map[string]any{"scope": k[0], "target_id": k[1], "killed": false,
			"reason": "cleared (demo)"})
	}
	time.Sleep(5 * time.Second)
	after := d.actionState(heldAction)
	if after != "QUEUED" || len(d.hrCalls()) != sent {
		t.Fatalf("the killed run's step ran: %v, %d calls (was %d)", after, len(d.hrCalls()), sent)
	}
	d.logf("opal (a second operator) clears both kills: the killed run stays failed and its step stays %v, never sent", after)

	d.builderDemo(hr["id"].(string), master)

	d.step("S9. otto revokes every Studio key: runs stop at once")
	revoked := d.must(200, "otto", "POST", "/v1/studio/credentials/revoke-all", map[string]any{"reason": "suspected runtime compromise (demo)"})
	d.logf("otto revokes %v Studio key(s)", revoked["revoked"])
	d.waitRun(d.runAs("stella", agent, "E-1"), "FAILED", "credential_pending")
	d.logf("the next run fails closed: credential_pending, until a new key is proposed and approved")

	d.step("S10. No master, derived key or runtime key anywhere; the answer is in no log and no journal entry")
	d.studioScan(master, key)
	v := d.must(200, "audra", "GET", "/v1/audit/verify", nil)
	if v["valid"] != true {
		t.Fatalf("audit verify = %v", v)
	}
	d.logf("audit chain verified: %v events", v["count"])
}

// runtimePrincipal creates the runtime's service principal with
// studio_runtime alone and its key, which only the runtime keeps.
func (d *demo) runtimePrincipal() {
	d.t.Helper()
	p := d.must(201, "alice", "POST", "/v1/principals", map[string]any{"kind": "service", "name": "studio-runtime",
		"display_name": "Agent runtime"})
	id := p["id"].(string)
	d.ids["studio-runtime"] = id
	g := d.must(201, "alice", "POST", "/v1/role-grants", map[string]any{"principal_id": id, "role": "studio_runtime"})
	d.must(204, "bob", "POST", "/v1/role-grants/"+g["id"].(string)+"/approve", nil)
	cred, hash := d.newKey("studio-runtime", identity.KindPrincipal)
	d.must(201, "alice", "POST", "/v1/credentials", map[string]any{"id": cred, "kind": identity.KindPrincipal,
		"principal_id": id, "hash": hash, "expires_in_days": 1})
	d.must(204, "bob", "POST", "/v1/credentials/"+cred.String()+"/approve", nil)
	d.logf("service principal studio-runtime holds studio_runtime alone; its key is approved by a second admin")
}

// certifyLeaveTool registers hr-mcp, waits for the scan and certifies
// get_leave_balance: READ_ONLY, one attempt, keeping results 10 minutes so
// the runtime can read the output. It returns the tool's id.
func (d *demo) certifyLeaveTool() string {
	d.t.Helper()
	conn := d.must(201, "erin", "POST", "/v1/connectors", map[string]any{"name": "hr-mcp", "protocol": "mcp",
		"endpoint": "http://fakemcp-hr:8091/mcp", "secret_ref": "hr-mcp"})
	tool := d.waitTool(conn["id"].(string), func(t map[string]any) bool { return t["definition"] != nil })
	def := tool["definition"].(map[string]any)
	d.logf("discovered %v: definition #%v, read-only %v", tool["name"], def["seq"], def["read_only"])
	path := "/v1/tools/" + tool["id"].(string)
	c := d.must(201, "erin", "POST", path+"/contracts", map[string]any{"definition_id": def["id"],
		"side_effects": []string{"READ_ONLY"}, "idempotency_mode": "none", "reconciliation_lookup": "none",
		"reconciliation_consistency": "none", "proof_standard": "none", "no_effect_errors": []string{"definition_changed"},
		"max_attempts": 1, "result_retention_seconds": 600})
	d.must(204, "rita", "POST", path+"/contract", map[string]any{"contract_id": c["id"]})
	d.logf("erin certifies it READ_ONLY, one attempt, keeping results for 10 minutes; rita activates the contract")
	return tool["id"].(string)
}

// startRuntime gives agent-runtime a fresh random master and its key, and
// starts it. The master exists only in the runtime's volume and in this
// test's memory (to derive keys for the leak scan).
func (d *demo) startRuntime() *studioruntime.Master {
	d.t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		d.t.Fatal(err)
	}
	secret := hex.EncodeToString(raw)
	file := filepath.Join(d.t.TempDir(), "master")
	if err := os.WriteFile(file, []byte(secret), 0o600); err != nil {
		d.t.Fatal(err)
	}
	master, err := studioruntime.LoadMaster(file, "v1")
	if err != nil {
		d.t.Fatal(err)
	}
	d.mu.Lock()
	key := d.keys["studio-runtime"]
	d.mu.Unlock()
	if err := d.p.startRuntime(map[string][]byte{"master": []byte(secret), "keys": []byte(key + "\n")}); err != nil {
		d.t.Fatalf("start agent-runtime: %v", err)
	}
	d.logf("agent-runtime started: a 32-byte random master (never on disk in the repository) and its key, read-only")
	return master
}

// approveRuntimeKey waits for the runtime's key proposal for version in
// the Studio queue and has rita approve it. It returns the credential id.
func (d *demo) approveRuntimeKey(version string) string {
	d.t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		queue := d.must(200, "rita", "GET", "/v1/studio/requests", nil)
		for _, k := range queue["keys"].([]any) {
			k := k.(map[string]any)
			if k["version_id"] == version && k["proposed_by_runtime"] == true {
				d.logf("rita's queue shows the runtime's key for %v v%v (tools %v, master %v); she approves it",
					k["agent_name"], k["version"], k["capability"], k["master_version"])
				d.must(204, "rita", "POST", "/v1/credentials/"+k["id"].(string)+"/approve", nil)
				return k["id"].(string)
			}
		}
		if time.Now().After(deadline) {
			d.t.Fatalf("the runtime proposed no key: %v", queue)
		}
		time.Sleep(time.Second)
	}
}

// runAs starts a run of agent for employee and returns its id.
func (d *demo) runAs(who, agent, employee string) string {
	d.t.Helper()
	run := d.must(201, who, "POST", "/v1/studio/agents/"+agent+"/runs", map[string]any{"inputs": map[string]string{"employee_id": employee}})
	return run["id"].(string)
}

// waitRun waits until run is finished in state (with reason) and returns it
// as stella sees it.
func (d *demo) waitRun(run, state, reason string) map[string]any {
	d.t.Helper()
	return d.waitRunAs("stella", run, state, reason)
}

// waitRunAs is waitRun as who sees the run.
func (d *demo) waitRunAs(who, run, state, reason string) map[string]any {
	d.t.Helper()
	deadline := time.Now().Add(120 * time.Second)
	for {
		got := d.must(200, who, "GET", "/v1/studio/runs/"+run, nil)
		if s := got["state"]; s == "SUCCEEDED" || s == "FAILED" {
			if s != state || (reason != "" && got["failure_reason"] != reason) {
				d.t.Fatalf("run %s = %v, want %s %s\nruntime log:\n%s", run, got, state, reason, d.p.runtimeLogs())
			}
			return got
		}
		if time.Now().After(deadline) {
			d.t.Fatalf("run %s did not finish: %v\nruntime log:\n%s", run, got, d.p.runtimeLogs())
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// actionState reads action id's state as audra (202 while it is not final).
func (d *demo) actionState(id string) any {
	d.t.Helper()
	code, body := d.call("audra", "GET", "/v1/actions/"+id, nil)
	if code != 200 && code != 202 {
		d.t.Fatalf("GET /v1/actions/%s = %d %v", id, code, body)
	}
	return body["state"]
}

// waitStep waits until run has recorded its first step and returns the
// step's action id.
func (d *demo) waitStep(run string) string {
	d.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		got := d.must(200, "stella", "GET", "/v1/studio/runs/"+run, nil)
		if steps, _ := got["steps"].([]any); len(steps) == 1 {
			return steps[0].(map[string]any)["action_id"].(string)
		}
		if time.Now().After(deadline) {
			d.t.Fatalf("run %s recorded no step: %v\nruntime log:\n%s", run, got, d.p.runtimeLogs())
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (d *demo) hrCalls() []mcpCall {
	d.t.Helper()
	token, err := os.ReadFile(filepath.Join(d.root, "deployments", "docker", "secrets", "fakemcp-hr-token.dev"))
	if err != nil {
		d.t.Fatal(err)
	}
	out, err := d.p.hrCalls(strings.TrimSpace(string(token)))
	if err != nil {
		d.t.Fatalf("reading the HR MCP call log: %v", err)
	}
	var calls []mcpCall
	if err := json.Unmarshal(out, &calls); err != nil {
		d.t.Fatalf("HR MCP call log: %v", err)
	}
	return calls
}

// studioScan looks for the master, every derived Studio key and the
// runtime's key in every API response, service log (the runtime's too) and
// the database dump, and for the answer in the logs and the journal.
func (d *demo) studioScan(master *studioruntime.Master, approved string) {
	d.t.Helper()
	d.mu.Lock()
	runtimeKey := d.keys["studio-runtime"]
	responses := strings.Join(d.responses, "\n")
	d.mu.Unlock()
	secrets := map[string]string{"the runtime's key": runtimeKey}
	for _, r := range master.Redactions() {
		secrets["the master"] = r
	}
	ids := strings.Fields(d.sql(`SELECT string_agg(id::text, ' ') FROM eacp.studio_credentials WHERE tenant_id = '` + d.tenant + `'`))
	if len(ids) == 0 || !strings.Contains(strings.Join(ids, " "), approved) {
		d.t.Fatalf("studio credentials = %v", ids)
	}
	for _, id := range ids {
		key, _ := master.Key(uuid.MustParse(d.tenant), uuid.MustParse(id))
		secrets["derived key "+id[:8]] = key
		secrets["derived secret "+id[:8]] = strings.SplitN(key, "_", 5)[4] // the secret alphabet contains '_'
	}
	logs := d.p.logs() + d.p.runtimeLogs()
	dump, err := d.p.postgres("pg_dump", "-U", "postgres", "--data-only", "eacp")
	if err != nil {
		d.t.Fatalf("pg_dump: %v", err)
	}
	for name, secret := range secrets {
		for where, text := range map[string]string{"an API response": responses, "a service log": logs, "the database": dump} {
			if strings.Contains(text, secret) {
				d.t.Fatalf("%s appears in %s", name, where)
			}
		}
	}
	d.logf("%d secrets (the master, %d derived keys and secrets, the runtime's key): none in %d API responses, %d log lines or a %d-byte database dump",
		len(secrets), 2*len(ids), strings.Count(responses, "\n")+1, strings.Count(logs, "\n"), len(dump))
	if strings.Contains(logs, leaveAnswer) {
		d.t.Fatal("the answer appears in a service log")
	}
	if n := d.sql(`SELECT count(*) FROM eacp.audit_events WHERE tenant_id = '` + d.tenant +
		`' AND convert_from(payload, 'UTF8') LIKE '%` + leaveAnswer + `%'`); n != "0" {
		d.t.Fatalf("the answer is journaled %s times", n)
	}
	d.logf("the answer is in no log line and no journal entry")
}
