package finops_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/finops"
	"eacp/internal/registry"
	"eacp/internal/registry/registrytest"
	"eacp/internal/storage"
	"eacp/internal/storage/pgtest"
)

func as(f *registrytest.Fixture, who string) registry.Actor {
	return registry.Actor{TenantID: f.Tenant, PrincipalID: f.P[who]}
}

func wantKind(t *testing.T, err error, kind error) {
	t.Helper()
	var re *registry.Error
	if !errors.As(err, &re) || re.Kind != kind {
		t.Fatalf("err = %v, want %v", err, kind)
	}
}

func chatSpan(model string, input, output int64, at time.Time) finops.Span {
	trace, id := ids()
	return finops.Span{TraceID: trace, SpanID: id, Operation: "chat", Provider: "openai", Model: model,
		InputTokens: input, OutputTokens: output, ObservedAt: at}
}

func TestRecordSpansBindsTheKeyAndReportsPartialSuccess(t *testing.T) {
	f := registrytest.New(t)
	a := f.ActiveAgent(t, "buyer")
	svc := finops.New(f.App)
	addPrice(t, f, "gpt-x", "USD", "1", "", "2")
	now := time.Now()
	good := chatSpan("gpt-x", 1000000, 1000000, now.Add(-time.Minute))
	old := chatSpan("gpt-x", 1, 1, now.Add(-8*24*time.Hour))
	in, err := svc.RecordSpans(context.Background(), f.Tenant, a.Version, []finops.Span{good, old})
	if err != nil {
		t.Fatal(err)
	}
	if in.Accepted != 1 || in.Rejected != 1 || in.Duplicates != 0 || in.Message == "" {
		t.Fatalf("ingest = %+v", in)
	}
	// An exporter's retry is a duplicate, never a second record.
	in, err = svc.RecordSpans(context.Background(), f.Tenant, a.Version, []finops.Span{good})
	if err != nil || in.Accepted != 0 || in.Duplicates != 1 {
		t.Fatalf("retry = %+v %v", in, err)
	}
	u := record(t, f, good.SpanID)
	if u.agent != a.Agent || u.version != a.Version || u.cost == nil || *u.cost != "3.000000" {
		t.Fatalf("record = %+v", u)
	}
	// A retired version's key reports nothing.
	ok(t, f.Exec("ravi", `UPDATE eacp.agent_versions SET state = 'RETIRED', state_reason = 'done' WHERE id = $1`, a.Version))
	_, err = svc.RecordSpans(context.Background(), f.Tenant, a.Version, []finops.Span{chatSpan("gpt-x", 1, 1, now)})
	wantKind(t, err, registry.ErrForbidden)
}

func TestPricesBillingAndSoftLimitsThroughTheService(t *testing.T) {
	f := registrytest.New(t)
	a := f.ActiveAgent(t, "buyer")
	svc := finops.New(f.App)
	ctx := context.Background()

	cached := "0.5"
	p, err := svc.AddPrice(ctx, as(f, "alice"), finops.NewPrice{Provider: "openai", Model: "gpt-x", Unit: "USD",
		InputPerMTok: "2.5", CachedPerMTok: &cached, OutputPerMTok: "10", Reason: "list price"})
	if err != nil || p.InputPerMTok != "2.5" || p.CachedPerMTok == nil || *p.CachedPerMTok != "0.5" || *p.CreatedBy != f.P["alice"] {
		t.Fatalf("price = %+v %v", p, err)
	}
	_, err = svc.AddPrice(ctx, as(f, "otto"), finops.NewPrice{Provider: "openai", Model: "gpt-y", Unit: "USD",
		InputPerMTok: "1", OutputPerMTok: "1", Reason: "x"})
	wantKind(t, err, registry.ErrForbidden)
	_, err = svc.AddPrice(ctx, as(f, "alice"), finops.NewPrice{Provider: "openai", Model: "gpt-y", Unit: "USD",
		InputPerMTok: "-1", OutputPerMTok: "1", Reason: "x"})
	wantKind(t, err, registry.ErrInvalid)
	if prices, err := svc.Prices(ctx, as(f, "audra")); err != nil || len(prices) != 1 {
		t.Fatalf("prices = %+v %v", prices, err)
	}

	lines := []finops.BillingLine{
		{ExternalID: "inv-1", AgentID: a.Agent, Provider: "openai", Model: "gpt-x", InputTokens: 10, Cost: "1.25",
			Unit: "USD", ObservedAt: time.Now().Add(-time.Hour)},
		{ExternalID: "inv-2", AgentID: a.Agent, Provider: "openai", Cost: "2", Unit: "USD", ObservedAt: time.Now().Add(-time.Hour)},
	}
	if im, err := svc.ImportBilling(ctx, as(f, "alice"), lines); err != nil || im.Inserted != 2 || im.Duplicates != 0 {
		t.Fatalf("import = %+v %v", im, err)
	}
	// A re-import keeps the first lines and reports the duplicates.
	if im, err := svc.ImportBilling(ctx, as(f, "alice"), lines); err != nil || im.Inserted != 0 || im.Duplicates != 2 {
		t.Fatalf("re-import = %+v %v", im, err)
	}
	// All or none: one bad line refuses the whole import.
	bad := []finops.BillingLine{
		{ExternalID: "inv-3", AgentID: a.Agent, Provider: "openai", Cost: "1", Unit: "USD", ObservedAt: time.Now()},
		{ExternalID: "inv-4", AgentID: uuid.New(), Provider: "openai", Cost: "1", Unit: "USD", ObservedAt: time.Now()},
	}
	_, err = svc.ImportBilling(ctx, as(f, "alice"), bad)
	wantKind(t, err, registry.ErrNotFound)
	_, err = svc.ImportBilling(ctx, as(f, "otto"), lines[:1])
	wantKind(t, err, registry.ErrForbidden)
	_, err = svc.ImportBilling(ctx, as(f, "alice"), []finops.BillingLine{{ExternalID: "x", AgentID: a.Agent,
		Provider: "openai", Cost: "1.0000001", Unit: "USD", ObservedAt: time.Now()}})
	wantKind(t, err, registry.ErrInvalid)
	usage, err := svc.Usage(ctx, as(f, "audra"), finops.UsageFilter{From: time.Now().Add(-24 * time.Hour), To: time.Now().Add(time.Hour)})
	if err != nil || len(usage) != 2 {
		t.Fatalf("usage = %+v %v", usage, err)
	}

	leaf := f.ID(t, "alice", `INSERT INTO eacp.budget_accounts (tenant_id, name, unit, agent_id)
		VALUES (eacp.current_tenant_id(), 'buyer-usd', 'USD', $1) RETURNING id`, a.Agent)
	limit := "10"
	l, err := svc.SetSoftLimit(ctx, as(f, "alice"), leaf, &limit, "monthly plan")
	if err != nil || l.MonthlyLimit == nil || *l.MonthlyLimit != "10" || l.MonthToDate != "3.25" || *l.SetBy != f.P["alice"] {
		t.Fatalf("soft limit = %+v %v", l, err)
	}
	_, err = svc.SetSoftLimit(ctx, as(f, "otto"), leaf, &limit, "mine")
	wantKind(t, err, registry.ErrForbidden)
	l, err = svc.SetSoftLimit(ctx, as(f, "bob"), leaf, nil, "no longer tracked")
	if err != nil || l.MonthlyLimit != nil {
		t.Fatalf("cleared = %+v %v", l, err)
	}
}

func TestAlertsThroughTheServiceAndTheEvaluatorAcrossTenants(t *testing.T) {
	f := registrytest.New(t)
	g := f.ForTenant(t, pgtest.TenantB)
	svc := finops.New(f.App)
	ctx := context.Background()
	for _, fx := range []*registrytest.Fixture{f, g} {
		a := fx.ActiveAgent(t, "buyer")
		if _, err := svc.RecordSpans(ctx, fx.Tenant, a.Version, []finops.Span{chatSpan("mystery", 5, 5, time.Now())}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := svc.EvaluateAll(ctx); err != nil || n < 2 {
		t.Fatalf("evaluate all = %d %v", n, err)
	}
	if n, err := svc.EvaluateAll(ctx); err != nil || n != 0 {
		t.Fatalf("re-evaluate = %d %v", n, err)
	}
	open, err := svc.Alerts(ctx, as(f, "otto"), true, 0)
	if err != nil || len(open) != 1 || open[0].Kind != "unpriced_usage" {
		t.Fatalf("tenant A alerts = %+v %v", open, err)
	}
	_, err = svc.Acknowledge(ctx, as(f, "carol"), open[0].ID, "mine")
	wantKind(t, err, registry.ErrForbidden)
	x, err := svc.Acknowledge(ctx, as(f, "otto"), open[0].ID, "adding a price")
	if err != nil || x.AcknowledgedBy == nil || *x.AcknowledgedBy != f.P["otto"] || *x.AckReason != "adding a price" {
		t.Fatalf("ack = %+v %v", x, err)
	}
	_, err = svc.Acknowledge(ctx, as(f, "alice"), open[0].ID, "again")
	wantKind(t, err, registry.ErrConflict)
	// Tenant B cannot see or acknowledge tenant A's alert.
	_, err = svc.Acknowledge(ctx, as(g, "otto"), open[0].ID, "cross")
	wantKind(t, err, registry.ErrNotFound)
	if open, err = svc.Alerts(ctx, as(f, "otto"), true, 0); err != nil || len(open) != 0 {
		t.Fatalf("open after ack = %+v %v", open, err)
	}
	if all, err := svc.Alerts(ctx, as(g, "otto"), false, 0); err != nil || len(all) != 1 || all[0].AcknowledgedAt != nil {
		t.Fatalf("tenant B alerts = %+v %v", all, err)
	}
}

// spend records one priced chat span (1 USD per million input tokens).
func spendAt(t *testing.T, f *registrytest.Fixture, version uuid.UUID, dollars int64, at time.Time) {
	t.Helper()
	in, err := finops.New(f.App).RecordSpans(context.Background(), f.Tenant, version,
		[]finops.Span{chatSpan("gpt-x", dollars*1000000, 0, at)})
	if err != nil || in.Accepted != 1 {
		t.Fatalf("spend = %+v %v", in, err)
	}
}

func bill(t *testing.T, f *registrytest.Fixture, agent uuid.UUID, id, cost string, at time.Time) {
	t.Helper()
	if _, err := finops.New(f.App).ImportBilling(context.Background(), as(f, "alice"), []finops.BillingLine{
		{ExternalID: id, AgentID: agent, Provider: "openai", Model: "gpt-x", Cost: cost, Unit: "USD", ObservedAt: at}}); err != nil {
		t.Fatal(err)
	}
}

func find(t *testing.T, c finops.Chargeback, kind, name string) finops.Group {
	t.Helper()
	for _, g := range c.Groups {
		if g.Kind == kind && g.Name == name {
			return g
		}
	}
	t.Fatalf("no %s group %q in %+v", kind, name, c.Groups)
	return finops.Group{}
}

func spendIn(g finops.Group, unit string) finops.UnitSpend {
	for _, s := range g.Spend {
		if s.Unit == unit {
			return s
		}
	}
	return finops.UnitSpend{}
}

// chargebackWorld: buyer (owned by carol) has a USD leaf under team under
// org, a funded THB leaf and an active 1 000 000 THB reservation through
// connector erp; reporter (owned by carol) has no leaf; analyst is owned by
// group payments and has only unpriced and agent-level usage.
type chargebackWorld struct {
	f                        *registrytest.Fixture
	buyer, reporter, analyst registrytest.Agent
	org, team, leaf, thb     uuid.UUID
	from, to                 time.Time
}

func newChargebackWorld(t *testing.T) chargebackWorld {
	t.Helper()
	f := registrytest.New(t)
	tool := f.ActiveToolWith(t, "erp", "purchase", registrytest.CostedContractSQL)
	w := chargebackWorld{f: f}
	w.buyer = f.ActiveAgent(t, "buyer", tool.Tool)
	w.reporter = f.ActiveAgent(t, "reporter")
	group := f.ID(t, "alice", `INSERT INTO eacp.groups (tenant_id, name, display_name)
		VALUES (eacp.current_tenant_id(), 'payments', 'Payments') RETURNING id`)
	analyst := f.ID(t, "erin", `INSERT INTO eacp.agents (tenant_id, name, display_name, environment, risk_class, owner_group_id)
		VALUES (eacp.current_tenant_id(), 'analyst', 'analyst', 'production', 'low', $1) RETURNING id`, group)
	w.analyst = registrytest.Agent{Agent: analyst, Version: f.ID(t, "erin", `INSERT INTO eacp.agent_versions
		(tenant_id, agent_id, runtime, code_ref) VALUES (eacp.current_tenant_id(), $1, 'python', 'git:abc') RETURNING id`, analyst)}
	addPrice(t, f, "gpt-x", "USD", "1", "", "0")

	account := `INSERT INTO eacp.budget_accounts (tenant_id, name, unit, parent_id, agent_id)
		VALUES (eacp.current_tenant_id(), $1, 'USD', $2, $3) RETURNING id`
	w.org = f.ID(t, "alice", account, "org", nil, nil)
	w.team = f.ID(t, "alice", account, "team", w.org, nil)
	w.leaf = f.ID(t, "alice", account, "buyer-usd", w.team, w.buyer.Agent)
	w.thb = f.FundAgent(t, w.buyer.Agent, "THB", "5000000")

	now := time.Now().UTC()
	today := now.Truncate(24 * time.Hour)
	yesterday := today.Add(-12 * time.Hour)
	// Just after midnight UTC, so "today" never slips into yesterday.
	early := today.Add(time.Second)
	// Today: reported 3, billed 5. Yesterday: reported 4, billed 1.
	// Effective = max per day = 5 + 4 = 9.
	spendAt(t, f, w.buyer.Version, 3, early)
	bill(t, f, w.buyer.Agent, "inv-today", "5", early)
	spendAt(t, f, w.buyer.Version, 4, yesterday)
	bill(t, f, w.buyer.Agent, "inv-yesterday", "1", yesterday)
	spendAt(t, f, w.reporter.Version, 2, early)
	svc := finops.New(f.App)
	if _, err := svc.RecordSpans(context.Background(), f.Tenant, w.analyst.Version, []finops.Span{
		chatSpan("mystery", 70, 30, early),
		{TraceID: "0123456789abcdef0123456789abcdef", SpanID: "0123456789abcdef", Operation: "invoke_agent",
			Provider: "openai", Model: "gpt-x", InputTokens: 1000000, ObservedAt: early},
	}); err != nil {
		t.Fatal(err)
	}

	// Two reservations: one stays ACTIVE (held tool spend); the other is
	// released by cancelling its action and counts for nothing.
	f.ActivatePolicy(t, registrytest.AllowPolicy)
	ctx := context.Background()
	var queued []uuid.UUID
	for range 2 {
		id := f.ReceivedAction(t, w.buyer.Version, "carol", "erp.purchase")
		ev := f.AgentID(t, w.buyer.Version, registrytest.AllowEvidenceSQL, id)
		ok(t, f.ExecAgent(w.buyer.Version, `UPDATE eacp.actions SET state = 'AUTHORIZED', state_reason = 'ok',
			decision_evidence_id = $2, enforced_payload = $3 WHERE id = $1`, id, ev, registrytest.ActionPayload))
		ok(t, storage.InTenantTx(ctx, f.App, f.Tenant.String(), func(tx pgx.Tx) error {
			if err := storage.SetAgent(ctx, tx, w.buyer.Version); err != nil {
				return err
			}
			var status string
			if err := tx.QueryRow(ctx, `SELECT eacp.budget_reserve($1, $2)`, id, tool.Contract).Scan(&status); err != nil {
				return err
			}
			if status != "reserved" {
				t.Errorf("reserve = %s", status)
			}
			var rev uuid.UUID
			if err := tx.QueryRow(ctx, registrytest.ReleaseEvidenceSQL, id, "allow").Scan(&rev); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, registrytest.QueueSQL, id, rev)
			return err
		}))
		queued = append(queued, id)
	}
	ok(t, f.ExecAgent(w.buyer.Version, `UPDATE eacp.actions SET state = 'CANCELLED', state_reason = 'not needed'
		WHERE id = $1`, queued[1]))
	var released string
	ownerRow(t, f, `SELECT state FROM eacp.budget_reservations WHERE action_id = $1`, []any{queued[1]}, &released)
	if released != "RELEASED" {
		t.Fatalf("cancelled action's reservation = %s", released)
	}
	w.from, w.to = today.Add(-48*time.Hour), now.Add(time.Hour)
	return w
}

func TestChargebackByAgentTakesTheGreaterSourcePerDay(t *testing.T) {
	w := newChargebackWorld(t)
	c, err := finops.New(w.f.App).Chargeback(context.Background(), as(w.f, "audra"), w.from, w.to, finops.ByAgent)
	if err != nil {
		t.Fatal(err)
	}
	buyer := find(t, c, "agent", "buyer")
	usd := spendIn(buyer, "USD")
	if usd.LLMReported != "7" || usd.LLMBilled != "6" || usd.LLMEffective != "9" || usd.Total != "9" {
		t.Fatalf("buyer USD = %+v", usd)
	}
	thb := spendIn(buyer, "THB")
	if thb.ToolHeld != "1000000" || thb.ToolCommitted != "0" || thb.Total != "1000000" {
		t.Fatalf("buyer THB = %+v", thb)
	}
	if len(buyer.Connectors) != 1 || buyer.Connectors[0].Unit != "THB" || buyer.Connectors[0].Held != "1000000" {
		t.Fatalf("buyer connectors = %+v", buyer.Connectors)
	}
	if buyer.Tokens != 7000000 || buyer.UnpricedTokens != 0 {
		t.Fatalf("buyer tokens = %d/%d", buyer.Tokens, buyer.UnpricedTokens)
	}
	if len(buyer.Models) != 1 || buyer.Models[0].Reported != "7" || buyer.Models[0].Billed != "6" {
		t.Fatalf("buyer models = %+v", buyer.Models)
	}
	analyst := find(t, c, "agent", "analyst")
	if len(analyst.Spend) != 0 || analyst.Tokens != 100 || analyst.UnpricedTokens != 100 || analyst.UnbilledTokens != 1000000 {
		t.Fatalf("analyst = %+v", analyst)
	}
	if len(analyst.Models) != 1 || analyst.Models[0].Unit != "" || analyst.Models[0].Model != "mystery" {
		t.Fatalf("analyst models = %+v", analyst.Models)
	}
}

func TestChargebackByTeamAndByAccountRollUp(t *testing.T) {
	w := newChargebackWorld(t)
	svc := finops.New(w.f.App)
	ctx := context.Background()
	team, err := svc.Chargeback(ctx, as(w.f, "otto"), w.from, w.to, finops.ByTeam)
	if err != nil {
		t.Fatal(err)
	}
	carol := find(t, team, "principal", "carol")
	if s := spendIn(carol, "USD"); s.LLMEffective != "11" {
		t.Fatalf("carol USD = %+v", s)
	}
	if payments := find(t, team, "group", "payments"); payments.UnpricedTokens != 100 {
		t.Fatalf("payments = %+v", payments)
	}

	acct, err := svc.Chargeback(ctx, as(w.f, "alice"), w.from, w.to, finops.ByAccount)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"org", "team", "buyer-usd"} {
		g := find(t, acct, "account", name)
		if s := spendIn(g, "USD"); s.Total != "9" || len(g.Spend) != 1 {
			t.Fatalf("%s = %+v", name, g.Spend)
		}
	}
	if s := spendIn(find(t, acct, "account", "agent-"+w.buyer.Agent.String()+"-thb"), "THB"); s.ToolHeld != "1000000" {
		t.Fatalf("THB leaf = %+v", s)
	}
	un := find(t, acct, "unassigned", "unassigned")
	if s := spendIn(un, "USD"); s.Total != "2" || un.UnpricedTokens != 100 || un.Tokens != 2000100 {
		t.Fatalf("unassigned = %+v", un)
	}

	_, err = svc.Chargeback(ctx, as(w.f, "alice"), w.to, w.from, finops.ByAgent)
	wantKind(t, err, registry.ErrInvalid)
	_, err = svc.Chargeback(ctx, as(w.f, "alice"), w.from, w.to, "tenant")
	wantKind(t, err, registry.ErrInvalid)
	_, err = svc.Chargeback(ctx, as(w.f, "alice"), w.to.Add(-500*24*time.Hour), w.to, finops.ByAgent)
	wantKind(t, err, registry.ErrInvalid)

	// Tenant isolation: another tenant sees none of it.
	g := w.f.ForTenant(t, pgtest.TenantB)
	other, err := svc.Chargeback(ctx, as(g, "alice"), w.from, w.to, finops.ByAgent)
	if err != nil || len(other.Groups) != 0 {
		t.Fatalf("tenant B chargeback = %+v %v", other, err)
	}
}

func TestDashboard(t *testing.T) {
	w := newChargebackWorld(t)
	f := w.f
	// A budget denial today.
	id := f.ReceivedAction(t, w.reporter.Version, "carol", "erp.purchase")
	ok(t, f.ExecAgent(w.reporter.Version, `UPDATE eacp.actions SET state = 'DENIED', state_reason = 'budget_account_missing'
		WHERE id = $1`, id))
	evaluate(t, f)
	d, err := finops.New(f.App).Dashboard(context.Background(), as(f, "otto"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(d)
	var usd, thb *finops.UnitDashboard
	for i := range d.Units {
		switch d.Units[i].Unit {
		case "USD":
			usd = &d.Units[i]
		case "THB":
			thb = &d.Units[i]
		}
	}
	if usd == nil || thb == nil {
		t.Fatalf("dashboard = %s", b)
	}
	// Month to date depends on whether yesterday is in this month.
	wantMTD := "11"
	if time.Now().UTC().Day() == 1 {
		wantMTD = "7"
	}
	if usd.Today != "7" || usd.MonthToDate != json.Number(wantMTD) || len(usd.TopAgents) != 2 || usd.TopAgents[0].Name != "buyer" {
		t.Fatalf("USD = %s", b)
	}
	if thb.Today != "1000000" || len(thb.TopAgents) != 1 {
		t.Fatalf("THB = %s", b)
	}
	if d.HardBlocksToday["budget_account_missing"] != 1 || d.OpenAlerts["unpriced_usage"] != 1 || d.UnpricedTokensToday != 100 {
		t.Fatalf("dashboard = %s", b)
	}
}
