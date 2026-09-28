package budget_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/budget"
	"github.com/atipongsena/eacp/internal/registry"
	"github.com/atipongsena/eacp/internal/registry/registrytest"
	"github.com/atipongsena/eacp/internal/storage"
)

func TestTxChangesLimitsInTheCallersTransaction(t *testing.T) {
	f := registrytest.New(t)
	ctx := context.Background()
	in := func(actor string, fn func(btx budget.Tx) error) error {
		return storage.InTenantTx(ctx, f.App, f.Tenant.String(), func(tx pgx.Tx) error {
			if err := storage.SetActor(ctx, tx, f.P[actor]); err != nil {
				return err
			}
			return fn(budget.Tx{Tx: tx})
		})
	}
	var acct uuid.UUID
	var raise budget.LimitChange
	if err := in("alice", func(btx budget.Tx) error {
		var err error
		if acct, err = btx.CreateAccount(ctx, budget.NewAccount{Name: "ops", Unit: "USD"}); err != nil {
			return err
		}
		raise, err = btx.ChangeLimit(ctx, acct, "100", "fund")
		return err
	}); err != nil || raise.State != "PROPOSED" {
		t.Fatalf("raise = %+v, %v", raise, err)
	}
	if err := in("alice", func(btx budget.Tx) error {
		_, err := btx.ChangeLimit(ctx, acct, "1e3", "bad")
		return err
	}); !errors.Is(err, registry.ErrInvalid) {
		t.Fatalf("malformed limit: %v", err)
	}
	var applied, lowered budget.LimitChange
	if err := in("bob", func(btx budget.Tx) error {
		var err error
		applied, err = btx.DecideLimitChange(ctx, raise.ID, true, "ok")
		return err
	}); err != nil || applied.State != "APPLIED" {
		t.Fatalf("apply = %+v, %v", applied, err)
	}
	if err := in("alice", func(btx budget.Tx) error {
		var err error
		lowered, err = btx.ChangeLimit(ctx, acct, "40", "trim")
		return err
	}); err != nil || lowered.State != "APPLIED" {
		t.Fatalf("decrease = %+v, %v", lowered, err)
	}
	got, err := budget.New(f.App).Get(ctx, registry.Actor{TenantID: f.Tenant, PrincipalID: f.P["alice"]}, acct)
	if err != nil || got.HardLimit != "40" {
		t.Fatalf("account = %+v, %v", got, err)
	}
}
