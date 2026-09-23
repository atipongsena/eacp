package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type recorded struct {
	method, uri string
	body        map[string]any
}

// recordingAPI answers every request with {"ok":true} and records it.
func recordingAPI(t *testing.T) (map[string]string, *[]recorded) {
	t.Helper()
	var got []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recorded{method: r.Method, uri: r.URL.RequestURI()}
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			if err := json.Unmarshal(b, &rec.body); err != nil {
				t.Errorf("body %q: %v", b, err)
			}
		}
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		got = append(got, rec)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return map[string]string{"EACP_API_URL": srv.URL, "EACP_API_KEY": "k"}, &got
}

func TestActionCommands(t *testing.T) {
	env, got := recordingAPI(t)
	const id, rid = "0b9f0e6a-4c8e-4a55-9d1e-4f2f6f1f9a01", "7c1d2e3f-0000-4000-8000-000000000001"
	cases := []struct {
		args []string
		want recorded
	}{
		{[]string{"list", "--state", "NEEDS_HUMAN_RESOLUTION", "--limit", "5"},
			recorded{"GET", "/v1/actions?limit=5&state=NEEDS_HUMAN_RESOLUTION", nil}},
		{[]string{"get", id}, recorded{"GET", "/v1/actions/" + id, nil}},
		{[]string{"evidence", id}, recorded{"GET", "/v1/actions/" + id + "/evidence", nil}},
		{[]string{"resolve", id, "--outcome", "succeeded", "--reason", "found it", "--evidence", "ERP search",
			"--external-reference", "PO-7"},
			recorded{"POST", "/v1/actions/" + id + "/resolutions", map[string]any{"outcome": "succeeded",
				"reason": "found it", "evidence": "ERP search", "external_reference": "PO-7"}}},
		{[]string{"resolve", id, "--outcome", "retry", "--reason", "nothing arrived"},
			recorded{"POST", "/v1/actions/" + id + "/resolutions", map[string]any{"outcome": "retry",
				"reason": "nothing arrived", "evidence": "", "external_reference": ""}}},
		{[]string{"confirm", id, rid, "--reason", "agreed"},
			recorded{"POST", "/v1/actions/" + id + "/resolutions/" + rid + "/confirm", map[string]any{"reason": "agreed"}}},
		{[]string{"withdraw", id, rid, "--reason", "wrong action"},
			recorded{"POST", "/v1/actions/" + id + "/resolutions/" + rid + "/withdraw", map[string]any{"reason": "wrong action"}}},
	}
	for _, c := range cases {
		*got = nil
		out, err := runWith(t, env, append([]string{"action"}, c.args...)...)
		if err != nil || !strings.Contains(out, `"ok": true`) {
			t.Fatalf("%v: %v\n%s", c.args, err, out)
		}
		if len(*got) != 1 {
			t.Fatalf("%v: %d requests", c.args, len(*got))
		}
		r := (*got)[0]
		wantBody, _ := json.Marshal(c.want.body)
		gotBody, _ := json.Marshal(r.body)
		if r.method != c.want.method || r.uri != c.want.uri || string(gotBody) != string(wantBody) {
			t.Errorf("%v sent %s %s %s, want %s %s %s", c.args, r.method, r.uri, gotBody,
				c.want.method, c.want.uri, wantBody)
		}
	}
}

func TestActionCommandsRejectIncompleteInput(t *testing.T) {
	env, got := recordingAPI(t)
	for _, args := range [][]string{
		{"action"},
		{"action", "list"},
		{"action", "get"},
		{"action", "evidence", "a", "b"},
		{"action", "resolve", "a", "--outcome", "failed"}, // no reason
		{"action", "resolve", "a", "--reason", "x"},       // no outcome
		{"action", "confirm", "a", "--reason", "x"},       // no resolution id
		{"action", "withdraw", "a", "r"},                  // no reason
		{"action", "resolve", "a", "--outcome", "x", "--reason", "y", "extra"},
		{"action", "delete", "a"},
	} {
		if _, err := runWith(t, env, args...); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	if len(*got) != 0 {
		t.Fatalf("incomplete input reached the API: %v", *got)
	}
}
