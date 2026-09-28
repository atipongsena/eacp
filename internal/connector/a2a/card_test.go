package a2a_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/connector/a2a"
	"github.com/atipongsena/eacp/internal/governance"
	"github.com/atipongsena/eacp/internal/worker"
)

const token = "a2a-test-canary"

// secret returns the worker-held Bearer value for endpoint's host.
func secret(t *testing.T, endpoint, value string) worker.Secret {
	t.Helper()
	tenant := uuid.New()
	host := strings.TrimPrefix(strings.TrimPrefix(endpoint, "http://"), "https://")
	host, _, _ = strings.Cut(host, "/")
	p := filepath.Join(t.TempDir(), "secrets.json")
	body := fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"a2a","host":%q,"value":%q}]}`, tenant, host, value)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := worker.LoadSecrets(p)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Resolve(tenant, "a2a", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// card is an Agent Card whose JSON-RPC 1.0 interface is url.
func card(url string) map[string]any {
	return map[string]any{
		"name":        "Procurement",
		"description": "Buys goods <within> policy & budget",
		"version":     "1.2.0",
		"supportedInterfaces": []any{
			map[string]any{"url": "grpc://other.test:50051", "protocolBinding": "GRPC", "protocolVersion": "1.0"},
			map[string]any{"url": url, "protocolBinding": "JSONRPC", "protocolVersion": "1.0"},
		},
		"capabilities":       map[string]any{"streaming": false, "pushNotifications": false},
		"defaultInputModes":  []any{"text/plain", "application/json"},
		"defaultOutputModes": []any{"text/plain"},
		"skills": []any{map[string]any{"id": "buy", "name": "Buy", "description": "Buy goods",
			"tags": []any{"erp"}, "examples": []any{"buy 10 laptops"}}},
		"provider":         map[string]any{"organization": "Example Corp", "url": "https://example.test"},
		"iconUrl":          "https://a2a.test/icon.png",
		"documentationUrl": "https://docs.test/procurement",
	}
}

// agent serves body (or handler) as the card of an agent whose endpoint
// is the returned URL + "/a2a".
type agent struct {
	srv      *httptest.Server
	endpoint string
	requests []*http.Request
}

func newAgent(t *testing.T, serve func(a *agent, w http.ResponseWriter, r *http.Request)) *agent {
	t.Helper()
	a := &agent{}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.requests = append(a.requests, r.Clone(context.Background()))
		serve(a, w, r)
	}))
	t.Cleanup(a.srv.Close)
	a.endpoint = a.srv.URL + "/a2a"
	return a
}

func serveJSON(v any) func(*agent, http.ResponseWriter, *http.Request) {
	return func(_ *agent, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
}

func discover(t *testing.T, a *agent) (worker.Discovery, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return a2a.New().Discover(ctx, a.endpoint, secret(t, a.endpoint, token))
}

func canonical(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	c, err := governance.Canonicalize(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(c)
}

func TestTheCardBecomesTheDelegateDefinition(t *testing.T) {
	a := newAgent(t, func(a *agent, w http.ResponseWriter, r *http.Request) {
		serveJSON(card(a.endpoint))(a, w, r)
	})
	d, err := discover(t, a)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.requests) != 1 {
		t.Fatalf("%d requests", len(a.requests))
	}
	r := a.requests[0]
	if r.Method != http.MethodGet || r.URL.Path != a2a.CardPath || r.Header.Get("Accept") != "application/json" ||
		r.Header.Get("Authorization") != "Bearer "+token {
		t.Fatalf("card request %s %s, Accept %q, Authorization %q", r.Method, r.URL.Path, r.Header.Get("Accept"),
			r.Header.Get("Authorization"))
	}
	if d.ProtocolVersion != "1.0" || string(d.ServerInfo) != `{"name":"Procurement","version":"1.2.0"}` ||
		len(d.Tools) != 1 || len(d.Rejected) != 0 {
		t.Fatalf("discovery %+v", d)
	}
	certified := card(a.endpoint)
	delete(certified, "iconUrl")
	delete(certified, "documentationUrl")
	var schema any
	if err := json.Unmarshal([]byte(a2a.PayloadSchema), &schema); err != nil {
		t.Fatal(err)
	}
	want := canonical(t, map[string]any{"name": "delegate", "inputSchema": schema, "agentCard": certified})
	tool := d.Tools[0]
	if tool.RemoteName != "delegate" || tool.Definition != want {
		t.Fatalf("definition\n%s\nwant\n%s", tool.Definition, want)
	}
	if tool.Display != `{"documentationUrl":"https://docs.test/procurement","iconUrl":"https://a2a.test/icon.png"}` {
		t.Fatalf("display %s", tool.Display)
	}
	if a2a.PayloadSchema != canonical(t, schema) {
		t.Fatal("the payload schema is not canonical")
	}
}

func TestEveryBadCardFailsTheScan(t *testing.T) {
	with := func(change func(c map[string]any, endpoint string)) func(*agent, http.ResponseWriter, *http.Request) {
		return func(a *agent, w http.ResponseWriter, r *http.Request) {
			c := card(a.endpoint)
			change(c, a.endpoint)
			serveJSON(c)(a, w, r)
		}
	}
	raw := func(body string) func(*agent, http.ResponseWriter, *http.Request) {
		return func(_ *agent, w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}
	}
	status := func(code int) func(*agent, http.ResponseWriter, *http.Request) {
		return func(_ *agent, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
	}
	iface := func(url, binding, version string) []any {
		return []any{map[string]any{"url": url, "protocolBinding": binding, "protocolVersion": version}}
	}
	skills := func(n int, dup bool) []any {
		var s []any
		for i := range n {
			id := fmt.Sprintf("s%d", i)
			if dup {
				id = "same"
			}
			s = append(s, map[string]any{"id": id, "name": id, "description": id, "tags": []any{}})
		}
		return s
	}
	for _, c := range []struct {
		name  string
		serve func(*agent, http.ResponseWriter, *http.Request)
		class string
	}{
		{"not found", status(http.StatusNotFound), "http_404"},
		{"unauthorized", status(http.StatusUnauthorized), "unauthorized"},
		{"forbidden", status(http.StatusForbidden), "unauthorized"},
		{"redirect", func(a *agent, w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == a2a.CardPath {
				http.Redirect(w, r, "/elsewhere", http.StatusFound)
				return
			}
			serveJSON(card(a.endpoint))(a, w, r)
		}, "http_302"},
		{"too large", raw(`{"name":"` + strings.Repeat("x", 256<<10) + `"}`), "too_large"},
		{"too large to certify", with(func(c map[string]any, _ string) { c["description"] = strings.Repeat("x", 70<<10) }),
			"too_large"},
		{"not JSON", raw(`<html>`), "card_invalid"},
		{"not an object", raw(`[]`), "card_invalid"},
		{"duplicate member", func(a *agent, w http.ResponseWriter, r *http.Request) {
			b, _ := json.Marshal(card(a.endpoint))
			raw(`{"name":"Evil",`+string(b[1:]))(a, w, r)
		}, "card_invalid"},
		{"U+0000", with(func(c map[string]any, _ string) { c["description"] = "a\u0000b" }), "card_invalid"},
		{"no name", with(func(c map[string]any, _ string) { delete(c, "name") }), "card_invalid"},
		{"empty name", with(func(c map[string]any, _ string) { c["name"] = "" }), "card_invalid"},
		{"skills not an array", with(func(c map[string]any, _ string) { c["skills"] = map[string]any{} }), "card_invalid"},
		{"no skills", with(func(c map[string]any, _ string) { delete(c, "skills") }), "card_invalid"},
		{"201 skills", with(func(c map[string]any, _ string) { c["skills"] = skills(201, false) }), "card_invalid"},
		{"skill without id", with(func(c map[string]any, _ string) {
			c["skills"] = []any{map[string]any{"name": "x", "description": "x", "tags": []any{}}}
		}), "card_invalid"},
		{"skill not an object", with(func(c map[string]any, _ string) { c["skills"] = []any{"buy"} }), "card_invalid"},
		{"duplicate skill id", with(func(c map[string]any, _ string) { c["skills"] = skills(2, true) }), "card_invalid"},
		{"interfaces not an array", with(func(c map[string]any, _ string) { c["supportedInterfaces"] = "x" }), "card_invalid"},
		{"no JSON-RPC interface", with(func(c map[string]any, e string) { c["supportedInterfaces"] = iface(e, "GRPC", "1.0") }),
			"no_supported_interface"},
		{"JSON-RPC 0.3 only", with(func(c map[string]any, e string) {
			c["supportedInterfaces"] = iface(e, "JSONRPC", "0.3")
		}), "no_supported_interface"},
		{"v0.3 card", with(func(c map[string]any, e string) {
			delete(c, "supportedInterfaces")
			c["url"], c["preferredTransport"], c["protocolVersion"] = e, "JSONRPC", "0.3.0"
		}), "no_supported_interface"},
		{"other host", with(func(c map[string]any, e string) {
			c["supportedInterfaces"] = iface(strings.Replace(e, "127.0.0.1", "localhost", 1), "JSONRPC", "1.0")
		}), "interface_mismatch"},
		{"other path", with(func(c map[string]any, e string) { c["supportedInterfaces"] = iface(e+"/v2", "JSONRPC", "1.0") }),
			"interface_mismatch"},
		{"trailing slash", with(func(c map[string]any, e string) { c["supportedInterfaces"] = iface(e+"/", "JSONRPC", "1.0") }),
			"interface_mismatch"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := newAgent(t, c.serve)
			_, err := discover(t, a)
			if got := worker.DiscoveryClass(err); got != c.class {
				t.Fatalf("class %q (%v), want %q", got, err, c.class)
			}
			for _, r := range a.requests {
				if r.URL.Path != a2a.CardPath {
					t.Fatalf("followed to %s", r.URL.Path)
				}
			}
		})
	}

	t.Run("transport", func(t *testing.T) {
		a := newAgent(t, serveJSON(nil))
		a.srv.Close()
		_, err := discover(t, a)
		if got := worker.DiscoveryClass(err); got != "transport" {
			t.Fatalf("class %q (%v)", got, err)
		}
	})
	t.Run("endpoint", func(t *testing.T) {
		for _, e := range []string{"ftp://a2a.test/a2a", "http://u:p@a2a.test/a2a", "http://a2a.test/a2a?x=1", "/a2a"} {
			_, err := a2a.New().Discover(context.Background(), e, worker.Secret{})
			var d *worker.DiscoveryError
			if !errors.As(err, &d) || d.Class != "invalid_endpoint" {
				t.Fatalf("%s: %v", e, err)
			}
		}
	})
}
