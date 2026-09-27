package llmgateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"eacp/internal/llm"
)

// maxDepth bounds the nesting of a request body.
const maxDepth = 128

// request is what the gateway reads from a request body; it keeps every
// top-level value's bytes so that forwarding changes only what it must.
type request struct {
	fields map[string]json.RawMessage
	model  string
	stream bool
	maxOut int64 // the requested output cap, 0 when none
}

// invalid is a 400 with a reason safe to show the caller: invalid_request,
// or the code it names.
type invalid struct{ why, code string }

func (e invalid) Error() string { return e.errorCode() + ": " + e.why }

func (e invalid) errorCode() string {
	if e.code == "" {
		return "invalid_request"
	}
	return e.code
}

// unbounded refuses an input whose usage the request's bytes do not bound.
func unbounded(why string) error { return invalid{why: why, code: "unbounded_input"} }

// parse reads body as one JSON object without duplicate keys (at any depth)
// and checks the fields the gateway relies on (ADR-031 §3.5 step 2).
func parse(a api, body []byte) (request, error) {
	if !utf8.Valid(body) {
		return request{}, invalid{why: "the body is not valid UTF-8"}
	}
	if err := checkObject(body); err != nil {
		return request{}, err
	}
	var r request
	if err := json.Unmarshal(body, &r.fields); err != nil {
		return request{}, invalid{why: "the body is not a JSON object"}
	}
	raw, ok := r.fields["model"]
	if !ok || json.Unmarshal(raw, &r.model) != nil || raw[0] != '"' {
		return request{}, invalid{why: "model must be a string"}
	}
	if n := utf8.RuneCountInString(r.model); n < 1 || n > 256 || hasControl(r.model) {
		return request{}, invalid{why: "model must be 1-256 characters without control characters"}
	}
	if raw, ok := r.fields["stream"]; ok && !isNull(raw) {
		if err := json.Unmarshal(raw, &r.stream); err != nil {
			return request{}, invalid{why: "stream must be a boolean"}
		}
	}
	switch a {
	case anthropic:
		n, err := positive(r.fields, "max_tokens")
		if err != nil {
			return request{}, err
		}
		if n == 0 {
			return request{}, invalid{why: "max_tokens is required"}
		}
		r.maxOut = n
	case openai:
		if raw, ok := r.fields["n"]; ok && !isNull(raw) && string(raw) != "1" {
			return request{}, invalid{why: "n must be 1"}
		}
		a, err := positive(r.fields, "max_completion_tokens")
		if err != nil {
			return request{}, err
		}
		b, err := positive(r.fields, "max_tokens")
		if err != nil {
			return request{}, err
		}
		if a != 0 && b != 0 {
			return request{}, invalid{why: "send at most one of max_completion_tokens and max_tokens"}
		}
		r.maxOut = max(a, b)
		if raw, ok := r.fields["stream_options"]; ok && !isNull(raw) {
			var so map[string]json.RawMessage
			if raw[0] != '{' || json.Unmarshal(raw, &so) != nil {
				return request{}, invalid{why: "stream_options must be an object"}
			}
			if u, ok := so["include_usage"]; ok && string(u) != "true" && string(u) != "false" {
				return request{}, invalid{why: "stream_options.include_usage must be a boolean"}
			}
		}
	}
	if err := checkBounded(a, r.fields); err != nil {
		return request{}, err
	}
	return r, nil
}

// checkBounded refuses, by the request's structure and never its content,
// what the reservation cannot cover (ADR-031 §4): inputs the provider
// fetches, stores or runs (URLs, file ids, server tools, MCP servers,
// containers, web search) and output priced above the rate card (audio,
// predictions, the priority tier). It reads message and system content
// blocks and the tools' types only: tool schemas and arguments are never
// looked into.
func checkBounded(a api, fields map[string]json.RawMessage) error {
	present := func(k string) bool { raw, ok := fields[k]; return ok && !isNull(raw) }
	var refused []string
	switch a {
	case anthropic:
		refused = []string{"mcp_servers", "container"}
	case openai:
		refused = []string{"web_search_options", "audio", "prediction"}
	}
	for _, k := range refused {
		if present(k) {
			return unbounded(k + " is not supported through the gateway")
		}
	}
	if present("tools") {
		var tools []map[string]json.RawMessage
		if json.Unmarshal(fields["tools"], &tools) != nil {
			return invalid{why: "tools must be an array of objects"}
		}
		for _, tl := range tools {
			typ, set := str(tl["type"])
			if a == anthropic && set && typ != "custom" || a == openai && typ != "function" && typ != "custom" {
				return unbounded("only custom or function tools are supported through the gateway")
			}
		}
	}
	if a == openai {
		if present("modalities") {
			var mods []string
			if json.Unmarshal(fields["modalities"], &mods) != nil {
				return invalid{why: "modalities must be an array of strings"}
			}
			for _, m := range mods {
				if m != "text" {
					return unbounded("only text output is supported through the gateway")
				}
			}
		}
		if present("service_tier") {
			if tier, _ := str(fields["service_tier"]); tier != "auto" && tier != "default" && tier != "flex" {
				return unbounded("service_tier must be auto, default or flex")
			}
		}
	}
	if a == anthropic && present("system") && fields["system"][0] == '[' {
		if err := anthropicBlocks(fields["system"]); err != nil {
			return err
		}
	}
	if !present("messages") {
		return nil
	}
	var msgs []map[string]json.RawMessage
	if json.Unmarshal(fields["messages"], &msgs) != nil {
		return invalid{why: "messages must be an array of objects"}
	}
	for _, m := range msgs {
		if a == openai {
			if raw, ok := m["audio"]; ok && !isNull(raw) {
				return unbounded("audio from an earlier answer is not supported through the gateway")
			}
		}
		content, ok := m["content"]
		if !ok || len(content) == 0 || content[0] != '[' {
			continue
		}
		var err error
		if a == anthropic {
			err = anthropicBlocks(content)
		} else {
			err = openaiParts(content)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// anthropicBlocks checks content blocks: every source is inline (base64,
// text, or content blocks checked in turn) and nothing names a file id.
// Nesting is bounded by maxDepth (checkObject).
func anthropicBlocks(raw json.RawMessage) error {
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return invalid{why: "content must be a string or an array of objects"}
	}
	for _, b := range blocks {
		if _, ok := b["file_id"]; ok {
			return unbounded("files by id are not supported through the gateway")
		}
		if src, ok := b["source"]; ok && !isNull(src) {
			var source map[string]json.RawMessage
			if src[0] != '{' || json.Unmarshal(src, &source) != nil {
				return unbounded("a content source must be inline")
			}
			switch typ, _ := str(source["type"]); typ {
			case "base64", "text":
			case "content":
				if c, ok := source["content"]; ok && len(c) > 0 && c[0] == '[' {
					if err := anthropicBlocks(c); err != nil {
						return err
					}
				}
			default:
				return unbounded("only inline (base64, text or content) sources are supported through the gateway")
			}
		}
		if typ, _ := str(b["type"]); typ == "tool_result" {
			if c, ok := b["content"]; ok && len(c) > 0 && c[0] == '[' {
				if err := anthropicBlocks(c); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// openaiParts checks content parts: images are data URLs and files are
// inline data, never an id.
func openaiParts(raw json.RawMessage) error {
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return invalid{why: "content must be a string or an array of objects"}
	}
	for _, part := range parts {
		if img, ok := part["image_url"]; ok && !isNull(img) {
			var v struct {
				URL string `json:"url"`
			}
			if json.Unmarshal(img, &v) != nil || !strings.HasPrefix(strings.ToLower(v.URL), "data:") {
				return unbounded("images must be data URLs")
			}
		}
		if f, ok := part["file"]; ok && !isNull(f) {
			var file map[string]json.RawMessage
			if json.Unmarshal(f, &file) != nil {
				return invalid{why: "file must be an object"}
			}
			if _, ok := file["file_id"]; ok {
				return unbounded("files by id are not supported through the gateway")
			}
		}
	}
	return nil
}

// str decodes raw as a JSON string; set is false for an absent or null value.
func str(raw json.RawMessage) (s string, set bool) {
	if len(raw) == 0 || isNull(raw) {
		return "", false
	}
	if json.Unmarshal(raw, &s) != nil {
		return string(raw), true // not a string: never equal to an allowed name
	}
	return s, true
}

var positiveInteger = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)

// positive reads an optional integer field that must be at least 1; 0
// means absent (or null).
func positive(fields map[string]json.RawMessage, name string) (int64, error) {
	raw, ok := fields[name]
	if !ok || isNull(raw) {
		return 0, nil
	}
	if !positiveInteger.Match(raw) {
		return 0, invalid{why: name + " must be an integer of at least 1"}
	}
	n, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return 0, invalid{why: name + " is too large"}
	}
	return n, nil
}

func isNull(raw json.RawMessage) bool { return string(raw) == "null" }

func hasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// jsonLevel is an open object (keys set) or array (keys nil).
type jsonLevel struct {
	keys      map[string]struct{}
	expectKey bool
}

// checkObject walks body's tokens: it must be exactly one object, nested at
// most maxDepth deep, with no key twice in any object.
func checkObject(body []byte) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return invalid{why: "the body is not valid JSON"}
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return invalid{why: "the body must be a JSON object"}
	}
	stack := []*jsonLevel{{keys: map[string]struct{}{}, expectKey: true}}
	for len(stack) > 0 {
		tok, err := dec.Token()
		if err != nil {
			return invalid{why: "the body is not valid JSON"}
		}
		top := stack[len(stack)-1]
		if top.keys != nil && top.expectKey {
			if d, ok := tok.(json.Delim); ok && d == '}' {
				stack = stack[:len(stack)-1]
				valueDone(stack)
				continue
			}
			k, ok := tok.(string)
			if !ok {
				return invalid{why: "the body is not valid JSON"}
			}
			if _, dup := top.keys[k]; dup {
				return invalid{why: fmt.Sprintf("duplicate key %q", k)}
			}
			top.keys[k] = struct{}{}
			top.expectKey = false
			continue
		}
		switch d, _ := tok.(json.Delim); d {
		case '{', '[':
			if len(stack) >= maxDepth {
				return invalid{why: "the body is nested too deeply"}
			}
			l := &jsonLevel{}
			if d == '{' {
				l.keys, l.expectKey = map[string]struct{}{}, true
			}
			stack = append(stack, l)
		case ']':
			stack = stack[:len(stack)-1]
			valueDone(stack)
		default:
			valueDone(stack)
		}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return invalid{why: "the body must be exactly one JSON object"}
	}
	return nil
}

// valueDone records that a value of the innermost level ended: an object
// expects its next key.
func valueDone(stack []*jsonLevel) {
	if n := len(stack); n > 0 && stack[n-1].keys != nil {
		stack[n-1].expectKey = true
	}
}

// rewrite is the upstream body: the agent's object with model replaced by
// the model's upstream name; an OpenAI request with no output cap gets the
// admitted one, and an OpenAI stream asks for its usage. Values keep their
// bytes; keys are sorted.
func rewrite(a api, r request, m llm.Model) []byte {
	fields := make(map[string]json.RawMessage, len(r.fields)+2)
	for k, v := range r.fields {
		fields[k] = v
	}
	fields["model"] = jsonString(m.UpstreamModel)
	if a == openai {
		if r.maxOut == 0 {
			fields["max_completion_tokens"] = json.RawMessage(strconv.FormatInt(m.MaxOutputTokens, 10))
		}
		if r.stream {
			so := map[string]json.RawMessage{}
			if raw, ok := fields["stream_options"]; ok && !isNull(raw) {
				_ = json.Unmarshal(raw, &so)
			}
			so["include_usage"] = json.RawMessage("true")
			fields["stream_options"] = encodeObject(so)
		}
	}
	return encodeObject(fields)
}

// encodeObject writes fields as a JSON object with sorted keys and the
// values' own bytes.
func encodeObject(fields map[string]json.RawMessage) []byte {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(jsonString(k))
		b.WriteByte(':')
		b.Write(fields[k])
	}
	b.WriteByte('}')
	return b.Bytes()
}

// jsonString encodes s as a JSON string without HTML escaping.
func jsonString(s string) json.RawMessage {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return bytes.TrimRight(b.Bytes(), "\n")
}
