package fakeerp

import (
	"encoding/json"
	"errors"
	"net/url"
	"time"
)

// ParseSPIFFEBundle returns the JWT authorities of a SPIFFE bundle (a JWKS
// whose keys carry "use": "jwt-svid" or "x509-svid", as `spire-server bundle
// show -format spiffe` prints it): RSA or P-256 keys with a kid.
func ParseSPIFFEBundle(raw []byte) ([]PublicKey, error) {
	var bundle struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(raw, &bundle); err != nil {
		return nil, errors.New("fakeerp: the SPIFFE bundle is not JSON")
	}
	var jwt []json.RawMessage
	for _, k := range bundle.Keys {
		var use struct {
			Use string `json:"use"`
		}
		if json.Unmarshal(k, &use) == nil && use.Use == "jwt-svid" {
			jwt = append(jwt, k)
		}
	}
	set, _ := json.Marshal(map[string]any{"keys": jwt})
	keys, err := ParseKeySet(set)
	if err != nil {
		return nil, errors.New("fakeerp: the SPIFFE bundle has no usable JWT authority")
	}
	out := keys[:0]
	for _, k := range keys {
		if k.KID != "" {
			out = append(out, k)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("fakeerp: the SPIFFE bundle has no JWT authority with a kid")
	}
	return out, nil
}

// SPIFFEClient is an OAuth client that authenticates with a JWT-SVID as its
// RFC 7523 client assertion (ADR-019 §3d): RS256 or ES256 by one of Keys
// (the trust bundle's JWT authorities, by kid), naming exactly Issuer (the
// SPIRE server's jwt_issuer) and Subject (the worker's SPIFFE ID), with
// Audience among its audiences.
type SPIFFEClient struct {
	ClientID string
	Issuer   string
	Audience string
	Subject  string
	Keys     []PublicKey
}

func (c *SPIFFEClient) valid() error {
	if c.ClientID == "" || c.Issuer == "" || c.Audience == "" || c.Subject == "" || len(c.Keys) == 0 {
		return errors.New("fakeerp: a SPIFFE client needs a client id, issuer, audience, subject and keys")
	}
	return nil
}

func (c *SPIFFEClient) verifyAssertion(form url.Values, now time.Time) bool {
	if form.Get("client_assertion_type") != jwtBearer || form.Get("client_id") != c.ClientID {
		return false
	}
	return verifySVID(form.Get("client_assertion"), c.Keys, c.Issuer, c.Subject, c.Audience, now)
}

// SPIFFEBearer accepts a JWT-SVID as the Bearer credential of the ERP API:
// verified as SPIFFEClient does, without an issuer. A JWT-SVID can be
// replayed until it expires; its audience limits where.
type SPIFFEBearer struct {
	Audience string
	Subject  string
	Keys     []PublicKey
}

func (b *SPIFFEBearer) valid() error {
	if b.Audience == "" || b.Subject == "" || len(b.Keys) == 0 {
		return errors.New("fakeerp: a SPIFFE bearer needs an audience, subject and keys")
	}
	return nil
}

func (b *SPIFFEBearer) verify(token string, now time.Time) bool {
	return verifySVID(token, b.Keys, "", b.Subject, b.Audience, now)
}

// verifySVID checks a JWT-SVID: RS256 or ES256 by the key its kid names, the
// exact subject (and issuer, unless empty), the audience, exp and nbf.
func verifySVID(token string, keys []PublicKey, issuer, subject, audience string, now time.Time) bool {
	j, ok := parseJWS(token)
	if !ok {
		return false
	}
	alg, _ := j.headerString("alg")
	kid, _ := j.headerString("kid")
	if (alg != "RS256" && alg != "ES256") || kid == "" {
		return false
	}
	var key PublicKey
	for _, k := range keys {
		if k.KID == kid {
			key = k
		}
	}
	if key.Key == nil || !verifySignature(alg, key.Key, j) {
		return false
	}
	sub, ok := j.claimString("sub")
	if !ok || sub != subject || !hasAudience(j.claims["aud"], audience) {
		return false
	}
	if iss, ok := j.claimString("iss"); issuer != "" && (!ok || iss != issuer) {
		return false
	}
	return j.timely(now, 0)
}
