package studio_test

// Raw-SQL tests of the Agent Hub (Phase 27b, ADR-033 Rev 1.3): department
// leads, listings, their tiered approval, visibility, runs through a listing
// and clones. Every rule is PostgreSQL's (migration 00029).

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/storage"
)

const (
	listProposeSQL = `SELECT eacp.studio_listing_propose($1, $2, $3, $4, $5)`
	listDecideSQL  = `SELECT eacp.studio_listing_decide($1, $2, $3)`
	listCancelSQL  = `SELECT eacp.studio_listing_cancel($1, $2)`
	listRetireSQL  = `SELECT eacp.studio_listing_retire($1, $2, $3)`
	cloneSQL       = `SELECT eacp.studio_clone($1, $2, $3, $4)`
	memberSQL      = `INSERT INTO eacp.group_memberships (tenant_id, group_id, principal_id, lead)
		VALUES (eacp.current_tenant_id(), $1, $2, $3) RETURNING id`
)

// hubFix adds the Hub's cast to the Studio fixture:
//
//	lena  a lead of hr (no role)
//	fred  a lead of finance (no role)
//	hank  a member of hr (no role)
type hubFix struct{ *fix }

func newHubFix(t *testing.T) *hubFix {
	t.Helper()
	f := &hubFix{newFix(t)}
	for _, p := range []struct {
		name  string
		group uuid.UUID
		lead  bool
	}{{"lena", f.hr, true}, {"fred", f.finance, true}, {"hank", f.hr, false}} {
		f.AddPrincipal(t, p.name, "human")
		f.ID(t, "alice", memberSQL, p.group, f.P[p.name], p.lead)
	}
	return f
}

// as returns a scalar that sql selects as actor, through the application role.
func (f *hubFix) as(actor, sql string, args ...any) string {
	f.t.Helper()
	var s string
	ctx := context.Background()
	ok(f.t, storage.InTenantTx(ctx, f.App, f.Tenant.String(), func(tx pgx.Tx) error {
		if err := storage.SetActor(ctx, tx, f.P[actor]); err != nil {
			return err
		}
		return tx.QueryRow(ctx, sql, args...).Scan(&s)
	}))
	return s
}

func (f *hubFix) propose(actor string, agent, version uuid.UUID, scope string, tags ...string) (uuid.UUID, error) {
	if tags == nil {
		tags = []string{}
	}
	return f.TryID(actor, listProposeSQL, agent, version, scope, tags, "please")
}

func (f *hubFix) mustPropose(actor string, agent, version uuid.UUID, scope string, tags ...string) uuid.UUID {
	f.t.Helper()
	p, err := f.propose(actor, agent, version, scope, tags...)
	ok(f.t, err)
	return p
}

// listingOf returns agent's listing id.
func (f *hubFix) listingOf(agent uuid.UUID) uuid.UUID {
	f.t.Helper()
	return uuid.MustParse(f.scalar(`SELECT id::text FROM eacp.studio_listings WHERE agent_id = $1`, agent))
}

// published proposes agent's version at scope and has approver approve it.
func (f *hubFix) published(agent, version uuid.UUID, scope, approver string, tags ...string) uuid.UUID {
	f.t.Helper()
	ok(f.t, f.Exec(approver, listDecideSQL, f.mustPropose("stella", agent, version, scope, tags...), true, "useful"))
	return f.listingOf(agent)
}

func (f *hubFix) sees(actor string, listing uuid.UUID) bool {
	f.t.Helper()
	return f.as(actor, `SELECT (count(*) = 1)::text FROM eacp.studio_hub_listings() WHERE id = $1`, listing) == "true"
}

func TestALeadIsSetOnlyWhenAnAdminAddsTheMembership(t *testing.T) {
	f := newHubFix(t)
	if got := f.scalar(`SELECT string_agg(p.name || '=' || m.lead, ',' ORDER BY p.name) FROM eacp.group_memberships m
		JOIN eacp.principals p ON p.tenant_id = m.tenant_id AND p.id = m.principal_id
		WHERE m.group_id = $1 AND m.removed_at IS NULL`, f.hr); got != "abe=false,hank=false,lena=true,stella=false" {
		t.Fatalf("hr = %s", got)
	}
	lena := f.scalar(`SELECT id::text FROM eacp.group_memberships WHERE principal_id = $1`, f.P["lena"])
	hank := f.scalar(`SELECT id::text FROM eacp.group_memberships WHERE principal_id = $1`, f.P["hank"])
	// Never changed afterwards, not even by an admin: the application role
	// cannot update the column (and the guard refuses a change as well).
	wantState(t, f.Exec("alice", `UPDATE eacp.group_memberships SET lead = false WHERE id = $1`, lena), sqlForbidden)
	wantState(t, f.Exec("bob", `UPDATE eacp.group_memberships SET lead = true WHERE id = $1`, hank), sqlForbidden)
	// Only an admin adds a membership, and only a human is a lead.
	_, err := f.TryID("rita", memberSQL, f.finance, f.P["carol"], true)
	wantState(t, err, sqlForbidden)
	_, err = f.TryID("alice", memberSQL, f.hr, f.P["rt"], true)
	wantState(t, err, sqlForbidden)
	// A lead's membership is removed like any other; the replacement decides the flag.
	ok(t, f.Exec("alice", `UPDATE eacp.group_memberships SET removed_at = now(), remove_reason = 'moved' WHERE id = $1`, lena))
	f.ID(t, "bob", memberSQL, f.hr, f.P["lena"], false)
}

func TestOnlyTheOwnerProposesAnActiveApprovedVersion(t *testing.T) {
	f := newHubFix(t)
	version, agent := f.approvedAgent("leave-bot")
	for _, who := range []string{"abe", "lena", "rita", "alice"} {
		_, err := f.propose(who, agent, version, "DEPARTMENT")
		wantState(t, err, sqlForbidden)
	}
	waiting := f.mustSave("stella", nil, "waiting-bot", example)
	_, err := f.propose("stella", f.agentOf(waiting), waiting, "DEPARTMENT")
	wantState(t, err, sqlBadState)
	_, err = f.propose("stella", agent, waiting, "DEPARTMENT")
	wantState(t, err, sqlForeignKey)
	for name, bad := range map[string]struct {
		scope string
		tags  []string
	}{
		"scope": {"TEAM", nil}, "private": {"PRIVATE", nil}, "tag": {"DEPARTMENT", []string{"Bad Tag"}},
		"repeated": {"DEPARTMENT", []string{"hr", "hr"}}, "template": {"DEPARTMENT", []string{"template"}},
		"too many": {"DEPARTMENT", []string{"a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8", "a9"}},
	} {
		_, err := f.propose("stella", agent, version, bad.scope, bad.tags...)
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		wantState(t, err, sqlCheck)
	}
	f.mustPropose("stella", agent, version, "DEPARTMENT", "hr", "leave")
	_, err = f.propose("stella", agent, version, "ORG")
	wantState(t, err, sqlBadState)
}

func TestEachScopeHasItsApprover(t *testing.T) {
	f := newHubFix(t)
	version, agent := f.approvedAgent("leave-bot")
	p := f.mustPropose("stella", agent, version, "DEPARTMENT", "leave")
	wantState(t, f.Exec("stella", listDecideSQL, p, true, "mine"), sqlBadState)
	for _, who := range []string{"hank", "abe", "fred", "rita", "alice", "carol"} {
		wantState(t, f.Exec(who, listDecideSQL, p, true, "looks fine"), sqlForbidden)
	}
	wantState(t, f.Exec("lena", listDecideSQL, p, true, " "), sqlCheck)
	ok(t, f.Exec("lena", listDecideSQL, p, true, "useful for HR"))
	wantState(t, f.Exec("lena", listDecideSQL, p, true, "again"), sqlBadState)
	listing := f.listingOf(agent)
	if got := f.scalar(`SELECT concat_ws(' ', scope, state, published_version_id = $2, tags::text, published_by = $3)
		FROM eacp.studio_listings WHERE id = $1`, listing, version, f.P["lena"]); got != "DEPARTMENT PUBLISHED t {leave} t" {
		t.Fatalf("listing = %s", got)
	}

	// The organisation: an admin or a registry approver, never a lead.
	org := f.mustPropose("stella", agent, version, "ORG", "leave", "hr")
	wantState(t, f.Exec("lena", listDecideSQL, org, true, "wider"), sqlForbidden)
	ok(t, f.Exec("rita", listDecideSQL, org, true, "useful for everyone"))
	if got := f.scalar(`SELECT concat_ws(' ', scope, state, tags::text) FROM eacp.studio_listings WHERE id = $1`,
		listing); got != "ORG PUBLISHED {hr,leave}" {
		t.Fatalf("listing = %s", got)
	}
	// Narrowing again goes through the narrower scope's approver; a rejection changes nothing.
	back := f.mustPropose("stella", agent, version, "DEPARTMENT")
	ok(t, f.Exec("lena", listDecideSQL, back, false, "keep it wide"))
	if got := f.scalar(`SELECT concat_ws(' ', l.scope, p.decision) FROM eacp.studio_listings l
		JOIN eacp.studio_listing_proposals p ON p.agent_id = l.agent_id WHERE p.id = $1`, back); got != "ORG rejected" {
		t.Fatalf("after the rejection = %s", got)
	}
}

func TestTheTemplateTagNeedsAnAdmin(t *testing.T) {
	f := newHubFix(t)
	version, agent := f.approvedAgent("leave-bot")
	p := f.mustPropose("stella", agent, version, "ORG", "template", "hr")
	wantState(t, f.Exec("rita", listDecideSQL, p, true, "a template"), sqlForbidden)
	ok(t, f.Exec("alice", listDecideSQL, p, true, "a template"))
}

func TestApprovalNeedsTheVersionStillActive(t *testing.T) {
	f := newHubFix(t)
	v1, agent := f.approvedAgent("leave-bot")
	old := f.mustPropose("stella", agent, v1, "DEPARTMENT")
	v2 := f.mustSave("stella", agent, "", example)
	ok(t, f.decide("abe", v2, true, "same tool"))
	wantState(t, f.Exec("lena", listDecideSQL, old, true, "stale"), sqlBadState)
	// Only the proposer cancels, once.
	wantState(t, f.Exec("lena", listCancelSQL, old, "not mine"), sqlForbidden)
	ok(t, f.Exec("stella", listCancelSQL, old, "replaced by v2"))
	wantState(t, f.Exec("stella", listCancelSQL, old, "again"), sqlBadState)
	listing := f.published(agent, v2, "DEPARTMENT", "lena")
	if got := f.scalar(`SELECT (published_version_id = $2)::text FROM eacp.studio_listings WHERE id = $1`, listing, v2); got != "true" {
		t.Fatal("the listing does not publish v2")
	}
}

func TestDeprecateAndWithdrawOnlyNarrow(t *testing.T) {
	f := newHubFix(t)
	version, agent := f.approvedAgent("leave-bot")
	listing := f.published(agent, version, "DEPARTMENT", "lena")
	for _, who := range []string{"hank", "fred", "sid", "rita", "carol"} {
		wantState(t, f.Exec(who, listRetireSQL, listing, "DEPRECATED", "old"), sqlForbidden)
	}
	wantState(t, f.Exec("lena", listRetireSQL, listing, "PUBLISHED", "back"), sqlCheck)
	wantState(t, f.Exec("lena", listRetireSQL, listing, "DEPRECATED", ""), sqlCheck)
	ok(t, f.Exec("lena", listRetireSQL, listing, "DEPRECATED", "a newer agent exists"))
	wantState(t, f.Exec("lena", listRetireSQL, listing, "DEPRECATED", "again"), sqlBadState)
	ok(t, f.Exec("stella", listRetireSQL, listing, "WITHDRAWN", "no longer needed"))
	wantState(t, f.Exec("bob", listRetireSQL, listing, "WITHDRAWN", "again"), sqlBadState)
	// Published again only through a new approved proposal; an admin withdraws any listing.
	f.published(agent, version, "ORG", "ravi")
	ok(t, f.Exec("bob", listRetireSQL, listing, "WITHDRAWN", "tenant policy"))
}

func TestTheHubShowsEachListingToItsAudience(t *testing.T) {
	f := newHubFix(t)
	version, agent := f.approvedAgent("leave-bot")
	p := f.mustPropose("stella", agent, version, "DEPARTMENT")
	if got := f.as("stella", `SELECT count(*)::text FROM eacp.studio_hub_listings()`); got != "0" {
		t.Fatalf("a proposal is not a listing: %s", got)
	}
	ok(t, f.Exec("lena", listDecideSQL, p, true, "useful"))
	listing := f.listingOf(agent)
	audience := func(state string, want map[string]bool) {
		t.Helper()
		for who, w := range want {
			if got := f.sees(who, listing); got != w {
				t.Fatalf("%s: %s sees = %v, want %v", state, who, got, w)
			}
		}
	}
	audience("department", map[string]bool{"stella": true, "hank": true, "lena": true, "abe": true,
		"sid": false, "carol": false, "rita": false, "rt": false})
	if got := f.as("hank", `SELECT eacp.studio_hub_definition($1)`, listing); got != example {
		t.Fatalf("definition = %s", got)
	}
	_, err := f.TryID("sid", `SELECT eacp.studio_hub_definition($1)::jsonb->>'kind'`, listing)
	wantState(t, err, sqlForeignKey)

	f.published(agent, version, "ORG", "rita")
	audience("org", map[string]bool{"sid": true, "carol": true, "rita": true, "rt": false})
	ok(t, f.Exec("stella", listRetireSQL, listing, "DEPRECATED", "superseded"))
	audience("deprecated", map[string]bool{"hank": true, "carol": true})
	ok(t, f.Exec("stella", listRetireSQL, listing, "WITHDRAWN", "gone"))
	audience("withdrawn", map[string]bool{"stella": true, "hank": false, "carol": false, "rita": false})
	if got := f.as("stella", `SELECT concat_ws(' ', state, runnable, run_count) FROM eacp.studio_hub_listings() WHERE id = $1`,
		listing); got != "WITHDRAWN f 0" {
		t.Fatalf("the owner's view = %s", got)
	}
}

func TestOthersRunOnlyThroughAVisibleListing(t *testing.T) {
	f := newHubFix(t)
	version, agent := f.approvedAgent("leave-bot")
	for _, who := range []string{"hank", "abe", "lena", "sid", "carol", "rt"} {
		_, err := f.TryID(who, startSQL, agent, inputs)
		wantState(t, err, sqlForbidden)
	}
	f.ID(t, "stella", startSQL, agent, inputs)
	// The owner runs it only while in its department.
	stella := f.scalar(`SELECT id::text FROM eacp.group_memberships WHERE principal_id = $1 AND group_id = $2`, f.P["stella"], f.hr)
	ok(t, f.Exec("alice", `UPDATE eacp.group_memberships SET removed_at = now(), remove_reason = 'moved' WHERE id = $1`, stella))
	_, err := f.TryID("stella", startSQL, agent, inputs)
	wantState(t, err, sqlForbidden)
	f.ID(t, "bob", memberSQL, f.hr, f.P["stella"], false)

	listing := f.published(agent, version, "DEPARTMENT", "lena")
	f.ID(t, "hank", startSQL, agent, inputs)
	_, err = f.TryID("sid", startSQL, agent, inputs)
	wantState(t, err, sqlForbidden)
	ok(t, f.Exec("lena", listRetireSQL, listing, "DEPRECATED", "superseded"))
	f.ID(t, "hank", startSQL, agent, inputs)
	ok(t, f.Exec("lena", listRetireSQL, listing, "WITHDRAWN", "gone"))
	_, err = f.TryID("hank", startSQL, agent, inputs)
	wantState(t, err, sqlForbidden)

	f.published(agent, version, "ORG", "rita")
	for _, who := range []string{"sid", "carol", "hank"} {
		f.ID(t, who, startSQL, agent, inputs)
	}
	_, err = f.TryID("rt", startSQL, agent, inputs)
	wantState(t, err, sqlForbidden)

	// A newer approved version is not the published one until it is proposed.
	v2 := f.mustSave("stella", agent, "", example)
	ok(t, f.decide("abe", v2, true, "same tool"))
	_, err = f.TryID("sid", startSQL, agent, inputs)
	wantState(t, err, sqlBadState)
	if got := f.as("sid", `SELECT runnable::text FROM eacp.studio_hub_listings() WHERE id = $1`, listing); got != "false" {
		t.Fatalf("runnable = %s", got)
	}
	run := f.ID(t, "stella", startSQL, agent, inputs)
	if got := f.scalar(`SELECT (version_id = $2)::text FROM eacp.studio_runs WHERE id = $1`, run, v2); got != "true" {
		t.Fatal("the owner's run is not on the ACTIVE version")
	}
}

func TestACloneCarriesNoPermission(t *testing.T) {
	f := newHubFix(t)
	version, agent := f.approvedAgent("leave-bot")
	listing := f.published(agent, version, "DEPARTMENT", "lena")
	_, err := f.TryID("sid", cloneSQL, listing, "my-leave-bot", "My leave", f.finance)
	wantState(t, err, sqlForeignKey)
	f.published(agent, version, "ORG", "rita")
	_, err = f.TryID("carol", cloneSQL, listing, "carol-bot", "Carol's", f.finance)
	wantState(t, err, sqlForbidden)
	_, err = f.TryID("sid", cloneSQL, listing, "my-leave-bot", "My leave", f.hr)
	wantState(t, err, sqlForbidden)
	_, err = f.TryID("sid", cloneSQL, listing, "leave-bot", "Taken", f.finance)
	wantState(t, err, "23505")

	clone := f.ID(t, "sid", cloneSQL, listing, "my-leave-bot", "My leave", f.finance)
	if got := f.scalar(`SELECT concat_ws(' ', v.state, v.active_allowlist_id IS NULL, s.decision IS NULL, s.definition = $2,
			s.capability::text, g.owner_principal_id = $3, sa.department_group_id = $4, sa.cloned_from_version = $5,
			(SELECT count(*) FROM eacp.credentials k WHERE k.agent_version_id = v.id),
			(SELECT count(*) FROM eacp.studio_listings l WHERE l.agent_id = v.agent_id))
		FROM eacp.agent_versions v
		JOIN eacp.studio_versions s ON s.tenant_id = v.tenant_id AND s.id = v.id
		JOIN eacp.agents g ON g.tenant_id = v.tenant_id AND g.id = v.agent_id
		JOIN eacp.studio_agents sa ON sa.tenant_id = v.tenant_id AND sa.id = v.agent_id
		WHERE v.id = $1`, clone, example, f.P["sid"], f.finance, version); got !=
		"REGISTERED t t t {hr-mcp.get_leave_balance} t t t 0 0" {
		t.Fatalf("clone = %s", got)
	}
	_, err = f.TryID("sid", startSQL, f.agentOf(clone), inputs)
	wantState(t, err, sqlBadState)
	// Never from a deprecated listing.
	ok(t, f.Exec("stella", listRetireSQL, listing, "DEPRECATED", "superseded"))
	_, err = f.TryID("sid", cloneSQL, listing, "late-bot", "Late", f.finance)
	wantState(t, err, sqlBadState)
}

func TestTheHubIsWrittenOnlyThroughItsFunctions(t *testing.T) {
	f := newHubFix(t)
	version, agent := f.approvedAgent("leave-bot")
	listing := f.published(agent, version, "DEPARTMENT", "lena")
	for name, sql := range map[string]string{
		"listing": `INSERT INTO eacp.studio_listings (tenant_id, agent_id, scope, state, published_version_id, tags, published_by)
			VALUES (eacp.current_tenant_id(), $1, 'ORG', 'PUBLISHED', $2, '{}', $3)`,
		"proposal": `INSERT INTO eacp.studio_listing_proposals (tenant_id, agent_id, version_id, scope, tags, proposed_by)
			VALUES (eacp.current_tenant_id(), $1, $2, 'ORG', '{}', $3)`,
	} {
		err := f.Exec("alice", sql, agent, version, f.P["alice"])
		if err == nil {
			t.Fatalf("%s: written", name)
		}
		wantState(t, err, sqlForbidden)
	}
	wantState(t, f.Exec("alice", `UPDATE eacp.studio_listings SET scope = 'ORG' WHERE id = $1`, listing), sqlForbidden)
	if got := f.scalar(`SELECT count(DISTINCT convert_from(payload, 'UTF8')::jsonb->>'action')::text FROM eacp.audit_events
		WHERE convert_from(payload, 'UTF8')::jsonb->>'action' IN ('studio_listings.insert', 'studio_listing_proposals.insert',
		                                                          'studio_listing_proposals.update')`); got != "3" {
		t.Fatalf("journaled Hub actions = %s", got)
	}
}
