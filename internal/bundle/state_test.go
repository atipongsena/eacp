package bundle

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"eacp/internal/registry/registrytest"
	"eacp/internal/storage"
)

func TestLoadStateReadsPeopleGroupsPolicyBudgetsAndPrices(t *testing.T) {
	f := registrytest.New(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	g := f.ID(t, "alice", `INSERT INTO eacp.groups (tenant_id, name, display_name, schedule_weight)
		VALUES (eacp.current_tenant_id(), 'ops', 'Ops', 3) RETURNING id`)
	f.ID(t, "alice", `INSERT INTO eacp.group_memberships (tenant_id, group_id, principal_id)
		VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, g, f.P["carol"])
	pol := f.ID(t, "alice", `INSERT INTO eacp.policy_bundles (tenant_id, content)
		VALUES (eacp.current_tenant_id(), $1::jsonb) RETURNING id`, allowPolicy)
	must(f.Exec("bob", `UPDATE eacp.tenant_policy_pointer SET current_bundle_id = $1, activation_reason = 'go'
		WHERE tenant_id = eacp.current_tenant_id()`, pol))
	acct := f.ID(t, "alice", `INSERT INTO eacp.budget_accounts (tenant_id, name, unit)
		VALUES (eacp.current_tenant_id(), 'ops', 'USD') RETURNING id`)
	must(f.Exec("alice", `INSERT INTO eacp.budget_soft_limits (tenant_id, account_id, monthly_limit, reason)
		VALUES (eacp.current_tenant_id(), $1, 150, 'watch')`, acct))
	open := f.ID(t, "alice", `INSERT INTO eacp.budget_limit_changes (tenant_id, account_id, new_limit, reason)
		VALUES (eacp.current_tenant_id(), $1, 100, 'fund') RETURNING id`, acct)
	f.ID(t, "alice", `INSERT INTO eacp.model_prices (tenant_id, provider, model, unit, input_per_mtok, output_per_mtok, reason)
		VALUES (eacp.current_tenant_id(), 'openai', 'gpt-4.1', 'USD', 2.5, 10, 'card') RETURNING id`)

	var st State
	must(storage.InTenantSnapshotTx(ctx, f.App, f.Tenant.String(), func(tx pgx.Tx) error {
		var err error
		st, err = loadState(ctx, tx, "people")
		return err
	}))
	if st.TenantID != f.Tenant {
		t.Fatalf("tenant = %s", st.TenantID)
	}
	if g := st.People["alice"].Grants["admin"]; !g.Approved || st.People["ci"].Kind != "service" {
		t.Fatalf("people = %+v", st.People)
	}
	if ops := st.GroupRows["ops"]; ops.Weight != 3 || ops.Members["carol"] == [16]byte{} {
		t.Fatalf("groups = %+v", st.GroupRows)
	}
	if st.Policy.ID == nil || *st.Policy.ID != pol || !strings.Contains(string(st.Policy.Content), "permitted") {
		t.Fatalf("policy = %+v", st.Policy)
	}
	b := st.Budgets["ops"]
	if b.HardLimit != "0" || b.SoftLimit == nil || *b.SoftLimit != "150" || b.OpenProposal == nil || *b.OpenProposal != open {
		t.Fatalf("budget = %+v", b)
	}
	if p := st.Prices["openai gpt-4.1"]; p.Input != "2.5" || p.Output != "10" || p.Cached != nil || p.Unit != "USD" {
		t.Fatalf("prices = %+v", st.Prices)
	}
}
