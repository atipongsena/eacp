package bundle_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"eacp/internal/bundle"
	"eacp/internal/governance"
	"eacp/internal/registry"
	"eacp/internal/registry/registrytest"
	"eacp/internal/storage"
)

const peopleDoc = `{
 "principals": {"dana": {"kind": "human", "subject": "dana@example.com", "display_name": "Dana", "roles": ["auditor"]}},
 "groups": {"ops": {"display_name": "Ops", "schedule_weight": 2, "members": ["dana"]}}}`

func gov(name, doc string) bundle.Request {
	return bundle.Request{Bundle: name, Desired: json.RawMessage(doc)}
}

// declare is an existing fixture principal as a bundle declares it.
func declare(t *testing.T, f *registrytest.Fixture, name string, roles ...string) string {
	t.Helper()
	var kind, display string
	var subject *string
	ok(t, storage.InTenantTx(context.Background(), f.Owner, f.Tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT kind, subject, display_name FROM eacp.principals
			WHERE name = $1`, name).Scan(&kind, &subject, &display)
	}))
	p := map[string]any{"kind": kind, "display_name": display, "roles": append([]string{}, roles...)}
	if subject != nil {
		p["subject"] = *subject
	}
	raw, err := json.Marshal(p)
	ok(t, err)
	return fmt.Sprintf("%q: %s", name, raw)
}

// adopt is the imports block that adopts existing fixture principals.
func adopt(f *registrytest.Fixture, names ...string) string {
	var out []string
	for _, n := range names {
		out = append(out, fmt.Sprintf(`{"to": "principal.%s", "id": "%s"}`, n, f.P[n]))
	}
	return `"imports": [` + strings.Join(out, ", ") + `]`
}

// submitted plans r and submits it as who.
func submitted(t *testing.T, f *registrytest.Fixture, s *bundle.Service, who string, r bundle.Request) bundle.ChangeSet {
	t.Helper()
	ctx := context.Background()
	p, err := s.Plan(ctx, as(f, who), r)
	ok(t, err)
	cs, err := s.Submit(ctx, as(f, who), p.ID)
	ok(t, err)
	return cs
}

func TestAnIdentityBundleIsAppliedByTwoAdmins(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()
	p, err := s.Plan(ctx, as(f, "alice"), gov("people", peopleDoc))
	ok(t, err)
	var got []string
	for _, st := range p.Steps {
		got = append(got, st.Stage+" "+st.Op+" "+st.Address)
	}
	if want := "submit create principal.dana|submit propose role.dana.auditor|submit create group.ops|" +
		"submit create member.ops.dana|approve activate role.dana.auditor"; strings.Join(got, "|") != want {
		t.Fatalf("steps = %v", got)
	}
	_, err = s.Submit(ctx, as(f, "alice"), p.ID)
	ok(t, err)
	_, err = s.Approve(ctx, as(f, "alice"), p.ID)
	wantCode(t, err, bundle.CodeSamePrincipal)
	// A registry approver may approve change sets, but the grant trigger
	// wants an admin.
	_, err = s.Approve(ctx, as(f, "rita"), p.ID)
	if wantCode(t, err, bundle.CodeStepFailed); !errors.Is(err, registry.ErrForbidden) {
		t.Fatalf("a registry approver approving a grant: %v", err)
	}
	cs, err := s.Approve(ctx, as(f, "bob"), p.ID)
	ok(t, err)
	if cs.State != "APPLIED" {
		t.Fatalf("state = %s", cs.State)
	}
	if n := count(t, f, `SELECT count(*) FROM eacp.role_grants g JOIN eacp.principals p ON p.id = g.principal_id
		WHERE p.name = 'dana' AND g.role = 'auditor' AND g.proposed_by = $1 AND g.approved_by = $2`,
		f.P["alice"], f.P["bob"]); n != 1 {
		t.Fatal("dana's grant was not proposed by alice and approved by bob")
	}
	if n := count(t, f, `SELECT count(*) FROM eacp.group_memberships m JOIN eacp.groups g ON g.id = m.group_id
		WHERE g.name = 'ops' AND g.schedule_weight = 2 AND m.removed_at IS NULL`); n != 1 {
		t.Fatal("the membership was not added")
	}
	if auditCount(t, f, "change_set.submitted") != 1 || auditCount(t, f, "change_set.applied") != 1 {
		t.Fatal("the change set's moves are journaled")
	}
	again, err := s.Plan(ctx, as(f, "alice"), gov("people", peopleDoc))
	ok(t, err)
	if len(again.Steps) != 0 {
		t.Fatalf("replan = %+v", again.Steps)
	}
}

func TestTheGranteeCannotApproveTheirOwnGrant(t *testing.T) {
	f, s := setup(t)
	doc := `{"principals": {` + declare(t, f, "alice", "admin") + `, ` + declare(t, f, "bob", "admin", "auditor") +
		`}, ` + adopt(f, "alice", "bob") + `}`
	cs := submitted(t, f, s, "alice", gov("admins", doc))
	_, err := s.Approve(context.Background(), as(f, "bob"), cs.ID)
	wantCode(t, err, bundle.CodeStepFailed)
}

func TestTheAdminFloorHoldsWhenAPlanRevokesAnAdmin(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()
	both := `{"principals": {` + declare(t, f, "alice", "admin") + `, ` + declare(t, f, "bob", "admin") + `}, ` +
		adopt(f, "alice", "bob") + `}`
	if cs := submitted(t, f, s, "alice", gov("admins", both)); cs.State != "APPLIED" {
		t.Fatalf("adoption = %s", cs.State)
	}
	lone := gov("admins", `{"principals": {`+declare(t, f, "alice")+`, `+declare(t, f, "bob", "admin")+`}}`)
	lone.Prune = true
	_, err := s.Plan(ctx, as(f, "alice"), lone)
	be := wantCode(t, err, bundle.CodePlanBlocked)
	if !strings.Contains(fmt.Sprint(be.Findings), bundle.KindAdminFloor) {
		t.Fatalf("findings = %+v", be.Findings)
	}
	withDave := gov("admins", `{"principals": {`+declare(t, f, "alice")+`, `+declare(t, f, "bob", "admin")+
		`, "dave": {"kind": "human", "subject": "dave@example.com", "display_name": "Dave", "roles": ["admin"]}}}`)
	withDave.Prune = true
	cs := submitted(t, f, s, "alice", withDave)
	cs, err = s.Approve(ctx, as(f, "bob"), cs.ID)
	ok(t, err)
	if n := count(t, f, `SELECT count(*) FROM eacp.role_grants g JOIN eacp.principals p ON p.id = g.principal_id
		WHERE g.role = 'admin' AND g.approved_at IS NOT NULL AND g.revoked_at IS NULL AND p.name IN ('bob', 'dave')`); n != 2 ||
		count(t, f, `SELECT count(*) FROM eacp.role_grants WHERE role = 'admin' AND revoked_at IS NULL
			AND principal_id = $1`, f.P["alice"]) != 0 {
		t.Fatal("bob and dave are the admins, alice is not")
	}
}

func TestAConcurrentRevokeCannotBreakTheAdminFloor(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()
	// carol is a third admin that the bundle does not manage.
	carolAdmin := f.ID(t, "alice", `INSERT INTO eacp.role_grants (tenant_id, principal_id, role)
		VALUES (eacp.current_tenant_id(), $1, 'admin') RETURNING id`, f.P["carol"])
	ok(t, f.Exec("bob", `UPDATE eacp.role_grants SET approved_at = now() WHERE id = $1`, carolAdmin))
	both := `{"principals": {` + declare(t, f, "alice", "admin") + `, ` + declare(t, f, "bob", "admin") + `}, ` +
		adopt(f, "alice", "bob") + `}`
	submitted(t, f, s, "alice", gov("admins", both))
	lone := gov("admins", `{"principals": {`+declare(t, f, "alice")+`, `+declare(t, f, "bob", "admin")+`}}`)
	lone.Prune = true
	cs := submitted(t, f, s, "alice", lone) // three admins, one revoked: two remain
	ok(t, f.Exec("bob", `UPDATE eacp.role_grants SET revoked_at = now(), revoke_reason = 'left' WHERE id = $1`, carolAdmin))
	_, err := s.Approve(ctx, as(f, "bob"), cs.ID)
	wantCode(t, err, bundle.CodeStepFailed)
	if n := count(t, f, `SELECT count(*) FROM eacp.role_grants WHERE role = 'admin' AND revoked_at IS NULL
		AND principal_id = $1`, f.P["alice"]); n != 1 {
		t.Fatal("the approval committed below two admins")
	}
}

func TestAPolicyIsCreatedByOneAdminAndActivatedByAnother(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()
	doc := `{"policy": {"content": ` + allowPolicy + `}}`
	cs := submitted(t, f, s, "alice", gov("policy", doc))
	cs, err := s.Approve(ctx, as(f, "bob"), cs.ID)
	ok(t, err)
	cur, err := governance.NewStore(f.App).CurrentPolicy(ctx, f.Tenant)
	ok(t, err)
	if cs.Steps[0].ObjectID == nil || cur.ID != *cs.Steps[0].ObjectID {
		t.Fatalf("current policy %s, created %v", cur.ID, cs.Steps[0].ObjectID)
	}
	reformatted := `{"policy": {"content": {"rules": [{"reason": "permitted", "verdict": "allow", "id": "all"}],
	 "format_version": 1}}}`
	again, err := s.Plan(ctx, as(f, "alice"), gov("policy", reformatted))
	ok(t, err)
	if len(again.Steps) != 0 {
		t.Fatalf("replan = %+v", again.Steps)
	}
	_, err = s.Plan(ctx, as(f, "alice"), gov("other", doc))
	be := wantCode(t, err, bundle.CodePlanBlocked)
	if len(be.Findings) != 1 || be.Findings[0].Address != bundle.PolicyAddress || be.Findings[0].Kind != bundle.KindUnmanaged {
		t.Fatalf("findings = %+v", be.Findings)
	}
}

func TestABudgetIsCreatedAndRaisedInOneChangeSet(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()
	doc := `{"budgets": {"root": {"unit": "USD", "hard_limit": 1000},
	 "team": {"unit": "USD", "parent": "root", "hard_limit": 200, "soft_limit": 150}}}`
	cs := submitted(t, f, s, "alice", gov("money", doc))
	if len(cs.Steps) != 7 {
		t.Fatalf("steps = %+v", cs.Steps)
	}
	_, err := s.Approve(ctx, as(f, "bob"), cs.ID)
	ok(t, err)
	if n := count(t, f, `SELECT count(*) FROM eacp.budget_accounts
		WHERE (name, hard_limit) IN (('root', 1000), ('team', 200))`); n != 2 {
		t.Fatal("the limits were not raised")
	}
	if n := count(t, f, `SELECT count(*) FROM eacp.budget_soft_limits l JOIN eacp.budget_accounts b ON b.id = l.account_id
		WHERE b.name = 'team' AND l.monthly_limit = 150`); n != 1 {
		t.Fatal("the soft limit was not set")
	}
	again, err := s.Plan(ctx, as(f, "alice"), gov("money", doc))
	ok(t, err)
	if len(again.Steps) != 0 {
		t.Fatalf("replan = %+v", again.Steps)
	}
	lower := strings.NewReplacer(`"hard_limit": 1000`, `"hard_limit": 900`, `"hard_limit": 200`, `"hard_limit": 100`).Replace(doc)
	if cs := submitted(t, f, s, "alice", gov("money", lower)); cs.State != "APPLIED" {
		t.Fatalf("decreases need one admin: %s", cs.State)
	}
	if n := count(t, f, `SELECT count(*) FROM eacp.budget_accounts
		WHERE (name, hard_limit) IN (('root', 900), ('team', 100))`); n != 2 {
		t.Fatal("the limits were not lowered")
	}
}

func TestAPriceIsAddedForwardOnly(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()
	doc := `{"prices": {"gpt": {"provider": "openai", "model": "gpt-4.1", "unit": "USD",
	 "input_per_mtok": 2.5, "output_per_mtok": 10}}}`
	if cs := submitted(t, f, s, "alice", gov("rates", doc)); cs.State != "APPLIED" {
		t.Fatalf("a price needs one admin: %s", cs.State)
	}
	same, err := s.Plan(ctx, as(f, "alice"), gov("rates", strings.Replace(doc, "2.5", "2.50", 1)))
	ok(t, err)
	if len(same.Steps) != 0 {
		t.Fatalf("replan = %+v", same.Steps)
	}
	submitted(t, f, s, "alice", gov("rates", strings.Replace(doc, `"output_per_mtok": 10`, `"output_per_mtok": 12`, 1)))
	if n := count(t, f, `SELECT count(*) FROM eacp.model_prices WHERE provider = 'openai' AND model = 'gpt-4.1'`); n != 2 {
		t.Fatalf("prices = %d, want the old one kept and a new one added", n)
	}
	if n := count(t, f, `SELECT count(*) FROM eacp.model_prices a JOIN eacp.model_prices b
		ON b.model = a.model AND b.output_per_mtok = 10 WHERE a.output_per_mtok = 12 AND a.effective_from > b.effective_from`); n != 1 {
		t.Fatal("the new price takes effect after the old one")
	}
}

func TestAConcurrentGrantApprovalMakesTheChangeSetStale(t *testing.T) {
	f, s := setup(t)
	cs := submitted(t, f, s, "alice", gov("people", peopleDoc))
	grant := cs.Steps[1].ObjectID
	if grant == nil {
		t.Fatalf("steps = %+v", cs.Steps)
	}
	ok(t, f.Exec("bob", `UPDATE eacp.role_grants SET approved_at = now() WHERE id = $1`, *grant))
	_, err := s.Approve(context.Background(), as(f, "bob"), cs.ID)
	wantCode(t, err, bundle.CodeStale)
}

func TestIdentityDrift(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()
	cs := submitted(t, f, s, "alice", gov("people", peopleDoc))
	_, err := s.Approve(ctx, as(f, "bob"), cs.ID)
	ok(t, err)
	ok(t, f.Exec("alice", `UPDATE eacp.group_memberships SET removed_at = now(), remove_reason = 'left'
		WHERE principal_id = (SELECT id FROM eacp.principals WHERE name = 'dana')`))
	dr, err := s.Drift(ctx, as(f, "audra"), "people")
	ok(t, err)
	got := map[string]string{}
	for _, e := range dr.Entries {
		got[e.Address] = e.Status
	}
	if got["principal.dana"] != bundle.DriftInSync || got["group.ops"] != bundle.DriftInSync ||
		got["member.ops.dana"] != bundle.DriftModified {
		t.Fatalf("drift = %v", got)
	}
}

func TestDriftReportsGrantsAndMembersAddedOutsideTheBundle(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()
	cs := submitted(t, f, s, "alice", gov("people", peopleDoc))
	_, err := s.Approve(ctx, as(f, "bob"), cs.ID)
	ok(t, err)
	dana := f.ID(t, "alice", `SELECT id FROM eacp.principals WHERE name = 'dana'`)
	grant := f.ID(t, "alice", `INSERT INTO eacp.role_grants (tenant_id, principal_id, role)
		VALUES (eacp.current_tenant_id(), $1, 'operator') RETURNING id`, dana)
	ok(t, f.Exec("bob", `UPDATE eacp.role_grants SET approved_at = now() WHERE id = $1`, grant))
	f.ID(t, "alice", `INSERT INTO eacp.group_memberships (tenant_id, group_id, principal_id)
		SELECT eacp.current_tenant_id(), id, $1 FROM eacp.groups WHERE name = 'ops' RETURNING id`, f.P["carol"])
	dr, err := s.Drift(ctx, as(f, "audra"), "people")
	ok(t, err)
	got := map[string]string{}
	for _, e := range dr.Entries {
		got[e.Address] = e.Status
	}
	if got["role.dana.operator"] != bundle.DriftModified || got["member.ops.carol"] != bundle.DriftModified {
		t.Fatalf("drift = %v", got)
	}
}
