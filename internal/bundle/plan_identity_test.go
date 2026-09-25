package bundle

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

var (
	alice      = uuid.MustParse("00000000-0000-4000-8000-00000000a001")
	bob        = uuid.MustParse("00000000-0000-4000-8000-00000000a002")
	dana       = uuid.MustParse("00000000-0000-4000-8000-00000000a003")
	aliceAdmin = uuid.MustParse("00000000-0000-4000-8000-00000000a0a1")
	bobAdmin   = uuid.MustParse("00000000-0000-4000-8000-00000000a0a2")
	danaAudit  = uuid.MustParse("00000000-0000-4000-8000-00000000a0a3")
	danaOps    = uuid.MustParse("00000000-0000-4000-8000-00000000a0a4")
	opsID      = uuid.MustParse("00000000-0000-4000-8000-00000000b0b1")
	opsAlice   = uuid.MustParse("00000000-0000-4000-8000-00000000b0b2")
	opsDana    = uuid.MustParse("00000000-0000-4000-8000-00000000b0b3")
)

// admins is empty() with alice and bob as the tenant's two admins.
func admins() State {
	st := empty()
	st.People["alice"] = PrincipalState{ID: alice, Kind: "human", Subject: "alice@example.com", DisplayName: "Alice",
		Grants: map[string]GrantState{"admin": {ID: aliceAdmin, Approved: true}}}
	st.People["bob"] = PrincipalState{ID: bob, Kind: "human", Subject: "bob@example.com", DisplayName: "Bob",
		Grants: map[string]GrantState{"admin": {ID: bobAdmin, Approved: true}}}
	st.Principals["alice"], st.Principals["bob"] = alice, bob
	return st
}

const identityDoc = `{
 "principals": {"dana": {"kind": "human", "subject": "Dana@Example.com", "display_name": "Dana",
                         "roles": ["operator", "auditor"]}},
 "groups": {"ops": {"display_name": "Ops", "schedule_weight": 2, "members": ["dana", "alice"]}}}`

// identityApplied is admins() after identityDoc was applied.
func identityApplied() State {
	st := admins()
	st.People["dana"] = PrincipalState{ID: dana, Kind: "human", Subject: "dana@example.com", DisplayName: "Dana",
		Grants: map[string]GrantState{"auditor": {ID: danaAudit, Approved: true}, "operator": {ID: danaOps, Approved: true}}}
	st.Principals["dana"] = dana
	st.GroupRows["ops"] = GroupState{ID: opsID, DisplayName: "Ops", Weight: 2,
		Members: map[string]uuid.UUID{"alice": opsAlice, "dana": opsDana}}
	st.Groups["ops"] = opsID
	st.Managed["principal.dana"], st.Managed["group.ops"] = dana, opsID
	return st
}

func TestIdentityIsCreatedBeforeItIsGrantedOrJoined(t *testing.T) {
	d := diff("people", cs, mustDoc(t, identityDoc), admins(), false)
	wantOps(t, d,
		"1 submit create principal.dana",
		"2 submit propose role.dana.auditor",
		"3 submit propose role.dana.operator",
		"4 submit create group.ops",
		"5 submit create member.ops.alice",
		"6 submit create member.ops.dana",
		"7 approve activate role.dana.auditor",
		"8 approve activate role.dana.operator")
	if p := d.Steps[0].Payload.Principal; p == nil || p.Subject != "dana@example.com" || p.Name != "dana" {
		t.Fatalf("principal payload = %+v", p)
	}
	if p := d.Steps[1].Payload; p.Name != "auditor" || p.Parent != "principal.dana" {
		t.Fatalf("role payload = %+v", p)
	}
	if p := d.Steps[4].Payload; p.OtherID == nil || *p.OtherID != alice || p.Parent != "group.ops" {
		t.Fatalf("member payload = %+v", p)
	}
	if p := d.Steps[5].Payload; p.Other != "principal.dana" {
		t.Fatalf("member payload = %+v", p)
	}
	if p := d.Steps[6].Payload; p.ProposalStep != 2 {
		t.Fatalf("activation payload = %+v", p)
	}
	wantFinding(t, d, "principal.alice", KindUnmanagedReference)
}

func TestTheAppliedIdentityPlansNothing(t *testing.T) {
	d := diff("people", cs, mustDoc(t, identityDoc), identityApplied(), false)
	wantOps(t, d)
	if blocked(d.Findings) {
		t.Fatalf("findings = %+v", d.Findings)
	}
}

func TestPruneRevokesUndeclaredRolesAndMembersLast(t *testing.T) {
	raw := strings.Replace(identityDoc, `"roles": ["operator", "auditor"]`, `"roles": ["auditor"]`, 1)
	raw = strings.Replace(raw, `"members": ["dana", "alice"]`, `"members": ["dana"]`, 1)
	d := diff("people", cs, mustDoc(t, raw), identityApplied(), false)
	wantOps(t, d)
	wantFinding(t, d, "role.dana.operator", KindOrphan)
	wantFinding(t, d, "member.ops.alice", KindOrphan)
	d = diff("people", cs, mustDoc(t, raw), identityApplied(), true)
	wantOps(t, d, "1 approve revoke role.dana.operator", "2 approve revoke member.ops.alice")
	if p := d.Steps[0].Payload; p.ObjectID == nil || *p.ObjectID != danaOps {
		t.Fatalf("revoke payload = %+v", p)
	}
}

func TestAPendingGrantBlocks(t *testing.T) {
	st := identityApplied()
	st.People["dana"].Grants["operator"] = GrantState{ID: danaOps, Approved: false}
	d := diff("people", cs, mustDoc(t, identityDoc), st, false)
	wantFinding(t, d, "role.dana.operator", KindPending)
	for _, f := range d.Findings {
		if f.Kind == KindPending && !strings.Contains(f.Detail, danaOps.String()) {
			t.Fatalf("a pending finding names the grant to decide: %+v", f)
		}
	}
}

const adminsDoc = `{"principals": {
 "alice": {"kind": "human", "subject": "alice@example.com", "display_name": "Alice", "roles": ALICE},
 "bob": {"kind": "human", "subject": "bob@example.com", "display_name": "Bob", "roles": ["admin"]}DAVE}}`

func TestTheAdminFloorBlocksAPlanThatLeavesOneAdmin(t *testing.T) {
	st := admins()
	st.Managed["principal.alice"], st.Managed["principal.bob"] = alice, bob
	lone := strings.NewReplacer("ALICE", "[]", "DAVE", "").Replace(adminsDoc)
	d := diff("admins", cs, mustDoc(t, lone), st, true)
	wantFinding(t, d, "role.alice.admin", KindAdminFloor)
	// Without prune nothing is revoked and there is no floor to keep.
	d = diff("admins", cs, mustDoc(t, lone), st, false)
	wantFinding(t, d, "role.alice.admin", KindOrphan)
	if blocked(d.Findings) {
		t.Fatalf("findings = %+v", d.Findings)
	}
	// A new admin in the same change set keeps two.
	withDave := strings.NewReplacer("ALICE", "[]", "DAVE", `,
 "dave": {"kind": "human", "subject": "dave@example.com", "display_name": "Dave", "roles": ["admin"]}`).Replace(adminsDoc)
	d = diff("admins", cs, mustDoc(t, withDave), st, true)
	if blocked(d.Findings) {
		t.Fatalf("findings = %+v", d.Findings)
	}
	wantOps(t, d,
		"1 submit create principal.dave",
		"2 submit propose role.dave.admin",
		"3 approve activate role.dave.admin",
		"4 approve revoke role.alice.admin")
}

func TestIdentityFieldsAreImmutableAndDisabledPrincipalsAreLeftAlone(t *testing.T) {
	raw := strings.Replace(identityDoc, `"display_name": "Dana"`, `"display_name": "Dana B"`, 1)
	wantFinding(t, diff("people", cs, mustDoc(t, raw), identityApplied(), false), "principal.dana", KindUnsupported)
	raw = strings.Replace(identityDoc, `"schedule_weight": 2`, `"schedule_weight": 3`, 1)
	wantFinding(t, diff("people", cs, mustDoc(t, raw), identityApplied(), false), "group.ops", KindUnsupported)
	st := identityApplied()
	x := st.People["dana"]
	x.Disabled = true
	st.People["dana"] = x
	wantFinding(t, diff("people", cs, mustDoc(t, identityDoc), st, false), "principal.dana", KindUnsupported)
}

func TestAnAgentMayBeOwnedByAGroupCreatedInTheSameBundle(t *testing.T) {
	raw := `{"groups": {"ops": {"display_name": "Ops"}},
	 "agents": {"bot": {"display_name": "Bot", "environment": "production", "risk_class": "low",
	   "owner": {"group": "ops"}, "version": {"runtime": "python", "code_ref": "git:1"}}}}`
	d := diff("ops", cs, mustDoc(t, raw), empty(), false)
	wantOps(t, d, "1 submit create group.ops", "2 submit create agent.bot", "3 submit create version.bot")
	if p := d.Steps[0].Payload.Group; p == nil || p.Weight != 1 {
		t.Fatalf("group payload = %+v", p)
	}
	if p := d.Steps[1].Payload; p.Other != "group.ops" {
		t.Fatalf("agent payload = %+v", p)
	}
}

func TestExistingPrincipalsAreImportedByID(t *testing.T) {
	raw := strings.NewReplacer("ALICE", `["admin"]`, "DAVE", "").Replace(adminsDoc)
	d := diff("admins", cs, mustDoc(t, raw), admins(), false)
	wantFinding(t, d, "principal.alice", KindUnmanaged)
	raw = strings.TrimSuffix(raw, "}") + `, "imports": [{"to": "principal.alice", "id": "` + alice.String() +
		`"}, {"to": "principal.bob", "id": "` + bob.String() + `"}]}`
	wantOps(t, diff("admins", cs, mustDoc(t, raw), admins(), false),
		"1 submit import principal.alice", "2 submit import principal.bob")
}
