package bench_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/bench"
	"github.com/atipongsena/eacp/internal/registry/registrytest"
	"github.com/atipongsena/eacp/internal/storage/pgtest"
	"github.com/atipongsena/eacp/internal/worker"
)

// collected drives one action to SUCCEEDED and leaves another QUEUED.
func collected(t *testing.T) (f *registrytest.Fixture, done, queued uuid.UUID) {
	t.Helper()
	f = registrytest.New(t)
	tool := f.ActiveTool(t, "erp", "purchase")
	f.ActivatePolicy(t, registrytest.AllowPolicy)
	agent := f.ActiveAgent(t, "buyer", tool.Tool)
	done = f.QueuedAction(t, agent.Version, "carol", "erp.purchase")
	ctx := context.Background()
	s := worker.NewStore(f.App, "bench")
	l, ok, err := s.Claim(ctx, worker.Candidate{TenantID: f.Tenant, ActionID: done}, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim = %v %v", ok, err)
	}
	if d, _, err := s.Intent(ctx, l, 2*time.Second); err != nil || d != worker.Dispatched {
		t.Fatalf("intent = %s %v", d, err)
	}
	if _, err := s.Complete(ctx, l, worker.Result{Outcome: worker.Succeeded, ExternalReference: "po-1"},
		100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	queued = f.QueuedAction(t, agent.Version, "carol", "erp.purchase")
	return f, done, queued
}

func TestReadTimelinesReadsEveryStage(t *testing.T) {
	f, done, queued := collected(t)
	ctx := context.Background()
	admin := pgtest.Pool(t, f.DB.AdminDSN)
	ids := []uuid.UUID{done, queued}
	tls, err := bench.ReadTimelines(ctx, admin, f.Tenant, ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(tls) != 2 || tls[0].ActionID != done.String() || tls[1].ActionID != queued.String() {
		t.Fatalf("timelines = %+v, want one per id in order", tls)
	}
	s := bench.ComputeStages(tls)
	if s.Terminal["SUCCEEDED"] != 1 || s.Open != 1 {
		t.Fatalf("terminal %v, open %d; want one SUCCEEDED and one open", s.Terminal, s.Open)
	}
	for name, got := range map[string][]time.Duration{"governance": s.Governance, "queue wait": s.QueueWait,
		"lease": s.Lease, "external": s.External, "end-to-end": s.EndToEnd} {
		want := 1
		if name == "governance" {
			want = 2 // the queued action was decided too
		}
		if len(got) != want || got[0] < 0 {
			t.Errorf("%s = %v, want %d non-negative duration(s)", name, got, want)
		}
	}
	open, err := bench.OpenCount(ctx, admin, f.Tenant, ids)
	if err != nil || open != 1 {
		t.Fatalf("OpenCount = %d %v, want 1", open, err)
	}
	if open, err := bench.TenantOpen(ctx, admin, f.Tenant); err != nil || open != 1 {
		t.Fatalf("TenantOpen = %d %v, want the one queued action", open, err)
	}
	db, err := bench.ReadDB(ctx, admin)
	if err != nil || db.Commits <= 0 || db.SizeBytes <= 0 {
		t.Fatalf("ReadDB = %+v %v", db, err)
	}
	if calls, _, err := bench.ReadFunctionStats(ctx, admin, "no_such_function"); err != nil || calls != 0 {
		t.Fatalf("ReadFunctionStats of an unknown function = %d %v, want 0 and no error", calls, err)
	}
	if calls, err := bench.ReadLLMCalls(ctx, admin, f.Tenant, time.Now().Add(-time.Hour), time.Now()); err != nil || len(calls) != 0 {
		t.Fatalf("ReadLLMCalls = %v %v, want none", calls, err)
	}
}

func TestDuplicateKeysAreCounted(t *testing.T) {
	f, _, _ := collected(t)
	n, err := bench.DuplicateKeys(context.Background(), pgtest.Pool(t, f.DB.AdminDSN), f.Tenant, time.Now().Add(-time.Hour))
	if err != nil || n != 0 {
		t.Fatalf("DuplicateKeys = %d %v, want 0", n, err)
	}
}
