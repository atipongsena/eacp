package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestFinOpsCommands(t *testing.T) {
	env, got := recordingAPI(t)
	const id = "0b9f0e6a-4c8e-4a55-9d1e-4f2f6f1f9a01"
	dir := t.TempDir()
	array := filepath.Join(dir, "array.json")
	object := filepath.Join(dir, "object.json")
	line := `{"external_id":"inv-1","agent_id":"` + id + `","provider":"openai","cost":"1","unit":"USD","observed_at":"2026-09-01T00:00:00Z"}`
	if err := os.WriteFile(array, []byte("\n["+line+"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(object, []byte(`{"lines":[`+line+`]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	lines := map[string]any{"lines": []any{map[string]any{"external_id": "inv-1", "agent_id": id, "provider": "openai",
		"cost": "1", "unit": "USD", "observed_at": "2026-09-01T00:00:00Z"}}}
	for _, tc := range []struct {
		args   []string
		method string
		uri    string
		body   map[string]any
	}{
		{[]string{"dashboard"}, "GET", "/v1/finops/dashboard", nil},
		{[]string{"chargeback"}, "GET", "/v1/finops/chargeback", nil},
		{[]string{"chargeback", "--by", "account", "--from", "2026-09-01T00:00:00Z"}, "GET",
			"/v1/finops/chargeback?from=2026-09-01T00%3A00%3A00Z&group_by=account", nil},
		{[]string{"usage", "--agent", id, "--limit", "10"}, "GET", "/v1/finops/usage?agent_id=" + id + "&limit=10", nil},
		{[]string{"prices"}, "GET", "/v1/finops/prices", nil},
		{[]string{"price", "add", "--provider", "openai", "--model", "gpt-x", "--unit", "USD", "--input", "2.5",
			"--output", "10", "--cached", "1.25", "--reason", "list price"}, "POST", "/v1/finops/prices",
			map[string]any{"provider": "openai", "model": "gpt-x", "unit": "USD", "input_per_mtok": "2.5",
				"output_per_mtok": "10", "cached_input_per_mtok": "1.25", "reason": "list price"}},
		{[]string{"billing", "import", array}, "POST", "/v1/finops/billing", lines},
		{[]string{"billing", "import", object}, "POST", "/v1/finops/billing", lines},
		{[]string{"soft-limits"}, "GET", "/v1/finops/soft-limits", nil},
		{[]string{"soft-limit", id, "--limit", "100", "--reason", "plan"}, "PUT", "/v1/finops/soft-limits/" + id,
			map[string]any{"monthly_limit": "100", "reason": "plan"}},
		{[]string{"soft-limit", id, "--clear", "--reason", "done"}, "PUT", "/v1/finops/soft-limits/" + id,
			map[string]any{"monthly_limit": nil, "reason": "done"}},
		{[]string{"alerts", "--open"}, "GET", "/v1/finops/alerts?open=true", nil},
		{[]string{"ack", id, "--reason", "seen"}, "POST", "/v1/finops/alerts/" + id + "/ack",
			map[string]any{"reason": "seen"}},
	} {
		*got = nil
		out, err := runWith(t, env, append([]string{"finops"}, tc.args...)...)
		if err != nil || !strings.Contains(out, `"ok": true`) {
			t.Fatalf("%v: %v %s", tc.args, err, out)
		}
		if len(*got) != 1 || (*got)[0].method != tc.method || (*got)[0].uri != tc.uri {
			t.Fatalf("%v sent %+v", tc.args, *got)
		}
		if tc.body != nil && !reflect.DeepEqual((*got)[0].body, tc.body) {
			t.Fatalf("%v body = %v, want %v", tc.args, (*got)[0].body, tc.body)
		}
	}
	for _, args := range [][]string{
		{"finops"}, {"finops", "forecast"}, {"finops", "dashboard", "extra"},
		{"finops", "price", "add", "--provider", "openai"}, {"finops", "price", "remove"},
		{"finops", "billing", "import"}, {"finops", "billing", "import", filepath.Join(dir, "missing.json")},
		{"finops", "soft-limit", id, "--reason", "x"}, {"finops", "soft-limit", id, "--limit", "1", "--clear", "--reason", "x"},
		{"finops", "soft-limit", "nope", "--limit", "1", "--reason", "x"}, {"finops", "ack", id},
		{"finops", "usage", "--limit", "0"}, {"finops", "alerts", "--from", "x"},
	} {
		if _, err := runWith(t, env, args...); err == nil {
			t.Errorf("%v: want an error", args)
		}
	}
}
