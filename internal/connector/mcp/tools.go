package mcp

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	"github.com/atipongsena/eacp/internal/governance"
	"github.com/atipongsena/eacp/internal/worker"
)

// Tool names EACP accepts: the characters the specification recommends,
// 1–128 of them (MCP 2026-07-28, "Tool Names").
var toolName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

const (
	maxDefinition = 64 << 10 // as eacp.tool_definitions.definition
	maxDisplay    = 16 << 10 // as eacp.tool_definitions.display
)

// Rejection reasons recorded with a scan (ADR-023 §2).
const (
	rejectInvalidJSON   = "invalid_json"
	rejectNotObject     = "not_an_object"
	rejectInvalidName   = "invalid_name"
	rejectDuplicateName = "duplicate_name"
	rejectInputSchema   = "invalid_input_schema"
	rejectMCPHeader     = "invalid_x_mcp_header"
	rejectUnsupported   = "unsupported_json"
	rejectTooLarge      = "too_large"
)

type candidate struct {
	name   string
	tool   worker.DiscoveredTool
	reason string
}

// canonicalTools turns listed tool objects into their canonical definition
// and display texts, and rejects the ones EACP cannot fingerprint or that
// the transport requires a client to drop.
func canonicalTools(raws []json.RawMessage) ([]worker.DiscoveredTool, []worker.RejectedTool) {
	cands := make([]candidate, 0, len(raws))
	count := map[string]int{}
	for _, raw := range raws {
		c := canonicalTool(raw)
		cands = append(cands, c)
		count[c.name]++
	}
	tools := []worker.DiscoveredTool{}
	rejected := []worker.RejectedTool{}
	for _, c := range cands {
		switch {
		case c.reason != "":
			rejected = append(rejected, worker.RejectedTool{RemoteName: clip(c.name, 128), Reason: c.reason})
		case count[c.name] > 1:
			// Which one is real is unknowable: neither is accepted.
			rejected = append(rejected, worker.RejectedTool{RemoteName: c.name, Reason: rejectDuplicateName})
		default:
			tools = append(tools, c.tool)
		}
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].RemoteName < tools[j].RemoteName })
	return tools, rejected
}

func canonicalTool(raw json.RawMessage) candidate {
	var named struct {
		Name any `json:"name"`
	}
	_ = json.Unmarshal(raw, &named)
	name, _ := named.Name.(string)

	canon, err := governance.Canonicalize(raw)
	if err != nil {
		return candidate{name: name, reason: rejectInvalidJSON}
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(canon, &obj) != nil || obj == nil {
		return candidate{name: name, reason: rejectNotObject}
	}
	if !toolName.MatchString(name) {
		return candidate{name: name, reason: rejectInvalidName}
	}
	// PostgreSQL's jsonb cannot hold U+0000.
	if bytes.Contains(canon, []byte(`\u0000`)) {
		return candidate{name: name, reason: rejectUnsupported}
	}
	var schema map[string]any
	if err := decode(obj["inputSchema"], &schema); err != nil || schema == nil || schema["type"] != "object" {
		return candidate{name: name, reason: rejectInputSchema}
	}
	if !validMCPHeaders(schema) {
		return candidate{name: name, reason: rejectMCPHeader}
	}

	// Display-only fields leave the fingerprinted definition (ADR-023 §3).
	display := map[string]json.RawMessage{}
	for _, k := range []string{"title", "icons"} {
		if v, ok := obj[k]; ok {
			display[k] = v
			delete(obj, k)
		}
	}
	if raw, ok := obj["annotations"]; ok {
		var a map[string]json.RawMessage
		if json.Unmarshal(raw, &a) == nil && a != nil {
			if v, ok := a["title"]; ok {
				display["annotations.title"] = v
				delete(a, "title")
				obj["annotations"], _ = json.Marshal(a)
			}
		}
	}
	def, err1 := recanonical(obj)
	disp, err2 := recanonical(display)
	if err1 != nil || err2 != nil {
		return candidate{name: name, reason: rejectInvalidJSON}
	}
	if len(def) > maxDefinition || len(disp) > maxDisplay {
		return candidate{name: name, reason: rejectTooLarge}
	}
	return candidate{name: name, tool: worker.DiscoveredTool{RemoteName: name, Definition: def, Display: disp}}
}

// recanonical re-encodes v in RFC 8785 form (encoding/json escapes HTML
// characters and orders keys by bytes; JCS decides both).
func recanonical(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	c, err := governance.Canonicalize(b)
	return string(c), err
}

func decode(raw json.RawMessage, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	return d.Decode(v)
}

// validMCPHeaders applies the x-mcp-header rules of the Streamable HTTP
// transport (MCP 2026-07-28): each annotation is a non-empty HTTP token,
// unique case-insensitively, on a property of type integer, string or
// boolean reached from the root through "properties" keys only. An
// annotation anywhere else invalidates the tool.
func validMCPHeaders(schema map[string]any) bool {
	seen := map[string]bool{}
	var walk func(node any, path []string) bool
	walk = func(node any, path []string) bool {
		switch n := node.(type) {
		case []any:
			for _, v := range n {
				if !walk(v, append(path, "[]")) {
					return false
				}
			}
		case map[string]any:
			// In a "properties" map the keys are property names, not keywords.
			inProperties := len(path) > 0 && path[len(path)-1] == "properties"
			for k, v := range n {
				if k == "x-mcp-header" && !inProperties {
					h, ok := v.(string)
					if !ok || !staticallyReachable(path) || !httpToken(h) || seen[strings.ToLower(h)] {
						return false
					}
					if t, _ := n["type"].(string); t != "integer" && t != "string" && t != "boolean" {
						return false
					}
					seen[strings.ToLower(h)] = true
					continue
				}
				if !walk(v, append(path, k)) {
					return false
				}
			}
		}
		return true
	}
	return walk(schema, nil)
}

// staticallyReachable: the path is properties/<name>(/properties/<name>)*.
func staticallyReachable(path []string) bool {
	if len(path) < 2 || len(path)%2 != 0 {
		return false
	}
	for i := 0; i < len(path); i += 2 {
		if path[i] != "properties" {
			return false
		}
	}
	return true
}

// httpToken reports whether s is 1*tchar (RFC 9110 §5.6.2).
func httpToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9') ||
			strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0 {
			continue
		}
		return false
	}
	return true
}
