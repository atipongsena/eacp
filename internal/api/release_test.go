package api_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/api"
	"github.com/atipongsena/eacp/internal/identity"
	"github.com/atipongsena/eacp/internal/registry/registrytest"
	"github.com/atipongsena/eacp/internal/release"
)

func TestReleaseAPI(t *testing.T) {
	pdp := &switchPDP{}
	h := newHarness(t, func(f *registrytest.Fixture, s *api.Server) {
		s.WithReleases(release.New(f.App, release.Options{Provider: pdp}))
	})
	tool := h.f.ActiveTool(t, "erp", "purchase")
	h.f.ActivatePolicy(t, registrytest.AllowPolicy)
	stable := h.f.ActiveAgent(t, "buyer", tool.Tool)
	candidate := h.f.ID(t, "erin", `INSERT INTO eacp.agent_versions (tenant_id, agent_id, runtime, code_ref)
		VALUES (eacp.current_tenant_id(), $1, 'python', 'git:next') RETURNING id`, stable.Agent)
	al := h.f.ID(t, "erin", `INSERT INTO eacp.agent_allowlists (tenant_id, agent_version_id, tool_ids)
		VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, candidate, []uuid.UUID{tool.Tool})
	if err := h.f.Exec("rita", `UPDATE eacp.agent_versions SET active_allowlist_id = $1 WHERE id = $2`, al, candidate); err != nil {
		t.Fatal(err)
	}
	candidateKey := h.issue(identity.KindAgent, candidate, "erin", "rita")
	stableKey := h.issue(identity.KindAgent, stable.Version, "erin", "rita")

	open := map[string]any{"candidate_version_id": candidate, "reason": "model upgrade",
		"required_suites": []string{"accuracy"}, "min_replay_cases": 1, "min_shadow_cases": 0,
		"canary_steps": []int{10000}, "min_canary_actions": 1}
	code, body := h.as("otto", "POST", "/v1/releases", open)
	h.want(403, code, body)
	code, body = h.as("erin", "POST", "/v1/releases", map[string]any{"candidate_version_id": candidate,
		"reason": "x", "required_suites": []string{"accuracy"}, "max_cost_ratio": "1e3"})
	h.want(400, code, body)
	code, rel := h.as("erin", "POST", "/v1/releases", open)
	h.want(201, code, rel)
	id := str(rel, "id")
	if rel["state"] != "EVALUATING" || str(rel, "stable_version_id") != stable.Version.String() ||
		rel["min_replay_agreement"] != 0.9 {
		t.Fatalf("release = %v", rel)
	}
	path := "/v1/releases/" + id

	code, body = h.as("erin", "POST", path+"/evaluations", map[string]any{"suite": "accuracy", "score": "0.95",
		"threshold": "0.9", "dataset_digest": fill64("cd"), "evidence_ref": "ci://run/1"})
	h.want(201, code, body)
	if body["passed"] != true {
		t.Fatalf("evaluation = %v", body)
	}

	// Replay through the agent route; a stable version key cannot observe.
	ref := h.f.QueuedAction(t, stable.Version, "carol", "erp.purchase")
	if err := h.f.ExecAgent(stable.Version, `UPDATE eacp.actions SET state = 'CANCELLED', state_reason = 'x'
		WHERE id = $1`, ref); err != nil {
		t.Fatal(err)
	}
	proposal := map[string]any{"kind": "replay", "reference_action_id": ref, "subject": "carol@tenant-a.test",
		"operation": "purchase", "target": "erp", "tool": "erp.purchase", "tool_schema_version": "1",
		"resource": "po", "payload": map[string]any{"currency": "THB", "amount": 1000000}}
	code, body = h.do(stableKey, "POST", "/v1/agent/release/observations", proposal)
	h.want(403, code, body)
	code, body = h.as("erin", "POST", "/v1/agent/release/observations", proposal)
	h.want(403, code, body)
	pdp.down.Store(true)
	code, body = h.do(candidateKey, "POST", "/v1/agent/release/observations", proposal)
	h.want(503, code, body)
	if body["error"] != "governance_unavailable" {
		t.Fatalf("outage = %v", body)
	}
	pdp.down.Store(false)
	code, obs := h.do(candidateKey, "POST", "/v1/agent/release/observations", proposal)
	h.want(201, code, obs)
	if obs["verdict"] != "allow" || obs["agrees"] != true || obs["recorded"] == nil {
		t.Fatalf("observation = %v", obs)
	}

	// Advancing: only a registry approver, naming the reviewed state.
	code, body = h.as("erin", "POST", path+"/advance", map[string]any{"from": "EVALUATING", "reason": "ok"})
	h.want(403, code, body)
	code, body = h.as("ravi", "POST", path+"/advance", map[string]any{"from": "SHADOW", "reason": "ok"})
	h.want(409, code, body)
	code, body = h.as("ravi", "POST", path+"/advance", map[string]any{"from": "EVALUATING", "reason": "replay agrees"})
	h.want(200, code, body)
	code, body = h.as("ravi", "POST", path+"/advance", map[string]any{"from": "SHADOW", "reason": "no shadow required"})
	h.want(200, code, body)
	if body["state"] != "CANARY" || body["canary_bp"] != float64(10000) {
		t.Fatalf("canary = %v", body)
	}

	code, route := h.do(candidateKey, "GET", "/v1/agent/release/route?subject=carol@tenant-a.test", nil)
	h.want(200, code, route)
	if route["cohort"] != "canary" || str(route, "version_id") != candidate.String() {
		t.Fatalf("route = %v", route)
	}
	code, body = h.do(candidateKey, "GET", "/v1/agent/release/route", nil)
	h.want(400, code, body)

	code, detail := h.as("audra", "GET", path, nil)
	h.want(200, code, detail)
	if evals, _ := detail["evaluations"].([]any); len(evals) != 1 || detail["canary"] == nil {
		t.Fatalf("detail = %v", detail)
	}
	code, body = h.as("carol", "GET", path, nil)
	h.want(403, code, body)
	code, body = h.as("audra", "GET", "/v1/releases?state=CANARY&agent_id="+stable.Agent.String(), nil)
	h.want(200, code, body)
	if list, _ := body["releases"].([]any); len(list) != 1 {
		t.Fatalf("list = %v", body)
	}
	code, body = h.as("audra", "GET", "/v1/releases?agent_id=buyer", nil)
	h.want(400, code, body)

	// An operator contains: the rollback suspends the candidate.
	code, body = h.as("erin", "POST", path+"/rollback", map[string]any{"reason": "x"})
	h.want(403, code, body)
	code, body = h.as("otto", "POST", path+"/rollback", map[string]any{"reason": "latency spike"})
	h.want(200, code, body)
	if body["state"] != "ROLLED_BACK" {
		t.Fatalf("rollback = %v", body)
	}
	code, body = h.as("otto", "POST", path+"/rollback", map[string]any{"reason": "again"})
	h.want(409, code, body)
	// The cohort goes back to the stable version.
	code, route = h.do(candidateKey, "GET", "/v1/agent/release/route?subject=carol@tenant-a.test", nil)
	h.want(200, code, route)
	if route["cohort"] != "stable" || str(route, "version_id") != stable.Version.String() {
		t.Fatalf("route after rollback = %v", route)
	}
}

func fill64(pair string) string {
	out := ""
	for range 32 {
		out += pair
	}
	return out
}
