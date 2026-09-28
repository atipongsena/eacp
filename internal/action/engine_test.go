package action_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/action"
	"github.com/atipongsena/eacp/internal/approval"
	"github.com/atipongsena/eacp/internal/governance"
	"github.com/atipongsena/eacp/internal/registry"
	"github.com/atipongsena/eacp/internal/registry/registrytest"
	"github.com/atipongsena/eacp/internal/storage"
	"github.com/atipongsena/eacp/internal/storage/pgtest"
)

const (
	allowAll     = `{"format_version":1,"rules":[{"id":"all","verdict":"allow","reason":"permitted"}]}`
	denyAll      = `{"format_version":1,"rules":[{"id":"all","verdict":"deny","reason":"forbidden"}]}`
	noRules      = `{"format_version":1,"rules":[{"id":"other","match":{"operation":"refund"},"verdict":"allow","reason":"refunds"}]}`
	transformAll = `{"format_version":1,"rules":[{"id":"cap","verdict":"transform","reason":"capped","set":{"amount":1000}}]}`
	escalateAll  = `{"format_version":1,"rules":[{"id":"review","verdict":"escalate","reason":"needs review","approval":{"quorum":2,"eligible_roles":["approver"],"ttl_seconds":600}}]}`
	escalateCap  = `{"format_version":1,"rules":[{"id":"review","verdict":"escalate","reason":"capped review","set":{"amount":1000},"approval":{"quorum":2,"eligible_roles":["approver"],"ttl_seconds":600}}]}`
	payload      = `{"currency":"THB","amount":2400000}`
)

// provider is a governance provider whose behaviour a test can switch.
type provider struct {
	mode  atomic.Value // "", "down", "malformed", "digest"
	calls atomic.Int64
}

func (p *provider) set(mode string) { p.mode.Store(mode) }

func (p *provider) Evaluate(ctx context.Context, req governance.GovernanceRequest) (governance.GovernanceDecision, error) {
	p.calls.Add(1)
	mode, _ := p.mode.Load().(string)
	if mode == "down" {
		return governance.GovernanceDecision{}, errors.New("pdp unreachable")
	}
	d, err := governance.LocalProvider{InstanceID: "local-test"}.Evaluate(ctx, req)
	switch mode {
	case "malformed":
		d.DecisionID = uuid.Nil
	case "digest":
		d.EnforcedDigest[0] ^= 0xff
	}
	return d, err
}

type env struct {
	t     *testing.T
	f     *registrytest.Fixture
	tool  registrytest.Tooling
	agent registrytest.Agent
	e     *action.Engine
	pdp   *provider
	logs  *bytes.Buffer
	mu    *sync.Mutex
}

func newEnv(t *testing.T, policy string, opts ...func(*action.Options)) env {
	t.Helper()
	return newEnvWith(t, policy, registrytest.SafeContractSQL, opts...)
}

// newEnvWith is newEnv with contractSQL as erp.purchase's active contract.
func newEnvWith(t *testing.T, policy, contractSQL string, opts ...func(*action.Options)) env {
	t.Helper()
	f := registrytest.New(t)
	tool := f.ActiveToolWith(t, "erp", "purchase", contractSQL)
	agent := f.ActiveAgent(t, "buyer", tool.Tool)
	v := env{t: t, f: f, tool: tool, agent: agent, pdp: &provider{}, logs: &bytes.Buffer{}, mu: &sync.Mutex{}}
	v.activate(policy)
	o := action.Options{
		Provider: v.pdp,
		Log:      slog.New(slog.NewJSONHandler(lockedWriter{v.mu, v.logs}, nil)),
		Limits:   action.Limits{MaxQueuedPerTenant: 1000, MaxQueuedGlobal: 1000},
	}
	for _, fn := range opts {
		fn(&o)
	}
	v.e = action.New(f.App, o)
	return v
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func (v env) logged() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.logs.String()
}

// activate creates a policy version (alice) and activates it (bob).
func (v env) activate(policy string) uuid.UUID {
	v.t.Helper()
	id := v.f.ID(v.t, "alice", `INSERT INTO eacp.policy_bundles (tenant_id, content)
		VALUES (eacp.current_tenant_id(), $1::jsonb) RETURNING id`, policy)
	if err := v.f.Exec("bob", `UPDATE eacp.tenant_policy_pointer SET current_bundle_id = $1,
		activation_reason = 'test' WHERE tenant_id = eacp.current_tenant_id()`, id); err != nil {
		v.t.Fatal(err)
	}
	return id
}

func (v env) actor() action.Actor {
	return action.Agent(v.f.Tenant, v.agent.Agent, v.agent.Version)
}

func (v env) submission(key string) action.Submission {
	return action.Submission{
		IdempotencyKey: key, Subject: "carol@tenant-a.test", Operation: "purchase", Target: "erp",
		Tool: "erp.purchase", ToolSchemaVersion: "1", Resource: "po", Payload: json.RawMessage(payload),
	}
}

func (v env) submit(key string) (action.View, error) {
	return v.e.Submit(context.Background(), v.actor(), v.submission(key))
}

func (v env) mustSubmit(key, want string) action.View {
	v.t.Helper()
	got, err := v.submit(key)
	if err != nil {
		v.t.Fatalf("submit %s: %v", key, err)
	}
	if got.State != want {
		v.t.Fatalf("submit %s state = %s (%s), want %s", key, got.State, got.StateReason, want)
	}
	return got
}

func (v env) advance(id uuid.UUID) (action.View, error) {
	return v.e.Advance(context.Background(), v.actor(), id)
}

func (v env) get(id uuid.UUID) action.View {
	v.t.Helper()
	got, err := v.e.Get(context.Background(), v.f.Tenant, id)
	if err != nil {
		v.t.Fatal(err)
	}
	return got
}

func (v env) count(sql string, args ...any) int {
	v.t.Helper()
	var n int
	err := storage.InTenantTx(context.Background(), v.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), sql, args...).Scan(&n)
	})
	if err != nil {
		v.t.Fatal(err)
	}
	return n
}

func (v env) approve(request uuid.UUID) {
	v.t.Helper()
	svc := approval.New(v.f.App)
	for _, name := range []string{"amy", "ben"} {
		if _, err := svc.Vote(context.Background(), registry.Actor{TenantID: v.f.Tenant, PrincipalID: v.f.P[name]},
			request, approval.Approve, "reviewed"); err != nil {
			v.t.Fatal(err)
		}
	}
}

func (v env) requestState(id uuid.UUID) string {
	v.t.Helper()
	var state string
	err := storage.InTenantTx(context.Background(), v.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT state FROM eacp.approval_requests WHERE id = $1`, id).Scan(&state)
	})
	if err != nil {
		v.t.Fatal(err)
	}
	return state
}

func TestAllowReleasesWithPinnedPolicyAndContract(t *testing.T) {
	v := newEnv(t, allowAll)
	got := v.mustSubmit("k1", "QUEUED")
	if got.PolicyVersion == nil || *got.PolicyVersion != 1 || got.ConnectorContractVersion == nil ||
		*got.ConnectorContractVersion != 1 || got.ReleasedAt == nil {
		t.Fatalf("pins = %+v", got)
	}
	if string(got.EnforcedPayload) != `{"amount":2400000,"currency":"THB"}` || got.EnforcedDigest == "" {
		t.Fatalf("enforced payload = %s digest %s", got.EnforcedPayload, got.EnforcedDigest)
	}
	if got.OperationKey != "eacp:"+pgtest.TenantA+":"+got.ID.String() {
		t.Fatalf("operation key = %s", got.OperationKey)
	}
	// Submission and release are two evaluations with durable evidence.
	if n := v.count(`SELECT count(*) FROM eacp.decision_evidence WHERE action_id = $1`, got.ID); n != 2 {
		t.Fatalf("evidence rows = %d, want 2", n)
	}
	if n := v.count(`SELECT count(*) FROM eacp.outbox_events WHERE aggregate_id = $1 AND topic = 'action.queued'`, got.ID); n != 1 {
		t.Fatalf("outbox rows = %d, want 1", n)
	}
}

func TestTransformPersistsEnforcedPayloadAndDigest(t *testing.T) {
	v := newEnv(t, transformAll)
	got := v.mustSubmit("k1", "QUEUED")
	if string(got.EnforcedPayload) != `{"amount":1000,"currency":"THB"}` || got.EnforcedDigest == got.InputDigest {
		t.Fatalf("transform = %s %s/%s", got.EnforcedPayload, got.InputDigest, got.EnforcedDigest)
	}
}

func TestDenyAndDefaultDenyAreTerminalWithEvidence(t *testing.T) {
	for name, policy := range map[string]string{"deny": denyAll, "no rule": noRules} {
		t.Run(name, func(t *testing.T) {
			v := newEnv(t, policy)
			got := v.mustSubmit("k1", "DENIED")
			if got.StateReason == "" {
				t.Fatal("denial without reason")
			}
			if n := v.count(`SELECT count(*) FROM eacp.decision_evidence WHERE action_id = $1 AND verdict = 'deny'`, got.ID); n != 1 {
				t.Fatalf("deny evidence = %d", n)
			}
		})
	}
}

func TestCapabilityAndSubjectDenialsLeaveAnAuditableAction(t *testing.T) {
	v := newEnv(t, allowAll)
	v.f.ActiveTool(t, "crm", "export")
	cases := map[string]func(*action.Submission){
		"subject_invalid":       func(s *action.Submission) { s.Subject = "mallory@tenant-a.test" },
		"unknown_tool":          func(s *action.Submission) { s.Tool = "erp.missing" },
		"tool_not_in_allowlist": func(s *action.Submission) { s.Tool = "crm.export" },
	}
	for reason, mutate := range cases {
		s := v.submission("k-" + reason)
		mutate(&s)
		got, err := v.e.Submit(context.Background(), v.actor(), s)
		if err != nil || got.State != "DENIED" || got.StateReason != reason {
			t.Fatalf("%s: %+v, err = %v", reason, got, err)
		}
		if n := v.count(`SELECT count(*) FROM eacp.decision_evidence WHERE action_id = $1`, got.ID); n != 0 {
			t.Fatalf("%s: capability denial consulted governance (%d evidence rows)", reason, n)
		}
	}
	if err := v.f.Exec("otto", `UPDATE eacp.agent_versions SET state = 'SUSPENDED', state_reason = 'incident'
		WHERE id = $1`, v.agent.Version); err != nil {
		t.Fatal(err)
	}
	got, err := v.submit("k-suspended")
	if err != nil || got.State != "DENIED" || got.StateReason != "agent_version_not_active" {
		t.Fatalf("suspended: %+v, err = %v", got, err)
	}
}

func TestGovernanceOutageKeepsActionReceivedUntilResubmission(t *testing.T) {
	v := newEnv(t, allowAll)
	v.pdp.set("down")
	got, err := v.submit("k1")
	if !errors.Is(err, action.ErrGovernanceUnavailable) || got.State != "RECEIVED" || got.ID == uuid.Nil {
		t.Fatalf("outage: %+v, err = %v", got, err)
	}
	if n := v.count(`SELECT count(*) FROM eacp.decision_evidence WHERE action_id = $1`, got.ID); n != 0 {
		t.Fatalf("outage recorded %d evidence rows", n)
	}
	v.pdp.set("")
	again := v.mustSubmit("k1", "QUEUED")
	if again.ID != got.ID {
		t.Fatalf("resubmission created a new action: %s vs %s", again.ID, got.ID)
	}
}

func TestMalformedDecisionStaysReceivedAndAlerts(t *testing.T) {
	v := newEnv(t, allowAll)
	v.pdp.set("malformed")
	got, err := v.submit("k1")
	if !errors.Is(err, action.ErrGovernanceUnavailable) || got.State != "RECEIVED" {
		t.Fatalf("malformed: %+v, err = %v", got, err)
	}
	if !strings.Contains(v.logged(), `"alert":"governance.malformed_decision"`) {
		t.Fatalf("no alert logged: %s", v.logged())
	}
}

func TestDigestMismatchDeniesAndAlerts(t *testing.T) {
	v := newEnv(t, allowAll)
	v.pdp.set("digest")
	got := v.mustSubmit("k1", "DENIED")
	if got.StateReason != "digest_mismatch" || !strings.Contains(v.logged(), `"alert":"governance.digest_mismatch"`) {
		t.Fatalf("digest mismatch: %+v; logs %s", got, v.logged())
	}
}

func TestIdempotentReplayAndConflict(t *testing.T) {
	v := newEnv(t, allowAll)
	first := v.mustSubmit("k1", "QUEUED")
	replay := v.mustSubmit("k1", "QUEUED")
	if replay.ID != first.ID {
		t.Fatalf("replay created %s, want %s", replay.ID, first.ID)
	}
	changed := v.submission("k1")
	changed.Payload = json.RawMessage(`{"currency":"THB","amount":24000000}`)
	if _, err := v.e.Submit(context.Background(), v.actor(), changed); !errors.Is(err, action.ErrIdempotencyConflict) {
		t.Fatalf("changed payload = %v, want ErrIdempotencyConflict", err)
	}
	// Key order and whitespace do not change the request identity.
	same := v.submission("k1")
	same.Payload = json.RawMessage(`{ "amount": 2400000, "currency": "THB" }`)
	if got, err := v.e.Submit(context.Background(), v.actor(), same); err != nil || got.ID != first.ID {
		t.Fatalf("canonical replay = %+v, err = %v", got, err)
	}
	if n := v.count(`SELECT count(*) FROM eacp.actions`); n != 1 {
		t.Fatalf("actions = %d", n)
	}
}

func TestConcurrentSubmissionsWithOneKeyCreateOneAction(t *testing.T) {
	v := newEnv(t, allowAll)
	var wg sync.WaitGroup
	ids := make(chan uuid.UUID, 8)
	for range 8 {
		wg.Go(func() {
			got, err := v.submit("same-key")
			if err != nil {
				t.Errorf("submit: %v", err)
				return
			}
			ids <- got.ID
		})
	}
	wg.Wait()
	close(ids)
	var first uuid.UUID
	for id := range ids {
		if first == uuid.Nil {
			first = id
		} else if id != first {
			t.Fatalf("different actions for one key: %s %s", first, id)
		}
	}
	if n := v.count(`SELECT count(*) FROM eacp.actions`); n != 1 {
		t.Fatalf("actions = %d", n)
	}
	if n := v.count(`SELECT count(*) FROM eacp.outbox_events WHERE topic = 'action.queued'`); n != 1 {
		t.Fatalf("outbox rows = %d", n)
	}
}

func TestAdmissionLimitRejectsWithoutCreatingAnAction(t *testing.T) {
	for name, limits := range map[string]action.Limits{
		"tenant": {MaxQueuedPerTenant: 1, MaxQueuedGlobal: 100},
		"global": {MaxQueuedPerTenant: 100, MaxQueuedGlobal: 1},
	} {
		t.Run(name, func(t *testing.T) {
			v := newEnv(t, allowAll, func(o *action.Options) { o.Limits = limits })
			v.mustSubmit("k1", "QUEUED")
			if _, err := v.submit("k2"); !errors.Is(err, action.ErrAdmission) {
				t.Fatalf("over limit = %v, want ErrAdmission", err)
			}
			if n := v.count(`SELECT count(*) FROM eacp.actions`); n != 1 {
				t.Fatalf("rejected submission created an action (%d)", n)
			}
			v.mustSubmit("k1", "QUEUED") // a replay is never rejected by admission
		})
	}
}

func TestApprovalThenParallelReleasesConsumeTheGrantOnce(t *testing.T) {
	v := newEnv(t, escalateAll)
	got := v.mustSubmit("k1", "PENDING_APPROVAL")
	if got.ApprovalRequestID == nil {
		t.Fatal("no approval request")
	}
	v.approve(*got.ApprovalRequestID)
	if st := v.get(got.ID).State; st != "AUTHORIZED" {
		t.Fatalf("after quorum state = %s", st)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := v.advance(got.ID); err != nil {
				t.Errorf("advance: %v", err)
			}
		})
	}
	wg.Wait()
	if st := v.get(got.ID).State; st != "QUEUED" {
		t.Fatalf("state = %s", st)
	}
	if n := v.count(`SELECT count(*) FROM eacp.approval_grants WHERE consumed_by_action_id = $1`, got.ID); n != 1 {
		t.Fatalf("consumed grants = %d", n)
	}
	if n := v.count(`SELECT count(*) FROM eacp.outbox_events WHERE aggregate_id = $1 AND topic = 'action.queued'`, got.ID); n != 1 {
		t.Fatalf("outbox rows = %d", n)
	}
}

func TestTransformThenApproveBindsTheEnforcedPayload(t *testing.T) {
	v := newEnv(t, escalateCap)
	got := v.mustSubmit("k1", "PENDING_APPROVAL")
	view, err := approval.New(v.f.App).GetEligible(context.Background(),
		registry.Actor{TenantID: v.f.Tenant, PrincipalID: v.f.P["amy"]}, *got.ApprovalRequestID)
	if err != nil {
		t.Fatal(err)
	}
	var shown map[string]any
	if err := json.Unmarshal(view.EnforcedPayload, &shown); err != nil || shown["amount"] != float64(1000) ||
		view.EnforcedDigest != got.EnforcedDigest {
		t.Fatalf("approver sees %s %s, action %s", view.EnforcedPayload, view.EnforcedDigest, got.EnforcedDigest)
	}
	v.approve(*got.ApprovalRequestID)
	released, err := v.advance(got.ID)
	if err != nil || released.State != "QUEUED" || string(released.EnforcedPayload) != `{"amount":1000,"currency":"THB"}` {
		t.Fatalf("released = %+v, err = %v", released, err)
	}
}

func TestPolicyChangeAfterGrant(t *testing.T) {
	t.Run("still escalates: new request, old one voided", func(t *testing.T) {
		v := newEnv(t, escalateAll)
		got := v.mustSubmit("k1", "PENDING_APPROVAL")
		old := *got.ApprovalRequestID
		v.approve(old)
		v.activate(escalateAll)
		again, err := v.advance(got.ID)
		if err != nil || again.State != "PENDING_APPROVAL" || again.ApprovalRequestID == nil ||
			*again.ApprovalRequestID == old || *again.PolicyVersion != 2 {
			t.Fatalf("re-approval = %+v, err = %v", again, err)
		}
		if st := v.requestState(old); st != "VOIDED" {
			t.Fatalf("old request = %s", st)
		}
		if n := v.count(`SELECT count(*) FROM eacp.approval_grants WHERE action_id = $1
			AND consumed_at IS NULL AND expires_at > now()`, got.ID); n != 0 {
			t.Fatalf("usable old grants = %d", n)
		}
	})
	t.Run("now allows: released, old approval voided", func(t *testing.T) {
		v := newEnv(t, escalateAll)
		got := v.mustSubmit("k1", "PENDING_APPROVAL")
		v.approve(*got.ApprovalRequestID)
		v.activate(allowAll)
		again, err := v.advance(got.ID)
		if err != nil || again.State != "QUEUED" || *again.PolicyVersion != 2 {
			t.Fatalf("release = %+v, err = %v", again, err)
		}
		if st := v.requestState(*got.ApprovalRequestID); st != "VOIDED" {
			t.Fatalf("old request = %s", st)
		}
		if n := v.count(`SELECT count(*) FROM eacp.approval_grants WHERE consumed_at IS NOT NULL`); n != 0 {
			t.Fatalf("a stale grant was consumed")
		}
	})
	t.Run("now denies", func(t *testing.T) {
		v := newEnv(t, escalateAll)
		got := v.mustSubmit("k1", "PENDING_APPROVAL")
		v.approve(*got.ApprovalRequestID)
		v.activate(denyAll)
		again, err := v.advance(got.ID)
		if err != nil || again.State != "DENIED" {
			t.Fatalf("release = %+v, err = %v", again, err)
		}
	})
}

func TestActivationBetweenRevalidationAndReleaseIsDetected(t *testing.T) {
	var once sync.Once
	var v env
	v = newEnv(t, allowAll, func(o *action.Options) {
		o.BeforeRelease = func(context.Context) {
			once.Do(func() { v.activate(transformAll) })
		}
	})
	// The first revalidation ran under v1, but v2 was active when the release
	// transaction locked the pointer, so R0 ran again under v2, whose
	// transform changes the enforced digest: the action is denied rather
	// than released under a policy that is no longer current.
	got := v.mustSubmit("k1", "DENIED")
	if got.StateReason != "digest_changed" || *got.PolicyVersion != 2 {
		t.Fatalf("final = %+v", got)
	}
}

func TestActivationWaitsForTheReleaseTransaction(t *testing.T) {
	var v env
	var v2 uuid.UUID
	done := make(chan error, 1)
	v = newEnv(t, allowAll, func(o *action.Options) {
		o.InRelease = func(ctx context.Context, tx pgx.Tx) error {
			go func() {
				done <- v.f.Exec("bob", `UPDATE eacp.tenant_policy_pointer SET current_bundle_id = $1,
					activation_reason = 'race' WHERE tenant_id = eacp.current_tenant_id()`, v2)
			}()
			select {
			case err := <-done:
				t.Errorf("activation finished inside the release transaction: %v", err)
			case <-time.After(250 * time.Millisecond):
			}
			return nil
		}
	})
	v2 = v.f.ID(t, "alice", `INSERT INTO eacp.policy_bundles (tenant_id, content)
		VALUES (eacp.current_tenant_id(), $1::jsonb) RETURNING id`, denyAll)
	got := v.mustSubmit("k1", "QUEUED")
	if *got.PolicyVersion != 1 {
		t.Fatalf("released under v%d", *got.PolicyVersion)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("activation never completed")
	}
}

func TestRegistryDriftBeforeReleaseDenies(t *testing.T) {
	v := newEnv(t, escalateAll)
	got := v.mustSubmit("k1", "PENDING_APPROVAL")
	v.approve(*got.ApprovalRequestID)
	if err := v.f.Exec("otto", `UPDATE eacp.tool_contracts SET revoked_at = now(), revoke_reason = 'unsafe'
		WHERE id = $1`, v.tool.Contract); err != nil {
		t.Fatal(err)
	}
	again, err := v.advance(got.ID)
	if err != nil || again.State != "DENIED" || again.StateReason != "contract_revoked" {
		t.Fatalf("drift = %+v, err = %v", again, err)
	}
	if n := v.count(`SELECT count(*) FROM eacp.approval_grants WHERE consumed_at IS NOT NULL`); n != 0 {
		t.Fatal("grant consumed for a denied action")
	}
	if st := v.requestState(*got.ApprovalRequestID); st != "VOIDED" {
		t.Fatalf("request = %s", st)
	}
}

func TestFailureInsideReleaseLeavesGrantUnconsumed(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	v := newEnv(t, escalateAll, func(o *action.Options) {
		o.InRelease = func(context.Context, pgx.Tx) error {
			if fail.Load() {
				return errors.New("crash inside R1")
			}
			return nil
		}
	})
	got := v.mustSubmit("k1", "PENDING_APPROVAL")
	v.approve(*got.ApprovalRequestID)
	if _, err := v.advance(got.ID); err == nil {
		t.Fatal("advance succeeded despite the injected failure")
	}
	if st := v.get(got.ID).State; st != "AUTHORIZED" {
		t.Fatalf("state after failed release = %s", st)
	}
	if n := v.count(`SELECT count(*) FROM eacp.approval_grants WHERE consumed_at IS NULL`); n != 1 {
		t.Fatalf("unconsumed grants = %d", n)
	}
	fail.Store(false)
	if again, err := v.advance(got.ID); err != nil || again.State != "QUEUED" {
		t.Fatalf("retry = %+v, err = %v", again, err)
	}
}

func TestExpiredGrantExpiresTheAction(t *testing.T) {
	v := newEnv(t, escalateAll)
	got := v.mustSubmit("k1", "PENDING_APPROVAL")
	v.approve(*got.ApprovalRequestID)
	if err := v.f.Exec("carol", `UPDATE eacp.approval_requests SET expires_at = now() - interval '1 second'
		WHERE id = $1`, *got.ApprovalRequestID); err != nil {
		t.Fatal(err)
	}
	again, err := v.advance(got.ID)
	if err != nil || again.State != "EXPIRED" {
		t.Fatalf("expired grant = %+v, err = %v", again, err)
	}
	if n := v.count(`SELECT count(*) FROM eacp.approval_grants WHERE consumed_at IS NOT NULL`); n != 0 {
		t.Fatal("expired grant consumed")
	}
}

func TestEvidenceIsReconstructibleFromTheAction(t *testing.T) {
	v := newEnv(t, escalateAll)
	got := v.mustSubmit("k1", "PENDING_APPROVAL")
	v.approve(*got.ApprovalRequestID)
	if _, err := v.advance(got.ID); err != nil {
		t.Fatal(err)
	}
	err := storage.InTenantTx(context.Background(), v.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		ctx := context.Background()
		var verdicts []string
		rows, err := tx.Query(ctx, `SELECT e.verdict FROM eacp.decision_evidence e
			WHERE e.action_id = $1 ORDER BY e.recorded_at, e.id`, got.ID)
		if err != nil {
			return err
		}
		verdicts, err = pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		var votes, consumed int
		if err := tx.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM eacp.approval_votes v JOIN eacp.approval_requests r ON r.tenant_id = v.tenant_id
			   AND r.id = v.request_id WHERE r.action_id = $1),
			(SELECT count(*) FROM eacp.approval_grants WHERE consumed_by_action_id = $1)`, got.ID).
			Scan(&votes, &consumed); err != nil {
			return err
		}
		var moves []string
		rows, err = tx.Query(ctx, `SELECT convert_from(payload, 'UTF8')::jsonb #>> '{data,to}' FROM eacp.audit_events
			WHERE convert_from(payload, 'UTF8')::jsonb #>> '{subject,id}' = $1 ORDER BY seq`, got.ID.String())
		if err != nil {
			return err
		}
		moves, err = pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		if strings.Join(verdicts, ",") != "escalate,escalate" || votes != 2 || consumed != 1 ||
			strings.Join(moves, ",") != "RECEIVED,PENDING_APPROVAL,AUTHORIZED,QUEUED" {
			t.Errorf("evidence %v votes %d consumed %d journal %v", verdicts, votes, consumed, moves)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSubmissionValidation(t *testing.T) {
	v := newEnv(t, allowAll)
	for name, mutate := range map[string]func(*action.Submission){
		"key":       func(s *action.Submission) { s.IdempotencyKey = "" },
		"key space": func(s *action.Submission) { s.IdempotencyKey = "a b" },
		"subject":   func(s *action.Submission) { s.Subject = "" },
		"payload":   func(s *action.Submission) { s.Payload = json.RawMessage(`{"a":1,"a":2}`) },
		"lifetime":  func(s *action.Submission) { s.Lifetime = 25 * time.Hour },
	} {
		s := v.submission("k-" + name)
		mutate(&s)
		if _, err := v.e.Submit(context.Background(), v.actor(), s); !errors.Is(err, registry.ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if n := v.count(`SELECT count(*) FROM eacp.actions`); n != 0 {
		t.Fatalf("invalid submissions created %d actions", n)
	}
}

// blockingPDP answers only when released, so a test can act while a
// decision is in flight.
type blockingPDP struct {
	provider
	entered, release chan struct{}
}

func (p *blockingPDP) Evaluate(ctx context.Context, req governance.GovernanceRequest) (governance.GovernanceDecision, error) {
	p.entered <- struct{}{}
	<-p.release
	return p.provider.Evaluate(ctx, req)
}

// Invariant 18: governance never blocks cancellation. An action waiting on
// the PDP (or stuck RECEIVED during an outage) is cancelled at once (T5a),
// and the decision that arrives afterwards changes nothing: no evidence is
// recorded and the action stays CANCELLED.
func TestCancelWinsOverAnInFlightDecision(t *testing.T) {
	v := newEnv(t, allowAll)
	pdp := &blockingPDP{entered: make(chan struct{}), release: make(chan struct{})}
	e := action.New(v.f.App, action.Options{Provider: pdp})
	type result struct {
		view action.View
		err  error
	}
	done := make(chan result)
	go func() {
		got, err := e.Submit(context.Background(), v.actor(), v.submission("k1"))
		done <- result{got, err}
	}()
	<-pdp.entered
	var id uuid.UUID
	if err := storage.InTenantTx(context.Background(), v.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT id FROM eacp.actions WHERE idempotency_key = 'k1'`).Scan(&id)
	}); err != nil {
		t.Fatal(err)
	}
	got, err := e.Cancel(context.Background(), v.actor(), id, "no longer needed")
	if err != nil || got.State != "CANCELLED" {
		t.Fatalf("cancel while RECEIVED = %+v, %v", got, err)
	}
	close(pdp.release)
	r := <-done
	if r.err != nil || r.view.State != "CANCELLED" {
		t.Fatalf("submission = %+v, %v", r.view, r.err)
	}
	if n := v.count(`SELECT count(*) FROM eacp.decision_evidence WHERE action_id = $1`, id); n != 0 {
		t.Fatalf("a late decision recorded %d evidence rows", n)
	}
}
