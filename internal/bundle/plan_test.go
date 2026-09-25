package bundle

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"eacp/internal/registry"
)

var (
	carol   = uuid.MustParse("00000000-0000-4000-8000-00000000ca01")
	cs      = uuid.MustParse("00000000-0000-4000-8000-0000000000c5")
	connID  = uuid.MustParse("00000000-0000-4000-8000-000000000c01")
	toolID  = uuid.MustParse("00000000-0000-4000-8000-000000000701")
	ctID    = uuid.MustParse("00000000-0000-4000-8000-000000000c71")
	agentID = uuid.MustParse("00000000-0000-4000-8000-000000000a01")
	v1ID    = uuid.MustParse("00000000-0000-4000-8000-000000000b01")
	v2ID    = uuid.MustParse("00000000-0000-4000-8000-000000000b02")
	defID   = uuid.MustParse("00000000-0000-4000-8000-000000000d01")
)

func readOnly() registry.Contract {
	return registry.Contract{SideEffects: []string{"READ_ONLY"}, IdempotencyMode: "none",
		ReconciliationLookup: "none", ReconciliationConsistency: "none", ProofStandard: "none", MaxAttempts: 3}
}

func mustDoc(t *testing.T, raw string) Document {
	t.Helper()
	d, err := Decode(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	if fs := Validate(d, json.RawMessage(raw)); len(fs) != 0 {
		t.Fatalf("invalid test document: %+v", fs)
	}
	return d
}

func empty() State {
	return State{Managed: map[string]uuid.UUID{}, ManagedElsewhere: map[uuid.UUID]string{},
		Connectors: map[string]ConnectorState{}, Agents: map[string]AgentState{},
		Principals: map[string]uuid.UUID{"carol": carol}, Groups: map[string]uuid.UUID{}}
}

// applied is the state after the validDoc bundle "ledger" was applied.
func applied() State {
	st := empty()
	c := readOnly()
	st.Connectors["ledger"] = ConnectorState{ID: connID, Protocol: "http", Endpoint: "http://fakeerp:8090",
		SecretRef: "ledger", Tools: map[string]ToolState{"post_entry": {ID: toolID, ActiveContractID: &ctID,
			ActiveContract: &c}}}
	st.Agents["ledger-bot"] = AgentState{ID: agentID, DisplayName: "Ledger bot", Environment: "production",
		RiskClass: "high", OwnerPrincipalID: carol, Versions: []VersionState{{ID: v1ID, Number: 1,
			Runtime: "python", CodeRef: "git:aaa111", State: registry.StateActive,
			AllowedTools: []string{"ledger.post_entry"}}}}
	for addr, id := range map[string]uuid.UUID{"connector.ledger": connID, "tool.ledger.post_entry": toolID,
		"agent.ledger-bot": agentID, "version.ledger-bot": v1ID} {
		st.Managed[addr] = id
	}
	return st
}

func ops(d Diff) []string {
	out := make([]string, 0, len(d.Steps))
	for _, s := range d.Steps {
		out = append(out, fmt.Sprintf("%d %s %s %s", s.Ordinal, s.Stage, s.Op, s.Address))
	}
	return out
}

func wantOps(t *testing.T, d Diff, want ...string) {
	t.Helper()
	if got := ops(d); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("steps:\n%s\nwant:\n%s\nfindings: %+v", strings.Join(got, "\n"), strings.Join(want, "\n"), d.Findings)
	}
}

func wantFinding(t *testing.T, d Diff, addr, kind string) {
	t.Helper()
	for _, f := range d.Findings {
		if f.Address == addr && f.Kind == kind {
			return
		}
	}
	t.Fatalf("no %s finding at %s: %+v", kind, addr, d.Findings)
}

func TestDiffCreatesEverythingInDependencyOrder(t *testing.T) {
	d := diff("ledger", cs, mustDoc(t, validDoc), empty(), false)
	wantOps(t, d,
		"1 submit create connector.ledger",
		"2 submit create tool.ledger.post_entry",
		"3 submit propose contract.ledger.post_entry",
		"4 submit create agent.ledger-bot",
		"5 submit create version.ledger-bot",
		"6 submit propose allowlist.ledger-bot",
		"7 approve activate contract.ledger.post_entry",
		"8 approve activate allowlist.ledger-bot",
		"9 approve transition version.ledger-bot")
	if p := d.Steps[1].Payload; p.Parent != "connector.ledger" || p.ParentID != nil {
		t.Fatalf("tool parent = %+v", p)
	}
	if p := d.Steps[6].Payload; p.ProposalStep != 3 {
		t.Fatalf("contract activation proposal = %d", p.ProposalStep)
	}
	if p := d.Steps[8].Payload; p.To != registry.StateActive || !strings.Contains(p.Reason, cs.String()) {
		t.Fatalf("transition = %+v", p)
	}
	if p := d.Steps[3].Payload; p.Agent == nil || p.Agent.OwnerPrincipalID != carol {
		t.Fatalf("agent payload = %+v", p)
	}
	if len(d.Findings) != 0 {
		t.Fatalf("findings = %+v", d.Findings)
	}
}

func TestDiffOfTheAppliedStateIsEmpty(t *testing.T) {
	d := diff("ledger", cs, mustDoc(t, validDoc), applied(), false)
	wantOps(t, d)
	if len(d.Findings) != 0 || len(d.Refs) == 0 {
		t.Fatalf("findings %+v, refs %v", d.Findings, d.Refs)
	}
}

func TestContractNumbersAndAllowlistOrderDoNotDrift(t *testing.T) {
	st := applied()
	c := readOnly()
	c.CostFixed = "0.000000"
	c.NoEffectErrors = []string{}
	tl := st.Connectors["ledger"].Tools["post_entry"]
	tl.ActiveContract = &c
	st.Connectors["ledger"].Tools["post_entry"] = tl
	raw := strings.Replace(validDoc, `"max_attempts": 3`, `"max_attempts": 3, "cost_fixed": 0`, 1)
	wantOps(t, diff("ledger", cs, mustDoc(t, raw), st, false))
	if !sameContract(registry.Contract{CostFixed: "5", RetryMaxCost: "1.50"},
		registry.Contract{CostFixed: "5.000000", RetryMaxCost: "1.5"}) {
		t.Fatal("equal numbers compare unequal")
	}
}

func TestDiffProposesChangedContractsAndAllowlists(t *testing.T) {
	raw := strings.Replace(validDoc, `"max_attempts": 3`, `"max_attempts": 2`, 1)
	wantOps(t, diff("ledger", cs, mustDoc(t, raw), applied(), false),
		"1 submit propose contract.ledger.post_entry",
		"2 approve activate contract.ledger.post_entry")

	st := applied()
	st.Connectors["ledger"].Tools["audit"] = ToolState{ID: uuid.New()}
	raw = strings.Replace(validDoc, `"tools": {`, `"tools": {"audit": {}, `, 1)
	raw = strings.Replace(raw, `["ledger.post_entry"]`, `["ledger.post_entry", "ledger.audit"]`, 1)
	st.Managed["tool.ledger.audit"] = st.Connectors["ledger"].Tools["audit"].ID
	d := diff("ledger", cs, mustDoc(t, raw), st, false)
	wantOps(t, d,
		"1 submit propose allowlist.ledger-bot",
		"2 approve activate allowlist.ledger-bot")
	if got := d.Steps[0].Payload.Tools; strings.Join(got, ",") != "ledger.audit,ledger.post_entry" {
		t.Fatalf("allowlist = %v", got)
	}
}

func TestANewCodeRefOfALiveAgentRequiresARelease(t *testing.T) {
	raw := strings.Replace(validDoc, "git:aaa111", "git:bbb222", 1)
	d := diff("ledger", cs, mustDoc(t, raw), applied(), false)
	wantFinding(t, d, "version.ledger-bot", KindRequiresRelease)

	// Without state ACTIVE the bundle registers the new version; the old
	// ACTIVE one is reported, never retired, even with prune.
	raw = strings.Replace(raw, `, "state": "ACTIVE"`, "", 1)
	d = diff("ledger", cs, mustDoc(t, raw), applied(), true)
	wantOps(t, d,
		"1 submit create version.ledger-bot",
		"2 submit propose allowlist.ledger-bot",
		"3 approve activate allowlist.ledger-bot")
	wantFinding(t, d, "version.ledger-bot", KindOrphan)
}

func TestDiffAdoptsAPromotedVersionAndRetiresTheOldOneOnPrune(t *testing.T) {
	st := applied()
	ag := st.Agents["ledger-bot"]
	// A release promoted v2 (git:bbb222) and suspended v1.
	ag.Versions = []VersionState{
		{ID: v2ID, Number: 2, Runtime: "python", CodeRef: "git:bbb222", State: registry.StateActive,
			AllowedTools: []string{"ledger.post_entry"}},
		{ID: v1ID, Number: 1, Runtime: "python", CodeRef: "git:aaa111", State: registry.StateSuspended,
			AllowedTools: []string{"ledger.post_entry"}},
	}
	st.Agents["ledger-bot"] = ag
	raw := strings.Replace(validDoc, "git:aaa111", "git:bbb222", 1)
	d := diff("ledger", cs, mustDoc(t, raw), st, false)
	wantOps(t, d, "1 submit import version.ledger-bot")
	wantFinding(t, d, "version.ledger-bot", KindOrphan)
	if d.Steps[0].Payload.ObjectID == nil || *d.Steps[0].Payload.ObjectID != v2ID {
		t.Fatalf("import = %+v", d.Steps[0].Payload)
	}
	d = diff("ledger", cs, mustDoc(t, raw), st, true)
	wantOps(t, d, "1 submit import version.ledger-bot", "2 approve transition version.ledger-bot")
	if p := d.Steps[1].Payload; p.To != registry.StateRetired || p.ParentID == nil || *p.ParentID != v1ID {
		t.Fatalf("retire = %+v", p)
	}
}

func TestDiffNeverUndoesContainmentOrChangesImmutableFields(t *testing.T) {
	st := applied()
	ag := st.Agents["ledger-bot"]
	ag.Versions[0].State = registry.StateSuspended
	st.Agents["ledger-bot"] = ag
	wantFinding(t, diff("ledger", cs, mustDoc(t, validDoc), st, false), "version.ledger-bot", KindContained)

	raw := strings.Replace(validDoc, "http://fakeerp:8090", "http://other:1", 1)
	raw = strings.Replace(raw, `"risk_class": "high"`, `"risk_class": "low"`, 1)
	d := diff("ledger", cs, mustDoc(t, raw), applied(), false)
	wantFinding(t, d, "connector.ledger", KindUnsupported)
	wantFinding(t, d, "agent.ledger-bot", KindUnsupported)
}

func TestExistingObjectsMustBeImportedAndBelongToOneBundle(t *testing.T) {
	st := applied()
	st.Managed = map[string]uuid.UUID{}
	d := diff("ledger", cs, mustDoc(t, validDoc), st, false)
	wantFinding(t, d, "connector.ledger", KindUnmanaged)
	wantFinding(t, d, "agent.ledger-bot", KindUnmanaged)

	raw := strings.Replace(validDoc, `"agents"`, fmt.Sprintf(`"imports": [{"to": "connector.ledger", "id": "%s"},
		{"to": "agent.ledger-bot", "id": "%s"}], "agents"`, connID, agentID), 1)
	d = diff("ledger", cs, mustDoc(t, raw), st, false)
	wantOps(t, d, "1 submit import connector.ledger", "2 submit import agent.ledger-bot",
		"3 submit import version.ledger-bot")

	st.ManagedElsewhere[connID] = "finance"
	d = diff("ledger", cs, mustDoc(t, raw), st, false)
	wantFinding(t, d, "connector.ledger", KindUnmanaged)

	raw = strings.Replace(raw, connID.String(), uuid.NewString(), 1)
	wantFinding(t, diff("ledger", cs, mustDoc(t, raw), applied(), false), "connector.ledger", KindUnresolvedReference)
}

func TestMCPToolsAreDiscoveredAndPinTheirDefinition(t *testing.T) {
	raw := strings.Replace(validDoc, `"protocol": "http"`, `"protocol": "mcp"`, 1)
	d := diff("ledger", cs, mustDoc(t, raw), empty(), false)
	wantFinding(t, d, "tool.ledger.post_entry", KindUnresolvedReference)

	st := applied()
	c := st.Connectors["ledger"]
	c.Protocol = "mcp"
	tl := c.Tools["post_entry"]
	tl.DefinitionID = &defID
	c.Tools["post_entry"] = tl
	st.Connectors["ledger"] = c
	d = diff("ledger", cs, mustDoc(t, raw), st, false)
	wantOps(t, d, "1 submit propose contract.ledger.post_entry", "2 approve activate contract.ledger.post_entry")
	if p := d.Steps[0].Payload.Contract; p.DefinitionID == nil || *p.DefinitionID != defID {
		t.Fatalf("contract does not pin the definition: %+v", p)
	}
	tl.Quarantined = true
	c.Tools["post_entry"] = tl
	wantFinding(t, diff("ledger", cs, mustDoc(t, raw), st, false), "contract.ledger.post_entry", KindContained)
}

func TestReferencesAndOrphans(t *testing.T) {
	raw := strings.Replace(validDoc, `["ledger.post_entry"]`, `["ledger.post_entry", "erp.po", "nope.x"]`, 1)
	st := applied()
	st.Connectors["erp"] = ConnectorState{ID: uuid.New(), Protocol: "http", Tools: map[string]ToolState{
		"po": {ID: uuid.New()}}}
	d := diff("ledger", cs, mustDoc(t, raw), st, false)
	wantFinding(t, d, "tool.erp.po", KindUnmanagedReference)
	wantFinding(t, d, "allowlist.ledger-bot", KindUnresolvedReference)

	raw = strings.Replace(validDoc, `"principal": "carol"`, `"principal": "nobody"`, 1)
	wantFinding(t, diff("ledger", cs, mustDoc(t, raw), empty(), false), "agent.ledger-bot", KindUnresolvedReference)

	// Dropping the tool and the agent: orphans, and with prune the tool's
	// contract is revoked. The ACTIVE version is never retired by prune.
	noAgent := `{"connectors": {"ledger": {"protocol": "http", "endpoint": "http://fakeerp:8090", "secret_ref": "ledger"}}}`
	d = diff("ledger", cs, mustDoc(t, noAgent), applied(), false)
	wantOps(t, d)
	wantFinding(t, d, "tool.ledger.post_entry", KindOrphan)
	wantFinding(t, d, "agent.ledger-bot", KindOrphan)
	wantFinding(t, d, "version.ledger-bot", KindOrphan)
	d = diff("ledger", cs, mustDoc(t, noAgent), applied(), true)
	wantOps(t, d, "1 approve revoke contract.ledger.post_entry")
	if p := d.Steps[0].Payload; p.ObjectID == nil || *p.ObjectID != ctID {
		t.Fatalf("revoke = %+v", p)
	}
}

func TestPlansAreBoundedTo2000Steps(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"connectors": {`)
	for i := range 700 {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"c%03d": {"protocol": "http", "endpoint": "http://x", "secret_ref": "s",
			"tools": {"t": {"contract": {"side_effects": ["READ_ONLY"], "idempotency_mode": "none",
			"reconciliation_lookup": "none", "reconciliation_consistency": "none", "proof_standard": "none",
			"max_attempts": 1}}}}`, i)
	}
	b.WriteString(`}}`)
	d := diff("big", cs, mustDoc(t, b.String()), empty(), false)
	wantFinding(t, d, "bundle", KindInvalid)
}
