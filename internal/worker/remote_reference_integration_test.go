package worker_test

import (
	"context"
	"testing"
	"time"

	"eacp/internal/worker"
)

// remoteConnector returns whatever result its function makes of the call.
type remoteConnector func(worker.Call) worker.Result

func (c remoteConnector) Execute(_ context.Context, call worker.Call) worker.Result { return c(call) }

func (remoteConnector) Lookup(context.Context, worker.LookupCall) worker.LookupResult {
	return worker.LookupResult{Status: worker.LookupUnknown}
}

// TestEvidenceShowsTheRemoteReference: the remote task id a connector
// reports is kept with an ambiguous or no-effect attempt and shown in the
// evidence (ADR-030 §6); it is dropped from a success, when malformed and
// when it contains the credential.
func TestEvidenceShowsTheRemoteReference(t *testing.T) {
	v := newERPEnv(t)
	for _, c := range []struct {
		name   string
		result func(worker.Call) worker.Result
		want   string
	}{
		{"ambiguous", func(worker.Call) worker.Result {
			return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "a2a_failed", RemoteReference: "task-7"}
		}, "task-7"},
		{"no effect", func(worker.Call) worker.Result {
			return worker.Result{Outcome: worker.NoEffect, ErrorClass: "validation", RemoteReference: "ctx:task.8"}
		}, "ctx:task.8"},
		{"success", func(worker.Call) worker.Result {
			return worker.Result{Outcome: worker.Succeeded, ExternalReference: "PO-1", RemoteReference: "task-9"}
		}, ""},
		{"malformed", func(worker.Call) worker.Result {
			return worker.Result{Outcome: worker.Ambiguous, RemoteReference: "task 10"}
		}, ""},
		{"credential", func(call worker.Call) worker.Result {
			return worker.Result{Outcome: worker.Ambiguous, RemoteReference: "t-" + call.Secret.Reveal()}
		}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			w, err := worker.New(v.f.App, worker.Options{ID: "w1", Lease: 5 * time.Second,
				Connectors: map[string]worker.Connector{"http": remoteConnector(c.result)}, Secrets: v.secrets,
				Backoff: func(int) time.Duration { return time.Hour }})
			if err != nil {
				t.Fatal(err)
			}
			a := v.submit("create_po_eventual", map[string]any{})
			if n, err := w.RunOnce(context.Background()); err != nil || n != 1 {
				t.Fatalf("dispatched %d, %v", n, err)
			}
			ev, err := v.e.Evidence(context.Background(), v.f.Tenant, a.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(ev.Attempts) != 1 || ev.Attempts[0].RemoteReference != c.want {
				t.Fatalf("attempts %+v, want remote reference %q", ev.Attempts, c.want)
			}
		})
	}
}

// TestAKillKeepsTheReferenceOfASuccess: a kill that lands during a call
// turns its success into an unknown outcome; the reference the connector
// proved is kept as the remote reference, so the human who settles the
// action can find the work (ADR-030 §5).
func TestAKillKeepsTheReferenceOfASuccess(t *testing.T) {
	v := newERPEnv(t)
	w, err := worker.New(v.f.App, worker.Options{ID: "w1", Lease: 5 * time.Second,
		Connectors: map[string]worker.Connector{"http": remoteConnector(func(call worker.Call) worker.Result {
			if err := v.f.Exec("otto", `SELECT eacp.set_kill('action', $1, true, 'incident containment')`,
				call.ActionID); err != nil {
				t.Error(err)
			}
			return worker.Result{Outcome: worker.Succeeded, ExternalReference: "task-9"}
		})}, Secrets: v.secrets, Backoff: func(int) time.Duration { return time.Hour }})
	if err != nil {
		t.Fatal(err)
	}
	a := v.submit("create_po_eventual", map[string]any{})
	if n, err := w.RunOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("dispatched %d, %v", n, err)
	}
	ev, err := v.e.Evidence(context.Background(), v.f.Tenant, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.Attempts) != 1 || ev.Attempts[0].ErrorClass != "kill_interrupted" ||
		ev.Attempts[0].ExternalReference != "" || ev.Attempts[0].RemoteReference != "task-9" {
		t.Fatalf("attempts %+v", ev.Attempts)
	}
}
