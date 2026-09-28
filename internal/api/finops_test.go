package api_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/atipongsena/eacp/internal/finops"
	"github.com/atipongsena/eacp/internal/identity"
	"github.com/atipongsena/eacp/internal/storage/pgtest"
)

// otlp posts an OTLP export with the agent key.
func (h *harness) otlp(key, contentType, encoding string, body []byte) (int, map[string]any) {
	h.t.Helper()
	req, _ := http.NewRequest("POST", h.srv.URL+"/v1/agent/otlp/v1/traces", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if encoding != "" {
		req.Header.Set("Content-Encoding", encoding)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		h.t.Fatalf("response Content-Type = %q", ct)
	}
	return resp.StatusCode, out
}

func otlpExport(spans ...string) []byte {
	return []byte(`{"resourceSpans":[{"resource":{},"scopeSpans":[{"scope":{},"spans":[` +
		strings.Join(spans, ",") + `]}]}]}`)
}

func usageSpan(spanID string, end time.Time, model string, input int) string {
	return `{"traceId":"5b8efff798038103d269b633813fc60c","spanId":"` + spanID + `","endTimeUnixNano":"` +
		strconv.FormatInt(end.UnixNano(), 10) + `","attributes":[
		{"key":"gen_ai.operation.name","value":{"stringValue":"chat"}},
		{"key":"gen_ai.provider.name","value":{"stringValue":"openai"}},
		{"key":"gen_ai.request.model","value":{"stringValue":"` + model + `"}},
		{"key":"gen_ai.usage.input_tokens","value":{"intValue":"` + strconv.Itoa(input) + `"}},
		{"key":"gen_ai.usage.output_tokens","value":{"intValue":"0"}}]}`
}

func TestOTLPReceiverBindsUsageToTheAgentKey(t *testing.T) {
	h := newHarness(t)
	a := h.f.ActiveAgent(t, "buyer")
	key := h.issue(identity.KindAgent, a.Version, "erin", "rita")
	now := time.Now()

	code, body := h.otlp(key, "application/json", "", otlpExport(usageSpan("eee19b7ec3c1b174", now, "gpt-x", 1000)))
	h.want(200, code, body)
	if len(body) != 0 {
		t.Fatalf("full success sets no partialSuccess: %v", body)
	}
	// One malformed and one too-old span, gzip-encoded: partial success.
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	zw.Write(otlpExport(usageSpan("eee19b7ec3c1b175", now, "gpt-x", 1), usageSpan("zz", now, "gpt-x", 1),
		usageSpan("eee19b7ec3c1b176", now.Add(-8*24*time.Hour), "gpt-x", 1)))
	zw.Close()
	code, body = h.otlp(key, "application/json; charset=utf-8", "gzip", zipped.Bytes())
	h.want(200, code, body)
	ps, _ := body["partialSuccess"].(map[string]any)
	if ps["rejectedSpans"] != "2" || ps["errorMessage"] == "" {
		t.Fatalf("partial success = %v", body)
	}
	// A retried export is not counted twice.
	code, body = h.otlp(key, "application/json", "", otlpExport(usageSpan("eee19b7ec3c1b174", now, "gpt-x", 1000)))
	h.want(200, code, body)

	code, body = h.otlp(key, "application/x-protobuf", "", []byte{0x0a})
	h.want(415, code, body)
	code, body = h.otlp(key, "application/json", "br", []byte("{}"))
	h.want(415, code, body)
	code, body = h.otlp(key, "application/json", "", []byte("not json"))
	h.want(400, code, body)
	code, body = h.otlp(key, "application/json", "", bytes.Repeat([]byte(" "), 4<<20+1))
	h.want(413, code, body)
	// A principal key is not an agent.
	code, body = h.otlp(h.keys["alice"], "application/json", "", otlpExport())
	h.want(403, code, body)

	code, body = h.as("audra", "GET", "/v1/finops/usage", nil)
	h.want(200, code, body)
	if usage, _ := body["usage"].([]any); len(usage) != 2 {
		t.Fatalf("usage = %v", body)
	}
	code, body = h.as("carol", "GET", "/v1/finops/usage", nil)
	h.want(403, code, body)
}

func TestFinOpsAPI(t *testing.T) {
	h := newHarness(t)
	a := h.f.ActiveAgent(t, "buyer")
	key := h.issue(identity.KindAgent, a.Version, "erin", "rita")

	price := map[string]any{"provider": "openai", "model": "gpt-x", "unit": "USD", "input_per_mtok": "2",
		"output_per_mtok": "8", "reason": "list price"}
	code, body := h.as("otto", "POST", "/v1/finops/prices", price)
	h.want(403, code, body)
	code, body = h.as("alice", "POST", "/v1/finops/prices", price)
	h.want(201, code, body)
	price["effective_from"] = "2020-01-01T00:00:00Z"
	code, body = h.as("alice", "POST", "/v1/finops/prices", price)
	h.want(400, code, body)
	code, body = h.as("audra", "GET", "/v1/finops/prices", nil)
	h.want(200, code, body)

	// Priced from now on.
	code, body = h.otlp(key, "application/json", "", otlpExport(usageSpan("eee19b7ec3c1b174", time.Now(), "gpt-x", 500000)))
	h.want(200, code, body)

	lines := map[string]any{"lines": []map[string]any{{"external_id": "inv-1", "agent_id": a.Agent, "provider": "openai",
		"model": "gpt-x", "cost": "4", "unit": "USD", "observed_at": time.Now().Add(-time.Minute)}}}
	code, body = h.as("otto", "POST", "/v1/finops/billing", lines)
	h.want(403, code, body)
	code, body = h.as("alice", "POST", "/v1/finops/billing", lines)
	h.want(200, code, body)
	if body["inserted"] != float64(1) {
		t.Fatalf("import = %v", body)
	}

	code, body = h.as("otto", "GET", "/v1/finops/chargeback?group_by=agent", nil)
	h.want(200, code, body)
	groups, _ := body["groups"].([]any)
	if len(groups) != 1 {
		t.Fatalf("chargeback = %v", body)
	}
	spend, _ := groups[0].(map[string]any)["spend"].([]any)
	if s, _ := spend[0].(map[string]any); s["llm_reported"] != 1.0 || s["llm_billed"] != 4.0 || s["llm_effective"] != 4.0 {
		t.Fatalf("spend = %v", spend)
	}
	code, body = h.as("otto", "GET", "/v1/finops/chargeback?group_by=planet", nil)
	h.want(400, code, body)
	code, body = h.as("otto", "GET", "/v1/finops/chargeback?from=yesterday", nil)
	h.want(400, code, body)

	code, body = h.as("alice", "POST", "/v1/budgets", map[string]any{"name": "buyer-usd", "unit": "USD", "agent_id": a.Agent})
	h.want(201, code, body)
	account := str(body, "id")
	code, body = h.as("otto", "PUT", "/v1/finops/soft-limits/"+account, map[string]any{"monthly_limit": "5", "reason": "plan"})
	h.want(403, code, body)
	code, body = h.as("alice", "PUT", "/v1/finops/soft-limits/"+account, map[string]any{"monthly_limit": "5", "reason": "plan"})
	h.want(200, code, body)
	if body["month_to_date"] != 4.0 {
		t.Fatalf("soft limit = %v", body)
	}
	code, body = h.as("audra", "GET", "/v1/finops/soft-limits", nil)
	h.want(200, code, body)

	// The dashboard shows spend; alerts come from the evaluator.
	code, body = h.as("audra", "GET", "/v1/finops/dashboard", nil)
	h.want(200, code, body)
	units, _ := body["units"].([]any)
	if len(units) != 1 || units[0].(map[string]any)["today"] != 4.0 {
		t.Fatalf("dashboard = %v", body)
	}
	code, body = h.as("carol", "GET", "/v1/finops/dashboard", nil)
	h.want(403, code, body)
	code, body = h.as("otto", "GET", "/v1/finops/alerts?open=true", nil)
	h.want(200, code, body)
	if alerts, _ := body["alerts"].([]any); len(alerts) != 0 {
		t.Fatalf("alerts before evaluation = %v", body)
	}
	code, body = h.as("otto", "GET", "/v1/finops/alerts?open=maybe", nil)
	h.want(400, code, body)
	code, body = h.as("otto", "POST", "/v1/finops/alerts/"+account+"/ack", map[string]any{"reason": "x"})
	h.want(404, code, body)
}

// Invariant 8 over the FinOps API: a tenant-B admin and operator see none
// of tenant A's spend, and tenant A's agents, accounts and alerts do not
// exist for them.
func TestFinOpsOfOtherTenantsAreNotFound(t *testing.T) {
	h := newHarness(t)
	a := h.f.ActiveAgent(t, "buyer")
	key := h.issue(identity.KindAgent, a.Version, "erin", "rita")
	code, body := h.otlp(key, "application/json", "", otlpExport(usageSpan("eee19b7ec3c1b174", time.Now(), "mystery", 10)))
	h.want(200, code, body)
	code, body = h.as("alice", "POST", "/v1/budgets", map[string]any{"name": "buyer-usd", "unit": "USD", "agent_id": a.Agent})
	h.want(201, code, body)
	account := str(body, "id")
	if n, err := finops.New(h.f.App).Evaluate(context.Background(), h.f.Tenant); err != nil || n != 1 {
		t.Fatalf("evaluate = %d %v", n, err)
	}
	code, body = h.as("otto", "GET", "/v1/finops/alerts", nil)
	h.want(200, code, body)
	alerts, _ := body["alerts"].([]any)
	if len(alerts) != 1 {
		t.Fatalf("tenant A alerts = %v", body)
	}
	alert, _ := alerts[0].(map[string]any)["id"].(string)

	eve := h.principalIn(pgtest.TenantB, "eve", "admin", "operator")
	for _, path := range []string{"/v1/finops/usage", "/v1/finops/alerts", "/v1/finops/soft-limits", "/v1/finops/prices"} {
		code, body = h.do(eve, "GET", path, nil)
		h.want(200, code, body)
		for k, v := range body {
			if list, _ := v.([]any); len(list) != 0 {
				t.Fatalf("%s %s = %v", path, k, list)
			}
		}
	}
	code, body = h.do(eve, "GET", "/v1/finops/chargeback", nil)
	h.want(200, code, body)
	if groups, _ := body["groups"].([]any); len(groups) != 0 {
		t.Fatalf("tenant B chargeback = %v", body)
	}
	code, body = h.do(eve, "GET", "/v1/finops/dashboard", nil)
	h.want(200, code, body)
	if units, _ := body["units"].([]any); len(units) != 0 || body["unpriced_tokens_today"] != 0.0 {
		t.Fatalf("tenant B dashboard = %v", body)
	}
	code, body = h.do(eve, "POST", "/v1/finops/alerts/"+alert+"/ack", map[string]any{"reason": "b"})
	h.want(404, code, body)
	code, body = h.do(eve, "PUT", "/v1/finops/soft-limits/"+account, map[string]any{"monthly_limit": "1", "reason": "b"})
	h.want(404, code, body)
	code, body = h.do(eve, "POST", "/v1/finops/billing", map[string]any{"lines": []map[string]any{{"external_id": "b-1",
		"agent_id": a.Agent, "provider": "openai", "cost": "1", "unit": "USD", "observed_at": time.Now()}}})
	h.want(404, code, body)
}
