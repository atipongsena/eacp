// Package mcptest is a small, strict MCP Streamable HTTP server for tests.
// It serves the modern revision 2026-07-28, a legacy initialize-based
// revision, or both, and validates the request headers the specification
// requires, so a client that deviates fails.
package mcptest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"
)

// Era selects which protocol generations the server speaks.
type Era int

const (
	Modern Era = iota // 2026-07-28 only
	Legacy            // initialize handshake only
	Dual              // both
)

const modernVersion = "2026-07-28"

// Server is a fake MCP server. Its fields may be changed between scans
// through the setters, which are safe for concurrent use.
type Server struct {
	*httptest.Server
	Token string

	mu            sync.Mutex
	era           Era
	legacyVersion string
	sse           bool
	pageSize      int
	tools         []json.RawMessage
	sessions      map[string]bool
	nextSession   int
	requests      []Request
	handler       http.HandlerFunc
}

// Request is what the server saw of one HTTP request.
type Request struct {
	HTTPMethod string
	RPCMethod  string
	Header     http.Header
	Params     map[string]json.RawMessage
}

// New starts a server of era that requires token as its bearer token and
// lists tools (raw JSON objects). A legacy server negotiates 2025-11-25.
func New(t testing.TB, era Era, token string, tools ...string) *Server {
	s := &Server{Token: token, era: era, legacyVersion: "2025-11-25", sessions: map[string]bool{}}
	s.SetTools(tools...)
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// SetTools replaces the listed tools.
func (s *Server) SetTools(tools ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools = s.tools[:0]
	for _, t := range tools {
		s.tools = append(s.tools, json.RawMessage(t))
	}
}

// SetSSE makes responses Server-Sent Event streams.
func (s *Server) SetSSE(on bool) { s.mu.Lock(); s.sse = on; s.mu.Unlock() }

// SetPageSize paginates tools/list (0: one page).
func (s *Server) SetPageSize(n int) { s.mu.Lock(); s.pageSize = n; s.mu.Unlock() }

// SetLegacyVersion sets the revision a legacy handshake negotiates.
func (s *Server) SetLegacyVersion(v string) { s.mu.Lock(); s.legacyVersion = v; s.mu.Unlock() }

// SetHandler replaces the server's behaviour entirely (nil restores it).
func (s *Server) SetHandler(h http.HandlerFunc) { s.mu.Lock(); s.handler = h; s.mu.Unlock() }

// Requests returns the requests seen so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// URL is the MCP endpoint.
func (s *Server) URL() string { return s.Server.URL + "/mcp" }

type rpcRequest struct {
	JSONRPC string                     `json:"jsonrpc"`
	ID      json.RawMessage            `json:"id"`
	Method  string                     `json:"method"`
	Params  map[string]json.RawMessage `json:"params"`
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	h := s.handler
	s.mu.Unlock()
	if h != nil {
		h(w, r)
		return
	}
	var req rpcRequest
	if r.Method == http.MethodPost {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	s.mu.Lock()
	s.requests = append(s.requests, Request{HTTPMethod: r.Method, RPCMethod: req.Method, Header: r.Header.Clone(), Params: req.Params})
	era, legacy := s.era, s.legacyVersion
	s.mu.Unlock()

	if r.URL.Path != "/mcp" {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+s.Token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodDelete {
		s.mu.Lock()
		delete(s.sessions, r.Header.Get("Mcp-Session-Id"))
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodPost || req.JSONRPC != "2.0" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if accept := r.Header.Values("Accept"); !slices.Contains(accept, "application/json, text/event-stream") {
		http.Error(w, "Accept must list application/json and text/event-stream", http.StatusNotAcceptable)
		return
	}
	var meta map[string]json.RawMessage
	_ = json.Unmarshal(req.Params["_meta"], &meta)
	var metaVersion string
	_ = json.Unmarshal(meta["io.modelcontextprotocol/protocolVersion"], &metaVersion)
	modernRequest := metaVersion != "" || r.Header.Get("Mcp-Method") != ""

	switch {
	case modernRequest && era != Legacy:
		s.serveModern(w, r, req, metaVersion, meta)
	case !modernRequest && era != Modern:
		s.serveLegacy(w, r, req, legacy)
	case era == Legacy:
		// A legacy server does not know modern requests: no session, no body.
		http.Error(w, "Bad Request: missing session", http.StatusBadRequest)
	default:
		s.rpcError(w, http.StatusBadRequest, req.ID, -32022, "Unsupported protocol version",
			map[string]any{"supported": []string{modernVersion}, "requested": ""})
	}
}

func (s *Server) serveModern(w http.ResponseWriter, r *http.Request, req rpcRequest, version string, meta map[string]json.RawMessage) {
	if r.Header.Get("MCP-Protocol-Version") != version || r.Header.Get("Mcp-Method") != req.Method {
		s.rpcError(w, http.StatusBadRequest, req.ID, -32020, "Header mismatch", nil)
		return
	}
	if _, ok := meta["io.modelcontextprotocol/clientCapabilities"]; !ok {
		s.rpcError(w, http.StatusBadRequest, req.ID, -32602, "clientCapabilities are required", nil)
		return
	}
	if version != modernVersion {
		s.rpcError(w, http.StatusBadRequest, req.ID, -32022, "Unsupported protocol version",
			map[string]any{"supported": []string{modernVersion}, "requested": version})
		return
	}
	info := map[string]any{"io.modelcontextprotocol/serverInfo": map[string]string{"name": "fake-mcp", "version": "1.0.0"}}
	switch req.Method {
	case "server/discover":
		s.result(w, req.ID, map[string]any{
			"resultType": "complete", "supportedVersions": []string{modernVersion},
			"capabilities": map[string]any{"tools": map[string]any{}}, "_meta": info,
			"ttlMs": 0, "cacheScope": "private",
		})
	case "tools/list":
		s.listTools(w, req, map[string]any{"resultType": "complete", "_meta": info, "ttlMs": 0, "cacheScope": "private"})
	default:
		s.rpcError(w, http.StatusNotFound, req.ID, -32601, "Method not found", nil)
	}
}

func (s *Server) serveLegacy(w http.ResponseWriter, r *http.Request, req rpcRequest, version string) {
	if req.Method == "initialize" {
		s.mu.Lock()
		s.nextSession++
		id := "session-" + strconv.Itoa(s.nextSession)
		s.sessions[id] = true
		s.mu.Unlock()
		w.Header().Set("Mcp-Session-Id", id)
		s.result(w, req.ID, map[string]any{
			"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]string{"name": "legacy-mcp", "version": "0.9"},
		})
		return
	}
	s.mu.Lock()
	live := s.sessions[r.Header.Get("Mcp-Session-Id")]
	s.mu.Unlock()
	if !live {
		http.Error(w, "Bad Request: missing session", http.StatusBadRequest)
		return
	}
	if req.Method == "notifications/initialized" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if version >= "2025-06-18" && r.Header.Get("MCP-Protocol-Version") != version {
		http.Error(w, "Bad Request: MCP-Protocol-Version", http.StatusBadRequest)
		return
	}
	if req.Method != "tools/list" {
		s.rpcError(w, http.StatusOK, req.ID, -32601, "Method not found", nil)
		return
	}
	s.listTools(w, req, map[string]any{})
}

func (s *Server) listTools(w http.ResponseWriter, req rpcRequest, result map[string]any) {
	s.mu.Lock()
	tools, size := slices.Clone(s.tools), s.pageSize
	s.mu.Unlock()
	start := 0
	var cursor string
	_ = json.Unmarshal(req.Params["cursor"], &cursor)
	if cursor != "" {
		n, err := strconv.Atoi(cursor)
		if err != nil || n < 0 || n > len(tools) {
			s.rpcError(w, http.StatusOK, req.ID, -32602, "Invalid cursor", nil)
			return
		}
		start = n
	}
	end := len(tools)
	if size > 0 && start+size < end {
		end = start + size
		result["nextCursor"] = strconv.Itoa(end)
	}
	page := tools[start:end]
	if page == nil {
		page = []json.RawMessage{}
	}
	result["tools"] = page
	s.result(w, req.ID, result)
}

func (s *Server) result(w http.ResponseWriter, id json.RawMessage, result any) {
	s.write(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (s *Server) rpcError(w http.ResponseWriter, status int, id json.RawMessage, code int, msg string, data any) {
	e := map[string]any{"code": code, "message": msg}
	if data != nil {
		e["data"] = data
	}
	body := map[string]any{"jsonrpc": "2.0", "error": e}
	if len(id) > 0 {
		body["id"] = id
	}
	s.write(w, status, body)
}

func (s *Server) write(w http.ResponseWriter, status int, body any) {
	b, _ := json.Marshal(body)
	s.mu.Lock()
	sse := s.sse
	s.mu.Unlock()
	if sse && status == http.StatusOK {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(status)
		// A comment, an unrelated notification, then the response.
		fmt.Fprint(w, ": keep-alive\r\n\r\n")
		fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}
