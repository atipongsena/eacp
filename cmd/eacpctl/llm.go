package main

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"
)

const llmModelUsage = `usage: eacpctl llm-model register --name N --provider anthropic|openai --base-url URL --upstream MODEL
    --secret-ref REF --max-output-tokens N [--timeout-ms N] | list`

const llmCallsUsage = `usage: eacpctl llm-calls list [--agent <uuid>] [--model NAME] [--state DENIED|ADMITTED|SETTLED]
    [--from RFC3339] [--to RFC3339] [--limit N] | show <id>`

func runLLMModel(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	if len(args) == 1 && args[0] == "list" {
		return call(ctx, getenv, out, "GET", "/v1/llm-models", nil)
	}
	if len(args) == 0 || args[0] != "register" {
		return errors.New(llmModelUsage)
	}
	fs := newFlags("llm-model register")
	name := fs.String("name", "", "the model's name in requests")
	provider := fs.String("provider", "", "anthropic or openai")
	baseURL := fs.String("base-url", "", "the provider's API base URL")
	upstream := fs.String("upstream", "", "the provider's model id")
	secretRef := fs.String("secret-ref", "", "the provider key's name in the gateway's secrets file")
	maxOut := fs.Int64("max-output-tokens", 0, "the output cap")
	timeout := fs.Int("timeout-ms", 0, "the call timeout (default 600000)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *name == "" || *provider == "" || *baseURL == "" || *upstream == "" || *secretRef == "" || *maxOut <= 0 ||
		len(fs.Args()) != 0 {
		return errors.New(llmModelUsage)
	}
	body := map[string]any{"name": *name, "provider": *provider, "base_url": *baseURL, "upstream_model": *upstream,
		"secret_ref": *secretRef, "max_output_tokens": *maxOut}
	if *timeout != 0 {
		body["timeout_ms"] = *timeout
	}
	return call(ctx, getenv, out, "POST", "/v1/llm-models", body)
}

func runLLMCalls(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	if len(args) == 2 && args[0] == "show" {
		if _, err := uuid.Parse(args[1]); err != nil {
			return errors.New(llmCallsUsage)
		}
		return call(ctx, getenv, out, "GET", "/v1/llm-calls/"+args[1], nil)
	}
	if len(args) == 0 || args[0] != "list" {
		return errors.New(llmCallsUsage)
	}
	fs := newFlags("llm-calls list")
	agent := fs.String("agent", "", "an agent uuid")
	model := fs.String("model", "", "a model name")
	state := fs.String("state", "", "DENIED, ADMITTED or SETTLED")
	from := fs.String("from", "", "RFC 3339 start")
	to := fs.String("to", "", "RFC 3339 end")
	limit := fs.Int("limit", 0, "at most 1000")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if len(fs.Args()) != 0 {
		return errors.New(llmCallsUsage)
	}
	if *agent != "" {
		if _, err := uuid.Parse(*agent); err != nil {
			return errors.New(llmCallsUsage)
		}
	}
	for _, t := range []string{*from, *to} {
		if t != "" {
			if _, err := time.Parse(time.RFC3339, t); err != nil {
				return errors.New(llmCallsUsage)
			}
		}
	}
	q := url.Values{}
	for k, v := range map[string]string{"agent": *agent, "model": *model, "state": *state, "from": *from, "to": *to} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if *limit != 0 {
		q.Set("limit", strconv.Itoa(*limit))
	}
	path := "/v1/llm-calls"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	return call(ctx, getenv, out, "GET", path, nil)
}
