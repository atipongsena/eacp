package demo

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// tenantV is the Vault demo's tenant: the worker's connector-secrets manifest
// binds "fakeerp-vault" to the Fake ERP's static token and
// "fakeerp-vault-oauth" to its OAuth client, both read from Vault KV v2
// (ADR-019 Rev 1.3).
const tenantV = "00000000-0000-4000-8000-0000000000a7"

// TestVaultDemo shows ADR-019 Rev 1.3 (Phase 24d): the worker logs in to
// Vault (AppRole on compose, Kubernetes auth on the cluster) and reads its
// connector credentials from KV v2 when it needs them. A credential deleted
// in Vault withholds work after one refresh interval and never dispatches;
// restored, the work runs. No Vault token or Vault-held value appears in
// responses, logs or the database.
func TestVaultDemo(t *testing.T) {
	d := newDemo(t, tenantV)
	d.subject, d.secretRef = "carol@wayne.test", "fakeerp-vault"
	d.extra = [][2]string{{"erp-oauth", "fakeerp-vault-oauth"}}

	d.step("V0. Bootstrap tenant Wayne; a policy allows routine ERP work")
	d.tenantWithCast("wayne", "Wayne", []member{
		{"erin", "registry_editor"}, {"rita", "registry_approver"}, {"ravi", "registry_approver"},
		{"carol", "approver"}, {"audra", "auditor"},
	})
	policy := d.must(201, "alice", "POST", "/v1/policies", map[string]any{"content": json.RawMessage(`{
		"format_version": 1, "rules": [
		{"id": "routine", "match": {"target": "erp"}, "verdict": "allow", "reason": "routine purchase"}]}`)})
	d.must(204, "bob", "POST", "/v1/policies/"+policy["id"].(string)+"/activate", map[string]any{"reason": "reviewed"})

	d.step("V1. Two connectors, two Vault-held credentials: the static ERP token and the OAuth client secret")
	d.register()
	principals := map[string]string{} // operation key -> the principal the ERP must see
	for _, p := range []struct{ idem, tool, principal string }{
		{"vault-static-1", "erp.create_po", "execution-worker"},
		{"vault-oauth-1", "erp-oauth.create_po", "oauth:eacp-worker"},
	} {
		id := d.submit(p.idem, "purchase", p.tool, map[string]any{"amount": 100})
		d.until(id, "SUCCEEDED")
		d.onePO(id)
		principals[d.action(id)["operation_key"].(string)] = p.principal
	}
	entries := d.erpAudit()
	for key, want := range principals {
		seen := false
		for _, e := range entries {
			if e.OperationKey == key {
				seen = true
				if e.Principal != want {
					d.t.Fatalf("operation %s used principal %q, want %q", key, e.Principal, want)
				}
			}
		}
		if !seen {
			d.t.Fatalf("the ERP audit has no call for operation %s", key)
		}
	}
	d.logf("erp.create_po ran as principal execution-worker (the static token from secret/eacp/fakeerp); " +
		"erp-oauth.create_po as oauth:eacp-worker (a token minted with the client secret from secret/eacp/fakeerp-oauth)")

	d.step("V2. The static token is deleted in Vault: after one refresh interval the purchase is withheld")
	version := d.kvVersion("secret/eacp/fakeerp")
	if out, err := d.p.vault("kv", "delete", "secret/eacp/fakeerp"); err != nil {
		d.t.Fatalf("vault kv delete: %v\n%s", err, out)
	}
	d.logf("vault kv delete secret/eacp/fakeerp (soft-deletes version %d); the worker refreshes every 30 s", version)
	time.Sleep(35 * time.Second) // past the worker's refresh interval: the cached value is gone
	held := d.submit("vault-static-2", "purchase", "erp.create_po", map[string]any{"amount": 200})
	d.until(held, "QUEUED", "LEASED")
	time.Sleep(12 * time.Second) // several worker poll intervals
	if a := d.action(held); a["state"] != "QUEUED" && a["state"] != "LEASED" {
		d.t.Fatalf("a purchase without its credential moved on: %v", a)
	}
	if n := d.sql(`SELECT attempt_count FROM eacp.actions WHERE id = '` + held + `'`); n != "0" {
		d.t.Fatalf("a purchase without its credential was attempted %s times", n)
	}
	key := d.action(held)["operation_key"].(string)
	if n := d.committedPOs()[key]; n != 0 {
		d.t.Fatalf("a purchase without its credential reached the ERP %d times", n)
	}
	d.logf("the next purchase waits QUEUED: no attempt, no PO; the worker serves no stale value")
	if out, err := d.p.vault("kv", "undelete", fmt.Sprintf("-versions=%d", version), "secret/eacp/fakeerp"); err != nil {
		d.t.Fatalf("vault kv undelete: %v\n%s", err, out)
	}
	d.until(held, "SUCCEEDED")
	d.onePO(held)
	d.logf("vault kv undelete restores version %d: the held purchase succeeds with one PO", version)

	d.step("V3. Search responses, logs and the database for Vault tokens, Vault-held values and issued tokens")
	client, err := os.ReadFile(filepath.Join(d.root, "deployments", "docker", "secrets", "fakeerp-oauth-client.dev"))
	if err != nil {
		d.t.Fatalf("run deployments/docker/secrets/prepare_fakeerp_token.py first: %v", err)
	}
	values := map[string]string{"the static ERP token": d.token, "the OAuth client secret": strings.TrimSpace(string(client))}
	issued := map[string]bool{}
	for _, e := range d.erpAudit() {
		if e.Path == "/oauth/token" && e.Outcome == "token_issued" {
			issued[e.TokenSHA256] = true
		}
	}
	d.mu.Lock()
	responses := strings.Join(d.responses, "\n")
	d.mu.Unlock()
	dump, err := d.p.postgres("pg_dump", "-U", "postgres", "--data-only", "eacp")
	if err != nil {
		d.t.Fatalf("pg_dump: %v", err)
	}
	for where, text := range map[string]string{"an API response": responses, "a service log": d.p.logs(), "the database": dump} {
		for name, v := range values {
			if strings.Contains(text, v) {
				d.t.Fatalf("%s appears in %s", name, where)
			}
		}
		if m := vaultToken.FindString(text); m != "" {
			d.t.Fatalf("a Vault token (%.8s...) appears in %s", m, where)
		}
		if n := containsIssued(text, issued); n > 0 {
			d.t.Fatalf("%d issued token(s) appear in %s", n, where)
		}
	}
	d.logf("no Vault token, neither Vault-held value and none of the %d issued tokens appears in responses, logs "+
		"or the database", len(issued))
}

// vaultToken matches a Vault service token: "hvs." and its base64url body.
var vaultToken = regexp.MustCompile(`hvs\.[A-Za-z0-9_-]{20,}`)

// kvVersion is the current version of a KV v2 secret in the dev Vault.
func (d *demo) kvVersion(path string) int {
	d.t.Helper()
	out, err := d.p.vault("kv", "metadata", "get", "-format=json", path)
	if err != nil {
		d.t.Fatalf("vault kv metadata get %s: %v\n%s", path, err, out)
	}
	var meta struct {
		Data struct {
			CurrentVersion int `json:"current_version"`
		} `json:"data"`
	}
	body := out[max(strings.Index(out, "{"), 0):] // the CLI's stderr shares the output
	if err := json.Unmarshal([]byte(body), &meta); err != nil || meta.Data.CurrentVersion == 0 {
		d.t.Fatalf("vault kv metadata get %s: %v\n%s", path, err, out)
	}
	return meta.Data.CurrentVersion
}

func TestVaultTokenMatchesAServiceToken(t *testing.T) {
	if !vaultToken.MatchString("X-Vault-Token: hvs.CAESIJ3kq_2x-ZbWQ1a2b3c4d5e6f7g8h9") {
		t.Fatal("a service token was missed")
	}
	if vaultToken.MatchString("hvs.short dev-only-vault-root-token") {
		t.Fatal("a false positive")
	}
}
