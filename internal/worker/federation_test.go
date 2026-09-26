package worker_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"eacp/internal/jwttest"
	"eacp/internal/logging"
	"eacp/internal/worker"
)

const jwtBearer = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"

// assertion returns a platform-style JWT expiring exp after now.
func assertion(s *jwttest.Signer, now time.Time, exp time.Duration, sub string) string {
	return s.Sign(map[string]any{"iss": "https://issuer.test", "sub": sub, "aud": []string{"idp"},
		"iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(exp).Unix()})
}

func writeAssertion(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertionStore(t *testing.T, tokenURL string, c *clock, file string, opts ...worker.LoadOption) *worker.SecretStore {
	t.Helper()
	body := fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"erp","host":"erp.internal:8443",
		"oauth2":{"token_url":%q,"client_id":"eacp-wif","client_assertion_file":%q,"scope":"api://erp/.default"}}]}`,
		tenant, tokenURL, file)
	opts = append([]worker.LoadOption{worker.AllowPlainTokenURL(), worker.WithClock(c.now)}, opts...)
	s, err := worker.LoadSecrets(secretsFile(t, body), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAnAssertionAuthenticatesTheTokenRequest(t *testing.T) {
	p := newIDP(t)
	c := &clock{t: time.Now()}
	jwt := assertion(jwttest.New(t), c.now(), 10*time.Minute, "worker-1")
	file := filepath.Join(t.TempDir(), "token")
	writeAssertion(t, file, jwt)
	s := assertionStore(t, p.srv.URL, c, file)
	if _, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, time.Minute); err != nil {
		t.Fatal(err)
	}
	form, auth := p.last()
	if form.Get("grant_type") != "client_credentials" || form.Get("client_id") != "eacp-wif" ||
		form.Get("client_assertion_type") != jwtBearer || form.Get("client_assertion") != jwt ||
		form.Get("scope") != "api://erp/.default" {
		t.Fatalf("form = %v", form)
	}
	if auth != [2]string{} {
		t.Fatalf("an assertion request also sent Basic auth %q", auth)
	}
}

func TestEveryMintReadsTheCurrentAssertion(t *testing.T) {
	p := newIDP(t)
	c := &clock{t: time.Now()}
	signer := jwttest.New(t)
	file := filepath.Join(t.TempDir(), "token")
	writeAssertion(t, file, assertion(signer, c.now(), 10*time.Minute, "worker-1"))
	s := assertionStore(t, p.srv.URL, c, file)
	ctx := context.Background()
	tok, err := s.Credential(ctx, tenant, "erp", erpEndpoint, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	rotated := assertion(signer, c.now(), 20*time.Minute, "worker-2") // the kubelet rotated the file
	writeAssertion(t, file, rotated)
	s.Rejected(tenant, "erp", tok)
	if _, err := s.Credential(ctx, tenant, "erp", erpEndpoint, time.Minute); err != nil {
		t.Fatal(err)
	}
	if form, _ := p.last(); form.Get("client_assertion") != rotated {
		t.Fatal("the second mint did not send the rotated assertion")
	}
}

func TestTheAssertionIsTrimmed(t *testing.T) {
	p := newIDP(t)
	c := &clock{t: time.Now()}
	jwt := assertion(jwttest.New(t), c.now(), 10*time.Minute, "worker-1")
	file := filepath.Join(t.TempDir(), "token")
	writeAssertion(t, file, jwt+"\r\n")
	s := assertionStore(t, p.srv.URL, c, file)
	if _, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, time.Minute); err != nil {
		t.Fatal(err)
	}
	if form, _ := p.last(); form.Get("client_assertion") != jwt {
		t.Fatal("the assertion was sent with its line ending")
	}
}

func TestAnUnusableAssertionFailsTheMintAndBacksOff(t *testing.T) {
	for kind, spoil := range map[string]func(t *testing.T, file string, s *jwttest.Signer, now time.Time){
		"unreadable": func(t *testing.T, file string, _ *jwttest.Signer, _ time.Time) {
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
		},
		"invalid": func(t *testing.T, file string, _ *jwttest.Signer, _ time.Time) {
			writeAssertion(t, file, "not.a.jwt")
		},
		"expired": func(t *testing.T, file string, s *jwttest.Signer, now time.Time) {
			writeAssertion(t, file, assertion(s, now, 5*time.Second, "worker-1"))
		},
	} {
		t.Run(kind, func(t *testing.T) {
			p := newIDP(t)
			c := &clock{t: time.Now()}
			signer := jwttest.New(t)
			file := filepath.Join(t.TempDir(), "token")
			writeAssertion(t, file, assertion(signer, c.now(), 10*time.Minute, "worker-1"))
			var logs bytes.Buffer
			var mu sync.Mutex
			s := assertionStore(t, p.srv.URL, c, file,
				worker.WithLogger(slog.New(slog.NewJSONHandler(syncWriter{&logs, &mu}, nil))))
			spoil(t, file, signer, c.now())
			if _, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, time.Minute); !errors.Is(err, worker.ErrCredentialUnavailable) {
				t.Fatalf("err = %v, want ErrCredentialUnavailable", err)
			}
			if p.mints.Load() != 0 {
				t.Fatal("a token request was made without a usable assertion")
			}
			if len(s.Available()) != 0 {
				t.Fatal("the binding did not back off")
			}
			mu.Lock()
			defer mu.Unlock()
			if !strings.Contains(logs.String(), `"class":"assertion_`+kind+`"`) {
				t.Fatalf("the failure class was not logged: %s", logs.String())
			}
		})
	}
}

func TestAssertionsAreRedactedAndScrubbed(t *testing.T) {
	p := newIDP(t)
	c := &clock{t: time.Now()}
	jwt := assertion(jwttest.New(t), c.now(), 10*time.Minute, "worker-1")
	file := filepath.Join(t.TempDir(), "token")
	writeAssertion(t, file, jwt)
	set := logging.NewSecretSet()
	s := assertionStore(t, p.srv.URL, c, file, worker.WithRedaction(set))
	if _, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, time.Minute); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(set.Values(), ","), jwt) {
		t.Fatal("the redaction set lacks the assertion")
	}
	if !strings.Contains(strings.Join(s.Values(), ","), jwt) {
		t.Fatal("Values lacks the assertion")
	}
	for _, v := range s.Values() {
		if v == "" {
			t.Fatal("Values holds an empty string (no client secret)")
		}
	}
	c.add(11 * time.Minute)
	if strings.Contains(strings.Join(s.Values(), ","), jwt) {
		t.Fatal("an expired assertion is still in Values")
	}
}

func TestAnExpiredAssertionIsAcceptedAtLoad(t *testing.T) {
	c := &clock{t: time.Now()}
	file := filepath.Join(t.TempDir(), "token")
	writeAssertion(t, file, assertion(jwttest.New(t), c.now(), -time.Minute, "worker-1"))
	assertionStore(t, "https://idp.example/token", c, file) // fails the test on an error
}
