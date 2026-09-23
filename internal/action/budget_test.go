package action_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"eacp/internal/action"
	"eacp/internal/budget"
	"eacp/internal/registry"
	"eacp/internal/registry/registrytest"
	"eacp/internal/worker"
)

// The env payload costs 2 400 000 THB under registrytest.CostedContractSQL.
const cost = 2400000

type releaseStartKey struct{}

// newBudgetEnv is newEnv with a costed erp.purchase contract and, unless
// limit is empty, a THB account for the agent with that hard limit.
func newBudgetEnv(t *testing.T, policy, limit string) (env, uuid.UUID) {
	t.Helper()
	v := newEnvWith(t, policy, registrytest.CostedContractSQL)
	var account uuid.UUID
	if limit != "" {
		account = v.f.FundAgent(t, v.agent.Agent, "THB", limit)
	}
	return v, account
}

func (v env) account(id uuid.UUID) budget.Account {
	v.t.Helper()
	a, err := budget.New(v.f.App).Get(context.Background(),
		registry.Actor{TenantID: v.f.Tenant, PrincipalID: v.f.P["audra"]}, id)
	if err != nil {
		v.t.Fatal(err)
	}
	return a
}

// reservation is the state of the action's latest reservation, as its
// evidence shows it.
func (v env) reservation(id uuid.UUID) string {
	v.t.Helper()
	ev, err := v.e.Evidence(context.Background(), v.f.Tenant, id)
	if err != nil {
		v.t.Fatal(err)
	}
	if len(ev.Budget) == 0 {
		return "none"
	}
	r := ev.Budget[len(ev.Budget)-1]
	return fmt.Sprintf("%s %s/%s", r.State, r.Amount, r.CommittedAmount)
}

// Invariant 3 (MASTER_PLAN §103, [B]): hard budgets cannot oversubscribe.
// 100 concurrent submissions against one agent leaf that holds exactly 37
// costs: exactly 37 are released, the other 63 are denied for budget, and
// the leaf ends exactly at its limit.
func TestConcurrentReleasesNeverOversubscribeAHardBudget(t *testing.T) {
	const n, fits = 100, 37
	v := newEnvWith(t, allowAll, registrytest.CostedContractSQL, func(o *action.Options) {
		o.BeforeRelease = func(ctx context.Context) {
			*ctx.Value(releaseStartKey{}).(*time.Time) = time.Now()
		}
	})
	account := v.f.FundAgent(t, v.agent.Agent, "THB", fmt.Sprint(cost*fits))
	latency := make([]time.Duration, n)
	states := make([]string, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			var began time.Time
			ctx := context.WithValue(context.Background(), releaseStartKey{}, &began)
			got, err := v.e.Submit(ctx, v.actor(), v.submission(fmt.Sprintf("k%03d", i)))
			if began.IsZero() {
				t.Errorf("submit %d never entered the release boundary: %v", i, err)
				return
			}
			// The response includes one final Get after R1 commits. This is
			// therefore a conservative upper bound on release latency.
			latency[i] = time.Since(began)
			if err != nil {
				t.Errorf("submit %d: %v", i, err)
				return
			}
			states[i] = got.State + " " + got.StateReason
		})
	}
	close(start)
	wg.Wait()

	counts := map[string]int{}
	for _, s := range states {
		counts[s]++
	}
	if counts["QUEUED permitted"] != fits || counts["DENIED "+action.BudgetExceeded] != n-fits {
		t.Fatalf("outcomes = %v, want %d queued and %d denied for budget", counts, fits, n-fits)
	}
	a := v.account(account)
	if a.Reserved != json.Number(fmt.Sprint(cost*fits)) || a.Available != "0" || a.Committed != "0" {
		t.Fatalf("account = %+v", a)
	}
	if got := v.count(`SELECT count(*) FROM eacp.budget_reservations WHERE state = 'ACTIVE'`); got != fits {
		t.Fatalf("ACTIVE reservations = %d", got)
	}
	if got := v.count(`SELECT count(*) FROM eacp.audit_events
		WHERE convert_from(payload, 'UTF8')::jsonb ->> 'action' = 'budget.reserved'`); got != fits {
		t.Fatalf("journaled reservations = %d", got)
	}
	slices.Sort(latency)
	t.Logf("%d concurrent releases on one budget leaf (R1 through final Get, upper bound): p50 %v, p99 %v, max %v",
		n, latency[n/2], latency[n*99/100-1], latency[n-1])
}

func TestABudgetDenialNeverSpendsTheApproval(t *testing.T) {
	v, _ := newBudgetEnv(t, escalateAll, fmt.Sprint(cost-1))
	got := v.mustSubmit("k1", "PENDING_APPROVAL")
	v.approve(*got.ApprovalRequestID)
	after, err := v.advance(got.ID)
	if err != nil || after.State != "DENIED" || after.StateReason != action.BudgetExceeded {
		t.Fatalf("release = %+v %v", after, err)
	}
	if n := v.count(`SELECT count(*) FROM eacp.approval_grants WHERE action_id = $1 AND consumed_at IS NOT NULL`, got.ID); n != 0 {
		t.Fatalf("consumed grants = %d", n)
	}
	// The release decision is the denial's evidence.
	if n := v.count(`SELECT count(*) FROM eacp.decision_evidence WHERE action_id = $1`, got.ID); n != 2 {
		t.Fatalf("decisions = %d", n)
	}
	if r := v.reservation(got.ID); r != "none" {
		t.Fatalf("reservation = %s", r)
	}
}

func TestBudgetFailuresDenyClosed(t *testing.T) {
	v, _ := newBudgetEnv(t, allowAll, "")
	if got := v.mustSubmit("k1", "DENIED"); got.StateReason != action.BudgetAccountMissing {
		t.Fatalf("without an account = %s", got.StateReason)
	}
	v.f.FundAgent(t, v.agent.Agent, "THB", fmt.Sprint(10*cost))
	s := v.submission("k2")
	s.Payload = json.RawMessage(`{"currency":"USD","amount":1}`)
	got, err := v.e.Submit(context.Background(), v.actor(), s)
	if err != nil || got.State != "DENIED" || got.StateReason != action.BudgetCostInvalid {
		t.Fatalf("a payload in another currency = %+v %v", got, err)
	}
	s = v.submission("k4")
	s.Payload = json.RawMessage(`{"currency":"THB","amount":0.0000001}`)
	got, err = v.e.Submit(context.Background(), v.actor(), s)
	if err != nil || got.State != "DENIED" || got.StateReason != action.BudgetCostInvalid {
		t.Fatalf("an amount too precise for an exact reservation = %+v %v", got, err)
	}
	if got := v.mustSubmit("k3", "QUEUED"); v.reservation(got.ID) != "ACTIVE 2400000/" {
		t.Fatalf("reservation = %s", v.reservation(got.ID))
	}
}

// ADR-012 §5: the outcome settles the reservation in its own transaction:
// success commits, an outcome without effect releases, and an unknown
// outcome holds its reservation until a human resolves it.
func TestSettlementFollowsTheOutcome(t *testing.T) {
	v, account := newBudgetEnv(t, allowAll, fmt.Sprint(10*cost))
	ctx := context.Background()
	r := reconcileEnv{env: v}
	settled := func(id uuid.UUID, want string) {
		t.Helper()
		if got := v.reservation(id); got != want {
			t.Fatalf("reservation of %s = %s, want %s", id, got, want)
		}
	}

	ok := v.mustSubmit("ok", "QUEUED")
	settled(ok.ID, "ACTIVE 2400000/")
	st, l := v.dispatch(ok.ID, "w1")
	if to := v.complete(st, l, worker.Result{Outcome: worker.Succeeded, ExternalReference: "PO-1"}); to != "SUCCEEDED" {
		t.Fatalf("ok = %s", to)
	}
	settled(ok.ID, "COMMITTED 2400000/2400000")

	refused := v.mustSubmit("refused", "QUEUED")
	st, l = v.dispatch(refused.ID, "w1")
	if to := v.complete(st, l, worker.Result{Outcome: worker.NoEffect, ErrorClass: "refused"}); to != "FAILED" {
		t.Fatalf("refused = %s", to)
	}
	settled(refused.ID, "RELEASED 2400000/0")

	cancelled := v.mustSubmit("cancelled", "QUEUED")
	if _, err := v.e.Cancel(ctx, v.actor(), cancelled.ID, "not needed"); err != nil {
		t.Fatal(err)
	}
	settled(cancelled.ID, "RELEASED 2400000/0")

	// Unknown outcomes hold their reservation through escalation.
	unknown := map[string]action.View{}
	for _, key := range []string{"found", "absent"} {
		got := v.mustSubmit(key, "QUEUED")
		st, l := v.dispatch(got.ID, "w1")
		if to := v.complete(st, l, ambiguous); to != "UNKNOWN_OUTCOME" {
			t.Fatalf("%s = %s", key, to)
		}
		unknown[key] = v.get(got.ID)
		settled(got.ID, "ACTIVE 2400000/")
	}
	r.settle(unknown["found"])
	r.settle(unknown["absent"])
	r.sweepWith(func(s *action.Sweeper) { s.ReconcileMaxAge = time.Millisecond })
	for key, got := range unknown {
		if st := v.get(got.ID).State; st != "NEEDS_HUMAN_RESOLUTION" {
			t.Fatalf("%s = %s", key, st)
		}
		settled(got.ID, "ACTIVE 2400000/")
	}
	if a := v.account(account); a.Reserved != "4800000" || a.Committed != "2400000" || a.Available != "16800000" {
		t.Fatalf("account while two outcomes are unknown = %+v", a)
	}
	if _, _, err := v.e.Resolve(ctx, r.operator("otto"), unknown["found"].ID, action.Resolution{Outcome: "succeeded",
		Reason: "found in the ERP", Evidence: "ERP search", ExternalReference: "PO-9"}); err != nil {
		t.Fatal(err)
	}
	settled(unknown["found"].ID, "COMMITTED 2400000/2400000")
	if _, _, err := v.e.Resolve(ctx, r.operator("otto"), unknown["absent"].ID, action.Resolution{Outcome: "failed",
		Reason: "not in the ERP", Evidence: "ERP audit export"}); err != nil {
		t.Fatal(err)
	}
	settled(unknown["absent"].ID, "RELEASED 2400000/0")

	if a := v.account(account); a.Reserved != "0" || a.Committed != "4800000" || a.Available != "19200000" {
		t.Fatalf("account = %+v", a)
	}
	// The evidence journals the reservation and its settlement.
	ev, err := v.e.Evidence(ctx, v.f.Tenant, ok.ID)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, j := range ev.Journal {
		if j.Subject.Type == "budget_reservation" {
			kinds = append(kinds, j.Kind+" by "+j.Actor.Kind)
		}
	}
	if !slices.Equal(kinds, []string{"budget.reserved by agent", "budget.committed by system"}) || !ev.Chain.Verified {
		t.Fatalf("budget journal = %v (chain %+v)", kinds, ev.Chain)
	}
}

// §47 reservation TTL: an action that is never dispatched expires at
// not_after, and the expiry releases its reservation.
func TestAnExpiredReleaseGivesItsBudgetBack(t *testing.T) {
	v, account := newBudgetEnv(t, allowAll, fmt.Sprint(cost))
	s := v.submission("short")
	s.Lifetime = time.Second
	got, err := v.e.Submit(context.Background(), v.actor(), s)
	if err != nil || got.State != "QUEUED" {
		t.Fatalf("submit = %+v %v", got, err)
	}
	if v.mustSubmit("waiting", "DENIED").StateReason != action.BudgetExceeded {
		t.Fatal("the budget was not held")
	}
	time.Sleep(time.Until(got.NotAfter) + 50*time.Millisecond)
	v.sweep()
	if st := v.get(got.ID).State; st != "EXPIRED" {
		t.Fatalf("state = %s", st)
	}
	if r := v.reservation(got.ID); r != "RELEASED 2400000/0" {
		t.Fatalf("reservation = %s", r)
	}
	if a := v.account(account); a.Available != json.Number(fmt.Sprint(cost)) {
		t.Fatalf("account = %+v", a)
	}
	v.mustSubmit("later", "QUEUED")
}
