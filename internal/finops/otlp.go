package finops

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// OTLP/HTTP JSON encoding of ExportTraceServiceRequest (OTLP specification,
// research/REFERENCES.md): lowerCamelCase keys, hex trace and span ids,
// 64-bit integers as decimal strings (a JSON number is accepted too), and
// unknown fields ignored. Only the fields EACP reads are declared.
type otlpRequest struct {
	ResourceSpans []struct {
		ScopeSpans []struct {
			Spans []otlpSpan `json:"spans"`
		} `json:"scopeSpans"`
	} `json:"resourceSpans"`
}

type otlpSpan struct {
	TraceID      string          `json:"traceId"`
	SpanID       string          `json:"spanId"`
	EndTimeNanos json.RawMessage `json:"endTimeUnixNano"`
	Attributes   []otlpKeyValue  `json:"attributes"`
}

type otlpKeyValue struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

type otlpValue struct {
	StringValue *string         `json:"stringValue"`
	IntValue    json.RawMessage `json:"intValue"`
}

// GenAI semantic-convention attributes (open-telemetry/semantic-conventions-genai
// at 8ffdf568, status Development).
const (
	attrOperation     = "gen_ai.operation.name"
	attrProvider      = "gen_ai.provider.name"
	attrRequestModel  = "gen_ai.request.model"
	attrResponseModel = "gen_ai.response.model"
	attrInputTokens   = "gen_ai.usage.input_tokens"
	attrOutputTokens  = "gen_ai.usage.output_tokens"
	attrCacheRead     = "gen_ai.usage.cache_read.input_tokens"

	// UnknownProvider records usage whose span names no provider; it stays
	// unpriced unless an admin prices it.
	UnknownProvider = "unknown"

	// MaxSpans bounds the usage spans of one export request.
	MaxSpans = 1000
	// maxTokens matches the table's CHECK.
	maxTokens = 1_000_000_000_000
)

var (
	tracePattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	spanPattern  = regexp.MustCompile(`^[0-9a-f]{16}$`)
	namePattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

// Span is the usage an OTel GenAI span reports. The agent is never read
// from the span: the ingest binds it from the key.
type Span struct {
	TraceID         string
	SpanID          string
	Operation       string
	Provider        string
	Model           string // response model, else request model; "" when neither
	InputTokens     int64
	CacheReadTokens int64
	OutputTokens    int64
	ObservedAt      time.Time // the span's end
}

// Parsed is the usage found in one export request.
type Parsed struct {
	Spans []Span
	// Rejected counts usage spans that were malformed; Message describes the
	// first. Spans without GenAI usage are not counted: they are accepted
	// and ignored.
	Rejected int
	Message  string
}

func (p *Parsed) reject(format string, args ...any) {
	if p.Rejected == 0 {
		p.Message = fmt.Sprintf(format, args...)
	}
	p.Rejected++
}

// ParseTraces reads an OTLP/HTTP JSON trace export and returns its usage
// spans. It fails only when the body is not an export request.
func ParseTraces(body []byte) (Parsed, error) {
	var req otlpRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&req); err != nil {
		return Parsed{}, fmt.Errorf("not an OTLP JSON trace export: %v", err)
	}
	var out Parsed
	for _, rs := range req.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			for _, s := range ss.Spans {
				span, usage, err := parseSpan(s)
				switch {
				case !usage:
				case err != nil:
					out.reject("span %s/%s: %v", clip(s.TraceID), clip(s.SpanID), err)
				case len(out.Spans) >= MaxSpans:
					out.reject("more than %d usage spans in one request", MaxSpans)
				default:
					out.Spans = append(out.Spans, span)
				}
			}
		}
	}
	return out, nil
}

// parseSpan returns usage = false for a span that is not a GenAI usage span
// (no operation name or no usage attribute).
func parseSpan(s otlpSpan) (Span, bool, error) {
	var sp Span
	var op *string
	tokens := map[string]int64{}
	var tokenErr error
	var model, requestModel string
	for _, kv := range s.Attributes {
		switch kv.Key {
		case attrOperation:
			op = kv.Value.StringValue
		case attrProvider:
			if kv.Value.StringValue != nil {
				sp.Provider = *kv.Value.StringValue
			}
		case attrResponseModel:
			if kv.Value.StringValue != nil {
				model = *kv.Value.StringValue
			}
		case attrRequestModel:
			if kv.Value.StringValue != nil {
				requestModel = *kv.Value.StringValue
			}
		case attrInputTokens, attrOutputTokens, attrCacheRead:
			n, err := intValue(kv.Value.IntValue)
			if err == nil && (n < 0 || n > maxTokens) {
				err = fmt.Errorf("%d is outside 0..10^12", n)
			}
			if err != nil && tokenErr == nil {
				tokenErr = fmt.Errorf("%s: %v", kv.Key, err)
			}
			tokens[kv.Key] = n
		}
	}
	if op == nil || len(tokens) == 0 {
		return Span{}, false, nil
	}
	if tokenErr != nil {
		return Span{}, true, tokenErr
	}
	sp.Operation = *op
	if !namePattern.MatchString(sp.Operation) {
		return Span{}, true, fmt.Errorf("%s is not a well-formed operation name", attrOperation)
	}
	if sp.Provider == "" {
		sp.Provider = UnknownProvider
	} else if !namePattern.MatchString(sp.Provider) {
		return Span{}, true, fmt.Errorf("%s is not a well-formed provider name", attrProvider)
	}
	if model == "" {
		model = requestModel
	}
	if len(model) > 256 || strings.ContainsFunc(model, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return Span{}, true, fmt.Errorf("the model name is too long or contains control characters")
	}
	sp.Model = model
	sp.TraceID, sp.SpanID = strings.ToLower(s.TraceID), strings.ToLower(s.SpanID)
	if !tracePattern.MatchString(sp.TraceID) || strings.Trim(sp.TraceID, "0") == "" {
		return Span{}, true, fmt.Errorf("traceId must be 32 hex digits, not all zero")
	}
	if !spanPattern.MatchString(sp.SpanID) || strings.Trim(sp.SpanID, "0") == "" {
		return Span{}, true, fmt.Errorf("spanId must be 16 hex digits, not all zero")
	}
	end, err := intValue(s.EndTimeNanos)
	if err != nil || end <= 0 {
		return Span{}, true, fmt.Errorf("endTimeUnixNano must be a positive integer")
	}
	sp.ObservedAt = time.Unix(0, end).UTC()
	sp.InputTokens, sp.OutputTokens, sp.CacheReadTokens = tokens[attrInputTokens], tokens[attrOutputTokens], tokens[attrCacheRead]
	return sp, true, nil
}

// intValue decodes an OTLP int64: a decimal string or a JSON integer.
func intValue(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, fmt.Errorf("an integer value is required")
	}
	text := string(raw)
	if raw[0] == '"' {
		if err := json.Unmarshal(raw, &text); err != nil {
			return 0, err
		}
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not an integer", clip(text))
	}
	return n, nil
}

func clip(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}
