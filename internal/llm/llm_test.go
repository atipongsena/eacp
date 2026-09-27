package llm_test

// Tests of the Go store over the LLM-call ledger (ADR-031).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/llm"
	"eacp/internal/registry"
	"eacp/internal/storage"
	"eacp/internal/storage/pgtest"
)

func decision() llm.Decision {
	return llm.Decision{ID: uuid.New(), BundleID: uuid.New(), Version: 1, Verdict: "allow"}
}

func (e *env) request(model, provider string, bytes, maxOut int64) llm.AdmitRequest {
	return llm.AdmitRequest{AgentVersionID: e.agent.Version, ModelName: model, Provider: provider, GatewayID: "gw-1",
		RequestBytes: bytes, MaxOutputTokens: maxOut, TraceID: "0af7651916cd43dd8448eb211c80319c", Decision: decision()}
}

// overdue moves call's deadline into the past as the schema owner.
func (e *env) overdue(t *testing.T, call uuid.UUID) {
	t.Helper()
	must(t, storage.InTenantTx(context.Background(), e.f.Owner, e.f.Tenant.String(), func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `ALTER TABLE eacp.llm_calls DISABLE TRIGGER USER;
			UPDATE eacp.llm_calls SET deadline = now() - interval '1 second' WHERE id = '`+call.String()+`';
			ALTER TABLE eacp.llm_calls ENABLE TRIGGER USER`)
		return err
	}))
}

func TestStoreAdmitSettleRoundTrip(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	s := llm.New(e.f.App)
	before := time.Now()
	a, err := s.Admit(ctx, e.f.Tenant, e.request("sonnet", "anthropic", 1000, 100))
	must(t, err)
	if a.Denial != "" || a.CallID == uuid.Nil || a.Model.ID != e.sonnet || a.Model.UpstreamModel != "claude-x" ||
		a.Model.BaseURL != "https://api.anthropic.test" || a.Model.SecretRef != "llm" ||
		a.Model.Timeout != 600*time.Second || a.Model.MaxOutputTokens != 100 {
		t.Fatalf("admission %+v", a)
	}
	if d := a.Deadline.Sub(before); d < 659*time.Second || d > 662*time.Second {
		t.Fatalf("deadline in %v", d)
	}
	// Without a requested cap the model's applies.
	b, err := s.Admit(ctx, e.f.Tenant, e.request("sonnet", "anthropic", 10, 0))
	must(t, err)
	if b.Model.MaxOutputTokens != 4096 {
		t.Fatalf("default cap %d", b.Model.MaxOutputTokens)
	}

	killed, err := s.Killed(ctx, e.f.Tenant, a.CallID)
	must(t, err)
	epoch, err := s.KillEpoch(ctx, e.f.Tenant)
	must(t, err)
	if killed || epoch != 0 {
		t.Fatalf("killed %v epoch %d", killed, epoch)
	}

	must(t, s.Settle(ctx, e.f.Tenant, a.CallID, llm.Settlement{Outcome: llm.OutcomeSucceeded, ProviderStatus: 200,
		Usage: llm.Usage{Input: 100, Output: 50, Known: true}}))
	c, err := s.Get(ctx, e.f.Tenant, a.CallID)
	must(t, err)
	if c.State != "SETTLED" || c.Outcome != llm.OutcomeSucceeded || *c.ProviderStatus != 200 || *c.InputTokens != 100 ||
		*c.OutputTokens != 50 || *c.CostAmount != "0.001050" || *c.CommittedAmount != "0.001050" ||
		c.CostUnit != "USD" || c.AgentID != e.agent.Agent || c.ModelName != "sonnet" ||
		c.TraceID != "0af7651916cd43dd8448eb211c80319c" || c.Verdict != "allow" {
		t.Fatalf("call %+v", c)
	}
	// A second settlement is refused.
	if err := s.Settle(ctx, e.f.Tenant, a.CallID, llm.Settlement{Outcome: llm.OutcomeKilled}); err == nil {
		t.Fatal("settled twice")
	}
	// A call whose outcome is not one the gateway may report is refused before PostgreSQL.
	if err := s.Settle(ctx, e.f.Tenant, b.CallID, llm.Settlement{Outcome: llm.OutcomeAbandoned}); err == nil {
		t.Fatal("the gateway abandoned a call")
	}

	must(t, e.f.Exec("otto", `SELECT eacp.set_kill('model', $1, true, 'provider incident')`, e.sonnet))
	killed, err = s.Killed(ctx, e.f.Tenant, b.CallID)
	must(t, err)
	epoch, err = s.KillEpoch(ctx, e.f.Tenant)
	must(t, err)
	if !killed || epoch < 1 {
		t.Fatalf("killed %v epoch %d", killed, epoch)
	}
}

func TestStoreDenialIsReturnedNotAnError(t *testing.T) {
	e := newEnv(t)
	s := llm.New(e.f.App)
	a, err := s.Admit(context.Background(), e.f.Tenant, e.request("gpt", "openai", 100, 10))
	must(t, err)
	if a.Denial != "model_not_in_allowlist" || a.CallID == uuid.Nil || a.Model.BaseURL != "" {
		t.Fatalf("admission %+v", a)
	}
	c, err := s.Get(context.Background(), e.f.Tenant, a.CallID)
	must(t, err)
	if c.State != "DENIED" || c.Denial != "model_not_in_allowlist" || c.Deadline != nil {
		t.Fatalf("call %+v", c)
	}
	// A PDP denial is recorded with the decision.
	r := e.request("sonnet", "anthropic", 100, 10)
	r.Decision.Verdict, r.Decision.Denial = "deny", "policy_denied"
	a, err = s.Admit(context.Background(), e.f.Tenant, r)
	must(t, err)
	if a.Denial != "policy_denied" {
		t.Fatalf("admission %+v", a)
	}
}

func TestStoreSweepAll(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	s := llm.New(e.f.App)
	a, err := s.Admit(ctx, e.f.Tenant, e.request("sonnet", "anthropic", 1000, 100))
	must(t, err)
	fresh, err := s.Admit(ctx, e.f.Tenant, e.request("sonnet", "anthropic", 1000, 100))
	must(t, err)
	e.overdue(t, a.CallID)
	n, err := s.SweepAll(ctx)
	must(t, err)
	if n != 1 {
		t.Fatalf("swept %d", n)
	}
	for id, want := range map[uuid.UUID]string{a.CallID: llm.OutcomeAbandoned, fresh.CallID: ""} {
		c, err := s.Get(ctx, e.f.Tenant, id)
		must(t, err)
		if c.Outcome != want {
			t.Fatalf("call %s outcome %q, want %q", id, c.Outcome, want)
		}
	}
	if n, err := s.SweepAll(ctx); err != nil || n != 0 {
		t.Fatalf("second sweep %d %v", n, err)
	}
}

func TestStoreListFiltersAndTenantIsolation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	s := llm.New(e.f.App)
	for _, r := range []llm.AdmitRequest{
		e.request("sonnet", "anthropic", 100, 10),
		e.request("sonnet", "anthropic", 100, 10),
		e.request("gpt", "openai", 100, 10),
	} {
		_, err := s.Admit(ctx, e.f.Tenant, r)
		must(t, err)
	}
	count := func(f llm.Filter) int {
		t.Helper()
		calls, err := s.List(ctx, e.f.Tenant, f)
		must(t, err)
		return len(calls)
	}
	if n := count(llm.Filter{}); n != 3 {
		t.Fatalf("all: %d", n)
	}
	if n := count(llm.Filter{State: "DENIED"}); n != 1 {
		t.Fatalf("denied: %d", n)
	}
	if n := count(llm.Filter{Model: "gpt"}); n != 1 {
		t.Fatalf("gpt: %d", n)
	}
	if n := count(llm.Filter{AgentID: e.agent.Agent}); n != 3 {
		t.Fatalf("agent: %d", n)
	}
	if n := count(llm.Filter{AgentID: uuid.New()}); n != 0 {
		t.Fatalf("other agent: %d", n)
	}
	if n := count(llm.Filter{From: time.Now().Add(time.Hour)}); n != 0 {
		t.Fatalf("future: %d", n)
	}
	if n := count(llm.Filter{To: time.Now().Add(-time.Hour)}); n != 0 {
		t.Fatalf("past: %d", n)
	}
	calls, err := s.List(ctx, e.f.Tenant, llm.Filter{Limit: 1})
	must(t, err)
	if len(calls) != 1 || calls[0].ModelName != "gpt" {
		t.Fatalf("newest first: %+v", calls)
	}
	if _, err := s.List(ctx, e.f.Tenant, llm.Filter{State: "BOGUS"}); err == nil {
		t.Fatal("unknown state accepted")
	}
	// Another tenant sees nothing.
	other, err := s.List(ctx, uuid.MustParse(pgtest.TenantB), llm.Filter{})
	must(t, err)
	if len(other) != 0 {
		t.Fatalf("tenant B sees %d calls", len(other))
	}
	_, err = s.Get(ctx, uuid.MustParse(pgtest.TenantB), calls[0].ID)
	var re *registry.Error
	if !errors.As(err, &re) || re.Kind != registry.ErrNotFound {
		t.Fatalf("cross-tenant get: %v", err)
	}
}
