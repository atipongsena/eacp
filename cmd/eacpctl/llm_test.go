package main

import (
	"strings"
	"testing"
)

func TestLLMModelCommands(t *testing.T) {
	env, got := recordingAPI(t)
	const id = "0b9f0e6a-4c8e-4a55-9d1e-4f2f6f1f9a01"
	for _, tc := range []struct {
		args   []string
		method string
		uri    string
		body   map[string]any
	}{
		{[]string{"llm-model", "register", "--name", "sonnet", "--provider", "anthropic", "--base-url",
			"https://api.anthropic.com", "--upstream", "claude-sonnet-4-5", "--secret-ref", "anthropic",
			"--max-output-tokens", "8192", "--timeout-ms", "120000"}, "POST", "/v1/llm-models",
			map[string]any{"name": "sonnet", "provider": "anthropic", "base_url": "https://api.anthropic.com",
				"upstream_model": "claude-sonnet-4-5", "secret_ref": "anthropic", "max_output_tokens": float64(8192),
				"timeout_ms": float64(120000)}},
		{[]string{"llm-model", "list"}, "GET", "/v1/llm-models", nil},
		{[]string{"llm-calls", "list"}, "GET", "/v1/llm-calls", nil},
		{[]string{"llm-calls", "list", "--agent", id, "--model", "sonnet", "--state", "SETTLED", "--from",
			"2026-09-01T00:00:00Z", "--limit", "20"}, "GET",
			"/v1/llm-calls?agent=" + id + "&from=2026-09-01T00%3A00%3A00Z&limit=20&model=sonnet&state=SETTLED", nil},
		{[]string{"llm-calls", "show", id}, "GET", "/v1/llm-calls/" + id, nil},
		{[]string{"kill", "activate", "model", id, "--reason", "provider incident"}, "POST", "/v1/killswitch",
			map[string]any{"scope": "model", "target_id": id, "killed": true}},
	} {
		*got = nil
		out, err := runWith(t, env, tc.args...)
		if err != nil || !strings.Contains(out, `"ok": true`) {
			t.Fatalf("%v: %v %s", tc.args, err, out)
		}
		if len(*got) != 1 || (*got)[0].method != tc.method || (*got)[0].uri != tc.uri {
			t.Fatalf("%v sent %+v", tc.args, *got)
		}
		for k, v := range tc.body {
			if (*got)[0].body[k] != v {
				t.Fatalf("%v body = %v", tc.args, (*got)[0].body)
			}
		}
	}
	for _, bad := range [][]string{{"llm-model"}, {"llm-model", "register", "--name", "x"},
		{"llm-calls"}, {"llm-calls", "show", "x"}, {"llm-calls", "list", "--limit", "x"}, {"llm-calls", "list", "extra"}} {
		if _, err := runWith(t, env, bad...); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
}
