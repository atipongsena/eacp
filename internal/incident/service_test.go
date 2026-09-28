package incident_test

import (
	"context"
	"errors"
	"testing"

	"github.com/atipongsena/eacp/internal/incident"
	"github.com/atipongsena/eacp/internal/registry"
	"github.com/atipongsena/eacp/internal/registry/registrytest"
)

func as(f *registrytest.Fixture, name string) registry.Actor {
	return registry.Actor{TenantID: f.Tenant, PrincipalID: f.P[name]}
}

func wantKind(t *testing.T, err error, kind error) {
	t.Helper()
	if !errors.Is(err, kind) {
		t.Fatalf("err = %v, want %v", err, kind)
	}
}

func TestAnIncidentIsWorkedThroughTheService(t *testing.T) {
	f := registrytest.New(t)
	s := incident.New(f.App)
	ctx := context.Background()
	tool := f.ActiveTool(t, "erp", "po")

	_, err := s.Open(ctx, as(f, "carol"), incident.NewIncident{Title: "x", Severity: "low", Reason: "r"})
	wantKind(t, err, registry.ErrForbidden)
	_, err = s.Open(ctx, as(f, "otto"), incident.NewIncident{Title: "x", Severity: "low"})
	wantKind(t, err, registry.ErrInvalid)
	in, err := s.Open(ctx, as(f, "otto"), incident.NewIncident{Title: "odd purchases", Severity: "critical",
		SubjectType: "tool", SubjectID: &tool.Tool, Reason: "reported by finance"})
	ok(t, err)
	if in.Kind != "manual" || in.State != "OPEN" || len(in.Events) != 1 || in.OpenedBy == nil || *in.OpenedBy != f.P["otto"] {
		t.Fatalf("opened = %+v", in)
	}
	_, err = s.Resolve(ctx, as(f, "otto"), in.ID, "contained", "done")
	wantKind(t, err, registry.ErrConflict)
	opal := f.P["opal"]
	_, err = s.Assign(ctx, as(f, "otto"), in.ID, &opal)
	ok(t, err)
	_, err = s.Note(ctx, as(f, "opal"), in.ID, "checking the ERP log")
	ok(t, err)
	_, err = s.Link(ctx, as(f, "opal"), in.ID, "tool", tool.Tool)
	ok(t, err)
	_, err = s.Acknowledge(ctx, as(f, "opal"), in.ID, "mine")
	ok(t, err)
	_, err = s.Resolve(ctx, as(f, "opal"), in.ID, "contained", "done")
	wantKind(t, err, registry.ErrForbidden)
	done, err := s.Resolve(ctx, as(f, "otto"), in.ID, "false_positive", "a scheduled batch")
	ok(t, err)
	if done.State != "RESOLVED" || len(done.Events) != 6 || done.Events[5].Kind != "resolved" {
		t.Fatalf("resolved = %+v", done)
	}
	_, err = s.Get(ctx, as(f, "audra"), in.ID)
	ok(t, err)
	list, err := s.List(ctx, as(f, "audra"), incident.Filter{State: "RESOLVED"})
	ok(t, err)
	if len(list) != 1 || list[0].ID != in.ID || list[0].Events != nil {
		t.Fatalf("list = %+v", list)
	}
	_, err = s.List(ctx, as(f, "audra"), incident.Filter{Limit: 501})
	wantKind(t, err, registry.ErrInvalid)
	_, err = s.Acknowledge(ctx, as(f, "otto"), tool.Tool, "no such incident")
	wantKind(t, err, registry.ErrNotFound)
}

func TestEvaluateAllOpensIncidentsForTenantsWithSignals(t *testing.T) {
	f := registrytest.New(t)
	s := incident.New(f.App)
	tool := f.ActiveTool(t, "erp", "po")
	f.ActiveAgent(t, "buyer", tool.Tool)
	ok(t, f.Exec("otto", `SELECT eacp.set_kill('tool', $1, true, 'contain', 'security_incident')`, tool.Tool))
	n, err := s.EvaluateAll(context.Background())
	ok(t, err)
	if n < 1 {
		t.Fatalf("EvaluateAll opened %d", n)
	}
	list, err := s.List(context.Background(), as(f, "otto"), incident.Filter{Kind: "kill"})
	ok(t, err)
	if len(list) != 1 || list[0].Severity != "critical" {
		t.Fatalf("list = %+v", list)
	}
}

func TestTheSummaryCountsTheTenant(t *testing.T) {
	f := registrytest.New(t)
	s := incident.New(f.App)
	ctx := context.Background()
	tool := f.ActiveTool(t, "erp", "purchase")
	agent := f.ActiveAgent(t, "buyer", tool.Tool)
	replica(t, f, scannerQuarantine, tool.Tool, "0 seconds")
	ok(t, f.Exec("otto", `SELECT eacp.set_kill('tool', $1, true, 'contain', 'security_incident')`, tool.Tool))
	action := f.ReceivedAction(t, agent.Version, "carol", "erp.purchase")
	replica(t, f, `UPDATE eacp.actions SET state = 'NEEDS_HUMAN_RESOLUTION' WHERE id = $1`, action)
	if _, err := s.Evaluate(ctx, f.Tenant); err != nil {
		t.Fatal(err)
	}
	sum, err := s.Summary(ctx, as(f, "audra"))
	ok(t, err)
	if sum.Agents.Registered != 1 || sum.Agents.Production != 1 || sum.Agents.HighRisk != 1 ||
		sum.Agents.Versions.Active != 1 || sum.Security.QuarantinedTools != 1 || sum.Security.ActiveKills != 1 ||
		sum.Security.OpenIncidents["critical"] != 2 || sum.Security.OpenIncidents["high"] != 1 ||
		sum.Security.Unacknowledged != 3 || sum.Execution.NeedsHuman != 1 || sum.AsOf.IsZero() ||
		sum.FinOps.SpendToday == nil {
		t.Fatalf("summary = %+v", sum)
	}
}
