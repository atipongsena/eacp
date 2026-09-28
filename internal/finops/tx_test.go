package finops_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/finops"
	"github.com/atipongsena/eacp/internal/registry"
	"github.com/atipongsena/eacp/internal/registry/registrytest"
	"github.com/atipongsena/eacp/internal/storage"
)

func TestTxAddsPricesAndSoftLimitsInTheCallersTransaction(t *testing.T) {
	f := registrytest.New(t)
	ctx := context.Background()
	acct := f.ID(t, "alice", `INSERT INTO eacp.budget_accounts (tenant_id, name, unit)
		VALUES (eacp.current_tenant_id(), 'ops', 'USD') RETURNING id`)
	in := func(fn func(ftx finops.Tx) error) error {
		return storage.InTenantTx(ctx, f.App, f.Tenant.String(), func(tx pgx.Tx) error {
			if err := storage.SetActor(ctx, tx, f.P["alice"]); err != nil {
				return err
			}
			return fn(finops.Tx{Tx: tx})
		})
	}
	if err := in(func(ftx finops.Tx) error {
		_, err := ftx.AddPrice(ctx, finops.NewPrice{Provider: "openai", Model: "gpt-4.1", Unit: "USD",
			InputPerMTok: "-1", OutputPerMTok: "10", Reason: "card"})
		return err
	}); !errors.Is(err, registry.ErrInvalid) {
		t.Fatalf("negative price: %v", err)
	}
	var p finops.Price
	limit := "150"
	if err := in(func(ftx finops.Tx) error {
		var err error
		if p, err = ftx.AddPrice(ctx, finops.NewPrice{Provider: "openai", Model: "gpt-4.1", Unit: "USD",
			InputPerMTok: "2.5", OutputPerMTok: "10", Reason: "card"}); err != nil {
			return err
		}
		return ftx.SetSoftLimit(ctx, acct, &limit, "watch")
	}); err != nil || p.InputPerMTok != "2.5" {
		t.Fatalf("price = %+v, %v", p, err)
	}
	alice := registry.Actor{TenantID: f.Tenant, PrincipalID: f.P["alice"]}
	limits, err := finops.New(f.App).SoftLimits(ctx, alice)
	if err != nil || len(limits) != 1 || limits[0].MonthlyLimit == nil || *limits[0].MonthlyLimit != "150" {
		t.Fatalf("soft limits = %+v, %v", limits, err)
	}
}
