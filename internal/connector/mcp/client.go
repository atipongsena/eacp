// Package mcp discovers the tools of MCP servers over the Streamable HTTP
// transport (ADR-023 §2). It speaks the modern revision 2026-07-28
// (per-request metadata, no sessions) and falls back to the legacy
// initialize handshake (2025-11-25, 2025-06-18, 2025-03-26) as the
// specification's backward-compatibility rules describe. Discovery only
// reads: it never calls a tool.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/atipongsena/eacp/internal/worker"
)

// Protocol revisions (research/REFERENCES.md, MCP specification).
const (
	// Modern is the only stateless revision EACP speaks.
	Modern = "2026-07-28"
	// Legacy is the revision EACP offers in an initialize handshake.
	Legacy = "2025-11-25"
)

// legacyAccepted are the initialize-based revisions EACP accepts from a
// server, newest first.
var legacyAccepted = []string{"2025-11-25", "2025-06-18", "2025-03-26"}

// Modern JSON-RPC errors (MCP 2026-07-28 schema). Any of them identifies a
// modern server, so the client does not fall back to initialize.
const (
	codeHeaderMismatch                   = -32020
	codeMissingRequiredCapability        = -32021
	codeUnsupportedProtocolVersion       = -32022
	metaProtocolVersion                  = "io.modelcontextprotocol/protocolVersion"
	metaClientInfo                       = "io.modelcontextprotocol/clientInfo"
	metaClientCapabilities               = "io.modelcontextprotocol/clientCapabilities"
	defaultMaxPages                      = 50
	defaultMaxTools                      = 500
	defaultMaxBytes                int64 = 4 << 20
)

// Client discovers MCP tools. It never uses proxy environment variables or
// follows redirects, so a server cannot forward the worker-held token to
// another host.
type Client struct {
	http     *http.Client
	maxPages int
	maxTools int
	maxBytes int64
}

// New returns a Client with the ADR-023 limits.
func New() *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &Client{
		http: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		maxPages: defaultMaxPages, maxTools: defaultMaxTools, maxBytes: defaultMaxBytes,
	}
}

func fail(class string, format string, args ...any) error {
	return &worker.DiscoveryError{Class: class, Err: fmt.Errorf(format, args...)}
}

// session is one discovery against one endpoint.
type session struct {
	c        *Client
	endpoint string
	secret   string
	version  string // negotiated revision
	legacyID string // legacy Mcp-Session-Id, if the server assigned one
	nextID   int
	budget   int64 // response bytes left
}

// Discover lists every tool of the MCP server at endpoint, authenticating
// with secret as a bearer token.
func (c *Client) Discover(ctx context.Context, endpoint string, secret worker.Secret) (worker.Discovery, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" {
		return worker.Discovery{}, fail("invalid_endpoint", "endpoint is not an http(s) URL without credentials")
	}
	if secret.Reveal() == "" || secret.SignsRequests() { // a Bearer only; AWS keys are never sent as one
		return worker.Discovery{}, fail("no_credential", "no credential for the endpoint")
	}
	s := &session{c: c, endpoint: u.String(), secret: secret.Reveal(), budget: c.maxBytes}

	info, modern, err := s.probe(ctx)
	if err != nil {
		return worker.Discovery{}, err
	}
	if !modern {
		if info, err = s.initialize(ctx); err != nil {
			return worker.Discovery{}, err
		}
		defer s.close()
	}
	return s.listTools(ctx, info)
}

// probe sends server/discover as a modern request. It reports a modern
// server, or none (legacy) when the answer is not a recognized modern
// response (MCP 2026-07-28, "Backward Compatibility").
func (s *session) probe(ctx context.Context) (json.RawMessage, bool, error) {
	s.version = Modern
	res, err := s.call(ctx, "server/discover", map[string]any{})
	var rpc *rpcError
	switch {
	case errors.As(err, &rpc) && rpc.modern():
		if rpc.Code != codeUnsupportedProtocolVersion {
			return nil, false, fail("protocol_error", "server/discover: %s", rpc.Message)
		}
		// A modern server that does not speak 2026-07-28: use a legacy
		// revision it lists, if any.
		for _, v := range rpc.supported() {
			if slices.Contains(legacyAccepted, v) {
				return nil, false, nil
			}
		}
		return nil, false, fail("unsupported_version", "server supports %v", rpc.supported())
	case errors.As(err, &rpc) || errors.Is(err, errNotModern):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	var d struct {
		SupportedVersions []string `json:"supportedVersions"`
		Meta              struct {
			ServerInfo json.RawMessage `json:"io.modelcontextprotocol/serverInfo"`
		} `json:"_meta"`
	}
	if json.Unmarshal(res, &d) != nil {
		return nil, false, fail("invalid_response", "server/discover result is malformed")
	}
	if !slices.Contains(d.SupportedVersions, Modern) {
		for _, v := range d.SupportedVersions {
			if slices.Contains(legacyAccepted, v) {
				return nil, false, nil
			}
		}
		return nil, false, fail("unsupported_version", "server supports %v", d.SupportedVersions)
	}
	return d.Meta.ServerInfo, true, nil
}

// initialize opens a legacy session (MCP 2025-11-25 lifecycle).
func (s *session) initialize(ctx context.Context) (json.RawMessage, error) {
	s.version = ""
	res, err := s.call(ctx, "initialize", map[string]any{
		"protocolVersion": Legacy,
		"capabilities":    map[string]any{},
		"clientInfo":      clientInfo,
	})
	if err != nil {
		var rpc *rpcError
		if errors.As(err, &rpc) || errors.Is(err, errNotModern) {
			return nil, fail("protocol_error", "initialize failed: %v", err)
		}
		return nil, err
	}
	var r struct {
		ProtocolVersion string          `json:"protocolVersion"`
		ServerInfo      json.RawMessage `json:"serverInfo"`
	}
	if json.Unmarshal(res, &r) != nil {
		return nil, fail("invalid_response", "initialize result is malformed")
	}
	if !slices.Contains(legacyAccepted, r.ProtocolVersion) {
		return nil, fail("unsupported_version", "server chose protocol %q", r.ProtocolVersion)
	}
	s.version = r.ProtocolVersion
	if err := s.notify(ctx, "notifications/initialized"); err != nil {
		return nil, err
	}
	return r.ServerInfo, nil
}

// close ends a legacy session (best effort, MCP 2025-11-25 transports).
func (s *session) close() {
	if s.legacyID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, s.endpoint, nil)
	if err != nil {
		return
	}
	s.headers(req, "")
	if resp, err := s.c.http.Do(req); err == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
	}
}

func (s *session) listTools(ctx context.Context, serverInfo json.RawMessage) (worker.Discovery, error) {
	d := worker.Discovery{ProtocolVersion: s.version, ServerInfo: identity(serverInfo)}
	var raws []json.RawMessage
	cursor := ""
	for page := 0; ; page++ {
		if page == s.c.maxPages {
			return worker.Discovery{}, fail("too_many_pages", "more than %d pages", s.c.maxPages)
		}
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		res, err := s.call(ctx, "tools/list", params)
		if err != nil {
			var rpc *rpcError
			if errors.As(err, &rpc) || errors.Is(err, errNotModern) {
				return worker.Discovery{}, fail("protocol_error", "tools/list failed: %v", err)
			}
			return worker.Discovery{}, err
		}
		var r struct {
			Tools      []json.RawMessage `json:"tools"`
			NextCursor *string           `json:"nextCursor"`
			Meta       struct {
				ServerInfo json.RawMessage `json:"io.modelcontextprotocol/serverInfo"`
			} `json:"_meta"`
		}
		if json.Unmarshal(res, &r) != nil || r.Tools == nil {
			return worker.Discovery{}, fail("invalid_response", "tools/list result has no tools array")
		}
		if d.ServerInfo == nil {
			d.ServerInfo = identity(r.Meta.ServerInfo)
		}
		raws = append(raws, r.Tools...)
		if len(raws) > s.c.maxTools {
			return worker.Discovery{}, fail("too_many_tools", "more than %d tools", s.c.maxTools)
		}
		if r.NextCursor == nil || *r.NextCursor == "" {
			break
		}
		cursor = *r.NextCursor
	}
	d.Tools, d.Rejected = canonicalTools(raws)
	return d, nil
}

// identity keeps the server's self-reported name and version, bounded.
func identity(raw json.RawMessage) json.RawMessage {
	var v struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil || v.Name == "" {
		return nil
	}
	b, _ := json.Marshal(map[string]string{"name": clip(v.Name, 128), "version": clip(v.Version, 64)})
	return b
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

var clientInfo = map[string]string{"name": "eacp-scanner", "version": "1"}

// errNotModern is an HTTP 400/404/405 answer without a JSON-RPC error: a
// legacy server that does not know the modern request.
var errNotModern = errors.New("the server did not answer as a modern MCP server")

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("JSON-RPC error %d: %s", e.Code, clip(e.Message, 200))
}

func (e *rpcError) modern() bool {
	return e.Code == codeHeaderMismatch || e.Code == codeMissingRequiredCapability || e.Code == codeUnsupportedProtocolVersion
}

func (e *rpcError) supported() []string {
	var d struct {
		Supported []string `json:"supported"`
	}
	_ = json.Unmarshal(e.Data, &d)
	return d.Supported
}

func (s *session) headers(req *http.Request, method string) {
	req.Header.Set("Authorization", "Bearer "+s.secret)
	req.Header.Set("Accept", "application/json, text/event-stream")
	if s.version != "" {
		req.Header.Set("MCP-Protocol-Version", s.version)
	}
	if s.legacyID != "" {
		req.Header.Set("Mcp-Session-Id", s.legacyID)
	}
	if method != "" && s.version == Modern {
		req.Header.Set("Mcp-Method", method)
	}
}

// call sends one JSON-RPC request and returns its result.
func (s *session) call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	if s.version == Modern {
		params["_meta"] = map[string]any{
			metaProtocolVersion:    Modern,
			metaClientInfo:         clientInfo,
			metaClientCapabilities: map[string]any{},
		}
	}
	s.nextID++
	id := s.nextID
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, fail("invalid_request", "encode %s", method)
	}
	resp, err := s.post(ctx, body, method)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if method == "initialize" {
		if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
			if !visibleASCII(sid) || len(sid) > 256 {
				return nil, fail("protocol_error", "invalid Mcp-Session-Id")
			}
			s.legacyID = sid
		}
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fail("unauthorized", "HTTP %d", resp.StatusCode)
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound ||
		resp.StatusCode == http.StatusMethodNotAllowed:
		// A modern server explains a 400/404 with a JSON-RPC error; anything
		// else is a legacy server (MCP 2026-07-28, "Backward Compatibility").
		msg, err := s.readMessage(resp, id)
		if err == nil && msg.Error != nil {
			return nil, msg.Error
		}
		return nil, errNotModern
	case resp.StatusCode != http.StatusOK:
		return nil, fail("http_status", "HTTP %d", resp.StatusCode)
	}
	msg, err := s.readMessage(resp, id)
	if err != nil {
		return nil, err
	}
	if msg.Error != nil {
		return nil, msg.Error
	}
	if len(msg.Result) == 0 {
		return nil, fail("invalid_response", "%s: response has no result", method)
	}
	var kind struct {
		ResultType *string `json:"resultType"`
	}
	if json.Unmarshal(msg.Result, &kind) != nil {
		return nil, fail("invalid_response", "%s: result is not an object", method)
	}
	// An absent resultType is "complete" (earlier revisions).
	if kind.ResultType != nil && *kind.ResultType != "complete" {
		return nil, fail("input_required", "%s: result type %q", method, *kind.ResultType)
	}
	return msg.Result, nil
}

// notify sends a JSON-RPC notification (legacy handshake only).
func (s *session) notify(ctx context.Context, method string) error {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	resp, err := s.post(ctx, body, method)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fail("protocol_error", "%s: HTTP %d", method, resp.StatusCode)
	}
	return nil
}

func (s *session) post(ctx context.Context, body []byte, method string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fail("invalid_endpoint", "build request")
	}
	req.Header.Set("Content-Type", "application/json")
	s.headers(req, method)
	resp, err := s.c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fail("timeout", "%s: %v", method, ctx.Err())
		}
		return nil, fail("transport_error", "%s: request failed", method)
	}
	return resp, nil
}

type message struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// readMessage reads the JSON-RPC response to request id from a JSON body
// or an SSE stream, within the scan's response budget. Notifications and
// server requests on a stream are ignored.
func (s *session) readMessage(resp *http.Response, id int) (message, error) {
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	body := &budgetReader{r: resp.Body, left: &s.budget}
	want := strconv.Itoa(id)
	switch mt {
	case "application/json":
		b, err := io.ReadAll(body)
		if err != nil {
			return message{}, err
		}
		var m message
		if json.Unmarshal(b, &m) != nil {
			return message{}, fail("invalid_response", "body is not a JSON-RPC message")
		}
		if m.Error == nil && string(m.ID) != want {
			return message{}, fail("invalid_response", "response id does not match")
		}
		return m, nil
	case "text/event-stream":
		// Server-Sent Events: "data:" lines accumulate until a blank line
		// ends the event; comments and other fields are ignored.
		r := bufio.NewReader(body)
		var data []string
		for {
			line, err := r.ReadString('\n')
			var derr *worker.DiscoveryError
			if errors.As(err, &derr) {
				return message{}, err
			}
			end := err != nil
			line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			if v, ok := strings.CutPrefix(line, "data:"); ok {
				data = append(data, strings.TrimPrefix(v, " "))
			}
			if (line == "" || end) && len(data) > 0 {
				var m message
				if json.Unmarshal([]byte(strings.Join(data, "\n")), &m) == nil && m.Method == "" &&
					(string(m.ID) == want || (m.Error != nil && len(m.ID) == 0)) {
					return m, nil
				}
				data = data[:0]
			}
			if end {
				return message{}, fail("invalid_response", "stream ended without a response")
			}
		}
	default:
		return message{}, fail("invalid_response", "unexpected content type %q", mt)
	}
}

// budgetReader fails once the scan's response budget is spent.
type budgetReader struct {
	r    io.Reader
	left *int64
}

func (b *budgetReader) Read(p []byte) (int, error) {
	if *b.left <= 0 {
		return 0, fail("too_large", "responses exceed the scan budget")
	}
	if int64(len(p)) > *b.left {
		p = p[:*b.left]
	}
	n, err := b.r.Read(p)
	*b.left -= int64(n)
	return n, err
}

func visibleASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return s != ""
}
