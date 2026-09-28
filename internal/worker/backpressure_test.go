package worker_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/worker"
)

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Invariant 9 (MASTER_PLAN §103, [B]): a connector failure cannot starve
// unrelated connector pools. The ledger connector hangs: the worker's
// bulkhead lets it hold only two of four slots, so erp actions queued behind
// it still run. When its calls fail, the worker's breaker opens and shares
// the circuit, so a second worker does not call it either, until an
// operator enables the connector again.
func TestConnectorFailureDoesNotStarveUnrelatedConnectors(t *testing.T) {
	v := newEnv(t)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var slowCalls atomic.Int32
	v.conn.fn = func(ctx context.Context, c worker.Call) worker.Result {
		if c.Tool != "ledger.post" {
			return worker.Result{Outcome: worker.Succeeded, ExternalReference: "REF-" + c.ActionID.String()[:8]}
		}
		slowCalls.Add(1)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "transport_error"}
	}
	var fast []uuid.UUID
	for range 6 {
		v.submit("ledger.post")
	}
	for range 3 {
		fast = append(fast, v.submit("erp.lookup").ID)
	}

	w1 := v.worker("w1", func(o *worker.Options) {
		o.Concurrency, o.GroupConcurrency = 4, 2
		o.BreakerFailures, o.BreakerCooldown = 2, time.Minute
		o.PollInterval = 20 * time.Millisecond
	})
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { w1.Run(ctx); close(stopped) }()
	stop := func() { cancel(); unblock(); <-stopped }
	defer stop()

	waitFor(t, 10*time.Second, "the erp actions to succeed beside the hanging connector", func() bool {
		for _, id := range fast {
			if v.get(id).State != "SUCCEEDED" {
				return false
			}
		}
		return true
	})
	if n := slowCalls.Load(); n != 2 {
		t.Fatalf("hanging calls = %d, want the bulkhead's 2", n)
	}

	// Both hanging calls fail: the breaker opens and shares the circuit.
	unblock()
	waitFor(t, 10*time.Second, "the shared circuit to open", func() bool {
		return v.count(`SELECT count(*) FROM eacp.connector_circuits
			WHERE connector_id = $1 AND open_until > now()`, v.tools["ledger.post"].Connector) == 1
	})
	time.Sleep(300 * time.Millisecond) // the loop keeps polling meanwhile
	if n := slowCalls.Load(); n != 2 {
		t.Fatalf("calls to the open connector = %d, want 2", n)
	}
	stop()

	// A second worker with a closed breaker of its own is held back by the
	// shared circuit, and serves nothing else.
	w2 := v.worker("w2")
	if n := v.runOnce(w2); n != 0 || slowCalls.Load() != 2 {
		t.Fatalf("second worker executed %d (calls %d) while the circuit is open", n, slowCalls.Load())
	}
	if n := v.count(`SELECT count(*) FROM eacp.actions WHERE tool = 'ledger.post' AND state = 'QUEUED'`); n != 4 {
		t.Fatalf("queued ledger actions = %d, want 4", n)
	}
	// An operator enables the connector: dispatch resumes, bulkheaded.
	if err := v.f.Exec("otto", switchSQL, v.tools["ledger.post"].Connector, false, "ledger recovered"); err != nil {
		t.Fatal(err)
	}
	if n := v.runOnce(w2); n != 2 || slowCalls.Load() != 4 {
		t.Fatalf("after enabling: executed %d, calls %d; want 2 and 4", n, slowCalls.Load())
	}
}

// A lease claimed before the circuit opened is released before its dispatch
// intent: nothing is called, and the action is queued again (T17).
func TestLeaseIsReleasedWhenTheCircuitOpensBeforeDispatch(t *testing.T) {
	v := newEnv(t)
	got := v.submit("erp.lookup")
	store := worker.NewStore(v.f.App, "w1")
	l, ok, err := store.Claim(context.Background(), worker.Candidate{TenantID: v.f.Tenant, ActionID: got.ID}, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim = %v %v", ok, err)
	}
	if err := v.f.ExecWorker("w9", 1, tripSQL, v.tools["erp.lookup"].Connector, 60, "tripped elsewhere"); err != nil {
		t.Fatal(err)
	}
	v.worker("w1").Execute(context.Background(), l)
	if v.conn.count() != 0 {
		t.Fatal("a connector with an open circuit was called")
	}
	if st := v.get(got.ID); st.State != "QUEUED" || st.StateReason != "connector circuit open" {
		t.Fatalf("action = %s (%s), want QUEUED (connector circuit open)", st.State, st.StateReason)
	}
}

// The per-worker breaker holds on its own: after an operator clears the
// shared circuit, the worker whose breaker opened still neither claims nor
// dispatches to that connector until its cooldown ends; another worker may.
func TestLocalBreakerHoldsWithoutTheSharedCircuit(t *testing.T) {
	v := newEnv(t)
	v.conn.fn = func(context.Context, worker.Call) worker.Result {
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "transport_error"}
	}
	ledger := v.tools["ledger.post"].Connector
	w1 := v.worker("w1", func(o *worker.Options) { o.BreakerFailures, o.BreakerCooldown = 1, time.Minute })
	v.submit("ledger.post")
	if n := v.runOnce(w1); n != 1 || v.conn.count() != 1 {
		t.Fatalf("first run executed %d, calls %d", n, v.conn.count())
	}
	if n := v.count(`SELECT count(*) FROM eacp.connector_circuits WHERE connector_id = $1 AND open_until > now()`, ledger); n != 1 {
		t.Fatal("the breaker did not open the shared circuit")
	}
	if err := v.f.Exec("otto", switchSQL, ledger, false, "cleared by an operator"); err != nil {
		t.Fatal(err)
	}
	next := v.submit("ledger.post")
	if n := v.runOnce(w1); n != 0 || v.conn.count() != 1 {
		t.Fatalf("worker with an open breaker executed %d (calls %d)", n, v.conn.count())
	}
	// Even a lease it holds is released before any dispatch intent.
	l, ok, err := worker.NewStore(v.f.App, "w1").Claim(context.Background(),
		worker.Candidate{TenantID: v.f.Tenant, ActionID: next.ID}, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim = %v %v", ok, err)
	}
	w1.Execute(context.Background(), l)
	if got := v.get(next.ID); v.conn.count() != 1 || got.State != "QUEUED" || got.StateReason != "connector circuit open" {
		t.Fatalf("held lease: calls %d, action %s (%s)", v.conn.count(), got.State, got.StateReason)
	}
	if n := v.runOnce(v.worker("w2")); n != 1 || v.conn.count() != 2 {
		t.Fatalf("another worker executed %d (calls %d)", n, v.conn.count())
	}
}
