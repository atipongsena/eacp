package studioruntime

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// ErrResultUnavailable is a placeholder whose value is missing: the run
// fails result_unavailable and the literal placeholder is never sent.
var ErrResultUnavailable = errors.New("result_unavailable")

var placeholder = regexp.MustCompile(`\{\{([^{}]*)\}\}`)

// Env is what placeholders refer to: the run's inputs and the outputs of
// its earlier steps, by step id.
type Env struct {
	Inputs  map[string]string
	Outputs map[string]any
}

// Render substitutes the placeholders in value, a decoded JSON value. A
// string that is exactly one placeholder becomes the referenced value with
// its JSON type; otherwise each placeholder is replaced by its value, a
// string as is and anything else as JSON. PostgreSQL validated the paths
// when the definition was saved; a value missing now is
// ErrResultUnavailable.
func Render(value any, env Env) (any, error) {
	switch v := value.(type) {
	case string:
		if m := placeholder.FindStringSubmatch(v); m != nil && m[0] == v {
			return env.lookup(m[1])
		}
		return renderText(v, env)
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, x := range v {
			r, err := Render(x, env)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			r, err := Render(x, env)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	default:
		return v, nil
	}
}

// renderText replaces every placeholder in s by its value as text.
func renderText(s string, env Env) (string, error) {
	var failed error
	out := placeholder.ReplaceAllStringFunc(s, func(m string) string {
		v, err := env.lookup(m[2 : len(m)-2])
		if err != nil {
			failed = err
			return ""
		}
		if str, ok := v.(string); ok {
			return str
		}
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(v); err != nil {
			failed = ErrResultUnavailable
			return ""
		}
		return strings.TrimSuffix(b.String(), "\n")
	})
	if failed != nil {
		return "", failed
	}
	return out, nil
}

// lookup resolves inputs.<name> or steps.<id>.output[.key...]; an array is
// indexed by a decimal key.
func (e Env) lookup(path string) (any, error) {
	parts := strings.Split(path, ".")
	switch {
	case len(parts) == 2 && parts[0] == "inputs":
		v, ok := e.Inputs[parts[1]]
		if !ok {
			return nil, ErrResultUnavailable
		}
		return v, nil
	case len(parts) >= 3 && parts[0] == "steps" && parts[2] == "output":
		v, ok := e.Outputs[parts[1]]
		if !ok {
			return nil, ErrResultUnavailable
		}
		for _, k := range parts[3:] {
			switch c := v.(type) {
			case map[string]any:
				if v, ok = c[k]; !ok {
					return nil, ErrResultUnavailable
				}
			case []any:
				i, err := strconv.Atoi(k)
				if err != nil || i < 0 || i >= len(c) {
					return nil, ErrResultUnavailable
				}
				v = c[i]
			default:
				return nil, ErrResultUnavailable
			}
		}
		if v == nil {
			return nil, ErrResultUnavailable
		}
		return v, nil
	}
	return nil, ErrResultUnavailable
}

// decodeJSON decodes raw keeping numbers exact.
func decodeJSON(raw []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}
