package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"eacp/internal/jwttest"
	"eacp/internal/logging"
	"eacp/internal/worker"
)

// gcp is an STS and an IAM Credentials endpoint on one server, as the test
// scripts them.
type gcp struct {
	srv            *httptest.Server
	target         atomic.Int64 // requests that reached a redirect's target
	mu             sync.Mutex
	stsReq, impReq *http.Request
	impBody        []byte
	federated      string
	answer         func() (int, any)
}

func newGCP(t *testing.T) *gcp {
	t.Helper()
	g := &gcp{}
	g.answer = func() (int, any) {
		return 200, map[string]any{"accessToken": "ya29.sa-" + canary, "expireTime": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
	}
	var n atomic.Int64
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/sts":
			_ = r.ParseForm()
			fed := fmt.Sprintf("fed-%d-%s", n.Add(1), canary)
			g.mu.Lock()
			g.stsReq, g.federated = r, fed
			g.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fed, "issued_token_type": accessToken,
				"token_type": "Bearer", "expires_in": 600})
		case r.URL.Path == "/elsewhere":
			g.target.Add(1)
		default:
			body, _ := io.ReadAll(r.Body)
			g.mu.Lock()
			g.impReq, g.impBody = r, body
			answer := g.answer
			g.mu.Unlock()
			status, v := answer()
			if status == http.StatusTemporaryRedirect {
				w.Header().Set("Location", "/elsewhere")
			}
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(v)
		}
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *gcp) set(answer func() (int, any)) { g.mu.Lock(); g.answer = answer; g.mu.Unlock() }

// store loads a file-subject exchange that impersonates, on clock c.
func (g *gcp) store(t *testing.T, c *clock, subject string, opts ...worker.LoadOption) *worker.SecretStore {
	t.Helper()
	return exchangeStore(t, g.srv.URL+"/sts", c, "", fileSubject(subject)+
		`,"scope":"https://www.googleapis.com/auth/cloud-platform","impersonate":{"url":"`+g.srv.URL+impersonation+
		`","scope":["https://www.googleapis.com/auth/devstorage.read_only"]}`, opts...)
}

func TestImpersonationTradesTheFederatedToken(t *testing.T) {
	g := newGCP(t)
	s := g.store(t, &clock{t: time.Now()}, subjectFile(t, jwttest.New(t), time.Hour))
	tok, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if tok.Reveal() != "ya29.sa-"+canary {
		t.Fatalf("credential = %q", tok.Reveal())
	}
	if g.impReq.Method != http.MethodPost || g.impReq.Header.Get("Authorization") != "Bearer "+g.federated ||
		!strings.HasPrefix(g.impReq.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("impersonation request: %s %v", g.impReq.Method, g.impReq.Header)
	}
	var body map[string]any
	if err := json.Unmarshal(g.impBody, &body); err != nil || fmt.Sprint(body["scope"]) != "[https://www.googleapis.com/auth/devstorage.read_only]" ||
		body["lifetime"] != "3600s" || len(body) != 2 {
		t.Fatalf("body = %s", g.impBody)
	}
}

func TestEachTokenGoesOnlyToItsHop(t *testing.T) {
	g := newGCP(t)
	file := subjectFile(t, jwttest.New(t), time.Hour)
	s := g.store(t, &clock{t: time.Now()}, file)
	tok, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	subject := readFile(t, file)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stsReq.Header.Get("Authorization") != "" || g.stsReq.PostForm.Get("subject_token") != subject {
		t.Fatal("the STS request carried a Bearer, or not the subject token")
	}
	if strings.Contains(string(g.impBody), subject) || strings.Contains(g.impReq.Header.Get("Authorization"), subject) {
		t.Fatal("the subject token reached the impersonation endpoint")
	}
	if tok.Reveal() == subject || tok.Reveal() == g.federated {
		t.Fatal("the connector would receive the subject or the federated token")
	}
}

func TestEveryInvalidImpersonationResponseIsRefused(t *testing.T) {
	at := func(d time.Duration) string { return time.Now().Add(d).UTC().Format(time.RFC3339) }
	file := subjectFile(t, jwttest.New(t), time.Hour)
	for name, c := range map[string]struct {
		status int
		body   any
		class  string
	}{
		"forbidden":       {403, map[string]any{"error": map[string]any{"code": 403}}, "impersonation_http_403"},
		"not json":        {200, "not an object", "impersonation_invalid"},
		"empty token":     {200, map[string]any{"accessToken": "", "expireTime": at(time.Hour)}, "impersonation_invalid"},
		"token space":     {200, map[string]any{"accessToken": "a b", "expireTime": at(time.Hour)}, "impersonation_invalid"},
		"token too large": {200, map[string]any{"accessToken": strings.Repeat("a", 8193), "expireTime": at(time.Hour)}, "impersonation_invalid"},
		"bad time":        {200, map[string]any{"accessToken": "x", "expireTime": "tomorrow"}, "impersonation_invalid"},
		"no time":         {200, map[string]any{"accessToken": "x"}, "impersonation_invalid"},
		"in the past":     {200, map[string]any{"accessToken": "x", "expireTime": at(-time.Minute)}, "impersonation_invalid"},
		"days ahead":      {200, map[string]any{"accessToken": "x", "expireTime": at(25 * time.Hour)}, "impersonation_invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			g := newGCP(t)
			g.set(func() (int, any) { return c.status, c.body })
			buf := &safeBuffer{}
			s := g.store(t, &clock{t: time.Now()}, file, worker.WithLogger(slog.New(slog.NewJSONHandler(buf, nil))))
			if _, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, time.Second); !errors.Is(err, worker.ErrCredentialUnavailable) {
				t.Fatalf("err = %v", err)
			}
			if !strings.Contains(buf.String(), `"class":"`+c.class+`"`) || len(s.Available()) != 0 {
				t.Fatalf("not withheld with %s: %s", c.class, buf.String())
			}
			if strings.Contains(buf.String(), canary) {
				t.Fatal("a token reached the log")
			}
		})
	}
	g := newGCP(t)
	buf := &safeBuffer{}
	s := g.store(t, &clock{t: time.Now()}, file, worker.WithLogger(slog.New(slog.NewJSONHandler(buf, nil))))
	g.set(func() (int, any) { g.srv.CloseClientConnections(); return 200, nil })
	if _, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, time.Second); !errors.Is(err, worker.ErrCredentialUnavailable) ||
		!strings.Contains(buf.String(), `"class":"transport"`) {
		t.Fatalf("a broken connection: %v %s", err, buf.String())
	}
}

func TestAnImpersonatedTokenIsUsedForAtMostAnHour(t *testing.T) {
	g := newGCP(t)
	g.set(func() (int, any) {
		return 200, map[string]any{"accessToken": "ya29.long-" + canary, "expireTime": time.Now().Add(12 * time.Hour).UTC().Format(time.RFC3339)}
	})
	c := &clock{t: time.Now()}
	s := g.store(t, c, subjectFile(t, jwttest.New(t), 24*time.Hour))
	tok, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, 61*time.Minute); !errors.Is(err, worker.ErrCredentialTooShort) {
		t.Fatalf("a 61-minute call: %v", err)
	}
	c.add(2 * time.Hour)
	if !strings.Contains(strings.Join(s.Values(), "\n"), tok.Reveal()) {
		t.Fatal("the token left Values before its real expiry")
	}
}

func TestTheImpersonationEndpointMayNotRedirect(t *testing.T) {
	g := newGCP(t)
	g.set(func() (int, any) { return http.StatusTemporaryRedirect, map[string]any{} })
	buf := &safeBuffer{}
	s := g.store(t, &clock{t: time.Now()}, subjectFile(t, jwttest.New(t), time.Hour), worker.WithLogger(slog.New(slog.NewJSONHandler(buf, nil))))
	if _, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, time.Second); !errors.Is(err, worker.ErrCredentialUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if g.target.Load() != 0 || !strings.Contains(buf.String(), "impersonation_http_307") {
		t.Fatalf("followed the redirect (%d) or wrong class: %s", g.target.Load(), buf.String())
	}
}

func TestTheFederatedTokenIsRedactedAndScrubbed(t *testing.T) {
	g := newGCP(t)
	set := logging.NewSecretSet()
	c := &clock{t: time.Now()}
	s := g.store(t, c, subjectFile(t, jwttest.New(t), time.Hour), worker.WithRedaction(set))
	if _, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, time.Minute); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	federated := g.federated
	g.mu.Unlock()
	if !strings.Contains(strings.Join(set.Values(), "\n"), federated) {
		t.Fatal("the redaction set lacks the federated token")
	}
	if !strings.Contains(strings.Join(s.Values(), "\n"), federated) {
		t.Fatal("Values lacks the federated token while it lives")
	}
	c.add(601 * time.Second)
	if strings.Contains(strings.Join(s.Values(), "\n"), federated) {
		t.Fatal("Values keeps the federated token after it expired")
	}
}
