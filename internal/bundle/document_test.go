package bundle

import (
	"encoding/json"
	"strings"
	"testing"
)

const validDoc = `{
 "connectors": {"ledger": {"protocol": "http", "endpoint": "http://fakeerp:8090", "secret_ref": "ledger",
   "tools": {"post_entry": {"contract": {"side_effects": ["READ_ONLY"], "idempotency_mode": "none",
     "reconciliation_lookup": "none", "reconciliation_consistency": "none", "proof_standard": "none",
     "max_attempts": 3}}}}},
 "agents": {"ledger-bot": {"display_name": "Ledger bot", "environment": "production", "risk_class": "high",
   "owner": {"principal": "carol"}, "version": {"runtime": "python", "code_ref": "git:aaa111"},
   "allowlist": ["ledger.post_entry"], "state": "ACTIVE"}}}`

func findingKinds(fs []Finding) map[string]string {
	out := map[string]string{}
	for _, f := range fs {
		out[f.Address] = f.Kind
	}
	return out
}

func TestAValidDocumentHasNoFindings(t *testing.T) {
	d, err := Decode(json.RawMessage(validDoc))
	if err != nil {
		t.Fatal(err)
	}
	if fs := Validate(d, json.RawMessage(validDoc)); len(fs) != 0 {
		t.Fatalf("findings = %+v", fs)
	}
}

func TestDecodeRefusesUnknownFieldsSoNoSecretIsCarried(t *testing.T) {
	for _, raw := range []string{
		`{"connectors": {"ledger": {"protocol": "http", "endpoint": "http://x", "secret_ref": "l", "token": "s3cr3t"}}}`,
		`{"agents": {"a1": {"password": "x"}}}`,
		`{"secrets": {}}`,
		`{"connectors": {}} {"trailing": 1}`,
		``,
	} {
		if _, err := Decode(json.RawMessage(raw)); err == nil {
			t.Errorf("Decode(%s) accepted it", raw)
		}
	}
}

func TestValidateReportsEveryStructuralProblem(t *testing.T) {
	raw := `{
	 "connectors": {"Bad_Name": {"protocol": "ftp", "endpoint": "ftp://x", "secret_ref": "has space",
	   "tools": {"t": {"contract": {"definition_id": "0b9f0e6a-4c8e-4a55-9d1e-4f2f6f1f9a01"}}}}},
	 "agents": {"bot": {"display_name": " ", "environment": "prod", "risk_class": "extreme",
	   "owner": {"principal": "carol", "group": "team"}, "version": {"runtime": "", "code_ref": ""},
	   "allowlist": ["no-dot", "a.b", "a.b"], "state": "RETIRED"},
	  "idle": {"display_name": "Idle", "environment": "staging", "risk_class": "low", "owner": {"group": "team"},
	   "version": {"runtime": "go", "code_ref": "git:1"}, "state": "ACTIVE"}},
	 "imports": [{"to": "agent.missing", "id": "0b9f0e6a-4c8e-4a55-9d1e-4f2f6f1f9a01"},
	   {"to": "allowlist.bot", "id": "0b9f0e6a-4c8e-4a55-9d1e-4f2f6f1f9a01"}]}`
	raw = strings.Replace(raw, `"code_ref": "git:1"`, `"code_ref": "${var.ref}"`, 1)
	d, err := Decode(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	got := findingKinds(Validate(d, json.RawMessage(raw)))
	for addr, kind := range map[string]string{
		"bundle":               KindUnresolvedVariable,
		"connector.Bad_Name":   KindInvalid,
		"contract.Bad_Name.t":  KindInvalid,
		"agent.bot":            KindInvalid,
		"version.bot":          KindInvalid,
		"allowlist.bot":        KindInvalid,
		"version.idle":         KindInvalid, // ACTIVE without an allowlist
		"import.agent.missing": KindInvalid,
		"import.allowlist.bot": KindInvalid,
	} {
		if got[addr] != kind {
			t.Errorf("%s: finding %q, want %q (all: %v)", addr, got[addr], kind, got)
		}
	}
}

func TestOnlyOrphansAndUnmanagedReferencesDoNotBlock(t *testing.T) {
	for kind, want := range map[string]bool{
		KindInvalid: true, KindUnresolvedVariable: true, KindUnresolvedReference: true, KindUnsupported: true,
		KindUnmanaged: true, KindRequiresRelease: true, KindContained: true,
		KindOrphan: false, KindUnmanagedReference: false,
	} {
		if (Finding{Kind: kind}).Blocking() != want {
			t.Errorf("%s blocking = %v", kind, !want)
		}
	}
}
