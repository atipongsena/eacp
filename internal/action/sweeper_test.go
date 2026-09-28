package action_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/atipongsena/eacp/internal/action"
	"github.com/atipongsena/eacp/internal/registry"
)

func (v env) sweep() action.Stats {
	v.t.Helper()
	s := action.NewSweeper(v.e)
	s.Grace = 0
	st, err := s.RunOnce(context.Background())
	if err != nil {
		v.t.Fatal(err)
	}
	return st
}

func TestSweeperRecoversActionsLeftByAnOutage(t *testing.T) {
	v := newEnv(t, allowAll)
	v.pdp.set("down")
	got, err := v.submit("k1")
	if !errors.Is(err, action.ErrGovernanceUnavailable) {
		t.Fatalf("submit = %v", err)
	}
	v.sweep()
	if st := v.get(got.ID).State; st != "RECEIVED" {
		t.Fatalf("state during outage = %s, want RECEIVED (never DENIED)", st)
	}
	v.pdp.set("")
	v.sweep()
	if st := v.get(got.ID).State; st != "QUEUED" {
		t.Fatalf("state after recovery = %s", st)
	}
}

func TestSweeperReleasesApprovedActions(t *testing.T) {
	v := newEnv(t, escalateAll)
	got := v.mustSubmit("k1", "PENDING_APPROVAL")
	v.approve(*got.ApprovalRequestID)
	v.sweep()
	if st := v.get(got.ID).State; st != "QUEUED" {
		t.Fatalf("state = %s", st)
	}
	// A release that cannot revalidate leaves the approval unspent.
	got2 := v.mustSubmit("k2", "PENDING_APPROVAL")
	v.approve(*got2.ApprovalRequestID)
	v.pdp.set("down")
	v.sweep()
	if st := v.get(got2.ID).State; st != "AUTHORIZED" {
		t.Fatalf("state with PDP down = %s", st)
	}
	if n := v.count(`SELECT count(*) FROM eacp.approval_grants WHERE action_id = $1 AND consumed_at IS NULL`, got2.ID); n != 1 {
		t.Fatalf("unspent grants = %d", n)
	}
}

func TestSweeperExpiresOverdueActionsAndApprovals(t *testing.T) {
	v := newEnv(t, allowAll)
	short := func(key string) action.Submission {
		s := v.submission(key)
		s.Lifetime = time.Second
		return s
	}
	queued, err := v.e.Submit(context.Background(), v.actor(), short("queued"))
	if err != nil || queued.State != "QUEUED" {
		t.Fatalf("queued = %+v, err = %v", queued, err)
	}
	v.pdp.set("down")
	received, _ := v.e.Submit(context.Background(), v.actor(), short("received"))
	v.pdp.set("")
	v.activate(escalateAll)
	pending, err := v.e.Submit(context.Background(), v.actor(), short("pending"))
	if err != nil || pending.State != "PENDING_APPROVAL" {
		t.Fatalf("pending = %+v, err = %v", pending, err)
	}
	time.Sleep(1100 * time.Millisecond)
	v.sweep()
	for name, id := range map[string]action.View{"queued": queued, "received": received, "pending": pending} {
		if st := v.get(id.ID).State; st != "EXPIRED" {
			t.Errorf("%s: state = %s, want EXPIRED", name, st)
		}
	}
	if st := v.requestState(*pending.ApprovalRequestID); st != "EXPIRED" {
		t.Fatalf("request = %s", st)
	}
}

func TestCancelBeforeDispatchNeverConsultsGovernance(t *testing.T) {
	v := newEnv(t, escalateAll)
	ctx := context.Background()
	pending := v.mustSubmit("k1", "PENDING_APPROVAL")
	authorized := v.mustSubmit("k2", "PENDING_APPROVAL")
	v.approve(*authorized.ApprovalRequestID)
	v.activate(allowAll)
	queued := v.mustSubmit("k3", "QUEUED")
	calls := v.pdp.calls.Load()
	v.pdp.set("down")

	other := v.f.ActiveAgent(t, "other", v.tool.Tool)
	if _, err := v.e.Cancel(ctx, action.Agent(v.f.Tenant, other.Agent, other.Version), pending.ID, "mine"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("other agent cancel = %v, want ErrNotFound", err)
	}
	if _, err := v.e.Cancel(ctx, action.Principal(v.f.Tenant, v.f.P["erin"]), pending.ID, "no"); !errors.Is(err, registry.ErrForbidden) {
		t.Fatalf("non-operator cancel = %v, want ErrForbidden", err)
	}
	for who, c := range map[string]struct {
		a  action.Actor
		id action.View
	}{
		"agent":    {v.actor(), pending},
		"operator": {action.Principal(v.f.Tenant, v.f.P["otto"]), authorized},
		"subject":  {action.Principal(v.f.Tenant, v.f.P["carol"]), queued},
	} {
		got, err := v.e.Cancel(ctx, c.a, c.id.ID, "no longer needed")
		if err != nil || got.State != "CANCELLED" {
			t.Fatalf("%s cancel = %+v, err = %v", who, got, err)
		}
	}
	if v.pdp.calls.Load() != calls {
		t.Fatal("cancellation consulted governance")
	}
	if st := v.requestState(*pending.ApprovalRequestID); st != "VOIDED" {
		t.Fatalf("request after cancel = %s", st)
	}
	if n := v.count(`SELECT count(*) FROM eacp.approval_grants WHERE action_id = $1
		AND consumed_at IS NULL AND expires_at > now()`, authorized.ID); n != 0 {
		t.Fatalf("usable grants after cancel = %d", n)
	}
	if _, err := v.e.Cancel(ctx, v.actor(), pending.ID, "again"); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("cancel of a terminal action = %v, want ErrConflict", err)
	}
	if _, err := v.e.Cancel(ctx, v.actor(), pending.ID, " "); !errors.Is(err, registry.ErrInvalid) {
		t.Fatalf("cancel without reason = %v", err)
	}
}
