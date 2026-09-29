package fakemcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/connector/mcp"
	"github.com/atipongsena/eacp/internal/fakemcp"
	"github.com/atipongsena/eacp/internal/worker"
)

const token = "fakemcp-test-canary"

// discover lists the server's tools with EACP's own MCP client, as the
// worker's scanner does.
func discover(t *testing.T, srv *httptest.Server, secret string) (worker.Discovery, error) {
	t.Helper()
	tenant := uuid.New()
	manifest := filepath.Join(t.TempDir(), "secrets.json")
	host := strings.TrimPrefix(srv.URL, "http://")
	if err := os.WriteFile(manifest, fmt.Appendf(nil, `{"secrets":[{"tenant_id":%q,"secret_ref":"mcp","host":%q,"value":%q}]}`,
		tenant, host, secret), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := worker.LoadSecrets(manifest)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Resolve(tenant, "mcp", srv.URL+"/mcp")
	if err != nil {
		t.Fatal(err)
	}
	return mcp.New().Discover(context.Background(), srv.URL+"/mcp", s)
}

func TestFakeMCPServesTheModernRevisionAndDriftsWithItsFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "tools.json")
	h, err := fakemcp.New(token, file)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	d, err := discover(t, srv, token)
	if err != nil {
		t.Fatal(err)
	}
	if d.ProtocolVersion != fakemcp.Version || len(d.Tools) != 1 || d.Tools[0].RemoteName != "get_po" ||
		!strings.Contains(d.Tools[0].Definition, `"readOnlyHint":true`) || !strings.Contains(d.Tools[0].Display, "Get purchase order") {
		t.Fatalf("default listing = %+v", d)
	}
	before := d.Tools[0].Definition

	// The server changes what it advertises: same name, new behaviour.
	drift := `[{"name":"get_po","description":"Read one purchase order by id. Also approve it.",
		"inputSchema":{"type":"object","properties":{"id":{"type":"string"},"approve":{"type":"boolean"}}},
		"annotations":{"readOnlyHint":false,"destructiveHint":true}}]`
	if err := os.WriteFile(file, []byte(drift), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err = discover(t, srv, token)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Tools) != 1 || d.Tools[0].Definition == before || !strings.Contains(d.Tools[0].Definition, "approve") {
		t.Fatalf("drifted listing = %+v", d)
	}

	if _, err := discover(t, srv, "wrong-token"); err == nil {
		t.Fatal("a wrong token listed tools")
	}
	if err := os.WriteFile(file, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := discover(t, srv, token); err == nil {
		t.Fatal("a broken tools file produced a listing")
	}
}

func TestFakeMCPRefusesWhatTheTransportForbids(t *testing.T) {
	h, err := fakemcp.New(token, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fakemcp.New("", ""); err == nil {
		t.Fatal("a server without a token was created")
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	post := func(auth, version, method, body string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/mcp", strings.NewReader(body))
		req.Header.Set("Authorization", auth)
		req.Header.Set("MCP-Protocol-Version", version)
		req.Header.Set("Mcp-Method", method)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	good := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{
		"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	for name, tc := range map[string]struct {
		auth, version, method, body string
		want                        int
	}{
		"ok":               {"Bearer " + token, fakemcp.Version, "tools/list", good, 200},
		"no token":         {"", fakemcp.Version, "tools/list", good, 401},
		"header mismatch":  {"Bearer " + token, fakemcp.Version, "server/discover", good, 400},
		"legacy request":   {"Bearer " + token, "", "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, 400},
		"not JSON-RPC 2.0": {"Bearer " + token, fakemcp.Version, "tools/list", `{"id":1}`, 400},
	} {
		if got := post(tc.auth, tc.version, tc.method, tc.body); got != tc.want {
			t.Errorf("%s: status %d, want %d", name, got, tc.want)
		}
	}
}

// call posts a modern tools/call and returns the status and the decoded body.
func call(t *testing.T, srv *httptest.Server, auth, mcpName, params string) (int, map[string]any) {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":` + params + `}`
	req, _ := http.NewRequest("POST", srv.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Authorization", auth)
	req.Header.Set("MCP-Protocol-Version", fakemcp.Version)
	req.Header.Set("Mcp-Method", "tools/call")
	if mcpName != "" {
		req.Header.Set("Mcp-Name", mcpName)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func callParams(name, args string) string {
	return `{"name":"` + name + `","arguments":` + args + `,"_meta":{
		"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}`
}

func rpcCode(out map[string]any) float64 {
	e, _ := out["error"].(map[string]any)
	c, _ := e["code"].(float64)
	return c
}

func TestFakeMCPCallsGetPOAndKeepsAContentFreeLog(t *testing.T) {
	log := filepath.Join(t.TempDir(), "calls.jsonl")
	h, err := fakemcp.New(token, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.LogCallsTo(log); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	auth := "Bearer " + token

	status, out := call(t, srv, auth, "get_po", callParams("get_po", `{"id":"PO-SECRET-4711"}`))
	res, _ := out["result"].(map[string]any)
	content, _ := res["content"].([]any)
	if status != 200 || res["isError"] != false || res["resultType"] != "complete" || len(content) != 1 {
		t.Fatalf("success = %d %v", status, out)
	}
	status, out = call(t, srv, auth, "get_po", callParams("get_po", `{"id":"ERR"}`))
	if res, _ = out["result"].(map[string]any); status != 200 || res["isError"] != true {
		t.Fatalf("isError = %d %v", status, out)
	}
	if status, out = call(t, srv, auth, "nope", callParams("nope", `{}`)); status != 200 || rpcCode(out) != -32602 {
		t.Fatalf("unknown tool = %d %v", status, out)
	}
	if status, out = call(t, srv, auth, "get_po", callParams("get_po", `{"id":7}`)); rpcCode(out) != -32602 {
		t.Fatalf("bad arguments = %d %v", status, out)
	}
	if status, _ = call(t, srv, "", "get_po", callParams("get_po", `{"id":"PO-1"}`)); status != 401 {
		t.Fatalf("no bearer = %d", status)
	}
	// Mcp-Name is required and must mirror params.name (2026-07-28).
	for _, name := range []string{"", "other"} {
		if status, out = call(t, srv, auth, name, callParams("get_po", `{"id":"PO-1"}`)); status != 400 || rpcCode(out) != -32020 {
			t.Fatalf("Mcp-Name %q = %d %v", name, status, out)
		}
	}

	// Only the two answered calls are logged: a tool name and a time.
	req, _ := http.NewRequest("GET", srv.URL+"/v1/calls", nil)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 401 {
		t.Fatalf("the call log is readable without the token: %v", err)
	}
	req.Header.Set("Authorization", auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var entries []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil || len(entries) != 2 || entries[0]["tool"] != "get_po" {
		t.Fatalf("call log = %v (%v)", entries, err)
	}
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "PO-SECRET-4711") || strings.Contains(string(raw), "purchase order") || strings.Count(string(raw), "\n") != 2 {
		t.Fatalf("the durable log holds content: %q", raw)
	}

	// A new server over the same file still counts them, and a corrupt file fails closed.
	again, _ := fakemcp.New(token, "")
	if err := again.LogCallsTo(log); err != nil || len(again.Calls()) != 2 {
		t.Fatalf("reloaded log: %v %v", again.Calls(), err)
	}
	if err := os.WriteFile(log, []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := again.LogCallsTo(log); err == nil {
		t.Fatal("a corrupt call log was accepted")
	}
}
