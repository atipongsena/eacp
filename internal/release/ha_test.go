package release_test

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/release"
)

// breached returns a service whose tenant has one CANARY release with a
// guardrail breach, and that release's id.
func breached(t *testing.T) (svc, uuid.UUID) {
	t.Helper()
	v := newSvc(t)
	id := v.canary(t, cohort(t, v.world), plan{canaryActions: 2})
	v.f.QueuedAction(t, v.world.candidate, "carol", "erp.purchase")
	ok(t, v.f.ExecAgent(v.world.candidate, denySQL, v.f.ReceivedAction(t, v.world.candidate, "carol", "erp.purchase")))
	return v, id
}

func TestReleaseEvaluationSkipsATenantAnotherReplicaHolds(t *testing.T) {
	v, _ := breached(t)
	ctx := context.Background()
	hold := v.f.HoldLoopLock(t, "release")
	if n, err := v.s.Evaluate(ctx, v.f.Tenant); err != nil || n != 0 {
		t.Fatalf("evaluate while held = %d, %v; want a skip", n, err)
	}
	hold()
	if n, err := v.s.Evaluate(ctx, v.f.Tenant); err != nil || n != 1 {
		t.Fatalf("evaluate after release = %d, %v", n, err)
	}
}

// release_evaluate locks each CANARY release FOR UPDATE, so a second
// evaluation finds it ROLLED_BACK and does nothing; a double move would fail
// the state guard and surface as an error.
func TestConcurrentReleaseEvaluationsRollBackOnce(t *testing.T) {
	v, id := breached(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if err := v.f.ExecSystem(release.SystemActor, `SELECT eacp.release_evaluate()`); err != nil {
				t.Error(err)
			}
		})
		wg.Go(func() {
			if _, err := v.s.Evaluate(ctx, v.f.Tenant); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	d, err := v.s.Get(ctx, v.principal("audra"), id)
	if err != nil || d.State != "ROLLED_BACK" {
		t.Fatalf("release = %+v, %v", d.Release, err)
	}
}
