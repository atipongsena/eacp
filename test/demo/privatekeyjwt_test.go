package demo

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// tenantP is the private_key_jwt demo's tenant: the worker's connector-secrets
// manifest binds its "fakeerp-pkjwt" reference to the Fake ERP's token
// endpoint with the worker's own signing key and certificate (ADR-019 Rev 1.2).
const tenantP = "00000000-0000-4000-8000-0000000000a6"

// pkjwtClient is the Fake ERP client the worker's assertions name as iss and sub.
const pkjwtClient = "eacp-worker-pkjwt"

// TestPrivateKeyJWTDemo shows ADR-019 Rev 1.2 (Phase 24c): the binding holds
// no client secret, only the worker's private key. Each mint presents a fresh
// assertion signed with it; the Fake ERP verifies it against the client's
// registered JWKS, accepts each jti once and mints the token the purchase
// uses. No token, key or assertion appears in responses, logs or the database.
func TestPrivateKeyJWTDemo(t *testing.T) {
	d := newDemo(t, tenantP)
	d.subject, d.secretRef = "carol@stark.test", "fakeerp-pkjwt"

	d.step("P0. Bootstrap tenant Stark; a policy allows routine ERP work")
	d.tenantWithCast("stark", "Stark", []member{
		{"erin", "registry_editor"}, {"rita", "registry_approver"}, {"ravi", "registry_approver"},
		{"carol", "approver"}, {"audra", "auditor"},
	})
	policy := d.must(201, "alice", "POST", "/v1/policies", map[string]any{"content": json.RawMessage(`{
		"format_version": 1, "rules": [
		{"id": "routine", "match": {"target": "erp"}, "verdict": "allow", "reason": "routine purchase"}]}`)})
	d.must(204, "bob", "POST", "/v1/policies/"+policy["id"].(string)+"/activate", map[string]any{"reason": "reviewed"})

	d.step("P1. The ERP connector names secret_ref fakeerp-pkjwt: the worker signs a fresh assertion per mint")
	d.register()
	var keys []string
	for _, idem := range []string{"pkjwt-1", "pkjwt-2", "pkjwt-3"} {
		id := d.submit(idem, "purchase", "erp.create_po", map[string]any{"amount": 100})
		d.until(id, "SUCCEEDED")
		d.onePO(id)
		keys = append(keys, d.action(id)["operation_key"].(string))
	}

	d.step("P2. The ERP audit: each token was issued for a distinct, single-use assertion")
	entries := d.erpAudit()
	issued, jtis, issuances := map[string]bool{}, map[string]bool{}, 0
	for _, e := range entries {
		if e.Path == "/oauth/token" && e.Outcome == "token_issued" && e.Principal == "oauth:"+pkjwtClient {
			issuances++
			issued[e.TokenSHA256] = true
			if e.AssertionJTI == "" || e.AssertionSHA256 == "" {
				d.t.Fatalf("a private_key_jwt issuance recorded no jti or assertion hash: %+v", e)
			}
			jtis[e.AssertionJTI] = true
		}
	}
	for _, k := range keys {
		for _, e := range entries {
			if e.OperationKey == k && e.Principal != "oauth:"+pkjwtClient {
				d.t.Fatalf("operation %s used principal %q, not a private_key_jwt token", k, e.Principal)
			}
		}
	}
	if issuances == 0 || len(jtis) != issuances {
		d.t.Fatalf("%d issuance(s) for %d distinct jti(s)", issuances, len(jtis))
	}
	d.logf("3 purchases, one PO each, as principal oauth:%s; %d token(s) minted for %d distinct assertion jti(s)",
		pkjwtClient, len(issued), len(jtis))

	d.step("P3. Search responses, logs and the database for tokens, private keys and signed assertions")
	d.mu.Lock()
	responses := strings.Join(d.responses, "\n")
	d.mu.Unlock()
	dump, err := d.p.postgres("pg_dump", "-U", "postgres", "--data-only", "eacp")
	if err != nil {
		d.t.Fatalf("pg_dump: %v", err)
	}
	for where, text := range map[string]string{"an API response": responses, "a service log": d.p.logs(), "the database": dump} {
		if n := containsIssued(text, issued); n > 0 {
			d.t.Fatalf("%d issued token(s) appear in %s", n, where)
		}
		if strings.Contains(text, "PRIVATE KEY") {
			d.t.Fatalf("a private key appears in %s", where)
		}
		if n := containsSignedAssertion(text, pkjwtClient); n > 0 {
			d.t.Fatalf("%d signed assertion(s) appear in %s", n, where)
		}
	}
	d.logf("no private key, assertion or any of the %d issued tokens appears in responses, logs or the database", len(issued))
}

// containsSignedAssertion counts the JWTs in text whose payload's iss is iss:
// any client assertion the worker signed, audited or not.
func containsSignedAssertion(text, iss string) int {
	n := 0
	for _, m := range jwtRun.FindAllString(text, -1) {
		payload, err := base64.RawURLEncoding.DecodeString(strings.Split(m, ".")[1])
		var claims struct{ Iss string }
		if err == nil && json.Unmarshal(payload, &claims) == nil && claims.Iss == iss {
			n++
		}
	}
	return n
}

func TestContainsSignedAssertionFindsAJWT(t *testing.T) {
	seg := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	jwt := seg(`{"alg":"PS256"}`) + "." + seg(`{"iss":"`+pkjwtClient+`","sub":"`+pkjwtClient+`"}`) + ".c2ln"
	if containsSignedAssertion("assertion="+jwt+" end", pkjwtClient) != 1 {
		t.Fatal("a signed assertion was missed")
	}
	other := seg(`{"alg":"PS256"}`) + "." + seg(`{"iss":"someone"}`) + ".c2ln"
	if containsSignedAssertion(other+" eacp.worker.claim", pkjwtClient) != 0 {
		t.Fatal("a false positive")
	}
}
