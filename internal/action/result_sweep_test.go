package action_test

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/action"
	"github.com/atipongsena/eacp/internal/storage"
)

// keepContractSQL is a read-only contract for tool $1 that keeps a
// success's output for five minutes (ADR-034).
const keepContractSQL = `INSERT INTO eacp.tool_contracts
	(tenant_id, tool_id, side_effects, idempotency_mode, reconciliation_lookup,
	 reconciliation_consistency, proof_standard, max_attempts, result_retention_seconds)
	VALUES (eacp.current_tenant_id(), $1, '{READ_ONLY}', 'none', 'none', 'none', 'none', 1, 300)
	RETURNING id`

// settle does what the worker does for queued action id: lease, intent and a
// success whose output is kept, as w1.
func (v env) settle(id uuid.UUID, output string) {
	v.t.Helper()
	ctx := context.Background()
	err := storage.InTenantTx(ctx, v.f.App, v.f.Tenant.String(), func(tx pgx.Tx) error {
		if err := storage.SetWorker(ctx, tx, "w1", 1); err != nil {
			return err
		}
		for _, sql := range []string{
			`UPDATE eacp.actions SET state = 'LEASED', lease_generation = lease_generation + 1, worker_id = 'w1',
				leased_until = now() + interval '1 minute' WHERE id = $1`,
			`UPDATE eacp.actions SET state = 'EXECUTING', attempt_count = attempt_count + 1,
				leased_until = now() + interval '1 minute' WHERE id = $1`,
			`UPDATE eacp.action_attempts SET outcome = 'succeeded', external_reference = 'R-1'
				WHERE action_id = $1 AND lease_generation = 1`,
		} {
			if _, err := tx.Exec(ctx, sql, id); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `SELECT eacp.action_result_record($1, $2, NULL)`, id, output); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE eacp.actions SET state = 'SUCCEEDED', state_reason = 'succeeded' WHERE id = $1`, id)
		return err
	})
	if err != nil {
		v.t.Fatal(err)
	}
}

// TestTheSweeperClearsExpiredResultsOnce (ADR-034, ADR-029): two sweepers
// racing clear each expired result's content once, keep its metadata, and
// leave unexpired results alone. Sweeping needs no open action.
func TestTheSweeperClearsExpiredResultsOnce(t *testing.T) {
	v := newEnvWith(t, allowAll, keepContractSQL)
	var expired, kept []uuid.UUID
	for i := range 4 {
		got := v.mustSubmit("k"+uuid.NewString(), "QUEUED")
		v.settle(got.ID, `{"n":1}`)
		if i < 3 {
			expired = append(expired, got.ID)
		} else {
			kept = append(kept, got.ID)
		}
	}
	ctx := context.Background()
	if err := storage.InTenantTx(ctx, v.f.Owner, v.f.Tenant.String(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE eacp.action_results SET expires_at = now() - interval '1 second'
			WHERE action_id = ANY($1)`, expired)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stats := make([]action.Stats, 2)
	for i := range stats {
		wg.Go(func() {
			s := action.NewSweeper(v.e)
			st, err := s.RunOnce(ctx)
			if err != nil {
				t.Error(err)
			}
			stats[i] = st
		})
	}
	wg.Wait()
	if n := stats[0].Pruned + stats[1].Pruned; n != len(expired) {
		t.Fatalf("pruned %d + %d, want %d", stats[0].Pruned, stats[1].Pruned, len(expired))
	}
	if n := v.count(`SELECT count(*) FROM eacp.action_results WHERE action_id = ANY($1)
		AND pruned_at IS NOT NULL AND sha256 IS NOT NULL`, expired); n != len(expired) {
		t.Fatalf("pruned rows with metadata = %d", n)
	}
	if n := v.count(`SELECT count(*) FROM eacp.action_results WHERE action_id = ANY($1) AND pruned_at IS NULL`, kept); n != 1 {
		t.Fatalf("unexpired rows = %d", n)
	}
	var content int
	if err := storage.InTenantTx(ctx, v.f.Owner, v.f.Tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM eacp.action_results WHERE output IS NOT NULL`).Scan(&content)
	}); err != nil {
		t.Fatal(err)
	}
	if content != 1 {
		t.Fatalf("rows with content = %d, want 1", content)
	}
	if st := v.sweep(); st.Pruned != 0 {
		t.Fatalf("second pass pruned %d", st.Pruned)
	}
}
