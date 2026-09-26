package action_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/action"
)

// Two replicas' sweepers select the same overdue actions before either
// moves them (a blocker holds the rows). Each action expires once, and the
// counters add up to the actions, not to twice as many.
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

	admin, err := pgx.Connect(ctx, v.f.DB.AdminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	blocker, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
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
	// Both sweepers have selected the actions and wait on the first row. Only
	// a superuser sees other roles' waits; the blocker's own connection would
	// see a cached snapshot inside its transaction.
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
			t.Fatalf("the sweepers never both waited (%d waiting)", waiting)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	for _, id := range ids {
		if st := v.get(id).State; st != "EXPIRED" {
			t.Errorf("%s = %s, want EXPIRED", id, st)
		}
	}
	if sum := stats[0].Expired + stats[1].Expired; sum != len(ids) {
		t.Fatalf("expired counters = %d + %d, want %d in total", stats[0].Expired, stats[1].Expired, len(ids))
	}
}
