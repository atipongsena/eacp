package fakeerp_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"eacp/internal/fakeerp"
)

const credential = "fake-erp-test-canary"

type environment struct {
	t      *testing.T
	path   string
	server *httptest.Server
	tenant uuid.UUID
}

func newEnvironment(t *testing.T) *environment {
	t.Helper()
	e := &environment{t: t, path: filepath.Join(t.TempDir(), "erp.log"), tenant: uuid.New()}
	e.open()
	t.Cleanup(e.close)
	return e
}

func (e *environment) open() {
	e.t.Helper()
	h, err := fakeerp.New(credential, e.path)
	if err != nil {
		e.t.Fatal(err)
	}
	e.server = httptest.NewServer(h)
}

func (e *environment) close() {
	if e.server != nil {
		e.server.Close()
		e.server = nil
	}
}

func (e *environment) request(method, path, key, body string, authorized bool) (*http.Response, error) {
	e.t.Helper()
	req, err := http.NewRequest(method, e.server.URL+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	if authorized {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	req.Header.Set("X-EACP-Tenant-ID", e.tenant.String())
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return http.DefaultClient.Do(req)
}

func (e *environment) execute(key, scenario string) (*http.Response, error) {
	return e.executeTool(key, "erp.create_po", scenario)
}

func (e *environment) executeTool(key, tool, scenario string) (*http.Response, error) {
	e.t.Helper()
	payload := map[string]any{"amount": 42}
	if scenario != "" {
		payload["scenario"] = scenario
		payload["delay_ms"] = 50
		if scenario == "delayed_visibility" {
			payload["visibility_delay_ms"] = 75
		}
	}
	b, _ := json.Marshal(map[string]any{"tool": tool, "payload": payload})
	return e.request(http.MethodPost, "/v1/execute", key, string(b), true)
}

func (e *environment) key() string { return "eacp:" + e.tenant.String() + ":" + uuid.NewString() }

func body(t *testing.T, r *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFakeERPRejectsUnauthenticatedPrivilegedCalls(t *testing.T) {
	e := newEnvironment(t)
	key := e.key()
	r, err := e.request(http.MethodPost, "/v1/execute", key, `{"tool":"erp.create_po","payload":{"amount":42}}`, false)
	if err != nil {
		t.Fatal(err)
	}
	body(t, r)
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("execute status = %d", r.StatusCode)
	}
	r, err = e.request(http.MethodGet, "/v1/operations/"+key, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	body(t, r)
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("lookup status = %d", r.StatusCode)
	}
	r, err = e.request(http.MethodGet, "/v1/operations/"+key, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	body(t, r)
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("unauthorized call created a record: lookup status = %d", r.StatusCode)
	}
}

func TestFakeERPRequiresBearerSchemeAndMatchingTenant(t *testing.T) {
	e := newEnvironment(t)
	key := e.key()
	req, err := http.NewRequest(http.MethodPost, e.server.URL+"/v1/execute",
		strings.NewReader(`{"tool":"erp.create_po","payload":{"amount":42}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", credential) // a raw token is not a bearer credential
	req.Header.Set("X-EACP-Tenant-ID", e.tenant.String())
	req.Header.Set("Idempotency-Key", key)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body(t, r)
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("raw-token status = %d", r.StatusCode)
	}
	req, err = http.NewRequest(http.MethodPost, e.server.URL+"/v1/execute",
		strings.NewReader(`{"tool":"erp.create_po","payload":{"amount":42}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	req.Header.Set("X-EACP-Tenant-ID", uuid.NewString())
	req.Header.Set("Idempotency-Key", key)
	r, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body(t, r)
	if r.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("cross-tenant key status = %d", r.StatusCode)
	}
}

func TestFakeERPAuditDoesNotPersistUntrustedCredentialText(t *testing.T) {
	e := newEnvironment(t)
	r, err := e.request(http.MethodGet, "/v1/operations/"+credential, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	body(t, r)
	r, err = e.request(http.MethodGet, "/v1/audit", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	b := body(t, r)
	if bytes.Contains(b, []byte(credential)) {
		t.Fatal("untrusted request text containing credential was persisted in audit")
	}
}

func TestFakeERPStopsServingAfterOperationLogWriteFails(t *testing.T) {
	e := newEnvironment(t)
	key := e.key()
	saved := e.path + ".saved"
	if err := os.Rename(e.path, saved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(e.path, 0o700); err != nil {
		t.Fatal(err)
	}
	r, err := e.execute(key, "")
	if err != nil {
		t.Fatal(err)
	}
	body(t, r)
	if r.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("write failure status = %d", r.StatusCode)
	}
	if err := os.Remove(e.path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(saved, e.path); err != nil {
		t.Fatal(err)
	}
	r, err = e.execute(key, "")
	if err != nil {
		t.Fatal(err)
	}
	body(t, r)
	if r.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("ERP resumed after uncertain log failure: status = %d", r.StatusCode)
	}
}

func TestFakeERPRemembersNativeOperationAcrossRestartAndAuditsWorker(t *testing.T) {
	e := newEnvironment(t)
	key := e.key()
	first, err := e.execute(key, "")
	if err != nil {
		t.Fatal(err)
	}
	want := body(t, first)
	if first.StatusCode != http.StatusOK || !bytes.Contains(want, []byte(`"external_reference":"PO-`)) {
		t.Fatalf("first response status = %d body = %s", first.StatusCode, want)
	}
	e.close()
	e.open()
	second, err := e.execute(key, "")
	if err != nil {
		t.Fatal(err)
	}
	got := body(t, second)
	if second.StatusCode != http.StatusOK || !bytes.Equal(got, want) {
		t.Fatalf("duplicate response status = %d body = %s, want %s", second.StatusCode, got, want)
	}
	lookup, err := e.request(http.MethodGet, "/v1/operations/"+key, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if b := body(t, lookup); lookup.StatusCode != http.StatusOK || !bytes.Equal(b, want) {
		t.Fatalf("lookup status = %d body = %s", lookup.StatusCode, b)
	}
	audit, err := e.request(http.MethodGet, "/v1/audit", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	auditBody := body(t, audit)
	if audit.StatusCode != http.StatusOK || !bytes.Contains(auditBody, []byte(`"principal":"execution-worker"`)) ||
		bytes.Contains(auditBody, []byte(credential)) {
		t.Fatalf("audit status = %d; principal missing or credential leaked", audit.StatusCode)
	}
}

func TestFakeERPAcceptsRegisteredConnectorPrefix(t *testing.T) {
	e := newEnvironment(t)
	r, err := e.executeTool(e.key(), "fakeerp.create_po", "")
	if err != nil {
		t.Fatal(err)
	}
	body(t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("registered connector prefix status = %d", r.StatusCode)
	}
}

func TestFakeERPCorrelationOnlyDuplicateIsAConflict(t *testing.T) {
	e := newEnvironment(t)
	key := e.key()
	payload, _ := json.Marshal(map[string]any{"tool": "erp.create_po", "external_reference": key,
		"payload": map[string]any{"amount": 42}})
	var refs []string
	for range 2 {
		r, err := e.request(http.MethodPost, "/v1/execute", "", string(payload), true)
		if err != nil {
			t.Fatal(err)
		}
		b := body(t, r)
		if r.StatusCode != http.StatusOK {
			t.Fatalf("correlation-only status = %d", r.StatusCode)
		}
		var result struct {
			ExternalReference string `json:"external_reference"`
		}
		if err := json.Unmarshal(b, &result); err != nil {
			t.Fatal(err)
		}
		refs = append(refs, result.ExternalReference)
	}
	if refs[0] == refs[1] {
		t.Fatal("correlation-only duplicate was silently deduplicated")
	}
	r, err := e.request(http.MethodGet, "/v1/operations/"+key, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	body(t, r)
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate lookup status = %d", r.StatusCode)
	}
}

func TestFakeERPCorrelationDuplicateConflictsBeforeDelayedRecordIsVisible(t *testing.T) {
	e := newEnvironment(t)
	key := e.key()
	for _, payload := range []map[string]any{
		{"amount": 42},
		{"amount": 42, "scenario": "delayed_visibility", "visibility_delay_ms": 5000},
	} {
		b, _ := json.Marshal(map[string]any{
			"tool": "erp.create_po_eventual", "external_reference": key, "payload": payload,
		})
		r, err := e.request(http.MethodPost, "/v1/execute", "", string(b), true)
		if err != nil {
			t.Fatal(err)
		}
		body(t, r)
		if r.StatusCode != http.StatusOK {
			t.Fatalf("correlation-only execute status = %d", r.StatusCode)
		}
	}
	r, err := e.request(http.MethodGet, "/v1/operations/"+key, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	body(t, r)
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate with delayed second record lookup status = %d, want 409", r.StatusCode)
	}
}

func TestFakeERPFailureModesAndDelayedVisibility(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		effected bool
	}{
		{"fail_before_execute", 422, false},
		{"rate_limit", 429, false},
		{"5xx_before_effect", 503, false},
		{"outage", 503, false},
		{"5xx_after_effect", 503, true},
		{"slow_response", 200, true},
		{"delayed_visibility", 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnvironment(t)
			key := e.key()
			tool := "erp.create_po"
			if tc.name == "delayed_visibility" {
				tool = "erp.create_po_eventual"
			}
			r, err := e.executeTool(key, tool, tc.name)
			if err != nil {
				t.Fatal(err)
			}
			body(t, r)
			if r.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", r.StatusCode, tc.status)
			}
			lookup, err := e.request(http.MethodGet, "/v1/operations/"+key, "", "", true)
			if err != nil {
				t.Fatal(err)
			}
			body(t, lookup)
			want := http.StatusNotFound
			if tc.effected && tc.name != "delayed_visibility" {
				want = http.StatusOK
			}
			if lookup.StatusCode != want {
				t.Fatalf("lookup status = %d, want %d", lookup.StatusCode, want)
			}
			if tc.name == "delayed_visibility" {
				time.Sleep(90 * time.Millisecond)
				lookup, err = e.request(http.MethodGet, "/v1/operations/"+key, "", "", true)
				if err != nil {
					t.Fatal(err)
				}
				body(t, lookup)
				if lookup.StatusCode != http.StatusOK {
					t.Fatalf("record not visible after delay: %d", lookup.StatusCode)
				}
			}
		})
	}
}

func TestStrongFakeERPToolRejectsDelayedVisibility(t *testing.T) {
	e := newEnvironment(t)
	key := e.key()
	r, err := e.execute(key, "delayed_visibility")
	if err != nil {
		t.Fatal(err)
	}
	body(t, r)
	if r.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("strong tool allowed delayed visibility: status = %d", r.StatusCode)
	}
	lookup, err := e.request(http.MethodGet, "/v1/operations/"+key, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	body(t, lookup)
	if lookup.StatusCode != http.StatusNotFound {
		t.Fatalf("strong tool created hidden record: %d", lookup.StatusCode)
	}
}

func TestFakeERPCanLoseResponseAfterEffect(t *testing.T) {
	e := newEnvironment(t)
	for _, scenario := range []string{"execute_then_reset", "execute_then_timeout"} {
		t.Run(scenario, func(t *testing.T) {
			key := e.key()
			payload, _ := json.Marshal(map[string]any{"tool": "erp.create_po", "payload": map[string]any{
				"scenario": scenario, "delay_ms": 100,
			}})
			client := &http.Client{Timeout: 20 * time.Millisecond}
			req, _ := http.NewRequest(http.MethodPost, e.server.URL+"/v1/execute", bytes.NewReader(payload))
			req.Header.Set("Authorization", "Bearer "+credential)
			req.Header.Set("X-EACP-Tenant-ID", e.tenant.String())
			req.Header.Set("Idempotency-Key", key)
			r, err := client.Do(req)
			if err == nil {
				body(t, r)
				t.Fatal("lost-response scenario returned an HTTP response")
			}
			lookup, err := e.request(http.MethodGet, "/v1/operations/"+key, "", "", true)
			if err != nil {
				t.Fatal(err)
			}
			body(t, lookup)
			if lookup.StatusCode != http.StatusOK {
				t.Fatalf("effect not found after lost response: %d", lookup.StatusCode)
			}
		})
	}
}

func TestEventualFakeERPLostResponseCanPrecedeLookupVisibility(t *testing.T) {
	e := newEnvironment(t)
	key := e.key()
	payload := []byte(`{"tool":"erp.create_po_eventual","payload":{"scenario":"execute_then_timeout","delay_ms":100,"visibility_delay_ms":150}}`)
	client := &http.Client{Timeout: 20 * time.Millisecond}
	req, err := http.NewRequest(http.MethodPost, e.server.URL+"/v1/execute", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	req.Header.Set("X-EACP-Tenant-ID", e.tenant.String())
	req.Header.Set("Idempotency-Key", key)
	r, err := client.Do(req)
	if err == nil {
		body(t, r)
		t.Fatal("timeout scenario returned an HTTP response")
	}
	lookup := func() int {
		t.Helper()
		r, err := e.request(http.MethodGet, "/v1/operations/"+key, "", "", true)
		if err != nil {
			t.Fatal(err)
		}
		body(t, r)
		return r.StatusCode
	}
	if got := lookup(); got != http.StatusNotFound {
		t.Fatalf("premature lookup status = %d", got)
	}
	time.Sleep(170 * time.Millisecond)
	if got := lookup(); got != http.StatusOK {
		t.Fatalf("visible lookup status = %d", got)
	}
}

// A record can stay invisible to lookup for up to ten minutes, long enough
// for a reconciler to give up and a human to resolve it (the Slice A demo).
func TestFakeERPVisibilityDelayIsBoundedAtTenMinutes(t *testing.T) {
	e := newEnvironment(t)
	for delay, want := range map[int]int{600000: http.StatusOK, 600001: http.StatusUnprocessableEntity} {
		b, _ := json.Marshal(map[string]any{"tool": "erp.create_po_eventual", "external_reference": e.key(),
			"payload": map[string]any{"amount": 42, "scenario": "delayed_visibility", "visibility_delay_ms": delay}})
		r, err := e.request(http.MethodPost, "/v1/execute", "", string(b), true)
		if err != nil {
			t.Fatal(err)
		}
		body(t, r)
		if r.StatusCode != want {
			t.Errorf("visibility_delay_ms %d: status %d, want %d", delay, r.StatusCode, want)
		}
	}
}
