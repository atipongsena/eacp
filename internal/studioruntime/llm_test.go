package studioruntime_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/atipongsena/eacp/internal/governance"
	"github.com/atipongsena/eacp/internal/identity"
	"github.com/atipongsena/eacp/internal/llm"
	"github.com/atipongsena/eacp/internal/llmgateway"
	"github.com/atipongsena/eacp/internal/storage"
	"github.com/atipongsena/eacp/internal/studioruntime"
	"github.com/atipongsena/eacp/internal/worker"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const modelDefinition = `{"schema_version":2,"kind":"agent","inputs":{"employee_id":{"type":"string","max_length":64}},"limits":{"timeout_seconds":120,"max_output_tokens":100},"steps":[{"id":"classify","kind":"llm","model":"triage","instruction":"Return JSON only.","input":{"employee_id":"{{inputs.employee_id}}"},"max_output_tokens":100,"output_schema":{"type":"object","properties":{"eligible":{"type":"boolean"}},"required":["eligible"],"additionalProperties":false},"next":"answer"},{"id":"answer","kind":"respond","text":"Eligible: {{steps.classify.output.eligible}}"}]}`

func (r *rig) attachGateway(provider string, content string, beforeReply ...func()) (*httptest.Server, *atomic.Int64) {
	r.t.Helper()
	count := &atomic.Int64{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count.Add(1)
		if len(beforeReply) > 0 {
			beforeReply[0]()
		}
		w.Header().Set("Content-Type", "application/json")
		if provider == "openai" {
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5}})
		} else {
			_ = json.NewEncoder(w).Encode(map[string]any{"content": []any{map[string]any{"type": "text", "text": content}}, "stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 10, "output_tokens": 5}})
		}
	}))
	r.t.Cleanup(up.Close)
	r.f.LLMModel(r.t, "triage", provider, up.URL, "triage-upstream")
	r.f.ID(r.t, "alice", `INSERT INTO eacp.model_prices(tenant_id,provider,model,unit,input_per_mtok,output_per_mtok,reason) VALUES(eacp.current_tenant_id(),$1,'triage-upstream','USD',2,8,'test') RETURNING id`, provider)
	r.f.FundAgent(r.t, uuid.MustParse(r.agent), "USD", "1")
	u, _ := url.Parse(up.URL)
	path := filepath.Join(r.t.TempDir(), "gateway-secrets.json")
	if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"llm","host":%q,"value":"runtime-provider-canary-27c"}]}`, r.f.Tenant, u.Host)), 0o600); err != nil {
		r.t.Fatal(err)
	}
	secrets, err := worker.LoadSecrets(path, worker.RefuseSigningCredentials())
	if err != nil {
		r.t.Fatal(err)
	}
	ledger := llm.New(r.f.App)
	gateway, err := llmgateway.New(llmgateway.Options{ID: "gateway", Auth: func(ctx context.Context, key string) (identity.Caller, error) {
		return identity.Authenticate(ctx, r.f.App, key)
	}, Ledger: ledger, Policies: governance.NewStore(r.f.App), PDP: governance.LocalProvider{InstanceID: "gateway"}, Secrets: secrets, AgentRisk: ledger.AgentRisk})
	if err != nil {
		r.t.Fatal(err)
	}
	srv := httptest.NewServer(gateway)
	r.t.Cleanup(srv.Close)
	r.setGateway(srv.URL)
	return srv, count
}

func TestRuntimeWaitsForDurableLLMOutput(t *testing.T) {
	r := newRig(t)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	gateway, count := r.attachGateway("openai", `{"eligible":true}`, func() { close(started); <-release })
	r.useDefinition(modelDefinition)
	id := r.start("E-1")
	code, claimed := r.as("rt", "POST", "/v1/studio/runtime/claims", map[string]any{"runtime_id": "old", "master_version": "v1", "lease_seconds": 5, "limit": 1})
	r.want(200, code, claimed)
	c := claimed["runs"].([]any)[0].(map[string]any)
	code, n := r.as("rt", "POST", "/v1/studio/runtime/runs/"+id+"/llm/begin", map[string]any{"runtime_id": "old", "generation": 1, "index": 0})
	r.want(200, code, n)
	key, _ := r.master.Key(r.f.Tenant, uuid.MustParse(c["credential_id"].(string)))
	req, _ := http.NewRequest("POST", gateway.URL+"/v1/chat/completions", strings.NewReader(`{"model":"triage","max_tokens":100,"messages":[{"role":"user","content":"sample"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("EACP-Subject", "stella@tenant-a.test")
	req.Header.Set("EACP-Studio-Intent", n["intent_id"].(string))
	req.Header.Set("EACP-Studio-Runtime", "old")
	req.Header.Set("EACP-Studio-Generation", "1")
	done := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("provider did not start")
	}
	err := storage.InTenantTx(context.Background(), r.f.Owner, r.f.Tenant.String(), func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `UPDATE eacp.studio_runs SET leased_until=now()-interval '1 second' WHERE id=$1`, uuid.MustParse(id))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runDone := make(chan error, 1)
	go func() { _, err := r.rt.RunOnce(ctx); runDone <- err }()
	// Wait for the takeover generation. It cannot complete while the one
	// already-admitted provider request is blocked.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var generation int64
		err := storage.InTenantTx(ctx, r.f.Owner, r.f.Tenant.String(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT lease_generation FROM eacp.studio_runs WHERE id=$1`, uuid.MustParse(id)).Scan(&generation)
		})
		if err != nil {
			t.Fatal(err)
		}
		if generation == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("takeover did not claim")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-runDone:
		t.Fatal("run completed before settlement")
	default:
	}
	once.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
	if got := r.run(id); got["state"] != "SUCCEEDED" {
		t.Fatalf("settled recovery=%v", got)
	}
	if count.Load() != 1 {
		t.Fatal("pending intent was resent")
	}
}

func (r *rig) setGateway(origin string) {
	r.t.Helper()
	var err error
	r.rt, err = studioruntime.New(studioruntime.Options{API: r.srv.URL, Gateway: origin, Keys: map[uuid.UUID]string{r.f.Tenant: r.keys["rt"]}, Master: r.master, ID: "r1", Redact: r.redacted.add})
	if err != nil {
		r.t.Fatal(err)
	}
}

func TestRuntimeV2ModelStepsUseBothProviders(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			r := newRig(t)
			_, count := r.attachGateway(provider, `{"eligible":true}`)
			r.useDefinition(modelDefinition)
			id := r.start("E-1")
			if r.runOnce() != 1 {
				t.Fatal("claim count")
			}
			if got := r.run(id); got["state"] != "SUCCEEDED" || got["answer"] != "Eligible: true" {
				t.Fatalf("model run=%v", got)
			}
			if count.Load() != 1 {
				t.Fatal("provider call count")
			}
		})
	}
}

func TestRuntimeStopsOnInvalidModelOutput(t *testing.T) {
	r := newRig(t)
	_, count := r.attachGateway("openai", `{"eligible":"yes"}`)
	r.useDefinition(modelDefinition)
	id := r.start("E-1")
	r.runOnce()
	r.failed(id, "llm_invalid_output")
	if count.Load() != 1 {
		t.Fatal("provider call count")
	}
}

func TestRuntimeDoesNotRetryAnAmbiguousLLMRequest(t *testing.T) {
	r := newRig(t)
	gateway, count := r.attachGateway("openai", `{"eligible":true}`)
	r.useDefinition(modelDefinition)
	var requests atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		up, _ := http.NewRequestWithContext(req.Context(), "POST", gateway.URL+req.URL.Path, req.Body)
		up.Header = req.Header.Clone()
		response, err := http.DefaultClient.Do(up)
		if err != nil {
			t.Error("gateway proxy failed")
			return
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	}))
	t.Cleanup(proxy.Close)
	r.setGateway(proxy.URL)
	id := r.start("E-1")
	r.runOnce()
	if got := r.run(id); got["state"] != "SUCCEEDED" {
		t.Fatalf("recovery=%v", got)
	}
	if requests.Load() != 1 || count.Load() != 1 {
		t.Fatal("ambiguous request was retried")
	}
}

func TestRuntimeResumesWithTheSameLLMCall(t *testing.T) {
	r := newRig(t)
	gateway, count := r.attachGateway("openai", `{"eligible":true}`)
	r.useDefinition(modelDefinition)
	id := r.start("E-1")
	code, claimed := r.as("rt", "POST", "/v1/studio/runtime/claims", map[string]any{"runtime_id": "old", "master_version": "v1", "lease_seconds": 5, "limit": 1})
	r.want(200, code, claimed)
	c := claimed["runs"].([]any)[0].(map[string]any)
	code, n := r.as("rt", "POST", "/v1/studio/runtime/runs/"+id+"/llm/begin", map[string]any{"runtime_id": "old", "generation": 1, "index": 0})
	r.want(200, code, n)
	key, _ := r.master.Key(r.f.Tenant, uuid.MustParse(c["credential_id"].(string)))
	req, _ := http.NewRequest("POST", gateway.URL+"/v1/chat/completions", strings.NewReader(`{"model":"triage","max_tokens":100,"messages":[{"role":"user","content":"sample"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("EACP-Subject", "stella@tenant-a.test")
	req.Header.Set("EACP-Studio-Intent", n["intent_id"].(string))
	req.Header.Set("EACP-Studio-Runtime", "old")
	req.Header.Set("EACP-Studio-Generation", "1")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("initial call")
	}
	err = storage.InTenantTx(context.Background(), r.f.Owner, r.f.Tenant.String(), func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `UPDATE eacp.studio_runs SET leased_until=now()-interval '1 second' WHERE id=$1`, uuid.MustParse(id))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	r.runOnce()
	if got := r.run(id); got["state"] != "SUCCEEDED" {
		t.Fatalf("takeover=%v", got)
	}
	if count.Load() != 1 {
		t.Fatal("takeover forwarded another call")
	}
}
