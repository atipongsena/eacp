package microsoftagt_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/integrations/governance/microsoftagt"
	"github.com/atipongsena/eacp/internal/action"
	"github.com/atipongsena/eacp/internal/registry/registrytest"
)

const allowAll = `{"format_version":1,"rules":[{"id":"all","verdict":"allow","reason":"permitted"}]}`

// The action engine uses the AGT client like any provider: the decision is
// recorded with its provider evidence, a PDP outage leaves the action
// RECEIVED and cancellable (§103 invariant 18), and recovery evaluates it.
func TestEngineWithTheAGTProvider(t *testing.T) {
	f := registrytest.New(t)
	tool := f.ActiveTool(t, "erp", "purchase")
	agent := f.ActiveAgent(t, "buyer", tool.Tool)
	f.ActivatePolicy(t, allowAll)
	s := &sidecar{t: t}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	p, err := microsoftagt.New(microsoftagt.Config{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	e := action.New(f.App, action.Options{Provider: p, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Limits: action.Limits{MaxQueuedPerTenant: 100, MaxQueuedGlobal: 100}})
	ctx := context.Background()
	actor := action.Agent(f.Tenant, agent.Agent, agent.Version)
	submission := func(key string) action.Submission {
		return action.Submission{IdempotencyKey: key, Subject: "carol@tenant-a.test", Operation: "purchase",
			Target: "erp", Tool: "erp.purchase", ToolSchemaVersion: "1", Resource: "po",
			Payload: json.RawMessage(`{"amount":10}`)}
	}

	got, err := e.Submit(ctx, actor, submission("allowed"))
	if err != nil || got.State != "QUEUED" {
		t.Fatalf("allowed: %+v, %v", got, err)
	}
	ev, err := e.Evidence(ctx, f.Tenant, got.ID)
	// Evaluation and the release-boundary revalidation both ask the PDP.
	if err != nil || len(ev.Decisions) != 2 || !ev.Chain.Verified {
		t.Fatalf("evidence = %+v, %v", ev, err)
	}
	for _, d := range ev.Decisions {
		if d.Provider != microsoftagt.ProviderName || d.ProviderInstanceID != "agt-pdp-test" ||
			string(d.ProviderEvidence) != `{"rule_id": "r", "acs_action_identity": "sha256:00"}` {
			t.Fatalf("decision evidence = %+v (%s)", d, d.ProviderEvidence)
		}
	}

	s.status, s.body = 503, `{"error":"pdp_runtime_error"}`
	down, err := e.Submit(ctx, actor, submission("outage"))
	if !errors.Is(err, action.ErrGovernanceUnavailable) || down.State != "RECEIVED" || down.ID == uuid.Nil {
		t.Fatalf("outage: %+v, %v", down, err)
	}
	calls := s.calls.Load()
	cancelled, err := e.Cancel(ctx, action.Principal(f.Tenant, f.P["carol"]), down.ID, "no longer needed")
	if err != nil || cancelled.State != "CANCELLED" || s.calls.Load() != calls {
		t.Fatalf("cancel during outage: %+v, %v (PDP calls %d -> %d)", cancelled, err, calls, s.calls.Load())
	}

	pending, err := e.Submit(ctx, actor, submission("recovers"))
	if !errors.Is(err, action.ErrGovernanceUnavailable) || pending.State != "RECEIVED" {
		t.Fatalf("second outage: %+v, %v", pending, err)
	}
	s.status, s.body = 0, ""
	again, err := e.Submit(ctx, actor, submission("recovers"))
	if err != nil || again.ID != pending.ID || again.State != "QUEUED" {
		t.Fatalf("after recovery: %+v, %v", again, err)
	}
}
