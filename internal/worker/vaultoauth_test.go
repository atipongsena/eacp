package worker

import (
	"context"
	"crypto"
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
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/atipongsena/eacp/internal/jwttest"
	"github.com/atipongsena/eacp/internal/logging"
)

// fakeIDP is a token endpoint that accepts Basic auth with secret, or a
// client assertion signed by pub (PS256) whose header names x5t.
type fakeIDP struct {
	srv    *httptest.Server
	mu     sync.Mutex
	secret string
	pub    *rsa.PublicKey
	x5t    string
	mints  int
	seen   []string   // the client secrets or x5t values presented, in order
	form   url.Values // the last request's form
}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	p := &fakeIDP{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		_ = r.ParseForm()
		p.form = r.PostForm
		ok := false
		if a := r.PostForm.Get("client_assertion"); a != "" {
			parts := strings.Split(a, ".")
			if len(parts) == 3 && p.pub != nil {
				h, _ := base64.RawURLEncoding.DecodeString(parts[0])
				var header map[string]any
				_ = json.Unmarshal(h, &header)
				sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
				sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
				ok = rsa.VerifyPSS(p.pub, crypto.SHA256, sum[:], sig,
					&rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}) == nil && header["x5t#S256"] == p.x5t
				p.seen = append(p.seen, fmt.Sprint(header["x5t#S256"]))
			}
		} else if _, secret, has := r.BasicAuth(); has {
			secret, _ = url.QueryUnescape(secret)
			p.seen = append(p.seen, secret)
			ok = secret == p.secret
		}
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
			return
		}
		p.mints++
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("tok-%d", p.mints),
			"token_type": "Bearer", "expires_in": 600, "issued_token_type": "urn:ietf:params:oauth:token-type:access_token"})
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakeIDP) set(fn func(p *fakeIDP)) { p.mu.Lock(); defer p.mu.Unlock(); fn(p) }

func newRSA(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	return k, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// certFor returns a self-signed certificate PEM for k and its x5t#S256.
func certFor(t *testing.T, k *rsa.PrivateKey, now time.Time) (string, string) {
	t.Helper()
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "eacp"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), base64.RawURLEncoding.EncodeToString(sum[:])
}

func oauthVaultStore(t *testing.T, f *fakeVault, idp *fakeIDP, c *vclock, set *logging.SecretSet, oauth string) *SecretStore {
	t.Helper()
	body := vaultFile(t, f.srv.URL, 30, fmt.Sprintf(`"oauth2":{"token_url":%q,"client_id":"eacp-worker",%s}`, idp.srv.URL, oauth))
	opts := []LoadOption{AllowPlainTokenURL(), WithClock(c.now)}
	if set != nil {
		opts = append(opts, WithRedaction(set))
	}
	s, err := LoadSecrets(writeFile(t, "secrets.json", body), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAnOAuthClientSecretComesFromVault(t *testing.T) {
	f, idp := newFakeVault(t), newFakeIDP(t)
	f.put("secret/data/eacp/idp", map[string]any{"client_secret": "cs1-" + vaultCanary})
	idp.set(func(p *fakeIDP) { p.secret = "cs1-" + vaultCanary })
	c := &vclock{t: time.Now()}
	s := oauthVaultStore(t, f, idp, c, nil, `"client_secret_vault":{"path":"github.com/atipongsena/eacp/idp","key":"client_secret"}`)
	ctx := context.Background()
	tok, err := s.Credential(ctx, vaultTenant, "erp", vaultEndpoint, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(s.Values(), "cs1-"+vaultCanary) {
		t.Fatal("the client secret from Vault is not scrubbed")
	}
	// The IdP rotates the client secret; Vault holds the new one.
	f.put("secret/data/eacp/idp", map[string]any{"client_secret": "cs2-" + vaultCanary})
	idp.set(func(p *fakeIDP) { p.secret = "cs2-" + vaultCanary })
	s.Rejected(vaultTenant, "erp", tok)
	if _, err := s.Credential(ctx, vaultTenant, "erp", vaultEndpoint, time.Minute); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("the cached old secret minted: %v", err)
	}
	c.add(time.Second) // the back-off; the failed mint dropped the cached secret
	if _, err := s.Credential(ctx, vaultTenant, "erp", vaultEndpoint, time.Minute); err != nil {
		t.Fatalf("the rotated secret was not read: %v", err)
	}
	idp.mu.Lock()
	defer idp.mu.Unlock()
	if want := []string{"cs1-" + vaultCanary, "cs1-" + vaultCanary, "cs2-" + vaultCanary}; !slices.Equal(idp.seen, want) {
		t.Fatalf("the IdP saw %v, want %v", idp.seen, want)
	}
}

func TestAPrivateKeyComesFromVault(t *testing.T) {
	f, idp := newFakeVault(t), newFakeIDP(t)
	c := &vclock{t: time.Now()}
	k1, pem1 := newRSA(t)
	cert1, x5t1 := certFor(t, k1, c.now())
	f.put("secret/data/eacp/key", map[string]any{"pem": pem1, "cert": cert1})
	idp.set(func(p *fakeIDP) { p.pub, p.x5t = &k1.PublicKey, x5t1 })
	set := logging.NewSecretSet()
	s := oauthVaultStore(t, f, idp, c, set, `"private_key_jwt":{"alg":"PS256",
		"key_vault":{"path":"github.com/atipongsena/eacp/key","key":"pem"},"certificate_vault":{"path":"github.com/atipongsena/eacp/key","key":"cert"}}`)
	ctx := context.Background()
	tok, err := s.Credential(ctx, vaultTenant, "erp", vaultEndpoint, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(s.Values(), pem1) {
		t.Fatal("the private key is in Values()")
	}
	redacted := set.Values()
	if line := strings.Split(pem1, "\n")[1]; !slices.Contains(redacted, pem1) || !slices.Contains(redacted, line) {
		t.Fatal("the private key from Vault is not redacted, line by line")
	}
	// Rotate the key and certificate in Vault.
	k2, pem2 := newRSA(t)
	cert2, x5t2 := certFor(t, k2, c.now())
	f.put("secret/data/eacp/key", map[string]any{"pem": pem2, "cert": cert2})
	idp.set(func(p *fakeIDP) { p.pub, p.x5t = &k2.PublicKey, x5t2 })
	c.add(31 * time.Second)
	s.Rejected(vaultTenant, "erp", tok)
	if tok, err = s.Credential(ctx, vaultTenant, "erp", vaultEndpoint, time.Minute); err != nil {
		t.Fatalf("the rotated key was not used: %v", err)
	}
	// A bad PEM in Vault fails the mint and backs off.
	f.put("secret/data/eacp/key", map[string]any{"pem": "not a pem", "cert": cert2})
	c.add(31 * time.Second)
	s.Rejected(vaultTenant, "erp", tok)
	if _, err := s.Credential(ctx, vaultTenant, "erp", vaultEndpoint, time.Minute); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("a bad PEM from Vault minted: %v", err)
	}
	if len(s.Available()) != 0 {
		t.Fatal("the binding did not back off")
	}
	idp.mu.Lock()
	defer idp.mu.Unlock()
	if !slices.Equal(idp.seen, []string{x5t1, x5t2}) {
		t.Fatalf("the IdP saw x5t %v", idp.seen)
	}
}

func TestInvalidOAuthVaultEntriesRejectTheWholeFile(t *testing.T) {
	_, keyPEM := newRSA(t)
	ref := `{"path":"github.com/atipongsena/eacp/x","key":"k"}`
	for name, oauth := range map[string]string{
		"client_secret and client_secret_vault":   `"client_secret":"s","client_secret_vault":` + ref,
		"client_secret_vault and private_key_jwt": `"client_secret_vault":` + ref + `,"private_key_jwt":{"alg":"PS256","key_vault":` + ref + `}`,
		"key and key_vault":                       fmt.Sprintf(`"private_key_jwt":{"alg":"PS256","key":%q,"key_vault":%s}`, keyPEM, ref),
		"certificate_file and certificate_vault":  `"private_key_jwt":{"alg":"PS256","key_vault":` + ref + `,"certificate_file":"/x","certificate_vault":` + ref + `}`,
		"no key form":                             `"private_key_jwt":{"alg":"PS256","certificate_vault":` + ref + `}`,
		"bad alg with key_vault":                  `"private_key_jwt":{"alg":"HS256","key_vault":` + ref + `}`,
		"empty key_id with key_vault":             `"private_key_jwt":{"alg":"PS256","key_id":"","key_vault":` + ref + `}`,
		"bad ref":                                 `"client_secret_vault":{"path":"../x","key":"k"}`,
	} {
		body := vaultFile(t, "http://vault:8200", 30, `"oauth2":{"token_url":"http://idp:8080/token","client_id":"eacp",`+oauth+`}`)
		if _, err := LoadSecrets(writeFile(t, "secrets.json", body), AllowPlainTokenURL()); err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), "PRIVATE KEY") {
			t.Errorf("%s: the error repeats the key: %v", name, err)
		}
	}
	noVault := fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"erp","host":"erp.internal:8443",
		"oauth2":{"token_url":"http://idp:8080/token","client_id":"eacp","client_secret_vault":%s}}]}`, vaultTenant, ref)
	if _, err := LoadSecrets(writeFile(t, "secrets.json", noVault), AllowPlainTokenURL()); err == nil {
		t.Error("client_secret_vault without a vault object was accepted")
	}
	inlineKey := vaultFile(t, "http://vault:8200", 30, fmt.Sprintf(`"oauth2":{"token_url":"http://idp:8080/token",
		"client_id":"eacp","private_key_jwt":{"alg":"PS256","key":%q,"certificate_vault":%s}}`, keyPEM, ref))
	if _, err := LoadSecrets(writeFile(t, "secrets.json", inlineKey), AllowPlainTokenURL()); err != nil {
		t.Errorf("an inline key with a certificate from Vault was refused: %v", err)
	}
}

// TestAnExchangeClientSecretComesFromVault: a token exchange that
// authenticates its client with a Vault-held secret reads it at the mint.
func TestAnExchangeClientSecretComesFromVault(t *testing.T) {
	f, idp := newFakeVault(t), newFakeIDP(t)
	f.put("secret/data/eacp/idp", map[string]any{"client_secret": "cs1-" + vaultCanary})
	idp.set(func(p *fakeIDP) { p.secret = "cs1-" + vaultCanary })
	now := time.Now()
	subject := jwttest.New(t).Sign(map[string]any{"sub": "system:serviceaccount:eacp:eacp-worker", "aud": "sts",
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix()})
	file := writeFile(t, "subject", subject)
	s := oauthVaultStore(t, f, idp, &vclock{t: now}, nil, fmt.Sprintf(`"grant":"token_exchange","subject_token":{"file":%q},`+
		`"client_secret_vault":{"path":"github.com/atipongsena/eacp/idp","key":"client_secret"}`, file))
	if _, err := s.Credential(context.Background(), vaultTenant, "erp", vaultEndpoint, time.Minute); err != nil {
		t.Fatal(err)
	}
	idp.mu.Lock()
	defer idp.mu.Unlock()
	if !slices.Equal(idp.seen, []string{"cs1-" + vaultCanary}) || idp.form.Get("subject_token") != subject ||
		idp.form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:token-exchange" {
		t.Fatalf("the IdP saw %v, form %v", idp.seen, idp.form)
	}
}
