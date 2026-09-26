package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"eacp/internal/fakeerp"
)

func TestFakeERPStartupRequiresCredentialFile(t *testing.T) {
	data := filepath.Join(t.TempDir(), "erp.log")
	if _, _, err := loadHandler("", data, fakeerp.Options{}); err == nil {
		t.Fatal("started without a credential path")
	}
	if _, _, err := loadHandler(filepath.Join(t.TempDir(), "missing"), data, fakeerp.Options{}); err == nil {
		t.Fatal("started without a credential")
	}
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadHandler(path, data, fakeerp.Options{}); err == nil {
		t.Fatal("started with an empty credential")
	}
	if err := os.WriteFile(path, []byte("startup-test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, token, err := loadHandler(path, data, fakeerp.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if token != "startup-test-token" {
		t.Fatal("credential file was not loaded")
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/audit", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("uncredentialed audit status = %d", w.Code)
	}
}

func TestOAuthSettingsFailClosed(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "client")
	if err := os.WriteFile(secret, []byte("client-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	o, err := loadOAuth(env(map[string]string{}))
	if err != nil || o.OAuthClientID != "" {
		t.Fatalf("no settings = %+v, %v", o, err)
	}
	o, err = loadOAuth(env(map[string]string{"EACP_FAKEERP_OAUTH_CLIENT_ID": "eacp-worker",
		"EACP_FAKEERP_OAUTH_CLIENT_SECRET_FILE": secret}))
	if err != nil || o.OAuthClientID != "eacp-worker" || o.OAuthClientSecret != "client-secret" || o.TokenTTL != 300*time.Second {
		t.Fatalf("defaults = %v", err)
	}
	o, err = loadOAuth(env(map[string]string{"EACP_FAKEERP_OAUTH_CLIENT_ID": "eacp-worker",
		"EACP_FAKEERP_OAUTH_CLIENT_SECRET_FILE": secret, "EACP_FAKEERP_OAUTH_TTL": "90s"}))
	if err != nil || o.TokenTTL != 90*time.Second {
		t.Fatalf("ttl = %v, %v", o.TokenTTL, err)
	}
	for name, m := range map[string]map[string]string{
		"id without secret": {"EACP_FAKEERP_OAUTH_CLIENT_ID": "x"},
		"secret without id": {"EACP_FAKEERP_OAUTH_CLIENT_SECRET_FILE": secret},
		"unreadable secret": {"EACP_FAKEERP_OAUTH_CLIENT_ID": "x", "EACP_FAKEERP_OAUTH_CLIENT_SECRET_FILE": filepath.Join(dir, "absent")},
		"ttl too long":      {"EACP_FAKEERP_OAUTH_CLIENT_ID": "x", "EACP_FAKEERP_OAUTH_CLIENT_SECRET_FILE": secret, "EACP_FAKEERP_OAUTH_TTL": "2h"},
		"ttl too short":     {"EACP_FAKEERP_OAUTH_CLIENT_ID": "x", "EACP_FAKEERP_OAUTH_CLIENT_SECRET_FILE": secret, "EACP_FAKEERP_OAUTH_TTL": "500ms"},
		"ttl not duration":  {"EACP_FAKEERP_OAUTH_CLIENT_ID": "x", "EACP_FAKEERP_OAUTH_CLIENT_SECRET_FILE": secret, "EACP_FAKEERP_OAUTH_TTL": "soon"},
	} {
		if _, err := loadOAuth(env(m)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
