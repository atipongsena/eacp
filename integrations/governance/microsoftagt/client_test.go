package microsoftagt_test

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"eacp/integrations/governance/microsoftagt"
	"eacp/internal/governance"
	"eacp/internal/governance/conformance"
)

const referencePath = "../../../test/conformance/governance_reference.json"

// sidecar is a fake AGT PDP speaking the eacp-agt-pdp/1 wire protocol. It
// decides with the local provider, so the reference set's expectations
// hold, and lets a test corrupt any part of the response.
type sidecar struct {
	t       *testing.T
	mutate  func(map[string]any)
	status  int
	body    string
	delay   time.Duration
	lastReq atomic.Value // map[string]any
	calls   atomic.Int64
}

func (s *sidecar) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.calls.Add(1)
	if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
		health := map[string]any{"status": "ok", "protocol": microsoftagt.Protocol,
			"versions":             map[string]any{"agt": microsoftagt.Pinned.AGT, "acs": microsoftagt.Pinned.ACS, "opa": microsoftagt.Pinned.OPA},
			"provider_instance_id": "agt-pdp-test"}
		if s.mutate != nil {
			s.mutate(health)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(health)
		return
	}
	if r.URL.Path != "/v1/evaluate" || r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
		http.Error(w, `{"error":"not_found"}`, http.StatusNotFound)
		return
	}
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-r.Context().Done():
			return
		}
	}
	if s.status != 0 {
		w.WriteHeader(s.status)
		_, _ = io.WriteString(w, s.body)
		return
	}
	if s.body != "" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, s.body)
		return
	}
	var in struct {
		Protocol        string             `json:"protocol"`
		Binding         governance.Binding `json:"binding"`
		RiskClass       string             `json:"risk_class"`
		SideEffectClass string             `json:"side_effect_class"`
		Policy          struct {
			BundleID string          `json:"bundle_id"`
			Version  int             `json:"version"`
			Content  json.RawMessage `json:"content"`
		} `json:"policy"`
	}
	raw, _ := io.ReadAll(r.Body)
	var generic map[string]any
	_ = json.Unmarshal(raw, &generic)
	s.lastReq.Store(generic)
	if err := json.Unmarshal(raw, &in); err != nil || in.Protocol != microsoftagt.Protocol {
		http.Error(w, `{"error":"bad_request"}`, http.StatusBadRequest)
		return
	}
	req := governance.GovernanceRequest{Binding: in.Binding, RiskClass: in.RiskClass, SideEffectClass: in.SideEffectClass,
		PolicyVersion: in.Policy.Version, Policy: in.Policy.Content}
	_ = req.PolicyBundleID.UnmarshalText([]byte(in.Policy.BundleID))
	d, err := governance.LocalProvider{InstanceID: "agt-pdp-test"}.Evaluate(r.Context(), req)
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"pdp_runtime_error","detail":"`+err.Error()+`"}`)
		return
	}
	out := map[string]any{
		"protocol": microsoftagt.Protocol, "verdict": d.Verdict, "reasons": d.Reasons,
		"enforced_payload": d.EnforcedPayload,
		"input_digest":     hex.EncodeToString(d.InputDigest[:]), "enforced_digest": hex.EncodeToString(d.EnforcedDigest[:]),
		"policy_bundle_id": d.PolicyBundleID, "policy_version": d.PolicyVersion,
		"provider": microsoftagt.ProviderName, "provider_instance_id": d.ProviderInstanceID,
		"decision_id": d.DecisionID, "evaluated_at": d.EvaluatedAt.Format(time.RFC3339Nano), "approval": d.Approval,
		"versions": map[string]any{"agt": microsoftagt.Pinned.AGT, "acs": microsoftagt.Pinned.ACS, "opa": microsoftagt.Pinned.OPA},
		"evidence": map[string]any{"acs_action_identity": "sha256:00", "rule_id": "r"},
	}
	if s.mutate != nil {
		s.mutate(out)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func newSidecar(t *testing.T) (*sidecar, *microsoftagt.Provider) {
	t.Helper()
	s := &sidecar{t: t}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	p, err := microsoftagt.New(microsoftagt.Config{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return s, p
}

func request(t *testing.T) governance.GovernanceRequest {
	t.Helper()
	f, err := conformance.Load(referencePath)
	if err != nil {
		t.Fatal(err)
	}
	req, err := f.Request(f.Cases[0])
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// The wire protocol carries every field of the reference set losslessly:
// verdicts, Unicode, numbers, approval requirements and digests.
func TestReferenceSetRoundTripsTheWireProtocol(t *testing.T) {
	_, p := newSidecar(t)
	f, err := conformance.Load(referencePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			req, err := f.Request(c)
			if err != nil {
				t.Fatal(err)
			}
			d, err := governance.EvaluateChecked(context.Background(), p, req)
			if err := conformance.Check(c.Expect, d, err); err != nil {
				t.Fatal(err)
			}
			if err == nil && c.Expect.Error == "" &&
				(d.Provider != microsoftagt.ProviderName || string(d.ProviderEvidence) != `{"acs_action_identity":"sha256:00","rule_id":"r"}`) {
				t.Fatalf("provider = %s, evidence = %s", d.Provider, d.ProviderEvidence)
			}
		})
	}
}

func TestEvaluateSendsTheBindingAndPinnedPolicy(t *testing.T) {
	s, p := newSidecar(t)
	req := request(t)
	if _, err := p.Evaluate(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	got, _ := s.lastReq.Load().(map[string]any)
	policy, _ := got["policy"].(map[string]any)
	binding, _ := got["binding"].(map[string]any)
	if got["protocol"] != microsoftagt.Protocol || got["risk_class"] != req.RiskClass ||
		got["side_effect_class"] != req.SideEffectClass || policy["bundle_id"] != req.PolicyBundleID.String() ||
		policy["version"] != float64(req.PolicyVersion) || binding["tenant"] != req.Binding.TenantID.String() ||
		binding["subject"] != req.Binding.Subject || binding["payload"] == nil || policy["content"] == nil {
		t.Fatalf("sidecar received %v", got)
	}
}

// Every failure class is transient: EvaluateChecked reports it, nothing is
// executable, and a bad response (as opposed to an unreachable PDP) is
// malformed so the engine raises the alert (ADR-002 §5, §6).
func TestFailuresAreTransientAndBadResponsesAreMalformed(t *testing.T) {
	cases := []struct {
		name      string
		setup     func(*sidecar)
		malformed bool
		mismatch  bool
	}{
		{"503", func(s *sidecar) { s.status, s.body = 503, `{"error":"pdp_runtime_error"}` }, false, false},
		{"422 unsupported policy", func(s *sidecar) { s.status, s.body = 422, `{"error":"policy_unsupported"}` }, false, false},
		{"500", func(s *sidecar) { s.status = 500 }, false, false},
		{"redirect", func(s *sidecar) { s.status, s.body = 307, "" }, false, false},
		{"not JSON", func(s *sidecar) { s.body = "<html>" }, true, false},
		{"trailing data", func(s *sidecar) { s.body = `{} {}` }, true, false},
		{"unknown field", func(s *sidecar) { s.mutate = func(m map[string]any) { m["surprise"] = 1 } }, true, false},
		{"unknown nested field", func(s *sidecar) {
			s.mutate = func(m map[string]any) { m["versions"].(map[string]any)["rust"] = "1" }
		}, true, false},
		{"oversized body", func(s *sidecar) {
			s.mutate = func(m map[string]any) { m["reasons"] = []string{strings.Repeat("x", 3<<20)} }
		}, true, false},
		{"wrong protocol", func(s *sidecar) { s.mutate = func(m map[string]any) { m["protocol"] = "eacp-agt-pdp/2" } }, true, true},
		{"wrong provider", func(s *sidecar) { s.mutate = func(m map[string]any) { m["provider"] = "local" } }, true, false},
		{"AGT version", func(s *sidecar) {
			s.mutate = func(m map[string]any) { m["versions"].(map[string]any)["agt"] = "5.0.1" }
		}, true, true},
		{"ACS version", func(s *sidecar) {
			s.mutate = func(m map[string]any) { m["versions"].(map[string]any)["acs"] = "0.3.1b2" }
		}, true, true},
		{"OPA version", func(s *sidecar) {
			s.mutate = func(m map[string]any) { m["versions"].(map[string]any)["opa"] = "1.20.1" }
		}, true, true},
		{"missing versions", func(s *sidecar) { s.mutate = func(m map[string]any) { delete(m, "versions") } }, true, true},
		{"bad digest", func(s *sidecar) { s.mutate = func(m map[string]any) { m["input_digest"] = "zz" } }, true, false},
		{"short digest", func(s *sidecar) { s.mutate = func(m map[string]any) { m["enforced_digest"] = "abcd" } }, true, false},
		{"clock ahead", func(s *sidecar) {
			s.mutate = func(m map[string]any) { m["evaluated_at"] = time.Now().Add(time.Minute).Format(time.RFC3339Nano) }
		}, true, false},
		{"clock behind", func(s *sidecar) {
			s.mutate = func(m map[string]any) { m["evaluated_at"] = time.Now().Add(-time.Minute).Format(time.RFC3339Nano) }
		}, true, false},
		{"evidence not an object", func(s *sidecar) { s.mutate = func(m map[string]any) { m["evidence"] = []int{1} } }, true, false},
		{"missing decision id", func(s *sidecar) { s.mutate = func(m map[string]any) { delete(m, "decision_id") } }, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, p := newSidecar(t)
			c.setup(s)
			d, err := governance.EvaluateChecked(context.Background(), p, request(t))
			if err == nil {
				t.Fatalf("decision %+v, want failure", d)
			}
			if !errors.Is(err, governance.ErrProviderUnavailable) && !errors.Is(err, governance.ErrMalformedDecision) {
				t.Fatalf("err = %v, want transient", err)
			}
			if errors.Is(err, governance.ErrMalformedDecision) != c.malformed {
				t.Fatalf("err = %v, malformed = %v", err, c.malformed)
			}
			if errors.Is(err, microsoftagt.ErrVersionMismatch) != c.mismatch {
				t.Fatalf("err = %v, version mismatch = %v", err, c.mismatch)
			}
		})
	}
}

func TestEvaluateHonoursTheCallerDeadline(t *testing.T) {
	s, p := newSidecar(t)
	s.delay = 5 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := governance.EvaluateChecked(ctx, p, request(t))
	if err == nil || errors.Is(err, governance.ErrMalformedDecision) || time.Since(start) > 2*time.Second {
		t.Fatalf("slow PDP: err = %v after %v", err, time.Since(start))
	}
}

func TestUnreachablePDPIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	p, err := microsoftagt.New(microsoftagt.Config{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := governance.EvaluateChecked(context.Background(), p, request(t)); err == nil ||
		errors.Is(err, governance.ErrMalformedDecision) {
		t.Fatalf("unreachable PDP: %v", err)
	}
	if err := p.Health(context.Background()); err == nil || errors.Is(err, microsoftagt.ErrVersionMismatch) {
		t.Fatalf("unreachable health = %v", err)
	}
}

func TestHealthChecksTheVersionPins(t *testing.T) {
	s, p := newSidecar(t)
	if err := p.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.mutate = func(m map[string]any) { m["versions"].(map[string]any)["acs"] = "0.4.0" }
	if err := p.Health(context.Background()); !errors.Is(err, microsoftagt.ErrVersionMismatch) {
		t.Fatalf("health with wrong ACS = %v", err)
	}
	s.mutate = func(m map[string]any) { m["protocol"] = "other" }
	if err := p.Health(context.Background()); !errors.Is(err, microsoftagt.ErrVersionMismatch) {
		t.Fatalf("health with wrong protocol = %v", err)
	}
}

// ADR-002 §2, §8: plain HTTP only to loopback; anything else needs mTLS.
func TestNewRejectsUnsafeTransports(t *testing.T) {
	dir := t.TempDir()
	if err := microsoftagt.WriteDevPKI(dir, []string{"agt-pdp", "127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	tlsFiles := microsoftagt.Config{CAFile: filepath.Join(dir, "ca.pem"),
		CertFile: filepath.Join(dir, "client.pem"), KeyFile: filepath.Join(dir, "client-key.pem")}
	withURL := func(c microsoftagt.Config, url string) microsoftagt.Config { c.URL = url; return c }
	for _, ok := range []microsoftagt.Config{
		{URL: "http://127.0.0.1:8181"}, {URL: "http://[::1]:8181"}, {URL: "http://localhost:8181"},
		{URL: "http://127.0.0.1:8181/"}, withURL(tlsFiles, "https://agt-pdp:8443"),
	} {
		if _, err := microsoftagt.New(ok); err != nil {
			t.Errorf("New(%s) = %v", ok.URL, err)
		}
	}
	for _, bad := range []microsoftagt.Config{
		{}, {URL: "http://agt-pdp:8181"}, {URL: "http://10.0.0.1:8181"}, {URL: "http://0.0.0.0:8181"},
		{URL: "https://agt-pdp:8443"}, withURL(microsoftagt.Config{CAFile: tlsFiles.CAFile}, "https://agt-pdp:8443"),
		withURL(tlsFiles, "http://127.0.0.1:8181"), {URL: "ftp://127.0.0.1"}, {URL: "http://127.0.0.1:8181/v1"},
		{URL: "http://127.0.0.1:8181?x=1"}, {URL: "http://user:pw@127.0.0.1:8181"}, {URL: "127.0.0.1:8181"},
		withURL(microsoftagt.Config{CAFile: tlsFiles.CAFile, CertFile: tlsFiles.CertFile, KeyFile: tlsFiles.CAFile}, "https://agt-pdp:8443"),
	} {
		if _, err := microsoftagt.New(bad); err == nil {
			t.Errorf("New(%+v) accepted", bad)
		}
	}
}

func mtlsServer(t *testing.T, dir string, h http.Handler, maxVersion uint16) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	cfg, err := microsoftagt.ServerTLSConfig(filepath.Join(dir, "ca.pem"), filepath.Join(dir, "server.pem"), filepath.Join(dir, "server-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if maxVersion != 0 {
		cfg.MinVersion, cfg.MaxVersion = tls.VersionTLS12, maxVersion
	}
	srv.TLS = cfg
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func TestMutualTLS(t *testing.T) {
	good, other := t.TempDir(), t.TempDir()
	for _, dir := range []string{good, other} {
		if err := microsoftagt.WriteDevPKI(dir, []string{"127.0.0.1"}); err != nil {
			t.Fatal(err)
		}
	}
	client := func(url, caDir, certDir string) *microsoftagt.Provider {
		p, err := microsoftagt.New(microsoftagt.Config{URL: url, CAFile: filepath.Join(caDir, "ca.pem"),
			CertFile: filepath.Join(certDir, "client.pem"), KeyFile: filepath.Join(certDir, "client-key.pem")})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	s := &sidecar{t: t}
	srv := mtlsServer(t, good, s, 0)
	if _, err := governance.EvaluateChecked(context.Background(), client(srv.URL, good, good), request(t)); err != nil {
		t.Fatalf("mTLS evaluate: %v", err)
	}
	calls := s.calls.Load()
	// A client certificate from another CA is refused by the server; a
	// server certificate from another CA is refused by the client.
	for name, p := range map[string]*microsoftagt.Provider{
		"foreign client cert": client(srv.URL, good, other),
		"foreign server CA":   client(srv.URL, other, good),
	} {
		if _, err := governance.EvaluateChecked(context.Background(), p, request(t)); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	if s.calls.Load() != calls {
		t.Fatal("a rejected TLS peer reached the handler")
	}
	old := mtlsServer(t, good, s, tls.VersionTLS12)
	if _, err := governance.EvaluateChecked(context.Background(), client(old.URL, good, good), request(t)); err == nil {
		t.Fatal("TLS 1.2 accepted")
	}
}
