package health

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func get(t *testing.T, h http.Handler, path string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %q", rec.Body.String())
	}
	return rec, body
}

func newMux(logBuf *bytes.Buffer, checks ...Check) http.Handler {
	mux := http.NewServeMux()
	Register(mux, slog.New(slog.NewJSONHandler(logBuf, nil)), time.Second, checks...)
	return mux
}

func TestLivenessIsAlwaysOK(t *testing.T) {
	var logs bytes.Buffer
	mux := newMux(&logs, Check{Name: "db", Fn: func(context.Context) error { return errors.New("down") }})
	rec, body := get(t, mux, "/healthz")
	if rec.Code != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("liveness = %d %v, want 200 ok", rec.Code, body)
	}
}

func TestReadinessOKWhenAllChecksPass(t *testing.T) {
	var logs bytes.Buffer
	ok := func(context.Context) error { return nil }
	rec, body := get(t, newMux(&logs, Check{"db", ok}, Check{"schema", ok}), "/readyz")
	if rec.Code != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("readiness = %d %v, want 200 ok", rec.Code, body)
	}
	checks := body["checks"].(map[string]any)
	if checks["db"] != "ok" || checks["schema"] != "ok" {
		t.Fatalf("checks = %v", checks)
	}
}

func TestReadinessFailsWithoutLeakingErrorDetails(t *testing.T) {
	var logs bytes.Buffer
	failing := Check{"db", func(context.Context) error {
		return errors.New("dial postgres://eacp_app:hunter2@db: refused")
	}}
	rec, body := get(t, newMux(&logs, Check{"schema", func(context.Context) error { return nil }}, failing), "/readyz")
	if rec.Code != http.StatusServiceUnavailable || body["status"] != "fail" {
		t.Fatalf("readiness = %d %v, want 503 fail", rec.Code, body)
	}
	if body["checks"].(map[string]any)["db"] != "fail" {
		t.Fatalf("checks = %v, want db=fail", body["checks"])
	}
	if strings.Contains(rec.Body.String(), "hunter2") || strings.Contains(rec.Body.String(), "refused") {
		t.Fatalf("error details leaked in response body: %s", rec.Body.String())
	}
	if !strings.Contains(logs.String(), "refused") {
		t.Fatalf("expected failing check to be logged, logs: %s", logs.String())
	}
}

func TestReadinessTimesOutChecksThatIgnoreContext(t *testing.T) {
	var logs bytes.Buffer
	mux := http.NewServeMux()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	Register(mux, slog.New(slog.NewJSONHandler(&logs, nil)), 50*time.Millisecond,
		Check{"stuck", func(context.Context) error { <-release; return nil }})

	start := time.Now()
	rec, body := get(t, mux, "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness = %d, want 503", rec.Code)
	}
	if body["checks"].(map[string]any)["stuck"] != "timeout" {
		t.Fatalf("checks = %v, want stuck=timeout", body["checks"])
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("readiness took %v; a check ignoring ctx blocked the probe", elapsed)
	}
}

func TestReadinessTimesOutHangingChecks(t *testing.T) {
	var logs bytes.Buffer
	mux := http.NewServeMux()
	Register(mux, slog.New(slog.NewJSONHandler(&logs, nil)), 50*time.Millisecond,
		Check{"slow", func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }})

	start := time.Now()
	rec, _ := get(t, mux, "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness = %d, want 503", rec.Code)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("readiness took %v, check timeout not applied", elapsed)
	}
}
