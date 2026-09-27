// Package fakellm is the development LLM provider (ADR-031 §3.7): it speaks
// Anthropic Messages and OpenAI Chat Completions, requires its own key (it
// keeps only the key's SHA-256), answers with fixed text and exact usage,
// and keeps a durable JSONL audit of every call without any content.
//
// The last user message picks a scenario: error_429, error_500, no_usage,
// cut (a stream dropped after two events) or slow (an event a second for
// 60 s); anything else is a normal answer. Usage: input = 10 + the last user
// message's bytes / 4, output = 20.
package fakellm

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Reply is the fixed answer text.
const Reply = "Hello from fakellm."

const outputTokens = 20

// Entry is one audited call: never a prompt, an answer or a key.
type Entry struct {
	At           time.Time `json:"at"`
	API          string    `json:"api"` // anthropic or openai
	Model        string    `json:"model"`
	Stream       bool      `json:"stream"`
	Scenario     string    `json:"scenario,omitempty"`
	Status       int       `json:"status"`
	InputTokens  int64     `json:"input_tokens,omitempty"`
	OutputTokens int64     `json:"output_tokens,omitempty"`
}

type provider struct {
	key  [32]byte
	path string
	mu   sync.Mutex
	log  []Entry
}

// New loads the durable audit log at dataPath (a corrupt log fails
// closed) and returns the provider's handler.
func New(key, dataPath string) (http.Handler, error) {
	if key == "" || dataPath == "" {
		return nil, errors.New("fakellm: a key and a data path are required")
	}
	f, err := os.OpenFile(dataPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("fakellm: open audit log: %w", err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return nil, fmt.Errorf("fakellm: sync audit log: %w", err)
	}
	if err := syncDir(filepath.Dir(dataPath)); err != nil {
		return nil, err
	}
	p := &provider{key: sha256.Sum256([]byte(key)), path: dataPath}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil || e.At.IsZero() {
			return nil, errors.New("fakellm: corrupt audit log")
		}
		p.log = append(p.log, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("fakellm: read audit log: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) { p.serve(w, r, "anthropic") })
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) { p.serve(w, r, "openai") })
	mux.HandleFunc("GET /v1/audit", p.audit)
	return mux, nil
}

func (p *provider) keyed(got string) bool {
	sum := sha256.Sum256([]byte(got))
	return got != "" && subtle.ConstantTimeCompare(sum[:], p.key[:]) == 1
}

func bearer(r *http.Request) string {
	v, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return v
}

func (p *provider) audit(w http.ResponseWriter, r *http.Request) {
	if !p.keyed(bearer(r)) && !p.keyed(r.Header.Get("x-api-key")) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	p.mu.Lock()
	calls := append([]Entry{}, p.log...)
	p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"calls": calls})
}

// record appends e durably before the answer is sent.
func (p *provider) record(e Entry) (int, error) {
	e.At = time.Now().UTC()
	b, err := json.Marshal(e)
	if err != nil {
		return 0, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	f, err := os.OpenFile(p.path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	_, err = f.Write(append(b, '\n'))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	p.log = append(p.log, e)
	return len(p.log), nil
}

type request struct {
	Model         string `json:"model"`
	Stream        bool   `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

// lastUserText is the text of the last user message: a string, or the text
// blocks of a content array.
func (r request) lastUserText() string {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		m := r.Messages[i]
		if m.Role != "user" {
			continue
		}
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			return s
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal(m.Content, &blocks)
		var b strings.Builder
		for _, x := range blocks {
			if x.Type == "text" {
				b.WriteString(x.Text)
			}
		}
		return b.String()
	}
	return ""
}

var scenarios = map[string]bool{"error_429": true, "error_500": true, "no_usage": true, "cut": true, "slow": true}

func (p *provider) serve(w http.ResponseWriter, r *http.Request, api string) {
	key := bearer(r)
	if api == "anthropic" {
		key = r.Header.Get("x-api-key")
	}
	if !p.keyed(key) {
		writeError(w, api, http.StatusUnauthorized, "authentication_error", "invalid key")
		return
	}
	var req request
	if err := json.NewDecoder(io.LimitReader(r.Body, 32<<20)).Decode(&req); err != nil || req.Model == "" {
		writeError(w, api, http.StatusBadRequest, "invalid_request_error", "invalid body")
		return
	}
	text := strings.TrimSpace(req.lastUserText())
	scenario := ""
	if scenarios[text] {
		scenario = text
	}
	e := Entry{API: api, Model: req.Model, Stream: req.Stream, Scenario: scenario, Status: http.StatusOK}
	switch scenario {
	case "error_429":
		e.Status = http.StatusTooManyRequests
	case "error_500":
		e.Status = http.StatusInternalServerError
	default:
		e.InputTokens, e.OutputTokens = 10+int64(len(req.lastUserText()))/4, outputTokens
	}
	n, err := p.record(e)
	if err != nil {
		writeError(w, api, http.StatusInternalServerError, "api_error", "audit log unavailable")
		return
	}
	id := fmt.Sprintf("fake_%d", n)
	switch e.Status {
	case http.StatusTooManyRequests:
		writeError(w, api, e.Status, "rate_limit_error", "rate limited")
		return
	case http.StatusInternalServerError:
		writeError(w, api, e.Status, "api_error", "internal error")
		return
	}
	usage := scenario != "no_usage"
	if !req.Stream {
		if scenario == "slow" {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(60 * time.Second):
			}
		}
		p.answer(w, r, api, id, req, e, usage, scenario == "cut")
		return
	}
	if api == "anthropic" {
		p.anthropicStream(w, r, id, req, e, usage, scenario)
		return
	}
	p.openaiStream(w, r, id, req, e, usage && req.StreamOptions != nil && req.StreamOptions.IncludeUsage, scenario)
}

func (p *provider) answer(w http.ResponseWriter, r *http.Request, api, id string, req request, e Entry, usage, cut bool) {
	var body map[string]any
	if api == "anthropic" {
		w.Header().Set("request-id", "req_"+id)
		body = map[string]any{"id": "msg_" + id, "type": "message", "role": "assistant", "model": req.Model,
			"content": []any{map[string]any{"type": "text", "text": Reply}}, "stop_reason": "end_turn",
			"stop_sequence": nil}
		if usage {
			body["usage"] = map[string]any{"input_tokens": e.InputTokens, "output_tokens": e.OutputTokens,
				"cache_creation_input_tokens": 0, "cache_read_input_tokens": 0}
		}
	} else {
		w.Header().Set("x-request-id", "req_"+id)
		body = map[string]any{"id": "chatcmpl-" + id, "object": "chat.completion", "created": time.Now().Unix(),
			"model": req.Model, "choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": Reply}}}}
		if usage {
			body["usage"] = map[string]any{"prompt_tokens": e.InputTokens, "completion_tokens": e.OutputTokens,
				"total_tokens": e.InputTokens + e.OutputTokens, "prompt_tokens_details": map[string]any{"cached_tokens": 0}}
		}
	}
	b, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
	if cut {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b[:len(b)/2])
		http.NewResponseController(w).Flush()
		panic(http.ErrAbortHandler)
	}
	_, _ = w.Write(b)
}

// events writes a stream: each event is flushed; cut drops the connection
// after two events; slow waits a second before each content event.
type events struct {
	w     http.ResponseWriter
	r     *http.Request
	rc    *http.ResponseController
	count int
	cut   bool
}

func (s *events) send(event string, data any) bool {
	if s.cut && s.count == 2 {
		panic(http.ErrAbortHandler)
	}
	b, _ := json.Marshal(data)
	var buf bytes.Buffer
	if event != "" {
		fmt.Fprintf(&buf, "event: %s\n", event)
	}
	fmt.Fprintf(&buf, "data: %s\n\n", b)
	if _, err := s.w.Write(buf.Bytes()); err != nil {
		return false
	}
	s.count++
	return s.rc.Flush() == nil
}

func (s *events) wait() bool {
	select {
	case <-s.r.Context().Done():
		return false
	case <-time.After(time.Second):
		return true
	}
}

func startStream(w http.ResponseWriter, r *http.Request, header, id, scenario string) *events {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set(header, "req_"+id)
	w.WriteHeader(http.StatusOK)
	return &events{w: w, r: r, rc: http.NewResponseController(w), cut: scenario == "cut"}
}

func (p *provider) anthropicStream(w http.ResponseWriter, r *http.Request, id string, req request, e Entry,
	usage bool, scenario string) {
	s := startStream(w, r, "request-id", id, scenario)
	ok := s.send("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_" + id, "type": "message", "role": "assistant", "model": req.Model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": e.InputTokens,
			"output_tokens": 1, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0}}}) &&
		s.send("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""}})
	chunks := []string{Reply}
	if scenario == "slow" {
		chunks = make([]string, 60)
		for i := range chunks {
			chunks[i] = "."
		}
	}
	for _, c := range chunks {
		if !ok || (scenario == "slow" && !s.wait()) {
			return
		}
		ok = s.send("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": c}})
	}
	ok = ok && s.send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	if usage {
		ok = ok && s.send("message_delta", map[string]any{"type": "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": e.OutputTokens}})
	}
	_ = ok && s.send("message_stop", map[string]any{"type": "message_stop"})
}

func (p *provider) openaiStream(w http.ResponseWriter, r *http.Request, id string, req request, e Entry,
	usage bool, scenario string) {
	s := startStream(w, r, "x-request-id", id, scenario)
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{"id": "chatcmpl-" + id, "object": "chat.completion.chunk", "created": time.Now().Unix(),
			"model": req.Model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
			"usage": nil}
	}
	ok := s.send("", chunk(map[string]any{"role": "assistant", "content": ""}, nil))
	chunks := []string{Reply}
	if scenario == "slow" {
		chunks = make([]string, 60)
		for i := range chunks {
			chunks[i] = "."
		}
	}
	for _, c := range chunks {
		if !ok || (scenario == "slow" && !s.wait()) {
			return
		}
		ok = s.send("", chunk(map[string]any{"content": c}, nil))
	}
	ok = ok && s.send("", chunk(map[string]any{}, "stop"))
	if usage {
		ok = ok && s.send("", map[string]any{"id": "chatcmpl-" + id, "object": "chat.completion.chunk",
			"created": time.Now().Unix(), "model": req.Model, "choices": []any{},
			"usage": map[string]any{"prompt_tokens": e.InputTokens, "completion_tokens": e.OutputTokens,
				"total_tokens": e.InputTokens + e.OutputTokens, "prompt_tokens_details": map[string]any{"cached_tokens": 0}}})
	}
	if ok {
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		_ = s.rc.Flush()
	}
}

func writeError(w http.ResponseWriter, api string, status int, kind, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if api == "anthropic" {
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]any{"type": kind, "message": msg}})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "type": kind, "code": kind}})
}
