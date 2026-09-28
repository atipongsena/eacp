// Package a2a delegates work to remote agents over A2A 1.0 (ADR-030). An
// A2A connector's endpoint is the agent's JSON-RPC interface URL. The
// scanner fetches the agent's card and records one tool, delegate, whose
// definition carries the card; the worker sends a delegation once, follows
// the task it starts and classifies the result. Nothing the remote agent
// returns is stored beyond ids, states and digests.
package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/atipongsena/eacp/internal/governance"
	"github.com/atipongsena/eacp/internal/worker"
)

// Protocol constants (research/REFERENCES.md, a2a-go v2.6.0).
const (
	// Version is the A2A protocol version EACP speaks, sent as A2A-Version.
	Version = "1.0"
	// Binding is the only protocol binding EACP speaks.
	Binding = "JSONRPC"
	// CardPath is the Agent Card's well-known path on the endpoint's origin.
	CardPath = "/.well-known/agent-card.json"

	maxCardBytes     = 256 << 10
	maxResponseBytes = 1 << 20
	maxSkills        = 200
	maxDefinition    = 64 << 10 // as eacp.tool_definitions.definition
	maxDisplay       = 16 << 10 // as eacp.tool_definitions.display
)

// PayloadSchema is the delegate tool's input schema (ADR-030 §3.3), in RFC
// 8785 form: text and/or data, nothing else.
const PayloadSchema = `{"additionalProperties":false,"minProperties":1,"properties":{"data":{"type":"object"},` +
	`"text":{"maxLength":65536,"minLength":1,"type":"string"}},"type":"object"}`

// remoteID is the form of a task or message id EACP stores and logs.
var remoteID = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,256}$`)

// Client discovers A2A agents and delegates to them. It never uses proxy
// environment variables or follows redirects, so an agent cannot forward
// the worker-held credential to another host.
type Client struct {
	http *http.Client
	// PollStart and PollMax bound the GetTask interval (1 s doubling to 5 s);
	// CancelBudget bounds the one CancelTask sent on interruption (2 s).
	PollStart, PollMax, CancelBudget time.Duration
	// Log records each delegation's ids, state and digests, never content.
	Log *slog.Logger
}

// New returns a Client with the ADR-030 limits.
func New() *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &Client{
		http: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		PollStart: time.Second, PollMax: 5 * time.Second, CancelBudget: 2 * time.Second,
		Log: slog.New(slog.DiscardHandler),
	}
}

func fail(class string, format string, args ...any) error {
	return &worker.DiscoveryError{Class: class, Err: fmt.Errorf(format, args...)}
}

// parseEndpoint accepts an absolute http(s) URL without user info, query
// or fragment.
func parseEndpoint(endpoint string) (*url.URL, bool) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, false
	}
	return u, true
}

// readAll reads at most limit bytes; errTooLarge reports more.
func readAll(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errTooLarge
	}
	return b, nil
}

var errTooLarge = errors.New("response too large")

// recanonical re-encodes v in RFC 8785 form (encoding/json escapes HTML
// characters and orders keys by bytes; JCS decides both).
func recanonical(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	c, err := governance.Canonicalize(b)
	return string(c), err
}

// object decodes canonical JSON text into its members.
func object(raw []byte) (map[string]json.RawMessage, bool) {
	var m map[string]json.RawMessage
	d := json.NewDecoder(bytes.NewReader(raw))
	if d.Decode(&m) != nil || m == nil {
		return nil, false
	}
	return m, true
}

// str decodes raw as a string.
func str(raw json.RawMessage) (string, bool) {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

func contextClass(ctx context.Context) string {
	if ctx.Err() != nil {
		return "timeout"
	}
	return "transport"
}
