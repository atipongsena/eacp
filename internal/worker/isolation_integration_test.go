package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"eacp/internal/action"
	"eacp/internal/approval"
	"eacp/internal/bundle"
	"eacp/internal/connector/mcp"
	"eacp/internal/connector/mcp/mcptest"
	"eacp/internal/finops"
	"eacp/internal/fleet"
	"eacp/internal/identity"
	"eacp/internal/incident"
	"eacp/internal/llm"
	"eacp/internal/messaging"
	"eacp/internal/registry"
	"eacp/internal/release"
	"eacp/internal/storage"
	"eacp/internal/storage/pgtest"
	"eacp/internal/worker"
)

// Invariant 8: after a complete flow in tenant A (registry, policy,
// governance, approval, budget, execution, reconciliation, operator
// resolution, journal, outbox and inbox, MCP discovery, kills, fleet operations,
// FinOps, releases, change sets), neither tenant B nor a session without a tenant sees
// or changes a single row of any table.
func TestAnotherTenantSeesAndChangesNothingAfterAFullFlow(t *testing.T) {
	v := newERPEnvWith(t, reviewPolicy)
	ctx := context.Background()
	approve := func(a action.View) {
		t.Helper()
		for _, who := range []string{"amy", "ben"} {
			if _, err := approval.New(v.f.App).Vote(ctx, registry.Actor{TenantID: v.f.Tenant, PrincipalID: v.f.P[who]},
				*a.ApprovalRequestID, approval.Approve, "reviewed"); err != nil {
				t.Fatal(err)
			}
		}
	}
	// A budget on create_po (ADR-012): a costed contract version and a
	// funded account (a two-person limit change); the flow reserves and
	// commits through the worker and the reconciler.
	costed := v.f.ID(t, "erin", `INSERT INTO eacp.tool_contracts (tenant_id, tool_id, side_effects, idempotency_mode,
			idempotency_key_field, correlation_field, reconciliation_lookup, reconciliation_consistency, proof_standard,
			no_effect_errors, max_attempts, timeout_ms, cost_unit, cost_fixed)
		SELECT c.tenant_id, c.tool_id, c.side_effects, c.idempotency_mode, c.idempotency_key_field, c.correlation_field,
			c.reconciliation_lookup, c.reconciliation_consistency, c.proof_standard, c.no_effect_errors, c.max_attempts,
			c.timeout_ms, 'TOOL_CALLS', 1
		FROM eacp.tools t JOIN eacp.tool_contracts c ON c.id = t.active_contract_id WHERE t.name = 'create_po'
		RETURNING id`)
	if err := v.f.Exec("ravi", `UPDATE eacp.tools t SET active_contract_id = c.id FROM eacp.tool_contracts c
		WHERE c.id = $1 AND t.id = c.tool_id`, costed); err != nil {
		t.Fatal(err)
	}
	v.f.FundAgent(t, v.agent.Agent, "TOOL_CALLS", "10")

	found := v.submitTo("PENDING_APPROVAL", "create_po", map[string]any{"scenario": "execute_then_timeout", "delay_ms": 300})
	hidden := v.submitTo("PENDING_APPROVAL", "create_po_eventual", map[string]any{"scenario": "execute_then_timeout",
		"delay_ms": 300, "visibility_delay_ms": 5000})
	approve(found)
	approve(hidden)
	v.sweep()
	v.runWorker(2)
	if got, _ := v.reconcile(v.reconciler(1, 100*time.Millisecond), found.ID); got.State != "SUCCEEDED" {
		t.Fatalf("found = %s", got.State)
	}
	var settled string
	if err := storage.InTenantTx(ctx, v.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state FROM eacp.budget_reservations WHERE action_id = $1`, found.ID).Scan(&settled)
	}); err != nil || settled != "COMMITTED" {
		t.Fatalf("found's reservation = %q %v", settled, err)
	}
	if got, _ := v.reconcile(v.reconciler(1, 100*time.Millisecond), hidden.ID); got.State != "NEEDS_HUMAN_RESOLUTION" {
		t.Fatalf("hidden = %s", got.State)
	}
	if _, _, err := v.e.Resolve(ctx, action.Principal(v.f.Tenant, v.f.P["otto"]), hidden.ID, action.Resolution{
		Outcome: "succeeded", Reason: "ERP back office shows the PO", Evidence: "ERP search",
		ExternalReference: "PO-" + hidden.ID.String()}); err != nil {
		t.Fatal(err)
	}

	// A work hint recorded by the hint consumer's inbox (ADR-014 §5).
	inbox, err := messaging.NewInbox(v.f.App, messaging.WorkConsumer, 0)
	if err != nil {
		t.Fatal(err)
	}
	if fresh, err := inbox.Record(ctx, v.f.Tenant, uuid.New()); err != nil || !fresh {
		t.Fatalf("inbox record = %v, %v", fresh, err)
	}

	// An MCP server discovered by the scanner (ADR-023): scan state, a scan
	// and a tool definition.
	mcpServer := mcptest.New(t, mcptest.Modern, scanToken, scanGetPO)
	mcpID := v.f.ID(t, "erin", `INSERT INTO eacp.connectors (tenant_id, name, protocol, endpoint, secret_ref)
		VALUES (eacp.current_tenant_id(), 'sap-mcp', 'mcp', $1, 'sap-mcp') RETURNING id`, mcpServer.URL())
	mcpSecrets, err := worker.LoadSecrets(secretsFile(t, fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"sap-mcp","host":%q,"value":%q}]}`,
		v.f.Tenant, strings.TrimPrefix(mcpServer.Server.URL, "http://"), scanToken)))
	if err != nil {
		t.Fatal(err)
	}
	scanner, err := worker.NewScanner(v.f.App, worker.ScannerOptions{ID: "scan-a", Secrets: mcpSecrets, Discoverer: mcp.New()})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := scanner.RunOnce(ctx); n != 1 || err != nil {
		t.Fatalf("mcp scan = %d, %v", n, err)
	}
	// Phase 15: the editor's observed dependency is also tenant-isolated.
	v.f.ID(t, "erin", `INSERT INTO eacp.dependency_edges
		(tenant_id, from_kind, from_id, to_kind, to_id, source, confidence, observed_at, expires_at)
		VALUES (eacp.current_tenant_id(), 'agent_version', $1, 'mcp', $2,
		'deployment_manifest', 'high', now(), now() + interval '1 day') RETURNING id`, v.agent.Version, mcpID)
	// Phase 16: a kill creates both scoped state and the tenant epoch. Use
	// an already finished action so the flow above remains deterministic.
	if err := v.f.Exec("otto", `SELECT eacp.set_kill('action', $1, true, 'isolation fixture')`, found.ID); err != nil {
		t.Fatal(err)
	}
	// Phase 17: a fleet operation on an agent the flow does not use.
	spare := v.f.ActiveAgent(t, "spare")
	if _, err := fleet.New(v.f.App).Apply(ctx, registry.Actor{TenantID: v.f.Tenant, PrincipalID: v.f.P["otto"]},
		fleet.Request{Kind: "pause", Selector: fleet.Selector{AgentIDs: []uuid.UUID{spare.Agent}}, Reason: "isolation fixture"}); err != nil {
		t.Fatal(err)
	}
	// Phase 18: a price, a reported span, a billing line, a soft limit and
	// the evaluator's alert for the unpriced span.
	fin := finops.New(v.f.App)
	alice := registry.Actor{TenantID: v.f.Tenant, PrincipalID: v.f.P["alice"]}
	if _, err := fin.AddPrice(ctx, alice, finops.NewPrice{Provider: "openai", Model: "gpt-x", Unit: "USD",
		InputPerMTok: "1", OutputPerMTok: "1", Reason: "isolation fixture"}); err != nil {
		t.Fatal(err)
	}
	if in, err := fin.RecordSpans(ctx, v.f.Tenant, v.agent.Version, []finops.Span{{TraceID: strings.Repeat("ab", 16),
		SpanID: strings.Repeat("cd", 8), Operation: "chat", Provider: "openai", Model: "unpriced", InputTokens: 1,
		ObservedAt: time.Now()}}); err != nil || in.Accepted != 1 {
		t.Fatalf("span = %+v %v", in, err)
	}
	if _, err := fin.ImportBilling(ctx, alice, []finops.BillingLine{{ExternalID: "inv-1", AgentID: v.agent.Agent,
		Provider: "openai", Cost: "1", Unit: "USD", ObservedAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	usd := v.f.ID(t, "alice", `INSERT INTO eacp.budget_accounts (tenant_id, name, unit) VALUES (eacp.current_tenant_id(), 'usd', 'USD') RETURNING id`)
	limit := "100"
	if _, err := fin.SetSoftLimit(ctx, alice, usd, &limit, "isolation fixture"); err != nil {
		t.Fatal(err)
	}
	if n, err := fin.Evaluate(ctx, v.f.Tenant); err != nil || n != 1 {
		t.Fatalf("finops evaluate = %d %v", n, err)
	}

	// Phase 19: a release of a new version of the flow's agent, an evaluation
	// and a replay of the found action. The candidate's allowlist is empty, so
	// the proposal is denied by capability and needs no PDP.
	candidate := v.f.ID(t, "erin", `INSERT INTO eacp.agent_versions (tenant_id, agent_id, runtime, code_ref)
		VALUES (eacp.current_tenant_id(), $1, 'python', 'git:next') RETURNING id`, v.agent.Agent)
	emptyAllowlist := v.f.ID(t, "erin", `INSERT INTO eacp.agent_allowlists (tenant_id, agent_version_id, tool_ids)
		VALUES (eacp.current_tenant_id(), $1, '{}') RETURNING id`, candidate)
	if err := v.f.Exec("rita", `UPDATE eacp.agent_versions SET active_allowlist_id = $1 WHERE id = $2`, emptyAllowlist, candidate); err != nil {
		t.Fatal(err)
	}
	rel := release.New(v.f.App, release.Options{})
	erin := registry.Actor{TenantID: v.f.Tenant, PrincipalID: v.f.P["erin"]}
	opened, err := rel.Open(ctx, erin, release.OpenRequest{CandidateVersionID: candidate, Reason: "isolation fixture",
		Plan: release.Plan{RequiredSuites: []string{"accuracy"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rel.RecordEvaluation(ctx, erin, opened.ID, release.EvaluationInput{Suite: "accuracy", Score: "1",
		Threshold: "0.5", DatasetDigest: strings.Repeat("ab", 32), EvidenceRef: "ci://isolation"}); err != nil {
		t.Fatal(err)
	}
	p := release.Proposal{Kind: "replay", ReferenceActionID: found.ID}
	var payload string
	if err := storage.InTenantTx(ctx, v.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT subject, operation, target, tool, tool_schema_version, resource, input_payload::text
			FROM eacp.actions WHERE id = $1`, found.ID).Scan(&p.Subject, &p.Operation, &p.Target, &p.Tool,
			&p.ToolSchemaVersion, &p.Resource, &payload)
	}); err != nil {
		t.Fatal(err)
	}
	p.Payload = []byte(payload)
	if o, err := rel.Observe(ctx, release.Agent{TenantID: v.f.Tenant, AgentID: v.agent.Agent, VersionID: candidate}, p); err != nil ||
		o.CapabilityDenial == nil {
		t.Fatalf("replay = %+v, %v", o, err)
	}

	// Phase 20: a bundle with one connector and tool, planned and submitted
	// by erin and approved by rita (ADR-026).
	gac := bundle.New(v.f.App)
	cs, err := gac.Plan(ctx, erin, bundle.Request{Bundle: "ledger", Desired: json.RawMessage(`{
	 "connectors": {"ledger": {"protocol": "http", "endpoint": "http://fakeerp:8090", "secret_ref": "ledger",
	   "tools": {"post_entry": {"contract": {"side_effects": ["READ_ONLY"], "idempotency_mode": "none",
	     "reconciliation_lookup": "none", "reconciliation_consistency": "none", "proof_standard": "none",
	     "max_attempts": 3}}}}}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gac.Submit(ctx, erin, cs.ID); err != nil {
		t.Fatal(err)
	}
	if cs, err := gac.Approve(ctx, registry.Actor{TenantID: v.f.Tenant, PrincipalID: v.f.P["rita"]}, cs.ID); err != nil ||
		cs.State != "APPLIED" {
		t.Fatalf("bundle approve = %+v, %v", cs, err)
	}

	// Phase 22a: a manual incident with a note (ADR-027).
	inc := incident.New(v.f.App)
	otto := registry.Actor{TenantID: v.f.Tenant, PrincipalID: v.f.P["otto"]}
	triage, err := inc.Open(ctx, otto, incident.NewIncident{Title: "odd purchases", Severity: "medium", Reason: "triage"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inc.Note(ctx, otto, triage.ID, "looking into it"); err != nil {
		t.Fatal(err)
	}

	// Phase 25b: a declared model and a gateway call the ledger refused
	// (the flow's allowlist names no models, ADR-031).
	v.f.ID(t, "erin", `INSERT INTO eacp.llm_models (tenant_id, name, provider, base_url, upstream_model, secret_ref,
		max_output_tokens) VALUES (eacp.current_tenant_id(), 'sonnet', 'anthropic', 'https://llm.invalid', 'up-1', 'llm', 1000)
		RETURNING id`)
	if adm, err := llm.New(v.f.App).Admit(ctx, v.f.Tenant, llm.AdmitRequest{AgentVersionID: v.agent.Version,
		ModelName: "sonnet", Provider: "anthropic", GatewayID: "gw-a", RequestBytes: 10,
		Decision: llm.Decision{ID: uuid.New(), BundleID: uuid.New(), Version: 1, Verdict: "allow"}}); err != nil ||
		adm.Denial != "model_not_in_allowlist" {
		t.Fatalf("llm admit = %+v, %v", adm, err)
	}

	// Registry records the action flow does not touch: a group with a member
	// and an approved principal key.
	g := v.f.ID(t, "alice", `INSERT INTO eacp.groups (tenant_id, name, display_name)
		VALUES (eacp.current_tenant_id(), 'buyers', 'Buyers') RETURNING id`)
	v.f.ID(t, "alice", `INSERT INTO eacp.group_memberships (tenant_id, group_id, principal_id)
		VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, g, v.f.P["carol"])
	credID := uuid.New()
	_, hash, err := identity.NewKey(identity.KindPrincipal, v.f.Tenant, credID)
	if err != nil {
		t.Fatal(err)
	}
	v.f.ID(t, "alice", `INSERT INTO eacp.credentials (tenant_id, id, kind, principal_id, secret_hash, expires_at)
		VALUES (eacp.current_tenant_id(), $1, 'pk', $2, $3, now() + interval '1 day') RETURNING id`, credID, v.f.P["carol"], hash)
	if err := v.f.Exec("bob", `UPDATE eacp.credentials SET approved_at = now() WHERE id = $1`, credID); err != nil {
		t.Fatal(err)
	}

	admin, err := pgx.Connect(ctx, v.f.DB.AdminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	rows, err := admin.Query(ctx, `SELECT relname FROM pg_class
		WHERE relnamespace = 'eacp'::regnamespace AND relkind IN ('r', 'p') ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	// ref names table and the column holding its tenant.
	ref := func(table string) (name, key string) {
		if table == "tenants" {
			return "eacp.tenants", "id"
		}
		return "eacp." + pgx.Identifier{table}.Sanitize(), "tenant_id"
	}
	ofA := " = '" + pgtest.TenantA + "'"
	// count counts tenant A's rows of table as seen from tenant ("" for none).
	count := func(tenant, table string) (n int) {
		t.Helper()
		name, key := ref(table)
		sql := "SELECT count(*) FROM " + name + " WHERE " + key + ofA
		if tenant == "" {
			if err := v.f.App.QueryRow(ctx, sql).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		if err := storage.InTenantTx(ctx, v.f.App, tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, sql).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// change runs sql as tenant B and returns the rows it changed; a refusal
	// by privileges or policy changes nothing.
	change := func(sql string) int64 {
		t.Helper()
		var n int64
		err := storage.InTenantTx(ctx, v.f.App, pgtest.TenantB, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, sql)
			n = tag.RowsAffected()
			return err
		})
		var pg *pgconn.PgError
		if err != nil && !(errors.As(err, &pg) && pg.Code == "42501") {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}

	var empty []string
	for _, table := range tables {
		inA := count(pgtest.TenantA, table)
		if inA == 0 {
			empty = append(empty, table)
		}
		if inB, none := count(pgtest.TenantB, table), count("", table); inB != 0 || none != 0 {
			t.Errorf("%s: tenant A sees %d rows, tenant B %d, no tenant %d", table, inA, inB, none)
		}
		name, key := ref(table)
		if n := change("UPDATE " + name + " SET " + key + " = " + key + " WHERE " + key + ofA); n != 0 {
			t.Errorf("tenant B updated %d rows of %s", n, table)
		}
		if n := change("DELETE FROM " + name + " WHERE " + key + ofA); n != 0 {
			t.Errorf("tenant B deleted %d rows of %s", n, table)
		}
		if after := count(pgtest.TenantA, table); after != inA {
			t.Errorf("%s: tenant A had %d rows, now %d", table, inA, after)
		}
	}
	// The flow touched every table, so the sweep above is not vacuous.
	if len(empty) != 0 {
		t.Fatalf("tables without tenant A rows: %v", empty)
	}
}
