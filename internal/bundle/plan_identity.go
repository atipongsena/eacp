package bundle

import (
	"strings"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/registry"
)

// principal plans a declared principal and its roles. Every live grant of a
// principal this bundle manages is the bundle's: prune revokes the ones it no
// longer declares, after every other step of the approval.
func (p *planner) principal(name string) {
	want, addr := p.doc.Principals[name], "principal."+name
	cur, exists := p.st.People[name]
	var id uuid.UUID
	if !exists {
		p.add(StageSubmit, addr, OpCreate, Payload{Principal: &registry.NewPrincipal{Kind: want.Kind, Name: name,
			Subject: strings.ToLower(strings.TrimSpace(want.Subject)), DisplayName: want.DisplayName}})
	} else {
		if !p.owned(addr, cur.ID) {
			return
		}
		id = cur.ID
		p.ref("principal", id)
		if cur.Disabled {
			p.find(addr, KindUnsupported, "the principal is disabled; a bundle never enables or disables anyone")
			return
		}
		for _, f := range []struct{ field, have, want string }{{"kind", cur.Kind, want.Kind},
			{"subject", cur.Subject, strings.ToLower(strings.TrimSpace(want.Subject))},
			{"display_name", cur.DisplayName, want.DisplayName}} {
			if f.have != f.want {
				p.find(addr, KindUnsupported, "%s is immutable: %q in the registry, %q in the bundle", f.field,
					f.have, f.want)
			}
		}
	}
	wanted := map[string]bool{}
	for _, r := range want.Roles {
		wanted[r] = true
	}
	for _, role := range sortedKeys(wanted) {
		raddr := "role." + name + "." + role
		if g, ok := cur.Grants[role]; ok {
			p.ref("role", g.ID)
			if !g.Approved {
				p.find(raddr, KindPending, "grant %s was proposed outside this bundle: approve or revoke it through the API first", g.ID)
			}
			continue
		}
		par, parID := parent(addr, id)
		n := p.add(StageSubmit, raddr, OpPropose, Payload{Name: role, Parent: par, ParentID: parID})
		p.add(StageApprove, raddr, OpActivate, Payload{ProposalStep: n})
		if role == "admin" && want.Kind == "human" {
			p.admins++
		}
	}
	for _, role := range sortedKeys(cur.Grants) {
		if wanted[role] {
			continue
		}
		g, raddr := cur.Grants[role], "role."+name+"."+role
		p.ref("role", g.ID)
		switch {
		case !p.prune:
			p.find(raddr, KindOrphan, "role %s is no longer declared; prune revokes it", role)
		case !g.Approved:
			p.find(raddr, KindPending, "grant %s was proposed outside this bundle: approve or revoke it through the API first", g.ID)
		default:
			gid := g.ID
			p.late = append(p.late, Step{Address: raddr, Op: OpRevoke, Stage: StageApprove,
				Payload: Payload{ObjectID: &gid, Reason: p.reason + " prune"}})
			if role == "admin" && cur.Kind == "human" {
				p.revokedAdmins = append(p.revokedAdmins, raddr)
			}
		}
	}
}

// group plans a declared group and its members. Every live membership of a
// group this bundle manages is the bundle's.
func (p *planner) group(name string) {
	want, addr := p.doc.Groups[name], "group."+name
	cur, exists := p.st.GroupRows[name]
	var id uuid.UUID
	if !exists {
		weight := want.ScheduleWeight
		if weight == 0 {
			weight = 1
		}
		p.add(StageSubmit, addr, OpCreate, Payload{Group: &GroupSpec{Name: name, DisplayName: want.DisplayName,
			Weight: weight}})
	} else {
		if !p.owned(addr, cur.ID) {
			return
		}
		id = cur.ID
		p.ref("group", id)
		if cur.DisplayName != want.DisplayName {
			p.find(addr, KindUnsupported, "display_name is immutable: %q in the registry, %q in the bundle",
				cur.DisplayName, want.DisplayName)
		}
		if want.ScheduleWeight != 0 && want.ScheduleWeight != cur.Weight {
			p.find(addr, KindUnsupported, "schedule_weight is set only when the group is created (it is %d)", cur.Weight)
		}
	}
	wanted := map[string]bool{}
	for _, m := range want.Members {
		wanted[m] = true
	}
	for _, m := range sortedKeys(wanted) {
		maddr := "member." + name + "." + m
		if mid, ok := cur.Members[m]; ok {
			p.ref("member", mid)
			continue
		}
		other, otherID, ok := p.principalRef(maddr, m)
		if !ok {
			continue
		}
		par, parID := parent(addr, id)
		p.add(StageSubmit, maddr, OpCreate, Payload{Parent: par, ParentID: parID, Other: other, OtherID: otherID})
	}
	for _, m := range sortedKeys(cur.Members) {
		if wanted[m] {
			continue
		}
		mid, maddr := cur.Members[m], "member."+name+"."+m
		p.ref("member", mid)
		if !p.prune {
			p.find(maddr, KindOrphan, "%s is no longer a declared member; prune removes it", m)
			continue
		}
		p.late = append(p.late, Step{Address: maddr, Op: OpRevoke, Stage: StageApprove,
			Payload: Payload{ObjectID: &mid, Reason: p.reason + " prune"}})
	}
}

// principalRef names a principal a step needs: an existing enabled one, or
// one this bundle creates, by address until the step runs.
func (p *planner) principalRef(addr, name string) (string, *uuid.UUID, bool) {
	cur, exists := p.st.People[name]
	_, declared := p.doc.Principals[name]
	switch {
	case exists && cur.Disabled:
		p.find(addr, KindUnresolvedReference, "principal %s is disabled", name)
		return "", nil, false
	case exists:
		id := cur.ID
		p.ref("principal", id)
		if !declared {
			p.find("principal."+name, KindUnmanagedReference, "%s names %s, which this bundle does not declare", addr, name)
		}
		return "", &id, true
	case declared:
		return "principal." + name, nil, true
	}
	p.find(addr, KindUnresolvedReference, "no principal %q", name)
	return "", nil, false
}

// adminFloor blocks a plan that revokes an admin grant and would leave fewer
// than two approved admins on enabled human principals. PostgreSQL checks
// again when the stage commits.
func (p *planner) adminFloor() {
	if len(p.revokedAdmins) == 0 {
		return
	}
	n := 0
	for _, ps := range p.st.People {
		if g, ok := ps.Grants["admin"]; ok && g.Approved && ps.Kind == "human" && !ps.Disabled {
			n++
		}
	}
	if left := n + p.admins - len(p.revokedAdmins); left < 2 {
		p.find(p.revokedAdmins[0], KindAdminFloor, "this plan would leave %d admins; a tenant keeps at least two", left)
	}
}
