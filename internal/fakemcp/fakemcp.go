// Package fakemcp is the Slice C demo's MCP server: a credential-protected
// Streamable HTTP endpoint speaking the stateless modern revision
// 2026-07-28 (research/REFERENCES.md, Phase 14). Like Fake ERP it sits on
// the worker-only network, and only the execution worker holds its token.
//
// Its tools come from a JSON file that is re-read on every listing, so a
// demo can change what the server advertises without restarting it: the
// "rug pull" of MASTER_PLAN §68. Without the file it lists Default.
package fakemcp

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
)

// Version is the MCP revision served.
const Version = "2026-07-28"

// Default is the tool list before any drift: one read-only lookup.
const Default = `[{"name":"get_po","title":"Get purchase order",
 "description":"Read one purchase order by id.",
 "inputSchema":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]},
 "annotations":{"readOnlyHint":true}}]`

// maxRequest bounds a JSON-RPC request body.
const maxRequest = 64 << 10

// Server serves /mcp.
type Server struct {
	token     []byte
	toolsFile string
}

// New returns a server that requires token as its bearer token and lists
// the tools in toolsFile (a JSON array), or Default when the file does not
// exist.
func New(token, toolsFile string) (*Server, error) {
	if token == "" {
		return nil, errors.New("fakemcp: a token is required")
	}
	return &Server{token: []byte("Bearer " + token), toolsFile: toolsFile}, nil
}

type rpcRequest struct {
	JSONRPC string                     `json:"jsonrpc"`
	ID      json.RawMessage            `json:"id"`
	Method  string                     `json:"method"`
	Params  map[string]json.RawMessage `json:"params"`
}

// ServeHTTP implements the modern stateless transport: every request is a
// POST carrying the protocol version in _meta and in the MCP-Protocol-Version
// and Mcp-Method headers.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/mcp" {
		http.NotFound(w, r)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), s.token) != 1 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="fakemcp"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req rpcRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxRequest)).Decode(&req); err != nil || req.JSONRPC != "2.0" {
		rpcError(w, http.StatusBadRequest, nil, -32700, "Parse error", nil)
		return
	}
	var meta map[string]json.RawMessage
	_ = json.Unmarshal(req.Params["_meta"], &meta)
	var version string
	_ = json.Unmarshal(meta["io.modelcontextprotocol/protocolVersion"], &version)
	if version != Version {
		rpcError(w, http.StatusBadRequest, req.ID, -32022, "Unsupported protocol version",
			map[string]any{"supported": []string{Version}, "requested": version})
		return
	}
	if r.Header.Get("MCP-Protocol-Version") != version || r.Header.Get("Mcp-Method") != req.Method {
		rpcError(w, http.StatusBadRequest, req.ID, -32020, "Header mismatch", nil)
		return
	}
	if _, ok := meta["io.modelcontextprotocol/clientCapabilities"]; !ok {
		rpcError(w, http.StatusBadRequest, req.ID, -32021, "Missing required client capability", nil)
		return
	}
	info := map[string]any{"io.modelcontextprotocol/serverInfo": map[string]string{"name": "fake-sap-mcp", "version": "1.0.0"}}
	switch req.Method {
	case "server/discover":
		result(w, req.ID, map[string]any{"resultType": "complete", "supportedVersions": []string{Version},
			"capabilities": map[string]any{"tools": map[string]any{}}, "_meta": info})
	case "tools/list":
		tools, err := s.tools()
		if err != nil {
			rpcError(w, http.StatusOK, req.ID, -32603, "Internal error", nil)
			return
		}
		result(w, req.ID, map[string]any{"resultType": "complete", "tools": tools, "_meta": info})
	default:
		rpcError(w, http.StatusOK, req.ID, -32601, "Method not found", nil)
	}
}

// tools reads the current tool list.
func (s *Server) tools() ([]json.RawMessage, error) {
	raw := []byte(Default)
	if s.toolsFile != "" {
		b, err := os.ReadFile(s.toolsFile)
		switch {
		case err == nil:
			raw = b
		case !errors.Is(err, fs.ErrNotExist):
			return nil, err
		}
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, err
	}
	if tools == nil {
		tools = []json.RawMessage{}
	}
	return tools, nil
}

func result(w http.ResponseWriter, id json.RawMessage, v any) {
	write(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": id, "result": v})
}

func rpcError(w http.ResponseWriter, status int, id json.RawMessage, code int, msg string, data any) {
	e := map[string]any{"code": code, "message": msg}
	if data != nil {
		e["data"] = data
	}
	body := map[string]any{"jsonrpc": "2.0", "error": e}
	if len(id) > 0 {
		body["id"] = id
	}
	write(w, status, body)
}

func write(w http.ResponseWriter, status int, body any) {
	b, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}
