package fakellm_test

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"eacp/internal/fakellm"
)

const key = "fakellm-provider-key-canary"

func serve(t *testing.T, data string) *httptest.Server {
	t.Helper()
	h, err := fakellm.New(key, data)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

func call(t *testing.T, s *httptest.Server, path, body string, header map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest("POST", s.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

var (
	anth = map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"}
	oai  = map[string]string{"Authorization": "Bearer " + key}
)

func messages(text string, stream bool) string {
	b, _ := json.Marshal(map[string]any{"model": "claude-fake", "max_tokens": 100, "stream": stream,
		"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}}}})
	return string(b)
}

func chat(text string, stream bool) string {
	b, _ := json.Marshal(map[string]any{"model": "gpt-fake", "stream": stream,
		"stream_options": map[string]any{"include_usage": true},
		"messages":       []any{map[string]any{"role": "user", "content": text}}})
	return string(b)
}

func TestFakeLLMServesBothShapes(t *testing.T) {
	s := serve(t, filepath.Join(t.TempDir(), "llm.log"))
	prompt := strings.Repeat("p", 40) // input = 10 + 40/4 = 20

	resp, body := call(t, s, "/v1/messages", messages(prompt, false), anth)
	var m struct {
		Type    string `json:"type"`
		Model   string `json:"model"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Usage map[string]int64 `json:"usage"`
	}
	if err := json.Unmarshal(body, &m); err != nil || resp.StatusCode != 200 || m.Type != "message" ||
		m.Model != "claude-fake" || len(m.Content) != 1 || m.Usage["input_tokens"] != 20 || m.Usage["output_tokens"] != 20 ||
		resp.Header.Get("request-id") == "" {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}

	resp, body = call(t, s, "/v1/chat/completions", chat(prompt, false), oai)
	var c struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			Prompt     int64 `json:"prompt_tokens"`
			Completion int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &c); err != nil || resp.StatusCode != 200 || c.Object != "chat.completion" ||
		len(c.Choices) != 1 || c.Usage.Prompt != 20 || c.Usage.Completion != 20 || resp.Header.Get("x-request-id") == "" {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}

	resp, body = call(t, s, "/v1/messages", messages(prompt, true), anth)
	text := string(body)
	if resp.Header.Get("Content-Type") != "text/event-stream" || !strings.HasPrefix(text, "event: message_start\n") ||
		!strings.Contains(text, `"input_tokens":20`) || !strings.Contains(text, "event: message_delta\n") ||
		!strings.Contains(text, `"usage":{"output_tokens":20}`) || !strings.HasSuffix(text, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n") {
		t.Fatalf("anthropic stream %q", text)
	}
	resp, body = call(t, s, "/v1/chat/completions", chat(prompt, true), oai)
	text = string(body)
	if resp.Header.Get("Content-Type") != "text/event-stream" || !strings.Contains(text, `"prompt_tokens":20`) ||
		!strings.HasSuffix(text, "data: [DONE]\n\n") {
		t.Fatalf("openai stream %q", text)
	}
	// Without include_usage, no usage chunk.
	resp, body = call(t, s, "/v1/chat/completions", `{"model":"gpt-fake","stream":true,"messages":[{"role":"user","content":"hi"}]}`, oai)
	if strings.Contains(string(body), "prompt_tokens") || resp.StatusCode != 200 {
		t.Fatalf("usage without include_usage: %q", body)
	}
}

func TestFakeLLMScenarios(t *testing.T) {
	s := serve(t, filepath.Join(t.TempDir(), "llm.log"))
	for _, status := range []int{429, 500} {
		name := map[int]string{429: "error_429", 500: "error_500"}[status]
		resp, body := call(t, s, "/v1/messages", messages(name, false), anth)
		if resp.StatusCode != status || !strings.Contains(string(body), `"type":"error"`) {
			t.Fatalf("%s: %d %s", name, resp.StatusCode, body)
		}
		resp, body = call(t, s, "/v1/chat/completions", chat(name, true), oai)
		if resp.StatusCode != status || !strings.Contains(string(body), `"error"`) {
			t.Fatalf("%s: %d %s", name, resp.StatusCode, body)
		}
	}
	_, body := call(t, s, "/v1/messages", messages("no_usage", false), anth)
	if strings.Contains(string(body), "usage") {
		t.Fatalf("no_usage: %s", body)
	}
	_, body = call(t, s, "/v1/messages", messages("no_usage", true), anth)
	if strings.Contains(string(body), "message_delta") || !strings.Contains(string(body), "message_stop") {
		t.Fatalf("no_usage stream: %s", body)
	}
	_, body = call(t, s, "/v1/chat/completions", chat("no_usage", true), oai)
	if strings.Contains(string(body), "prompt_tokens") || !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("no_usage stream: %s", body)
	}

	// cut: two events, then the connection drops.
	req, _ := http.NewRequest("POST", s.URL+"/v1/messages", strings.NewReader(messages("cut", true)))
	for k, v := range anth {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err == nil || strings.Count(string(b), "event: ") != 2 {
		t.Fatalf("cut: %v %q", err, b)
	}

	// slow: an event a second.
	req, _ = http.NewRequest("POST", s.URL+"/v1/messages", strings.NewReader(messages("slow", true)))
	for k, v := range anth {
		req.Header.Set(k, v)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(resp.Body)
	start := time.Now()
	events := 0
	for events < 4 {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "event: ") {
			events++
		}
	}
	resp.Body.Close()
	if d := time.Since(start); d < time.Second || d > 5*time.Second {
		t.Fatalf("four slow events took %v", d)
	}
}

func TestFakeLLMNeedsItsKey(t *testing.T) {
	s := serve(t, filepath.Join(t.TempDir(), "llm.log"))
	for _, c := range []struct {
		path   string
		header map[string]string
	}{
		{"/v1/messages", nil},
		{"/v1/messages", map[string]string{"x-api-key": "wrong"}},
		{"/v1/messages", map[string]string{"Authorization": "Bearer " + key}}, // Anthropic takes x-api-key
		{"/v1/chat/completions", map[string]string{"x-api-key": key}},         // OpenAI takes a Bearer
		{"/v1/chat/completions", map[string]string{"Authorization": "Bearer wrong"}},
	} {
		if resp, _ := call(t, s, c.path, messages("hi", false), c.header); resp.StatusCode != 401 {
			t.Fatalf("%s %v: %d", c.path, c.header, resp.StatusCode)
		}
	}
	req, _ := http.NewRequest("GET", s.URL+"/v1/audit", nil)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("unkeyed audit: %d", resp.StatusCode)
	}
	if _, err := fakellm.New("", filepath.Join(t.TempDir(), "x")); err == nil {
		t.Fatal("started without a key")
	}
}

func audit(t *testing.T, s *httptest.Server) []map[string]any {
	t.Helper()
	req, _ := http.NewRequest("GET", s.URL+"/v1/audit", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Calls []map[string]any `json:"calls"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != 200 {
		t.Fatalf("audit: %d %v", resp.StatusCode, err)
	}
	return out.Calls
}

func TestFakeLLMAuditHasNoContent(t *testing.T) {
	data := filepath.Join(t.TempDir(), "llm.log")
	s := serve(t, data)
	call(t, s, "/v1/messages", messages("PROMPT-CANARY", false), anth)
	call(t, s, "/v1/chat/completions", chat("error_429", false), oai)
	calls := audit(t, s)
	if len(calls) != 2 || calls[0]["api"] != "anthropic" || calls[0]["model"] != "claude-fake" ||
		calls[0]["stream"] != false || calls[0]["status"] != float64(200) || calls[0]["input_tokens"] != float64(13) ||
		calls[0]["output_tokens"] != float64(20) || calls[1]["status"] != float64(429) || calls[1]["scenario"] != "error_429" {
		t.Fatalf("audit %v", calls)
	}
	raw, _ := os.ReadFile(data)
	for _, canary := range []string{"PROMPT-CANARY", key, "Hello from fakellm"} {
		if strings.Contains(string(raw), canary) {
			t.Fatalf("the log keeps %q", canary)
		}
	}
}

func TestFakeLLMLogSurvivesARestart(t *testing.T) {
	data := filepath.Join(t.TempDir(), "llm.log")
	s := serve(t, data)
	call(t, s, "/v1/messages", messages("hi", false), anth)
	s.Close()
	s = serve(t, data)
	call(t, s, "/v1/messages", messages("hi", false), anth)
	if n := len(audit(t, s)); n != 2 {
		t.Fatalf("%d calls after a restart", n)
	}
	if err := os.WriteFile(data, []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fakellm.New(key, data); err == nil {
		t.Fatal("a corrupt log was accepted")
	}
}
