package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const eacpYML = `bundle:
  name: procurement
targets:
  dev:
    default: true
  prod:
    tenant: 11111111-1111-4111-8111-111111111111
    variables:
      erp_endpoint: https://erp.example.com
variables:
  erp_endpoint:
    default: http://fakeerp:8090
  code_ref: {}
resources:
  connectors:
    erp:
      protocol: http
      endpoint: ${var.erp_endpoint}
      secret_ref: erp-token
      tools:
        create_po:
          contract: {side_effects: [READ_ONLY], idempotency_mode: none, reconciliation_lookup: none,
                     reconciliation_consistency: none, proof_standard: none, max_attempts: 3}
import:
  - {to: connector.erp, id: 0b9f0e6a-4c8e-4a55-9d1e-4f2f6f1f9a01}
`

const agentsYML = `resources:
  agents:
    buyer:
      display_name: Buyer
      environment: production
      risk_class: high
      owner: {group: procurement}
      version: {runtime: python, code_ref: "${var.code_ref}"}
      allowlist: [erp.create_po]
      state: ACTIVE
`

func writeBundle(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadBundleResolvesTargetsAndVariables(t *testing.T) {
	dir := writeBundle(t, map[string]string{"eacp.yml": eacpYML, "resources/agents.yml": agentsYML})
	b, err := loadBundle(dir, "prod", map[string]string{"code_ref": "git:abc"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(b.desired)
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	conn := got["connectors"].(map[string]any)["erp"].(map[string]any)
	agent := got["agents"].(map[string]any)["buyer"].(map[string]any)
	if b.name != "procurement" || conn["endpoint"] != "https://erp.example.com" ||
		agent["version"].(map[string]any)["code_ref"] != "git:abc" {
		t.Fatalf("resolved = %s", raw)
	}
	if im := got["imports"].([]any); len(im) != 1 || im[0].(map[string]any)["to"] != "connector.erp" {
		t.Fatalf("imports = %v", got["imports"])
	}
	b, err = loadBundle(dir, "", map[string]string{"code_ref": "git:abc"}) // the default target
	if err != nil || b.targetName != "dev" {
		t.Fatalf("default target = %q, %v", b.targetName, err)
	}

	for name, tc := range map[string]struct {
		files  map[string]string
		target string
		vars   map[string]string
		want   string
	}{
		"missing variable":   {map[string]string{"eacp.yml": eacpYML, "resources/agents.yml": agentsYML}, "dev", nil, "code_ref"},
		"undeclared --var":   {map[string]string{"eacp.yml": eacpYML}, "dev", map[string]string{"nope": "x"}, "nope"},
		"unknown target":     {map[string]string{"eacp.yml": eacpYML}, "qa", nil, "qa"},
		"unknown field":      {map[string]string{"eacp.yml": eacpYML + "secrets: {a: b}\n"}, "dev", nil, "secrets"},
		"duplicate resource": {map[string]string{"eacp.yml": eacpYML, "resources/a.yml": agentsYML, "resources/b.yml": agentsYML}, "dev", map[string]string{"code_ref": "x"}, "buyer"},
		"no name":            {map[string]string{"eacp.yml": "resources: {}\n"}, "", nil, "bundle.name"},
	} {
		dir := writeBundle(t, tc.files)
		if _, err := loadBundle(dir, tc.target, tc.vars); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want mention of %q", name, err, tc.want)
		}
	}
}

func TestBundleCommands(t *testing.T) {
	env, got := recordingAPI(t)
	dir := writeBundle(t, map[string]string{"eacp.yml": eacpYML, "resources/agents.yml": agentsYML})
	const id = "0b9f0e6a-4c8e-4a55-9d1e-4f2f6f1f9a01"
	for _, tc := range []struct {
		args   []string
		method string
		uri    string
		body   map[string]any
	}{
		{[]string{"plan", "-C", dir, "--var", "code_ref=git:1", "--dry-run", "--prune"}, "POST", "/v1/change-sets", nil},
		{[]string{"approve", id}, "POST", "/v1/change-sets/" + id + "/approve", nil},
		{[]string{"reject", id, "--reason", "redo"}, "POST", "/v1/change-sets/" + id + "/reject",
			map[string]any{"reason": "redo"}},
		{[]string{"status", id}, "GET", "/v1/change-sets/" + id, nil},
		{[]string{"list", "-C", dir}, "GET", "/v1/change-sets?bundle=procurement", nil},
		{[]string{"drift", "-C", dir}, "GET", "/v1/bundles/procurement/drift", nil},
	} {
		*got = nil
		out, err := runWith(t, env, append([]string{"bundle"}, tc.args...)...)
		if err != nil || !strings.Contains(out, `"ok": true`) {
			t.Fatalf("%v: %v %s", tc.args, err, out)
		}
		if len(*got) != 1 || (*got)[0].method != tc.method || (*got)[0].uri != tc.uri {
			t.Fatalf("%v sent %+v", tc.args, *got)
		}
		if tc.body != nil && !reflect.DeepEqual((*got)[0].body, tc.body) {
			t.Fatalf("%v body = %v", tc.args, (*got)[0].body)
		}
		if tc.args[0] == "plan" {
			b := (*got)[0].body
			if b["bundle"] != "procurement" || b["dry_run"] != true || b["prune"] != true || b["desired"] == nil {
				t.Fatalf("plan body = %v", b)
			}
		}
	}
	// validate is offline.
	*got = nil
	out, err := runWith(t, env, "bundle", "validate", "-C", dir, "--var", "code_ref=git:1")
	if err != nil || len(*got) != 0 || !strings.Contains(out, `"buyer"`) {
		t.Fatalf("validate: %v, %d calls, %s", err, len(*got), out)
	}
	for _, args := range [][]string{
		{"bundle"}, {"bundle", "explode"}, {"bundle", "approve"}, {"bundle", "approve", "not-a-uuid"},
		{"bundle", "reject", id}, {"bundle", "plan", "-C", dir}, // code_ref has no value
	} {
		if _, err := runWith(t, env, args...); err == nil {
			t.Errorf("%v: want an error", args)
		}
	}
}

func TestBundleDeployPlansThenSubmitsAndChecksTheTenant(t *testing.T) {
	const id = "7c1d2e3f-0000-4000-8000-000000000001"
	var calls []string
	tenant := "11111111-1111-4111-8111-111111111111"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/me":
			_, _ = w.Write([]byte(`{"tenant_id": "` + tenant + `"}`))
		case "/v1/change-sets":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id": "` + id + `", "state": "PLANNED", "steps": []}`))
		default:
			_, _ = w.Write([]byte(`{"id": "` + id + `", "state": "SUBMITTED"}`))
		}
	}))
	t.Cleanup(srv.Close)
	env := map[string]string{"EACP_API_URL": srv.URL, "EACP_API_KEY": "k"}
	dir := writeBundle(t, map[string]string{"eacp.yml": eacpYML, "resources/agents.yml": agentsYML})

	if _, err := runWith(t, env, "bundle", "deploy", "-C", dir, "-t", "prod", "--var", "code_ref=git:1"); err != nil {
		t.Fatal(err)
	}
	want := []string{"GET /v1/me", "POST /v1/change-sets", "POST /v1/change-sets/" + id + "/submit"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v", calls)
	}
	tenant = "22222222-2222-4222-8222-222222222222"
	calls = nil
	_, err := runWith(t, env, "bundle", "deploy", "-C", dir, "-t", "prod", "--var", "code_ref=git:1")
	if err == nil || !strings.Contains(err.Error(), "tenant") || len(calls) != 1 {
		t.Fatalf("mismatched tenant: %v, calls %v", err, calls)
	}
}

// A bundle comes from a repository, so its target's api never redirects the
// API key: it must agree with EACP_API_URL, which stays required.
func TestABundleNeverRedirectsTheAPIKey(t *testing.T) {
	serve := func(hits *int) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*hits++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"change_sets": [], "entries": []}`))
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	var trusted, other int
	api, elsewhere := serve(&trusted), serve(&other)
	yml := strings.Replace(eacpYML, "  dev:\n    default: true\n", "  dev:\n    default: true\n    api: "+elsewhere.URL+"\n", 1)
	dir := writeBundle(t, map[string]string{"eacp.yml": yml})
	const id = "0b9f0e6a-4c8e-4a55-9d1e-4f2f6f1f9a01"

	for name, env := range map[string]map[string]string{
		"a different EACP_API_URL": {"EACP_API_URL": api.URL, "EACP_API_KEY": "k"},
		"no EACP_API_URL":          {"EACP_API_KEY": "k"},
	} {
		if _, err := runWith(t, env, "bundle", "list", "-C", dir); err == nil || !strings.Contains(err.Error(), "EACP_API_URL") {
			t.Errorf("%s: list = %v", name, err)
		}
	}
	// approve, status and reject never read the bundle.
	if _, err := runWith(t, map[string]string{"EACP_API_URL": api.URL, "EACP_API_KEY": "k"}, "bundle", "approve", id, "-C", dir); err != nil {
		t.Fatal(err)
	}
	if other != 0 || trusted != 1 {
		t.Fatalf("the bundle's api got %d requests, EACP_API_URL %d", other, trusted)
	}
	// A target api that agrees with EACP_API_URL is used.
	if _, err := runWith(t, map[string]string{"EACP_API_URL": elsewhere.URL + "/", "EACP_API_KEY": "k"}, "bundle", "list", "-C", dir); err != nil || other != 1 {
		t.Fatalf("agreeing api: %v, %d requests", err, other)
	}
}
