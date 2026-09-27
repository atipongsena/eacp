package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"eacp/internal/logging"
)

// TestRepeatedVaultReadsKeepTheRedactionSetConstant: a cached value used
// again adds nothing to the redaction set, which every log line scans.
func TestRepeatedVaultReadsKeepTheRedactionSetConstant(t *testing.T) {
	f := newFakeVault(t)
	f.put("secret/data/eacp/erp", map[string]any{"token": "erp-" + vaultCanary})
	set := logging.NewSecretSet()
	s, err := LoadSecrets(writeFile(t, "secrets.json", vaultFile(t, f.srv.URL, 30, staticVault)),
		AllowPlainTokenURL(), WithClock((&vclock{t: time.Now()}).now), WithRedaction(set))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.Credential(ctx, vaultTenant, "erp", vaultEndpoint, time.Minute); err != nil {
		t.Fatal(err)
	}
	size := len(set.Values())
	for range 100 {
		if _, err := s.Credential(ctx, vaultTenant, "erp", vaultEndpoint, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(set.Values()); got != size {
		t.Fatalf("the redaction set grew from %d to %d entries over 100 cached reads", size, got)
	}
}

// TestConcurrentCallersShareAFailedVaultRead: callers queued behind a failing
// read get its failure without a request of their own, and the back-off
// grows one step, not one per caller.
func TestConcurrentCallersShareAFailedVaultRead(t *testing.T) {
	f := newFakeVault(t)
	f.put("secret/data/eacp/erp", map[string]any{"token": "v"})
	f.readStatus, f.readDelay = http.StatusServiceUnavailable, 200*time.Millisecond
	c := &vclock{t: time.Now()}
	s := vaultStore(t, f, c, 30, staticVault)
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if _, err := s.Credential(context.Background(), vaultTenant, "erp", vaultEndpoint, time.Minute); !errors.Is(err, ErrCredentialUnavailable) {
				t.Errorf("err = %v", err)
			}
		})
	}
	wg.Wait()
	f.mu.Lock()
	reads := f.reads["secret/data/eacp/erp"]
	f.mu.Unlock()
	if reads != 1 {
		t.Fatalf("%d KV reads for 10 queued callers of one failing path, want 1", reads)
	}
	if len(s.Available()) != 0 {
		t.Fatal("the binding is not backing off")
	}
	c.add(minMintBackoff)
	if len(s.Available()) != 1 {
		t.Fatal("the back-off grew once per queued caller")
	}
}

// TestASlowVaultFailureStillBacksOff: the back-off starts when the failed
// read ends, so a read that times out still withholds the binding.
func TestASlowVaultFailureStillBacksOff(t *testing.T) {
	f := newFakeVault(t)
	f.put("secret/data/eacp/erp", map[string]any{"token": "v"})
	c := &vclock{t: time.Now()}
	f.readStatus, f.onRead = http.StatusServiceUnavailable, func() { c.add(10 * time.Second) } // the client timeout
	s := vaultStore(t, f, c, 30, staticVault)
	if _, err := s.Credential(context.Background(), vaultTenant, "erp", vaultEndpoint, time.Minute); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if len(s.Available()) != 0 {
		t.Fatal("a read that failed slowly left the binding available")
	}
}

// TestAFailedMintDoesNotBlockAvailability: dropping the provider's Vault
// paths after a failed mint waits for no path lock while holding the lock
// that Available and Values need, so a Vault read hanging elsewhere cannot
// stall every claim loop.
func TestAFailedMintDoesNotBlockAvailability(t *testing.T) {
	f := newFakeVault(t)
	f.put("secret/data/eacp/idp", map[string]any{"client_secret": "cs-" + vaultCanary})
	c := &vclock{t: time.Now()}
	var path *vaultPath
	held := make(chan struct{})
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		path.mu.Lock() // another reader of the path hangs on Vault
		time.AfterFunc(1500*time.Millisecond, path.mu.Unlock)
		close(held)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
	}))
	t.Cleanup(idp.Close)
	body := vaultFile(t, f.srv.URL, 30, fmt.Sprintf(`"oauth2":{"token_url":%q,"client_id":"eacp-worker",
		"client_secret_vault":{"path":"eacp/idp","key":"client_secret"}}`, idp.URL))
	s, err := LoadSecrets(writeFile(t, "secrets.json", body), AllowPlainTokenURL(), WithClock(c.now))
	if err != nil {
		t.Fatal(err)
	}
	p := s.m[secretKey{vaultTenant, "erp"}].oauth
	path = p.vault.path(*p.secretRef)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = s.Credential(context.Background(), vaultTenant, "erp", vaultEndpoint, time.Minute)
	}()
	<-held
	time.Sleep(200 * time.Millisecond) // the mint has its answer and drops its Vault paths
	start := time.Now()
	s.Available()
	if waited := time.Since(start); waited > 500*time.Millisecond {
		t.Fatalf("Available waited %v for a Vault path lock", waited)
	}
	<-done
}
