package mcp_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/connector/mcp"
	"github.com/atipongsena/eacp/internal/connector/mcp/mcptest"
	"github.com/atipongsena/eacp/internal/governance"
	"github.com/atipongsena/eacp/internal/jwttest"
	"github.com/atipongsena/eacp/internal/worker"
)

// createTool is the certified tool of these tests: its outputSchema
// requires a string "po".
const createTool = `{"name":"create_po","description":"Create a purchase order",` +
	`"inputSchema":{"type":"object","properties":{"amount":{"type":"number"},"note":{"type":"string"}}},` +
	`"outputSchema":{"type":"object","properties":{"po":{"type":"string"}},"required":["po"]}}`

// payload is an enforced payload in RFC 8785 form; JCS does not escape <, &
// or >, so the server must receive exactly these bytes.
const payload = `{"amount":5,"note":"a<b&c>"}`

var reference = regexp.MustCompile(`^mcp:sha256:[0-9a-f]{64}$`)

func canonical(t *testing.T, raw string) string {
	t.Helper()
	b, err := governance.Canonicalize([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// toolCall is one dispatch of create_po (or of tool, when given) to endpoint.
func toolCall(t *testing.T, endpoint, args string, tool ...string) worker.Call {
	t.Helper()
	def := createTool
	if len(tool) > 0 {
		def = tool[0]
	}
	tenant, action := uuid.New(), uuid.New()
	return worker.Call{TenantID: tenant, ActionID: action, OperationKey: "eacp:" + tenant.String() + ":" + action.String(),
		Attempt: 1, Generation: 1, Tool: "erp.create_po", Endpoint: endpoint, Payload: json.RawMessage(args),
		RemoteName: "create_po", Definition: canonical(t, def),
		Contract: worker.Contract{Version: 1, SideEffects: []string{"REVERSIBLE_WRITE"}, IdempotencyMode: "none", MaxAttempts: 1},
		Secret:   secret(t, endpoint, token)}
}

// digestOf is the reference a success must carry for result.
func digestOf(t *testing.T, result any) string {
	t.Helper()
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(canonical(t, string(b))))
	return "mcp:sha256:" + hex.EncodeToString(sum[:])
}

func execute(t *testing.T, c *mcp.Client, call worker.Call, timeout time.Duration) worker.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return c.Execute(ctx, call)
}

// rawServer is a modern MCP server that answers server/discover itself, lists
// createTool and answers every tools/call with answer, counting them.
func rawServer(t *testing.T, answer func(w http.ResponseWriter, r *http.Request, id json.RawMessage)) (string, *atomic.Int32) {
	t.Helper()
	return rawListing(t, func(w http.ResponseWriter, _ *http.Request, id json.RawMessage) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","tools":[%s]}}`, id, createTool)
	}, answer)
}

// rawListing is a modern MCP server that answers server/discover itself,
// every tools/list with list and every tools/call with answer, counting the
// tools/call requests.
func rawListing(t *testing.T, list, answer func(w http.ResponseWriter, r *http.Request, id json.RawMessage)) (string, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "tools/call":
			calls.Add(1)
			answer(w, r, req.ID)
		case "tools/list":
			list(w, r, req.ID)
		default:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","supportedVersions":["2026-07-28"]}}`, req.ID)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/mcp", &calls
}

// syncBuffer is a log sink safe for concurrent writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type classRow struct {
	name    string
	tool    string // the listed and certified tool (default createTool)
	server  func(t *testing.T, tool string) (url string, calls func() int)
	timeout time.Duration // default 5 s
	outcome worker.Outcome
	class   string
	result  any // a success: the result whose digest is the reference
	calls   int // tools/call requests that reached the server
}

// replying is a modern mcptest server listing tool that answers tools/call
// with reply.
func replying(reply mcptest.Reply) func(*testing.T, string) (string, func() int) {
	return func(t *testing.T, tool string) (string, func() int) {
		s := mcptest.New(t, mcptest.Modern, token, tool)
		s.OnCall(func(string, json.RawMessage) mcptest.Reply { return reply })
		return s.URL(), func() int { return len(s.Calls()) }
	}
}

func raw(answer func(w http.ResponseWriter, r *http.Request, id json.RawMessage)) func(*testing.T, string) (string, func() int) {
	return func(t *testing.T, _ string) (string, func() int) {
		u, calls := rawServer(t, answer)
		return u, func() int { return int(calls.Load()) }
	}
}

func rpcReply(code int) mcptest.Reply { return mcptest.Reply{RPCCode: code, RPCMessage: "refused"} }

// rawResult answers tools/call with a JSON-RPC response whose result is the
// raw JSON text result.
func rawResult(result string) func(*testing.T, string) (string, func() int) {
	return raw(func(w http.ResponseWriter, _ *http.Request, id json.RawMessage) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, id, result)
	})
}

var (
	success     = map[string]any{"resultType": "complete", "content": []any{map[string]any{"type": "text", "text": "PO-1 raised"}}, "structuredContent": map[string]any{"po": "PO-1"}}
	textOnly    = map[string]any{"resultType": "complete", "content": []any{map[string]any{"type": "text", "text": "PO-2 raised"}}}
	unresolving = `{"name":"create_po","inputSchema":{"type":"object"},"outputSchema":{"$ref":"https://schemas.example/po.json"}}`
)

func classRows() []classRow {
	return []classRow{
		{name: "success", server: replying(mcptest.Reply{Result: success}), outcome: worker.Succeeded, result: success, calls: 1},
		{name: "success without structured content", server: replying(mcptest.Reply{Result: textOnly}), outcome: worker.Succeeded, result: textOnly, calls: 1},
		{name: "tool error", server: replying(mcptest.Reply{Result: map[string]any{"resultType": "complete", "isError": true,
			"content": []any{map[string]any{"type": "text", "text": "no budget"}}}}), outcome: worker.Ambiguous, class: "mcp_tool_error", calls: 1},
		{name: "isError that is not a boolean", server: replying(mcptest.Reply{Result: map[string]any{"resultType": "complete", "isError": "no",
			"content": []any{}}}), outcome: worker.Ambiguous, class: "invalid_response", calls: 1},
		{name: "structured content breaks the output schema", server: replying(mcptest.Reply{Result: map[string]any{"resultType": "complete",
			"content": []any{}, "structuredContent": map[string]any{"po": 7}}}), outcome: worker.Ambiguous, class: "mcp_output_invalid", calls: 1},
		{name: "an output schema that does not resolve", tool: unresolving, server: replying(mcptest.Reply{Result: success}),
			outcome: worker.Ambiguous, class: "mcp_output_invalid", calls: 1},
		{name: "input required", server: replying(mcptest.Reply{Result: map[string]any{"resultType": "input_required", "inputRequests": map[string]any{}}}),
			outcome: worker.Ambiguous, class: "mcp_input_required", calls: 1},
		{name: "parse error", server: replying(rpcReply(-32700)), outcome: worker.NoEffect, class: "mcp_rpc_32700", calls: 1},
		{name: "invalid request", server: replying(rpcReply(-32600)), outcome: worker.NoEffect, class: "mcp_rpc_32600", calls: 1},
		{name: "method not found", server: replying(rpcReply(-32601)), outcome: worker.NoEffect, class: "mcp_rpc_32601", calls: 1},
		{name: "invalid params", server: replying(rpcReply(-32602)), outcome: worker.NoEffect, class: "mcp_rpc_32602", calls: 1},
		{name: "internal error", server: replying(rpcReply(-32603)), outcome: worker.Ambiguous, class: "mcp_rpc_32603", calls: 1},
		{name: "header mismatch after the send", server: replying(rpcReply(-32020)), outcome: worker.Ambiguous, class: "mcp_rpc_32020", calls: 1},
		{name: "JSON-RPC error with HTTP 400", server: replying(mcptest.Reply{RPCStatus: http.StatusBadRequest, RPCCode: -32602, RPCMessage: "bad"}),
			outcome: worker.NoEffect, class: "mcp_rpc_32602", calls: 1},
		{name: "401 in reply to the call", server: replying(mcptest.Reply{RPCStatus: http.StatusUnauthorized}), outcome: worker.NoEffect, class: "unauthorized", calls: 1},
		{name: "403 in reply to the call", server: replying(mcptest.Reply{RPCStatus: http.StatusForbidden}), outcome: worker.NoEffect, class: "unauthorized", calls: 1},
		{name: "HTTP 500 in reply to the call", server: replying(mcptest.Reply{RPCStatus: http.StatusInternalServerError}), outcome: worker.Ambiguous, class: "http_status", calls: 1},
		{name: "401 before the call", server: func(t *testing.T, tool string) (string, func() int) {
			s := mcptest.New(t, mcptest.Modern, "another-token", tool)
			return s.URL(), func() int { return len(s.Calls()) }
		}, outcome: worker.NoEffect, class: "unauthorized", calls: 0},
		{name: "connection refused", server: func(t *testing.T, tool string) (string, func() int) {
			s := mcptest.New(t, mcptest.Modern, token, tool)
			s.Close()
			return s.URL(), func() int { return 0 }
		}, outcome: worker.NoEffect, class: "connection_refused_before_send", calls: 0},
		{name: "initialize fails before the call", server: func(t *testing.T, tool string) (string, func() int) {
			s := mcptest.New(t, mcptest.Legacy, token, tool)
			s.SetLegacyVersion("2024-11-05")
			return s.URL(), func() int { return len(s.Calls()) }
		}, outcome: worker.NoEffect, class: "definition_unverified", calls: 0},
		{name: "no reply before the deadline", server: replying(mcptest.Reply{Result: success, Delay: time.Minute}), timeout: time.Second,
			outcome: worker.Ambiguous, class: "timeout", calls: 1},
		{name: "malformed JSON", server: raw(func(w http.ResponseWriter, _ *http.Request, _ json.RawMessage) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":2,"result":{"content":[`)
		}), outcome: worker.Ambiguous, class: "invalid_response", calls: 1},
		{name: "a reply over the response budget", server: replying(mcptest.Reply{Result: map[string]any{"resultType": "complete",
			"content": []any{map[string]any{"type": "text", "text": strings.Repeat("x", 5<<20)}}}}),
			outcome: worker.Ambiguous, class: "response_too_large", calls: 1},
		{name: "a null result", server: rawResult(`null`), outcome: worker.Ambiguous, class: "invalid_response", calls: 1},
		{name: "an array result", server: rawResult(`[]`), outcome: worker.Ambiguous, class: "invalid_response", calls: 1},
		{name: "a number result", server: rawResult(`1`), outcome: worker.Ambiguous, class: "invalid_response", calls: 1},
		{name: "a string result", server: rawResult(`"x"`), outcome: worker.Ambiguous, class: "invalid_response", calls: 1},
		{name: "a positive JSON-RPC code", server: replying(rpcReply(32602)), outcome: worker.Ambiguous, class: "invalid_response", calls: 1},
	}
}

// TestExecuteClassifiesEveryReply runs every row of ADR-032 S3.4.
func TestExecuteClassifiesEveryReply(t *testing.T) {
	for _, row := range classRows() {
		t.Run(row.name, func(t *testing.T) {
			tool := row.tool
			if tool == "" {
				tool = createTool
			}
			url, calls := row.server(t, tool)
			timeout := row.timeout
			if timeout == 0 {
				timeout = 5 * time.Second
			}
			res := execute(t, mcp.New(), toolCall(t, url, payload, tool), timeout)
			if res.Outcome != row.outcome || res.ErrorClass != row.class {
				t.Fatalf("result %+v, want %s %q", res, row.outcome, row.class)
			}
			if row.outcome == worker.Succeeded {
				if !reference.MatchString(res.ExternalReference) || res.ExternalReference != digestOf(t, row.result) {
					t.Fatalf("reference %q, want %q", res.ExternalReference, digestOf(t, row.result))
				}
			} else if res.ExternalReference != "" || res.RemoteReference != "" {
				t.Fatalf("a %s result carries references %+v", row.outcome, res)
			}
			if got := calls(); got != row.calls {
				t.Fatalf("%d tools/call requests reached the server, want %d", got, row.calls)
			}
		})
	}
}

// TestExecuteCertifiedToolErrorIsNoEffect: isError is a no effect only when
// the contract certifies mcp_tool_error (ADR-032 S3.4); otherwise it stays
// ambiguous.
func TestExecuteCertifiedToolErrorIsNoEffect(t *testing.T) {
	toolError := mcptest.Reply{Result: map[string]any{"resultType": "complete", "isError": true,
		"content": []any{map[string]any{"type": "text", "text": "no budget"}}}}
	for _, c := range []struct {
		name      string
		certified []string
		outcome   worker.Outcome
	}{
		{"not certified", nil, worker.Ambiguous},
		{"other classes certified", []string{"mcp_rpc_32602", "unauthorized"}, worker.Ambiguous},
		{"certified", []string{"mcp_rpc_32602", "mcp_tool_error"}, worker.NoEffect},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := mcptest.New(t, mcptest.Modern, token, createTool)
			s.OnCall(func(string, json.RawMessage) mcptest.Reply { return toolError })
			call := toolCall(t, s.URL(), payload)
			call.Contract.NoEffectErrors = c.certified
			res := execute(t, mcp.New(), call, 5*time.Second)
			if res.Outcome != c.outcome || res.ErrorClass != "mcp_tool_error" || res.ExternalReference != "" {
				t.Fatalf("result %+v, want %s mcp_tool_error", res, c.outcome)
			}
			if len(s.Calls()) != 1 {
				t.Fatalf("%d calls", len(s.Calls()))
			}
		})
	}
}

// TestExecuteSendsTheRemoteNameAndThePayload: the server receives the
// tool's remote name and the enforced payload byte for byte, with the
// modern headers and _meta.
func TestExecuteSendsTheRemoteNameAndThePayload(t *testing.T) {
	s := mcptest.New(t, mcptest.Modern, token, createTool)
	s.OnCall(func(string, json.RawMessage) mcptest.Reply { return mcptest.Reply{Result: success} })
	if res := execute(t, mcp.New(), toolCall(t, s.URL(), payload), 5*time.Second); res.Outcome != worker.Succeeded {
		t.Fatalf("result %+v", res)
	}
	calls := s.Calls()
	if len(calls) != 1 || calls[0].Name != "create_po" || string(calls[0].Arguments) != payload {
		t.Fatalf("calls %+v", calls)
	}
	h := calls[0].Header
	if h.Get("Authorization") != "Bearer "+token || h.Get("Mcp-Method") != "tools/call" || h.Get("MCP-Protocol-Version") != mcp.Modern {
		t.Fatalf("headers %v", h)
	}
	if h.Get("Mcp-Name") != "create_po" {
		t.Fatalf("Mcp-Name %q, want the remote name", h.Get("Mcp-Name"))
	}
	for _, r := range s.Requests() {
		if r.RPCMethod != "tools/call" {
			if r.Header.Get("Mcp-Name") != "" {
				t.Fatalf("%s carries Mcp-Name %q", r.RPCMethod, r.Header.Get("Mcp-Name"))
			}
			continue
		}
		var meta map[string]json.RawMessage
		if json.Unmarshal(r.Params["_meta"], &meta) != nil || string(meta["io.modelcontextprotocol/protocolVersion"]) != `"2026-07-28"` {
			t.Fatalf("tools/call without modern _meta: %s", r.Params["_meta"])
		}
	}
}

// TestExecuteRefusesAPayloadThatIsNotAnObject (Review Focus 2): nothing is
// sent.
func TestExecuteRefusesAPayloadThatIsNotAnObject(t *testing.T) {
	for _, p := range []string{`null`, `[]`, `"x"`, `1`, ``, `{"a":1} {}`} {
		t.Run(p, func(t *testing.T) {
			s := mcptest.New(t, mcptest.Modern, token, createTool)
			res := execute(t, mcp.New(), toolCall(t, s.URL(), p), 5*time.Second)
			if res.Outcome != worker.NoEffect || res.ErrorClass != "invalid_payload" {
				t.Fatalf("result %+v", res)
			}
			if len(s.Calls()) != 0 || len(s.Requests()) != 0 {
				t.Fatalf("calls %d, requests %d", len(s.Calls()), len(s.Requests()))
			}
		})
	}
}

// TestExecuteRefusesAnUnsafeContract: an MCP contract is single-attempt with
// no idempotency (ADR-032 S3.2); anything else is refused before any request.
func TestExecuteRefusesAnUnsafeContract(t *testing.T) {
	cases := map[string]func(*worker.Call){
		"native idempotency":     func(c *worker.Call) { c.Contract.IdempotencyMode = "native" },
		"correlation only":       func(c *worker.Call) { c.Contract.IdempotencyMode = "correlation_only" },
		"no idempotency mode":    func(c *worker.Call) { c.Contract.IdempotencyMode = "" },
		"two attempts":           func(c *worker.Call) { c.Contract.MaxAttempts = 2 },
		"zero attempts":          func(c *worker.Call) { c.Contract.MaxAttempts = 0 },
		"no remote name":         func(c *worker.Call) { c.RemoteName = "" },
		"an invalid remote name": func(c *worker.Call) { c.RemoteName = "create po" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			s := mcptest.New(t, mcptest.Modern, token, createTool)
			call := toolCall(t, s.URL(), payload)
			change(&call)
			res := execute(t, mcp.New(), call, 5*time.Second)
			if res.Outcome != worker.Ambiguous || res.ErrorClass != "invalid_contract" {
				t.Fatalf("result %+v", res)
			}
			if len(s.Requests()) != 0 {
				t.Fatalf("%d requests reached the server", len(s.Requests()))
			}
		})
	}
}

// TestExecuteRefusesASigningCredential: AWS keys sign requests; an MCP
// server never receives one (ADR-019 §3f).
func TestExecuteRefusesASigningCredential(t *testing.T) {
	var contacted atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { contacted.Add(1) }))
	defer srv.Close()
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<AssumeRoleWithWebIdentityResponse><AssumeRoleWithWebIdentityResult><Credentials>`+
			`<AccessKeyId>ASIAMCPEXECUTETEST01</AccessKeyId><SecretAccessKey>mcp-secret</SecretAccessKey>`+
			`<SessionToken>mcp-session</SessionToken><Expiration>%s</Expiration></Credentials>`+
			`</AssumeRoleWithWebIdentityResult></AssumeRoleWithWebIdentityResponse>`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	}))
	defer sts.Close()
	subject := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(subject, []byte(jwttest.New(t).Sign(map[string]any{"sub": "w", "exp": time.Now().Add(time.Hour).Unix()})), 0o600); err != nil {
		t.Fatal(err)
	}
	tenant := uuid.New()
	p := filepath.Join(t.TempDir(), "secrets.json")
	body := fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"mcp","host":%q,"aws":{"role_arn":"arn:aws:iam::123456789012:role/eacp",
		"region":"us-east-1","service":"execute-api","sts_endpoint":%q,"subject_token":{"file":%q}}}]}`,
		tenant, strings.TrimPrefix(srv.URL, "http://"), sts.URL, subject)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := worker.LoadSecrets(p, worker.AllowPlainTokenURL())
	if err != nil {
		t.Fatal(err)
	}
	endpoint := srv.URL + "/mcp"
	sec, err := store.Credential(context.Background(), tenant, "mcp", endpoint, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	call := toolCall(t, endpoint, payload)
	call.TenantID, call.Secret = tenant, sec
	res := execute(t, mcp.New(), call, 5*time.Second)
	if res.Outcome != worker.Ambiguous || res.ErrorClass != "unsupported_credential" || contacted.Load() != 0 {
		t.Fatalf("result %+v, contacted %d", res, contacted.Load())
	}
}

// TestExecuteWorksOverSSE (Review Focus 4): the reply arrives on an event
// stream after a comment and an unrelated notification.
func TestExecuteWorksOverSSE(t *testing.T) {
	for _, c := range []struct {
		reply   mcptest.Reply
		outcome worker.Outcome
		class   string
	}{
		{mcptest.Reply{Result: success}, worker.Succeeded, ""},
		{mcptest.Reply{Result: map[string]any{"resultType": "complete", "isError": true, "content": []any{}}}, worker.Ambiguous, "mcp_tool_error"},
		{rpcReply(-32602), worker.NoEffect, "mcp_rpc_32602"},
	} {
		t.Run(string(c.outcome)+c.class, func(t *testing.T) {
			s := mcptest.New(t, mcptest.Modern, token, createTool)
			s.SetSSE(true)
			s.OnCall(func(string, json.RawMessage) mcptest.Reply { return c.reply })
			res := execute(t, mcp.New(), toolCall(t, s.URL(), payload), 5*time.Second)
			if res.Outcome != c.outcome || res.ErrorClass != c.class {
				t.Fatalf("result %+v", res)
			}
			if c.outcome == worker.Succeeded && res.ExternalReference != digestOf(t, success) {
				t.Fatalf("reference %q", res.ExternalReference)
			}
			if len(s.Calls()) != 1 {
				t.Fatalf("%d calls", len(s.Calls()))
			}
		})
	}
}

// TestExecuteWorksOnALegacyServer (Review Focus 5): initialize, the call in
// the session, then the session is closed.
func TestExecuteWorksOnALegacyServer(t *testing.T) {
	legacyResult := map[string]any{"content": []any{map[string]any{"type": "text", "text": "PO-3 raised"}}, "structuredContent": map[string]any{"po": "PO-3"}}
	for _, version := range []string{"2025-11-25", "2025-06-18", "2025-03-26"} {
		t.Run(version, func(t *testing.T) {
			s := mcptest.New(t, mcptest.Legacy, token, createTool)
			s.SetLegacyVersion(version)
			s.OnCall(func(string, json.RawMessage) mcptest.Reply { return mcptest.Reply{Result: legacyResult} })
			res := execute(t, mcp.New(), toolCall(t, s.URL(), payload), 5*time.Second)
			if res.Outcome != worker.Succeeded || res.ExternalReference != digestOf(t, legacyResult) {
				t.Fatalf("result %+v", res)
			}
			var flow []string
			for _, r := range s.Requests() {
				flow = append(flow, r.HTTPMethod+" "+r.RPCMethod+" "+r.Header.Get("Mcp-Session-Id")+" "+r.Header.Get("MCP-Protocol-Version"))
			}
			want := []string{
				"POST server/discover  2026-07-28",
				"POST initialize  ",
				"POST notifications/initialized session-1 " + version,
				"POST tools/list session-1 " + version,
				"POST tools/call session-1 " + version,
				"DELETE  session-1 " + version,
			}
			if strings.Join(flow, "|") != strings.Join(want, "|") {
				t.Fatalf("flow\n%s\nwant\n%s", strings.Join(flow, "\n"), strings.Join(want, "\n"))
			}
			if calls := s.Calls(); len(calls) != 1 || calls[0].Name != "create_po" || string(calls[0].Arguments) != payload {
				t.Fatalf("calls %+v", calls)
			}
		})
	}
}

// TestExecuteNeverLeaksOutput (ADR-032 invariant 3): a canary in the tool's
// content, structured content and error text appears in no result field and
// no log line, and the bearer token in no log line.
func TestExecuteNeverLeaksOutput(t *testing.T) {
	const canary = "mcp-output-canary-5d1c"
	for name, reply := range map[string]mcptest.Reply{
		"success": {Result: map[string]any{"resultType": "complete", "content": []any{map[string]any{"type": "text", "text": canary}},
			"structuredContent": map[string]any{"po": canary}}},
		"tool error": {Result: map[string]any{"resultType": "complete", "isError": true,
			"content": []any{map[string]any{"type": "text", "text": canary}}, "structuredContent": map[string]any{"po": canary}}},
		"invalid output": {Result: map[string]any{"resultType": "complete", "content": []any{map[string]any{"type": "text", "text": canary}},
			"structuredContent": map[string]any{"po": 1, "note": canary}}},
		"input required": {Result: map[string]any{"resultType": "input_required", "inputRequests": map[string]any{canary: canary}}},
		"protocol error": {RPCCode: -32602, RPCMessage: canary},
		"server error":   {RPCCode: -32000, RPCMessage: canary},
	} {
		t.Run(name, func(t *testing.T) {
			s := mcptest.New(t, mcptest.Modern, token, createTool)
			s.OnCall(func(string, json.RawMessage) mcptest.Reply { return reply })
			buf := &syncBuffer{}
			c := mcp.New()
			c.Log = slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			call := toolCall(t, s.URL(), payload)
			res := execute(t, c, call, 5*time.Second)
			if len(s.Calls()) != 1 {
				t.Fatalf("%d calls", len(s.Calls()))
			}
			// Only a success returns output, and only in Output (ADR-034),
			// for the worker to keep or drop per the contract.
			if (name == "success") != strings.Contains(string(res.Output), canary) {
				t.Fatalf("%s: output %s", name, res.Output)
			}
			res.Output = nil
			if strings.Contains(fmt.Sprintf("%#v", res), canary) {
				t.Fatalf("the result carries tool output: %+v", res)
			}
			logs := buf.String()
			if !strings.Contains(logs, call.ActionID.String()) {
				t.Fatalf("the call was not logged: %s", logs)
			}
			if strings.Contains(logs, canary) || strings.Contains(logs, token) {
				t.Fatalf("a log line carries tool output or the token: %s", logs)
			}
		})
	}
}

// TestExecuteSendsOnceAndNeverFollowsARedirect (ADR-032 invariants 1 and
// 4): a redirect is not followed, a reset after the send is not replayed,
// and every row of the classification sends at most one tools/call.
func TestExecuteSendsOnceAndNeverFollowsARedirect(t *testing.T) {
	t.Run("redirect", func(t *testing.T) {
		var reached atomic.Int32
		elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1) }))
		defer elsewhere.Close()
		url, calls := rawServer(t, func(w http.ResponseWriter, r *http.Request, _ json.RawMessage) {
			http.Redirect(w, r, elsewhere.URL+"/mcp", http.StatusTemporaryRedirect)
		})
		res := execute(t, mcp.New(), toolCall(t, url, payload), 5*time.Second)
		if res.Outcome != worker.Ambiguous || res.ErrorClass != "http_status" {
			t.Fatalf("result %+v", res)
		}
		if reached.Load() != 0 || calls.Load() != 1 {
			t.Fatalf("redirect target reached %d times, %d calls", reached.Load(), calls.Load())
		}
	})
	t.Run("a reset after the send is not replayed", func(t *testing.T) {
		url, calls := rawServer(t, func(w http.ResponseWriter, _ *http.Request, _ json.RawMessage) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
		})
		res := execute(t, mcp.New(), toolCall(t, url, payload), 5*time.Second)
		if res.Outcome != worker.Ambiguous || res.ErrorClass != "transport_error" || calls.Load() != 1 {
			t.Fatalf("result %+v, %d calls", res, calls.Load())
		}
	})
	t.Run("every row sends at most once", func(t *testing.T) {
		for _, row := range classRows() {
			if row.calls > 1 {
				t.Fatalf("row %q expects %d calls", row.name, row.calls)
			}
		}
	})
}

// TestLookupIsAlwaysUnknown: MCP has no lookup by operation key (ADR-032
// S3.1).
func TestLookupIsAlwaysUnknown(t *testing.T) {
	var c worker.Connector = mcp.New()
	if r := c.Lookup(context.Background(), worker.LookupCall{OperationKey: "k"}); r.Status != worker.LookupUnknown || r.ExternalReference != "" {
		t.Fatalf("lookup %+v", r)
	}
}

// otherTool is a listed tool that is not the certified one.
const otherTool = `{"name":"list_po","description":"List purchase orders","inputSchema":{"type":"object"}}`

// TestAChangedDefinitionIsNeverCalled (ADR-032 S3.5): a server that lists the
// certified tool differently is not called, and the alert names no definition
// text.
func TestAChangedDefinitionIsNeverCalled(t *testing.T) {
	for name, listed := range map[string]string{
		"a changed description":  strings.Replace(createTool, "Create a purchase order", "Create a purchase order and email the vendor", 1),
		"a changed inputSchema":  strings.Replace(createTool, `"note":{"type":"string"}`, `"note":{"type":"string"},"vendor":{"type":"string"}`, 1),
		"new annotations":        strings.Replace(createTool, `"inputSchema"`, `"annotations":{"readOnlyHint":true},"inputSchema"`, 1),
		"a changed outputSchema": strings.Replace(createTool, `"required":["po"]`, `"required":[]`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if listed == createTool {
				t.Fatal("the fixture did not change the definition")
			}
			s := mcptest.New(t, mcptest.Modern, token, listed)
			buf := &syncBuffer{}
			c := mcp.New()
			c.Log = slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			call := toolCall(t, s.URL(), payload)
			res := execute(t, c, call, 5*time.Second)
			if res.Outcome != worker.NoEffect || res.ErrorClass != "definition_changed" || res.ExternalReference != "" {
				t.Fatalf("result %+v", res)
			}
			if len(s.Calls()) != 0 {
				t.Fatalf("%d tools/call requests reached the server", len(s.Calls()))
			}
			logs := buf.String()
			if !strings.Contains(logs, `"alert":"worker.mcp_definition_changed"`) {
				t.Fatalf("no alert in the log: %s", logs)
			}
			for _, want := range []string{call.ActionID.String(), "erp.create_po", hostOf(t, s.URL())} {
				if !strings.Contains(logs, want) {
					t.Fatalf("the alert lacks %q: %s", want, logs)
				}
			}
			for _, secret := range []string{"purchase order", "vendor", "readOnlyHint", "inputSchema", token} {
				if strings.Contains(logs, secret) {
					t.Fatalf("the log carries %q: %s", secret, logs)
				}
			}
		})
	}
}

func hostOf(t *testing.T, endpoint string) string {
	t.Helper()
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

// TestAMissingOrRejectedToolIsNeverCalled: a tool that is not listed, or is
// listed but rejected by the scanner's rules, is tool_missing.
func TestAMissingOrRejectedToolIsNeverCalled(t *testing.T) {
	rejected := strings.Replace(createTool, `"note":{"type":"string"}`, `"note":{"type":"string","x-mcp-header":"not a token"}`, 1)
	for name, listed := range map[string][]string{
		"not listed":                  {otherTool},
		"nothing listed":              {},
		"rejected by the scanner":     {rejected},
		"listed twice":                {createTool, createTool},
		"rejected beside another one": {otherTool, rejected},
	} {
		t.Run(name, func(t *testing.T) {
			s := mcptest.New(t, mcptest.Modern, token, listed...)
			res := execute(t, mcp.New(), toolCall(t, s.URL(), payload), 5*time.Second)
			if res.Outcome != worker.NoEffect || res.ErrorClass != "tool_missing" {
				t.Fatalf("result %+v", res)
			}
			if len(s.Calls()) != 0 {
				t.Fatalf("%d tools/call requests reached the server", len(s.Calls()))
			}
		})
	}
}

// TestAnUnverifiableListingIsNeverCalled: a listing that cannot complete
// means nothing was called; it is definition_unverified, never timeout or
// response_too_large.
func TestAnUnverifiableListingIsNeverCalled(t *testing.T) {
	answer := func(w http.ResponseWriter, _ *http.Request, id json.RawMessage) {
		t.Errorf("a tools/call was sent")
	}
	unverified := func(t *testing.T, u string, calls *atomic.Int32, timeout time.Duration) {
		t.Helper()
		res := execute(t, mcp.New(), toolCall(t, u, payload), timeout)
		if res.Outcome != worker.NoEffect || res.ErrorClass != "definition_unverified" || calls.Load() != 0 {
			t.Fatalf("result %+v, %d calls", res, calls.Load())
		}
	}
	t.Run("a failing second page", func(t *testing.T) {
		var pages atomic.Int32
		u, calls := rawListing(t, func(w http.ResponseWriter, r *http.Request, id json.RawMessage) {
			if pages.Add(1) > 1 {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","tools":[%s],"nextCursor":"1"}}`, id, otherTool)
		}, answer)
		unverified(t, u, calls, 5*time.Second)
		if pages.Load() != 2 {
			t.Fatalf("%d pages requested", pages.Load())
		}
	})
	t.Run("a listing over the response budget", func(t *testing.T) {
		huge := `{"name":"create_po","description":"` + strings.Repeat("x", 5<<20) + `","inputSchema":{"type":"object"}}`
		s := mcptest.New(t, mcptest.Modern, token, huge)
		res := execute(t, mcp.New(), toolCall(t, s.URL(), payload), 10*time.Second)
		if res.Outcome != worker.NoEffect || res.ErrorClass != "definition_unverified" || len(s.Calls()) != 0 {
			t.Fatalf("result %+v, %d calls", res, len(s.Calls()))
		}
	})
	t.Run("a hang while listing", func(t *testing.T) {
		u, calls := rawListing(t, func(w http.ResponseWriter, r *http.Request, id json.RawMessage) {
			<-r.Context().Done()
		}, answer)
		unverified(t, u, calls, time.Second)
	})
	t.Run("a cancelled listing", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		u, calls := rawListing(t, func(w http.ResponseWriter, r *http.Request, id json.RawMessage) {
			cancel()
			<-r.Context().Done()
		}, answer)
		res := mcp.New().Execute(ctx, toolCall(t, u, payload))
		if res.Outcome != worker.NoEffect || res.ErrorClass != "definition_unverified" || calls.Load() != 0 {
			t.Fatalf("result %+v, %d calls", res, calls.Load())
		}
	})
	t.Run("a listing that is not a listing", func(t *testing.T) {
		u, calls := rawListing(t, func(w http.ResponseWriter, _ *http.Request, id json.RawMessage) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete"}}`, id)
		}, answer)
		unverified(t, u, calls, 5*time.Second)
	})
	t.Run("a JSON-RPC error while listing", func(t *testing.T) {
		u, calls := rawListing(t, func(w http.ResponseWriter, _ *http.Request, id json.RawMessage) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":"boom"}}`, id)
		}, answer)
		unverified(t, u, calls, 5*time.Second)
	})
	t.Run("input required while listing", func(t *testing.T) {
		u, calls := rawListing(t, func(w http.ResponseWriter, _ *http.Request, id json.RawMessage) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"resultType":"input_required","inputRequests":{}}}`, id)
		}, answer)
		unverified(t, u, calls, 5*time.Second)
	})
	t.Run("401 and 403 while listing", func(t *testing.T) {
		for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
			u, calls := rawListing(t, func(w http.ResponseWriter, _ *http.Request, _ json.RawMessage) {
				http.Error(w, "no", status)
			}, answer)
			res := execute(t, mcp.New(), toolCall(t, u, payload), 5*time.Second)
			if res.Outcome != worker.NoEffect || res.ErrorClass != "unauthorized" || calls.Load() != 0 {
				t.Fatalf("status %d: result %+v, %d calls", status, res, calls.Load())
			}
		}
	})
}

// TestTheSameDefinitionInAnotherKeyOrderIsNotDrift: the listing is
// canonicalized before the comparison, so key order, whitespace and the
// display-only fields do not change the definition.
func TestTheSameDefinitionInAnotherKeyOrderIsNotDrift(t *testing.T) {
	listed := `{
  "title": "Create PO (display only)",
  "outputSchema": {"required": ["po"], "properties": {"po": {"type": "string"}}, "type": "object"},
  "inputSchema": {"properties": {"note": {"type": "string"}, "amount": {"type": "number"}}, "type": "object"},
  "description": "Create a purchase order",
  "name": "create_po"
}`
	s := mcptest.New(t, mcptest.Modern, token, listed)
	s.OnCall(func(string, json.RawMessage) mcptest.Reply { return mcptest.Reply{Result: success} })
	res := execute(t, mcp.New(), toolCall(t, s.URL(), payload), 5*time.Second)
	if res.Outcome != worker.Succeeded || len(s.Calls()) != 1 {
		t.Fatalf("result %+v, %d calls", res, len(s.Calls()))
	}
}

// TestAToolOnALaterPageIsFound: the check lists every page.
func TestAToolOnALaterPageIsFound(t *testing.T) {
	s := mcptest.New(t, mcptest.Modern, token, otherTool, strings.Replace(otherTool, "list_po", "get_po", 1), createTool)
	s.SetPageSize(1)
	s.OnCall(func(string, json.RawMessage) mcptest.Reply { return mcptest.Reply{Result: success} })
	res := execute(t, mcp.New(), toolCall(t, s.URL(), payload), 5*time.Second)
	if res.Outcome != worker.Succeeded || len(s.Calls()) != 1 || s.Calls()[0].Name != "create_po" {
		t.Fatalf("result %+v, calls %+v", res, s.Calls())
	}
}

// TestTheDefinitionCheckIsTheListingThenOneCall: the wire carries the probe,
// every page of the listing and exactly one tools/call, in that order.
func TestTheDefinitionCheckIsTheListingThenOneCall(t *testing.T) {
	for name, c := range map[string]struct {
		tools []string
		page  int
		want  string
	}{
		"one page":  {[]string{createTool}, 0, "server/discover tools/list tools/call"},
		"two pages": {[]string{otherTool, createTool}, 1, "server/discover tools/list tools/list tools/call"},
	} {
		t.Run(name, func(t *testing.T) {
			s := mcptest.New(t, mcptest.Modern, token, c.tools...)
			s.SetPageSize(c.page)
			s.OnCall(func(string, json.RawMessage) mcptest.Reply { return mcptest.Reply{Result: success} })
			if res := execute(t, mcp.New(), toolCall(t, s.URL(), payload), 5*time.Second); res.Outcome != worker.Succeeded {
				t.Fatalf("result %+v", res)
			}
			var flow []string
			for _, r := range s.Requests() {
				flow = append(flow, r.RPCMethod)
			}
			if strings.Join(flow, " ") != c.want {
				t.Fatalf("flow %q, want %q", strings.Join(flow, " "), c.want)
			}
		})
	}
}

// TestAnEmptyCertifiedDefinitionIsRefused: without a usable certified
// definition there is nothing to compare, so nothing is sent.
func TestAnEmptyCertifiedDefinitionIsRefused(t *testing.T) {
	for name, def := range map[string]string{"empty": "", "not JSON": "{", "not an object": "[]"} {
		t.Run(name, func(t *testing.T) {
			s := mcptest.New(t, mcptest.Modern, token, createTool)
			call := toolCall(t, s.URL(), payload)
			call.Definition = def
			res := execute(t, mcp.New(), call, 5*time.Second)
			if res.Outcome != worker.NoEffect || res.ErrorClass != "definition_unverified" || len(s.Requests()) != 0 {
				t.Fatalf("result %+v, %d requests", res, len(s.Requests()))
			}
		})
	}
}

// TestAToolWithHeaderMirroringIsRefused: the value encoding of Mcp-Param
// headers is not specified upstream, so a certified inputSchema with
// x-mcp-header is refused before any request is sent.
func TestAToolWithHeaderMirroringIsRefused(t *testing.T) {
	for name, tool := range map[string]string{
		"on a string property": `{"name":"create_po","inputSchema":{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"Region"}}}}`,
		"nested":               `{"name":"create_po","inputSchema":{"type":"object","properties":{"a":{"type":"object","properties":{"b":{"type":"integer","x-mcp-header":"B"}}}}}}`,
		"in a combinator":      `{"name":"create_po","inputSchema":{"type":"object","anyOf":[{"properties":{"a":{"type":"string","x-mcp-header":"A"}}}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := mcptest.New(t, mcptest.Modern, token, tool)
			res := execute(t, mcp.New(), toolCall(t, s.URL(), payload, tool), 5*time.Second)
			if res.Outcome != worker.NoEffect || res.ErrorClass != "unsupported_header_mirroring" {
				t.Fatalf("result %+v", res)
			}
			if len(s.Requests()) != 0 {
				t.Fatalf("%d requests reached the server", len(s.Requests()))
			}
		})
	}
}

// TestAHeaderMirroringKeyInAnotherCaseIsStillRefused: encoding/json matches
// object keys case-insensitively and lets the last one win, so a certified
// definition may carry a second "inputschema" that would hide the real
// inputSchema's x-mcp-header from a struct decoder. Whatever the key, no
// x-mcp-header anywhere in the definition is called.
func TestAHeaderMirroringKeyInAnotherCaseIsStillRefused(t *testing.T) {
	for name, tool := range map[string]string{
		"a later case variant hides it": `{"name":"create_po","inputSchema":{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"Region"}}},"inputschema":{"type":"object"}}`,
		"an earlier case variant":       `{"name":"create_po","INPUTSCHEMA":{"type":"object"},"inputSchema":{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"Region"}}}}`,
		"only in a variant":             `{"name":"create_po","inputSchema":{"type":"object"},"inputschema":{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"Region"}}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := mcptest.New(t, mcptest.Modern, token, tool)
			res := execute(t, mcp.New(), toolCall(t, s.URL(), payload, tool), 5*time.Second)
			if res.Outcome != worker.NoEffect || res.ErrorClass != "unsupported_header_mirroring" {
				t.Fatalf("result %+v", res)
			}
			if len(s.Requests()) != 0 {
				t.Fatalf("%d requests reached the server", len(s.Requests()))
			}
		})
	}
}

// TestTheReplyHasItsOwnBudget: the listing may use most of the response
// budget without starving the reply of a call that then runs.
func TestTheReplyHasItsOwnBudget(t *testing.T) {
	pad := strings.Repeat("x", 60000)
	tools := []string{createTool}
	for i := 0; i < 60; i++ { // about 3.6 MiB of a 4 MiB budget
		tools = append(tools, fmt.Sprintf(`{"name":"pad_%d","description":%q,"inputSchema":{"type":"object"}}`, i, pad))
	}
	s := mcptest.New(t, mcptest.Modern, token, tools...)
	big := map[string]any{"resultType": "complete", "content": []any{map[string]any{"type": "text", "text": strings.Repeat("y", 1<<20)}},
		"structuredContent": map[string]any{"po": "PO-9"}}
	s.OnCall(func(string, json.RawMessage) mcptest.Reply { return mcptest.Reply{Result: big} })
	res := execute(t, mcp.New(), toolCall(t, s.URL(), payload), 20*time.Second)
	if res.Outcome != worker.Succeeded || len(s.Calls()) != 1 {
		t.Fatalf("result %+v, %d calls", res, len(s.Calls()))
	}
}

// TestExecuteSendsNoMcpNameOnTheLegacyRevision: Mcp-Name belongs to the
// modern revision only.
func TestExecuteSendsNoMcpNameOnTheLegacyRevision(t *testing.T) {
	s := mcptest.New(t, mcptest.Legacy, token, createTool)
	s.OnCall(func(string, json.RawMessage) mcptest.Reply { return mcptest.Reply{Result: success} })
	if res := execute(t, mcp.New(), toolCall(t, s.URL(), payload), 5*time.Second); res.Outcome != worker.Succeeded {
		t.Fatalf("result %+v", res)
	}
	for _, r := range s.Requests() {
		if r.Header.Get("Mcp-Name") != "" {
			t.Fatalf("legacy %s carries Mcp-Name", r.RPCMethod)
		}
	}
}

// TestASuccessReturnsItsResultAsOutput (ADR-034): the output is the whole
// CallToolResult, so its RFC 8785 digest is the reference.
func TestASuccessReturnsItsResultAsOutput(t *testing.T) {
	result := map[string]any{"resultType": "complete", "content": []any{map[string]any{"type": "text", "text": "PO-1"}},
		"structuredContent": map[string]any{"po": "PO-1"}}
	s := mcptest.New(t, mcptest.Modern, token, createTool)
	s.OnCall(func(string, json.RawMessage) mcptest.Reply { return mcptest.Reply{Result: result} })
	res := execute(t, mcp.New(), toolCall(t, s.URL(), payload), 5*time.Second)
	if res.Outcome != worker.Succeeded || len(res.Output) == 0 {
		t.Fatalf("result %+v", res)
	}
	canon, err := governance.Canonicalize(res.Output)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canon)
	if res.ExternalReference != "mcp:sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("reference %s is not the digest of the output %s", res.ExternalReference, canon)
	}
	var got map[string]any
	if json.Unmarshal(res.Output, &got) != nil || got["structuredContent"].(map[string]any)["po"] != "PO-1" {
		t.Fatalf("output %s", res.Output)
	}
}
