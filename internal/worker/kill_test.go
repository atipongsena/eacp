package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/atipongsena/eacp/internal/action"
	"github.com/atipongsena/eacp/internal/storage"
	"github.com/atipongsena/eacp/internal/storage/pgtest"
	"github.com/atipongsena/eacp/internal/worker"
)

func setKillAs(t *testing.T, v *env, operator, scope string, target uuid.UUID, killed bool) int64 {
	t.Helper()
	ctx := context.Background()
	var epoch int64
	err := storage.InTenantTx(ctx, v.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		if err := storage.SetActor(ctx, tx, v.f.P[operator]); err != nil {
			return err
		}
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT eacp.set_kill($1, $2, $3, $4)`, scope, target, killed, "incident containment").Scan(&raw); err != nil {
			return err
		}
		var state struct {
			Epoch int64 `json:"epoch"`
		}
		if err := json.Unmarshal(raw, &state); err != nil {
			return err
		}
		epoch = state.Epoch
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return epoch
}

func setKill(t *testing.T, v *env, scope string, target uuid.UUID, killed bool) int64 {
	return setKillAs(t, v, "otto", scope, target, killed)
}

func TestKillBeforeClaimAndIntent(t *testing.T) {
	v := newEnv(t)
	a := v.submit("ledger.post")
	if epoch := setKill(t, v, "agent_version", v.agent.Version, true); epoch != 1 {
		t.Fatalf("first kill epoch = %d", epoch)
	}
	w := v.worker("kill-claim")
	if n := v.runOnce(w); n != 0 || v.conn.count() != 0 {
		t.Fatalf("killed action: claims=%d calls=%d", n, v.conn.count())
	}
	if got := v.get(a.ID); got.State != "QUEUED" {
		t.Fatalf("state after killed claim = %s", got.State)
	}
	if epoch := setKillAs(t, v, "opal", "agent_version", v.agent.Version, false); epoch != 2 {
		t.Fatalf("resume epoch = %d", epoch)
	}
	l, ok, err := w.Store().Claim(context.Background(), worker.Candidate{TenantID: v.f.Tenant, ActionID: a.ID}, 5*time.Second)
	if err != nil || !ok {
		t.Fatalf("claim = %+v %v %v", l, ok, err)
	}
	if epoch := setKill(t, v, "tool", v.tools["ledger.post"].Tool, true); epoch != 1 {
		t.Fatalf("tool kill epoch = %d", epoch)
	}
	decision, _, err := w.Store().Intent(context.Background(), l, 5*time.Second)
	if err != nil || decision != worker.Killed {
		t.Fatalf("intent under kill = %s, %v", decision, err)
	}
	if n := v.count(`SELECT count(*) FROM eacp.action_attempts WHERE action_id = $1`, a.ID); n != 0 {
		t.Fatalf("attempts after killed intent = %d", n)
	}
	if v.conn.count() != 0 {
		t.Fatal("external call after kill")
	}
}

func TestEveryBoundScopeWithholdsClaim(t *testing.T) {
	v := newEnv(t)
	a := v.submit("ledger.post")
	w := v.worker("kill-scopes")
	for _, tc := range []struct {
		scope  string
		target uuid.UUID
	}{
		{"tenant", v.f.Tenant},
		{"agent", v.agent.Agent},
		{"agent_version", v.agent.Version},
		{"action", a.ID},
		{"connector", v.tools["ledger.post"].Connector},
		{"tool", v.tools["ledger.post"].Tool},
	} {
		t.Run(tc.scope, func(t *testing.T) {
			setKill(t, v, tc.scope, tc.target, true)
			cands, err := w.Store().Claimable(context.Background(), []string{"http"}, v.secrets.Bindings(), 100, nil)
			if err != nil || len(cands) != 0 {
				t.Fatalf("killed claim hint = %v, %v", cands, err)
			}
			if _, ok, err := w.Store().Claim(context.Background(), worker.Candidate{TenantID: v.f.Tenant, ActionID: a.ID}, 5*time.Second); err != nil || ok {
				t.Fatalf("killed raw claim = %v, %v", ok, err)
			}
			setKillAs(t, v, "opal", tc.scope, tc.target, false)
		})
	}
}

func TestTeamKillMatchesTheReleasedOwnerGroup(t *testing.T) {
	v := newEnv(t)
	group := v.f.ID(t, "alice", `INSERT INTO eacp.groups(tenant_id, name, display_name)
		VALUES (eacp.current_tenant_id(), 'purchasing', 'Purchasing') RETURNING id`)
	agent := v.f.ID(t, "erin", `INSERT INTO eacp.agents
		(tenant_id, name, display_name, environment, risk_class, owner_group_id)
		VALUES (eacp.current_tenant_id(), 'group-buyer', 'Group Buyer', 'production', 'high', $1) RETURNING id`, group)
	version := v.f.ID(t, "erin", `INSERT INTO eacp.agent_versions(tenant_id, agent_id, runtime, code_ref)
		VALUES (eacp.current_tenant_id(), $1, 'python', 'git:test') RETURNING id`, agent)
	allow := v.f.ID(t, "erin", `INSERT INTO eacp.agent_allowlists(tenant_id, agent_version_id, tool_ids)
		VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, version, []uuid.UUID{v.tools["ledger.post"].Tool})
	if err := v.f.Exec("rita", `UPDATE eacp.agent_versions SET active_allowlist_id = $1 WHERE id = $2`, allow, version); err != nil {
		t.Fatal(err)
	}
	if err := v.f.Exec("ravi", `UPDATE eacp.agent_versions SET state = 'ACTIVE', state_reason = 'go live' WHERE id = $1`, version); err != nil {
		t.Fatal(err)
	}
	a, err := v.e.Submit(context.Background(), action.Agent(v.f.Tenant, agent, version), action.Submission{
		IdempotencyKey: uuid.NewString(), Subject: "carol@tenant-a.test", Operation: "post", Target: "erp",
		Tool: "ledger.post", ToolSchemaVersion: "1", Resource: "po",
		Payload: json.RawMessage(`{"amount":42,"currency":"THB"}`),
	})
	if err != nil || a.State != "QUEUED" {
		t.Fatalf("submit group-owned action = %+v, %v", a, err)
	}
	setKill(t, v, "team", group, true)
	w := v.worker("team-kill")
	if _, ok, err := w.Store().Claim(context.Background(), worker.Candidate{TenantID: v.f.Tenant, ActionID: a.ID}, 5*time.Second); err != nil || ok {
		t.Fatalf("team-killed claim = %v, %v", ok, err)
	}
}

func TestRawDispatchIntentIsDatabaseFencedByKill(t *testing.T) {
	v := newEnv(t)
	a := v.submit("ledger.post")
	w := v.worker("kill-raw")
	l, ok, err := w.Store().Claim(context.Background(), worker.Candidate{TenantID: v.f.Tenant, ActionID: a.ID}, 5*time.Second)
	if err != nil || !ok {
		t.Fatalf("claim = %+v %v %v", l, ok, err)
	}
	setKill(t, v, "action", a.ID, true)
	err = storage.InTenantTx(context.Background(), v.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		if err := storage.SetWorker(context.Background(), tx, "kill-raw", l.Generation); err != nil {
			return err
		}
		_, err := tx.Exec(context.Background(), `UPDATE eacp.actions SET state = 'EXECUTING',
			attempt_count = attempt_count + 1, leased_until = now() + interval '30 seconds' WHERE id = $1`, a.ID)
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "53300" {
		t.Fatalf("raw T16 under kill = %v", err)
	}
	if n := v.count(`SELECT count(*) FROM eacp.action_attempts WHERE action_id = $1`, a.ID); n != 0 {
		t.Fatalf("attempts after rejected T16 = %d", n)
	}
}

func TestKillAfterIntentBeforeCallDoesNotCallConnector(t *testing.T) {
	v := newEnv(t)
	a := v.submit("ledger.post")
	w := v.worker("kill-after-intent", func(o *worker.Options) {
		o.AfterIntent = func(context.Context, worker.Lease) { setKill(t, v, "action", a.ID, true) }
	})
	if n := v.runOnce(w); n != 1 || v.conn.count() != 0 {
		t.Fatalf("claims = %d, calls = %d", n, v.conn.count())
	}
	if got := v.get(a.ID); got.State != "UNKNOWN_OUTCOME" || got.AttemptCount != 1 {
		t.Fatalf("action after intent kill = %+v", got)
	}
}

func TestCompletionSerializesWithKillAndKeepsOutcomeUnknown(t *testing.T) {
	v := newEnv(t)
	a := v.submit("ledger.post")
	w := v.worker("kill-complete")
	l, ok, err := w.Store().Claim(context.Background(), worker.Candidate{TenantID: v.f.Tenant, ActionID: a.ID}, 5*time.Second)
	if err != nil || !ok {
		t.Fatalf("claim = %+v %v %v", l, ok, err)
	}
	if decision, _, err := w.Store().Intent(context.Background(), l, 5*time.Second); err != nil || decision != worker.Dispatched {
		t.Fatalf("intent = %s, %v", decision, err)
	}
	ready := make(chan struct{})
	release := make(chan struct{})
	killDone := make(chan error, 1)
	go func() {
		killDone <- storage.InTenantTx(context.Background(), v.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
			if err := storage.SetActor(context.Background(), tx, v.f.P["otto"]); err != nil {
				return err
			}
			if _, err := tx.Exec(context.Background(), `SELECT eacp.set_kill('action', $1, true, 'incident')`, a.ID); err != nil {
				return err
			}
			close(ready)
			<-release
			return nil
		})
	}()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("kill transaction did not start")
	}
	completed := make(chan error, 1)
	go func() {
		_, err := w.Store().Complete(context.Background(), l,
			worker.Result{Outcome: worker.Succeeded, ExternalReference: "PO-1"}, 0)
		completed <- err
	}()
	early := false
	select {
	case <-completed:
		early = true
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-killDone; err != nil {
		t.Fatal(err)
	}
	if early {
		t.Fatal("completion passed an uncommitted kill")
	}
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
	if got := v.get(a.ID); got.State != "UNKNOWN_OUTCOME" {
		t.Fatalf("state after racing kill = %s", got.State)
	}
}

func TestKillDuringCallForcesUnknownOutcome(t *testing.T) {
	v := newEnv(t)
	a := v.submit("ledger.post")
	started := make(chan struct{})
	v.conn.fn = func(ctx context.Context, _ worker.Call) worker.Result {
		close(started)
		<-ctx.Done()
		return worker.Result{Outcome: worker.NoEffect, ErrorClass: "safe_no_effect"}
	}
	// A long lease keeps the heartbeat out of the window: only the poll can see the kill.
	w := v.worker("kill-live", func(o *worker.Options) {
		o.Lease = 30 * time.Second
		o.KillPollInterval = 100 * time.Millisecond
	})
	done := make(chan struct{})
	go func() { defer close(done); _, _ = w.RunOnce(context.Background()) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("call did not start")
	}
	setKill(t, v, "action", a.ID, true)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("kill did not cancel the call")
	}
	if got := v.get(a.ID); got.State != "UNKNOWN_OUTCOME" {
		t.Fatalf("state after killed call = %s", got.State)
	}
}

// The kill poll is frequent, so it must only read: the lease is extended
// on its own, slower schedule.
func TestKillPollDoesNotWriteTheActionRow(t *testing.T) {
	v := newEnv(t)
	a := v.submit("ledger.post")
	xmin := `SELECT xmin::text::bigint FROM eacp.actions WHERE id = $1`
	var before, after int
	v.conn.fn = func(context.Context, worker.Call) worker.Result {
		before = v.count(xmin, a.ID)
		time.Sleep(500 * time.Millisecond)
		after = v.count(xmin, a.ID)
		return worker.Result{Outcome: worker.Succeeded, ExternalReference: "PO-1"}
	}
	w := v.worker("kill-poll-read", func(o *worker.Options) {
		o.Lease = 30 * time.Second
		o.KillPollInterval = 20 * time.Millisecond
	})
	if n := v.runOnce(w); n != 1 {
		t.Fatalf("claims = %d", n)
	}
	if before != after {
		t.Fatalf("kill polls rewrote the action row (xmin %d -> %d)", before, after)
	}
	if got := v.get(a.ID); got.State != "SUCCEEDED" {
		t.Fatalf("state = %s", got.State)
	}
}

// ADR-016 §4: an epoch change during the call is unknown even when the
// scope is resumed before the worker looks, and even for another target.
func TestKillResumedDuringCallStillLeavesOutcomeUnknown(t *testing.T) {
	v := newEnv(t)
	a := v.submit("ledger.post")
	v.conn.fn = func(context.Context, worker.Call) worker.Result {
		setKill(t, v, "connector", v.tools["ledger.post"].Connector, true)
		setKillAs(t, v, "opal", "connector", v.tools["ledger.post"].Connector, false)
		return worker.Result{Outcome: worker.Succeeded, ExternalReference: "PO-1"}
	}
	w := v.worker("kill-resumed")
	if n := v.runOnce(w); n != 1 {
		t.Fatalf("claims = %d", n)
	}
	if got := v.get(a.ID); got.State != "UNKNOWN_OUTCOME" {
		t.Fatalf("state after kill and resume during call = %s", got.State)
	}
}

// The dispatch pins the tenant epoch, so a kill that was activated and
// resumed before the intent does not make a later call unknown.
func TestEarlierKillDoesNotTaintALaterDispatch(t *testing.T) {
	v := newEnv(t)
	other := v.submit("ledger.post")
	setKill(t, v, "action", other.ID, true)
	setKillAs(t, v, "opal", "action", other.ID, false)
	w := v.worker("kill-earlier")
	if n := v.runOnce(w); n != 1 {
		t.Fatalf("claims = %d", n)
	}
	if got := v.get(other.ID); got.State != "SUCCEEDED" {
		t.Fatalf("state after an earlier, resumed kill = %s", got.State)
	}
}

func TestKillSignalWakesEveryInFlightDatabaseCheck(t *testing.T) {
	v := newEnv(t)
	a := v.submit("ledger.post")
	started := make(chan struct{})
	v.conn.fn = func(ctx context.Context, _ worker.Call) worker.Result {
		close(started)
		<-ctx.Done()
		return worker.Result{Outcome: worker.Ambiguous}
	}
	// Neither the heartbeat nor the poll fires within the deadline below.
	w := v.worker("kill-signal", func(o *worker.Options) {
		o.Lease = 30 * time.Second
		o.KillPollInterval = 10 * time.Second
	})
	done := make(chan struct{})
	go func() { defer close(done); _, _ = w.RunOnce(context.Background()) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("call did not start")
	}
	setKill(t, v, "action", a.ID, true)
	w.NotifyKill()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("kill signal did not wake the PostgreSQL check")
	}
	if got := v.get(a.ID); got.State != "UNKNOWN_OUTCOME" {
		t.Fatalf("state after signal = %s", got.State)
	}
}

func TestKillWriteRequiresOperatorAndMonotonicEpoch(t *testing.T) {
	v := newEnv(t)
	a := v.submit("ledger.post")
	err := v.f.Exec("carol", `SELECT eacp.set_kill('action', $1, true, 'reason')`, a.ID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("non-operator kill = %v", err)
	}
	if first := setKill(t, v, "action", a.ID, true); first != 1 {
		t.Fatalf("first epoch = %d", first)
	}
	err = v.f.Exec("otto", `SELECT eacp.set_kill('action', $1, false, 'resume')`, a.ID)
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("self resume = %v", err)
	}
	if second := setKillAs(t, v, "opal", "action", a.ID, false); second != 2 {
		t.Fatalf("second epoch = %d", second)
	}
}

func TestKillRejectsDirectWriteAndForeignTarget(t *testing.T) {
	v := newEnv(t)
	foreign := v.f.ForTenant(t, pgtest.TenantB).NewAgent(t, "foreign")
	err := v.f.Exec("otto", `SELECT eacp.set_kill('agent', $1, true, 'incident')`, foreign.Agent)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("foreign target = %v", err)
	}
	err = v.f.Exec("otto", `INSERT INTO eacp.kill_states
		(tenant_id, scope, target_id, killed, epoch, reason, changed_by, killed_by)
		VALUES (eacp.current_tenant_id(), 'tenant', eacp.current_tenant_id(), true, 999,
		'forged', $1, $1)`, v.f.P["otto"])
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("direct kill write = %v", err)
	}
	if n := v.count(`SELECT count(*) FROM eacp.kill_states`); n != 0 {
		t.Fatalf("kill rows after rejected writes = %d", n)
	}
}

func TestKillAndResumeAreJournaledWithActorAndReason(t *testing.T) {
	v := newEnv(t)
	a := v.submit("ledger.post")
	setKill(t, v, "action", a.ID, true)
	setKillAs(t, v, "opal", "action", a.ID, false)
	if n := v.count(`SELECT count(*) FROM eacp.audit_events
		WHERE convert_from(payload, 'UTF8')::jsonb->>'action' IN ('kill.activated', 'kill.resumed')
		AND convert_from(payload, 'UTF8')::jsonb->>'reason' = 'incident containment'`); n != 2 {
		t.Fatalf("kill audit records = %d", n)
	}
	if n := v.count(`SELECT count(*) FROM eacp.outbox_events WHERE topic = 'kill.changed'`); n != 2 {
		t.Fatalf("kill outbox records = %d", n)
	}
}
