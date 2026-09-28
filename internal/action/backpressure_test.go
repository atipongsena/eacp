package action_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/atipongsena/eacp/internal/action"
	"github.com/atipongsena/eacp/internal/worker"
)

// queueCappedContractSQL is SafeContractSQL with at most two released,
// unfinished actions in the connector's queue (ADR-022 §1).
const queueCappedContractSQL = `INSERT INTO eacp.tool_contracts
	(tenant_id, tool_id, side_effects, idempotency_mode, reconciliation_lookup,
	 reconciliation_consistency, proof_standard, max_attempts, max_queued)
	VALUES (eacp.current_tenant_id(), $1, '{READ_ONLY}', 'none', 'none', 'none', 'none', 3, 2)
	RETURNING id`

func admissionScope(t *testing.T, err error) string {
	t.Helper()
	var a *action.AdmissionError
	if !errors.Is(err, action.ErrAdmission) || !errors.As(err, &a) {
		t.Fatalf("error = %v, want an admission error", err)
	}
	return a.Scope
}

// Invariant 9 at admission: a full connector queue refuses that connector's
// submissions and still admits another connector's. Unreleased actions
// count against the tenant's pending limit.
func TestAdmissionBoundsPendingAndConnectorQueues(t *testing.T) {
	v := newEnvWith(t, allowAll, queueCappedContractSQL, func(o *action.Options) {
		o.Limits.MaxPendingPerTenant = 2
	})
	mail := v.f.ActiveTool(t, "mail", "send")
	mailer := v.f.ActiveAgent(t, "mailer", mail.Tool)
	mailActor := action.Agent(v.f.Tenant, mailer.Agent, mailer.Version)
	sendMail := func(key string) (action.View, error) {
		s := v.submission(key)
		s.Tool, s.Operation = "mail.send", "send"
		return v.e.Submit(context.Background(), mailActor, s)
	}

	v.mustSubmit("q1", "QUEUED")
	v.mustSubmit("q2", "QUEUED")
	_, err := v.submit("q3")
	if scope := admissionScope(t, err); scope != "connector" {
		t.Fatalf("scope = %q, want connector", scope)
	}
	if n := v.count(`SELECT count(*) FROM eacp.actions WHERE idempotency_key = 'q3'`); n != 0 {
		t.Fatalf("a rejected submission created %d actions", n)
	}
	// A replay of an admitted submission is never rejected.
	if got, err := v.submit("q1"); err != nil || got.State != "QUEUED" {
		t.Fatalf("replay = %+v %v", got, err)
	}
	// Another connector's queue is unaffected.
	if got, err := sendMail("m1"); err != nil || got.State != "QUEUED" {
		t.Fatalf("mail while erp is full = %+v %v", got, err)
	}

	// With governance down, actions wait RECEIVED; the pending limit bounds them.
	v.pdp.set("down")
	for _, key := range []string{"p1", "p2"} {
		if got, err := sendMail(key); !errors.Is(err, action.ErrGovernanceUnavailable) || got.State != "RECEIVED" {
			t.Fatalf("submit %s = %+v %v", key, got, err)
		}
	}
	_, err = sendMail("p3")
	if scope := admissionScope(t, err); scope != "pending" {
		t.Fatalf("scope = %q, want pending", scope)
	}
}

// retryCostContractSQL is a budgeted, natively idempotent financial write
// priced like CostedContractSQL, with five attempts and retries that may
// cost at most two further calls.
var retryCostContractSQL = fmt.Sprintf(`INSERT INTO eacp.tool_contracts
	(tenant_id, tool_id, side_effects, idempotency_mode, idempotency_key_field, reconciliation_lookup,
	 reconciliation_consistency, proof_standard, no_effect_errors, max_attempts, timeout_ms,
	 cost_unit, cost_amount_field, cost_unit_field, retry_max_cost)
	VALUES (eacp.current_tenant_id(), $1, '{REVERSIBLE_WRITE,FINANCIAL}', 'native', 'Idempotency-Key', 'none',
	 'none', 'none', '{refused}', 5, 200, 'THB', 'amount', 'currency', %d)
	RETURNING id`, 2*cost)

func TestRetryBudgetBoundsRetryCost(t *testing.T) {
	v := newEnvWith(t, allowAll, retryCostContractSQL)
	v.f.FundAgent(t, v.agent.Agent, "THB", fmt.Sprint(10*cost))
	refused := worker.Result{Outcome: worker.NoEffect, ErrorClass: "refused"}
	got := v.mustSubmit("costly", "QUEUED")
	for attempt := 1; attempt <= 3; attempt++ {
		st, l := v.dispatch(got.ID, fmt.Sprintf("w%d", attempt))
		want := "RETRY_WAIT"
		if attempt == 3 { // a third retry would take retry cost to 3 × cost
			want = "FAILED"
		}
		if to := v.complete(st, l, refused); to != want {
			t.Fatalf("attempt %d = %s, want %s", attempt, to, want)
		}
		if want == "RETRY_WAIT" {
			time.Sleep(150 * time.Millisecond)
			v.sweep()
			if st := v.get(got.ID).State; st != "QUEUED" {
				t.Fatalf("after attempt %d the retry is %s", attempt, st)
			}
		}
	}
	if n := v.count(`SELECT count(*) FROM eacp.action_attempts WHERE action_id = $1`, got.ID); n != 3 {
		t.Fatalf("attempts = %d, want 3", n)
	}
}
