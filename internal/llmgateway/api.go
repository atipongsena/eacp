package llmgateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"eacp/internal/llm"
	"eacp/internal/worker"
)

// api is a provider request shape the gateway serves.
type api int

const (
	anthropic api = iota // Anthropic Messages
	openai               // OpenAI Chat Completions
)

func (a api) provider() string {
	if a == anthropic {
		return "anthropic"
	}
	return "openai"
}

func (a api) route() string {
	if a == anthropic {
		return "/v1/messages"
	}
	return "/v1/chat/completions"
}

// errorBody is a provider-shaped error whose message is "eacp: <code>"
// (and detail, if any).
func (a api) errorBody(status int, code, detail string) []byte {
	msg := "eacp: " + code
	if detail != "" {
		msg += ": " + detail
	}
	if a == anthropic {
		return fmt.Appendf(nil, `{"type":"error","error":{"type":%s,"message":%s}}`,
			jsonString(anthropicErrorType(status)), jsonString(msg))
	}
	return fmt.Appendf(nil, `{"error":{"message":%s,"type":%s,"code":%s}}`,
		jsonString(msg), jsonString(openaiErrorType(status)), jsonString(code))
}

func anthropicErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	}
	return "api_error"
}

func openaiErrorType(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusRequestEntityTooLarge:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_denied"
	}
	return "server_error"
}

// streamError is a provider-shaped error event that ends a relayed stream.
func (a api) streamError(status int, code string) []byte {
	if a == anthropic {
		return append(append([]byte("event: error\ndata: "), a.errorBody(status, code, "")...), "\n\n"...)
	}
	return append(append([]byte("data: "), a.errorBody(status, code, "")...), "\n\n"...)
}

// authorize sets the provider credential on an upstream request.
func (a api) authorize(h http.Header, secret worker.Secret) {
	if a == anthropic {
		h.Set("x-api-key", secret.Reveal())
		return
	}
	h.Set("Authorization", "Bearer "+secret.Reveal())
}

// forwarded lists the agent headers a request shape passes upstream,
// besides Accept and the W3C trace context.
func (a api) forwarded() []string {
	if a == anthropic {
		return []string{"anthropic-version", "anthropic-beta"}
	}
	return nil
}

// ------------------------------------------------------------ usage

type anthropicUsage struct {
	Input      *int64 `json:"input_tokens"`
	Output     *int64 `json:"output_tokens"`
	CacheWrite *int64 `json:"cache_creation_input_tokens"`
	CacheRead  *int64 `json:"cache_read_input_tokens"`
}

// full is a complete usage: input and output present, nothing negative.
func (u *anthropicUsage) full() (llm.Usage, bool) {
	if u == nil || u.Input == nil || u.Output == nil {
		return llm.Usage{}, false
	}
	out := llm.Usage{Input: *u.Input, Output: *u.Output, CacheWrite: val(u.CacheWrite), CacheRead: val(u.CacheRead),
		Known: true}
	return out, out.Input >= 0 && out.Output >= 0 && out.CacheWrite >= 0 && out.CacheRead >= 0
}

type openaiUsage struct {
	Prompt     *int64 `json:"prompt_tokens"`
	Completion *int64 `json:"completion_tokens"`
	Details    *struct {
		Cached *int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// usage maps OpenAI's counts: cached tokens are part of the prompt.
func (u *openaiUsage) usage() (llm.Usage, bool) {
	if u == nil || u.Prompt == nil || u.Completion == nil {
		return llm.Usage{}, false
	}
	var cached int64
	if u.Details != nil {
		cached = val(u.Details.Cached)
	}
	if *u.Prompt < 0 || *u.Completion < 0 || cached < 0 || cached > *u.Prompt {
		return llm.Usage{}, false
	}
	return llm.Usage{Input: *u.Prompt - cached, CacheRead: cached, Output: *u.Completion, Known: true}, true
}

func val(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// usage reads a non-streamed response's usage.
func (a api) usage(body []byte) (llm.Usage, bool) {
	if a == anthropic {
		var r struct {
			Usage *anthropicUsage `json:"usage"`
		}
		if json.Unmarshal(body, &r) != nil {
			return llm.Usage{}, false
		}
		return r.Usage.full()
	}
	var r struct {
		Usage *openaiUsage `json:"usage"`
	}
	if json.Unmarshal(body, &r) != nil {
		return llm.Usage{}, false
	}
	return r.Usage.usage()
}

// tracker follows a stream's events to its usage.
type tracker interface {
	observe(data []byte)
	usage() (llm.Usage, bool) // known only once the stream completed with usage
}

func (a api) tracker() tracker {
	if a == anthropic {
		return &anthropicTracker{}
	}
	return &openaiTracker{}
}

// anthropicTracker: message_start carries the input usage, each
// message_delta the cumulative usage, message_stop the end; an error event
// ends the stream without a usage.
type anthropicTracker struct {
	start, delta, stop, failed bool
	u                          llm.Usage
}

func (t *anthropicTracker) observe(data []byte) {
	var ev struct {
		Type    string `json:"type"`
		Message *struct {
			Usage *anthropicUsage `json:"usage"`
		} `json:"message"`
		Usage *anthropicUsage `json:"usage"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	switch ev.Type {
	case "message_start":
		if ev.Message == nil {
			t.failed = true
			return
		}
		u, ok := ev.Message.Usage.full()
		if !ok {
			t.failed = true
			return
		}
		t.u, t.start = u, true
	case "message_delta":
		d := ev.Usage
		if d == nil || d.Output == nil || *d.Output < 0 {
			t.failed = true
			return
		}
		t.u.Output = *d.Output
		for _, f := range []struct {
			from *int64
			to   *int64
		}{{d.Input, &t.u.Input}, {d.CacheWrite, &t.u.CacheWrite}, {d.CacheRead, &t.u.CacheRead}} {
			if f.from != nil {
				if *f.from < 0 {
					t.failed = true
					return
				}
				*f.to = *f.from
			}
		}
		t.delta = true
	case "message_stop":
		t.stop = true
	case "error":
		t.failed = true
	}
}

func (t *anthropicTracker) usage() (llm.Usage, bool) {
	if !t.start || !t.delta || !t.stop || t.failed {
		return llm.Usage{}, false
	}
	return t.u, true
}

// openaiTracker: the usage arrives in a chunk of its own (include_usage),
// and "[DONE]" ends the stream.
type openaiTracker struct {
	done, got, failed bool
	u                 llm.Usage
}

func (t *openaiTracker) observe(data []byte) {
	if string(bytes.TrimSpace(data)) == "[DONE]" {
		t.done = true
		return
	}
	var ev struct {
		Usage *openaiUsage     `json:"usage"`
		Error *json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	if ev.Error != nil && string(*ev.Error) != "null" {
		t.failed = true
	}
	if ev.Usage != nil {
		u, ok := ev.Usage.usage()
		if !ok {
			t.failed = true
			return
		}
		t.u, t.got = u, true
	}
}

func (t *openaiTracker) usage() (llm.Usage, bool) {
	if !t.done || !t.got || t.failed {
		return llm.Usage{}, false
	}
	return t.u, true
}
