package api_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/identity"
)

// hubHarness is studioRunHarness plus the Hub's cast: lena leads hr (added
// through the API with lead), hank is in hr with no role, and fran
// (studio_author) is in finance. It returns the agent and version ids and
// the two groups.
func hubHarness(t *testing.T) (h *harness, agent, version string, hr, finance uuid.UUID) {
	t.Helper()
	h, agent, version = studioRunHarness(t)
	hr = h.f.ID(t, "alice", `SELECT id FROM eacp.groups WHERE name = 'hr'`)
	finance = h.f.ID(t, "alice", `INSERT INTO eacp.groups (tenant_id, name, display_name)
		VALUES (eacp.current_tenant_id(), 'finance', 'Finance') RETURNING id`)
	for name, roles := range map[string][]string{"lena": nil, "hank": nil, "fran": {"studio_author"}} {
		id := h.f.AddPrincipal(t, name, "human", roles...)
		h.keys[name] = h.issue(identity.KindPrincipal, id, "alice", "bob")
	}
	code, body := h.as("rita", "POST", "/v1/groups/"+hr.String()+"/members", map[string]any{"principal_id": h.f.P["lena"], "lead": true})
	h.want(403, code, body)
	for who, g := range map[string]uuid.UUID{"lena": hr, "hank": hr, "fran": finance} {
		code, body = h.as("alice", "POST", "/v1/groups/"+g.String()+"/members",
			map[string]any{"principal_id": h.f.P[who], "lead": who == "lena"})
		h.want(201, code, body)
	}
	return h, agent, version, hr, finance
}

func TestALeadIsAddedThroughTheAPIAndShownInMe(t *testing.T) {
	h, _, _, _, _ := hubHarness(t)
	for who, lead := range map[string]bool{"lena": true, "hank": false} {
		code, me := h.as(who, "GET", "/v1/me", nil)
		h.want(200, code, me)
		groups, _ := me["groups"].([]any)
		if len(groups) != 1 || groups[0].(map[string]any)["lead"] != lead {
			t.Fatalf("%s's groups = %v", who, me["groups"])
		}
	}
}

func TestTheHubThroughTheAPI(t *testing.T) {
	h, agent, version, hr, finance := hubHarness(t)
	runs := "/v1/studio/agents/" + agent + "/runs"
	run := map[string]any{"inputs": map[string]string{"employee_id": "E-1"}}
	listing := "/v1/studio/agents/" + agent + "/listing"

	code, body := h.as("hank", "POST", runs, run)
	h.want(403, code, body)
	code, own := h.as("stella", "GET", listing, nil)
	h.want(200, code, own)
	if own["listing"] != nil || own["proposal"] != nil {
		t.Fatalf("before a proposal = %v", own)
	}
	code, body = h.as("sid", "GET", listing, nil)
	h.want(404, code, body)

	// Only the owner proposes.
	propose := map[string]any{"version_id": version, "scope": "DEPARTMENT", "tags": []string{"leave"}, "note": "for HR"}
	code, body = h.as("hank", "POST", listing, propose)
	h.want(403, code, body)
	code, body = h.as("abe", "POST", listing, propose)
	h.want(403, code, body)
	code, body = h.as("stella", "POST", listing, map[string]any{"version_id": version, "scope": "TEAM", "tags": []string{}})
	h.want(400, code, body)
	code, p := h.as("stella", "POST", listing, propose)
	h.want(201, code, p)
	if p["scope"] != "DEPARTMENT" || p["decision"] != nil || str(p, "version_id") != version || p["note"] != "for HR" {
		t.Fatalf("proposal = %v", p)
	}
	code, body = h.as("stella", "POST", listing, propose)
	h.want(409, code, body)

	// Only a lead of hr sees it to decide; nobody decides their own.
	for who, want := range map[string]int{"lena": 1, "rita": 0, "stella": 0, "hank": 0} {
		code, q := h.as(who, "GET", "/v1/studio/listing-requests", nil)
		h.want(200, code, q)
		if got, _ := q["proposals"].([]any); len(got) != want {
			t.Fatalf("%s's queue = %v", who, q)
		}
		if want == 1 {
			got := q["proposals"].([]any)[0].(map[string]any)
			tools, _ := got["tools"].([]any)
			if got["agent_name"] != "leave-bot" || got["department_name"] != "HR" || len(tools) != 1 {
				t.Fatalf("queued = %v", got)
			}
		}
	}
	decide := "/v1/studio/listing-proposals/" + str(p, "id")
	code, body = h.as("stella", "POST", decide+"/approve", map[string]any{"reason": "mine"})
	h.want(409, code, body)
	code, body = h.as("rita", "POST", decide+"/approve", map[string]any{"reason": "ok"})
	h.want(403, code, body)
	code, body = h.as("lena", "POST", decide+"/approve", map[string]any{"reason": ""})
	h.want(400, code, body)
	code, d := h.as("lena", "POST", decide+"/approve", map[string]any{"reason": "useful for HR"})
	h.want(200, code, d)
	if d["state"] != "PUBLISHED" {
		t.Fatalf("decision = %v", d)
	}

	// hr sees and runs it; finance sees nothing.
	code, hub := h.as("hank", "GET", "/v1/studio/hub", nil)
	h.want(200, code, hub)
	list, _ := hub["listings"].([]any)
	if len(list) != 1 {
		t.Fatalf("hank's hub = %v", hub)
	}
	l := list[0].(map[string]any)
	if l["name"] != "leave-bot" || l["scope"] != "DEPARTMENT" || l["runnable"] != true || l["department_name"] != "HR" {
		t.Fatalf("listing = %v", l)
	}
	id := str(l, "id")
	for query, want := range map[string]int{"?q=LEAVE": 1, "?q=payroll": 0, "?tag=leave": 1, "?tag=nope": 0,
		"?department=" + hr.String(): 1, "?department=" + finance.String(): 0} {
		code, got := h.as("hank", "GET", "/v1/studio/hub"+query, nil)
		h.want(200, code, got)
		if n, _ := got["listings"].([]any); len(n) != want {
			t.Fatalf("%s = %v", query, got)
		}
	}
	code, body = h.as("hank", "GET", "/v1/studio/hub?department=nope", nil)
	h.want(400, code, body)
	code, one := h.as("hank", "GET", "/v1/studio/hub/"+id, nil)
	h.want(200, code, one)
	if def, _ := one["definition"].(map[string]any); def["kind"] != "agent" || len(one["tools"].([]any)) != 1 {
		t.Fatalf("hub listing = %v", one)
	}
	code, body = h.as("fran", "GET", "/v1/studio/hub/"+id, nil)
	h.want(404, code, body)
	code, hub = h.as("fran", "GET", "/v1/studio/hub", nil)
	h.want(200, code, hub)
	if n, _ := hub["listings"].([]any); len(n) != 0 {
		t.Fatalf("fran's hub = %v", hub)
	}
	code, body = h.as("hank", "POST", runs, run)
	h.want(201, code, body)
	code, body = h.as("fran", "POST", runs, run)
	h.want(403, code, body)
	code, own = h.as("stella", "GET", listing, nil)
	h.want(200, code, own)
	if own["listing"].(map[string]any)["state"] != "PUBLISHED" || own["proposal"].(map[string]any)["decision"] != "approved" {
		t.Fatalf("the owner's view = %v", own)
	}

	// A clone: only from a listing its author sees, and it starts unapproved.
	clone := "/v1/studio/listings/" + id + "/clone"
	copyOf := map[string]any{"name": "fran-leave-bot", "display_name": "Leave (finance)", "department_id": finance}
	code, body = h.as("fran", "POST", clone, copyOf)
	h.want(404, code, body)
	code, p = h.as("stella", "POST", listing, map[string]any{"version_id": version, "scope": "ORG", "tags": []string{"leave"}})
	h.want(201, code, p)
	code, body = h.as("lena", "POST", "/v1/studio/listing-proposals/"+str(p, "id")+"/approve", map[string]any{"reason": "wider"})
	h.want(403, code, body)
	code, body = h.as("rita", "POST", "/v1/studio/listing-proposals/"+str(p, "id")+"/approve", map[string]any{"reason": "wider"})
	h.want(200, code, body)
	code, body = h.as("hank", "POST", clone, copyOf)
	h.want(403, code, body)
	code, c := h.as("fran", "POST", clone, copyOf)
	h.want(201, code, c)
	if c["status"] != "waiting_for_approval" || c["agent_name"] != "fran-leave-bot" {
		t.Fatalf("clone = %v", c)
	}

	// Deprecate and withdraw: the owner, an admin or the scope's approver.
	code, body = h.as("lena", "POST", "/v1/studio/listings/"+id+"/deprecate", map[string]any{"reason": "old"})
	h.want(403, code, body)
	code, d = h.as("stella", "POST", "/v1/studio/listings/"+id+"/deprecate", map[string]any{"reason": "a newer agent exists"})
	h.want(200, code, d)
	if d["state"] != "DEPRECATED" {
		t.Fatalf("deprecated = %v", d)
	}
	code, body = h.as("fran", "POST", clone, map[string]any{"name": "late-bot", "display_name": "Late", "department_id": finance})
	h.want(409, code, body)
	code, body = h.as("bob", "POST", "/v1/studio/listings/"+id+"/withdraw", map[string]any{"reason": "tenant policy"})
	h.want(200, code, body)
	code, body = h.as("hank", "POST", runs, run)
	h.want(403, code, body)

	// The proposer cancels an open proposal once.
	code, p = h.as("stella", "POST", listing, propose)
	h.want(201, code, p)
	cancel := "/v1/studio/listing-proposals/" + str(p, "id") + "/cancel"
	code, body = h.as("stella", "POST", cancel, map[string]any{"reason": "not yet"})
	h.want(204, code, body)
	code, body = h.as("stella", "POST", cancel, map[string]any{"reason": "again"})
	h.want(409, code, body)

	for _, path := range []string{"/v1/studio/hub/x", "/v1/studio/listings/x/withdraw", "/v1/studio/listing-proposals/x/approve"} {
		method := "POST"
		if path == "/v1/studio/hub/x" {
			method = "GET"
		}
		code, body = h.as("stella", method, path, map[string]any{"reason": "x"})
		h.want(400, code, body)
	}
}
