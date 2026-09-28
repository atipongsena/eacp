package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/atipongsena/eacp/internal/config"
)

func TestConsoleIsMountedUnlessDisabled(t *testing.T) {
	for _, on := range []bool{true, false} {
		mux := http.NewServeMux()
		mountUI(config.Config{UI: on}, mux)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/", nil))
		want := http.StatusNotFound
		if on {
			want = http.StatusOK
		}
		if rec.Code != want {
			t.Errorf("EACP_UI on=%v: GET /ui/ = %d, want %d", on, rec.Code, want)
		}
	}
}
