package bundle

import (
	"encoding/json"
	"strings"
	"testing"
)

const allowPolicy = `{"format_version":1,"rules":[{"id":"all","verdict":"allow","reason":"permitted"}]}`

const governanceDoc = `{
 "principals": {"dana": {"kind": "human", "subject": "dana@example.com", "display_name": "Dana", "roles": ["auditor"]},
                "ci-bot": {"kind": "service", "display_name": "CI bot", "roles": ["auditor"]}},
 "groups": {"ops": {"display_name": "Ops", "schedule_weight": 2, "members": ["dana"]}},
 "policy": {"content": ` + allowPolicy + `},
 "budgets": {"root": {"unit": "USD", "hard_limit": 1000},
             "team": {"unit": "USD", "parent": "root", "hard_limit": 200, "soft_limit": 150}},
 "prices": {"gpt": {"provider": "openai", "model": "gpt-4.1", "unit": "USD", "input_per_mtok": 2.5, "output_per_mtok": 10}},
 "imports": [{"to": "policy.tenant", "id": "00000000-0000-4000-8000-00000000c001"}]}`

func TestAValidGovernanceDocumentHasNoFindings(t *testing.T) {
	d, err := Decode(json.RawMessage(governanceDoc))
	if err != nil {
		t.Fatal(err)
	}
	if fs := Validate(d, json.RawMessage(governanceDoc)); len(fs) != 0 {
		t.Fatalf("findings = %+v", fs)
	}
}

func TestValidateReportsIdentityPolicyBudgetAndPriceProblems(t *testing.T) {
	for name, tc := range map[string]struct{ from, to, addr string }{
		"service role":  {`"display_name": "CI bot", "roles": ["auditor"]`, `"display_name": "CI bot", "roles": ["admin"]`, "principal.ci-bot"},
		"no subject":    {`"subject": "dana@example.com", `, ``, "principal.dana"},
		"unknown role":  {`"display_name": "Dana", "roles": ["auditor"]`, `"display_name": "Dana", "roles": ["root"]`, "principal.dana"},
		"kind":          {`"kind": "human"`, `"kind": "robot"`, "principal.dana"},
		"weight":        {`"schedule_weight": 2`, `"schedule_weight": 11`, "group.ops"},
		"member twice":  {`"members": ["dana"]`, `"members": ["dana", "dana"]`, "group.ops"},
		"policy":        {`"rules":[{"id":"all","verdict":"allow","reason":"permitted"}]`, `"rules":[]`, PolicyAddress},
		"unit":          {`"unit": "USD", "hard_limit": 1000`, `"unit": "usd", "hard_limit": 1000`, "budget.root"},
		"precision":     {`"hard_limit": 1000`, `"hard_limit": 0.0000001`, "budget.root"},
		"negative":      {`"hard_limit": 200`, `"hard_limit": -1`, "budget.team"},
		"soft zero":     {`"soft_limit": 150`, `"soft_limit": 0`, "budget.team"},
		"cycle":         {`"unit": "USD", "hard_limit": 1000`, `"unit": "USD", "parent": "team", "hard_limit": 1000`, "budget.root"},
		"parent unit":   {`"unit": "USD", "parent": "root"`, `"unit": "EUR", "parent": "root"`, "budget.team"},
		"agent parent":  {`"unit": "USD", "hard_limit": 1000`, `"unit": "USD", "agent": "buyer", "hard_limit": 1000`, "budget.team"},
		"model":         {`"model": "gpt-4.1"`, `"model": ""`, "price.gpt"},
		"price amount":  {`"input_per_mtok": 2.5`, `"input_per_mtok": -2.5`, "price.gpt"},
		"same model":    {`"output_per_mtok": 10}}`, `"output_per_mtok": 10}, "gpt2": {"provider": "openai", "model": "gpt-4.1", "unit": "USD", "input_per_mtok": 1, "output_per_mtok": 1}}`, "price.gpt2"},
		"import a role": {`"to": "policy.tenant"`, `"to": "role.dana.auditor"`, "import.role.dana.auditor"},
	} {
		raw := strings.Replace(governanceDoc, tc.from, tc.to, 1)
		if raw == governanceDoc {
			t.Fatalf("%s: the fixture does not contain %s", name, tc.from)
		}
		d, err := Decode(json.RawMessage(raw))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := findingKinds(Validate(d, json.RawMessage(raw))); got[tc.addr] != KindInvalid {
			t.Errorf("%s: findings %v, want invalid at %s", name, got, tc.addr)
		}
	}
}

// Studio's roles (ADR-033): studio_runtime is held alone, by a service
// principal; studio_author is a human role.
func TestStudioRolesKeepTheirPrincipalKindInABundle(t *testing.T) {
	ok := strings.Replace(governanceDoc, `"display_name": "CI bot", "roles": ["auditor"]`,
		`"display_name": "CI bot", "roles": ["studio_runtime"]`, 1)
	ok = strings.Replace(ok, `"display_name": "Dana", "roles": ["auditor"]`,
		`"display_name": "Dana", "roles": ["studio_author", "auditor"]`, 1)
	d, err := Decode(json.RawMessage(ok))
	if err != nil {
		t.Fatal(err)
	}
	if fs := Validate(d, json.RawMessage(ok)); len(fs) != 0 {
		t.Fatalf("findings = %+v", fs)
	}
	for name, tc := range map[string]struct{ from, to, addr string }{
		"runtime and more": {`"roles": ["studio_runtime"]`, `"roles": ["studio_runtime", "auditor"]`, "principal.ci-bot"},
		"human runtime":    {`"roles": ["studio_author", "auditor"]`, `"roles": ["studio_runtime"]`, "principal.dana"},
		"service author":   {`"roles": ["studio_runtime"]`, `"roles": ["studio_author"]`, "principal.ci-bot"},
	} {
		raw := strings.Replace(ok, tc.from, tc.to, 1)
		d, err := Decode(json.RawMessage(raw))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := findingKinds(Validate(d, json.RawMessage(raw))); got[tc.addr] != KindInvalid {
			t.Errorf("%s: findings %v, want invalid at %s", name, got, tc.addr)
		}
	}
}

func TestAmountsAreCanonicalNumeric6(t *testing.T) {
	for in, want := range map[string]string{"1000": "1000", "1000.000": "1000", "2.50": "2.5", "0": "0",
		"0.000001": "0.000001", "1e3": "1000"} {
		if got, ok := amount(json.Number(in)); !ok || got != want {
			t.Errorf("amount(%s) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"-1", "0.0000001", "1000000000000000", "", "x"} {
		if _, ok := amount(json.Number(in)); ok {
			t.Errorf("amount(%s) is accepted", in)
		}
	}
	if cmpAmount("2.5", "2.50") != 0 || cmpAmount("10", "9") <= 0 {
		t.Fatal("amounts compare by value")
	}
}
