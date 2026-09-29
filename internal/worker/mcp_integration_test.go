package worker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/action"
	"github.com/atipongsena/eacp/internal/connector"
	"github.com/atipongsena/eacp/internal/connector/a2a"
	"github.com/atipongsena/eacp/internal/connector/mcp"
	"github.com/atipongsena/eacp/internal/connector/mcp/mcptest"
	"github.com/atipongsena/eacp/internal/governance"
	"github.com/atipongsena/eacp/internal/registry/registrytest"
	"github.com/atipongsena/eacp/internal/storage"
	"github.com/atipongsena/eacp/internal/storage/pgtest"
	"github.com/atipongsena/eacp/internal/worker"
)

const (
	mcpToken = canary + "-mcp"
	// mcpOutput marks everything the fake server answers: it must never
	// leave the worker (ADR-032 S3.6).
	mcpOutput = "tool-output-canary-9b2d"
	// mcpCreate is a tool whose server name is not a valid EACP name, so the
	// registry derives one and the call must use the server's (Review Focus 3).
	mcpCreate  = `{"name":"Create.PO","description":"Create a purchase order","inputSchema":{"type":"object","properties":{"supplier":{"type":"string"},"lines":{"type":"array"}}}}`
	mcpChanged = `{"name":"Create.PO","description":"Create a purchase order and pay it","inputSchema":{"type":"object","properties":{"supplier":{"type":"string"},"lines":{"type":"array"}}}}`
)

var mcpReference = regexp.MustCompile(`^mcp:sha256:[0-9a-f]{64}$`)

// mcpEnv is a tenant with an MCP connector to the fake server, Create.PO
// discovered and certified, and an agent allowed to call it. One client
// serves both the scanner and the worker.
type mcpEnv struct {
	t       *testing.T
	f       *registrytest.Fixture
	srv     *mcptest.Server
	conn    uuid.UUID
	tool    uuid.UUID
	name    string // the derived EACP tool name
	secrets *worker.SecretStore
	client  *mcp.Client
	scanner *worker.Scanner
	w       *worker.Worker
	e       *action.Engine
	agent   registrytest.Agent
	log     *slog.Logger
	logs    *bytes.Buffer
	logMu   *sync.Mutex
}

// newMCPEnv certifies Create.PO with the no-effect classes noEffect and a
// call budget of timeoutMS.
func newMCPEnv(t *testing.T, noEffect []string, timeoutMS int) *mcpEnv {
	t.Helper()
	v := &mcpEnv{t: t, logs: &bytes.Buffer{}, logMu: &sync.Mutex{}}
	v.srv = mcptest.New(t, mcptest.Modern, mcpToken, mcpCreate)
	v.f = registrytest.New(t)
	v.conn = v.f.ID(t, "erin", `INSERT INTO eacp.connectors (tenant_id, name, protocol, endpoint, secret_ref)
		VALUES (eacp.current_tenant_id(), 'sap-mcp', 'mcp', $1, 'sap-mcp') RETURNING id`, v.srv.URL())
	var err error
	v.secrets, err = worker.LoadSecrets(secretsFile(t, fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"sap-mcp",
		"host":%q,"value":%q}]}`, pgtest.TenantA, strings.TrimPrefix(v.srv.Server.URL, "http://"), mcpToken)))
	if err != nil {
		t.Fatal(err)
	}
	v.log = slog.New(slog.NewJSONHandler(syncWriter{v.logs, v.logMu}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	v.client = mcp.New()
	v.client.Log = v.log
	v.scanner, err = worker.NewScanner(v.f.App, worker.ScannerOptions{ID: "scan-a", Interval: 15 * time.Minute,
		Timeout: 5 * time.Second, Secrets: v.secrets, Discoverers: map[string]worker.Discoverer{"mcp": v.client}, Log: v.log})
	if err != nil {
		t.Fatal(err)
	}
	runScan(t, v.scanner, 1)

	var def uuid.UUID
	v.owner(`SELECT id, definition_id, name FROM eacp.tools WHERE connector_id = $1 AND remote_name = 'Create.PO'`,
		[]any{v.conn}, &v.tool, &def, &v.name)
	if v.name == "Create.PO" {
		t.Fatalf("the EACP name %q was not derived from the server name", v.name)
	}
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
	v.e = action.New(v.f.App, action.Options{Provider: governance.LocalProvider{InstanceID: "mcp-test"}})
	v.w = v.worker("w1", map[string]worker.Connector{"mcp": v.client}, 0)
	return v
}

// worker returns a worker with connectors; killPoll 0 keeps the default.
func (v *mcpEnv) worker(id string, connectors map[string]worker.Connector, killPoll time.Duration) *worker.Worker {
	v.t.Helper()
	w, err := worker.New(v.f.App, worker.Options{ID: id, Lease: 30 * time.Second, Log: v.log,
		KillPollInterval: killPoll, Connectors: connectors, Secrets: v.secrets,
		Backoff: func(int) time.Duration { return time.Hour }})
	if err != nil {
		v.t.Fatal(err)
	}
	return w
}

func (v *mcpEnv) owner(sql string, args []any, dest ...any) {
	v.t.Helper()
	err := storage.InTenantTx(context.Background(), v.f.Owner, v.f.Tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), sql, args...).Scan(dest...)
	})
	if err != nil {
		v.t.Fatalf("%v\nSQL: %s", err, sql)
	}
}

// submit submits a call of Create.PO with args.
func (v *mcpEnv) submit(args map[string]any) action.View {
	v.t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		v.t.Fatal(err)
	}
	got, err := v.e.Submit(context.Background(), action.Agent(v.f.Tenant, v.agent.Agent, v.agent.Version),
		action.Submission{IdempotencyKey: uuid.NewString(), Subject: "carol@tenant-a.test", Operation: "create",
			Target: "sap-mcp", Tool: "sap-mcp." + v.name, ToolSchemaVersion: "1", Resource: "purchase_order", Payload: b})
	if err != nil {
		v.t.Fatal(err)
	}
	return got
}

// call submits a call of Create.PO with args and runs the worker once when
// the action is queued.
func (v *mcpEnv) call(args map[string]any) action.View {
	v.t.Helper()
	got := v.submit(args)
	if got.State != "QUEUED" {
		return got
	}
	if n, err := v.w.RunOnce(context.Background()); err != nil || n != 1 {
		v.t.Fatalf("dispatched %d, %v", n, err)
	}
	return v.get(got.ID)
}

func (v *mcpEnv) get(id uuid.UUID) action.View {
	v.t.Helper()
	got, err := v.e.Get(context.Background(), v.f.Tenant, id)
	if err != nil {
		v.t.Fatal(err)
	}
	return got
}

func (v *mcpEnv) evidence(id uuid.UUID) action.Evidence {
	v.t.Helper()
	ev, err := v.e.Evidence(context.Background(), v.f.Tenant, id)
	if err != nil {
		v.t.Fatal(err)
	}
	return ev
}

func (v *mcpEnv) logged() string {
	v.logMu.Lock()
	defer v.logMu.Unlock()
	return v.logs.String()
}

// persisted lists every table of the test database with a row that contains
// value in any column: text, arrays and JSON through the row's JSON form,
// and bytea as raw bytes.
func (v *mcpEnv) persisted(value string) []string {
	v.t.Helper()
	ctx := context.Background()
	type table struct {
		schema, name string
		bytea        []string
	}
	var tables []table
	rows, err := v.f.Owner.Query(ctx, `SELECT n.nspname, c.relname,
		COALESCE(array_agg(a.attname::text ORDER BY a.attnum) FILTER (WHERE a.atttypid = 'bytea'::regtype), '{}')
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
		WHERE c.relkind IN ('r', 'p') AND n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema'
		GROUP BY n.nspname, c.relname ORDER BY 1, 2`)
	if err != nil {
		v.t.Fatal(err)
	}
	for rows.Next() {
		var tb table
		if err := rows.Scan(&tb.schema, &tb.name, &tb.bytea); err != nil {
			v.t.Fatal(err)
		}
		tables = append(tables, tb)
	}
	if err := rows.Err(); err != nil {
		v.t.Fatal(err)
	}
	if len(tables) < 20 {
		v.t.Fatalf("only %d tables to search", len(tables))
	}
	var found []string
	for _, tb := range tables {
		ident := pgx.Identifier{tb.schema, tb.name}.Sanitize()
		where := `strpos(to_jsonb(x)::text, $1) > 0`
		for _, col := range tb.bytea {
			where += ` OR position(convert_to($1, 'UTF8') IN x.` + pgx.Identifier{col}.Sanitize() + `) > 0`
		}
		var n int
		// The owner in the fixture tenant sees every row the test wrote.
		err := storage.InTenantTx(ctx, v.f.Owner, v.f.Tenant.String(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM `+ident+` x WHERE `+where, value).Scan(&n)
		})
		if err != nil {
			v.t.Fatalf("search %s: %v", ident, err)
		}
		if n > 0 {
			found = append(found, fmt.Sprintf("%s (%d rows)", ident, n))
		}
	}
	return found
}

// TestTheWorkerCallsAnMCPTool (ADR-032 S3.3, Review Focus 3): the scanner
// discovers Create.PO, two people certify it, and the worker calls it once
// through the real connector with the server's name and the enforced
// payload; the success carries the result's digest.
func TestTheWorkerCallsAnMCPTool(t *testing.T) {
	v := newMCPEnv(t, []string{"definition_changed"}, 5000)
	done := v.call(map[string]any{"supplier": "ACME", "lines": []any{map[string]any{"sku": "LAPTOP-14", "qty": 10}}})
	if done.State != "SUCCEEDED" || !mcpReference.MatchString(done.ExternalReference) {
		t.Fatalf("call: %s (%s) %q", done.State, done.StateReason, done.ExternalReference)
	}
	calls := v.srv.Calls()
	if len(calls) != 1 {
		t.Fatalf("%d tools/call reached the server", len(calls))
	}
	if calls[0].Name != "Create.PO" || !bytes.Equal(calls[0].Arguments, done.EnforcedPayload) {
		t.Fatalf("the server received %q with %s; the enforced payload is %s",
			calls[0].Name, calls[0].Arguments, done.EnforcedPayload)
	}
	ev := v.evidence(done.ID)
	if len(ev.Attempts) != 1 || ev.Attempts[0].Outcome != "succeeded" ||
		ev.Attempts[0].ExternalReference != done.ExternalReference {
		t.Fatalf("attempts %+v", ev.Attempts)
	}
	if n, err := v.w.RunOnce(context.Background()); err != nil || n != 0 || len(v.srv.Calls()) != 1 {
		t.Fatalf("a second pass dispatched %d (%v), %d calls", n, err, len(v.srv.Calls()))
	}
}

// TestAChangedDefinitionBetweenScansIsNeverCalled (ADR-032 S3.5): the server
// changes Create.PO after the scan that certified it, and nothing rescans:
// the registry still holds the certified definition, but the worker's own
// listing differs, so no tools/call is sent and the certified no-effect
// fails the action. The next scan quarantines the tool.
func TestAChangedDefinitionBetweenScansIsNeverCalled(t *testing.T) {
	v := newMCPEnv(t, []string{"definition_changed"}, 5000)
	v.srv.SetTools(mcpChanged)

	got := v.call(map[string]any{"supplier": "ACME"})
	ev := v.evidence(got.ID)
	if got.State != "FAILED" || len(ev.Attempts) != 1 || ev.Attempts[0].Outcome != "no_effect" ||
		ev.Attempts[0].ErrorClass != "definition_changed" {
		t.Fatalf("changed definition: %s (%s), attempts %+v", got.State, got.StateReason, ev.Attempts)
	}
	if n := len(v.srv.Calls()); n != 0 {
		t.Fatalf("%d tools/call reached the server", n)
	}
	if !strings.Contains(v.logged(), "worker.mcp_definition_changed") {
		t.Fatal("no security alert was logged")
	}

	if err := v.f.Exec("otto", `UPDATE eacp.mcp_servers SET requested_at = now(), request_reason = 'vendor release'
		WHERE connector_id = $1`, v.conn); err != nil {
		t.Fatal(err)
	}
	runScan(t, v.scanner, 1)
	if denied := v.call(map[string]any{"supplier": "ACME"}); denied.State != "DENIED" || denied.StateReason != "tool_quarantined" {
		t.Fatalf("after the rescan: %s %s", denied.State, denied.StateReason)
	}
	if n := len(v.srv.Calls()); n != 0 {
		t.Fatalf("%d tools/call reached the server", n)
	}
}

// TestAnMCPToolErrorIsUnknownUntilCertified (ADR-032 S3.4): a result with
// isError proves nothing unless the contract certifies mcp_tool_error; then
// it is a no-effect failure.
func TestAnMCPToolErrorIsUnknownUntilCertified(t *testing.T) {
	toolError := func(string, json.RawMessage) mcptest.Reply {
		return mcptest.Reply{Result: map[string]any{"resultType": "complete", "isError": true,
			"content": []any{map[string]any{"type": "text", "text": "supplier blocked"}}}}
	}
	for _, c := range []struct {
		name     string
		noEffect []string
		state    string
		outcome  string
	}{
		{"uncertified", []string{"definition_changed"}, "UNKNOWN_OUTCOME", "ambiguous"},
		{"certified", []string{"definition_changed", "mcp_tool_error"}, "FAILED", "no_effect"},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := newMCPEnv(t, c.noEffect, 5000)
			v.srv.OnCall(toolError)
			got := v.call(map[string]any{"supplier": "ACME"})
			ev := v.evidence(got.ID)
			if got.State != c.state || len(ev.Attempts) != 1 || ev.Attempts[0].Outcome != c.outcome ||
				ev.Attempts[0].ErrorClass != "mcp_tool_error" || ev.Attempts[0].ExternalReference != "" {
				t.Fatalf("tool error: %s (%s), attempts %+v", got.State, got.StateReason, ev.Attempts)
			}
			if n := len(v.srv.Calls()); n != 1 {
				t.Fatalf("%d tools/call reached the server", n)
			}
		})
	}
}

// TestAnMCPActionIsNeverRetried (ADR-032 invariant 1): the connection is
// reset after the server read the tools/call. Nothing proves the tool did
// not run, so the action waits for a human and is never sent again.
func TestAnMCPActionIsNeverRetried(t *testing.T) {
	v := newMCPEnv(t, []string{"definition_changed", "connection_refused_before_send"}, 5000)
	// A second server answers everything but tools/call, which the first one
	// reads in full and then drops without an answer.
	inner := mcptest.New(t, mcptest.Modern, mcpToken, mcpCreate)
	var calls atomic.Int32
	v.srv.SetHandler(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Method != "tools/call" {
			r.Body = io.NopCloser(bytes.NewReader(body))
			inner.Config.Handler.ServeHTTP(w, r)
			return
		}
		calls.Add(1)
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	})

	got := v.call(map[string]any{"supplier": "ACME"})
	ev := v.evidence(got.ID)
	if got.State != "UNKNOWN_OUTCOME" || len(ev.Attempts) != 1 || ev.Attempts[0].Outcome != "ambiguous" ||
		ev.Attempts[0].ErrorClass != "transport_error" {
		t.Fatalf("reset call: %s (%s), attempts %+v", got.State, got.StateReason, ev.Attempts)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("%d tools/call reached the server", n)
	}
	if n, err := v.w.RunOnce(context.Background()); err != nil || n != 0 {
		t.Fatalf("the worker ran again: dispatched %d, %v", n, err)
	}
	if n := calls.Load(); n != 1 || v.get(got.ID).State != "UNKNOWN_OUTCOME" || len(v.evidence(got.ID).Attempts) != 1 {
		t.Fatalf("after another pass: %d tools/call, state %s", n, v.get(got.ID).State)
	}
}

// TestAKillDuringAnMCPCallIsUnknown (ADR-016, ADR-032 S3.3): the server
// holds the tools/call and the tenant is killed; the worker cuts the call,
// and a call that was sent and cut has an unknown outcome.
func TestAKillDuringAnMCPCallIsUnknown(t *testing.T) {
	v := newMCPEnv(t, []string{"definition_changed"}, 30000)
	v.srv.OnCall(func(string, json.RawMessage) mcptest.Reply { return mcptest.Reply{Delay: time.Minute} })
	w := v.worker("w-kill", map[string]worker.Connector{"mcp": v.client}, 100*time.Millisecond)
	a := v.submit(map[string]any{"supplier": "ACME"})
	if a.State != "QUEUED" {
		t.Fatalf("submit: %s %s", a.State, a.StateReason)
	}
	done := make(chan struct{})
	go func() { defer close(done); _, _ = w.RunOnce(context.Background()) }()
	deadline := time.Now().Add(10 * time.Second)
	for len(v.srv.Calls()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the tools/call was not sent")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := v.f.Exec("otto", `SELECT eacp.set_kill('tenant', $1, true, 'incident containment')`, v.f.Tenant); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the kill did not end the call")
	}
	got := v.get(a.ID)
	ev := v.evidence(a.ID)
	if got.State != "UNKNOWN_OUTCOME" || len(ev.Attempts) != 1 || ev.Attempts[0].Outcome != "ambiguous" ||
		ev.Attempts[0].ErrorClass != "kill_interrupted" || ev.Attempts[0].ExternalReference != "" {
		t.Fatalf("killed call: %s (%s), attempts %+v", got.State, got.StateReason, ev.Attempts)
	}
	if n := len(v.srv.Calls()); n != 1 {
		t.Fatalf("%d tools/call reached the server", n)
	}
}

// TestNothingOfTheOutputIsPersisted (ADR-032 S3.6, invariant 3): a canary in
// a result's content and structuredContent, in a tool error's text and in a
// JSON-RPC error's message reaches no log line and no column of any table;
// neither does the server's token.
func TestNothingOfTheOutputIsPersisted(t *testing.T) {
	v := newMCPEnv(t, []string{"definition_changed", "mcp_tool_error"}, 5000)
	replies := []mcptest.Reply{
		{Result: map[string]any{"resultType": "complete",
			"content":           []any{map[string]any{"type": "text", "text": "PO " + mcpOutput + "-content created"}},
			"structuredContent": map[string]any{"po": mcpOutput + "-structured", "note": mcpOutput}}},
		{Result: map[string]any{"resultType": "complete", "isError": true,
			"content": []any{map[string]any{"type": "text", "text": "rejected: " + mcpOutput + "-error"}}}},
		{RPCCode: -32602, RPCMessage: "invalid params " + mcpOutput + "-invalid"},
		{RPCCode: -32603, RPCMessage: "internal error " + mcpOutput + "-rpc"},
	}
	var next atomic.Int32
	v.srv.OnCall(func(string, json.RawMessage) mcptest.Reply { return replies[next.Add(1)-1] })
	// The payload is the agent's own input and is stored: it shows that the
	// search finds what is there.
	const marker = "payload-marker-51c7"
	states := []string{"SUCCEEDED", "FAILED", "UNKNOWN_OUTCOME", "UNKNOWN_OUTCOME"}
	for i, want := range states {
		got := v.call(map[string]any{"supplier": marker})
		if got.State != want {
			t.Fatalf("call %d: %s (%s), want %s", i, got.State, got.StateReason, want)
		}
	}
	if n := len(v.srv.Calls()); n != len(states) {
		t.Fatalf("%d tools/call reached the server", n)
	}

	if found := v.persisted(marker); len(found) == 0 {
		t.Fatal("the search did not find the stored payload")
	}
	for _, value := range []string{mcpOutput, mcpToken} {
		if found := v.persisted(value); len(found) != 0 {
			t.Fatalf("%q was persisted in %v", value, found)
		}
	}
	logs := v.logged()
	if !strings.Contains(logs, "mcp tool call") || !strings.Contains(logs, "attempt completed") {
		t.Fatal("the client or the worker logged no call")
	}
	for _, value := range []string{mcpOutput, mcpToken, marker} {
		if strings.Contains(logs, value) {
			t.Fatalf("%q reached the log", value)
		}
	}
}

// TestAWorkerWithoutMCPLeavesTheActionQueued (ADR-032 S3.1): a worker that
// does not serve mcp never claims an MCP action.
func TestAWorkerWithoutMCPLeavesTheActionQueued(t *testing.T) {
	v := newMCPEnv(t, []string{"definition_changed"}, 5000)
	w := v.worker("w-other", map[string]worker.Connector{"http": connector.NewHTTP(), "a2a": a2a.New()}, 0)
	a := v.submit(map[string]any{"supplier": "ACME"})
	if a.State != "QUEUED" {
		t.Fatalf("submit: %s %s", a.State, a.StateReason)
	}
	if n, err := w.RunOnce(context.Background()); err != nil || n != 0 {
		t.Fatalf("claimed %d, %v", n, err)
	}
	if got := v.get(a.ID); got.State != "QUEUED" || got.AttemptCount != 0 || len(v.srv.Calls()) != 0 {
		t.Fatalf("after a worker without mcp: %s, %d attempts, %d calls", got.State, got.AttemptCount, len(v.srv.Calls()))
	}
}
