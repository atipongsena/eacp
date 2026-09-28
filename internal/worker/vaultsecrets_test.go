package worker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

var vaultTenant = uuid.MustParse("00000000-0000-4000-8000-0000000000a7")

const vaultEndpoint = "https://erp.internal:8443/v1"

// vaultStore loads a secrets file whose vault block uses f with AppRole and
// the given refresh, and whose only binding is body (the part after "host").
func vaultStore(t *testing.T, f *fakeVault, c *vclock, refresh int, binding string) *SecretStore {
	t.Helper()
	s, err := LoadSecrets(writeFile(t, "secrets.json", vaultFile(t, f.srv.URL, refresh, binding)),
		AllowPlainTokenURL(), WithClock(c.now))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func vaultFile(t *testing.T, address string, refresh int, binding string) string {
	t.Helper()
	roleFile, secretFile := writeFile(t, "role_id", "role-1"), writeFile(t, "secret_id", "secret-"+vaultCanary)
	return fmt.Sprintf(`{"vault":{"address":%q,"refresh_seconds":%d,"auth":{"approle":{"role_id_file":%q,"secret_id_file":%q}}},
		"secrets":[{"tenant_id":%q,"secret_ref":"erp","host":"erp.internal:8443",%s}]}`,
		address, refresh, roleFile, secretFile, vaultTenant, binding)
}

const staticVault = `"value_vault":{"path":"eacp/erp","key":"token"}`

func TestAStaticCredentialComesFromVault(t *testing.T) {
	f := newFakeVault(t)
	f.put("secret/data/eacp/erp", map[string]any{"token": "erp-" + vaultCanary})
	s := vaultStore(t, f, &vclock{t: time.Now()}, 30, staticVault)
	got, err := s.Credential(context.Background(), vaultTenant, "erp", vaultEndpoint, time.Minute)
	if err != nil || got.Reveal() != "erp-"+vaultCanary {
		t.Fatalf("credential %q, err %v", got.Reveal(), err)
	}
	if !slices.Contains(s.Values(), "erp-"+vaultCanary) {
		t.Fatal("the Vault value is not in Values()")
	}
	if len(s.Available()) != 1 {
		t.Fatal("the binding is not available")
	}
}

func TestAVaultFailureBacksTheBindingOff(t *testing.T) {
	f := newFakeVault(t)
	f.put("secret/data/eacp/erp", map[string]any{"token": "v"})
	c := &vclock{t: time.Now()}
	s := vaultStore(t, f, c, 30, staticVault)
	ctx := context.Background()
	f.set(func(f *fakeVault) { f.readStatus = 503 })
	if _, err := s.Credential(ctx, vaultTenant, "erp", vaultEndpoint, time.Minute); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if len(s.Available()) != 0 {
		t.Fatal("a failing binding is still available")
	}
	reads := func() int { return f.readsOf("secret/data/eacp/erp") }
	if _, err := s.Credential(ctx, vaultTenant, "erp", vaultEndpoint, time.Minute); err == nil || reads() != 1 {
		t.Fatalf("during the back-off: err %v after %d reads", err, reads())
	}
	c.add(time.Second)
	_, _ = s.Credential(ctx, vaultTenant, "erp", vaultEndpoint, time.Minute) // second failure: 2 s back-off
	c.add(time.Second)
	if _, err := s.Credential(ctx, vaultTenant, "erp", vaultEndpoint, time.Minute); err == nil || reads() != 2 {
		t.Fatalf("the back-off did not double: %d reads", reads())
	}
	f.set(func(f *fakeVault) { f.readStatus = 0 })
	c.add(time.Second)
	if _, err := s.Credential(ctx, vaultTenant, "erp", vaultEndpoint, time.Minute); err != nil {
		t.Fatalf("after the back-off: %v", err)
	}
	if len(s.Available()) != 1 {
		t.Fatal("a success did not reset the back-off")
	}
}

func TestAStaleVaultValueIsNeverServed(t *testing.T) {
	f := newFakeVault(t)
	f.put("secret/data/eacp/erp", map[string]any{"token": "v"})
	c := &vclock{t: time.Now()}
	s := vaultStore(t, f, c, 30, staticVault)
	ctx := context.Background()
	if _, err := s.Credential(ctx, vaultTenant, "erp", vaultEndpoint, time.Minute); err != nil {
		t.Fatal(err)
	}
	f.set(func(f *fakeVault) { f.readStatus = 503 })
	c.add(31 * time.Second)
	if got, err := s.Credential(ctx, vaultTenant, "erp", vaultEndpoint, time.Minute); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("a stale value was served: %q, %v", got.Reveal(), err)
	}
}

func TestARejectedVaultCredentialIsReadAgain(t *testing.T) {
	f := newFakeVault(t)
	f.put("secret/data/eacp/erp", map[string]any{"token": "old-" + vaultCanary})
	c := &vclock{t: time.Now()}
	s := vaultStore(t, f, c, 30, staticVault)
	ctx := context.Background()
	old, err := s.Credential(ctx, vaultTenant, "erp", vaultEndpoint, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	f.put("secret/data/eacp/erp", map[string]any{"token": "new-" + vaultCanary})
	c.add(5 * time.Second)
	s.Rejected(vaultTenant, "erp", old)
	got, err := s.Credential(ctx, vaultTenant, "erp", vaultEndpoint, time.Minute)
	if err != nil || got.Reveal() != "new-"+vaultCanary {
		t.Fatalf("after a rejection: %q, %v", got.Reveal(), err)
	}
	if !slices.Contains(s.Values(), "old-"+vaultCanary) {
		t.Fatal("the value just rotated out is no longer scrubbed")
	}
	c.add(31 * time.Second)
	if _, err := s.Credential(ctx, vaultTenant, "erp", vaultEndpoint, time.Minute); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(s.Values(), "old-"+vaultCanary) {
		t.Fatal("the previous value outlived one refresh interval after the rotation")
	}
}

func TestTheWorkerStartsWithVaultDown(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := "http://" + l.Addr().String()
	_ = l.Close() // nothing listens there now
	s, err := LoadSecrets(writeFile(t, "secrets.json", vaultFile(t, addr, 30, staticVault)), AllowPlainTokenURL())
	if err != nil {
		t.Fatalf("the worker refused to start with Vault down: %v", err)
	}
	if _, err := s.Credential(context.Background(), vaultTenant, "erp", vaultEndpoint, time.Minute); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestInvalidVaultEntriesRejectTheWholeFile(t *testing.T) {
	roleFile, secretFile := writeFile(t, "role_id", "role-1"), writeFile(t, "secret_id", "s-"+vaultCanary)
	empty := writeFile(t, "empty", "")
	approle := fmt.Sprintf(`"approle":{"role_id_file":%q,"secret_id_file":%q}`, roleFile, secretFile)
	vault := func(fields string) string {
		return `"vault":{` + fields + `},`
	}
	good := vault(`"address":"http://vault:8200","auth":{` + approle + `}`)
	binding := func(b string) string {
		return fmt.Sprintf(`"secrets":[{"tenant_id":%q,"secret_ref":"erp","host":"erp.internal:8443",%s}]`, vaultTenant, b)
	}
	ref := func(r string) string { return binding(`"value_vault":` + r) }
	cases := map[string]string{
		"value_vault without vault":   binding(staticVault),
		"value and value_vault":       good + binding(`"value":"x",`+staticVault),
		"path with ..":                good + ref(`{"path":"a/../b","key":"k"}`),
		"leading slash":               good + ref(`{"path":"/a","key":"k"}`),
		"trailing slash":              good + ref(`{"path":"a/","key":"k"}`),
		"empty segment":               good + ref(`{"path":"a//b","key":"k"}`),
		"path too long":               good + ref(`{"path":"`+strings.Repeat("a/", 128)+`a","key":"k"}`),
		"key with a space":            good + ref(`{"path":"a","key":"a b"}`),
		"no key":                      good + ref(`{"path":"a"}`),
		"bad mount":                   good + ref(`{"path":"a","key":"k","mount":"a/b"}`),
		"refresh 29":                  vault(`"address":"http://vault:8200","refresh_seconds":29,"auth":{`+approle+`}`) + binding(staticVault),
		"refresh 3601":                vault(`"address":"http://vault:8200","refresh_seconds":3601,"auth":{`+approle+`}`) + binding(staticVault),
		"ftp address":                 vault(`"address":"ftp://vault:8200","auth":{`+approle+`}`) + binding(staticVault),
		"address with user info":      vault(`"address":"http://u:p@vault:8200","auth":{`+approle+`}`) + binding(staticVault),
		"address with query":          vault(`"address":"http://vault:8200?x=1","auth":{`+approle+`}`) + binding(staticVault),
		"address with path":           vault(`"address":"http://vault:8200/v1","auth":{`+approle+`}`) + binding(staticVault),
		"no auth":                     vault(`"address":"http://vault:8200","auth":{}`) + binding(staticVault),
		"both auths":                  vault(`"address":"http://vault:8200","auth":{`+approle+`,"kubernetes":{"role":"r","jwt_file":"`+strings.ReplaceAll(roleFile, `\`, `\\`)+`"}}`) + binding(staticVault),
		"kubernetes without role":     vault(`"address":"http://vault:8200","auth":{"kubernetes":{"jwt_file":"`+strings.ReplaceAll(roleFile, `\`, `\\`)+`"}}`) + binding(staticVault),
		"kubernetes without jwt_file": vault(`"address":"http://vault:8200","auth":{"kubernetes":{"role":"r"}}`) + binding(staticVault),
		"role_id and role_id_file":    vault(fmt.Sprintf(`"address":"http://vault:8200","auth":{"approle":{"role_id":"r","role_id_file":%q,"secret_id_file":%q}}`, roleFile, secretFile)) + binding(staticVault),
		"no secret_id":                vault(fmt.Sprintf(`"address":"http://vault:8200","auth":{"approle":{"role_id_file":%q}}`, roleFile)) + binding(staticVault),
		"empty secret_id_file":        vault(fmt.Sprintf(`"address":"http://vault:8200","auth":{"approle":{"role_id_file":%q,"secret_id_file":%q}}`, roleFile, empty)) + binding(staticVault),
		"unreadable secret_id_file":   vault(fmt.Sprintf(`"address":"http://vault:8200","auth":{"approle":{"role_id_file":%q,"secret_id_file":"/absent"}}`, roleFile)) + binding(staticVault),
		"unknown vault member":        vault(`"address":"http://vault:8200","token":"x","auth":{`+approle+`}`) + binding(staticVault),
		"unknown ref member":          good + ref(`{"path":"a","key":"k","version":2}`),
	}
	for name, body := range cases {
		_, err := LoadSecrets(writeFile(t, "secrets.json", "{"+body+"}"), AllowPlainTokenURL())
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), vaultCanary) {
			t.Errorf("%s: the error repeats a value: %v", name, err)
		}
	}
	plain := "{" + good + binding(staticVault) + "}"
	if _, err := LoadSecrets(writeFile(t, "secrets.json", plain)); err == nil {
		t.Error("an http Vault address was accepted outside development")
	}
	if _, err := LoadSecrets(writeFile(t, "secrets.json", plain), AllowPlainTokenURL()); err != nil {
		t.Errorf("a valid file was refused: %v", err)
	}
}
