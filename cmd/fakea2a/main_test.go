package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestFakeA2ASettingsFailClosed(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("startup-test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	good := map[string]string{
		"EACP_FAKEA2A_TOKEN_FILE": tokenFile,
		"EACP_FAKEA2A_DATA_FILE":  filepath.Join(dir, "a2a.log"),
		"EACP_FAKEA2A_ENDPOINT":   "http://fakea2a:8092/a2a",
	}
	env := func(change map[string]string) func(string) string {
		return func(k string) string {
			if v, ok := change[k]; ok {
				return v
			}
			return good[k]
		}
	}
	for name, change := range map[string]map[string]string{
		"no token file":      {"EACP_FAKEA2A_TOKEN_FILE": ""},
		"missing token file": {"EACP_FAKEA2A_TOKEN_FILE": filepath.Join(dir, "missing")},
		"empty token":        {"EACP_FAKEA2A_TOKEN_FILE": empty},
		"no data file":       {"EACP_FAKEA2A_DATA_FILE": ""},
		"no endpoint":        {"EACP_FAKEA2A_ENDPOINT": ""},
		"relative endpoint":  {"EACP_FAKEA2A_ENDPOINT": "/a2a"},
		"endpoint with user": {"EACP_FAKEA2A_ENDPOINT": "http://u:p@fakea2a:8092/a2a"},
		"endpoint path":      {"EACP_FAKEA2A_ENDPOINT": "http://fakea2a:8092/rpc"},
	} {
		if _, _, err := loadHandler(env(change)); err == nil {
			t.Errorf("%s: started", name)
		}
	}
	h, token, err := loadHandler(env(nil))
	if err != nil || token != "startup-test-token" {
		t.Fatalf("good settings: %v", err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/a2a", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("uncredentialed status = %d", w.Code)
	}
}
