// Package health serves liveness (/healthz) and readiness (/readyz) probes.
package health

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// Check is a named readiness check. Fn should honour ctx cancellation.
type Check struct {
	Name string
	Fn   func(context.Context) error
}

type response struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

// Register mounts /healthz and /readyz on mux.
//
// Liveness only reports that the process is serving. Readiness runs every
// check concurrently and answers within the timeout even if a check ignores
// its context; such a check is reported as "timeout". Failure details are
// logged, not returned, because probe endpoints may be reachable by
// untrusted callers.
func Register(mux *http.ServeMux, log *slog.Logger, timeout time.Duration, checks ...Check) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		write(w, http.StatusOK, response{Status: "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()

		type result struct{ name, status string }
		done := make(chan result, len(checks)) // buffered: late checks never block
		for _, c := range checks {
			go func() {
				status := "ok"
				if err := c.Fn(ctx); err != nil {
					status = "fail"
					log.WarnContext(ctx, "readiness check failed", "check", c.Name, "err", err)
				}
				done <- result{c.Name, status}
			}()
		}

		results := make(map[string]string, len(checks))
		for _, c := range checks {
			results[c.Name] = "timeout"
		}
	collect:
		for range checks {
			select {
			case r := <-done:
				results[r.name] = r.status
			case <-ctx.Done():
				break collect
			}
		}
		for name, s := range results {
			if s == "timeout" {
				log.WarnContext(ctx, "readiness check timed out", "check", name)
			}
		}

		resp := response{Status: "ok", Checks: results}
		code := http.StatusOK
		for _, s := range results {
			if s != "ok" {
				resp.Status, code = "fail", http.StatusServiceUnavailable
			}
		}
		write(w, code, resp)
	})
}

func write(w http.ResponseWriter, code int, v response) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
