package demo

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// tenantX is the token-exchange demo's tenant: on Kubernetes the worker's
// connector-secrets manifest binds "fakeerp-sts" to an RFC 8693 exchange of
// the worker's projected service-account token, and "fakeerp-sts-sa" to an
// exchange of its JWT-SVID whose federated token impersonates a service
// account (deployments/k8s/connector-secrets.exchange.json, ADR-019 Rev 1.5).
const tenantX = "00000000-0000-4000-8000-0000000000a9"

// impersonatedAccount is the service account "fakeerp-sts-sa" impersonates.
const impersonatedAccount = "eacp-erp@eacp-demo.iam.gserviceaccount.com"

// TestTokenExchangeDemo shows ADR-019 Rev 1.5 (Phase 24f) on Kubernetes: the
// worker holds no client credential for either binding. It trades a platform
// identity at Fake ERP's STS for an access token of its own subject, or for a
// federated token that only buys a service account's token. No subject token,
// federated token or access token appears in responses, logs or the database.
func TestTokenExchangeDemo(t *testing.T) {
	if os.Getenv("EACP_DEMO_PLATFORM") != "k8s" {
		t.Skip("the token-exchange demo needs the cluster's identities: run scripts/k8s-e2e.sh (docs/KUBERNETES.md)")
	}
	d := newDemo(t, tenantX)
	d.subject, d.secretRef = "carol@tyrell.test", "fakeerp-sts"
	d.extra = [][2]string{{"erp-oauth", "fakeerp-sts-sa"}}

	d.step("X0. Bootstrap tenant Tyrell; a policy allows routine ERP work")
	d.tenantWithCast("tyrell", "Tyrell", []member{
		{"erin", "registry_editor"}, {"rita", "registry_approver"}, {"ravi", "registry_approver"},
		{"carol", "approver"}, {"audra", "auditor"},
	})
	policy := d.must(201, "alice", "POST", "/v1/policies", map[string]any{"content": json.RawMessage(`{
		"format_version": 1, "rules": [
		{"id": "routine", "match": {"target": "erp"}, "verdict": "allow", "reason": "routine purchase"}]}`)})
	d.must(204, "bob", "POST", "/v1/policies/"+policy["id"].(string)+"/activate", map[string]any{"reason": "reviewed"})

	d.step("X1. Two connectors, no client credential: the projected token exchanged, and the JWT-SVID exchanged then impersonating")
	d.register()
	principals := map[string]string{} // operation key -> the principal the ERP must see
	for _, p := range []struct{ idem, tool, principal string }{
		{"sts-1", "erp.create_po", "sts:" + workerSubject},
		{"sts-sa-1", "erp-oauth.create_po", "sa:" + impersonatedAccount},
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
			if e.OperationKey != key {
				continue
			}
			seen = true
			if e.Principal != want {
				d.t.Fatalf("operation %s used principal %q, want %q", key, e.Principal, want)
			}
		}
		if !seen {
			d.t.Fatalf("the ERP audit has no call for operation %s", key)
		}
	}
	issued, subjects := map[string]bool{}, map[string]bool{} // SHA-256 of every token issued and subject exchanged
	exchanged := map[string]int{}                            // subject -> exchanges
	impersonations := 0
	for _, e := range entries {
		if e.Outcome != "token_issued" {
			continue
		}
		switch {
		case strings.HasPrefix(e.Principal, "sts:"):
			if e.Path != "/oauth/token" || e.SubjectSHA256 == "" {
				d.t.Fatalf("an exchange recorded no subject hash: %+v", e)
			}
			issued[e.TokenSHA256], subjects[e.SubjectSHA256] = true, true
			exchanged[strings.TrimPrefix(e.Principal, "sts:")]++
		case e.Principal == "sa:"+impersonatedAccount:
			issued[e.TokenSHA256] = true
			impersonations++
		}
	}
	if exchanged[workerSubject] == 0 || exchanged[workerSPIFFEID] == 0 || impersonations == 0 {
		d.t.Fatalf("exchanges %v, impersonations %d: want both subjects exchanged and one impersonation", exchanged, impersonations)
	}
	d.logf("erp.create_po ran as principal sts:%s (%d exchange(s) of the projected token); erp-oauth.create_po as "+
		"sa:%s (%d exchange(s) of the JWT-SVID, %d impersonation(s))", workerSubject, exchanged[workerSubject],
		impersonatedAccount, exchanged[workerSPIFFEID], impersonations)

	d.step("X2. Search responses, logs and the database for subject tokens and every issued token")
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
		for _, subject := range []string{workerSubject, workerSPIFFEID} {
			if n := containsAssertion(text, subjects, subject); n > 0 {
				d.t.Fatalf("%d subject token(s) appear in %s", n, where)
			}
		}
	}
	d.logf("no subject token and none of the %d issued tokens appears in responses, logs or the database", len(issued))
}
