package worker_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"eacp/internal/fakeerp"
	"eacp/internal/storage/pgtest"
)

// jitVault is a minimal Vault: AppRole login and one KV v2 secret,
// eacp/fakeerp with key "token".
type jitVault struct {
	srv    *httptest.Server
	mu     sync.Mutex
	value  string
	token  string
	logins int
	reads  int
}

func newJITVault(t *testing.T, value string) *jitVault {
	t.Helper()
	jv := &jitVault{value: value}
	jv.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jv.mu.Lock()
		defer jv.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/auth/approle/login":
			jv.logins++
			jv.token = fmt.Sprintf("hvs.%s-jit-%d", canary, jv.logins)
			_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{"client_token": jv.token,
				"lease_duration": 3600, "renewable": true}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/secret/data/eacp/fakeerp" &&
			r.Header.Get("X-Vault-Token") == jv.token:
			jv.reads++
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"data":     map[string]any{"token": jv.value},
				"metadata": map[string]any{"version": jv.reads, "deletion_time": "", "destroyed": false}}})
		default:
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"errors":["permission denied"]}`))
		}
	}))
	t.Cleanup(jv.srv.Close)
	return jv
}

func (jv *jitVault) set(value string) { jv.mu.Lock(); jv.value = value; jv.mu.Unlock() }

// TestTheWorkerExecutesWithAVaultCredential: the binding names a Vault KV v2
// value; the worker logs in with AppRole and reads it when it needs it. A
// rejected value is read again at once, so a rotation in Vault takes effect
// on the next action (ADR-019 Rev 1.3).
func TestTheWorkerExecutesWithAVaultCredential(t *testing.T) {
	stale := canary + "-stale-erp-token"
	jv := newJITVault(t, stale)
	dir := t.TempDir()
	roleFile, secretFile := filepath.Join(dir, "role_id"), filepath.Join(dir, "secret_id")
	if err := os.WriteFile(roleFile, []byte("role-1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secretFile, []byte(canary+"-secret-id"), 0o600); err != nil {
		t.Fatal(err)
	}
	v := newJITFile(t, func(string) fakeerp.Options { return fakeerp.Options{} }, func(_, host string) string {
		return fmt.Sprintf(`{"vault":{"address":%q,"refresh_seconds":30,"auth":{"approle":{"role_id_file":%q,"secret_id_file":%q}}},
			"secrets":[{"tenant_id":%q,"secret_ref":"erp-jit","host":%q,"value_vault":{"path":"eacp/fakeerp","key":"token"}}]}`,
			jv.srv.URL, roleFile, secretFile, pgtest.TenantA, host)
	}, 100)
	ctx := context.Background()

	// Vault holds a token the ERP no longer accepts: the attempt is refused.
	refused := v.submit("")
	v.run(1)
	if got := v.get(refused.ID); got.AttemptCount != 1 || got.State == "SUCCEEDED" {
		t.Fatalf("refused action = %+v", got)
	}
	// Vault is rotated; the clock stays within the refresh interval, so only
	// the rejection makes the worker read Vault again.
	jv.set(jitStatic)
	next := v.submit("")
	waitSucceeded := func(id uuid.UUID) {
		t.Helper()
		for tries := 0; v.get(id).State != "SUCCEEDED"; tries++ {
			if tries == 5 {
				t.Fatalf("action = %+v", v.get(id))
			}
			if _, err := v.w.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			time.Sleep(150 * time.Millisecond) // past the worker's retry back-off
		}
	}
	waitSucceeded(next.ID)
	third := v.submit("")
	waitSucceeded(third.ID)
	jv.mu.Lock()
	reads, token := jv.reads, jv.token
	jv.mu.Unlock()
	if reads != 2 {
		t.Fatalf("%d KV reads: want the first read and one after the rejection", reads)
	}
	executes, refusedCalls := 0, 0
	for _, e := range v.audit() {
		if e["path"] != "/v1/execute" {
			continue
		}
		switch e["principal"] {
		case "execution-worker":
			executes++
		default:
			refusedCalls++
		}
	}
	if executes < 2 || refusedCalls < 1 {
		t.Fatalf("%d executes with the rotated token, %d refused", executes, refusedCalls)
	}
	v.assertNotPersisted(token, stale, canary+"-secret-id")
}
