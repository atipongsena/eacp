package registry_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/registry"
	"github.com/atipongsena/eacp/internal/registry/registrytest"
	"github.com/atipongsena/eacp/internal/storage"
)

// txAs runs fn as actor in one transaction of the fixture tenant.
func txAs(t *testing.T, f *registrytest.Fixture, actor string, fn func(ctx context.Context, rtx registry.Tx) error) {
	t.Helper()
	ctx := context.Background()
	if err := storage.InTenantTx(ctx, f.App, f.Tenant.String(), func(tx pgx.Tx) error {
		if err := storage.SetActor(ctx, tx, f.P[actor]); err != nil {
			return err
		}
		return fn(ctx, registry.Tx{Tx: tx})
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTxIdentityWritesRunInTheCallersTransaction(t *testing.T) {
	f := registrytest.New(t)
	var dana, grant, group, member uuid.UUID
	txAs(t, f, "alice", func(ctx context.Context, rtx registry.Tx) error {
		p, err := rtx.CreatePrincipal(ctx, registry.NewPrincipal{Kind: "human", Name: "dana",
			Subject: " Dana@Example.com ", DisplayName: "Dana"})
		if err != nil {
			return err
		}
		dana = p.ID
		if grant, err = rtx.ProposeRole(ctx, dana, "auditor"); err != nil {
			return err
		}
		if group, err = rtx.CreateGroup(ctx, "auditors", "Auditors", 3); err != nil {
			return err
		}
		member, err = rtx.AddMember(ctx, group, dana)
		return err
	})
	txAs(t, f, "bob", func(ctx context.Context, rtx registry.Tx) error {
		if err := rtx.ApproveRole(ctx, grant); err != nil {
			return err
		}
		if err := rtx.RemoveMember(ctx, member, "moved"); err != nil {
			return err
		}
		return rtx.RevokeRole(ctx, grant, "moved")
	})
	var n int
	if err := storage.InTenantTx(context.Background(), f.Owner, f.Tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM eacp.principals p
			JOIN eacp.role_grants g ON g.principal_id = p.id AND g.approved_by = $2 AND g.revoked_at IS NOT NULL
			JOIN eacp.group_memberships m ON m.principal_id = p.id AND m.removed_at IS NOT NULL
			JOIN eacp.groups gr ON gr.id = m.group_id AND gr.schedule_weight = 3
			WHERE p.id = $1 AND p.subject = 'dana@example.com'`, dana, f.P["bob"]).Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("identity writes = %d, %v", n, err)
	}
}
