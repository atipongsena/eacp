package demo

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// tenantS is the SPIFFE demo's tenant: on Kubernetes the worker's
// connector-secrets manifest binds "fakeerp-spiffe" to the worker's JWT-SVID
// for audience fakeerp-api and "fakeerp-spiffe-oauth" to an OAuth client that
// authenticates with its JWT-SVID for audience fakeerp-token
// (deployments/k8s/connector-secrets.spiffe.json, ADR-019 Rev 1.4).
const tenantS = "00000000-0000-4000-8000-0000000000a8"

// workerSPIFFEID is the SPIFFE ID the development SPIRE issues the worker
// (namespace eacp, ServiceAccount eacp-worker; scripts/k8s-e2e.sh).
const workerSPIFFEID = "spiffe://eacp.test/ns/eacp/sa/eacp-worker"

// TestSPIFFEDemo shows ADR-019 Rev 1.4 (Phase 24e) on Kubernetes: the worker
// holds no secret for either binding. The SPIFFE CSI driver mounts the SPIRE
// agent's Workload API into the worker alone; the worker fetches its JWT-SVID
// for the ERP's audience and presents it as the Bearer credential, or as the
// client assertion of an OAuth mint. No SVID or token appears in responses,
// logs or the database.
func TestSPIFFEDemo(t *testing.T) {
	if os.Getenv("EACP_DEMO_PLATFORM") != "k8s" {
		t.Skip("the SPIFFE demo needs the cluster's SPIRE: run scripts/k8s-e2e.sh (docs/KUBERNETES.md)")
	}
	d := newDemo(t, tenantS)
	d.subject, d.secretRef = "carol@cyberdyne.test", "fakeerp-spiffe"
	d.extra = [][2]string{{"erp-oauth", "fakeerp-spiffe-oauth"}}

	d.step("S0. Bootstrap tenant Cyberdyne; a policy allows routine ERP work")
	d.tenantWithCast("cyberdyne", "Cyberdyne", []member{
		{"erin", "registry_editor"}, {"rita", "registry_approver"}, {"ravi", "registry_approver"},
		{"carol", "approver"}, {"audra", "auditor"},
	})
	policy := d.must(201, "alice", "POST", "/v1/policies", map[string]any{"content": json.RawMessage(`{
		"format_version": 1, "rules": [
		{"id": "routine", "match": {"target": "erp"}, "verdict": "allow", "reason": "routine purchase"}]}`)})
	d.must(204, "bob", "POST", "/v1/policies/"+policy["id"].(string)+"/activate", map[string]any{"reason": "reviewed"})

	d.step("S1. Two connectors, no secret: the worker's JWT-SVID as the ERP's Bearer and as an OAuth client assertion")
	d.register()
	principals := map[string]string{} // operation key -> the principal the ERP must see
	for _, p := range []struct{ idem, tool, principal string }{
		{"spiffe-bearer-1", "erp.create_po", "spiffe:" + workerSPIFFEID},
		{"spiffe-oauth-1", "erp-oauth.create_po", "oauth:eacp-worker-spiffe"},
	} {
		id := d.submit(p.idem, "purchase", p.tool, map[string]any{"amount": 100})
		d.until(id, "SUCCEEDED")
		d.onePO(id)
		principals[d.action(id)["operation_key"].(string)] = p.principal
	}
	entries := d.erpAudit()
	svids := map[string]bool{} // the SHA-256 of every SVID the ERP saw
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
			if e.Path == "/v1/execute" && want == "spiffe:"+workerSPIFFEID {
				if e.SVIDSHA256 == "" {
					d.t.Fatalf("an SVID execute recorded no SVID hash: %+v", e)
				}
				svids[e.SVIDSHA256] = true
			}
		}
		if !seen {
			d.t.Fatalf("the ERP audit has no call for operation %s", key)
		}
	}
	issued := map[string]bool{}
	for _, e := range entries {
		if e.Path == "/oauth/token" && e.Outcome == "token_issued" && e.Principal == "oauth:eacp-worker-spiffe" {
			issued[e.TokenSHA256] = true
			if e.AssertionSHA256 == "" {
				d.t.Fatal("a SPIFFE issuance recorded no assertion hash")
			}
			svids[e.AssertionSHA256] = true
		}
	}
	if len(issued) == 0 {
		d.t.Fatal("the ERP issued no token to the SPIFFE client")
	}
	d.logf("erp.create_po ran as principal spiffe:%s (its JWT-SVID for fakeerp-api); erp-oauth.create_po as "+
		"oauth:eacp-worker-spiffe (%d token(s) minted with its JWT-SVID for fakeerp-token)", workerSPIFFEID, len(issued))

	d.step("S2. Search responses, logs and the database for SVIDs and every issued token")
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
		if n := containsAssertion(text, svids, workerSPIFFEID); n > 0 {
			d.t.Fatalf("%d JWT-SVID(s) appear in %s", n, where)
		}
	}
	d.logf("no JWT-SVID and none of the %d issued tokens appears in responses, logs or the database", len(issued))
}
