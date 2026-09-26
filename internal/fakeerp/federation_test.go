package fakeerp_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"eacp/internal/fakeerp"
	"eacp/internal/jwttest"
)

const (
	fedClient  = "eacp-wif"
	fedIssuer  = "https://issuer.test"
	fedSubject = "system:serviceaccount:eacp:eacp-worker"
	jwtBearer  = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
)

func federated(t *testing.T, s *jwttest.Signer) *fakeerp.Federated {
	t.Helper()
	keys, err := fakeerp.ParseJWKS(s.JWKS())
	if err != nil {
		t.Fatal(err)
	}
	return &fakeerp.Federated{ClientID: fedClient, Issuer: fedIssuer, Audience: "fakeerp", Subject: fedSubject, Keys: keys}
}

// fedERP is a Fake ERP whose only OAuth client is federated.
func fedERP(t *testing.T) (*httptest.Server, *jwttest.Signer) {
	t.Helper()
	s := jwttest.New(t)
	h, err := fakeerp.NewWithOptions(credential, filepath.Join(t.TempDir(), "erp.log"), fakeerp.Options{Federated: federated(t, s)})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, s
}

func goodClaims() map[string]any {
	now := time.Now().Unix()
	return map[string]any{"iss": fedIssuer, "sub": fedSubject, "aud": []string{"fakeerp"},
		"exp": now + 600, "iat": now, "nbf": now}
}

func assertionForm(jwt string) url.Values {
	return url.Values{"grant_type": {"client_credentials"}, "client_id": {fedClient},
		"client_assertion_type": {jwtBearer}, "client_assertion": {jwt}}
}

// postToken sends form to the token endpoint, with Basic auth when basic.
func postToken(t *testing.T, srv *httptest.Server, form url.Values, basic bool) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if basic {
		req.SetBasicAuth(url.QueryEscape(oauthClient), url.QueryEscape(oauthSecret))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestAFederatedAssertionMintsAToken(t *testing.T) {
	srv, s := fedERP(t)
	jwt := s.Sign(goodClaims())
	status, got := postToken(t, srv, assertionForm(jwt), false)
	if status != 200 || got["token_type"] != "Bearer" {
		t.Fatalf("status %d, %v", status, got)
	}
	if code := executeWith(t, srv, got["access_token"].(string)); code != 200 {
		t.Fatalf("the minted token = %d", code)
	}
	entries, raw := auditOf(t, srv)
	sum := sha256.Sum256([]byte(jwt))
	if !strings.Contains(raw, `"assertion_sha256":"`+hex.EncodeToString(sum[:])+`"`) {
		t.Fatal("the issuance audit lacks the assertion's SHA-256")
	}
	if strings.Contains(raw, jwt) {
		t.Fatal("the audit holds the assertion")
	}
	if entries[0].Principal != "oauth:"+fedClient || entries[0].Outcome != "token_issued" {
		t.Fatalf("issuance audit = %+v", entries[0])
	}
}

func TestAStringAudienceIsAccepted(t *testing.T) {
	srv, s := fedERP(t)
	c := goodClaims()
	c["aud"] = "fakeerp"
	if status, got := postToken(t, srv, assertionForm(s.Sign(c)), false); status != 200 {
		t.Fatalf("status %d, %v", status, got)
	}
}

func TestInvalidAssertionsAreRefused(t *testing.T) {
	srv, s := fedERP(t)
	other := jwttest.New(t)
	now := time.Now().Unix()
	with := func(k string, v any) string {
		c := goodClaims()
		c[k] = v
		return s.Sign(c)
	}
	header := func(h map[string]any) string { return s.SignWith(h, goodClaims()) }
	cases := map[string]url.Values{
		"alg HS256":        assertionForm(header(map[string]any{"alg": "HS256", "kid": s.KID})),
		"alg none":         assertionForm(header(map[string]any{"alg": "none", "kid": s.KID})),
		"unknown kid":      assertionForm(header(map[string]any{"alg": "RS256", "kid": "other"})),
		"another key":      assertionForm(other.Sign(goodClaims())),
		"wrong issuer":     assertionForm(with("iss", "https://evil.test")),
		"wrong subject":    assertionForm(with("sub", "system:serviceaccount:eacp:eacp")),
		"wrong audience":   assertionForm(with("aud", []string{"other"})),
		"expired":          assertionForm(with("exp", now-60)),
		"not yet valid":    assertionForm(with("nbf", now+120)),
		"issued in future": assertionForm(with("iat", now+120)),
		"no expiry":        assertionForm(with("exp", nil)),
		"malformed":        assertionForm("a.b.c"),
		"wrong client id":  func() url.Values { f := assertionForm(s.Sign(goodClaims())); f.Set("client_id", "x"); return f }(),
		"wrong assertion type": func() url.Values {
			f := assertionForm(s.Sign(goodClaims()))
			f.Set("client_assertion_type", "urn:x")
			return f
		}(),
	}
	for name, form := range cases {
		status, got := postToken(t, srv, form, false)
		if status != 401 || got["error"] != "invalid_client" {
			t.Errorf("%s: status %d, %v", name, status, got)
		}
	}
	entries, raw := auditOf(t, srv)
	refusals := 0
	for _, e := range entries {
		if e.Path != "/oauth/token" {
			continue
		}
		if e.Outcome != "invalid_client" {
			t.Fatalf("a refused assertion was audited as %q", e.Outcome)
		}
		refusals++
	}
	if refusals != len(cases) {
		t.Fatalf("%d audited refusals for %d requests", refusals, len(cases))
	}
	if strings.Contains(raw, "eyJ") { // every JWT segment of a JSON object starts so
		t.Fatal("the audit holds an assertion")
	}
}

func TestTwoAuthenticationMethodsAreRefused(t *testing.T) {
	s := jwttest.New(t)
	h, err := fakeerp.NewWithOptions(credential, filepath.Join(t.TempDir(), "erp.log"), fakeerp.Options{
		OAuthClientID: oauthClient, OAuthClientSecret: oauthSecret, Federated: federated(t, s)})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	if status, got := postToken(t, srv, assertionForm(s.Sign(goodClaims())), true); status != 400 || got["error"] != "invalid_request" {
		t.Fatalf("status %d, %v", status, got)
	}
	// Each client still works on its own.
	if status, _ := postToken(t, srv, assertionForm(s.Sign(goodClaims())), false); status != 200 {
		t.Fatalf("the federated client = %d", status)
	}
	if status, _ := mint(t, srv, oauthClient, oauthSecret, "client_credentials"); status != 200 {
		t.Fatalf("the secret client = %d", status)
	}
}

func TestAFederatedOnlyERPRefusesBasicAuth(t *testing.T) {
	srv, _ := fedERP(t)
	if status, _ := mint(t, srv, oauthClient, oauthSecret, "client_credentials"); status != 401 {
		t.Fatalf("Basic auth without a secret client = %d", status)
	}
}

func TestParseJWKSIgnoresOtherKeysAndNeedsAnRSAKey(t *testing.T) {
	s := jwttest.New(t)
	var set map[string][]map[string]any
	_ = json.Unmarshal(s.JWKS(), &set)
	ec := map[string]any{"kty": "EC", "kid": "ec", "crv": "P-256", "x": "AAAA", "y": "AAAA"}
	mixed, _ := json.Marshal(map[string]any{"keys": append(set["keys"], ec)})
	keys, err := fakeerp.ParseJWKS(mixed)
	if err != nil || len(keys) != 1 || keys[s.KID] == nil {
		t.Fatalf("keys = %v, err = %v", keys, err)
	}
	onlyEC, _ := json.Marshal(map[string]any{"keys": []any{ec}})
	for name, raw := range map[string][]byte{"only EC": onlyEC, "empty": []byte(`{}`), "not JSON": []byte(`x`)} {
		if _, err := fakeerp.ParseJWKS(raw); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestFederatedOptionsFailClosed(t *testing.T) {
	s := jwttest.New(t)
	path := filepath.Join(t.TempDir(), "erp.log")
	for name, spoil := range map[string]func(f *fakeerp.Federated){
		"no client id": func(f *fakeerp.Federated) { f.ClientID = "" },
		"no issuer":    func(f *fakeerp.Federated) { f.Issuer = "" },
		"no audience":  func(f *fakeerp.Federated) { f.Audience = "" },
		"no subject":   func(f *fakeerp.Federated) { f.Subject = "" },
		"no keys":      func(f *fakeerp.Federated) { f.Keys = nil },
	} {
		f := federated(t, s)
		spoil(f)
		if _, err := fakeerp.NewWithOptions(credential, path, fakeerp.Options{Federated: f}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
