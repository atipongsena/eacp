package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestReleaseCommands(t *testing.T) {
	env, got := recordingAPI(t)
	const id = "0b9f0e6a-4c8e-4a55-9d1e-4f2f6f1f9a01"
	for _, tc := range []struct {
		args   []string
		method string
		uri    string
		body   map[string]any
	}{
		{[]string{"list"}, "GET", "/v1/releases", nil},
		{[]string{"list", "--agent", id, "--state", "CANARY"}, "GET", "/v1/releases?agent_id=" + id + "&state=CANARY", nil},
		{[]string{"show", id}, "GET", "/v1/releases/" + id, nil},
		{[]string{"open", "--candidate", id, "--suite", "accuracy", "--suite", "safety", "--reason", "model upgrade",
			"--min-replay", "50", "--min-shadow", "0", "--steps", "100,1000,10000", "--min-canary-actions", "30"},
			"POST", "/v1/releases", map[string]any{"candidate_version_id": id, "reason": "model upgrade",
				"required_suites": []any{"accuracy", "safety"}, "min_replay_cases": float64(50),
				"min_shadow_cases": float64(0), "canary_steps": []any{float64(100), float64(1000), float64(10000)},
				"min_canary_actions": float64(30)}},
		{[]string{"evaluation", id, "--suite", "accuracy", "--score", "0.93", "--threshold", "0.9",
			"--dataset-digest", strings.Repeat("ab", 32), "--evidence", "ci://run/7"}, "POST", "/v1/releases/" + id + "/evaluations",
			map[string]any{"suite": "accuracy", "score": "0.93", "threshold": "0.9",
				"dataset_digest": strings.Repeat("ab", 32), "evidence_ref": "ci://run/7"}},
		{[]string{"advance", id, "--from", "CANARY", "--from-bp", "100", "--reason", "healthy"}, "POST",
			"/v1/releases/" + id + "/advance", map[string]any{"from": "CANARY", "from_canary_bp": float64(100), "reason": "healthy"}},
		{[]string{"rollback", id, "--reason", "latency"}, "POST", "/v1/releases/" + id + "/rollback",
			map[string]any{"reason": "latency"}},
	} {
		*got = nil
		out, err := runWith(t, env, append([]string{"release"}, tc.args...)...)
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
		{"release"}, {"release", "promote", id}, {"release", "show"}, {"release", "show", "nope"},
		{"release", "list", "--agent", "buyer"},
		{"release", "open", "--candidate", id, "--reason", "x"}, {"release", "open", "--suite", "a", "--reason", "x"},
		{"release", "open", "--candidate", id, "--suite", "a", "--reason", "x", "--steps", "1,x"},
		{"release", "evaluation", id, "--suite", "a", "--score", "1"},
		{"release", "advance", id, "--reason", "x"}, {"release", "advance", id, "--from", "SHADOW"},
		{"release", "rollback", id}, {"release", "rollback", "nope", "--reason", "x"},
	} {
		if _, err := runWith(t, env, args...); err == nil {
			t.Errorf("%v: want an error", args)
		}
	}
}
