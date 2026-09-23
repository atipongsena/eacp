package registry_test

// Schema-level tests: every rule here is enforced by PostgreSQL itself
// (ADR-003 §8), so each test issues raw SQL as the application role and
// bypasses the Go layer entirely.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"eacp/internal/registry/registrytest"
	"eacp/internal/storage"
	"eacp/internal/storage/pgtest"
)

const (
	sqlForbidden    = "42501" // insufficient_privilege: role / two-person / SoD
	sqlBadState     = "55000" // object_not_in_prerequisite_state
	sqlCheck        = "23514" // check_violation
	sqlForeignKey   = "23503"
	sqlUnique       = "23505"
	proposeGrantSQL = `INSERT INTO eacp.role_grants (tenant_id, principal_id, role)
		VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`
)

func wantState(t *testing.T, err error, codes ...string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("err = %v, want SQLSTATE %v", err, codes)
	}
	for _, c := range codes {
		if pgErr.Code == c {
			return
		}
	}
	t.Fatalf("SQLSTATE %s (%s), want %v", pgErr.Code, pgErr.Message, codes)
}

func ok(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// ---------- principals and role grants ----------

func TestRoleGrantNeedsTwoDistinctAdminsNeitherBeingGrantee(t *testing.T) {
	f := registrytest.New(t)
	g := f.ID(t, "alice", proposeGrantSQL, f.P["carol"], "registry_editor")

	wantState(t, f.Exec("alice", `UPDATE eacp.role_grants SET approved_at = now() WHERE id = $1`, g), sqlForbidden)
	wantState(t, f.Exec("erin", `UPDATE eacp.role_grants SET approved_at = now() WHERE id = $1`, g), sqlForbidden)
	ok(t, f.Exec("bob", `UPDATE eacp.role_grants SET approved_at = now() WHERE id = $1`, g))

	var by uuid.UUID
	storage.InTenantTx(context.Background(), f.Owner, pgtest.TenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT approved_by FROM eacp.role_grants WHERE id = $1`, g).Scan(&by)
	})
	if by != f.P["bob"] {
		t.Fatalf("approved_by = %s, want bob (set by the database from the actor)", by)
	}
}

func TestNobodyProposesOrApprovesTheirOwnRole(t *testing.T) {
	f := registrytest.New(t)
	_, err := f.TryID("alice", proposeGrantSQL, f.P["alice"], "registry_approver")
	wantState(t, err, sqlForbidden)

	g := f.ID(t, "bob", proposeGrantSQL, f.P["alice"], "registry_approver")
	wantState(t, f.Exec("alice", `UPDATE eacp.role_grants SET approved_at = now() WHERE id = $1`, g), sqlForbidden)
}

func TestOnlyAdminsProposeGrants(t *testing.T) {
	f := registrytest.New(t)
	for _, actor := range []string{"erin", "rita", "otto", "carol", "ci", ""} {
		_, err := f.TryID(actor, proposeGrantSQL, f.P["carol"], "auditor")
		wantState(t, err, sqlForbidden)
	}
}

func TestServicePrincipalsCannotHoldPrivilegedRoles(t *testing.T) {
	f := registrytest.New(t)
	for _, role := range []string{"admin", "registry_approver", "operator", "approver"} {
		_, err := f.TryID("alice", proposeGrantSQL, f.P["ci"], role)
		wantState(t, err, sqlForbidden)
	}
}

func TestUnapprovedAndRevokedGrantsConferNothing(t *testing.T) {
	f := registrytest.New(t)
	tool := f.ActiveTool(t, "erp", "read")
	agent := f.NewAgent(t, "a1")
	al := f.ID(t, "erin", `INSERT INTO eacp.agent_allowlists (tenant_id, agent_version_id, tool_ids)
		VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, agent.Version, []uuid.UUID{tool.Tool})

	// carol has a proposed-but-unapproved registry_approver grant.
	f.ID(t, "alice", proposeGrantSQL, f.P["carol"], "registry_approver")
	wantState(t, f.Exec("carol", `UPDATE eacp.agent_versions SET active_allowlist_id = $1 WHERE id = $2`, al, agent.Version), sqlForbidden)

	// rita's grant is revoked by one admin.
	ok(t, f.Exec("alice", `UPDATE eacp.role_grants SET revoked_at = now(), revoke_reason = 'left team'
		WHERE principal_id = $1 AND role = 'registry_approver'`, f.P["rita"]))
	wantState(t, f.Exec("rita", `UPDATE eacp.agent_versions SET active_allowlist_id = $1 WHERE id = $2`, al, agent.Version), sqlForbidden)
	ok(t, f.Exec("ravi", `UPDATE eacp.agent_versions SET active_allowlist_id = $1 WHERE id = $2`, al, agent.Version))
}

func TestRevocationIsMonotonic(t *testing.T) {
	f := registrytest.New(t)
	revoke := `UPDATE eacp.role_grants SET revoked_at = now(), revoke_reason = 'x' WHERE principal_id = $1`
	ok(t, f.Exec("alice", revoke, f.P["erin"]))
	wantState(t, f.Exec("alice", `UPDATE eacp.role_grants SET revoked_at = NULL WHERE principal_id = $1`, f.P["erin"]), sqlBadState)
	wantState(t, f.Exec("alice", `UPDATE eacp.role_grants SET revoke_reason = 'other' WHERE principal_id = $1`, f.P["erin"]), sqlBadState)
}

func TestRevocationNeedsReason(t *testing.T) {
	f := registrytest.New(t)
	wantState(t, f.Exec("alice", `UPDATE eacp.role_grants SET revoked_at = now() WHERE principal_id = $1`, f.P["erin"]), sqlCheck)
}

func TestDisabledPrincipalLosesAllPower(t *testing.T) {
	f := registrytest.New(t)
	ok(t, f.Exec("alice", `UPDATE eacp.principals SET disabled_at = now(), disable_reason = 'offboarded' WHERE id = $1`, f.P["bob"]))
	_, err := f.TryID("bob", proposeGrantSQL, f.P["carol"], "auditor")
	wantState(t, err, sqlForbidden)
	wantState(t, f.Exec("alice", `UPDATE eacp.principals SET disabled_at = NULL WHERE id = $1`, f.P["bob"]), sqlBadState)
}

func TestOneHumanOnePrincipal(t *testing.T) {
	f := registrytest.New(t)
	_, err := f.TryID("alice", `INSERT INTO eacp.principals (tenant_id, kind, name, subject, display_name)
		VALUES (eacp.current_tenant_id(), 'human', 'alice-2', 'alice@tenant-a.test', 'Alice again') RETURNING id`)
	wantState(t, err, sqlUnique)
	_, err = f.TryID("alice", `INSERT INTO eacp.principals (tenant_id, kind, name, display_name)
		VALUES (eacp.current_tenant_id(), 'human', 'nosubject', 'No subject') RETURNING id`)
	wantState(t, err, sqlCheck)
}

func TestAppRoleCannotActWithoutActor(t *testing.T) {
	f := registrytest.New(t)
	_, err := f.TryID("", `INSERT INTO eacp.principals (tenant_id, kind, name, subject, display_name)
		VALUES (eacp.current_tenant_id(), 'human', 'mallory', 'mallory@x.test', 'M') RETURNING id`)
	wantState(t, err, sqlForbidden)
	_, err = f.TryID("", `INSERT INTO eacp.role_grants (tenant_id, principal_id, role, approved_at)
		VALUES (eacp.current_tenant_id(), $1, 'admin', now()) RETURNING id`, f.P["carol"])
	wantState(t, err, sqlForbidden)
}

func TestAppRoleCannotInsertPreApprovedGrant(t *testing.T) {
	f := registrytest.New(t)
	_, err := f.TryID("alice", `INSERT INTO eacp.role_grants (tenant_id, principal_id, role, approved_at)
		VALUES (eacp.current_tenant_id(), $1, 'admin', now()) RETURNING id`, f.P["carol"])
	wantState(t, err, sqlForbidden)
}

func TestGroupMembershipIsAdminOnly(t *testing.T) {
	f := registrytest.New(t)
	_, err := f.TryID("erin", `INSERT INTO eacp.groups (tenant_id, name, display_name)
		VALUES (eacp.current_tenant_id(), 'finance', 'Finance') RETURNING id`)
	wantState(t, err, sqlForbidden)
	g := f.ID(t, "alice", `INSERT INTO eacp.groups (tenant_id, name, display_name)
		VALUES (eacp.current_tenant_id(), 'finance', 'Finance') RETURNING id`)
	add := `INSERT INTO eacp.group_memberships (tenant_id, group_id, principal_id)
		VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`
	_, err = f.TryID("erin", add, g, f.P["carol"])
	wantState(t, err, sqlForbidden)
	f.ID(t, "alice", add, g, f.P["carol"])
	_, err = f.TryID("alice", add, g, f.P["carol"])
	wantState(t, err, sqlUnique)
	ok(t, f.Exec("bob", `UPDATE eacp.group_memberships SET removed_at = now(), remove_reason = 'moved' WHERE group_id = $1`, g))
	wantState(t, f.Exec("bob", `UPDATE eacp.group_memberships SET removed_at = NULL WHERE group_id = $1`, g), sqlBadState)
	f.ID(t, "alice", add, g, f.P["carol"]) // re-adding creates a new row
}

// ---------- agents and versions ----------

func TestAgentRowsAreImmutable(t *testing.T) {
	f := registrytest.New(t)
	a := f.NewAgent(t, "a1")
	wantState(t, f.Exec("erin", `UPDATE eacp.agents SET owner_principal_id = $1 WHERE id = $2`, f.P["erin"], a.Agent), sqlForbidden)
	wantState(t, f.Exec("erin", `DELETE FROM eacp.agents WHERE id = $1`, a.Agent), sqlForbidden)
}

func TestAgentNeedsExactlyOneOwner(t *testing.T) {
	f := registrytest.New(t)
	g := f.ID(t, "alice", `INSERT INTO eacp.groups (tenant_id, name, display_name)
		VALUES (eacp.current_tenant_id(), 'ops', 'Ops') RETURNING id`)
	ins := `INSERT INTO eacp.agents (tenant_id, name, display_name, environment, risk_class, owner_principal_id, owner_group_id)
		VALUES (eacp.current_tenant_id(), $1, 'x', $2, 'low', $3, $4) RETURNING id`
	for _, env := range []string{"development", "staging", "production"} {
		_, err := f.TryID("erin", ins, "none-"+env, env, nil, nil)
		wantState(t, err, sqlCheck)
	}
	_, err := f.TryID("erin", ins, "both", "production", f.P["carol"], g)
	wantState(t, err, sqlCheck)
	f.ID(t, "erin", ins, "group-owned", "production", nil, g)
}

func TestOnlyRegistryEditorsRegister(t *testing.T) {
	f := registrytest.New(t)
	_, err := f.TryID("rita", `INSERT INTO eacp.agents (tenant_id, name, display_name, environment, risk_class, owner_principal_id)
		VALUES (eacp.current_tenant_id(), 'xx', 'x', 'production', 'low', $1) RETURNING id`, f.P["carol"])
	wantState(t, err, sqlForbidden)
	_, err = f.TryID("ci", `INSERT INTO eacp.agents (tenant_id, name, display_name, environment, risk_class, owner_principal_id)
		VALUES (eacp.current_tenant_id(), 'xy', 'x', 'production', 'low', $1) RETURNING id`, f.P["carol"])
	wantState(t, err, sqlForbidden)
	f.ID(t, "erin", `INSERT INTO eacp.agents (tenant_id, name, display_name, environment, risk_class, owner_principal_id)
		VALUES (eacp.current_tenant_id(), 'xx', 'x', 'production', 'low', $1) RETURNING id`, f.P["carol"])
}

func TestVersionNumbersAreAssignedByDatabase(t *testing.T) {
	f := registrytest.New(t)
	a := f.NewAgent(t, "a1")
	v2 := f.ID(t, "erin", `INSERT INTO eacp.agent_versions (tenant_id, agent_id, version, runtime, code_ref)
		VALUES (eacp.current_tenant_id(), $1, 99, 'python', 'git:def') RETURNING id`, a.Agent)
	var n int
	storage.InTenantTx(context.Background(), f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT version FROM eacp.agent_versions WHERE id = $1`, v2).Scan(&n)
	})
	if n != 2 {
		t.Fatalf("version = %d, want 2", n)
	}
}

func proposeAllowlist(t *testing.T, f *registrytest.Fixture, actor string, version uuid.UUID, tools ...uuid.UUID) uuid.UUID {
	t.Helper()
	return f.ID(t, actor, `INSERT INTO eacp.agent_allowlists (tenant_id, agent_version_id, tool_ids)
		VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, version, tools)
}

const (
	setAllowlist = `UPDATE eacp.agent_versions SET active_allowlist_id = $1 WHERE id = $2`
	transition   = `UPDATE eacp.agent_versions SET state = $1, state_reason = $2 WHERE id = $3`
)

func TestAllowlistActivationIsTwoPerson(t *testing.T) {
	f := registrytest.New(t)
	tool := f.ActiveTool(t, "erp", "read")
	a := f.NewAgent(t, "a1")
	byRita := proposeAllowlist(t, f, "rita", a.Version, tool.Tool) // approvers may also author
	wantState(t, f.Exec("rita", setAllowlist, byRita, a.Version), sqlForbidden)
	wantState(t, f.Exec("erin", setAllowlist, byRita, a.Version), sqlForbidden) // editor cannot activate
	ok(t, f.Exec("ravi", setAllowlist, byRita, a.Version))
	wantState(t, f.Exec("ravi", `UPDATE eacp.agent_versions SET active_allowlist_id = NULL WHERE id = $1`, a.Version), sqlBadState)
}

func TestAllowlistBelongsToItsVersionAndIsImmutable(t *testing.T) {
	f := registrytest.New(t)
	tool := f.ActiveTool(t, "erp", "read")
	a, b := f.NewAgent(t, "a1"), f.NewAgent(t, "a2")
	alB := proposeAllowlist(t, f, "erin", b.Version, tool.Tool)
	wantState(t, f.Exec("rita", setAllowlist, alB, a.Version), sqlBadState)
	wantState(t, f.Exec("erin", `UPDATE eacp.agent_allowlists SET tool_ids = '{}' WHERE id = $1`, alB), sqlForbidden)
}

func TestAllowlistRejectsUnknownOrForeignTools(t *testing.T) {
	f := registrytest.New(t)
	a := f.NewAgent(t, "a1")
	_, err := f.TryID("erin", `INSERT INTO eacp.agent_allowlists (tenant_id, agent_version_id, tool_ids)
		VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, a.Version, []uuid.UUID{uuid.New()})
	wantState(t, err, sqlForeignKey)
}

func TestVersionActivationRules(t *testing.T) {
	f := registrytest.New(t)
	tool := f.ActiveTool(t, "erp", "read")
	a := f.NewAgent(t, "a1") // created by erin
	wantState(t, f.Exec("ravi", transition, "ACTIVE", "no allowlist yet", a.Version), sqlBadState)

	al := proposeAllowlist(t, f, "rita", a.Version, tool.Tool)
	ok(t, f.Exec("ravi", setAllowlist, al, a.Version))
	wantState(t, f.Exec("rita", transition, "ACTIVE", "rita authored the allowlist", a.Version), sqlForbidden)
	wantState(t, f.Exec("otto", transition, "ACTIVE", "operator cannot grant", a.Version), sqlForbidden)
	wantState(t, f.Exec("ravi", transition, "ACTIVE", "", a.Version), sqlCheck)
	ok(t, f.Exec("ravi", transition, "ACTIVE", "go live", a.Version))
}

func TestVersionCreatorCannotActivate(t *testing.T) {
	f := registrytest.New(t)
	tool := f.ActiveTool(t, "erp", "read")
	agent := f.ID(t, "erin", `INSERT INTO eacp.agents (tenant_id, name, display_name, environment, risk_class, owner_principal_id)
		VALUES (eacp.current_tenant_id(), 'a1', 'a1', 'production', 'low', $1) RETURNING id`, f.P["carol"])
	// Give rita (an approver) the editor role too, so she can create a version.
	f.ID(t, "alice", proposeGrantSQL, f.P["rita"], "registry_editor")
	ok(t, f.Exec("bob", `UPDATE eacp.role_grants SET approved_at = now() WHERE principal_id = $1 AND role = 'registry_editor'`, f.P["rita"]))
	v := f.ID(t, "rita", `INSERT INTO eacp.agent_versions (tenant_id, agent_id, runtime, code_ref)
		VALUES (eacp.current_tenant_id(), $1, 'go', 'git:1') RETURNING id`, agent)
	al := proposeAllowlist(t, f, "erin", v, tool.Tool)
	ok(t, f.Exec("rita", setAllowlist, al, v))
	wantState(t, f.Exec("rita", transition, "ACTIVE", "own version", v), sqlForbidden)
	ok(t, f.Exec("ravi", transition, "ACTIVE", "second person", v))
}

func TestAtMostOneActiveVersionPerAgent(t *testing.T) {
	f := registrytest.New(t)
	tool := f.ActiveTool(t, "erp", "read")
	a := f.ActiveAgent(t, "a1", tool.Tool)
	v2 := f.ID(t, "erin", `INSERT INTO eacp.agent_versions (tenant_id, agent_id, runtime, code_ref)
		VALUES (eacp.current_tenant_id(), $1, 'python', 'git:v2') RETURNING id`, a.Agent)
	ok(t, f.Exec("rita", setAllowlist, proposeAllowlist(t, f, "erin", v2, tool.Tool), v2))
	wantState(t, f.Exec("ravi", transition, "ACTIVE", "second active", v2), sqlUnique)
	ok(t, f.Exec("otto", transition, "SUSPENDED", "rolling", a.Version))
	ok(t, f.Exec("ravi", transition, "ACTIVE", "now alone", v2))
}

func TestLifecycleTransitionTable(t *testing.T) {
	allowed := map[[2]string]string{ // from,to -> an actor allowed to do it
		{"REGISTERED", "RETIRED"}:     "rita",
		{"REGISTERED", "QUARANTINED"}: "otto",
		{"REGISTERED", "REVOKED"}:     "otto",
		{"ACTIVE", "SUSPENDED"}:       "otto",
		{"ACTIVE", "RETIRED"}:         "rita",
		{"ACTIVE", "QUARANTINED"}:     "otto",
		{"ACTIVE", "REVOKED"}:         "otto",
		{"SUSPENDED", "ACTIVE"}:       "ravi",
		{"SUSPENDED", "RETIRED"}:      "rita",
		{"SUSPENDED", "QUARANTINED"}:  "otto",
		{"SUSPENDED", "REVOKED"}:      "otto",
		{"QUARANTINED", "SUSPENDED"}:  "ravi",
		{"QUARANTINED", "REVOKED"}:    "otto",
	}
	// path from REGISTERED to each state, using valid transitions.
	reach := map[string][]string{
		"REGISTERED":  nil,
		"ACTIVE":      {"ACTIVE"},
		"SUSPENDED":   {"ACTIVE", "SUSPENDED"},
		"QUARANTINED": {"QUARANTINED"},
		"RETIRED":     {"RETIRED"},
		"REVOKED":     {"REVOKED"},
	}
	actorFor := func(from, to string) string {
		if a, ok := allowed[[2]string{from, to}]; ok {
			return a
		}
		return "ravi"
	}
	states := []string{"REGISTERED", "ACTIVE", "SUSPENDED", "QUARANTINED", "RETIRED", "REVOKED"}

	f := registrytest.New(t)
	tool := f.ActiveTool(t, "erp", "read")
	for _, from := range states {
		for _, to := range states {
			if from == to {
				continue
			}
			t.Run(from+"->"+to, func(t *testing.T) {
				a := f.NewAgent(t, "lc-"+uuid.NewString()[:8])
				ok(t, f.Exec("rita", setAllowlist, proposeAllowlist(t, f, "erin", a.Version, tool.Tool), a.Version))
				cur := "REGISTERED"
				for _, s := range reach[from] {
					ok(t, f.Exec(actorFor(cur, s), transition, s, "setup", a.Version))
					cur = s
				}
				err := f.Exec(actorFor(from, to), transition, to, "test", a.Version)
				if _, legal := allowed[[2]string{from, to}]; legal || (from == "REGISTERED" && to == "ACTIVE") {
					ok(t, err)
				} else {
					wantState(t, err, sqlBadState)
				}
			})
		}
	}
}

func TestQuarantineReleaseNeedsSecondPerson(t *testing.T) {
	f := registrytest.New(t)
	tool := f.ActiveTool(t, "erp", "read")
	a := f.ActiveAgent(t, "a1", tool.Tool)
	ok(t, f.Exec("rita", transition, "QUARANTINED", "suspicious", a.Version))
	wantState(t, f.Exec("rita", transition, "SUSPENDED", "self release", a.Version), sqlForbidden)
	wantState(t, f.Exec("otto", transition, "SUSPENDED", "operator release", a.Version), sqlForbidden)
	ok(t, f.Exec("ravi", transition, "SUSPENDED", "reviewed", a.Version))
}

func TestTerminalVersionsAreFrozen(t *testing.T) {
	f := registrytest.New(t)
	tool := f.ActiveTool(t, "erp", "read")
	a := f.ActiveAgent(t, "a1", tool.Tool)
	ok(t, f.Exec("otto", transition, "REVOKED", "compromised", a.Version))
	wantState(t, f.Exec("ravi", transition, "ACTIVE", "undo", a.Version), sqlBadState)
	_, err := f.TryID("erin", `INSERT INTO eacp.agent_allowlists (tenant_id, agent_version_id, tool_ids)
		VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, a.Version, []uuid.UUID{tool.Tool})
	wantState(t, err, sqlBadState)
}

func TestStateAndPointerCannotChangeTogether(t *testing.T) {
	f := registrytest.New(t)
	tool := f.ActiveTool(t, "erp", "read")
	a := f.NewAgent(t, "a1")
	al := proposeAllowlist(t, f, "erin", a.Version, tool.Tool)
	wantState(t, f.Exec("ravi", `UPDATE eacp.agent_versions SET active_allowlist_id = $1, state = 'ACTIVE', state_reason = 'x' WHERE id = $2`, al, a.Version), sqlBadState)
}

// ---------- connectors, tools, contracts ----------

func TestConnectorsAndToolsAreImmutable(t *testing.T) {
	f := registrytest.New(t)
	tl := f.ActiveTool(t, "erp", "read")
	wantState(t, f.Exec("erin", `UPDATE eacp.connectors SET endpoint = 'http://evil' WHERE id = $1`, tl.Connector), sqlForbidden)
	wantState(t, f.Exec("erin", `UPDATE eacp.tools SET name = 'other' WHERE id = $1`, tl.Tool), sqlForbidden)
}

func TestConnectorRejectsBadEndpointAndSecretRef(t *testing.T) {
	f := registrytest.New(t)
	ins := `INSERT INTO eacp.connectors (tenant_id, name, protocol, endpoint, secret_ref)
		VALUES (eacp.current_tenant_id(), $1, 'http', $2, $3) RETURNING id`
	for name, c := range map[string][2]string{
		"no scheme":     {"fakeerp:8090", "ref"},
		"whitespace":    {"http://a b", "ref"},
		"secret value?": {"http://ok", "Bearer abc"},
		"uppercase ref": {"http://ok", "REF"},
	} {
		_, err := f.TryID("erin", ins, "c-"+uuid.NewString()[:6], c[0], c[1])
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		wantState(t, err, sqlCheck)
	}
}

func TestContractValidationMatrix(t *testing.T) {
	f := registrytest.New(t)
	tl := f.ActiveTool(t, "erp", "read")
	ins := `INSERT INTO eacp.tool_contracts
		(tenant_id, tool_id, side_effects, idempotency_mode, idempotency_key_field, correlation_field,
		 reconciliation_lookup, reconciliation_consistency, proof_standard, max_attempts, credential_custody)
		VALUES (eacp.current_tenant_id(), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`
	type c struct {
		effects                []string
		idem, key, corr        any
		lookup, consist, proof string
		attempts               int
		custody                string
	}
	good := c{[]string{"IRREVERSIBLE_WRITE", "FINANCIAL"}, "native", "Idempotency-Key", "external_reference", "by_operation_key", "strong", "authoritative", 3, "worker"}
	bad := map[string]func(c) c{
		"no effects":                     func(x c) c { x.effects = []string{}; return x },
		"unknown effect":                 func(x c) c { x.effects = []string{"DELETE_EVERYTHING"}; return x },
		"read-only mixed":                func(x c) c { x.effects = []string{"READ_ONLY", "FINANCIAL"}; return x },
		"native without key":             func(x c) c { x.key = nil; return x },
		"correlation without field":      func(x c) c { x.idem, x.corr = "correlation_only", nil; return x },
		"unknown idempotency":            func(x c) c { x.idem = "maybe"; return x },
		"authoritative eventual":         func(x c) c { x.consist = "eventual"; return x },
		"authoritative without lookup":   func(x c) c { x.lookup, x.consist = "none", "none"; return x },
		"lookup without consistency":     func(x c) c { x.consist = "none"; return x },
		"no lookup but consistency":      func(x c) c { x.lookup, x.proof = "none", "none"; return x },
		"no lookup but best effort":      func(x c) c { x.lookup, x.consist, x.proof = "none", "none", "best_effort"; return x },
		"agent custody":                  func(x c) c { x.custody = "agent"; return x },
		"write, no idempotency, retries": func(x c) c { x.idem, x.key = "none", nil; return x },
		"zero attempts":                  func(x c) c { x.attempts = 0; return x },
	}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			x := mutate(good)
			_, err := f.TryID("erin", ins, tl.Tool, x.effects, x.idem, x.key, x.corr, x.lookup, x.consist, x.proof, x.attempts, x.custody)
			wantState(t, err, sqlCheck)
		})
	}
	t.Run("good", func(t *testing.T) {
		f.ID(t, "erin", ins, tl.Tool, good.effects, good.idem, good.key, good.corr, good.lookup, good.consist, good.proof, good.attempts, good.custody)
	})
	t.Run("write without idempotency but single attempt", func(t *testing.T) {
		f.ID(t, "erin", ins, tl.Tool, []string{"IRREVERSIBLE_WRITE"}, "none", nil, nil, "none", "none", "none", 1, "worker")
	})
}

func TestContractActivationIsTwoPersonAndRevocationMonotonic(t *testing.T) {
	f := registrytest.New(t)
	conn := f.ID(t, "erin", `INSERT INTO eacp.connectors (tenant_id, name, protocol, endpoint, secret_ref)
		VALUES (eacp.current_tenant_id(), 'erp', 'http', 'http://fakeerp:8090', 'erp') RETURNING id`)
	tool := f.ID(t, "erin", `INSERT INTO eacp.tools (tenant_id, connector_id, name) VALUES (eacp.current_tenant_id(), $1, 'read') RETURNING id`, conn)
	byRita := f.ID(t, "rita", registrytest.SafeContractSQL, tool)
	activate := `UPDATE eacp.tools SET active_contract_id = $1 WHERE id = $2`
	wantState(t, f.Exec("rita", activate, byRita, tool), sqlForbidden)
	wantState(t, f.Exec("erin", activate, byRita, tool), sqlForbidden)
	ok(t, f.Exec("ravi", activate, byRita, tool))
	wantState(t, f.Exec("ravi", `UPDATE eacp.tools SET active_contract_id = NULL WHERE id = $1`, tool), sqlBadState)

	revoke := `UPDATE eacp.tool_contracts SET revoked_at = now(), revoke_reason = $1 WHERE id = $2`
	wantState(t, f.Exec("erin", revoke, "editor cannot revoke", byRita), sqlForbidden)
	ok(t, f.Exec("otto", revoke, "incident", byRita))
	wantState(t, f.Exec("otto", `UPDATE eacp.tool_contracts SET revoked_at = NULL, revoke_reason = NULL WHERE id = $1`, byRita), sqlBadState)
	wantState(t, f.Exec("otto", revoke, "again", byRita), sqlBadState)

	// A revoked contract cannot be (re)activated, even by a second person.
	other := f.ID(t, "erin", registrytest.SafeContractSQL, tool)
	ok(t, f.Exec("rita", activate, other, tool))
	wantState(t, f.Exec("ravi", activate, byRita, tool), sqlBadState)
}

func TestContractFieldsAreImmutable(t *testing.T) {
	f := registrytest.New(t)
	tl := f.ActiveTool(t, "erp", "read")
	wantState(t, f.Exec("erin", `UPDATE eacp.tool_contracts SET max_attempts = 9 WHERE id = $1`, tl.Contract), sqlForbidden)
}

func TestContractFromAnotherToolCannotBeActivated(t *testing.T) {
	f := registrytest.New(t)
	a, b := f.ActiveTool(t, "erp", "read"), f.ActiveTool(t, "crm", "read")
	c := f.ID(t, "erin", registrytest.SafeContractSQL, b.Tool)
	wantState(t, f.Exec("rita", `UPDATE eacp.tools SET active_contract_id = $1 WHERE id = $2`, c, a.Tool), sqlBadState)
}

func TestContractFingerprintIsComputedByDatabase(t *testing.T) {
	f := registrytest.New(t)
	tl := f.ActiveTool(t, "erp", "read")
	var match bool
	storage.InTenantTx(context.Background(), f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `
			SELECT fingerprint = eacp.tool_fingerprint(tenant_id, tool_id) AND octet_length(fingerprint) = 32
			FROM eacp.tool_contracts WHERE id = $1`, tl.Contract).Scan(&match)
	})
	if !match {
		t.Fatal("stored fingerprint does not match the tool's current fingerprint")
	}
}

// ---------- credentials ----------

func proposeCred(f *registrytest.Fixture, actor, kind string, principal, version any, expires time.Duration) (uuid.UUID, error) {
	return f.TryID(actor, `INSERT INTO eacp.credentials
		(tenant_id, id, kind, principal_id, agent_version_id, secret_hash, expires_at)
		VALUES (eacp.current_tenant_id(), gen_random_uuid(), $1, $2, $3, sha256('x'::bytea),
		        now() + $4::bigint * interval '1 second')
		RETURNING id`, kind, principal, version, int64(expires.Seconds()))
}

func TestPrincipalCredentialsAreTwoPerson(t *testing.T) {
	f := registrytest.New(t)
	_, err := proposeCred(f, "erin", "pk", f.P["carol"], nil, time.Hour)
	wantState(t, err, sqlForbidden)

	c, err := proposeCred(f, "alice", "pk", f.P["bob"], nil, time.Hour)
	ok(t, err)
	approve := `UPDATE eacp.credentials SET approved_at = now() WHERE id = $1`
	wantState(t, f.Exec("alice", approve, c), sqlForbidden) // proposer
	wantState(t, f.Exec("bob", approve, c), sqlForbidden)   // subject
	c2, _ := proposeCred(f, "alice", "pk", f.P["carol"], nil, time.Hour)
	ok(t, f.Exec("bob", approve, c2))
	wantState(t, f.Exec("bob", `UPDATE eacp.credentials SET approved_at = now() - interval '1 day' WHERE id = $1`, c2), sqlBadState)
}

func TestAdminMayRotateOwnKeyWithSecondAdmin(t *testing.T) {
	f := registrytest.New(t)
	c, err := proposeCred(f, "alice", "pk", f.P["alice"], nil, time.Hour)
	ok(t, err)
	ok(t, f.Exec("bob", `UPDATE eacp.credentials SET approved_at = now() WHERE id = $1`, c))
}

func TestAgentCredentialsAreTwoPerson(t *testing.T) {
	f := registrytest.New(t)
	a := f.NewAgent(t, "a1")
	_, err := proposeCred(f, "otto", "ak", nil, a.Version, time.Hour)
	wantState(t, err, sqlForbidden)
	c, err := proposeCred(f, "erin", "ak", nil, a.Version, time.Hour)
	ok(t, err)
	wantState(t, f.Exec("erin", `UPDATE eacp.credentials SET approved_at = now() WHERE id = $1`, c), sqlForbidden)
	wantState(t, f.Exec("alice", `UPDATE eacp.credentials SET approved_at = now() WHERE id = $1`, c), sqlForbidden)
	ok(t, f.Exec("rita", `UPDATE eacp.credentials SET approved_at = now() WHERE id = $1`, c))
	ok(t, f.Exec("otto", `UPDATE eacp.credentials SET revoked_at = now(), revoke_reason = 'rotated' WHERE id = $1`, c))
	wantState(t, f.Exec("otto", `UPDATE eacp.credentials SET revoked_at = NULL WHERE id = $1`, c), sqlBadState)
}

func TestCredentialLifetimeIsBounded(t *testing.T) {
	f := registrytest.New(t)
	_, err := proposeCred(f, "alice", "pk", f.P["carol"], nil, 91*24*time.Hour)
	wantState(t, err, sqlCheck)
	_, err = proposeCred(f, "alice", "pk", f.P["carol"], nil, -time.Hour)
	wantState(t, err, sqlCheck)
}

func TestCredentialSubjectMustMatchKind(t *testing.T) {
	f := registrytest.New(t)
	a := f.NewAgent(t, "a1")
	_, err := proposeCred(f, "alice", "pk", nil, a.Version, time.Hour)
	wantState(t, err, sqlCheck)
	_, err = proposeCred(f, "erin", "ak", f.P["carol"], nil, time.Hour)
	wantState(t, err, sqlCheck)
}

// ---------- tenant isolation ----------

func TestCrossTenantReferencesAreRejected(t *testing.T) {
	f := registrytest.New(t)
	// Bootstrap one admin-like editor in tenant B through the owner path.
	var editorB uuid.UUID
	ok(t, storage.InTenantTx(context.Background(), f.Owner, pgtest.TenantB, func(tx pgx.Tx) error {
		if err := tx.QueryRow(context.Background(), `INSERT INTO eacp.principals (tenant_id, kind, name, subject, display_name)
			VALUES (eacp.current_tenant_id(), 'human', 'eve', 'eve@b.test', 'Eve') RETURNING id`).Scan(&editorB); err != nil {
			return err
		}
		_, err := tx.Exec(context.Background(), `INSERT INTO eacp.role_grants (tenant_id, principal_id, role, approved_at)
			VALUES (eacp.current_tenant_id(), $1, 'registry_editor', now())`, editorB)
		return err
	}))
	// Eve (tenant B) names carol (tenant A) as owner: the composite FK rejects it.
	err := f.ExecIn(pgtest.TenantB, editorB.String(), `INSERT INTO eacp.agents
		(tenant_id, name, display_name, environment, risk_class, owner_principal_id)
		VALUES (eacp.current_tenant_id(), 'xx', 'x', 'production', 'low', $1)`, f.P["carol"])
	wantState(t, err, sqlForeignKey)
	// Tenant A's actor ids mean nothing in tenant B.
	err = f.ExecIn(pgtest.TenantB, f.P["erin"].String(), `INSERT INTO eacp.agents
		(tenant_id, name, display_name, environment, risk_class, owner_principal_id)
		VALUES (eacp.current_tenant_id(), 'yy', 'y', 'production', 'low', $1)`, editorB)
	wantState(t, err, sqlForbidden)
}

func TestEveryRegistryTableIsTenantIsolated(t *testing.T) {
	f := registrytest.New(t)
	tool := f.ActiveTool(t, "erp", "read")
	a := f.ActiveAgent(t, "a1", tool.Tool)
	_, err := proposeCred(f, "erin", "ak", nil, a.Version, time.Hour)
	ok(t, err)
	g := f.ID(t, "alice", `INSERT INTO eacp.groups (tenant_id, name, display_name) VALUES (eacp.current_tenant_id(), 'gg', 'g') RETURNING id`)
	f.ID(t, "alice", `INSERT INTO eacp.group_memberships (tenant_id, group_id, principal_id) VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, g, f.P["carol"])

	tables := []string{"principals", "role_grants", "groups", "group_memberships", "credentials",
		"agents", "agent_versions", "agent_allowlists", "connectors", "tools", "tool_contracts"}
	for _, tbl := range tables {
		t.Run(tbl, func(t *testing.T) {
			var inA, inB, none int
			storage.InTenantTx(context.Background(), f.App, pgtest.TenantA, func(tx pgx.Tx) error {
				return tx.QueryRow(context.Background(), "SELECT count(*) FROM eacp."+tbl).Scan(&inA)
			})
			storage.InTenantTx(context.Background(), f.App, pgtest.TenantB, func(tx pgx.Tx) error {
				return tx.QueryRow(context.Background(), "SELECT count(*) FROM eacp."+tbl).Scan(&inB)
			})
			f.App.QueryRow(context.Background(), "SELECT count(*) FROM eacp."+tbl).Scan(&none)
			if inA == 0 || inB != 0 || none != 0 {
				t.Fatalf("%s: tenant A sees %d, tenant B sees %d, no context sees %d", tbl, inA, inB, none)
			}
		})
	}
}

func TestAppRoleCannotDeleteRegistryRows(t *testing.T) {
	f := registrytest.New(t)
	for _, tbl := range []string{"principals", "role_grants", "credentials", "agent_versions", "tool_contracts", "tools"} {
		wantState(t, f.Exec("alice", "DELETE FROM eacp."+tbl), sqlForbidden)
	}
}

// ---------- Phase 2 code review fixes ----------

func auditCountRaw(t *testing.T, f *registrytest.Fixture) (n int) {
	t.Helper()
	ok(t, storage.InTenantTx(context.Background(), f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM eacp.audit_events`).Scan(&n)
	}))
	return n
}

// Every registry change is audited by the database itself, even when made
// with raw SQL that bypasses the Go layer (review finding 2).
func TestRawSQLChangesAreAuditedByTheDatabase(t *testing.T) {
	f := registrytest.New(t)
	tl := f.ActiveTool(t, "erp", "read")
	before := auditCountRaw(t, f)
	ok(t, f.Exec("otto", `UPDATE eacp.tool_contracts SET revoked_at = now(), revoke_reason = 'raw' WHERE id = $1`, tl.Contract))
	if got := auditCountRaw(t, f) - before; got != 1 {
		t.Fatalf("raw revocation produced %d audit events, want 1", got)
	}
	var action, reason, actor string
	ok(t, storage.InTenantTx(context.Background(), f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `
			SELECT p->>'action', p->>'reason', p->'actor'->>'id'
			FROM (SELECT convert_from(payload, 'UTF8')::jsonb AS p FROM eacp.audit_events ORDER BY seq DESC LIMIT 1) e`,
		).Scan(&action, &reason, &actor)
	}))
	if action != "tool_contracts.update" || reason != "raw" || actor != f.P["otto"].String() {
		t.Fatalf("audit event = %s / %s / %s", action, reason, actor)
	}
	// A no-op UPDATE changes nothing and records nothing.
	before = auditCountRaw(t, f)
	ok(t, f.Exec("alice", `UPDATE eacp.principals SET disable_reason = disable_reason WHERE id = $1`, f.P["carol"]))
	if got := auditCountRaw(t, f) - before; got != 0 {
		t.Fatalf("no-op update produced %d audit events", got)
	}
}

func TestAuditNeverRecordsCredentialHashes(t *testing.T) {
	f := registrytest.New(t)
	_, err := proposeCred(f, "alice", "pk", f.P["carol"], nil, time.Hour)
	ok(t, err)
	var leaked bool
	ok(t, storage.InTenantTx(context.Background(), f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT bool_or(convert_from(payload, 'UTF8') LIKE '%secret_hash%') FROM eacp.audit_events`).Scan(&leaked)
	}))
	if leaked {
		t.Fatal("audit payload contains secret_hash")
	}
}

// Service principals may hold only auditor in Slice A (review finding 3): a
// human who controls a service principal must not be able to author as the
// service and approve as themselves.
func TestServicePrincipalsHoldOnlyAuditor(t *testing.T) {
	f := registrytest.New(t)
	for _, role := range []string{"registry_editor", "admin", "registry_approver", "operator", "approver"} {
		_, err := f.TryID("alice", proposeGrantSQL, f.P["ci"], role)
		wantState(t, err, sqlForbidden)
	}
}

func TestContractFieldNamesCannotBeBlank(t *testing.T) {
	f := registrytest.New(t)
	tl := f.ActiveTool(t, "erp", "read")
	ins := `INSERT INTO eacp.tool_contracts
		(tenant_id, tool_id, side_effects, idempotency_mode, idempotency_key_field, correlation_field,
		 reconciliation_lookup, reconciliation_consistency, proof_standard, max_attempts)
		VALUES (eacp.current_tenant_id(), $1, '{IRREVERSIBLE_WRITE}', $2, $3, $4, 'none', 'none', 'none', 10) RETURNING id`
	for name, args := range map[string][]any{
		"blank native key":        {"native", "  ", nil},
		"blank correlation field": {"correlation_only", nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.TryID("erin", ins, append([]any{tl.Tool}, args...)...)
			wantState(t, err, sqlCheck)
		})
	}
}
