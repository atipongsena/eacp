package worker_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/registry/registrytest"
	"github.com/atipongsena/eacp/internal/storage"
	"github.com/atipongsena/eacp/internal/storage/pgtest"
)

// retainedContractSQL is a read-only contract for tool $1 that keeps a
// success's output for five minutes (ADR-034).
const retainedContractSQL = `INSERT INTO eacp.tool_contracts
	(tenant_id, tool_id, side_effects, idempotency_mode, reconciliation_lookup,
	 reconciliation_consistency, proof_standard, max_attempts, result_retention_seconds)
	VALUES (eacp.current_tenant_id(), $1, '{READ_ONLY}', 'none', 'none', 'none', 'none', 3, 300)
	RETURNING id`

const recordSQL = `SELECT eacp.action_result_record($1, $2, $3)`

// resultSchema is a schema whose agent may call hr.balance, a tool whose
// contract keeps its output, besides erp.lookup, whose contract keeps none.
type resultSchema struct {
	schema
	keep  registrytest.Tooling
	other registrytest.Agent
}

func newResultSchema(t *testing.T) resultSchema {
	t.Helper()
	f := registrytest.New(t)
	s := resultSchema{schema: schema{f: f}}
	s.read = f.ActiveTool(t, "erp", "lookup")
	s.keep = f.ActiveToolWith(t, "hr", "balance", retainedContractSQL)
	s.agent = f.ActiveAgent(t, "reader", s.read.Tool, s.keep.Tool)
	s.other = f.ActiveAgent(t, "snoop", s.keep.Tool)
	s.policy = f.ActivatePolicy(t, registrytest.AllowPolicy)
	return s
}

// succeed completes w1's generation 1 attempt of id as a success, records
// output (or withheld) and moves the action to SUCCEEDED, in one
// transaction as the worker does. It reports what the record returned.
func (s resultSchema) succeed(id uuid.UUID, output, withheld any) (bool, error) {
	var kept bool
	err := s.inWorker("w1", 1, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM eacp.actions WHERE id = $1 FOR UPDATE`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, completeSQL, id, 1, "succeeded", "ref-1", ""); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, recordSQL, id, output, withheld).Scan(&kept); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, finishSQL, id, "SUCCEEDED", "succeeded", nil)
		return err
	})
	return kept, err
}

// readAs reads action id's result as agent version v.
func (s resultSchema) readAs(v, id uuid.UUID) (output string, found bool, err error) {
	ctx := context.Background()
	err = storage.InTenantTx(ctx, s.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		if err := storage.SetAgent(ctx, tx, v); err != nil {
			return err
		}
		var out *string
		err := tx.QueryRow(ctx, `SELECT output FROM eacp.action_result($1)`, id).Scan(&out)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err == nil {
			found = true
			if out != nil {
				output = *out
			}
		}
		return err
	})
	return output, found, err
}

// expireResult moves a result's expiry into the past as the schema owner.
func (s resultSchema) expireResult(t *testing.T, id uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	must(t, storage.InTenantTx(ctx, s.f.Owner, pgtest.TenantA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE eacp.action_results SET expires_at = now() - interval '1 second'
			WHERE action_id = $1`, id)
		return err
	}))
}

func TestResultRetentionIsBoundedAndImmutable(t *testing.T) {
	s := newResultSchema(t)
	insert := `INSERT INTO eacp.tool_contracts
		(tenant_id, tool_id, side_effects, idempotency_mode, reconciliation_lookup,
		 reconciliation_consistency, proof_standard, max_attempts, result_retention_seconds)
		VALUES (eacp.current_tenant_id(), $1, '{READ_ONLY}', 'none', 'none', 'none', 'none', 3, $2)`
	wantCode(t, s.f.Exec("erin", insert, s.keep.Tool, 59), "23514")
	wantCode(t, s.f.Exec("erin", insert, s.keep.Tool, 86401), "23514")
	must(t, s.f.Exec("erin", insert, s.keep.Tool, 60))
	must(t, s.f.Exec("erin", insert, s.keep.Tool, 86400))
	wantCode(t, s.f.Exec("rita", `UPDATE eacp.tool_contracts SET result_retention_seconds = 600 WHERE id = $1`,
		s.keep.Contract), "42501")
}

func TestResultContentIsNotSelectable(t *testing.T) {
	s := newResultSchema(t)
	id := s.executing(t, "hr.balance")
	if _, err := s.succeed(id, `{"days":12}`, nil); err != nil {
		t.Fatal(err)
	}
	if n := s.count(t, `SELECT count(*) FROM eacp.action_results WHERE action_id = $1 AND bytes = 11`, id); n != 1 {
		t.Fatalf("metadata rows = %d", n)
	}
	wantCode(t, s.f.Exec("alice", `SELECT output FROM eacp.action_results`), "42501")
	wantCode(t, s.f.ExecWorker("w1", 1, `SELECT output FROM eacp.action_results`), "42501")
	wantCode(t, s.f.ExecAgent(s.agent.Version, `SELECT * FROM eacp.action_results`), "42501")
}

func TestResultsCannotBeWrittenDirectly(t *testing.T) {
	s := newResultSchema(t)
	id := s.executing(t, "hr.balance")
	if _, err := s.succeed(id, `{"days":12}`, nil); err != nil {
		t.Fatal(err)
	}
	other := s.executing(t, "hr.balance")
	wantCode(t, s.f.ExecWorker("w1", 1, `INSERT INTO eacp.action_results
		(tenant_id, action_id, agent_id, contract_id, lease_generation, output, expires_at)
		VALUES (eacp.current_tenant_id(), $1, $2, $3, 1, '{}', now() + interval '1 hour')`,
		other, s.agent.Agent, s.keep.Contract), "42501")
	wantCode(t, s.f.ExecWorker("w1", 1, `UPDATE eacp.action_results SET expires_at = now() + interval '1 day'`), "42501")
	wantCode(t, s.f.ExecSystem("sweeper", `DELETE FROM eacp.action_results`), "42501")
}

func TestOnlyTheLeaseHolderRecordsASuccess(t *testing.T) {
	s := newResultSchema(t)
	id := s.executing(t, "hr.balance")

	// No outcome yet: the attempt has not succeeded.
	wantCode(t, s.f.ExecWorker("w1", 1, recordSQL, id, `{}`, nil), "55000")
	// Another worker, a stale generation, the sweeper and a principal are refused.
	wantCode(t, s.f.ExecWorker("w2", 1, recordSQL, id, `{}`, nil), "42501")
	wantCode(t, s.f.ExecWorker("w1", 2, recordSQL, id, `{}`, nil), "42501")
	wantCode(t, s.f.ExecSystem("sweeper", recordSQL, id, `{}`, nil), "42501")
	wantCode(t, s.f.Exec("alice", recordSQL, id, `{}`, nil), "42501")

	// An ambiguous attempt keeps nothing.
	amb := s.executing(t, "hr.balance")
	wantCode(t, s.inWorker("w1", 1, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, completeSQL, amb, 1, "ambiguous", "", "timeout"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, recordSQL, amb, `{}`, nil)
		return err
	}), "55000")

	// Output or a withheld reason, never both or neither; bounded JSON only.
	_, err := s.succeed(id, `{}`, "too_large")
	wantCode(t, err, "22023")
	_, err = s.succeed(id, nil, nil)
	wantCode(t, err, "22023")
	_, err = s.succeed(id, `not json`, nil)
	wantCode(t, err, "23514")
	_, err = s.succeed(id, `"`+strings.Repeat("x", 65535)+`"`, nil)
	wantCode(t, err, "23514")
	_, err = s.succeed(id, nil, "because")
	wantCode(t, err, "23514")

	kept, err := s.succeed(id, `{"days":12}`, nil)
	if err != nil || !kept {
		t.Fatalf("record = %v, %v", kept, err)
	}
	// One result per action, and none once the action has left EXECUTING.
	wantCode(t, s.f.ExecWorker("w1", 1, recordSQL, id, `{}`, nil), "55000")
}

func TestOneResultPerAction(t *testing.T) {
	s := newResultSchema(t)
	id := s.executing(t, "hr.balance")
	wantCode(t, s.inWorker("w1", 1, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, completeSQL, id, 1, "succeeded", "ref-1", ""); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, recordSQL, id, `{}`, nil); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, recordSQL, id, `{}`, nil)
		return err
	}), "23505")
}

func TestNoRetentionStoresNothing(t *testing.T) {
	s := newResultSchema(t)
	id := s.executing(t, "erp.lookup")
	kept, err := s.succeed(id, `{"po":"PO-1"}`, nil)
	if err != nil || kept {
		t.Fatalf("record = %v, %v; want false, nil", kept, err)
	}
	if n := s.count(t, `SELECT count(*) FROM eacp.action_results`); n != 0 {
		t.Fatalf("rows = %d", n)
	}
	if s.state(t, id) != "SUCCEEDED" {
		t.Fatal("the success itself must commit")
	}
}

func TestRecordComputesTheDigest(t *testing.T) {
	s := newResultSchema(t)
	id := s.executing(t, "hr.balance")
	out := `{"days":12,"name":"Somchai"}`
	if _, err := s.succeed(id, out, nil); err != nil {
		t.Fatal(err)
	}
	var digest string
	var bytes int
	var agent, contract uuid.UUID
	var keep time.Duration
	ctx := context.Background()
	must(t, storage.InTenantTx(ctx, s.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT sha256, bytes, agent_id, contract_id, expires_at - created_at
			FROM eacp.action_results WHERE action_id = $1`, id).Scan(&digest, &bytes, &agent, &contract, &keep)
	}))
	sum := sha256.Sum256([]byte(out))
	if digest != hex.EncodeToString(sum[:]) || bytes != len(out) || agent != s.agent.Agent ||
		contract != s.keep.Contract || keep != 300*time.Second {
		t.Fatalf("row = %s %d %s %s %s", digest, bytes, agent, contract, keep)
	}

	// A withheld result has a reason and no digest.
	w := s.executing(t, "hr.balance")
	if _, err := s.succeed(w, nil, "contains_credential"); err != nil {
		t.Fatal(err)
	}
	if n := s.count(t, `SELECT count(*) FROM eacp.action_results
		WHERE action_id = $1 AND withheld = 'contains_credential' AND sha256 IS NULL AND bytes IS NULL`, w); n != 1 {
		t.Fatal("withheld row")
	}
}

func TestOnlyTheActionsAgentReadsTheResult(t *testing.T) {
	s := newResultSchema(t)
	id := s.executing(t, "hr.balance")
	if _, err := s.succeed(id, `{"days":12}`, nil); err != nil {
		t.Fatal(err)
	}
	out, found, err := s.readAs(s.agent.Version, id)
	if err != nil || !found || out != `{"days":12}` {
		t.Fatalf("own read = %q %v %v", out, found, err)
	}
	// Another agent of the tenant sees nothing.
	if _, found, err := s.readAs(s.other.Version, id); err != nil || found {
		t.Fatalf("other agent read = %v %v", found, err)
	}
	// No principal and no system actor reads content.
	wantCode(t, s.f.Exec("alice", `SELECT * FROM eacp.action_result($1)`, id), "42501")
	wantCode(t, s.f.ExecSystem("sweeper", `SELECT * FROM eacp.action_result($1)`, id), "42501")
	wantCode(t, s.f.ExecWorker("w1", 1, `SELECT * FROM eacp.action_result($1)`, id), "42501")

	// A suspended version reads nothing either.
	must(t, s.f.Exec("otto", `UPDATE eacp.agent_versions SET state = 'SUSPENDED', state_reason = 'test'
		WHERE id = $1`, s.agent.Version))
	_, _, err = s.readAs(s.agent.Version, id)
	wantCode(t, err, "42501")
	must(t, s.f.Exec("rita", `UPDATE eacp.agent_versions SET state = 'ACTIVE', state_reason = 'test'
		WHERE id = $1`, s.agent.Version))

	// After its expiry nothing is served, pruned or not.
	s.expireResult(t, id)
	if _, found, err := s.readAs(s.agent.Version, id); err != nil || found {
		t.Fatalf("expired read = %v %v", found, err)
	}
}

func TestPruneNeedsTheSweeper(t *testing.T) {
	s := newResultSchema(t)
	id := s.executing(t, "hr.balance")
	if _, err := s.succeed(id, `{"days":12}`, nil); err != nil {
		t.Fatal(err)
	}
	prune := `SELECT eacp.action_results_prune(10)`
	wantCode(t, s.f.Exec("alice", prune), "42501")
	wantCode(t, s.f.ExecWorker("w1", 1, prune), "42501")
	wantCode(t, s.f.ExecAgent(s.agent.Version, prune), "42501")

	pruned := func() int {
		t.Helper()
		var n int
		ctx := context.Background()
		must(t, storage.InTenantTx(ctx, s.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
			if err := storage.SetSystem(ctx, tx, "sweeper"); err != nil {
				return err
			}
			return tx.QueryRow(ctx, prune).Scan(&n)
		}))
		return n
	}
	if n := pruned(); n != 0 {
		t.Fatalf("pruned %d before expiry", n)
	}
	var tenants int
	must(t, s.f.App.QueryRow(context.Background(),
		`SELECT count(*) FROM eacp.action_result_tenants() t WHERE t = $1`, s.f.Tenant).Scan(&tenants))
	if tenants != 0 {
		t.Fatal("tenant listed before expiry")
	}
	s.expireResult(t, id)
	must(t, s.f.App.QueryRow(context.Background(),
		`SELECT count(*) FROM eacp.action_result_tenants() t WHERE t = $1`, s.f.Tenant).Scan(&tenants))
	if tenants != 1 {
		t.Fatal("tenant not listed after expiry")
	}
	if n := pruned(); n != 1 {
		t.Fatalf("pruned %d, want 1", n)
	}
	if n := pruned(); n != 0 {
		t.Fatalf("pruned again: %d", n)
	}
	// The metadata stays as evidence; the content is gone for good.
	if n := s.count(t, `SELECT count(*) FROM eacp.action_results
		WHERE action_id = $1 AND pruned_at IS NOT NULL AND sha256 IS NOT NULL`, id); n != 1 {
		t.Fatal("metadata row")
	}
	ctx := context.Background()
	var content *string
	must(t, storage.InTenantTx(ctx, s.f.Owner, pgtest.TenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT output FROM eacp.action_results WHERE action_id = $1`, id).Scan(&content)
	}))
	if content != nil {
		t.Fatal("content kept after pruning")
	}
}
