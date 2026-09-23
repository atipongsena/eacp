// Package connector implements the worker's HTTP connector protocol.
package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"github.com/google/uuid"

	"eacp/internal/worker"
)

const maxResponseBytes = 16 << 10

var headerName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]*$`)

// HTTP speaks EACP's JSON HTTP connector protocol. It never uses proxy
// environment variables or follows redirects, so a target cannot forward
// a worker-held bearer credential to another host.
type HTTP struct{ client *http.Client }

func NewHTTP() *HTTP {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &HTTP{client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}}
}

func endpoint(base string, parts ...string) (string, bool) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	return u.JoinPath(parts...).String(), true
}

func safeKeyHeader(name string) bool {
	if !headerName.MatchString(name) {
		return false
	}
	switch http.CanonicalHeaderKey(name) {
	case "Authorization", "Proxy-Authorization", "Cookie", "Host", "Content-Type", "X-Eacp-Tenant-Id":
		return false
	}
	return true
}

func validOperationKey(key string, tenant uuid.UUID) bool {
	parts := strings.Split(key, ":")
	if len(parts) != 3 || parts[0] != "eacp" || parts[1] != tenant.String() {
		return false
	}
	action, err := uuid.Parse(parts[2])
	return err == nil && action != uuid.Nil
}

func (h *HTTP) request(ctx context.Context, method, endpoint, tenant, secret string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("X-EACP-Tenant-ID", tenant)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return h.client.Do(req)
}

func readResponse(r *http.Response, dst any) bool {
	defer r.Body.Close()
	b, err := io.ReadAll(io.LimitReader(r.Body, maxResponseBytes+1))
	if err != nil || len(b) > maxResponseBytes {
		return false
	}
	return json.Unmarshal(b, dst) == nil
}

// Execute sends only the enforced payload inside the protocol envelope.
// For native idempotency the declared header carries the stable operation
// key; for correlation-only the declared envelope field carries it.
func (h *HTTP) Execute(ctx context.Context, c worker.Call) worker.Result {
	target, ok := endpoint(c.Endpoint, "v1", "execute")
	if !ok || c.Secret.Reveal() == "" || !validOperationKey(c.OperationKey, c.TenantID) {
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_endpoint_or_credential"}
	}
	body := map[string]any{"tool": c.Tool, "payload": c.Payload}
	switch c.Contract.IdempotencyMode {
	case "native":
		if !safeKeyHeader(c.Contract.IdempotencyKeyField) || c.OperationKey == "" {
			return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_contract"}
		}
	case "correlation_only":
		field := c.Contract.CorrelationField
		if field == "" || field == "tool" || field == "payload" || c.OperationKey == "" {
			return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_contract"}
		}
		body[field] = c.OperationKey
	case "none":
	default:
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_contract"}
	}
	b, err := json.Marshal(body)
	if err != nil {
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_payload"}
	}
	// Set the native key on a request constructed here rather than allowing
	// generic caller-controlled headers to overwrite authentication.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(b))
	if err != nil {
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_endpoint"}
	}
	// Go treats a POST with Idempotency-Key and GetBody as replayable on a
	// reused connection. The target may have applied the first send before
	// losing its response, so only the worker may decide on another attempt.
	req.GetBody = nil
	req.Header.Set("Authorization", "Bearer "+c.Secret.Reveal())
	req.Header.Set("X-EACP-Tenant-ID", c.TenantID.String())
	req.Header.Set("Content-Type", "application/json")
	if c.Contract.IdempotencyMode == "native" {
		req.Header.Set(c.Contract.IdempotencyKeyField, c.OperationKey)
	}
	r, err := h.client.Do(req)
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, syscall.ECONNREFUSED) {
			return worker.Result{Outcome: worker.NoEffect, ErrorClass: "connection_refused_before_send"}
		}
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "transport_error"}
	}
	var data struct {
		ExternalReference string `json:"external_reference"`
		ErrorClass        string `json:"error_class"`
	}
	if !readResponse(r, &data) {
		return worker.Result{Outcome: worker.Ambiguous, ErrorClass: "invalid_response"}
	}
	if r.StatusCode >= 200 && r.StatusCode < 300 && strings.TrimSpace(data.ExternalReference) != "" {
		return worker.Result{Outcome: worker.Succeeded, ExternalReference: data.ExternalReference}
	}
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		if data.ExternalReference == "" && slices.Contains(c.Contract.NoEffectErrors, data.ErrorClass) && data.ErrorClass != "" {
			return worker.Result{Outcome: worker.NoEffect, ErrorClass: data.ErrorClass}
		}
	}
	return worker.Result{Outcome: worker.Ambiguous, ErrorClass: data.ErrorClass}
}

// Lookup reports what the HTTP target currently sees. A 404 is merely
// absence; the reconciler must consult proof_standard before acting on it.
func (h *HTTP) Lookup(ctx context.Context, c worker.LookupCall) worker.LookupResult {
	if !validOperationKey(c.OperationKey, c.TenantID) || c.Secret.Reveal() == "" {
		return worker.LookupResult{Status: worker.LookupUnknown}
	}
	target, ok := endpoint(c.Endpoint, "v1", "operations", url.PathEscape(c.OperationKey))
	if !ok {
		return worker.LookupResult{Status: worker.LookupUnknown}
	}
	r, err := h.request(ctx, http.MethodGet, target, c.TenantID.String(), c.Secret.Reveal(), nil)
	if err != nil {
		return worker.LookupResult{Status: worker.LookupUnknown}
	}
	if r.StatusCode != http.StatusOK && r.StatusCode != http.StatusNotFound {
		r.Body.Close()
		return worker.LookupResult{Status: worker.LookupUnknown}
	}
	var data struct {
		ExternalReference string `json:"external_reference"`
		ErrorClass        string `json:"error_class"`
	}
	if !readResponse(r, &data) {
		return worker.LookupResult{Status: worker.LookupUnknown}
	}
	if r.StatusCode == http.StatusNotFound {
		if data.ErrorClass == "not_found" && data.ExternalReference == "" {
			return worker.LookupResult{Status: worker.LookupAbsent}
		}
		return worker.LookupResult{Status: worker.LookupUnknown}
	}
	if strings.TrimSpace(data.ExternalReference) == "" ||
		len(data.ExternalReference) > 512 || strings.Contains(data.ExternalReference, c.Secret.Reveal()) {
		return worker.LookupResult{Status: worker.LookupUnknown}
	}
	return worker.LookupResult{Status: worker.LookupFound, ExternalReference: data.ExternalReference}
}
