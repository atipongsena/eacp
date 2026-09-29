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
	"bufio"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"sync"
	"time"
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

// Path is where the server speaks MCP; CallsPath serves the call log.
const (
	Path      = "/mcp"
	CallsPath = "/v1/calls"
)

// Call is one answered tools/call in the durable log. It names the tool and
// the time and nothing else: never an argument value or a result.
type Call struct {
	At   time.Time `json:"at"`
	Tool string    `json:"tool"`
}

// Server serves /mcp.
type Server struct {
	token     []byte
	toolsFile string

	mu       sync.Mutex
	callsLog string
	calls    []Call
	failed   bool
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
	if r.URL.Path != Path && r.URL.Path != CallsPath {
		http.NotFound(w, r)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), s.token) != 1 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="fakemcp"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.URL.Path == CallsPath {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		write(w, http.StatusOK, s.Calls())
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
	if req.Method == "tools/call" {
		// From 2026-07-28 a tools/call mirrors params.name in Mcp-Name.
		var name string
		_ = json.Unmarshal(req.Params["name"], &name)
		if got := r.Header.Get("Mcp-Name"); got == "" || got != name {
			rpcError(w, http.StatusBadRequest, req.ID, -32020, "Header mismatch", nil)
			return
		}
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
	case "tools/call":
		s.call(w, req, info)
	default:
		rpcError(w, http.StatusOK, req.ID, -32601, "Method not found", nil)
	}
}

// call answers get_po: the text names the purchase order, an id of "ERR"
// is a tool error (isError true). Any other tool is unknown.
func (s *Server) call(w http.ResponseWriter, req rpcRequest, info map[string]any) {
	var name string
	_ = json.Unmarshal(req.Params["name"], &name)
	var args struct {
		ID *string `json:"id"`
	}
	if name != "get_po" || json.Unmarshal(req.Params["arguments"], &args) != nil || args.ID == nil {
		rpcError(w, http.StatusOK, req.ID, -32602, "Invalid params", nil)
		return
	}
	if err := s.record(name); err != nil {
		rpcError(w, http.StatusOK, req.ID, -32603, "Internal error", nil)
		return
	}
	text, isError := "Purchase order "+*args.ID+": open, 800 THB", false
	if *args.ID == "ERR" {
		text, isError = "purchase order not found", true
	}
	result(w, req.ID, map[string]any{"resultType": "complete", "isError": isError,
		"content": []any{map[string]any{"type": "text", "text": text}}, "_meta": info})
}

// LogCallsTo makes the call log durable at path. An existing log is loaded
// (a corrupt one fails closed); every answered call is written and synced
// before the reply.
func (s *Server) LogCallsTo(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("fakemcp: open call log: %w", err)
	}
	defer f.Close()
	var calls []Call
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var c Call
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil || c.At.IsZero() || c.Tool == "" {
			return errors.New("fakemcp: corrupt call log")
		}
		calls = append(calls, c)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("fakemcp: read call log: %w", err)
	}
	s.mu.Lock()
	s.callsLog, s.calls, s.failed = path, calls, false
	s.mu.Unlock()
	return nil
}

// Calls returns the logged calls, oldest first.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call{}, s.calls...)
}

// record logs a call before it is answered. A failed write stops the server
// answering calls: it never forgets one.
func (s *Server) record(tool string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed {
		return errors.New("fakemcp: call log unavailable")
	}
	c := Call{At: time.Now().UTC(), Tool: tool}
	if s.callsLog != "" {
		b, _ := json.Marshal(c)
		f, err := os.OpenFile(s.callsLog, os.O_WRONLY|os.O_APPEND, 0o600)
		if err == nil {
			if _, err = f.Write(append(b, '\n')); err == nil {
				err = f.Sync()
			}
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
		if err != nil {
			s.failed = true
			return err
		}
	}
	s.calls = append(s.calls, c)
	return nil
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
