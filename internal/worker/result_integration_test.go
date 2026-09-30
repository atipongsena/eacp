package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/storage"
	"github.com/atipongsena/eacp/internal/storage/pgtest"
	"github.com/atipongsena/eacp/internal/worker"
)

// outputCanary marks tool output: it must reach the calling agent and
// nothing else (ADR-034).
const outputCanary = "OUTPUT-CANARY-5d1c"

func newResultEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWith(t, map[string]string{"hr.balance": retainedContractSQL})
}

// result reads action id's result as the env's agent: its output, withheld
// reason and whether there is one.
func (v *env) result(id uuid.UUID) (output, withheld string, found bool) {
	v.t.Helper()
	ctx := context.Background()
	err := storage.InTenantTx(ctx, v.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		if err := storage.SetAgent(ctx, tx, v.agent.Version); err != nil {
			return err
		}
		var out, why *string
		err := tx.QueryRow(ctx, `SELECT output, withheld FROM eacp.action_result($1)`, id).Scan(&out, &why)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		if out != nil {
			output = *out
		}
		if why != nil {
			withheld = *why
		}
		return nil
	})
	if err != nil {
		v.t.Fatal(err)
	}
	return output, withheld, found
}

func succeedWith(output string) func(context.Context, worker.Call) worker.Result {
	return func(_ context.Context, c worker.Call) worker.Result {
		return worker.Result{Outcome: worker.Succeeded, ExternalReference: "R-" + c.ActionID.String()[:8],
			Output: json.RawMessage(output)}
	}
}

func TestASuccessKeepsItsOutputForTheAgent(t *testing.T) {
	v := newResultEnv(t)
	v.conn.fn = succeedWith(`{"name":"` + outputCanary + `", "days": 12}`)
	a := v.submit("hr.balance")
	v.runOnce(v.worker("w1"))
	if got := v.get(a.ID); got.State != "SUCCEEDED" {
		t.Fatalf("state %s (%s)", got.State, got.StateReason)
	}
	if c := v.conn.calls[0]; c.Contract.ResultRetention != 300 {
		t.Fatalf("call retention = %d", c.Contract.ResultRetention)
	}
	out, withheld, found := v.result(a.ID)
	if !found || withheld != "" || out != `{"days":12,"name":"`+outputCanary+`"}` {
		t.Fatalf("result = %q %q %v", out, withheld, found)
	}
	// Nowhere else: not the log, the action, its attempts, the journal or the outbox.
	if strings.Contains(v.logs.String(), outputCanary) {
		t.Fatal("output in the worker log")
	}
	for _, sql := range []string{
		`SELECT count(*) FROM eacp.actions WHERE to_jsonb(actions)::text LIKE '%' || $1 || '%'`,
		`SELECT count(*) FROM eacp.action_attempts WHERE to_jsonb(action_attempts)::text LIKE '%' || $1 || '%'`,
		`SELECT count(*) FROM eacp.audit_events WHERE convert_from(payload, 'UTF8') LIKE '%' || $1 || '%'`,
		`SELECT count(*) FROM eacp.outbox_events WHERE to_jsonb(outbox_events)::text LIKE '%' || $1 || '%'`,
	} {
		if n := v.count(sql, outputCanary); n != 0 {
			t.Fatalf("output stored: %s", sql)
		}
	}
}

func TestNothingIsKeptWithoutRetention(t *testing.T) {
	v := newResultEnv(t)
	v.conn.fn = succeedWith(`{"po":"` + outputCanary + `"}`)
	a := v.submit("erp.lookup")
	v.runOnce(v.worker("w1"))
	if got := v.get(a.ID); got.State != "SUCCEEDED" {
		t.Fatalf("state %s", got.State)
	}
	if n := v.count(`SELECT count(*) FROM eacp.action_results`); n != 0 {
		t.Fatalf("rows = %d", n)
	}
}

func TestAnAmbiguousResultKeepsNoOutput(t *testing.T) {
	v := newResultEnv(t)
	v.conn.fn = func(context.Context, worker.Call) worker.Result {
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "timeout", Output: json.RawMessage(`{"a":1}`)}
	}
	a := v.submit("hr.balance")
	v.runOnce(v.worker("w1"))
	if got := v.get(a.ID); got.State != "RETRY_WAIT" {
		t.Fatalf("state %s", got.State)
	}
	if n := v.count(`SELECT count(*) FROM eacp.action_results`); n != 0 {
		t.Fatalf("rows = %d", n)
	}
}

func TestAKilledSuccessKeepsNoOutput(t *testing.T) {
	v := newResultEnv(t)
	v.conn.fn = func(_ context.Context, c worker.Call) worker.Result {
		if err := v.f.Exec("otto", `SELECT eacp.set_kill('action', $1, true, 'incident containment')`, c.ActionID); err != nil {
			t.Error(err)
		}
		return worker.Result{Outcome: worker.Succeeded, ExternalReference: "R-1", Output: json.RawMessage(`{"a":1}`)}
	}
	a := v.submit("hr.balance")
	v.runOnce(v.worker("w1"))
	if got := v.get(a.ID); got.State != "UNKNOWN_OUTCOME" {
		t.Fatalf("state %s (%s)", got.State, got.StateReason)
	}
	if n := v.count(`SELECT count(*) FROM eacp.action_results`); n != 0 {
		t.Fatalf("rows = %d", n)
	}
}

func TestACredentialInTheOutputIsWithheld(t *testing.T) {
	v := newResultEnv(t)
	// The call's own secret, and one the worker holds for another connector.
	for _, leak := range []func(worker.Call) string{
		func(c worker.Call) string { return c.Secret.Reveal() },
		func(worker.Call) string { return canary + "-bank" },
	} {
		v.conn.fn = func(_ context.Context, c worker.Call) worker.Result {
			return worker.Result{Outcome: worker.Succeeded, ExternalReference: "R-1",
				Output: json.RawMessage(`{"echo":"` + leak(c) + `"}`)}
		}
		a := v.submit("hr.balance")
		v.runOnce(v.worker("w-" + uuid.NewString()[:8]))
		if got := v.get(a.ID); got.State != "SUCCEEDED" {
			t.Fatalf("state %s", got.State)
		}
		out, withheld, found := v.result(a.ID)
		if !found || out != "" || withheld != "contains_credential" {
			t.Fatalf("result = %q %q %v", out, withheld, found)
		}
	}
	if n := v.count(`SELECT count(*) FROM eacp.audit_events WHERE convert_from(payload, 'UTF8') LIKE '%' || $1 || '%'`,
		canary); n != 0 || strings.Contains(v.logs.String(), canary) {
		t.Fatal("secret leaked")
	}
}

func TestAnOversizedOutputIsWithheldAndTheActionSucceeds(t *testing.T) {
	v := newResultEnv(t)
	v.conn.fn = succeedWith(`"` + strings.Repeat("x", 70000) + `"`)
	a := v.submit("hr.balance")
	v.runOnce(v.worker("w1"))
	if got := v.get(a.ID); got.State != "SUCCEEDED" {
		t.Fatalf("state %s", got.State)
	}
	if _, withheld, found := v.result(a.ID); !found || withheld != "too_large" {
		t.Fatalf("result withheld %q %v", withheld, found)
	}
}

func TestAReadOnlyRetryKeepsTheSucceedingAttemptsOutput(t *testing.T) {
	v := newResultEnv(t)
	var n atomic.Int32
	v.conn.fn = func(_ context.Context, c worker.Call) worker.Result {
		if n.Add(1) == 1 {
			return worker.Result{Outcome: worker.Ambiguous, Output: json.RawMessage(`{"attempt":1}`)}
		}
		return worker.Result{Outcome: worker.Succeeded, ExternalReference: "R-1", Output: json.RawMessage(`{"attempt":2}`)}
	}
	w := v.worker("reader")
	a := v.submit("hr.balance")
	v.runOnce(w)
	time.Sleep(150 * time.Millisecond)
	v.sweep()
	v.runOnce(w)
	if got := v.get(a.ID); got.State != "SUCCEEDED" || got.AttemptCount != 2 {
		t.Fatalf("read = %s after %d attempts", got.State, got.AttemptCount)
	}
	if out, _, _ := v.result(a.ID); out != `{"attempt":2}` {
		t.Fatalf("output = %q", out)
	}
	if k := v.count(`SELECT count(*) FROM eacp.action_results WHERE action_id = $1 AND lease_generation = 2`, a.ID); k != 1 {
		t.Fatalf("rows of generation 2 = %d", k)
	}
}
