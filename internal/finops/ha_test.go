package finops_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/finops"
	"github.com/atipongsena/eacp/internal/registry/registrytest"
	"github.com/atipongsena/eacp/internal/storage"
)

func unpricedUsage(t *testing.T, f *registrytest.Fixture) {
	t.Helper()
	a := f.ActiveAgent(t, "buyer")
	if _, err := finops.New(f.App).RecordSpans(context.Background(), f.Tenant, a.Version,
		[]finops.Span{chatSpan("mystery", 5, 5, time.Now())}); err != nil {
		t.Fatal(err)
	}
}

func alertCount(t *testing.T, f *registrytest.Fixture) int {
	t.Helper()
	var n int
	if err := storage.InTenantTx(context.Background(), f.Owner, f.Tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM eacp.finops_alerts`).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestFinOpsEvaluationSkipsATenantAnotherReplicaHolds(t *testing.T) {
	f := registrytest.New(t)
	unpricedUsage(t, f)
	s := finops.New(f.App)
	release := f.HoldLoopLock(t, "finops")
	if n, err := s.Evaluate(context.Background(), f.Tenant); err != nil || n != 0 {
		t.Fatalf("evaluate while held = %d, %v; want a skip", n, err)
	}
	if n := alertCount(t, f); n != 0 {
		t.Fatalf("a skipped evaluation raised %d alerts", n)
	}
	release()
	if n, err := s.Evaluate(context.Background(), f.Tenant); err != nil || n != 1 {
		t.Fatalf("evaluate after release = %d, %v", n, err)
	}
}

func TestConcurrentFinOpsEvaluationsActOnce(t *testing.T) {
	f := registrytest.New(t)
	unpricedUsage(t, f)
	s := finops.New(f.App)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if err := f.ExecSystem(finops.SystemActor, `SELECT eacp.finops_evaluate()`); err != nil {
				t.Error(err)
			}
		})
		wg.Go(func() {
			if _, err := s.Evaluate(context.Background(), f.Tenant); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if n := alertCount(t, f); n != 1 {
		t.Fatalf("alerts = %d, want 1", n)
	}
}
