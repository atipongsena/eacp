package llmgateway_test

// The official SDKs, pointed at the gateway with an EACP agent key as their
// API key, work unchanged (ADR-031 §6).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	aoption "github.com/anthropics/anthropic-sdk-go/option"
	"github.com/openai/openai-go/v3"
	ooption "github.com/openai/openai-go/v3/option"

	"eacp/internal/llm"
)

// clearSDKEnv keeps a developer's own provider settings out of the test.
func clearSDKEnv(t *testing.T) {
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "OPENAI_API_KEY",
		"OPENAI_BASE_URL", "OPENAI_ORG_ID", "OPENAI_PROJECT_ID", "OPENAI_ADMIN_KEY", "OPENAI_CUSTOM_HEADERS"} {
		t.Setenv(k, "")
	}
}

func upstreamJSON(t *testing.T, r recorded) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.body, &m); err != nil {
		t.Fatalf("upstream body %s: %v", r.body, err)
	}
	return m
}

func TestAnthropicSDKThroughTheGateway(t *testing.T) {
	clearSDKEnv(t)
	x := newHarness(t, anthropicOK)
	client := anthropic.NewClient(aoption.WithBaseURL(x.gw.URL), aoption.WithAPIKey(agentKey))
	ctx := context.Background()
	params := anthropic.MessageNewParams{Model: "sonnet", MaxTokens: 100,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))}}

	msg, err := client.Messages.New(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.Content) != 1 || msg.Content[0].Text != "hello" || msg.Usage.InputTokens != 10 || msg.Usage.OutputTokens != 5 {
		t.Fatalf("message %+v", msg)
	}
	wantSettled(t, x.ledger.settlement(t), llm.OutcomeSucceeded, 200,
		llm.Usage{Input: 10, CacheRead: 2, CacheWrite: 3, Output: 5, Known: true})
	up := x.prov.requests()[0]
	if b := upstreamJSON(t, up); b["model"] != "upstream-model-1" || b["max_tokens"] != float64(100) ||
		up.header.Get("x-api-key") != providerKey || up.header.Get("anthropic-version") == "" {
		t.Fatalf("upstream %v %s", up.header, up.body)
	}

	x.prov.set(func(w http.ResponseWriter, _ *http.Request) { sse(w, anthropicEvents, nil) })
	stream := client.Messages.NewStreaming(ctx, params)
	var acc anthropic.Message
	for stream.Next() {
		if err := acc.Accumulate(stream.Current()); err != nil {
			t.Fatal(err)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if len(acc.Content) != 1 || acc.Content[0].Text != "Hello" || acc.Usage.OutputTokens != 15 {
		t.Fatalf("accumulated %+v", acc)
	}
	wantSettled(t, x.ledger.settlement(t), llm.OutcomeSucceeded, 200,
		llm.Usage{Input: 10, CacheRead: 2, CacheWrite: 3, Output: 15, Known: true})
	if b := upstreamJSON(t, x.prov.requests()[1]); b["stream"] != true {
		t.Fatalf("upstream %v", b)
	}

	// A denial is the SDK's API error, and the SDK does not retry it.
	x.ledger.set(func(l *fakeLedger) { l.denial = "model_not_in_allowlist" })
	before := len(x.ledger.admitted())
	_, err = client.Messages.New(ctx, params)
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 403 || !strings.Contains(err.Error(), "model_not_in_allowlist") {
		t.Fatalf("denied: %v", err)
	}
	if n := len(x.ledger.admitted()) - before; n != 1 || len(x.prov.requests()) != 2 {
		t.Fatalf("%d admissions for one denied call; provider saw %d requests", n, len(x.prov.requests()))
	}
}

func TestOpenAISDKThroughTheGateway(t *testing.T) {
	clearSDKEnv(t)
	x := newHarness(t, openaiOK)
	client := openai.NewClient(ooption.WithBaseURL(x.gw.URL+"/v1"), ooption.WithAPIKey(agentKey))
	ctx := context.Background()
	params := openai.ChatCompletionNewParams{Model: "gpt",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hi")}}

	resp, err := client.Chat.Completions.New(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "hello" || resp.Usage.PromptTokens != 100 {
		t.Fatalf("completion %+v", resp)
	}
	wantSettled(t, x.ledger.settlement(t), llm.OutcomeSucceeded, 200, llm.Usage{Input: 60, CacheRead: 40, Output: 7, Known: true})
	up := x.prov.requests()[0]
	if b := upstreamJSON(t, up); b["model"] != "upstream-model-1" || b["max_completion_tokens"] != float64(4096) ||
		up.header.Get("Authorization") != "Bearer "+providerKey {
		t.Fatalf("upstream %v %s", up.header, up.body)
	}

	x.prov.set(openaiStream(true))
	stream := client.Chat.Completions.NewStreaming(ctx, params)
	var acc openai.ChatCompletionAccumulator
	for stream.Next() {
		acc.AddChunk(stream.Current())
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if len(acc.Choices) != 1 || acc.Choices[0].Message.Content != "Hello" || acc.Usage.CompletionTokens != 4 {
		t.Fatalf("accumulated %+v", acc.ChatCompletion)
	}
	wantSettled(t, x.ledger.settlement(t), llm.OutcomeSucceeded, 200, llm.Usage{Input: 20, CacheRead: 10, Output: 4, Known: true})
	so, _ := upstreamJSON(t, x.prov.requests()[1])["stream_options"].(map[string]any)
	if so["include_usage"] != true {
		t.Fatalf("stream options %v", so)
	}

	x.ledger.set(func(l *fakeLedger) { l.denial = "killed" })
	before := len(x.ledger.admitted())
	_, err = client.Chat.Completions.New(ctx, params)
	var apiErr *openai.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 403 || apiErr.Code != "killed" {
		t.Fatalf("denied: %v", err)
	}
	if n := len(x.ledger.admitted()) - before; n != 1 || len(x.prov.requests()) != 2 {
		t.Fatalf("%d admissions for one denied call; provider saw %d requests", n, len(x.prov.requests()))
	}
}
