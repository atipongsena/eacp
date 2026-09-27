package demo

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// tenantY is the AWS demo's tenant: on Kubernetes the worker's
// connector-secrets manifest binds "fakeerp-aws" to AssumeRoleWithWebIdentity
// with the worker's projected service-account token, and "fakeerp-aws-spiffe"
// with its JWT-SVID; both sign ERP calls with SigV4
// (deployments/k8s/connector-secrets.aws.json, ADR-019 Rev 1.6).
const tenantY = "00000000-0000-4000-8000-0000000000aa"

// assumedRole is the ARN of the role eacp-erp's sessions, without the session name.
const assumedRole = "arn:aws:sts::000000000000:assumed-role/eacp-erp/"

// awsSecretKey re-derives the secret key Fake ERP issued for keyID from its
// credential (internal/fakeerp/aws.go): the demo can then look for it.
func awsSecretKey(credential, keyID string) string {
	m := hmac.New(sha256.New, []byte(credential))
	m.Write([]byte("aws-secret:" + keyID))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// TestAWSDemo shows ADR-019 Rev 1.6 (Phase 24g) on Kubernetes: the worker
// holds no AWS credential. It assumes a role at Fake ERP's STS with a
// platform identity and signs each ERP call with SigV4. No web identity
// token, secret key or session token appears in responses, logs or the
// database.
func TestAWSDemo(t *testing.T) {
	if os.Getenv("EACP_DEMO_PLATFORM") != "k8s" {
		t.Skip("the AWS demo needs the cluster's identities: run scripts/k8s-e2e.sh (docs/KUBERNETES.md)")
	}
	d := newDemo(t, tenantY)
	d.subject, d.secretRef = "carol@soylent.test", "fakeerp-aws"
	d.extra = [][2]string{{"erp-oauth", "fakeerp-aws-spiffe"}}

	d.step("Y0. Bootstrap tenant Soylent; a policy allows routine ERP work")
	d.tenantWithCast("soylent", "Soylent", []member{
		{"erin", "registry_editor"}, {"rita", "registry_approver"}, {"ravi", "registry_approver"},
		{"carol", "approver"}, {"audra", "auditor"},
	})
	policy := d.must(201, "alice", "POST", "/v1/policies", map[string]any{"content": json.RawMessage(`{
		"format_version": 1, "rules": [
		{"id": "routine", "match": {"target": "erp"}, "verdict": "allow", "reason": "routine purchase"}]}`)})
	d.must(204, "bob", "POST", "/v1/policies/"+policy["id"].(string)+"/activate", map[string]any{"reason": "reviewed"})

	d.step("Y1. Two connectors, no AWS credential: a role assumed with the projected token, and with the JWT-SVID")
	d.register()
	principals := map[string]string{} // operation key -> the principal the ERP must see
	for _, p := range []struct{ idem, tool, session string }{
		{"aws-1", "erp.create_po", "eacp-worker-k8s"},
		{"aws-spiffe-1", "erp-oauth.create_po", "eacp-worker-spiffe"},
	} {
		id := d.submit(p.idem, "purchase", p.tool, map[string]any{"amount": 100})
		d.until(id, "SUCCEEDED")
		d.onePO(id)
		principals[d.action(id)["operation_key"].(string)] = "aws:" + assumedRole + p.session
	}
	entries := d.erpAudit()
	keys := map[string]string{} // access key id -> the principal it was issued to
	sessions, subjects := map[string]bool{}, map[string]bool{}
	for _, e := range entries {
		if e.Outcome != "keys_issued" {
			continue
		}
		if e.Path != "/aws/sts" || e.AWSAccessKeyID == "" || e.TokenSHA256 == "" || e.SubjectSHA256 == "" {
			d.t.Fatalf("an issuance recorded no key id, session token hash or subject hash: %+v", e)
		}
		keys[e.AWSAccessKeyID], sessions[e.TokenSHA256], subjects[e.SubjectSHA256] = e.Principal, true, true
	}
	for key, want := range principals {
		seen := false
		for _, e := range entries {
			if e.OperationKey != key {
				continue
			}
			seen = true
			if e.Principal != want || keys[e.AWSAccessKeyID] != want {
				d.t.Fatalf("operation %s used principal %q with key %q, want %q and a key issued to it", key, e.Principal,
					e.AWSAccessKeyID, want)
			}
		}
		if !seen {
			d.t.Fatalf("the ERP audit has no call for operation %s", key)
		}
	}
	d.logf("erp.create_po ran as %seacp-worker-k8s and erp-oauth.create_po as %seacp-worker-spiffe, each signed with "+
		"SigV4 by keys the ERP's STS issued (%d key(s))", assumedRole, assumedRole, len(keys))

	d.step("Y2. Search responses, logs and the database for web identity tokens, secret keys and session tokens")
	d.mu.Lock()
	responses := strings.Join(d.responses, "\n")
	d.mu.Unlock()
	dump, err := d.p.postgres("pg_dump", "-U", "postgres", "--data-only", "eacp")
	if err != nil {
		d.t.Fatalf("pg_dump: %v", err)
	}
	for where, text := range map[string]string{"an API response": responses, "a service log": d.p.logs(), "the database": dump} {
		if n := containsIssued(text, sessions); n > 0 {
			d.t.Fatalf("%d session token(s) appear in %s", n, where)
		}
		for keyID := range keys {
			if strings.Contains(text, awsSecretKey(d.token, keyID)) {
				d.t.Fatalf("the secret key of %s appears in %s", keyID, where)
			}
		}
		for _, subject := range []string{workerSubject, workerSPIFFEID} {
			if n := containsAssertion(text, subjects, subject); n > 0 {
				d.t.Fatalf("%d web identity token(s) appear in %s", n, where)
			}
		}
	}
	d.logf("no web identity token, none of the %d secret keys and none of their session tokens appears in responses, "+
		"logs or the database", len(keys))
}
