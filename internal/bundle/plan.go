package bundle

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"slices"
	"strings"

	"github.com/google/uuid"

	"eacp/internal/budget"
	"eacp/internal/finops"
	"eacp/internal/registry"
)

// Step operations and stages.
const (
	OpCreate     = "create"
	OpPropose    = "propose"
	OpActivate   = "activate"
	OpTransition = "transition"
	OpRevoke     = "revoke"
	OpImport     = "import"
	OpSet        = "set"

	StageSubmit  = "submit"
	StageApprove = "approve"
)

// Step is one registry write of a change set.
type Step struct {
	Ordinal  int        `json:"ordinal"`
	Address  string     `json:"address"`
	Op       string     `json:"op"`
	Stage    string     `json:"stage"`
	Payload  Payload    `json:"payload"`
	ObjectID *uuid.UUID `json:"object_id,omitempty"`
}

// Payload is what a step needs. Parent names the object the step acts on
// or under: ParentID when it exists at plan time, else the address of the
// step of this change set that creates or imports it. Other and OtherID name
// a second object the same way (a membership's principal, an agent's owner,
// an account's agent).
type Payload struct {
	Connector    *registry.NewConnector `json:"connector,omitempty"`
	Agent        *registry.NewAgent     `json:"agent,omitempty"`
	Version      *registry.NewVersion   `json:"version,omitempty"`
	Contract     *registry.Contract     `json:"contract,omitempty"`
	Name         string                 `json:"name,omitempty"`
	Tools        []string               `json:"tools,omitempty"`
	Models       []string               `json:"models,omitempty"` // kept from the active allowlist (ADR-031)
	Parent       string                 `json:"parent,omitempty"`
	ParentID     *uuid.UUID             `json:"parent_id,omitempty"`
	ProposalStep int                    `json:"proposal_step,omitempty"`
	From         registry.State         `json:"from,omitempty"`
	To           registry.State         `json:"to,omitempty"`
	Reason       string                 `json:"reason,omitempty"`
	ObjectID     *uuid.UUID             `json:"object_id,omitempty"`
	Principal    *registry.NewPrincipal `json:"principal,omitempty"`
	Group        *GroupSpec             `json:"group,omitempty"`
	Policy       json.RawMessage        `json:"policy,omitempty"`
	Budget       *budget.NewAccount     `json:"budget,omitempty"`
	Limit        string                 `json:"limit,omitempty"`
	SoftLimit    *string                `json:"soft_limit,omitempty"`
	Price        *finops.NewPrice       `json:"price,omitempty"`
	Other        string                 `json:"other,omitempty"`
	OtherID      *uuid.UUID             `json:"other_id,omitempty"`
}

// GroupSpec is a group to create.
type GroupSpec struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Weight      int    `json:"schedule_weight"`
}

// Ref is a registry object a plan read; the change set's digest covers it.
type Ref struct {
	Kind string    `json:"kind"`
	ID   uuid.UUID `json:"id"`
}

// Diff is a plan before it is recorded.
type Diff struct {
	Steps    []Step
	Findings []Finding
	Refs     []Ref
}

type planner struct {
	doc           Document
	st            State
	prune         bool
	reason        string
	managed       map[string]uuid.UUID
	submit        []Step
	approve       []Step
	findings      []Finding
	refs          map[Ref]bool
	declared      map[string]bool // "connector.tool" of the tools this plan keeps or creates
	late          []Step          // approve-stage prunes of grants and memberships, after every other step
	admins        int             // admin grants this plan proposes for human principals
	revokedAdmins []string        // the admin grants this plan revokes, by address
}

// diff computes the steps that converge st to doc. It is pure: the same
// document and state always give the same plan.
func diff(bundle string, changeSet uuid.UUID, doc Document, st State, prune bool) Diff {
	p := &planner{doc: doc, st: st, prune: prune,
		reason:  fmt.Sprintf("bundle %s change set %s", bundle, changeSet),
		managed: map[string]uuid.UUID{}, refs: map[Ref]bool{}, declared: map[string]bool{}}
	for k, v := range st.Managed {
		p.managed[k] = v
	}
	p.imports()
	for _, name := range sortedKeys(doc.Principals) {
		p.principal(name)
	}
	for _, name := range sortedKeys(doc.Groups) {
		p.group(name)
	}
	for _, name := range sortedKeys(doc.Connectors) {
		p.connector(name)
	}
	for _, name := range sortedKeys(doc.Agents) {
		p.agent(name)
	}
	p.policy()
	p.budgets()
	for _, name := range sortedKeys(doc.Prices) {
		p.price(name)
	}
	p.orphans()
	p.adminFloor()

	steps := append(append(p.submit, p.approve...), p.late...)
	for i := range steps {
		steps[i].Ordinal = i + 1
	}
	if len(steps) > MaxSteps {
		p.find("bundle", KindInvalid, "the plan has %d steps; a change set has at most %d: split the bundle",
			len(steps), MaxSteps)
	}
	refs := make([]Ref, 0, len(p.refs))
	for r := range p.refs {
		refs = append(refs, r)
	}
	slices.SortFunc(refs, func(a, b Ref) int {
		if c := strings.Compare(a.Kind, b.Kind); c != 0 {
			return c
		}
		return bytes.Compare(a.ID[:], b.ID[:])
	})
	return Diff{Steps: steps, Findings: p.findings, Refs: refs}
}

func (p *planner) find(addr, kind, format string, args ...any) {
	p.findings = append(p.findings, Finding{Address: addr, Kind: kind, Detail: fmt.Sprintf(format, args...)})
}

func (p *planner) ref(kind string, id uuid.UUID) { p.refs[Ref{kind, id}] = true }

// add appends a step and returns its ordinal (only submit ordinals are
// final while planning; the approve stage follows the whole submit stage).
func (p *planner) add(stage, addr, op string, pl Payload) int {
	s := Step{Address: addr, Op: op, Stage: stage, Payload: pl}
	if stage == StageSubmit {
		p.submit = append(p.submit, s)
		return len(p.submit)
	}
	p.approve = append(p.approve, s)
	return 0
}

// parent names addr's object: by id when it exists, else by address.
func parent(addr string, id uuid.UUID) (string, *uuid.UUID) {
	if id != uuid.Nil {
		return "", &id
	}
	return addr, nil
}

// owned reports whether this bundle manages the existing object at addr.
func (p *planner) owned(addr string, id uuid.UUID) bool {
	if p.managed[addr] == id {
		return true
	}
	if other := p.st.ManagedElsewhere[id]; other != "" {
		p.find(addr, KindUnmanaged, "managed by bundle %s", other)
		return false
	}
	p.find(addr, KindUnmanaged, "exists (%s) and is not managed by this bundle: add an import", id)
	return false
}

func (p *planner) imports() {
	for _, im := range p.doc.Imports {
		kind, name, _ := strings.Cut(im.To, ".")
		if !p.names(kind, name, im.ID) {
			p.find(im.To, KindUnresolvedReference, "import id %s is not the %s named %s", im.ID, kind, name)
			continue
		}
		if p.managed[im.To] == im.ID {
			continue
		}
		if other := p.st.ManagedElsewhere[im.ID]; other != "" {
			p.find(im.To, KindUnmanaged, "managed by bundle %s", other)
			continue
		}
		id := im.ID
		p.add(StageSubmit, im.To, OpImport, Payload{ObjectID: &id})
		p.managed[im.To] = im.ID
		p.ref(kind, im.ID)
	}
}

// names reports whether id is the object that kind and name denote.
func (p *planner) names(kind, name string, id uuid.UUID) bool {
	st := p.st
	switch kind {
	case "connector":
		return st.Connectors[name].ID == id && id != uuid.Nil
	case "tool":
		conn, tool, _ := strings.Cut(name, ".")
		t, ok := st.Connectors[conn].Tools[tool]
		return ok && t.ID == id
	case "agent":
		return st.Agents[name].ID == id && id != uuid.Nil
	case "version":
		return slices.ContainsFunc(st.Agents[name].Versions, func(v VersionState) bool { return v.ID == id })
	case "principal":
		return st.People[name].ID == id && id != uuid.Nil
	case "group":
		return st.GroupRows[name].ID == id && id != uuid.Nil
	case "budget":
		return st.Budgets[name].ID == id && id != uuid.Nil
	case "policy":
		return name == "tenant" && st.Policy.ID != nil && *st.Policy.ID == id
	case "price":
		w := p.doc.Prices[name]
		cur, ok := st.Prices[w.Provider+" "+w.Model]
		return ok && cur.ID == id
	}
	return false
}

func (p *planner) connector(name string) {
	want, addr := p.doc.Connectors[name], "connector."+name
	cur, exists := p.st.Connectors[name]
	var id uuid.UUID
	if !exists {
		p.add(StageSubmit, addr, OpCreate, Payload{Connector: &registry.NewConnector{Name: name,
			Protocol: want.Protocol, Endpoint: want.Endpoint, SecretRef: want.SecretRef}})
	} else {
		if !p.owned(addr, cur.ID) {
			return
		}
		id = cur.ID
		p.ref("connector", id)
		for field, pair := range map[string][2]string{"protocol": {cur.Protocol, want.Protocol},
			"endpoint": {cur.Endpoint, want.Endpoint}, "secret_ref": {cur.SecretRef, want.SecretRef}} {
			if pair[0] != pair[1] {
				p.find(addr, KindUnsupported, "%s is immutable: %q in the registry, %q in the bundle", field,
					pair[0], pair[1])
			}
		}
	}
	for _, tn := range sortedKeys(want.Tools) {
		p.tool(name, tn, want, cur.Tools, id)
	}
}

func (p *planner) tool(conn, name string, want Connector, tools map[string]ToolState, connID uuid.UUID) {
	addr, caddr := "tool."+conn+"."+name, "contract."+conn+"."+name
	ts, exists := tools[name]
	var toolID uuid.UUID
	switch {
	case !exists && discovered(want.Protocol):
		p.find(addr, KindUnresolvedReference, "MCP and A2A tools are discovered, never declared (ADR-023, ADR-030): scan the connector first")
		return
	case !exists:
		par, parID := parent("connector."+conn, connID)
		p.add(StageSubmit, addr, OpCreate, Payload{Name: name, Parent: par, ParentID: parID})
	default:
		toolID = ts.ID
		p.ref("tool", ts.ID)
		if ts.ActiveContractID != nil {
			p.ref("contract", *ts.ActiveContractID)
		}
	}
	p.declared[conn+"."+name] = true
	c := want.Tools[name].Contract
	if c == nil {
		return
	}
	desired := *c
	if discovered(want.Protocol) {
		if ts.Quarantined {
			p.find(caddr, KindContained, "the tool is quarantined: release it through the registry (ADR-023)")
			return
		}
		if ts.DefinitionID == nil {
			p.find(caddr, KindUnresolvedReference, "the tool has no current definition")
			return
		}
		def := *ts.DefinitionID
		desired.DefinitionID = &def
	}
	if exists && ts.ActiveContract != nil && sameContract(*ts.ActiveContract, desired) {
		return
	}
	par, parID := parent(addr, toolID)
	n := p.add(StageSubmit, caddr, OpPropose, Payload{Contract: &desired, Parent: par, ParentID: parID})
	p.add(StageApprove, caddr, OpActivate, Payload{Parent: par, ParentID: parID, ProposalStep: n})
}

func (p *planner) agent(name string) {
	want, addr := p.doc.Agents[name], "agent."+name
	cur, exists := p.st.Agents[name]
	var id uuid.UUID
	na := registry.NewAgent{Name: name, DisplayName: want.DisplayName, Environment: want.Environment,
		RiskClass: want.RiskClass}
	other, ok := p.owner(addr, want.Owner, &na)
	if !ok {
		return
	}
	if !exists {
		p.add(StageSubmit, addr, OpCreate, Payload{Agent: &na, Other: other})
	} else {
		if !p.owned(addr, cur.ID) {
			return
		}
		id = cur.ID
		p.ref("agent", id)
		for field, pair := range map[string][2]string{"display_name": {cur.DisplayName, want.DisplayName},
			"environment": {cur.Environment, want.Environment}, "risk_class": {cur.RiskClass, want.RiskClass},
			"owner": {cur.OwnerPrincipalID.String() + cur.OwnerGroupID.String(),
				na.OwnerPrincipalID.String() + na.OwnerGroupID.String()}} {
			if pair[0] != pair[1] {
				p.find(addr, KindUnsupported, "%s is immutable: agents are never changed in place", field)
			}
		}
	}
	p.version(name, want, cur, id)
}

// owner resolves the agent's owner: an existing principal or group, or one
// this bundle creates, named by its address until the step runs.
func (p *planner) owner(addr string, o Owner, na *registry.NewAgent) (string, bool) {
	if o.Principal != "" {
		if id, ok := p.st.Principals[o.Principal]; ok {
			na.OwnerPrincipalID = id
			p.ref("principal", id)
			return "", true
		}
		if _, declared := p.doc.Principals[o.Principal]; declared {
			if _, exists := p.st.People[o.Principal]; !exists {
				return "principal." + o.Principal, true
			}
		}
		p.find(addr, KindUnresolvedReference, "no enabled principal %q", o.Principal)
		return "", false
	}
	if id, ok := p.st.Groups[o.Group]; ok {
		na.OwnerGroupID = id
		p.ref("group", id)
		return "", true
	}
	if _, declared := p.doc.Groups[o.Group]; declared {
		return "group." + o.Group, true
	}
	p.find(addr, KindUnresolvedReference, "no group %q", o.Group)
	return "", false
}

func (p *planner) version(name string, want Agent, cur AgentState, agentID uuid.UUID) {
	vaddr, laddr := "version."+name, "allowlist."+name
	var v *VersionState
	for i := range cur.Versions { // newest first
		x := &cur.Versions[i]
		p.ref("version", x.ID)
		if v == nil && x.Runtime == want.Version.Runtime && x.CodeRef == want.Version.CodeRef &&
			x.State != registry.StateRetired && x.State != registry.StateRevoked {
			v = x
		}
	}
	old := p.managed[vaddr]
	var versionID uuid.UUID
	switch {
	case v != nil && v.ID == old:
		versionID = v.ID
	case v != nil:
		versionID = v.ID
		id := v.ID
		p.add(StageSubmit, vaddr, OpImport, Payload{ObjectID: &id})
	default:
		par, parID := parent("agent."+name, agentID)
		p.add(StageSubmit, vaddr, OpCreate, Payload{Version: &registry.NewVersion{Runtime: want.Version.Runtime,
			CodeRef: want.Version.CodeRef}, Parent: par, ParentID: parID})
	}
	if old != uuid.Nil && old != versionID {
		p.replaced(vaddr, cur, old)
	}

	tools := slices.Clone(want.Allowlist)
	slices.Sort(tools)
	for _, t := range tools {
		p.toolRef(laddr, t)
	}
	var current, models []string
	if v != nil {
		current, models = v.AllowedTools, v.AllowedModels
	}
	if len(tools) > 0 && !slices.Equal(tools, current) {
		par, parID := parent(vaddr, versionID)
		n := p.add(StageSubmit, laddr, OpPropose, Payload{Tools: tools, Models: models, Parent: par, ParentID: parID})
		p.add(StageApprove, laddr, OpActivate, Payload{Parent: par, ParentID: parID, ProposalStep: n})
	}

	if want.State != string(registry.StateActive) {
		return
	}
	if v != nil {
		switch v.State {
		case registry.StateActive:
			return
		case registry.StateSuspended, registry.StateQuarantined:
			p.find(vaddr, KindContained, "version %d is %s: resume or release it through fleet operations (ADR-024)",
				v.Number, v.State)
			return
		}
	}
	for _, x := range cur.Versions {
		if x.State == registry.StateActive && (v == nil || x.ID != v.ID) {
			p.find(vaddr, KindRequiresRelease, "version %d is ACTIVE: promote the new version with a release (ADR-018)",
				x.Number)
			return
		}
	}
	from := registry.StateRegistered
	if v != nil {
		from = v.State
	}
	par, parID := parent(vaddr, versionID)
	p.add(StageApprove, vaddr, OpTransition, Payload{From: from, To: registry.StateActive, Reason: p.reason,
		Parent: par, ParentID: parID})
}

// replaced handles a version the bundle managed but no longer wants: with
// prune a REGISTERED or SUSPENDED one is retired; an ACTIVE one never is.
func (p *planner) replaced(addr string, cur AgentState, old uuid.UUID) {
	for _, x := range cur.Versions {
		if x.ID != old {
			continue
		}
		switch {
		case x.State == registry.StateRetired || x.State == registry.StateRevoked:
		case p.prune && (x.State == registry.StateRegistered || x.State == registry.StateSuspended):
			id := x.ID
			p.ref("version", id)
			p.add(StageApprove, addr, OpTransition, Payload{From: x.State, To: registry.StateRetired,
				Reason: p.reason + " prune", ParentID: &id})
		default:
			p.find(addr, KindOrphan, "version %d (%s) is no longer declared; nothing is retired while it is %s",
				x.Number, x.State, x.State)
		}
	}
}

func (p *planner) toolRef(addr, ref string) {
	if p.declared[ref] {
		return
	}
	conn, tool, _ := strings.Cut(ref, ".")
	if ts, ok := p.st.Connectors[conn].Tools[tool]; ok {
		p.ref("tool", ts.ID)
		p.find("tool."+ref, KindUnmanagedReference, "%s uses %s, which this bundle does not declare", addr, ref)
		return
	}
	p.find(addr, KindUnresolvedReference, "no tool %s", ref)
}

func (p *planner) orphans() {
	for _, addr := range sortedKeys(p.st.Managed) {
		if declared(p.doc, addr) {
			continue
		}
		kind, name, _ := strings.Cut(addr, ".")
		switch kind {
		case "tool":
			conn, tool, _ := strings.Cut(name, ".")
			ts := p.st.Connectors[conn].Tools[tool]
			if p.prune && ts.ActiveContract != nil {
				id := *ts.ActiveContractID
				p.ref("contract", id)
				p.add(StageApprove, "contract."+name, OpRevoke, Payload{ObjectID: &id, Reason: p.reason + " prune"})
				continue
			}
		case "version":
			p.replaced(addr, p.st.Agents[name], p.st.Managed[addr])
			continue
		}
		p.find(addr, KindOrphan, "%s is managed by this bundle but no longer declared; nothing is deleted", addr)
	}
}

// sameContract compares contracts as the database stores them: arrays as
// sets and numbers by value.
func sameContract(a, b registry.Contract) bool {
	ja, _ := json.Marshal(normalContract(a))
	jb, _ := json.Marshal(normalContract(b))
	return bytes.Equal(ja, jb)
}

func normalContract(c registry.Contract) registry.Contract {
	c.SideEffects = sortedCopy(c.SideEffects)
	c.NoEffectErrors = sortedCopy(c.NoEffectErrors)
	c.CostFixed = normalNumber(c.CostFixed, "0")
	c.RetryMaxCost = normalNumber(c.RetryMaxCost, "")
	return c
}

func normalNumber(n json.Number, empty string) json.Number {
	if n == "" {
		n = json.Number(empty)
	}
	if n == "" {
		return ""
	}
	r, ok := new(big.Rat).SetString(string(n))
	if !ok {
		return n
	}
	return json.Number(r.RatString())
}

// sortedCopy is s sorted and without duplicates, as the database stores it.
func sortedCopy(s []string) []string {
	out := slices.Clone(s)
	if out == nil {
		out = []string{}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
