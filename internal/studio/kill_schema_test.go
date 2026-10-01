package studio_test

// Raw-SQL tests of the run kill scope (Phase 28, ADR-016 Rev 1.1): PostgreSQL
// binds every Studio action to its run when it is inserted, and a kill that
// matches a run stops the run and its actions (migration 00030).

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/registry/registrytest"
	"github.com/atipongsena/eacp/internal/storage"
)

const killSQL = `SELECT eacp.set_kill($1, $2, $3, 'containment')`

// running starts a run of agent as stella and claims it as runtime r1.
func (f *fix) running(agent uuid.UUID) (uuid.UUID, int64) {
	f.t.Helper()
	run := f.ID(f.t, "stella", startSQL, agent, inputs)
	c := f.mustClaim("r1")
	if c.ID != run {
		f.t.Fatalf("claimed %s, want %s", c.ID, run)
	}
	return run, c.Generation
}

// killed reports whether eacp.action_killed (T14, T16 and the claim filter) stops action a.
func (f *fix) killed(a uuid.UUID) string {
	f.t.Helper()
	return f.scalar(`SELECT eacp.action_killed(a)::text FROM eacp.actions a WHERE id = $1`, a)
}

// beat sends a heartbeat for run as runtime r1 and returns its answer.
func (f *fix) beat(run uuid.UUID, gen int64) (string, error) {
	var s string
	ctx := context.Background()
	err := storage.InTenantTx(ctx, f.App, f.Tenant.String(), func(tx pgx.Tx) error {
		if err := storage.SetActor(ctx, tx, f.P["rt"]); err != nil {
			return err
		}
		return tx.QueryRow(ctx, heartbeatSQL, run, "r1", gen).Scan(&s)
	})
	return s, err
}

func TestEveryStudioActionIsBoundToItsRun(t *testing.T) {
	f := newFix(t)
	version, agent := f.approvedAgent("leave-bot")
	other, _ := f.approvedAgent("other-bot")
	run := f.ID(t, "stella", startSQL, agent, inputs)
	try := func(v uuid.UUID, key, subject, tool string) error {
		_, err := f.TryAgentID(v, registrytest.ReceivedActionSQL, v, key, subject, tool)
		return err
	}
	// A queued run has no runtime yet: it submits nothing.
	wantState(t, try(version, stepKey(run, 0), stella, "hr-mcp.get_leave_balance"), sqlForbidden)
	f.mustClaim("r1")
	for name, err := range map[string]error{
		"no run":          try(version, "my-own-key", stella, "hr-mcp.get_leave_balance"),
		"malformed run":   try(version, "studio:not-a-run:0", stella, "hr-mcp.get_leave_balance"),
		"unknown run":     try(version, stepKey(uuid.New(), 0), stella, "hr-mcp.get_leave_balance"),
		"another version": try(other, stepKey(run, 0), stella, "hr-mcp.get_leave_balance"),
		"another subject": try(version, stepKey(run, 0), "abe@tenant-a.test", "hr-mcp.get_leave_balance"),
		"another tool":    try(version, stepKey(run, 0), stella, "erp.read"),
		"a respond step":  try(version, stepKey(run, 1), stella, "hr-mcp.get_leave_balance"),
		"no such step":    try(version, stepKey(run, 7), stella, "hr-mcp.get_leave_balance"),
	} {
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		wantState(t, err, sqlForbidden)
	}
	a := f.action(version, stepKey(run, 0), stella)
	if got := f.scalar(`SELECT (studio_run_id = $2)::text FROM eacp.actions WHERE id = $1`, a, run); got != "true" {
		t.Fatalf("bound to its run = %q", got)
	}
	// The binding never changes.
	wantState(t, f.ExecAgent(version, `UPDATE eacp.actions SET studio_run_id = NULL WHERE id = $1`, a),
		sqlForbidden, sqlBadState)
	// An agent that is not a Studio agent names no run, whatever its key says.
	plain := f.ActiveAgent(t, "plain", f.erpRead.Tool)
	p := f.AgentID(t, plain.Version, registrytest.ReceivedActionSQL, plain.Version, stepKey(run, 0), stella, "erp.read")
	if got := f.scalar(`SELECT (studio_run_id IS NULL)::text FROM eacp.actions WHERE id = $1`, p); got != "true" {
		t.Fatalf("a plain action is bound to a run: %q", got)
	}
}

func TestARunKillStopsThatRunsActionsOnly(t *testing.T) {
	f := newFix(t)
	version, agent := f.approvedAgent("leave-bot")
	run1, _ := f.running(agent)
	a1 := f.action(version, stepKey(run1, 0), stella)
	run2, _ := f.running(agent)
	a2 := f.action(version, stepKey(run2, 0), stella)

	wantState(t, f.Exec("stella", killSQL, "run", run1, true), sqlForbidden)
	wantState(t, f.Exec("otto", killSQL, "run", uuid.New(), true), sqlForeignKey)
	wantState(t, f.Exec("otto", killSQL, "run", agent, true), sqlForeignKey)
	// global still waits for platform authority.
	wantState(t, f.Exec("otto", killSQL, "global", f.Tenant, true), sqlCheck)

	ok(t, f.Exec("otto", killSQL, "run", run1, true))
	if got := f.killed(a1) + " " + f.killed(a2); got != "true false" {
		t.Fatalf("killed = %q", got)
	}
	// Clearing it is a second operator's.
	wantState(t, f.Exec("otto", killSQL, "run", run1, false), sqlForbidden)
	ok(t, f.Exec("opal", killSQL, "run", run1, false))
	if got := f.killed(a1); got != "false" {
		t.Fatalf("resumed = %q", got)
	}
}

func TestAKilledRunStopsAtItsHeartbeat(t *testing.T) {
	f := newFix(t)
	version, agent := f.approvedAgent("leave-bot")
	run, gen := f.running(agent)
	a := f.action(version, stepKey(run, 0), stella)
	if got, err := f.beat(run, gen); err != nil || got != "active" {
		t.Fatalf("heartbeat = %q, %v", got, err)
	}
	ok(t, f.Exec("otto", killSQL, "run", run, true))
	if got, err := f.beat(run, gen); err != nil || got != "killed" {
		t.Fatalf("heartbeat of a killed run = %q, %v", got, err)
	}
	if got := f.scalar(`SELECT concat_ws(' ', state, failure_reason, inputs IS NULL, finished_at IS NOT NULL)
		FROM eacp.studio_runs WHERE id = $1`, run); got != "FAILED killed t t" {
		t.Fatalf("run = %q", got)
	}
	// PostgreSQL finished it: the runtime holds nothing more.
	_, err := f.beat(run, gen)
	wantState(t, err, sqlForbidden)
	// Resuming the scope restarts nothing: the run stays failed, and its
	// queued step never runs for a run nobody is waiting on.
	ok(t, f.Exec("opal", killSQL, "run", run, false))
	if got := f.scalar(`SELECT state FROM eacp.studio_runs WHERE id = $1`, run); got != "FAILED" {
		t.Fatalf("resumed run = %q", got)
	}
	if got := f.killed(a); got != "true" {
		t.Fatalf("an action of a killed run after the resume = %q", got)
	}
}

func TestKillsOfTheAgentStopNewAndQueuedRuns(t *testing.T) {
	f := newFix(t)
	version, agent := f.approvedAgent("leave-bot")
	for _, tc := range []struct {
		scope  string
		target uuid.UUID
	}{
		{"tenant", f.Tenant},
		{"agent", agent},
		{"agent_version", version},
	} {
		t.Run(tc.scope, func(t *testing.T) {
			queued := f.ID(t, "stella", startSQL, agent, inputs)
			ok(t, f.Exec("otto", killSQL, tc.scope, tc.target, true))
			_, err := f.TryID("stella", startSQL, agent, inputs)
			wantState(t, err, sqlBadState)
			if got, err := f.claim("r1", 10); err != nil || len(got) != 0 {
				t.Fatalf("claimed under a kill: %v, %v", got, err)
			}
			if got := f.scalar(`SELECT concat_ws(' ', state, failure_reason) FROM eacp.studio_runs WHERE id = $1`,
				queued); got != "FAILED killed" {
				t.Fatalf("queued run = %q", got)
			}
			ok(t, f.Exec("opal", killSQL, tc.scope, tc.target, false))
		})
	}
	// With every scope cleared, runs start and are claimed again.
	run := f.ID(t, "stella", startSQL, agent, inputs)
	if c := f.mustClaim("r1"); c.ID != run {
		t.Fatalf("claimed %s", c.ID)
	}
}

// A tool kill stops the run's actions but not the run itself: its next step
// waits and the run ends by its own rules (ADR-016 Rev 1.1).
func TestAToolKillDoesNotEndARun(t *testing.T) {
	f := newFix(t)
	version, agent := f.approvedAgent("leave-bot")
	run, gen := f.running(agent)
	a := f.action(version, stepKey(run, 0), stella)
	ok(t, f.Exec("otto", killSQL, "tool", f.balance.Tool, true))
	if got := f.killed(a); got != "true" {
		t.Fatalf("killed = %q", got)
	}
	if got, err := f.beat(run, gen); err != nil || got != "active" {
		t.Fatalf("heartbeat = %q, %v", got, err)
	}
}

func TestARunKillIncidentNamesTheRunsVersion(t *testing.T) {
	f := newFix(t)
	version, agent := f.approvedAgent("leave-bot")
	run, _ := f.running(agent)
	ok(t, f.Exec("otto", killSQL, "run", run, true))
	ctx := context.Background()
	ok(t, storage.InTenantTx(ctx, f.App, f.Tenant.String(), func(tx pgx.Tx) error {
		if err := storage.SetSystem(ctx, tx, "incident"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT eacp.incident_evaluate()`)
		return err
	}))
	if got := f.scalar(`SELECT (affected::text LIKE '%' || $1 || '%')::text FROM eacp.incidents WHERE kind = 'kill'`,
		version.String()); got != "true" {
		t.Fatalf("incident affects its version = %q", got)
	}
}
