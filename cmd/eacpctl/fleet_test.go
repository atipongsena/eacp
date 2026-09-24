package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestFleetCommands(t *testing.T) {
	env, got := recordingAPI(t)
	const id = "0b9f0e6a-4c8e-4a55-9d1e-4f2f6f1f9a01"
	for _, tc := range []struct {
		args   []string
		method string
		uri    string
		body   map[string]any
	}{
		{[]string{"status"}, "GET", "/v1/fleet/health", nil},
		{[]string{"status", "--environment", "production", "--window", "1h"}, "GET",
			"/v1/fleet/health?environment=production&window=1h", nil},
		{[]string{"list", "--health", "degraded"}, "GET", "/v1/fleet/agents?health=degraded", nil},
		{[]string{"operation", id}, "GET", "/v1/fleet/operations/" + id, nil},
		{[]string{"pause", "--agent", "buyer", "--agent", "seller", "--reason", "incident", "--dry-run"}, "POST",
			"/v1/fleet/operations", map[string]any{"kind": "pause", "reason": "incident", "dry_run": true,
				"selector": map[string]any{"agents": []any{"buyer", "seller"}}}},
		{[]string{"quarantine", "--tool", "erp.post", "--environment", "production", "--reason", "drift"}, "POST",
			"/v1/fleet/operations", map[string]any{"kind": "quarantine", "reason": "drift",
				"selector": map[string]any{"tool": "erp.post", "environment": "production"}}},
		{[]string{"pause", "--all", "--reason", "drill"}, "POST", "/v1/fleet/operations",
			map[string]any{"kind": "pause", "reason": "drill", "selector": map[string]any{"all": true}}},
		{[]string{"resume", id, "--reason", "cleared"}, "POST", "/v1/fleet/operations",
			map[string]any{"kind": "resume", "reason": "cleared", "source_operation_id": id}},
		{[]string{"release", id, "--reason", "cleared"}, "POST", "/v1/fleet/operations",
			map[string]any{"kind": "release", "reason": "cleared", "source_operation_id": id}},
		{[]string{"rollback", "buyer", "--to", id, "--reason", "regression"}, "POST", "/v1/fleet/operations",
			map[string]any{"kind": "rollback", "reason": "regression", "to_version_id": id,
				"selector": map[string]any{"agents": []any{"buyer"}}}},
	} {
		*got = nil
		out, err := runWith(t, env, append([]string{"fleet"}, tc.args...)...)
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
		{"fleet"}, {"fleet", "reboot"}, {"fleet", "pause", "--agent", "x"}, {"fleet", "resume", "--reason", "x"},
		{"fleet", "resume", "not-a-uuid", "--reason", "x"}, {"fleet", "rollback", "--reason", "x"},
		{"fleet", "operation"}, {"fleet", "pause", "--reason", "x", "extra"},
	} {
		if _, err := runWith(t, env, args...); err == nil {
			t.Errorf("%v: want a usage error", args)
		}
	}
}
