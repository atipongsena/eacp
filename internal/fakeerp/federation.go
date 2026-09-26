package fakeerp

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/url"
	"strings"
	"time"
)

// jwtBearer is the RFC 7523 client assertion type.
const jwtBearer = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"

// assertionSkew tolerates clock differences between the issuer and the ERP.
const assertionSkew = 30 * time.Second

// Federated is an OAuth client that authenticates with a JWT its platform
// issued (RFC 7523 client assertion, workload identity federation), as an
// Entra ID federated identity credential does: the assertion must be RS256,
// signed by one of Keys, and name exactly Issuer and Subject, with Audience
// among its audiences (ADR-019).
type Federated struct {
	ClientID string
	Issuer   string
	Audience string
	Subject  string
	Keys     map[string]*rsa.PublicKey // by kid
}

func (f *Federated) valid() error {
	if f.ClientID == "" || f.Issuer == "" || f.Audience == "" || f.Subject == "" || len(f.Keys) == 0 {
		return errors.New("fakeerp: a federated client needs a client id, issuer, audience, subject and keys")
	}
	return nil
}

// ParseJWKS returns the RSA keys of a JSON Web Key Set by key id. Keys of
// other types are ignored; a set without an RSA key is an error.
func ParseJWKS(raw []byte) (map[string]*rsa.PublicKey, error) {
	keys, err := ParseKeySet(raw)
	if err != nil {
		return nil, err
	}
	out := map[string]*rsa.PublicKey{}
	for _, k := range keys {
		if r, ok := k.Key.(*rsa.PublicKey); ok && k.KID != "" {
			out[k.KID] = r
		}
	}
	if len(out) == 0 {
		return nil, errors.New("fakeerp: the JWKS has no RSA key with a kid")
	}
	return out, nil
}

// verifyAssertion checks a token request authenticated by a platform-issued
// client assertion. It reports only whether the client is authenticated,
// never why: the answer to the client is invalid_client either way.
func (f *Federated) verifyAssertion(form url.Values, now time.Time) bool {
	if form.Get("client_assertion_type") != jwtBearer || form.Get("client_id") != f.ClientID {
		return false
	}
	j, ok := parseJWS(form.Get("client_assertion"))
	if !ok {
		return false
	}
	alg, _ := j.headerString("alg")
	kid, _ := j.headerString("kid")
	key := f.Keys[kid]
	if alg != "RS256" || key == nil || !verifySignature(alg, key, j) {
		return false
	}
	iss, ok1 := j.claimString("iss")
	sub, ok2 := j.claimString("sub")
	if !ok1 || !ok2 || iss != f.Issuer || sub != f.Subject || !hasAudience(j.claims["aud"], f.Audience) {
		return false
	}
	return j.timely(now, 0)
}

// jws is a compact JWS whose header and claims were decoded strictly: exact
// member names, NumericDates as JSON numbers.
type jws struct {
	header, claims map[string]json.RawMessage
	input          string // header.payload, the signed bytes
	sig            []byte
}

func parseJWS(s string) (jws, bool) {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return jws{}, false
	}
	var j jws
	for i, dst := range []*map[string]json.RawMessage{&j.header, &j.claims} {
		b, err := base64.RawURLEncoding.DecodeString(parts[i])
		if err != nil || json.Unmarshal(b, dst) != nil || *dst == nil {
			return jws{}, false
		}
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) == 0 {
		return jws{}, false
	}
	j.input, j.sig = parts[0]+"."+parts[1], sig
	return j, true
}

func stringMember(m map[string]json.RawMessage, name string) (string, bool) {
	var v string
	raw, ok := m[name]
	if !ok || json.Unmarshal(raw, &v) != nil {
		return "", false
	}
	return v, true
}

func (j jws) headerString(name string) (string, bool) { return stringMember(j.header, name) }
func (j jws) claimString(name string) (string, bool)  { return stringMember(j.claims, name) }

// numericDate reads a NumericDate claim in whole seconds; a quoted number is
// refused.
func (j jws) numericDate(name string) (time.Time, bool, bool) {
	raw, present := j.claims[name]
	if !present {
		return time.Time{}, false, true
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] == '"' {
		return time.Time{}, true, false
	}
	var n json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&n) != nil {
		return time.Time{}, true, false
	}
	i, err := n.Int64()
	if err != nil {
		return time.Time{}, true, false
	}
	return time.Unix(i, 0), true, true
}

// timely checks exp (required, in the future, and no further ahead than
// maxAhead when it is positive), and nbf and iat when present, with skew.
func (j jws) timely(now time.Time, maxAhead time.Duration) bool {
	exp, present, ok := j.numericDate("exp")
	if !present || !ok || !now.Before(exp.Add(assertionSkew)) {
		return false
	}
	if maxAhead > 0 && exp.After(now.Add(maxAhead+assertionSkew)) {
		return false
	}
	for _, name := range []string{"nbf", "iat"} {
		at, present, ok := j.numericDate(name)
		if present && (!ok || at.After(now.Add(assertionSkew))) {
			return false
		}
	}
	return true
}

// hasAudience reports whether aud, a string or an array of strings, holds want.
func hasAudience(aud json.RawMessage, want string) bool {
	var one string
	if json.Unmarshal(aud, &one) == nil {
		return one == want
	}
	var many []string
	if json.Unmarshal(aud, &many) != nil {
		return false
	}
	for _, a := range many {
		if a == want {
			return true
		}
	}
	return false
}

// verifySignature checks j's signature under alg with key; the key type must
// be the algorithm's (RSA for RS256 and PS256, ECDSA P-256 for ES256).
func verifySignature(alg string, key crypto.PublicKey, j jws) bool {
	sum := sha256.Sum256([]byte(j.input))
	switch k := key.(type) {
	case *rsa.PublicKey:
		switch alg {
		case "RS256":
			return rsa.VerifyPKCS1v15(k, crypto.SHA256, sum[:], j.sig) == nil
		case "PS256":
			return rsa.VerifyPSS(k, crypto.SHA256, sum[:], j.sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}) == nil
		}
	case *ecdsa.PublicKey:
		if alg == "ES256" && len(j.sig) == 64 {
			return ecdsa.Verify(k, sum[:], new(big.Int).SetBytes(j.sig[:32]), new(big.Int).SetBytes(j.sig[32:]))
		}
	}
	return false
}
