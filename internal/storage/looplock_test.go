package storage_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/storage"
)

// beginIn opens a transaction in tenant's context and returns it; the test
// ends it.
func beginIn(t *testing.T, pool interface {
	Begin(context.Context) (pgx.Tx, error)
}, tenant string) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	if tenant != "" {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
			t.Fatal(err)
		}
	}
	return tx
}

func tryLock(t *testing.T, tx pgx.Tx, loop string) bool {
	t.Helper()
	got, err := storage.TryLoopLock(context.Background(), tx, loop)
	if err != nil {
		t.Fatalf("TryLoopLock(%q) = %v", loop, err)
	}
	return got
}

func TestTryLoopLockIsPerLoopAndTenant(t *testing.T) {
	db := migratedDB(t)
	pool := appPool(t, db.AppDSN, 8)
	ctx := context.Background()
	holder := beginIn(t, pool, tenantA)
	if !tryLock(t, holder, "incident") {
		t.Fatal("first lock refused")
	}
	other := beginIn(t, pool, tenantA)
	if tryLock(t, other, "incident") {
		t.Fatal("the same loop and tenant was locked twice")
	}
	if !tryLock(t, other, "finops") {
		t.Fatal("another loop of the same tenant was refused")
	}
	if !tryLock(t, beginIn(t, pool, tenantB), "incident") {
		t.Fatal("the same loop of another tenant was refused")
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if !tryLock(t, beginIn(t, pool, tenantA), "incident") {
		t.Fatal("the lock outlived its transaction")
	}
}

func TestTryLoopLockRejectsBadNames(t *testing.T) {
	db := migratedDB(t)
	tx := beginIn(t, appPool(t, db.AppDSN, 2), tenantA)
	for _, bad := range []string{"", "Incident", "a b", "1abc", "eacp.kill", "abcdefghijklmnopqrstuvwxyz0123456"} {
		if got, err := storage.TryLoopLock(context.Background(), tx, bad); err == nil || got {
			t.Errorf("TryLoopLock(%q) = %v, %v; want an error", bad, got, err)
		}
	}
}

func TestTryLoopLockNeedsATenant(t *testing.T) {
	db := migratedDB(t)
	tx := beginIn(t, appPool(t, db.AppDSN, 2), "")
	if got, err := storage.TryLoopLock(context.Background(), tx, "incident"); err == nil || got {
		t.Fatalf("TryLoopLock without a tenant = %v, %v; want an error", got, err)
	}
}
