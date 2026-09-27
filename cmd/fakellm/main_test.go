package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestFakeLLMStartupRequiresItsKeyFile(t *testing.T) {
	data := filepath.Join(t.TempDir(), "llm.log")
	if _, _, err := loadHandler("", data); err == nil {
		t.Fatal("started without a key path")
	}
	if _, _, err := loadHandler(filepath.Join(t.TempDir(), "missing"), data); err == nil {
		t.Fatal("started without a key")
	}
	path := filepath.Join(t.TempDir(), "key")
	for _, bad := range []string{"\n", "two words\n"} {
		if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := loadHandler(path, data); err == nil {
			t.Fatalf("started with key %q", bad)
		}
	}
	if err := os.WriteFile(path, []byte("sk-fake-startup\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, key, err := loadHandler(path, data)
	if err != nil || key != "sk-fake-startup" {
		t.Fatalf("%q %v", key, err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/audit", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unkeyed audit: %d", w.Code)
	}
	if _, _, err := loadHandler(path, ""); err == nil {
		t.Fatal("started without a data file")
	}
}
