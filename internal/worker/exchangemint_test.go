package worker_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/proto/spiffe/workload"

	"github.com/atipongsena/eacp/internal/jwttest"
	"github.com/atipongsena/eacp/internal/logging"
	"github.com/atipongsena/eacp/internal/spiffetest"
	"github.com/atipongsena/eacp/internal/worker"
)

// exchanged answers a token exchange the way GCP's STS does.
func exchanged(n int64) (int, any) {
	return 200, map[string]any{"access_token": fmt.Sprintf("sts-%d-%s", n, canary), "issued_token_type": accessToken,
		"token_type": "Bearer", "expires_in": 600}
}

// exchangeStore loads one token-exchange binding "erp" against the STS at
// tokenURL; oauth is appended to the oauth2 object.
func exchangeStore(t *testing.T, tokenURL string, c *clock, spiffe, oauth string, opts ...worker.LoadOption) *worker.SecretStore {
	t.Helper()
	opts = append([]worker.LoadOption{worker.AllowPlainTokenURL(), worker.WithClock(c.now)}, opts...)
	s, err := worker.LoadSecrets(exchangeFile(t, spiffe, fmt.Sprintf(`"token_url":%q,%s`, tokenURL, oauth)), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimRight(string(b), "\r\n")
}

func fileSubject(path string) string { return fmt.Sprintf(`"subject_token":{"file":%q}`, path) }

func TestAnExchangeSendsTheSubjectToken(t *testing.T) {
	p := newIDP(t)
	p.set(exchanged)
	file := subjectFile(t, jwttest.New(t), time.Hour)
	s := exchangeStore(t, p.srv.URL, &clock{t: time.Now()}, "",
		fileSubject(file)+`,"audience":"//iam.googleapis.com/pool","scope":"https://www.googleapis.com/auth/cloud-platform"`)
	tok, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	form, auth := p.last()
	subject := readFile(t, file)
	for k, want := range map[string]string{"grant_type": tokenExchange, "subject_token": subject,
		"subject_token_type": jwtTokenType, "requested_token_type": accessToken,
		"audience": "//iam.googleapis.com/pool", "scope": "https://www.googleapis.com/auth/cloud-platform"} {
		if got := form.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if auth != [2]string{} || form.Has("client_id") || form.Has("client_assertion") {
		t.Fatalf("a request without client authentication carried some: %v %v", auth, form)
	}
	if tok.Reveal() != "sts-1-"+canary {
		t.Fatalf("credential = %q", tok.Reveal())
	}
}

func TestExchangeClientAuthentication(t *testing.T) {
	signer := jwttest.New(t)
	file := subjectFile(t, signer, time.Hour)
	clientAssertion := subjectFile(t, signer, time.Hour)
	for name, c := range map[string]struct {
		oauth string
		check func(t *testing.T, form map[string][]string, auth [2]string)
	}{
		"public client": {`,"client_id":"eacp"`, func(t *testing.T, form map[string][]string, auth [2]string) {
			if auth != [2]string{} || fmt.Sprint(form["client_id"]) != "[eacp]" || form["client_assertion"] != nil {
				t.Fatalf("form %v, basic %v", form, auth)
			}
		}},
		"client secret": {`,"client_id":"eacp+sts","client_secret":"a b"`, func(t *testing.T, form map[string][]string, auth [2]string) {
			if auth != [2]string{"eacp%2Bsts", "a+b"} || form["client_id"] != nil {
				t.Fatalf("form %v, basic %v", form, auth)
			}
		}},
		"private_key_jwt": {fmt.Sprintf(`,"client_id":"eacp","private_key_jwt":{"alg":"RS256","key":%q}`, pkcs8PEM(t, rsaKey(t, 2048))),
			func(t *testing.T, form map[string][]string, auth [2]string) {
				if auth != [2]string{} || fmt.Sprint(form["client_id"]) != "[eacp]" ||
					fmt.Sprint(form["client_assertion_type"]) != "["+jwtBearer+"]" || len(form["client_assertion"]) != 1 {
					t.Fatalf("form %v, basic %v", form, auth)
				}
			}},
		"client assertion": {fmt.Sprintf(`,"client_id":"eacp","client_assertion_file":%q`, clientAssertion),
			func(t *testing.T, form map[string][]string, auth [2]string) {
				if auth != [2]string{} || fmt.Sprint(form["client_id"]) != "[eacp]" ||
					fmt.Sprint(form["client_assertion_type"]) != "["+jwtBearer+"]" || len(form["client_assertion"]) != 1 {
					t.Fatalf("form %v, basic %v", form, auth)
				}
			}},
	} {
		t.Run(name, func(t *testing.T) {
			p := newIDP(t)
			p.set(exchanged)
			s := exchangeStore(t, p.srv.URL, &clock{t: time.Now()}, "", fileSubject(file)+c.oauth)
			if _, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, time.Minute); err != nil {
				t.Fatal(err)
			}
			form, auth := p.last()
			if form.Get("grant_type") != tokenExchange || form.Get("subject_token") == "" {
				t.Fatalf("not an exchange: %v", form)
			}
			c.check(t, form, auth)
		})
	}
}

func TestAnSVIDSubjectIsFetchedPerMint(t *testing.T) {
	a := spiffetest.New(t)
	var mu sync.Mutex
	var issued []string
	a.Handle(func(r *workload.JWTSVIDRequest) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		ttl := time.Hour
		if len(issued) == 0 {
			ttl = 15 * time.Second // outlives one exchange, not the next
		}
		svid := a.SVID(r.SpiffeId, r.Audience, ttl)
		issued = append(issued, svid)
		return svid, nil
	})
	p := newIDP(t)
	p.set(exchanged)
	c := &clock{t: time.Now()}
	spiffe := fmt.Sprintf(`{"endpoint":%q,"spiffe_id":%q}`, a.Addr(), spiffeWorker)
	s := exchangeStore(t, p.srv.URL, c, spiffe, `"subject_token":{"spiffe":{"audience":"sts"}}`)
	tok, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if form, _ := p.last(); len(issued) != 1 || form.Get("subject_token") != issued[0] {
		t.Fatalf("the subject token is not the agent's SVID (%d issued)", len(issued))
	}
	s.Rejected(tenant, "erp", tok)
	c.add(10 * time.Second) // the first SVID would not outlive the next exchange
	if _, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, time.Minute); err != nil {
		t.Fatal(err)
	}
	if form, _ := p.last(); len(issued) != 2 || form.Get("subject_token") != issued[1] || p.mints.Load() != 2 {
		t.Fatalf("%d SVIDs, %d mints", len(issued), p.mints.Load())
	}
}

func TestTheSubjectTokenMustOutliveTheExchange(t *testing.T) {
	p := newIDP(t)
	p.set(exchanged)
	buf := &safeBuffer{}
	s := exchangeStore(t, p.srv.URL, &clock{t: time.Now()}, "", fileSubject(subjectFile(t, jwttest.New(t), 5*time.Second)),
		worker.WithLogger(slog.New(slog.NewJSONHandler(buf, nil))))
	if _, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, time.Second); !errors.Is(err, worker.ErrCredentialUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if p.mints.Load() != 0 || !strings.Contains(buf.String(), "assertion_expired") {
		t.Fatalf("%d requests; log %s", p.mints.Load(), buf.String())
	}
}

func TestEveryInvalidExchangeResponseIsRefused(t *testing.T) {
	file := subjectFile(t, jwttest.New(t), time.Hour)
	ok := func(issued any) map[string]any {
		m := map[string]any{"access_token": "x", "token_type": "Bearer", "expires_in": 60}
		if issued != nil {
			m["issued_token_type"] = issued
		}
		return m
	}
	for name, c := range map[string]struct {
		status int
		body   any
		class  string
	}{
		"no issued_token_type":  {200, ok(nil), "wrong_token_type"},
		"a jwt":                 {200, ok("urn:ietf:params:oauth:token-type:jwt"), "wrong_token_type"},
		"a refresh token":       {200, ok("urn:ietf:params:oauth:token-type:refresh_token"), "wrong_token_type"},
		"issued type as number": {200, ok(1), "invalid_json"},
		"error status":          {400, map[string]any{"error": "invalid_grant"}, "http_400"},
		"not bearer":            {200, map[string]any{"access_token": "x", "issued_token_type": accessToken, "token_type": "N_A", "expires_in": 60}, "not_bearer"},
		"no expiry":             {200, map[string]any{"access_token": "x", "issued_token_type": accessToken, "token_type": "Bearer"}, "invalid_expires_in"},
	} {
		t.Run(name, func(t *testing.T) {
			p := newIDP(t)
			p.set(func(int64) (int, any) { return c.status, c.body })
			buf := &safeBuffer{}
			s := exchangeStore(t, p.srv.URL, &clock{t: time.Now()}, "", fileSubject(file),
				worker.WithLogger(slog.New(slog.NewJSONHandler(buf, nil))))
			if _, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, time.Second); !errors.Is(err, worker.ErrCredentialUnavailable) {
				t.Fatalf("err = %v", err)
			}
			if !strings.Contains(buf.String(), `"class":"`+c.class+`"`) {
				t.Fatalf("log lacks class %s: %s", c.class, buf.String())
			}
			if len(s.Available()) != 0 {
				t.Fatal("a failed exchange did not withhold the binding")
			}
		})
	}
}

func TestExchangeTokensAreRedactedAndScrubbed(t *testing.T) {
	p := newIDP(t)
	p.set(exchanged)
	file := subjectFile(t, jwttest.New(t), time.Hour)
	set := logging.NewSecretSet()
	s := exchangeStore(t, p.srv.URL, &clock{t: time.Now()}, "", fileSubject(file), worker.WithRedaction(set))
	tok, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	subject := readFile(t, file)
	redacted, live := strings.Join(set.Values(), "\n"), strings.Join(s.Values(), "\n")
	for name, v := range map[string]string{"subject token": subject, "access token": tok.Reveal()} {
		if !strings.Contains(redacted, v) {
			t.Errorf("the redaction set lacks the %s", name)
		}
		if !strings.Contains(live, v) {
			t.Errorf("Values lacks the %s", name)
		}
	}
}

// TestTheSubjectTokenIsReadAfterClientAuthentication: the subject token is
// read just before the request, after any client-authentication fetch, so a
// slow fetch cannot spend its margin (the kubelet may rotate it meanwhile).
func TestTheSubjectTokenIsReadAfterClientAuthentication(t *testing.T) {
	signer := jwttest.New(t)
	file := subjectFile(t, signer, time.Hour)
	a := spiffetest.New(t)
	var rotated string
	a.Handle(func(r *workload.JWTSVIDRequest) (string, error) {
		// The platform rotates the subject token while the SVID is fetched.
		rotated = assertion(signer, time.Now().Add(time.Second), time.Hour, stsSubject)
		writeAssertion(t, file, rotated)
		return a.SVID(r.SpiffeId, r.Audience, time.Hour), nil
	})
	p := newIDP(t)
	p.set(exchanged)
	spiffe := fmt.Sprintf(`{"endpoint":%q,"spiffe_id":%q}`, a.Addr(), spiffeWorker)
	s := exchangeStore(t, p.srv.URL, &clock{t: time.Now()}, spiffe,
		fileSubject(file)+`,"client_id":"eacp","client_assertion_spiffe":{"audience":"sts-client"}`)
	if _, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, time.Minute); err != nil {
		t.Fatal(err)
	}
	if form, _ := p.last(); form.Get("subject_token") != rotated || form.Get("client_assertion") == "" {
		t.Fatal("the subject token was read before the client assertion was fetched")
	}
}

func TestAShortSPIFFESubjectIsRefused(t *testing.T) {
	a := spiffetest.New(t)
	a.Handle(func(r *workload.JWTSVIDRequest) (string, error) {
		return a.SVID(r.SpiffeId, r.Audience, 5*time.Second), nil
	})
	p := newIDP(t)
	p.set(exchanged)
	buf := &safeBuffer{}
	spiffe := fmt.Sprintf(`{"endpoint":%q,"spiffe_id":%q}`, a.Addr(), spiffeWorker)
	s := exchangeStore(t, p.srv.URL, &clock{t: time.Now()}, spiffe, `"subject_token":{"spiffe":{"audience":"sts"}}`,
		worker.WithLogger(slog.New(slog.NewJSONHandler(buf, nil))))
	if _, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, time.Second); !errors.Is(err, worker.ErrCredentialUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if p.mints.Load() != 0 || !strings.Contains(buf.String(), `"class":"assertion_expired"`) {
		t.Fatalf("%d requests; log %s", p.mints.Load(), buf.String())
	}
}

func TestATooShortLogNamesTheGrant(t *testing.T) {
	p := newIDP(t)
	p.set(func(int64) (int, any) {
		return 200, map[string]any{"access_token": "short-" + canary, "issued_token_type": accessToken,
			"token_type": "Bearer", "expires_in": 60}
	})
	buf := &safeBuffer{}
	s := exchangeStore(t, p.srv.URL, &clock{t: time.Now()}, "", fileSubject(subjectFile(t, jwttest.New(t), time.Hour)),
		worker.WithLogger(slog.New(slog.NewJSONHandler(buf, nil))))
	if _, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, 2*time.Minute); !errors.Is(err, worker.ErrCredentialTooShort) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(buf.String(), `"msg":"credential lifetime shorter than the call"`) ||
		!strings.Contains(buf.String(), `"grant":"token_exchange"`) {
		t.Fatalf("log: %s", buf.String())
	}
}
