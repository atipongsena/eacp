package llm_test

// Schema-level tests of the LLM-call ledger (ADR-031, migration 00024), in
// raw SQL as eacp_app: admission, reservations, settlement and the sweeper.

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"eacp/internal/registry/registrytest"
	"eacp/internal/storage"
)

const (
	sqlForbidden = "42501"
	sqlBadState  = "55000"
)

func wantState(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("err = %v, want SQLSTATE %s", err, code)
	}
}

// env is a tenant with an agent whose ACTIVE version may call model sonnet
// (anthropic, priced 3/15 USD per million tokens, cap 4096) and not gpt
// (openai, priced), and a USD leaf account with limit 1.
type env struct {
	f       *registrytest.Fixture
	agent   registrytest.Agent
	group   uuid.UUID
	sonnet  uuid.UUID
	gpt     uuid.UUID
	account uuid.UUID
}

func newEnv(t *testing.T) *env {
	t.Helper()
	f := registrytest.New(t)
	e := &env{f: f}
	e.sonnet = f.LLMModel(t, "sonnet", "anthropic", "https://api.anthropic.test", "claude-x")
	e.gpt = f.LLMModel(t, "gpt", "openai", "https://api.openai.test", "gpt-x")
	e.price(t, "anthropic", "claude-x", "3", "15", nil, nil)
	e.price(t, "openai", "gpt-x", "2", "8", nil, nil)
	e.group = f.ID(t, "alice", `INSERT INTO eacp.groups (tenant_id, name, display_name)
		VALUES (eacp.current_tenant_id(), 'ml', 'ml') RETURNING id`)
	e.agent.Agent = f.ID(t, "erin", `INSERT INTO eacp.agents (tenant_id, name, display_name, environment, risk_class,
		owner_group_id) VALUES (eacp.current_tenant_id(), 'writer', 'writer', 'production', 'high', $1) RETURNING id`, e.group)
	e.agent.Version = f.ID(t, "erin", `INSERT INTO eacp.agent_versions (tenant_id, agent_id, runtime, code_ref)
		VALUES (eacp.current_tenant_id(), $1, 'python', 'git:abc') RETURNING id`, e.agent.Agent)
	al := f.ID(t, "erin", `INSERT INTO eacp.agent_allowlists (tenant_id, agent_version_id, tool_ids, model_ids)
		VALUES (eacp.current_tenant_id(), $1, '{}', $2) RETURNING id`, e.agent.Version, []uuid.UUID{e.sonnet})
	must(t, f.Exec("rita", `UPDATE eacp.agent_versions SET active_allowlist_id = $1 WHERE id = $2`, al, e.agent.Version))
	must(t, f.Exec("ravi", `UPDATE eacp.agent_versions SET state = 'ACTIVE', state_reason = 'go live' WHERE id = $1`,
		e.agent.Version))
	e.account = f.FundAgent(t, e.agent.Agent, "USD", "1")
	return e
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (e *env) price(t *testing.T, provider, model, in, out string, cached, cacheWrite any) {
	t.Helper()
	e.f.ID(t, "alice", `INSERT INTO eacp.model_prices (tenant_id, provider, model, unit, input_per_mtok,
		output_per_mtok, cached_input_per_mtok, cache_write_per_mtok, reason)
		VALUES (eacp.current_tenant_id(), $1, $2, 'USD', $3::numeric, $4::numeric, $5::numeric, $6::numeric, 'list')
		RETURNING id`, provider, model, in, out, cached, cacheWrite)
}

type admitIn struct {
	ModelName       string         `json:"model_name"`
	Provider        string         `json:"provider"`
	Subject         string         `json:"subject,omitempty"`
	Stream          bool           `json:"stream"`
	RequestBytes    int64          `json:"request_bytes"`
	MaxOutputTokens *int64         `json:"max_output_tokens"`
	TraceID         string         `json:"trace_id,omitempty"`
	GatewayID       string         `json:"gateway_id"`
	Decision        map[string]any `json:"decision"`
}

func allowed() map[string]any {
	return map[string]any{"id": uuid.NewString(), "bundle_id": uuid.NewString(), "version": 1, "verdict": "allow",
		"input_digest": "00", "denial": ""}
}

func req(model, provider string, bytes, maxOut int64) admitIn {
	return admitIn{ModelName: model, Provider: provider, RequestBytes: bytes, MaxOutputTokens: &maxOut,
		GatewayID: "gw-1", Decision: allowed()}
}

type admission struct {
	CallID          uuid.UUID `json:"call_id"`
	Denial          string    `json:"denial"`
	ModelID         uuid.UUID `json:"model_id"`
	UpstreamModel   string    `json:"upstream_model"`
	BaseURL         string    `json:"base_url"`
	SecretRef       string    `json:"secret_ref"`
	TimeoutMS       int       `json:"timeout_ms"`
	MaxOutputTokens int64     `json:"max_output_tokens"`
	Deadline        time.Time `json:"deadline"`
}

func (e *env) admitAs(version uuid.UUID, in admitIn) (admission, error) {
	var out admission
	b, _ := json.Marshal(in)
	err := storage.InTenantTx(context.Background(), e.f.App, e.f.Tenant.String(), func(tx pgx.Tx) error {
		if err := storage.SetAgent(context.Background(), tx, version); err != nil {
			return err
		}
		var raw []byte
		if err := tx.QueryRow(context.Background(), `SELECT eacp.llm_admit($1::jsonb)`, b).Scan(&raw); err != nil {
			return err
		}
		return json.Unmarshal(raw, &out)
	})
	return out, err
}

func (e *env) admit(t *testing.T, in admitIn) admission {
	t.Helper()
	a, err := e.admitAs(e.agent.Version, in)
	must(t, err)
	return a
}

type settled struct {
	Cost      *string `json:"cost_amount"`
	Unit      *string `json:"cost_unit"`
	Committed *string `json:"committed_amount"`
}

func (e *env) settleAs(component string, call uuid.UUID, outcome string, status int, in, cr, cw, out int64, known bool) (settled, error) {
	var s settled
	err := storage.InTenantTx(context.Background(), e.f.App, e.f.Tenant.String(), func(tx pgx.Tx) error {
		if err := storage.SetSystem(context.Background(), tx, component); err != nil {
			return err
		}
		var raw []byte
		if err := tx.QueryRow(context.Background(), `SELECT eacp.llm_settle($1, $2, $3, $4, $5, $6, $7, $8)`,
			call, outcome, status, in, cr, cw, out, known).Scan(&raw); err != nil {
			return err
		}
		return json.Unmarshal(raw, &s)
	})
	return s, err
}

func (e *env) settle(t *testing.T, call uuid.UUID, outcome string, status int, in, cr, cw, out int64, known bool) settled {
	t.Helper()
	s, err := e.settleAs("llm_gateway", call, outcome, status, in, cr, cw, out, known)
	must(t, err)
	return s
}

func (e *env) row(t *testing.T, sql string, args []any, dest ...any) {
	t.Helper()
	err := storage.InTenantTx(context.Background(), e.f.Owner, e.f.Tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), sql, args...).Scan(dest...)
	})
	if err != nil {
		t.Fatalf("%v\nSQL: %s", err, sql)
	}
}

func (e *env) audits(t *testing.T, action string) int {
	t.Helper()
	var n int
	e.row(t, `SELECT count(*) FROM eacp.audit_events WHERE convert_from(payload, 'UTF8')::jsonb->>'action' = $1`,
		[]any{action}, &n)
	return n
}

func TestAdmitDeniesInOrder(t *testing.T) {
	e := newEnv(t)
	draft := e.f.ID(t, "erin", `INSERT INTO eacp.agent_versions (tenant_id, agent_id, runtime, code_ref)
		VALUES (eacp.current_tenant_id(), $1, 'python', 'git:draft') RETURNING id`, e.agent.Agent)
	unpriced := e.f.LLMModel(t, "haiku", "anthropic", "https://api.anthropic.test", "claude-unpriced")
	al := e.f.ID(t, "erin", `INSERT INTO eacp.agent_allowlists (tenant_id, agent_version_id, tool_ids, model_ids)
		VALUES (eacp.current_tenant_id(), $1, '{}', $2) RETURNING id`, draft, []uuid.UUID{e.sonnet})
	must(t, e.f.Exec("rita", `UPDATE eacp.agent_versions SET active_allowlist_id = $1 WHERE id = $2`, al, draft))

	policy := req("sonnet", "anthropic", 100, 10)
	policy.Decision = allowed()
	policy.Decision["verdict"], policy.Decision["denial"] = "deny", "policy_denied"
	subject := req("sonnet", "anthropic", 100, 10)
	subject.Subject = "nobody@tenant-a.test"
	cases := []struct {
		code    string
		version uuid.UUID
		in      admitIn
		before  func()
	}{
		{"agent_version_not_active", draft, req("sonnet", "anthropic", 100, 10), nil},
		{"subject_invalid", e.agent.Version, subject, nil},
		{"unknown_model", e.agent.Version, req("nope", "anthropic", 100, 10), nil},
		{"wrong_provider", e.agent.Version, req("sonnet", "openai", 100, 10), nil},
		{"model_not_in_allowlist", e.agent.Version, req("gpt", "openai", 100, 10), nil},
		{"policy_denied", e.agent.Version, policy, nil},
		{"max_tokens_over_cap", e.agent.Version, req("sonnet", "anthropic", 100, 4097), nil},
		{"budget_exceeded", e.agent.Version, req("sonnet", "anthropic", 100, 4096), func() { e.f.SetLimit(t, e.account, "0.01") }},
		{"killed", e.agent.Version, req("sonnet", "anthropic", 100, 10), func() {
			must(t, e.f.Exec("otto", `SELECT eacp.set_kill('team', $1, true, 'incident')`, e.group))
		}},
	}
	for _, c := range cases {
		if c.before != nil {
			c.before()
		}
		a, err := e.admitAs(c.version, c.in)
		must(t, err)
		if a.Denial != c.code || a.CallID == uuid.Nil {
			t.Fatalf("%s: admission %+v", c.code, a)
		}
		var state, denial string
		var reservations int
		e.row(t, `SELECT c.state, c.denial, (SELECT count(*) FROM eacp.budget_reservations r WHERE r.llm_call_id = c.id)
			FROM eacp.llm_calls c WHERE c.id = $1`, []any{a.CallID}, &state, &denial, &reservations)
		if state != "DENIED" || denial != c.code || reservations != 0 {
			t.Fatalf("%s: row %s %s %d", c.code, state, denial, reservations)
		}
	}
	// A model without a price is never served (it cannot be metered).
	must(t, e.f.Exec("opal", `SELECT eacp.set_kill('team', $1, false, 'resolved')`, e.group))
	al2 := e.f.ID(t, "erin", `INSERT INTO eacp.agent_allowlists (tenant_id, agent_version_id, tool_ids, model_ids)
		VALUES (eacp.current_tenant_id(), $1, '{}', $2) RETURNING id`, e.agent.Version, []uuid.UUID{e.sonnet, unpriced})
	must(t, e.f.Exec("ravi", `UPDATE eacp.agent_versions SET active_allowlist_id = $1 WHERE id = $2`, al2, e.agent.Version))
	if a := e.admit(t, req("haiku", "anthropic", 100, 10)); a.Denial != "model_unpriced" {
		t.Fatalf("unpriced: %+v", a)
	}
	if n := e.audits(t, "llm.denied"); n != len(cases)+1 {
		t.Fatalf("%d llm.denied events", n)
	}
}

func TestAdmitReservesTheEstimate(t *testing.T) {
	e := newEnv(t)
	e.price(t, "anthropic", "claude-x", "3", "15", "0.3", "3.75") // a later price wins
	a := e.admit(t, req("sonnet", "anthropic", 2000, 1000))
	if a.Denial != "" || a.UpstreamModel != "claude-x" || a.BaseURL != "https://api.anthropic.test" ||
		a.SecretRef != "llm" || a.TimeoutMS != 600000 || a.MaxOutputTokens != 1000 ||
		time.Until(a.Deadline) < 10*time.Minute {
		t.Fatalf("admission %+v", a)
	}
	// 2000 bytes x max(3, 0.3, 3.75) + 1000 x 15 = 22 500 micro-USD.
	var amount, reserved string
	e.row(t, `SELECT r.amount::text, a.reserved::text FROM eacp.budget_reservations r
		JOIN eacp.budget_accounts a ON a.tenant_id = r.tenant_id AND a.id = r.account_id
		WHERE r.llm_call_id = $1 AND r.state = 'ACTIVE'`, []any{a.CallID}, &amount, &reserved)
	if amount != "0.022500" || reserved != "0.022500" {
		t.Fatalf("reservation %s, reserved %s", amount, reserved)
	}
	var last string
	e.row(t, `SELECT convert_from(payload, 'UTF8')::jsonb->>'action' FROM eacp.audit_events ORDER BY seq DESC LIMIT 1`,
		nil, &last)
	if last != "llm.admitted" {
		t.Fatalf("last audit event %s", last)
	}
	// Without an output cap the model's cap applies.
	noCap := req("sonnet", "anthropic", 10, 0)
	noCap.MaxOutputTokens = nil
	if b := e.admit(t, noCap); b.MaxOutputTokens != 4096 {
		t.Fatalf("no cap: %+v", b)
	}
}

func TestAdmitWithoutALeafAccountRunsUnreserved(t *testing.T) {
	e := newEnv(t)
	must(t, storage.InTenantTx(context.Background(), e.f.Owner, e.f.Tenant.String(), func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `UPDATE eacp.model_prices SET unit = 'EUR' WHERE provider = 'anthropic'`)
		return err
	}))
	a := e.admit(t, req("sonnet", "anthropic", 100, 10))
	var n int
	e.row(t, `SELECT count(*) FROM eacp.budget_reservations WHERE llm_call_id = $1`, []any{a.CallID}, &n)
	if a.Denial != "" || n != 0 {
		t.Fatalf("admission %+v, %d reservations", a, n)
	}
	s := e.settle(t, a.CallID, "succeeded", 200, 100, 0, 0, 50, true)
	if s.Cost == nil || *s.Cost != "0.001050" || s.Committed != nil {
		t.Fatalf("unreserved settlement %+v", s)
	}
}

func TestSettleOutcomes(t *testing.T) {
	e := newEnv(t)
	type want struct{ cost, committed any }
	check := func(name string, s settled, w want) {
		t.Helper()
		str := func(p *string) any {
			if p == nil {
				return nil
			}
			return *p
		}
		if str(s.Cost) != w.cost || str(s.Committed) != w.committed {
			t.Fatalf("%s: cost %v committed %v, want %v", name, str(s.Cost), str(s.Committed), w)
		}
	}
	// 100 in x 3 + 50 out x 15 = 1 050 micro-USD, under the 4 500 reserved.
	ok := e.admit(t, req("sonnet", "anthropic", 1000, 100))
	check("succeeded", e.settle(t, ok.CallID, "succeeded", 200, 100, 0, 0, 50, true), want{"0.001050", "0.001050"})
	var source string
	var cost string
	var input, cache, output int64
	e.row(t, `SELECT source, cost_amount::text, input_tokens, cache_read_tokens, output_tokens FROM eacp.usage_records
		WHERE llm_call_id = $1`, []any{ok.CallID}, &source, &cost, &input, &cache, &output)
	if source != "gateway" || cost != "0.001050" || input != 100 || cache != 0 || output != 50 {
		t.Fatalf("usage row %s %s %d %d %d", source, cost, input, cache, output)
	}
	// A cost above the reservation: the budget commits the reservation, the call keeps the full cost.
	over := e.admit(t, req("sonnet", "anthropic", 10, 10)) // 10 x 3 + 10 x 15 = 180 micro-USD
	check("over", e.settle(t, over.CallID, "succeeded", 200, 1000, 0, 0, 10, true), want{"0.003150", "0.000180"})
	errd := e.admit(t, req("sonnet", "anthropic", 1000, 100))
	check("provider_error", e.settle(t, errd.CallID, "provider_error", 429, 0, 0, 0, 0, false), want{"0.000000", "0.000000"})
	for _, outcome := range []string{"usage_unknown", "killed"} {
		a := e.admit(t, req("sonnet", "anthropic", 1000, 100)) // 1000 x 3 + 100 x 15 = 4 500
		check(outcome, e.settle(t, a.CallID, outcome, 200, 0, 0, 0, 0, false), want{nil, "0.004500"})
		var n int
		e.row(t, `SELECT count(*) FROM eacp.usage_records WHERE llm_call_id = $1`, []any{a.CallID}, &n)
		if n != 0 {
			t.Fatalf("%s wrote a usage row", outcome)
		}
	}
	// Cache writes without a cache-write price are unpriced: full commit.
	cw := e.admit(t, req("sonnet", "anthropic", 1000, 100))
	check("unpriced cache write", e.settle(t, cw.CallID, "succeeded", 200, 100, 0, 20, 50, true), want{nil, "0.004500"})
	// Folding moves settled amounts into committed.
	must(t, e.f.Exec("alice", `SELECT eacp.budget_fold($1)`, e.account))
	var committed, reserved string
	e.row(t, `SELECT committed::text, reserved::text FROM eacp.budget_accounts WHERE id = $1`, []any{e.account},
		&committed, &reserved)
	// 0.001050 + 0.000180 + 0 + 0.0045 x 3
	if committed != "0.014730" || reserved != "0.000000" {
		t.Fatalf("committed %s reserved %s", committed, reserved)
	}
	if n := e.audits(t, "llm.settled"); n != 6 {
		t.Fatalf("%d llm.settled events", n)
	}
}

func TestSettleIsOnceAndOnlyByTheGateway(t *testing.T) {
	e := newEnv(t)
	a := e.admit(t, req("sonnet", "anthropic", 100, 10))
	_, err := e.settleAs("llm_sweeper", a.CallID, "succeeded", 200, 1, 0, 0, 1, true)
	wantState(t, err, sqlForbidden)
	_, err = e.settleAs("sweeper", a.CallID, "succeeded", 200, 1, 0, 0, 1, true)
	wantState(t, err, sqlForbidden)
	wantState(t, e.f.ExecAgent(e.agent.Version, `SELECT eacp.llm_settle($1, 'succeeded', 200, 1, 0, 0, 1, true)`, a.CallID),
		sqlForbidden)
	wantState(t, e.f.Exec("alice", `SELECT eacp.llm_settle($1, 'succeeded', 200, 1, 0, 0, 1, true)`, a.CallID),
		sqlForbidden)
	_, err = e.settleAs("llm_gateway", a.CallID, "abandoned", 0, 0, 0, 0, 0, false)
	wantState(t, err, sqlForbidden) // only the sweeper abandons
	e.settle(t, a.CallID, "succeeded", 200, 1, 0, 0, 1, true)
	_, err = e.settleAs("llm_gateway", a.CallID, "succeeded", 200, 1, 0, 0, 1, true)
	wantState(t, err, sqlBadState)
}

func TestSweepSettlesOverdueCallsOnce(t *testing.T) {
	e := newEnv(t)
	a := e.admit(t, req("sonnet", "anthropic", 1000, 100))
	fresh := e.admit(t, req("sonnet", "anthropic", 1000, 100))
	must(t, storage.InTenantTx(context.Background(), e.f.Owner, e.f.Tenant.String(), func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `ALTER TABLE eacp.llm_calls DISABLE TRIGGER USER;
			UPDATE eacp.llm_calls SET deadline = now() - interval '1 second' WHERE id = '`+a.CallID.String()+`';
			ALTER TABLE eacp.llm_calls ENABLE TRIGGER USER`)
		return err
	}))
	var tenants []uuid.UUID
	rows, err := e.f.App.Query(context.Background(), `SELECT eacp.llm_sweep_tenants()`)
	must(t, err)
	tenants, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	must(t, err)
	if len(tenants) != 1 || tenants[0] != e.f.Tenant {
		t.Fatalf("tenants %v", tenants)
	}
	var wg sync.WaitGroup
	counts := make([]int, 2)
	for i := range counts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = storage.InTenantTx(context.Background(), e.f.App, e.f.Tenant.String(), func(tx pgx.Tx) error {
				if err := storage.SetSystem(context.Background(), tx, "llm_sweeper"); err != nil {
					return err
				}
				return tx.QueryRow(context.Background(), `SELECT eacp.llm_sweep()`).Scan(&counts[i])
			})
		}()
	}
	wg.Wait()
	if counts[0]+counts[1] != 1 {
		t.Fatalf("swept %v", counts)
	}
	var outcome, committed string
	e.row(t, `SELECT outcome, committed_amount::text FROM eacp.llm_calls WHERE id = $1`, []any{a.CallID}, &outcome, &committed)
	var state string
	e.row(t, `SELECT state FROM eacp.llm_calls WHERE id = $1`, []any{fresh.CallID}, &state)
	if outcome != "abandoned" || committed != "0.004500" || state != "ADMITTED" {
		t.Fatalf("swept %s %s, fresh %s", outcome, committed, state)
	}
	wantState(t, e.f.ExecSystem("llm_gateway", `SELECT eacp.llm_sweep()`), sqlForbidden)
}

func TestLedgerNeverChanges(t *testing.T) {
	e := newEnv(t)
	a := e.admit(t, req("sonnet", "anthropic", 100, 10))
	for _, sql := range []string{
		`UPDATE eacp.llm_calls SET model_name = 'gpt' WHERE id = $1`,
		`UPDATE eacp.llm_calls SET state = 'SETTLED', outcome = 'succeeded' WHERE id = $1`,
	} {
		wantState(t, e.f.ExecSystem("llm_gateway", sql, a.CallID), sqlForbidden)
	}
	wantState(t, e.f.Exec("alice", `DELETE FROM eacp.llm_calls WHERE id = $1`, a.CallID), sqlForbidden)
	_, err := e.f.TryAgentID(e.agent.Version, `INSERT INTO eacp.llm_calls (tenant_id, agent_version_id, model_name,
		stream, request_bytes, max_output_tokens, state, gateway_id)
		VALUES (eacp.current_tenant_id(), $1, 'sonnet', false, 1, 1, 'ADMITTED', 'gw') RETURNING id`, e.agent.Version)
	wantState(t, err, sqlForbidden)
}

// The gate a trigger reads (eacp.llm_call) is a setting any transaction can
// set; setting it by hand must not let raw SQL as eacp_app write the ledger,
// an LLM reservation or gateway usage outside llm_admit, llm_settle and
// llm_sweep (ADR-031 §3).
func TestTheLedgerCannotBeWrittenBySettingItsGate(t *testing.T) {
	e := newEnv(t)
	a := e.admit(t, req("sonnet", "anthropic", 100, 10))
	gate := func(id uuid.UUID, stmt string) string {
		return "DO $$ BEGIN PERFORM set_config('eacp.llm_call', '" + id.String() + "', true); " + stmt + "; END $$"
	}
	forged := uuid.New()
	// An admitted call for a model off the allowlist, over the cap, with no reservation.
	wantState(t, e.f.ExecAgent(e.agent.Version, gate(forged, `INSERT INTO eacp.llm_calls (tenant_id, id,
		agent_version_id, model_id, model_name, provider, stream, request_bytes, max_output_tokens, price_id, cost_unit,
		state, gateway_id, deadline) SELECT eacp.current_tenant_id(), '`+forged.String()+`', '`+e.agent.Version.String()+`',
		'`+e.gpt.String()+`', 'gpt', 'openai', false, 1, 99999999, id, 'USD', 'ADMITTED', 'gw', now() + interval '1 hour'
		FROM eacp.model_prices WHERE model = 'gpt-x'`)), sqlForbidden)
	// A settlement at cost 0 that leaves the reservation active.
	wantState(t, e.f.ExecSystem("llm_gateway", gate(a.CallID, `UPDATE eacp.llm_calls SET state = 'SETTLED',
		outcome = 'succeeded', input_tokens = 1, cache_read_tokens = 0, cache_write_tokens = 0, output_tokens = 1,
		cost_amount = 0, committed_amount = 0 WHERE id = '`+a.CallID.String()+`'`)), sqlForbidden)
	// The reservation released by hand.
	wantState(t, e.f.ExecSystem("llm_gateway", gate(a.CallID, `UPDATE eacp.budget_reservations SET state = 'RELEASED',
		settle_reason = 'forged' WHERE llm_call_id = '`+a.CallID.String()+`'`)), sqlForbidden)
	// A second reservation for the same agent's call, made by hand.
	wantState(t, e.f.ExecAgent(e.agent.Version, gate(forged, `INSERT INTO eacp.budget_reservations (tenant_id,
		account_id, unit, llm_call_id, amount, expires_at) VALUES (eacp.current_tenant_id(), '`+e.account.String()+`',
		'USD', '`+a.CallID.String()+`', 0.000001, now() + interval '1 hour')`)), sqlForbidden)
	// The call is still ADMITTED and its reservation ACTIVE: llm_settle still works.
	if s := e.settle(t, a.CallID, "succeeded", 200, 10, 0, 0, 5, true); s.Cost == nil {
		t.Fatalf("settle after the forgeries = %+v", s)
	}
}

func TestKillScopesBindLLMCalls(t *testing.T) {
	for _, scope := range []string{"tenant", "team", "agent", "agent_version", "model"} {
		t.Run(scope, func(t *testing.T) {
			e := newEnv(t)
			target := map[string]uuid.UUID{"tenant": e.f.Tenant, "team": e.group, "agent": e.agent.Agent,
				"agent_version": e.agent.Version, "model": e.sonnet}[scope]
			a := e.admit(t, req("sonnet", "anthropic", 100, 10))
			var killed bool
			killedQ := func() bool {
				must(t, storage.InTenantTx(context.Background(), e.f.App, e.f.Tenant.String(), func(tx pgx.Tx) error {
					return tx.QueryRow(context.Background(), `SELECT eacp.llm_call_killed($1)`, a.CallID).Scan(&killed)
				}))
				return killed
			}
			if killedQ() {
				t.Fatal("killed before any kill")
			}
			must(t, e.f.Exec("otto", `SELECT eacp.set_kill($1, $2, true, 'incident')`, scope, target))
			if !killedQ() {
				t.Fatal("not killed")
			}
			if b := e.admit(t, req("sonnet", "anthropic", 100, 10)); b.Denial != "killed" {
				t.Fatalf("next call %+v", b)
			}
		})
	}
}

func TestReservationGuardForLLMCalls(t *testing.T) {
	e := newEnv(t)
	a := e.admit(t, req("sonnet", "anthropic", 100, 10))
	_, err := e.f.TryAgentID(e.agent.Version, `INSERT INTO eacp.budget_reservations (tenant_id, account_id, unit,
		llm_call_id, amount, expires_at) VALUES (eacp.current_tenant_id(), $1, 'USD', $2, 0.5, now() + interval '1 hour')
		RETURNING id`, e.account, a.CallID)
	wantState(t, err, sqlForbidden)
	wantState(t, e.f.ExecSystem("llm_gateway", `UPDATE eacp.budget_reservations SET state = 'RELEASED',
		settle_reason = 'x' WHERE llm_call_id = $1`, a.CallID), sqlForbidden)
}

func TestGatewaySpendReachesFinOps(t *testing.T) {
	e := newEnv(t)
	a := e.admit(t, req("sonnet", "anthropic", 1000, 100))
	e.settle(t, a.CallID, "succeeded", 200, 100, 0, 0, 50, true)
	var reported, effective string
	e.row(t, `SELECT llm_reported::text, llm_effective::text FROM eacp.finops_agent_spend(now() - interval '1 day',
		now() + interval '1 day') WHERE agent_id = $1 AND unit = 'USD'`, []any{e.agent.Agent}, &reported, &effective)
	if reported != "0.001050" || effective != "0.001050" {
		t.Fatalf("reported %s effective %s", reported, effective)
	}
	var metric string
	e.row(t, `SELECT eacp.release_version_metrics($1, now() - interval '1 day')->'cost'->>'USD'`,
		[]any{e.agent.Version}, &metric)
	if metric != "0.001050" {
		t.Fatalf("release cost %q", metric)
	}
}
