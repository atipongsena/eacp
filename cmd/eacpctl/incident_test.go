package main

import (
	"strings"
	"testing"
)

func TestIncidentCommands(t *testing.T) {
	env, got := recordingAPI(t)
	const id = "0b9f0e6a-4c8e-4a55-9d1e-4f2f6f1f9a01"
	const other = "0b9f0e6a-4c8e-4a55-9d1e-4f2f6f1f9a02"
	for _, tc := range []struct {
		args   []string
		method string
		uri    string
		body   map[string]any
	}{
		{[]string{"incident", "list"}, "GET", "/v1/incidents", nil},
		{[]string{"incident", "list", "--state", "OPEN", "--severity", "critical", "--limit", "20"}, "GET",
			"/v1/incidents?limit=20&severity=critical&state=OPEN", nil},
		{[]string{"incident", "show", id}, "GET", "/v1/incidents/" + id, nil},
		{[]string{"incident", "open", "--title", "odd", "--severity", "high", "--reason", "finance",
			"--subject-type", "tool", "--subject-id", other}, "POST", "/v1/incidents",
			map[string]any{"title": "odd", "severity": "high", "reason": "finance", "subject_type": "tool", "subject_id": other}},
		{[]string{"incident", "ack", id, "--reason", "mine"}, "POST", "/v1/incidents/" + id + "/acknowledge",
			map[string]any{"reason": "mine"}},
		{[]string{"incident", "assign", id, other}, "POST", "/v1/incidents/" + id + "/assign",
			map[string]any{"assignee_id": other}},
		{[]string{"incident", "assign", id, "none"}, "POST", "/v1/incidents/" + id + "/assign",
			map[string]any{"assignee_id": nil}},
		{[]string{"incident", "note", id, "--text", "checking"}, "POST", "/v1/incidents/" + id + "/notes",
			map[string]any{"text": "checking"}},
		{[]string{"incident", "link", id, "kill_state", other}, "POST", "/v1/incidents/" + id + "/links",
			map[string]any{"kind": "kill_state", "id": other}},
		{[]string{"incident", "resolve", id, "--code", "contained", "--reason", "done"}, "POST",
			"/v1/incidents/" + id + "/resolve", map[string]any{"resolution": "contained", "reason": "done"}},
		{[]string{"soc", "summary"}, "GET", "/v1/soc/summary", nil},
	} {
		*got = nil
		out, err := runWith(t, env, tc.args...)
		if err != nil || !strings.Contains(out, `"ok": true`) {
			t.Fatalf("%v: %v %s", tc.args, err, out)
		}
		if len(*got) != 1 || (*got)[0].method != tc.method || (*got)[0].uri != tc.uri {
			t.Fatalf("%v sent %+v", tc.args, *got)
		}
		for k, v := range tc.body {
			if (*got)[0].body[k] != v {
				t.Fatalf("%v body = %v", tc.args, (*got)[0].body)
			}
		}
	}
	for _, bad := range [][]string{{"incident"}, {"incident", "show", "x"}, {"incident", "ack", id},
		{"incident", "open", "--title", "t"}, {"incident", "open", "--title", "t", "--severity", "low", "--reason", "r",
			"--subject-type", "tool"}, {"incident", "resolve", id, "--reason", "r"}, {"incident", "link", id, "tool"},
		{"soc"}} {
		if _, err := runWith(t, env, bad...); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
}
