package main

import (
	"strings"
	"testing"
)

func TestKillCommands(t *testing.T) {
	env, got := recordingAPI(t)
	const id = "0b9f0e6a-4c8e-4a55-9d1e-4f2f6f1f9a01"
	for _, tc := range []struct {
		args   []string
		method string
		killed bool
	}{
		{[]string{"activate", "agent_version", id, "--reason", "incident"}, "POST", true},
		{[]string{"resume", "agent_version", id, "--reason", "cleared"}, "POST", false},
		{[]string{"activate", "run", id, "--reason", "wrong employee"}, "POST", true},
		{[]string{"list"}, "GET", false},
	} {
		*got = nil
		out, err := runWith(t, env, append([]string{"kill"}, tc.args...)...)
		if err != nil || !strings.Contains(out, `"ok": true`) {
			t.Fatalf("%v: %v %s", tc.args, err, out)
		}
		if len(*got) != 1 || (*got)[0].method != tc.method || (*got)[0].uri != "/v1/killswitch" {
			t.Fatalf("%v sent %v", tc.args, *got)
		}
		if tc.method == "POST" {
			if body := (*got)[0].body; body["scope"] != tc.args[1] || body["target_id"] != id ||
				body["killed"] != tc.killed || body["reason"] == "" {
				t.Fatalf("body = %v", body)
			}
		}
	}
}

// global still waits for platform authority (ADR-016 Rev 1.1).
func TestKillRefusesTheGlobalScope(t *testing.T) {
	env, got := recordingAPI(t)
	if _, err := runWith(t, env, "kill", "activate", "global", "0b9f0e6a-4c8e-4a55-9d1e-4f2f6f1f9a01",
		"--reason", "x"); err == nil || len(*got) != 0 {
		t.Fatalf("global = %v, sent %v", err, *got)
	}
}
