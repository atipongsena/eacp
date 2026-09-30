package studio_test

// Raw-SQL tests of Studio runs and runtime keys (Phase 27a-2, ADR-033):
// every rule is PostgreSQL's (migration 00028).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/registry/registrytest"
	"github.com/atipongsena/eacp/internal/storage"
)

const (
	startSQL     = `SELECT eacp.studio_run_start($1, $2)`
	heartbeatSQL = `SELECT eacp.studio_run_heartbeat($1, $2, $3, 30)`
	stepSQL      = `SELECT eacp.studio_run_step($1, $2, $3, $4, $5)`
	finishSQL    = `SELECT eacp.studio_run_finish($1, $2, $3, $4, $5, $6)`
	answerSQL    = `SELECT eacp.studio_run_answer($1)`
	proposeSQL   = `SELECT eacp.studio_credential_propose($1, $2, decode(repeat('cd', 32), 'hex'), $3)`
	revokeAllSQL = `SELECT eacp.studio_credentials_revoke_all($1)`
	inputs       = `{"employee_id": "E-1"}`
	answerCanary = "ANSWER-CANARY-77a1"
	inputCanary  = "INPUT-CANARY-9c2e"
)

// approvedAgent saves the example as stella and has rita approve it.
func (f *fix) approvedAgent(name string) (version, agent uuid.UUID) {
	f.t.Helper()
	version = f.mustSave("stella", nil, name, example)
	ok(f.t, f.decide("rita", version, true, "read-only tool"))
	return version, f.agentOf(version)
}

// claimed is one run the runtime claimed.
type claimed struct {
	ID           uuid.UUID         `json:"id"`
	VersionID    uuid.UUID         `json:"version_id"`
	Generation   int64             `json:"generation"`
	Subject      string            `json:"subject"`
	Inputs       map[string]string `json:"inputs"`
	Definition   json.RawMessage   `json:"definition"`
	Steps        []map[string]any  `json:"steps"`
	CredentialID *uuid.UUID        `json:"credential_id"`
	Credential   string            `json:"credential"`
}

// claim claims up to limit runs as the runtime principal rt, runtime id id.
func (f *fix) claim(id string, limit int) ([]claimed, error) {
	var out []claimed
	ctx := context.Background()
	err := storage.InTenantTx(ctx, f.App, f.Tenant.String(), func(tx pgx.Tx) error {
		if err := storage.SetActor(ctx, tx, f.P["rt"]); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT eacp.studio_run_claim($1, 'v1', 30, $2)`, id, limit)
		if err != nil {
			return err
		}
		raw, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
		if err != nil {
			return err
		}
		for _, r := range raw {
			var c claimed
			if err := json.Unmarshal(r, &c); err != nil {
				return err
			}
			out = append(out, c)
		}
		return nil
	})
	return out, err
}

func (f *fix) mustClaim(id string) claimed {
	f.t.Helper()
	got, err := f.claim(id, 10)
	ok(f.t, err)
	if len(got) != 1 {
		f.t.Fatalf("claimed %d runs", len(got))
	}
	return got[0]
}

// action submits the run's step index as the agent version, with key and subject.
func (f *fix) action(version uuid.UUID, key, subject string) uuid.UUID {
	f.t.Helper()
	return f.AgentID(f.t, version, registrytest.ReceivedActionSQL, version, key, subject, "hr-mcp.get_leave_balance")
}

func stepKey(run uuid.UUID, index int) string { return fmt.Sprintf("studio:%s:%d", run, index) }

const stella = "stella@tenant-a.test"

func TestARunIsStartedByADepartmentMemberWithItsInputs(t *testing.T) {
	f := newFix(t)
	_, agent := f.approvedAgent("leave-bot")
	run := f.ID(t, "abe", startSQL, agent, inputs)
	if got := f.scalar(`SELECT concat_ws(' ', state, requested_by = $2, inputs->>'employee_id',
			deadline BETWEEN now() + interval '299 seconds' AND now() + interval '301 seconds')
		FROM eacp.studio_runs WHERE id = $1`, run, f.P["abe"]); got != "QUEUED t E-1 t" {
		t.Fatalf("run = %q", got)
	}
	// sid (finance) and carol (no department) are not members of hr.
	for _, who := range []string{"sid", "carol", "rt"} {
		_, err := f.TryID(who, startSQL, agent, inputs)
		wantState(t, err, sqlForbidden)
	}
	for name, in := range map[string]string{
		"missing": `{}`, "extra": `{"employee_id": "E-1", "other": "x"}`, "number": `{"employee_id": 7}`,
		"too long": `{"employee_id": "` + strings.Repeat("x", 65) + `"}`, "array": `[]`,
	} {
		_, err := f.TryID("stella", startSQL, agent, in)
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		wantState(t, err, sqlCheck)
	}
	// Not approved, or no longer ACTIVE.
	waiting := f.agentOf(f.mustSave("stella", nil, "waiting-bot", example))
	_, err := f.TryID("stella", startSQL, waiting, inputs)
	wantState(t, err, sqlBadState)
	v, suspended := f.approvedAgent("suspended-bot")
	ok(t, f.Exec("otto", `UPDATE eacp.agent_versions SET state = 'SUSPENDED', state_reason = 'pause' WHERE id = $1`, v))
	_, err = f.TryID("stella", startSQL, suspended, inputs)
	wantState(t, err, sqlBadState)
	_, err = f.TryID("stella", startSQL, f.NewAgent(t, "plain").Agent, inputs)
	wantState(t, err, sqlForeignKey)
}

func TestOnlyTheLeaseHolderMovesARun(t *testing.T) {
	f := newFix(t)
	version, agent := f.approvedAgent("leave-bot")
	run := f.ID(t, "stella", startSQL, agent, inputs)
	// Only the runtime claims.
	for _, who := range []string{"stella", "rita", "otto"} {
		wantState(t, f.Exec(who, `SELECT eacp.studio_run_claim('r1', 'v1', 30, 10)`), sqlForbidden)
	}
	c := f.mustClaim("r1")
	if c.ID != run || c.VersionID != version || c.Generation != 1 || c.Subject != stella ||
		c.Inputs["employee_id"] != "E-1" || c.Credential != "pending" || c.CredentialID != nil || len(c.Steps) != 0 {
		t.Fatalf("claimed = %+v", c)
	}
	var def map[string]any
	if json.Unmarshal(c.Definition, &def) != nil || def["kind"] != "agent" {
		t.Fatalf("definition = %s", c.Definition)
	}
	if more, _ := f.claim("r2", 10); len(more) != 0 {
		t.Fatalf("a live lease was claimed again: %+v", more)
	}
	ok(t, f.Exec("rt", heartbeatSQL, run, "r1", int64(1)))
	wantState(t, f.Exec("rt", heartbeatSQL, run, "r2", int64(1)), sqlForbidden)
	wantState(t, f.Exec("rt", heartbeatSQL, run, "r1", int64(2)), sqlForbidden)
	wantState(t, f.Exec("stella", heartbeatSQL, run, "r1", int64(1)), sqlForbidden)

	// A lapsed lease is claimed again with the next generation; the old holder is fenced.
	f.owner(func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `UPDATE eacp.studio_runs SET leased_until = now() - interval '1 second'`)
		return err
	})
	if c := f.mustClaim("r2"); c.Generation != 2 {
		t.Fatalf("generation = %d", c.Generation)
	}
	a := f.action(version, stepKey(run, 0), stella)
	wantState(t, f.Exec("rt", stepSQL, run, "r1", int64(1), 0, a), sqlForbidden)
	wantState(t, f.Exec("rt", finishSQL, run, "r1", int64(1), "FAILED", nil, "action_failed"), sqlForbidden)
	ok(t, f.Exec("rt", stepSQL, run, "r2", int64(2), 0, a))
}

func TestTwoRuntimesNeverShareARun(t *testing.T) {
	f := newFix(t)
	_, agent := f.approvedAgent("leave-bot")
	for range 6 {
		f.ID(t, "stella", startSQL, agent, inputs)
	}
	var mu sync.Mutex
	seen := map[uuid.UUID]int{}
	var wg sync.WaitGroup
	for i := range 3 {
		wg.Go(func() {
			got, err := f.claim(fmt.Sprintf("r%d", i), 6)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, c := range got {
				seen[c.ID]++
			}
		})
	}
	wg.Wait()
	if len(seen) != 6 {
		t.Fatalf("claimed %d of 6 runs", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("run %s claimed %d times", id, n)
		}
	}
}

func TestAStepIsTheRunsOwnAction(t *testing.T) {
	f := newFix(t)
	version, agent := f.approvedAgent("leave-bot")
	run := f.ID(t, "stella", startSQL, agent, inputs)
	f.mustClaim("r1")
	other, _ := f.approvedAgent("other-bot")
	for name, a := range map[string]uuid.UUID{
		"another version": f.action(other, stepKey(run, 0), stella),
		"another subject": f.action(version, stepKey(run, 0)+"x", "abe@tenant-a.test"),
		"another key":     f.action(version, "my-own-key", stella),
		"unknown action":  uuid.New(),
	} {
		err := f.Exec("rt", stepSQL, run, "r1", int64(1), 0, a)
		if err == nil {
			t.Fatalf("%s: recorded", name)
		}
		wantState(t, err, sqlForbidden, sqlForeignKey)
	}
	a := f.action(version, stepKey(run, 0), stella)
	// Only the next tool_call index: 1 is the respond step.
	wantState(t, f.Exec("rt", stepSQL, run, "r1", int64(1), 1, a), sqlCheck, sqlBadState)
	ok(t, f.Exec("rt", stepSQL, run, "r1", int64(1), 0, a))
	wantState(t, f.Exec("rt", stepSQL, run, "r1", int64(1), 0, a), sqlBadState)
	if got := f.scalar(`SELECT concat_ws(' ', step_index, step_id, action_id = $2) FROM eacp.studio_run_steps
		WHERE run_id = $1`, run, a); got != "0 lookup t" {
		t.Fatalf("step = %q", got)
	}
	// A resumed claim lists the recorded step.
	f.owner(func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `UPDATE eacp.studio_runs SET leased_until = now() - interval '1 second'`)
		return err
	})
	if c := f.mustClaim("r2"); len(c.Steps) != 1 || c.Steps[0]["action_id"] != a.String() {
		t.Fatalf("resumed steps = %v", c.Steps)
	}
}

func TestARunSucceedsOnlyWithEveryStepAndAnAnswer(t *testing.T) {
	f := newFix(t)
	version, agent := f.approvedAgent("leave-bot")
	run := f.ID(t, "stella", startSQL, agent, inputs)
	f.mustClaim("r1")
	wantState(t, f.Exec("rt", finishSQL, run, "r1", int64(1), "SUCCEEDED", "12 days", nil), sqlBadState)
	ok(t, f.Exec("rt", stepSQL, run, "r1", int64(1), 0, f.action(version, stepKey(run, 0), stella)))
	for name, args := range map[string][]any{
		"no answer":     {"SUCCEEDED", nil, nil},
		"reason":        {"SUCCEEDED", "12 days", "action_failed"},
		"too large":     {"SUCCEEDED", strings.Repeat("x", 65537), nil},
		"fail answer":   {"FAILED", "12 days", "action_failed"},
		"no reason":     {"FAILED", nil, nil},
		"unknown":       {"FAILED", nil, "because"},
		"unknown state": {"DONE", "x", nil},
	} {
		err := f.Exec("rt", finishSQL, append([]any{run, "r1", int64(1)}, args...)...)
		if err == nil {
			t.Fatalf("%s: finished", name)
		}
		wantState(t, err, sqlCheck, sqlBadState)
	}
	ok(t, f.Exec("rt", finishSQL, run, "r1", int64(1), "SUCCEEDED", "You have 12 days", nil))
	if got := f.scalar(`SELECT concat_ws(' ', state, inputs IS NULL, finished_at IS NOT NULL,
			answer_expires_at BETWEEN now() + interval '3599 seconds' AND now() + interval '3601 seconds')
		FROM eacp.studio_runs WHERE id = $1`, run); got != "SUCCEEDED t t t" {
		t.Fatalf("run = %q", got)
	}
	wantState(t, f.Exec("rt", finishSQL, run, "r1", int64(1), "FAILED", nil, "action_failed"), sqlForbidden, sqlBadState)
	// A failure clears the inputs too.
	run2 := f.ID(t, "stella", startSQL, agent, inputs)
	f.mustClaim("r1")
	ok(t, f.Exec("rt", finishSQL, run2, "r1", int64(1), "FAILED", nil, "credential_pending"))
	if got := f.scalar(`SELECT concat_ws(' ', state, failure_reason, inputs IS NULL, answer IS NULL)
		FROM eacp.studio_runs WHERE id = $1`, run2); got != "FAILED credential_pending t t" {
		t.Fatalf("failed run = %q", got)
	}
}

// finished starts, claims, steps and finishes a run of agent as stella with answer.
func (f *fix) finished(version, agent uuid.UUID, answer string) uuid.UUID {
	f.t.Helper()
	run := f.ID(f.t, "stella", startSQL, agent, `{"employee_id": "`+inputCanary+`"}`)
	c := f.mustClaim("r1")
	ok(f.t, f.Exec("rt", stepSQL, run, "r1", c.Generation, 0, f.action(version, stepKey(run, 0), stella)))
	ok(f.t, f.Exec("rt", finishSQL, run, "r1", c.Generation, "SUCCEEDED", answer, nil))
	return run
}

func TestOnlyTheRequesterReadsTheAnswerBeforeItExpires(t *testing.T) {
	f := newFix(t)
	version, agent := f.approvedAgent("leave-bot")
	run := f.finished(version, agent, answerCanary)
	read := func(who string) string {
		t.Helper()
		var s *string
		ctx := context.Background()
		ok(t, storage.InTenantTx(ctx, f.App, f.Tenant.String(), func(tx pgx.Tx) error {
			if err := storage.SetActor(ctx, tx, f.P[who]); err != nil {
				return err
			}
			return tx.QueryRow(ctx, answerSQL, run).Scan(&s)
		}))
		if s == nil {
			return ""
		}
		return *s
	}
	if got := read("stella"); got != answerCanary {
		t.Fatalf("answer = %q", got)
	}
	for _, who := range []string{"abe", "rita", "otto", "audra", "rt"} {
		if got := read(who); got != "" {
			t.Fatalf("%s read %q", who, got)
		}
	}
	// The application role cannot select the answer or the inputs.
	for _, col := range []string{"answer", "inputs"} {
		wantState(t, f.Exec("stella", `SELECT `+col+` FROM eacp.studio_runs`), sqlForbidden)
	}
	f.owner(func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `UPDATE eacp.studio_runs SET answer_expires_at = now() - interval '1 second'`)
		return err
	})
	if got := read("stella"); got != "" {
		t.Fatalf("expired answer = %q", got)
	}
}

func TestTheSweeperExpiresRunsAndAnswersOnce(t *testing.T) {
	f := newFix(t)
	version, agent := f.approvedAgent("leave-bot")
	answered := f.finished(version, agent, "done")
	queued := f.ID(t, "stella", startSQL, agent, inputs)
	running := f.ID(t, "stella", startSQL, agent, inputs)
	fresh := f.ID(t, "stella", startSQL, agent, inputs)
	f.owner(func(tx pgx.Tx) error {
		ctx := context.Background()
		if _, err := tx.Exec(ctx, `UPDATE eacp.studio_runs SET answer_expires_at = now() - interval '1 second' WHERE id = $1`, answered); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE eacp.studio_runs SET state = 'RUNNING', runtime_id = 'r9', lease_generation = 1,
			leased_until = now() - interval '1 second' WHERE id = $1`, running); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE eacp.studio_runs SET deadline = now() - interval '1 second' WHERE id = ANY($1)`,
			[]uuid.UUID{queued, running})
		return err
	})
	// Only the sweeper expires.
	wantState(t, f.Exec("otto", `SELECT eacp.studio_runs_expire(100)`), sqlForbidden)
	var mu sync.Mutex
	total := 0
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			ctx := context.Background()
			err := storage.InTenantTx(ctx, f.App, f.Tenant.String(), func(tx pgx.Tx) error {
				if err := storage.SetSystem(ctx, tx, "sweeper"); err != nil {
					return err
				}
				var n int
				if err := tx.QueryRow(ctx, `SELECT eacp.studio_runs_expire(100)`).Scan(&n); err != nil {
					return err
				}
				mu.Lock()
				total += n
				mu.Unlock()
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if total != 3 {
		t.Fatalf("expired %d, want 3", total)
	}
	if got := f.scalar(`SELECT string_agg(concat_ws(' ', state, COALESCE(failure_reason, '-'), answer IS NULL,
			answer_pruned_at IS NOT NULL), ', ' ORDER BY created_at) FROM eacp.studio_runs WHERE id = ANY($1)`,
		[]uuid.UUID{answered, queued, running, fresh}); got != "SUCCEEDED - t t, FAILED deadline_exceeded t f, FAILED deadline_exceeded t f, QUEUED - t f" {
		t.Fatalf("runs = %q", got)
	}
	var ids []uuid.UUID
	f.owner(func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), `SELECT eacp.studio_run_tenants()`)
		if err != nil {
			return err
		}
		ids, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		return err
	})
	if len(ids) != 0 {
		t.Fatalf("tenants with work after the sweep = %v", ids)
	}
}

func TestRunsAreJournaledWithoutInputsOrAnswer(t *testing.T) {
	f := newFix(t)
	version, agent := f.approvedAgent("leave-bot")
	f.finished(version, agent, answerCanary)
	if got := f.scalar(`SELECT (count(*) >= 4)::text FROM eacp.audit_events
		WHERE convert_from(payload, 'UTF8')::jsonb->>'action' IN ('studio_runs.insert', 'studio_runs.update',
		                                                          'studio_run_steps.insert')`); got != "true" {
		t.Fatalf("journaled run events = %s", got)
	}
	for _, canary := range []string{answerCanary, inputCanary} {
		if n := f.scalar(`SELECT count(*)::text FROM eacp.audit_events WHERE convert_from(payload, 'UTF8') LIKE '%' || $1 || '%'`,
			canary); n != "0" {
			t.Fatalf("%s journaled %s times", canary, n)
		}
	}
}

func TestTheRuntimeProposesKeysOnlyForApprovedVersionsAndRecordsTheMaster(t *testing.T) {
	f := newFix(t)
	version, agent := f.approvedAgent("leave-bot")
	waiting := f.mustSave("stella", agent, "", example)
	id := uuid.New()
	for _, who := range []string{"erin", "rita", "stella"} {
		wantState(t, f.Exec(who, proposeSQL, version, uuid.New(), "v1"), sqlForbidden)
	}
	wantState(t, f.Exec("rt", proposeSQL, waiting, uuid.New(), "v1"), sqlForbidden, sqlBadState)
	wantState(t, f.Exec("rt", proposeSQL, version, uuid.New(), "V1"), sqlCheck)
	ok(t, f.Exec("rt", proposeSQL, version, id, "v1"))
	if got := f.scalar(`SELECT concat_ws(' ', c.kind, c.proposed_by = $2, c.approved_at IS NULL, s.master_version,
			c.expires_at BETWEEN now() + interval '89 days' AND now() + interval '90 days')
		FROM eacp.credentials c JOIN eacp.studio_credentials s ON s.tenant_id = c.tenant_id AND s.id = c.id
		WHERE c.id = $1`, id, f.P["rt"]); got != "ak t t v1 t" {
		t.Fatalf("proposal = %q", got)
	}
	// A pending proposal is enough: a second one is not due.
	wantState(t, f.Exec("rt", proposeSQL, version, uuid.New(), "v1"), sqlBadState)
	// Another master version is due separately (rotation of the master).
	ok(t, f.Exec("rt", proposeSQL, version, uuid.New(), "v2"))

	// The claim uses the approved key of the runtime's master.
	ok(t, f.Exec("ravi", `UPDATE eacp.credentials SET approved_at = now() WHERE id = $1`, id))
	f.ID(t, "stella", startSQL, agent, inputs)
	if c := f.mustClaim("r1"); c.Credential != "ok" || c.CredentialID == nil || *c.CredentialID != id {
		t.Fatalf("claim credential = %s %v", c.Credential, c.CredentialID)
	}
}

func TestKeysAreDueBeforeTheyExpire(t *testing.T) {
	f := newFix(t)
	version, agent := f.approvedAgent("leave-bot")
	due := func() []uuid.UUID {
		t.Helper()
		var out []uuid.UUID
		ctx := context.Background()
		ok(t, storage.InTenantTx(ctx, f.App, f.Tenant.String(), func(tx pgx.Tx) error {
			if err := storage.SetActor(ctx, tx, f.P["rt"]); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, `SELECT eacp.studio_credentials_due('v1')`)
			if err != nil {
				return err
			}
			out, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
			return err
		}))
		return out
	}
	if got := due(); len(got) != 1 || got[0] != version {
		t.Fatalf("due = %v", got)
	}
	id := uuid.New()
	ok(t, f.Exec("rt", proposeSQL, version, id, "v1"))
	if got := due(); len(got) != 0 {
		t.Fatalf("due with a pending proposal = %v", got)
	}
	ok(t, f.Exec("ravi", `UPDATE eacp.credentials SET approved_at = now() WHERE id = $1`, id))
	if got := due(); len(got) != 0 {
		t.Fatalf("due with a fresh key = %v", got)
	}
	// 30 days before expiry the successor is due; an expired key makes runs fail closed.
	f.owner(func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `ALTER TABLE eacp.credentials DISABLE TRIGGER credentials_guard;
			UPDATE eacp.credentials SET expires_at = now() + interval '29 days';
			ALTER TABLE eacp.credentials ENABLE TRIGGER credentials_guard`)
		return err
	})
	if got := due(); len(got) != 1 {
		t.Fatalf("due near expiry = %v", got)
	}
	f.owner(func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `ALTER TABLE eacp.credentials DISABLE TRIGGER credentials_guard;
			UPDATE eacp.credentials SET expires_at = now() - interval '1 second';
			ALTER TABLE eacp.credentials ENABLE TRIGGER credentials_guard`)
		return err
	})
	f.ID(t, "stella", startSQL, agent, inputs)
	if c := f.mustClaim("r1"); c.Credential != "expired" || c.CredentialID != nil {
		t.Fatalf("claim credential = %s", c.Credential)
	}
}

func TestTheBulkRevocationRevokesOnlyStudioKeys(t *testing.T) {
	f := newFix(t)
	version, _ := f.approvedAgent("leave-bot")
	studioKey := uuid.New()
	ok(t, f.Exec("rt", proposeSQL, version, studioKey, "v1"))
	// A key an editor proposed for the Studio version is a Studio key too.
	editorKey := f.ID(t, "erin", `INSERT INTO eacp.credentials (tenant_id, id, kind, agent_version_id, secret_hash, expires_at)
		VALUES (eacp.current_tenant_id(), gen_random_uuid(), 'ak', $1, decode(repeat('ef', 32), 'hex'),
		        now() + interval '30 days') RETURNING id`, version)
	plain := f.ActiveAgent(t, "plain", f.erpRead.Tool)
	plainKey := f.ID(t, "erin", `INSERT INTO eacp.credentials (tenant_id, id, kind, agent_version_id, secret_hash, expires_at)
		VALUES (eacp.current_tenant_id(), gen_random_uuid(), 'ak', $1, decode(repeat('ef', 32), 'hex'),
		        now() + interval '30 days') RETURNING id`, plain.Version)
	for _, who := range []string{"rita", "rt", "alice"} {
		wantState(t, f.Exec(who, revokeAllSQL, "suspected runtime compromise"), sqlForbidden)
	}
	wantState(t, f.Exec("otto", revokeAllSQL, " "), sqlCheck)
	var n int
	ctx := context.Background()
	ok(t, storage.InTenantTx(ctx, f.App, f.Tenant.String(), func(tx pgx.Tx) error {
		if err := storage.SetActor(ctx, tx, f.P["otto"]); err != nil {
			return err
		}
		return tx.QueryRow(ctx, revokeAllSQL, "suspected runtime compromise").Scan(&n)
	}))
	if n != 2 {
		t.Fatalf("revoked %d", n)
	}
	if got := f.scalar(`SELECT string_agg((revoked_at IS NOT NULL)::text, ' ' ORDER BY array_position($1::uuid[], id))
		FROM eacp.credentials WHERE id = ANY($1)`, []uuid.UUID{studioKey, editorKey, plainKey}); got != "true true false" {
		t.Fatalf("revoked = %q", got)
	}
}

func TestAnExpiringStudioKeyOpensAnIncident(t *testing.T) {
	f := newFix(t)
	version, _ := f.approvedAgent("leave-bot")
	id := uuid.New()
	ok(t, f.Exec("rt", proposeSQL, version, id, "v1"))
	ok(t, f.Exec("ravi", `UPDATE eacp.credentials SET approved_at = now() WHERE id = $1`, id))
	evaluate := func() int {
		t.Helper()
		var n int
		ctx := context.Background()
		ok(t, storage.InTenantTx(ctx, f.App, f.Tenant.String(), func(tx pgx.Tx) error {
			if err := storage.SetSystem(ctx, tx, "incident"); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT eacp.incident_evaluate()`).Scan(&n)
		}))
		return n
	}
	if n := evaluate(); n != 0 {
		t.Fatalf("opened %d for a fresh key", n)
	}
	f.owner(func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `ALTER TABLE eacp.credentials DISABLE TRIGGER credentials_guard;
			UPDATE eacp.credentials SET expires_at = now() + interval '6 days';
			ALTER TABLE eacp.credentials ENABLE TRIGGER credentials_guard`)
		return err
	})
	if n := evaluate(); n != 1 {
		t.Fatalf("opened %d near expiry", n)
	}
	if n := evaluate(); n != 0 {
		t.Fatalf("opened %d again", n)
	}
	if got := f.scalar(`SELECT concat_ws(' ', kind, subject_type, subject_id = $1, severity) FROM eacp.incidents`,
		version); got != "studio_credential agent_version t high" {
		t.Fatalf("incident = %q", got)
	}
}

// TestAHeartbeatSaysWhetherTheVersionIsStillActive: the runtime learns at
// each heartbeat that its run's version was replaced, so it sends nothing
// more (ADR-033, version_replaced); a revoked key has its own reason.
func TestAHeartbeatSaysWhetherTheVersionIsStillActive(t *testing.T) {
	f := newFix(t)
	version, agent := f.approvedAgent("leave-bot")
	run := f.ID(t, "stella", startSQL, agent, inputs)
	f.mustClaim("r1")
	active := func() bool {
		var b bool
		ctx := context.Background()
		ok(t, storage.InTenantTx(ctx, f.App, f.Tenant.String(), func(tx pgx.Tx) error {
			if err := storage.SetActor(ctx, tx, f.P["rt"]); err != nil {
				return err
			}
			return tx.QueryRow(ctx, heartbeatSQL, run, "r1", int64(1)).Scan(&b)
		}))
		return b
	}
	if !active() {
		t.Fatal("an active version reads as replaced")
	}
	v2 := f.mustSave("stella", agent, "", example)
	ok(t, f.decide("rita", v2, true, "second version"))
	if active() {
		t.Fatalf("version %s was replaced but reads as active", version)
	}
	ok(t, f.Exec("rt", finishSQL, run, "r1", int64(1), "FAILED", nil, "credential_revoked"))
}
