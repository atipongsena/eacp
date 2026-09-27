// Package demo runs the Slice A and Slice C demo scripts (MASTER_PLAN §111)
// against a running demo stack: see docs/DEMO.md and scripts/demo.sh.
package demo

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"eacp/integrations/governance/microsoftagt"
	"eacp/internal/identity"
)

// The demo tenants: the local connector-secrets manifest binds the Fake ERP
// credential to both, and the Fake MCP credential to Slice C's
// (deployments/docker/secrets). Each demo has its own tenant, so they can
// run on the same stack in either order.
const (
	tenantA = "00000000-0000-4000-8000-0000000000d1"
	tenantC = "00000000-0000-4000-8000-0000000000c3"
)

type demo struct {
	t      *testing.T
	tenant string
	root   string
	p      platform
	api    string
	token  string // the Fake ERP credential: only the worker and the ERP hold it

	mu        sync.Mutex
	responses []string // every API response body, for the secret scan
	keys      map[string]string
	reader    string      // who reads actions: the submitting agent by default
	subject   string      // the purchases' subject (post)
	secretRef string      // the ERP connector's secret_ref (register)
	extra     [][2]string // more ERP connectors (name, secret_ref), each with create_po (register)
	client    *http.Client
	retries   atomic.Int64 // requests call had to send again
	ids       map[string]string
	contracts map[string]map[string]any
}

// newDemo connects to the demo stack for tenant.
func newDemo(t *testing.T, tenant string) *demo {
	if os.Getenv("EACP_DEMO") != "1" {
		t.Skip("set EACP_DEMO=1 and run scripts/demo.sh (docs/DEMO.md)")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(filepath.Join(root, "deployments", "docker", "secrets", "fakeerp-token.dev"))
	if err != nil {
		t.Fatalf("run deployments/docker/secrets/prepare_fakeerp_token.py first: %v", err)
	}
	d := &demo{t: t, tenant: tenant, root: root, p: newPlatform(t, root),
		api: env("EACP_DEMO_API", "http://127.0.0.1:18080"), token: strings.TrimSpace(string(token)),
		subject: "carol@acme.test", secretRef: "fakeerp", client: http.DefaultClient, keys: map[string]string{}, ids: map[string]string{}, contracts: map[string]map[string]any{}}
	d.ready()
	return d
}

func TestSliceADemo(t *testing.T) {
	d := newDemo(t, tenantA)

	d.step("0. Bootstrap tenant Acme with two admins (eacpctl, break-glass owner path)")
	d.bootstrap()

	d.step("1. Register the procurement agent: owner, version, tool allowlist")
	d.register()

	d.step("2. The agent tries to call Fake ERP directly")
	d.bypass()

	d.step("3. The agent requests a tool outside its allowlist")
	denied := d.submit("outside-allowlist", "purchase", "erp.cancel_po", map[string]any{"po": "PO-1"})
	d.until(denied, "DENIED")
	d.logf("DENIED before governance: %s", d.action(denied)["state_reason"])

	d.step("4. A routine purchase is governed (allow) and executed through the Go fabric")
	routine := d.submit("routine-1", "purchase", "erp.create_po", map[string]any{"amount": 1200})
	d.until(routine, "SUCCEEDED")
	d.onePO(routine)

	d.step("5. A high-value purchase needs two approvers; self-approval is rejected")
	high := d.submit("high-value-1", "purchase_high_value", "erp.create_po", map[string]any{"amount": 2400000})
	d.until(high, "PENDING_APPROVAL")
	request := d.action(high)["approval_request_id"].(string)
	code, body := d.call("carol", "POST", "/v1/approvals/"+request+"/votes", map[string]any{
		"decision": "APPROVE", "reason": "my own purchase"})
	if code/100 != 4 {
		d.t.Fatalf("carol (subject and agent owner) approved her own purchase: %d %v", code, body)
	}
	d.logf("carol (subject and owner) cannot approve: HTTP %d %v", code, body["error"])
	d.vote("amy", request, "PENDING")

	d.step("6. Restart the API, the worker and PostgreSQL while the approval is pending")
	d.p.restart("controlplane-api", "execution-worker", "postgres")
	d.ready()
	code, body = d.call("ben", "GET", "/v1/approvals/"+request, nil)
	if code != 200 || body["state"] != "PENDING" {
		d.t.Fatalf("approval after restart = %d %v", code, body)
	}
	d.logf("approval %s is still PENDING", request)
	d.vote("ben", request, "GRANTED") // amy's vote survived: ben's completes the quorum
	d.until(high, "SUCCEEDED")
	d.onePO(high)

	d.step("7. Kill the worker mid-dispatch: UNKNOWN_OUTCOME, reconcile, exactly one PO")
	killed := d.submit("killed-1", "purchase", "erp.create_po", map[string]any{"amount": 900,
		"scenario": "slow_response", "delay_ms": 5000})
	d.until(killed, "EXECUTING")
	d.p.kill("execution-worker")
	d.logf("execution-worker killed while its call was in flight")
	d.p.start("execution-worker") // a new worker process
	d.until(killed, "SUCCEEDED")
	// The journal shows the path: the lease lapsed during the call (T23),
	// the outcome was reconciled (T28, T30), and nothing was re-dispatched.
	moves := d.moves(killed)
	if !slices.Equal(moves, []string{"RECEIVED", "AUTHORIZED", "QUEUED", "LEASED", "EXECUTING",
		"UNKNOWN_OUTCOME: lease expired during the call", "RECONCILING", "SUCCEEDED"}) {
		d.t.Fatalf("journaled path = %v", moves)
	}
	d.logf("journaled path: %s", strings.Join(moves, " → "))
	d.onePO(killed)
	d.checks(killed, "found")

	d.step("8. Duplicate submissions produce one action and one PO")
	var wg sync.WaitGroup
	dup := make([]string, 5)
	for i := range dup {
		wg.Go(func() {
			dup[i] = d.submit("duplicate-1", "purchase", "erp.create_po", map[string]any{"amount": 450})
		})
	}
	wg.Wait()
	for _, id := range dup {
		if id != dup[0] {
			d.t.Fatalf("one idempotency key produced actions %v", dup)
		}
	}
	d.until(dup[0], "SUCCEEDED")
	d.logf("5 concurrent submissions → action %s", dup[0])
	d.onePO(dup[0])

	d.step("9. Execute, then time out: the outcome is reconciled, not guessed")
	timeout := d.submit("timeout-1", "purchase", "erp.create_po", map[string]any{"amount": 700,
		"scenario": "execute_then_timeout", "delay_ms": 5000})
	d.until(timeout, "UNKNOWN_OUTCOME", "RECONCILING", "SUCCEEDED")
	d.until(timeout, "SUCCEEDED")
	d.onePO(timeout)
	d.checks(timeout, "found")

	d.step("10. Delayed visibility: no retry; a human resolves with evidence")
	hidden := d.submit("hidden-1", "purchase", "erp.create_po_eventual", map[string]any{"amount": 300,
		"scenario": "execute_then_timeout", "delay_ms": 5000, "visibility_delay_ms": 600000})
	d.until(hidden, "NEEDS_HUMAN_RESOLUTION")
	d.logf("after BEST_EFFORT lookups: %s", d.action(hidden)["state_reason"])
	d.checks(hidden, "absent", "absent", "absent")
	if a := d.action(hidden); a["attempt_count"] != float64(1) {
		d.t.Fatalf("the unknown write was retried: %v", a)
	}
	code, body = d.call("otto", "POST", "/v1/actions/"+hidden+"/resolutions", map[string]any{
		"outcome": "succeeded", "reason": "ERP back office shows the purchase order",
		"evidence": "ERP search by operation key, 2026-09-23", "external_reference": "PO-" + hidden})
	if code != 200 {
		d.t.Fatalf("resolution = %d %v", code, body)
	}
	d.until(hidden, "SUCCEEDED")
	d.logf("otto (operator) resolved it: SUCCEEDED, %s", d.action(hidden)["external_reference"])
	d.onePO(hidden)

	d.step("11. The AGT PDP goes down: new actions wait, cancel still works, recovery resumes")
	d.p.stop("agt-pdp")
	waiting := d.unavailable("pdp-down-1", map[string]any{"amount": 150})
	dropped := d.unavailable("pdp-down-2", map[string]any{"amount": 175})
	cancelled := d.must(200, "agent", "POST", "/v1/actions/"+dropped+"/cancel", map[string]any{"reason": "no longer needed"})
	if cancelled["state"] != "CANCELLED" {
		d.t.Fatalf("cancel during the PDP outage = %v", cancelled)
	}
	d.logf("cancel needs no PDP: %s → CANCELLED", dropped[:8])
	d.p.start("agt-pdp")
	d.logf("agt-pdp restarted; the sweeper evaluates the waiting action")
	d.until(waiting, "SUCCEEDED")
	d.onePO(waiting)

	d.step("12. NATS goes down: purchases still execute by polling, and the outbox drains afterwards")
	d.natsOutage()

	d.step("13. A hard budget: 100 concurrent purchases race for a budget that fits 37")
	d.budgetRace()

	d.step("14. Reconstruct the high-value purchase from its action_id")
	d.evidence(high)

	d.step("15. Search for the ERP credential in API responses, logs and the database")
	d.secretScan()

	d.step("Slice A demo complete")
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func (d *demo) step(s string) { d.t.Logf("\n=== %s", s) }
func (d *demo) logf(f string, a ...any) {
	d.t.Helper()
	d.t.Logf("    "+f, a...)
}

func (d *demo) ready() {
	d.t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		resp, err := http.Get(d.api + "/readyz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		if time.Now().After(deadline) {
			d.t.Fatalf("the demo API at %s is not ready: %v", d.api, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// call sends one API request as a named key holder and records the body.
func (d *demo) call(who, method, path string, body any, headers ...string) (int, map[string]any) {
	d.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, d.api+path, rd)
	if err != nil {
		d.t.Fatal(err)
	}
	d.mu.Lock()
	key := d.keys[who]
	d.mu.Unlock()
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	var resp *http.Response
	for try := 0; ; try++ { // ride out a restart in progress
		resp, err = d.client.Do(req)
		if err == nil || try == 20 {
			break
		}
		d.retries.Add(1)
		time.Sleep(500 * time.Millisecond)
		if rd != nil {
			b, _ := json.Marshal(body)
			req.Body = io.NopCloser(bytes.NewReader(b))
		}
	}
	if err != nil {
		d.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	d.mu.Lock()
	d.responses = append(d.responses, string(raw))
	d.mu.Unlock()
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func (d *demo) must(want int, who, method, path string, body any) map[string]any {
	d.t.Helper()
	code, out := d.call(who, method, path, body)
	if code != want {
		d.t.Fatalf("%s %s as %s = %d %v, want %d", method, path, who, code, out, want)
	}
	return out
}

// newKey generates a key that its holder keeps; only its hash is registered.
func (d *demo) newKey(who string, kind identity.Kind) (credential uuid.UUID, hash string) {
	d.t.Helper()
	credential = uuid.New()
	key, h, err := identity.NewKey(kind, uuid.MustParse(d.tenant), credential)
	if err != nil {
		d.t.Fatal(err)
	}
	d.mu.Lock()
	d.keys[who] = key
	d.mu.Unlock()
	return credential, hex.EncodeToString(h)
}

func (d *demo) bootstrap() {
	d.tenantWithCast("acme", "Acme", []member{
		{"erin", "registry_editor"}, {"rita", "registry_approver"}, {"ravi", "registry_approver"},
		{"otto", "operator"}, {"amy", "approver"}, {"ben", "approver"}, {"carol", "approver"}, {"audra", "auditor"},
	})
	d.logf("people: erin (registry editor), rita and ravi (registry approvers), otto (operator), " +
		"amy, ben and carol (approvers), audra (auditor); every grant and key approved by a second admin")

	policy := d.must(201, "alice", "POST", "/v1/policies", map[string]any{"content": json.RawMessage(`{
		"format_version": 1, "rules": [
		{"id": "high-value", "match": {"operation": "purchase_high_value"}, "verdict": "escalate",
		 "reason": "high-value purchase needs two approvers",
		 "approval": {"quorum": 2, "eligible_roles": ["approver"], "ttl_seconds": 3600}},
		{"id": "routine", "match": {"target": "erp"}, "verdict": "allow", "reason": "routine purchase"}]}`)})
	d.must(204, "bob", "POST", "/v1/policies/"+policy["id"].(string)+"/activate", map[string]any{"reason": "reviewed"})
	d.logf("policy v%v active: high-value purchases escalate to two approvers", policy["version"])
}

type member struct{ name, role string }

// tenantWithCast creates d's tenant with admins alice and bob (eacpctl, the
// owner's break-glass path). Then, for each member, alice proposes the
// person, their role and the key they generated themselves; bob approves.
func (d *demo) tenantWithCast(slug, name string, cast []member) {
	var admins []string
	for _, n := range []string{"alice", "bob"} {
		cred, hash := d.newKey(n, identity.KindPrincipal)
		admins = append(admins, "--admin",
			fmt.Sprintf("name=%s,subject=%s@%s.test,credential=%s,hash=%s", n, n, slug, cred, hash))
	}
	out, err := d.p.eacpctl(append([]string{"tenant", "create",
		"--id", d.tenant, "--slug", slug, "--name", name}, admins...)...)
	if err != nil {
		d.t.Fatalf("eacpctl tenant create: %v\n%s\nThe demo needs a fresh stack: run scripts/demo.sh.", err, out)
	}
	me := d.must(200, "alice", "GET", "/v1/me", nil)
	d.logf("tenant %s (%s): alice and bob are admins %v", d.tenant, name, me["roles"])
	for _, c := range cast {
		p := d.must(201, "alice", "POST", "/v1/principals", map[string]any{"kind": "human", "name": c.name,
			"subject": c.name + "@" + slug + ".test", "display_name": strings.ToUpper(c.name[:1]) + c.name[1:]})
		id := p["id"].(string)
		d.ids[c.name] = id
		g := d.must(201, "alice", "POST", "/v1/role-grants", map[string]any{"principal_id": id, "role": c.role})
		d.must(204, "bob", "POST", "/v1/role-grants/"+g["id"].(string)+"/approve", nil)
		cred, hash := d.newKey(c.name, identity.KindPrincipal)
		d.must(201, "alice", "POST", "/v1/credentials", map[string]any{"id": cred, "kind": identity.KindPrincipal,
			"principal_id": id, "hash": hash, "expires_in_days": 1})
		d.must(204, "bob", "POST", "/v1/credentials/"+cred.String()+"/approve", nil)
	}
}

func (d *demo) register() {
	conn := d.must(201, "erin", "POST", "/v1/connectors", map[string]any{"name": "erp", "protocol": "http",
		"endpoint": "http://fakeerp:8090", "secret_ref": d.secretRef})
	contracts := map[string]map[string]any{
		"create_po": {"side_effects": []string{"IRREVERSIBLE_WRITE", "FINANCIAL"}, "idempotency_mode": "native",
			"idempotency_key_field": "Idempotency-Key", "reconciliation_lookup": "by_operation_key",
			"reconciliation_consistency": "strong", "proof_standard": "authoritative",
			"no_effect_errors": []string{"validation"}, "max_attempts": 2, "timeout_ms": 3000},
		"create_po_eventual": {"side_effects": []string{"IRREVERSIBLE_WRITE", "FINANCIAL"},
			"idempotency_mode": "correlation_only", "correlation_field": "external_reference",
			"reconciliation_lookup": "by_operation_key", "reconciliation_consistency": "eventual",
			"proof_standard": "best_effort", "no_effect_errors": []string{"validation"}, "max_attempts": 1,
			"timeout_ms": 2000},
		"cancel_po": {"side_effects": []string{"IRREVERSIBLE_WRITE"}, "idempotency_mode": "native",
			"idempotency_key_field": "Idempotency-Key", "reconciliation_lookup": "by_operation_key",
			"reconciliation_consistency": "strong", "proof_standard": "authoritative", "max_attempts": 1},
	}
	for _, name := range []string{"create_po", "create_po_eventual", "cancel_po"} {
		tool := d.must(201, "erin", "POST", "/v1/connectors/"+conn["id"].(string)+"/tools", map[string]any{"name": name})
		c := d.must(201, "erin", "POST", "/v1/tools/"+tool["id"].(string)+"/contracts", contracts[name])
		d.must(204, "rita", "POST", "/v1/tools/"+tool["id"].(string)+"/contract", map[string]any{"contract_id": c["id"]})
		d.contracts[name] = contracts[name]
		d.ids["tool "+name] = tool["id"].(string)
	}
	allowed := []string{"erp.create_po", "erp.create_po_eventual"}
	for _, x := range d.extra {
		other := d.must(201, "erin", "POST", "/v1/connectors", map[string]any{"name": x[0], "protocol": "http",
			"endpoint": "http://fakeerp:8090", "secret_ref": x[1]})
		tool := d.must(201, "erin", "POST", "/v1/connectors/"+other["id"].(string)+"/tools", map[string]any{"name": "create_po"})
		c := d.must(201, "erin", "POST", "/v1/tools/"+tool["id"].(string)+"/contracts", contracts["create_po"])
		d.must(204, "rita", "POST", "/v1/tools/"+tool["id"].(string)+"/contract", map[string]any{"contract_id": c["id"]})
		d.ids["tool "+x[0]+".create_po"] = tool["id"].(string)
		allowed = append(allowed, x[0]+".create_po")
		d.logf("connector %s (http://fakeerp:8090, secret_ref %s), tool create_po; contract approved by rita", x[0], x[1])
	}
	d.logf("connector erp (http://fakeerp:8090; its credential lives only in the worker), tools create_po " +
		"(AUTHORITATIVE lookup), create_po_eventual (BEST_EFFORT) and cancel_po; contracts approved by rita")

	agent := d.must(201, "erin", "POST", "/v1/agents", map[string]any{"name": "procurement-bot",
		"display_name": "Procurement bot", "environment": "production", "risk_class": "high",
		"owner_principal_id": d.ids["carol"]})
	version := d.must(201, "erin", "POST", "/v1/agents/"+agent["id"].(string)+"/versions",
		map[string]any{"runtime": "python", "code_ref": "git:demo"})
	vid := version["id"].(string)
	d.ids["agent procurement-bot"] = agent["id"].(string)
	al := d.must(201, "erin", "POST", "/v1/agent-versions/"+vid+"/allowlists",
		map[string]any{"tools": allowed})
	d.must(204, "rita", "POST", "/v1/agent-versions/"+vid+"/allowlist", map[string]any{"allowlist_id": al["id"]})
	d.must(204, "ravi", "POST", "/v1/agent-versions/"+vid+"/transitions", map[string]any{"to": "ACTIVE", "reason": "go live"})
	cred, hash := d.newKey("agent", identity.KindAgent)
	d.must(201, "erin", "POST", "/v1/credentials", map[string]any{"id": cred, "kind": identity.KindAgent,
		"agent_version_id": vid, "hash": hash, "expires_in_days": 1})
	d.must(204, "rita", "POST", "/v1/credentials/"+cred.String()+"/approve", nil)
	self := d.must(200, "agent", "GET", "/v1/agent/self", nil)
	d.logf("agent procurement-bot (owner carol) version %s is %s with allowlist [%s]",
		vid, self["state"], strings.Join(allowed, ", "))
}

// bypass shows that the agent runtime has no route to the ERP and no
// credential, while it can reach the control plane.
func (d *demo) bypass() {
	if out, err := d.p.agent("wget", "-q", "-T", "3", "-O-",
		"http://fakeerp:8090/v1/operations/x"); err == nil {
		d.t.Fatalf("the agent reached Fake ERP directly:\n%s", out)
	} else {
		d.logf("agent → fakeerp:8090 fails: %s", strings.TrimSpace(firstLine(out)))
	}
	if out, err := d.p.agent("ls", "/run/secrets"); err == nil {
		d.t.Fatalf("the agent has secrets mounted:\n%s", out)
	}
	d.logf("the agent has no /run/secrets and no ERP credential")
	if out, err := d.p.agent("wget", "-q", "-T", "3", "-O-", "http://controlplane-api:8080/readyz"); err != nil {
		d.t.Fatalf("the agent cannot reach the control plane: %v\n%s", err, out)
	}
	d.logf("agent → controlplane-api:8080 works: the control plane is its only path")
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// sql runs a query as the PostgreSQL superuser in the demo database and
// returns its single value.
func (d *demo) sql(query string) string {
	d.t.Helper()
	out, err := d.p.postgres("psql", "-U", "postgres", "-d", "eacp", "-tAc", query)
	if err != nil {
		d.t.Fatalf("psql %q: %v\n%s", query, err, out)
	}
	return strings.TrimSpace(out)
}

// natsOutage shows ADR-014 §1: NATS carries hints and events only. Before
// the outage the relay has published the demo's outbox; with NATS stopped a
// purchase still executes exactly once (the worker polls PostgreSQL) and its
// outbox rows wait; after a restart the relay publishes them.
func (d *demo) natsOutage() {
	d.t.Helper()
	const pending = `SELECT count(*) FROM eacp.outbox_events WHERE published_at IS NULL`
	d.waitSQL(pending, "0")
	d.logf("NATS up: %s outbox rows published as work hints and dashboard events",
		d.sql(`SELECT count(*) FROM eacp.outbox_events WHERE published_at IS NOT NULL`))
	d.p.stop("nats")
	id := d.submit("nats-down-1", "purchase", "erp.create_po", map[string]any{"amount": 120})
	d.until(id, "SUCCEEDED")
	d.onePO(id)
	waiting := d.sql(pending)
	if waiting == "0" {
		d.t.Fatal("outbox rows were marked published while NATS was down")
	}
	d.logf("NATS down: %s executed by polling; %s outbox rows wait", id[:8], waiting)
	d.p.start("nats")
	d.waitSQL(pending, "0")
	d.logf("NATS restarted: the relay published every waiting row")
}

// budgetRace shows ADR-012 and §103 invariant 3: create_po gets a costed
// contract version (the payload's amount in THB, two-person), the agent a
// THB budget of 3 700 (a two-person raise), and 100 purchases of 100 THB
// are submitted at once. Exactly 37 execute, one PO each; 63 are denied
// for budget; the account ends exactly at its limit.
func (d *demo) budgetRace() {
	d.t.Helper()
	costed := map[string]any{"cost_unit": "THB", "cost_amount_field": "amount", "cost_unit_field": "currency"}
	for k, v := range d.contracts["create_po"] {
		costed[k] = v
	}
	tool := d.ids["tool create_po"]
	c := d.must(201, "erin", "POST", "/v1/tools/"+tool+"/contracts", costed)
	d.must(204, "rita", "POST", "/v1/tools/"+tool+"/contract", map[string]any{"contract_id": c["id"]})
	d.logf("erin proposes create_po contract v2 (a call costs the payload's amount in THB); rita activates it")
	acct := d.must(201, "alice", "POST", "/v1/budgets", map[string]any{"name": "procurement-bot-thb", "unit": "THB",
		"agent_id": d.ids["agent procurement-bot"]})
	path := "/v1/budgets/" + acct["id"].(string)
	raise := d.must(201, "alice", "POST", path+"/limit", map[string]any{"limit": "3700", "reason": "demo budget"})
	if code, body := d.call("alice", "POST", "/v1/budget-limit-changes/"+raise["id"].(string)+"/approve",
		map[string]any{"reason": "my own raise"}); code != 403 {
		d.t.Fatalf("alice approved her own raise: %d %v", code, body)
	}
	d.must(200, "bob", "POST", "/v1/budget-limit-changes/"+raise["id"].(string)+"/approve", map[string]any{"reason": "agreed"})
	d.logf("alice proposes a 3 700 THB limit for procurement-bot; she cannot approve it herself; bob does")

	const n, fits = 100, 37
	began := time.Now()
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			code, body := d.post(fmt.Sprintf("budget-%03d", i), "purchase", "erp.create_po", map[string]any{"amount": 100})
			if code != 200 && code != 202 && code != 503 {
				d.t.Errorf("budget purchase %d = %d %v", i, code, body)
			}
		})
	}
	wg.Wait()
	d.logf("%d concurrent purchases of 100 THB submitted in %v", n, time.Since(began).Round(time.Millisecond))
	const race = `FROM eacp.actions WHERE idempotency_key LIKE 'budget-%'`
	d.waitSQL(`SELECT count(*) `+race+` AND state IN ('SUCCEEDED', 'DENIED')`, fmt.Sprint(n))
	succeeded := d.sql(`SELECT count(*) ` + race + ` AND state = 'SUCCEEDED'`)
	denied := d.sql(`SELECT count(*) ` + race + ` AND state = 'DENIED' AND state_reason = 'budget_exceeded'`)
	if succeeded != fmt.Sprint(fits) || denied != fmt.Sprint(n-fits) {
		d.t.Fatalf("budget race: %s succeeded and %s were denied for budget, want %d and %d", succeeded, denied, fits, n-fits)
	}
	pos := d.committedPOs()
	for _, k := range strings.Fields(d.sql(`SELECT string_agg(operation_key, ' ') ` + race + ` AND state = 'SUCCEEDED'`)) {
		if pos[k] != 1 {
			d.t.Fatalf("ERP holds %d purchase orders for %s, want 1", pos[k], k)
		}
	}
	for _, k := range strings.Fields(d.sql(`SELECT string_agg(operation_key, ' ') ` + race + ` AND state = 'DENIED'`)) {
		if pos[k] != 0 {
			d.t.Fatalf("a denied purchase reached the ERP: %s", k)
		}
	}
	d.logf("%s SUCCEEDED with exactly one PO each; %s DENIED budget_exceeded, none reached the ERP", succeeded, denied)
	a := d.must(200, "audra", "GET", path, nil)
	if a["committed"] != float64(3700) || a["reserved"] != float64(0) || a["available"] != float64(0) {
		d.t.Fatalf("budget account = %v", a)
	}
	d.logf("account procurement-bot-thb: limit %v, committed %v, available %v", a["hard_limit"], a["committed"], a["available"])
}

// waitSQL waits until query returns want.
func (d *demo) waitSQL(query, want string) {
	d.t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for got := d.sql(query); got != want; got = d.sql(query) {
		if time.Now().After(deadline) {
			d.t.Fatalf("%s = %s, want %s", query, got, want)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// submit submits an action as the agent and returns its id.
func (d *demo) submit(idem, operation, tool string, payload map[string]any) string {
	d.t.Helper()
	code, body := d.post(idem, operation, tool, payload)
	if code != 200 && code != 202 {
		d.t.Fatalf("submit %s = %d %v", idem, code, body)
	}
	id, _ := body["id"].(string)
	if id == "" {
		d.t.Fatalf("submit %s: no action id in %v", idem, body)
	}
	return id
}

func (d *demo) post(idem, operation, tool string, payload map[string]any) (int, map[string]any) {
	d.t.Helper()
	payload["currency"] = "THB"
	return d.call("agent", "POST", "/v1/actions?wait=2s", map[string]any{"subject": d.subject,
		"operation": operation, "target": "erp", "tool": tool, "tool_schema_version": "1", "resource": "po",
		"payload": payload}, "Idempotency-Key", idem)
}

// unavailable submits a routine purchase while the PDP is down: ADR-002 §6
// requires 503 with the action persisted RECEIVED, never DENIED.
func (d *demo) unavailable(idem string, payload map[string]any) string {
	d.t.Helper()
	code, body := d.post(idem, "purchase", "erp.create_po", payload)
	id, _ := body["action_id"].(string)
	if code != 503 || body["error"] != "governance_unavailable" || body["state"] != "RECEIVED" || id == "" {
		d.t.Fatalf("submit while the PDP is down = %d %v", code, body)
	}
	d.logf("PDP down: HTTP 503 governance_unavailable, action %s stays RECEIVED", id[:8])
	return id
}

func (d *demo) action(id string) map[string]any {
	d.t.Helper()
	reader := d.reader
	if reader == "" {
		reader = "agent"
	}
	code, body := d.call(reader, "GET", "/v1/actions/"+id, nil)
	if code != 200 && code != 202 {
		d.t.Fatalf("GET action %s = %d %v", id, code, body)
	}
	return body
}

// until waits for action id to reach one of states.
func (d *demo) until(id string, states ...string) {
	d.t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	last := ""
	for {
		a := d.action(id)
		state, _ := a["state"].(string)
		if state != last {
			d.logf("%s → %s", id[:8], state)
			last = state
		}
		if slices.Contains(states, state) {
			return
		}
		if time.Now().After(deadline) {
			d.t.Fatalf("action %s is %s (%v), want %v", id, state, a["state_reason"], states)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func (d *demo) vote(who, request, want string) {
	d.t.Helper()
	body := d.must(200, who, "POST", "/v1/approvals/"+request+"/votes", map[string]any{
		"decision": "APPROVE", "reason": "within budget and supplier approved"})
	if body["request_state"] != want {
		d.t.Fatalf("after %s's vote the request is %v, want %s", who, body["request_state"], want)
	}
	d.logf("%s approves: request %s", who, want)
}

// committedPOs counts the purchase orders Fake ERP committed per
// operation key, from its audit (read with the ERP credential, as an
// auditor of the ERP would).
func (d *demo) committedPOs() map[string]int {
	d.t.Helper()
	pos := map[string]int{}
	for _, e := range d.erpAudit() {
		if e.Outcome == "effect_committed" {
			pos[e.OperationKey]++
		}
	}
	return pos
}

// erpEntry is one Fake ERP audit entry.
type erpEntry struct {
	Principal    string `json:"principal"`
	Path         string `json:"path"`
	OperationKey string `json:"operation_key"`
	Outcome      string `json:"outcome"`
	TokenSHA256  string `json:"token_sha256"` // a token issuance: never the token
	// A federated issuance: the assertion's SHA-256, never the assertion.
	AssertionSHA256 string `json:"assertion_sha256"`
	// A private_key_jwt issuance: the assertion's jti, accepted once.
	AssertionJTI string `json:"assertion_jti"`
}

// erpAudit reads the Fake ERP audit with the ERP credential, as an auditor
// of the ERP would.
func (d *demo) erpAudit() []erpEntry {
	d.t.Helper()
	out, err := d.p.erpAudit(d.token)
	if err != nil {
		d.t.Fatalf("reading the ERP audit: %v", err)
	}
	var entries []erpEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		d.t.Fatalf("ERP audit: %v", err)
	}
	return entries
}

// onePO checks that Fake ERP committed exactly one purchase order for the
// action.
func (d *demo) onePO(id string) {
	d.t.Helper()
	key := d.action(id)["operation_key"].(string)
	if n := d.committedPOs()[key]; n != 1 {
		d.t.Fatalf("ERP holds %d purchase orders for %s, want exactly 1", n, key)
	}
	d.logf("ERP audit: exactly one purchase order for %s", key)
}

func (d *demo) checks(id string, want ...string) {
	d.t.Helper()
	ev := d.must(200, "audra", "GET", "/v1/actions/"+id+"/evidence", nil)
	var got []string
	checks, _ := ev["reconciliation_checks"].([]any)
	for _, c := range checks {
		got = append(got, c.(map[string]any)["result"].(string))
	}
	if !slices.Equal(got, want) {
		d.t.Fatalf("reconciliation checks of %s = %v, want %v", id, got, want)
	}
	d.logf("reconciliation checks: %v", got)
}

func (d *demo) evidence(id string) {
	d.t.Helper()
	ev := d.must(200, "audra", "GET", "/v1/actions/"+id+"/evidence", nil)
	for _, x := range ev["decisions"].([]any) {
		dec := x.(map[string]any)
		d.logf("governance: %s under policy v%v (%v), enforced digest %.16s…", dec["verdict"], dec["policy_version"],
			dec["reasons"], dec["enforced_digest"])
		// ADR-002 §8: the decision came from the AGT sidecar, and its
		// evidence names the engine stack and rule that decided.
		pe, _ := dec["provider_evidence"].(map[string]any)
		versions, _ := pe["versions"].(map[string]any)
		if dec["provider"] != microsoftagt.ProviderName || pe["rule_id"] != "high-value" ||
			versions["agt"] != microsoftagt.Pinned.AGT || versions["acs"] != microsoftagt.Pinned.ACS ||
			versions["opa"] != microsoftagt.Pinned.OPA {
			d.t.Fatalf("decision provenance = %v %v", dec["provider"], dec["provider_evidence"])
		}
		d.logf("  decided by %s (%v): AGT %v, ACS %v, OPA %v, rule %v, ACS identity %.23s…", dec["provider"],
			dec["provider_instance_id"], versions["agt"], versions["acs"], versions["opa"], pe["rule_id"], pe["acs_action_identity"])
	}
	for _, x := range ev["approvals"].([]any) {
		a := x.(map[string]any)
		d.logf("approval %s: %s, quorum %v", a["id"], a["state"], a["required_quorum"])
		for _, v := range a["votes"].([]any) {
			vote := v.(map[string]any)
			d.logf("  vote %s by %s: %q", vote["decision"], d.name(vote["approver_principal_id"].(string)), vote["reason"])
		}
		if g, ok := a["grant"].(map[string]any); !ok || g["consumed_by_action_id"] != id {
			d.t.Fatalf("grant = %v", a["grant"])
		}
		d.logf("  one-time grant consumed by this action at release")
	}
	for _, x := range ev["attempts"].([]any) {
		at := x.(map[string]any)
		d.logf("attempt %v by %s: %s %s (operation key %s)", at["attempt"], at["worker_id"], at["outcome"],
			at["external_reference"], at["operation_key"])
	}
	var moves []string
	for _, x := range ev["journal"].([]any) {
		j := x.(map[string]any)
		if data, ok := j["data"].(map[string]any); ok && strings.HasPrefix(j["event"].(string), "action.") && data["to"] != nil {
			moves = append(moves, data["to"].(string))
		}
	}
	d.logf("journal: %s", strings.Join(moves, " → "))
	chain := ev["chain"].(map[string]any)
	if chain["verified"] != true {
		d.t.Fatalf("the journal chain does not verify: %v", chain)
	}
	d.logf("journal: %d entries about this action; the tenant's chain of %v entries verifies (head %.16s…)",
		len(ev["journal"].([]any)), chain["count"], chain["head"])
}

// moves returns the journaled states of action id, with the reason for
// UNKNOWN_OUTCOME.
func (d *demo) moves(id string) []string {
	d.t.Helper()
	ev := d.must(200, "audra", "GET", "/v1/actions/"+id+"/evidence", nil)
	var out []string
	for _, x := range ev["journal"].([]any) {
		j := x.(map[string]any)
		data, _ := j["data"].(map[string]any)
		to, _ := data["to"].(string)
		if to == "" || (j["event"] != "action.received" && j["event"] != "action.transition") {
			continue
		}
		if to == "UNKNOWN_OUTCOME" {
			to += ": " + j["reason"].(string)
		}
		out = append(out, to)
	}
	return out
}

func (d *demo) name(id string) string {
	for n, v := range d.ids {
		if v == id {
			return n
		}
	}
	return id
}

func (d *demo) secretScan() {
	d.t.Helper()
	secrets := map[string]string{"the ERP credential": d.token}
	if mcp, err := os.ReadFile(filepath.Join(d.root, "deployments", "docker", "secrets", "fakemcp-token.dev")); err == nil {
		secrets["the MCP credential"] = strings.TrimSpace(string(mcp))
	} else {
		d.t.Fatalf("reading the MCP credential: %v", err)
	}
	find := func(where, text string) {
		for name, secret := range secrets {
			if strings.Contains(text, secret) {
				d.t.Fatalf("%s appears in %s", name, where)
			}
		}
	}
	d.mu.Lock()
	responses := strings.Join(d.responses, "\n")
	d.mu.Unlock()
	find("an API response", responses)
	d.logf("%d API responses: no credential", len(d.responses))
	logs := d.p.logs()
	find("a service log", logs)
	d.logf("%d lines of service logs: no credential", strings.Count(logs, "\n"))
	dump, err := d.p.postgres("pg_dump", "-U", "postgres", "--data-only", "eacp")
	if err != nil {
		d.t.Fatalf("pg_dump: %v", err)
	}
	find("the database", dump)
	d.logf("database dump of %d bytes: no credential", len(dump))
}
