package release_test

// Schema-level tests (ADR-018): every release rule is enforced by
// PostgreSQL, so these tests issue raw SQL as the application role.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/atipongsena/eacp/internal/registry/registrytest"
	"github.com/atipongsena/eacp/internal/storage"
	"github.com/atipongsena/eacp/internal/storage/pgtest"
)

const (
	sqlForbidden = "42501"
	sqlBadState  = "55000"
	sqlCheck     = "23514"
	sqlUnique    = "23505"

	openSQL = `INSERT INTO eacp.agent_releases (tenant_id, id, candidate_version_id, required_suites, reason,
		min_replay_cases, min_shadow_cases, canary_steps, min_canary_actions)
		VALUES (eacp.current_tenant_id(), $1, $2, $3, 'next model', $4, $5, $6, $7) RETURNING id`
	moveSQL = `UPDATE eacp.agent_releases SET state = $2, canary_bp = $3, change_reason = $4 WHERE id = $1`
	evalSQL = `INSERT INTO eacp.agent_release_evaluations
		(tenant_id, release_id, suite, score, threshold, dataset_digest, evidence_ref)
		VALUES (eacp.current_tenant_id(), $1, $2, $3, 0.8, repeat('ab', 32), 'ci://run/1') RETURNING id`
	observeSQL = `INSERT INTO eacp.agent_release_observations
		(tenant_id, kind, reference_action_id, subject, operation, target, tool, tool_schema_version,
		 resource, input_payload, verdict, policy_version)
		VALUES (eacp.current_tenant_id(), $1, $2, $3, 'purchase', 'erp', $4, '1', 'po', $5, $6, $7) RETURNING id`
	authorizeSQL = `UPDATE eacp.actions SET state = 'AUTHORIZED', state_reason = 'ok',
		decision_evidence_id = $2, enforced_payload = $3 WHERE id = $1`
	cancelSQL = `UPDATE eacp.actions SET state = 'CANCELLED', state_reason = 'not needed' WHERE id = $1`
	denySQL   = `UPDATE eacp.actions SET state = 'DENIED', state_reason = 'tool_not_in_allowlist' WHERE id = $1`
)

func wantState(t *testing.T, err error, codes ...string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("err = %v, want SQLSTATE %v", err, codes)
	}
	for _, c := range codes {
		if pgErr.Code == c {
			return
		}
	}
	t.Fatalf("SQLSTATE %s (%s), want %v", pgErr.Code, pgErr.Message, codes)
}

func ok(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// plan is a release plan; zero fields take the test defaults below.
type plan struct {
	suites        []string
	replay        int
	shadow        int
	steps         []int
	canaryActions int
}

// world is a tenant with an allow-all policy, the tool erp.purchase, the
// stable version of agent buyer (ACTIVE) and a REGISTERED candidate whose
// allowlist erin authored and rita activated. Erin opens releases.
type world struct {
	f         *registrytest.Fixture
	tool      registrytest.Tooling
	policy    uuid.UUID
	stable    registrytest.Agent
	candidate uuid.UUID
}

func newWorld(t *testing.T) *world {
	t.Helper()
	f := registrytest.New(t)
	w := &world{f: f}
	w.tool = f.ActiveTool(t, "erp", "purchase")
	w.policy = f.ActivatePolicy(t, registrytest.AllowPolicy)
	w.stable = f.ActiveAgent(t, "buyer", w.tool.Tool)
	w.candidate = w.version(t, w.tool.Tool)
	return w
}

// version registers another REGISTERED version of buyer with tools.
func (w *world) version(t *testing.T, tools ...uuid.UUID) uuid.UUID {
	t.Helper()
	if tools == nil {
		tools = []uuid.UUID{}
	}
	v := w.f.ID(t, "erin", `INSERT INTO eacp.agent_versions (tenant_id, agent_id, runtime, code_ref)
		VALUES (eacp.current_tenant_id(), $1, 'python', 'git:next') RETURNING id`, w.stable.Agent)
	al := w.f.ID(t, "erin", `INSERT INTO eacp.agent_allowlists (tenant_id, agent_version_id, tool_ids)
		VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, v, tools)
	ok(t, w.f.Exec("rita", `UPDATE eacp.agent_versions SET active_allowlist_id = $1 WHERE id = $2`, al, v))
	return v
}

func (w *world) open(id uuid.UUID, p plan) (uuid.UUID, error) {
	if p.suites == nil {
		p.suites = []string{"accuracy"}
	}
	if p.steps == nil {
		p.steps = []int{5000, 10000}
	}
	if p.canaryActions == 0 {
		p.canaryActions = 1
	}
	return w.f.TryID("erin", openSQL, id, w.candidate, p.suites, p.replay, p.shadow, p.steps, p.canaryActions)
}

func (w *world) mustOpen(t *testing.T, p plan) uuid.UUID {
	t.Helper()
	id, err := w.open(uuid.New(), p)
	ok(t, err)
	return id
}

func (w *world) move(actor string, release uuid.UUID, state string, bp any) error {
	return w.f.Exec(actor, moveSQL, release, state, bp, "reviewed")
}

func (w *world) pass(t *testing.T, release uuid.UUID, suite string) {
	t.Helper()
	w.f.ID(t, "erin", evalSQL, release, suite, 0.95)
}

// canary opens a release with p, passes its suite and moves it to CANARY
// at its first step (ravi, for both moves).
func (w *world) canary(t *testing.T, id uuid.UUID, p plan) uuid.UUID {
	t.Helper()
	if id == uuid.Nil {
		id = uuid.New()
	}
	_, err := w.open(id, p)
	ok(t, err)
	w.pass(t, id, "accuracy")
	ok(t, w.move("ravi", id, "SHADOW", nil))
	first := 5000
	if p.steps != nil {
		first = p.steps[0]
	}
	ok(t, w.move("ravi", id, "CANARY", first))
	return id
}

func (w *world) query(t *testing.T, dst any, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	ok(t, storage.InTenantTx(ctx, w.f.Owner, w.f.Tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(dst)
	}))
}

func (w *world) versionState(t *testing.T, v uuid.UUID) string {
	t.Helper()
	var s string
	w.query(t, &s, `SELECT state FROM eacp.agent_versions WHERE id = $1`, v)
	return s
}

func (w *world) releaseState(t *testing.T, r uuid.UUID) string {
	t.Helper()
	var s string
	w.query(t, &s, `SELECT state FROM eacp.agent_releases WHERE id = $1`, r)
	return s
}

func (w *world) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	w.query(t, &n, sql, args...)
	return n
}

// audits returns the tenant's journal entries with action prefix, oldest first.
func (w *world) audits(t *testing.T, prefix string) []map[string]any {
	t.Helper()
	ctx := context.Background()
	var out []map[string]any
	ok(t, storage.InTenantTx(ctx, w.f.Owner, w.f.Tenant.String(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT payload FROM eacp.audit_events ORDER BY seq`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				return err
			}
			var p map[string]any
			if err := json.Unmarshal(raw, &p); err != nil {
				return err
			}
			if a, _ := p["action"].(string); len(a) >= len(prefix) && a[:len(prefix)] == prefix {
				out = append(out, p)
			}
		}
		return rows.Err()
	}))
	return out
}

// reference is a terminal (CANCELLED) action of version with an allow
// decision, for subject.
func (w *world) reference(t *testing.T, version uuid.UUID, subject string) uuid.UUID {
	t.Helper()
	id := w.f.QueuedAction(t, version, subject, "erp.purchase")
	ok(t, w.f.ExecAgent(version, cancelSQL, id))
	return id
}

// observe records the candidate's proposal; a verdict is made under policy
// version 1, the fixture's active policy.
func (w *world) observe(kind string, reference uuid.UUID, subject, tool, payload string, verdict any) (uuid.UUID, error) {
	var version any
	if verdict != nil {
		version = 1
	}
	return w.f.TryAgentID(w.candidate, observeSQL, kind, reference, subject+"@tenant-a.test", tool, payload, verdict, version)
}

// --------------------------------------------------------------- opening

func TestReleaseOpensFromTheDatabasesStableVersion(t *testing.T) {
	w := newWorld(t)
	id := w.mustOpen(t, plan{})
	var stable, agent, creator uuid.UUID
	var state string
	var steps []int32
	ctx := context.Background()
	ok(t, storage.InTenantTx(ctx, w.f.App, w.f.Tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT stable_version_id, agent_id, created_by, state, canary_steps
			FROM eacp.agent_releases WHERE id = $1`, id).Scan(&stable, &agent, &creator, &state, &steps)
	}))
	if stable != w.stable.Version || agent != w.stable.Agent || creator != w.f.P["erin"] || state != "EVALUATING" {
		t.Fatalf("release = stable %s agent %s by %s in %s", stable, agent, creator, state)
	}
	if a := w.audits(t, "release."); len(a) != 1 || a[0]["action"] != "release.opened" || a[0]["reason"] != "next model" {
		t.Fatalf("journal = %v", a)
	}

	// One open release per agent.
	_, err := w.open(uuid.New(), plan{})
	wantState(t, err, sqlUnique)
}

func TestReleaseOpeningRules(t *testing.T) {
	w := newWorld(t)
	// Only registry roles open a release.
	_, err := w.f.TryID("carol", openSQL, uuid.New(), w.candidate, []string{"accuracy"}, 0, 0, []int{10000}, 1)
	wantState(t, err, sqlForbidden)
	_, err = w.f.TryID("otto", openSQL, uuid.New(), w.candidate, []string{"accuracy"}, 0, 0, []int{10000}, 1)
	wantState(t, err, sqlForbidden)

	// The plan is checked.
	for _, p := range []plan{
		{suites: []string{}},
		{suites: []string{"Bad Suite"}},
		{steps: []int{}},
		{steps: []int{500, 500, 10000}},
		{steps: []int{500, 5000}},
		{steps: []int{0, 10000}},
		{replay: -1},
	} {
		_, err := w.open(uuid.New(), p)
		wantState(t, err, sqlCheck)
	}

	// The candidate is a REGISTERED or SUSPENDED version of an agent with an
	// ACTIVE version, and is not that version.
	w.candidate = w.stable.Version
	_, err = w.open(uuid.New(), plan{})
	wantState(t, err, sqlBadState)
	other := w.f.ActiveAgent(t, "seller", w.tool.Tool)
	w.candidate = other.Version
	_, err = w.open(uuid.New(), plan{})
	wantState(t, err, sqlBadState)
	lonely := w.f.NewAgent(t, "lonely")
	w.candidate = lonely.Version
	_, err = w.open(uuid.New(), plan{})
	wantState(t, err, sqlBadState)

	// A release starts EVALUATING and its plan never changes.
	w.candidate = w.version(t, w.tool.Tool)
	_, err = w.f.TryID("erin", `INSERT INTO eacp.agent_releases (tenant_id, candidate_version_id, required_suites,
		reason, state) VALUES (eacp.current_tenant_id(), $1, '{accuracy}', 'x', 'CANARY') RETURNING id`, w.candidate)
	wantState(t, err, sqlBadState)
	_, err = w.f.TryID("erin", `INSERT INTO eacp.agent_releases (tenant_id, candidate_version_id, required_suites,
		reason) VALUES (eacp.current_tenant_id(), $1, '{accuracy}', ' ') RETURNING id`, w.candidate)
	wantState(t, err, sqlCheck)
	id := w.mustOpen(t, plan{})
	wantState(t, w.f.Exec("rita", `UPDATE eacp.agent_releases SET min_canary_actions = 0 WHERE id = $1`, id), sqlForbidden)
	wantState(t, w.f.Exec("rita", `DELETE FROM eacp.agent_releases WHERE id = $1`, id), sqlForbidden)
}

// ------------------------------------------------------------ evaluation

func TestEvaluationGate(t *testing.T) {
	w := newWorld(t)
	id := w.mustOpen(t, plan{suites: []string{"accuracy", "safety"}})

	// Recording is for registry roles; passed is computed.
	_, err := w.f.TryID("carol", evalSQL, id, "accuracy", 0.9)
	wantState(t, err, sqlForbidden)
	_, err = w.f.TryID("erin", evalSQL, id, "Bad", 0.9)
	wantState(t, err, sqlCheck)
	w.pass(t, id, "accuracy")
	var passed bool
	w.query(t, &passed, `SELECT passed FROM eacp.agent_release_evaluations WHERE release_id = $1`, id)
	if !passed {
		t.Fatal("0.95 >= 0.8 did not pass")
	}

	// Every required suite must pass.
	wantState(t, w.move("ravi", id, "SHADOW", nil), sqlBadState)
	w.f.ID(t, "erin", evalSQL, id, "safety", 0.5)
	wantState(t, w.move("ravi", id, "SHADOW", nil), sqlBadState)
	w.pass(t, id, "safety")
	// The latest result per suite counts.
	w.f.ID(t, "erin", evalSQL, id, "accuracy", 0.1)
	wantState(t, w.move("ravi", id, "SHADOW", nil), sqlBadState)
	w.pass(t, id, "accuracy")

	// The opener and anyone who recorded a result cannot advance it.
	wantState(t, w.move("erin", id, "SHADOW", nil), sqlForbidden)
	w.f.ID(t, "rita", evalSQL, id, "accuracy", 0.99)
	wantState(t, w.move("rita", id, "SHADOW", nil), sqlForbidden)
	wantState(t, w.move("otto", id, "SHADOW", nil), sqlForbidden)
	// Stages are not skipped.
	wantState(t, w.move("ravi", id, "CANARY", 5000), sqlBadState)
	wantState(t, w.move("ravi", id, "PROMOTED", nil), sqlBadState)
	ok(t, w.move("ravi", id, "SHADOW", nil))

	if a := w.audits(t, "release."); len(a) != 8 || a[7]["action"] != "release.shadow" ||
		a[1]["action"] != "release.evaluation_recorded" {
		t.Fatalf("journal = %v", a)
	}
	// Evaluations are insert-only and stop at CANARY.
	wantState(t, w.f.Exec("erin", `UPDATE eacp.agent_release_evaluations SET score = 1 WHERE release_id = $1`, id), sqlForbidden)
	ok(t, w.move("ravi", id, "CANARY", 5000))
	_, err = w.f.TryID("erin", evalSQL, id, "accuracy", 0.99)
	wantState(t, err, sqlBadState)
}

// ------------------------------------------------------------- observing

func TestReplayAnswersOnlyFromTheRecordedReference(t *testing.T) {
	w := newWorld(t)
	id := w.mustOpen(t, plan{replay: 2})
	ref := w.reference(t, w.stable.Version, "carol")
	payload := registrytest.ActionPayload

	// A match returns the reference's recorded outcome.
	obs, err := w.observe("replay", ref, "carol", "erp.purchase", payload, "allow")
	ok(t, err)
	var row struct {
		release                                         uuid.UUID
		tool, outcome, payload, agrees                  bool
		candidateOutcome, referenceOutcome, referenceSt string
		recorded                                        []byte
	}
	ctx := context.Background()
	ok(t, storage.InTenantTx(ctx, w.f.Owner, w.f.Tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT release_id, tool_match, outcome_match, payload_match, agrees,
			candidate_outcome, reference_outcome, reference_state, recorded FROM eacp.agent_release_observations
			WHERE id = $1`, obs).Scan(&row.release, &row.tool, &row.outcome, &row.payload, &row.agrees,
			&row.candidateOutcome, &row.referenceOutcome, &row.referenceSt, &row.recorded)
	}))
	var rec map[string]any
	_ = json.Unmarshal(row.recorded, &rec)
	if row.release != id || !row.tool || !row.outcome || !row.payload || !row.agrees ||
		row.candidateOutcome != "allow" || row.referenceOutcome != "allow" || rec["state"] != "CANCELLED" {
		t.Fatalf("replay = %+v recorded %v", row, rec)
	}

	// A different payload is a miss: no recording answers it.
	ref2 := w.reference(t, w.stable.Version, "carol")
	obs2, err := w.observe("replay", ref2, "carol", "erp.purchase", `{"amount":5,"currency":"THB"}`, "deny")
	ok(t, err)
	var recorded []byte
	var agrees bool
	w.query(t, &recorded, `SELECT recorded FROM eacp.agent_release_observations WHERE id = $1`, obs2)
	w.query(t, &agrees, `SELECT agrees FROM eacp.agent_release_observations WHERE id = $1`, obs2)
	if recorded != nil || agrees {
		t.Fatalf("a miss was answered (%s) or agreed (%v)", recorded, agrees)
	}

	// The candidate's capability is checked against its own allowlist; a
	// denied proposal carries no verdict.
	w.f.ActiveTool(t, "crm", "lookup")
	ref3 := w.reference(t, w.stable.Version, "carol")
	_, err = w.observe("replay", ref3, "carol", "crm.lookup", payload, "allow")
	wantState(t, err, sqlCheck)
	obs3, err := w.observe("replay", ref3, "carol", "crm.lookup", payload, nil)
	ok(t, err)
	var denial string
	w.query(t, &denial, `SELECT capability_denial FROM eacp.agent_release_observations WHERE id = $1`, obs3)
	if denial != "tool_not_in_allowlist" {
		t.Fatalf("denial = %q", denial)
	}
	// An allowed proposal needs a verdict under the current policy.
	ref4 := w.reference(t, w.stable.Version, "carol")
	_, err = w.observe("replay", ref4, "carol", "erp.purchase", payload, nil)
	wantState(t, err, sqlCheck)
	_, err = w.f.TryAgentID(w.candidate, observeSQL, "replay", ref4, "carol@tenant-a.test", "erp.purchase",
		payload, "allow", 7)
	wantState(t, err, sqlBadState)

	// Each reference once; references are terminal actions of another
	// version of the same agent, for the same subject.
	_, err = w.observe("replay", ref, "carol", "erp.purchase", payload, "allow")
	wantState(t, err, sqlUnique)
	open := w.f.ReceivedAction(t, w.stable.Version, "carol", "erp.purchase")
	_, err = w.observe("replay", open, "carol", "erp.purchase", payload, "allow")
	wantState(t, err, sqlBadState)
	_, err = w.observe("replay", ref4, "amy", "erp.purchase", payload, "allow")
	wantState(t, err, sqlBadState)
	seller := w.f.ActiveAgent(t, "seller", w.tool.Tool)
	foreign := w.reference(t, seller.Version, "carol")
	_, err = w.observe("replay", foreign, "carol", "erp.purchase", payload, "allow")
	wantState(t, err, sqlBadState)
	// Shadow observations wait for SHADOW; only the candidate observes.
	_, err = w.observe("shadow", ref4, "carol", "erp.purchase", payload, "allow")
	wantState(t, err, sqlBadState)
	_, err = w.f.TryAgentID(w.stable.Version, observeSQL, "replay", ref4, "carol@tenant-a.test", "erp.purchase",
		payload, "allow", 1)
	wantState(t, err, sqlForbidden, sqlBadState)
	_, err = w.f.TryID("erin", observeSQL, "replay", ref4, "carol@tenant-a.test", "erp.purchase", payload, "allow", 1)
	wantState(t, err, sqlForbidden)

	// The replay gate compares the agreement rate: 1 of 3 cases agreed.
	w.pass(t, id, "accuracy")
	wantState(t, w.move("ravi", id, "SHADOW", nil), sqlBadState)
	_, err = w.observe("replay", ref4, "carol", "erp.purchase", payload, "allow")
	ok(t, err)
	wantState(t, w.move("ravi", id, "SHADOW", nil), sqlBadState) // 2 of 4 < 0.9
	if a := w.audits(t, "release.observation_recorded"); len(a) != 4 {
		t.Fatalf("observations journaled: %d", len(a))
	}
}

func TestShadowIsStructurallyNonDestructive(t *testing.T) {
	w := newWorld(t)
	id := w.mustOpen(t, plan{shadow: 1})
	w.pass(t, id, "accuracy")
	old := w.f.ReceivedAction(t, w.stable.Version, "carol", "erp.purchase")
	ok(t, w.move("ravi", id, "SHADOW", nil))

	// A shadow pairs with a live action of the stable version.
	live := w.f.QueuedAction(t, w.stable.Version, "carol", "erp.purchase")
	before := w.count(t, `SELECT (SELECT count(*) FROM eacp.actions) + (SELECT count(*) FROM eacp.outbox_events)
		+ (SELECT count(*) FROM eacp.action_attempts) + (SELECT count(*) FROM eacp.budget_reservations)`)
	_, err := w.observe("shadow", live, "carol", "erp.purchase", registrytest.ActionPayload, "allow")
	ok(t, err)
	after := w.count(t, `SELECT (SELECT count(*) FROM eacp.actions) + (SELECT count(*) FROM eacp.outbox_events)
		+ (SELECT count(*) FROM eacp.action_attempts) + (SELECT count(*) FROM eacp.budget_reservations)`)
	if after != before {
		t.Fatalf("a shadow observation created execution rows: %d -> %d", before, after)
	}
	// An action from before the shadow began is not a shadow reference.
	_, err = w.observe("shadow", old, "carol", "erp.purchase", registrytest.ActionPayload, "allow")
	wantState(t, err, sqlBadState)

	// The candidate cannot act for real: it is not ACTIVE, and only the
	// release's canary may activate it.
	real := w.f.ReceivedAction(t, w.candidate, "carol", "erp.purchase")
	ev := w.f.AgentID(t, w.candidate, registrytest.AllowEvidenceSQL, real)
	wantState(t, w.f.ExecAgent(w.candidate, authorizeSQL, real, ev, registrytest.ActionPayload), sqlBadState)
	wantState(t, w.f.Exec("ravi", `UPDATE eacp.agent_versions SET state = 'ACTIVE', state_reason = 'shortcut'
		WHERE id = $1`, w.candidate), sqlBadState, sqlUnique)
	ok(t, w.f.Exec("otto", `UPDATE eacp.agent_versions SET state = 'SUSPENDED', state_reason = 'pause'
		WHERE id = $1`, w.stable.Version))
	wantState(t, w.f.Exec("ravi", `UPDATE eacp.agent_versions SET state = 'ACTIVE', state_reason = 'shortcut'
		WHERE id = $1`, w.candidate), sqlBadState)
}

// ---------------------------------------------------------------- canary

func TestCanaryActivatesTheCandidateBesideTheStable(t *testing.T) {
	w := newWorld(t)
	id := w.mustOpen(t, plan{})
	w.pass(t, id, "accuracy")
	ok(t, w.move("ravi", id, "SHADOW", nil))
	// Entering CANARY is a grant: not by the candidate's creator or
	// allowlist author, and at the plan's first step.
	wantState(t, w.move("ravi", id, "CANARY", 10000), sqlBadState)
	wantState(t, w.move("otto", id, "CANARY", 5000), sqlForbidden)
	ok(t, w.move("ravi", id, "CANARY", 5000))
	if w.versionState(t, w.stable.Version) != "ACTIVE" || w.versionState(t, w.candidate) != "ACTIVE" {
		t.Fatal("stable and candidate are not both ACTIVE")
	}
	// No third ACTIVE version, and no ACTIVE version outside a canary.
	third := w.version(t, w.tool.Tool)
	wantState(t, w.f.Exec("ravi", `UPDATE eacp.agent_versions SET state = 'ACTIVE', state_reason = 'x'
		WHERE id = $1`, third), sqlUnique)
	if a := w.audits(t, "release.canary"); len(a) != 1 {
		t.Fatalf("journal = %v", a)
	}
}

func TestOneActiveVersionOutsideACanaryUnderConcurrency(t *testing.T) {
	f := registrytest.New(t)
	a := f.NewAgent(t, "racer")
	w := &world{f: f, stable: a}
	v2 := w.version(t)
	v1 := a.Version
	al := f.ID(t, "erin", `INSERT INTO eacp.agent_allowlists (tenant_id, agent_version_id, tool_ids)
		VALUES (eacp.current_tenant_id(), $1, '{}') RETURNING id`, v1)
	ok(t, f.Exec("rita", `UPDATE eacp.agent_versions SET active_allowlist_id = $1 WHERE id = $2`, al, v1))

	ctx := context.Background()
	tx1, err := f.App.Begin(ctx)
	ok(t, err)
	defer tx1.Rollback(ctx)
	_, err = tx1.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, f.Tenant.String())
	ok(t, err)
	ok(t, storage.SetActor(ctx, tx1, f.P["ravi"]))
	_, err = tx1.Exec(ctx, `UPDATE eacp.agent_versions SET state = 'ACTIVE', state_reason = 'first' WHERE id = $1`, v1)
	ok(t, err)
	done := make(chan error, 1)
	go func() {
		done <- f.Exec("ravi", `UPDATE eacp.agent_versions SET state = 'ACTIVE', state_reason = 'second' WHERE id = $1`, v2)
	}()
	select {
	case err := <-done:
		t.Fatalf("the second activation did not wait for the first: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	ok(t, tx1.Commit(ctx))
	wantState(t, <-done, sqlUnique)
}

// cohort finds a release id under which carol's bucket is below 5000 and
// amy's is not.
func cohort(t *testing.T, w *world) uuid.UUID {
	t.Helper()
	for range 200 {
		id := uuid.New()
		var in, out int
		w.query(t, &in, `SELECT eacp.release_bucket($1, $2)`, id, w.f.P["carol"])
		w.query(t, &out, `SELECT eacp.release_bucket($1, $2)`, id, w.f.P["amy"])
		if in < 5000 && out >= 5000 {
			return id
		}
	}
	t.Fatal("no release id splits carol and amy")
	return uuid.Nil
}

func TestCanaryServesOnlyItsCohort(t *testing.T) {
	w := newWorld(t)
	var b1, b2 int
	w.query(t, &b1, `SELECT eacp.release_bucket('00000000-0000-4000-8000-000000000001', '00000000-0000-4000-8000-000000000002')`)
	w.query(t, &b2, `SELECT eacp.release_bucket('00000000-0000-4000-8000-000000000001', '00000000-0000-4000-8000-000000000002')`)
	if b1 != b2 || b1 < 0 || b1 >= 10000 {
		t.Fatalf("bucket = %d, %d", b1, b2)
	}
	id := w.canary(t, cohort(t, w), plan{})

	// In the cohort: authorized and queued.
	inside := w.f.QueuedAction(t, w.candidate, "carol", "erp.purchase")
	var drift *string
	w.query(t, &drift, `SELECT eacp.dispatch_drift(a) FROM eacp.actions a WHERE id = $1`, inside)
	if drift != nil {
		t.Fatalf("dispatch drift of a cohort action = %s", *drift)
	}
	// Outside: the backstop refuses every move toward execution.
	outside := w.f.ReceivedAction(t, w.candidate, "amy", "erp.purchase")
	ev := w.f.AgentID(t, w.candidate, registrytest.AllowEvidenceSQL, outside)
	wantState(t, w.f.ExecAgent(w.candidate, authorizeSQL, outside, ev, registrytest.ActionPayload), sqlBadState)
	var denial string
	w.query(t, &denial, `SELECT eacp.release_denial($1, $2)`, w.candidate, w.f.P["amy"])
	if denial != "canary_cohort" {
		t.Fatalf("release denial = %q", denial)
	}
	w.query(t, &denial, `SELECT COALESCE(eacp.release_denial($1, NULL), 'none')`, w.candidate)
	if denial != "canary_cohort" {
		t.Fatalf("unknown subject = %q", denial)
	}
	// The stable version is not restricted, and routes are consistent.
	w.f.QueuedAction(t, w.stable.Version, "amy", "erp.purchase")
	var route uuid.UUID
	w.query(t, &route, `SELECT eacp.release_route($1, $2)`, w.stable.Agent, w.f.P["carol"])
	if route != w.candidate {
		t.Fatalf("carol routes to %s", route)
	}
	w.query(t, &route, `SELECT eacp.release_route($1, $2)`, w.stable.Agent, w.f.P["amy"])
	if route != w.stable.Version {
		t.Fatalf("amy routes to %s", route)
	}

	// After a rollback the queued cohort action is revoked before dispatch.
	ok(t, w.f.Exec("otto", moveSQL, id, "ROLLED_BACK", 5000, "latency complaints"))
	w.query(t, &denial, `SELECT eacp.dispatch_drift(a) FROM eacp.actions a WHERE id = $1`, inside)
	if denial != "agent_version_not_active" {
		t.Fatalf("drift after rollback = %q", denial)
	}
}

func TestCanaryStepsAndPromotionNeedEvidence(t *testing.T) {
	w := newWorld(t)
	id := w.canary(t, cohort(t, w), plan{canaryActions: 2})

	// Not enough candidate actions in the step yet.
	wantState(t, w.move("ravi", id, "CANARY", 10000), sqlBadState)
	w.f.QueuedAction(t, w.candidate, "carol", "erp.purchase")
	w.f.QueuedAction(t, w.candidate, "carol", "erp.purchase")
	// Only the next step, by a second approver.
	wantState(t, w.move("ravi", id, "CANARY", 7000), sqlBadState)
	wantState(t, w.move("ravi", id, "PROMOTED", 5000), sqlBadState)
	wantState(t, w.move("erin", id, "CANARY", 10000), sqlForbidden)
	wantState(t, w.move("otto", id, "CANARY", 10000), sqlForbidden)
	ok(t, w.move("rita", id, "CANARY", 10000))

	// Each step starts a new window.
	wantState(t, w.move("ravi", id, "PROMOTED", 10000), sqlBadState)
	for range 2 {
		w.f.QueuedAction(t, w.candidate, "carol", "erp.purchase")
	}
	// A breach blocks promotion: the candidate denies twice, the stable never.
	for range 2 {
		ok(t, w.f.ExecAgent(w.candidate, denySQL, w.f.ReceivedAction(t, w.candidate, "carol", "erp.purchase")))
	}
	var report map[string]any
	var raw []byte
	w.query(t, &raw, `SELECT eacp.release_canary_report($1)`, id)
	_ = json.Unmarshal(raw, &report)
	if report["sufficient"] != true || len(report["breaches"].([]any)) != 1 {
		t.Fatalf("report = %s", raw)
	}
	wantState(t, w.move("ravi", id, "PROMOTED", 10000), sqlBadState)
}

func TestPromotionSuspendsTheStable(t *testing.T) {
	w := newWorld(t)
	id := w.canary(t, cohort(t, w), plan{steps: []int{10000}})
	w.f.QueuedAction(t, w.candidate, "carol", "erp.purchase")
	wantState(t, w.move("erin", id, "PROMOTED", 10000), sqlForbidden)
	ok(t, w.move("ravi", id, "PROMOTED", 10000))
	if w.releaseState(t, id) != "PROMOTED" || w.versionState(t, w.stable.Version) != "SUSPENDED" ||
		w.versionState(t, w.candidate) != "ACTIVE" {
		t.Fatal("promotion did not hand over")
	}
	// A promoted candidate is unrestricted; the release is terminal.
	w.f.QueuedAction(t, w.candidate, "amy", "erp.purchase")
	wantState(t, w.f.Exec("otto", moveSQL, id, "ROLLED_BACK", 10000, "too late"), sqlBadState)
	if a := w.audits(t, "release.promoted"); len(a) != 1 {
		t.Fatalf("journal = %v", a)
	}
	// The next release starts from the promoted version.
	promoted := w.candidate
	w.candidate = w.version(t, w.tool.Tool)
	next := w.mustOpen(t, plan{})
	var stable uuid.UUID
	w.query(t, &stable, `SELECT stable_version_id FROM eacp.agent_releases WHERE id = $1`, next)
	if stable != promoted {
		t.Fatalf("the next release's stable is %s, want the promoted %s", stable, promoted)
	}
}

func TestRollbackIsContainment(t *testing.T) {
	w := newWorld(t)
	id := w.canary(t, uuid.Nil, plan{})
	wantState(t, w.f.Exec("carol", moveSQL, id, "ROLLED_BACK", 5000, "no"), sqlForbidden)
	wantState(t, w.f.Exec("otto", moveSQL, id, "ROLLED_BACK", 5000, " "), sqlCheck)
	ok(t, w.f.Exec("otto", moveSQL, id, "ROLLED_BACK", 5000, "errors reported"))
	if w.releaseState(t, id) != "ROLLED_BACK" || w.versionState(t, w.candidate) != "SUSPENDED" ||
		w.versionState(t, w.stable.Version) != "ACTIVE" {
		t.Fatal("rollback did not contain the candidate")
	}
	// Rolled back before CANARY: nothing to suspend.
	w.candidate = w.version(t, w.tool.Tool)
	early := w.mustOpen(t, plan{})
	ok(t, w.f.Exec("rita", moveSQL, early, "ROLLED_BACK", nil, "abandoned"))
	if w.versionState(t, w.candidate) != "REGISTERED" {
		t.Fatal("an early rollback changed the candidate")
	}
	if a := w.audits(t, "release.rolled_back"); len(a) != 2 {
		t.Fatalf("journal = %v", a)
	}
}

// ------------------------------------------------------------ guardrails

func TestGuardrailBreaches(t *testing.T) {
	w := newWorld(t)
	id := w.mustOpen(t, plan{})
	breaches := func(candidate, stable string) []string {
		t.Helper()
		var raw []byte
		w.query(t, &raw, `SELECT eacp.release_breaches(r, $2::jsonb, $3::jsonb) FROM eacp.agent_releases r
			WHERE id = $1`, id, candidate, stable)
		var out []struct {
			Guardrail string `json:"guardrail"`
		}
		ok(t, json.Unmarshal(raw, &out))
		var names []string
		for _, b := range out {
			names = append(names, b.Guardrail)
		}
		return names
	}
	base := `{"actions":100,"denied":2,"succeeded":90,"failed":2,"dispatched":92,"unknown":0,
		"latency_p95_ms":200,"cost_per_action":{"THB":10}}`
	same := func(names []string, want ...string) {
		t.Helper()
		if len(names) != len(want) {
			t.Fatalf("breaches = %v, want %v", names, want)
		}
		for i := range want {
			if names[i] != want[i] {
				t.Fatalf("breaches = %v, want %v", names, want)
			}
		}
	}
	same(breaches(base, base))
	// Within tolerance: +0.04 denials, +0.04 failures, latency x1.5, cost x1.5.
	same(breaches(`{"actions":100,"denied":6,"succeeded":88,"failed":6,"dispatched":94,"unknown":0,
		"latency_p95_ms":300,"cost_per_action":{"THB":15}}`, base))
	// Failures are FAILED / (SUCCEEDED + FAILED): 7 of 100 is within 0.0217 + 0.05.
	same(breaches(`{"actions":100,"denied":2,"succeeded":93,"failed":7,"dispatched":100,"unknown":0,
		"latency_p95_ms":200,"cost_per_action":{"THB":10}}`, base))
	same(breaches(`{"actions":100,"denied":8,"succeeded":80,"failed":12,"dispatched":92,"unknown":2,
		"latency_p95_ms":301,"cost_per_action":{"THB":15.01,"USD":1}}`, base),
		"policy_violations", "failures", "unknown_outcomes", "latency", "cost:THB", "cost:USD")
	// A stable with no data counts as zero; latency needs both.
	same(breaches(`{"actions":10,"denied":1,"succeeded":9,"failed":0,"dispatched":9,"unknown":0,
		"latency_p95_ms":900,"cost_per_action":{}}`, `{"actions":0,"denied":0,"succeeded":0,"failed":0,
		"dispatched":0,"unknown":0,"latency_p95_ms":null,"cost_per_action":{}}`), "policy_violations")
}

func TestAutomaticRollbackOnlyWithdraws(t *testing.T) {
	w := newWorld(t)
	id := w.canary(t, cohort(t, w), plan{canaryActions: 2})
	evaluate := func(component string) error {
		return w.f.ExecSystem(component, `SELECT eacp.release_evaluate()`)
	}
	// Only the release system actor evaluates.
	wantState(t, evaluate("sweeper"), sqlForbidden)
	wantState(t, w.f.Exec("otto", `SELECT eacp.release_evaluate()`), sqlForbidden)
	// The system cannot move a release itself, nor suspend versions.
	wantState(t, w.f.ExecSystem("release", moveSQL, id, "PROMOTED", 5000, "auto"), sqlForbidden)
	wantState(t, w.f.ExecSystem("release", moveSQL, id, "ROLLED_BACK", 5000, "auto"), sqlBadState)
	wantState(t, w.f.ExecSystem("release", `UPDATE eacp.agent_versions SET state = 'SUSPENDED', state_reason = 'x'
		WHERE id = $1`, w.stable.Version), sqlForbidden)
	wantState(t, w.f.ExecSystem("release", `UPDATE eacp.agent_versions SET state = 'SUSPENDED', state_reason = 'x'
		WHERE id = $1`, w.candidate), sqlForbidden)

	// Too few actions, then a sufficient report without a breach: nothing happens.
	w.f.QueuedAction(t, w.candidate, "carol", "erp.purchase")
	ok(t, evaluate("release"))
	w.f.QueuedAction(t, w.candidate, "carol", "erp.purchase")
	w.f.QueuedAction(t, w.stable.Version, "amy", "erp.purchase")
	ok(t, evaluate("release"))
	if w.releaseState(t, id) != "CANARY" {
		t.Fatal("a report without a breach rolled back")
	}

	// A breach (one of three candidate actions denied, none of the stable's) rolls back.
	ok(t, w.f.ExecAgent(w.candidate, denySQL, w.f.ReceivedAction(t, w.candidate, "carol", "erp.purchase")))
	ok(t, evaluate("release"))
	if w.releaseState(t, id) != "ROLLED_BACK" || w.versionState(t, w.candidate) != "SUSPENDED" ||
		w.versionState(t, w.stable.Version) != "ACTIVE" {
		t.Fatalf("release %s, candidate %s", w.releaseState(t, id), w.versionState(t, w.candidate))
	}
	var changedBy *uuid.UUID
	var breaches []byte
	w.query(t, &changedBy, `SELECT changed_by FROM eacp.agent_releases WHERE id = $1`, id)
	w.query(t, &breaches, `SELECT breaches FROM eacp.agent_releases WHERE id = $1`, id)
	if changedBy != nil || len(breaches) < 10 {
		t.Fatalf("changed by %v, breaches %s", changedBy, breaches)
	}
	a := w.audits(t, "release.rolled_back")
	if len(a) != 1 || a[0]["actor"].(map[string]any)["kind"] != "system" {
		t.Fatalf("journal = %v", a)
	}
	var tenants int
	w.query(t, &tenants, `SELECT count(*) FROM eacp.release_tenants()`)
	if tenants != 0 {
		t.Fatalf("tenants with canaries = %d", tenants)
	}
}

func TestReleaseTablesAreTenantScoped(t *testing.T) {
	w := newWorld(t)
	id := w.mustOpen(t, plan{})
	w.pass(t, id, "accuracy")
	b := w.f.ForTenant(t, pgtest.TenantB)
	n := -1
	ctx := context.Background()
	ok(t, storage.InTenantTx(ctx, b.App, b.Tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM eacp.agent_releases)
			+ (SELECT count(*) FROM eacp.agent_release_evaluations)
			+ (SELECT count(*) FROM eacp.agent_release_observations)`).Scan(&n)
	}))
	if n != 0 {
		t.Fatalf("tenant B sees %d release rows", n)
	}
}
