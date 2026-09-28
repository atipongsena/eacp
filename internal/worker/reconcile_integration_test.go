package worker_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/action"
	"github.com/atipongsena/eacp/internal/connector"
	"github.com/atipongsena/eacp/internal/fakeerp"
	"github.com/atipongsena/eacp/internal/governance"
	"github.com/atipongsena/eacp/internal/registry/registrytest"
	"github.com/atipongsena/eacp/internal/storage"
	"github.com/atipongsena/eacp/internal/storage/pgtest"
	"github.com/atipongsena/eacp/internal/worker"
)

// Fake ERP contracts with a 100 ms call budget. create_po reads its lookup
// from a strongly consistent log and is natively idempotent: AUTHORITATIVE.
// create_po_eventual may show a record late and only correlates the key:
// BEST_EFFORT, one attempt.
const (
	strongPOContractSQL = `INSERT INTO eacp.tool_contracts
		(tenant_id, tool_id, side_effects, idempotency_mode, idempotency_key_field,
		 reconciliation_lookup, reconciliation_consistency, proof_standard,
		 no_effect_errors, max_attempts, timeout_ms)
		VALUES (eacp.current_tenant_id(), $1, '{IRREVERSIBLE_WRITE,FINANCIAL}', 'native', 'Idempotency-Key',
		 'by_operation_key', 'strong', 'authoritative', '{validation}', 2, 100)
		RETURNING id`
	eventualPOContractSQL = `INSERT INTO eacp.tool_contracts
		(tenant_id, tool_id, side_effects, idempotency_mode, correlation_field,
		 reconciliation_lookup, reconciliation_consistency, proof_standard,
		 no_effect_errors, max_attempts, timeout_ms)
		VALUES (eacp.current_tenant_id(), $1, '{IRREVERSIBLE_WRITE,FINANCIAL}', 'correlation_only', 'external_reference',
		 'by_operation_key', 'eventual', 'best_effort', '{validation}', 1, 100)
		RETURNING id`
)

type erpEnv struct {
	t       *testing.T
	f       *registrytest.Fixture
	srv     *httptest.Server
	secret  string
	secrets *worker.SecretStore
	e       *action.Engine
	agent   registrytest.Agent
	w       *worker.Worker
}

func newERPEnv(t *testing.T) *erpEnv {
	t.Helper()
	return newERPEnvWith(t, registrytest.AllowPolicy)
}

// newERPEnvWith is newERPEnv under the given active policy.
func newERPEnvWith(t *testing.T, policy string) *erpEnv {
	t.Helper()
	v := &erpEnv{t: t, secret: canary + "-erp"}
	h, err := fakeerp.New(v.secret, filepath.Join(t.TempDir(), "erp.log"))
	if err != nil {
		t.Fatal(err)
	}
	v.srv = httptest.NewServer(h)
	t.Cleanup(v.srv.Close)
	u, _ := url.Parse(v.srv.URL)
	v.f = registrytest.New(t)
	conn := v.f.ID(t, "erin", `INSERT INTO eacp.connectors (tenant_id, name, protocol, endpoint, secret_ref)
		VALUES (eacp.current_tenant_id(), 'erp', 'http', $1, 'erp') RETURNING id`, v.srv.URL)
	var tools []uuid.UUID
	for name, contract := range map[string]string{"create_po": strongPOContractSQL, "create_po_eventual": eventualPOContractSQL} {
		tool := v.f.ID(t, "erin", `INSERT INTO eacp.tools (tenant_id, connector_id, name)
			VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, conn, name)
		c := v.f.ID(t, "erin", contract, tool)
		if err := v.f.Exec("rita", `UPDATE eacp.tools SET active_contract_id = $1 WHERE id = $2`, c, tool); err != nil {
			t.Fatal(err)
		}
		tools = append(tools, tool)
	}
	v.agent = v.f.ActiveAgent(t, "buyer", tools...)
	v.f.ActivatePolicy(t, policy)
	v.secrets, err = worker.LoadSecrets(secretsFile(t, fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"erp","host":%q,"value":%q}]}`,
		pgtest.TenantA, u.Host, v.secret)))
	if err != nil {
		t.Fatal(err)
	}
	v.e = action.New(v.f.App, action.Options{Provider: governance.LocalProvider{InstanceID: "reconcile-test"}})
	v.w, err = worker.New(v.f.App, worker.Options{ID: "w1", Lease: 5 * time.Second,
		Connectors: map[string]worker.Connector{"http": connector.NewHTTP()}, Secrets: v.secrets,
		Backoff: func(int) time.Duration { return 100 * time.Millisecond }})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (v *erpEnv) reconciler(maxAttempts int, backoff time.Duration) *worker.Reconciler {
	v.t.Helper()
	r, err := worker.NewReconciler(v.f.App, worker.ReconcilerOptions{ID: "r1", Lease: 5 * time.Second,
		MaxAttempts: maxAttempts, Connectors: map[string]worker.Connector{"http": connector.NewHTTP()},
		Secrets: v.secrets, Backoff: func(int) time.Duration { return backoff }})
	if err != nil {
		v.t.Fatal(err)
	}
	return r
}

// submit creates a QUEUED action on tool (create_po or create_po_eventual)
// whose enforced payload asks Fake ERP for scenario.
func (v *erpEnv) submit(tool string, payload map[string]any) action.View {
	v.t.Helper()
	return v.submitTo("QUEUED", tool, payload)
}

// submitTo submits like submit and expects the action to land in state.
func (v *erpEnv) submitTo(state, tool string, payload map[string]any) action.View {
	v.t.Helper()
	payload["amount"], payload["currency"] = 42, "THB"
	b, _ := json.Marshal(payload)
	got, err := v.e.Submit(context.Background(), action.Agent(v.f.Tenant, v.agent.Agent, v.agent.Version),
		action.Submission{IdempotencyKey: uuid.NewString(), Subject: "carol@tenant-a.test", Operation: "post",
			Target: "erp", Tool: "erp." + tool, ToolSchemaVersion: "1", Resource: "po", Payload: b})
	if err != nil || got.State != state {
		v.t.Fatalf("submit = %+v, err = %v", got, err)
	}
	return got
}

func (v *erpEnv) get(id uuid.UUID) action.View {
	v.t.Helper()
	got, err := v.e.Get(context.Background(), v.f.Tenant, id)
	if err != nil {
		v.t.Fatal(err)
	}
	return got
}

func (v *erpEnv) runWorker(want int) {
	v.t.Helper()
	if n, err := v.w.RunOnce(context.Background()); err != nil || n != want {
		v.t.Fatalf("worker dispatched %d actions, want %d (err %v)", n, want, err)
	}
}

func (v *erpEnv) sweep() {
	v.t.Helper()
	s := action.NewSweeper(v.e)
	s.Grace = 0
	if _, err := s.RunOnce(context.Background()); err != nil {
		v.t.Fatal(err)
	}
}

// reconcile runs r until the action leaves UNKNOWN_OUTCOME and
// RECONCILING, waiting for each scheduled reconciliation; it returns the
// action and the check results it saw.
func (v *erpEnv) reconcile(r *worker.Reconciler, id uuid.UUID) (action.View, []string) {
	v.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		got := v.get(id)
		if got.State != "UNKNOWN_OUTCOME" && got.State != "RECONCILING" {
			return got, v.checks(id)
		}
		if time.Now().After(deadline) {
			v.t.Fatalf("still %s after 15s (checks %v)", got.State, v.checks(id))
		}
		if got.NextReconcileAt != nil {
			time.Sleep(time.Until(*got.NextReconcileAt) + 20*time.Millisecond)
		}
		if _, err := r.RunOnce(context.Background()); err != nil {
			v.t.Fatal(err)
		}
	}
}

func (v *erpEnv) checks(id uuid.UUID) []string {
	v.t.Helper()
	ev, err := v.e.Evidence(context.Background(), v.f.Tenant, id)
	if err != nil {
		v.t.Fatal(err)
	}
	var out []string
	for _, c := range ev.Checks {
		out = append(out, c.Result)
	}
	return out
}

// effects counts the purchase orders Fake ERP committed for key.
func (v *erpEnv) effects(key string) int {
	v.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, v.srv.URL+"/v1/audit", nil)
	req.Header.Set("Authorization", "Bearer "+v.secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		v.t.Fatal(err)
	}
	defer resp.Body.Close()
	var entries []struct {
		OperationKey string `json:"operation_key"`
		Outcome      string `json:"outcome"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		v.t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if e.OperationKey == key && e.Outcome == "effect_committed" {
			n++
		}
	}
	return n
}

// Flagship 1 (MASTER_PLAN §81): Fake ERP executes, the response is lost,
// the action is UNKNOWN_OUTCOME, the lookup finds the record: SUCCEEDED,
// with exactly one ERP record and no second dispatch.
func TestLostResponseIsReconciledToSuccessWithOneRecord(t *testing.T) {
	v := newERPEnv(t)
	for _, scenario := range []string{"execute_then_reset", "execute_then_timeout", "5xx_after_effect"} {
		t.Run(scenario, func(t *testing.T) {
			a := v.submit("create_po", map[string]any{"scenario": scenario, "delay_ms": 300})
			v.runWorker(1)
			if got := v.get(a.ID); got.State != "UNKNOWN_OUTCOME" {
				t.Fatalf("after a lost response = %s (%s)", got.State, got.StateReason)
			}
			v.runWorker(0) // never re-dispatched while unknown (invariant 12)
			got, checks := v.reconcile(v.reconciler(3, 100*time.Millisecond), a.ID)
			if got.State != "SUCCEEDED" || got.ExternalReference != "PO-"+a.ID.String() || fmt.Sprint(checks) != "[found]" {
				t.Fatalf("reconciled = %s %q %v", got.State, got.ExternalReference, checks)
			}
			if n := v.effects(a.OperationKey); n != 1 || got.AttemptCount != 1 {
				t.Fatalf("ERP records = %d after %d attempts", n, got.AttemptCount)
			}
		})
	}
	var leaked int
	ctx := context.Background()
	if err := storage.InTenantTx(ctx, v.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM eacp.reconciliation_checks
			WHERE to_jsonb(reconciliation_checks)::text LIKE '%' || $1 || '%')
			+ (SELECT count(*) FROM eacp.audit_events WHERE convert_from(payload, 'UTF8') LIKE '%' || $1 || '%')`,
			v.secret).Scan(&leaked)
	}); err != nil || leaked != 0 {
		t.Fatalf("credential in reconciliation evidence (count=%d err=%v)", leaked, err)
	}
}

// Flagship 2: delayed visibility under BEST_EFFORT. "Not found" is never
// proof: no retry, and either a human decides or the record shows up.
func TestDelayedVisibilityIsNeverRetried(t *testing.T) {
	v := newERPEnv(t)
	hidden := v.submit("create_po_eventual", map[string]any{"scenario": "execute_then_timeout", "delay_ms": 300,
		"visibility_delay_ms": 5000})
	v.runWorker(1)
	got, checks := v.reconcile(v.reconciler(2, 100*time.Millisecond), hidden.ID)
	if got.State != "NEEDS_HUMAN_RESOLUTION" || fmt.Sprint(checks) != "[absent absent]" {
		t.Fatalf("hidden record = %s (%s) checks %v", got.State, got.StateReason, checks)
	}
	v.sweep()
	v.runWorker(0)
	if n := v.effects(hidden.OperationKey); n != 1 || v.get(hidden.ID).AttemptCount != 1 {
		t.Fatalf("hidden: ERP records = %d", n)
	}

	late := v.submit("create_po_eventual", map[string]any{"scenario": "execute_then_timeout", "delay_ms": 300,
		"visibility_delay_ms": 2500})
	v.runWorker(1)
	got, checks = v.reconcile(v.reconciler(10, 700*time.Millisecond), late.ID)
	if got.State != "SUCCEEDED" || len(checks) < 2 || checks[0] != "absent" || checks[len(checks)-1] != "found" {
		t.Fatalf("late record = %s checks %v", got.State, checks)
	}
	if n := v.effects(late.OperationKey); n != 1 || got.AttemptCount != 1 {
		t.Fatalf("late: ERP records = %d after %d attempts", n, got.AttemptCount)
	}
}

// Flagship 3: a worker killed while EXECUTING. The lease lapses, the
// action is UNKNOWN_OUTCOME and is never re-dispatched blindly; the
// reconciler finds the record, or proves its absence under AUTHORITATIVE
// and retries with the same operation key. Either way: one ERP record.
func TestWorkerKilledWhileExecutingIsReconciledNotRedispatched(t *testing.T) {
	v := newERPEnv(t)
	ctx := context.Background()
	crash := func(a action.View, call bool) {
		t.Helper()
		s := worker.NewStore(v.f.App, "doomed")
		l, ok, err := s.Claim(ctx, worker.Candidate{TenantID: v.f.Tenant, ActionID: a.ID}, 5*time.Second)
		if err != nil || !ok {
			t.Fatalf("claim = %v %v", ok, err)
		}
		if d, _, err := s.Intent(ctx, l, 1200*time.Millisecond); err != nil || d != worker.Dispatched {
			t.Fatalf("intent = %s %v", d, err)
		}
		if call { // the ERP commits, then the worker dies before recording anything
			secret, err := v.secrets.Resolve(v.f.Tenant, "erp", v.srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			res := connector.NewHTTP().Execute(ctx, worker.Call{TenantID: v.f.Tenant, ActionID: a.ID,
				OperationKey: a.OperationKey, Attempt: 1, Generation: l.Generation, Tool: "erp.create_po",
				Endpoint: v.srv.URL, Payload: a.EnforcedPayload, Secret: secret,
				Contract: worker.Contract{IdempotencyMode: "native", IdempotencyKeyField: "Idempotency-Key"}})
			if res.Outcome != worker.Succeeded {
				t.Fatalf("ERP call = %+v", res)
			}
		}
	}
	before := v.submit("create_po", map[string]any{})
	after := v.submit("create_po", map[string]any{})
	crash(before, false)
	crash(after, true)
	time.Sleep(1600 * time.Millisecond) // both leases lapse
	v.sweep()                           // T23
	for _, a := range []action.View{before, after} {
		if got := v.get(a.ID); got.State != "UNKNOWN_OUTCOME" || got.StateReason != "lease expired during the call" {
			t.Fatalf("after the crash = %s (%s)", got.State, got.StateReason)
		}
	}
	v.runWorker(0) // no blind re-dispatch (invariant 12)

	// The call had committed: found.
	got, checks := v.reconcile(v.reconciler(3, 100*time.Millisecond), after.ID)
	if got.State != "SUCCEEDED" || fmt.Sprint(checks) != "[found]" || v.effects(after.OperationKey) != 1 {
		t.Fatalf("committed before the crash = %s %v", got.State, checks)
	}
	// The call never left the worker: authoritative absence permits a retry
	// with the same operation key (T31), dispatched under a new lease.
	got, checks = v.reconcile(v.reconciler(3, 100*time.Millisecond), before.ID)
	if got.State != "RETRY_WAIT" || fmt.Sprint(checks) != "[absent]" || v.effects(before.OperationKey) != 0 {
		t.Fatalf("never dispatched = %s %v", got.State, checks)
	}
	time.Sleep(150 * time.Millisecond)
	v.sweep() // T25
	v.runWorker(1)
	got = v.get(before.ID)
	if got.State != "SUCCEEDED" || got.AttemptCount != 2 || got.OperationKey != before.OperationKey ||
		v.effects(before.OperationKey) != 1 {
		t.Fatalf("retried = %+v, ERP records %d", got, v.effects(before.OperationKey))
	}
}

// AUTHORITATIVE absence is the only road to FAILED after an unknown
// outcome, and only once no retry remains (T32).
func TestAuthoritativeAbsenceFailsOnlyWhenNoRetryRemains(t *testing.T) {
	v := newERPEnv(t)
	a := v.submit("create_po", map[string]any{"scenario": "rate_limit"}) // 429: ambiguous, no effect
	r := v.reconciler(3, 100*time.Millisecond)
	for attempt := 1; attempt <= 2; attempt++ {
		v.runWorker(1)
		if got := v.get(a.ID); got.State != "UNKNOWN_OUTCOME" {
			t.Fatalf("attempt %d = %s", attempt, got.State)
		}
		got, _ := v.reconcile(r, a.ID)
		want := map[int]string{1: "RETRY_WAIT", 2: "FAILED"}[attempt]
		if got.State != want {
			t.Fatalf("attempt %d reconciled = %s (%s)", attempt, got.State, got.StateReason)
		}
		time.Sleep(150 * time.Millisecond)
		v.sweep()
	}
	if got := v.get(a.ID); got.AttemptCount != 2 || v.effects(a.OperationKey) != 0 {
		t.Fatalf("failed = %+v", got)
	}
}

// Flagship 4: conflicting evidence. A correlation-only write whose key
// now names two ERP records (a duplicate sent outside the control plane)
// is never settled by the reconciler: it goes to a human (T34).
func TestConflictingEvidenceGoesToAHuman(t *testing.T) {
	v := newERPEnv(t)
	ctx := context.Background()
	a := v.submit("create_po_eventual", map[string]any{"scenario": "execute_then_timeout", "delay_ms": 300})
	v.runWorker(1)
	if got := v.get(a.ID); got.State != "UNKNOWN_OUTCOME" {
		t.Fatalf("after a lost response = %s", got.State)
	}
	secret, err := v.secrets.Resolve(v.f.Tenant, "erp", v.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	dup := connector.NewHTTP().Execute(ctx, worker.Call{TenantID: v.f.Tenant, ActionID: a.ID,
		OperationKey: a.OperationKey, Attempt: 1, Generation: 1, Tool: "erp.create_po_eventual", Endpoint: v.srv.URL,
		Payload: json.RawMessage(`{"amount":42,"currency":"THB"}`), Secret: secret,
		Contract: worker.Contract{IdempotencyMode: "correlation_only", CorrelationField: "external_reference"}})
	if dup.Outcome != worker.Succeeded {
		t.Fatalf("duplicate = %+v", dup)
	}
	got, checks := v.reconcile(v.reconciler(3, 100*time.Millisecond), a.ID)
	if got.State != "NEEDS_HUMAN_RESOLUTION" || fmt.Sprint(checks) != "[conflict]" || got.ExternalReference != "" {
		t.Fatalf("conflict = %s (%s) checks %v", got.State, got.StateReason, checks)
	}
	if n := v.effects(a.OperationKey); n != 2 || got.AttemptCount != 1 {
		t.Fatalf("ERP records = %d after %d attempts", n, got.AttemptCount)
	}
}

// A configured backoff outside what the database accepts is clamped to
// (100 ms, 1 h]; it never strands an action in RECONCILING.
func TestReconcilerClampsItsBackoff(t *testing.T) {
	v := newERPEnv(t)
	for _, backoff := range []time.Duration{-time.Second, 0, 3 * time.Hour} {
		a := v.submit("create_po_eventual", map[string]any{"scenario": "execute_then_timeout", "delay_ms": 300,
			"visibility_delay_ms": 5000})
		v.runWorker(1)
		got := v.get(a.ID)
		time.Sleep(time.Until(*got.NextReconcileAt) + 20*time.Millisecond)
		if _, err := v.reconciler(3, backoff).RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		got = v.get(a.ID)
		if got.State != "UNKNOWN_OUTCOME" || got.ReconcileAttempts != 1 || got.NextReconcileAt == nil ||
			time.Until(*got.NextReconcileAt) > time.Hour {
			t.Fatalf("backoff %s: %s attempts %d next %v", backoff, got.State, got.ReconcileAttempts, got.NextReconcileAt)
		}
	}
}
