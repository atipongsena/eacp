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

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/fakeerp"
)

const (
	oauthClient = "eacp+worker"
	oauthSecret = "client s3cret:canary"
)

func oauthERP(t *testing.T, ttl time.Duration, path string) *httptest.Server {
	t.Helper()
	h, err := fakeerp.NewWithOptions(credential, path, fakeerp.Options{
		OAuthClientID: oauthClient, OAuthClientSecret: oauthSecret, TokenTTL: ttl})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// mint asks srv for a token as RFC 6749 §2.3.1 says: the id and secret are
// form-urlencoded before Basic auth.
func mint(t *testing.T, srv *httptest.Server, id, secret, grant string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/oauth/token", strings.NewReader(url.Values{"grant_type": {grant}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(url.QueryEscape(id), url.QueryEscape(secret))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode == 200 && resp.Header.Get("Cache-Control") != "no-store" {
		t.Error("a token response may be cached")
	}
	return resp.StatusCode, out
}

func executeWith(t *testing.T, srv *httptest.Server, bearer string) int {
	t.Helper()
	tenant := uuid.New()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/execute",
		strings.NewReader(`{"tool":"erp.create_po","payload":{"amount":1}}`))
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-EACP-Tenant-ID", tenant.String())
	req.Header.Set("Idempotency-Key", "eacp:"+tenant.String()+":"+uuid.NewString())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

type auditEntry struct {
	Principal   string     `json:"principal"`
	Path        string     `json:"path"`
	Outcome     string     `json:"outcome"`
	TokenSHA256 string     `json:"token_sha256"`
	ExpiresAt   *time.Time `json:"expires_at"`
}

func auditOf(t *testing.T, srv *httptest.Server) ([]auditEntry, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+"/v1/audit", nil)
	req.Header.Set("Authorization", "Bearer "+credential)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw := body(t, resp)
	var out []auditEntry
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out, string(raw)
}

func TestAMintedTokenAuthorisesUntilItExpires(t *testing.T) {
	srv := oauthERP(t, 2*time.Second, filepath.Join(t.TempDir(), "erp.log"))
	status, got := mint(t, srv, oauthClient, oauthSecret, "client_credentials")
	tok, _ := got["access_token"].(string)
	if status != 200 || len(tok) != 43 || got["token_type"] != "Bearer" || got["expires_in"] != float64(2) {
		t.Fatalf("mint = %d %v", status, got)
	}
	if code := executeWith(t, srv, tok); code != 200 {
		t.Fatalf("execute with a fresh token = %d", code)
	}
	entries, raw := auditOf(t, srv)
	if strings.Contains(raw, tok) || strings.Contains(raw, oauthSecret) {
		t.Fatal("the audit holds the token or the client secret")
	}
	sum := sha256.Sum256([]byte(tok))
	var issued, executed bool
	for _, e := range entries {
		if e.Path == "/oauth/token" && e.Outcome == "token_issued" && e.Principal == "oauth:"+oauthClient &&
			e.TokenSHA256 == hex.EncodeToString(sum[:]) && e.ExpiresAt != nil {
			issued = true
		}
		if e.Path == "/v1/execute" && e.Principal == "oauth:"+oauthClient && e.Outcome == "effect_committed" {
			executed = true
		}
	}
	if !issued || !executed {
		t.Fatalf("audit lacks the issuance (%v) or the oauth principal (%v): %s", issued, executed, raw)
	}
	time.Sleep(2100 * time.Millisecond)
	if code := executeWith(t, srv, tok); code != 401 {
		t.Fatalf("execute with an expired token = %d, want 401", code)
	}
	if code := executeWith(t, srv, credential); code != 200 {
		t.Fatalf("the static credential stopped working: %d", code)
	}
}

func TestTheTokenEndpointRefusesBadClientsAndGrants(t *testing.T) {
	srv := oauthERP(t, time.Minute, filepath.Join(t.TempDir(), "erp.log"))
	if status, got := mint(t, srv, oauthClient, "wrong", "client_credentials"); status != 401 || got["error"] != "invalid_client" {
		t.Fatalf("wrong secret = %d %v", status, got)
	}
	if status, got := mint(t, srv, "other", oauthSecret, "client_credentials"); status != 401 || got["error"] != "invalid_client" {
		t.Fatalf("wrong client = %d %v", status, got)
	}
	if status, got := mint(t, srv, oauthClient, oauthSecret, "password"); status != 400 || got["error"] != "unsupported_grant_type" {
		t.Fatalf("wrong grant = %d %v", status, got)
	}
	if code := executeWith(t, srv, "not-a-token"); code != 401 {
		t.Fatalf("unknown token = %d", code)
	}
	entries, raw := auditOf(t, srv)
	if strings.Contains(raw, "wrong") || strings.Contains(raw, oauthSecret) {
		t.Fatal("the audit holds a presented secret")
	}
	refused := 0
	for _, e := range entries {
		if e.Path == "/oauth/token" && (e.Outcome == "invalid_client" || e.Outcome == "unsupported_grant_type") {
			refused++
		}
	}
	if refused != 3 {
		t.Fatalf("%d refused mints audited, want 3", refused)
	}
}

func TestMintedTokensSurviveARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "erp.log")
	srv := oauthERP(t, time.Minute, path)
	_, got := mint(t, srv, oauthClient, oauthSecret, "client_credentials")
	srv.Close()
	again := oauthERP(t, time.Minute, path)
	if code := executeWith(t, again, got["access_token"].(string)); code != 200 {
		t.Fatalf("a token issued before the restart = %d", code)
	}
}

func TestWithoutAnOAuthClientThereIsNoTokenEndpoint(t *testing.T) {
	e := newEnvironment(t)
	if status, _ := mint(t, e.server, oauthClient, oauthSecret, "client_credentials"); status != 404 && status != 405 {
		t.Fatalf("token endpoint without a client = %d", status)
	}
}

func TestOAuthOptionsFailClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "erp.log")
	for name, o := range map[string]fakeerp.Options{
		"id without secret": {OAuthClientID: "x"},
		"secret without id": {OAuthClientSecret: "x"},
		"ttl too short":     {OAuthClientID: "x", OAuthClientSecret: "y", TokenTTL: 500 * time.Millisecond},
		"ttl too long":      {OAuthClientID: "x", OAuthClientSecret: "y", TokenTTL: 2 * time.Hour},
	} {
		if _, err := fakeerp.NewWithOptions(credential, path, o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
