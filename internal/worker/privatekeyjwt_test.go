package worker_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/atipongsena/eacp/internal/logging"
	"github.com/atipongsena/eacp/internal/worker"
)

func rsaKey(t *testing.T, bits int) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func ecKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// pkcs8PEM returns key as a PKCS #8 PEM block.
func pkcs8PEM(t *testing.T, key any) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// selfSigned returns a certificate for key valid from notBefore to notAfter.
func selfSigned(t *testing.T, key crypto.Signer, notBefore, notAfter time.Time) (*x509.Certificate, string) {
	t.Helper()
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "eacp-test"},
		NotBefore: notBefore, NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return cert, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// pkjwtStore loads one oauth2 binding whose private_key_jwt object is pkjwt.
func pkjwtStore(t *testing.T, tokenURL string, c *clock, pkjwt map[string]any, opts ...worker.LoadOption) *worker.SecretStore {
	t.Helper()
	obj, _ := json.Marshal(pkjwt)
	body := fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"erp","host":"erp.internal:8443",
		"oauth2":{"token_url":%q,"client_id":"eacp-pkjwt","private_key_jwt":%s}}]}`, tenant, tokenURL, obj)
	opts = append([]worker.LoadOption{worker.AllowPlainTokenURL(), worker.WithClock(c.now)}, opts...)
	s, err := worker.LoadSecrets(secretsFile(t, body), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// decodeJWT splits a compact JWS into its header and claims.
func decodeJWT(t *testing.T, jwt string) (map[string]any, map[string]any, []string) {
	t.Helper()
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("not a compact JWS: %q", jwt)
	}
	var out [2]map[string]any
	for i := range 2 {
		b, err := base64.RawURLEncoding.DecodeString(parts[i])
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.UseNumber()
		if err := dec.Decode(&out[i]); err != nil {
			t.Fatal(err)
		}
	}
	return out[0], out[1], parts
}

func verifySignature(t *testing.T, alg string, pub crypto.PublicKey, parts []string) {
	t.Helper()
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	switch alg {
	case "RS256":
		err = rsa.VerifyPKCS1v15(pub.(*rsa.PublicKey), crypto.SHA256, sum[:], sig)
	case "PS256":
		err = rsa.VerifyPSS(pub.(*rsa.PublicKey), crypto.SHA256, sum[:], sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
	case "ES256":
		if len(sig) != 64 {
			t.Fatalf("an ES256 signature of %d bytes, want the raw 64-byte R||S", len(sig))
		}
		r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
		if !ecdsa.Verify(pub.(*ecdsa.PublicKey), sum[:], r, s) {
			err = fmt.Errorf("bad ES256 signature")
		}
	}
	if err != nil {
		t.Fatalf("%s signature does not verify: %v", alg, err)
	}
}

func TestPrivateKeyJWTSignsEachAlgorithm(t *testing.T) {
	for alg, key := range map[string]crypto.Signer{"RS256": rsaKey(t, 2048), "PS256": rsaKey(t, 2048), "ES256": ecKey(t)} {
		t.Run(alg, func(t *testing.T) {
			p := newIDP(t)
			c := &clock{t: time.Unix(1_800_000_000, 0)}
			s := pkjwtStore(t, p.srv.URL+"/token", c, map[string]any{"alg": alg, "key": pkcs8PEM(t, key)})
			if _, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, time.Minute); err != nil {
				t.Fatal(err)
			}
			form, auth := p.last()
			if form.Get("client_id") != "eacp-pkjwt" || form.Get("client_assertion_type") != jwtBearer ||
				form.Get("grant_type") != "client_credentials" || auth != [2]string{} {
				t.Fatalf("form = %v, basic auth = %q", form, auth)
			}
			header, claims, parts := decodeJWT(t, form.Get("client_assertion"))
			verifySignature(t, alg, key.Public(), parts)
			if header["alg"] != alg || header["typ"] != "JWT" {
				t.Fatalf("header = %v", header)
			}
			now := json.Number(fmt.Sprint(c.now().Unix()))
			if claims["iss"] != "eacp-pkjwt" || claims["sub"] != "eacp-pkjwt" || claims["aud"] != p.srv.URL+"/token" ||
				claims["iat"] != now || claims["nbf"] != now || claims["exp"] != json.Number(fmt.Sprint(c.now().Unix()+300)) {
				t.Fatalf("claims = %v", claims)
			}
			jti, err := base64.RawURLEncoding.DecodeString(fmt.Sprint(claims["jti"]))
			if err != nil || len(jti) != 16 {
				t.Fatalf("jti %v is not 128 random bits", claims["jti"])
			}
		})
	}
}

func TestTheHeaderNamesTheKey(t *testing.T) {
	key := rsaKey(t, 2048)
	c := &clock{t: time.Now()}
	cert, certPEM := selfSigned(t, key, c.now().Add(-time.Hour), c.now().Add(time.Hour))
	thumb := sha256.Sum256(cert.Raw)
	for name, tc := range map[string]struct {
		extra    map[string]any
		kid, x5t any
	}{
		"key id":      {map[string]any{"key_id": "k-1"}, "k-1", nil},
		"certificate": {map[string]any{"certificate": certPEM}, nil, base64.RawURLEncoding.EncodeToString(thumb[:])},
		"neither":     {map[string]any{}, nil, nil},
	} {
		t.Run(name, func(t *testing.T) {
			p := newIDP(t)
			obj := map[string]any{"alg": "PS256", "key": pkcs8PEM(t, key)}
			for k, v := range tc.extra {
				obj[k] = v
			}
			s := pkjwtStore(t, p.srv.URL, c, obj)
			if _, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, time.Minute); err != nil {
				t.Fatal(err)
			}
			form, _ := p.last()
			header, _, _ := decodeJWT(t, form.Get("client_assertion"))
			if header["kid"] != tc.kid || header["x5t#S256"] != tc.x5t {
				t.Fatalf("header = %v", header)
			}
		})
	}
}

func TestEveryMintHasANewJTI(t *testing.T) {
	p := newIDP(t)
	c := &clock{t: time.Now()}
	s := pkjwtStore(t, p.srv.URL, c, map[string]any{"alg": "ES256", "key": pkcs8PEM(t, ecKey(t))})
	ctx := context.Background()
	tok, err := s.Credential(ctx, tenant, "erp", erpEndpoint, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := p.last()
	s.Rejected(tenant, "erp", tok) // the same second: a new mint must still carry a new jti
	if _, err := s.Credential(ctx, tenant, "erp", erpEndpoint, time.Minute); err != nil {
		t.Fatal(err)
	}
	second, _ := p.last()
	_, c1, _ := decodeJWT(t, first.Get("client_assertion"))
	_, c2, _ := decodeJWT(t, second.Get("client_assertion"))
	if c1["jti"] == c2["jti"] || first.Get("client_assertion") == second.Get("client_assertion") {
		t.Fatal("two mints reused a jti")
	}
}

func TestAPrivateKeyIsRedactedButNeverAValue(t *testing.T) {
	p := newIDP(t)
	c := &clock{t: time.Now()}
	keyPEM := pkcs8PEM(t, rsaKey(t, 2048))
	set := logging.NewSecretSet()
	s := pkjwtStore(t, p.srv.URL, c, map[string]any{"alg": "RS256", "key": keyPEM}, worker.WithRedaction(set))
	body := strings.Split(keyPEM, "\n")[1] // a line of the key's base64 body
	if !strings.Contains(strings.Join(set.Values(), ","), body) {
		t.Fatal("the redaction set lacks the private key")
	}
	if strings.Contains(strings.Join(s.Values(), ","), body) {
		t.Fatal("the private key is in Values: it is never sent anywhere")
	}
	if _, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, time.Minute); err != nil {
		t.Fatal(err)
	}
	form, _ := p.last()
	jwt := form.Get("client_assertion")
	if !strings.Contains(strings.Join(set.Values(), ","), jwt) || !strings.Contains(strings.Join(s.Values(), ","), jwt) {
		t.Fatal("the assertion is not redacted and scrubbed")
	}
	c.add(6 * time.Minute)
	if strings.Contains(strings.Join(s.Values(), ","), jwt) {
		t.Fatal("an expired assertion is still in Values")
	}
}

func TestAnInlineKeyAndACRLFPEMLoad(t *testing.T) {
	p := newIDP(t)
	crlf := strings.ReplaceAll(pkcs8PEM(t, ecKey(t)), "\n", "\r\n") + "\r\n"
	s := pkjwtStore(t, p.srv.URL, &clock{t: time.Now()}, map[string]any{"alg": "ES256", "key": crlf})
	if _, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidPrivateKeyJWTEntriesRejectTheWholeFile(t *testing.T) {
	now := time.Now()
	rsa2048, p256 := rsaKey(t, 2048), ecKey(t)
	good := pkcs8PEM(t, rsa2048)
	_, otherCert := selfSigned(t, rsaKey(t, 2048), now.Add(-time.Hour), now.Add(time.Hour))
	_, edPriv, _ := ed25519GenerateKey()
	encrypted := strings.Replace(good, "-----BEGIN PRIVATE KEY-----\n",
		"-----BEGIN PRIVATE KEY-----\nProc-Type: 4,ENCRYPTED\nDEK-Info: AES-128-CBC,00000000000000000000000000000000\n\n", 1)
	keyFile := secretsFile(t, good) // any readable path will do
	cases := map[string]string{
		"unknown alg":              fmt.Sprintf(`{"alg":"HS256","key":%q}`, good),
		"ES256 with an RSA key":    fmt.Sprintf(`{"alg":"ES256","key":%q}`, good),
		"RS256 with a P-256 key":   fmt.Sprintf(`{"alg":"RS256","key":%q}`, pkcs8PEM(t, p256)),
		"RSA 1024":                 fmt.Sprintf(`{"alg":"RS256","key":%q}`, pkcs8PEM(t, rsaKey(t, 1024))),
		"Ed25519":                  fmt.Sprintf(`{"alg":"ES256","key":%q}`, pkcs8PEM(t, edPriv)),
		"encrypted PEM":            fmt.Sprintf(`{"alg":"RS256","key":%q}`, encrypted),
		"two PEM blocks":           fmt.Sprintf(`{"alg":"RS256","key":%q}`, good+good),
		"not PEM":                  `{"alg":"RS256","key":"` + canary + `"}`,
		"key and key_file":         fmt.Sprintf(`{"alg":"RS256","key":%q,"key_file":%q}`, good, keyFile),
		"no key":                   `{"alg":"RS256"}`,
		"unreadable key_file":      `{"alg":"RS256","key_file":"/absent/key.pem"}`,
		"certificate of other key": fmt.Sprintf(`{"alg":"RS256","key":%q,"certificate":%q}`, good, otherCert),
		"empty certificate":        fmt.Sprintf(`{"alg":"RS256","key":%q,"certificate":""}`, good),
		"empty certificate_file":   fmt.Sprintf(`{"alg":"RS256","key":%q,"certificate_file":%q}`, good, secretsFile(t, "")),
		"empty key_id":             fmt.Sprintf(`{"alg":"RS256","key":%q,"key_id":""}`, good),
		"key id with a space":      fmt.Sprintf(`{"alg":"RS256","key":%q,"key_id":"a b"}`, good),
		"unknown member":           fmt.Sprintf(`{"alg":"RS256","key":%q,"x5c":"x"}`, good),
	}
	for name, obj := range cases {
		body := fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"erp","host":"erp.example","oauth2":{"token_url":"https://idp.example/token","client_id":"eacp","private_key_jwt":%s}}]}`, tenant, obj)
		if _, err := worker.LoadSecrets(secretsFile(t, body)); err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), "PRIVATE KEY") || strings.Contains(err.Error(), canary) {
			t.Errorf("%s: the error repeats the key: %v", name, err)
		}
	}
	withSecret := fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"erp","host":"erp.example","oauth2":{"token_url":"https://idp.example/token","client_id":"eacp","client_secret":"s","private_key_jwt":{"alg":"RS256","key":%q}}}]}`, tenant, good)
	if _, err := worker.LoadSecrets(secretsFile(t, withSecret)); err == nil {
		t.Error("private_key_jwt beside client_secret was accepted")
	}
	fromFile := fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"erp","host":"erp.example","oauth2":{"token_url":"https://idp.example/token","client_id":"eacp","private_key_jwt":{"alg":"PS256","key_file":%q}}}]}`, tenant, keyFile)
	if _, err := worker.LoadSecrets(secretsFile(t, fromFile)); err != nil {
		t.Errorf("a key_file was refused: %v", err)
	}
}

func ed25519GenerateKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

// TestACertificateOutsideItsValidityFailsTheMintNotTheFile: a certificate
// that has expired (or is not yet valid) is a time-based condition, like an
// expired platform assertion: it must not stop every other binding at the
// worker's next start. The binding's mints fail and back off until it is
// valid; nothing is sent meanwhile (ADR-019 §3b).
func TestACertificateOutsideItsValidityFailsTheMintNotTheFile(t *testing.T) {
	for name, window := range map[string][2]time.Duration{
		"expired":       {-2 * time.Hour, -time.Hour},
		"not yet valid": {time.Hour, 2 * time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			p := newIDP(t)
			c := &clock{t: time.Now()}
			key := rsaKey(t, 2048)
			_, cert := selfSigned(t, key, c.now().Add(window[0]), c.now().Add(window[1]))
			var logs bytes.Buffer
			var mu sync.Mutex
			s := pkjwtStore(t, p.srv.URL, c, map[string]any{"alg": "PS256", "key": pkcs8PEM(t, key), "certificate": cert},
				worker.WithLogger(slog.New(slog.NewJSONHandler(syncWriter{&logs, &mu}, nil))))
			if _, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, time.Minute); !errors.Is(err, worker.ErrCredentialUnavailable) {
				t.Fatalf("err = %v, want ErrCredentialUnavailable", err)
			}
			if p.mints.Load() != 0 {
				t.Fatal("a token request was made with a certificate outside its validity")
			}
			if len(s.Available()) != 0 {
				t.Fatal("the binding did not back off")
			}
			mu.Lock()
			logged := logs.String()
			mu.Unlock()
			if !strings.Contains(logged, `"class":"certificate_not_valid"`) {
				t.Fatalf("the failure class was not logged: %s", logged)
			}
			if name == "not yet valid" { // once it is valid, the binding mints
				c.add(time.Hour + 2*time.Minute)
				if _, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, time.Minute); err != nil {
					t.Fatalf("a valid certificate still fails: %v", err)
				}
			}
		})
	}
}
