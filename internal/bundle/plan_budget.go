package bundle

import (
	"slices"

	"github.com/google/uuid"

	"eacp/internal/budget"
	"eacp/internal/finops"
)

// budgets plans the declared accounts. Accounts are created parents first;
// limits are lowered children first at submit (a decrease needs one admin)
// and raised parents first at approval, so each child's escrow comes from a
// parent that already holds it; soft limits are set at submit.
func (p *planner) budgets() {
	type target struct {
		name, addr, par string
		parID           *uuid.UUID
		cur             BudgetState
	}
	var ts []target
	for _, name := range p.budgetOrder() {
		want, addr := p.doc.Budgets[name], "budget."+name
		cur, exists := p.st.Budgets[name]
		parentAddr, parentID, ok := p.budgetParent(addr, want)
		if !ok {
			continue
		}
		agentAddr, agentID, ok := p.budgetAgent(addr, want)
		if !ok {
			continue
		}
		if !exists {
			p.add(StageSubmit, addr, OpCreate, Payload{Budget: &budget.NewAccount{Name: name, Unit: want.Unit},
				Parent: parentAddr, ParentID: parentID, Other: agentAddr, OtherID: agentID})
			cur = BudgetState{HardLimit: "0"}
		} else {
			if !p.owned(addr, cur.ID) {
				continue
			}
			p.ref("budget", cur.ID)
			if cur.Unit != want.Unit {
				p.find(addr, KindUnsupported, "unit is immutable: %s in the registry, %s in the bundle", cur.Unit, want.Unit)
			}
			if parentAddr != "" || !sameID(cur.ParentID, parentID) {
				p.find(addr, KindUnsupported, "parent is immutable")
			}
			if agentAddr != "" || !sameID(cur.AgentID, agentID) {
				p.find(addr, KindUnsupported, "agent is immutable")
			}
			if cur.OpenProposal != nil {
				p.find(addr, KindPending, "limit change %s is open: apply or reject it through the API first",
					*cur.OpenProposal)
			}
		}
		par, parID := parent(addr, cur.ID)
		ts = append(ts, target{name: name, addr: addr, par: par, parID: parID, cur: cur})
	}
	for i := len(ts) - 1; i >= 0; i-- { // lowered children first
		t := ts[i]
		if limit, _ := amount(p.doc.Budgets[t.name].HardLimit); cmpAmount(limit, t.cur.HardLimit) < 0 {
			p.add(StageSubmit, t.addr, OpSet, Payload{Limit: limit, Reason: p.reason, Parent: t.par, ParentID: t.parID})
		}
	}
	for _, t := range ts { // raised parents first
		if limit, _ := amount(p.doc.Budgets[t.name].HardLimit); cmpAmount(limit, t.cur.HardLimit) > 0 {
			n := p.add(StageSubmit, t.addr, OpPropose, Payload{Limit: limit, Reason: p.reason, Parent: t.par,
				ParentID: t.parID})
			p.add(StageApprove, t.addr, OpActivate, Payload{ProposalStep: n, Reason: p.reason})
		}
	}
	for _, t := range ts {
		want := p.doc.Budgets[t.name].SoftLimit
		if want == nil {
			continue
		}
		soft, _ := amount(*want)
		if t.cur.SoftLimit != nil && cmpAmount(soft, *t.cur.SoftLimit) == 0 {
			continue
		}
		p.add(StageSubmit, t.addr, OpSet, Payload{SoftLimit: &soft, Reason: p.reason, Parent: t.par, ParentID: t.parID})
	}
}

// budgetOrder is the declared accounts, parents before their children.
func (p *planner) budgetOrder() []string {
	depth := func(name string) int {
		d := 0
		for cur := p.doc.Budgets[name].Parent; cur != "" && d <= len(p.doc.Budgets); cur = p.doc.Budgets[cur].Parent {
			if _, ok := p.doc.Budgets[cur]; !ok {
				break
			}
			d++
		}
		return d
	}
	names := sortedKeys(p.doc.Budgets)
	slices.SortStableFunc(names, func(a, b string) int { return depth(a) - depth(b) })
	return names
}

// budgetParent resolves an account's parent: an existing account, or one
// this bundle creates.
func (p *planner) budgetParent(addr string, want Budget) (string, *uuid.UUID, bool) {
	if want.Parent == "" {
		return "", nil, true
	}
	cur, exists := p.st.Budgets[want.Parent]
	_, declared := p.doc.Budgets[want.Parent]
	switch {
	case exists:
		if cur.Unit != want.Unit {
			p.find(addr, KindInvalid, "parent %s is in %s, not %s", want.Parent, cur.Unit, want.Unit)
			return "", nil, false
		}
		if cur.AgentID != nil {
			p.find(addr, KindInvalid, "parent %s belongs to an agent; an agent's account has no children", want.Parent)
			return "", nil, false
		}
		id := cur.ID
		p.ref("budget", id)
		if !declared {
			p.find("budget."+want.Parent, KindUnmanagedReference, "%s names %s, which this bundle does not declare",
				addr, want.Parent)
		}
		return "", &id, true
	case declared:
		return "budget." + want.Parent, nil, true
	}
	p.find(addr, KindUnresolvedReference, "no budget %q", want.Parent)
	return "", nil, false
}

// budgetAgent resolves the agent an account is bound to.
func (p *planner) budgetAgent(addr string, want Budget) (string, *uuid.UUID, bool) {
	if want.Agent == "" {
		return "", nil, true
	}
	cur, exists := p.st.Agents[want.Agent]
	_, declared := p.doc.Agents[want.Agent]
	switch {
	case exists:
		id := cur.ID
		p.ref("agent", id)
		if !declared {
			p.find("agent."+want.Agent, KindUnmanagedReference, "%s names %s, which this bundle does not declare",
				addr, want.Agent)
		}
		return "", &id, true
	case declared:
		return "agent." + want.Agent, nil, true
	}
	p.find(addr, KindUnresolvedReference, "no agent %q", want.Agent)
	return "", nil, false
}

// price plans a declared price: a new one, effective when it is applied,
// whenever the declared rates differ from the price in effect.
func (p *planner) price(name string) {
	want, addr := p.doc.Prices[name], "price."+name
	key := want.Provider + " " + want.Model
	// One bundle prices a model: ownership follows provider and model, not
	// the id of the price in effect, which the API may have replaced.
	owner := ""
	for id, other := range p.st.ManagedElsewhere {
		if p.st.PriceKeys[id] == key && (owner == "" || other < owner) {
			owner = other
		}
	}
	if owner != "" {
		p.find(addr, KindUnmanaged, "%s is priced by bundle %s", key, owner)
		return
	}
	if id, ok := p.managed[addr]; ok {
		if k, known := p.st.PriceKeys[id]; known && k != key {
			p.find(addr, KindUnsupported, "provider and model are immutable: %s manages %s, not %s", addr, k, key)
			return
		}
	}
	cur, exists := p.st.Prices[key]
	if exists {
		if _, managed := p.managed[addr]; !managed {
			if other := p.st.ManagedElsewhere[cur.ID]; other != "" {
				p.find(addr, KindUnmanaged, "managed by bundle %s", other)
			} else {
				p.find(addr, KindUnmanaged, "%s %s is priced (%s) and not managed by this bundle: add an import",
					want.Provider, want.Model, cur.ID)
			}
			return
		}
		p.ref("price", cur.ID)
		if samePrice(cur, want) {
			return
		}
	}
	np := finops.NewPrice{Provider: want.Provider, Model: want.Model, Unit: want.Unit, Reason: p.reason}
	np.InputPerMTok, _ = amount(want.InputPerMTok)
	np.OutputPerMTok, _ = amount(want.OutputPerMTok)
	if want.CachedPerMTok != nil {
		c, _ := amount(*want.CachedPerMTok)
		np.CachedPerMTok = &c
	}
	p.add(StageSubmit, addr, OpCreate, Payload{Price: &np})
}

// samePrice compares a price in effect with a declared one by value. An
// absent cached-input price is the input price.
func samePrice(cur PriceState, want Price) bool {
	in, _ := amount(want.InputPerMTok)
	out, _ := amount(want.OutputPerMTok)
	cached, curCached := in, cur.Input
	if want.CachedPerMTok != nil {
		cached, _ = amount(*want.CachedPerMTok)
	}
	if cur.Cached != nil {
		curCached = *cur.Cached
	}
	return cur.Unit == want.Unit && cmpAmount(cur.Input, in) == 0 && cmpAmount(cur.Output, out) == 0 &&
		cmpAmount(curCached, cached) == 0
}

func sameID(a, b *uuid.UUID) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}
