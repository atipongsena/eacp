package worker_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/action"
	"github.com/atipongsena/eacp/internal/connector"
	"github.com/atipongsena/eacp/internal/fakeerp"
	"github.com/atipongsena/eacp/internal/governance"
	"github.com/atipongsena/eacp/internal/registry/registrytest"
	"github.com/atipongsena/eacp/internal/storage"
	"github.com/atipongsena/eacp/internal/storage/pgtest"
	"github.com/atipongsena/eacp/internal/worker"
)

const httpContractSQL = `INSERT INTO eacp.tool_contracts
	(tenant_id, tool_id, side_effects, idempotency_mode, idempotency_key_field,
	 reconciliation_lookup, reconciliation_consistency, proof_standard,
	 no_effect_errors, max_attempts, timeout_ms)
	VALUES (eacp.current_tenant_id(), $1, '{IRREVERSIBLE_WRITE}', 'native', 'Idempotency-Key',
	 'by_operation_key', 'strong', 'authoritative', '{validation}', 2, 100)
	RETURNING id`

func TestWorkerDispatchesRealHTTPToFakeERPWithStableOperationKey(t *testing.T) {
	ctx := context.Background()
	const secretValue = canary + "-http"
	h, err := fakeerp.New(secretValue, filepath.Join(t.TempDir(), "erp.log"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	f := registrytest.New(t)
	connID := f.ID(t, "erin", `INSERT INTO eacp.connectors (tenant_id, name, protocol, endpoint, secret_ref)
		VALUES (eacp.current_tenant_id(), 'erp', 'http', $1, 'erp') RETURNING id`, srv.URL)
	toolID := f.ID(t, "erin", `INSERT INTO eacp.tools (tenant_id, connector_id, name)
		VALUES (eacp.current_tenant_id(), $1, 'create_po') RETURNING id`, connID)
	contractID := f.ID(t, "erin", httpContractSQL, toolID)
	if err := f.Exec("rita", `UPDATE eacp.tools SET active_contract_id = $1 WHERE id = $2`, contractID, toolID); err != nil {
		t.Fatal(err)
	}
	agent := f.ActiveAgent(t, "buyer", toolID)
	f.ActivatePolicy(t, registrytest.AllowPolicy)
	secrets, err := worker.LoadSecrets(secretsFile(t, fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"erp","host":%q,"value":%q}]}`,
		pgtest.TenantA, u.Host, secretValue)))
	if err != nil {
		t.Fatal(err)
	}
	httpConnector := connector.NewHTTP()
	w, err := worker.New(f.App, worker.Options{ID: "real-http", Lease: 5 * time.Second,
		Connectors: map[string]worker.Connector{"http": httpConnector}, Secrets: secrets,
		Backoff: func(int) time.Duration { return time.Second }})
	if err != nil {
		t.Fatal(err)
	}
	engine := action.New(f.App, action.Options{Provider: governance.LocalProvider{InstanceID: "http-test"}})
	submit := func(scenario string) action.View {
		t.Helper()
		payload := map[string]any{"amount": 42, "currency": "THB"}
		if scenario != "" {
			payload["scenario"] = scenario
			payload["delay_ms"] = 300
		}
		b, _ := json.Marshal(payload)
		view, err := engine.Submit(ctx, action.Agent(f.Tenant, agent.Agent, agent.Version),
			action.Submission{IdempotencyKey: uuid.NewString(), Subject: "carol@tenant-a.test",
				Operation: "post", Target: "erp", Tool: "erp.create_po", ToolSchemaVersion: "1",
				Resource: "po", Payload: b})
		if err != nil || view.State != "QUEUED" {
			t.Fatalf("submit = %+v, err = %v", view, err)
		}
		return view
	}
	lookup := func(view action.View) worker.LookupResult {
		t.Helper()
		secret, err := secrets.Resolve(f.Tenant, "erp", srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		return httpConnector.Lookup(ctx, worker.LookupCall{TenantID: f.Tenant,
			OperationKey: view.OperationKey, Endpoint: srv.URL, Secret: secret})
	}
	run := func() {
		t.Helper()
		if n, err := w.RunOnce(ctx); err != nil || n != 1 {
			t.Fatalf("worker ran %d calls, err = %v", n, err)
		}
	}
	success := submit("")
	run()
	got, err := engine.Get(ctx, f.Tenant, success.ID)
	if err != nil || got.State != "SUCCEEDED" || got.ExternalReference == "" || got.AttemptCount != 1 {
		t.Fatalf("successful action = %+v, err = %v", got, err)
	}
	if evidence := lookup(got); evidence.Status != worker.LookupFound || evidence.ExternalReference != got.ExternalReference {
		t.Fatalf("success lookup = %+v", evidence)
	}
	lost := submit("execute_then_timeout")
	run()
	got, err = engine.Get(ctx, f.Tenant, lost.ID)
	if err != nil || got.State != "UNKNOWN_OUTCOME" || got.AttemptCount != 1 {
		t.Fatalf("lost response action = %+v, err = %v", got, err)
	}
	if evidence := lookup(got); evidence.Status != worker.LookupFound || evidence.ExternalReference == "" {
		t.Fatalf("lost response lookup = %+v", evidence)
	}
	if n, err := w.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("ambiguous write redispatched: n=%d err=%v", n, err)
	}
	for _, sql := range []string{
		`SELECT count(*) FROM eacp.actions WHERE to_jsonb(actions)::text LIKE '%' || $1 || '%'`,
		`SELECT count(*) FROM eacp.action_attempts WHERE to_jsonb(action_attempts)::text LIKE '%' || $1 || '%'`,
		`SELECT count(*) FROM eacp.audit_events WHERE convert_from(payload, 'UTF8') LIKE '%' || $1 || '%'`,
		`SELECT count(*) FROM eacp.outbox_events WHERE to_jsonb(outbox_events)::text LIKE '%' || $1 || '%'`,
	} {
		var count int
		err := storage.InTenantTx(ctx, f.App, pgtest.TenantA, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, sql, secretValue).Scan(&count)
		})
		if err != nil || count != 0 {
			t.Fatalf("credential persisted by real connector (count=%d err=%v)", count, err)
		}
	}
	if strings.Contains(got.StateReason, secretValue) {
		t.Fatal("credential in state reason")
	}
}
