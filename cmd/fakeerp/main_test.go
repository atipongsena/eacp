package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestFakeERPStartupRequiresCredentialFile(t *testing.T) {
	data := filepath.Join(t.TempDir(), "erp.log")
	if _, _, err := loadHandler("", data); err == nil {
		t.Fatal("started without a credential path")
	}
	if _, _, err := loadHandler(filepath.Join(t.TempDir(), "missing"), data); err == nil {
		t.Fatal("started without a credential")
	}
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadHandler(path, data); err == nil {
		t.Fatal("started with an empty credential")
	}
	if err := os.WriteFile(path, []byte("startup-test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, token, err := loadHandler(path, data)
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
