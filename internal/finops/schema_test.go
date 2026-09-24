package finops_test

// Schema-level tests (ADR-025): PostgreSQL binds usage to the agent key,
// prices it, keeps it insert-only and raises alerts. Raw SQL as eacp_app.

import (
	"context"
	"encoding/json"
	"errors"
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
	sqlCheck     = "23514"
	sqlUnique    = "23505"

	priceSQL = `INSERT INTO eacp.model_prices
		(tenant_id, provider, model, unit, input_per_mtok, cached_input_per_mtok, output_per_mtok, effective_from, reason)
		VALUES (eacp.current_tenant_id(), $1, $2, $3, $4, $5, $6, COALESCE($7, now()), 'rate card') RETURNING id`
	// otelSQL: $1 trace, $2 span, $3 operation, $4 model, $5 input, $6 cache, $7 output, $8 observed offset.
	otelSQL = `INSERT INTO eacp.usage_records
		(tenant_id, source, agent_id, trace_id, span_id, provider, operation, model,
		 input_tokens, cache_read_tokens, output_tokens, observed_at, cost_amount, cost_unit)
		VALUES (eacp.current_tenant_id(), 'otel', gen_random_uuid(), $1, $2, 'openai', $3, $4, $5, $6, $7,
		        now() - $8::interval, 999, 'XXX')`
	billingSQL = `INSERT INTO eacp.usage_records
		(tenant_id, source, agent_id, external_id, provider, operation, model,
		 input_tokens, output_tokens, observed_at, cost_amount, cost_unit)
		VALUES (eacp.current_tenant_id(), 'provider_billing', $1, $2, 'openai', 'chat', 'gpt-x', 10, 20,
		        now() - interval '1 day', $3, $4)`
)

func wantState(t *testing.T, err error, codes ...string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("err = %v, want SQLSTATE %v", err, codes)
	}
	for _, c := range codes {
		if pgErr.Code == c {
			return
		}
	}
	t.Fatalf("SQLSTATE %s (%s), want %v", pgErr.Code, pgErr.Message, codes)
}

func ok(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

var spanN int

// ids returns a fresh trace and span id.
func ids() (string, string) {
	spanN++
	u := uuid.New()
	return hexOf(u[:]), hexOf(u[:8])
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, 2*len(b))
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&15])
	}
	return string(out)
}

type usage struct {
	agent, version uuid.UUID
	source         string
	model          *string
	cost           *string
	unit           *string
	billable       bool
}

func ownerRow(t *testing.T, f *registrytest.Fixture, sql string, args []any, dest ...any) {
	t.Helper()
	ctx := context.Background()
	ok(t, storage.InTenantTx(ctx, f.Owner, f.Tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(dest...)
	}))
}

func record(t *testing.T, f *registrytest.Fixture, span string) usage {
	t.Helper()
	var u usage
	var version *uuid.UUID
	ownerRow(t, f, `SELECT agent_id, agent_version_id, source, model, cost_amount::text, cost_unit, billable
		FROM eacp.usage_records WHERE span_id = $1 OR external_id = $1`, []any{span},
		&u.agent, &version, &u.source, &u.model, &u.cost, &u.unit, &u.billable)
	if version != nil {
		u.version = *version
	}
	return u
}

// addPrice seeds a rate card effective 30 days ago. Only the schema owner
// (bootstrap) may backdate a price; an admin's price starts now.
func addPrice(t *testing.T, f *registrytest.Fixture, model, unit, in, cached, out string) uuid.UUID {
	t.Helper()
	var c any
	if cached != "" {
		c = cached
	}
	var id uuid.UUID
	ownerRow(t, f, priceSQL, []any{"openai", model, unit, in, c, out, time.Now().Add(-30 * 24 * time.Hour)}, &id)
	return id
}

func TestOtelUsageIsBoundToTheAgentKeyAndPricedInPostgres(t *testing.T) {
	f := registrytest.New(t)
	a := f.ActiveAgent(t, "buyer")
	addPrice(t, f, "gpt-x", "USD", "2.5", "1.25", "10")
	trace, span := ids()
	// The agent claims another agent and a cost; PostgreSQL ignores both.
	ok(t, f.ExecAgent(a.Version, otelSQL, trace, span, "chat", "gpt-x", 1000, 400, 500, "1 minute"))
	u := record(t, f, span)
	if u.agent != a.Agent || u.version != a.Version || u.source != "otel" || !u.billable {
		t.Fatalf("usage identity = %+v", u)
	}
	// (600 × 2.5 + 400 × 1.25 + 500 × 10) / 10^6
	if u.cost == nil || *u.cost != "0.007000" || u.unit == nil || *u.unit != "USD" {
		t.Fatalf("cost = %v %v", u.cost, u.unit)
	}
}

func TestCostRoundsUpAndTheCacheRuleNeverUndercharges(t *testing.T) {
	f := registrytest.New(t)
	a := f.ActiveAgent(t, "buyer")
	addPrice(t, f, "tiny", "USD", "0.5", "", "0")
	addPrice(t, f, "tinier", "USD", "0.4", "", "0")
	addPrice(t, f, "cached", "USD", "2", "1", "0")
	for _, tc := range []struct {
		model             string
		input, cache, out int
		want              string
	}{
		{"tiny", 1, 0, 0, "0.000001"},         // 0.0000005 rounds up
		{"tinier", 1, 0, 0, "0.000001"},       // 0.0000004 rounds up too, never down to zero
		{"cached", 1000, 2000, 0, "0.004000"}, // cache > input: 1000 × 2 + 2000 × 1
		{"cached", 3000, 1000, 0, "0.005000"}, // 2000 × 2 + 1000 × 1
	} {
		trace, span := ids()
		ok(t, f.ExecAgent(a.Version, otelSQL, trace, span, "chat", tc.model, tc.input, tc.cache, tc.out, "1 minute"))
		if u := record(t, f, span); u.cost == nil || *u.cost != tc.want {
			t.Errorf("%+v: cost = %v", tc, u.cost)
		}
	}
}

func TestUnpricedAndAgentLevelUsage(t *testing.T) {
	f := registrytest.New(t)
	a := f.ActiveAgent(t, "buyer")
	addPrice(t, f, "gpt-x", "USD", "1", "", "1")
	trace, span := ids()
	ok(t, f.ExecAgent(a.Version, otelSQL, trace, span, "chat", "unknown-model", 10, 0, 10, "1 minute"))
	if u := record(t, f, span); u.cost != nil || u.unit != nil || !u.billable {
		t.Fatalf("unpriced = %+v", u)
	}
	trace, span = ids()
	ok(t, f.ExecAgent(a.Version, otelSQL, trace, span, "chat", nil, 10, 0, 10, "1 minute"))
	if u := record(t, f, span); u.cost != nil {
		t.Fatalf("no model = %+v", u)
	}
	trace, span = ids()
	ok(t, f.ExecAgent(a.Version, otelSQL, trace, span, "invoke_agent", "gpt-x", 10, 0, 10, "1 minute"))
	if u := record(t, f, span); u.billable {
		t.Fatalf("agent-level usage is billable: %+v", u)
	}
}

func TestPriceInEffectAtObservationAndNoRetroactivePrices(t *testing.T) {
	f := registrytest.New(t)
	a := f.ActiveAgent(t, "buyer")
	addPrice(t, f, "gpt-x", "USD", "1", "", "0")
	// An admin's price takes effect from its insertion: a span observed a
	// minute earlier keeps the old price.
	f.ID(t, "alice", priceSQL, "openai", "gpt-x", "USD", "50", nil, "0", nil)
	f.ID(t, "alice", priceSQL, "openai", "gpt-x", "USD", "100", nil, "0", "2099-01-01T00:00:00Z")
	trace, span := ids()
	ok(t, f.ExecAgent(a.Version, otelSQL, trace, span, "chat", "gpt-x", 1000000, 0, 0, "1 minute"))
	if u := record(t, f, span); u.cost == nil || *u.cost != "1.000000" {
		t.Fatalf("a later price was applied: %v", u.cost)
	}
	trace, span = ids()
	ok(t, f.ExecAgent(a.Version, otelSQL, trace, span, "chat", "gpt-x", 1000000, 0, 0, "-1 second"))
	if u := record(t, f, span); u.cost == nil || *u.cost != "50.000000" {
		t.Fatalf("the current price was not applied: %v", u.cost)
	}
	_, err := f.TryID("alice", priceSQL, "openai", "gpt-x", "USD", "0.1", nil, "0", "2020-01-01T00:00:00Z")
	wantState(t, err, sqlCheck)
	_, err = f.TryID("otto", priceSQL, "openai", "gpt-y", "USD", "1", nil, "1", nil)
	wantState(t, err, sqlForbidden)
	_, err = f.TryID("alice", priceSQL, "openai", "gpt-y", "usd", "1", nil, "1", nil)
	wantState(t, err, sqlCheck)
}

func TestOtelUsageWindowAndIdempotentSpans(t *testing.T) {
	f := registrytest.New(t)
	a := f.ActiveAgent(t, "buyer")
	trace, span := ids()
	ok(t, f.ExecAgent(a.Version, otelSQL, trace, span, "chat", "gpt-x", 1, 0, 1, "1 minute"))
	wantState(t, f.ExecAgent(a.Version, otelSQL, trace, span, "chat", "gpt-x", 1, 0, 1, "1 minute"), sqlUnique)
	ok(t, f.ExecAgent(a.Version, otelSQL+` ON CONFLICT (tenant_id, trace_id, span_id) WHERE source = 'otel' DO NOTHING`,
		trace, span, "chat", "gpt-x", 1, 0, 1, "1 minute"))
	var n int
	ownerRow(t, f, `SELECT count(*) FROM eacp.usage_records`, nil, &n)
	if n != 1 {
		t.Fatalf("records after a retried span = %d", n)
	}
	trace, span = ids()
	wantState(t, f.ExecAgent(a.Version, otelSQL, trace, span, "chat", "gpt-x", 1, 0, 1, "8 days"), sqlCheck)
	wantState(t, f.ExecAgent(a.Version, otelSQL, trace, span, "chat", "gpt-x", 1, 0, 1, "-10 minutes"), sqlCheck)
	wantState(t, f.ExecAgent(a.Version, otelSQL, trace, span, "chat", "gpt-x", -1, 0, 1, "1 minute"), sqlCheck)
}

func TestOnlyTheRightActorMayRecordUsage(t *testing.T) {
	f := registrytest.New(t)
	a := f.ActiveAgent(t, "buyer")
	trace, span := ids()
	wantState(t, f.Exec("alice", otelSQL, trace, span, "chat", "gpt-x", 1, 0, 1, "1 minute"), sqlForbidden)
	// A transaction that binds a principal and an agent is neither.
	ctx := context.Background()
	wantState(t, storage.InTenantTx(ctx, f.App, f.Tenant.String(), func(tx pgx.Tx) error {
		if err := storage.SetActor(ctx, tx, f.P["alice"]); err != nil {
			return err
		}
		if err := storage.SetAgent(ctx, tx, a.Version); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, otelSQL, trace, span, "chat", "gpt-x", 1, 0, 1, "1 minute")
		return err
	}), sqlForbidden)
	wantState(t, storage.InTenantTx(ctx, f.App, f.Tenant.String(), func(tx pgx.Tx) error {
		if err := storage.SetAgent(ctx, tx, a.Version); err != nil {
			return err
		}
		if err := storage.SetSystem(ctx, tx, "finops"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, otelSQL, trace, span, "chat", "gpt-x", 1, 0, 1, "1 minute")
		return err
	}), sqlForbidden)
	wantState(t, f.ExecAgent(a.Version, billingSQL, a.Agent, "inv-1", "1.5", "USD"), sqlForbidden)
	wantState(t, storage.InTenantTx(context.Background(), f.App, f.Tenant.String(), func(tx pgx.Tx) error {
		if err := storage.SetActor(context.Background(), tx, f.P["alice"]); err != nil {
			return err
		}
		if err := storage.SetAgent(context.Background(), tx, a.Version); err != nil {
			return err
		}
		_, err := tx.Exec(context.Background(), billingSQL, a.Agent, "inv-1", "1.5", "USD")
		return err
	}), sqlForbidden)
	wantState(t, f.Exec("otto", billingSQL, a.Agent, "inv-1", "1.5", "USD"), sqlForbidden)
	wantState(t, f.Exec("alice", billingSQL, a.Agent, "inv-1", nil, nil), sqlCheck)
	ok(t, f.Exec("alice", billingSQL, a.Agent, "inv-1", "1.5", "USD"))
	u := record(t, f, "inv-1")
	if u.source != "provider_billing" || u.cost == nil || *u.cost != "1.500000" || u.agent != a.Agent {
		t.Fatalf("billing = %+v", u)
	}
	wantState(t, f.Exec("alice", billingSQL, a.Agent, "inv-1", "1.5", "USD"), sqlUnique)
}

func TestFinOpsTablesAreInsertOnly(t *testing.T) {
	f := registrytest.New(t)
	a := f.ActiveAgent(t, "buyer")
	price := addPrice(t, f, "gpt-x", "USD", "1", "", "1")
	ok(t, f.Exec("alice", billingSQL, a.Agent, "inv-1", "1.5", "USD"))
	for _, sql := range []string{
		`UPDATE eacp.model_prices SET input_per_mtok = 0 WHERE id = '` + price.String() + `'`,
		`DELETE FROM eacp.model_prices`,
		`UPDATE eacp.usage_records SET cost_amount = 0`,
		`DELETE FROM eacp.usage_records`,
	} {
		wantState(t, f.Exec("alice", sql), sqlForbidden)
	}
}

const alertSQL = `INSERT INTO eacp.finops_alerts (tenant_id, kind, subject_type, subject_id, unit, period_start, observed, threshold)
	VALUES (eacp.current_tenant_id(), 'spend_anomaly', 'agent', $1, 'USD', date_trunc('hour', now()), 10, 1) RETURNING id`

func TestAlertsAreRaisedByFinOpsAndAcknowledgedOnce(t *testing.T) {
	f := registrytest.New(t)
	a := f.ActiveAgent(t, "buyer")
	_, err := f.TryID("otto", alertSQL, a.Agent)
	wantState(t, err, sqlForbidden)
	ok(t, f.ExecSystem("finops", alertSQL, a.Agent))
	var id uuid.UUID
	ownerRow(t, f, `SELECT id FROM eacp.finops_alerts`, nil, &id)
	wantState(t, f.ExecSystem("sweeper", alertSQL, a.Agent), sqlForbidden)
	ack := `UPDATE eacp.finops_alerts SET ack_reason = $2, acknowledged_at = now() WHERE id = $1`
	wantState(t, f.Exec("carol", ack, id, "seen"), sqlForbidden)
	wantState(t, f.Exec("otto", ack, id, " "), sqlCheck)
	wantState(t, f.Exec("otto", `UPDATE eacp.finops_alerts SET observed = 0, ack_reason = 'x', acknowledged_at = now() WHERE id = $1`, id), sqlForbidden)
	ok(t, f.Exec("otto", ack, id, "investigating"))
	wantState(t, f.Exec("alice", ack, id, "again"), sqlBadState)
	var by uuid.UUID
	ownerRow(t, f, `SELECT acknowledged_by FROM eacp.finops_alerts WHERE id = $1`, []any{id}, &by)
	if by != f.P["otto"] {
		t.Fatalf("acknowledged_by = %s", by)
	}
	var actions []string
	ownerRow(t, f, `SELECT array_agg(convert_from(payload, 'UTF8')::jsonb->>'action' ORDER BY seq)
		FROM eacp.audit_events WHERE convert_from(payload, 'UTF8') LIKE '%finops_alert%'`, nil, &actions)
	if len(actions) != 2 || actions[0] != "finops_alert.raised" || actions[1] != "finops_alert.acknowledged" {
		t.Fatalf("alert journal = %v", actions)
	}
}

func TestSoftLimitsAreAdminOnlyAndJournaled(t *testing.T) {
	f := registrytest.New(t)
	account := f.ID(t, "alice", `INSERT INTO eacp.budget_accounts (tenant_id, name, unit)
		VALUES (eacp.current_tenant_id(), 'org', 'USD') RETURNING id`)
	set := `INSERT INTO eacp.budget_soft_limits (tenant_id, account_id, monthly_limit, reason)
		VALUES (eacp.current_tenant_id(), $1, $2, $3)
		ON CONFLICT (tenant_id, account_id) DO UPDATE SET monthly_limit = EXCLUDED.monthly_limit, reason = EXCLUDED.reason`
	wantState(t, f.Exec("otto", set, account, "100", "plan"), sqlForbidden)
	wantState(t, f.Exec("alice", set, account, "100", ""), sqlCheck)
	wantState(t, f.Exec("alice", set, account, "-1", "plan"), sqlCheck)
	ok(t, f.Exec("alice", set, account, "100", "plan"))
	ok(t, f.Exec("bob", set, account, "150", "more"))
	var limit string
	var by uuid.UUID
	ownerRow(t, f, `SELECT monthly_limit::text, set_by FROM eacp.budget_soft_limits WHERE account_id = $1`, []any{account}, &limit, &by)
	if limit != "150.000000" || by != f.P["bob"] {
		t.Fatalf("soft limit = %s by %s", limit, by)
	}
	// Soft limits, prices and billing lines are journaled with their actor
	// and reason; agent spans are not (they are high-volume observations).
	addPrice(t, f, "gpt-x", "USD", "1", "", "1")
	f.ID(t, "alice", priceSQL, "openai", "gpt-y", "USD", "1", nil, "1", nil)
	a := f.ActiveAgent(t, "buyer")
	ok(t, f.Exec("alice", billingSQL, a.Agent, "inv-1", "1", "USD"))
	trace, span := ids()
	ok(t, f.ExecAgent(a.Version, otelSQL, trace, span, "chat", "gpt-x", 1, 0, 1, "1 minute"))
	var journal []string
	ownerRow(t, f, `SELECT array_agg(concat_ws(' ', e->>'action', e->'actor'->>'id', e->>'reason') ORDER BY seq)
		FROM (SELECT seq, convert_from(payload, 'UTF8')::jsonb AS e FROM eacp.audit_events) x
		WHERE e->>'action' ~ '^(budget_soft_limits|model_prices|usage_records)\.'`, nil, &journal)
	system := "00000000-0000-0000-0000-000000000000"
	want := []string{
		"budget_soft_limits.insert " + f.P["alice"].String() + " plan",
		"budget_soft_limits.update " + f.P["bob"].String() + " more",
		"model_prices.insert " + system + " rate card",
		"model_prices.insert " + f.P["alice"].String() + " rate card",
		"usage_records.insert " + f.P["alice"].String() + " billing import inv-1",
	}
	if len(journal) != len(want) {
		t.Fatalf("journal = %q", journal)
	}
	for i := range want {
		if journal[i] != want[i] {
			t.Fatalf("journal = %q, want %q", journal, want)
		}
	}
}

// evaluate runs the evaluator as the finops system component.
func evaluate(t *testing.T, f *registrytest.Fixture) int {
	t.Helper()
	ctx := context.Background()
	var n int
	ok(t, storage.InTenantTx(ctx, f.App, f.Tenant.String(), func(tx pgx.Tx) error {
		if err := storage.SetSystem(ctx, tx, "finops"); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT eacp.finops_evaluate()`).Scan(&n)
	}))
	return n
}

type alert struct {
	Kind, Unit string
	Subject    uuid.UUID
	Observed   string
	Detail     map[string]any
}

func alerts(t *testing.T, f *registrytest.Fixture) []alert {
	t.Helper()
	ctx := context.Background()
	var out []alert
	ok(t, storage.InTenantTx(ctx, f.Owner, f.Tenant.String(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT kind, unit, subject_id, observed::text, detail FROM eacp.finops_alerts ORDER BY kind, created_at`)
		if err != nil {
			return err
		}
		var a alert
		var detail []byte
		var observed *string
		_, err = pgx.ForEachRow(rows, []any{&a.Kind, &a.Unit, &a.Subject, &observed, &detail}, func() error {
			a.Observed = ""
			if observed != nil {
				a.Observed = *observed
			}
			a.Detail = nil
			_ = json.Unmarshal(detail, &a.Detail)
			out = append(out, a)
			return nil
		})
		return err
	}))
	return out
}

func TestEvaluatorRaisesSoftLimitAlertsOnceOverTheSubtree(t *testing.T) {
	f := registrytest.New(t)
	a := f.ActiveAgent(t, "buyer")
	addPrice(t, f, "gpt-x", "USD", "1", "", "0")
	root := f.ID(t, "alice", `INSERT INTO eacp.budget_accounts (tenant_id, name, unit)
		VALUES (eacp.current_tenant_id(), 'org', 'USD') RETURNING id`)
	f.ID(t, "alice", `INSERT INTO eacp.budget_accounts (tenant_id, name, unit, parent_id, agent_id)
		VALUES (eacp.current_tenant_id(), 'buyer-usd', 'USD', $1, $2) RETURNING id`, root, a.Agent)
	ok(t, f.Exec("alice", `INSERT INTO eacp.budget_soft_limits (tenant_id, account_id, monthly_limit, reason)
		VALUES (eacp.current_tenant_id(), $1, 100, 'plan')`, root))
	spend := func(dollars int) {
		trace, span := ids()
		ok(t, f.ExecAgent(a.Version, otelSQL, trace, span, "chat", "gpt-x", dollars*1000000, 0, 0, "1 minute"))
	}
	spend(70)
	if n := evaluate(t, f); n != 0 {
		t.Fatalf("alerts at 70%% = %d", n)
	}
	spend(15)
	if n := evaluate(t, f); n != 1 {
		t.Fatalf("alerts at 85%% = %d", n)
	}
	if n := evaluate(t, f); n != 0 {
		t.Fatalf("re-evaluation raised %d alerts", n)
	}
	spend(20)
	evaluate(t, f)
	got := alerts(t, f)
	if len(got) != 2 || got[0].Kind != "soft_limit_exceeded" || got[1].Kind != "soft_limit_warning" ||
		got[0].Subject != root || got[0].Observed != "105.000000" {
		t.Fatalf("alerts = %+v", got)
	}
}

func TestEvaluatorFlagsSpendAnomaliesOnlyAgainstABaseline(t *testing.T) {
	f := registrytest.New(t)
	steady := f.ActiveAgent(t, "steady")
	fresh := f.ActiveAgent(t, "fresh")
	addPrice(t, f, "gpt-x", "USD", "1", "", "0")
	// Two days ago, one dollar: the baseline mean is 1/168 per hour.
	past := `INSERT INTO eacp.usage_records
		(tenant_id, source, agent_id, trace_id, span_id, provider, operation, model, input_tokens, observed_at)
		VALUES (eacp.current_tenant_id(), 'otel', gen_random_uuid(), $1, $2, 'openai', 'chat', 'gpt-x', $3, $4::timestamptz)`
	var lastHour string
	ownerRow(t, f, `SELECT (date_trunc('hour', now()) - interval '30 minutes')::text`, nil, &lastHour)
	var twoDaysAgo string
	ownerRow(t, f, `SELECT (now() - interval '2 days')::text`, nil, &twoDaysAgo)
	trace, span := ids()
	ok(t, f.ExecAgent(steady.Version, past, trace, span, 1000000, twoDaysAgo))
	trace, span = ids()
	ok(t, f.ExecAgent(steady.Version, past, trace, span, 5000000, lastHour))
	// A new agent's first spend has no baseline to break.
	trace, span = ids()
	ok(t, f.ExecAgent(fresh.Version, past, trace, span, 5000000, lastHour))
	// The factor is 3: a mean of 1/hour flags exactly 3, a mean of 2/hour
	// does not flag 5.
	spiky := f.ActiveAgent(t, "spiky")
	busy := f.ActiveAgent(t, "busy")
	for _, s := range []struct {
		version  uuid.UUID
		baseline int
		last     int
	}{{spiky.Version, 168, 3}, {busy.Version, 336, 5}} {
		trace, span = ids()
		ok(t, f.ExecAgent(s.version, past, trace, span, s.baseline*1000000, twoDaysAgo))
		trace, span = ids()
		ok(t, f.ExecAgent(s.version, past, trace, span, s.last*1000000, lastHour))
	}
	// Free usage (a zero price) is spend of zero: never an anomaly.
	addPrice(t, f, "free", "USD", "0", "", "0")
	free := f.ActiveAgent(t, "free")
	freeSQL := `INSERT INTO eacp.usage_records
		(tenant_id, source, agent_id, trace_id, span_id, provider, operation, model, input_tokens, observed_at)
		VALUES (eacp.current_tenant_id(), 'otel', gen_random_uuid(), $1, $2, 'openai', 'chat', 'free', 1000, $3::timestamptz)`
	for _, at := range []string{twoDaysAgo, lastHour} {
		trace, span = ids()
		ok(t, f.ExecAgent(free.Version, freeSQL, trace, span, at))
	}
	evaluate(t, f)
	got := alerts(t, f)
	flagged := map[uuid.UUID]string{}
	for _, a := range got {
		if a.Kind != "spend_anomaly" || a.Unit != "USD" {
			t.Fatalf("anomalies = %+v", got)
		}
		flagged[a.Subject] = a.Observed
	}
	if len(flagged) != 2 || flagged[steady.Agent] != "5.000000" || flagged[spiky.Agent] != "3.000000" {
		t.Fatalf("anomalies = %+v", got)
	}
	if n := evaluate(t, f); n != 0 {
		t.Fatalf("re-evaluation raised %d", n)
	}
}

func TestEvaluatorFlagsUnpricedUsageOncePerDay(t *testing.T) {
	f := registrytest.New(t)
	a := f.ActiveAgent(t, "buyer")
	for range 2 {
		trace, span := ids()
		ok(t, f.ExecAgent(a.Version, otelSQL, trace, span, "chat", "mystery", 100, 0, 50, "1 minute"))
	}
	trace, span := ids()
	ok(t, f.ExecAgent(a.Version, otelSQL, trace, span, "invoke_agent", "mystery", 100, 0, 50, "1 minute"))
	if n := evaluate(t, f); n != 1 {
		t.Fatalf("unpriced alerts = %d", n)
	}
	got := alerts(t, f)
	if got[0].Kind != "unpriced_usage" || got[0].Subject != a.Agent || got[0].Detail["tokens"] != float64(300) {
		t.Fatalf("unpriced = %+v", got)
	}
	wantState(t, f.Exec("otto", `SELECT eacp.finops_evaluate()`), sqlForbidden)
}
