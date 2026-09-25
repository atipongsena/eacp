package action_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"eacp/internal/action"
)

// ADR-018 §4: the engine denies a canary candidate's action outside its
// cohort at T2, with a reason, instead of tripping the database backstop.
func TestCanaryCandidateIsDeniedOutsideItsCohort(t *testing.T) {
	v := newEnv(t, allowAll)
	candidate := v.f.ID(t, "erin", `INSERT INTO eacp.agent_versions (tenant_id, agent_id, runtime, code_ref)
		VALUES (eacp.current_tenant_id(), $1, 'python', 'git:next') RETURNING id`, v.agent.Agent)
	al := v.f.ID(t, "erin", `INSERT INTO eacp.agent_allowlists (tenant_id, agent_version_id, tool_ids)
		VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, candidate, []uuid.UUID{v.tool.Tool})
	if err := v.f.Exec("rita", `UPDATE eacp.agent_versions SET active_allowlist_id = $1 WHERE id = $2`, al, candidate); err != nil {
		t.Fatal(err)
	}
	// A release id that puts carol inside the first step (5000) and amy outside.
	var release uuid.UUID
	for range 200 {
		id := uuid.New()
		if v.count(`SELECT (eacp.release_bucket($1, $2) < 5000 AND eacp.release_bucket($1, $3) >= 5000)::int`,
			id, v.f.P["carol"], v.f.P["amy"]) == 1 {
			release = id
			break
		}
	}
	v.f.ID(t, "erin", `INSERT INTO eacp.agent_releases (tenant_id, id, candidate_version_id, required_suites, reason,
		min_replay_cases, min_shadow_cases, canary_steps) VALUES (eacp.current_tenant_id(), $1, $2, '{accuracy}',
		'next model', 0, 0, '{5000,10000}') RETURNING id`, release, candidate)
	v.f.ID(t, "erin", `INSERT INTO eacp.agent_release_evaluations (tenant_id, release_id, suite, score, threshold,
		dataset_digest, evidence_ref) VALUES (eacp.current_tenant_id(), $1, 'accuracy', 1, 0.5, repeat('ab', 32),
		'ci://1') RETURNING id`, release)
	actor := action.Agent(v.f.Tenant, v.agent.Agent, candidate)
	if err := v.f.Exec("ravi", `UPDATE eacp.agent_releases SET state = 'SHADOW', change_reason = 'ok' WHERE id = $1`, release); err != nil {
		t.Fatal(err)
	}
	// In shadow the candidate cannot act at all.
	sub := v.submission("shadow-1")
	got, err := v.e.Submit(context.Background(), actor, sub)
	if err != nil || got.State != "DENIED" || got.StateReason != "agent_version_not_active" {
		t.Fatalf("shadow submission = %+v, %v", got, err)
	}
	if err := v.f.Exec("ravi", `UPDATE eacp.agent_releases SET state = 'CANARY', canary_bp = 5000,
		change_reason = 'ok' WHERE id = $1`, release); err != nil {
		t.Fatal(err)
	}

	sub = v.submission("canary-in")
	got, err = v.e.Submit(context.Background(), actor, sub)
	if err != nil || got.State != "QUEUED" {
		t.Fatalf("cohort submission = %+v, %v", got, err)
	}
	sub = v.submission("canary-out")
	sub.Subject = "amy@tenant-a.test"
	got, err = v.e.Submit(context.Background(), actor, sub)
	if err != nil || got.State != "DENIED" || got.StateReason != "canary_cohort" {
		t.Fatalf("outside submission = %+v, %v", got, err)
	}
	// The stable version serves amy.
	sub.IdempotencyKey = "stable-amy"
	if got, err := v.e.Submit(context.Background(), v.actor(), sub); err != nil || got.State != "QUEUED" {
		t.Fatalf("stable submission = %+v, %v", got, err)
	}
}
