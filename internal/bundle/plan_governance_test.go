package bundle

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

var (
	polID   = uuid.MustParse("00000000-0000-4000-8000-00000000c001")
	rootID  = uuid.MustParse("00000000-0000-4000-8000-00000000c002")
	teamID  = uuid.MustParse("00000000-0000-4000-8000-00000000c003")
	priceID = uuid.MustParse("00000000-0000-4000-8000-00000000c004")
	openID  = uuid.MustParse("00000000-0000-4000-8000-00000000c005")
)

func TestAPolicyIsCreatedThenActivatedAndComparedCanonically(t *testing.T) {
	doc := `{"policy": {"content": ` + allowPolicy + `}}`
	st := empty()
	d := diff("gov", cs, mustDoc(t, doc), st, false)
	wantOps(t, d, "1 submit create "+PolicyAddress, "2 approve activate "+PolicyAddress)
	if d.Steps[1].Payload.ProposalStep != 1 || !slices.Contains(d.Refs, Ref{"policy", tenantID}) {
		t.Fatalf("steps %+v refs %+v", d.Steps, d.Refs)
	}
	id := polID
	st.Policy = PolicyState{ID: &id, Version: 3,
		Content: json.RawMessage(`{"rules":[{"reason":"permitted","verdict":"allow","id":"all"}],"format_version":1}`)}
	st.Managed[PolicyAddress] = polID
	wantOps(t, diff("gov", cs, mustDoc(t, doc), st, false))
	delete(st.Managed, PolicyAddress)
	wantFinding(t, diff("gov", cs, mustDoc(t, doc), st, false), PolicyAddress, KindUnmanaged)
	st.AddressElsewhere[PolicyAddress] = "other"
	wantFinding(t, diff("gov", cs, mustDoc(t, doc), st, false), PolicyAddress, KindUnmanaged)
}

const budgetDoc = `{"budgets": {
 "root": {"unit": "USD", "hard_limit": 1000},
 "team": {"unit": "USD", "parent": "root", "hard_limit": 200, "soft_limit": 150}}}`

func TestBudgetsAreCreatedParentsFirstAndRaisedByTheApprover(t *testing.T) {
	d := diff("money", cs, mustDoc(t, budgetDoc), empty(), false)
	wantOps(t, d,
		"1 submit create budget.root",
		"2 submit create budget.team",
		"3 submit propose budget.root",
		"4 submit propose budget.team",
		"5 submit set budget.team",
		"6 approve activate budget.root",
		"7 approve activate budget.team")
	if p := d.Steps[1].Payload; p.Parent != "budget.root" || p.Budget == nil || p.Budget.Unit != "USD" {
		t.Fatalf("create payload = %+v", p)
	}
	if p := d.Steps[2].Payload; p.Limit != "1000" || p.Parent != "budget.root" {
		t.Fatalf("propose payload = %+v", p)
	}
	if p := d.Steps[4].Payload; p.SoftLimit == nil || *p.SoftLimit != "150" || p.Limit != "" {
		t.Fatalf("soft limit payload = %+v", p)
	}
	if d.Steps[5].Payload.ProposalStep != 3 || d.Steps[6].Payload.ProposalStep != 4 {
		t.Fatalf("activations = %+v", d.Steps[5:])
	}
}

// moneyApplied is the state after budgetDoc was applied.
func moneyApplied() State {
	st := empty()
	soft, root := "150", rootID
	st.Budgets["root"] = BudgetState{ID: rootID, Unit: "USD", HardLimit: "1000"}
	st.Budgets["team"] = BudgetState{ID: teamID, Unit: "USD", ParentID: &root, HardLimit: "200", SoftLimit: &soft}
	st.Managed["budget.root"], st.Managed["budget.team"] = rootID, teamID
	return st
}

func TestBudgetLimitsAreComparedByValueAndLoweredChildrenFirst(t *testing.T) {
	raw := strings.Replace(budgetDoc, `"hard_limit": 1000`, `"hard_limit": 1000.000`, 1)
	wantOps(t, diff("money", cs, mustDoc(t, raw), moneyApplied(), false))
	raw = strings.Replace(budgetDoc, `"hard_limit": 1000`, `"hard_limit": 900`, 1)
	raw = strings.Replace(raw, `"hard_limit": 200`, `"hard_limit": 100`, 1)
	d := diff("money", cs, mustDoc(t, raw), moneyApplied(), false)
	wantOps(t, d, "1 submit set budget.team", "2 submit set budget.root")
	if p := d.Steps[0].Payload; p.Limit != "100" || p.ParentID == nil || *p.ParentID != teamID {
		t.Fatalf("decrease payload = %+v", p)
	}
}

func TestBudgetIdentityIsImmutableAndAnOpenIncreaseBlocks(t *testing.T) {
	raw := strings.Replace(budgetDoc, `"unit": "USD", "parent": "root"`, `"unit": "USD"`, 1)
	wantFinding(t, diff("money", cs, mustDoc(t, raw), moneyApplied(), false), "budget.team", KindUnsupported)
	st := moneyApplied()
	open := openID
	x := st.Budgets["root"]
	x.OpenProposal = &open
	st.Budgets["root"] = x
	wantFinding(t, diff("money", cs, mustDoc(t, budgetDoc), st, false), "budget.root", KindPending)
}

func TestAnAgentBudgetNamesItsAgent(t *testing.T) {
	st := empty()
	st.Agents["ledger-bot"] = AgentState{ID: agentID}
	raw := `{"budgets": {"bot": {"unit": "USD", "agent": "ledger-bot", "hard_limit": 0}}}`
	d := diff("money", cs, mustDoc(t, raw), st, false)
	wantOps(t, d, "1 submit create budget.bot")
	if p := d.Steps[0].Payload; p.OtherID == nil || *p.OtherID != agentID {
		t.Fatalf("create payload = %+v", p)
	}
	wantFinding(t, d, "agent.ledger-bot", KindUnmanagedReference)
}

const priceDoc = `{"prices": {"gpt": {"provider": "openai", "model": "gpt-4.1", "unit": "USD",
 "input_per_mtok": 2.5, "output_per_mtok": 10}}}`

func TestAPriceIsAddedWhenItDiffersFromTheOneInEffect(t *testing.T) {
	d := diff("rates", cs, mustDoc(t, priceDoc), empty(), false)
	wantOps(t, d, "1 submit create price.gpt")
	if p := d.Steps[0].Payload.Price; p == nil || p.InputPerMTok != "2.5" || p.OutputPerMTok != "10" ||
		p.CachedPerMTok != nil || p.EffectiveFrom != nil {
		t.Fatalf("price payload = %+v", p)
	}
	st := empty()
	st.Prices["openai gpt-4.1"] = PriceState{ID: priceID, Unit: "USD", Input: "2.50", Output: "10"}
	wantFinding(t, diff("rates", cs, mustDoc(t, priceDoc), st, false), "price.gpt", KindUnmanaged)
	st.Managed["price.gpt"] = priceID
	wantOps(t, diff("rates", cs, mustDoc(t, priceDoc), st, false))
	raw := strings.Replace(priceDoc, `"output_per_mtok": 10`, `"output_per_mtok": 12`, 1)
	wantOps(t, diff("rates", cs, mustDoc(t, raw), st, false), "1 submit create price.gpt")
}
