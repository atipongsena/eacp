// Package microsoftagt is the Go client of the Microsoft AGT/ACS sidecar
// PDP (ADR-002 §2, §8). It implements governance.GovernanceProvider over the
// eacp-agt-pdp/1 wire protocol and holds no AGT or ACS types: the sidecar
// (sidecars/agt-pdp) wraps the pinned AGT policy layer and ACS engine.
//
// The client is pure: it keeps no approval state and never retries. Every
// failure is transient for the caller (governance.EvaluateChecked); a
// response that is present but wrong wraps governance.ErrMalformedDecision so
// the action engine raises the malformed-decision alert.
package microsoftagt

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"eacp/internal/governance"
)

const (
	// Protocol is the sidecar wire protocol this client speaks.
	Protocol = "eacp-agt-pdp/1"
	// ProviderName is recorded as the decision evidence provider.
	ProviderName = "microsoft-agt"
	// MaxResponseBytes bounds a sidecar response body. A decision echoes
	// the enforced payload, which EACP bounds to 1 MiB.
	MaxResponseBytes = 2 << 20
	// MaxClockSkew bounds how far a decision's evaluated_at may fall outside
	// the call window. The approval TTL counts from evaluated_at, so a
	// skewed PDP clock must not stretch it.
	MaxClockSkew = 5 * time.Second
)

// Versions identifies the engine stack that decided.
type Versions struct {
	AGT string `json:"agt"`
	ACS string `json:"acs"`
	OPA string `json:"opa"`
}

// Pinned is the only engine stack EACP accepts (ADR-002 §7, §8;
// research/REFERENCES.md). Bumping it requires the conformance suite.
var Pinned = Versions{AGT: "5.0.0", ACS: "0.3.1b1", OPA: "1.20.2"}

// ErrVersionMismatch: the sidecar runs another protocol or engine stack.
var ErrVersionMismatch = errors.New("microsoftagt: sidecar protocol or engine versions differ from the pins")

// Config selects the sidecar. Plain http is accepted only to a loopback
// host; https requires mutual TLS (all three files).
type Config struct {
	URL      string
	CAFile   string
	CertFile string
	KeyFile  string
}

// Provider is the AGT sidecar governance provider.
type Provider struct {
	base   *url.URL
	client *http.Client
}

var _ governance.GovernanceProvider = (*Provider)(nil)

// New validates the transport and fails closed on anything unsafe.
func New(cfg Config) (*Provider, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") {
		return nil, errors.New("microsoftagt: the PDP URL must be scheme://host:port with no path, query or credentials")
	}
	u.Path = ""
	tlsFiles := cfg.CAFile != "" || cfg.CertFile != "" || cfg.KeyFile != ""
	transport := &http.Transport{
		Proxy:               nil, // never route governance calls through an environment proxy
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	switch u.Scheme {
	case "http":
		if tlsFiles {
			return nil, errors.New("microsoftagt: TLS files are set but the PDP URL is plain http")
		}
		if !loopback(u.Hostname()) {
			return nil, fmt.Errorf("microsoftagt: plain http is allowed only to a loopback host, not %q; use https with mutual TLS", u.Hostname())
		}
	case "https":
		if cfg.CAFile == "" || cfg.CertFile == "" || cfg.KeyFile == "" {
			return nil, errors.New("microsoftagt: https requires the CA, client certificate and client key files (mutual TLS)")
		}
		cfgTLS, err := clientTLSConfig(cfg)
		if err != nil {
			return nil, err
		}
		transport.TLSClientConfig = cfgTLS
	default:
		return nil, fmt.Errorf("microsoftagt: unsupported PDP URL scheme %q", u.Scheme)
	}
	return &Provider{base: u, client: &http.Client{
		Transport: transport,
		// A redirect is a failure, never followed.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func clientTLSConfig(cfg Config) (*tls.Config, error) {
	pool, err := certPool(cfg.CAFile)
	if err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("microsoftagt: client certificate: %w", err)
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, Certificates: []tls.Certificate{cert}}, nil
}

// ServerTLSConfig is the sidecar side of mutual TLS, for tests and for any
// Go-hosted PDP: TLS 1.3 and a client certificate from caFile.
func ServerTLSConfig(caFile, certFile, keyFile string) (*tls.Config, error) {
	pool, err := certPool(caFile)
	if err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("microsoftagt: server certificate: %w", err)
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert,
		Certificates: []tls.Certificate{cert}}, nil
}

func certPool(caFile string) (*x509.CertPool, error) {
	raw, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("microsoftagt: CA file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(raw) {
		return nil, errors.New("microsoftagt: CA file holds no certificate")
	}
	return pool, nil
}

type wireRequest struct {
	Protocol        string             `json:"protocol"`
	Binding         governance.Binding `json:"binding"`
	RiskClass       string             `json:"risk_class"`
	SideEffectClass string             `json:"side_effect_class"`
	Policy          wirePolicy         `json:"policy"`
}

type wirePolicy struct {
	BundleID uuid.UUID       `json:"bundle_id"`
	Version  int             `json:"version"`
	Content  json.RawMessage `json:"content"`
}

type wireDecision struct {
	Protocol           string                          `json:"protocol"`
	Verdict            governance.Verdict              `json:"verdict"`
	Reasons            []string                        `json:"reasons"`
	EnforcedPayload    json.RawMessage                 `json:"enforced_payload"`
	InputDigest        string                          `json:"input_digest"`
	EnforcedDigest     string                          `json:"enforced_digest"`
	PolicyBundleID     uuid.UUID                       `json:"policy_bundle_id"`
	PolicyVersion      int                             `json:"policy_version"`
	Provider           string                          `json:"provider"`
	ProviderInstanceID string                          `json:"provider_instance_id"`
	DecisionID         uuid.UUID                       `json:"decision_id"`
	EvaluatedAt        time.Time                       `json:"evaluated_at"`
	Approval           *governance.ApprovalRequirement `json:"approval"`
	Versions           Versions                        `json:"versions"`
	Evidence           json.RawMessage                 `json:"evidence"`
}

type wireHealth struct {
	Status             string   `json:"status"`
	Protocol           string   `json:"protocol"`
	Versions           Versions `json:"versions"`
	ProviderInstanceID string   `json:"provider_instance_id"`
}

func malformed(format string, args ...any) error {
	return fmt.Errorf("%w: microsoftagt: %s", governance.ErrMalformedDecision, fmt.Sprintf(format, args...))
}

// Evaluate asks the sidecar for a decision. It never consults the PDP for
// anything but a decision and keeps no state between calls.
func (p *Provider) Evaluate(ctx context.Context, req governance.GovernanceRequest) (governance.GovernanceDecision, error) {
	body, err := json.Marshal(wireRequest{Protocol: Protocol, Binding: req.Binding, RiskClass: req.RiskClass,
		SideEffectClass: req.SideEffectClass,
		Policy:          wirePolicy{BundleID: req.PolicyBundleID, Version: req.PolicyVersion, Content: req.Policy}})
	if err != nil {
		return governance.GovernanceDecision{}, fmt.Errorf("microsoftagt: request: %w", err)
	}
	start := time.Now()
	var w wireDecision
	if err := p.call(ctx, http.MethodPost, "/v1/evaluate", body, &w); err != nil {
		return governance.GovernanceDecision{}, err
	}
	end := time.Now()
	if w.Protocol != Protocol || w.Versions != Pinned {
		return governance.GovernanceDecision{}, fmt.Errorf("%w: %w: protocol %q, versions %+v", governance.ErrMalformedDecision,
			ErrVersionMismatch, w.Protocol, w.Versions)
	}
	if w.Provider != ProviderName {
		return governance.GovernanceDecision{}, malformed("provider %q", w.Provider)
	}
	if w.EvaluatedAt.Before(start.Add(-MaxClockSkew)) || w.EvaluatedAt.After(end.Add(MaxClockSkew)) {
		return governance.GovernanceDecision{}, malformed("evaluated_at %s is outside the call window", w.EvaluatedAt)
	}
	d := governance.GovernanceDecision{
		Verdict: w.Verdict, Reasons: w.Reasons, EnforcedPayload: w.EnforcedPayload,
		PolicyBundleID: w.PolicyBundleID, PolicyVersion: w.PolicyVersion,
		Provider: w.Provider, ProviderInstanceID: w.ProviderInstanceID, DecisionID: w.DecisionID,
		EvaluatedAt: w.EvaluatedAt.UTC(), Approval: w.Approval, ProviderEvidence: w.Evidence,
	}
	if d.InputDigest, err = digest(w.InputDigest); err != nil {
		return governance.GovernanceDecision{}, err
	}
	if d.EnforcedDigest, err = digest(w.EnforcedDigest); err != nil {
		return governance.GovernanceDecision{}, err
	}
	return d, nil
}

// Health checks that the sidecar answers with the pinned protocol and
// engine versions. An unreachable sidecar is an ordinary error; a reachable
// one with other versions is ErrVersionMismatch.
func (p *Provider) Health(ctx context.Context) error {
	var h wireHealth
	if err := p.call(ctx, http.MethodGet, "/healthz", nil, &h); err != nil {
		return err
	}
	if h.Protocol != Protocol || h.Versions != Pinned {
		return fmt.Errorf("%w: protocol %q, versions %+v, want %s %+v", ErrVersionMismatch, h.Protocol, h.Versions, Protocol, Pinned)
	}
	if h.Status != "ok" {
		return fmt.Errorf("microsoftagt: sidecar status %q", h.Status)
	}
	return nil
}

func digest(s string) ([32]byte, error) {
	var out [32]byte
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != len(out) {
		return out, malformed("digest %q is not 32 hex-encoded bytes", s)
	}
	copy(out[:], raw)
	return out, nil
}

// call performs one request. Transport failures and non-200 statuses are
// plain errors (unavailable); a 200 that cannot be decoded strictly is
// malformed.
func (p *Provider) call(ctx context.Context, method, path string, body []byte, out any) error {
	u := *p.base
	u.Path = path
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return fmt.Errorf("microsoftagt: %w", err)
	}
	httpReq.Header.Set("Accept", "application/json")
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("microsoftagt: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("microsoftagt: reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return fmt.Errorf("microsoftagt: %s %s: status %d %s", method, path, resp.StatusCode, sanitize(e.Error))
	}
	if len(raw) > MaxResponseBytes {
		return malformed("response exceeds %d bytes", MaxResponseBytes)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return malformed("content type %q", ct)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return malformed("response: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return malformed("trailing data after the response")
	}
	return nil
}

// sanitize keeps a sidecar error code short and printable for logs.
func sanitize(s string) string {
	if len(s) > 64 {
		s = s[:64]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return '?'
		}
		return r
	}, s)
}
