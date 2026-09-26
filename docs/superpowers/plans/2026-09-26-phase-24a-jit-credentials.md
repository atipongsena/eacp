# Phase 24a JIT Credentials Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The execution worker resolves connector credentials through providers, and an OAuth 2.0 client-credentials provider mints short-lived bearer tokens just in time, with custody unchanged (ADR-001, new ADR-019).

**Architecture:** `worker.SecretStore` keeps its name and file but each entry is now a static secret or an `oauth2` provider. `Credential(ctx, tenant, ref, endpoint, validFor)` returns a token that stays valid for the whole call; `Available()` withholds a binding from claims while its token endpoint backs off. `logging.SecretSet` lets a logger built at startup redact tokens minted later. Fake ERP gains a token endpoint so the whole path runs for real in tests, compose and Kubernetes.

**Tech Stack:** Go 1.27 standard library (net/http, net/url, crypto), PostgreSQL 18 (unchanged schema), docker compose, minikube (unchanged scripts).

**Spec:** `docs/superpowers/specs/2026-09-26-jit-credentials-design.md`

## Global Constraints

- Providers run only inside `execution-worker`; no API, table, message or log learns a credential or provider configuration beyond `secret_ref`.
- No new Go module and no download.
- No migration: the schema is unchanged.
- Token response: HTTP 200 JSON; `access_token` 1–8192 printable ASCII without spaces; `token_type` `Bearer` (case-insensitive); `expires_in` integer 1–3600. Anything else is a failed mint.
- Token request: `POST`, form `grant_type=client_credentials[&scope][&resource]`, `client_secret_basic` with form-urlencoded id and secret, 10 s timeout, no redirects, response ≤ 64 KiB.
- `validFor` = call budget + `worker.CredentialSkew` (30 s): worker = `eacp.call_timeout` of the pinned contract; reconciler = `min(job.Timeout, Lease/2)`; scanner = `Timeout`.
- Back-off after a failed mint: 1 s doubling to 60 s; success resets it.
- Minted tokens enter the redaction set until expiry + 24 h; at most 10 000 temporary values.
- `token_url` must be `https`, or `http` only with `AllowPlainTokenURL()` (the worker passes it when `EACP_ENV` is `development` or `test`).
- A rejected token (`error_class` `unauthorized`) is classified exactly as today (ADR-004); the provider only drops it.
- Commit as the user only; no Co-Authored-By trailer. Tests run with `-race` and `EACP_TEST_ADMIN_DSN`.

## Review Focus

1. A token whose remaining life is shorter than the call must never be sent: pinned by `TestAReusedTokenMustOutliveTheCall` (Task 2) and `TestATokenShorterThanTheCallNeverDispatches` (Task 4).
2. A failing token endpoint must not churn leases or hammer the endpoint: `TestFailedMintsBackOffAndWithholdTheBinding` (Task 2) and `TestAFailingTokenEndpointWithholdsClaimsUntilTheBackOffPasses` (Task 4).
3. Concurrent calls for one binding must share one mint: `TestConcurrentCallersShareOneMint` (Task 2).
4. A redirect from the token endpoint must never carry the client secret elsewhere: `TestTheTokenEndpointMayNotRedirect` (Task 2).
5. Tokens must never persist or print: `TestALoggerRedactsASecretAddedAfterItWasBuilt` (Task 1), the database and log checks in `TestTheWorkerExecutesWithAMintedToken` (Task 4) and the scan in `TestJITDemo` (Task 5).

---

### Task 1: A redaction set that grows after the logger is built

**Files:**
- Create: `internal/logging/secrets.go`
- Modify: `internal/logging/logging.go` (redactor reads a `*SecretSet`; `NewWithSet`)
- Modify: `internal/service/service.go` (Deps owns one set; `Redaction()`; `RedactSecrets` adds permanently)
- Test: `internal/logging/secrets_test.go`, `internal/service/service_test.go`

**Interfaces:**
- Produces: `logging.NewSecretSet(values ...string) *SecretSet`; `(*SecretSet).AddPermanent(values ...string)`; `(*SecretSet).Add(value string, until time.Time)`; `(*SecretSet).Values() []string`; `logging.NewWithSet(w io.Writer, level slog.Leveler, format string, set *SecretSet) *slog.Logger`; `(*service.Deps).Redaction() *logging.SecretSet`.

- [ ] **Step 1: Write the failing tests** (`internal/logging/secrets_test.go`, package `logging`)

```go
package logging

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestALoggerRedactsASecretAddedAfterItWasBuilt(t *testing.T) {
	set := NewSecretSet()
	var buf bytes.Buffer
	log := NewWithSet(&buf, slog.LevelDebug, "json", set)
	set.Add(canary+"-minted", time.Now().Add(time.Hour))
	set.AddPermanent(canary + "-static")
	log.Info("call", "reference", "PO-"+canary+"-minted", "err", fmt.Errorf("bad %s", canary+"-static"))
	if strings.Contains(buf.String(), canary) {
		t.Fatalf("a secret added after the logger was built leaked: %s", buf.String())
	}
}

func TestTemporarySecretsExpireAndAreBounded(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	set := NewSecretSet("perm")
	set.now = func() time.Time { return now }
	set.Add("short", now.Add(time.Minute))
	set.Add("long", now.Add(time.Hour))
	now = now.Add(2 * time.Minute)
	got := strings.Join(set.Values(), ",")
	if got != "perm,long" {
		t.Fatalf("values = %s, want perm,long", got)
	}
	for i := range maxTemporary + 5 {
		set.Add(fmt.Sprintf("v%d", i), now.Add(time.Hour))
	}
	if n := len(set.Values()); n != 1+maxTemporary {
		t.Fatalf("%d values, want %d", n, 1+maxTemporary)
	}
	if v := set.Values(); v[1] == "long" || v[1] == "v0" {
		t.Fatalf("the oldest temporary values were not dropped first: %v", v[:3])
	}
	set.Add("", now.Add(time.Hour))
	set.AddPermanent("")
	if n := len(set.Values()); n != 1+maxTemporary {
		t.Fatal("an empty value was added")
	}
}
```

Append to `internal/service/service_test.go`:

```go
func TestTheServiceLoggerRedactsValuesAddedLater(t *testing.T) {
	var out bytes.Buffer
	deps, stop, err := service.Start(context.Background(), "redact-test", mapEnv(map[string]string{}),
		config.Options{}, &out)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	log := deps.Log
	deps.Redaction().Add("tok-canary-late", time.Now().Add(time.Hour))
	log.Info("minted", "value", "tok-canary-late")
	if strings.Contains(out.String(), "tok-canary-late") {
		t.Fatalf("value added after the logger was taken leaked: %s", out.String())
	}
}
```

(Use the file's existing env helper; if it is named differently, use that name — read `TestRedactSecretsExtendsTheServiceLogger` first and copy its setup.)

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/logging ./internal/service -run 'Redact|Temporary'`
Expected: FAIL to compile: `undefined: NewSecretSet`, `undefined: NewWithSet`, `deps.Redaction undefined`.

- [ ] **Step 3: Implement** `internal/logging/secrets.go`

```go
package logging

import (
	"sync"
	"time"
)

// maxTemporary bounds the temporary values a set keeps; the oldest go first.
const maxTemporary = 10000

// SecretSet is the set of values a logger redacts. It may grow after the
// logger is built: a value added later is redacted from then on. Temporary
// values (short-lived credentials) are kept until their time passes.
type SecretSet struct {
	mu        sync.RWMutex
	permanent []string
	temporary []temporarySecret // in insertion order
	now       func() time.Time
}

type temporarySecret struct {
	value string
	until time.Time
}

// NewSecretSet returns a set holding values permanently.
func NewSecretSet(values ...string) *SecretSet {
	s := &SecretSet{now: time.Now}
	s.AddPermanent(values...)
	return s
}

// AddPermanent redacts values for the life of the set. Empty values are ignored.
func (s *SecretSet) AddPermanent(values ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range values {
		if v != "" {
			s.permanent = append(s.permanent, v)
		}
	}
}

// Add redacts value until the given time, dropping expired values and, past
// maxTemporary, the oldest ones.
func (s *SecretSet) Add(value string, until time.Time) {
	if value == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	kept := s.temporary[:0]
	for _, t := range s.temporary {
		if t.until.After(now) {
			kept = append(kept, t)
		}
	}
	kept = append(kept, temporarySecret{value, until})
	if over := len(kept) - maxTemporary; over > 0 {
		kept = append(kept[:0], kept[over:]...)
	}
	s.temporary = kept
}

// Values returns the permanent values, then the unexpired temporary ones.
func (s *SecretSet) Values() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := s.now()
	out := make([]string, 0, len(s.permanent)+len(s.temporary))
	out = append(out, s.permanent...)
	for _, t := range s.temporary {
		if t.until.After(now) {
			out = append(out, t.value)
		}
	}
	return out
}
```

In `logging.go`, replace `redactor{secrets []string}` by `redactor{set *SecretSet}`; `New` becomes:

```go
func New(w io.Writer, level slog.Leveler, format string, secrets ...string) *slog.Logger {
	return NewWithSet(w, level, format, NewSecretSet(secrets...))
}

// NewWithSet returns a logger that redacts every value in set, including
// values added after it was built.
func NewWithSet(w io.Writer, level slog.Leveler, format string, set *SecretSet) *slog.Logger {
	r := redactor{set: set}
	opts := &slog.HandlerOptions{Level: level, ReplaceAttr: r.replace}
	if format == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}
```

`replace` uses `secrets := r.set.Values()` once per attribute (`len(secrets) > 0` where it used `len(r.secrets) > 0`), and `scrub(s, secrets)` iterates it.

In `service.go`: `Deps` replaces `secrets []string` with `redaction *logging.SecretSet`. `Start` builds `set := logging.NewSecretSet(dsnPassword(cfg.DatabaseURL), dsnPassword(cfg.NATSURL))` and `log := logging.NewWithSet(out, cfg.LogLevel, cfg.LogFormat, set).With("service", name)`. `RedactSecrets(values...)` becomes `d.redaction.AddPermanent(values...)` (the logger already reads the set). Add:

```go
// Redaction is the service logger's redaction set: values added to it are
// redacted from every log line from then on (short-lived credentials).
func (d *Deps) Redaction() *logging.SecretSet { return d.redaction }
```

- [ ] **Step 4: Run the packages**

Run: `go test -race ./internal/logging ./internal/service`
Expected: PASS (existing `TestRedactSecretsExtendsTheServiceLogger` and the logging tests still pass).

- [ ] **Step 5: Commit**

```bash
git add internal/logging internal/service
git commit -m "feat(logging): a redaction set that grows after the logger is built"
```

---

### Task 2: The OAuth 2.0 client-credentials provider in the worker's secret store

**Files:**
- Create: `internal/worker/oauth2.go`
- Modify: `internal/worker/secrets.go`
- Test: `internal/worker/oauth2_test.go`, `internal/worker/secrets_test.go` (file validation cases)

**Interfaces:**
- Consumes: `logging.SecretSet.Add`, `AddPermanent` (Task 1).
- Produces (package `worker`):
  - `var ErrCredentialUnavailable = errors.New("worker: credential unavailable")`
  - `const CredentialSkew = 30 * time.Second`
  - `type LoadOption func(*loadConfig)`; `AllowPlainTokenURL() LoadOption`; `WithRedaction(*logging.SecretSet) LoadOption`; `WithClock(func() time.Time) LoadOption`; `WithLogger(*slog.Logger) LoadOption`
  - `LoadSecrets(path string, opts ...LoadOption) (*SecretStore, error)`
  - `(*SecretStore).Credential(ctx context.Context, tenant uuid.UUID, ref, endpoint string, validFor time.Duration) (Secret, error)`
  - `(*SecretStore).Resolve(tenant uuid.UUID, ref, endpoint string) (Secret, error)` = `Credential(context.Background(), …, 0)`
  - `(*SecretStore).Available() []Binding`; `Bindings()` and `Values()` as before (Values now also returns client secrets and live tokens); `(*SecretStore).Rejected(tenant uuid.UUID, ref string, s Secret)`

- [ ] **Step 1: Write the failing tests** (`internal/worker/oauth2_test.go`, package `worker_test`)

```go
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
	srv     *httptest.Server
	mints   atomic.Int64
	mu      sync.Mutex
	answer  func(n int64) (int, any) // status and JSON body for the n-th mint
	gotForm url.Values
	gotAuth [2]string
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
		p.gotForm, p.gotAuth = r.PostForm, [2]string{id, secret}
		status, body := p.answer(n)
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func oauthStore(t *testing.T, p *idp, c *clock, extra string, opts ...worker.LoadOption) *worker.SecretStore {
	t.Helper()
	body := fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"erp","host":"erp.internal:8443",
		"oauth2":{"token_url":%q,"client_id":"eacp worker","client_secret":%q%s}}]}`,
		tenant, p.srv.URL+"/oauth2/token", canary+"-client", extra)
	opts = append([]worker.LoadOption{worker.AllowPlainTokenURL(), worker.WithClock(c.now)}, opts...)
	s, err := worker.LoadSecrets(secretsFile(t, body), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const endpoint = "https://erp.internal:8443/v1"

func TestAMintedTokenIsReusedWhileItOutlivesTheCall(t *testing.T) {
	p := newIDP(t)
	c := &clock{t: time.Now()}
	s := oauthStore(t, p, c, `,"scope":"erp.purchase erp.read","resource":"https://erp.internal"`)
	ctx := context.Background()
	first, err := s.Credential(ctx, tenant, "erp", endpoint, time.Minute)
	if err != nil || first.Reveal() != "tok-1-"+canary {
		t.Fatalf("first = %v", err)
	}
	if p.gotForm.Get("grant_type") != "client_credentials" || p.gotForm.Get("scope") != "erp.purchase erp.read" ||
		p.gotForm.Get("resource") != "https://erp.internal" {
		t.Fatalf("form = %v", p.gotForm)
	}
	if p.gotAuth != [2]string{"eacp+worker", canary + "-client"} {
		t.Fatalf("basic auth = %q, want the form-urlencoded id and secret", p.gotAuth)
	}
	c.add(5 * time.Minute) // 5 min left of 10
	again, err := s.Credential(ctx, tenant, "erp", endpoint, time.Minute)
	if err != nil || again.Reveal() != first.Reveal() || p.mints.Load() != 1 {
		t.Fatalf("a valid token was not reused: mints=%d err=%v", p.mints.Load(), err)
	}
	if !strings.Contains(strings.Join(s.Values(), ","), first.Reveal()) {
		t.Fatal("Values does not include the live token")
	}
}

func TestAReusedTokenMustOutliveTheCall(t *testing.T) {
	p := newIDP(t)
	c := &clock{t: time.Now()}
	s := oauthStore(t, p, c, "")
	ctx := context.Background()
	if _, err := s.Credential(ctx, tenant, "erp", endpoint, time.Minute); err != nil {
		t.Fatal(err)
	}
	c.add(9*time.Minute + 30*time.Second) // 30 s left, the call needs a minute
	fresh, err := s.Credential(ctx, tenant, "erp", endpoint, time.Minute)
	if err != nil || fresh.Reveal() != "tok-2-"+canary {
		t.Fatalf("expected a fresh mint: %v", err)
	}
	p.answer = func(n int64) (int, any) {
		return 200, map[string]any{"access_token": "short-" + canary, "token_type": "Bearer", "expires_in": 30}
	}
	c.add(9*time.Minute + 30*time.Second)
	if _, err := s.Credential(ctx, tenant, "erp", endpoint, time.Minute); !errors.Is(err, worker.ErrCredentialUnavailable) {
		t.Fatalf("a token living 30 s was used for a 60 s call: %v", err)
	}
}

func TestEveryInvalidTokenResponseIsRefused(t *testing.T) {
	for name, a := range map[string]struct {
		status int
		body   any
	}{
		"error response":      {400, map[string]any{"error": "invalid_client"}},
		"redirect status":     {302, map[string]any{}},
		"not bearer":          {200, map[string]any{"access_token": "x", "token_type": "mac", "expires_in": 60}},
		"no expiry":           {200, map[string]any{"access_token": "x", "token_type": "Bearer"}},
		"expiry as string":    {200, map[string]any{"access_token": "x", "token_type": "Bearer", "expires_in": "60"}},
		"too long lived":      {200, map[string]any{"access_token": "x", "token_type": "Bearer", "expires_in": 3601}},
		"zero lifetime":       {200, map[string]any{"access_token": "x", "token_type": "Bearer", "expires_in": 0}},
		"empty token":         {200, map[string]any{"access_token": "", "token_type": "Bearer", "expires_in": 60}},
		"token with a space":  {200, map[string]any{"access_token": "a b", "token_type": "Bearer", "expires_in": 60}},
		"token too large":     {200, map[string]any{"access_token": strings.Repeat("a", 8193), "token_type": "Bearer", "expires_in": 60}},
		"not an object":       {200, []string{"x"}},
	} {
		t.Run(name, func(t *testing.T) {
			p := newIDP(t)
			p.answer = func(int64) (int, any) { return a.status, a.body }
			s := oauthStore(t, p, &clock{t: time.Now()}, "")
			if _, err := s.Credential(context.Background(), tenant, "erp", endpoint, time.Second); !errors.Is(err, worker.ErrCredentialUnavailable) {
				t.Fatalf("err = %v, want ErrCredentialUnavailable", err)
			}
		})
	}
}

func TestTheTokenEndpointMayNotRedirect(t *testing.T) {
	var elsewhere atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { elsewhere.Add(1) }))
	defer other.Close()
	p := newIDP(t)
	p.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusTemporaryRedirect)
	})
	s := oauthStore(t, p, &clock{t: time.Now()}, "")
	if _, err := s.Credential(context.Background(), tenant, "erp", endpoint, time.Second); !errors.Is(err, worker.ErrCredentialUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if elsewhere.Load() != 0 {
		t.Fatal("the client secret followed a redirect")
	}
}

func TestConcurrentCallersShareOneMint(t *testing.T) {
	p := newIDP(t)
	release := make(chan struct{})
	p.answer = func(n int64) (int, any) {
		<-release
		return 200, map[string]any{"access_token": fmt.Sprintf("tok-%d", n), "token_type": "Bearer", "expires_in": 600}
	}
	s := oauthStore(t, p, &clock{t: time.Now()}, "")
	var wg sync.WaitGroup
	got := make([]string, 8)
	for i := range got {
		wg.Go(func() {
			sec, err := s.Credential(context.Background(), tenant, "erp", endpoint, time.Minute)
			if err == nil {
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
	fail := true
	p.answer = func(n int64) (int, any) {
		if fail {
			return 503, map[string]any{"error": "temporarily_unavailable"}
		}
		return 200, map[string]any{"access_token": "ok", "token_type": "Bearer", "expires_in": 600}
	}
	c := &clock{t: time.Now()}
	s := oauthStore(t, p, c, "")
	ctx := context.Background()
	if len(s.Available()) != 1 {
		t.Fatal("a fresh binding is not available")
	}
	for _, wait := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		if _, err := s.Credential(ctx, tenant, "erp", endpoint, time.Second); !errors.Is(err, worker.ErrCredentialUnavailable) {
			t.Fatal(err)
		}
		before := p.mints.Load()
		if len(s.Available()) != 0 {
			t.Fatal("a backing-off binding is still offered for claims")
		}
		if _, err := s.Credential(ctx, tenant, "erp", endpoint, time.Second); !errors.Is(err, worker.ErrCredentialUnavailable) || p.mints.Load() != before {
			t.Fatal("a backing-off binding called the token endpoint")
		}
		c.add(wait - time.Millisecond)
		if len(s.Available()) != 0 {
			t.Fatalf("available before its back-off of %v passed", wait)
		}
		c.add(time.Millisecond)
	}
	c.add(time.Hour)
	fail = false
	if _, err := s.Credential(ctx, tenant, "erp", endpoint, time.Second); err != nil {
		t.Fatal(err)
	}
	fail = true
	c.add(time.Hour) // the token expired; the next mint fails and backs off from 1 s again
	_, _ = s.Credential(ctx, tenant, "erp", endpoint, time.Second)
	c.add(time.Second)
	if len(s.Available()) != 1 {
		t.Fatal("a success did not reset the back-off")
	}
}

func TestARejectedTokenIsDropped(t *testing.T) {
	p := newIDP(t)
	s := oauthStore(t, p, &clock{t: time.Now()}, "")
	ctx := context.Background()
	first, _ := s.Credential(ctx, tenant, "erp", endpoint, time.Minute)
	s.Rejected(tenant, "erp", worker.Secret{}) // another value: ignored
	if again, _ := s.Credential(ctx, tenant, "erp", endpoint, time.Minute); again.Reveal() != first.Reveal() {
		t.Fatal("a rejection of another value dropped the token")
	}
	s.Rejected(tenant, "erp", first)
	if again, _ := s.Credential(ctx, tenant, "erp", endpoint, time.Minute); again.Reveal() == first.Reveal() {
		t.Fatal("a rejected token was reused")
	}
}

func TestMintedTokensAreRedactedFromTheWorkerLog(t *testing.T) {
	p := newIDP(t)
	set := logging.NewSecretSet()
	s := oauthStore(t, p, &clock{t: time.Now()}, "", worker.WithRedaction(set))
	tok, err := s.Credential(context.Background(), tenant, "erp", endpoint, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	values := strings.Join(set.Values(), ",")
	if !strings.Contains(values, tok.Reveal()) || !strings.Contains(values, canary+"-client") {
		t.Fatalf("the redaction set lacks the token or the client secret")
	}
}

func TestAnOAuthBindingIsStillBoundToItsHost(t *testing.T) {
	p := newIDP(t)
	s := oauthStore(t, p, &clock{t: time.Now()}, "")
	if _, err := s.Credential(context.Background(), tenant, "erp", "https://evil.example/v1", time.Second); !errors.Is(err, worker.ErrNoCredential) {
		t.Fatalf("err = %v", err)
	}
	if p.mints.Load() != 0 {
		t.Fatal("a token was minted for a host the binding does not name")
	}
}
```

Append to `internal/worker/secrets_test.go` (package `worker_test`):

```go
func TestInvalidOAuthEntriesRejectTheWholeFile(t *testing.T) {
	good := `"token_url":"https://idp.example/token","client_id":"eacp","client_secret":"s"`
	for name, oauth := range map[string]string{
		"plain http":          `"token_url":"http://idp.example/token","client_id":"eacp","client_secret":"s"`,
		"user info":           `"token_url":"https://u:p@idp.example/token","client_id":"eacp","client_secret":"s"`,
		"query":               `"token_url":"https://idp.example/token?x=1","client_id":"eacp","client_secret":"s"`,
		"fragment":            `"token_url":"https://idp.example/token#x","client_id":"eacp","client_secret":"s"`,
		"relative":            `"token_url":"/token","client_id":"eacp","client_secret":"s"`,
		"upper-case host":     `"token_url":"https://IDP.example/token","client_id":"eacp","client_secret":"s"`,
		"client id colon":     `"token_url":"https://idp.example/token","client_id":"a:b","client_secret":"s"`,
		"no client id":        `"token_url":"https://idp.example/token","client_secret":"s"`,
		"no client secret":    `"token_url":"https://idp.example/token","client_id":"eacp"`,
		"both secrets":        good + `,"client_secret_file":"/x"`,
		"unreadable file":     `"token_url":"https://idp.example/token","client_id":"eacp","client_secret_file":"/absent/file"`,
		"bad scope":           good + `,"scope":"a  b"`,
		"quoted scope":        good + `,"scope":"a\"b"`,
		"resource fragment":   good + `,"resource":"https://erp.example#x"`,
		"relative resource":   good + `,"resource":"erp"`,
		"unknown field":       good + `,"audience":"x"`,
	} {
		body := fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"erp","host":"erp.example","oauth2":{%s}}]}`, tenant, oauth)
		if _, err := worker.LoadSecrets(secretsFile(t, body)); err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), `"s"`) {
			t.Errorf("%s: error shows a secret: %v", name, err)
		}
	}
	both := fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"erp","host":"erp.example","value":"v","oauth2":{%s}}]}`, tenant, good)
	if _, err := worker.LoadSecrets(secretsFile(t, both)); err == nil {
		t.Error("an entry with a value and oauth2 was accepted")
	}
	ok := fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"erp","host":"erp.example","oauth2":{%s,"scope":"a b","resource":"https://erp.example/api"}}]}`, tenant, good)
	if _, err := worker.LoadSecrets(secretsFile(t, ok)); err != nil {
		t.Errorf("a valid entry was refused: %v", err)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/worker -run 'Token|Mint|OAuth|Redirect|Rejected|Concurrent|BackOff|Outlive'`
Expected: FAIL to compile: `undefined: worker.AllowPlainTokenURL`, `worker.WithClock`, `s.Credential`, `worker.ErrCredentialUnavailable`.

- [ ] **Step 3: Implement.** In `secrets.go`:

- add `ErrCredentialUnavailable`, `CredentialSkew`, `loadConfig{allowPlain bool; redact *logging.SecretSet; now func() time.Time; log *slog.Logger}` and the four `LoadOption` constructors;
- the entry struct gains `OAuth2 *oauthEntry \`json:"oauth2"\`` with `oauthEntry{TokenURL, ClientID string; ClientSecret, ClientSecretFile *string; Scope, Resource string}` (json names `token_url`, `client_id`, `client_secret`, `client_secret_file`, `scope`, `resource`);
- exactly one of `value`, `value_file`, `oauth2` (`"worker: secret %d: exactly one of value, value_file and oauth2 is required"`);
- `secretEntry` becomes `{host string; secret Secret; oauth *oauthProvider}`;
- validation of `oauth2` in a helper `newOAuthProvider(i int, e oauthEntry, b Binding, c loadConfig) (*oauthProvider, error)` in `oauth2.go`, returning errors `worker: secret %d: <field> …` that never contain a value:

```go
var (
	clientIDPattern = regexp.MustCompile(`^[\x21-\x39\x3b-\x7e]{1,256}$`) // printable, no space, no ':'
	scopePattern    = regexp.MustCompile(`^[\x21\x23-\x5b\x5d-\x7e]+( [\x21\x23-\x5b\x5d-\x7e]+)*$`)
	tokenPattern    = regexp.MustCompile(`^[\x21-\x7e]{1,8192}$`)
)

func newOAuthProvider(i int, e oauthEntry, b Binding, c loadConfig) (*oauthProvider, error) {
	bad := func(what string) error { return fmt.Errorf("worker: secret %d: oauth2 %s", i, what) }
	u, err := url.Parse(e.TokenURL)
	if err != nil || !u.IsAbs() || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		strings.Contains(e.TokenURL, "#") || !hostPattern.MatchString(u.Host) {
		return nil, bad("token_url must be an absolute URL with a lowercase host and no user info, query or fragment")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && c.allowPlain) {
		return nil, bad("token_url must be https (http only in development or test)")
	}
	if !clientIDPattern.MatchString(e.ClientID) {
		return nil, bad("client_id must be 1-256 printable characters without spaces or ':'")
	}
	if (e.ClientSecret == nil) == (e.ClientSecretFile == nil) {
		return nil, bad("needs exactly one of client_secret and client_secret_file")
	}
	var secret string
	if e.ClientSecret != nil {
		secret = *e.ClientSecret
	} else {
		raw, err := os.ReadFile(*e.ClientSecretFile)
		if err != nil {
			return nil, bad("client_secret_file cannot be read")
		}
		secret = strings.TrimRight(string(raw), "\r\n")
	}
	if secret == "" || len(secret) > maxSecret {
		return nil, bad(fmt.Sprintf("client secret must be 1-%d bytes", maxSecret))
	}
	if e.Scope != "" && (len(e.Scope) > 1024 || !scopePattern.MatchString(e.Scope)) {
		return nil, bad("scope must be scope tokens separated by single spaces")
	}
	if e.Resource != "" {
		r, err := url.Parse(e.Resource)
		if err != nil || !r.IsAbs() || r.Fragment != "" || strings.Contains(e.Resource, "#") || len(e.Resource) > 2048 {
			return nil, bad("resource must be an absolute URL without fragment")
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	return &oauthProvider{
		binding: b, tokenURL: u.String(), clientID: e.ClientID, clientSecret: Secret{secret},
		scope: e.Scope, resource: e.Resource, now: c.now, redact: c.redact, log: c.log,
		client: &http.Client{Timeout: 10 * time.Second, Transport: transport,
			// A redirect could carry the client secret to another host.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}
```

The provider (`oauth2.go`):

```go
// oauthProvider mints OAuth 2.0 client-credentials tokens (RFC 6749 §4.4)
// for one binding and caches the latest in memory until it expires.
type oauthProvider struct {
	binding                         Binding
	tokenURL, clientID              string
	clientSecret                    Secret
	scope, resource                 string
	client                          *http.Client
	now                             func() time.Time
	redact                          *logging.SecretSet
	log                             *slog.Logger

	mint sync.Mutex // one mint at a time; waiting callers share its token

	mu           sync.Mutex
	token        Secret
	expiry       time.Time
	backoff      time.Duration
	backoffUntil time.Time
}

func (p *oauthProvider) cached(validFor time.Duration) (Secret, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.token.v != "" && p.now().Add(validFor).Before(p.expiry) {
		return p.token, true
	}
	return Secret{}, false
}

func (p *oauthProvider) available(now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !now.Before(p.backoffUntil)
}

func (p *oauthProvider) credential(ctx context.Context, validFor time.Duration) (Secret, error) {
	if s, ok := p.cached(validFor); ok {
		return s, nil
	}
	p.mint.Lock()
	defer p.mint.Unlock()
	if s, ok := p.cached(validFor); ok {
		return s, nil
	}
	if !p.available(p.now()) {
		return Secret{}, ErrCredentialUnavailable
	}
	tok, expiry, class := p.request(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	if class == "" && !p.now().Add(validFor).Before(expiry) {
		class = "lifetime_shorter_than_call"
	}
	if class != "" {
		p.backoff = min(max(2*p.backoff, time.Second), time.Minute)
		p.backoffUntil = p.now().Add(p.backoff)
		p.log.WarnContext(ctx, "credential mint failed", "tenant", p.binding.TenantID.String(),
			"secret_ref", p.binding.Ref, "token_host", hostOf(p.tokenURL), "class", class, "backoff", p.backoff)
		if class == "lifetime_shorter_than_call" { // still usable for shorter calls
			p.token, p.expiry = tok, expiry
			p.redactUntil(tok, expiry)
		}
		return Secret{}, ErrCredentialUnavailable
	}
	p.backoff, p.backoffUntil = 0, time.Time{}
	p.token, p.expiry = tok, expiry
	p.redactUntil(tok, expiry)
	p.log.InfoContext(ctx, "credential minted", "tenant", p.binding.TenantID.String(),
		"secret_ref", p.binding.Ref, "token_host", hostOf(p.tokenURL), "expires_in", expiry.Sub(p.now()).Round(time.Second))
	return tok, nil
}

func (p *oauthProvider) redactUntil(tok Secret, expiry time.Time) {
	if p.redact != nil {
		p.redact.Add(tok.v, expiry.Add(24*time.Hour))
	}
}

// request performs one token request. It returns a failure class, never a
// response body or secret.
func (p *oauthProvider) request(ctx context.Context) (Secret, time.Time, string) {
	form := url.Values{"grant_type": {"client_credentials"}}
	if p.scope != "" {
		form.Set("scope", p.scope)
	}
	if p.resource != "" {
		form.Set("resource", p.resource)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Secret{}, time.Time{}, "request"
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// RFC 6749 §2.3.1: the id and secret are form-urlencoded before Basic auth.
	req.SetBasicAuth(url.QueryEscape(p.clientID), url.QueryEscape(p.clientSecret.v))
	start := p.now()
	resp, err := p.client.Do(req)
	if err != nil {
		return Secret{}, time.Time{}, "transport"
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
	if err != nil || len(body) > 64<<10 {
		return Secret{}, time.Time{}, "response_unreadable"
	}
	if resp.StatusCode != http.StatusOK {
		return Secret{}, time.Time{}, fmt.Sprintf("http_%d", resp.StatusCode)
	}
	var tr struct {
		AccessToken string      `json:"access_token"`
		TokenType   string      `json:"token_type"`
		ExpiresIn   json.Number `json:"expires_in"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&tr); err != nil {
		return Secret{}, time.Time{}, "invalid_json"
	}
	if !tokenPattern.MatchString(tr.AccessToken) {
		return Secret{}, time.Time{}, "invalid_access_token"
	}
	if !strings.EqualFold(tr.TokenType, "Bearer") {
		return Secret{}, time.Time{}, "not_bearer"
	}
	n, err := tr.ExpiresIn.Int64()
	if err != nil || n < 1 || n > 3600 {
		return Secret{}, time.Time{}, "invalid_expires_in"
	}
	return Secret{tr.AccessToken}, start.Add(time.Duration(n) * time.Second), ""
}

func (p *oauthProvider) rejected(s Secret) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s.v != "" && p.token.v == s.v {
		p.token, p.expiry = Secret{}, time.Time{}
	}
}

func (p *oauthProvider) live() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []string{p.clientSecret.v}
	if p.token.v != "" && p.now().Before(p.expiry) {
		out = append(out, p.token.v)
	}
	return out
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}
```

In `secrets.go`, `SecretStore` gains `now func() time.Time`; `LoadSecrets` applies options (defaults: `now = time.Now`, `log = slog.New(slog.DiscardHandler)`), builds oauth entries with `newOAuthProvider`, and adds each client secret with `c.redact.AddPermanent` when a set is given. New methods:

```go
// Credential returns a credential for tenant's secret reference at endpoint
// that stays valid for at least validFor. ErrNoCredential: no binding for
// this tenant, reference and exact host. ErrCredentialUnavailable: the
// provider cannot produce one now.
func (s *SecretStore) Credential(ctx context.Context, tenant uuid.UUID, ref, endpoint string,
	validFor time.Duration) (Secret, error) {
	if s == nil {
		return Secret{}, ErrNoCredential
	}
	e, ok := s.m[secretKey{tenant, ref}]
	if !ok || e.host != endpointHost(endpoint) {
		return Secret{}, ErrNoCredential
	}
	if e.oauth != nil {
		return e.oauth.credential(ctx, validFor)
	}
	return e.secret, nil
}

// Resolve is Credential for a credential that only needs to be valid now.
func (s *SecretStore) Resolve(tenant uuid.UUID, ref, endpoint string) (Secret, error) {
	return s.Credential(context.Background(), tenant, ref, endpoint, 0)
}

// Available lists the bindings the worker can serve now: every static one
// and each OAuth binding that is not backing off after a failed mint.
func (s *SecretStore) Available() []Binding {
	var out []Binding
	for _, b := range s.Bindings() {
		if e := s.m[secretKey{b.TenantID, b.Ref}]; e.oauth == nil || e.oauth.available(s.now()) {
			out = append(out, b)
		}
	}
	return out
}

// Rejected tells the provider the target refused s; an OAuth provider drops
// it so the next attempt mints a new one. The attempt's outcome is
// unchanged.
func (s *SecretStore) Rejected(tenant uuid.UUID, ref string, secret Secret) {
	if s == nil {
		return
	}
	if e, ok := s.m[secretKey{tenant, ref}]; ok && e.oauth != nil {
		e.oauth.rejected(secret)
	}
}
```

`Values()` returns static values plus `e.oauth.live()` for OAuth entries. `Bindings()` is unchanged (every entry).

- [ ] **Step 4: Run the worker unit tests**

Run: `go test -race ./internal/worker -run 'Token|Mint|OAuth|Redirect|Rejected|Concurrent|BackOff|Outlive|Secrets|Invalid'`
Expected: PASS. Then `go test -race ./internal/worker ./internal/connector/... ./internal/fakemcp ./internal/messaging` — PASS (static callers unchanged).

- [ ] **Step 5: Commit**

```bash
git add internal/worker/oauth2.go internal/worker/oauth2_test.go internal/worker/secrets.go internal/worker/secrets_test.go
git commit -m "feat(worker): OAuth 2.0 client-credentials provider - short-lived tokens, reuse only while they outlive the call, back-off"
```

---

### Task 3: Fake ERP issues and accepts short-lived tokens

**Files:**
- Modify: `internal/fakeerp/erp.go` (Options, token endpoint, token principals, audit fields)
- Modify: `cmd/fakeerp/main.go` (OAuth env)
- Test: `internal/fakeerp/oauth_test.go`, `cmd/fakeerp/main_test.go`

**Interfaces:**
- Produces: `fakeerp.Options{OAuthClientID, OAuthClientSecret string; TokenTTL time.Duration}`; `fakeerp.NewWithOptions(token, dataPath string, o Options) (http.Handler, error)`; `New(token, dataPath)` = `NewWithOptions(token, dataPath, Options{})`. Audit entries gain `token_sha256` and `expires_at`. Principal of a minted token: `oauth:<client_id>`. Route `POST /oauth/token` only when `OAuthClientID` is set.

- [ ] **Step 1: Write the failing tests** (`internal/fakeerp/oauth_test.go`, package `fakeerp_test`; read `erp_test.go` first and reuse its helpers for POSTing `/v1/execute` — the snippet below uses local helpers so it stands alone)

```go
package fakeerp_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"eacp/internal/fakeerp"
)

const static = "static-erp-token"

func oauthERP(t *testing.T, ttl time.Duration, path string) *httptest.Server {
	t.Helper()
	h, err := fakeerp.NewWithOptions(static, path, fakeerp.Options{
		OAuthClientID: "eacp worker", OAuthClientSecret: "client s3cret", TokenTTL: ttl})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func mint(t *testing.T, srv *httptest.Server, id, secret, grant string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/oauth/token", strings.NewReader(url.Values{"grant_type": {grant}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(url.QueryEscape(id), url.QueryEscape(secret))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

func execute(t *testing.T, srv *httptest.Server, bearer string) int {
	t.Helper()
	tenant := uuid.New()
	key := "eacp:" + tenant.String() + ":" + uuid.NewString()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/execute",
		strings.NewReader(`{"tool":"erp.create_po","payload":{"amount":1}}`))
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("X-EACP-Tenant-ID", tenant.String())
	req.Header.Set("Idempotency-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

type entry struct {
	Principal   string     `json:"principal"`
	Path        string     `json:"path"`
	Outcome     string     `json:"outcome"`
	TokenSHA256 string     `json:"token_sha256"`
	ExpiresAt   *time.Time `json:"expires_at"`
}

func auditOf(t *testing.T, srv *httptest.Server) []entry {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+"/v1/audit", nil)
	req.Header.Set("Authorization", "Bearer "+static)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []entry
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAMintedTokenAuthorisesUntilItExpires(t *testing.T) {
	srv := oauthERP(t, 2*time.Second, filepath.Join(t.TempDir(), "erp.log"))
	status, body := mint(t, srv, "eacp worker", "client s3cret", "client_credentials")
	tok, _ := body["access_token"].(string)
	if status != 200 || len(tok) != 43 || body["token_type"] != "Bearer" || body["expires_in"] != float64(2) {
		t.Fatalf("mint = %d %v", status, body)
	}
	if code := execute(t, srv, tok); code != 200 {
		t.Fatalf("execute with a fresh token = %d", code)
	}
	var issued, executed bool
	sum := sha256.Sum256([]byte(tok))
	for _, e := range auditOf(t, srv) {
		if e.Path == "/oauth/token" && e.Outcome == "token_issued" && e.Principal == "oauth:eacp worker" &&
			e.TokenSHA256 == hex.EncodeToString(sum[:]) && e.ExpiresAt != nil {
			issued = true
		}
		if e.Path == "/v1/execute" && e.Principal == "oauth:eacp worker" && e.Outcome == "effect_committed" {
			executed = true
		}
		if strings.Contains(e.TokenSHA256+e.Principal, tok) {
			t.Fatal("the audit holds the token itself")
		}
	}
	if !issued || !executed {
		t.Fatalf("audit lacks issuance (%v) or the oauth principal (%v)", issued, executed)
	}
	time.Sleep(2100 * time.Millisecond)
	if code := execute(t, srv, tok); code != 401 {
		t.Fatalf("execute with an expired token = %d, want 401", code)
	}
	if code := execute(t, srv, static); code != 200 {
		t.Fatalf("the static credential stopped working: %d", code)
	}
}

func TestTheTokenEndpointRefusesBadClientsAndGrants(t *testing.T) {
	srv := oauthERP(t, time.Minute, filepath.Join(t.TempDir(), "erp.log"))
	if status, body := mint(t, srv, "eacp worker", "wrong", "client_credentials"); status != 401 || body["error"] != "invalid_client" {
		t.Fatalf("wrong secret = %d %v", status, body)
	}
	if status, body := mint(t, srv, "other", "client s3cret", "client_credentials"); status != 401 || body["error"] != "invalid_client" {
		t.Fatalf("wrong client = %d %v", status, body)
	}
	if status, body := mint(t, srv, "eacp worker", "client s3cret", "password"); status != 400 || body["error"] != "unsupported_grant_type" {
		t.Fatalf("wrong grant = %d %v", status, body)
	}
	if code := execute(t, srv, "not-a-token"); code != 401 {
		t.Fatalf("unknown token = %d", code)
	}
}

func TestMintedTokensSurviveARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "erp.log")
	srv := oauthERP(t, time.Minute, path)
	_, body := mint(t, srv, "eacp worker", "client s3cret", "client_credentials")
	srv.Close()
	again := oauthERP(t, time.Minute, path)
	if code := execute(t, again, body["access_token"].(string)); code != 200 {
		t.Fatalf("a token issued before the restart = %d", code)
	}
}

func TestWithoutAnOAuthClientThereIsNoTokenEndpoint(t *testing.T) {
	h, err := fakeerp.New(static, filepath.Join(t.TempDir(), "erp.log"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	if status, _ := mint(t, srv, "eacp worker", "client s3cret", "client_credentials"); status != 404 && status != 405 {
		t.Fatalf("token endpoint without a client = %d", status)
	}
}
```

Append to `cmd/fakeerp/main_test.go` a table test for `loadOAuth(getenv)` (read the file first; follow its style):

```go
func TestOAuthSettingsFailClosed(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "client")
	if err := os.WriteFile(secret, []byte("client-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	o, err := loadOAuth(env(map[string]string{}))
	if err != nil || o.OAuthClientID != "" {
		t.Fatalf("no settings = %+v, %v", o, err)
	}
	o, err = loadOAuth(env(map[string]string{"EACP_FAKEERP_OAUTH_CLIENT_ID": "eacp-worker",
		"EACP_FAKEERP_OAUTH_CLIENT_SECRET_FILE": secret}))
	if err != nil || o.OAuthClientSecret != "client-secret" || o.TokenTTL != 300*time.Second {
		t.Fatalf("defaults = %+v, %v", o, err)
	}
	for name, m := range map[string]map[string]string{
		"id without secret": {"EACP_FAKEERP_OAUTH_CLIENT_ID": "x"},
		"secret without id": {"EACP_FAKEERP_OAUTH_CLIENT_SECRET_FILE": secret},
		"unreadable secret": {"EACP_FAKEERP_OAUTH_CLIENT_ID": "x", "EACP_FAKEERP_OAUTH_CLIENT_SECRET_FILE": filepath.Join(dir, "absent")},
		"ttl too long":      {"EACP_FAKEERP_OAUTH_CLIENT_ID": "x", "EACP_FAKEERP_OAUTH_CLIENT_SECRET_FILE": secret, "EACP_FAKEERP_OAUTH_TTL": "2h"},
		"ttl not duration":  {"EACP_FAKEERP_OAUTH_CLIENT_ID": "x", "EACP_FAKEERP_OAUTH_CLIENT_SECRET_FILE": secret, "EACP_FAKEERP_OAUTH_TTL": "soon"},
	} {
		if _, err := loadOAuth(env(m)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/fakeerp ./cmd/fakeerp`
Expected: FAIL to compile: `undefined: fakeerp.NewWithOptions`, `undefined: loadOAuth`.

- [ ] **Step 3: Implement.** In `erp.go`:

- `audit` gains `TokenSHA256 string \`json:"token_sha256,omitempty"\`` and `ExpiresAt *time.Time \`json:"expires_at,omitempty"\``.
- `ERP` gains `oauth Options` and `tokens map[string]issued` (`issued{principal string; expires time.Time}`), keyed by hex SHA-256.
- `apply(ev)`: when `ev.Audit.Outcome == "token_issued"` and `ExpiresAt != nil`, record `tokens[ev.Audit.TokenSHA256] = issued{ev.Audit.Principal, *ev.Audit.ExpiresAt}` (so tokens survive a restart, like every effect).
- `principal(r)`: the static token → `execution-worker`; otherwise hash the bearer value and, if it is in `tokens` with `time.Now().Before(expires)`, return its principal (`oauth:<id>`); else `unauthenticated`.
- `func privileged(p string) bool { return p == "execution-worker" || strings.HasPrefix(p, "oauth:") }` replaces every `e.principal(r) != "execution-worker"` check (three sites: execute, lookup, audit list).
- `NewWithOptions` validates: `OAuthClientID` and `OAuthClientSecret` both set or both empty; `TokenTTL` 0 → 300 s, else 1 s–3600 s; registers `mux.HandleFunc("POST /oauth/token", e.issue)` only with a client.
- `issue`:

```go
// issue is an OAuth 2.0 client-credentials token endpoint (RFC 6749 §4.4).
// It stores only each token's SHA-256 and expiry, in the durable log.
func (e *ERP) issue(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	id, secret, ok := r.BasicAuth()
	var err1, err2 error
	id, err1 = url.QueryUnescape(id)
	secret, err2 = url.QueryUnescape(secret)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.failed {
		errorJSON(w, 503, "operation_log_unavailable")
		return
	}
	now := time.Now().UTC()
	ev := event{Audit: audit{At: now, Principal: "unauthenticated", Method: r.Method, Path: "/oauth/token"}}
	if !ok || err1 != nil || err2 != nil || id != e.oauth.OAuthClientID ||
		subtle.ConstantTimeCompare([]byte(secret), []byte(e.oauth.OAuthClientSecret)) != 1 {
		ev.Audit.Outcome = "invalid_client"
		if !e.log(ev) {
			errorJSON(w, 503, "audit_unavailable")
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="fakeerp"`)
		writeJSON(w, 401, map[string]string{"error": "invalid_client"})
		return
	}
	ev.Audit.Principal = "oauth:" + id
	if r.ParseForm() != nil || r.PostForm.Get("grant_type") != "client_credentials" {
		ev.Audit.Outcome = "unsupported_grant_type"
		if !e.log(ev) {
			errorJSON(w, 503, "audit_unavailable")
			return
		}
		writeJSON(w, 400, map[string]string{"error": "unsupported_grant_type"})
		return
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		errorJSON(w, 503, "token_unavailable")
		return
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	expires := now.Add(e.oauth.TokenTTL).Truncate(time.Second)
	ev.Audit.Outcome, ev.Audit.TokenSHA256, ev.Audit.ExpiresAt = "token_issued", hex.EncodeToString(sum[:]), &expires
	if !e.log(ev) {
		errorJSON(w, 503, "audit_unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"access_token": token, "token_type": "Bearer",
		"expires_in": int(e.oauth.TokenTTL / time.Second)})
}
```

(Imports: `crypto/rand`, `crypto/sha256`, `encoding/base64`, `encoding/hex`, `net/url`.)

In `cmd/fakeerp/main.go`:

```go
// loadOAuth reads the optional OAuth client of the token endpoint. The id
// and secret file come together; the TTL defaults to 300s (1s-1h).
func loadOAuth(getenv func(string) string) (fakeerp.Options, error) {
	id, file := getenv("EACP_FAKEERP_OAUTH_CLIENT_ID"), getenv("EACP_FAKEERP_OAUTH_CLIENT_SECRET_FILE")
	if id == "" && file == "" {
		return fakeerp.Options{}, nil
	}
	if id == "" || file == "" {
		return fakeerp.Options{}, errors.New("fakeerp: EACP_FAKEERP_OAUTH_CLIENT_ID and EACP_FAKEERP_OAUTH_CLIENT_SECRET_FILE go together")
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return fakeerp.Options{}, errors.New("fakeerp: cannot read the OAuth client secret file")
	}
	secret := strings.TrimRight(string(b), "\r\n")
	if secret == "" || len(secret) > 4096 {
		return fakeerp.Options{}, errors.New("fakeerp: invalid OAuth client secret file")
	}
	ttl := 300 * time.Second
	if v := getenv("EACP_FAKEERP_OAUTH_TTL"); v != "" {
		if ttl, err = time.ParseDuration(v); err != nil || ttl < time.Second || ttl > time.Hour {
			return fakeerp.Options{}, errors.New("fakeerp: EACP_FAKEERP_OAUTH_TTL must be 1s-1h")
		}
	}
	return fakeerp.Options{OAuthClientID: id, OAuthClientSecret: secret, TokenTTL: ttl}, nil
}
```

`loadHandler` takes the options and calls `NewWithOptions`; `main` calls `loadOAuth(os.Getenv)` and `d.RedactSecrets(token, o.OAuthClientSecret)`.

- [ ] **Step 4: Run**

Run: `go test -race ./internal/fakeerp ./cmd/fakeerp ./internal/worker ./internal/connector/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/fakeerp cmd/fakeerp
git commit -m "feat(fakeerp): an OAuth client-credentials token endpoint; short-lived tokens authorise until they expire"
```

---

### Task 4: The worker, reconciler and scanner use JIT credentials

**Files:**
- Modify: `internal/worker/store.go` (`Job.CallTimeout` from `eacp.call_timeout`)
- Modify: `internal/worker/worker.go`, `internal/worker/reconciler.go`, `internal/worker/scanner.go`
- Modify: `cmd/execution-worker/main.go`
- Test: `internal/worker/jit_integration_test.go`

**Interfaces:**
- Consumes: Task 2's `Credential`, `Available`, `Rejected`, `Values`, `CredentialSkew`, `ErrCredentialUnavailable`, `LoadOption`s; Task 3's `fakeerp.NewWithOptions`, `fakeerp.Options`; Task 1's `(*service.Deps).Redaction()`.
- Produces: `Job.CallTimeout time.Duration`; release reason `credential unavailable`.

- [ ] **Step 1: Write the failing tests** (`internal/worker/jit_integration_test.go`, package `worker_test`; reuse `httpContractSQL`, `secretsFile`, `canary` from the package's other test files)

```go
package worker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/action"
	"eacp/internal/connector"
	"eacp/internal/fakeerp"
	"eacp/internal/governance"
	"eacp/internal/logging"
	"eacp/internal/registry/registrytest"
	"eacp/internal/storage"
	"eacp/internal/storage/pgtest"
	"eacp/internal/worker"
)

// jitEnv is a tenant whose ERP connector authenticates with tokens minted by
// the Fake ERP's token endpoint, behind a switch that can make it fail.
type jitEnv struct {
	f       *registrytest.Fixture
	srv     *httptest.Server
	engine  *action.Engine
	agent   registrytest.Agent
	secrets *worker.SecretStore
	w       *worker.Worker
	log     *bytes.Buffer
	failing atomic.Bool
	mints   atomic.Int64
	mu      sync.Mutex
	tokens  []string // every token the endpoint issued
	clock   *clock
}

func newJIT(t *testing.T, ttl time.Duration, timeoutMS int) *jitEnv {
	t.Helper()
	v := &jitEnv{clock: &clock{t: time.Now()}}
	h, err := fakeerp.NewWithOptions(canary+"-static", filepath.Join(t.TempDir(), "erp.log"),
		fakeerp.Options{OAuthClientID: "eacp-worker", OAuthClientSecret: canary + "-client", TokenTTL: ttl})
	if err != nil {
		t.Fatal(err)
	}
	v.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			h.ServeHTTP(w, r)
			return
		}
		v.mints.Add(1)
		if v.failing.Load() {
			http.Error(w, `{"error":"temporarily_unavailable"}`, 503)
			return
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		var body struct {
			AccessToken string `json:"access_token"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		v.mu.Lock()
		v.tokens = append(v.tokens, body.AccessToken)
		v.mu.Unlock()
		for k, vals := range rec.Header() {
			w.Header()[k] = vals
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	}))
	t.Cleanup(v.srv.Close)
	u, _ := url.Parse(v.srv.URL)
	v.f = registrytest.New(t)
	connID := v.f.ID(t, "erin", `INSERT INTO eacp.connectors (tenant_id, name, protocol, endpoint, secret_ref)
		VALUES (eacp.current_tenant_id(), 'erp', 'http', $1, 'erp-jit') RETURNING id`, v.srv.URL)
	toolID := v.f.ID(t, "erin", `INSERT INTO eacp.tools (tenant_id, connector_id, name)
		VALUES (eacp.current_tenant_id(), $1, 'create_po') RETURNING id`, connID)
	contractID := v.f.ID(t, "erin", strings.Replace(httpContractSQL, "2, 100)", fmt.Sprintf("2, %d)", timeoutMS), 1), toolID)
	if err := v.f.Exec("rita", `UPDATE eacp.tools SET active_contract_id = $1 WHERE id = $2`, contractID, toolID); err != nil {
		t.Fatal(err)
	}
	v.agent = v.f.ActiveAgent(t, "buyer", toolID)
	v.f.ActivatePolicy(t, registrytest.AllowPolicy)
	set := logging.NewSecretSet()
	v.log = &bytes.Buffer{}
	logger := logging.NewWithSet(syncWriter{w: v.log}, slog.LevelDebug, "json", set)
	v.secrets, err = worker.LoadSecrets(secretsFile(t, fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"erp-jit","host":%q,
		"oauth2":{"token_url":%q,"client_id":"eacp-worker","client_secret":%q,"scope":"erp.purchase"}}]}`,
		pgtest.TenantA, u.Host, v.srv.URL+"/oauth/token", canary+"-client")),
		worker.AllowPlainTokenURL(), worker.WithRedaction(set), worker.WithClock(v.clock.now), worker.WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}
	v.w, err = worker.New(v.f.App, worker.Options{ID: "jit", Lease: 5 * time.Second, Log: logger,
		Connectors: map[string]worker.Connector{"http": connector.NewHTTP()}, Secrets: v.secrets,
		Backoff: func(int) time.Duration { return time.Second }})
	if err != nil {
		t.Fatal(err)
	}
	v.engine = action.New(v.f.App, action.Options{Provider: governance.LocalProvider{InstanceID: "jit-test"}})
	return v
}

type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s syncWriter) Write(p []byte) (int, error) { return s.w.Write(p) }

func (v *jitEnv) submit(t *testing.T, scenario string) action.View {
	t.Helper()
	payload := map[string]any{"amount": 7, "currency": "THB"}
	if scenario != "" {
		payload["scenario"], payload["delay_ms"] = scenario, 300
	}
	b, _ := json.Marshal(payload)
	view, err := v.engine.Submit(context.Background(), action.Agent(v.f.Tenant, v.agent.Agent, v.agent.Version),
		action.Submission{IdempotencyKey: uuid.NewString(), Subject: "carol@tenant-a.test", Operation: "post",
			Target: "erp", Tool: "erp.create_po", ToolSchemaVersion: "1", Resource: "po", Payload: b})
	if err != nil || view.State != "QUEUED" {
		t.Fatalf("submit = %+v, %v", view, err)
	}
	return view
}

func (v *jitEnv) get(t *testing.T, id uuid.UUID) action.View {
	t.Helper()
	got, err := v.engine.Get(context.Background(), v.f.Tenant, id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// principals returns the ERP audit's principal per path and outcome.
func (v *jitEnv) audit(t *testing.T) []map[string]any {
	t.Helper()
	req, _ := http.NewRequest("GET", v.srv.URL+"/v1/audit", nil)
	req.Header.Set("Authorization", "Bearer "+canary+"-static")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTheWorkerExecutesWithAMintedToken(t *testing.T) {
	v := newJIT(t, 10*time.Minute, 100)
	ctx := context.Background()
	var ids []uuid.UUID
	for range 3 {
		ids = append(ids, v.submit(t, "").ID)
	}
	for range 3 {
		if n, err := v.w.RunOnce(ctx); err != nil || n != 1 {
			t.Fatalf("run = %d, %v", n, err)
		}
	}
	for _, id := range ids {
		if got := v.get(t, id); got.State != "SUCCEEDED" || got.AttemptCount != 1 {
			t.Fatalf("action = %+v", got)
		}
	}
	if v.mints.Load() != 1 {
		t.Fatalf("%d mints for 3 calls, want one token reused", v.mints.Load())
	}
	executes := 0
	for _, e := range v.audit(t) {
		if e["path"] == "/v1/execute" {
			executes++
			if e["principal"] != "oauth:eacp-worker" {
				t.Fatalf("an execute used principal %v, not the minted token", e["principal"])
			}
		}
	}
	if executes != 3 {
		t.Fatalf("%d executes", executes)
	}
	v.mu.Lock()
	tokens := append([]string{canary + "-client"}, v.tokens...)
	v.mu.Unlock()
	for _, tok := range tokens {
		for _, sql := range []string{
			`SELECT count(*) FROM eacp.actions WHERE to_jsonb(actions)::text LIKE '%' || $1 || '%'`,
			`SELECT count(*) FROM eacp.action_attempts WHERE to_jsonb(action_attempts)::text LIKE '%' || $1 || '%'`,
			`SELECT count(*) FROM eacp.audit_events WHERE convert_from(payload, 'UTF8') LIKE '%' || $1 || '%'`,
			`SELECT count(*) FROM eacp.outbox_events WHERE to_jsonb(outbox_events)::text LIKE '%' || $1 || '%'`,
		} {
			var count int
			if err := storage.InTenantTx(ctx, v.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, sql, tok).Scan(&count)
			}); err != nil || count != 0 {
				t.Fatalf("a credential was persisted (count=%d err=%v)", count, err)
			}
		}
		if strings.Contains(v.log.String(), tok) {
			t.Fatal("a credential reached the worker log")
		}
	}
	if !strings.Contains(v.log.String(), "credential minted") {
		t.Fatal("the mint was not logged")
	}
}

func TestATokenShorterThanTheCallNeverDispatches(t *testing.T) {
	v := newJIT(t, 5*time.Second, 100) // the call needs 100 ms + 30 s
	view := v.submit(t, "")
	if n, err := v.w.RunOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("run = %d, %v", n, err)
	}
	got := v.get(t, view.ID)
	if got.State != "QUEUED" || got.StateReason != "credential unavailable" || got.AttemptCount != 0 {
		t.Fatalf("action = %+v", got)
	}
	for _, e := range v.audit(t) {
		if e["path"] == "/v1/execute" {
			t.Fatal("a call was made with a token that could expire during it")
		}
	}
}

func TestAFailingTokenEndpointWithholdsClaimsUntilTheBackOffPasses(t *testing.T) {
	v := newJIT(t, 10*time.Minute, 100)
	ctx := context.Background()
	v.failing.Store(true)
	view := v.submit(t, "")
	if n, _ := v.w.RunOnce(ctx); n != 1 {
		t.Fatalf("first run claimed %d", n)
	}
	if got := v.get(t, view.ID); got.State != "QUEUED" || got.StateReason != "credential unavailable" || got.AttemptCount != 0 {
		t.Fatalf("action = %+v", got)
	}
	mints := v.mints.Load()
	for range 3 {
		if n, _ := v.w.RunOnce(ctx); n != 0 {
			t.Fatal("the worker claimed work for a binding that is backing off")
		}
	}
	if v.mints.Load() != mints {
		t.Fatal("the token endpoint was called during the back-off")
	}
	v.failing.Store(false)
	v.clock.add(2 * time.Second)
	if n, err := v.w.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("run after the back-off = %d, %v", n, err)
	}
	if got := v.get(t, view.ID); got.State != "SUCCEEDED" {
		t.Fatalf("action = %+v", got)
	}
}

func TestTheReconcilerLooksUpWithAMintedToken(t *testing.T) {
	v := newJIT(t, 10*time.Minute, 100)
	ctx := context.Background()
	view := v.submit(t, "execute_then_timeout")
	if n, _ := v.w.RunOnce(ctx); n != 1 {
		t.Fatal("not claimed")
	}
	if got := v.get(t, view.ID); got.State != "UNKNOWN_OUTCOME" {
		t.Fatalf("action = %+v", got)
	}
	r, err := worker.NewReconciler(v.f.App, worker.ReconcilerOptions{ID: "jit-r", Lease: 5 * time.Second,
		Connectors: map[string]worker.Connector{"http": connector.NewHTTP()}, Secrets: v.secrets,
		MaxAttempts: 3, MaxAge: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for v.get(t, view.ID).State != "SUCCEEDED" {
		if time.Now().After(deadline) {
			t.Fatalf("action = %+v", v.get(t, view.ID))
		}
		_, _ = r.RunOnce(ctx)
		time.Sleep(200 * time.Millisecond)
	}
	var lookups int
	for _, e := range v.audit(t) {
		if e["path"] == "/v1/operations/{key}" {
			lookups++
			if e["principal"] != "oauth:eacp-worker" {
				t.Fatalf("a lookup used principal %v", e["principal"])
			}
		}
	}
	if lookups == 0 {
		t.Fatal("no lookup reached the ERP")
	}
}
```

(`syncWriter` must lock: write `func (s *syncWriter) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.w.Write(p) }` and pass `&syncWriter{w: v.log}`; read `v.log.String()` only after the worker is idle. Check `registrytest` for the fixture/agent type names — `registrytest.Fixture`, `ActiveAgent`'s return type — and `ReconcilerOptions` field names in `reconciler.go`, and adjust the struct field types to them.)

- [ ] **Step 2: Run to verify they fail**

Run: `EACP_TEST_ADMIN_DSN=... go test ./internal/worker -run 'Minted|ShorterThanTheCall|FailingTokenEndpoint|ReconcilerLooksUp'`
Expected: FAIL: the worker resolves with `Resolve` (validFor 0), so the short token is dispatched (`a call was made…`), and claims continue during the back-off.

- [ ] **Step 3: Implement.**

`store.go`: `Job` gains `CallTimeout time.Duration // eacp.call_timeout of the pinned contract`; `Load` selects `extract(epoch FROM eacp.call_timeout(a.connector_contract_id))::float8` into a `float64` and sets `j.CallTimeout = time.Duration(t * float64(time.Second))`.

`worker.go` `execute`:

```go
	conn := w.o.Connectors[job.Protocol]
	if conn == nil {
		log.WarnContext(exec, "worker cannot serve this connector; releasing", "protocol", job.Protocol)
		w.release(exec, l, log, "worker cannot serve this connector")
		return
	}
	// The credential must outlive the whole call (ADR-019).
	secret, err := w.o.Secrets.Credential(exec, l.TenantID, job.SecretRef, job.Endpoint, job.CallTimeout+CredentialSkew)
	if err != nil {
		reason := "worker cannot serve this connector"
		if errors.Is(err, ErrCredentialUnavailable) {
			reason = "credential unavailable"
		}
		log.WarnContext(exec, "no credential; releasing", "reason", reason)
		w.release(exec, l, log, reason)
		return
	}
```

After `res = classify(scrub(res, w.o.Secrets.Values()), job.Contract)` add:

```go
	if res.ErrorClass == "unauthorized" {
		w.o.Secrets.Rejected(l.TenantID, job.SecretRef, secret) // the outcome stays as classified
	}
```

In `claim`, pass `w.o.Secrets.Available()` instead of `w.o.Secrets.Bindings()`.

`reconciler.go`: `Reconcilable(..., r.o.Secrets.Available(), ...)`; in `reconcile`:

```go
	budget := min(job.Timeout, r.o.Lease/2)
	secret, err := r.o.Secrets.Credential(ctx, l.TenantID, job.SecretRef, job.Endpoint, budget+CredentialSkew)
	if errors.Is(err, ErrCredentialUnavailable) {
		// Nothing was looked up, so nothing is decided: the lease lapses and
		// the action waits for its next reconciliation (T33).
		log.WarnContext(ctx, "credential unavailable; not reconciling now")
		return
	}
	if conn != nil && err == nil {
		lookupCtx, cancel := context.WithTimeout(ctx, budget)
		...
```

`scanner.go`: `ScansDue(ctx, s.o.Secrets.Available(), ...)`; in `scan`:

```go
	secret, err := s.o.Secrets.Credential(ctx, l.TenantID, l.SecretRef, l.Endpoint, s.o.Timeout+CredentialSkew)
	if errors.Is(err, ErrCredentialUnavailable) {
		log.WarnContext(ctx, "credential unavailable; scan not recorded")
		return // the scan lease expires and the scan is retried
	}
	if err != nil { ...existing no_credential path... }
```

`cmd/execution-worker/main.go`:

```go
				opts := []worker.LoadOption{worker.WithRedaction(d.Redaction()), worker.WithLogger(d.Log)}
				if env := d.Config.Environment; env == "development" || env == "test" {
					opts = append(opts, worker.AllowPlainTokenURL())
				}
				if secrets, err = worker.LoadSecrets(path, opts...); err != nil {
					return err
				}
				d.RedactSecrets(secrets.Values()...)
```

- [ ] **Step 4: Run the worker suite**

Run: `EACP_TEST_ADMIN_DSN=... go test -race -count=1 ./internal/worker ./cmd/...`
Expected: PASS (new tests and every existing worker test).

- [ ] **Step 5: Commit**

```bash
git add internal/worker cmd/execution-worker
git commit -m "feat(worker): execute, reconcile and scan with credentials that outlive the call; withhold claims while a token endpoint backs off"
```

---

### Task 5: Compose, Kubernetes and the JIT demo

**Files:**
- Modify: `deployments/docker/secrets/connector-secrets.dev.json` (tenant `00000000-0000-4000-8000-0000000000a4`, `fakeerp-jit`)
- Modify: `deployments/docker/secrets/prepare_fakeerp_token.py` (writes `fakeerp-oauth-client.dev`)
- Modify: `.gitignore` (the new generated file, next to the other `*.dev` tokens — check how they are ignored)
- Modify: `docker-compose.yml` (fakeerp OAuth env and secret)
- Modify: `deployments/k8s/dev/fakes.yaml`, `scripts/k8s-e2e.sh` (the same for Kubernetes; default `TESTS` adds `TestJITDemo`)
- Modify: `scripts/demo.sh` (`DEMO` letters `A`, `C`, `J`)
- Create: `test/demo/jit_test.go`
- Modify: `test/demo/demo_test.go` (`d.secretRef`, default `fakeerp`, used by `register`)
- Modify: `test/security/network_isolation_test.go` (`TestAgentCannotReachTheTokenEndpoint`; the ERP credential-mount test also covers the OAuth client file)

**Interfaces:**
- Consumes: Tasks 3–4 end to end.
- Produces: `TestJITDemo`; demo field `secretRef string`.

- [ ] **Step 1: Write the failing tests.** `test/demo/jit_test.go`:

```go
package demo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// tenantJ is the JIT demo's tenant: the local connector-secrets manifest
// binds its "fakeerp-jit" reference to the Fake ERP's token endpoint.
const tenantJ = "00000000-0000-4000-8000-0000000000a4"

// TestJITDemo shows ADR-019 / Phase 24a: purchases execute with short-lived
// tokens minted just in time from the Fake ERP's token endpoint; the ERP sees
// only the token's principal, and neither the client secret nor any issued
// token appears in API responses, logs or the database.
func TestJITDemo(t *testing.T) {
	d := newDemo(t, tenantJ)
	d.subject, d.secretRef = "carol@umbrella.test", "fakeerp-jit"

	d.step("J0. Tenant Umbrella, the Slice A cast and an allow-ERP policy")
	d.tenantWithCast("umbrella", "Umbrella", []member{
		{"erin", "registry_editor"}, {"rita", "registry_approver"}, {"ravi", "registry_approver"},
		{"otto", "operator"}, {"carol", "approver"}, {"audra", "auditor"},
	})
	policy := d.must(201, "alice", "POST", "/v1/policies", map[string]any{"content": json.RawMessage(`{
		"format_version": 1, "rules": [
		{"id": "routine", "match": {"target": "erp"}, "verdict": "allow", "reason": "routine purchase"}]}`)})
	d.must(204, "bob", "POST", "/v1/policies/"+policy["id"].(string)+"/activate", map[string]any{"reason": "reviewed"})

	d.step("J1. The ERP connector names secret_ref fakeerp-jit: the worker mints a token for each call")
	d.register()
	var keys []string
	for i := range 5 {
		id := d.submit("jit-"+string(rune('a'+i)), "purchase", "erp.create_po", map[string]any{"amount": 100})
		d.until(id, "SUCCEEDED")
		d.onePO(id)
		keys = append(keys, d.action(id)["operation_key"].(string))
	}

	d.step("J2. The ERP audit: every call carried a minted token, and the tokens were issued just in time")
	entries := d.erpAudit()
	issued := map[string]bool{}
	for _, e := range entries {
		if e.Path == "/oauth/token" && e.Outcome == "token_issued" {
			issued[e.TokenSHA256] = true
		}
	}
	for _, k := range keys {
		for _, e := range entries {
			if e.OperationKey == k && e.Principal != "oauth:eacp-worker" {
				t.Fatalf("operation %s used principal %q, not a minted token", k, e.Principal)
			}
		}
	}
	if len(issued) == 0 {
		t.Fatal("the ERP issued no token")
	}
	d.logf("5 purchases, one PO each, all with principal oauth:eacp-worker; %d token(s) issued", len(issued))

	d.step("J3. Search responses, logs and the database for the client secret and every issued token")
	client, err := os.ReadFile(filepath.Join(d.root, "deployments", "docker", "secrets", "fakeerp-oauth-client.dev"))
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.TrimSpace(string(client))
	d.mu.Lock()
	responses := strings.Join(d.responses, "\n")
	d.mu.Unlock()
	dump, err := d.p.postgres("pg_dump", "-U", "postgres", "--data-only", "eacp")
	if err != nil {
		t.Fatalf("pg_dump: %v", err)
	}
	tokenShaped := regexp.MustCompile(`[A-Za-z0-9_-]{43}`)
	for where, text := range map[string]string{"an API response": responses, "a service log": d.p.logs(), "the database": dump} {
		if strings.Contains(text, secret) {
			t.Fatalf("the OAuth client secret appears in %s", where)
		}
		for _, m := range tokenShaped.FindAllString(text, -1) {
			sum := sha256.Sum256([]byte(m))
			if issued[hex.EncodeToString(sum[:])] {
				t.Fatalf("an issued token appears in %s", where)
			}
		}
	}
	d.logf("no client secret and none of the %d issued tokens in responses, logs or the database", len(issued))
	d.step("JIT demo complete")
}
```

Add to `demo_test.go`: field `secretRef string` (default `"fakeerp"` in `newDemo`), `register` uses `"secret_ref": d.secretRef`, and an `erpAudit()` helper returning `[]erpEntry{Principal, Path, Outcome, OperationKey, TokenSHA256 string}` parsed from `d.p.erpAudit(d.token)` (reuse it inside `committedPOs`).

`test/security/network_isolation_test.go`:

```go
func TestAgentCannotReachTheTokenEndpoint(t *testing.T) {
	requireStack(t)
	out, err := fromAgent("wget", "-q", "-T", "3", "-O", "-", "--post-data", "grant_type=client_credentials",
		"http://fakeerp:8090/oauth/token")
	if err == nil {
		t.Fatalf("the agent reached the token endpoint:\n%s", out)
	}
}
```

(Use the file's existing gate helper — read the top of the file; the helper may be named differently.)

- [ ] **Step 2: Run to verify they fail**

Run: `DEMO=J bash scripts/demo.sh`
Expected: FAIL — `DEMO must be A, C or unset`, then (after the script change) `eacpctl tenant create`/register works but the worker has no `fakeerp-jit` binding, so the first purchase never leaves `QUEUED`.

- [ ] **Step 3: Implement.**

`connector-secrets.dev.json` gains:

```json
    {
      "tenant_id": "00000000-0000-4000-8000-0000000000a4",
      "secret_ref": "fakeerp-jit",
      "host": "fakeerp:8090",
      "oauth2": {
        "token_url": "http://fakeerp:8090/oauth/token",
        "client_id": "eacp-worker",
        "client_secret": "dev-only-fakeerp-oauth-client-secret",
        "scope": "erp.purchase"
      }
    }
```

`prepare_fakeerp_token.py`: `write_verifier` skips entries without `value` (`matches` filters `"value" in item`), and a new function writes the single `oauth2.client_secret` of `secret_ref` `fakeerp-jit` to `fakeerp-oauth-client.dev` the same way (0o644, atomic replace).

`docker-compose.yml` fakeerp: `EACP_FAKEERP_OAUTH_CLIENT_ID: eacp-worker`, `EACP_FAKEERP_OAUTH_CLIENT_SECRET_FILE: /run/secrets/fakeerp_oauth_client`, `EACP_FAKEERP_OAUTH_TTL: 300s`; `secrets: [fakeerp_token, fakeerp_oauth_client]`; top-level `secrets.fakeerp_oauth_client.file: ./deployments/docker/secrets/fakeerp-oauth-client.dev`.

`deployments/k8s/dev/fakes.yaml` fakeerp: the same env (secret file mounted from Secret `fakeerp-oauth-client` key `secret` at `/run/secrets/fakeerp-oauth`); `scripts/k8s-e2e.sh` creates it (`secret eacp-deps fakeerp-oauth-client --from-file=secret=deployments/docker/secrets/fakeerp-oauth-client.dev`) and its default `TESTS` becomes `TestSliceADemo|TestKubernetesDisruption|TestJITDemo`.

`scripts/demo.sh`: accept `A`, `C`, `J` and combinations (default `ACJ`); build the `-run` pattern from the letters (`A`→`TestSliceADemo`, `C`→`TestSliceCDemo`, `J`→`TestJITDemo`, joined by `|`, anchored with `^(…)$`).

- [ ] **Step 4: Run the demos and the security tests**

Run: `bash scripts/demo.sh` (all three), then `docker compose up -d --build` and `EACP_COMPOSE_TEST=1 go test -count=1 ./test/security/`
Expected: `TestSliceADemo`, `TestSliceCDemo`, `TestJITDemo` PASS; security tests PASS (including the new one).

Then: `KEEP=1 bash scripts/k8s-e2e.sh` — Expected: `TestSliceADemo`, `TestKubernetesDisruption`, `TestJITDemo` PASS. Delete the profile afterwards.

- [ ] **Step 5: Commit**

```bash
git add deployments docker-compose.yml scripts test/demo test/security .gitignore
git commit -m "test(demo): a JIT demo on compose and Kubernetes - purchases with minted tokens, no secret or token anywhere"
```

---

### Task 6: ADR-019 and the documentation

**Files:**
- Create: `docs/adr/ADR-019-credential-custody.md`
- Modify: `docs/adr/README.md`, `docs/MASTER_PLAN.md` §96, `AGENTS.md` (status, a rule line, commands note for `DEMO=J`), `README.md`, `docs/KUBERNETES.md`, `docs/DEMO.md`, `docs/INVARIANTS.md` (Phase 24 paragraph)

- [ ] **Step 1:** ADR-019 Rev 1.0 — Status, Context (Slice A static custody: the worker alone holds credentials, host-bound per tenant and reference, redacted, scrubbed from results, never persisted; §96 asks for short-lived credentials), Decision (§1 custody rules as built; §2 the provider seam — `Credential(validFor)`, `Available`, `Rejected`; §3 the OAuth 2.0 client-credentials provider with every rule of the spec §3.3; §4 redaction set; §5 failure handling — no dispatch, lease released, claims withheld, reconciler lets the lease lapse, scanner does not record; §6 Fake ERP token endpoint; §7 proof), Consequences, and the spec §5 assumptions table.
- [ ] **Step 2:** ADR README row `ADR-019 | Credential Custody | Accepted (Rev 1.0) | Phase 24a`; MASTER_PLAN §96 status paragraph (24a delivered; 24b/24c Vault, SPIFFE/SPIRE, cloud workload identity next); AGENTS.md status and the rule: "Connector credentials come from providers in the worker only (ADR-019). A credential must outlive the whole call (`Credential(…, validFor)`); a provider that fails withholds its binding from claims and nothing is dispatched. OAuth tokens are minted with client credentials, never persisted, redacted until expiry + 24 h, and refused unless Bearer with `expires_in` ≤ 3600; the token endpoint never redirects. Never add a provider outside `internal/worker`." README Phase 24 section; KUBERNETES.md (token endpoint host in `worker.connectorEgress`; inline `client_secret` since the chart mounts one file; `EACP_ENV` must not be development for https to be enforced); DEMO.md (the JIT demo, `DEMO=J`); INVARIANTS paragraph (the JIT integration tests join invariant 11's credential custody entries if the map lists them — check `docs/INVARIANTS.md` for the credential invariant and add `TestTheWorkerExecutesWithAMintedToken` there).
- [ ] **Step 3: Verify and commit**

Run: `go vet ./... && EACP_TEST_ADMIN_DSN=... EACP_HELM_REQUIRED=1 go test -race -count=1 ./...`
Expected: every package ok.

```bash
git add docs AGENTS.md README.md
git commit -m "docs: ADR-019 Credential Custody; Phase 24a delivered"
```
