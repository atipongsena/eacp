package registry_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/audit"
	"eacp/internal/identity"
	"eacp/internal/registry"
	"eacp/internal/registry/registrytest"
	"eacp/internal/storage"
	"eacp/internal/storage/pgtest"
)

type env struct {
	f   *registrytest.Fixture
	svc *registry.Service
	ctx context.Context
}

func newEnv(t *testing.T) env {
	f := registrytest.New(t)
	return env{f: f, svc: registry.New(f.App), ctx: context.Background()}
}

func (e env) as(name string) registry.Actor {
	return registry.Actor{TenantID: e.f.Tenant, PrincipalID: e.f.P[name]}
}

func (e env) auditCount(t *testing.T) int64 {
	t.Helper()
	var res audit.Result
	err := storage.InTenantReadTx(e.ctx, e.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		var err error
		res, err = audit.Verify(e.ctx, tx)
		return err
	})
	if err != nil {
		t.Fatalf("audit chain: %v", err)
	}
	return res.Count
}

func must[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func noErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantErr(t *testing.T, err, kind error) {
	t.Helper()
	if !errors.Is(err, kind) {
		t.Fatalf("err = %v, want %v", err, kind)
	}
}

var readContract = registry.Contract{
	SideEffects: []string{"READ_ONLY"}, IdempotencyMode: "none",
	ReconciliationLookup: "none", ReconciliationConsistency: "none", ProofStandard: "none", MaxAttempts: 3,
}

var createPOContract = registry.Contract{
	SideEffects:     []string{"IRREVERSIBLE_WRITE", "FINANCIAL"},
	IdempotencyMode: "native", IdempotencyKeyField: "Idempotency-Key", CorrelationField: "external_reference",
	ReconciliationLookup: "by_operation_key", ReconciliationConsistency: "strong", ProofStandard: "authoritative",
	NoEffectErrors: []string{"http_400_validation"}, MaxAttempts: 3,
}

// wired builds: connector erp, tool create_po (contract active), agent a1
// owned by carol with version 1 allowed erp.create_po and ACTIVE.
type wired struct {
	connector, tool, contract, agent, version, allowlist uuid.UUID
}

func (e env) wire(t *testing.T) wired {
	t.Helper()
	var w wired
	w.connector = must[registry.Connector](t)(e.svc.RegisterConnector(e.ctx, e.as("erin"), registry.NewConnector{
		Name: "erp", Protocol: "http", Endpoint: "http://fakeerp:8090", SecretRef: "erp"})).ID
	w.tool = must[uuid.UUID](t)(e.svc.RegisterTool(e.ctx, e.as("erin"), w.connector, "create_po"))
	w.contract = must[uuid.UUID](t)(e.svc.ProposeContract(e.ctx, e.as("erin"), w.tool, createPOContract))
	noErr(t, e.svc.ActivateContract(e.ctx, e.as("rita"), w.tool, w.contract))
	w.agent = must[registry.Agent](t)(e.svc.RegisterAgent(e.ctx, e.as("erin"), registry.NewAgent{
		Name: "procurement-bot", DisplayName: "Procurement bot", Environment: "production",
		RiskClass: "high", OwnerPrincipalID: e.f.P["carol"]})).ID
	w.version = must[registry.Version](t)(e.svc.RegisterVersion(e.ctx, e.as("erin"), w.agent, registry.NewVersion{
		Runtime: "python", CodeRef: "git:abc"})).ID
	w.allowlist = must[uuid.UUID](t)(e.svc.ProposeAllowlist(e.ctx, e.as("erin"), w.version, []string{"erp.create_po"}))
	noErr(t, e.svc.ActivateAllowlist(e.ctx, e.as("rita"), w.version, w.allowlist))
	noErr(t, e.svc.TransitionVersion(e.ctx, e.as("ravi"), w.version, registry.StateActive, "go live"))
	return w
}

func check(t *testing.T, e env, version uuid.UUID, tool string) (registry.Grant, registry.Denial) {
	t.Helper()
	var g registry.Grant
	var d registry.Denial
	err := storage.InTenantTx(e.ctx, e.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		var err error
		g, d, err = registry.CheckCapability(e.ctx, tx, version, tool)
		return err
	})
	if err != nil {
		t.Fatalf("CheckCapability: %v", err)
	}
	return g, d
}

func TestRegistryFlowGrantsCapabilityAndAuditsEveryChange(t *testing.T) {
	e := newEnv(t)
	before := e.auditCount(t)
	w := e.wire(t)
	if got := e.auditCount(t) - before; got != 9 {
		t.Fatalf("audit events = %d, want 9 (one per change)", got)
	}

	g, d := check(t, e, w.version, "erp.create_po")
	if d != "" || g.ToolID != w.tool || g.ContractID != w.contract || g.ContractVersion != 1 {
		t.Fatalf("grant = %+v, denial = %q", g, d)
	}

	detail := must[registry.AgentDetail](t)(e.svc.GetAgent(e.ctx, e.as("carol"), "procurement-bot"))
	if detail.OwnerPrincipalID != e.f.P["carol"] || len(detail.Versions) != 1 {
		t.Fatalf("detail = %+v", detail)
	}
	v := detail.Versions[0]
	if v.State != registry.StateActive || v.Number != 1 || !slices.Equal(v.AllowedTools, []string{"erp.create_po"}) {
		t.Fatalf("version = %+v", v)
	}
	byID := must[registry.AgentDetail](t)(e.svc.GetAgent(e.ctx, e.as("carol"), w.agent.String()))
	if byID.ID != w.agent {
		t.Fatal("GetAgent by id")
	}
}

func TestServiceMapsDatabaseRulesToErrors(t *testing.T) {
	e := newEnv(t)
	w := e.wire(t)

	// Two-person / role violations -> ErrForbidden.
	c2 := must[uuid.UUID](t)(e.svc.ProposeContract(e.ctx, e.as("rita"), w.tool, readContract))
	wantErr(t, e.svc.ActivateContract(e.ctx, e.as("rita"), w.tool, c2), registry.ErrForbidden)
	_, err := e.svc.RegisterAgent(e.ctx, e.as("otto"), registry.NewAgent{Name: "x1", DisplayName: "x",
		Environment: "production", RiskClass: "low", OwnerPrincipalID: e.f.P["carol"]})
	wantErr(t, err, registry.ErrForbidden)

	// Duplicates and illegal transitions -> ErrConflict.
	_, err = e.svc.RegisterAgent(e.ctx, e.as("erin"), registry.NewAgent{Name: "procurement-bot", DisplayName: "dup",
		Environment: "production", RiskClass: "low", OwnerPrincipalID: e.f.P["carol"]})
	wantErr(t, err, registry.ErrConflict)
	wantErr(t, e.svc.TransitionVersion(e.ctx, e.as("ravi"), w.version, registry.StateRegistered, "back"), registry.ErrConflict)

	// Invalid values -> ErrInvalid, naming the violated rule.
	bad := createPOContract
	bad.IdempotencyMode, bad.IdempotencyKeyField, bad.CorrelationField = "none", "", ""
	_, err = e.svc.ProposeContract(e.ctx, e.as("erin"), w.tool, bad)
	wantErr(t, err, registry.ErrInvalid)
	if err == nil || !strings.Contains(err.Error(), "unsafe_writes_single_attempt") {
		t.Fatalf("error %v does not name the violated rule", err)
	}
	_, err = e.svc.RegisterAgent(e.ctx, e.as("erin"), registry.NewAgent{Name: "no-owner", DisplayName: "x",
		Environment: "production", RiskClass: "low"})
	wantErr(t, err, registry.ErrInvalid)

	// Unknown references -> ErrNotFound.
	_, err = e.svc.ProposeAllowlist(e.ctx, e.as("erin"), w.version, []string{"erp.nope"})
	wantErr(t, err, registry.ErrNotFound)
	_, err = e.svc.GetAgent(e.ctx, e.as("erin"), "ghost")
	wantErr(t, err, registry.ErrNotFound)
}

func TestRejectedChangeWritesNoAuditEvent(t *testing.T) {
	e := newEnv(t)
	w := e.wire(t)
	before := e.auditCount(t)
	c2 := must[uuid.UUID](t)(e.svc.ProposeContract(e.ctx, e.as("rita"), w.tool, readContract))
	wantErr(t, e.svc.ActivateContract(e.ctx, e.as("rita"), w.tool, c2), registry.ErrForbidden)
	if got := e.auditCount(t) - before; got != 1 {
		t.Fatalf("audit events = %d, want 1 (the proposal only)", got)
	}
}

func TestCheckCapabilityDenials(t *testing.T) {
	e := newEnv(t)
	w := e.wire(t)
	other := must[uuid.UUID](t)(e.svc.RegisterTool(e.ctx, e.as("erin"), w.connector, "read_po"))

	cases := []struct {
		name  string
		setup func(t *testing.T) (uuid.UUID, string)
		want  registry.Denial
	}{
		{"unknown version", func(*testing.T) (uuid.UUID, string) { return uuid.New(), "erp.create_po" }, registry.DenyVersionNotActive},
		{"malformed tool ref", func(*testing.T) (uuid.UUID, string) { return w.version, "create_po" }, registry.DenyUnknownTool},
		{"unknown tool", func(*testing.T) (uuid.UUID, string) { return w.version, "erp.delete_po" }, registry.DenyUnknownTool},
		{"not in allowlist", func(*testing.T) (uuid.UUID, string) { return w.version, "erp.read_po" }, registry.DenyNotInAllowlist},
		{"no active contract", func(t *testing.T) (uuid.UUID, string) {
			v := must[registry.Version](t)(e.svc.RegisterVersion(e.ctx, e.as("erin"), w.agent, registry.NewVersion{Runtime: "py", CodeRef: "git:2"}))
			al := must[uuid.UUID](t)(e.svc.ProposeAllowlist(e.ctx, e.as("erin"), v.ID, []string{"erp.create_po", "erp.read_po"}))
			noErr(t, e.svc.ActivateAllowlist(e.ctx, e.as("rita"), v.ID, al))
			noErr(t, e.svc.TransitionVersion(e.ctx, e.as("otto"), w.version, registry.StateSuspended, "swap"))
			noErr(t, e.svc.TransitionVersion(e.ctx, e.as("ravi"), v.ID, registry.StateActive, "swap"))
			_ = other
			return v.ID, "erp.read_po"
		}, registry.DenyNoContract},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, tool := c.setup(t)
			if _, d := check(t, e, v, tool); d != c.want {
				t.Fatalf("denial = %q, want %q", d, c.want)
			}
		})
	}
}

func TestCheckCapabilityDeniesSuspendedRevokedAndDrifted(t *testing.T) {
	t.Run("suspended version", func(t *testing.T) {
		e := newEnv(t)
		w := e.wire(t)
		noErr(t, e.svc.TransitionVersion(e.ctx, e.as("otto"), w.version, registry.StateSuspended, "incident"))
		if _, d := check(t, e, w.version, "erp.create_po"); d != registry.DenyVersionNotActive {
			t.Fatalf("denial = %q", d)
		}
	})
	t.Run("revoked contract", func(t *testing.T) {
		e := newEnv(t)
		w := e.wire(t)
		noErr(t, e.svc.RevokeContract(e.ctx, e.as("otto"), w.contract, "vendor bug"))
		if _, d := check(t, e, w.version, "erp.create_po"); d != registry.DenyContractRevoked {
			t.Fatalf("denial = %q", d)
		}
	})
	t.Run("fingerprint drift", func(t *testing.T) {
		e := newEnv(t)
		w := e.wire(t)
		admin, err := pgx.Connect(e.ctx, e.f.DB.AdminDSN)
		if err != nil {
			t.Fatal(err)
		}
		defer admin.Close(e.ctx)
		// Someone with superuser access repoints the connector, disabling
		// triggers so that not even the audit journal records it.
		if _, err := admin.Exec(e.ctx, "SET session_replication_role = replica"); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(e.ctx, `UPDATE eacp.connectors SET endpoint = 'http://attacker:80' WHERE id = $1`, w.connector); err != nil {
			t.Fatal(err)
		}
		if _, d := check(t, e, w.version, "erp.create_po"); d != registry.DenyFingerprintMismatch {
			t.Fatalf("denial = %q", d)
		}
	})
}

// ADR-004 principle 7: a guard that read the registry FOR SHARE holds off a
// concurrent capability-reducing change until the guarded transaction ends.
func TestCapabilityCheckSerialisesWithSuspension(t *testing.T) {
	e := newEnv(t)
	w := e.wire(t)
	tx, err := e.f.App.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(e.ctx)
	if _, err := tx.Exec(e.ctx, `SELECT set_config('app.tenant_id', $1, true)`, pgtest.TenantA); err != nil {
		t.Fatal(err)
	}
	if _, d, err := registry.CheckCapability(e.ctx, tx, w.version, "erp.create_po"); err != nil || d != "" {
		t.Fatalf("check: %q %v", d, err)
	}

	done := make(chan error, 1)
	go func() {
		done <- e.svc.TransitionVersion(e.ctx, e.as("otto"), w.version, registry.StateSuspended, "incident")
	}()
	select {
	case err := <-done:
		t.Fatalf("suspension completed (%v) while a guarded transaction held the version", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		noErr(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("suspension never completed after the guard committed")
	}
}

func TestPrincipalGrantAndCredentialLifecycle(t *testing.T) {
	e := newEnv(t)
	dave := must[registry.Principal](t)(e.svc.CreatePrincipal(e.ctx, e.as("alice"), registry.NewPrincipal{
		Kind: "human", Name: "dave", Subject: "Dave@Tenant-A.test", DisplayName: "Dave"}))
	if dave.Subject != "dave@tenant-a.test" {
		t.Fatalf("subject not canonicalised: %q", dave.Subject)
	}
	g := must[uuid.UUID](t)(e.svc.ProposeRole(e.ctx, e.as("alice"), dave.ID, "registry_editor"))
	wantErr(t, e.svc.ApproveRole(e.ctx, e.as("alice"), g), registry.ErrForbidden)
	noErr(t, e.svc.ApproveRole(e.ctx, e.as("bob"), g))

	credID := uuid.New()
	key, hash, _ := identity.NewKey(identity.KindPrincipal, e.f.Tenant, credID)
	noErr(t, e.svc.ProposeCredential(e.ctx, e.as("alice"), registry.NewCredential{
		ID: credID, Kind: identity.KindPrincipal, PrincipalID: dave.ID, Hash: hash, ExpiresAt: time.Now().Add(24 * time.Hour)}))
	noErr(t, e.svc.ApproveCredential(e.ctx, e.as("bob"), credID))
	caller, err := identity.Authenticate(e.ctx, e.f.App, key)
	if err != nil || caller.PrincipalID != dave.ID || !caller.HasRole("registry_editor") {
		t.Fatalf("caller = %+v, err = %v", caller, err)
	}

	noErr(t, e.svc.RevokeRole(e.ctx, e.as("bob"), g, "moved team"))
	noErr(t, e.svc.RevokeCredential(e.ctx, e.as("alice"), credID, "offboarding"))
	if _, err := identity.Authenticate(e.ctx, e.f.App, key); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("revoked key authenticated: %v", err)
	}
	noErr(t, e.svc.DisablePrincipal(e.ctx, e.as("alice"), dave.ID, "left"))
}

func TestGroupsAndMemberships(t *testing.T) {
	e := newEnv(t)
	g := must[uuid.UUID](t)(e.svc.CreateGroup(e.ctx, e.as("alice"), "finance", "Finance"))
	m := must[uuid.UUID](t)(e.svc.AddMember(e.ctx, e.as("alice"), g, e.f.P["carol"]))
	noErr(t, e.svc.RemoveMember(e.ctx, e.as("bob"), m, "moved"))
	a := must[registry.Agent](t)(e.svc.RegisterAgent(e.ctx, e.as("erin"), registry.NewAgent{Name: "team-bot",
		DisplayName: "Team bot", Environment: "staging", RiskClass: "medium", OwnerGroupID: g}))
	if a.OwnerGroupID != g {
		t.Fatal("group owner not recorded")
	}
}

func TestSchedulerSettingsUseRegistryRolesAndBounds(t *testing.T) {
	e := newEnv(t)
	g := must[uuid.UUID](t)(e.svc.CreateGroupWeighted(e.ctx, e.as("alice"), "priority-team", "Priority", 3))
	var weight int
	noErr(t, storage.InTenantTx(e.ctx, e.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `SELECT schedule_weight FROM eacp.groups WHERE id = $1`, g).Scan(&weight)
	}))
	if weight != 3 {
		t.Fatalf("group weight = %d, want 3", weight)
	}
	wantState(t, e.f.Exec("alice", `UPDATE eacp.groups SET schedule_weight = 10 WHERE id = $1`, g), sqlForbidden)
	_, err := e.svc.CreateGroupWeighted(e.ctx, e.as("alice"), "too-heavy", "Too heavy", 11)
	wantErr(t, err, registry.ErrInvalid)
	_, err = e.svc.CreateGroupWeighted(e.ctx, e.as("erin"), "unauthorized", "Unauthorized", 2)
	wantErr(t, err, registry.ErrForbidden)
	w := e.wire(t)
	c := readContract
	c.SchedulePriority = 7
	id := must[uuid.UUID](t)(e.svc.ProposeContract(e.ctx, e.as("erin"), w.tool, c))
	var priority int
	noErr(t, storage.InTenantTx(e.ctx, e.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `SELECT schedule_priority FROM eacp.tool_contracts WHERE id = $1`, id).Scan(&priority)
	}))
	if priority != 7 {
		t.Fatalf("contract priority = %d, want 7", priority)
	}
	wantState(t, e.f.Exec("erin", `UPDATE eacp.tool_contracts SET schedule_priority = 9 WHERE id = $1`, id), sqlForbidden)
	wantState(t, e.f.Exec("erin", `UPDATE eacp.tool_contracts SET max_inflight = NULL WHERE id = $1`, id), sqlForbidden)
	c.SchedulePriority = 10
	_, err = e.svc.ProposeContract(e.ctx, e.as("erin"), w.tool, c)
	wantErr(t, err, registry.ErrInvalid)
}

func TestListingsAreTenantScoped(t *testing.T) {
	e := newEnv(t)
	e.wire(t)
	agents := must[[]registry.Agent](t)(e.svc.ListAgents(e.ctx, e.as("carol")))
	if len(agents) != 1 || agents[0].Name != "procurement-bot" {
		t.Fatalf("agents = %+v", agents)
	}
	conns := must[[]registry.Connector](t)(e.svc.ListConnectors(e.ctx, e.as("carol")))
	if len(conns) != 1 || !slices.Equal(conns[0].Tools, []string{"create_po"}) {
		t.Fatalf("connectors = %+v", conns)
	}
	outsider := registry.Actor{TenantID: uuid.MustParse(pgtest.TenantB), PrincipalID: e.f.P["carol"]}
	if got, err := e.svc.ListAgents(e.ctx, outsider); err != nil || len(got) != 0 {
		t.Fatalf("tenant B sees %d agents (err %v)", len(got), err)
	}
}

func TestBackpressureSettingsAndConnectorCircuit(t *testing.T) {
	e := newEnv(t)
	w := e.wire(t)
	c := createPOContract
	c.MaxQueued, c.RetryMaxElapsedMS = 50, 60000
	c.CostUnit, c.CostAmountField, c.RetryMaxCost = "THB", "amount", "1500.5"
	id := must[uuid.UUID](t)(e.svc.ProposeContract(e.ctx, e.as("erin"), w.tool, c))
	var queued, elapsed int
	var cost string
	noErr(t, storage.InTenantTx(e.ctx, e.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `SELECT max_queued, retry_max_elapsed_ms, retry_max_cost::text
			FROM eacp.tool_contracts WHERE id = $1`, id).Scan(&queued, &elapsed, &cost)
	}))
	if queued != 50 || elapsed != 60000 || cost != "1500.500000" {
		t.Fatalf("contract limits = %d %d %s", queued, elapsed, cost)
	}
	wantState(t, e.f.Exec("erin", `UPDATE eacp.tool_contracts SET max_queued = 1 WHERE id = $1`, id), sqlForbidden)
	c.CostUnit, c.CostAmountField = "", ""
	_, err := e.svc.ProposeContract(e.ctx, e.as("erin"), w.tool, c)
	wantErr(t, err, registry.ErrInvalid) // a retry cost needs a budget unit

	// Every connector has a circuit, closed when registered.
	got := must[registry.Circuit](t)(e.svc.ConnectorCircuit(e.ctx, e.as("audra"), w.connector))
	if got.Open || got.Disabled || got.Reason != "registered" {
		t.Fatalf("new circuit = %+v", got)
	}
	_, err = e.svc.SetConnectorDisabled(e.ctx, e.as("erin"), w.connector, true, "not mine")
	wantErr(t, err, registry.ErrForbidden)
	_, err = e.svc.SetConnectorDisabled(e.ctx, e.as("otto"), w.connector, true, "")
	wantErr(t, err, registry.ErrInvalid)
	_, err = e.svc.SetConnectorDisabled(e.ctx, e.as("otto"), uuid.New(), true, "unknown")
	wantErr(t, err, registry.ErrNotFound)
	got = must[registry.Circuit](t)(e.svc.SetConnectorDisabled(e.ctx, e.as("otto"), w.connector, true, "vendor incident"))
	if !got.Open || !got.Disabled || got.ChangedBy == nil || *got.ChangedBy != e.f.P["otto"] {
		t.Fatalf("disabled circuit = %+v", got)
	}
	got = must[registry.Circuit](t)(e.svc.SetConnectorDisabled(e.ctx, e.as("opal"), w.connector, false, "recovered"))
	if got.Open || got.Disabled || got.Reason != "recovered" {
		t.Fatalf("enabled circuit = %+v", got)
	}
}
