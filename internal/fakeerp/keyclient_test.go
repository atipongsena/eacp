package fakeerp_test

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"eacp/internal/fakeerp"
)

const (
	keyClient   = "eacp-worker-pkjwt"
	keyAudience = "http://fakeerp:8090/oauth/token"
)

// keyPair signs test assertions with any header, by the algorithm the
// header names (RS256, PS256 or ES256); another alg signs RS256.
type keyPair struct {
	rsa *rsa.PrivateKey
	ec  *ecdsa.PrivateKey
	kid string
	x5t string
}

func newKeyPair(t *testing.T, kid string) *keyPair {
	t.Helper()
	r, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	e, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(kid)) // stands in for a certificate thumbprint
	return &keyPair{rsa: r, ec: e, kid: kid, x5t: base64.RawURLEncoding.EncodeToString(sum[:])}
}

func b64(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

// signRaw signs pre-encoded header and payload segments.
func (k *keyPair) signRaw(alg, header, payload string) string {
	input := header + "." + payload
	sum := sha256.Sum256([]byte(input))
	var sig []byte
	switch alg {
	case "PS256":
		sig, _ = rsa.SignPSS(rand.Reader, k.rsa, crypto.SHA256, sum[:], &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
	case "ES256":
		r, s, _ := ecdsa.Sign(rand.Reader, k.ec, sum[:])
		sig = make([]byte, 64)
		r.FillBytes(sig[:32])
		s.FillBytes(sig[32:])
	default:
		sig, _ = rsa.SignPKCS1v15(rand.Reader, k.rsa, crypto.SHA256, sum[:])
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (k *keyPair) sign(header, claims map[string]any) string {
	alg, _ := header["alg"].(string)
	return k.signRaw(alg, b64(header), b64(claims))
}

// jwks publishes the RSA key under kid "<kid>-rsa" and x5t#S256, the EC key
// under kid "<kid>-ec", plus an EC P-384 key that must be ignored.
func (k *keyPair) jwks() []byte {
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	enc := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	b, _ := json.Marshal(map[string]any{"keys": []map[string]string{
		{"kty": "RSA", "kid": k.kid + "-rsa", "x5t#S256": k.x5t, "n": enc(k.rsa.N.Bytes()),
			"e": enc(big.NewInt(int64(k.rsa.E)).Bytes())},
		{"kty": "EC", "crv": "P-256", "kid": k.kid + "-ec", "x": enc(k.ec.X.FillBytes(make([]byte, 32))),
			"y": enc(k.ec.Y.FillBytes(make([]byte, 32)))},
		{"kty": "EC", "crv": "P-384", "kid": "p384", "x": enc(p384.X.FillBytes(make([]byte, 48))),
			"y": enc(p384.Y.FillBytes(make([]byte, 48)))},
		{"kty": "RSA", "n": enc(k.rsa.N.Bytes()), "e": "AQAB"}, // neither kid nor x5t#S256: ignored
	}})
	return b
}

func keyClaims() map[string]any {
	now := time.Now().Unix()
	return map[string]any{"iss": keyClient, "sub": keyClient, "aud": keyAudience, "jti": randomJTI(),
		"iat": now, "nbf": now, "exp": now + 300}
}

func randomJTI() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func keyForm(jwt string) url.Values {
	return url.Values{"grant_type": {"client_credentials"}, "client_id": {keyClient},
		"client_assertion_type": {jwtBearer}, "client_assertion": {jwt}}
}

// keyERP serves a Fake ERP whose only OAuth client is the key client, over
// the operation log at path (a restart reopens the same log).
func keyERP(t *testing.T, k *keyPair, path string) *httptest.Server {
	t.Helper()
	keys, err := fakeerp.ParseKeySet(k.jwks())
	if err != nil {
		t.Fatal(err)
	}
	h, err := fakeerp.NewWithOptions(credential, path, fakeerp.Options{
		KeyClient: &fakeerp.KeyClient{ClientID: keyClient, Audience: keyAudience, Keys: keys}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func TestAKeyClientAssertionMintsAToken(t *testing.T) {
	k := newKeyPair(t, "k1")
	for name, header := range map[string]map[string]any{
		"RS256 by kid":              {"alg": "RS256", "typ": "JWT", "kid": "k1-rsa"},
		"PS256 by x5t#S256 (Entra)": {"alg": "PS256", "typ": "JWT", "x5t#S256": k.x5t},
		"ES256 by kid":              {"alg": "ES256", "typ": "JWT", "kid": "k1-ec"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := keyERP(t, k, filepath.Join(t.TempDir(), "erp.log"))
			claims := keyClaims()
			status, got := postToken(t, srv, keyForm(k.sign(header, claims)), false)
			if status != 200 || got["token_type"] != "Bearer" {
				t.Fatalf("status %d, %v", status, got)
			}
			if code := executeWith(t, srv, got["access_token"].(string)); code != 200 {
				t.Fatalf("the minted token = %d", code)
			}
			_, raw := auditOf(t, srv)
			var entries []map[string]any
			_ = json.Unmarshal([]byte(raw), &entries)
			if entries[0]["principal"] != "oauth:"+keyClient || entries[0]["assertion_jti"] != claims["jti"] {
				t.Fatalf("issuance audit = %v", entries[0])
			}
		})
	}
}

func TestKeyClientAssertionsAreRefused(t *testing.T) {
	k := newKeyPair(t, "k1")
	other := newKeyPair(t, "k1")
	srv := keyERP(t, k, filepath.Join(t.TempDir(), "erp.log"))
	rs := map[string]any{"alg": "RS256", "typ": "JWT", "kid": "k1-rsa"}
	now := time.Now().Unix()
	with := func(key string, v any) string {
		c := keyClaims()
		if v == nil {
			delete(c, key)
		} else {
			c[key] = v
		}
		return k.sign(rs, c)
	}
	quotedExp := func() string {
		c := keyClaims()
		c["exp"] = "9999999999"
		return k.sign(rs, c)
	}()
	upperISS := func() string {
		c := keyClaims()
		c["ISS"] = c["iss"]
		delete(c, "iss")
		return k.sign(rs, c)
	}()
	for name, jwt := range map[string]string{
		"alg none":           k.sign(map[string]any{"alg": "none", "kid": "k1-rsa"}, keyClaims()),
		"alg HS256":          k.sign(map[string]any{"alg": "HS256", "kid": "k1-rsa"}, keyClaims()),
		"ES256 over RSA key": k.sign(map[string]any{"alg": "ES256", "kid": "k1-rsa"}, keyClaims()),
		"RS256 over EC key":  k.sign(map[string]any{"alg": "RS256", "kid": "k1-ec"}, keyClaims()),
		"unknown kid":        k.sign(map[string]any{"alg": "RS256", "kid": "nope"}, keyClaims()),
		"unknown thumbprint": k.sign(map[string]any{"alg": "PS256", "x5t#S256": "AAAA"}, keyClaims()),
		"another key":        other.sign(rs, keyClaims()),
		"iss is not sub":     with("iss", "someone"),
		"wrong audience":     with("aud", "https://evil.test/token"),
		"expired":            with("exp", now-60),
		"exp beyond an hour": with("exp", now+2*3600),
		"nbf in the future":  with("nbf", now+120),
		"iat in the future":  with("iat", now+120),
		"no jti":             with("jti", nil),
		"exp as a string":    quotedExp,
		"ISS instead of iss": upperISS,
		"malformed":          "a.b.c",
	} {
		if status, got := postToken(t, srv, keyForm(jwt), false); status != 401 || got["error"] != "invalid_client" {
			t.Errorf("%s: status %d, %v", name, status, got)
		}
	}
}

func TestAJTIIsSingleUse(t *testing.T) {
	k := newKeyPair(t, "k1")
	path := filepath.Join(t.TempDir(), "erp.log")
	srv := keyERP(t, k, path)
	jwt := k.sign(map[string]any{"alg": "ES256", "kid": "k1-ec"}, keyClaims())
	if status, _ := postToken(t, srv, keyForm(jwt), false); status != 200 {
		t.Fatalf("first use = %d", status)
	}
	if status, _ := postToken(t, srv, keyForm(jwt), false); status != 401 {
		t.Fatalf("a replayed assertion = %d", status)
	}
	srv.Close()
	again := keyERP(t, k, path) // a restart rebuilds the replay record from the log
	if status, _ := postToken(t, again, keyForm(jwt), false); status != 401 {
		t.Fatalf("a replay after a restart = %d", status)
	}
	fresh := k.sign(map[string]any{"alg": "ES256", "kid": "k1-ec"}, keyClaims())
	if status, _ := postToken(t, again, keyForm(fresh), false); status != 200 {
		t.Fatalf("a new assertion after a restart = %d", status)
	}
}

func TestParseKeySet(t *testing.T) {
	k := newKeyPair(t, "k1")
	keys, err := fakeerp.ParseKeySet(k.jwks())
	if err != nil || len(keys) != 2 {
		t.Fatalf("keys = %+v, err = %v; want the RSA and the P-256 key", keys, err)
	}
	for _, raw := range [][]byte{[]byte(`{}`), []byte(`x`), []byte(`{"keys":[{"kty":"oct","kid":"a","k":"AAAA"}]}`)} {
		if _, err := fakeerp.ParseKeySet(raw); err == nil {
			t.Errorf("%s: accepted", raw)
		}
	}
}

func TestKeyClientOptionsFailClosed(t *testing.T) {
	k := newKeyPair(t, "k1")
	keys, _ := fakeerp.ParseKeySet(k.jwks())
	path := filepath.Join(t.TempDir(), "erp.log")
	for name, c := range map[string]*fakeerp.KeyClient{
		"no client id": {Audience: keyAudience, Keys: keys},
		"no audience":  {ClientID: keyClient, Keys: keys},
		"no keys":      {ClientID: keyClient, Audience: keyAudience},
	} {
		if _, err := fakeerp.NewWithOptions(credential, path, fakeerp.Options{KeyClient: c}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
