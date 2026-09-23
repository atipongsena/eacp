// Package logging builds slog loggers that redact secrets.
//
// Redaction is defence in depth: code must still avoid logging secrets, but
// a mistake here must not leak connector credentials (ADR-001 §3).
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// Redacted replaces any value that is, or contains, a secret.
const Redacted = "[REDACTED]"

// sensitiveKeyFragments are matched against attribute keys after lowercasing
// and removing '_' and '-'.
var sensitiveKeyFragments = []string{
	"password", "passwd", "secret", "token", "apikey",
	"authorization", "credential", "cookie", "privatekey",
}

// New returns a logger writing to w in the given format ("json" or "text").
// Any value in secrets is redacted wherever it appears in the message or in
// attribute values; attributes with sensitive-looking keys are always redacted.
func New(w io.Writer, level slog.Leveler, format string, secrets ...string) *slog.Logger {
	r := redactor{}
	for _, s := range secrets {
		if s != "" {
			r.secrets = append(r.secrets, s)
		}
	}
	opts := &slog.HandlerOptions{Level: level, ReplaceAttr: r.replace}
	if format == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

type redactor struct {
	secrets []string
}

// replace is used as slog.HandlerOptions.ReplaceAttr. Built-in handlers call
// it for the message and for every non-group attribute (including those
// added with Logger.With), after resolving LogValuers.
func (r redactor) replace(_ []string, a slog.Attr) slog.Attr {
	if isSensitiveKey(a.Key) {
		return slog.String(a.Key, Redacted)
	}
	switch a.Value.Kind() {
	case slog.KindString:
		return slog.String(a.Key, r.scrub(a.Value.String()))
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok {
			return slog.String(a.Key, r.scrub(err.Error()))
		}
		if len(r.secrets) > 0 {
			s := fmt.Sprintf("%+v", a.Value.Any())
			if scrubbed := r.scrub(s); scrubbed != s {
				return slog.String(a.Key, scrubbed)
			}
		}
	}
	return a
}

func (r redactor) scrub(s string) string {
	for _, secret := range r.secrets {
		s = strings.ReplaceAll(s, secret, Redacted)
	}
	return s
}

func isSensitiveKey(key string) bool {
	k := strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(key))
	for _, frag := range sensitiveKeyFragments {
		if strings.Contains(k, frag) {
			return true
		}
	}
	return false
}
