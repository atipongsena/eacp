package worker_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/action"
	"github.com/atipongsena/eacp/internal/fleet"
	"github.com/atipongsena/eacp/internal/registry"
)

// ADR-024 §1: a fleet pause suspends capability. Queued work is denied at
// the dispatch intent, a new submission is denied, and nothing reaches the
// connector. Resume restores submission, never the denied action.
func TestFleetPauseDeniesQueuedAndNewWork(t *testing.T) {
	v := newEnv(t)
	ctx := context.Background()
	queued := v.submit("ledger.post")
	fl := fleet.New(v.f.App)
	op, err := fl.Apply(ctx, registry.Actor{TenantID: v.f.Tenant, PrincipalID: v.f.P["otto"]},
		fleet.Request{Kind: "pause", Selector: fleet.Selector{AgentIDs: []uuid.UUID{v.agent.Agent}}, Reason: "incident"})
	if err != nil {
		t.Fatal(err)
	}

	w := v.worker("fleet-pause")
	v.runOnce(w)
	if got := v.get(queued.ID); got.State != "DENIED" || v.conn.count() != 0 {
		t.Fatalf("queued action after pause = %s (%s), calls = %d", got.State, got.StateReason, v.conn.count())
	}
	submit := func() action.View {
		got, err := v.e.Submit(ctx, action.Agent(v.f.Tenant, v.agent.Agent, v.agent.Version), action.Submission{
			IdempotencyKey: uuid.NewString(), Subject: "carol@tenant-a.test", Operation: "post", Target: "erp",
			Tool: "ledger.post", ToolSchemaVersion: "1", Resource: "po", Payload: json.RawMessage(`{"amount":42,"currency":"THB"}`)})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := submit(); got.State != "DENIED" {
		t.Fatalf("submission while paused = %s", got.State)
	}

	if _, err := fl.Apply(ctx, registry.Actor{TenantID: v.f.Tenant, PrincipalID: v.f.P["ravi"]},
		fleet.Request{Kind: "resume", SourceOperationID: op.ID, Reason: "cleared"}); err != nil {
		t.Fatal(err)
	}
	if got := submit(); got.State != "QUEUED" {
		t.Fatalf("submission after resume = %s", got.State)
	}
	if got := v.get(queued.ID); got.State != "DENIED" {
		t.Fatalf("resume changed a denied action: %s", got.State)
	}
}
