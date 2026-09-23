package action_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"eacp/internal/action"
	"eacp/internal/registry"
	"eacp/internal/worker"
)

// claim leases a QUEUED action for worker w (T14).
func (v env) claim(id uuid.UUID, w string, lease time.Duration) (*worker.Store, worker.Lease) {
	v.t.Helper()
	s := worker.NewStore(v.f.App, w)
	l, ok, err := s.Claim(context.Background(), worker.Candidate{TenantID: v.f.Tenant, ActionID: id}, lease)
	if err != nil || !ok {
		v.t.Fatalf("claim = %v %v", ok, err)
	}
	return s, l
}

// dispatch claims an action and commits its dispatch intent (T16).
func (v env) dispatch(id uuid.UUID, w string) (*worker.Store, worker.Lease) {
	v.t.Helper()
	s, l := v.claim(id, w, time.Minute)
	if d, _, err := s.Intent(context.Background(), l, 2*time.Second); err != nil || d != worker.Dispatched {
		v.t.Fatalf("intent = %s %v", d, err)
	}
	return s, l
}

func (v env) complete(s *worker.Store, l worker.Lease, r worker.Result) string {
	v.t.Helper()
	c, err := s.Complete(context.Background(), l, r, 100*time.Millisecond)
	if err != nil {
		v.t.Fatal(err)
	}
	return c.State
}

var ambiguous = worker.Result{Outcome: worker.Ambiguous, ErrorClass: "timeout"}

func TestCancelAfterReleaseFollowsTheDispatchIntent(t *testing.T) {
	v := newEnv(t, allowAll)
	ctx := context.Background()

	// LEASED: nothing was dispatched, so the action is cancelled outright and
	// the holder's dispatch intent is fenced out.
	leased := v.mustSubmit("k1", "QUEUED")
	s, l := v.claim(leased.ID, "w1", time.Minute)
	got, err := v.e.Cancel(ctx, v.actor(), leased.ID, "withdrawn")
	if err != nil || got.State != "CANCELLED" {
		t.Fatalf("cancel LEASED = %+v %v", got, err)
	}
	if _, _, err := s.Intent(ctx, l, 2*time.Second); !errors.Is(err, worker.ErrLeaseLost) {
		t.Fatalf("intent after cancel = %v", err)
	}

	// EXECUTING: the call may be under way, so only a request is recorded.
	running := v.mustSubmit("k2", "QUEUED")
	s, l = v.dispatch(running.ID, "w1")
	got, err = v.e.Cancel(ctx, v.actor(), running.ID, "withdrawn")
	if err != nil || got.State != "EXECUTING" || got.CancelRequestedAt == nil {
		t.Fatalf("cancel EXECUTING = %+v %v", got, err)
	}
	if _, err := v.e.Cancel(ctx, v.actor(), running.ID, "again"); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("second cancel = %v, want ErrConflict", err)
	}
	if cancelRequested, err := s.Heartbeat(ctx, l, time.Minute); err != nil || !cancelRequested {
		t.Fatalf("heartbeat = %v %v, want the cancellation", cancelRequested, err)
	}
	// Even a read is not retried once cancelled; its outcome stays unknown.
	if st := v.complete(s, l, ambiguous); st != "UNKNOWN_OUTCOME" || v.get(running.ID).StateReason != "cancelled during the call" {
		t.Fatalf("result after cancel = %s (%s)", st, v.get(running.ID).StateReason)
	}

	// RETRY_WAIT: the request stops the next attempt (T27).
	waiting := v.mustSubmit("k3", "QUEUED")
	s, l = v.dispatch(waiting.ID, "w1")
	if st := v.complete(s, l, ambiguous); st != "RETRY_WAIT" {
		t.Fatalf("ambiguous read = %s", st)
	}
	got, err = v.e.Cancel(ctx, v.actor(), waiting.ID, "withdrawn")
	if err != nil || got.State != "RETRY_WAIT" || got.CancelRequestedAt == nil {
		t.Fatalf("cancel RETRY_WAIT = %+v %v", got, err)
	}
	v.sweep()
	if got := v.get(waiting.ID); got.State != "FAILED" || got.StateReason != "cancelled" {
		t.Fatalf("after sweep = %s (%s)", got.State, got.StateReason)
	}
}

func TestSweeperSchedulesRetries(t *testing.T) {
	v := newEnv(t, allowAll)
	same := v.mustSubmit("k1", "QUEUED")
	s, l := v.dispatch(same.ID, "w1")
	if st := v.complete(s, l, ambiguous); st != "RETRY_WAIT" {
		t.Fatalf("ambiguous read = %s", st)
	}
	if got := v.get(same.ID); got.AttemptCount != 1 || got.NextAttemptAt == nil || got.LeaseGeneration != 1 {
		t.Fatalf("waiting = %+v", got)
	}
	if st := v.sweep(); st.Retried != 0 || v.get(same.ID).State != "RETRY_WAIT" {
		t.Fatalf("a retry ran before it was due: %+v", st)
	}
	time.Sleep(150 * time.Millisecond)
	if st := v.sweep(); st.Retried != 1 {
		t.Fatalf("stats = %+v", st)
	}
	if got := v.get(same.ID); got.State != "QUEUED" || got.StateReason != "retry" || *got.PolicyVersion != 1 {
		t.Fatalf("T25 = %+v", got)
	}
	// The next attempt takes a new generation and keeps the operation key.
	s, l = v.dispatch(same.ID, "w2")
	if l.Generation != 2 || v.complete(s, l, worker.Result{Outcome: worker.Succeeded, ExternalReference: "R-1"}) != "SUCCEEDED" {
		t.Fatalf("second attempt generation %d", l.Generation)
	}
	if got := v.get(same.ID); got.AttemptCount != 2 || got.OperationKey != same.OperationKey {
		t.Fatalf("after retry = %+v", got)
	}

	// Under a new policy the retry goes back through the release boundary
	// (T26), and is released again only if the new policy allows it.
	moved := v.mustSubmit("k2", "QUEUED")
	s, l = v.dispatch(moved.ID, "w1")
	v.complete(s, l, ambiguous)
	v.activate(denyAll)
	time.Sleep(150 * time.Millisecond)
	v.sweep()
	if got := v.get(moved.ID); got.State != "DENIED" {
		t.Fatalf("retry under a denying policy = %s (%s)", got.State, got.StateReason)
	}
	if n := v.count(`SELECT count(*) FROM eacp.audit_events
		WHERE convert_from(payload, 'UTF8')::jsonb->'subject'->>'id' = $1::text
		  AND convert_from(payload, 'UTF8')::jsonb->'data'->>'to' = 'AUTHORIZED'
		  AND convert_from(payload, 'UTF8')::jsonb->>'reason' = 'policy changed before retry'`, moved.ID); n != 1 {
		t.Fatalf("T26 journal entries = %d", n)
	}
}

func TestSweeperReclaimsLapsedLeases(t *testing.T) {
	v := newEnv(t, allowAll)
	// T17: a lease that lapsed before its dispatch intent is re-queued.
	idle := v.mustSubmit("k1", "QUEUED")
	v.claim(idle.ID, "w1", time.Second)
	// T24: an expired read is retried (the lapsed lease was its backoff)
	// until its attempts run out (T27).
	read := v.mustSubmit("k2", "QUEUED")
	for attempt := int64(1); attempt <= 3; attempt++ {
		_, l := v.dispatch(read.ID, "w1")
		if err := v.f.ExecWorker("w1", l.Generation, `UPDATE eacp.actions
			SET leased_until = now() + interval '1 second' WHERE id = $1`, read.ID); err != nil {
			t.Fatal(err)
		}
		if st := v.sweep(); st.Reclaimed != 0 {
			t.Fatalf("a live lease was reclaimed: %+v", st)
		}
		time.Sleep(1100 * time.Millisecond)
		st := v.sweep()
		if want := 1 + btoi(attempt == 1); st.Reclaimed != want || st.Retried != 1 {
			t.Fatalf("attempt %d stats = %+v", attempt, st)
		}
		want, reason := "QUEUED", "retry"
		if attempt == 3 {
			want, reason = "FAILED", "retry budget exhausted"
		}
		if got := v.get(read.ID); got.State != want || got.StateReason != reason || got.AttemptCount != int(attempt) {
			t.Fatalf("attempt %d = %s (%s)", attempt, got.State, got.StateReason)
		}
	}
	if got := v.get(idle.ID); got.State != "QUEUED" || got.StateReason != "lease expired before dispatch" {
		t.Fatalf("T17 = %s (%s)", got.State, got.StateReason)
	}
	if n := v.count(`SELECT count(*) FROM eacp.audit_events
		WHERE convert_from(payload, 'UTF8')::jsonb->'subject'->>'id' = $1::text
		  AND convert_from(payload, 'UTF8')::jsonb->'data'->>'to' = 'RETRY_WAIT'
		  AND convert_from(payload, 'UTF8')::jsonb->>'reason' = 'lease expired during a read'`, read.ID); n != 3 {
		t.Fatalf("T24 journal entries = %d", n)
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestAdmissionCountsActionsInFlight(t *testing.T) {
	v := newEnv(t, allowAll, func(o *action.Options) {
		o.Limits = action.Limits{MaxQueuedPerTenant: 1, MaxQueuedGlobal: 100}
	})
	first := v.mustSubmit("k1", "QUEUED")
	s, l := v.dispatch(first.ID, "w1")
	if _, err := v.submit("k2"); !errors.Is(err, action.ErrAdmission) {
		t.Fatalf("submit while one executes = %v, want ErrAdmission", err)
	}
	v.complete(s, l, ambiguous) // RETRY_WAIT still holds the slot
	if _, err := v.submit("k2"); !errors.Is(err, action.ErrAdmission) {
		t.Fatalf("submit while one waits to retry = %v, want ErrAdmission", err)
	}
	time.Sleep(150 * time.Millisecond)
	v.sweep()
	s, l = v.dispatch(first.ID, "w1")
	v.complete(s, l, worker.Result{Outcome: worker.Succeeded, ExternalReference: "R-1"})
	v.mustSubmit("k2", "QUEUED")
}
