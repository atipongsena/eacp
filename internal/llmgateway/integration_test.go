package llmgateway_test

// The gateway over PostgreSQL, the local PDP and fakellm (ADR-031):
// metering, hard budgets, kills, the sweeper and secrecy.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/fakellm"
	"github.com/atipongsena/eacp/internal/governance"
	"github.com/atipongsena/eacp/internal/identity"
	"github.com/atipongsena/eacp/internal/llm"
	"github.com/atipongsena/eacp/internal/llmgateway"
	"github.com/atipongsena/eacp/internal/registry/registrytest"
	"github.com/atipongsena/eacp/internal/storage"
	"github.com/atipongsena/eacp/internal/worker"
)

const (
	fakeKey    = "fakellm-provider-key-canary-3c"
	llmPolicy  = `{"format_version":1,"rules":[{"id":"llm","match":{"operation":"llm.generate"},"verdict":"allow","reason":"llm_allowed"}]}`
	modelSQL   = `INSERT INTO eacp.llm_models (tenant_id, name, provider, base_url, upstream_model, secret_ref, max_output_tokens, timeout_ms) VALUES (eacp.current_tenant_id(), $1, $2, $3, $4, 'llm', 100000, 10000) RETURNING id`
	priceSQLIT = `INSERT INTO eacp.model_prices (tenant_id, provider, model, unit, input_per_mtok, output_per_mtok, reason) VALUES (eacp.current_tenant_id(), $1, $2, 'USD', $3, $4, 'list price') RETURNING id`
)

type llmEnv struct {
	t        *testing.T
	f        *registrytest.Fixture
	gw       *httptest.Server
	fake     *httptest.Server
	store    *llm.Store
	agent    registrytest.Agent
	sonnet   uuid.UUID
	account  uuid.UUID
	agentKey string
	logs     *syncBuffer
}

// ledgerHook wraps the store to run a function right after each admission.
type ledgerHook struct {
	*llm.Store
	after func()
}

func (l ledgerHook) Admit(ctx context.Context, tenant uuid.UUID, r llm.AdmitRequest) (llm.Admission, error) {
	a, err := l.Store.Admit(ctx, tenant, r)
	if err == nil && l.after != nil {
		l.after()
	}
	return a, err
}

func newLLMEnv(t *testing.T, afterAdmit func(*llmEnv), provider ...http.Handler) *llmEnv {
	t.Helper()
	f := registrytest.New(t)
	e := &llmEnv{t: t, f: f, store: llm.New(f.App), logs: &syncBuffer{}}
	fake, err := fakellm.New(fakeKey, filepath.Join(t.TempDir(), "fakellm.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(provider) > 0 {
		fake = provider[0]
	}
	e.fake = httptest.NewServer(fake)
	t.Cleanup(e.fake.Close)

	f.ActivatePolicy(t, llmPolicy)
	e.sonnet = f.ID(t, "erin", modelSQL, "sonnet", "anthropic", e.fake.URL, "claude-fake")
	f.ID(t, "erin", modelSQL, "gpt", "openai", e.fake.URL, "gpt-fake")
	f.ID(t, "alice", priceSQLIT, "anthropic", "claude-fake", "3", "15")
	f.ID(t, "alice", priceSQLIT, "openai", "gpt-fake", "2", "8")
	group := f.ID(t, "alice", `INSERT INTO eacp.groups (tenant_id, name, display_name)
		VALUES (eacp.current_tenant_id(), 'ml', 'ml') RETURNING id`)
	e.agent.Agent = f.ID(t, "erin", `INSERT INTO eacp.agents (tenant_id, name, display_name, environment, risk_class,
		owner_group_id) VALUES (eacp.current_tenant_id(), 'writer', 'writer', 'production', 'high', $1) RETURNING id`, group)
	e.agent.Version = f.ID(t, "erin", `INSERT INTO eacp.agent_versions (tenant_id, agent_id, runtime, code_ref)
		VALUES (eacp.current_tenant_id(), $1, 'python', 'git:abc') RETURNING id`, e.agent.Agent)
	al := f.ID(t, "erin", `INSERT INTO eacp.agent_allowlists (tenant_id, agent_version_id, tool_ids, model_ids)
		VALUES (eacp.current_tenant_id(), $1, '{}', $2) RETURNING id`, e.agent.Version, []uuid.UUID{e.sonnet})
	must(t, f.Exec("rita", `UPDATE eacp.agent_versions SET active_allowlist_id = $1 WHERE id = $2`, al, e.agent.Version))
	must(t, f.Exec("ravi", `UPDATE eacp.agent_versions SET state = 'ACTIVE', state_reason = 'go live' WHERE id = $1`,
		e.agent.Version))
	e.account = f.FundAgent(t, e.agent.Agent, "USD", "0.05")

	credID := uuid.New()
	key, hash, err := identity.NewKey(identity.KindAgent, f.Tenant, credID)
	if err != nil {
		t.Fatal(err)
	}
	e.agentKey = key
	f.ID(t, "erin", `INSERT INTO eacp.credentials (tenant_id, id, kind, agent_version_id, secret_hash, expires_at)
		VALUES (eacp.current_tenant_id(), $1, 'ak', $2, $3, now() + interval '1 day') RETURNING id`,
		credID, e.agent.Version, hash)
	must(t, f.Exec("rita", `UPDATE eacp.credentials SET approved_at = now() WHERE id = $1`, credID))

	u, _ := url.Parse(e.fake.URL)
	path := filepath.Join(t.TempDir(), "llm_secrets.json")
	doc := fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"llm","host":%q,"value":%q}]}`, f.Tenant, u.Host, fakeKey)
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	secrets, err := worker.LoadSecrets(path, worker.RefuseSigningCredentials())
	if err != nil {
		t.Fatal(err)
	}
	var ledger llmgateway.Ledger = e.store
	if afterAdmit != nil {
		ledger = ledgerHook{Store: e.store, after: func() { afterAdmit(e) }}
	}
	gw, err := llmgateway.New(llmgateway.Options{ID: "gw-it",
		Auth: func(ctx context.Context, key string) (identity.Caller, error) {
			return identity.Authenticate(ctx, f.App, key)
		},
		Ledger: ledger, Policies: governance.NewStore(f.App), PDP: governance.LocalProvider{InstanceID: "llm-gateway"},
		Secrets: secrets, AgentRisk: e.store.AgentRisk, KillPoll: 100 * time.Millisecond,
		Log: slog.New(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	if err != nil {
		t.Fatal(err)
	}
	e.gw = httptest.NewServer(gw)
	t.Cleanup(e.gw.Close)
	return e
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (e *llmEnv) messages(text string, maxTokens int, stream bool) (int, []byte) {
	e.t.Helper()
	body := fmt.Sprintf(`{"model":"sonnet","max_tokens":%d,"stream":%t,"messages":[{"role":"user","content":%q}]}`,
		maxTokens, stream, text)
	req, _ := http.NewRequest("POST", e.gw.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", e.agentKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (e *llmEnv) calls() []llm.Call {
	e.t.Helper()
	calls, err := e.store.List(context.Background(), e.f.Tenant, llm.Filter{})
	must(e.t, err)
	return calls
}

// latest waits for the newest call to leave ADMITTED.
func (e *llmEnv) latest() llm.Call {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c := e.calls()[0]
		if c.State != "ADMITTED" || time.Now().After(deadline) {
			return c
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e *llmEnv) budget() (reserved, committed string) {
	e.t.Helper()
	must(e.t, e.f.Exec("alice", `SELECT eacp.budget_fold($1)`, e.account))
	must(e.t, storage.InTenantTx(context.Background(), e.f.Owner, e.f.Tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT reserved::text, committed::text FROM eacp.budget_accounts
			WHERE id = $1`, e.account).Scan(&reserved, &committed)
	}))
	return reserved, committed
}

func str(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func TestTheGatewayMetersAndLimitsSpend(t *testing.T) {
	e := newLLMEnv(t, nil)
	// fakellm: input = 10 + len("hello there")/4 = 12, output = 20: 12 x 3 + 20 x 15 = 336 micro-USD.
	if code, body := e.messages("hello there", 100, false); code != 200 || !strings.Contains(string(body), fakellm.Reply) {
		t.Fatalf("%d %s", code, body)
	}
	c := e.latest()
	if c.State != "SETTLED" || c.Outcome != llm.OutcomeSucceeded || *c.InputTokens != 12 || *c.OutputTokens != 20 ||
		str(c.CostAmount) != "0.000336" || str(c.CommittedAmount) != "0.000336" || c.CostUnit != "USD" || c.Stream {
		t.Fatalf("call %+v", c)
	}
	if code, _ := e.messages("hello there", 100, true); code != 200 {
		t.Fatalf("stream %d", code)
	}
	if c := e.latest(); c.Outcome != llm.OutcomeSucceeded || !c.Stream || str(c.CostAmount) != "0.000336" {
		t.Fatalf("streamed call %+v", c)
	}
	if code, _ := e.messages("error_429", 100, false); code != 429 {
		t.Fatalf("error_429 %d", code)
	}
	if c := e.latest(); c.Outcome != llm.OutcomeProviderError || str(c.CommittedAmount) != "0.000000" || *c.ProviderStatus != 429 {
		t.Fatalf("provider error %+v", c)
	}
	if reserved, committed := e.budget(); reserved != "0.000000" || committed != "0.000672" {
		t.Fatalf("reserved %s committed %s", reserved, committed)
	}
	// 4 000 output tokens reserve 0.06 USD: over the 0.05 limit.
	code, body := e.messages("hi", 4000, false)
	if code != 403 || !strings.Contains(string(body), "eacp: budget_exceeded") {
		t.Fatalf("%d %s", code, body)
	}
	if c := e.latest(); c.State != "DENIED" || c.Denial != "budget_exceeded" {
		t.Fatalf("denied call %+v", c)
	}
	// A model outside the allowlist.
	req, _ := http.NewRequest("POST", e.gw.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+e.agentKey)
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 403 || !strings.Contains(string(b), `"code":"model_not_in_allowlist"`) {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	// fakellm saw exactly the three admitted calls.
	req, _ = http.NewRequest("GET", e.fake.URL+"/v1/audit", nil)
	req.Header.Set("x-api-key", fakeKey)
	resp, err = http.DefaultClient.Do(req)
	must(t, err)
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if n := strings.Count(string(b), `"api":"anthropic"`); n != 3 || strings.Contains(string(b), "openai") {
		t.Fatalf("fakellm audit %s", b)
	}
}

func TestAKillBeforeTheUpstreamCallCuts(t *testing.T) {
	var killed bool
	e := newLLMEnv(t, func(e *llmEnv) {
		if !killed {
			killed = true
			must(e.t, e.f.Exec("otto", `SELECT eacp.set_kill('model', $1, true, 'provider incident')`, e.sonnet))
		}
	})
	code, body := e.messages("hi", 100, false)
	if code != 403 || !strings.Contains(string(body), "eacp: killed") {
		t.Fatalf("%d %s", code, body)
	}
	c := e.latest()
	if c.State != "SETTLED" || c.Outcome != llm.OutcomeKilled || c.ProviderStatus != nil {
		t.Fatalf("call %+v", c)
	}
	// The kill stands: the next call is denied at admission.
	if code, body := e.messages("hi", 100, false); code != 403 || !strings.Contains(string(body), "eacp: killed") {
		t.Fatalf("%d %s", code, body)
	}
	if c := e.latest(); c.State != "DENIED" || c.Denial != "killed" {
		t.Fatalf("call %+v", c)
	}
	must(t, e.f.Exec("opal", `SELECT eacp.set_kill('model', $1, false, 'resolved')`, e.sonnet))

	// A kill during a slow stream cuts it within a couple of polls.
	body = []byte(`{"model":"sonnet","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"slow"}]}`)
	req, _ := http.NewRequest("POST", e.gw.URL+"/v1/messages", strings.NewReader(string(body)))
	req.Header.Set("x-api-key", e.agentKey)
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	readEvent(t, br)
	must(t, e.f.Exec("otto", `SELECT eacp.set_kill('agent', $1, true, 'incident')`, e.agent.Agent))
	start := time.Now()
	rest, _ := io.ReadAll(br)
	if time.Since(start) > 3*time.Second || !strings.Contains(string(rest), "eacp: killed") {
		t.Fatalf("after %v: %q", time.Since(start), rest)
	}
	if c := e.latest(); c.Outcome != llm.OutcomeKilled || str(c.CommittedAmount) == "0.000000" {
		t.Fatalf("call %+v", c)
	}
}

func TestTheSweeperSettlesAnAbandonedCall(t *testing.T) {
	e := newLLMEnv(t, nil)
	// A gateway that died after admission leaves the call ADMITTED.
	a, err := e.store.Admit(context.Background(), e.f.Tenant, llm.AdmitRequest{AgentVersionID: e.agent.Version,
		ModelName: "sonnet", Provider: "anthropic", GatewayID: "gw-dead", RequestBytes: 1000, MaxOutputTokens: 100,
		Decision: llm.Decision{ID: uuid.New(), BundleID: uuid.New(), Version: 1, Verdict: "allow"}})
	if err != nil || a.Denial != "" {
		t.Fatalf("%+v %v", a, err)
	}
	must(t, storage.InTenantTx(context.Background(), e.f.Owner, e.f.Tenant.String(), func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `ALTER TABLE eacp.llm_calls DISABLE TRIGGER USER;
			UPDATE eacp.llm_calls SET deadline = now() - interval '1 second' WHERE id = '`+a.CallID.String()+`';
			ALTER TABLE eacp.llm_calls ENABLE TRIGGER USER`)
		return err
	}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		llmgateway.RunSweeper(ctx, 50*time.Millisecond, e.store, slog.New(slog.NewJSONHandler(e.logs, nil)))
		close(done)
	}()
	defer func() { cancel(); <-done }()
	c := e.latest()
	// 1000 bytes x 3 + 100 x 15 = 4 500 micro-USD, committed in full.
	if c.Outcome != llm.OutcomeAbandoned || str(c.CommittedAmount) != "0.004500" || c.CostAmount != nil {
		t.Fatalf("call %+v", c)
	}
}

func TestNothingSecretIsPersistedByTheGateway(t *testing.T) {
	e := newLLMEnv(t, nil)
	const prompt = "PROMPT-CANARY-8d1"
	e.messages(prompt, 100, false)
	e.messages(prompt, 100, true)
	e.messages("error_500", 100, false)
	e.latest()
	conn, err := pgx.Connect(context.Background(), e.f.DB.AdminDSN)
	must(t, err)
	defer conn.Close(context.Background())
	rows, err := conn.Query(context.Background(), `SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'eacp' AND table_type = 'BASE TABLE'`)
	must(t, err)
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	must(t, err)
	var dump strings.Builder
	for _, table := range tables {
		rows, err := conn.Query(context.Background(), `SELECT t::text FROM eacp.`+pgx.Identifier{table}.Sanitize()+` t`)
		must(t, err)
		texts, err := pgx.CollectRows(rows, pgx.RowTo[string])
		must(t, err)
		for _, s := range texts {
			dump.WriteString(s)
		}
	}
	// The journal is bytea; decode it too.
	rows, err = conn.Query(context.Background(), `SELECT convert_from(payload, 'UTF8') FROM eacp.audit_events`)
	must(t, err)
	journal, err := pgx.CollectRows(rows, pgx.RowTo[string])
	must(t, err)
	all := dump.String() + strings.Join(journal, "") + e.logs.String()
	if !strings.Contains(all, "llm.settled") {
		t.Fatal("the dump holds no settlement")
	}
	secret := strings.SplitN(e.agentKey, "_", 5)[4]
	for _, canary := range []string{fakeKey, e.agentKey, secret, prompt, fakellm.Reply} {
		if strings.Contains(all, canary) {
			t.Fatalf("a row, journal entry or log line holds %q", canary)
		}
	}
}
