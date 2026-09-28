package fakeerp_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/atipongsena/eacp/internal/fakeerp"
)

const (
	spiffeClient   = "eacp-worker-spiffe"
	spiffeIssuer   = "https://spire.eacp.test"
	spiffeTokenAud = "fakeerp-token"
	spiffeAPIAud   = "fakeerp-api"
	spiffeWorker   = "spiffe://eacp.test/ns/eacp/sa/eacp-worker"
)

// spiffeBundle is a SPIFFE bundle with k's RSA and EC keys as JWT
// authorities and other's RSA key as an X.509 authority (never a JWT key).
func spiffeBundle(k, other *keyPair) []byte {
	enc := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	b, _ := json.Marshal(map[string]any{"spiffe_sequence": 1, "keys": []map[string]any{
		{"use": "jwt-svid", "kty": "RSA", "kid": k.kid + "-rsa", "n": enc(k.rsa.N.Bytes()),
			"e": enc(big.NewInt(int64(k.rsa.E)).Bytes())},
		{"use": "jwt-svid", "kty": "EC", "crv": "P-256", "kid": k.kid + "-ec",
			"x": enc(k.ec.X.FillBytes(make([]byte, 32))), "y": enc(k.ec.Y.FillBytes(make([]byte, 32)))},
		{"use": "x509-svid", "kty": "RSA", "kid": "x509", "n": enc(other.rsa.N.Bytes()),
			"e": enc(big.NewInt(int64(other.rsa.E)).Bytes()), "x5c": []string{"MIIB"}},
	}})
	return b
}

func svidClaims(aud string) map[string]any {
	now := time.Now().Unix()
	return map[string]any{"iss": spiffeIssuer, "sub": spiffeWorker, "aud": []string{aud}, "iat": now, "exp": now + 300}
}

func spiffeERP(t *testing.T) (*httptest.Server, *keyPair, *keyPair) {
	t.Helper()
	k, other := newKeyPair(t, "spire"), newKeyPair(t, "x509")
	keys, err := fakeerp.ParseSPIFFEBundle(spiffeBundle(k, other))
	if err != nil {
		t.Fatal(err)
	}
	h, err := fakeerp.NewWithOptions(credential, filepath.Join(t.TempDir(), "erp.log"), fakeerp.Options{
		SPIFFEClient: &fakeerp.SPIFFEClient{ClientID: spiffeClient, Issuer: spiffeIssuer, Audience: spiffeTokenAud,
			Subject: spiffeWorker, Keys: keys},
		SPIFFEBearer: &fakeerp.SPIFFEBearer{Audience: spiffeAPIAud, Subject: spiffeWorker, Keys: keys}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, k, other
}

func spiffeForm(jwt string) url.Values {
	return url.Values{"grant_type": {"client_credentials"}, "client_id": {spiffeClient},
		"client_assertion_type": {jwtBearer}, "client_assertion": {jwt}}
}

func TestParseSPIFFEBundleKeepsOnlyJWTAuthorities(t *testing.T) {
	keys, err := fakeerp.ParseSPIFFEBundle(spiffeBundle(newKeyPair(t, "spire"), newKeyPair(t, "x509")))
	if err != nil {
		t.Fatal(err)
	}
	var kids []string
	for _, k := range keys {
		kids = append(kids, k.KID)
	}
	slices.Sort(kids)
	if !slices.Equal(kids, []string{"spire-ec", "spire-rsa"}) {
		t.Fatalf("kids = %v", kids)
	}
}

func TestABundleWithoutJWTAuthoritiesIsRefused(t *testing.T) {
	x509Only := strings.ReplaceAll(string(spiffeBundle(newKeyPair(t, "a"), newKeyPair(t, "b"))), `"jwt-svid"`, `"x509-svid"`)
	for _, raw := range []string{x509Only, `{"keys":[]}`, `not json`} {
		if _, err := fakeerp.ParseSPIFFEBundle([]byte(raw)); err == nil {
			t.Fatalf("accepted %.40s", raw)
		}
	}
}

func TestASPIFFEAssertionMintsATokenForTheSPIFFEClient(t *testing.T) {
	srv, k, _ := spiffeERP(t)
	for _, h := range []map[string]any{{"alg": "ES256", "kid": "spire-ec", "typ": "JWT"}, {"alg": "RS256", "kid": "spire-rsa"}} {
		jwt := k.sign(h, svidClaims(spiffeTokenAud))
		status, got := postToken(t, srv, spiffeForm(jwt), false)
		if status != 200 || got["access_token"] == nil {
			t.Fatalf("%v: %d %v", h["alg"], status, got)
		}
		if code := executeWith(t, srv, got["access_token"].(string)); code != 200 {
			t.Fatalf("the minted token: %d", code)
		}
	}
	entries, raw := auditOf(t, srv)
	issued := 0
	for _, e := range entries {
		if e.Outcome == "token_issued" && e.Principal == "oauth:"+spiffeClient {
			issued++
		}
	}
	if issued != 2 || !strings.Contains(raw, `"assertion_sha256"`) {
		t.Fatalf("%d issuances to %s", issued, spiffeClient)
	}
}

func TestTheSPIFFEClientRefusesBadSVIDs(t *testing.T) {
	srv, k, other := spiffeERP(t)
	ec := map[string]any{"alg": "ES256", "kid": "spire-ec"}
	with := func(name string, v any) map[string]any {
		c := svidClaims(spiffeTokenAud)
		if v == nil {
			delete(c, name)
		} else {
			c[name] = v
		}
		return c
	}
	now := time.Now().Unix()
	for name, jwt := range map[string]string{
		"unknown kid":        k.sign(map[string]any{"alg": "ES256", "kid": "nope"}, svidClaims(spiffeTokenAud)),
		"x509 authority":     other.sign(map[string]any{"alg": "RS256", "kid": "x509"}, svidClaims(spiffeTokenAud)),
		"alg none":           k.signRaw("none", b64(map[string]any{"alg": "none", "kid": "spire-ec"}), b64(svidClaims(spiffeTokenAud))),
		"alg HS256":          k.sign(map[string]any{"alg": "HS256", "kid": "spire-rsa"}, svidClaims(spiffeTokenAud)),
		"alg PS256":          k.sign(map[string]any{"alg": "PS256", "kid": "spire-rsa"}, svidClaims(spiffeTokenAud)),
		"ES256 on RSA key":   k.sign(map[string]any{"alg": "ES256", "kid": "spire-rsa"}, svidClaims(spiffeTokenAud)),
		"bad signature":      k.sign(ec, svidClaims(spiffeTokenAud))[:40] + "x" + k.sign(ec, svidClaims(spiffeTokenAud))[41:],
		"wrong issuer":       k.sign(ec, with("iss", "https://other")),
		"no issuer":          k.sign(ec, with("iss", nil)),
		"wrong subject":      k.sign(ec, with("sub", "spiffe://eacp.test/ns/eacp/sa/other")),
		"wrong audience":     k.sign(ec, with("aud", []string{spiffeAPIAud})),
		"expired":            k.sign(ec, with("exp", now-60)),
		"not yet valid":      k.sign(ec, with("nbf", now+120)),
		"a quoted exp":       k.sign(ec, with("exp", "9999999999")),
		"another client_id":  "",
		"not a JWT":          "not-a-jwt",
		"the bearer's claim": k.sign(ec, svidClaims(spiffeAPIAud)),
	} {
		form := spiffeForm(jwt)
		if name == "another client_id" {
			form = spiffeForm(k.sign(ec, svidClaims(spiffeTokenAud)))
			form.Set("client_id", "someone")
		}
		if status, got := postToken(t, srv, form, false); status != 401 || got["error"] != "invalid_client" {
			t.Errorf("%s: %d %v", name, status, got)
		}
	}
}

func TestAnSVIDBearerAuthorisesTheERPAPI(t *testing.T) {
	srv, k, _ := spiffeERP(t)
	svid := k.sign(map[string]any{"alg": "ES256", "kid": "spire-ec"}, svidClaims(spiffeAPIAud))
	if code := executeWith(t, srv, svid); code != 200 {
		t.Fatalf("execute with an SVID: %d", code)
	}
	_, raw := auditOf(t, srv)
	var entries []struct {
		Principal  string `json:"principal"`
		Path       string `json:"path"`
		SVIDSHA256 string `json:"svid_sha256"`
	}
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(svid))
	found := false
	for _, e := range entries {
		if e.Path == "/v1/execute" {
			found = e.Principal == "spiffe:"+spiffeWorker && e.SVIDSHA256 == hex.EncodeToString(sum[:])
		}
	}
	if !found || strings.Contains(raw, svid) {
		t.Fatalf("audit = %s", raw)
	}
}

func TestAnSVIDBearerMustMatch(t *testing.T) {
	srv, k, other := spiffeERP(t)
	ec := map[string]any{"alg": "ES256", "kid": "spire-ec"}
	with := func(name string, v any) map[string]any {
		c := svidClaims(spiffeAPIAud)
		c[name] = v
		return c
	}
	now := time.Now().Unix()
	for name, jwt := range map[string]string{
		"unknown kid":    k.sign(map[string]any{"alg": "ES256", "kid": "nope"}, svidClaims(spiffeAPIAud)),
		"x509 authority": other.sign(map[string]any{"alg": "RS256", "kid": "x509"}, svidClaims(spiffeAPIAud)),
		"alg PS256":      k.sign(map[string]any{"alg": "PS256", "kid": "spire-rsa"}, svidClaims(spiffeAPIAud)),
		"bad signature":  k.sign(ec, svidClaims(spiffeAPIAud))[:40] + "x" + k.sign(ec, svidClaims(spiffeAPIAud))[41:],
		"wrong subject":  k.sign(ec, with("sub", "spiffe://eacp.test/ns/eacp/sa/other")),
		"wrong audience": k.sign(ec, with("aud", []string{spiffeTokenAud})),
		"expired":        k.sign(ec, with("exp", now-60)),
		"not yet valid":  k.sign(ec, with("nbf", now+120)),
	} {
		if code := executeWith(t, srv, jwt); code != 401 {
			t.Errorf("%s: %d", name, code)
		}
	}
}

func TestSPIFFEOptionsFailClosed(t *testing.T) {
	keys, err := fakeerp.ParseSPIFFEBundle(spiffeBundle(newKeyPair(t, "a"), newKeyPair(t, "b")))
	if err != nil {
		t.Fatal(err)
	}
	full := fakeerp.SPIFFEClient{ClientID: spiffeClient, Issuer: spiffeIssuer, Audience: spiffeTokenAud,
		Subject: spiffeWorker, Keys: keys}
	bearer := fakeerp.SPIFFEBearer{Audience: spiffeAPIAud, Subject: spiffeWorker, Keys: keys}
	path := filepath.Join(t.TempDir(), "erp.log")
	for name, o := range map[string]fakeerp.Options{
		"client without issuer":   {SPIFFEClient: &fakeerp.SPIFFEClient{ClientID: spiffeClient, Audience: "a", Subject: "s", Keys: keys}},
		"client without keys":     {SPIFFEClient: &fakeerp.SPIFFEClient{ClientID: spiffeClient, Issuer: "i", Audience: "a", Subject: "s"}},
		"bearer without audience": {SPIFFEBearer: &fakeerp.SPIFFEBearer{Subject: "s", Keys: keys}},
		"bearer without keys":     {SPIFFEBearer: &fakeerp.SPIFFEBearer{Audience: "a", Subject: "s"}},
		"client id shared with the key client": {SPIFFEClient: &full,
			KeyClient: &fakeerp.KeyClient{ClientID: spiffeClient, Audience: "a", Keys: keys}},
	} {
		if _, err := fakeerp.NewWithOptions(credential, path, o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := fakeerp.NewWithOptions(credential, path, fakeerp.Options{SPIFFEBearer: &bearer}); err != nil {
		t.Fatalf("a bearer-only ERP: %v", err)
	}
	_ = os.Remove(path)
}
