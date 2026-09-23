package governance

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Binding is the complete identity of an action. The payload in this value
// is the submitted payload; an enforced payload may replace it after policy
// evaluation. Both digests cover every field, not just the JSON payload.
type Binding struct {
	TenantID          uuid.UUID       `json:"tenant"`
	AgentID           uuid.UUID       `json:"agent"`
	AgentVersionID    uuid.UUID       `json:"agent_version"`
	Subject           string          `json:"subject"`
	Operation         string          `json:"operation"`
	Target            string          `json:"target"`
	Tool              string          `json:"tool"`
	ToolSchemaVersion string          `json:"tool_schema_version"`
	Resource          string          `json:"resource"`
	Payload           json.RawMessage `json:"payload"`
}

// Digests returns SHA-256(JCS(binding)) for the submitted and enforced
// payloads. It rejects ambiguous JSON instead of hashing a lossy parse.
func Digests(b Binding, enforcedPayload json.RawMessage) (input, enforced [32]byte, err error) {
	if b.TenantID == uuid.Nil || b.AgentID == uuid.Nil || b.AgentVersionID == uuid.Nil ||
		b.Subject == "" || b.Operation == "" || b.Target == "" || b.Tool == "" ||
		b.ToolSchemaVersion == "" || b.Resource == "" {
		return input, enforced, errors.New("governance: incomplete action binding")
	}
	for _, s := range []string{b.Subject, b.Operation, b.Target, b.Tool, b.ToolSchemaVersion, b.Resource} {
		if !utf8.ValidString(s) {
			return input, enforced, errors.New("governance: invalid binding Unicode")
		}
	}
	input, err = digestBinding(b, b.Payload)
	if err != nil {
		return input, enforced, err
	}
	enforced, err = digestBinding(b, enforcedPayload)
	return input, enforced, err
}

func digestBinding(b Binding, payload json.RawMessage) ([32]byte, error) {
	var zero [32]byte
	canonicalPayload, err := canonicalize(payload)
	if err != nil {
		return zero, fmt.Errorf("governance: payload: %w", err)
	}
	b.Payload = canonicalPayload
	raw, err := json.Marshal(b)
	if err != nil {
		return zero, err
	}
	canonicalBinding, err := canonicalize(raw)
	if err != nil {
		return zero, fmt.Errorf("governance: binding: %w", err)
	}
	return sha256.Sum256(canonicalBinding), nil
}

// canonicalize implements RFC 8785 for the I-JSON subset. Object keys are
// ordered by UTF-16 code units, strings use ECMAScript JSON escaping, and
// numbers use the shortest IEEE 754 representation at ECMAScript thresholds.
func canonicalize(raw []byte) ([]byte, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("invalid UTF-8")
	}
	if err := validateSurrogates(raw); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := parseJSON(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	var out bytes.Buffer
	if err := writeCanonical(&out, v); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func parseJSON(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch v := tok.(type) {
	case json.Delim:
		switch v {
		case '{':
			m := make(map[string]any)
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyTok.(string)
				if !ok {
					return nil, errors.New("non-string JSON object key")
				}
				if _, exists := m[key]; exists {
					return nil, fmt.Errorf("duplicate JSON key %q", key)
				}
				item, err := parseJSON(dec)
				if err != nil {
					return nil, err
				}
				m[key] = item
			}
			_, err := dec.Token()
			return m, err
		case '[':
			var a []any
			for dec.More() {
				item, err := parseJSON(dec)
				if err != nil {
					return nil, err
				}
				a = append(a, item)
			}
			_, err := dec.Token()
			return a, err
		default:
			return nil, errors.New("unexpected JSON delimiter")
		}
	case nil, bool, string, json.Number:
		return v, nil
	default:
		return nil, fmt.Errorf("unexpected JSON token %T", tok)
	}
}

func writeCanonical(out *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if x {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case string:
		writeString(out, x)
	case json.Number:
		n, err := canonicalNumber(string(x))
		if err != nil {
			return err
		}
		out.WriteString(n)
	case []any:
		out.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := writeCanonical(out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.SortFunc(keys, compareUTF16)
		out.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				out.WriteByte(',')
			}
			writeString(out, k)
			out.WriteByte(':')
			if err := writeCanonical(out, x[k]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return fmt.Errorf("unsupported JSON value %T", v)
	}
	return nil
}

func compareUTF16(a, b string) int {
	aa, bb := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	return slices.Compare(aa, bb)
}

func writeString(out *bytes.Buffer, s string) {
	out.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			out.WriteByte('\\')
			out.WriteRune(r)
		case '\b':
			out.WriteString(`\b`)
		case '\t':
			out.WriteString(`\t`)
		case '\n':
			out.WriteString(`\n`)
		case '\f':
			out.WriteString(`\f`)
		case '\r':
			out.WriteString(`\r`)
		default:
			if r < 0x20 {
				fmt.Fprintf(out, `\u%04x`, r)
			} else {
				out.WriteRune(r)
			}
		}
	}
	out.WriteByte('"')
}

func canonicalNumber(raw string) (string, error) {
	// Integer tokens above the I-JSON interoperable integer range can silently
	// change meaning when parsed as float64. Require strings for such values.
	if !strings.ContainsAny(raw, ".eE") {
		if i, err := strconv.ParseInt(raw, 10, 64); err != nil || i < -9007199254740992 || i > 9007199254740992 {
			return "", errors.New("integer outside I-JSON interoperable range")
		}
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return "", errors.New("invalid IEEE 754 number")
	}
	if f == 0 {
		return "0", nil
	}
	a := math.Abs(f)
	if a >= 1e-6 && a < 1e21 {
		return strconv.FormatFloat(f, 'f', -1, 64), nil
	}
	s := strconv.FormatFloat(f, 'e', -1, 64)
	parts := strings.SplitN(s, "e", 2)
	exponent, _ := strconv.Atoi(parts[1])
	if exponent >= 0 {
		return parts[0] + "e+" + strconv.Itoa(exponent), nil
	}
	return parts[0] + "e" + strconv.Itoa(exponent), nil
}

// encoding/json replaces unpaired escaped UTF-16 surrogates with U+FFFD.
// Reject them before decoding so the signed bytes cannot silently change.
func validateSurrogates(raw []byte) error {
	for i := 0; i < len(raw); {
		if raw[i] != '"' {
			i++
			continue
		}
		i++
		for i < len(raw) && raw[i] != '"' {
			if raw[i] != '\\' {
				i++
				continue
			}
			i++
			if i >= len(raw) {
				return errors.New("unterminated JSON escape")
			}
			if raw[i] != 'u' {
				i++
				continue
			}
			if i+4 >= len(raw) {
				return errors.New("short Unicode escape")
			}
			u, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
			if err != nil {
				return err
			}
			i += 5
			if u >= 0xdc00 && u <= 0xdfff {
				return errors.New("unpaired low surrogate")
			}
			if u >= 0xd800 && u <= 0xdbff {
				if i+6 > len(raw) || raw[i] != '\\' || raw[i+1] != 'u' {
					return errors.New("unpaired high surrogate")
				}
				low, err := strconv.ParseUint(string(raw[i+2:i+6]), 16, 16)
				if err != nil || low < 0xdc00 || low > 0xdfff {
					return errors.New("unpaired high surrogate")
				}
				i += 6
			}
		}
		if i < len(raw) {
			i++
		}
	}
	return nil
}
