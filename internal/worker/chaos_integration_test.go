package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atipongsena/eacp/internal/action"
	"github.com/atipongsena/eacp/internal/approval"
	"github.com/atipongsena/eacp/internal/connector"
	"github.com/atipongsena/eacp/internal/governance"
	"github.com/atipongsena/eacp/internal/registry"
	"github.com/atipongsena/eacp/internal/storage/pgtest"
	"github.com/atipongsena/eacp/internal/worker"
)

// loops runs the worker, reconciler and sweeper loops as the services do,
// until the returned stop is called.
func (v *erpEnv) loops() (stop func()) {
	v.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w, err := worker.New(v.f.App, worker.Options{ID: "w-live", Lease: 5 * time.Second, PollInterval: 50 * time.Millisecond,
		Connectors: map[string]worker.Connector{"http": connector.NewHTTP()}, Secrets: v.secrets,
		Backoff: func(int) time.Duration { return 100 * time.Millisecond }})
	if err != nil {
		v.t.Fatal(err)
	}
	r, err := worker.NewReconciler(v.f.App, worker.ReconcilerOptions{ID: "r-live", Lease: 5 * time.Second,
		PollInterval: 50 * time.Millisecond, Connectors: map[string]worker.Connector{"http": connector.NewHTTP()},
		Secrets: v.secrets, Backoff: func(int) time.Duration { return 100 * time.Millisecond }})
	if err != nil {
		v.t.Fatal(err)
	}
	s := action.NewSweeper(v.e)
	s.Grace = 0
	var wg sync.WaitGroup
	wg.Go(func() { w.Run(ctx) })
	wg.Go(func() { r.Run(ctx) })
	wg.Go(func() { s.Run(ctx, 100*time.Millisecond) })
	return func() {
		cancel()
		wg.Wait()
	}
}

// settled waits until every action in ids is terminal.
func (v *erpEnv) settled(ids []uuid.UUID, within time.Duration) []action.View {
	v.t.Helper()
	deadline := time.Now().Add(within)
	for {
		var views []action.View
		done := true
		for _, id := range ids {
			got := v.get(id)
			views = append(views, got)
			done = done && got.Terminal()
		}
		if done {
			return views
		}
		if time.Now().After(deadline) {
			for _, got := range views {
				v.t.Logf("%s: %s (%s)", got.ID, got.State, got.StateReason)
			}
			v.t.Fatalf("actions not terminal after %s", within)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Chaos (MASTER_PLAN §82; invariants 1, 4, 5, 12, 16): the database drops
// every application connection again and again while the worker,
// reconciler and sweeper run. Transactions fail at every step, including
// around the dispatch intent and result commits. Every action still ends
// SUCCEEDED, with exactly one ERP record per operation key.
func TestLoopsSurviveRepeatedDatabaseConnectionLoss(t *testing.T) {
	v := newERPEnv(t)
	stop := v.loops()
	defer stop()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, v.f.DB.AdminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)

	// Work keeps arriving during the storm. A submission that loses its
	// connection is retried by its client with the same idempotency key.
	scenarios := []map[string]any{
		{}, {"scenario": "execute_then_timeout", "delay_ms": 300}, {"scenario": "execute_then_reset"},
		{"scenario": "5xx_after_effect"}, {"scenario": "slow_response", "delay_ms": 50},
	}
	actor := action.Agent(v.f.Tenant, v.agent.Agent, v.agent.Version)
	var ids []uuid.UUID
	keys := map[uuid.UUID]string{}
	kills, lost := 0, 0
	for i := 0; i < 25; i++ {
		payload := map[string]any{"amount": 42, "currency": "THB"}
		for k, val := range scenarios[i%len(scenarios)] {
			payload[k] = val
		}
		b, _ := json.Marshal(payload)
		sub := action.Submission{IdempotencyKey: uuid.NewString(), Subject: "carol@tenant-a.test", Operation: "post",
			Target: "erp", Tool: "erp.create_po", ToolSchemaVersion: "1", Resource: "po", Payload: b}
		for {
			a, err := v.e.Submit(ctx, actor, sub)
			if err == nil {
				ids = append(ids, a.ID)
				keys[a.ID] = a.OperationKey
				break
			}
			lost++
			time.Sleep(20 * time.Millisecond)
		}
		var n int
		if err := admin.QueryRow(ctx, `SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity
			WHERE datname = current_database() AND usename = 'eacp_app'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		kills += n
		time.Sleep(120 * time.Millisecond)
	}
	if kills == 0 {
		t.Fatal("no connection was terminated")
	}

	for _, got := range v.settled(ids, time.Minute) {
		if got.State != "SUCCEEDED" || got.ExternalReference != "PO-"+got.ID.String() {
			t.Errorf("%s = %s (%s)", got.ID, got.State, got.StateReason)
		}
		if n := v.effects(keys[got.ID]); n != 1 {
			t.Errorf("%s: %d ERP records", got.ID, n)
		}
	}
	var reconciled int
	for _, id := range ids {
		if ev, err := v.e.Evidence(ctx, v.f.Tenant, id); err == nil && len(ev.Checks) > 0 {
			reconciled++
		}
	}
	t.Logf("terminated %d connections; %d submissions lost their connection; %d of %d actions reconciled",
		kills, lost, reconciled, len(ids))
}

// Invariant 4: duplicate submissions, concurrent and repeated, create one
// action and one external effect.
func TestDuplicateSubmissionsProduceOneEffect(t *testing.T) {
	v := newERPEnv(t)
	stop := v.loops()
	defer stop()
	sub := action.Submission{IdempotencyKey: "po-2026-0042", Subject: "carol@tenant-a.test", Operation: "post",
		Target: "erp", Tool: "erp.create_po", ToolSchemaVersion: "1", Resource: "po",
		Payload: json.RawMessage(`{"amount":42,"currency":"THB","scenario":"execute_then_timeout","delay_ms":300}`)}
	actor := action.Agent(v.f.Tenant, v.agent.Agent, v.agent.Version)
	var wg sync.WaitGroup
	ids := make(chan uuid.UUID, 16)
	for range 8 {
		wg.Go(func() {
			got, err := v.e.Submit(context.Background(), actor, sub)
			if err != nil {
				t.Errorf("submit: %v", err)
				return
			}
			ids <- got.ID
		})
	}
	wg.Wait()
	first := <-ids
	final := v.settled([]uuid.UUID{first}, 30*time.Second)[0]
	// A retry after the outcome is known still returns the same action.
	again, err := v.e.Submit(context.Background(), actor, sub)
	if err != nil {
		t.Fatal(err)
	}
	close(ids)
	for id := range ids {
		if id != first {
			t.Fatalf("one key created actions %s and %s", first, id)
		}
	}
	if again.ID != first || final.State != "SUCCEEDED" || final.AttemptCount != 1 {
		t.Fatalf("final = %+v, resubmission %s", final, again.ID)
	}
	if n := v.effects(final.OperationKey); n != 1 {
		t.Fatalf("ERP records = %d", n)
	}
}

// Invariant 16: approval and execution state are durable. Every EACP
// process stops at once, with an approval half-voted, an approved action
// not yet released, and a call cut off after its dispatch intent. New
// processes (fresh pools and services, nothing shared in memory) finish all
// of it: the half-voted approval keeps its vote, the grant is consumed once,
// and the interrupted call is reconciled instead of re-dispatched.
func TestRestartedServicesFinishEveryPendingAction(t *testing.T) {
	v := newERPEnvWith(t, reviewPolicy)
	ctx := context.Background()
	vote := func(pool *pgxpool.Pool, a action.View, who string) {
		t.Helper()
		if _, err := approval.New(pool).Vote(ctx, registry.Actor{TenantID: v.f.Tenant, PrincipalID: v.f.P[who]},
			*a.ApprovalRequestID, approval.Approve, "reviewed"); err != nil {
			t.Fatal(err)
		}
	}
	halfVoted := v.submitTo("PENDING_APPROVAL", "create_po", map[string]any{})
	vote(v.f.App, halfVoted, "amy")
	approved := v.submitTo("PENDING_APPROVAL", "create_po", map[string]any{})
	vote(v.f.App, approved, "amy")
	vote(v.f.App, approved, "ben")
	cut := v.submitTo("PENDING_APPROVAL", "create_po", map[string]any{"scenario": "slow_response", "delay_ms": 3000})
	vote(v.f.App, cut, "amy")
	vote(v.f.App, cut, "ben")
	v.sweep() // releases approved and cut
	// The worker process dies while its call to the ERP is in flight: the
	// dispatch intent is committed, the ERP commits the PO, and no result is
	// ever recorded.
	dying := worker.NewStore(v.f.App, "dying")
	l, ok, err := dying.Claim(ctx, worker.Candidate{TenantID: v.f.Tenant, ActionID: cut.ID}, 5*time.Second)
	if err != nil || !ok {
		t.Fatalf("claim = %v %v", ok, err)
	}
	if d, _, err := dying.Intent(ctx, l, 1200*time.Millisecond); err != nil || d != worker.Dispatched {
		t.Fatalf("intent = %s %v", d, err)
	}
	secret, err := v.secrets.Resolve(v.f.Tenant, "erp", v.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	callCtx, kill := context.WithTimeout(ctx, 200*time.Millisecond)
	defer kill()
	connector.NewHTTP().Execute(callCtx, worker.Call{TenantID: v.f.Tenant, ActionID: cut.ID,
		OperationKey: cut.OperationKey, Attempt: 1, Generation: l.Generation, Tool: "erp.create_po", Endpoint: v.srv.URL,
		Payload: v.get(cut.ID).EnforcedPayload, Secret: secret,
		Contract: worker.Contract{IdempotencyMode: "native", IdempotencyKeyField: "Idempotency-Key"}})
	if got := v.get(cut.ID); got.State != "EXECUTING" {
		t.Fatalf("cut = %s", got.State)
	}
	if got := v.get(approved.ID); got.State != "QUEUED" {
		t.Fatalf("approved = %s", got.State)
	}
	v.f.App.Reset() // every process is gone: no connection, no session state

	// New processes.
	pool := pgtest.Pool(t, v.f.DB.AppDSN)
	restarted := &erpEnv{t: t, f: v.f, srv: v.srv, secret: v.secret, secrets: v.secrets, agent: v.agent,
		e: action.New(pool, action.Options{Provider: governance.LocalProvider{InstanceID: "restarted"}})}
	if got := restarted.get(halfVoted.ID); got.State != "PENDING_APPROVAL" {
		t.Fatalf("half-voted = %s", got.State)
	}
	vote(pool, halfVoted, "ben") // amy's vote survived: ben's completes the quorum
	stop := restarted.loops()
	defer stop()
	for _, got := range restarted.settled([]uuid.UUID{halfVoted.ID, approved.ID, cut.ID}, time.Minute) {
		if got.State != "SUCCEEDED" || got.AttemptCount != 1 || restarted.effects(got.OperationKey) != 1 {
			t.Errorf("%s = %s after %d attempts, %d ERP records", got.ID, got.State, got.AttemptCount,
				restarted.effects(got.OperationKey))
		}
	}
	if ev, err := restarted.e.Evidence(ctx, v.f.Tenant, cut.ID); err != nil || len(ev.Checks) == 0 {
		t.Fatalf("the interrupted call was not reconciled: %+v %v", ev.Checks, err)
	}
}

// downPDP is a governance provider that cannot be reached.
type downPDP struct{}

func (downPDP) Evaluate(context.Context, governance.GovernanceRequest) (governance.GovernanceDecision, error) {
	return governance.GovernanceDecision{}, errors.New("pdp: connection refused")
}

// Invariant 18: with governance down, a new side-effecting action fails
// closed (it stays RECEIVED and is never dispatched), yet cancellation,
// reconciliation and containment all still work.
func TestGovernanceOutageFailsClosedWithoutBlockingSafety(t *testing.T) {
	v := newERPEnv(t)
	ctx := context.Background()
	unknown := v.submit("create_po", map[string]any{"scenario": "execute_then_timeout", "delay_ms": 300})
	v.runWorker(1)

	// The PDP goes down for every EACP component.
	v.e = action.New(v.f.App, action.Options{Provider: downPDP{}})
	blocked, err := v.e.Submit(ctx, action.Agent(v.f.Tenant, v.agent.Agent, v.agent.Version), action.Submission{
		IdempotencyKey: "during-outage", Subject: "carol@tenant-a.test", Operation: "post", Target: "erp",
		Tool: "erp.create_po", ToolSchemaVersion: "1", Resource: "po", Payload: json.RawMessage(`{"amount":42,"currency":"THB"}`)})
	if !errors.Is(err, action.ErrGovernanceUnavailable) || blocked.State != "RECEIVED" {
		t.Fatalf("submission during the outage = %+v, %v", blocked, err)
	}
	v.sweep()      // re-evaluation fails; nothing is released
	v.runWorker(0) // and nothing is dispatched

	// Reconciliation reads never consult governance.
	if got, checks := v.reconcile(v.reconciler(3, 100*time.Millisecond), unknown.ID); got.State != "SUCCEEDED" ||
		fmt.Sprint(checks) != "[found]" {
		t.Fatalf("reconciled during the outage = %s %v", got.State, checks)
	}
	// Cancellation never consults governance.
	if got, err := v.e.Cancel(ctx, action.Agent(v.f.Tenant, v.agent.Agent, v.agent.Version), blocked.ID,
		"outage"); err != nil || got.State != "CANCELLED" {
		t.Fatalf("cancel during the outage = %+v, %v", got, err)
	}
	// Containment: an operator suspends the agent version at once.
	if err := v.f.Exec("otto", `UPDATE eacp.agent_versions SET state = 'SUSPENDED', state_reason = 'incident'
		WHERE id = $1`, v.agent.Version); err != nil {
		t.Fatalf("suspension during the outage: %v", err)
	}
	if n := v.effects(blocked.OperationKey); n != 0 {
		t.Fatalf("the blocked action reached the ERP %d times", n)
	}
}
