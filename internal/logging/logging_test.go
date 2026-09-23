package logging

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

const canary = "canary-7f3a9e"

func newTestLogger(t *testing.T, secrets ...string) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	return New(&buf, slog.LevelDebug, "json", secrets...), &buf
}

func assertNoLeak(t *testing.T, out string) {
	t.Helper()
	if strings.Contains(out, canary) {
		t.Fatalf("secret leaked into log output: %s", out)
	}
	if !strings.Contains(out, Redacted) {
		t.Fatalf("expected %q marker in output: %s", Redacted, out)
	}
}

func TestSensitiveKeysAreRedacted(t *testing.T) {
	for _, key := range []string{"password", "DB_PASSWORD", "api_key", "apiKey", "token", "Authorization", "client_secret", "credential"} {
		t.Run(key, func(t *testing.T) {
			log, buf := newTestLogger(t)
			log.Info("msg", key, canary)
			assertNoLeak(t, buf.String())
		})
	}
}

func TestRegisteredSecretValuesAreRedactedInMessageAndAttrs(t *testing.T) {
	log, buf := newTestLogger(t, canary)
	log.Info("connecting with "+canary, "dsn", "postgres://u:"+canary+"@db/x")
	assertNoLeak(t, buf.String())
}

func TestRedactionAppliesToWithAttrsAndGroups(t *testing.T) {
	log, buf := newTestLogger(t, canary)
	log.With("api_key", "abc").WithGroup("conn").Info("msg",
		slog.Group("auth", slog.String("note", "uses "+canary)))
	out := buf.String()
	assertNoLeak(t, out)
	if strings.Contains(out, `"api_key":"abc"`) {
		t.Fatalf("api_key attribute from With() leaked: %s", out)
	}
}

func TestErrorValuesAreRedacted(t *testing.T) {
	log, buf := newTestLogger(t, canary)
	log.Error("failed", "err", errors.New("auth failed for "+canary))
	assertNoLeak(t, buf.String())
}

func TestNonSensitiveValuesAreKept(t *testing.T) {
	log, buf := newTestLogger(t, canary)
	log.Info("action queued", "action_id", "act_123", "state", "QUEUED")
	out := buf.String()
	if !strings.Contains(out, "act_123") || !strings.Contains(out, "QUEUED") {
		t.Fatalf("non-sensitive values were altered: %s", out)
	}
}

func TestEmptySecretIsIgnored(t *testing.T) {
	log, buf := newTestLogger(t, "")
	log.Info("hello world")
	if !strings.Contains(buf.String(), "hello world") {
		t.Fatalf("empty secret mangled output: %s", buf.String())
	}
}

func TestTextFormat(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, slog.LevelInfo, "text").Info("hello", "password", canary)
	out := buf.String()
	if strings.HasPrefix(out, "{") {
		t.Fatalf("expected text output, got JSON: %s", out)
	}
	assertNoLeak(t, out)
}
