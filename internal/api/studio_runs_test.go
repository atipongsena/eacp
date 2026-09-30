package api_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/identity"
	"github.com/atipongsena/eacp/internal/registry/registrytest"
)

// studioRunHarness is studioHarness with the runtime's key and stella's
// leave-bot approved by rita. It returns the agent and version ids.
func studioRunHarness(t *testing.T) (*harness, string, string) {
	t.Helper()
	h, hr := studioHarness(t)
	h.keys["rt"] = h.issue(identity.KindPrincipal, h.f.P["rt"], "alice", "bob")
	code, v := h.as("stella", "POST", "/v1/studio/agents", newStudioAgent("leave-bot", hr, studioDefinition))
	h.want(201, code, v)
	code, body := h.as("rita", "POST", "/v1/studio/versions/"+str(v, "id")+"/approve", map[string]any{"reason": "ok"})
	h.want(200, code, body)
	return h, str(v, "agent_id"), str(v, "id")
}

func TestStudioRunsThroughTheAPI(t *testing.T) {
	h, agent, version := studioRunHarness(t)
	runs := "/v1/studio/agents/" + agent + "/runs"

	code, body := h.as("carol", "POST", runs, map[string]any{"inputs": map[string]string{"employee_id": "E-1"}})
	h.want(403, code, body)
	code, body = h.as("stella", "POST", runs, map[string]any{"inputs": map[string]string{"nobody": "x"}})
	h.want(400, code, body)
	code, run := h.as("stella", "POST", runs, map[string]any{"inputs": map[string]string{"employee_id": "E-1"}})
	h.want(201, code, run)
	id := str(run, "id")
	if run["state"] != "QUEUED" || str(run, "version_id") != version {
		t.Fatalf("run = %v", run)
	}

	// The runtime claims it: no key yet.
	claim := map[string]any{"runtime_id": "r1", "master_version": "v1", "lease_seconds": 30, "limit": 5}
	code, body = h.as("stella", "POST", "/v1/studio/runtime/claims", claim)
	h.want(403, code, body)
	code, claimed := h.as("rt", "POST", "/v1/studio/runtime/claims", claim)
	h.want(200, code, claimed)
	list, _ := claimed["runs"].([]any)
	if len(list) != 1 {
		t.Fatalf("claimed = %v", claimed)
	}
	c := list[0].(map[string]any)
	if c["id"] != id || c["credential"] != "pending" || c["subject"] != "stella@tenant-a.test" || c["generation"] != float64(1) {
		t.Fatalf("claim = %v", c)
	}

	// It proposes a key; the approver sees it in the Studio queue.
	code, due := h.as("rt", "GET", "/v1/studio/runtime/credentials?master_version=v1", nil)
	h.want(200, code, due)
	if versions, _ := due["due"].([]any); len(versions) != 1 || versions[0] != version {
		t.Fatalf("due = %v", due)
	}
	sum := sha256.Sum256([]byte("a derived secret"))
	key := uuid.NewString()
	proposal := map[string]any{"version_id": version, "id": key, "hash": hex.EncodeToString(sum[:]), "master_version": "v1"}
	code, body = h.as("erin", "POST", "/v1/studio/runtime/credentials", proposal)
	h.want(403, code, body)
	code, body = h.as("rt", "POST", "/v1/studio/runtime/credentials", proposal)
	h.want(204, code, body)
	code, body = h.as("rt", "POST", "/v1/studio/runtime/credentials", proposal)
	h.want(409, code, body)
	code, queue := h.as("rita", "GET", "/v1/studio/requests", nil)
	h.want(200, code, queue)
	keys, _ := queue["keys"].([]any)
	if len(keys) != 1 {
		t.Fatalf("key requests = %v", queue)
	}
	k := keys[0].(map[string]any)
	if k["id"] != key || k["version_id"] != version || k["master_version"] != "v1" || k["proposed_by_runtime"] != true ||
		k["agent_name"] != "leave-bot" || len(str(k, "digest")) != 64 {
		t.Fatalf("key request = %v", k)
	}

	// The step's action, then the finish.
	a := h.f.AgentID(t, uuid.MustParse(version), registrytest.ReceivedActionSQL, uuid.MustParse(version),
		"studio:"+id+":0", "stella@tenant-a.test", "hr-mcp.get_leave_balance")
	runtime := "/v1/studio/runtime/runs/" + id
	code, body = h.as("rt", "POST", runtime+"/heartbeat", map[string]any{"runtime_id": "r1", "generation": 1, "lease_seconds": 30})
	h.want(200, code, body)
	if body["version_active"] != true {
		t.Fatalf("heartbeat = %v", body)
	}
	code, body = h.as("rt", "POST", runtime+"/steps", map[string]any{"runtime_id": "r2", "generation": 1, "index": 0, "action_id": a})
	h.want(403, code, body)
	code, body = h.as("rt", "POST", runtime+"/steps", map[string]any{"runtime_id": "r1", "generation": 1, "index": 0, "action_id": a})
	h.want(204, code, body)
	code, body = h.as("rt", "POST", runtime+"/finish", map[string]any{"runtime_id": "r1", "generation": 1,
		"state": "SUCCEEDED", "answer": "You have 12 days of leave left."})
	h.want(204, code, body)

	// Only the requester reads the answer; readers see the rest; others see nothing.
	code, got := h.as("stella", "GET", "/v1/studio/runs/"+id, nil)
	h.want(200, code, got)
	steps, _ := got["steps"].([]any)
	if got["state"] != "SUCCEEDED" || got["answer"] != "You have 12 days of leave left." || len(steps) != 1 ||
		steps[0].(map[string]any)["action_id"] != a.String() || str(got, "answer_expires_at") == "" {
		t.Fatalf("requester sees %v", got)
	}
	for _, who := range []string{"rita", "otto", "audra"} {
		code, got = h.as(who, "GET", "/v1/studio/runs/"+id, nil)
		h.want(200, code, got)
		if _, ok := got["answer"]; ok || got["state"] != "SUCCEEDED" {
			t.Fatalf("%s sees %v", who, got)
		}
	}
	for _, who := range []string{"sid", "carol"} {
		code, got = h.as(who, "GET", "/v1/studio/runs/"+id, nil)
		h.want(404, code, got)
	}
	code, got = h.as("stella", "GET", "/v1/studio/runs/not-a-uuid", nil)
	h.want(400, code, got)
}

func TestRuntimeRoutesAreForTheRuntimeOnly(t *testing.T) {
	h, _, _ := studioRunHarness(t)
	run := "/v1/studio/runtime/runs/" + uuid.NewString()
	for _, route := range []struct{ method, path string }{
		{"POST", "/v1/studio/runtime/claims"}, {"POST", run + "/heartbeat"}, {"POST", run + "/steps"},
		{"POST", run + "/finish"}, {"GET", "/v1/studio/runtime/credentials?master_version=v1"},
		{"POST", "/v1/studio/runtime/credentials"},
	} {
		for _, who := range []string{"stella", "rita", "alice", "otto"} {
			code, body := h.as(who, route.method, route.path, map[string]any{})
			if code != 403 {
				t.Fatalf("%s %s %s = %d %v", who, route.method, route.path, code, body)
			}
		}
	}
	// The runtime is refused everything else.
	for _, route := range []struct{ method, path string }{
		{"POST", "/v1/studio/agents"}, {"GET", "/v1/studio/requests"}, {"POST", "/v1/agents"},
		{"POST", "/v1/studio/credentials/revoke-all"},
	} {
		code, body := h.as("rt", route.method, route.path, map[string]any{})
		if code != 403 {
			t.Fatalf("rt %s %s = %d %v", route.method, route.path, code, body)
		}
	}
	code, body := h.as("rt", "GET", "/v1/studio/runtime/credentials", nil)
	h.want(400, code, body)
}

func TestOperatorsRevokeEveryStudioKey(t *testing.T) {
	h, _, version := studioRunHarness(t)
	sum := sha256.Sum256([]byte("secret"))
	code, body := h.as("rt", "POST", "/v1/studio/runtime/credentials", map[string]any{"version_id": version,
		"id": uuid.NewString(), "hash": hex.EncodeToString(sum[:]), "master_version": "v1"})
	h.want(204, code, body)
	code, body = h.as("rita", "POST", "/v1/studio/credentials/revoke-all", map[string]any{"reason": "compromise"})
	h.want(403, code, body)
	code, body = h.as("otto", "POST", "/v1/studio/credentials/revoke-all", map[string]any{})
	h.want(400, code, body)
	code, body = h.as("otto", "POST", "/v1/studio/credentials/revoke-all", map[string]any{"reason": "suspected runtime compromise"})
	h.want(200, code, body)
	if body["revoked"] != float64(1) {
		t.Fatalf("revoke-all = %v", body)
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "hash") {
		t.Fatal("hash in the response")
	}
}
