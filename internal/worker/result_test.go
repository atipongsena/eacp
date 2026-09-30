package worker_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/atipongsena/eacp/internal/worker"
)

// ADR-034: an output is kept as RFC 8785 JSON of at most 65 536 bytes and
// never with a credential in it.
func TestPrepareOutput(t *testing.T) {
	for name, tc := range map[string]struct {
		in       string
		want     string
		withheld string
	}{
		"canonical":        {in: `{ "b": 2, "a": [1, 2.50] }`, want: `{"a":[1,2.5],"b":2}`},
		"any json value":   {in: `"PO-1"`, want: `"PO-1"`},
		"empty":            {in: ``},
		"not json":         {in: `{"a":`, withheld: "invalid_output"},
		"credential":       {in: `{"echo":"tok-SECRET-1"}`, withheld: "contains_credential"},
		"escaped cred":     {in: `{"echo":"tok-SECRET-1"}`, withheld: "contains_credential"},
		"at the limit":     {in: `"` + strings.Repeat("x", 65534) + `"`, want: `"` + strings.Repeat("x", 65534) + `"`},
		"over the limit":   {in: `"` + strings.Repeat("x", 65535) + `"`, withheld: "too_large"},
		"large credential": {in: `"tok-SECRET-1` + strings.Repeat("x", 70000) + `"`, withheld: "contains_credential"},
	} {
		got, withheld := worker.PrepareOutput(json.RawMessage(tc.in), []string{"", "tok-SECRET-1"})
		if string(got) != tc.want || withheld != tc.withheld {
			t.Errorf("%s: got %q withheld %q, want %q %q", name, got, withheld, tc.want, tc.withheld)
		}
	}
}

// Only a success carries output past classification.
func TestClassifyKeepsOutputOnlyForASuccess(t *testing.T) {
	out := json.RawMessage(`{"days":12}`)
	c := worker.Contract{NoEffectErrors: []string{"refused"}}
	if r := worker.ClassifyResult(worker.Result{Outcome: worker.Succeeded, ExternalReference: "R-1", Output: out}, c); string(r.Output) != string(out) {
		t.Fatalf("success output = %s", r.Output)
	}
	for _, in := range []worker.Result{
		{Outcome: worker.Succeeded, Output: out}, // no reference: ambiguous
		{Outcome: worker.Ambiguous, ErrorClass: "timeout", Output: out},
		{Outcome: worker.NoEffect, ErrorClass: "refused", Output: out},
	} {
		if r := worker.ClassifyResult(in, c); r.Output != nil {
			t.Fatalf("%s kept output %s", r.Outcome, r.Output)
		}
	}
}
