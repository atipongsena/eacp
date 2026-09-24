package fleet_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/fleet"
	"eacp/internal/registry"
	"eacp/internal/registry/registrytest"
	"eacp/internal/storage"
	"eacp/internal/storage/pgtest"
)

type env struct {
	t *testing.T
	f *registrytest.Fixture
	s *fleet.Service
}

func newEnv(t *testing.T) env {
	f := registrytest.New(t)
	return env{t: t, f: f, s: fleet.New(f.App)}
}

func (e env) as(name string) registry.Actor {
	return registry.Actor{TenantID: e.f.Tenant, PrincipalID: e.f.P[name]}
}

func (e env) apply(name string, r fleet.Request) fleet.Operation {
	e.t.Helper()
	op, err := e.s.Apply(context.Background(), e.as(name), r)
	if err != nil {
		e.t.Fatalf("%s %s: %v", name, r.Kind, err)
	}
	return op
}

func (e env) state(v uuid.UUID) string {
	e.t.Helper()
	s, _ := versionState(e.t, e.f, v)
	return s
}

func wantKind(t *testing.T, err, kind error) {
	t.Helper()
	if !errors.Is(err, kind) {
		t.Fatalf("err = %v, want %v", err, kind)
	}
}

// stagingAgent registers an active agent in the staging environment.
func (e env) stagingAgent(name string) registrytest.Agent {
	e.t.Helper()
	var a registrytest.Agent
	a.Agent = e.f.ID(e.t, "erin", `INSERT INTO eacp.agents
		(tenant_id, name, display_name, environment, risk_class, owner_principal_id)
		VALUES (eacp.current_tenant_id(), $1, $1, 'staging', 'low', $2) RETURNING id`, name, e.f.P["carol"])
	a.Version = newVersion(e.t, e.f, a.Agent, "erin")
	if err := e.f.Exec("ravi", `UPDATE eacp.agent_versions SET state = 'ACTIVE', state_reason = 'go live' WHERE id = $1`, a.Version); err != nil {
		e.t.Fatal(err)
	}
	return a
}

func versionIDs(op fleet.Operation) []uuid.UUID {
	var ids []uuid.UUID
	for _, t := range op.Targets {
		ids = append(ids, t.VersionID)
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	return ids
}

func sorted(ids ...uuid.UUID) []uuid.UUID {
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	return ids
}

func TestPauseByFilterIsAtomicAndResumeUndoesExactlyIt(t *testing.T) {
	e := newEnv(t)
	buyer := e.f.ActiveAgent(t, "buyer")
	seller := e.f.ActiveAgent(t, "seller")
	staging := e.stagingAgent("tester")
	req := fleet.Request{Kind: "pause", Selector: fleet.Selector{Environment: "production"}, Reason: "incident 7", DryRun: true}

	plan := e.apply("otto", req)
	if plan.ID != uuid.Nil || !plan.DryRun || !slices.Equal(versionIDs(plan), sorted(buyer.Version, seller.Version)) {
		t.Fatalf("dry run = %+v", plan)
	}
	if e.state(buyer.Version) != "ACTIVE" {
		t.Fatal("a dry run changed a version")
	}

	req.DryRun = false
	op := e.apply("otto", req)
	if op.ID == uuid.Nil || len(op.Targets) != 2 || op.Targets[0].From != "ACTIVE" || op.Targets[0].To != "SUSPENDED" {
		t.Fatalf("pause = %+v", op)
	}
	for _, v := range []uuid.UUID{buyer.Version, seller.Version} {
		if e.state(v) != "SUSPENDED" {
			t.Fatalf("paused version = %s", e.state(v))
		}
	}
	if e.state(staging.Version) != "ACTIVE" {
		t.Fatal("pause touched an unselected agent")
	}
	stored, err := e.s.Operation(context.Background(), e.as("audra"), op.ID)
	if err != nil || stored.Kind != "pause" || stored.Reason != "incident 7" || stored.CreatedBy != e.f.P["otto"] ||
		!slices.Equal(versionIDs(stored), versionIDs(op)) {
		t.Fatalf("stored operation = %+v, %v", stored, err)
	}

	resumed := e.apply("ravi", fleet.Request{Kind: "resume", SourceOperationID: op.ID, Reason: "incident closed"})
	if !slices.Equal(versionIDs(resumed), versionIDs(op)) {
		t.Fatalf("resume = %+v", resumed)
	}
	if e.state(buyer.Version) != "ACTIVE" || e.state(seller.Version) != "ACTIVE" {
		t.Fatal("resume did not reactivate the paused versions")
	}
}

func TestSelectionMustBeExplicit(t *testing.T) {
	e := newEnv(t)
	e.f.ActiveAgent(t, "buyer")
	ctx := context.Background()
	for _, r := range []fleet.Request{
		{Kind: "pause", Reason: "x"},
		{Kind: "pause", Selector: fleet.Selector{All: true, Environment: "production"}, Reason: "x"},
		{Kind: "pause", Selector: fleet.Selector{Tool: "not-a-ref"}, Reason: "x"},
		{Kind: "pause", Selector: fleet.Selector{All: true}, Reason: " "},
		{Kind: "restart", Selector: fleet.Selector{All: true}, Reason: "x"},
		{Kind: "resume", Selector: fleet.Selector{All: true}, SourceOperationID: uuid.New(), Reason: "x"},
		{Kind: "resume", Reason: "x"},
		{Kind: "rollback", Selector: fleet.Selector{Agents: []string{"buyer", "seller"}}, Reason: "x"},
		{Kind: "rollback", Selector: fleet.Selector{Environment: "production"}, Reason: "x"},
		{Kind: "pause", Selector: fleet.Selector{All: true}, ToVersionID: uuid.New(), Reason: "x"},
	} {
		_, err := e.s.Apply(ctx, e.as("ravi"), r)
		if !errors.Is(err, registry.ErrInvalid) {
			t.Errorf("%+v: err = %v, want invalid", r, err)
		}
	}
	_, err := e.s.Apply(ctx, e.as("otto"), fleet.Request{Kind: "pause", Selector: fleet.Selector{Agents: []string{"nobody"}}, Reason: "x"})
	wantKind(t, err, registry.ErrNotFound)
	_, err = e.s.Apply(ctx, e.as("ravi"), fleet.Request{Kind: "resume", SourceOperationID: uuid.New(), Reason: "x"})
	wantKind(t, err, registry.ErrNotFound)
}

func TestAllSelectsTheWholeTenantAndSkipsWhatItCannotChange(t *testing.T) {
	e := newEnv(t)
	a := e.f.ActiveAgent(t, "buyer")
	idle := e.f.NewAgent(t, "idle") // REGISTERED only: nothing to pause
	op := e.apply("otto", fleet.Request{Kind: "pause", Selector: fleet.Selector{All: true}, Reason: "tenant drill"})
	if !slices.Equal(versionIDs(op), []uuid.UUID{a.Version}) || len(op.Skipped) != 1 ||
		op.Skipped[0].AgentID != idle.Agent || op.Skipped[0].Reason == "" {
		t.Fatalf("pause all = %+v", op)
	}
	// Nothing left to change is a conflict, not an empty operation.
	_, err := e.s.Apply(context.Background(), e.as("otto"), fleet.Request{Kind: "pause", Selector: fleet.Selector{All: true}, Reason: "again"})
	wantKind(t, err, registry.ErrConflict)
}

func TestToolSelectorMatchesTheActiveAllowlist(t *testing.T) {
	e := newEnv(t)
	ledger := e.f.ActiveTool(t, "erp", "post")
	a := e.f.ActiveAgent(t, "buyer", ledger.Tool)
	e.f.ActiveAgent(t, "reader")
	op := e.apply("otto", fleet.Request{Kind: "pause", Selector: fleet.Selector{Tool: "erp.post"}, Reason: "erp incident"})
	if !slices.Equal(versionIDs(op), []uuid.UUID{a.Version}) {
		t.Fatalf("tool selector = %+v", op)
	}
}

func TestOneRefusedVersionFailsTheWholeOperation(t *testing.T) {
	e := newEnv(t)
	buyer := e.f.ActiveAgent(t, "buyer")
	// rita authored this version's allowlist, so rita cannot grant it.
	own := e.f.NewAgent(t, "rita-owned")
	al := e.f.ID(t, "rita", `INSERT INTO eacp.agent_allowlists (tenant_id, agent_version_id, tool_ids)
		VALUES (eacp.current_tenant_id(), $1, '{}') RETURNING id`, own.Version)
	if err := e.f.Exec("ravi", `UPDATE eacp.agent_versions SET active_allowlist_id = $1 WHERE id = $2`, al, own.Version); err != nil {
		t.Fatal(err)
	}
	if err := e.f.Exec("ravi", `UPDATE eacp.agent_versions SET state = 'ACTIVE', state_reason = 'go live' WHERE id = $1`, own.Version); err != nil {
		t.Fatal(err)
	}
	pause := e.apply("otto", fleet.Request{Kind: "pause", Selector: fleet.Selector{All: true}, Reason: "drill"})
	_, err := e.s.Apply(context.Background(), e.as("rita"), fleet.Request{Kind: "resume", SourceOperationID: pause.ID, Reason: "end"})
	wantKind(t, err, registry.ErrForbidden)
	if e.state(buyer.Version) != "SUSPENDED" || e.state(own.Version) != "SUSPENDED" {
		t.Fatal("a refused resume changed a version")
	}
	e.apply("ravi", fleet.Request{Kind: "resume", SourceOperationID: pause.ID, Reason: "end"})
}

func TestQuarantineCoversEveryLiveVersionAndReleaseNeedsAnotherApprover(t *testing.T) {
	e := newEnv(t)
	a, v2 := rollbackAgent(t, e.f, "buyer")
	q := e.apply("otto", fleet.Request{Kind: "quarantine", Selector: fleet.Selector{AgentIDs: []uuid.UUID{a.Agent}}, Reason: "compromise"})
	if !slices.Equal(versionIDs(q), sorted(a.Version, v2)) {
		t.Fatalf("quarantine = %+v", q)
	}
	// With every version quarantined, there is nothing to roll back to.
	_, err := e.s.Apply(context.Background(), e.as("ravi"), fleet.Request{Kind: "rollback",
		Selector: fleet.Selector{AgentIDs: []uuid.UUID{a.Agent}}, Reason: "x"})
	wantKind(t, err, registry.ErrConflict)
	r := e.apply("ravi", fleet.Request{Kind: "release", SourceOperationID: q.ID, Reason: "cleared"})
	if len(r.Targets) != 2 || e.state(a.Version) != "SUSPENDED" || e.state(v2) != "SUSPENDED" {
		t.Fatalf("release = %+v", r)
	}
}

func TestRollbackActivatesThePreviousVersion(t *testing.T) {
	e := newEnv(t)
	a, v2 := rollbackAgent(t, e.f, "buyer")
	req := fleet.Request{Kind: "rollback", Selector: fleet.Selector{Agents: []string{"buyer"}}, Reason: "v2 regression"}
	op := e.apply("ravi", req)
	if len(op.Targets) != 2 || op.Targets[0].VersionID != v2 || op.Targets[0].To != "SUSPENDED" ||
		op.Targets[1].VersionID != a.Version || op.Targets[1].To != "ACTIVE" {
		t.Fatalf("rollback = %+v", op)
	}
	if e.state(a.Version) != "ACTIVE" || e.state(v2) != "SUSPENDED" {
		t.Fatal("rollback did not swap the versions")
	}
	// v1 is now the oldest ACTIVE version: there is no earlier one.
	_, err := e.s.Apply(context.Background(), e.as("ravi"), req)
	wantKind(t, err, registry.ErrConflict)
	// A named target must be older than the version it replaces.
	req.ToVersionID = v2
	_, err = e.s.Apply(context.Background(), e.as("ravi"), req)
	wantKind(t, err, registry.ErrConflict)
	req.ToVersionID = uuid.New()
	_, err = e.s.Apply(context.Background(), e.as("ravi"), req)
	wantKind(t, err, registry.ErrNotFound)
}

func TestFleetOperationsAreTenantScoped(t *testing.T) {
	e := newEnv(t)
	e.f.ActiveAgent(t, "buyer")
	other := e.f.ForTenant(t, pgtest.TenantB)
	foreign := other.ActiveAgent(t, "foreign")
	op := e.apply("otto", fleet.Request{Kind: "pause", Selector: fleet.Selector{All: true}, Reason: "drill"})
	if len(op.Targets) != 1 {
		t.Fatalf("pause all crossed tenants: %+v", op)
	}
	if s, _ := versionState(t, other, foreign.Version); s != "ACTIVE" {
		t.Fatalf("foreign version = %s", s)
	}
	b := fleet.New(other.App)
	_, err := b.Operation(context.Background(), registry.Actor{TenantID: other.Tenant, PrincipalID: other.P["audra"]}, op.ID)
	wantKind(t, err, registry.ErrNotFound)
	_, err = b.Apply(context.Background(), registry.Actor{TenantID: other.Tenant, PrincipalID: other.P["otto"]},
		fleet.Request{Kind: "pause", Selector: fleet.Selector{AgentIDs: []uuid.UUID{e.f.P["carol"]}}, Reason: "x"})
	wantKind(t, err, registry.ErrNotFound)
}

// adminExec runs sql as the superuser with triggers off, for fixtures the
// application may not create directly (an unresolved action outcome).
func adminExec(t *testing.T, f *registrytest.Fixture, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.Pool(t, f.DB.AdminDSN)
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, sql, args...)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func byName(items []fleet.AgentStatus, name string) fleet.AgentStatus {
	for _, it := range items {
		if it.Name == name {
			return it
		}
	}
	return fleet.AgentStatus{}
}

func TestFleetViewObservesContainmentDriftAndOpenWork(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ledger := e.f.ActiveTool(t, "erp", "post")
	crm := e.f.ActiveTool(t, "crm", "note")
	healthy := e.f.ActiveAgent(t, "healthy", ledger.Tool)
	e.f.ActiveAgent(t, "paused", ledger.Tool)
	killed := e.f.ActiveAgent(t, "killed", ledger.Tool)
	e.f.ActiveAgent(t, "drifted", crm.Tool)
	unknown := e.f.ActiveAgent(t, "unknown", ledger.Tool)
	group := e.f.ID(t, "alice", `INSERT INTO eacp.groups(tenant_id, name, display_name)
		VALUES (eacp.current_tenant_id(), 'empty', 'Empty') RETURNING id`)
	orphan := e.f.ID(t, "erin", `INSERT INTO eacp.agents
		(tenant_id, name, display_name, environment, risk_class, owner_group_id)
		VALUES (eacp.current_tenant_id(), 'orphan', 'Orphan', 'development', 'low', $1) RETURNING id`, group)

	e.apply("otto", fleet.Request{Kind: "pause", Selector: fleet.Selector{Agents: []string{"paused"}}, Reason: "x"})
	if err := e.f.Exec("otto", `SELECT eacp.set_kill('agent', $1, true, 'incident')`, killed.Agent); err != nil {
		t.Fatal(err)
	}
	if err := e.f.Exec("otto", `UPDATE eacp.tools SET quarantined_at = now(), quarantine_reason = 'drift' WHERE id = $1`, crm.Tool); err != nil {
		t.Fatal(err)
	}
	if err := e.f.Exec("otto", `UPDATE eacp.connector_circuits SET disabled = true, reason = 'maintenance' WHERE connector_id = $1`, crm.Connector); err != nil {
		t.Fatal(err)
	}
	e.f.ActivatePolicy(t, registrytest.AllowPolicy)
	act := e.f.QueuedAction(t, unknown.Version, "carol", "erp.post")
	adminExec(t, e.f, `UPDATE eacp.actions SET state = 'UNKNOWN_OUTCOME' WHERE id = $1`, act)
	e.f.QueuedAction(t, healthy.Version, "carol", "erp.post")

	agents, err := e.s.Agents(ctx, e.as("audra"), fleet.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, health string
		reason       string
	}{
		{"healthy", "ok", ""},
		{"paused", "contained", "no_active_version"},
		{"killed", "contained", "kill_active"},
		{"drifted", "degraded", "capability_drift"},
		{"drifted", "degraded", "circuit_open"},
		{"unknown", "degraded", "unknown_outcome"},
		{"orphan", "contained", "owner_unknown"},
	} {
		got := byName(agents, tc.name)
		if got.Health != tc.health || (tc.reason != "" && !slices.Contains(got.Reasons, tc.reason)) {
			t.Errorf("%s = %s %v, want %s with %q", tc.name, got.Health, got.Reasons, tc.health, tc.reason)
		}
	}
	h := byName(agents, "healthy")
	if h.ActiveVersion == nil || h.ActiveVersion.ID != healthy.Version || h.OpenActions["QUEUED"] != 1 ||
		len(h.Reasons) != 0 || !h.OwnerKnown {
		t.Errorf("healthy = %+v", h)
	}
	if k := byName(agents, "killed"); len(k.Kills) != 1 || k.Kills[0].Scope != "agent" {
		t.Errorf("killed kills = %+v", k.Kills)
	}
	d := byName(agents, "drifted")
	if len(d.Drift) != 1 || d.Drift[0].Tool != "crm.note" || d.Drift[0].Reason != "tool_quarantined" ||
		len(d.Circuits) != 1 || d.Circuits[0].State != "disabled" {
		t.Errorf("drifted = %+v", d)
	}
	if o := byName(agents, "orphan"); o.OwnerKnown || o.ID != orphan {
		t.Errorf("orphan = %+v", o)
	}

	filtered, err := e.s.Agents(ctx, e.as("audra"), fleet.Filter{Health: "degraded"})
	if err != nil || len(filtered) != 2 {
		t.Fatalf("degraded filter = %d agents, %v", len(filtered), err)
	}
	sum, err := e.s.Health(ctx, e.as("otto"), fleet.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Agents != 6 || sum.ByHealth["ok"] != 1 || sum.ByHealth["contained"] != 3 || sum.ByHealth["degraded"] != 2 ||
		sum.ByEnvironment["production"] != 5 || sum.UnknownOwner != 1 || sum.CapabilityDrift != 1 ||
		sum.ActiveKills != 1 || sum.OpenActions["UNKNOWN_OUTCOME"] != 1 || sum.OpenActions["QUEUED"] != 1 {
		t.Fatalf("summary = %+v", sum)
	}
}

// The read-only view cannot call eacp.action_capability_denial (it locks
// rows), so it repeats the rule. Both must agree for every allowlisted tool.
func TestFleetDriftAgreesWithTheCapabilityCheck(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ok := e.f.ActiveTool(t, "erp1", "post")
	quarantined := e.f.ActiveTool(t, "erp2", "void")
	revoked := e.f.ActiveTool(t, "erp3", "close")
	noContract := e.f.ID(t, "erin", `INSERT INTO eacp.tools (tenant_id, connector_id, name)
		VALUES (eacp.current_tenant_id(), $1, 'draft') RETURNING id`, ok.Connector)
	if err := e.f.Exec("otto", `UPDATE eacp.tools SET quarantined_at = now(), quarantine_reason = 'x' WHERE id = $1`, quarantined.Tool); err != nil {
		t.Fatal(err)
	}
	if err := e.f.Exec("otto", `UPDATE eacp.tool_contracts SET revoked_at = now(), revoke_reason = 'x' WHERE id = $1`, revoked.Contract); err != nil {
		t.Fatal(err)
	}
	a := e.f.ActiveAgent(t, "buyer", ok.Tool, quarantined.Tool, revoked.Tool, noContract)
	agents, err := e.s.Agents(ctx, e.as("audra"), fleet.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	view := map[string]string{}
	for _, d := range byName(agents, "buyer").Drift {
		view[d.Tool] = d.Reason
	}
	for ref, tool := range map[string]uuid.UUID{"erp1.post": ok.Tool, "erp2.void": quarantined.Tool,
		"erp3.close": revoked.Tool, "erp1.draft": noContract} {
		var want *string
		if err := storage.InTenantTx(ctx, e.f.App, e.f.Tenant.String(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT eacp.action_capability_denial($1, $2)`, a.Version, tool).Scan(&want)
		}); err != nil {
			t.Fatal(err)
		}
		got, has := view[ref]
		if (want == nil) == has || (want != nil && *want != got) {
			t.Errorf("%s: view %q (%v), capability check %v", ref, got, has, want)
		}
	}
}

// A resume skips an agent that has since activated another version, and
// still resumes the others.
func TestResumeSkipsAgentsWithAnotherActiveVersion(t *testing.T) {
	e := newEnv(t)
	a := e.f.ActiveAgent(t, "buyer")
	b := e.f.ActiveAgent(t, "seller")
	pause := e.apply("otto", fleet.Request{Kind: "pause", Selector: fleet.Selector{All: true}, Reason: "drill"})
	v2 := newVersion(t, e.f, a.Agent, "erin")
	if err := e.f.Exec("ravi", `UPDATE eacp.agent_versions SET state = 'ACTIVE', state_reason = 'hotfix' WHERE id = $1`, v2); err != nil {
		t.Fatal(err)
	}
	op := e.apply("ravi", fleet.Request{Kind: "resume", SourceOperationID: pause.ID, Reason: "cleared"})
	if !slices.Equal(versionIDs(op), []uuid.UUID{b.Version}) || len(op.Skipped) != 1 || op.Skipped[0].AgentID != a.Agent {
		t.Fatalf("resume = %+v", op)
	}
	if e.state(a.Version) != "SUSPENDED" || e.state(v2) != "ACTIVE" || e.state(b.Version) != "ACTIVE" {
		t.Fatal("resume reactivated a replaced version")
	}
}

func TestFleetViewMatchesEveryKillScope(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tool := e.f.ActiveTool(t, "erp", "post")
	group := e.f.ID(t, "alice", `INSERT INTO eacp.groups(tenant_id, name, display_name)
		VALUES (eacp.current_tenant_id(), 'buyers', 'Buyers') RETURNING id`)
	e.f.ID(t, "alice", `INSERT INTO eacp.group_memberships (tenant_id, group_id, principal_id)
		VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, group, e.f.P["carol"])
	agent := e.f.ID(t, "erin", `INSERT INTO eacp.agents
		(tenant_id, name, display_name, environment, risk_class, owner_group_id)
		VALUES (eacp.current_tenant_id(), 'buyer', 'Buyer', 'production', 'high', $1) RETURNING id`, group)
	v := e.f.ID(t, "erin", `INSERT INTO eacp.agent_versions (tenant_id, agent_id, runtime, code_ref)
		VALUES (eacp.current_tenant_id(), $1, 'python', 'git:1') RETURNING id`, agent)
	al := e.f.ID(t, "erin", `INSERT INTO eacp.agent_allowlists (tenant_id, agent_version_id, tool_ids)
		VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, v, []uuid.UUID{tool.Tool})
	if err := e.f.Exec("rita", `UPDATE eacp.agent_versions SET active_allowlist_id = $1 WHERE id = $2`, al, v); err != nil {
		t.Fatal(err)
	}
	if err := e.f.Exec("ravi", `UPDATE eacp.agent_versions SET state = 'ACTIVE', state_reason = 'go live' WHERE id = $1`, v); err != nil {
		t.Fatal(err)
	}
	e.f.ActiveAgent(t, "bystander")
	for scope, target := range map[string]uuid.UUID{"tenant": e.f.Tenant, "team": group, "agent": agent,
		"agent_version": v, "tool": tool.Tool, "connector": tool.Connector} {
		if err := e.f.Exec("otto", `SELECT eacp.set_kill($1, $2, true, 'drill')`, scope, target); err != nil {
			t.Fatal(err)
		}
		agents, err := e.s.Agents(ctx, e.as("audra"), fleet.Filter{})
		if err != nil {
			t.Fatal(err)
		}
		got := byName(agents, "buyer")
		if got.Health != fleet.HealthContained || len(got.Kills) != 1 || got.Kills[0].Scope != scope {
			t.Errorf("%s kill: buyer = %s %+v", scope, got.Health, got.Kills)
		}
		if by := byName(agents, "bystander"); (scope == "tenant") != (len(by.Kills) == 1) {
			t.Errorf("%s kill: bystander kills = %+v", scope, by.Kills)
		}
		if err := e.f.Exec("opal", `SELECT eacp.set_kill($1, $2, false, 'drill over')`, scope, target); err != nil {
			t.Fatal(err)
		}
	}
}

// The default rollback target is the newest SUSPENDED version older than
// the active one; a newer SUSPENDED version is never a rollback target.
func TestRollbackIgnoresNewerSuspendedVersions(t *testing.T) {
	e := newEnv(t)
	a, v2 := rollbackAgent(t, e.f, "buyer")
	v3 := newVersion(t, e.f, a.Agent, "erin")
	if err := e.f.Exec("otto", `UPDATE eacp.agent_versions SET state = 'QUARANTINED', state_reason = 'x' WHERE id = $1`, v3); err != nil {
		t.Fatal(err)
	}
	if err := e.f.Exec("ravi", `UPDATE eacp.agent_versions SET state = 'SUSPENDED', state_reason = 'x' WHERE id = $1`, v3); err != nil {
		t.Fatal(err)
	}
	op := e.apply("ravi", fleet.Request{Kind: "rollback", Selector: fleet.Selector{AgentIDs: []uuid.UUID{a.Agent}}, Reason: "regression"})
	if len(op.Targets) != 2 || op.Targets[0].VersionID != v2 || op.Targets[1].VersionID != a.Version {
		t.Fatalf("rollback = %+v", op)
	}
}
