package llmgateway_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"eacp/internal/jwttest"
	"eacp/internal/llm"
	"eacp/internal/llmgateway"
	"eacp/internal/worker"
)

func wantSettled(t *testing.T, got llm.Settlement, outcome string, status int, u llm.Usage) {
	t.Helper()
	if got.Outcome != outcome || got.ProviderStatus != status || got.Usage != u {
		t.Fatalf("settlement %+v, want %s %d %+v", got, outcome, status, u)
	}
}

func TestForwardRewritesOnlyWhatItMust(t *testing.T) {
	x := newHarness(t, anthropicOK)
	in := `{"model":"sonnet","max_tokens":100,"system":"<b>&</b>","messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"u1"}}`
	h := anthropicKey()
	h["anthropic-version"], h["anthropic-beta"] = "2023-06-01", "prompt-caching-2024-07-31"
	h["traceparent"], h["tracestate"] = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01", "k=v"
	h["X-Custom"], h["EACP-Subject"], h["Cookie"] = "leak", "alice@example.com", "session=1"
	h["Accept"] = "application/json"
	code, rh, body := x.post("/v1/messages", in, h)
	if code != 200 || rh.Get("request-id") != "req_123" || rh.Get("Content-Type") != "application/json" {
		t.Fatalf("%d %v %s", code, rh, body)
	}
	x.ledger.settlement(t)
	r := x.prov.requests()[0]
	if r.path != "/v1/messages" || r.header.Get("x-api-key") != providerKey || r.header.Get("Authorization") != "" ||
		r.header.Get("anthropic-version") != "2023-06-01" || r.header.Get("anthropic-beta") != "prompt-caching-2024-07-31" ||
		r.header.Get("traceparent") == "" || r.header.Get("tracestate") != "k=v" ||
		r.header.Get("Content-Type") != "application/json" || r.header.Get("Accept") != "application/json" {
		t.Fatalf("upstream headers %v", r.header)
	}
	for _, k := range []string{"X-Custom", "Eacp-Subject", "Cookie"} {
		if r.header.Get(k) != "" {
			t.Fatalf("header %s was forwarded", k)
		}
	}
	want := `{"max_tokens":100,"messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"u1"},` +
		`"model":"upstream-model-1","system":"<b>&</b>"}`
	if string(r.body) != want {
		t.Fatalf("upstream body\n%s\nwant\n%s", r.body, want)
	}

	// OpenAI: the key is a Bearer; no cap gets the model's; a stream asks for usage.
	x.prov.set(openaiOK)
	x.ledger.set(func(l *fakeLedger) { l.model.MaxOutputTokens = 2048 })
	if code, _, body := x.post("/v1/chat/completions", chatBody, openaiKey()); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	x.ledger.settlement(t)
	r = x.prov.requests()[1]
	if r.path != "/v1/chat/completions" || r.header.Get("Authorization") != "Bearer "+providerKey ||
		r.header.Get("x-api-key") != "" || r.header.Get("anthropic-version") != "" {
		t.Fatalf("upstream headers %v", r.header)
	}
	if string(r.body) != `{"max_completion_tokens":2048,"messages":[{"role":"user","content":"hi"}],"model":"upstream-model-1"}` {
		t.Fatalf("upstream body %s", r.body)
	}
	x.prov.set(openaiStream(true))
	code, _, _ = x.post("/v1/chat/completions",
		`{"model":"gpt","max_tokens":9,"stream":true,"stream_options":{"include_usage":false,"x":1},"messages":[]}`, openaiKey())
	if code != 200 {
		t.Fatalf("%d", code)
	}
	x.ledger.settlement(t)
	if r := x.prov.requests()[2]; string(r.body) !=
		`{"max_tokens":9,"messages":[],"model":"upstream-model-1","stream":true,"stream_options":{"include_usage":true,"x":1}}` {
		t.Fatalf("upstream body %s", r.body)
	}
}

func TestNonStreamSettlesExactUsage(t *testing.T) {
	x := newHarness(t, anthropicOK)
	code, _, body := x.post("/v1/messages", messagesBody, anthropicKey())
	if code != 200 || !strings.Contains(string(body), `"text":"hello"`) {
		t.Fatalf("%d %s", code, body)
	}
	wantSettled(t, x.ledger.settlement(t), llm.OutcomeSucceeded, 200,
		llm.Usage{Input: 10, CacheRead: 2, CacheWrite: 3, Output: 5, Known: true})

	x.prov.set(openaiOK)
	if code, h, _ := x.post("/v1/chat/completions", chatBody, openaiKey()); code != 200 || h.Get("x-request-id") != "req_456" {
		t.Fatalf("%d %v", code, h)
	}
	wantSettled(t, x.ledger.settlement(t), llm.OutcomeSucceeded, 200,
		llm.Usage{Input: 60, CacheRead: 40, Output: 7, Known: true})

	for name, reply := range map[string]string{
		"no usage":       `{"id":"x","content":[]}`,
		"negative":       `{"usage":{"input_tokens":-1,"output_tokens":5}}`,
		"not json":       `<html>`,
		"missing output": `{"usage":{"input_tokens":1}}`,
	} {
		x.prov.set(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, reply) })
		if code, _, body := x.post("/v1/messages", messagesBody, anthropicKey()); code != 200 || string(body) != reply {
			t.Fatalf("%s: %d %s", name, code, body)
		}
		wantSettled(t, x.ledger.settlement(t), llm.OutcomeUsageUnknown, 200, llm.Usage{})
	}
	x.prov.set(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":5,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":9}}}`)
	})
	x.post("/v1/chat/completions", chatBody, openaiKey())
	wantSettled(t, x.ledger.settlement(t), llm.OutcomeUsageUnknown, 200, llm.Usage{})
}

// sse writes events one by one, flushing each, and runs between(i) after event i.
func sse(w http.ResponseWriter, events []string, between func(i int)) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("request-id", "req_s")
	f := w.(http.Flusher)
	for i, e := range events {
		_, _ = io.WriteString(w, e)
		f.Flush()
		if between != nil {
			between(i)
		}
	}
}

var anthropicEvents = []string{
	"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":10,\"cache_creation_input_tokens\":3,\"cache_read_input_tokens\":2,\"output_tokens\":1}}}\n\n",
	"event: ping\ndata: {\"type\": \"ping\"}\n\n",
	"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n",
	"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":15}}\n\n",
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
}

func openaiEvents(usage bool) []string {
	ev := []string{
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}],\"usage\":null}\n\n",
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"}}],\"usage\":null}\n\n",
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":null}\n\n",
	}
	if usage {
		ev = append(ev, "data: {\"id\":\"c1\",\"choices\":[],\"usage\":{\"prompt_tokens\":30,\"completion_tokens\":4,"+
			"\"total_tokens\":34,\"prompt_tokens_details\":{\"cached_tokens\":10}}}\n\n")
	}
	return append(ev, "data: [DONE]\n\n")
}

func openaiStream(usage bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { sse(w, openaiEvents(usage), nil) }
}

// stream posts a streamed request and returns the response for reading.
func (x *harness) stream(ctx context.Context, path, body string, header map[string]string) *http.Response {
	x.t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "POST", x.gw.URL+path, strings.NewReader(body))
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		x.t.Fatal(err)
	}
	return resp
}

// readEvent reads one SSE event (through its blank line).
func readEvent(t *testing.T, br *bufio.Reader) string {
	t.Helper()
	var b strings.Builder
	for {
		line, err := br.ReadString('\n')
		b.WriteString(line)
		if err != nil {
			return b.String()
		}
		if line == "\n" {
			return b.String()
		}
	}
}

func TestStreamRelaysAndSettles(t *testing.T) {
	// Each event is sent only once the client has read the one before: the
	// gateway must flush every event as it arrives.
	read := make(chan int, len(anthropicEvents))
	x := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		sse(w, anthropicEvents, func(i int) {
			select {
			case <-read:
			case <-time.After(5 * time.Second):
			}
		})
	})
	body := `{"model":"sonnet","max_tokens":100,"stream":true,"messages":[]}`
	resp := x.stream(context.Background(), "/v1/messages", body, anthropicKey())
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" || resp.Header.Get("request-id") != "req_s" {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
	br := bufio.NewReader(resp.Body)
	for i, want := range anthropicEvents {
		done := make(chan string, 1)
		go func() { done <- readEvent(t, br) }()
		select {
		case got := <-done:
			if got != want {
				t.Fatalf("event %d = %q, want %q", i, got, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("event %d was not flushed", i)
		}
		read <- i
	}
	wantSettled(t, x.ledger.settlement(t), llm.OutcomeSucceeded, 200,
		llm.Usage{Input: 10, CacheRead: 2, CacheWrite: 3, Output: 15, Known: true})
	if a := x.ledger.admitted()[0]; !a.Stream {
		t.Fatal("not admitted as a stream")
	}

	x.prov.set(openaiStream(true))
	resp = x.stream(context.Background(), "/v1/chat/completions", `{"model":"gpt","stream":true,"messages":[]}`, openaiKey())
	all, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(all) != strings.Join(openaiEvents(true), "") {
		t.Fatalf("relayed %q", all)
	}
	wantSettled(t, x.ledger.settlement(t), llm.OutcomeSucceeded, 200,
		llm.Usage{Input: 20, CacheRead: 10, Output: 4, Known: true})
}

func TestStreamWithoutUsageIsUnknown(t *testing.T) {
	x := newHarness(t, openaiStream(false))
	streamBody := `{"model":"gpt","stream":true,"messages":[]}`
	resp := x.stream(context.Background(), "/v1/chat/completions", streamBody, openaiKey())
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	wantSettled(t, x.ledger.settlement(t), llm.OutcomeUsageUnknown, 200, llm.Usage{})

	for name, events := range map[string][]string{
		"no message_delta": {anthropicEvents[0], anthropicEvents[3], anthropicEvents[6]},
		"no message_stop":  anthropicEvents[:6],
		"error event": append(append([]string{}, anthropicEvents[:4]...),
			"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"),
		"partial event": {anthropicEvents[0], anthropicEvents[5], "event: message_stop\ndata: {\"type\""},
	} {
		x.prov.set(func(w http.ResponseWriter, _ *http.Request) { sse(w, events, nil) })
		resp := x.stream(context.Background(), "/v1/messages", `{"model":"sonnet","max_tokens":9,"stream":true}`, anthropicKey())
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if got := x.ledger.settlement(t); got.Outcome != llm.OutcomeUsageUnknown || got.Usage.Known {
			t.Fatalf("%s: %+v", name, got)
		}
	}
}

func TestClientDisconnectCancelsUpstream(t *testing.T) {
	cancelled := make(chan struct{})
	x := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		sse(w, anthropicEvents[:1], nil)
		<-r.Context().Done()
		close(cancelled)
	})
	ctx, cancel := context.WithCancel(context.Background())
	resp := x.stream(ctx, "/v1/messages", `{"model":"sonnet","max_tokens":9,"stream":true}`, anthropicKey())
	readEvent(t, bufio.NewReader(resp.Body))
	cancel()
	resp.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("the upstream request was not cancelled")
	}
	wantSettled(t, x.ledger.settlement(t), llm.OutcomeUsageUnknown, 200, llm.Usage{})
}

func TestProviderErrorIsReleased(t *testing.T) {
	for _, status := range []int{429, 500} {
		reply := fmt.Sprintf(`{"type":"error","error":{"type":"rate_limit_error","message":"status %d"}}`, status)
		x := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("request-id", "req_err")
			w.Header().Set("Set-Cookie", "provider=1")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, reply)
		})
		for _, body := range []string{messagesBody, `{"model":"sonnet","max_tokens":9,"stream":true}`} {
			code, h, got := x.post("/v1/messages", body, anthropicKey())
			if code != status || string(got) != reply || h.Get("request-id") != "req_err" || h.Get("Set-Cookie") != "" {
				t.Fatalf("%d: %d %v %s", status, code, h, got)
			}
			wantSettled(t, x.ledger.settlement(t), llm.OutcomeProviderError, status, llm.Usage{})
		}
	}
}

func TestKillCutsAStream(t *testing.T) {
	cancelled := make(chan struct{})
	x := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		sse(w, anthropicEvents[:1], nil)
		f := w.(http.Flusher)
		for {
			select {
			case <-r.Context().Done():
				close(cancelled)
				return
			case <-time.After(20 * time.Millisecond):
				_, _ = io.WriteString(w, anthropicEvents[3])
				f.Flush()
			}
		}
	})
	resp := x.stream(context.Background(), "/v1/messages", `{"model":"sonnet","max_tokens":9,"stream":true}`, anthropicKey())
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	readEvent(t, br)
	x.ledger.killed.Store(true)
	x.ledger.epoch.Add(1)
	start := time.Now()
	rest, _ := io.ReadAll(br)
	if time.Since(start) > 2*time.Second {
		t.Fatalf("the stream ran %v after the kill", time.Since(start))
	}
	if !strings.HasSuffix(string(rest),
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"permission_error\",\"message\":\"eacp: killed\"}}\n\n") {
		t.Fatalf("stream tail %q", rest[max(0, len(rest)-200):])
	}
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream not cancelled")
	}
	wantSettled(t, x.ledger.settlement(t), llm.OutcomeKilled, 200, llm.Usage{})

	// OpenAI streams end with an error chunk.
	x.ledger.killed.Store(false)
	x.prov.set(func(w http.ResponseWriter, r *http.Request) {
		sse(w, openaiEvents(false)[:1], nil)
		<-r.Context().Done()
	})
	resp2 := x.stream(context.Background(), "/v1/chat/completions", `{"model":"gpt","stream":true}`, openaiKey())
	defer resp2.Body.Close()
	br = bufio.NewReader(resp2.Body)
	readEvent(t, br)
	x.ledger.killed.Store(true)
	x.ledger.epoch.Add(1)
	rest, _ = io.ReadAll(br)
	if !strings.HasSuffix(string(rest),
		"data: {\"error\":{\"message\":\"eacp: killed\",\"type\":\"permission_denied\",\"code\":\"killed\"}}\n\n") {
		t.Fatalf("stream tail %q", rest)
	}
	wantSettled(t, x.ledger.settlement(t), llm.OutcomeKilled, 200, llm.Usage{})
}

func TestAKillDuringAdmissionStopsTheCallBeforeTheProvider(t *testing.T) {
	x := newHarness(t, anthropicOK)
	x.ledger.set(func(l *fakeLedger) {
		l.onAdmit = func() { l.epoch.Add(1); l.killed.Store(true) }
	})
	code, _, body := x.post("/v1/messages", messagesBody, anthropicKey())
	if code != 403 || !strings.Contains(string(body), "eacp: killed") {
		t.Fatalf("%d %s", code, body)
	}
	wantSettled(t, x.ledger.settlement(t), llm.OutcomeKilled, 0, llm.Usage{})
	if len(x.prov.requests()) != 0 {
		t.Fatal("a killed call reached the provider")
	}
}

func TestLimits(t *testing.T) {
	big := 16<<20 + 10
	x := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"pad":"`+strings.Repeat("a", big)+`","usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	code, _, body := x.post("/v1/messages", messagesBody, anthropicKey())
	if code != 502 || !strings.Contains(string(body), "eacp: response_too_large") {
		t.Fatalf("%d %.200s", code, body)
	}
	wantSettled(t, x.ledger.settlement(t), llm.OutcomeUsageUnknown, 200, llm.Usage{})

	x.prov.set(func(w http.ResponseWriter, _ *http.Request) {
		sse(w, []string{anthropicEvents[0], "event: content_block_delta\ndata: " + strings.Repeat("x", 1<<20+1) + "\n\n",
			anthropicEvents[5], anthropicEvents[6]}, nil)
	})
	resp := x.stream(context.Background(), "/v1/messages", `{"model":"sonnet","max_tokens":9,"stream":true}`, anthropicKey())
	all, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(all), "message_stop") || !strings.Contains(string(all), "eacp: event_too_large") {
		t.Fatalf("relayed past an oversized event: %.300q", all)
	}
	wantSettled(t, x.ledger.settlement(t), llm.OutcomeUsageUnknown, 200, llm.Usage{})

	// A timeout commits the reservation.
	y := newHarness(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	y.ledger.set(func(l *fakeLedger) { l.model.Timeout = 200 * time.Millisecond })
	code, _, body = y.post("/v1/messages", messagesBody, anthropicKey())
	if code != 504 || !strings.Contains(string(body), "eacp: provider_timeout") {
		t.Fatalf("%d %s", code, body)
	}
	wantSettled(t, y.ledger.settlement(t), llm.OutcomeUsageUnknown, 0, llm.Usage{})
}

func TestNoRedirectNoProxy(t *testing.T) {
	var elsewhere atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhere.Add(1)
		anthropicOK(w, nil)
	}))
	defer other.Close()
	x := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/v1/messages", http.StatusTemporaryRedirect)
	})
	code, h, _ := x.post("/v1/messages", messagesBody, anthropicKey())
	if code != 307 || h.Get("Location") != "" || elsewhere.Load() != 0 {
		t.Fatalf("redirect: %d %v, %d requests elsewhere", code, h, elsewhere.Load())
	}
	wantSettled(t, x.ledger.settlement(t), llm.OutcomeProviderError, 307, llm.Usage{})

	// A provider host the environment's proxy would serve: the gateway dials
	// it directly (and fails), never through the proxy.
	y := newHarness(t, anthropicOK, func(o *llmgateway.Options) { o.Secrets = secretsFor(t, "provider.invalid") })
	y.ledger.set(func(l *fakeLedger) { l.model.BaseURL = "http://provider.invalid" })
	before := proxyHits.Load()
	code, _, body := y.post("/v1/messages", messagesBody, anthropicKey())
	if code != 502 || proxyHits.Load() != before {
		t.Fatalf("%d %s; proxy hits %d -> %d", code, body, before, proxyHits.Load())
	}
	// The name did not resolve: nothing was sent, so nothing is charged.
	wantSettled(t, y.ledger.settlement(t), llm.OutcomeProviderError, 0, llm.Usage{})
}

// A provider that refuses the connection received nothing: the call is a
// provider error and its reservation is released (ADR-031 §5, as the HTTP
// connector's connection_refused_before_send).
func TestAConnectionThatNeverOpenedIsReleased(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	x := newHarness(t, anthropicOK, func(o *llmgateway.Options) { o.Secrets = secretsFor(t, addr) })
	x.ledger.set(func(l *fakeLedger) { l.model.BaseURL = "http://" + addr })
	code, _, body := x.post("/v1/messages", messagesBody, anthropicKey())
	if code != 502 || !strings.Contains(string(body), "eacp: provider_unreachable") {
		t.Fatalf("%d %s", code, body)
	}
	wantSettled(t, x.ledger.settlement(t), llm.OutcomeProviderError, 0, llm.Usage{})
}

func TestSigningCredentialIsRefused(t *testing.T) {
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		_, _ = fmt.Fprintf(w, `<AssumeRoleWithWebIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">
  <AssumeRoleWithWebIdentityResult><Credentials><AccessKeyId>ASIAEACPTESTKEY00001</AccessKeyId>
  <SecretAccessKey>aws-secret-canary</SecretAccessKey><SessionToken>aws-session-canary</SessionToken>
  <Expiration>%s</Expiration></Credentials></AssumeRoleWithWebIdentityResult>
</AssumeRoleWithWebIdentityResponse>`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	}))
	defer sts.Close()
	dir := t.TempDir()
	subject := filepath.Join(dir, "subject")
	now := time.Now()
	if err := os.WriteFile(subject, []byte(jwttest.New(t).Sign(map[string]any{"sub": "system:serviceaccount:eacp:gw",
		"aud": "sts.amazonaws.com", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()})), 0o600); err != nil {
		t.Fatal(err)
	}
	x := newHarness(t, anthropicOK) // the provider the signing credential is bound to
	u, _ := url.Parse(x.prov.srv.URL)
	path := filepath.Join(dir, "secrets.json")
	doc := fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"llm","host":%q,"aws":{"role_arn":"arn:aws:iam::123456789012:role/eacp",`+
		`"region":"eu-west-1","service":"execute-api","sts_endpoint":%q,"subject_token":{"file":%q}}}]}`,
		tenant, u.Host, sts.URL+"/", subject)
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := worker.LoadSecrets(path, worker.AllowPlainTokenURL())
	if err != nil {
		t.Fatal(err)
	}
	y := newHarness(t, anthropicOK, func(o *llmgateway.Options) { o.Secrets = store })
	y.ledger.set(func(l *fakeLedger) { l.model.BaseURL = x.prov.srv.URL })
	code, _, body := y.post("/v1/messages", messagesBody, anthropicKey())
	if code != 503 || !strings.Contains(string(body), "eacp: credential_unavailable") {
		t.Fatalf("%d %s", code, body)
	}
	wantSettled(t, y.ledger.settlement(t), llm.OutcomeProviderError, 0, llm.Usage{})

	// No credential for the model's host: the same.
	z := newHarness(t, anthropicOK, func(o *llmgateway.Options) { o.Secrets = secretsFor(t, "elsewhere:443") })
	code, _, body = z.post("/v1/messages", messagesBody, anthropicKey())
	if code != 503 || !strings.Contains(string(body), "eacp: credential_unavailable") {
		t.Fatalf("%d %s", code, body)
	}
	wantSettled(t, z.ledger.settlement(t), llm.OutcomeProviderError, 0, llm.Usage{})
	if len(x.prov.requests())+len(z.prov.requests()) != 0 {
		t.Fatal("a call without a usable credential reached the provider")
	}
}

func TestNothingSecretIsLogged(t *testing.T) {
	x := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"RESPONSE-CANARY"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	prompt := `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":"PROMPT-CANARY"}]}`
	h := anthropicKey()
	h["EACP-Subject"] = "alice@example.com"
	x.post("/v1/messages", prompt, h)
	x.ledger.settlement(t)
	x.ledger.set(func(l *fakeLedger) { l.denial = "budget_exceeded" })
	x.post("/v1/messages", prompt, h)
	x.ledger.set(func(l *fakeLedger) { l.denial = "" })
	x.prov.set(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = io.WriteString(w, `{"error":"RESPONSE-CANARY"}`)
	})
	x.post("/v1/messages", prompt, h)
	x.ledger.settlement(t)
	x.post("/v1/messages", prompt, map[string]string{"x-api-key": "eacp_wrong_key_canary"})
	logs := x.logs.String()
	if !strings.Contains(logs, "llm call settled") {
		t.Fatalf("no settlement was logged: %s", logs)
	}
	for _, canary := range []string{agentKey, providerKey, "PROMPT-CANARY", "RESPONSE-CANARY", "eacp_wrong_key_canary"} {
		if strings.Contains(logs, canary) {
			t.Fatalf("the logs contain %q:\n%s", canary, logs)
		}
	}
	var line map[string]any
	for _, l := range strings.Split(strings.TrimSpace(logs), "\n") {
		if strings.Contains(l, "llm call settled") {
			_ = json.Unmarshal([]byte(l), &line)
		}
	}
	for _, k := range []string{"call", "tenant", "agent", "model", "outcome", "status"} {
		if _, ok := line[k]; !ok {
			t.Fatalf("settlement log lacks %s: %v", k, line)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The upstream request cannot be rewound: without GetBody the transport
// never resends it (an HTTP/2 stream reset after the provider read it), so
// an admitted call reaches the provider at most once (ADR-031 §5).
func TestTheUpstreamRequestIsNeverResent(t *testing.T) {
	var rewindable atomic.Bool
	x := newHarness(t, anthropicOK, func(o *llmgateway.Options) {
		o.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			rewindable.Store(r.GetBody != nil)
			return http.DefaultTransport.RoundTrip(r)
		})}
	})
	if code, _, body := x.post("/v1/messages", messagesBody, anthropicKey()); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	x.ledger.settlement(t)
	if rewindable.Load() {
		t.Fatal("the upstream request carries GetBody: the transport may send it twice")
	}
}

// A provider that refuses the gateway's credential answers about the
// gateway's key, not the agent's: its body (which may echo part of the key)
// is never relayed, and the agent's SDK is not told its own key is bad.
func TestAProviderAuthFailureIsNotRelayed(t *testing.T) {
	for _, status := range []int{401, 403} {
		reply := `{"error":{"message":"Incorrect API key provided: sk-proj-****` + providerKey[len(providerKey)-4:] + `","type":"invalid_request_error"}}`
		x := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, reply)
		})
		code, _, body := x.post("/v1/chat/completions", `{"model":"gpt","messages":[]}`, openaiKey())
		if code != 502 || !strings.Contains(string(body), "eacp: provider_auth_failed") ||
			strings.Contains(string(body), providerKey[len(providerKey)-4:]) {
			t.Fatalf("%d: %d %s", status, code, body)
		}
		wantSettled(t, x.ledger.settlement(t), llm.OutcomeProviderError, status, llm.Usage{})
	}
}
