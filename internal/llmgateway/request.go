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

// invalid is a 400 invalid_request with a reason safe to show the caller.
type invalid struct{ why string }

func (e invalid) Error() string { return "invalid_request: " + e.why }

// parse reads body as one JSON object without duplicate keys (at any depth)
// and checks the fields the gateway relies on (ADR-031 §3.5 step 2).
func parse(a api, body []byte) (request, error) {
	if !utf8.Valid(body) {
		return request{}, invalid{"the body is not valid UTF-8"}
	}
	if err := checkObject(body); err != nil {
		return request{}, err
	}
	var r request
	if err := json.Unmarshal(body, &r.fields); err != nil {
		return request{}, invalid{"the body is not a JSON object"}
	}
	raw, ok := r.fields["model"]
	if !ok || json.Unmarshal(raw, &r.model) != nil || raw[0] != '"' {
		return request{}, invalid{"model must be a string"}
	}
	if n := utf8.RuneCountInString(r.model); n < 1 || n > 256 || hasControl(r.model) {
		return request{}, invalid{"model must be 1-256 characters without control characters"}
	}
	if raw, ok := r.fields["stream"]; ok && !isNull(raw) {
		if err := json.Unmarshal(raw, &r.stream); err != nil {
			return request{}, invalid{"stream must be a boolean"}
		}
	}
	switch a {
	case anthropic:
		n, err := positive(r.fields, "max_tokens")
		if err != nil {
			return request{}, err
		}
		if n == 0 {
			return request{}, invalid{"max_tokens is required"}
		}
		r.maxOut = n
	case openai:
		if raw, ok := r.fields["n"]; ok && !isNull(raw) && string(raw) != "1" {
			return request{}, invalid{"n must be 1"}
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
			return request{}, invalid{"send at most one of max_completion_tokens and max_tokens"}
		}
		r.maxOut = max(a, b)
		if raw, ok := r.fields["stream_options"]; ok && !isNull(raw) {
			var so map[string]json.RawMessage
			if raw[0] != '{' || json.Unmarshal(raw, &so) != nil {
				return request{}, invalid{"stream_options must be an object"}
			}
			if u, ok := so["include_usage"]; ok && string(u) != "true" && string(u) != "false" {
				return request{}, invalid{"stream_options.include_usage must be a boolean"}
			}
		}
	}
	return r, nil
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
		return 0, invalid{name + " must be an integer of at least 1"}
	}
	n, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return 0, invalid{name + " is too large"}
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
		return invalid{"the body is not valid JSON"}
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return invalid{"the body must be a JSON object"}
	}
	stack := []*jsonLevel{{keys: map[string]struct{}{}, expectKey: true}}
	for len(stack) > 0 {
		tok, err := dec.Token()
		if err != nil {
			return invalid{"the body is not valid JSON"}
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
				return invalid{"the body is not valid JSON"}
			}
			if _, dup := top.keys[k]; dup {
				return invalid{fmt.Sprintf("duplicate key %q", k)}
			}
			top.keys[k] = struct{}{}
			top.expectKey = false
			continue
		}
		switch d, _ := tok.(json.Delim); d {
		case '{', '[':
			if len(stack) >= maxDepth {
				return invalid{"the body is nested too deeply"}
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
		return invalid{"the body must be exactly one JSON object"}
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
