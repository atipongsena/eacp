package demo

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/atipongsena/eacp/internal/identity"
)

// tenantL is the LLM gateway demo's tenant: the gateway's provider manifest
// binds the Fake LLM's key to it (deployments/docker/secrets).
const tenantL = "00000000-0000-4000-8000-0000000000ac"

// promptCanary is prompt text that must reach the provider and nothing else.
const promptCanary = "PROMPT-CANARY-25b write a haiku about ledgers"

// sonnet's list price in the demo, USD per million tokens.
const sonnetIn, sonnetOut = 3, 15

// TestLLMGatewayDemo shows Phase 25b (ADR-031): an agent calls a model through
// the LLM gateway with its own EACP key and the Anthropic or OpenAI wire
// shape; EACP checks the model allowlist, reserves a hard USD budget, meters
// the provider's usage at PostgreSQL's price and cuts a call an operator kills.
func TestLLMGatewayDemo(t *testing.T) {
	d := newDemo(t, tenantL)
	c, ok := d.p.(*composePlatform)
	if !ok {
		t.Skip("the Helm chart has no LLM gateway yet; run the LLM demo on compose (scripts/demo.sh)")
	}
	d.reader = "audra"
	gateway := env("EACP_DEMO_LLM", "http://127.0.0.1:18083")
	waitReady(t, gateway)
	keyBytes, err := os.ReadFile(filepath.Join(d.root, "deployments", "docker", "secrets", "fakellm-key.dev"))
	if err != nil {
		t.Fatalf("reading the Fake LLM key: %v", err)
	}
	providerKey := strings.TrimSpace(string(keyBytes))

	d.step("L0. Bootstrap tenant Hooli-AI; a policy allows agents to generate text")
	d.tenantWithCast("hooli-ai", "Hooli-AI", []member{
		{"erin", "registry_editor"}, {"rita", "registry_approver"}, {"ravi", "registry_approver"},
		{"otto", "operator"}, {"olga", "operator"}, {"audra", "auditor"},
	})
	policy := d.must(201, "alice", "POST", "/v1/policies", map[string]any{"content": json.RawMessage(`{
		"format_version": 1, "rules": [
		{"id": "llm", "match": {"operation": "llm.generate"}, "verdict": "allow", "reason": "llm_allowed"}]}`)})
	d.must(204, "bob", "POST", "/v1/policies/"+policy["id"].(string)+"/activate", map[string]any{"reason": "reviewed"})

	d.step("L1. Declare sonnet and gpt on the Fake LLM, price them; writer may use sonnet with 0.05 USD")
	sonnet := d.must(201, "erin", "POST", "/v1/llm-models", map[string]any{"name": "sonnet", "provider": "anthropic",
		"base_url": "http://fakellm:8093", "upstream_model": "claude-fake", "secret_ref": "fakellm",
		"max_output_tokens": 100000, "timeout_ms": 90000})
	d.must(201, "erin", "POST", "/v1/llm-models", map[string]any{"name": "gpt", "provider": "openai",
		"base_url": "http://fakellm:8093", "upstream_model": "gpt-fake", "secret_ref": "fakellm",
		"max_output_tokens": 100000})
	for _, p := range [][3]string{{"anthropic", "claude-fake", fmt.Sprint(sonnetIn)}, {"openai", "gpt-fake", "2"}} {
		out := map[string]string{"claude-fake": fmt.Sprint(sonnetOut), "gpt-fake": "8"}[p[1]]
		d.must(201, "alice", "POST", "/v1/finops/prices", map[string]any{"provider": p[0], "model": p[1], "unit": "USD",
			"input_per_mtok": p[2], "output_per_mtok": out, "reason": "list price"})
	}
	team := d.must(201, "alice", "POST", "/v1/groups", map[string]any{"name": "content", "display_name": "content"})
	agent := d.must(201, "erin", "POST", "/v1/agents", map[string]any{"name": "writer", "display_name": "writer",
		"environment": "production", "risk_class": "medium", "owner_group_id": team["id"]})
	version := d.must(201, "erin", "POST", "/v1/agents/"+agent["id"].(string)+"/versions",
		map[string]any{"runtime": "python", "code_ref": "git:llm-demo"})
	vid := version["id"].(string)
	al := d.must(201, "erin", "POST", "/v1/agent-versions/"+vid+"/allowlists",
		map[string]any{"tools": []string{}, "models": []string{"sonnet"}})
	d.must(204, "rita", "POST", "/v1/agent-versions/"+vid+"/allowlist", map[string]any{"allowlist_id": al["id"]})
	d.must(204, "ravi", "POST", "/v1/agent-versions/"+vid+"/transitions", map[string]any{"to": "ACTIVE", "reason": "go live"})
	acct := d.must(201, "alice", "POST", "/v1/budgets", map[string]any{"name": "writer-usd", "unit": "USD",
		"agent_id": agent["id"]})
	raise := d.must(201, "alice", "POST", "/v1/budgets/"+acct["id"].(string)+"/limit",
		map[string]any{"limit": "0.05", "reason": "demo budget"})
	d.must(200, "bob", "POST", "/v1/budget-limit-changes/"+raise["id"].(string)+"/approve", map[string]any{"reason": "agreed"})
	cred, hash := d.newKey("writer", identity.KindAgent)
	d.must(201, "erin", "POST", "/v1/credentials", map[string]any{"id": cred, "kind": identity.KindAgent,
		"agent_version_id": vid, "hash": hash, "expires_in_days": 1})
	d.must(204, "rita", "POST", "/v1/credentials/"+cred.String()+"/approve", nil)
	d.logf("sonnet (anthropic, claude-fake at %d/%d USD per MTok) and gpt (openai) point at fakellm; writer's allowlist "+
		"names sonnet only and its USD account has a hard limit of 0.05 (alice proposes, bob approves)", sonnetIn, sonnetOut)

	llm := &llmClient{d: d, url: gateway}
	audit := func() int {
		d.t.Helper()
		out, err := exec.Command("docker", "run", "--rm", "--network", c.project+"_llm", "busybox:1.37", "wget", "-q",
			"-O-", "--header", "Authorization: Bearer "+providerKey, "http://fakellm:8093/v1/audit").Output()
		if err != nil {
			d.t.Fatalf("reading the Fake LLM audit: %v", err)
		}
		var a struct {
			Calls []json.RawMessage `json:"calls"`
		}
		if err := json.Unmarshal(out, &a); err != nil {
			d.t.Fatalf("Fake LLM audit: %v", err)
		}
		return len(a.Calls)
	}

	d.step("L2. writer calls sonnet with the Anthropic SDK's wire shape, plain and streamed")
	code, body := llm.messages(promptCanary, 100, false)
	if code != 200 || !strings.Contains(body, "Hello from fakellm.") {
		d.t.Fatalf("messages = %d %s", code, body)
	}
	plain := d.settled(llm.lastCall(), "succeeded")
	code, body = llm.messages(promptCanary, 100, true)
	if code != 200 || !strings.Contains(body, "event: message_stop") {
		d.t.Fatalf("streamed messages = %d %s", code, body)
	}
	streamed := d.settled(llm.lastCall(), "succeeded")
	// fakellm: input = 10 + len(prompt)/4, output = 20.
	wantIn, wantOut := int64(10+len(promptCanary)/4), int64(20)
	for _, call := range []map[string]any{plain, streamed} {
		in, out := int64(call["input_tokens"].(float64)), int64(call["output_tokens"].(float64))
		cost := decimal(d.t, call["cost_amount"])
		want := big.NewRat(in*sonnetIn+out*sonnetOut, 1_000_000)
		if in != wantIn || out != wantOut || cost.Cmp(want) != 0 || decimal(d.t, call["committed_amount"]).Cmp(cost) != 0 {
			d.t.Fatalf("metered call %v, want %d in, %d out, cost %s", call, wantIn, wantOut, want.FloatString(6))
		}
	}
	d.logf("both SUCCEEDED: %v input and %v output tokens each, cost %v USD computed by PostgreSQL from the rate card, "+
		"and exactly that committed to writer's budget", plain["input_tokens"], plain["output_tokens"], plain["cost_amount"])

	d.step("L3. gpt is not on writer's allowlist; a provider 429 costs nothing")
	code, body = llm.chat("gpt", "hello")
	if code != 403 || !strings.Contains(body, `"code":"model_not_in_allowlist"`) {
		d.t.Fatalf("gpt = %d %s", code, body)
	}
	d.settled(llm.lastCall(), "")
	code, body = llm.messages("error_429", 100, false)
	if code != 429 {
		d.t.Fatalf("error_429 = %d %s", code, body)
	}
	limited := d.settled(llm.lastCall(), "provider_error")
	if c, ok := limited["committed_amount"]; ok && decimal(d.t, c).Sign() != 0 {
		d.t.Fatalf("a provider error was charged: %v", limited)
	}
	d.logf("gpt DENIED model_not_in_allowlist before any provider call; the provider's 429 is relayed to writer, "+
		"settled %v and charged nothing", limited["outcome"])

	d.step("L4. A request that could cost more than the budget left is refused before it is sent")
	code, body = llm.messages("write a novel", 100000, false)
	if code != 403 || !strings.Contains(body, "eacp: budget_exceeded") {
		d.t.Fatalf("large max_tokens = %d %s", code, body)
	}
	d.settled(llm.lastCall(), "")
	d.logf("max_tokens 100000 could cost up to 1.5 USD: DENIED budget_exceeded, nothing reserved or sent")

	d.step("L5. otto kills model sonnet while a slow stream runs: the gateway cuts it within seconds")
	before := audit()
	events, done := llm.stream("slow")
	<-events // the first event arrived: the call is with the provider
	d.must(200, "otto", "POST", "/v1/killswitch", map[string]any{"scope": "model", "target_id": sonnet["id"],
		"killed": true, "reason": "runaway spend"})
	killedAt := time.Now()
	var cut string
	select {
	case cut = <-done:
	case <-time.After(20 * time.Second):
		d.t.Fatal("the killed stream kept running")
	}
	took := time.Since(killedAt)
	if !strings.Contains(cut, "killed") {
		d.t.Fatalf("the cut stream ended without a killed error: %q", cut)
	}
	slow := d.settled(llm.lastCall(), "killed")
	code, body = llm.messages("hello again", 100, false)
	if code != 403 || !strings.Contains(body, "eacp: killed") {
		d.t.Fatalf("after the kill = %d %s", code, body)
	}
	if n := audit(); n != before+1 {
		d.t.Fatalf("the provider saw %d calls after the kill, want 1", n-before)
	}
	d.logf("the stream ended %.1fs after the kill with an error event; the call is settled %v (committed %v USD); "+
		"writer's next call is DENIED killed and never reaches the provider", took.Seconds(), slow["outcome"],
		slow["committed_amount"])

	d.step("L6. Search responses, logs and the database for the provider key, the agent key and the prompt")
	d.mu.Lock()
	responses := strings.Join(d.responses, "\n")
	agentKey := d.keys["writer"]
	d.mu.Unlock()
	logs := d.p.logs() + c.must("logs", "--no-color", "llm-gateway", "fakellm")
	dump, err := d.p.postgres("pg_dump", "-U", "postgres", "--data-only", "eacp")
	if err != nil {
		d.t.Fatalf("pg_dump: %v", err)
	}
	if !strings.Contains(logs, `"msg":"llm call settled"`) {
		d.t.Fatal("the gateway logs no settlements; the log scan would be meaningless")
	}
	for where, text := range map[string]string{"an API response": responses, "a service log": logs, "the database": dump} {
		for name, secret := range map[string]string{"the provider key": providerKey, "writer's key": agentKey,
			"the prompt": "PROMPT-CANARY-25b"} {
			if strings.Contains(text, secret) {
				d.t.Fatalf("%s appears in %s", name, where)
			}
		}
	}
	d.logf("no provider key, agent key or prompt text in %d responses, the service logs or a %d-byte database dump",
		len(d.responses), len(dump))
	d.step("LLM gateway demo complete")
}

// waitReady waits for a service's /readyz.
func waitReady(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		resp, err := http.Get(base + "/readyz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s is not ready: %v", base, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func decimal(t *testing.T, v any) *big.Rat {
	t.Helper()
	s, _ := v.(string)
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("not a decimal: %v", v)
	}
	return r
}

// settled waits until the ledger has settled call and checks its outcome;
// want "" expects a denial (settled at admission).
func (d *demo) settled(id, want string) map[string]any {
	d.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		call := d.must(200, "audra", "GET", "/v1/llm-calls/"+id, nil)
		if call["state"] != "ADMITTED" {
			if want == "" && call["denial"] == nil || want != "" && call["outcome"] != want {
				d.t.Fatalf("call %s = %v, want outcome %q", id, call, want)
			}
			return call
		}
		if time.Now().After(deadline) {
			d.t.Fatalf("call %s is still ADMITTED: %v", id, call)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// llmClient is writer, calling the gateway with its EACP key.
type llmClient struct {
	d   *demo
	url string
}

func (l *llmClient) do(path, body string) (*http.Response, error) {
	req, err := http.NewRequest("POST", l.url+path, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	l.d.mu.Lock()
	key := l.d.keys["writer"]
	l.d.mu.Unlock()
	req.Header.Set("Content-Type", "application/json")
	if path == "/v1/messages" {
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return l.d.client.Do(req)
}

func (l *llmClient) send(path, body string) (int, string) {
	l.d.t.Helper()
	resp, err := l.do(path, body)
	if err != nil {
		l.d.t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	l.d.mu.Lock()
	l.d.responses = append(l.d.responses, string(raw))
	l.d.mu.Unlock()
	return resp.StatusCode, string(raw)
}

func messagesBody(text string, maxTokens int, stream bool) string {
	b, _ := json.Marshal(map[string]any{"model": "sonnet", "max_tokens": maxTokens, "stream": stream,
		"messages": []any{map[string]any{"role": "user", "content": text}}})
	return string(b)
}

func (l *llmClient) messages(text string, maxTokens int, stream bool) (int, string) {
	return l.send("/v1/messages", messagesBody(text, maxTokens, stream))
}

func (l *llmClient) chat(model, text string) (int, string) {
	b, _ := json.Marshal(map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": text}}})
	return l.send("/v1/chat/completions", string(b))
}

// stream starts a streamed Messages call: events receives once when the first
// event arrives, done the whole body once the stream ends.
func (l *llmClient) stream(text string) (<-chan struct{}, <-chan string) {
	events, done := make(chan struct{}, 1), make(chan string, 1)
	resp, err := l.do("/v1/messages", messagesBody(text, 100, true))
	if err != nil || resp.StatusCode != 200 {
		l.d.t.Fatalf("slow stream: %v %v", resp, err)
	}
	go func() {
		defer resp.Body.Close()
		var all strings.Builder
		br := bufio.NewReader(resp.Body)
		first := true
		for {
			line, err := br.ReadString('\n')
			all.WriteString(line)
			if first && strings.HasPrefix(line, "event: ") {
				first = false
				events <- struct{}{}
			}
			if err != nil {
				break
			}
		}
		l.d.mu.Lock()
		l.d.responses = append(l.d.responses, all.String())
		l.d.mu.Unlock()
		done <- all.String()
	}()
	return events, done
}

// lastCall is the id of writer's newest ledger row.
func (l *llmClient) lastCall() string {
	l.d.t.Helper()
	calls := l.d.must(200, "audra", "GET", "/v1/llm-calls?limit=1", nil)["calls"].([]any)
	if len(calls) == 0 {
		l.d.t.Fatal("the ledger is empty")
	}
	return calls[0].(map[string]any)["id"].(string)
}
