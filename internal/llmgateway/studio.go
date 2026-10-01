package llmgateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/atipongsena/eacp/internal/llm"
	"github.com/atipongsena/eacp/internal/worker"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/uuid"
)

// These identifiers carry the runtime fence; none is forwarded upstream.
func studioFence(r *http.Request, req request) (uuid.UUID, string, int64, error) {
	values := r.Header.Values("EACP-Studio-Intent")
	if len(values) == 0 {
		if r.Header.Get("EACP-Studio-Runtime") != "" || r.Header.Get("EACP-Studio-Generation") != "" {
			return uuid.Nil, "", 0, errors.New("missing intent")
		}
		return uuid.Nil, "", 0, nil
	}
	intent, err := uuid.Parse(r.Header.Get("EACP-Studio-Intent"))
	runtimeID := r.Header.Get("EACP-Studio-Runtime")
	gen, gerr := strconv.ParseInt(r.Header.Get("EACP-Studio-Generation"), 10, 64)
	if err != nil || intent == uuid.Nil || len(values) != 1 || !gatewayID.MatchString(runtimeID) || gerr != nil || gen < 1 || req.stream || req.maxOut < 1 {
		return uuid.Nil, "", 0, errors.New("invalid fence")
	}
	for _, key := range []string{"tools", "functions", "tool_choice", "function_call", "stream_options"} {
		if _, exists := req.fields[key]; exists {
			return uuid.Nil, "", 0, errors.New("unsupported Studio option")
		}
	}
	return intent, runtimeID, gen, nil
}

func extractStudioOutput(a api, data []byte, schema json.RawMessage) ([]byte, error) {
	if err := checkObject(data); err != nil {
		return nil, errors.New("invalid provider JSON")
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(data, &envelope) != nil {
		return nil, errors.New("invalid provider JSON")
	}
	var text string
	if a == openai {
		var choices []struct {
			Message struct {
				Content  *string         `json:"content"`
				Refusal  json.RawMessage `json:"refusal"`
				Tools    json.RawMessage `json:"tool_calls"`
				Function json.RawMessage `json:"function_call"`
			} `json:"message"`
			Finish string `json:"finish_reason"`
		}
		if json.Unmarshal(envelope["choices"], &choices) != nil || len(choices) != 1 || choices[0].Message.Content == nil || choices[0].Finish != "stop" {
			return nil, errors.New("invalid provider answer")
		}
		m := choices[0].Message
		for _, raw := range []json.RawMessage{m.Refusal, m.Tools, m.Function} {
			if len(raw) > 0 && !isNull(raw) {
				return nil, errors.New("unsupported provider answer")
			}
		}
		text = *m.Content
	} else {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		var stop string
		if json.Unmarshal(envelope["content"], &blocks) != nil || len(blocks) != 1 || blocks[0].Type != "text" || json.Unmarshal(envelope["stop_reason"], &stop) != nil || stop != "end_turn" {
			return nil, errors.New("invalid provider answer")
		}
		text = blocks[0].Text
	}
	output := []byte(text)
	if len(output) > 65536 || checkObject(output) != nil {
		return nil, errors.New("invalid typed JSON")
	}
	var contract jsonschema.Schema
	if json.Unmarshal(schema, &contract) != nil {
		return nil, errors.New("invalid output contract")
	}
	resolved, err := contract.Resolve(&jsonschema.ResolveOptions{Loader: func(*url.URL) (*jsonschema.Schema, error) { return nil, errors.New("schema references refused") }})
	if err != nil {
		return nil, errors.New("invalid output contract")
	}
	dec := json.NewDecoder(bytes.NewReader(output))
	dec.UseNumber()
	var value any
	if dec.Decode(&value) != nil || resolved.Validate(value) != nil {
		return nil, errors.New("typed output mismatch")
	}
	// Compact without decoding numbers through float64; PostgreSQL computes the digest.
	var compact bytes.Buffer
	if json.Compact(&compact, output) != nil {
		return nil, errors.New("invalid typed JSON")
	}
	return compact.Bytes(), nil
}

func (g *Gateway) studioResponse(w http.ResponseWriter, r *http.Request, c *call, data []byte, status int, usage llm.Usage, known bool, secret worker.Secret, killed bool) {
	outcome := llm.OutcomeSucceeded
	if !known {
		outcome = llm.OutcomeUsageUnknown
	}
	if killed {
		outcome = llm.OutcomeKilled
	}
	if known && !killed {
		// Check the complete envelope first so a credential hidden in an ignored field cannot escape.
		key, _ := agentKey(r, c.api)
		if containsStudioCredential(data, secret, key) {
			c.outputFailure = "llm_output_contains_credential"
		} else {
			output, err := extractStudioOutput(c.api, data, c.studio.OutputSchema)
			if err != nil {
				c.outputFailure = "llm_invalid_output"
			} else {
				c.output = output
			}
		}
	}
	if !g.settle(r.Context(), c, outcome, status, usage) {
		g.unavailable(w, c.api, "ledger_unavailable")
		return
	}
	if c.outputFailure != "" {
		g.fail(w, c.api, http.StatusBadGateway, c.outputFailure, "")
		return
	}
	if outcome != llm.OutcomeSucceeded {
		g.fail(w, c.api, http.StatusBadGateway, "llm_unknown", "")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"call_id": c.id.String(), "state": "settled"})
}

func containsStudioCredential(data []byte, secret worker.Secret, key string) bool {
	contains := func(value string) bool {
		return secret.Contains(value) || (key != "" && bytes.Contains([]byte(value), []byte(key)))
	}
	if contains(string(data)) {
		return true
	}
	if checkObject(data) != nil {
		return false
	}
	var value any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if dec.Decode(&value) != nil {
		return false
	}
	var visit func(any, int) bool
	visit = func(v any, depth int) bool {
		if depth > 128 {
			return true
		}
		switch v := v.(type) {
		case string:
			if contains(v) {
				return true
			}
			// A provider text block contains another JSON document. Inspect
			// decoded strings there too, including escaped credential bytes.
			if checkObject([]byte(v)) == nil {
				var nested any
				d := json.NewDecoder(bytes.NewReader([]byte(v)))
				d.UseNumber()
				if d.Decode(&nested) == nil {
					return visit(nested, depth+1)
				}
			}
		case []any:
			for _, item := range v {
				if visit(item, depth+1) {
					return true
				}
			}
		case map[string]any:
			for k, item := range v {
				if contains(k) || visit(item, depth+1) {
					return true
				}
			}
		}
		return false
	}
	return visit(value, 0)
}
