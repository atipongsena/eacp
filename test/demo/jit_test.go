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
// binds its "fakeerp-jit" reference to the Fake ERP's token endpoint
// instead of a static credential.
const tenantJ = "00000000-0000-4000-8000-0000000000a4"

// TestJITDemo shows ADR-019 (Phase 24a): purchases execute with short-lived
// tokens the worker mints just in time from the Fake ERP's OAuth 2.0 token
// endpoint. The ERP sees only the tokens' principal, and neither the client
// secret nor any issued token appears in API responses, logs or the
// database.
func TestJITDemo(t *testing.T) {
	d := newDemo(t, tenantJ)
	d.subject, d.secretRef = "carol@umbrella.test", "fakeerp-jit"

	d.step("J0. Bootstrap tenant Umbrella; a policy allows routine ERP work")
	d.tenantWithCast("umbrella", "Umbrella", []member{
		{"erin", "registry_editor"}, {"rita", "registry_approver"}, {"ravi", "registry_approver"},
		{"carol", "approver"}, {"audra", "auditor"},
	})
	policy := d.must(201, "alice", "POST", "/v1/policies", map[string]any{"content": json.RawMessage(`{
		"format_version": 1, "rules": [
		{"id": "routine", "match": {"target": "erp"}, "verdict": "allow", "reason": "routine purchase"}]}`)})
	d.must(204, "bob", "POST", "/v1/policies/"+policy["id"].(string)+"/activate", map[string]any{"reason": "reviewed"})

	d.step("J1. The ERP connector names secret_ref fakeerp-jit: the worker mints its credential per call")
	d.register()
	var keys []string
	for _, idem := range []string{"jit-1", "jit-2", "jit-3", "jit-4", "jit-5"} {
		id := d.submit(idem, "purchase", "erp.create_po", map[string]any{"amount": 100})
		d.until(id, "SUCCEEDED")
		d.onePO(id)
		keys = append(keys, d.action(id)["operation_key"].(string))
	}

	d.step("J2. The ERP audit: each call carried a minted token, issued just in time")
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
				d.t.Fatalf("operation %s used principal %q, not a minted token", k, e.Principal)
			}
		}
	}
	if len(issued) == 0 {
		d.t.Fatal("the ERP issued no token")
	}
	d.logf("5 purchases, one PO each, every call as principal oauth:eacp-worker; %d token(s) issued, "+
		"the ERP keeps only their SHA-256", len(issued))

	d.step("J3. Search responses, logs and the database for the client secret and every issued token")
	client, err := os.ReadFile(filepath.Join(d.root, "deployments", "docker", "secrets", "fakeerp-oauth-client.dev"))
	if err != nil {
		d.t.Fatalf("run deployments/docker/secrets/prepare_fakeerp_token.py first: %v", err)
	}
	secret := strings.TrimSpace(string(client))
	d.mu.Lock()
	responses := strings.Join(d.responses, "\n")
	d.mu.Unlock()
	dump, err := d.p.postgres("pg_dump", "-U", "postgres", "--data-only", "eacp")
	if err != nil {
		d.t.Fatalf("pg_dump: %v", err)
	}
	for where, text := range map[string]string{"an API response": responses, "a service log": d.p.logs(), "the database": dump} {
		if strings.Contains(text, secret) {
			d.t.Fatalf("the OAuth client secret appears in %s", where)
		}
		if n := containsIssued(text, issued); n > 0 {
			d.t.Fatalf("%d issued token(s) appear in %s", n, where)
		}
	}
	d.logf("neither the client secret nor any of the %d issued tokens appears in responses, logs or the database",
		len(issued))
}

// tokenRun matches runs of base64url characters long enough to hold a token.
var tokenRun = regexp.MustCompile(`[A-Za-z0-9_-]{43,}`)

// containsIssued counts the 43-character windows of text whose SHA-256 is an
// issued token's. Every window of every long run is checked, so a token
// cannot hide inside a longer string.
func containsIssued(text string, issued map[string]bool) int {
	n := 0
	for _, run := range tokenRun.FindAllString(text, -1) {
		for i := 0; i+43 <= len(run); i++ {
			sum := sha256.Sum256([]byte(run[i : i+43]))
			if issued[hex.EncodeToString(sum[:])] {
				n++
			}
		}
	}
	return n
}

func TestContainsIssuedFindsATokenInsideALongerString(t *testing.T) {
	token := strings.Repeat("A", 20) + "-_" + strings.Repeat("z", 21)
	sum := sha256.Sum256([]byte(token))
	issued := map[string]bool{hex.EncodeToString(sum[:]): true}
	if containsIssued("Bearer "+token+" end", issued) != 1 || containsIssued("x9"+token+"Q", issued) != 1 {
		t.Fatal("a token was missed")
	}
	if containsIssued(strings.Repeat("A", 60), issued) != 0 {
		t.Fatal("a false positive")
	}
}
