package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/atipongsena/eacp/internal/logging"
)

const vaultCanary = "vault-canary-7c2e"

// fakeVault is an in-process Vault: AppRole and Kubernetes login, KV v2
// reads. Each login issues a new token; reads need the latest one.
type fakeVault struct {
	srv    *httptest.Server
	logins atomic.Int64
	lease  atomic.Int64

	mu          sync.Mutex
	kv          map[string]map[string]any // "secret/data/eacp/erp" -> data.data
	meta        map[string]map[string]any // optional metadata override per path
	reads       map[string]int
	token       string
	lastLogin   map[string]any
	loginPath   string
	namespaces  []string
	loginStatus int
	readStatus  int
	readBody    string // a raw body answered instead of the KV data
	redirectTo  string
	readDelay   time.Duration // each KV read waits this long
	onRead      func()        // called at each KV read, e.g. to move a test clock
}

func newFakeVault(t *testing.T) *fakeVault {
	t.Helper()
	f := &fakeVault{kv: map[string]map[string]any{}, meta: map[string]map[string]any{}, reads: map[string]int{}}
	f.lease.Store(3600)
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeVault) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.namespaces = append(f.namespaces, r.Header.Get("X-Vault-Namespace"))
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/auth/") && strings.HasSuffix(r.URL.Path, "/login") {
		n := f.logins.Add(1)
		f.loginPath = r.URL.Path
		f.lastLogin = map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&f.lastLogin)
		if f.loginStatus != 0 {
			w.WriteHeader(f.loginStatus)
			_, _ = w.Write([]byte(`{"errors":["permission denied"]}`))
			return
		}
		f.token = fmt.Sprintf("hvs.%s-%d", vaultCanary, n)
		_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{
			"client_token": f.token, "lease_duration": f.lease.Load(), "renewable": true}})
		return
	}
	if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/v1/") {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/")
	if f.redirectTo != "" {
		http.Redirect(w, r, f.redirectTo+r.URL.Path, http.StatusTemporaryRedirect)
		return
	}
	if r.Header.Get("X-Vault-Token") == "" || r.Header.Get("X-Vault-Token") != f.token {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":["permission denied"]}`))
		return
	}
	f.reads[path]++
	if f.onRead != nil {
		f.onRead()
	}
	time.Sleep(f.readDelay)
	switch {
	case f.readStatus != 0:
		w.WriteHeader(f.readStatus)
		_, _ = w.Write([]byte(`{"errors":["nope"]}`))
	case f.readBody != "":
		_, _ = w.Write([]byte(f.readBody))
	case f.kv[path] == nil:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errors":[]}`))
	default:
		meta := map[string]any{"version": 1, "created_time": "2026-09-27T00:00:00Z", "deletion_time": "", "destroyed": false}
		for k, v := range f.meta[path] {
			meta[k] = v
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": f.kv[path], "metadata": meta}})
	}
}

func (f *fakeVault) put(path string, data map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kv[path] = data
}

func (f *fakeVault) readsOf(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads[path]
}

func (f *fakeVault) set(fn func(f *fakeVault)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// vclock is a settable clock.
type vclock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *vclock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *vclock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuffer) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// appRoleClient is a Vault client for f with AppRole login from files.
func appRoleClient(t *testing.T, f *fakeVault, c *vclock, logs *lockedBuffer, refresh int) *vaultClient {
	t.Helper()
	roleFile, secretFile := writeFile(t, "role_id", "role-1\n"), writeFile(t, "secret_id", "secret-"+vaultCanary+"\n")
	return vaultClientFor(t, f, c, logs, vaultEntry{Address: f.srv.URL, Namespace: "eacp", RefreshSeconds: refresh,
		Auth: vaultAuthEntry{AppRole: &vaultAppRoleAuth{RoleIDFile: &roleFile, SecretIDFile: &secretFile}}})
}

func vaultClientFor(t *testing.T, f *fakeVault, c *vclock, logs *lockedBuffer, e vaultEntry) *vaultClient {
	t.Helper()
	var w *lockedBuffer = logs
	if w == nil {
		w = &lockedBuffer{}
	}
	v, err := newVaultClient(e, loadConfig{allowPlain: true, now: c.now, redact: logging.NewSecretSet(),
		log: slog.New(slog.NewJSONHandler(w, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

var erpRef = vaultRef{Path: "eacp/erp", Key: "token"}

func TestVaultAppRoleLogin(t *testing.T) {
	f := newFakeVault(t)
	f.put("secret/data/eacp/erp", map[string]any{"token": "erp-" + vaultCanary})
	v := appRoleClient(t, f, &vclock{t: time.Now()}, nil, 300)
	got, class := v.value(context.Background(), erpRef)
	if class != "" || got.Reveal() != "erp-"+vaultCanary {
		t.Fatalf("value = %q, class %q", got.Reveal(), class)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loginPath != "/v1/auth/approle/login" || f.lastLogin["role_id"] != "role-1" ||
		f.lastLogin["secret_id"] != "secret-"+vaultCanary {
		t.Fatalf("login %s %v", f.loginPath, f.lastLogin)
	}
	for _, ns := range f.namespaces {
		if ns != "eacp" {
			t.Fatalf("a request without the namespace header: %q", ns)
		}
	}
}

func TestVaultKubernetesLoginRereadsTheJWT(t *testing.T) {
	f := newFakeVault(t)
	f.put("secret/data/eacp/erp", map[string]any{"token": "v"})
	jwt := writeFile(t, "token", "jwt-one\n")
	c := &vclock{t: time.Now()}
	v := vaultClientFor(t, f, c, nil, vaultEntry{Address: f.srv.URL, RefreshSeconds: 30,
		Auth: vaultAuthEntry{Kubernetes: &vaultKubernetesAuth{Role: "eacp-worker", JWTFile: jwt}}})
	if _, class := v.value(context.Background(), erpRef); class != "" {
		t.Fatal(class)
	}
	f.mu.Lock()
	if f.loginPath != "/v1/auth/kubernetes/login" || f.lastLogin["role"] != "eacp-worker" || f.lastLogin["jwt"] != "jwt-one" {
		t.Fatalf("login %s %v", f.loginPath, f.lastLogin)
	}
	f.mu.Unlock()
	if err := os.WriteFile(jwt, []byte("jwt-two"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.set(func(f *fakeVault) { f.token = "" }) // the old token no longer works: force a login
	c.add(31 * time.Second)
	if _, class := v.value(context.Background(), erpRef); class != "vault_forbidden" {
		t.Fatalf("class = %q, want vault_forbidden", class)
	}
	if _, class := v.value(context.Background(), erpRef); class != "" {
		t.Fatal(class)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastLogin["jwt"] != "jwt-two" {
		t.Fatalf("the second login sent %v", f.lastLogin["jwt"])
	}
}

func TestVaultTokenIsReusedThenRenewedByLogin(t *testing.T) {
	for _, tc := range []struct {
		lease       int64
		reuse, next time.Duration
	}{
		{90, 59 * time.Second, 61 * time.Second},                  // two thirds of 90 s
		{2764800, 39 * time.Minute, 40*time.Minute + time.Second}, // capped at an hour
	} {
		f := newFakeVault(t)
		f.lease.Store(tc.lease)
		f.put("secret/data/eacp/erp", map[string]any{"token": "v"})
		c := &vclock{t: time.Now()}
		v := appRoleClient(t, f, c, nil, 30)
		start := c.now()
		read := func(at time.Duration) {
			c.t = start.Add(at)
			v.drop(erpRef)
			if _, class := v.value(context.Background(), erpRef); class != "" {
				t.Fatal(class)
			}
		}
		read(0)
		read(tc.reuse)
		if n := f.logins.Load(); n != 1 {
			t.Fatalf("lease %d: %d logins before two thirds of the use", tc.lease, n)
		}
		read(tc.next)
		if n := f.logins.Load(); n != 2 {
			t.Fatalf("lease %d: %d logins after two thirds of the use", tc.lease, n)
		}
	}
}

func TestVaultCachesAPathForTheRefreshInterval(t *testing.T) {
	f := newFakeVault(t)
	f.put("secret/data/eacp/erp", map[string]any{"token": "v1", "other": "o"})
	c := &vclock{t: time.Now()}
	v := appRoleClient(t, f, c, nil, 30)
	ctx := context.Background()
	if got, _ := v.value(ctx, erpRef); got.Reveal() != "v1" {
		t.Fatal(got.Reveal())
	}
	f.put("secret/data/eacp/erp", map[string]any{"token": "v2", "other": "o"})
	c.add(29 * time.Second)
	if got, _ := v.value(ctx, vaultRef{Path: "eacp/erp", Key: "other"}); got.Reveal() != "o" {
		t.Fatal(got.Reveal())
	}
	if got, _ := v.value(ctx, erpRef); got.Reveal() != "v1" {
		t.Fatalf("a fresh cache was not used: %q", got.Reveal())
	}
	if n := f.readsOf("secret/data/eacp/erp"); n != 1 {
		t.Fatalf("%d reads within the refresh interval", n)
	}
	c.add(2 * time.Second)
	if got, _ := v.value(ctx, erpRef); got.Reveal() != "v2" {
		t.Fatalf("the rotated value was not read: %q", got.Reveal())
	}
	if n := f.readsOf("secret/data/eacp/erp"); n != 2 {
		t.Fatalf("%d reads after the refresh interval", n)
	}
	if v.refresh() != 30*time.Second {
		t.Fatal(v.refresh())
	}
}

func TestVaultFailureClasses(t *testing.T) {
	path := "secret/data/eacp/erp"
	for name, tc := range map[string]struct {
		spoil func(t *testing.T, f *fakeVault, secretFile string)
		class string
	}{
		"unreadable secret_id": {func(t *testing.T, _ *fakeVault, file string) { _ = os.Remove(file) }, "vault_login_unreadable"},
		"empty secret_id":      {func(t *testing.T, _ *fakeVault, file string) { _ = os.WriteFile(file, nil, 0o600) }, "vault_login_unreadable"},
		"login 400":            {func(_ *testing.T, f *fakeVault, _ string) { f.loginStatus = 400 }, "vault_login"},
		"lease 0":              {func(_ *testing.T, f *fakeVault, _ string) { f.lease.Store(0) }, "vault_login"},
		"read 503":             {func(_ *testing.T, f *fakeVault, _ string) { f.readStatus = 503 }, "vault_read"},
		"read not JSON":        {func(_ *testing.T, f *fakeVault, _ string) { f.readBody = "<html>" }, "vault_read"},
		"read 404":             {func(_ *testing.T, f *fakeVault, _ string) { delete(f.kv, path) }, "vault_missing"},
		"data null":            {func(_ *testing.T, f *fakeVault, _ string) { f.readBody = `{"data":{"data":null,"metadata":{}}}` }, "vault_missing"},
		"soft-deleted": {func(_ *testing.T, f *fakeVault, _ string) {
			f.meta[path] = map[string]any{"deletion_time": "2026-09-27T01:00:00Z"}
		}, "vault_missing"},
		"destroyed":   {func(_ *testing.T, f *fakeVault, _ string) { f.meta[path] = map[string]any{"destroyed": true} }, "vault_missing"},
		"key absent":  {func(_ *testing.T, f *fakeVault, _ string) { f.kv[path] = map[string]any{"other": "x"} }, "vault_missing"},
		"not string":  {func(_ *testing.T, f *fakeVault, _ string) { f.kv[path] = map[string]any{"token": 42} }, "vault_invalid"},
		"empty value": {func(_ *testing.T, f *fakeVault, _ string) { f.kv[path] = map[string]any{"token": ""} }, "vault_invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeVault(t)
			f.put(path, map[string]any{"token": "v"})
			roleFile, secretFile := writeFile(t, "role_id", "role-1"), writeFile(t, "secret_id", "s")
			v := vaultClientFor(t, f, &vclock{t: time.Now()}, nil, vaultEntry{Address: f.srv.URL,
				Auth: vaultAuthEntry{AppRole: &vaultAppRoleAuth{RoleIDFile: &roleFile, SecretIDFile: &secretFile}}})
			f.set(func(f *fakeVault) { tc.spoil(t, f, secretFile) })
			if got, class := v.value(context.Background(), erpRef); class != tc.class || got.Reveal() != "" {
				t.Fatalf("class = %q (value %q), want %q", class, got.Reveal(), tc.class)
			}
		})
	}
	t.Run("forbidden drops the token", func(t *testing.T) {
		f := newFakeVault(t)
		f.put(path, map[string]any{"token": "v"})
		v := appRoleClient(t, f, &vclock{t: time.Now()}, nil, 30)
		if _, class := v.value(context.Background(), erpRef); class != "" {
			t.Fatal(class)
		}
		f.set(func(f *fakeVault) { f.token = "revoked" })
		v.drop(erpRef)
		if _, class := v.value(context.Background(), erpRef); class != "vault_forbidden" {
			t.Fatalf("class = %q", class)
		}
		if _, class := v.value(context.Background(), erpRef); class != "" || f.logins.Load() != 2 {
			t.Fatalf("class %q after %d logins: a forbidden token was kept", class, f.logins.Load())
		}
	})
}

func TestVaultNeverFollowsARedirect(t *testing.T) {
	other := newFakeVault(t)
	f := newFakeVault(t)
	f.put("secret/data/eacp/erp", map[string]any{"token": "v"})
	v := appRoleClient(t, f, &vclock{t: time.Now()}, nil, 30)
	f.set(func(f *fakeVault) { f.redirectTo = other.srv.URL })
	if _, class := v.value(context.Background(), erpRef); class != "vault_read" {
		t.Fatalf("class = %q", class)
	}
	other.mu.Lock()
	defer other.mu.Unlock()
	if len(other.namespaces) != 0 {
		t.Fatal("the redirect was followed: the token reached another host")
	}
}

func TestVaultOneLoginAndOneReadUnderConcurrency(t *testing.T) {
	f := newFakeVault(t)
	f.put("secret/data/eacp/erp", map[string]any{"token": "v"})
	v := appRoleClient(t, f, &vclock{t: time.Now()}, nil, 30)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, class := v.value(context.Background(), erpRef); class != "" || got.Reveal() != "v" {
				t.Errorf("value %q class %q", got.Reveal(), class)
			}
		}()
	}
	wg.Wait()
	if f.logins.Load() != 1 || f.readsOf("secret/data/eacp/erp") != 1 {
		t.Fatalf("%d logins and %d reads for 20 concurrent callers", f.logins.Load(), f.readsOf("secret/data/eacp/erp"))
	}
}

func TestVaultNeverLogsATokenOrValue(t *testing.T) {
	f := newFakeVault(t)
	f.put("secret/data/eacp/erp", map[string]any{"token": "erp-" + vaultCanary})
	logs := &lockedBuffer{}
	c := &vclock{t: time.Now()}
	v := appRoleClient(t, f, c, logs, 30)
	ctx := context.Background()
	if _, class := v.value(ctx, erpRef); class != "" {
		t.Fatal(class)
	}
	f.set(func(f *fakeVault) { f.readStatus = 503 })
	c.add(time.Minute)
	if _, class := v.value(ctx, erpRef); class != "vault_read" {
		t.Fatal(class)
	}
	out := logs.String()
	if strings.Contains(out, vaultCanary) {
		t.Fatalf("a Vault token or value reached the log: %s", out)
	}
	if !strings.Contains(out, "vault login") || !strings.Contains(out, `"class":"vault_read"`) {
		t.Fatalf("the login and the failure were not logged: %s", out)
	}
	if !v.redactsAll(f) {
		t.Fatal("the token or the value is not in the redaction set")
	}
}

// redactsAll reports whether the issued token and the value are redacted.
func (v *vaultClient) redactsAll(f *fakeVault) bool {
	f.mu.Lock()
	tok := f.token
	f.mu.Unlock()
	have := map[string]bool{}
	for _, s := range v.redact.Values() {
		have[s] = true
	}
	return have[tok] && have["erp-"+vaultCanary]
}
