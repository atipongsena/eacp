package bundle

import (
	"encoding/json"
	"strings"
	"testing"
)

// An A2A connector (ADR-030) is declared like an MCP one: its delegate tool
// is discovered, never declared, and its contract pins the current definition.
const a2aConnectorDoc = `{"connectors": {"procurement": {"protocol": "a2a", "endpoint": "https://agents.example.com/a2a",
	"secret_ref": "procurement"}}}`

func TestAnA2AConnectorIsPlanned(t *testing.T) {
	d, err := Decode(json.RawMessage(a2aConnectorDoc))
	if err != nil {
		t.Fatal(err)
	}
	if fs := Validate(d, json.RawMessage(a2aConnectorDoc)); len(fs) != 0 {
		t.Fatalf("findings = %+v", fs)
	}
	wantOps(t, diff("procurement", cs, mustDoc(t, a2aConnectorDoc), empty(), false), "1 submit create connector.procurement")
}

func TestA2AToolsAreNeverDeclared(t *testing.T) {
	raw := strings.Replace(validDoc, `"protocol": "http"`, `"protocol": "a2a"`, 1)
	d := diff("ledger", cs, mustDoc(t, raw), empty(), false)
	wantFinding(t, d, "tool.ledger.post_entry", KindUnresolvedReference)
	for _, s := range d.Steps {
		if s.Address == "tool.ledger.post_entry" {
			t.Fatalf("an A2A tool was declared: %+v", s)
		}
	}
}

func TestAnA2AContractPinsTheCurrentDefinition(t *testing.T) {
	raw := strings.Replace(validDoc, `"protocol": "http"`, `"protocol": "a2a"`, 1)
	st := applied()
	c := st.Connectors["ledger"]
	c.Protocol = "a2a"
	tl := c.Tools["post_entry"]
	tl.DefinitionID = nil
	c.Tools["post_entry"] = tl
	st.Connectors["ledger"] = c
	wantFinding(t, diff("ledger", cs, mustDoc(t, raw), st, false), "contract.ledger.post_entry", KindUnresolvedReference)

	tl.DefinitionID = &defID
	c.Tools["post_entry"] = tl
	d := diff("ledger", cs, mustDoc(t, raw), st, false)
	wantOps(t, d, "1 submit propose contract.ledger.post_entry", "2 approve activate contract.ledger.post_entry")
	if p := d.Steps[0].Payload.Contract; p.DefinitionID == nil || *p.DefinitionID != defID {
		t.Fatalf("contract does not pin the definition: %+v", p)
	}
	tl.Quarantined = true
	c.Tools["post_entry"] = tl
	wantFinding(t, diff("ledger", cs, mustDoc(t, raw), st, false), "contract.ledger.post_entry", KindContained)
}
