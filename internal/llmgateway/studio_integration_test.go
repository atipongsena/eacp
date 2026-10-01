package llmgateway_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/atipongsena/eacp/internal/identity"
	"github.com/atipongsena/eacp/internal/registry"
	"github.com/atipongsena/eacp/internal/studio"
	"github.com/google/uuid"
)

func TestStudioGatewayOverPostgresForwardsOneCallAndKeepsOutputPrivate(t *testing.T) {
	var count atomic.Int64
	e := newLLMEnv(t, nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"{\"eligible\":true}"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`)
	}))
	f := e.f
	f.AddPrincipal(t, "stella", "human", "studio_author")
	f.AddPrincipal(t, "rt", "service", "studio_runtime")
	dept := f.ID(t, "alice", `INSERT INTO eacp.groups(tenant_id,name,display_name) VALUES(eacp.current_tenant_id(),'hr','HR') RETURNING id`)
	f.ID(t, "alice", `INSERT INTO eacp.group_memberships(tenant_id,group_id,principal_id) VALUES(eacp.current_tenant_id(),$1,$2) RETURNING id`, dept, f.P["stella"])
	definition := `{"schema_version":2,"kind":"agent","inputs":{},"limits":{"timeout_seconds":120,"max_output_tokens":100},"steps":[{"id":"classify","kind":"llm","model":"sonnet","instruction":"private-instruction-canary-27c","input":{},"max_output_tokens":100,"output_schema":` + closedOutputSchema + `,"next":"answer"},{"id":"answer","kind":"respond","text":"{{steps.classify.output.eligible}}"}]}`
	v := f.ID(t, "stella", `SELECT eacp.studio_save(NULL,'studio-model','Studio','Triage',$1,$2)`, dept, definition)
	must(t, f.Exec("rita", `SELECT eacp.studio_decide($1,true,'reviewed')`, v))
	svc := studio.New(f.App)
	author := registry.Actor{TenantID: f.Tenant, PrincipalID: f.P["stella"]}
	version, err := svc.Version(context.Background(), author, v, false)
	must(t, err)
	f.FundAgent(t, version.AgentID, "USD", "1")
	cred := uuid.New()
	key, hash, err := identity.NewKey(identity.KindAgent, f.Tenant, cred)
	must(t, err)
	must(t, f.Exec("rt", `SELECT eacp.studio_credential_propose($1,$2,$3,'v1')`, v, cred, hash))
	must(t, f.Exec("rita", `UPDATE eacp.credentials SET approved_at=now() WHERE id=$1`, cred))
	run, err := svc.Start(context.Background(), author, version.AgentID, json.RawMessage(`{}`))
	must(t, err)
	runtime := registry.Actor{TenantID: f.Tenant, PrincipalID: f.P["rt"]}
	claimed, err := svc.Claim(context.Background(), runtime, studio.ClaimRequest{RuntimeID: "r1", MasterVersion: "v1", LeaseSeconds: 30, Limit: 1})
	must(t, err)
	if len(claimed) != 1 {
		t.Fatal("run not claimed")
	}
	node, err := svc.BeginNode(context.Background(), runtime, run.ID, studio.NodeRequest{Lease: studio.Lease{RuntimeID: "r1", Generation: 1}, Index: 0}, true)
	must(t, err)
	var intent struct {
		ID uuid.UUID `json:"intent_id"`
	}
	must(t, json.Unmarshal(node, &intent))
	body := `{"model":"sonnet","max_tokens":100,"messages":[{"role":"user","content":"private-runtime-input-canary-27c"}]}`
	post := func() int {
		r, _ := http.NewRequest("POST", e.gw.URL+"/v1/messages", strings.NewReader(body))
		r.Header.Set("x-api-key", key)
		r.Header.Set("EACP-Studio-Intent", intent.ID.String())
		r.Header.Set("EACP-Studio-Runtime", "r1")
		r.Header.Set("EACP-Studio-Generation", "1")
		r.Header.Set("EACP-Subject", "stella@tenant-a.test")
		response, err := http.DefaultClient.Do(r)
		must(t, err)
		defer response.Body.Close()
		data, _ := io.ReadAll(response.Body)
		if strings.Contains(string(data), "eligible") {
			t.Fatal("provider content escaped")
		}
		return response.StatusCode
	}
	if post() != 200 {
		t.Fatal("Studio call was refused")
	}
	if post() != 503 {
		t.Fatal("consumed intent was admitted")
	}
	if count.Load() != 1 {
		t.Fatalf("provider calls=%d", count.Load())
	}
	output, err := svc.NodeOutput(context.Background(), runtime, run.ID, studio.NodeRequest{Lease: studio.Lease{RuntimeID: "r1", Generation: 1}, Index: 0})
	must(t, err)
	if !strings.Contains(string(output), `"eligible": true`) && !strings.Contains(string(output), `"eligible":true`) {
		t.Fatal("durable output unavailable")
	}
	if strings.Contains(e.logs.String(), key) || strings.Contains(e.logs.String(), fakeKey) || strings.Contains(e.logs.String(), "private-runtime-input-canary-27c") {
		t.Fatal("gateway log contains private content")
	}
}
