package fakeerp

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/url"
	"time"
)

// maxAssertionAhead: Okta refuses a private_key_jwt assertion whose exp is
// more than an hour ahead; so does the Fake ERP.
const maxAssertionAhead = time.Hour

// PublicKey is one verification key of a JWKS, found by its key id or its
// certificate's SHA-256 thumbprint (x5t#S256, as Entra ID sends).
type PublicKey struct {
	KID     string
	X5TS256 string
	Key     crypto.PublicKey // *rsa.PublicKey or *ecdsa.PublicKey (P-256)
}

// ParseKeySet reads the RSA and EC P-256 keys of a JSON Web Key Set that
// carry a kid or an x5t#S256. Other and malformed keys are ignored (RFC 7517
// §5); a set without a usable key is an error.
func ParseKeySet(raw []byte) ([]PublicKey, error) {
	var set struct {
		Keys []struct {
			Kty     string `json:"kty"`
			Kid     string `json:"kid"`
			X5TS256 string `json:"x5t#S256"`
			N       string `json:"n"`
			E       string `json:"e"`
			Crv     string `json:"crv"`
			X       string `json:"x"`
			Y       string `json:"y"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil, errors.New("fakeerp: the JWKS is not JSON")
	}
	var out []PublicKey
	for _, k := range set.Keys {
		if k.Kid == "" && k.X5TS256 == "" {
			continue
		}
		var key crypto.PublicKey
		switch {
		case k.Kty == "RSA":
			n, errN := base64.RawURLEncoding.DecodeString(k.N)
			e, errE := base64.RawURLEncoding.DecodeString(k.E)
			if errN != nil || errE != nil || len(n) == 0 || len(e) == 0 || len(e) > 4 {
				continue
			}
			key = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		case k.Kty == "EC" && k.Crv == "P-256":
			x, errX := base64.RawURLEncoding.DecodeString(k.X)
			y, errY := base64.RawURLEncoding.DecodeString(k.Y)
			if errX != nil || errY != nil || len(x) != 32 || len(y) != 32 {
				continue
			}
			pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
			if !pub.Curve.IsOnCurve(pub.X, pub.Y) { //nolint:staticcheck // a plain curve check of a demo JWKS
				continue
			}
			key = pub
		default:
			continue
		}
		out = append(out, PublicKey{KID: k.Kid, X5TS256: k.X5TS256, Key: key})
	}
	if len(out) == 0 {
		return nil, errors.New("fakeerp: the JWKS has no usable key")
	}
	return out, nil
}

// KeyClient is an OAuth client that authenticates with assertions it signs
// with its own key (private_key_jwt, OpenID Connect Core §9, RFC 7523):
// iss and sub are its id, aud is Audience (the token endpoint URL), and each
// jti is accepted once (ADR-019 §3b).
type KeyClient struct {
	ClientID string
	Audience string
	Keys     []PublicKey
}

func (c *KeyClient) valid() error {
	if c.ClientID == "" || c.Audience == "" || len(c.Keys) == 0 {
		return errors.New("fakeerp: a key client needs a client id, an audience and keys")
	}
	return nil
}

func (c *KeyClient) key(kid, x5t string) crypto.PublicKey {
	for _, k := range c.Keys {
		if kid != "" && k.KID == kid {
			return k.Key
		}
	}
	for _, k := range c.Keys {
		if x5t != "" && k.X5TS256 == x5t {
			return k.Key
		}
	}
	return nil
}

// verifyAssertion checks a private_key_jwt token request and returns the
// assertion's jti; the caller refuses a jti it has seen.
func (c *KeyClient) verifyAssertion(form url.Values, now time.Time) (string, bool) {
	if form.Get("client_assertion_type") != jwtBearer || form.Get("client_id") != c.ClientID {
		return "", false
	}
	j, ok := parseJWS(form.Get("client_assertion"))
	if !ok {
		return "", false
	}
	alg, _ := j.headerString("alg")
	kid, _ := j.headerString("kid")
	x5t, _ := j.headerString("x5t#S256")
	key := c.key(kid, x5t)
	if key == nil || !verifySignature(alg, key, j) {
		return "", false
	}
	iss, ok1 := j.claimString("iss")
	sub, ok2 := j.claimString("sub")
	jti, ok3 := j.claimString("jti")
	if !ok1 || !ok2 || !ok3 || iss != c.ClientID || sub != c.ClientID || jti == "" ||
		!hasAudience(j.claims["aud"], c.Audience) || !j.timely(now, maxAssertionAhead) {
		return "", false
	}
	return jti, true
}
