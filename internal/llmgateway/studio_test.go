package llmgateway_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/atipongsena/eacp/internal/llm"
	"github.com/google/uuid"
)

func studioHeaders() map[string]string {
	h := openaiKey()
	h["EACP-Studio-Intent"] = uuid.NewString()
	h["EACP-Studio-Runtime"] = "r1"
	h["EACP-Studio-Generation"] = "1"
	h["EACP-Subject"] = "stella@example.test"
	return h
}

const closedOutputSchema = `{"type":"object","properties":{"eligible":{"type":"boolean"}},"required":["eligible"],"additionalProperties":false}`

func TestStudioGatewaySettlesOnlyExtractedTypedOutput(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			x := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
				for _, key := range []string{"EACP-Studio-Intent", "EACP-Studio-Runtime", "EACP-Studio-Generation", "EACP-Subject"} {
					if r.Header.Get(key) != "" {
						t.Error("Studio header reached provider")
					}
				}
				w.Header().Set("Content-Type", "application/json")
				var body any
				if provider == "openai" {
					body = map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": `{"eligible":true}`}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5}}
				} else {
					body = map[string]any{"content": []any{map[string]any{"type": "text", "text": `{"eligible":true}`}}, "stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 10, "output_tokens": 5}}
				}
				_ = json.NewEncoder(w).Encode(body)
			})
			x.ledger.set(func(l *fakeLedger) {
				l.studio = &llm.StudioBinding{RunID: uuid.New(), Index: 0, OutputSchema: json.RawMessage(closedOutputSchema)}
			})
			headers := studioHeaders()
			path := "/v1/chat/completions"
			body := `{"model":"triage","max_tokens":100,"messages":[{"role":"user","content":"private-instruction-canary"}]}`
			if provider == "anthropic" {
				path = "/v1/messages"
				headers["x-api-key"] = agentKey
				delete(headers, "Authorization")
			}
			code, _, response := x.post(path, body, headers)
			if code != 200 {
				t.Fatalf("status=%d", code)
			}
			st := x.ledger.settlement(t)
			if st.Outcome != llm.OutcomeSucceeded || !st.Usage.Known || string(st.StudioOutput) != `{"eligible":true}` {
				t.Fatal("typed settlement missing")
			}
			a := x.ledger.admitted()[0]
			if a.StudioIntentID == uuid.Nil || a.StudioRuntimeID != "r1" || a.StudioGeneration != 1 {
				t.Fatal("admission lost fence")
			}
			if strings.Contains(string(response), "eligible") || strings.Contains(x.logs.String(), "private-instruction-canary") {
				t.Fatal("private content escaped")
			}
		})
	}
}

func TestStudioGatewayRejectsUnsafeModelAnswersAndStillSettlesUsage(t *testing.T) {
	for _, content := range []string{`{"eligible":"yes"}`, `{"eligible":true,"extra":"x"}`, `{"eligible":true,"eligible":false}`, "```json\n{}\n```", `{"eligible":true,"key":"` + providerKey + `"}`, `{"eligible":true,"key":"` + agentKey + `"}`} {
		t.Run("answer", func(t *testing.T) {
			x := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5}})
			})
			x.ledger.set(func(l *fakeLedger) {
				l.studio = &llm.StudioBinding{RunID: uuid.New(), OutputSchema: json.RawMessage(closedOutputSchema)}
			})
			code, _, body := x.post("/v1/chat/completions", `{"model":"triage","max_tokens":100,"messages":[{"role":"user","content":"x"}]}`, studioHeaders())
			if code != 502 {
				t.Fatalf("status=%d", code)
			}
			st := x.ledger.settlement(t)
			if st.Outcome != llm.OutcomeSucceeded || !st.Usage.Known || len(st.StudioOutput) != 0 || st.StudioFailure == "" {
				t.Fatal("unsafe answer did not settle spend without output")
			}
			if strings.Contains(string(body), content) || strings.Contains(x.logs.String(), content) {
				t.Fatal("unsafe content escaped")
			}
		})
	}
}

func TestStudioGatewayRejectsStreamingAndToolsBeforeAdmission(t *testing.T) {
	x := newHarness(t, openaiOK)
	for _, extra := range []string{`,"stream":true`, `,"tools":[]`, `,"functions":[]`, `,"tool_choice":"none"`, `,"function_call":"none"`} {
		code, _, _ := x.post("/v1/chat/completions", `{"model":"triage","max_tokens":100,"messages":[{"role":"user","content":"x"}]`+extra+`}`, studioHeaders())
		if code != 400 {
			t.Fatalf("status=%d", code)
		}
	}
	if len(x.ledger.admitted()) != 0 || len(x.prov.requests()) != 0 {
		t.Fatal("unsafe request was admitted")
	}
}
