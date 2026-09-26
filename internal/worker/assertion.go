package worker

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"math"
	"os"
	"strings"
	"time"
)

// maxAssertion bounds a client assertion file (a Kubernetes projected
// service-account token is about 1 KiB).
const maxAssertion = 16 << 10

// readAssertion reads a platform-issued client assertion (RFC 7523): a
// compact JWS whose payload has a numeric exp. The worker never checks its
// signature; only the IdP judges it. It returns the assertion and its expiry,
// or a failure class: assertion_unreadable, assertion_invalid, or
// assertion_expired when less than the token request timeout remains after
// now. A zero now skips the expiry check (at load, the kubelet may be about
// to rotate the file).
func readAssertion(path string, now time.Time) (Secret, time.Time, string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Secret{}, time.Time{}, "assertion_unreadable"
	}
	v := strings.TrimRight(string(raw), "\r\n")
	if v == "" || len(v) > maxAssertion {
		return Secret{}, time.Time{}, "assertion_unreadable"
	}
	exp, ok := assertionExpiry(v)
	if !ok {
		return Secret{}, time.Time{}, "assertion_invalid"
	}
	if !now.IsZero() && exp.Before(now.Add(tokenRequestTimeout)) {
		return Secret{}, time.Time{}, "assertion_expired"
	}
	return Secret{v}, exp, ""
}

// assertionExpiry parses a compact JWS far enough to read its exp: three
// non-empty unpadded base64url segments, a JSON object header and payload,
// and a numeric exp in whole seconds.
func assertionExpiry(jws string) (time.Time, bool) {
	parts := strings.Split(jws, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	var objects [2]map[string]json.RawMessage
	for i, p := range parts {
		if p == "" {
			return time.Time{}, false
		}
		b, err := base64.RawURLEncoding.DecodeString(p)
		if err != nil {
			return time.Time{}, false
		}
		if i < 2 && json.Unmarshal(b, &objects[i]) != nil {
			return time.Time{}, false
		}
		if i < 2 && objects[i] == nil { // the literal null
			return time.Time{}, false
		}
	}
	raw := bytes.TrimSpace(objects[1]["exp"])
	if len(raw) == 0 || raw[0] == '"' { // json.Number would accept a quoted number
		return time.Time{}, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var n json.Number
	if dec.Decode(&n) != nil {
		return time.Time{}, false
	}
	f, err := n.Float64()
	if err != nil || f != math.Trunc(f) || f < 0 || f > 1<<53 {
		return time.Time{}, false
	}
	return time.Unix(int64(f), 0), true
}
