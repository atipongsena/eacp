package api_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/identity"
)

// studioDefinition is the leave-balance agent of the 27a-1 spec.
const studioDefinition = `{"schema_version": 1, "kind": "agent",
 "inputs": {"employee_id": {"type": "string", "max_length": 64}},
 "steps": [
   {"id": "lookup", "kind": "tool_call", "tool": "hr-mcp.get_leave_balance", "tool_schema_version": "1",
    "operation": "lookup", "target": "hr", "resource": "leave_balance",
    "payload": {"employee_id": "{{inputs.employee_id}}"}},
   {"id": "answer", "kind": "respond",
    "text": "You have {{steps.lookup.output.structuredContent.days}} days of leave left."}],
 "limits": {"timeout_seconds": 300}}`

// studioHarness adds Studio's cast to the API harness: stella and sid
// (studio_author) and abe (studio_author and registry_approver), all in the
// hr department, and rt, the runtime's service principal.
func studioHarness(t *testing.T) (*harness, uuid.UUID) {
	t.Helper()
	h := newHarness(t)
	for name, roles := range map[string][]string{
		"stella": {"studio_author"}, "sid": {"studio_author"}, "abe": {"studio_author", "registry_approver"},
	} {
		id := h.f.AddPrincipal(t, name, "human", roles...)
		h.keys[name] = h.issue(identity.KindPrincipal, id, "alice", "bob")
	}
	h.f.AddPrincipal(t, "rt", "service", "studio_runtime")
	hr := h.f.ID(t, "alice", `INSERT INTO eacp.groups (tenant_id, name, display_name)
		VALUES (eacp.current_tenant_id(), 'hr', 'HR') RETURNING id`)
	for _, m := range []string{"stella", "sid", "abe"} {
		h.f.ID(t, "alice", `INSERT INTO eacp.group_memberships (tenant_id, group_id, principal_id)
			VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, hr, h.f.P[m])
	}
	h.f.ActiveTool(t, "hr-mcp", "get_leave_balance")
	return h, hr
}

func newStudioAgent(name string, dept uuid.UUID, def string) map[string]any {
	return map[string]any{"name": name, "display_name": "Leave balance", "description": "Days of leave left.",
		"department_id": dept, "definition": json.RawMessage(def)}
}

func TestStudioAuthorsSaveAndApproversDecide(t *testing.T) {
	h, hr := studioHarness(t)

	code, body := h.as("erin", "POST", "/v1/studio/agents", newStudioAgent("leave-bot", hr, studioDefinition))
	h.want(403, code, body)
	code, v1 := h.as("stella", "POST", "/v1/studio/agents", newStudioAgent("leave-bot", hr, studioDefinition))
	h.want(201, code, v1)
	if v1["status"] != "waiting_for_approval" || v1["waiting_on"] != "registry_approver" || v1["version"] != float64(1) ||
		len(str(v1, "digest")) != 64 || v1["capability"].([]any)[0] != "hr-mcp.get_leave_balance" {
		t.Fatalf("saved = %v", v1)
	}
	id, agent := str(v1, "id"), str(v1, "agent_id")

	// Invalid definitions name the rule.
	for fragment, def := range map[string]string{
		"schema_version":             strings.Replace(studioDefinition, `"schema_version": 1`, `"schema_version": 3`, 1),
		"definition_contains_secret": strings.Replace(studioDefinition, `"target": "hr"`, `"target": "Bearer abc"`, 1),
		"definition":                 `{"kind": `,
	} {
		code, body = h.as("stella", "POST", "/v1/studio/agents", newStudioAgent("bad-bot", hr, def))
		h.want(400, code, body)
		if !strings.Contains(str(body, "detail"), fragment) && !strings.Contains(str(body, "detail"), "body") {
			t.Fatalf("detail %q, want %q", body["detail"], fragment)
		}
	}
	code, body = h.as("stella", "POST", "/v1/studio/agents", newStudioAgent("leave-bot", hr, studioDefinition))
	h.want(409, code, body)

	// The author sees their agent; another author sees neither it nor its versions.
	code, list := h.as("stella", "GET", "/v1/studio/agents", nil)
	h.want(200, code, list)
	agents, _ := list["agents"].([]any)
	if len(agents) != 1 || agents[0].(map[string]any)["latest"].(map[string]any)["status"] != "waiting_for_approval" {
		t.Fatalf("agents = %v", list)
	}
	code, list = h.as("sid", "GET", "/v1/studio/agents", nil)
	h.want(200, code, list)
	if agents, _ := list["agents"].([]any); len(agents) != 0 {
		t.Fatalf("sid sees %v", list)
	}
	code, body = h.as("sid", "GET", "/v1/studio/versions/"+id, nil)
	h.want(404, code, body)
	code, body = h.as("sid", "POST", "/v1/studio/agents/"+agent+"/versions", map[string]any{"definition": json.RawMessage(studioDefinition)})
	h.want(404, code, body)
	for _, who := range []string{"rita", "audra"} {
		code, body = h.as(who, "GET", "/v1/studio/versions/"+id, nil)
		h.want(200, code, body)
		if def, _ := body["definition"].(map[string]any); def["kind"] != "agent" {
			t.Fatalf("%s reads %v", who, body)
		}
	}
	code, body = h.as("carol", "GET", "/v1/studio/versions/"+id, nil)
	h.want(403, code, body)

	// The approver's queue describes the tools in plain words.
	code, body = h.as("stella", "GET", "/v1/studio/requests", nil)
	h.want(403, code, body)
	code, queue := h.as("rita", "GET", "/v1/studio/requests", nil)
	h.want(200, code, queue)
	requests, _ := queue["requests"].([]any)
	if len(requests) != 1 {
		t.Fatalf("requests = %v", queue)
	}
	tool := requests[0].(map[string]any)["tools"].([]any)[0].(map[string]any)
	if tool["ref"] != "hr-mcp.get_leave_balance" || tool["protocol"] != "http" ||
		tool["in_plain_words"] != "reads data and changes nothing" {
		t.Fatalf("request tool = %v", tool)
	}

	// Decisions: the owner never decides; a reason is required; once only.
	code, body = h.as("stella", "POST", "/v1/studio/versions/"+id+"/approve", map[string]any{"reason": "mine"})
	h.want(403, code, body)
	code, own := h.as("abe", "POST", "/v1/studio/agents", newStudioAgent("abe-bot", hr, studioDefinition))
	h.want(201, code, own)
	code, body = h.as("abe", "POST", "/v1/studio/versions/"+str(own, "id")+"/approve", map[string]any{"reason": "mine"})
	h.want(409, code, body)
	code, body = h.as("rita", "POST", "/v1/studio/versions/"+id+"/approve", map[string]any{})
	h.want(400, code, body)
	code, body = h.as("rita", "POST", "/v1/studio/versions/not-a-uuid/approve", map[string]any{"reason": "ok"})
	h.want(400, code, body)
	code, body = h.as("rita", "POST", "/v1/studio/versions/"+uuid.NewString()+"/approve", map[string]any{"reason": "ok"})
	h.want(404, code, body)
	code, body = h.as("rita", "POST", "/v1/studio/versions/"+id+"/approve", map[string]any{"reason": "read-only tool"})
	h.want(200, code, body)
	if body["status"] != "waiting_for_credential" || body["waiting_on"] != "studio_runtime" || body["decision"] != "approved" {
		t.Fatalf("approved = %v", body)
	}
	code, body = h.as("ravi", "POST", "/v1/studio/versions/"+id+"/reject", map[string]any{"reason": "late"})
	h.want(409, code, body)

	// The runtime proposes a key; an approver approves it; the version is ready.
	cred := h.f.ID(t, "rt", `INSERT INTO eacp.credentials (tenant_id, id, kind, agent_version_id, secret_hash, expires_at)
		VALUES (eacp.current_tenant_id(), gen_random_uuid(), 'ak', $1, decode(repeat('ab', 32), 'hex'),
		        now() + interval '90 days') RETURNING id`, id)
	status := func(version string) (string, string) {
		t.Helper()
		code, body := h.as("stella", "GET", "/v1/studio/versions/"+version, nil)
		h.want(200, code, body)
		return str(body, "status"), str(body, "waiting_on")
	}
	if s, w := status(id); s != "waiting_for_credential" || w != "registry_approver" {
		t.Fatalf("proposed = %s %s", s, w)
	}
	if err := h.f.Exec("ravi", `UPDATE eacp.credentials SET approved_at = now() WHERE id = $1`, cred); err != nil {
		t.Fatal(err)
	}
	if s, w := status(id); s != "ready" || w != "" {
		t.Fatalf("ready = %s %s", s, w)
	}

	// A rejected version, then a replacement.
	next := func() string {
		t.Helper()
		code, body := h.as("stella", "POST", "/v1/studio/agents/"+agent+"/versions",
			map[string]any{"definition": json.RawMessage(studioDefinition)})
		h.want(201, code, body)
		return str(body, "id")
	}
	v2 := next()
	code, body = h.as("rita", "POST", "/v1/studio/versions/"+v2+"/reject", map[string]any{"reason": "use the portal"})
	h.want(200, code, body)
	if body["status"] != "rejected" || body["decision_reason"] != "use the portal" {
		t.Fatalf("rejected = %v", body)
	}
	v3 := next()
	code, body = h.as("ravi", "POST", "/v1/studio/versions/"+v3+"/approve", map[string]any{"reason": "ok"})
	h.want(200, code, body)
	if s, _ := status(id); s != "replaced" {
		t.Fatalf("v1 = %s", s)
	}
	if err := h.f.Exec("otto", `UPDATE eacp.agent_versions SET state = 'SUSPENDED', state_reason = 'pause' WHERE id = $1`, v3); err != nil {
		t.Fatal(err)
	}
	if s, _ := status(v3); s != "suspended" {
		t.Fatalf("v3 = %s", s)
	}
	code, list = h.as("rita", "GET", "/v1/studio/agents", nil)
	h.want(200, code, list)
	if agents, _ := list["agents"].([]any); len(agents) != 2 {
		t.Fatalf("approver sees %v", list)
	}
}

// TestMeListsTheCallersGroups: /v1/me names the caller's active groups, so
// the Studio page can offer the departments an author may save into (Phase
// 27a-3b). A removed membership disappears; nobody sees another's groups.
func TestMeListsTheCallersGroups(t *testing.T) {
	h, hr := studioHarness(t)
	code, me := h.as("stella", "GET", "/v1/me", nil)
	h.want(200, code, me)
	groups, _ := me["groups"].([]any)
	if len(groups) != 1 {
		t.Fatalf("stella's groups = %v", me["groups"])
	}
	g := groups[0].(map[string]any)
	if g["id"] != hr.String() || g["name"] != "hr" || g["display_name"] != "HR" {
		t.Fatalf("group = %v", g)
	}
	code, me = h.as("carol", "GET", "/v1/me", nil)
	h.want(200, code, me)
	if groups, ok := me["groups"].([]any); !ok || len(groups) != 0 {
		t.Fatalf("carol's groups = %v", me["groups"])
	}
	m := h.f.ID(t, "alice", `SELECT id FROM eacp.group_memberships WHERE group_id = $1 AND principal_id = $2`,
		hr, h.f.P["stella"])
	code, body := h.as("alice", "POST", "/v1/group-memberships/"+m.String()+"/remove", map[string]any{"reason": "moved"})
	h.want(204, code, body)
	code, me = h.as("stella", "GET", "/v1/me", nil)
	h.want(200, code, me)
	if groups, _ := me["groups"].([]any); len(groups) != 0 {
		t.Fatalf("stella's groups after removal = %v", me["groups"])
	}
}
