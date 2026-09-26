package demo

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// tenantF is the federated JIT demo's tenant: on Kubernetes the worker's
// connector-secrets manifest binds its "fakeerp-wif" reference to the Fake
// ERP's token endpoint with the worker's projected service-account token as
// the client assertion (deployments/k8s/connector-secrets.federated.json).
const tenantF = "00000000-0000-4000-8000-0000000000a5"

// workerSubject is the federated subject: the worker's own service account.
const workerSubject = "system:serviceaccount:eacp:eacp-worker"

// TestFederatedJITDemo shows ADR-019 Rev 1.1 (Phase 24b) on Kubernetes: the
// worker holds no client secret for this binding. It presents the token the
// kubelet projects for its own service account, the Fake ERP verifies it
// against the cluster's issuer and JWKS, and purchases execute with the
// tokens it mints. Neither an assertion nor a token appears in responses,
// logs or the database.
func TestFederatedJITDemo(t *testing.T) {
	if os.Getenv("EACP_DEMO_PLATFORM") != "k8s" {
		t.Skip("the federated demo needs the cluster's service-account issuer: run scripts/k8s-e2e.sh (docs/KUBERNETES.md)")
	}
	d := newDemo(t, tenantF)
	d.subject, d.secretRef = "carol@hooli.test", "fakeerp-wif"

	d.step("F0. Bootstrap tenant Hooli; a policy allows routine ERP work")
	d.tenantWithCast("hooli", "Hooli", []member{
		{"erin", "registry_editor"}, {"rita", "registry_approver"}, {"ravi", "registry_approver"},
		{"carol", "approver"}, {"audra", "auditor"},
	})
	policy := d.must(201, "alice", "POST", "/v1/policies", map[string]any{"content": json.RawMessage(`{
		"format_version": 1, "rules": [
		{"id": "routine", "match": {"target": "erp"}, "verdict": "allow", "reason": "routine purchase"}]}`)})
	d.must(204, "bob", "POST", "/v1/policies/"+policy["id"].(string)+"/activate", map[string]any{"reason": "reviewed"})

	d.step("F1. The ERP connector names secret_ref fakeerp-wif: the worker federates its service account per mint")
	d.register()
	var keys []string
	for _, idem := range []string{"wif-1", "wif-2", "wif-3"} {
		id := d.submit(idem, "purchase", "erp.create_po", map[string]any{"amount": 100})
		d.until(id, "SUCCEEDED")
		d.onePO(id)
		keys = append(keys, d.action(id)["operation_key"].(string))
	}

	d.step("F2. The ERP audit: tokens were issued to the federated client for the worker's assertion")
	entries := d.erpAudit()
	issued, assertions := map[string]bool{}, map[string]bool{}
	for _, e := range entries {
		if e.Path == "/oauth/token" && e.Outcome == "token_issued" && e.Principal == "oauth:eacp-worker-wif" {
			issued[e.TokenSHA256] = true
			if e.AssertionSHA256 == "" {
				d.t.Fatal("a federated issuance recorded no assertion hash")
			}
			assertions[e.AssertionSHA256] = true
		}
	}
	for _, k := range keys {
		for _, e := range entries {
			if e.OperationKey == k && e.Principal != "oauth:eacp-worker-wif" {
				d.t.Fatalf("operation %s used principal %q, not a federated token", k, e.Principal)
			}
		}
	}
	if len(issued) == 0 {
		d.t.Fatal("the ERP issued no federated token")
	}
	d.logf("3 purchases, one PO each, as principal oauth:eacp-worker-wif; %d token(s) minted for %d assertion(s) "+
		"of %s", len(issued), len(assertions), workerSubject)

	d.step("F3. Search responses, logs and the database for the assertions and every issued token")
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
		if n := containsAssertion(text, assertions, workerSubject); n > 0 {
			d.t.Fatalf("%d service-account assertion(s) appear in %s", n, where)
		}
	}
	d.logf("no assertion and none of the %d issued tokens appears in responses, logs or the database", len(issued))
}

// jwtRun matches compact JWS-shaped strings.
var jwtRun = regexp.MustCompile(`[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)

// containsAssertion counts the JWTs in text that are an audited assertion
// (by SHA-256) or whose payload names subject: any service-account token of
// the worker, audited or not.
func containsAssertion(text string, hashes map[string]bool, subject string) int {
	n := 0
	for _, m := range jwtRun.FindAllString(text, -1) {
		sum := sha256.Sum256([]byte(m))
		if hashes[hex.EncodeToString(sum[:])] {
			n++
			continue
		}
		payload, err := base64.RawURLEncoding.DecodeString(strings.Split(m, ".")[1])
		var claims struct{ Sub string }
		if err == nil && json.Unmarshal(payload, &claims) == nil && claims.Sub == subject {
			n++
		}
	}
	return n
}

func TestContainsAssertionFindsAJWT(t *testing.T) {
	seg := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	jwt := seg(`{"alg":"RS256"}`) + "." + seg(`{"sub":"`+workerSubject+`"}`) + ".c2ln"
	if containsAssertion("token="+jwt+" end", nil, workerSubject) != 1 {
		t.Fatal("a JWT naming the worker was missed")
	}
	other := seg(`{"alg":"RS256"}`) + "." + seg(`{"sub":"someone"}`) + ".c2ln"
	sum := sha256.Sum256([]byte(other))
	if containsAssertion(other, map[string]bool{hex.EncodeToString(sum[:]): true}, workerSubject) != 1 {
		t.Fatal("an audited assertion was missed")
	}
	if containsAssertion(other+" eacp.worker.claim", nil, workerSubject) != 0 {
		t.Fatal("a false positive")
	}
}
