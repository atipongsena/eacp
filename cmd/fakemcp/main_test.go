package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestFakeMCPStartupRequiresCredentialFile(t *testing.T) {
	if _, _, err := loadHandler("", "", ""); err == nil {
		t.Fatal("started without a credential path")
	}
	if _, _, err := loadHandler(filepath.Join(t.TempDir(), "missing"), "", ""); err == nil {
		t.Fatal("started without a credential")
	}
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadHandler(path, "", ""); err == nil {
		t.Fatal("started with an empty credential")
	}
	if err := os.WriteFile(path, []byte("startup-test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, token, err := loadHandler(path, "", "")
	if err != nil || token != "startup-test-token" {
		t.Fatalf("credential file was not loaded: %v", err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("uncredentialed status = %d", w.Code)
	}
}
