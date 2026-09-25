package governance_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"eacp/internal/governance"
	"eacp/internal/registry"
	"eacp/internal/registry/registrytest"
	"eacp/internal/storage"
)

func TestTxCreatesAndActivatesInTheCallersTransaction(t *testing.T) {
	f := registrytest.New(t)
	ctx := context.Background()
	in := func(actor string, fn func(gtx governance.Tx) error) error {
		return storage.InTenantTx(ctx, f.App, f.Tenant.String(), func(tx pgx.Tx) error {
			if err := storage.SetActor(ctx, tx, f.P[actor]); err != nil {
				return err
			}
			return fn(governance.Tx{Tx: tx})
		})
	}
	err := in("alice", func(gtx governance.Tx) error {
		_, err := gtx.CreatePolicy(ctx, json.RawMessage(`{"rules":[]}`))
		return err
	})
	if !errors.Is(err, registry.ErrInvalid) {
		t.Fatalf("invalid policy: %v", err)
	}
	var p governance.Policy
	if err := in("alice", func(gtx governance.Tx) error {
		var err error
		p, err = gtx.CreatePolicy(ctx,
			json.RawMessage(`{"format_version":1,"rules":[{"id":"all","verdict":"allow","reason":"permitted"}]}`))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := in("bob", func(gtx governance.Tx) error { return gtx.ActivatePolicy(ctx, p.ID, "reviewed") }); err != nil {
		t.Fatal(err)
	}
	cur, err := governance.NewStore(f.App).CurrentPolicy(ctx, f.Tenant)
	if err != nil || cur.ID != p.ID || p.Version != 1 {
		t.Fatalf("current = %+v, %v (created %+v)", cur, err, p)
	}
}
