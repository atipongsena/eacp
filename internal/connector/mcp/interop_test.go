package mcp_test

// Interoperability with the official MCP Go SDK (github.com/modelcontextprotocol/go-sdk
// v1.8.0, a test-only dependency): the client must discover the same tools
// from the reference server, in its modern and in its legacy-only form.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	eacpmcp "eacp/internal/connector/mcp"
	"eacp/internal/worker"
)

func sdkServer(t *testing.T, opts *sdk.ServerOptions, httpOpts *sdk.StreamableHTTPOptions) string {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{Name: "sdk-server", Version: "1.8.0"}, opts)
	noop := func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{}, nil
	}
	server.AddTool(&sdk.Tool{
		Name: "get_po", Title: "Read PO", Description: "Read a purchase order",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
		Annotations: &sdk.ToolAnnotations{Title: "PO reader", ReadOnlyHint: true},
	}, noop)
	server.AddTool(&sdk.Tool{
		Name: "create_po", Description: "Create a purchase order",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"amount":{"type":"number"}}}`),
	}, noop)
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, httpOpts)
	auth := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(auth)
	t.Cleanup(ts.Close)
	return ts.URL + "/mcp"
}

func TestInteropWithTheOfficialGoSDK(t *testing.T) {
	cases := map[string]struct {
		opts     *sdk.ServerOptions
		httpOpts *sdk.StreamableHTTPOptions
		version  string
	}{
		"modern stateless json": {nil, &sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true}, eacpmcp.Modern},
		"modern stateless sse":  {nil, &sdk.StreamableHTTPOptions{Stateless: true}, eacpmcp.Modern},
		// A stateful SDK handler advertises only the session-based (legacy)
		// revisions in its server/discover answer, so the client falls back.
		"stateful": {nil, &sdk.StreamableHTTPOptions{}, "2025-11-25"},
		"legacy only": {&sdk.ServerOptions{SupportedProtocolVersions: []string{"2025-11-25"}},
			&sdk.StreamableHTTPOptions{}, "2025-11-25"},
		"legacy 2025-06-18 json": {&sdk.ServerOptions{SupportedProtocolVersions: []string{"2025-06-18"}},
			&sdk.StreamableHTTPOptions{JSONResponse: true}, "2025-06-18"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			url := sdkServer(t, c.opts, c.httpOpts)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			d, err := eacpmcp.New().Discover(ctx, url, secret(t, url, token))
			if err != nil {
				t.Fatalf("discover: %v", err)
			}
			if d.ProtocolVersion != c.version || len(d.Rejected) != 0 || len(d.Tools) != 2 {
				t.Fatalf("version %q, tools %+v, rejected %+v", d.ProtocolVersion, d.Tools, d.Rejected)
			}
			var info struct{ Name string }
			if json.Unmarshal(d.ServerInfo, &info) != nil || info.Name != "sdk-server" {
				t.Fatalf("server info %s", d.ServerInfo)
			}
			byName := map[string]worker.DiscoveredTool{}
			for _, tool := range d.Tools {
				byName[tool.RemoteName] = tool
			}
			get := byName["get_po"]
			if get.Display != `{"annotations.title":"PO reader","title":"Read PO"}` {
				t.Fatalf("get_po display %s", get.Display)
			}
			var def struct {
				Annotations map[string]any `json:"annotations"`
				InputSchema map[string]any `json:"inputSchema"`
			}
			if json.Unmarshal([]byte(get.Definition), &def) != nil || def.Annotations["readOnlyHint"] != true ||
				def.InputSchema["type"] != "object" {
				t.Fatalf("get_po definition %s", get.Definition)
			}
		})
	}
}
