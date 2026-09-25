package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// An API answer is JSON only; a browser must never sniff it as HTML.
func TestJSONResponsesAreNeverSniffed(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, map[string]string{"title": "<img src=x>"})
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
}
