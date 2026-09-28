package worker_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/storage"
	"github.com/atipongsena/eacp/internal/storage/pgtest"
)

// completeRemoteSQL completes attempt $2 of action $1 with a remote
// reference (ADR-030): the remote agent's task id, kept even when the
// outcome is not a success.
const completeRemoteSQL = `UPDATE eacp.action_attempts SET outcome = $3, error_class = NULLIF($4, ''),
	external_reference = NULLIF($5, ''), remote_reference = NULLIF($6, '')
	WHERE action_id = $1 AND lease_generation = $2`

func (s schema) finishRemote(worker string, id any, outcome, class, ref, remote, to string) error {
	return s.inWorker(worker, 1, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM eacp.actions WHERE id = $1 FOR UPDATE`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, completeRemoteSQL, id, 1, outcome, class, ref, remote); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, finishSQL, id, to, "result", nil)
		return err
	})
}

func TestRemoteReferenceRules(t *testing.T) {
	s := newSchema(t)
	id := s.executing(t, "ledger.post")

	// A success has an external reference and no remote one.
	wantCode(t, s.finishRemote("w1", id, "succeeded", "", "PO-1", "task-1", "SUCCEEDED"), "23514")
	wantCode(t, s.finishRemote("w1", id, "ambiguous", "a2a_failed", "", "task 1", "UNKNOWN_OUTCOME"), "23514")
	wantCode(t, s.finishRemote("w1", id, "ambiguous", "a2a_failed", "", strings.Repeat("t", 513), "UNKNOWN_OUTCOME"), "23514")
	wantCode(t, s.finishRemote("w2", id, "ambiguous", "a2a_failed", "", "task-1", "UNKNOWN_OUTCOME"), "42501")
	must(t, s.finishRemote("w1", id, "ambiguous", "a2a_failed", "", "task:1.a-b_C", "UNKNOWN_OUTCOME"))

	ctx := context.Background()
	var remote string
	must(t, storage.InTenantTx(ctx, s.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT remote_reference FROM eacp.action_attempts WHERE action_id = $1`, id).Scan(&remote)
	}))
	if remote != "task:1.a-b_C" {
		t.Fatalf("remote reference %q", remote)
	}
	// It is part of the completion and never changes afterwards.
	wantCode(t, s.f.ExecWorker("w1", 1, `UPDATE eacp.action_attempts SET remote_reference = 'task-2'
		WHERE action_id = $1`, id), "55000")

	// A certified no-effect result keeps the task that refused it.
	refused := s.executing(t, "bank.pay")
	must(t, s.finishRemote("w1", refused, "no_effect", "refused", "", "task-9", "FAILED"))
}
