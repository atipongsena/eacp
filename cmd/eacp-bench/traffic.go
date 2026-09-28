package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"

	"eacp/internal/bench"
)

// traffic builds one step's requests: request i comes from a uniformly
// random agent, and a replay repeats request ReplayOf(i) exactly (agent,
// key and body).
type traffic struct {
	api     *client
	gateway *client
	agents  []agent
	pick    []int // agent index per request
	key     string
	delayMS int
}

func newTraffic(api, gateway *client, agents []agent, seed uint64, requests int, key string, delayMS int) *traffic {
	r := rand.New(rand.NewPCG(seed, 0x6561637062656e63))
	pick := make([]int, requests)
	for i := range pick {
		pick[i] = r.IntN(len(agents))
	}
	return &traffic{api: api, gateway: gateway, agents: agents, pick: pick, key: key, delayMS: delayMS}
}

// source returns the request whose agent and key request i uses.
func (t *traffic) source(i int) int {
	if bench.IsReplay(i) && bench.ReplayOf(i) >= 0 {
		return bench.ReplayOf(i)
	}
	return i
}

func (t *traffic) agentFor(i int) agent { return t.agents[t.pick[t.source(i)%len(t.pick)]] }

// action submits a purchase of 1 THB; 200, 201 and 202 are accepted.
func (t *traffic) action(ctx context.Context, i int) bench.Response {
	payload := map[string]any{"amount": 1, "currency": "THB"}
	if t.delayMS > 0 {
		payload["delay_ms"] = t.delayMS
	}
	body, _ := json.Marshal(map[string]any{"subject": subject, "operation": "purchase", "target": "erp",
		"tool": "erp.create_po", "tool_schema_version": "1", "resource": "po", "payload": payload})
	a := t.agentFor(i)
	req, err := http.NewRequestWithContext(ctx, "POST", t.api.base+"/v1/actions", bytes.NewReader(body))
	if err != nil {
		return bench.Response{Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+t.api.key(a.name))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", fmt.Sprintf("%s-%d", t.key, t.source(i)))
	resp, err := t.api.http.Do(req)
	if err != nil {
		return bench.Response{Err: err}
	}
	defer resp.Body.Close()
	var out struct {
		ID       string `json:"id"`
		ActionID string `json:"action_id"`
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out)
	r := bench.Response{Status: resp.StatusCode, ActionID: out.ID}
	if r.ActionID == "" {
		r.ActionID = out.ActionID
	}
	if resp.StatusCode < 200 || resp.StatusCode > 202 {
		r.ActionID = ""
	}
	return r
}

// llm sends one non-streaming Messages call through the gateway with the
// agent's own EACP key; only 200 is accepted.
func (t *traffic) llm(ctx context.Context, i int) bench.Response {
	body := []byte(`{"model":"sonnet","max_tokens":16,"stream":false,"messages":[{"role":"user","content":"benchmark"}]}`)
	req, err := http.NewRequestWithContext(ctx, "POST", t.gateway.base+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return bench.Response{Err: err}
	}
	req.Header.Set("x-api-key", t.api.key(t.agentFor(i).name))
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.gateway.http.Do(req)
	if err != nil {
		return bench.Response{Err: err}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return bench.Response{Status: resp.StatusCode}
}
