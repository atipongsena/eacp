package llmgateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"eacp/internal/governance"
	"eacp/internal/identity"
	"eacp/internal/llm"
	"eacp/internal/llmgateway"
	"eacp/internal/worker"
)

const (
	agentKey    = "eacp_agent_key_canary_5e1"
	principal   = "eacp_principal_key_7a2"
	providerKey = "sk-provider-canary-9f"
)

var (
	tenant  = uuid.MustParse("00000000-0000-4000-8000-0000000000ac")
	agentID = uuid.MustParse("00000000-0000-4000-8000-0000000000a1")
	version = uuid.MustParse("00000000-0000-4000-8000-0000000000a2")
	policy  = uuid.MustParse("00000000-0000-4000-8000-0000000000a3")
)

// proxyHits counts requests through the environment's proxy, which every
// test process points at a recording server (TestMain): the gateway must
// never use it.
var proxyHits atomic.Int64

func TestMain(m *testing.M) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"proxied":true}`)
	}))
	os.Setenv("HTTP_PROXY", proxy.URL)
	os.Setenv("HTTPS_PROXY", proxy.URL)
	code := m.Run()
	proxy.Close()
	os.Exit(code)
}

// ---------------------------------------------------------------- fakes

type settlement struct {
	call uuid.UUID
	s    llm.Settlement
}

type fakeLedger struct {
	mu        sync.Mutex
	admits    []llm.AdmitRequest
	admitErr  error
	denial    string // returned when the decision carries none
	model     llm.Model
	settled   chan settlement
	epoch     atomic.Int64
	killed    atomic.Bool
	onAdmit   func()
	killCalls atomic.Int64
}

func (l *fakeLedger) Admit(_ context.Context, tn uuid.UUID, r llm.AdmitRequest) (llm.Admission, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if tn != tenant {
		return llm.Admission{}, errors.New("wrong tenant")
	}
	if l.admitErr != nil {
		return llm.Admission{}, l.admitErr
	}
	l.admits = append(l.admits, r)
	if l.onAdmit != nil {
		l.onAdmit()
	}
	a := llm.Admission{CallID: uuid.New()}
	switch {
	case r.Decision.Denial != "":
		a.Denial = r.Decision.Denial
	case l.denial != "":
		a.Denial = l.denial
	default:
		a.Model, a.Deadline = l.model, time.Now().Add(time.Minute)
		if a.Model.MaxOutputTokens == 0 {
			a.Model.MaxOutputTokens = 4096
		}
		if r.MaxOutputTokens > 0 {
			a.Model.MaxOutputTokens = r.MaxOutputTokens
		}
	}
	return a, nil
}

func (l *fakeLedger) Settle(_ context.Context, tn, call uuid.UUID, s llm.Settlement) error {
	if tn != tenant {
		return errors.New("wrong tenant")
	}
	l.settled <- settlement{call, s}
	return nil
}

func (l *fakeLedger) Killed(context.Context, uuid.UUID, uuid.UUID) (bool, error) {
	l.killCalls.Add(1)
	return l.killed.Load(), nil
}

func (l *fakeLedger) KillEpoch(context.Context, uuid.UUID) (int64, error) { return l.epoch.Load(), nil }

func (l *fakeLedger) set(f func(*fakeLedger)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f(l)
}

func (l *fakeLedger) admitted() []llm.AdmitRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]llm.AdmitRequest(nil), l.admits...)
}

// settlement waits for the gateway's settlement of a call.
func (l *fakeLedger) settlement(t *testing.T) llm.Settlement {
	t.Helper()
	select {
	case s := <-l.settled:
		return s.s
	case <-time.After(5 * time.Second):
		t.Fatal("no settlement")
	}
	return llm.Settlement{}
}

func (l *fakeLedger) noSettlement(t *testing.T) {
	t.Helper()
	select {
	case s := <-l.settled:
		t.Fatalf("unexpected settlement %+v", s)
	case <-time.After(50 * time.Millisecond):
	}
}

type fakePDP struct {
	mu      sync.Mutex
	verdict governance.Verdict
	reason  string
	err     error
	last    governance.GovernanceRequest
	calls   int
	set     json.RawMessage // an enforced payload that differs from the input
}

func (p *fakePDP) Evaluate(_ context.Context, req governance.GovernanceRequest) (governance.GovernanceDecision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.last = req
	if p.err != nil {
		return governance.GovernanceDecision{}, p.err
	}
	enforced := req.Binding.Payload
	if p.set != nil {
		enforced = p.set
	}
	in, out, err := governance.Digests(req.Binding, enforced)
	if err != nil {
		return governance.GovernanceDecision{}, err
	}
	v, reason := p.verdict, p.reason
	if v == "" {
		v, reason = governance.VerdictAllow, "llm_allowed"
	}
	d := governance.GovernanceDecision{Verdict: v, EnforcedPayload: enforced, InputDigest: in, EnforcedDigest: out,
		PolicyBundleID: req.PolicyBundleID, PolicyVersion: req.PolicyVersion, Provider: "fake",
		ProviderInstanceID: "test", DecisionID: uuid.New(), Reasons: []string{reason}, EvaluatedAt: time.Now()}
	if v == governance.VerdictEscalate {
		d.Approval = &governance.ApprovalRequirement{Quorum: 1, EligibleRoles: []string{"approver"}, TTLSeconds: 60}
	}
	return d, nil
}

func (p *fakePDP) configure(v governance.Verdict, reason string, set json.RawMessage, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.verdict, p.reason, p.set, p.err = v, reason, set, err
}

func (p *fakePDP) lastRequest() governance.GovernanceRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}

func (p *fakePDP) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

type fakePolicies struct{ err error }

func (f fakePolicies) CurrentPolicy(context.Context, uuid.UUID) (governance.Policy, error) {
	return governance.Policy{ID: policy, Version: 3, Content: json.RawMessage(`{"rules":[]}`)}, f.err
}

// provider is a scripted upstream LLM API that records what it received.
type provider struct {
	srv     *httptest.Server
	mu      sync.Mutex
	reqs    []recorded
	handler http.HandlerFunc
}

type recorded struct {
	path   string
	header http.Header
	body   []byte
}

func (p *provider) set(h http.HandlerFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.handler = h
}

func (p *provider) requests() []recorded {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]recorded(nil), p.reqs...)
}

func newProvider(t *testing.T, h http.HandlerFunc) *provider {
	t.Helper()
	p := &provider{handler: h}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.reqs = append(p.reqs, recorded{r.URL.Path, r.Header.Clone(), body})
		h := p.handler
		p.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		h(w, r)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// secretsFor writes a secrets file binding ref "llm" to host.
func secretsFor(t *testing.T, host string) *worker.SecretStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secrets.json")
	doc := fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"llm","host":%q,"value":%q}]}`, tenant, host, providerKey)
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := worker.LoadSecrets(path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type harness struct {
	t      *testing.T
	gw     *httptest.Server
	ledger *fakeLedger
	pdp    *fakePDP
	prov   *provider
	logs   *syncBuffer
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func auth(_ context.Context, key string) (identity.Caller, error) {
	switch key {
	case agentKey:
		return identity.Caller{Kind: identity.KindAgent, TenantID: tenant, AgentID: agentID, AgentVersionID: version,
			AgentState: "ACTIVE"}, nil
	case principal:
		return identity.Caller{Kind: identity.KindPrincipal, TenantID: tenant, PrincipalID: uuid.New()}, nil
	}
	return identity.Caller{}, &identity.AuthError{Reason: "unknown_key"}
}

// newHarness serves a gateway whose models all point at a provider running h.
func newHarness(t *testing.T, h http.HandlerFunc, opts ...func(*llmgateway.Options)) *harness {
	t.Helper()
	prov := newProvider(t, h)
	u, _ := url.Parse(prov.srv.URL)
	x := &harness{t: t, prov: prov, logs: &syncBuffer{}, pdp: &fakePDP{},
		ledger: &fakeLedger{settled: make(chan settlement, 16), model: llm.Model{ID: uuid.New(),
			UpstreamModel: "upstream-model-1", BaseURL: prov.srv.URL, SecretRef: "llm", Timeout: 5 * time.Second}}}
	o := llmgateway.Options{ID: "gw-test", Auth: auth, Ledger: x.ledger, Policies: fakePolicies{}, PDP: x.pdp,
		Secrets: secretsFor(t, u.Host), MaxRequestBytes: 64 << 10, KillPoll: 50 * time.Millisecond,
		AgentRisk: func(context.Context, uuid.UUID, uuid.UUID) (string, error) { return "high", nil },
		Log:       slog.New(slog.NewJSONHandler(x.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	for _, f := range opts {
		f(&o)
	}
	gw, err := llmgateway.New(o)
	if err != nil {
		t.Fatal(err)
	}
	x.gw = httptest.NewServer(gw)
	t.Cleanup(x.gw.Close)
	return x
}

// post sends body to path with headers and returns the status, headers and body.
func (x *harness) post(path, body string, header map[string]string) (int, http.Header, []byte) {
	x.t.Helper()
	req, _ := http.NewRequest("POST", x.gw.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		x.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

func anthropicKey() map[string]string { return map[string]string{"x-api-key": agentKey} }
func openaiKey() map[string]string    { return map[string]string{"Authorization": "Bearer " + agentKey} }

const (
	messagesBody = `{"model":"sonnet","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
	chatBody     = `{"model":"gpt","messages":[{"role":"user","content":"hi"}]}`
)

func anthropicOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("request-id", "req_123")
	_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hello"}],`+
		`"usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":3,"cache_read_input_tokens":2}}`)
}

func openaiOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("x-request-id", "req_456")
	_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,`+
		`"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],`+
		`"usage":{"prompt_tokens":100,"completion_tokens":7,"total_tokens":107,"prompt_tokens_details":{"cached_tokens":40}}}`)
}

// ---------------------------------------------------------------- tests

func TestRoutesAndAuth(t *testing.T) {
	x := newHarness(t, anthropicOK)
	for _, c := range []struct {
		method, path string
		header       map[string]string
		want         int
	}{
		{"POST", "/v1/other", anthropicKey(), 404},
		{"GET", "/v1/messages", anthropicKey(), 404},
		{"POST", "/v1/messages/", anthropicKey(), 404},
		{"POST", "/v1/messages", nil, 401},
		{"POST", "/v1/messages", map[string]string{"x-api-key": "wrong"}, 401},
		{"POST", "/v1/messages", map[string]string{"x-api-key": principal}, 401},
		{"POST", "/v1/chat/completions", map[string]string{"Authorization": "Bearer " + principal}, 401},
		// The OpenAI route takes only a Bearer key.
		{"POST", "/v1/chat/completions", map[string]string{"x-api-key": agentKey}, 401},
		// Two different keys are refused.
		{"POST", "/v1/messages", map[string]string{"x-api-key": agentKey, "Authorization": "Bearer other"}, 401},
	} {
		req, _ := http.NewRequest(c.method, x.gw.URL+c.path, strings.NewReader(messagesBody))
		for k, v := range c.header {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s %s %v: %d, want %d", c.method, c.path, c.header, resp.StatusCode, c.want)
		}
	}
	if n := len(x.ledger.admitted()); n != 0 {
		t.Fatalf("%d admissions", n)
	}
	for _, h := range []map[string]string{anthropicKey(), {"Authorization": "Bearer " + agentKey}} {
		if code, _, body := x.post("/v1/messages", messagesBody, h); code != 200 {
			t.Fatalf("%v: %d %s", h, code, body)
		}
		x.ledger.settlement(t)
	}
}

func TestRequestValidation(t *testing.T) {
	x := newHarness(t, anthropicOK, func(o *llmgateway.Options) { o.MaxRequestBytes = 1024 })
	for name, c := range map[string]struct{ path, body string }{
		"not json":               {"/v1/messages", `hello`},
		"array":                  {"/v1/messages", `[1]`},
		"trailing data":          {"/v1/messages", messagesBody + `{}`},
		"duplicate model":        {"/v1/messages", `{"model":"sonnet","model":"opus","max_tokens":1,"messages":[]}`},
		"nested duplicate":       {"/v1/messages", `{"model":"sonnet","max_tokens":1,"messages":[{"role":"user","role":"x"}]}`},
		"no model":               {"/v1/messages", `{"max_tokens":1,"messages":[]}`},
		"model number":           {"/v1/messages", `{"model":7,"max_tokens":1,"messages":[]}`},
		"model empty":            {"/v1/messages", `{"model":"","max_tokens":1,"messages":[]}`},
		"model control":          {"/v1/messages", `{"model":"son\u0000net","max_tokens":1,"messages":[]}`},
		"model too long":         {"/v1/messages", `{"model":"` + strings.Repeat("m", 257) + `","max_tokens":1}`},
		"stream string":          {"/v1/messages", `{"model":"sonnet","max_tokens":1,"stream":"yes"}`},
		"no max_tokens":          {"/v1/messages", `{"model":"sonnet","messages":[]}`},
		"zero max_tokens":        {"/v1/messages", `{"model":"sonnet","max_tokens":0}`},
		"fraction max_tokens":    {"/v1/messages", `{"model":"sonnet","max_tokens":1.5}`},
		"exponent max_tokens":    {"/v1/messages", `{"model":"sonnet","max_tokens":1e3}`},
		"string max_tokens":      {"/v1/messages", `{"model":"sonnet","max_tokens":"10"}`},
		"huge max_tokens":        {"/v1/messages", `{"model":"sonnet","max_tokens":99999999999999999999}`},
		"invalid utf-8":          {"/v1/messages", "{\"model\":\"sonnet\",\"max_tokens\":1,\"x\":\"\xff\"}"},
		"n 2":                    {"/v1/chat/completions", `{"model":"gpt","n":2,"messages":[]}`},
		"n string":               {"/v1/chat/completions", `{"model":"gpt","n":"1","messages":[]}`},
		"both caps":              {"/v1/chat/completions", `{"model":"gpt","max_tokens":5,"max_completion_tokens":5}`},
		"zero cap":               {"/v1/chat/completions", `{"model":"gpt","max_completion_tokens":0}`},
		"stream_options not obj": {"/v1/chat/completions", `{"model":"gpt","stream":true,"stream_options":"all"}`},
		"include_usage not bool": {"/v1/chat/completions", `{"model":"gpt","stream":true,"stream_options":{"include_usage":1}}`},
		"deeply nested":          {"/v1/chat/completions", `{"model":"gpt","x":` + strings.Repeat("[", 200) + strings.Repeat("]", 200) + `}`},
	} {
		h := anthropicKey()
		if c.path == "/v1/chat/completions" {
			h = openaiKey()
		}
		code, _, body := x.post(c.path, c.body, h)
		if code != 400 || !strings.Contains(string(body), "invalid_request") {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
	if code, _, _ := x.post("/v1/messages", `{"model":"sonnet","max_tokens":1,"x":"`+strings.Repeat("a", 2000)+`"}`,
		anthropicKey()); code != 413 {
		t.Errorf("oversized body: %d", code)
	}
	if n := len(x.ledger.admitted()); n != 0 || x.pdp.callCount() != 0 || len(x.prov.requests()) != 0 {
		t.Fatalf("admitted %d, PDP calls %d, provider requests %d", n, x.pdp.callCount(), len(x.prov.requests()))
	}
	// n 1, one cap, and stream_options as an object are fine.
	code, _, body := x.post("/v1/chat/completions", `{"model":"gpt","n":1,"max_tokens":5,"stream":false,"messages":[]}`,
		openaiKey())
	if code != 200 {
		t.Fatalf("valid OpenAI request: %d %s", code, body)
	}
	x.ledger.settlement(t)
}

func TestDecisionBinding(t *testing.T) {
	x := newHarness(t, anthropicOK)
	code, _, body := x.post("/v1/messages", messagesBody, anthropicKey())
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	x.ledger.settlement(t)
	req := x.pdp.lastRequest()
	b := req.Binding
	if b.TenantID != tenant || b.AgentID != agentID || b.AgentVersionID != version || b.Subject != "agent:"+agentID.String() ||
		b.Operation != "llm.generate" || b.Target != "sonnet" || b.Resource != "sonnet" || b.Tool != "llm:anthropic" ||
		b.ToolSchemaVersion != "1" || req.SideEffectClass != "LLM_GENERATION" || req.RiskClass != "high" ||
		req.PolicyBundleID != policy || req.PolicyVersion != 3 {
		t.Fatalf("request %+v", req)
	}
	want := fmt.Sprintf(`{"max_output_tokens":100,"model":"sonnet","request_bytes":%d,"stream":false}`, len(messagesBody))
	if string(b.Payload) != want {
		t.Fatalf("payload %s, want %s", b.Payload, want)
	}
	a := x.ledger.admitted()[0]
	if a.AgentVersionID != version || a.ModelName != "sonnet" || a.Provider != "anthropic" || a.Subject != "" ||
		a.GatewayID != "gw-test" || a.Stream || a.RequestBytes != int64(len(messagesBody)) || a.MaxOutputTokens != 100 ||
		a.Decision.ID == uuid.Nil || a.Decision.BundleID != policy || a.Decision.Version != 3 ||
		a.Decision.Verdict != "allow" || a.Decision.InputDigest == [32]byte{} || a.Decision.Denial != "" {
		t.Fatalf("admission %+v", a)
	}

	// A subject and a trace context travel with the call; OpenAI with no cap sends null.
	tp := "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	x.ledger.set(func(l *fakeLedger) { l.model.UpstreamModel = "gpt-up" })
	h := openaiKey()
	h["EACP-Subject"], h["traceparent"] = "alice@example.com", tp
	if code, _, body := x.post("/v1/chat/completions", chatBody, h); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	x.ledger.settlement(t)
	if b := x.pdp.lastRequest().Binding; b.Subject != "alice@example.com" || b.Tool != "llm:openai" ||
		string(b.Payload) != fmt.Sprintf(`{"max_output_tokens":null,"model":"gpt","request_bytes":%d,"stream":false}`, len(chatBody)) {
		t.Fatalf("binding %+v %s", b, b.Payload)
	}
	if a := x.ledger.admitted()[1]; a.Subject != "alice@example.com" || a.TraceID != "0af7651916cd43dd8448eb211c80319c" ||
		a.MaxOutputTokens != 0 || a.Provider != "openai" {
		t.Fatalf("admission %+v", a)
	}

	for _, c := range []struct {
		verdict governance.Verdict
		reason  string
		set     json.RawMessage
		code    string
	}{
		{governance.VerdictDeny, "no_matching_rule", nil, "no_matching_rule"},
		{governance.VerdictDeny, "Not For You!", nil, "policy_denied"},
		{governance.VerdictEscalate, "needs_review", nil, "approval_unsupported"},
		{governance.VerdictTransform, "shorten", json.RawMessage(`{"max_output_tokens":1}`), "transform_unsupported"},
		// A policy that would change the metadata of an allowed call cannot be enforced.
		{governance.VerdictAllow, "ok", json.RawMessage(`{"model":"other"}`), "transform_unsupported"},
	} {
		x.pdp.configure(c.verdict, c.reason, c.set, nil)
		code, _, body := x.post("/v1/messages", messagesBody, anthropicKey())
		if code != 403 || !strings.Contains(string(body), "eacp: "+c.code) {
			t.Fatalf("%s/%s: %d %s", c.verdict, c.reason, code, body)
		}
		all := x.ledger.admitted()
		if d := all[len(all)-1].Decision; d.Denial != c.code || d.Verdict != string(c.verdict) {
			t.Fatalf("%s: decision %+v", c.verdict, d)
		}
	}
	x.ledger.noSettlement(t)
	before := len(x.prov.requests())
	if before != 2 {
		t.Fatalf("the provider saw %d requests", before)
	}

	// Governance unavailable: 503 and nothing recorded.
	n := len(x.ledger.admitted())
	x.pdp.configure("", "", nil, errors.New("sidecar down"))
	if code, h, body := x.post("/v1/messages", messagesBody, anthropicKey()); code != 503 || h.Get("Retry-After") == "" {
		t.Fatalf("PDP down: %d %s", code, body)
	}
	x.pdp.configure("", "", nil, nil)
	y := newHarness(t, anthropicOK, func(o *llmgateway.Options) { o.Policies = fakePolicies{err: errors.New("no policy")} })
	if code, _, _ := y.post("/v1/messages", messagesBody, anthropicKey()); code != 503 {
		t.Fatalf("no policy: %d", code)
	}
	x.ledger.set(func(l *fakeLedger) { l.admitErr = errors.New("database down") })
	if code, _, _ := x.post("/v1/messages", messagesBody, anthropicKey()); code != 503 {
		t.Fatalf("database down: %d", code)
	}
	if len(x.ledger.admitted()) != n || len(y.ledger.admitted()) != 0 || len(x.prov.requests()) != before {
		t.Fatal("an unavailable dependency recorded or forwarded a call")
	}
}

func TestDenialIsProviderShaped(t *testing.T) {
	x := newHarness(t, anthropicOK)
	x.ledger.set(func(l *fakeLedger) { l.denial = "budget_exceeded" })
	code, h, body := x.post("/v1/messages", messagesBody, anthropicKey())
	if code != 403 || h.Get("Content-Type") != "application/json" ||
		string(body) != `{"type":"error","error":{"type":"permission_error","message":"eacp: budget_exceeded"}}` {
		t.Fatalf("anthropic: %d %s", code, body)
	}
	code, _, body = x.post("/v1/chat/completions", chatBody, openaiKey())
	if code != 403 ||
		string(body) != `{"error":{"message":"eacp: budget_exceeded","type":"permission_denied","code":"budget_exceeded"}}` {
		t.Fatalf("openai: %d %s", code, body)
	}
	if len(x.prov.requests()) != 0 {
		t.Fatal("a denied call reached the provider")
	}
	x.ledger.noSettlement(t)
}

// Inputs the provider fetches or runs, or that price above the rate card,
// are not bounded by the request's bytes, so the reservation cannot cover
// them: they are refused before the PDP (ADR-031 §4). Look-alike keys inside
// tool schemas and tool arguments are content, never inspected.
func TestUnboundedInputsAreRefused(t *testing.T) {
	x := newHarness(t, anthropicOK)
	msg := func(content string) string {
		return `{"model":"sonnet","max_tokens":1,"messages":[{"role":"user","content":[` + content + `]}]}`
	}
	chat := func(extra string) string {
		return `{"model":"gpt","messages":[{"role":"user","content":"hi"}]` + extra + `}`
	}
	for name, c := range map[string]struct{ path, body string }{
		"image by url":        {"/v1/messages", msg(`{"type":"image","source":{"type":"url","url":"https://x.test/a.png"}}`)},
		"document by url":     {"/v1/messages", msg(`{"type":"document","source":{"type":"url","url":"https://x.test/a.pdf"}}`)},
		"document by file":    {"/v1/messages", msg(`{"type":"document","source":{"type":"file","file_id":"file_1"}}`)},
		"container upload":    {"/v1/messages", msg(`{"type":"container_upload","file_id":"file_1"}`)},
		"url in tool result":  {"/v1/messages", msg(`{"type":"tool_result","tool_use_id":"t","content":[{"type":"image","source":{"type":"url","url":"https://x.test/a.png"}}]}`)},
		"url in content doc":  {"/v1/messages", msg(`{"type":"document","source":{"type":"content","content":[{"type":"image","source":{"type":"url","url":"u"}}]}}`)},
		"source not object":   {"/v1/messages", msg(`{"type":"image","source":"https://x.test/a.png"}`)},
		"server tool":         {"/v1/messages", `{"model":"sonnet","max_tokens":1,"messages":[],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`},
		"mcp servers":         {"/v1/messages", `{"model":"sonnet","max_tokens":1,"messages":[],"mcp_servers":[{"type":"url","url":"https://m.test","name":"m"}]}`},
		"container":           {"/v1/messages", `{"model":"sonnet","max_tokens":1,"messages":[],"container":"c_1"}`},
		"image_url http":      {"/v1/chat/completions", `{"model":"gpt","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x.test/a.png"}}]}]}`},
		"file by id":          {"/v1/chat/completions", `{"model":"gpt","messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"file_1"}}]}]}`},
		"assistant audio ref": {"/v1/chat/completions", `{"model":"gpt","messages":[{"role":"assistant","audio":{"id":"audio_1"}}]}`},
		"web search":          {"/v1/chat/completions", chat(`,"web_search_options":{}`)},
		"audio output":        {"/v1/chat/completions", chat(`,"modalities":["text","audio"],"audio":{"voice":"alloy","format":"wav"}`)},
		"priority tier":       {"/v1/chat/completions", chat(`,"service_tier":"priority"`)},
		"prediction":          {"/v1/chat/completions", chat(`,"prediction":{"type":"content","content":"x"}`)},
		"non-function tool":   {"/v1/chat/completions", chat(`,"tools":[{"type":"web_search"}]`)},
	} {
		h := anthropicKey()
		if c.path == "/v1/chat/completions" {
			h = openaiKey()
		}
		code, _, body := x.post(c.path, c.body, h)
		if code != 400 || !strings.Contains(string(body), "unbounded_input") {
			t.Errorf("%s: %d %s", name, code, body)
			if code == 200 {
				x.ledger.settlement(t)
			}
		}
	}
	if n := len(x.ledger.admitted()); n != 0 || x.pdp.callCount() != 0 || len(x.prov.requests()) != 0 {
		t.Fatalf("admitted %d, PDP calls %d, provider requests %d", n, x.pdp.callCount(), len(x.prov.requests()))
	}
	for name, c := range map[string]struct{ path, body string }{
		"base64 image":                       {"/v1/messages", msg(`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}`)},
		"text document":                      {"/v1/messages", msg(`{"type":"document","source":{"type":"text","media_type":"text/plain","data":"hi"}}`)},
		"custom tool with look-alike schema": {"/v1/messages", `{"model":"sonnet","max_tokens":1,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"f","input":{"source":{"type":"url"},"file_id":"x"}}]}],"tools":[{"name":"f","input_schema":{"type":"object","properties":{"source":{"type":"object"},"file_id":{"type":"string"}}}},{"type":"custom","name":"g","input_schema":{"type":"object"}}]}`},
		"data image":                         {"/v1/chat/completions", `{"model":"gpt","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`},
		"file data":                          {"/v1/chat/completions", `{"model":"gpt","messages":[{"role":"user","content":[{"type":"file","file":{"file_data":"data:application/pdf;base64,AAAA","filename":"a.pdf"}}]}]}`},
		"function tool":                      {"/v1/chat/completions", chat(`,"tools":[{"type":"function","function":{"name":"f","parameters":{"properties":{"image_url":{"type":"string"}}}}}],"service_tier":"default"`)},
		"flex tier":                          {"/v1/chat/completions", chat(`,"service_tier":"flex","modalities":["text"]`)},
	} {
		h := anthropicKey()
		if c.path == "/v1/chat/completions" {
			h = openaiKey()
		}
		if code, _, body := x.post(c.path, c.body, h); code != 200 {
			t.Errorf("%s: %d %s", name, code, body)
		}
		x.ledger.settlement(t)
	}
}

// A key that cannot be checked (PostgreSQL down) is not a bad key: 503, so
// the agent's SDK retries, instead of 401, which it treats as final
// (ADR-031 §5: PostgreSQL down is 503).
func TestAnAuthenticationOutageIsRetryable(t *testing.T) {
	x := newHarness(t, anthropicOK, func(o *llmgateway.Options) {
		o.Auth = func(context.Context, string) (identity.Caller, error) {
			return identity.Caller{}, errors.New("identity: read credential: connection refused")
		}
	})
	code, h, body := x.post("/v1/messages", messagesBody, anthropicKey())
	if code != 503 || h.Get("Retry-After") == "" || !strings.Contains(string(body), "eacp: unavailable") {
		t.Fatalf("%d %v %s", code, h, body)
	}
	if n := len(x.ledger.admitted()); n != 0 || len(x.prov.requests()) != 0 {
		t.Fatalf("admitted %d, provider requests %d", n, len(x.prov.requests()))
	}
}
