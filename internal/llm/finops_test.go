package llm_test

import (
	"context"
	"testing"
	"time"

	"eacp/internal/finops"
	"eacp/internal/registry"
)

func TestGatewayUsageIsInChargeback(t *testing.T) {
	e := newEnv(t)
	a := e.admit(t, req("sonnet", "anthropic", 1000, 100))
	e.settle(t, a.CallID, "succeeded", 200, 100, 0, 0, 50, true)
	cb, err := finops.New(e.f.App).Chargeback(context.Background(),
		registry.Actor{TenantID: e.f.Tenant, PrincipalID: e.f.P["alice"]},
		time.Now().Add(-time.Hour), time.Now().Add(time.Hour), finops.ByAgent)
	must(t, err)
	for _, g := range cb.Groups {
		if g.ID == nil || *g.ID != e.agent.Agent {
			continue
		}
		if g.Tokens != 150 || len(g.Models) != 1 || g.Models[0].Model != "claude-x" ||
			g.Models[0].Reported != "0.00105" || g.Models[0].Tokens != 150 {
			t.Fatalf("group %+v", g)
		}
		return
	}
	t.Fatalf("no group for the agent: %+v", cb.Groups)
}
