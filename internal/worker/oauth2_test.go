package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"eacp/internal/logging"
	"eacp/internal/worker"
)

// idp is a token endpoint whose answers the test controls.
type idp struct {
	srv    *httptest.Server
	mints  atomic.Int64
	mu     sync.Mutex
	answer func(n int64) (int, any) // status and JSON body of the n-th mint
	form   url.Values
	auth   [2]string
}

func newIDP(t *testing.T) *idp {
	t.Helper()
	p := &idp{answer: func(n int64) (int, any) {
		return 200, map[string]any{"access_token": fmt.Sprintf("tok-%d-%s", n, canary), "token_type": "bearer", "expires_in": 600}
	}}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := p.mints.Add(1)
		_ = r.ParseForm()
		id, secret, _ := r.BasicAuth()
		p.mu.Lock()
		p.form, p.auth = r.PostForm, [2]string{id, secret}
		answer := p.answer
		p.mu.Unlock()
		status, body := answer(n)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *idp) set(answer func(n int64) (int, any)) { p.mu.Lock(); p.answer = answer; p.mu.Unlock() }

func (p *idp) last() (url.Values, [2]string) { p.mu.Lock(); defer p.mu.Unlock(); return p.form, p.auth }

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func oauthStore(t *testing.T, tokenURL string, c *clock, extra string, opts ...worker.LoadOption) *worker.SecretStore {
	t.Helper()
	body := fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"erp","host":"erp.internal:8443",
		"oauth2":{"token_url":%q,"client_id":"eacp+worker","client_secret":%q%s}}]}`,
		tenant, tokenURL, canary+"-client", extra)
	opts = append([]worker.LoadOption{worker.AllowPlainTokenURL(), worker.WithClock(c.now)}, opts...)
	s, err := worker.LoadSecrets(secretsFile(t, body), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const erpEndpoint = "https://erp.internal:8443/v1"

func TestAMintedTokenIsReusedWhileItOutlivesTheCall(t *testing.T) {
	p := newIDP(t)
	c := &clock{t: time.Now()}
	s := oauthStore(t, p.srv.URL+"/oauth2/token", c, `,"scope":"erp.purchase erp.read","resource":"https://erp.internal"`)
	ctx := context.Background()
	first, err := s.Credential(ctx, tenant, "erp", erpEndpoint, time.Minute)
	if err != nil || first.Reveal() != "tok-1-"+canary {
		t.Fatalf("first mint: %v", err)
	}
	form, auth := p.last()
	if form.Get("grant_type") != "client_credentials" || form.Get("scope") != "erp.purchase erp.read" ||
		form.Get("resource") != "https://erp.internal" {
		t.Fatalf("form = %v", form)
	}
	if auth != [2]string{"eacp%2Bworker", canary + "-client"} {
		t.Fatalf("basic auth = %q, want the form-urlencoded id and secret", auth)
	}
	c.add(5 * time.Minute) // 5 of 10 minutes left
	again, err := s.Credential(ctx, tenant, "erp", erpEndpoint, time.Minute)
	if err != nil || again.Reveal() != first.Reveal() || p.mints.Load() != 1 {
		t.Fatalf("a valid token was not reused: mints=%d err=%v", p.mints.Load(), err)
	}
	if !strings.Contains(strings.Join(s.Values(), ","), first.Reveal()) {
		t.Fatal("Values lacks the live token")
	}
}

func TestAReusedTokenMustOutliveTheCall(t *testing.T) {
	p := newIDP(t)
	c := &clock{t: time.Now()}
	s := oauthStore(t, p.srv.URL, c, "")
	ctx := context.Background()
	if _, err := s.Credential(ctx, tenant, "erp", erpEndpoint, time.Minute); err != nil {
		t.Fatal(err)
	}
	c.add(9*time.Minute + 30*time.Second) // 30 s left; the call needs a minute
	fresh, err := s.Credential(ctx, tenant, "erp", erpEndpoint, time.Minute)
	if err != nil || fresh.Reveal() != "tok-2-"+canary {
		t.Fatalf("expected a fresh mint: %v", err)
	}
	p.set(func(int64) (int, any) {
		return 200, map[string]any{"access_token": "short-" + canary, "token_type": "Bearer", "expires_in": 30}
	})
	c.add(9*time.Minute + 30*time.Second)
	if _, err := s.Credential(ctx, tenant, "erp", erpEndpoint, time.Minute); !errors.Is(err, worker.ErrCredentialTooShort) ||
		!errors.Is(err, worker.ErrCredentialUnavailable) {
		t.Fatalf("a token living 30 s was used for a 60 s call: %v", err)
	}
}

// The IdP answering with tokens too short for one call is not an outage: the
// binding stays available, shorter calls use the token, and longer calls are
// refused at once without minting again.
func TestATokenTooShortForOneCallDoesNotSuspendTheBinding(t *testing.T) {
	p := newIDP(t)
	p.set(func(n int64) (int, any) {
		return 200, map[string]any{"access_token": fmt.Sprintf("tok-%d", n), "token_type": "Bearer", "expires_in": 30}
	})
	c := &clock{t: time.Now()}
	s := oauthStore(t, p.srv.URL, c, "")
	ctx := context.Background()
	if _, err := s.Credential(ctx, tenant, "erp", erpEndpoint, time.Minute); !errors.Is(err, worker.ErrCredentialTooShort) {
		t.Fatalf("err = %v, want ErrCredentialTooShort", err)
	}
	if len(s.Available()) != 1 {
		t.Fatal("the binding backs off although its IdP answered")
	}
	if tok, err := s.Credential(ctx, tenant, "erp", erpEndpoint, 5*time.Second); err != nil || tok.Reveal() != "tok-1" {
		t.Fatalf("a shorter call did not get the token: %v", err)
	}
	for range 3 {
		if _, err := s.Credential(ctx, tenant, "erp", erpEndpoint, time.Minute); !errors.Is(err, worker.ErrCredentialTooShort) {
			t.Fatal(err)
		}
	}
	if p.mints.Load() != 1 {
		t.Fatalf("%d mints: a call longer than the IdP's tokens minted again", p.mints.Load())
	}
	// Once the token has expired, the next mint learns the IdP's lifetime anew.
	p.set(func(n int64) (int, any) {
		return 200, map[string]any{"access_token": fmt.Sprintf("tok-%d", n), "token_type": "Bearer", "expires_in": 600}
	})
	c.add(time.Minute)
	if _, err := s.Credential(ctx, tenant, "erp", erpEndpoint, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if tok, err := s.Credential(ctx, tenant, "erp", erpEndpoint, time.Minute); err != nil || tok.Reveal() != "tok-2" {
		t.Fatalf("a longer token lifetime was not learned: %v", err)
	}
}

func TestEveryInvalidTokenResponseIsRefused(t *testing.T) {
	for name, a := range map[string]struct {
		status int
		body   any
	}{
		"error response":     {400, map[string]any{"error": "invalid_client"}},
		"redirect status":    {302, map[string]any{}},
		"not bearer":         {200, map[string]any{"access_token": "x", "token_type": "mac", "expires_in": 60}},
		"no expiry":          {200, map[string]any{"access_token": "x", "token_type": "Bearer"}},
		"expiry as string":   {200, map[string]any{"access_token": "x", "token_type": "Bearer", "expires_in": "60"}},
		"fractional expiry":  {200, map[string]any{"access_token": "x", "token_type": "Bearer", "expires_in": 60.5}},
		"too long lived":     {200, map[string]any{"access_token": "x", "token_type": "Bearer", "expires_in": 86401}},
		"zero lifetime":      {200, map[string]any{"access_token": "x", "token_type": "Bearer", "expires_in": 0}},
		"empty token":        {200, map[string]any{"access_token": "", "token_type": "Bearer", "expires_in": 60}},
		"token with a space": {200, map[string]any{"access_token": "a b", "token_type": "Bearer", "expires_in": 60}},
		"token too large":    {200, map[string]any{"access_token": strings.Repeat("a", 8193), "token_type": "Bearer", "expires_in": 60}},
		"not an object":      {200, []string{"x"}},
	} {
		t.Run(name, func(t *testing.T) {
			p := newIDP(t)
			p.set(func(int64) (int, any) { return a.status, a.body })
			s := oauthStore(t, p.srv.URL, &clock{t: time.Now()}, "")
			if _, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, time.Second); !errors.Is(err, worker.ErrCredentialUnavailable) {
				t.Fatalf("err = %v, want ErrCredentialUnavailable", err)
			}
		})
	}
}

// longLived answers every mint with a token living 90 minutes, as Entra ID
// may (a random 60-90 minute default lifetime).
func longLived(n int64) (int, any) {
	return 200, map[string]any{"access_token": fmt.Sprintf("long-%d-%s", n, canary), "token_type": "Bearer", "expires_in": 5400}
}

func TestALongLivedTokenIsUsedForAtMostAnHour(t *testing.T) {
	p := newIDP(t)
	p.set(longLived)
	c := &clock{t: time.Now()}
	s := oauthStore(t, p.srv.URL, c, "")
	ctx := context.Background()
	first, err := s.Credential(ctx, tenant, "erp", erpEndpoint, time.Minute)
	if err != nil {
		t.Fatalf("a token living 90 minutes was refused: %v", err)
	}
	c.add(58 * time.Minute)
	if again, err := s.Credential(ctx, tenant, "erp", erpEndpoint, time.Minute); err != nil || again.Reveal() != first.Reveal() {
		t.Fatalf("the token was not reused within its first hour: %v", err)
	}
	c.add(2 * time.Minute) // an hour after the mint
	if fresh, err := s.Credential(ctx, tenant, "erp", erpEndpoint, time.Minute); err != nil || fresh.Reveal() == first.Reveal() {
		t.Fatalf("a token was used for more than an hour: %v", err)
	}
	if p.mints.Load() != 2 {
		t.Fatalf("%d mints, want 2", p.mints.Load())
	}
	if !strings.Contains(strings.Join(s.Values(), ","), first.Reveal()) {
		t.Fatal("the first token left Values before its real expiry (it still authorises at the target)")
	}
	c.add(31 * time.Minute) // past the first token's real expiry
	if strings.Contains(strings.Join(s.Values(), ","), first.Reveal()) {
		t.Fatal("an expired token is still in Values")
	}
}

func TestTheCapDecidesWhetherATokenIsTooShort(t *testing.T) {
	p := newIDP(t)
	p.set(longLived)
	s := oauthStore(t, p.srv.URL, &clock{t: time.Now()}, "")
	ctx := context.Background()
	for range 2 {
		if _, err := s.Credential(ctx, tenant, "erp", erpEndpoint, 3700*time.Second); !errors.Is(err, worker.ErrCredentialTooShort) {
			t.Fatalf("a call longer than an hour got a token: %v", err)
		}
	}
	if p.mints.Load() != 1 || len(s.Available()) != 1 {
		t.Fatalf("mints = %d, available = %d: want one mint and no back-off", p.mints.Load(), len(s.Available()))
	}
}

func TestALongLivedTokenIsRedacted(t *testing.T) {
	p := newIDP(t)
	p.set(longLived)
	set := logging.NewSecretSet()
	s := oauthStore(t, p.srv.URL, &clock{t: time.Now()}, "", worker.WithRedaction(set))
	tok, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(set.Values(), ","), tok.Reveal()) {
		t.Fatal("the redaction set lacks the token")
	}
}

func TestTheTokenEndpointMayNotRedirect(t *testing.T) {
	var elsewhere atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) }))
	defer other.Close()
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer redirecting.Close()
	s := oauthStore(t, redirecting.URL, &clock{t: time.Now()}, "")
	if _, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, time.Second); !errors.Is(err, worker.ErrCredentialUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if elsewhere.Load() != 0 {
		t.Fatal("the client secret followed a redirect")
	}
}

func TestConcurrentCallersShareOneMint(t *testing.T) {
	p := newIDP(t)
	release := make(chan struct{})
	p.set(func(n int64) (int, any) {
		<-release
		return 200, map[string]any{"access_token": fmt.Sprintf("tok-%d", n), "token_type": "Bearer", "expires_in": 600}
	})
	s := oauthStore(t, p.srv.URL, &clock{t: time.Now()}, "")
	var wg sync.WaitGroup
	got := make([]string, 8)
	for i := range got {
		wg.Go(func() {
			if sec, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, time.Minute); err == nil {
				got[i] = sec.Reveal()
			}
		})
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	for _, g := range got {
		if g != "tok-1" {
			t.Fatalf("callers got %v, want one shared mint", got)
		}
	}
	if p.mints.Load() != 1 {
		t.Fatalf("%d mints", p.mints.Load())
	}
}

func TestFailedMintsBackOffAndWithholdTheBinding(t *testing.T) {
	p := newIDP(t)
	var fail atomic.Bool
	fail.Store(true)
	p.set(func(int64) (int, any) {
		if fail.Load() {
			return 503, map[string]any{"error": "temporarily_unavailable"}
		}
		return 200, map[string]any{"access_token": "ok", "token_type": "Bearer", "expires_in": 600}
	})
	c := &clock{t: time.Now()}
	s := oauthStore(t, p.srv.URL, c, "")
	ctx := context.Background()
	if len(s.Available()) != 1 {
		t.Fatal("a fresh binding is not available")
	}
	for _, wait := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		if _, err := s.Credential(ctx, tenant, "erp", erpEndpoint, time.Second); !errors.Is(err, worker.ErrCredentialUnavailable) {
			t.Fatal(err)
		}
		before := p.mints.Load()
		if len(s.Available()) != 0 {
			t.Fatal("a backing-off binding is still offered for claims")
		}
		if _, err := s.Credential(ctx, tenant, "erp", erpEndpoint, time.Second); !errors.Is(err, worker.ErrCredentialUnavailable) || p.mints.Load() != before {
			t.Fatal("a backing-off binding called the token endpoint")
		}
		c.add(wait - time.Millisecond)
		if len(s.Available()) != 0 {
			t.Fatalf("available before its back-off of %v passed", wait)
		}
		c.add(time.Millisecond)
		if len(s.Available()) != 1 {
			t.Fatalf("not available once its back-off of %v passed", wait)
		}
	}
	fail.Store(false)
	if _, err := s.Credential(ctx, tenant, "erp", erpEndpoint, time.Second); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	c.add(time.Hour) // the token expired; the next mint fails and backs off from 1 s again
	_, _ = s.Credential(ctx, tenant, "erp", erpEndpoint, time.Second)
	c.add(time.Second)
	if len(s.Available()) != 1 {
		t.Fatal("a success did not reset the back-off")
	}
}

func TestTheBackOffIsCappedAtAMinute(t *testing.T) {
	p := newIDP(t)
	p.set(func(int64) (int, any) { return 500, map[string]any{} })
	c := &clock{t: time.Now()}
	s := oauthStore(t, p.srv.URL, c, "")
	for range 10 {
		_, _ = s.Credential(context.Background(), tenant, "erp", erpEndpoint, time.Second)
		c.add(time.Minute)
		if len(s.Available()) != 1 {
			t.Fatal("a back-off exceeded a minute")
		}
	}
}

func TestARejectedTokenIsDropped(t *testing.T) {
	p := newIDP(t)
	s := oauthStore(t, p.srv.URL, &clock{t: time.Now()}, "")
	ctx := context.Background()
	first, _ := s.Credential(ctx, tenant, "erp", erpEndpoint, time.Minute)
	s.Rejected(tenant, "erp", worker.Secret{}) // not the cached value: ignored
	if again, _ := s.Credential(ctx, tenant, "erp", erpEndpoint, time.Minute); again.Reveal() != first.Reveal() {
		t.Fatal("rejecting another value dropped the token")
	}
	s.Rejected(tenant, "erp", first)
	if again, _ := s.Credential(ctx, tenant, "erp", erpEndpoint, time.Minute); again.Reveal() == first.Reveal() {
		t.Fatal("a rejected token was reused")
	}
}

func TestMintedTokensAndClientSecretsAreRedacted(t *testing.T) {
	p := newIDP(t)
	set := logging.NewSecretSet()
	s := oauthStore(t, p.srv.URL, &clock{t: time.Now()}, "", worker.WithRedaction(set))
	tok, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	values := strings.Join(set.Values(), ",")
	if !strings.Contains(values, tok.Reveal()) || !strings.Contains(values, canary+"-client") {
		t.Fatal("the redaction set lacks the token or the client secret")
	}
	if all := strings.Join(s.Values(), ","); !strings.Contains(all, canary+"-client") {
		t.Fatal("Values lacks the client secret")
	}
}

func TestAnOAuthBindingIsStillBoundToItsHost(t *testing.T) {
	p := newIDP(t)
	s := oauthStore(t, p.srv.URL, &clock{t: time.Now()}, "")
	if _, err := s.Credential(context.Background(), tenant, "erp", "https://evil.example/v1", time.Second); !errors.Is(err, worker.ErrNoCredential) {
		t.Fatalf("err = %v", err)
	}
	if p.mints.Load() != 0 {
		t.Fatal("a token was minted for a host the binding does not name")
	}
}
