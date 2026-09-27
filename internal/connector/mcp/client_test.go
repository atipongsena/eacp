package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"eacp/internal/connector/mcp"
	"eacp/internal/connector/mcp/mcptest"
	"eacp/internal/jwttest"
	"eacp/internal/worker"
)

const token = "mcp-test-canary"

func secret(t *testing.T, endpoint, value string) worker.Secret {
	t.Helper()
	tenant := uuid.New()
	host := strings.TrimPrefix(strings.TrimPrefix(endpoint, "http://"), "https://")
	host, _, _ = strings.Cut(host, "/")
	p := filepath.Join(t.TempDir(), "secrets.json")
	body := fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"mcp","host":%q,"value":%q}]}`, tenant, host, value)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := worker.LoadSecrets(p)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Resolve(tenant, "mcp", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func discover(t *testing.T, s *mcptest.Server) (worker.Discovery, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return mcp.New().Discover(ctx, s.URL(), secret(t, s.URL(), token))
}

func wantClass(t *testing.T, err error, class string) {
	t.Helper()
	if err == nil {
		t.Fatalf("discovery succeeded, want %s", class)
	}
	if got := worker.DiscoveryClass(err); got != class {
		t.Fatalf("discovery error %v (class %s), want %s", err, got, class)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatal("a discovery error leaks the token")
	}
}

const (
	readPO = `{"name":"get_po","title":"Read PO","description":"Read a purchase order","icons":[{"src":"https://x.test/i.png"}],
		"inputSchema":{"type":"object","properties":{"id":{"type":"string"}}},
		"annotations":{"title":"PO reader","readOnlyHint":true},"_meta":{"com.example/tier":"gold"}}`
	createPO = `{"name":"Create.PO","description":"Create a purchase order","inputSchema":{"properties":{"amount":{"type":"number"}},"type":"object"}}`
)

func TestModernDiscoveryCanonicalizesDefinitions(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sse=%v", sse), func(t *testing.T) {
			s := mcptest.New(t, mcptest.Modern, token, readPO, createPO)
			s.SetSSE(sse)
			d, err := discover(t, s)
			if err != nil {
				t.Fatal(err)
			}
			if d.ProtocolVersion != mcp.Modern || string(d.ServerInfo) != `{"name":"fake-mcp","version":"1.0.0"}` {
				t.Fatalf("version %q, server %s", d.ProtocolVersion, d.ServerInfo)
			}
			if len(d.Tools) != 2 || len(d.Rejected) != 0 {
				t.Fatalf("tools %+v, rejected %+v", d.Tools, d.Rejected)
			}
			// Sorted by name; display-only fields split off; RFC 8785 text.
			c, r := d.Tools[0], d.Tools[1]
			if c.RemoteName != "Create.PO" || c.Display != `{}` ||
				c.Definition != `{"description":"Create a purchase order","inputSchema":{"properties":{"amount":{"type":"number"}},"type":"object"},"name":"Create.PO"}` {
				t.Fatalf("Create.PO = %+v", c)
			}
			if r.Definition != `{"_meta":{"com.example/tier":"gold"},"annotations":{"readOnlyHint":true},"description":"Read a purchase order","inputSchema":{"properties":{"id":{"type":"string"}},"type":"object"},"name":"get_po"}` ||
				r.Display != `{"annotations.title":"PO reader","icons":[{"src":"https://x.test/i.png"}],"title":"Read PO"}` {
				t.Fatalf("get_po definition %s\ndisplay %s", r.Definition, r.Display)
			}
			// Every request is modern: headers mirror the body, the token is sent.
			for _, req := range s.Requests() {
				if req.Header.Get("Authorization") != "Bearer "+token || req.Header.Get("MCP-Protocol-Version") != mcp.Modern ||
					req.Header.Get("Mcp-Method") != req.RPCMethod || req.Header.Get("Mcp-Session-Id") != "" {
					t.Fatalf("request %s headers %v", req.RPCMethod, req.Header)
				}
				var meta map[string]json.RawMessage
				if json.Unmarshal(req.Params["_meta"], &meta) != nil || string(meta["io.modelcontextprotocol/protocolVersion"]) != `"2026-07-28"` {
					t.Fatalf("request %s without modern _meta", req.RPCMethod)
				}
			}
		})
	}
}

func TestDiscoveryFollowsPagination(t *testing.T) {
	var tools []string
	for i := range 7 {
		tools = append(tools, fmt.Sprintf(`{"name":"t%d","inputSchema":{"type":"object"}}`, i))
	}
	s := mcptest.New(t, mcptest.Modern, token, tools...)
	s.SetPageSize(3)
	d, err := discover(t, s)
	if err != nil || len(d.Tools) != 7 {
		t.Fatalf("tools %d, err %v", len(d.Tools), err)
	}
	var cursors []string
	for _, r := range s.Requests() {
		if r.RPCMethod == "tools/list" {
			cursors = append(cursors, string(r.Params["cursor"]))
		}
	}
	if strings.Join(cursors, ",") != `,"3","6"` {
		t.Fatalf("cursors %q", cursors)
	}
}

func TestLegacyServersFallBackToInitialize(t *testing.T) {
	for _, c := range []struct {
		era     mcptest.Era
		version string
	}{{mcptest.Legacy, "2025-11-25"}, {mcptest.Legacy, "2025-06-18"}, {mcptest.Legacy, "2025-03-26"}} {
		t.Run(c.version, func(t *testing.T) {
			s := mcptest.New(t, c.era, token, readPO)
			s.SetLegacyVersion(c.version)
			s.SetSSE(true)
			d, err := discover(t, s)
			if err != nil {
				t.Fatal(err)
			}
			if d.ProtocolVersion != c.version || len(d.Tools) != 1 || string(d.ServerInfo) != `{"name":"legacy-mcp","version":"0.9"}` {
				t.Fatalf("discovery %+v", d)
			}
			var flow []string
			for _, r := range s.Requests() {
				flow = append(flow, r.HTTPMethod+" "+r.RPCMethod+" "+r.Header.Get("Mcp-Session-Id")+" "+r.Header.Get("MCP-Protocol-Version"))
			}
			want := []string{
				"POST server/discover  2026-07-28",
				"POST initialize  ",
				"POST notifications/initialized session-1 " + c.version,
				"POST tools/list session-1 " + c.version,
				"DELETE  session-1 " + c.version,
			}
			if strings.Join(flow, "|") != strings.Join(want, "|") {
				t.Fatalf("flow\n%s\nwant\n%s", strings.Join(flow, "\n"), strings.Join(want, "\n"))
			}
		})
	}
	t.Run("dual-era server stays modern", func(t *testing.T) {
		s := mcptest.New(t, mcptest.Dual, token, readPO)
		d, err := discover(t, s)
		if err != nil || d.ProtocolVersion != mcp.Modern {
			t.Fatalf("version %q, err %v", d.ProtocolVersion, err)
		}
	})
	t.Run("unaccepted legacy revision", func(t *testing.T) {
		s := mcptest.New(t, mcptest.Legacy, token, readPO)
		s.SetLegacyVersion("2024-11-05")
		_, err := discover(t, s)
		wantClass(t, err, "unsupported_version")
	})
}

func TestModernErrorsDoNotFallBack(t *testing.T) {
	s := mcptest.New(t, mcptest.Modern, token, readPO)
	s.SetHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32022,"message":"Unsupported protocol version","data":{"supported":["2027-01-01"],"requested":"2026-07-28"}}}`)
	})
	_, err := discover(t, s)
	wantClass(t, err, "unsupported_version")
	for _, r := range s.Requests() {
		if r.RPCMethod == "initialize" {
			t.Fatal("a modern error fell back to initialize")
		}
	}

	s.SetHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32020,"message":"Header mismatch"}}`)
	})
	_, err = discover(t, s)
	wantClass(t, err, "protocol_error")
}

func TestDiscoveryFailures(t *testing.T) {
	t.Run("wrong token", func(t *testing.T) {
		s := mcptest.New(t, mcptest.Dual, "other", readPO)
		_, err := discover(t, s)
		wantClass(t, err, "unauthorized")
		if len(s.Requests()) != 1 {
			t.Fatal("an unauthorized server was probed further")
		}
	})
	t.Run("redirect is not followed", func(t *testing.T) {
		var leaked atomic.Bool
		elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "" {
				leaked.Store(true)
			}
		}))
		defer elsewhere.Close()
		s := mcptest.New(t, mcptest.Modern, token, readPO)
		s.SetHandler(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, elsewhere.URL+"/mcp", http.StatusTemporaryRedirect)
		})
		_, err := discover(t, s)
		wantClass(t, err, "http_status")
		if leaked.Load() {
			t.Fatal("the token followed a redirect")
		}
	})
	t.Run("input required", func(t *testing.T) {
		s := mcptest.New(t, mcptest.Modern, token)
		s.SetHandler(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"resultType":"input_required","inputRequests":{}}}`)
		})
		_, err := discover(t, s)
		wantClass(t, err, "input_required")
	})
	t.Run("stream without a response times out", func(t *testing.T) {
		s := mcptest.New(t, mcptest.Modern, token)
		s.SetHandler(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		})
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		_, err := mcp.New().Discover(ctx, s.URL(), secret(t, s.URL(), token))
		if err == nil {
			t.Fatal("a silent stream completed a discovery")
		}
	})
	t.Run("response budget", func(t *testing.T) {
		s := mcptest.New(t, mcptest.Modern, token)
		s.SetHandler(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","pad":"`)
			chunk := strings.Repeat("x", 1<<16)
			for range 80 {
				fmt.Fprint(w, chunk)
			}
			fmt.Fprint(w, `"}}`)
		})
		_, err := discover(t, s)
		wantClass(t, err, "too_large")
	})
	t.Run("too many tools", func(t *testing.T) {
		var tools []string
		for i := range 501 {
			tools = append(tools, fmt.Sprintf(`{"name":"t%d","inputSchema":{"type":"object"}}`, i))
		}
		s := mcptest.New(t, mcptest.Modern, token, tools...)
		_, err := discover(t, s)
		wantClass(t, err, "too_many_tools")
	})
	t.Run("too many pages", func(t *testing.T) {
		var tools []string
		for i := range 60 {
			tools = append(tools, fmt.Sprintf(`{"name":"t%d","inputSchema":{"type":"object"}}`, i))
		}
		s := mcptest.New(t, mcptest.Modern, token, tools...)
		s.SetPageSize(1)
		_, err := discover(t, s)
		wantClass(t, err, "too_many_pages")
	})
	t.Run("endpoint with credentials", func(t *testing.T) {
		_, err := mcp.New().Discover(context.Background(), "http://user:pw@mcp.test/mcp", worker.Secret{})
		wantClass(t, err, "invalid_endpoint")
	})
	t.Run("connection refused", func(t *testing.T) {
		s := mcptest.New(t, mcptest.Modern, token)
		url := s.URL()
		sec := secret(t, url, token)
		s.Close()
		_, err := mcp.New().Discover(context.Background(), url, sec)
		wantClass(t, err, "transport_error")
	})
}

func TestUnacceptableToolsAreRejected(t *testing.T) {
	header := func(prop string) string {
		return `{"name":"hdr","inputSchema":{"type":"object","properties":` + prop + `}}`
	}
	cases := map[string]struct{ tool, reason string }{
		"bad name":                {`{"name":"get po","inputSchema":{"type":"object"}}`, "invalid_name"},
		"long name":               {`{"name":"` + strings.Repeat("a", 129) + `","inputSchema":{"type":"object"}}`, "invalid_name"},
		"no name":                 {`{"inputSchema":{"type":"object"}}`, "invalid_name"},
		"schema not object":       {`{"name":"s","inputSchema":{"type":"string"}}`, "invalid_input_schema"},
		"no schema":               {`{"name":"s"}`, "invalid_input_schema"},
		"duplicate key":           {`{"name":"d","name":"e","inputSchema":{"type":"object"}}`, "invalid_json"},
		"huge integer":            {`{"name":"n","inputSchema":{"type":"object","maximum":18446744073709551616}}`, "invalid_json"},
		"NUL":                     {`{"name":"z","description":"a\u0000b","inputSchema":{"type":"object"}}`, "unsupported_json"},
		"too large":               {`{"name":"big","description":"` + strings.Repeat("x", 70000) + `","inputSchema":{"type":"object"}}`, "too_large"},
		"header on number":        {header(`{"a":{"type":"number","x-mcp-header":"A"}}`), "invalid_x_mcp_header"},
		"header not a token":      {header(`{"a":{"type":"string","x-mcp-header":"A B"}}`), "invalid_x_mcp_header"},
		"header empty":            {header(`{"a":{"type":"string","x-mcp-header":""}}`), "invalid_x_mcp_header"},
		"header repeated":         {header(`{"a":{"type":"string","x-mcp-header":"R"},"b":{"type":"string","x-mcp-header":"r"}}`), "invalid_x_mcp_header"},
		"header under items":      {header(`{"a":{"type":"array","items":{"type":"string","x-mcp-header":"I"}}}`), "invalid_x_mcp_header"},
		"header under oneOf":      {header(`{"a":{"oneOf":[{"type":"string","x-mcp-header":"O"}]}}`), "invalid_x_mcp_header"},
		"header on the root":      {`{"name":"hdr","inputSchema":{"type":"object","x-mcp-header":"Root"}}`, "invalid_x_mcp_header"},
		"header in $defs":         {`{"name":"hdr","inputSchema":{"type":"object","$defs":{"d":{"type":"string","x-mcp-header":"D"}}}}`, "invalid_x_mcp_header"},
		"header value not string": {header(`{"a":{"type":"string","x-mcp-header":7}}`), "invalid_x_mcp_header"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := mcptest.New(t, mcptest.Modern, token, c.tool, `{"name":"fine","inputSchema":{"type":"object"}}`)
			d, err := discover(t, s)
			if err != nil {
				t.Fatal(err)
			}
			if len(d.Tools) != 1 || d.Tools[0].RemoteName != "fine" || len(d.Rejected) != 1 || d.Rejected[0].Reason != c.reason {
				t.Fatalf("tools %+v, rejected %+v; want one %s rejection", d.Tools, d.Rejected, c.reason)
			}
		})
	}
	t.Run("duplicate names reject both", func(t *testing.T) {
		s := mcptest.New(t, mcptest.Modern, token, `{"name":"x","inputSchema":{"type":"object"}}`,
			`{"name":"x","description":"shadow","inputSchema":{"type":"object"}}`)
		d, err := discover(t, s)
		if err != nil || len(d.Tools) != 0 || len(d.Rejected) != 2 || d.Rejected[0].Reason != "duplicate_name" {
			t.Fatalf("tools %+v, rejected %+v, err %v", d.Tools, d.Rejected, err)
		}
	})
	t.Run("valid headers are accepted", func(t *testing.T) {
		s := mcptest.New(t, mcptest.Modern, token, header(`{"region":{"type":"string","x-mcp-header":"Region"},
			"nested":{"type":"object","properties":{"n":{"type":"integer","x-mcp-header":"N"}}},
			"x-mcp-header":{"type":"boolean"}}`))
		d, err := discover(t, s)
		if err != nil || len(d.Tools) != 1 {
			t.Fatalf("tools %+v, rejected %+v, err %v", d.Tools, d.Rejected, err)
		}
	})
}

// TestDiscoverRefusesAnAWSCredential: temporary AWS keys sign requests; the
// MCP client never sends one as a Bearer.
func TestDiscoverRefusesAnAWSCredential(t *testing.T) {
	var contacted atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { contacted.Add(1) }))
	defer srv.Close()
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<AssumeRoleWithWebIdentityResponse><AssumeRoleWithWebIdentityResult><Credentials>`+
			`<AccessKeyId>ASIAMCPCLIENTTEST001</AccessKeyId><SecretAccessKey>mcp-secret</SecretAccessKey>`+
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
	secret, err := store.Credential(context.Background(), tenant, "mcp", srv.URL+"/mcp", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, err = mcp.New().Discover(context.Background(), srv.URL+"/mcp", secret)
	if worker.DiscoveryClass(err) != "no_credential" || contacted.Load() != 0 {
		t.Fatalf("class %q, contacted %d", worker.DiscoveryClass(err), contacted.Load())
	}
}
