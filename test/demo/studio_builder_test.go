package demo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/atipongsena/eacp/internal/studioruntime"
	"github.com/google/uuid"
)

// builderDemo extends the same public-API Studio demo on compose and Kubernetes.
func (d *demo) builderDemo(department string, master *studioruntime.Master) {
	d.t.Helper()
	d.step("S8. Full builder: governed models, fixed branches, previews, recovery and containment")
	policy := d.must(201, "alice", "POST", "/v1/policies", map[string]any{"content": json.RawMessage(`{"format_version":1,"rules":[{"id":"hr","match":{"target":"hr"},"verdict":"allow","reason":"read-only HR"},{"id":"models","match":{"operation":"llm.generate"},"verdict":"allow","reason":"governed generation"}]}`)})
	d.must(204, "bob", "POST", "/v1/policies/"+policy["id"].(string)+"/activate", map[string]any{"reason": "reviewed"})
	models := map[string]string{}
	for name, provider := range map[string]string{"triage": "openai", "triage-anthropic": "anthropic"} {
		m := d.must(201, "erin", "POST", "/v1/llm-models", map[string]any{"name": name, "provider": provider, "base_url": "http://fakellm:8093", "upstream_model": "studio-triage", "secret_ref": "fakellm", "max_output_tokens": 100, "timeout_ms": 90000})
		models[name] = m["id"].(string)
		d.must(201, "alice", "POST", "/v1/finops/prices", map[string]any{"provider": provider, "model": "studio-triage", "unit": "USD", "input_per_mtok": "1", "output_per_mtok": "2", "reason": "development fixture price"})
	}
	save := func(name, fixture, model string) (agent, version, key string, def map[string]any) {
		b, err := os.ReadFile(filepath.Join("testdata", fixture+".json"))
		if err != nil {
			d.t.Fatal(err)
		}
		if err = json.Unmarshal(b, &def); err != nil {
			d.t.Fatal(err)
		}
		for _, raw := range def["steps"].([]any) {
			s := raw.(map[string]any)
			if s["kind"] == "llm" {
				s["model"] = model
			}
		}
		v := d.must(201, "stella", "POST", "/v1/studio/agents", map[string]any{"name": name, "display_name": name, "description": "Full builder demo", "department_id": department, "definition": def})
		agent, version = v["agent_id"].(string), v["id"].(string)
		d.must(200, "rita", "POST", "/v1/studio/versions/"+version+"/approve", map[string]any{"reason": "fixed graph and bounded models"})
		key = d.approveRuntimeKey(version)
		return
	}
	budget := func(agent, name string) {
		acct := d.must(201, "alice", "POST", "/v1/budgets", map[string]any{"name": name, "unit": "USD", "agent_id": agent})
		proposal := d.must(201, "alice", "POST", "/v1/budgets/"+acct["id"].(string)+"/limit", map[string]any{"limit": "1", "reason": "development budget"})
		d.must(200, "bob", "POST", "/v1/budget-limit-changes/"+proposal["id"].(string)+"/approve", map[string]any{"reason": "reviewed"})
	}
	leave, leaveVersion, leaveKey, leaveDef := save("leave-triage", "leave-triage", "triage")
	before := d.studioProviderCalls()
	missing := d.runAs("stella", leave, "E-1")
	d.waitRun(missing, "FAILED", "llm_denied")
	if d.studioProviderCalls() != before || d.sql(`SELECT denial FROM eacp.llm_calls WHERE studio_run_id='`+missing+`'`) != "budget_pending" {
		d.t.Fatal("a Studio model reached the provider without a leaf budget")
	}
	budget(leave, "leave-usd")
	d.assertBuilderAnswer(d.waitRun(d.runAs("stella", leave, "E-1"), "SUCCEEDED", ""), leaveDef, 3)
	procurement, _, procKey, procDef := save("procurement-triage", "procurement-triage", "triage-anthropic")
	budget(procurement, "procurement-usd")
	run := func(request string) string {
		return d.must(201, "stella", "POST", "/v1/studio/agents/"+procurement+"/runs", map[string]any{"inputs": map[string]string{"request": request}})["id"].(string)
	}
	for scenario, index := range map[string]int{"studio_true": 2, "studio_false": 3} {
		d.assertBuilderAnswer(d.waitRun(run(scenario), "SUCCEEDED", ""), procDef, index)
	}
	d.waitRun(run("studio_invalid"), "FAILED", "llm_invalid_output")
	d.logf("both providers complete typed model steps; fixed true/false paths answer; missing budget and malformed output fail closed")

	d.p.stop("agent-runtime")
	toolCalls := len(d.hrCalls())
	before = d.studioProviderCalls()
	preview := d.must(201, "stella", "POST", "/v1/studio/versions/"+leaveVersion+"/previews", map[string]any{"inputs": map[string]string{"employee_id": "E-1"}, "samples": map[string]any{"lookup": map[string]any{"structuredContent": map[string]any{"days": 12}}}})["id"].(string)
	claimedPreview := d.must(200, "studio-runtime", "POST", "/v1/studio/runtime/claims", map[string]any{"runtime_id": "demo-preview", "master_version": "v1", "lease_seconds": 5, "limit": 20})
	var previewGeneration any
	for _, raw := range claimedPreview["runs"].([]any) {
		c := raw.(map[string]any)
		if c["id"] == preview {
			previewGeneration = c["generation"]
		}
	}
	if previewGeneration == nil {
		d.t.Fatal("preview fixture was not claimed")
	}
	d.must(200, "studio-runtime", "POST", "/v1/studio/runtime/runs/"+preview+"/nodes/begin", map[string]any{"runtime_id": "demo-preview", "generation": previewGeneration, "index": 0})
	key, _ := master.Key(uuid.MustParse(d.tenant), uuid.MustParse(leaveKey))
	status := d.builderHTTP(key, d.api, "/v1/actions", map[string]string{"Idempotency-Key": "studio:" + preview + ":0"}, map[string]any{"subject": "stella@wonka.test", "operation": "lookup", "tool": "hr-mcp.get_leave_balance", "tool_schema_version": "1", "target": "hr", "resource": "leave_balance", "payload": map[string]string{"employee_id": "E-1"}})
	if status != 403 {
		d.t.Fatalf("a preview agent action was not refused: HTTP %d", status)
	}
	d.p.start("agent-runtime")
	d.assertBuilderAnswer(d.waitRun(preview, "SUCCEEDED", ""), leaveDef, 3)
	if len(d.hrCalls()) != toolCalls || d.studioProviderCalls() != before+1 || d.sql(`SELECT count(*) FROM eacp.actions WHERE studio_run_id='`+preview+`'`) != "0" {
		d.t.Fatal("preview sent a tool action or repeated its model call")
	}
	d.logf("approved preview uses a sample for HR, one real model call and no action; a direct tool attempt is refused")

	// Emulate a crash after admission/settlement but before cursor completion.
	// Every write still uses the runtime API and the approved derived agent key.
	d.p.stop("agent-runtime")
	recovering := run("studio_true")
	claimed := d.must(200, "studio-runtime", "POST", "/v1/studio/runtime/claims", map[string]any{"runtime_id": "demo-recovery", "master_version": "v1", "lease_seconds": 10, "limit": 20})
	var generation any
	for _, raw := range claimed["runs"].([]any) {
		c := raw.(map[string]any)
		if c["id"] == recovering {
			generation = c["generation"]
		}
	}
	if generation == nil {
		d.t.Fatal("manual crash fixture did not claim its run")
	}
	lease := map[string]any{"runtime_id": "demo-recovery", "generation": generation, "index": 0}
	intent := d.must(200, "studio-runtime", "POST", "/v1/studio/runtime/runs/"+recovering+"/llm/begin", lease)
	key, _ = master.Key(uuid.MustParse(d.tenant), uuid.MustParse(procKey))
	headers := map[string]string{"EACP-Subject": "stella@wonka.test", "EACP-Studio-Intent": intent["intent_id"].(string), "EACP-Studio-Runtime": "demo-recovery", "EACP-Studio-Generation": fmt.Sprint(generation), "anthropic-version": "2023-06-01"}
	body := map[string]any{"model": "triage-anthropic", "max_tokens": 100, "stream": false, "system": "Return one JSON object.", "messages": []any{map[string]string{"role": "user", "content": `{"request":"studio_true"}`}}}
	before = d.studioProviderCalls()
	wrong := map[string]any{}
	for k, v := range body {
		wrong[k] = v
	}
	wrong["model"] = "triage"
	if status = d.builderModelHTTP(key, headers, wrong); status != 403 {
		d.t.Fatalf("a model outside the immutable intent was allowed: HTTP %d", status)
	}
	if d.studioProviderCalls() != before {
		d.t.Fatal("model denial reached the provider")
	}
	if status = d.builderModelHTTP(key, headers, body); status != 200 {
		d.t.Fatalf("crash fixture model: HTTP %d", status)
	}
	if status = d.builderModelHTTP(key, headers, body); status != 409 {
		d.t.Fatalf("consumed intent was reusable: HTTP %d", status)
	}
	d.p.start("agent-runtime")
	d.assertBuilderAnswer(d.waitRun(recovering, "SUCCEEDED", ""), procDef, 2)
	if d.studioProviderCalls() != before+1 || d.sql(`SELECT count(*) FROM eacp.llm_calls WHERE studio_run_id='`+recovering+`'`) != "1" {
		d.t.Fatal("recovery sent another provider request")
	}
	d.logf("lease takeover reuses the settled call; the consumed intent refuses a second request and the provider receives one call")

	for _, scope := range []string{"run", "model"} {
		before = d.studioProviderCalls()
		slow := run("studio_slow")
		d.waitSQL(`SELECT count(*) FROM eacp.llm_calls WHERE studio_run_id='`+slow+`' AND state='ADMITTED'`, "1")
		deadline := time.Now().Add(20 * time.Second)
		for d.studioProviderCalls() != before+1 {
			if time.Now().After(deadline) {
				d.t.Fatal("slow call did not reach provider")
			}
			time.Sleep(200 * time.Millisecond)
		}
		target := slow
		if scope == "model" {
			target = models["triage-anthropic"]
		}
		d.must(200, "otto", "POST", "/v1/killswitch", map[string]any{"scope": scope, "target_id": target, "killed": true, "reason": "stop full-builder call"})
		d.waitRun(slow, "FAILED", "killed")
		d.waitSQL(`SELECT count(*) FROM eacp.llm_calls WHERE studio_run_id='`+slow+`' AND state='SETTLED' AND outcome='killed'`, "1")
		d.must(200, "opal", "POST", "/v1/killswitch", map[string]any{"scope": scope, "target_id": target, "killed": false, "reason": "second operator clears"})
		d.waitRun(slow, "FAILED", "killed")
		if d.studioProviderCalls() != before+1 {
			d.t.Fatal("contained model call was resent")
		}
	}
	const prompt = "STUDIO-PROMPT-27C-CANARY"
	d.assertBuilderAnswer(d.waitRun(run(prompt), "SUCCEEDED", ""), procDef, 2)
	logs := d.p.logs() + d.p.runtimeLogs()
	dump, err := d.p.postgres("pg_dump", "-U", "postgres", "--data-only", "eacp")
	if err != nil {
		d.t.Fatal("reading private-content leak evidence")
	}
	providerKey, err := os.ReadFile(filepath.Join(d.root, "deployments", "docker", "secrets", "fakellm-key.dev"))
	if err != nil {
		d.t.Fatal(err)
	}
	for _, secret := range []string{prompt, strings.TrimSpace(string(providerKey))} {
		if strings.Contains(logs, secret) || strings.Contains(dump, secret) {
			d.t.Fatal("private Studio content appeared in logs or durable data")
		}
	}
	if d.sql(`SELECT count(*) FROM eacp.studio_node_results WHERE tenant_id='`+d.tenant+`' AND output IS NOT NULL`) != "0" {
		d.t.Fatal("terminal Studio runs retained typed output")
	}
	d.logf("run/model kills cut in-flight calls permanently; typed outputs are cleared and prompt/provider-key canaries are absent from logs and data")
	if k, ok := d.p.(*k8sPlatform); ok {
		k.studioNetworkProof()
		d.logf("runtime NetworkPolicies allow only API/gateway; PostgreSQL, PDP, NATS, HR and the provider refuse direct connections")
	}
}

func (d *demo) assertBuilderAnswer(run, def map[string]any, index int) {
	d.t.Helper()
	if run["answer"] != def["steps"].([]any)[index].(map[string]any)["text"] {
		d.t.Fatal("builder followed the wrong fixed response")
	}
}

func (d *demo) studioProviderCalls() int {
	d.t.Helper()
	key, err := os.ReadFile(filepath.Join(d.root, "deployments", "docker", "secrets", "fakellm-key.dev"))
	if err != nil {
		d.t.Fatal(err)
	}
	b, err := d.p.llmAudit(strings.TrimSpace(string(key)))
	if err != nil {
		d.t.Fatal("reading provider audit failed")
	}
	var result struct {
		Calls []json.RawMessage `json:"calls"`
	}
	if err = json.Unmarshal(b, &result); err != nil {
		d.t.Fatal("invalid provider audit")
	}
	return len(result.Calls)
}

func (d *demo) builderModelHTTP(key string, headers map[string]string, body any) int {
	d.t.Helper()
	origin := env("EACP_DEMO_LLM", "http://127.0.0.1:18083")
	if k, ok := d.p.(*k8sPlatform); ok {
		var cleanup func()
		var err error
		origin, cleanup, err = k.forwardOrigin("eacp", "eacp-llm-gateway", 8083)
		if err != nil {
			d.t.Fatal("gateway forwarding did not start")
		}
		defer cleanup()
	}
	return d.builderHTTP(key, origin, "/v1/messages", headers, body)
}

// builderHTTP keeps the derived key in memory and performs one HTTP send.
func (d *demo) builderHTTP(key, origin, path string, headers map[string]string, body any) int {
	d.t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		d.t.Fatal(err)
	}
	req, err := http.NewRequest("POST", origin+path, bytes.NewReader(b))
	if err != nil {
		d.t.Fatal(err)
	}
	req.GetBody = nil
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	response, err := (&http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		d.t.Fatal("builder request failed")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode
}
