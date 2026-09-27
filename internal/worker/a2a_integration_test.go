package worker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/action"
	"eacp/internal/connector/a2a"
	"eacp/internal/fakea2a"
	"eacp/internal/governance"
	"eacp/internal/registry/registrytest"
	"eacp/internal/storage"
	"eacp/internal/storage/pgtest"
	"eacp/internal/worker"
)

const a2aToken = canary + "-a2a"

// a2aEnv is a tenant with an A2A connector to the fake agent, its delegate
// discovered and certified, and an agent allowed to delegate.
type a2aEnv struct {
	t        *testing.T
	f        *registrytest.Fixture
	srv      *httptest.Server
	endpoint string
	cardFile string
	conn     uuid.UUID
	tool     uuid.UUID
	secrets  *worker.SecretStore
	scanner  *worker.Scanner
	w        *worker.Worker
	e        *action.Engine
	agent    registrytest.Agent
	logs     *bytes.Buffer
	logMu    *sync.Mutex
}

func a2aCard(endpoint string, skills ...string) []byte {
	var s []any
	for _, id := range append([]string{"purchase"}, skills...) {
		s = append(s, map[string]any{"id": id, "name": id, "description": "Skill " + id, "tags": []any{"erp"}})
	}
	b, _ := json.Marshal(map[string]any{"name": "Fake Procurement Agent", "description": "Raises purchase orders",
		"version": "1.0.0", "supportedInterfaces": []any{map[string]any{"url": endpoint, "protocolBinding": "JSONRPC",
			"protocolVersion": "1.0"}}, "capabilities": map[string]any{"streaming": false, "pushNotifications": false},
		"defaultInputModes": []any{"text/plain"}, "defaultOutputModes": []any{"text/plain"}, "skills": s})
	return b
}

// newA2AEnv certifies delegate with the no-effect classes noEffect and a
// call budget of timeoutMS.
func newA2AEnv(t *testing.T, noEffect []string, timeoutMS int) *a2aEnv {
	t.Helper()
	v := &a2aEnv{t: t, logs: &bytes.Buffer{}, logMu: &sync.Mutex{}, cardFile: filepath.Join(t.TempDir(), "card.json")}
	var handler atomic.Value
	v.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.Load().(http.Handler).ServeHTTP(w, r)
	}))
	t.Cleanup(v.srv.Close)
	v.endpoint = v.srv.URL + fakea2a.Path
	if err := os.WriteFile(v.cardFile, a2aCard(v.endpoint), 0o600); err != nil {
		t.Fatal(err)
	}
	agent, err := fakea2a.New(a2aToken, v.cardFile, filepath.Join(t.TempDir(), "a2a.log"), v.endpoint)
	if err != nil {
		t.Fatal(err)
	}
	handler.Store(http.Handler(agent))

	v.f = registrytest.New(t)
	v.conn = v.f.ID(t, "erin", `INSERT INTO eacp.connectors (tenant_id, name, protocol, endpoint, secret_ref)
		VALUES (eacp.current_tenant_id(), 'procurement', 'a2a', $1, 'procurement') RETURNING id`, v.endpoint)
	v.secrets, err = worker.LoadSecrets(secretsFile(t, fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"procurement",
		"host":%q,"value":%q}]}`, pgtest.TenantA, strings.TrimPrefix(v.srv.URL, "http://"), a2aToken)))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewJSONHandler(syncWriter{v.logs, v.logMu}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	client := a2a.New()
	client.PollStart, client.Log = 100*time.Millisecond, log
	v.scanner, err = worker.NewScanner(v.f.App, worker.ScannerOptions{ID: "scan-a", Interval: 15 * time.Minute,
		Timeout: 5 * time.Second, Secrets: v.secrets, Discoverers: map[string]worker.Discoverer{"a2a": client}, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	runScan(t, v.scanner, 1)

	var def uuid.UUID
	v.owner(`SELECT id, definition_id FROM eacp.tools WHERE connector_id = $1 AND remote_name = 'delegate'`,
		[]any{v.conn}, &v.tool, &def)
	contract := v.f.ID(t, "erin", `INSERT INTO eacp.tool_contracts
		(tenant_id, tool_id, definition_id, side_effects, idempotency_mode, reconciliation_lookup,
		 reconciliation_consistency, proof_standard, no_effect_errors, max_attempts, timeout_ms)
		VALUES (eacp.current_tenant_id(), $1, $2, '{IRREVERSIBLE_WRITE,FINANCIAL}', 'none', 'none', 'none', 'none',
		 $3::text[], 1, $4) RETURNING id`, v.tool, def, noEffect, timeoutMS)
	if err := v.f.Exec("rita", `UPDATE eacp.tools SET active_contract_id = $1 WHERE id = $2`, contract, v.tool); err != nil {
		t.Fatal(err)
	}
	v.agent = v.f.ActiveAgent(t, "buyer", v.tool)
	v.f.ActivatePolicy(t, registrytest.AllowPolicy)
	v.e = action.New(v.f.App, action.Options{Provider: governance.LocalProvider{InstanceID: "a2a-test"}})
	v.w, err = worker.New(v.f.App, worker.Options{ID: "w1", Lease: 10 * time.Second, Log: log,
		Connectors: map[string]worker.Connector{"a2a": client}, Secrets: v.secrets,
		Backoff: func(int) time.Duration { return time.Hour }})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (v *a2aEnv) owner(sql string, args []any, dest ...any) {
	v.t.Helper()
	err := storage.InTenantTx(context.Background(), v.f.Owner, v.f.Tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), sql, args...).Scan(dest...)
	})
	if err != nil {
		v.t.Fatalf("%v\nSQL: %s", err, sql)
	}
}

// delegate submits a delegation whose data part picks the fake agent's
// scenario and runs the worker once when the action is queued.
func (v *a2aEnv) delegate(scenario string) action.View {
	v.t.Helper()
	payload := map[string]any{"text": "Buy 10 laptops"}
	if scenario != "" {
		payload["data"] = map[string]any{"scenario": scenario}
	}
	b, _ := json.Marshal(payload)
	got, err := v.e.Submit(context.Background(), action.Agent(v.f.Tenant, v.agent.Agent, v.agent.Version),
		action.Submission{IdempotencyKey: uuid.NewString(), Subject: "carol@tenant-a.test", Operation: "delegate",
			Target: "procurement", Tool: "procurement.delegate", ToolSchemaVersion: "1", Resource: "purchase", Payload: b})
	if err != nil {
		v.t.Fatal(err)
	}
	if got.State != "QUEUED" {
		return got
	}
	if n, err := v.w.RunOnce(context.Background()); err != nil || n != 1 {
		v.t.Fatalf("dispatched %d, %v", n, err)
	}
	got, err = v.e.Get(context.Background(), v.f.Tenant, got.ID)
	if err != nil {
		v.t.Fatal(err)
	}
	return got
}

func (v *a2aEnv) evidence(id uuid.UUID) action.Evidence {
	v.t.Helper()
	ev, err := v.e.Evidence(context.Background(), v.f.Tenant, id)
	if err != nil {
		v.t.Fatal(err)
	}
	return ev
}

type a2aEntry struct {
	Method    string `json:"method"`
	MessageID string `json:"message_id"`
	TaskID    string `json:"task_id"`
	State     string `json:"state"`
}

func (v *a2aEnv) audit() []a2aEntry {
	v.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, v.srv.URL+"/v1/audit", nil)
	req.Header.Set("Authorization", "Bearer "+a2aToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		v.t.Fatal(err)
	}
	defer resp.Body.Close()
	var entries []a2aEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		v.t.Fatal(err)
	}
	return entries
}

func (v *a2aEnv) count(method, field, value string) int {
	n := 0
	for _, e := range v.audit() {
		if e.Method == method && ((field == "message" && e.MessageID == value) || (field == "task" && e.TaskID == value)) {
			n++
		}
	}
	return n
}

// TestTheWorkerDelegatesToAnA2AAgent: the scanner discovers delegate, two
// people certify it, and the worker delegates through the real connector:
// a completed task succeeds, an unfinished one is an unknown outcome whose
// task is cancelled once, a certified rejection is a failure, and a changed
// card quarantines delegate.
func TestTheWorkerDelegatesToAnA2AAgent(t *testing.T) {
	v := newA2AEnv(t, []string{"a2a_rejected"}, 1000)

	done := v.delegate("")
	if done.State != "SUCCEEDED" || !strings.HasPrefix(done.ExternalReference, "task-") {
		t.Fatalf("completed delegation: %s %q", done.State, done.ExternalReference)
	}
	if n := v.count("SendMessage", "message", done.ID.String()); n != 1 {
		t.Fatalf("%d messages carry the action id", n)
	}

	hang := v.delegate("hang")
	ev := v.evidence(hang.ID)
	if hang.State != "UNKNOWN_OUTCOME" || len(ev.Attempts) != 1 || ev.Attempts[0].ErrorClass != "a2a_interrupted" ||
		!strings.HasPrefix(ev.Attempts[0].RemoteReference, "task-") {
		t.Fatalf("hung delegation: %s, attempts %+v", hang.State, ev.Attempts)
	}
	if n := v.count("CancelTask", "task", ev.Attempts[0].RemoteReference); n != 1 {
		t.Fatalf("%d cancels of the hung task", n)
	}

	input := v.delegate("input_required")
	ev = v.evidence(input.ID)
	if input.State != "UNKNOWN_OUTCOME" || ev.Attempts[0].ErrorClass != "a2a_input_required" ||
		v.count("CancelTask", "task", ev.Attempts[0].RemoteReference) != 1 {
		t.Fatalf("input required: %s, attempts %+v", input.State, ev.Attempts)
	}

	rejected := v.delegate("reject")
	if rejected.State != "FAILED" || v.evidence(rejected.ID).Attempts[0].RemoteReference == "" {
		t.Fatalf("rejected delegation: %s", rejected.State)
	}

	// The agent gains a skill: the rescan quarantines the certified delegate.
	if err := os.WriteFile(v.cardFile, a2aCard(v.endpoint, "pay"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := v.f.Exec("otto", `UPDATE eacp.mcp_servers SET requested_at = now(), request_reason = 'vendor release'
		WHERE connector_id = $1`, v.conn); err != nil {
		t.Fatal(err)
	}
	runScan(t, v.scanner, 1)
	if denied := v.delegate(""); denied.State != "DENIED" || denied.StateReason != "tool_quarantined" {
		t.Fatalf("after the card changed: %s %s", denied.State, denied.StateReason)
	}
	if n := len(v.audit()); n == 0 {
		t.Fatal("no audit")
	}
}

// TestAnUncertifiedRejectionIsUnknown: without a2a_rejected in the
// contract, a rejection proves nothing and a human settles it.
func TestAnUncertifiedRejectionIsUnknown(t *testing.T) {
	v := newA2AEnv(t, []string{}, 1000)
	if got := v.delegate("reject"); got.State != "UNKNOWN_OUTCOME" {
		t.Fatalf("uncertified rejection: %s", got.State)
	}
}

// TestNothingSecretIsPersistedByADelegation: the agent's token appears in no
// row, journal entry, outbox message or worker log.
func TestNothingSecretIsPersistedByADelegation(t *testing.T) {
	v := newA2AEnv(t, []string{"a2a_rejected"}, 1000)
	for _, s := range []string{"", "hang", "reject"} {
		v.delegate(s)
	}
	ctx := context.Background()
	for _, sql := range []string{
		`SELECT count(*) FROM eacp.actions WHERE to_jsonb(actions)::text LIKE '%' || $1 || '%'`,
		`SELECT count(*) FROM eacp.action_attempts WHERE to_jsonb(action_attempts)::text LIKE '%' || $1 || '%'`,
		`SELECT count(*) FROM eacp.audit_events WHERE convert_from(payload, 'UTF8') LIKE '%' || $1 || '%'`,
		`SELECT count(*) FROM eacp.outbox_events WHERE to_jsonb(outbox_events)::text LIKE '%' || $1 || '%'`,
		`SELECT count(*) FROM eacp.tool_definitions WHERE definition LIKE '%' || $1 || '%' OR display LIKE '%' || $1 || '%'`,
		`SELECT count(*) FROM eacp.mcp_scans WHERE to_jsonb(mcp_scans)::text LIKE '%' || $1 || '%'`,
	} {
		var count int
		if err := storage.InTenantTx(ctx, v.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, sql, a2aToken).Scan(&count)
		}); err != nil || count != 0 {
			t.Fatalf("the token was persisted (count=%d err=%v): %s", count, err, sql)
		}
	}
	v.logMu.Lock()
	defer v.logMu.Unlock()
	if strings.Contains(v.logs.String(), a2aToken) || !strings.Contains(v.logs.String(), "a2a delegation") {
		t.Fatal("the token reached the log, or no delegation was logged")
	}
	if strings.Contains(v.logs.String(), "Buy 10 laptops") {
		t.Fatal("message content reached the log")
	}
}
