package fakeerp

import (
	"bytes"
	"crypto"
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
	var set struct {
		Keys []struct {
			Kty, Kid, N, E string
		} `json:"keys"`
	}
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil, errors.New("fakeerp: the JWKS is not JSON")
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		n, errN := base64.RawURLEncoding.DecodeString(k.N)
		e, errE := base64.RawURLEncoding.DecodeString(k.E)
		if errN != nil || errE != nil || len(n) == 0 || len(e) == 0 || len(e) > 4 {
			return nil, errors.New("fakeerp: a JWKS RSA key is malformed")
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if len(keys) == 0 {
		return nil, errors.New("fakeerp: the JWKS has no RSA key")
	}
	return keys, nil
}

// verifyAssertion checks a token request authenticated by a client
// assertion. It reports only whether the client is authenticated, never why:
// the answer to the client is invalid_client either way.
func (f *Federated) verifyAssertion(form url.Values, now time.Time) bool {
	if form.Get("client_assertion_type") != jwtBearer || form.Get("client_id") != f.ClientID {
		return false
	}
	parts := strings.Split(form.Get("client_assertion"), ".")
	if len(parts) != 3 {
		return false
	}
	var header struct{ Alg, Kid string }
	if !decodeSegment(parts[0], &header) || header.Alg != "RS256" {
		return false
	}
	key := f.Keys[header.Kid]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if key == nil || err != nil {
		return false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig) != nil {
		return false
	}
	var claims struct {
		Iss, Sub      string
		Aud           json.RawMessage
		Exp, Nbf, Iat *json.Number
	}
	if !decodeSegment(parts[1], &claims) || claims.Iss != f.Issuer || claims.Sub != f.Subject ||
		!hasAudience(claims.Aud, f.Audience) {
		return false
	}
	exp, ok := unix(claims.Exp)
	if !ok || !now.Before(exp.Add(assertionSkew)) {
		return false
	}
	for _, t := range []*json.Number{claims.Nbf, claims.Iat} {
		if t == nil {
			continue
		}
		at, ok := unix(t)
		if !ok || at.After(now.Add(assertionSkew)) {
			return false
		}
	}
	return true
}

func decodeSegment(s string, v any) bool {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	return dec.Decode(v) == nil
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

func unix(n *json.Number) (time.Time, bool) {
	if n == nil {
		return time.Time{}, false
	}
	i, err := n.Int64()
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(i, 0), true
}
