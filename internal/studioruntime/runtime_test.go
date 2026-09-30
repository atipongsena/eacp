package studioruntime_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/api"
	"github.com/atipongsena/eacp/internal/identity"
	"github.com/atipongsena/eacp/internal/registry/registrytest"
	"github.com/atipongsena/eacp/internal/storage"
	"github.com/atipongsena/eacp/internal/studioruntime"
	"github.com/atipongsena/eacp/internal/worker"
)

// masterCanary is the test's master secret: it must never leave the
// runtime (ADR-033 invariant 4).
const masterCanary = "MASTER-CANARY-3f9a0c71-5e2d-4b8a-9c6f-1d7e2a4b8c90"

// definition is the leave-balance agent of the 27a spec.
const definition = `{"schema_version": 1, "kind": "agent",
 "inputs": {"employee_id": {"type": "string", "max_length": 64}},
 "steps": [
   {"id": "lookup", "kind": "tool_call", "tool": "hr-mcp.get_leave_balance", "tool_schema_version": "1",
    "operation": "lookup", "target": "hr", "resource": "leave_balance",
    "payload": {"employee_id": "{{inputs.employee_id}}"}},
   {"id": "answer", "kind": "respond",
    "text": "You have {{steps.lookup.output.structuredContent.days}} days of leave left."}],
 "limits": {"timeout_seconds": 300}}`

// escalatePolicy makes every action wait for two approvers.
const escalatePolicy = `{"format_version":1,"rules":[{"id":"ask","verdict":"escalate","reason":"a human decides",
 "approval":{"quorum":2,"eligible_roles":["approver"],"ttl_seconds":600}}]}`

// keepContractSQL is a read-only contract that keeps a success's output
// for five minutes (ADR-034).
const keepContractSQL = `INSERT INTO eacp.tool_contracts
	(tenant_id, tool_id, side_effects, idempotency_mode, reconciliation_lookup,
	 reconciliation_consistency, proof_standard, max_attempts, result_retention_seconds)
	VALUES (eacp.current_tenant_id(), $1, '{READ_ONLY}', 'none', 'none', 'none', 'none', 1, 300)
	RETURNING id`

// connector is the in-test hr-mcp: it answers every call with output.
type connector struct {
	mu     sync.Mutex
	calls  []worker.Call
	output string
}

func (c *connector) Execute(_ context.Context, call worker.Call) worker.Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, call)
	return worker.Result{Outcome: worker.Succeeded, ExternalReference: "R-" + call.ActionID.String()[:8],
		Output: json.RawMessage(c.output)}
}

func (c *connector) Lookup(context.Context, worker.LookupCall) worker.LookupResult {
	return worker.LookupResult{}
}

func (c *connector) payloads() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, call := range c.calls {
		out = append(out, string(call.Payload))
	}
	return out
}

// lockedBuffer is a log or response sink shared by goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// teeWriter copies every response body into the rig's responses.
type teeWriter struct {
	http.ResponseWriter
	sink io.Writer
}

func (w teeWriter) Write(p []byte) (int, error) {
	_, _ = w.sink.Write(p)
	return w.ResponseWriter.Write(p)
}

// rig is a real API, a real worker with the in-test connector, stella's
// approved leave-bot and the runtime.
type rig struct {
	t         *testing.T
	f         *registrytest.Fixture
	srv       *httptest.Server
	keys      map[string]string
	conn      *connector
	master    *studioruntime.Master
	rt        *studioruntime.Runtime
	logs      *lockedBuffer // the API, the worker and the runtime
	responses *lockedBuffer
	redacted  *redactions
	agent     string
	version   string
}

type redactions struct {
	mu     sync.Mutex
	values []string
}

func (r *redactions) add(v ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values = append(r.values, v...)
}

func (r *redactions) has(v string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Contains(r.values, v)
}

func newRig(t *testing.T) *rig {
	t.Helper()
	f := registrytest.New(t)
	f.ActivatePolicy(t, registrytest.AllowPolicy)
	r := &rig{t: t, f: f, keys: map[string]string{}, logs: &lockedBuffer{}, responses: &lockedBuffer{},
		redacted: &redactions{}, conn: &connector{output: `{"structuredContent": {"days": 12}}`}}
	log := slog.New(slog.NewJSONHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	mux := http.NewServeMux()
	api.New(f.App, log).Register(mux)
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mux.ServeHTTP(teeWriter{w, r.responses}, req)
	}))
	t.Cleanup(r.srv.Close)

	f.AddPrincipal(t, "stella", "human", "studio_author")
	f.AddPrincipal(t, "rt", "service", "studio_runtime")
	hr := f.ID(t, "alice", `INSERT INTO eacp.groups (tenant_id, name, display_name)
		VALUES (eacp.current_tenant_id(), 'hr', 'HR') RETURNING id`)
	f.ID(t, "alice", `INSERT INTO eacp.group_memberships (tenant_id, group_id, principal_id)
		VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, hr, f.P["stella"])
	for _, who := range []string{"stella", "rita", "rt", "otto"} {
		r.keys[who] = r.issue(f.P[who])
	}
	f.ActiveToolWith(t, "hr-mcp", "get_leave_balance", keepContractSQL)

	code, v := r.as("stella", "POST", "/v1/studio/agents", map[string]any{"name": "leave-bot",
		"display_name": "Leave balance", "description": "Days of leave left.", "department_id": hr,
		"definition": json.RawMessage(definition)})
	r.want(201, code, v)
	r.agent, r.version = v["agent_id"].(string), v["id"].(string)
	code, v = r.as("rita", "POST", "/v1/studio/versions/"+r.version+"/approve", map[string]any{"reason": "read-only"})
	r.want(200, code, v)

	r.startWorker(log)
	masterFile := filepath.Join(t.TempDir(), "master")
	if err := os.WriteFile(masterFile, []byte(masterCanary+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var err error
	if r.master, err = studioruntime.LoadMaster(masterFile, "v1"); err != nil {
		t.Fatal(err)
	}
	r.rt, err = studioruntime.New(studioruntime.Options{
		API: r.srv.URL, Keys: map[uuid.UUID]string{f.Tenant: r.keys["rt"]}, Master: r.master, ID: "r1",
		Lease: 15 * time.Second, Concurrency: 4, Poll: 50 * time.Millisecond, Log: log, Redact: r.redacted.add,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// issue gives principal a key: alice proposes its hash, bob approves.
func (r *rig) issue(principal uuid.UUID) string {
	r.t.Helper()
	cred := uuid.New()
	key, hash, err := identity.NewKey(identity.KindPrincipal, r.f.Tenant, cred)
	if err != nil {
		r.t.Fatal(err)
	}
	r.f.ID(r.t, "alice", `INSERT INTO eacp.credentials (tenant_id, id, kind, principal_id, secret_hash, expires_at)
		VALUES (eacp.current_tenant_id(), $1, 'pk', $2, $3, now() + interval '30 days') RETURNING id`, cred, principal, hash)
	if err := r.f.Exec("bob", `UPDATE eacp.credentials SET approved_at = now() WHERE id = $1`, cred); err != nil {
		r.t.Fatal(err)
	}
	return key
}

// startWorker runs a real execution worker until the test ends.
func (r *rig) startWorker(log *slog.Logger) {
	r.t.Helper()
	secrets := filepath.Join(r.t.TempDir(), "secrets.json")
	body := fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"hr-mcp","host":"fakeerp:8090","value":"hr-token"}]}`, r.f.Tenant)
	if err := os.WriteFile(secrets, []byte(body), 0o600); err != nil {
		r.t.Fatal(err)
	}
	store, err := worker.LoadSecrets(secrets)
	if err != nil {
		r.t.Fatal(err)
	}
	w, err := worker.New(r.f.App, worker.Options{ID: "w1", Lease: 5 * time.Second,
		Connectors: map[string]worker.Connector{"http": r.conn}, Secrets: store,
		Backoff: func(int) time.Duration { return 100 * time.Millisecond }, Log: log})
	if err != nil {
		r.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			_, _ = w.RunOnce(ctx)
			select {
			case <-ctx.Done():
			case <-time.After(50 * time.Millisecond):
			}
		}
	}()
	r.t.Cleanup(func() { cancel(); <-done })
}

// as calls the API as who and decodes the JSON answer.
func (r *rig) as(who, method, path string, body any) (int, map[string]any) {
	r.t.Helper()
	return r.send(r.keys[who], method, path, nil, body)
}

func (r *rig) send(key, method, path string, header map[string]string, body any) (int, map[string]any) {
	r.t.Helper()
	var in io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		in = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, r.srv.URL+path, in)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (r *rig) want(code, got int, body map[string]any) {
	r.t.Helper()
	if got != code {
		r.t.Fatalf("status %d, want %d: %v", got, code, body)
	}
}

// approveKeys has the runtime propose every due key and rita approve them.
func (r *rig) approveKeys() {
	r.t.Helper()
	if _, err := r.rt.RotateOnce(context.Background()); err != nil {
		r.t.Fatal(err)
	}
	code, queue := r.as("rita", "GET", "/v1/studio/requests", nil)
	r.want(200, code, queue)
	keys, _ := queue["keys"].([]any)
	if len(keys) == 0 {
		r.t.Fatal("no key to approve")
	}
	for _, k := range keys {
		code, body := r.as("rita", "POST", "/v1/credentials/"+k.(map[string]any)["id"].(string)+"/approve", map[string]any{})
		r.want(204, code, body)
	}
}

// start starts a run as stella and returns its id.
func (r *rig) start(employee string) string {
	r.t.Helper()
	code, run := r.as("stella", "POST", "/v1/studio/agents/"+r.agent+"/runs",
		map[string]any{"inputs": map[string]string{"employee_id": employee}})
	r.want(201, code, run)
	return run["id"].(string)
}

// runOnce drives the claimed runs to their end.
func (r *rig) runOnce() int {
	r.t.Helper()
	n, err := r.rt.RunOnce(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	return n
}

// run reads run id as stella.
func (r *rig) run(id string) map[string]any {
	r.t.Helper()
	code, run := r.as("stella", "GET", "/v1/studio/runs/"+id, nil)
	r.want(200, code, run)
	return run
}

func (r *rig) failed(id, reason string) {
	r.t.Helper()
	if run := r.run(id); run["state"] != "FAILED" || run["failure_reason"] != reason {
		r.t.Fatalf("run = %v, want FAILED %s", run, reason)
	}
}

// expire sets every agent key's expiry to now() + interval, past the
// credentials guard (test set-up only).
func (r *rig) expire(interval string) {
	r.t.Helper()
	r.owner(func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `ALTER TABLE eacp.credentials DISABLE TRIGGER credentials_guard;
			UPDATE eacp.credentials SET expires_at = now() + interval '`+interval+`' WHERE kind = 'ak';
			ALTER TABLE eacp.credentials ENABLE TRIGGER credentials_guard`)
		return err
	})
}

// owner runs fn as the tenant owner (test set-up only).
func (r *rig) owner(fn func(pgx.Tx) error) {
	r.t.Helper()
	if err := storage.InTenantTx(context.Background(), r.f.Owner, r.f.Tenant.String(), fn); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) count(sql string, args ...any) int {
	r.t.Helper()
	var n int
	r.owner(func(tx pgx.Tx) error { return tx.QueryRow(context.Background(), sql, args...).Scan(&n) })
	return n
}

func TestARunEndToEnd(t *testing.T) {
	r := newRig(t)
	r.approveKeys()
	id := r.start("E-7")
	if n := r.runOnce(); n != 1 {
		t.Fatalf("ran %d", n)
	}
	run := r.run(id)
	steps, _ := run["steps"].([]any)
	if run["state"] != "SUCCEEDED" || run["answer"] != "You have 12 days of leave left." || len(steps) != 1 ||
		steps[0].(map[string]any)["action_state"] != "SUCCEEDED" {
		t.Fatalf("run = %v", run)
	}
	if got := r.conn.payloads(); len(got) != 1 || got[0] != `{"employee_id":"E-7"}` {
		t.Fatalf("connector payloads = %v", got)
	}
	// The action is the version's, for stella, under the step's key.
	if n := r.count(`SELECT count(*) FROM eacp.actions a JOIN eacp.principals p ON p.id = a.subject_principal_id
		WHERE a.agent_version_id = $1 AND p.name = 'stella' AND a.idempotency_key = $2 AND a.state = 'SUCCEEDED'`,
		r.version, "studio:"+id+":0"); n != 1 {
		t.Fatalf("actions = %d", n)
	}
	// Nothing is left to claim.
	if n := r.runOnce(); n != 0 {
		t.Fatalf("ran %d more", n)
	}
}

func TestACrashBeforeTheStepRecordResubmitsTheSameAction(t *testing.T) {
	r := newRig(t)
	r.approveKeys()
	id := r.start("E-7")

	// A runtime that crashed: it claimed, submitted the step's action and died before recording it.
	code, body := r.as("rt", "POST", "/v1/studio/runtime/claims",
		map[string]any{"runtime_id": "crashed", "master_version": "v1", "lease_seconds": 5, "limit": 1})
	r.want(200, code, body)
	claim := body["runs"].([]any)[0].(map[string]any)
	key, _ := r.master.Key(r.f.Tenant, uuid.MustParse(claim["credential_id"].(string)))
	code, first := r.send(key, "POST", "/v1/actions", map[string]string{"Idempotency-Key": "studio:" + id + ":0"},
		map[string]any{"subject": claim["subject"], "operation": "lookup", "target": "hr", "resource": "leave_balance",
			"tool": "hr-mcp.get_leave_balance", "tool_schema_version": "1", "payload": map[string]string{"employee_id": "E-7"}})
	if code != 200 && code != 202 {
		t.Fatalf("submit = %d %v", code, first)
	}
	r.owner(func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `UPDATE eacp.studio_runs SET leased_until = now() - interval '1 second'`)
		return err
	})

	if n := r.runOnce(); n != 1 {
		t.Fatalf("ran %d", n)
	}
	run := r.run(id)
	steps, _ := run["steps"].([]any)
	if run["state"] != "SUCCEEDED" || len(steps) != 1 || steps[0].(map[string]any)["action_id"] != first["id"] {
		t.Fatalf("run = %v, first action %v", run, first["id"])
	}
	if n := r.count(`SELECT count(*) FROM eacp.actions WHERE idempotency_key = $1`, "studio:"+id+":0"); n != 1 {
		t.Fatalf("actions = %d", n)
	}
	if got := r.conn.payloads(); len(got) != 1 {
		t.Fatalf("connector calls = %v", got)
	}
}

func TestARunFailsClosedWithoutAKey(t *testing.T) {
	r := newRig(t)
	pending := r.start("E-1")
	if n := r.runOnce(); n != 1 {
		t.Fatalf("ran %d", n)
	}
	r.failed(pending, "credential_pending")

	r.approveKeys()
	r.expire("-1 second")
	expired := r.start("E-2")
	r.runOnce()
	r.failed(expired, "credential_expired")
	if n := r.count(`SELECT count(*) FROM eacp.actions`); n != 0 {
		t.Fatalf("actions = %d", n)
	}
}

func TestARevokedKeyFailsTheRun(t *testing.T) {
	r := newRig(t)
	r.approveKeys()
	r.f.ActivatePolicy(t, escalatePolicy)
	id := r.start("E-1")
	done := make(chan int)
	go func() {
		n, _ := r.rt.RunOnce(context.Background())
		done <- n
	}()
	// The step waits for a human; meanwhile an operator revokes every Studio key.
	for deadline := time.Now().Add(20 * time.Second); r.count(`SELECT count(*) FROM eacp.studio_run_steps`) == 0; {
		if time.Now().After(deadline) {
			t.Fatalf("the step was never recorded: %v %s", r.run(id), r.logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	code, body := r.as("otto", "POST", "/v1/studio/credentials/revoke-all", map[string]any{"reason": "suspected compromise"})
	r.want(200, code, body)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the run did not stop")
	}
	r.failed(id, "credential_revoked")
}

func TestADeniedStepFailsTheRun(t *testing.T) {
	r := newRig(t)
	r.approveKeys()
	r.f.ActivatePolicy(t, `{"format_version":1,"rules":[{"id":"no","verdict":"deny","reason":"not today"}]}`)
	id := r.start("E-1")
	r.runOnce()
	r.failed(id, "action_denied")
	if got := r.conn.payloads(); len(got) != 0 {
		t.Fatalf("connector calls = %v", got)
	}
}

func TestAStepAwaitingApprovalFailsAtTheDeadline(t *testing.T) {
	r := newRig(t)
	r.approveKeys()
	r.f.ActivatePolicy(t, escalatePolicy)
	id := r.start("E-1")
	r.owner(func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `UPDATE eacp.studio_runs SET deadline = now() + interval '2 seconds'`)
		return err
	})
	r.runOnce()
	r.failed(id, "deadline_exceeded")
	if n := r.count(`SELECT count(*) FROM eacp.studio_run_steps s JOIN eacp.actions a ON a.id = s.action_id
		WHERE a.state = 'PENDING_APPROVAL'`); n != 1 {
		t.Fatalf("steps waiting for approval = %d", n)
	}
	if got := r.conn.payloads(); len(got) != 0 {
		t.Fatalf("connector calls = %v", got)
	}
}

func TestAReplacedVersionStopsTheRun(t *testing.T) {
	r := newRig(t)
	r.approveKeys()
	id := r.start("E-1")
	code, v2 := r.as("stella", "POST", "/v1/studio/agents/"+r.agent+"/versions",
		map[string]any{"definition": json.RawMessage(definition)})
	r.want(201, code, v2)
	code, body := r.as("rita", "POST", "/v1/studio/versions/"+v2["id"].(string)+"/approve", map[string]any{"reason": "v2"})
	r.want(200, code, body)
	r.runOnce()
	r.failed(id, "version_replaced")
	if n := r.count(`SELECT count(*) FROM eacp.actions`); n != 0 {
		t.Fatalf("actions = %d", n)
	}
}

func TestRotationProposesASuccessorAndNeverApproves(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if n, err := r.rt.RotateOnce(ctx); err != nil || n != 1 {
		t.Fatalf("proposed %d, %v", n, err)
	}
	// A pending proposal is not proposed again, and the runtime approves nothing.
	if n, err := r.rt.RotateOnce(ctx); err != nil || n != 0 {
		t.Fatalf("proposed %d again, %v", n, err)
	}
	if n := r.count(`SELECT count(*) FROM eacp.credentials WHERE kind = 'ak' AND approved_at IS NOT NULL`); n != 0 {
		t.Fatalf("approved = %d", n)
	}
	r.approveKeys()
	if n, err := r.rt.RotateOnce(ctx); err != nil || n != 0 {
		t.Fatalf("proposed %d with a fresh key, %v", n, err)
	}
	// Day 60: fewer than 30 days left, so a successor is proposed; the old key still works.
	r.expire("29 days")
	if n, err := r.rt.RotateOnce(ctx); err != nil || n != 1 {
		t.Fatalf("proposed %d successors, %v", n, err)
	}
	if got := r.count(`SELECT count(*) FROM eacp.credentials c JOIN eacp.studio_credentials s ON s.id = c.id
		WHERE c.approved_at IS NULL AND s.master_version = 'v1' AND c.expires_at > now() + interval '89 days'`); got != 1 {
		t.Fatalf("successors = %d", got)
	}
	if n := r.count(`SELECT count(*) FROM eacp.credentials WHERE approved_by = $1`, r.f.P["rt"]); n != 0 {
		t.Fatalf("the runtime approved %d keys", n)
	}
	id := r.start("E-1")
	r.runOnce()
	if run := r.run(id); run["state"] != "SUCCEEDED" {
		t.Fatalf("run = %v", run)
	}
}

func TestNoKeyOrMasterLeaks(t *testing.T) {
	r := newRig(t)
	r.approveKeys()
	id := r.start("E-1")
	r.runOnce()
	if run := r.run(id); run["state"] != "SUCCEEDED" {
		t.Fatalf("run = %v", run)
	}
	// The next key, proposed but unused, must not leak either.
	r.expire("1 day")
	if _, err := r.rt.RotateOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	canaries := []string{masterCanary, base64.RawURLEncoding.EncodeToString([]byte(masterCanary)),
		hex.EncodeToString([]byte(masterCanary))}
	var creds []uuid.UUID
	r.owner(func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), `SELECT id FROM eacp.studio_credentials`)
		if err != nil {
			return err
		}
		creds, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		return err
	})
	if len(creds) != 2 {
		t.Fatalf("studio keys = %d", len(creds))
	}
	used, _ := r.master.Key(r.f.Tenant, creds[0])
	if !r.redacted.has(masterCanary) {
		t.Fatal("the master was not added to the redaction set")
	}
	for _, c := range creds {
		key, _ := r.master.Key(r.f.Tenant, c)
		secret := key[strings.LastIndex(key, "_")+1:]
		canaries = append(canaries, key, secret)
		if key == used && !r.redacted.has(key) {
			t.Fatal("a used key was not added to the redaction set")
		}
	}
	for _, c := range canaries {
		if strings.Contains(r.logs.String(), c) {
			t.Fatalf("log carries %q", c)
		}
		if strings.Contains(r.responses.String(), c) {
			t.Fatalf("a response carries %q", c)
		}
		for _, sql := range []string{
			`SELECT count(*) FROM eacp.audit_events WHERE convert_from(payload, 'UTF8') LIKE '%' || $1 || '%'`,
			`SELECT count(*) FROM eacp.outbox_events WHERE to_jsonb(outbox_events)::text LIKE '%' || $1 || '%'`,
			`SELECT count(*) FROM eacp.studio_runs WHERE to_jsonb(studio_runs)::text LIKE '%' || $1 || '%'`,
			`SELECT count(*) FROM eacp.studio_run_steps WHERE to_jsonb(studio_run_steps)::text LIKE '%' || $1 || '%'`,
			`SELECT count(*) FROM eacp.credentials WHERE to_jsonb(credentials)::text LIKE '%' || $1 || '%'`,
			`SELECT count(*) FROM eacp.actions WHERE to_jsonb(actions)::text LIKE '%' || $1 || '%'`,
		} {
			if n := r.count(sql, c); n != 0 {
				t.Fatalf("%q stored: %s", c, sql)
			}
		}
	}
}

func TestTheRuntimeRefusesAWeakMaster(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	short := write("short", "  "+strings.Repeat("x", 31)+"\n")
	good := write("good", strings.Repeat("x", 32))
	for name, load := range map[string]func() error{
		"short":       func() error { _, err := studioruntime.LoadMaster(short, "v1"); return err },
		"missing":     func() error { _, err := studioruntime.LoadMaster(filepath.Join(dir, "none"), "v1"); return err },
		"bad version": func() error { _, err := studioruntime.LoadMaster(good, "1"); return err },
	} {
		if load() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	m, err := studioruntime.LoadMaster(good, "v2")
	if err != nil || m.Version() != "v2" {
		t.Fatalf("good master: %v", err)
	}
	// Keys differ by master version, tenant and credential.
	tenant, cred := uuid.New(), uuid.New()
	k1, h1 := m.Key(tenant, cred)
	m1, _ := studioruntime.LoadMaster(good, "v1")
	k2, _ := m1.Key(tenant, cred)
	k3, _ := m.Key(uuid.New(), cred)
	k4, _ := m.Key(tenant, uuid.New())
	if k1 == k2 || k1 == k3 || k1 == k4 || len(h1) != 32 {
		t.Fatal("derived keys collide")
	}
	if again, _ := m.Key(tenant, cred); again != k1 {
		t.Fatal("derivation is not deterministic")
	}

	pk, _, _ := identity.NewKey(identity.KindPrincipal, tenant, uuid.New())
	pk2, _, _ := identity.NewKey(identity.KindPrincipal, tenant, uuid.New())
	ak, _, _ := identity.NewKey(identity.KindAgent, tenant, uuid.New())
	for name, body := range map[string]string{
		"agent key": ak + "\n", "two keys for a tenant": pk + "\n" + pk2 + "\n", "garbage": "not-a-key\n", "empty": "# none\n",
	} {
		if _, err := studioruntime.LoadKeys(write(strings.ReplaceAll(name, " ", "-"), body)); err == nil {
			t.Errorf("key file %s: accepted", name)
		} else if strings.Contains(err.Error(), pk) || strings.Contains(err.Error(), ak) {
			t.Errorf("key file %s: the error carries the key", name)
		}
	}
	keys, err := studioruntime.LoadKeys(write("keys", "# the runtime\n\n"+pk+"\n"))
	if err != nil || keys[tenant] != pk {
		t.Fatalf("keys = %v, %v", len(keys), err)
	}
	for name, o := range map[string]studioruntime.Options{
		"no master": {API: "http://api:8080", Keys: keys, ID: "r1"},
		"no keys":   {API: "http://api:8080", Master: m, ID: "r1"},
		"no api":    {Keys: keys, Master: m, ID: "r1"},
		"no id":     {API: "http://api:8080", Keys: keys, Master: m},
	} {
		if _, err := studioruntime.New(o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
