package studio_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/action"
	"github.com/atipongsena/eacp/internal/governance"
)

// TestTheSweeperExpiresStudioRuns: the action sweeper also fails Studio runs
// past their deadline and clears expired answers (Phase 27a-2), counting each once.
func TestTheSweeperExpiresStudioRuns(t *testing.T) {
	f := newFix(t)
	version, agent := f.approvedAgent("leave-bot")
	answered := f.finished(version, agent, "done")
	late := f.ID(t, "stella", startSQL, agent, inputs)
	f.owner(func(tx pgx.Tx) error {
		ctx := context.Background()
		if _, err := tx.Exec(ctx, `UPDATE eacp.studio_runs SET answer_expires_at = now() - interval '1 second' WHERE id = $1`, answered); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE eacp.studio_runs SET deadline = now() - interval '1 second' WHERE id = $1`, late)
		return err
	})
	e := action.New(f.App, action.Options{Provider: governance.LocalProvider{InstanceID: "t"},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	s := action.NewSweeper(e)
	st, err := s.RunOnce(context.Background())
	ok(t, err)
	if st.StudioExpired != 2 {
		t.Fatalf("studio expired = %d", st.StudioExpired)
	}
	if st, err = s.RunOnce(context.Background()); err != nil || st.StudioExpired != 0 {
		t.Fatalf("second pass = %d, %v", st.StudioExpired, err)
	}
	if got := f.scalar(`SELECT string_agg(concat_ws(' ', state, answer IS NULL), ', ' ORDER BY created_at)
		FROM eacp.studio_runs`); got != "SUCCEEDED t, FAILED t" {
		t.Fatalf("runs = %q", got)
	}
}
