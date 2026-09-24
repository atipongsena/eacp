package finops_test

import (
	"strings"
	"testing"
	"time"

	"eacp/internal/finops"
)

// span builds an OTLP JSON span with the given attributes (raw JSON values).
func span(trace, spanID, end string, attrs ...string) string {
	return `{"traceId":"` + trace + `","spanId":"` + spanID + `","name":"chat gpt-x","kind":3,
		"startTimeUnixNano":"1","endTimeUnixNano":` + end + `,"attributes":[` + strings.Join(attrs, ",") + `],
		"status":{}}`
}

func str(k, v string) string { return `{"key":"` + k + `","value":{"stringValue":"` + v + `"}}` }
func num(k, v string) string { return `{"key":"` + k + `","value":{"intValue":` + v + `}}` }

func export(spans ...string) []byte {
	return []byte(`{"resourceSpans":[{"resource":{"attributes":[` + str("service.name", "buyer") + `]},
		"schemaUrl":"https://opentelemetry.io/schemas/1.30.0",
		"scopeSpans":[{"scope":{"name":"openai-instrumentation"},"spans":[` + strings.Join(spans, ",") + `]}]}]}`)
}

const (
	tr  = "5B8EFFF798038103D269B633813FC60C"
	end = `"1758844800000000000"`
)

func TestParseTracesReadsGenAIUsage(t *testing.T) {
	body := export(
		// A chat span: int64 as strings, the response model wins, hex is lower-cased.
		span(tr, "EEE19B7EC3C1B174", end, str("gen_ai.operation.name", "chat"), str("gen_ai.provider.name", "openai"),
			str("gen_ai.request.model", "gpt-4"), str("gen_ai.response.model", "gpt-4-0613"),
			num("gen_ai.usage.input_tokens", `"1000"`), num("gen_ai.usage.output_tokens", `"500"`),
			num("gen_ai.usage.cache_read.input_tokens", `"400"`)),
		// JSON numbers, the request model as fallback and no provider.
		span("5b8efff798038103d269b633813fc60d", "eee19b7ec3c1b175", `1758844800000000001`,
			str("gen_ai.operation.name", "embeddings"), str("gen_ai.request.model", "text-embedding-3-small"),
			num("gen_ai.usage.input_tokens", `42`)),
		// Not usage: no operation, or no usage attribute. Accepted and ignored.
		span(tr, "eee19b7ec3c1b176", end, num("gen_ai.usage.input_tokens", `"1"`)),
		span(tr, "eee19b7ec3c1b177", end, str("gen_ai.operation.name", "execute_tool")),
		span(tr, "eee19b7ec3c1b178", end, str("http.method", "GET")),
	)
	p, err := finops.ParseTraces(body)
	if err != nil {
		t.Fatal(err)
	}
	if p.Rejected != 0 || len(p.Spans) != 2 {
		t.Fatalf("parsed = %+v", p)
	}
	want := finops.Span{TraceID: strings.ToLower(tr), SpanID: "eee19b7ec3c1b174", Operation: "chat", Provider: "openai",
		Model: "gpt-4-0613", InputTokens: 1000, OutputTokens: 500, CacheReadTokens: 400,
		ObservedAt: time.Unix(0, 1758844800000000000).UTC()}
	if p.Spans[0] != want {
		t.Fatalf("chat span = %+v", p.Spans[0])
	}
	if s := p.Spans[1]; s.Provider != finops.UnknownProvider || s.Model != "text-embedding-3-small" ||
		s.InputTokens != 42 || s.OutputTokens != 0 || s.ObservedAt.UnixNano() != 1758844800000000001 {
		t.Fatalf("embeddings span = %+v", s)
	}
}

func TestParseTracesRejectsMalformedUsageSpans(t *testing.T) {
	op := str("gen_ai.operation.name", "chat")
	in := num("gen_ai.usage.input_tokens", `"10"`)
	for name, s := range map[string]string{
		"short trace id":        span("abc", "eee19b7ec3c1b174", end, op, in),
		"zero trace id":         span(strings.Repeat("0", 32), "eee19b7ec3c1b174", end, op, in),
		"non-hex span id":       span(tr, "zzzzzzzzzzzzzzzz", end, op, in),
		"zero span id":          span(tr, strings.Repeat("0", 16), end, op, in),
		"no end time":           span(tr, "eee19b7ec3c1b174", `null`, op, in),
		"zero end time":         span(tr, "eee19b7ec3c1b174", `"0"`, op, in),
		"negative tokens":       span(tr, "eee19b7ec3c1b174", end, op, num("gen_ai.usage.input_tokens", `"-1"`)),
		"too many tokens":       span(tr, "eee19b7ec3c1b174", end, op, num("gen_ai.usage.output_tokens", `"1000000000001"`)),
		"fractional tokens":     span(tr, "eee19b7ec3c1b174", end, op, num("gen_ai.usage.input_tokens", `1.5`)),
		"tokens as a string":    span(tr, "eee19b7ec3c1b174", end, op, str("gen_ai.usage.input_tokens", "10")),
		"bad operation":         span(tr, "eee19b7ec3c1b174", end, str("gen_ai.operation.name", "Chat Completion"), in),
		"bad provider":          span(tr, "eee19b7ec3c1b174", end, op, in, str("gen_ai.provider.name", "Open AI")),
		"control char in model": span(tr, "eee19b7ec3c1b174", end, op, in, str("gen_ai.request.model", `gpt\u0000x`)),
	} {
		p, err := finops.ParseTraces(export(s))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if p.Rejected != 1 || len(p.Spans) != 0 || p.Message == "" {
			t.Errorf("%s: parsed = %+v", name, p)
		}
	}
	for _, body := range []string{``, `[]`, `{"resourceSpans":{}}`, `{"resourceSpans":[`} {
		if _, err := finops.ParseTraces([]byte(body)); err == nil {
			t.Errorf("%q parsed", body)
		}
	}
}

func TestParseTracesBoundsOneRequest(t *testing.T) {
	spans := make([]string, 0, finops.MaxSpans+2)
	for i := range finops.MaxSpans + 2 {
		id := strings.Repeat("0", 12) + hex4(i+1)
		spans = append(spans, span(tr, id, end, str("gen_ai.operation.name", "chat"), num("gen_ai.usage.input_tokens", `1`)))
	}
	p, err := finops.ParseTraces(export(spans...))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Spans) != finops.MaxSpans || p.Rejected != 2 {
		t.Fatalf("spans = %d, rejected = %d", len(p.Spans), p.Rejected)
	}
}

func hex4(n int) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[n>>12&15], digits[n>>8&15], digits[n>>4&15], digits[n&15]})
}
