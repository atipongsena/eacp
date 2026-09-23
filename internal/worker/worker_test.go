package worker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/action"
	"eacp/internal/governance"
	"eacp/internal/registry/registrytest"
	"eacp/internal/storage"
	"eacp/internal/storage/pgtest"
	"eacp/internal/worker"
)

// fastWriteContractSQL is a non-idempotent write with a 500 ms call budget,
// so lease-expiry scenarios run quickly.
const fastWriteContractSQL = `INSERT INTO eacp.tool_contracts
	(tenant_id, tool_id, side_effects, idempotency_mode, reconciliation_lookup,
	 reconciliation_consistency, proof_standard, max_attempts, timeout_ms)
	VALUES (eacp.current_tenant_id(), $1, '{IRREVERSIBLE_WRITE}', 'none', 'none', 'none', 'none', 1, 500)
	RETURNING id`

// fake is a scripted connector that counts calls.
type fake struct {
	mu    sync.Mutex
	calls []worker.Call
	fn    func(context.Context, worker.Call) worker.Result
}

func (f *fake) Execute(ctx context.Context, c worker.Call) worker.Result {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	fn := f.fn
	f.mu.Unlock()
	if fn == nil {
		return worker.Result{Outcome: worker.Succeeded, ExternalReference: "PO-" + c.ActionID.String()[:8]}
	}
	return fn(ctx, c)
}

func (f *fake) Lookup(context.Context, worker.LookupCall) worker.LookupResult {
	return worker.LookupResult{Status: worker.LookupUnknown}
}

func (f *fake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type env struct {
	t       *testing.T
	f       *registrytest.Fixture
	e       *action.Engine
	agent   registrytest.Agent
	tools   map[string]registrytest.Tooling
	conn    *fake
	secrets *worker.SecretStore
	logs    *bytes.Buffer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	f := registrytest.New(t)
	v := &env{t: t, f: f, conn: &fake{}, logs: &bytes.Buffer{}, tools: map[string]registrytest.Tooling{}}
	v.tools["erp.lookup"] = f.ActiveTool(t, "erp", "lookup")
	v.tools["ledger.post"] = f.ActiveToolWith(t, "ledger", "post", registrytest.WriteContractSQL)
	v.tools["bank.pay"] = f.ActiveToolWith(t, "bank", "pay", registrytest.IdempotentContractSQL)
	v.tools["fast.post"] = f.ActiveToolWith(t, "fast", "post", fastWriteContractSQL)
	var ids []uuid.UUID
	for _, tl := range v.tools {
		ids = append(ids, tl.Tool)
	}
	v.agent = f.ActiveAgent(t, "buyer", ids...)
	f.ActivatePolicy(t, registrytest.AllowPolicy)
	var entries []string
	for _, ref := range []string{"erp", "ledger", "bank", "fast"} {
		entries = append(entries, fmt.Sprintf(`{"tenant_id":%q,"secret_ref":%q,"host":"fakeerp:8090","value":"%s-%s"}`,
			pgtest.TenantA, ref, canary, ref))
	}
	var err error
	v.secrets, err = worker.LoadSecrets(secretsFile(t, `{"secrets":[`+strings.Join(entries, ",")+`]}`))
	if err != nil {
		t.Fatal(err)
	}
	v.e = action.New(f.App, action.Options{Provider: governance.LocalProvider{InstanceID: "worker-test"}})
	return v
}

func (v *env) worker(id string, opts ...func(*worker.Options)) *worker.Worker {
	v.t.Helper()
	o := worker.Options{
		ID: id, Lease: 5 * time.Second, Connectors: map[string]worker.Connector{"http": v.conn},
		Secrets: v.secrets, Backoff: func(int) time.Duration { return 100 * time.Millisecond },
		Log: slog.New(slog.NewJSONHandler(syncWriter{v.logs, &sync.Mutex{}}, nil)),
	}
	for _, fn := range opts {
		fn(&o)
	}
	w, err := worker.New(v.f.App, o)
	if err != nil {
		v.t.Fatal(err)
	}
	return w
}

type syncWriter struct {
	b  *bytes.Buffer
	mu *sync.Mutex
}

func (w syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

// submit creates an action on tool and advances it to QUEUED.
func (v *env) submit(tool string) action.View {
	v.t.Helper()
	got, err := v.e.Submit(context.Background(), action.Agent(v.f.Tenant, v.agent.Agent, v.agent.Version),
		action.Submission{IdempotencyKey: uuid.NewString(), Subject: "carol@tenant-a.test", Operation: "post",
			Target: "erp", Tool: tool, ToolSchemaVersion: "1", Resource: "po",
			Payload: json.RawMessage(`{"amount":42,"currency":"THB"}`)})
	if err != nil || got.State != "QUEUED" {
		v.t.Fatalf("submit %s = %+v, err = %v", tool, got, err)
	}
	return got
}

func (v *env) get(id uuid.UUID) action.View {
	v.t.Helper()
	got, err := v.e.Get(context.Background(), v.f.Tenant, id)
	if err != nil {
		v.t.Fatal(err)
	}
	return got
}

func (v *env) runOnce(w *worker.Worker) int {
	v.t.Helper()
	n, err := w.RunOnce(context.Background())
	if err != nil {
		v.t.Fatal(err)
	}
	return n
}

func (v *env) sweep() {
	v.t.Helper()
	s := action.NewSweeper(v.e)
	s.Grace = 0
	if _, err := s.RunOnce(context.Background()); err != nil {
		v.t.Fatal(err)
	}
}

func (v *env) count(sql string, args ...any) int {
	v.t.Helper()
	ctx := context.Background()
	var n int
	if err := storage.InTenantTx(ctx, v.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&n)
	}); err != nil {
		v.t.Fatal(err)
	}
	return n
}

func TestWorkerExecutesAQueuedActionOnce(t *testing.T) {
	v := newEnv(t)
	a := v.submit("ledger.post")
	w := v.worker("w1")
	if n := v.runOnce(w); n != 1 {
		t.Fatalf("executed %d actions", n)
	}
	got := v.get(a.ID)
	if got.State != "SUCCEEDED" || got.ExternalReference == "" || got.LeaseGeneration != 1 || got.AttemptCount != 1 {
		t.Fatalf("action = %+v", got)
	}
	c := v.conn.calls[0]
	if v.conn.count() != 1 || c.OperationKey != got.OperationKey || c.Generation != 1 || c.Attempt != 1 ||
		string(c.Payload) != string(got.EnforcedPayload) || c.Secret.Reveal() != canary+"-ledger" ||
		c.Contract.MaxAttempts != 1 || c.Endpoint != "http://fakeerp:8090" {
		t.Fatalf("call = %+v", c)
	}
	if n := v.runOnce(w); n != 0 || v.conn.count() != 1 {
		t.Fatalf("second pass executed %d, calls %d", n, v.conn.count())
	}
	if n := v.count(`SELECT count(*) FROM eacp.action_attempts WHERE action_id = $1 AND outcome = 'succeeded'
		AND NOT late`, a.ID); n != 1 {
		t.Fatalf("attempt rows = %d", n)
	}
}

func TestLeaseRaceHasExactlyOneWinner(t *testing.T) {
	v := newEnv(t)
	a := v.submit("ledger.post")
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := range 8 {
		store := worker.NewStore(v.f.App, fmt.Sprintf("racer-%d", i))
		wg.Go(func() {
			_, ok, err := store.Claim(context.Background(), worker.Candidate{TenantID: v.f.Tenant, ActionID: a.ID}, 30*time.Second)
			if err != nil {
				t.Error(err)
			}
			if ok {
				wins.Add(1)
			}
		})
	}
	wg.Wait()
	if wins.Load() != 1 || v.get(a.ID).LeaseGeneration != 1 {
		t.Fatalf("wins = %d, generation %d", wins.Load(), v.get(a.ID).LeaseGeneration)
	}
}

// Invariants 1 and 12: a stale worker cannot commit, and cannot cause a
// second dispatch of a non-idempotent action.
func TestStaleWorkerBeforeIntentNeverDispatches(t *testing.T) {
	v := newEnv(t)
	a := v.submit("ledger.post")
	stale := worker.NewStore(v.f.App, "stale")
	l, ok, err := stale.Claim(context.Background(), worker.Candidate{TenantID: v.f.Tenant, ActionID: a.ID}, time.Second)
	if err != nil || !ok {
		t.Fatalf("claim = %v %v", ok, err)
	}
	time.Sleep(1100 * time.Millisecond) // the stale worker freezes; its lease expires
	v.sweep()                           // T17
	if got := v.get(a.ID); got.State != "QUEUED" {
		t.Fatalf("after lease expiry = %s", got.State)
	}
	if n := v.runOnce(v.worker("fresh")); n != 1 || v.get(a.ID).State != "SUCCEEDED" {
		t.Fatalf("fresh worker executed %d, state %s", n, v.get(a.ID).State)
	}
	// The stale worker wakes up: every fenced write fails, so it never calls.
	if _, _, err := stale.Intent(context.Background(), l, 2*time.Second); !errors.Is(err, worker.ErrLeaseLost) {
		t.Fatalf("stale intent = %v, want ErrLeaseLost", err)
	}
	if _, err := stale.Heartbeat(context.Background(), l, 5*time.Second); !errors.Is(err, worker.ErrLeaseLost) {
		t.Fatalf("stale heartbeat = %v", err)
	}
	if err := stale.Release(context.Background(), l, "x"); !errors.Is(err, worker.ErrLeaseLost) && err != nil {
		t.Fatalf("stale release = %v", err)
	}
	if v.conn.count() != 1 || v.get(a.ID).State != "SUCCEEDED" {
		t.Fatalf("calls = %d, state %s", v.conn.count(), v.get(a.ID).State)
	}
}

func TestStaleWorkerAfterIntentIsUnknownOutcomeAndLate(t *testing.T) {
	v := newEnv(t)
	a := v.submit("fast.post")
	stale := worker.NewStore(v.f.App, "stale")
	ctx := context.Background()
	l, _, err := stale.Claim(ctx, worker.Candidate{TenantID: v.f.Tenant, ActionID: a.ID}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	decision, d, err := stale.Intent(ctx, l, 1100*time.Millisecond)
	if err != nil || decision != worker.Dispatched || d.Attempt != 1 || d.Timeout != 500*time.Millisecond {
		t.Fatalf("intent = %s %+v %v", decision, d, err)
	}
	// The call goes out, then the worker freezes past its lease.
	time.Sleep(1800 * time.Millisecond)
	// T23: the effect may have happened. The contract offers no proof, and
	// the call has settled once the lease lapsed, so the same sweep hands
	// the unknown outcome to a human (T29).
	v.sweep()
	if got := v.get(a.ID); got.State != "NEEDS_HUMAN_RESOLUTION" ||
		got.StateReason != "no reconciliation proof: proof standard none" {
		t.Fatalf("after lease loss = %s (%s)", got.State, got.StateReason)
	}
	if n := v.count(`SELECT count(*) FROM eacp.audit_events WHERE convert_from(payload, 'UTF8')::jsonb->'subject'->>'id' = $1::text
		AND convert_from(payload, 'UTF8')::jsonb->'data'->>'to' = 'UNKNOWN_OUTCOME'`, a.ID); n != 1 {
		t.Fatalf("UNKNOWN_OUTCOME journaled %d times", n)
	}
	// Nothing is re-dispatched, by any worker, however often it runs.
	w := v.worker("fresh")
	for range 3 {
		v.runOnce(w)
		v.sweep()
	}
	if v.conn.count() != 0 {
		t.Fatalf("re-dispatched %d times", v.conn.count())
	}
	c, err := stale.Complete(ctx, l, worker.Result{Outcome: worker.Succeeded, ExternalReference: "PO-7"}, time.Second)
	if err != nil || !c.Late {
		t.Fatalf("late completion = %+v %v", c, err)
	}
	if got := v.get(a.ID); got.State != "NEEDS_HUMAN_RESOLUTION" || got.ExternalReference != "" {
		t.Fatalf("late result changed the action: %+v", got)
	}
}

func TestResultsAreClassifiedAndRetriedPerContract(t *testing.T) {
	v := newEnv(t)
	// A certified no-effect of an idempotent write retries with the same
	// operation key until its attempts run out.
	pay := v.submit("bank.pay")
	v.conn.fn = func(context.Context, worker.Call) worker.Result {
		return worker.Result{Outcome: worker.NoEffect, ErrorClass: "refused"}
	}
	w := v.worker("w1")
	for range 10 {
		v.runOnce(w)
		time.Sleep(150 * time.Millisecond)
		v.sweep()
		if v.get(pay.ID).State == "FAILED" {
			break
		}
	}
	got := v.get(pay.ID)
	if got.State != "FAILED" || got.AttemptCount != 3 || v.conn.count() != 3 || got.StateReason != "no effect: refused" {
		t.Fatalf("pay = %+v after %d calls", got, v.conn.count())
	}
	for i, c := range v.conn.calls {
		if c.OperationKey != got.OperationKey || c.Generation != int64(i+1) || c.Attempt != i+1 {
			t.Fatalf("call %d = key %s gen %d attempt %d", i, c.OperationKey, c.Generation, c.Attempt)
		}
	}

	// Ambiguity is never a failure, and a write is not retried.
	for name, res := range map[string]worker.Result{
		"ambiguous":              {Outcome: worker.Ambiguous, ErrorClass: "timeout"},
		"success without ref":    {Outcome: worker.Succeeded},
		"uncertified no-effect":  {Outcome: worker.NoEffect, ErrorClass: "http_500"},
		"unknown classification": {Outcome: "maybe"},
	} {
		v.conn = &fake{fn: func(context.Context, worker.Call) worker.Result { return res }}
		w := v.worker("w-" + strings.ReplaceAll(name, " ", "-"))
		a := v.submit("ledger.post")
		v.runOnce(w)
		v.sweep()
		v.runOnce(w)
		if got := v.get(a.ID); got.State != "UNKNOWN_OUTCOME" || v.conn.count() != 1 {
			t.Errorf("%s: state %s after %d calls", name, got.State, v.conn.count())
		}
	}

	// Nor is an idempotent write with attempts left: an ambiguous write is
	// reconciled, not retried (ADR-004 Rev 2.3), and the result is recorded.
	v.conn = &fake{fn: func(context.Context, worker.Call) worker.Result {
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "timeout"}
	}}
	idem := v.submit("bank.pay")
	v.runOnce(v.worker("w-idem"))
	if got := v.get(idem.ID); got.State != "UNKNOWN_OUTCOME" || got.StateReason != "ambiguous result" {
		t.Fatalf("ambiguous idempotent write = %s (%s)", got.State, got.StateReason)
	}
	if n := v.count(`SELECT count(*) FROM eacp.action_attempts WHERE action_id = $1 AND outcome = 'ambiguous'
		AND error_class = 'timeout' AND NOT late`, idem.ID); n != 1 {
		t.Fatalf("recorded attempts = %d", n)
	}

	// An ambiguous read is retried (T22a) and may then succeed.
	var n atomic.Int32
	v.conn = &fake{fn: func(_ context.Context, c worker.Call) worker.Result {
		if n.Add(1) == 1 {
			return worker.Result{Outcome: worker.Ambiguous}
		}
		return worker.Result{Outcome: worker.Succeeded, ExternalReference: "R-1"}
	}}
	w = v.worker("reader")
	read := v.submit("erp.lookup")
	v.runOnce(w)
	if got := v.get(read.ID); got.State != "RETRY_WAIT" {
		t.Fatalf("ambiguous read = %s", got.State)
	}
	time.Sleep(150 * time.Millisecond)
	v.sweep()
	v.runOnce(w)
	if got := v.get(read.ID); got.State != "SUCCEEDED" || got.AttemptCount != 2 {
		t.Fatalf("read = %+v", got)
	}
}

func TestCancelDuringTheCallCancelsIt(t *testing.T) {
	v := newEnv(t)
	a := v.submit("ledger.post")
	started := make(chan struct{})
	v.conn.fn = func(ctx context.Context, _ worker.Call) worker.Result {
		close(started)
		<-ctx.Done()
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "cancelled"}
	}
	done := make(chan struct{})
	go func() { v.runOnce(v.worker("w1")); close(done) }()
	<-started
	got, err := v.e.Cancel(context.Background(), action.Principal(v.f.Tenant, v.f.P["carol"]), a.ID, "no longer needed")
	if err != nil || got.State != "EXECUTING" || got.CancelRequestedAt == nil {
		t.Fatalf("cancel request = %+v %v", got, err)
	}
	select {
	case <-done:
	case <-time.After(4 * time.Second): // heartbeat every Lease/3 (~1.7s)
		t.Fatal("the call was not cancelled")
	}
	if got := v.get(a.ID); got.State != "UNKNOWN_OUTCOME" || got.StateReason != "cancelled during the call" {
		t.Fatalf("after cancel = %+v", got)
	}
}

func TestDriftBeforeDispatchNeverCalls(t *testing.T) {
	v := newEnv(t)
	moved := v.submit("ledger.post")
	revoked := v.submit("bank.pay")
	v.f.ActivatePolicy(t, `{"format_version":1,"rules":[{"id":"all","verdict":"allow","reason":"v2"}]}`)
	if err := v.f.Exec("otto", `UPDATE eacp.tool_contracts SET revoked_at = now(), revoke_reason = 'incident'
		WHERE id = $1`, v.tools["bank.pay"].Contract); err != nil {
		t.Fatal(err)
	}
	w := v.worker("w1")
	v.runOnce(w)
	if v.conn.count() != 0 {
		t.Fatalf("called %d times despite drift", v.conn.count())
	}
	if got := v.get(revoked.ID); got.State != "DENIED" || got.StateReason != "revoked_before_dispatch: contract_revoked" {
		t.Fatalf("revoked = %+v", got)
	}
	if got := v.get(moved.ID); got.State != "AUTHORIZED" {
		t.Fatalf("moved = %s", got.State)
	}
	// Re-released under v2 by the agent (or the sweeper), then executed.
	if got, err := v.e.Advance(context.Background(), action.Agent(v.f.Tenant, v.agent.Agent, v.agent.Version), moved.ID); err != nil || got.State != "QUEUED" || *got.PolicyVersion != 2 {
		t.Fatalf("re-release = %+v %v", got, err)
	}
	v.runOnce(w)
	if got := v.get(moved.ID); got.State != "SUCCEEDED" || v.conn.count() != 1 {
		t.Fatalf("after re-release = %s, calls %d", got.State, v.conn.count())
	}
}

func TestTamperedEnforcedPayloadIsDeniedWithAnAlert(t *testing.T) {
	v := newEnv(t)
	a := v.submit("ledger.post")
	ctx := context.Background()
	// Only the schema owner can do this, and only by disabling the guard.
	for _, sql := range []string{
		`ALTER TABLE eacp.actions DISABLE TRIGGER actions_guard`,
		`UPDATE eacp.actions SET enforced_payload = '{"amount":999999,"currency":"THB"}' WHERE id = '` + a.ID.String() + `'`,
		`ALTER TABLE eacp.actions ENABLE TRIGGER actions_guard`,
	} {
		if err := storage.InTenantTx(ctx, v.f.Owner, pgtest.TenantA, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	v.runOnce(v.worker("w1"))
	if got := v.get(a.ID); got.State != "DENIED" || got.StateReason != "enforced_digest_mismatch" || v.conn.count() != 0 {
		t.Fatalf("tampered = %+v, calls %d", got, v.conn.count())
	}
	if !strings.Contains(v.logs.String(), "worker.enforced_digest_mismatch") {
		t.Fatal("no security alert logged")
	}
}

// A worker told to stop releases what it leased but has not dispatched.
func TestShutdownReleasesUndispatchedLeases(t *testing.T) {
	v := newEnv(t)
	a := v.submit("ledger.post")
	w := v.worker("w1")
	l, ok, err := w.Store().Claim(context.Background(), worker.Candidate{TenantID: v.f.Tenant, ActionID: a.ID}, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim = %v %v", ok, err)
	}
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	w.Execute(stopped, l)
	if got := v.get(a.ID); got.State != "QUEUED" || got.StateReason != "worker shutting down" || v.conn.count() != 0 {
		t.Fatalf("after shutdown = %s (%s), calls %d", got.State, got.StateReason, v.conn.count())
	}
}

func TestWorkerClaimsOnlyWhatItCanServe(t *testing.T) {
	v := newEnv(t)
	a := v.submit("ledger.post")
	none := v.worker("nothing", func(o *worker.Options) { o.Connectors = nil })
	noSecret := v.worker("nosecret", func(o *worker.Options) { o.Secrets = nil })
	if v.runOnce(none) != 0 || v.runOnce(noSecret) != 0 || v.get(a.ID).State != "QUEUED" {
		t.Fatalf("an unserviceable action was claimed: %s", v.get(a.ID).State)
	}
}

func TestRunLoopExecutesAndStops(t *testing.T) {
	v := newEnv(t)
	w := v.worker("loop", func(o *worker.Options) { o.PollInterval = 50 * time.Millisecond })
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { w.Run(ctx); close(stopped) }()
	a := v.submit("erp.lookup")
	b := v.submit("ledger.post")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && (v.get(a.ID).State != "SUCCEEDED" || v.get(b.ID).State != "SUCCEEDED") {
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-stopped
	if v.get(a.ID).State != "SUCCEEDED" || v.get(b.ID).State != "SUCCEEDED" {
		t.Fatalf("states = %s %s", v.get(a.ID).State, v.get(b.ID).State)
	}
}

// Many quick completions must never leave the loop unable to stop.
func TestRunLoopStopsUnderLoad(t *testing.T) {
	v := newEnv(t)
	w := v.worker("busy", func(o *worker.Options) { o.Concurrency = 2; o.PollInterval = 10 * time.Millisecond })
	var ids []uuid.UUID
	for range 16 {
		ids = append(ids, v.submit("erp.lookup").ID)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { w.Run(ctx); close(stopped) }()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && v.count(`SELECT count(*) FROM eacp.actions WHERE state = 'SUCCEEDED'`) < len(ids) {
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	if n := v.count(`SELECT count(*) FROM eacp.actions WHERE state = 'SUCCEEDED'`); n != len(ids) || v.conn.count() != len(ids) {
		t.Fatalf("succeeded %d of %d with %d calls", n, len(ids), v.conn.count())
	}
}

// ADR-001 §3: a secret never appears in logs, action rows, attempts, the
// journal or the outbox.
func TestSecretCanaryNeverLeaks(t *testing.T) {
	v := newEnv(t)
	v.conn.fn = func(_ context.Context, c worker.Call) worker.Result {
		// A careless connector echoes its secret in the error class and ref.
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: c.Secret.Reveal(), ExternalReference: c.Secret.Reveal()}
	}
	a := v.submit("ledger.post")
	v.runOnce(v.worker("w1"))
	v.conn.fn = nil
	v.runOnce(v.worker("w2"))
	if strings.Contains(v.logs.String(), canary) {
		t.Fatal("secret in worker logs")
	}
	for _, sql := range []string{
		`SELECT count(*) FROM eacp.actions WHERE to_jsonb(actions)::text LIKE '%' || $1 || '%'`,
		`SELECT count(*) FROM eacp.action_attempts WHERE to_jsonb(action_attempts)::text LIKE '%' || $1 || '%'`,
		`SELECT count(*) FROM eacp.audit_events WHERE convert_from(payload, 'UTF8') LIKE '%' || $1 || '%'`,
		`SELECT count(*) FROM eacp.outbox_events WHERE to_jsonb(outbox_events)::text LIKE '%' || $1 || '%'`,
	} {
		if n := v.count(sql, canary); n != 0 {
			t.Fatalf("secret stored: %s", sql)
		}
	}
	if got := v.get(a.ID); got.State != "UNKNOWN_OUTCOME" {
		t.Fatalf("a result whose error class carried a secret = %s", got.State)
	}
}

func TestCertifiedNoEffectWithExternalReferenceIsAmbiguous(t *testing.T) {
	v := newEnv(t)
	v.conn.fn = func(context.Context, worker.Call) worker.Result {
		return worker.Result{Outcome: worker.NoEffect, ErrorClass: "validation", ExternalReference: "PO-created"}
	}
	a := v.submit("bank.pay")
	v.runOnce(v.worker("w1"))
	if got := v.get(a.ID); got.State != "UNKNOWN_OUTCOME" {
		t.Fatalf("contradictory connector result state = %s, want UNKNOWN_OUTCOME", got.State)
	}
}

func TestCertifiedNoEffectWithRedactedReferenceIsAmbiguous(t *testing.T) {
	v := newEnv(t)
	v.conn.fn = func(_ context.Context, c worker.Call) worker.Result {
		return worker.Result{Outcome: worker.NoEffect, ErrorClass: "validation", ExternalReference: c.Secret.Reveal()}
	}
	a := v.submit("bank.pay")
	v.runOnce(v.worker("w1"))
	if got := v.get(a.ID); got.State != "UNKNOWN_OUTCOME" {
		t.Fatalf("redacted contradictory result state = %s, want UNKNOWN_OUTCOME", got.State)
	}
}
