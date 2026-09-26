package action_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/action"
)

// raceSweepers runs two replicas' sweepers over ids so that both select the
// actions before either moves them: a blocker holds the rows until both
// sweepers wait on the first one. It returns each sweeper's counters.
func raceSweepers(t *testing.T, v env, ids []uuid.UUID) []action.Stats {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, v.f.DB.AdminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	blocker, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if _, err := blocker.Exec(ctx, `SELECT id FROM eacp.actions WHERE id = ANY($1) FOR UPDATE`, ids); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stats := make([]action.Stats, 2)
	for i := range stats {
		wg.Go(func() {
			s := action.NewSweeper(v.e)
			s.Grace = 0
			st, err := s.RunOnce(ctx)
			if err != nil {
				t.Error(err)
			}
			stats[i] = st
		})
	}
	// Only a superuser sees other roles' waits; the blocker's own connection
	// would see a cached snapshot inside its transaction.
	watcher, err := pgx.Connect(ctx, v.f.DB.AdminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close(ctx)
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := watcher.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= 2 {
			break
		}
		if time.Now().After(deadline) {
			_ = blocker.Rollback(ctx)
			wg.Wait()
			t.Fatalf("the sweepers never both waited (%d waiting)", waiting)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if logs := v.logged(); strings.Contains(logs, "sweep failed") {
		t.Fatalf("a sweeper failed:\n%s", logs)
	}
	return stats
}

// Two replicas' sweepers select the same overdue actions before either
// moves them. Each action expires once, and the counters add up to the
// actions, not to twice as many.
func TestTwoSweepersMoveEachActionOnceAndCountIt(t *testing.T) {
	v := newEnv(t, allowAll)
	ctx := context.Background()
	var ids []uuid.UUID
	for _, key := range []string{"a", "b", "c"} {
		s := v.submission(key)
		s.Lifetime = time.Second
		got, err := v.e.Submit(ctx, v.actor(), s)
		if err != nil || got.State != "QUEUED" {
			t.Fatalf("submit %s = %+v, %v", key, got, err)
		}
		ids = append(ids, got.ID)
	}
	time.Sleep(1100 * time.Millisecond)
	stats := raceSweepers(t, v, ids)
	for _, id := range ids {
		if st := v.get(id).State; st != "EXPIRED" {
			t.Errorf("%s = %s, want EXPIRED", id, st)
		}
	}
	if sum := stats[0].Expired + stats[1].Expired; sum != len(ids) {
		t.Fatalf("expired counters = %d + %d, want %d in total", stats[0].Expired, stats[1].Expired, len(ids))
	}
}

// The second sweeper finds a lapsed lease already reclaimed: the move ended
// the lease (leased_until is NULL), and the re-check must see "nothing to
// do", not fail the tenant's pass.
func TestTwoSweepersReclaimEachLapsedLeaseOnce(t *testing.T) {
	v := newEnv(t, allowAll)
	var ids []uuid.UUID
	for _, key := range []string{"a", "b"} {
		got := v.mustSubmit(key, "QUEUED")
		v.claim(got.ID, "w-"+key, 50*time.Millisecond)
		ids = append(ids, got.ID)
	}
	time.Sleep(200 * time.Millisecond)
	stats := raceSweepers(t, v, ids)
	for _, id := range ids {
		if st := v.get(id).State; st != "QUEUED" {
			t.Errorf("%s = %s, want QUEUED", id, st)
		}
	}
	if sum := stats[0].Reclaimed + stats[1].Reclaimed; sum != len(ids) {
		t.Fatalf("reclaimed counters = %d + %d, want %d in total", stats[0].Reclaimed, stats[1].Reclaimed, len(ids))
	}
}

// The same for a due retry: re-queueing it clears next_attempt_at.
func TestTwoSweepersRetryEachActionOnce(t *testing.T) {
	v := newEnv(t, allowAll)
	var ids []uuid.UUID
	for _, key := range []string{"a", "b"} {
		got := v.mustSubmit(key, "QUEUED")
		s, l := v.dispatch(got.ID, "w-"+key)
		if st := v.complete(s, l, ambiguous); st != "RETRY_WAIT" {
			t.Fatalf("%s: ambiguous read = %s", key, st)
		}
		ids = append(ids, got.ID)
	}
	time.Sleep(200 * time.Millisecond)
	stats := raceSweepers(t, v, ids)
	for _, id := range ids {
		if st := v.get(id).State; st != "QUEUED" {
			t.Errorf("%s = %s, want QUEUED", id, st)
		}
	}
	if sum := stats[0].Retried + stats[1].Retried; sum != len(ids) {
		t.Fatalf("retried counters = %d + %d, want %d in total", stats[0].Retried, stats[1].Retried, len(ids))
	}
}
