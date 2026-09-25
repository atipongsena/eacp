package release_test

// Rules that the first mutation run of migration 00019 showed untested
// (docs/reviews/2026-09-25-phase19-code-review.md). Raw SQL as eacp_app.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/storage/pgtest"
)

// adminExec runs sql as the database owner with triggers disabled, to
// stage rows no application path reaches directly.
func (w *world) adminExec(t *testing.T, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.Pool(t, w.f.DB.AdminDSN)
	ok(t, pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, sql, args...)
		return err
	}))
}

// setState moves version v with actor (a lifecycle transition).
func (w *world) setState(actor string, v uuid.UUID, state string) error {
	return w.f.Exec(actor, `UPDATE eacp.agent_versions SET state = $2, state_reason = 'drill' WHERE id = $1`, v, state)
}

func TestOpeningRejectsDuplicateSuitesAndCandidatesWithoutAnAllowlist(t *testing.T) {
	w := newWorld(t)
	_, err := w.open(uuid.New(), plan{suites: []string{"accuracy", "accuracy"}})
	wantState(t, err, sqlCheck)
	w.candidate = w.f.ID(t, "erin", `INSERT INTO eacp.agent_versions (tenant_id, agent_id, runtime, code_ref)
		VALUES (eacp.current_tenant_id(), $1, 'python', 'git:bare') RETURNING id`, w.stable.Agent)
	_, err = w.open(uuid.New(), plan{})
	wantState(t, err, sqlBadState)
}

func TestEvaluationRecorderIsTheActorAndTheThresholdPasses(t *testing.T) {
	w := newWorld(t)
	id := w.mustOpen(t, plan{})
	e := w.f.ID(t, "erin", `INSERT INTO eacp.agent_release_evaluations
		(tenant_id, release_id, suite, score, threshold, dataset_digest, evidence_ref, recorded_by)
		VALUES (eacp.current_tenant_id(), $1, 'accuracy', 0.8, 0.8, repeat('ab', 32), 'ci://run/2', $2) RETURNING id`,
		id, w.f.P["ravi"])
	var by uuid.UUID
	var passed bool
	w.query(t, &by, `SELECT recorded_by FROM eacp.agent_release_evaluations WHERE id = $1`, e)
	w.query(t, &passed, `SELECT passed FROM eacp.agent_release_evaluations WHERE id = $1`, e)
	if by != w.f.P["erin"] || !passed {
		t.Fatalf("recorded by %s, passed %v", by, passed)
	}
	// ravi recorded nothing, so ravi may advance.
	ok(t, w.move("ravi", id, "SHADOW", nil))
}

func TestReleaseMovesKeepTheirShape(t *testing.T) {
	w := newWorld(t)
	id := w.mustOpen(t, plan{})
	// Before the canary no version changes, so only the role check refuses an editor.
	wantState(t, w.f.Exec("erin", moveSQL, id, "ROLLED_BACK", nil, "editor"), sqlForbidden)
	w.pass(t, id, "accuracy")
	wantState(t, w.move("ravi", id, "SHADOW", 5000), sqlBadState)
	ok(t, w.move("ravi", id, "SHADOW", nil))
	ok(t, w.move("ravi", id, "CANARY", 5000))
	wantState(t, w.f.Exec("erin", moveSQL, id, "ROLLED_BACK", 5000, "editor"), sqlForbidden)
	wantState(t, w.f.Exec("otto", moveSQL, id, "ROLLED_BACK", 10000, "wider"), sqlBadState)
	ok(t, w.f.Exec("otto", moveSQL, id, "ROLLED_BACK", 5000, "contained"))
}

func TestForwardMovesExcludeTheOpenerCreatorAndAuthor(t *testing.T) {
	t.Run("opener", func(t *testing.T) {
		w := newWorld(t)
		id, err := w.f.TryID("ravi", openSQL, uuid.New(), w.candidate, []string{"accuracy"}, 0, 0, []int{10000}, 1)
		ok(t, err)
		w.pass(t, id, "accuracy")
		wantState(t, w.move("ravi", id, "SHADOW", nil), sqlForbidden)
		ok(t, w.move("rita", id, "SHADOW", nil))
	})
	t.Run("creator", func(t *testing.T) {
		w := newWorld(t)
		g := w.f.ID(t, "alice", `INSERT INTO eacp.role_grants (tenant_id, principal_id, role)
			VALUES (eacp.current_tenant_id(), $1, 'registry_editor') RETURNING id`, w.f.P["ravi"])
		ok(t, w.f.Exec("bob", `UPDATE eacp.role_grants SET approved_at = now() WHERE id = $1`, g))
		w.candidate = w.f.ID(t, "ravi", `INSERT INTO eacp.agent_versions (tenant_id, agent_id, runtime, code_ref)
			VALUES (eacp.current_tenant_id(), $1, 'python', 'git:ravi') RETURNING id`, w.stable.Agent)
		al := w.f.ID(t, "erin", `INSERT INTO eacp.agent_allowlists (tenant_id, agent_version_id, tool_ids)
			VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, w.candidate, []uuid.UUID{w.tool.Tool})
		ok(t, w.f.Exec("rita", `UPDATE eacp.agent_versions SET active_allowlist_id = $1 WHERE id = $2`, al, w.candidate))
		id := cohort(t, w)
		_, err := w.open(id, plan{})
		ok(t, err)
		w.pass(t, id, "accuracy")
		ok(t, w.move("rita", id, "SHADOW", nil))
		ok(t, w.move("rita", id, "CANARY", 5000))
		w.f.QueuedAction(t, w.candidate, "carol", "erp.purchase")
		wantState(t, w.move("ravi", id, "CANARY", 10000), sqlForbidden)
		ok(t, w.move("rita", id, "CANARY", 10000))
	})
	t.Run("allowlist author", func(t *testing.T) {
		w := newWorld(t)
		w.candidate = w.f.ID(t, "erin", `INSERT INTO eacp.agent_versions (tenant_id, agent_id, runtime, code_ref)
			VALUES (eacp.current_tenant_id(), $1, 'python', 'git:next') RETURNING id`, w.stable.Agent)
		al := w.f.ID(t, "ravi", `INSERT INTO eacp.agent_allowlists (tenant_id, agent_version_id, tool_ids)
			VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, w.candidate, []uuid.UUID{w.tool.Tool})
		ok(t, w.f.Exec("rita", `UPDATE eacp.agent_versions SET active_allowlist_id = $1 WHERE id = $2`, al, w.candidate))
		id := cohort(t, w)
		_, err := w.open(id, plan{})
		ok(t, err)
		w.pass(t, id, "accuracy")
		ok(t, w.move("rita", id, "SHADOW", nil))
		ok(t, w.move("rita", id, "CANARY", 5000))
		w.f.QueuedAction(t, w.candidate, "carol", "erp.purchase")
		wantState(t, w.move("ravi", id, "CANARY", 10000), sqlForbidden)
		ok(t, w.move("rita", id, "CANARY", 10000))
	})
}

// Entering CANARY needs the shadow gate, passing suites (recorded again in
// SHADOW) and an ACTIVE stable version, each on its own.
func TestCanaryEntryChecksEachGate(t *testing.T) {
	w := newWorld(t)
	id := w.mustOpen(t, plan{shadow: 1})
	w.pass(t, id, "accuracy")
	ok(t, w.move("ravi", id, "SHADOW", nil))
	wantState(t, w.move("ravi", id, "CANARY", 5000), sqlBadState) // no shadow case yet
	live := w.f.QueuedAction(t, w.stable.Version, "carol", "erp.purchase")
	_, err := w.observe("shadow", live, "carol", "erp.purchase", `{"amount":1000000,"currency":"THB"}`, "allow")
	ok(t, err)

	w.f.ID(t, "erin", evalSQL, id, "accuracy", 0.1)
	wantState(t, w.move("ravi", id, "CANARY", 5000), sqlBadState) // the latest result fails
	w.pass(t, id, "accuracy")

	ok(t, w.setState("otto", w.stable.Version, "SUSPENDED"))
	wantState(t, w.move("ravi", id, "CANARY", 5000), sqlBadState) // no ACTIVE stable
	ok(t, w.setState("ravi", w.stable.Version, "ACTIVE"))
	ok(t, w.move("ravi", id, "CANARY", 5000))
}

func TestAStepNeedsAnActiveCandidate(t *testing.T) {
	w := newWorld(t)
	id := w.canary(t, cohort(t, w), plan{})
	w.f.QueuedAction(t, w.candidate, "carol", "erp.purchase")
	ok(t, w.setState("otto", w.candidate, "SUSPENDED"))
	wantState(t, w.move("rita", id, "CANARY", 10000), sqlBadState)
}

// Beside a canary candidate only its stable version may be ACTIVE, and the
// route of a subject outside the cohort is the stable version whatever the
// row order.
func TestOnlyTheCanaryPairIsActive(t *testing.T) {
	w := newWorld(t)
	w.canary(t, cohort(t, w), plan{})
	third := w.version(t, w.tool.Tool)

	// The stable version may come back beside the candidate; its row is then
	// newer than the candidate's.
	ok(t, w.setState("otto", w.stable.Version, "SUSPENDED"))
	wantState(t, w.setState("ravi", third, "ACTIVE"), sqlUnique)
	ok(t, w.setState("ravi", w.stable.Version, "ACTIVE"))
	var route uuid.UUID
	w.query(t, &route, `SELECT eacp.release_route($1, $2)`, w.stable.Agent, w.f.P["amy"])
	if route != w.stable.Version {
		t.Fatalf("amy routes to %s, want the stable %s", route, w.stable.Version)
	}

	// The candidate never comes back beside another version.
	ok(t, w.setState("otto", w.stable.Version, "SUSPENDED"))
	ok(t, w.setState("otto", w.candidate, "SUSPENDED"))
	ok(t, w.setState("ravi", third, "ACTIVE"))
	wantState(t, w.setState("ravi", w.candidate, "ACTIVE"), sqlUnique)
}

func TestTheSystemRollsBackOnlyABreachedSufficientCanary(t *testing.T) {
	rollback := func(w *world, id uuid.UUID, bp any) error {
		return w.f.ExecSystem("release", moveSQL, id, "ROLLED_BACK", bp, "auto")
	}
	t.Run("healthy", func(t *testing.T) {
		w := newWorld(t)
		id := w.canary(t, cohort(t, w), plan{})
		w.f.QueuedAction(t, w.candidate, "carol", "erp.purchase")
		wantState(t, rollback(w, id, 5000), sqlBadState)
	})
	t.Run("insufficient", func(t *testing.T) {
		w := newWorld(t)
		id := w.canary(t, cohort(t, w), plan{canaryActions: 3})
		ok(t, w.f.ExecAgent(w.candidate, denySQL, w.f.ReceivedAction(t, w.candidate, "carol", "erp.purchase")))
		wantState(t, rollback(w, id, 5000), sqlBadState)
	})
	t.Run("not a canary", func(t *testing.T) {
		w := newWorld(t)
		id := w.mustOpen(t, plan{})
		w.pass(t, id, "accuracy")
		ok(t, w.move("ravi", id, "SHADOW", nil))
		// The candidate's refused submissions look like a breach.
		ok(t, w.f.ExecAgent(w.candidate, denySQL, w.f.ReceivedAction(t, w.candidate, "carol", "erp.purchase")))
		wantState(t, rollback(w, id, nil), sqlForbidden)
	})
	t.Run("only its own candidate", func(t *testing.T) {
		w := newWorld(t)
		early := w.mustOpen(t, plan{})
		ok(t, w.f.Exec("otto", moveSQL, early, "ROLLED_BACK", nil, "abandoned"))
		w.canary(t, cohort(t, w), plan{})
		wantState(t, w.f.ExecSystem("release", `UPDATE eacp.agent_versions SET state = 'SUSPENDED',
			state_reason = 'x' WHERE id = $1`, w.candidate), sqlForbidden)
	})
	t.Run("only a suspension", func(t *testing.T) {
		w := newWorld(t)
		w.canary(t, cohort(t, w), plan{})
		ok(t, w.f.ExecAgent(w.candidate, denySQL, w.f.ReceivedAction(t, w.candidate, "carol", "erp.purchase")))
		wantState(t, w.f.ExecSystem("release", fmt.Sprintf(`DO $$ BEGIN
			PERFORM eacp.release_evaluate();
			UPDATE eacp.agent_versions SET state = 'QUARANTINED', state_reason = 'x' WHERE id = '%s';
			END $$`, w.candidate)), sqlForbidden)
	})
}

func TestObservationComparisons(t *testing.T) {
	w := newWorld(t)
	crm := w.f.ActiveTool(t, "crm", "lookup")
	w.candidate = w.version(t, w.tool.Tool, crm.Tool)
	id := w.mustOpen(t, plan{})
	payload := `{"amount":1000000,"currency":"THB"}`

	// The candidate's own action is never a reference.
	own := w.f.ReceivedAction(t, w.candidate, "carol", "erp.purchase")
	ok(t, w.f.ExecAgent(w.candidate, denySQL, own))
	_, err := w.observe("replay", own, "carol", "erp.purchase", payload, nil)
	wantState(t, err, sqlBadState)

	// Another allowed tool with the same verdict does not agree.
	ref := w.reference(t, w.stable.Version, "carol")
	obs, err := w.observe("replay", ref, "carol", "crm.lookup", payload, "allow")
	ok(t, err)
	var agrees, outcome bool
	w.query(t, &agrees, `SELECT agrees FROM eacp.agent_release_observations WHERE id = $1`, obs)
	w.query(t, &outcome, `SELECT outcome_match FROM eacp.agent_release_observations WHERE id = $1`, obs)
	if agrees || !outcome {
		t.Fatalf("different tool: agrees %v, outcome match %v", agrees, outcome)
	}

	// The agent cannot supply the replay response.
	ref2 := w.reference(t, w.stable.Version, "carol")
	miss := w.f.AgentID(t, w.candidate, `INSERT INTO eacp.agent_release_observations
		(tenant_id, kind, reference_action_id, subject, operation, target, tool, tool_schema_version,
		 resource, input_payload, verdict, policy_version, recorded)
		VALUES (eacp.current_tenant_id(), 'replay', $1, 'carol@tenant-a.test', 'purchase', 'erp', 'erp.purchase', '1',
		 'po', '{"amount":5}', 'allow', 1, '{"state":"SUCCEEDED"}') RETURNING id`, ref2)
	var recorded []byte
	w.query(t, &recorded, `SELECT recorded FROM eacp.agent_release_observations WHERE id = $1`, miss)
	if recorded != nil {
		t.Fatalf("a miss kept the agent's response %s", recorded)
	}

	// A shadow reference is the stable version's, not another version's.
	w.pass(t, id, "accuracy")
	ok(t, w.move("ravi", id, "SHADOW", nil))
	third := w.version(t, w.tool.Tool)
	other := w.f.ReceivedAction(t, third, "carol", "erp.purchase")
	_, err = w.observe("shadow", other, "carol", "erp.purchase", payload, "allow")
	wantState(t, err, sqlBadState)
}

// A proposal the candidate's allowlist denies never agrees, even with a
// reference denied the same way before governance.
func TestCapabilityDeniedProposalNeverAgrees(t *testing.T) {
	w := newWorld(t)
	w.candidate = w.version(t)
	w.mustOpen(t, plan{})
	ref := w.f.ReceivedAction(t, w.stable.Version, "carol", "erp.purchase")
	ok(t, w.f.ExecAgent(w.stable.Version, denySQL, ref))
	obs, err := w.observe("replay", ref, "carol", "erp.purchase", `{"amount":1000000,"currency":"THB"}`, nil)
	ok(t, err)
	var row struct {
		tool, outcome, agrees bool
		reference             string
	}
	ctx := context.Background()
	ok(t, w.ownerTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT tool_match, outcome_match, agrees, reference_outcome
			FROM eacp.agent_release_observations WHERE id = $1`, obs).Scan(&row.tool, &row.outcome, &row.agrees, &row.reference)
	}))
	if !row.tool || !row.outcome || row.agrees || row.reference != "denied:tool_not_in_allowlist" {
		t.Fatalf("observation = %+v", row)
	}
}

func TestReplayGateNeedsItsCases(t *testing.T) {
	w := newWorld(t)
	id := w.mustOpen(t, plan{replay: 2})
	w.pass(t, id, "accuracy")
	ref := w.reference(t, w.stable.Version, "carol")
	_, err := w.observe("replay", ref, "carol", "erp.purchase", `{"amount":1000000,"currency":"THB"}`, "allow")
	ok(t, err)
	wantState(t, w.move("ravi", id, "SHADOW", nil), sqlBadState) // 1 agreeing case of 2
}

func TestCohortBoundaryAndRedispatch(t *testing.T) {
	t.Run("boundary", func(t *testing.T) {
		w := newWorld(t)
		// A step equal to carol's bucket leaves carol outside (bucket < step).
		var id uuid.UUID
		var b int
		for range 50 {
			id = uuid.New()
			w.query(t, &b, `SELECT eacp.release_bucket($1, $2)`, id, w.f.P["carol"])
			if b >= 1 && b < 10000 {
				break
			}
		}
		w.canary(t, id, plan{steps: []int{b, 10000}})
		var denial string
		w.query(t, &denial, `SELECT COALESCE(eacp.release_denial($1, $2), 'none')`, w.candidate, w.f.P["carol"])
		if denial != "canary_cohort" {
			t.Fatalf("carol at bucket %d with step %d: %q", b, b, denial)
		}
	})
	t.Run("a later release's cohort", func(t *testing.T) {
		w := newWorld(t)
		first := w.canary(t, cohort(t, w), plan{})
		queued := w.f.QueuedAction(t, w.candidate, "carol", "erp.purchase")
		ok(t, w.f.Exec("otto", moveSQL, first, "ROLLED_BACK", 5000, "regression"))
		// The same candidate again, under a release whose first step excludes carol.
		var second uuid.UUID
		for range 200 {
			second = uuid.New()
			var b int
			w.query(t, &b, `SELECT eacp.release_bucket($1, $2)`, second, w.f.P["carol"])
			if b >= 5000 {
				break
			}
		}
		w.canary(t, second, plan{})
		var drift string
		w.query(t, &drift, `SELECT COALESCE(eacp.dispatch_drift(a), 'none') FROM eacp.actions a WHERE id = $1`, queued)
		if drift != "canary_cohort" {
			t.Fatalf("drift of the earlier cohort's action = %q", drift)
		}
	})
}

func TestCanaryMetricsCountUnknownOutcomesAndIgnoreReleasedReservations(t *testing.T) {
	w := newWorld(t)
	id := w.canary(t, cohort(t, w), plan{})
	act := w.f.QueuedAction(t, w.candidate, "carol", "erp.purchase")
	account := w.f.FundAgent(t, w.stable.Agent, "THB", "1000")
	w.adminExec(t, `UPDATE eacp.actions SET state = 'UNKNOWN_OUTCOME' WHERE id = $1`, act)
	w.adminExec(t, `INSERT INTO eacp.budget_reservations (tenant_id, account_id, unit, action_id, contract_id, amount,
		state, committed_amount, expires_at, settled_at, settle_reason)
		VALUES ($1, $2, 'THB', $3, $4, 100, 'RELEASED', 0, now() + interval '1 hour', now(), 'cancelled')`,
		w.f.Tenant, account, act, w.tool.Contract)
	var raw []byte
	w.query(t, &raw, `SELECT eacp.release_canary_report($1)`, id)
	var report struct {
		Candidate struct {
			Unknown int            `json:"unknown"`
			Cost    map[string]any `json:"cost"`
		} `json:"candidate"`
	}
	ok(t, json.Unmarshal(raw, &report))
	if report.Candidate.Unknown != 1 || len(report.Candidate.Cost) != 0 {
		t.Fatalf("report = %s", raw)
	}
}

func (w *world) ownerTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, w.f.Owner, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, w.f.Tenant.String()); err != nil {
			return err
		}
		return fn(tx)
	})
}
