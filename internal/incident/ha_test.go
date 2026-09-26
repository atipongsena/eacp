package incident_test

import (
	"context"
	"sync"
	"testing"

	"eacp/internal/incident"
	"eacp/internal/registry/registrytest"
)

func killedTool(t *testing.T, f *registrytest.Fixture) {
	t.Helper()
	tool := f.ActiveTool(t, "erp", "purchase")
	f.ActiveAgent(t, "buyer", tool.Tool)
	ok(t, f.Exec("otto", `SELECT eacp.set_kill('tool', $1, true, 'contain', 'security_incident')`, tool.Tool))
}

func TestIncidentEvaluationSkipsATenantAnotherReplicaHolds(t *testing.T) {
	f := registrytest.New(t)
	killedTool(t, f)
	s := incident.New(f.App)
	release := f.HoldLoopLock(t, "incident")
	if n, err := s.Evaluate(context.Background(), f.Tenant); err != nil || n != 0 {
		t.Fatalf("evaluate while held = %d, %v; want a skip", n, err)
	}
	if got := incidents(t, f, "kill"); len(got) != 0 {
		t.Fatalf("a skipped evaluation opened %d incidents", len(got))
	}
	release()
	if n, err := s.Evaluate(context.Background(), f.Tenant); err != nil || n != 1 {
		t.Fatalf("evaluate after release = %d, %v", n, err)
	}
}

// Without the lock, PostgreSQL alone still opens one incident per signal:
// the lock saves work and decides nothing.
func TestConcurrentIncidentEvaluationsActOnce(t *testing.T) {
	f := registrytest.New(t)
	killedTool(t, f)
	s := incident.New(f.App)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() { evaluate(t, f) }) // the SQL function, no loop lock
		wg.Go(func() {
			if _, err := s.Evaluate(context.Background(), f.Tenant); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if got := incidents(t, f, "kill"); len(got) != 1 {
		t.Fatalf("kill incidents = %d, want 1", len(got))
	}
}
