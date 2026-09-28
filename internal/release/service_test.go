package release_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/governance"
	"github.com/atipongsena/eacp/internal/registry"
	"github.com/atipongsena/eacp/internal/release"
)

// pdp is the local provider, which a test can take down, counting calls.
type pdp struct {
	down  atomic.Bool
	calls atomic.Int64
}

func (p *pdp) Evaluate(ctx context.Context, req governance.GovernanceRequest) (governance.GovernanceDecision, error) {
	p.calls.Add(1)
	if p.down.Load() {
		return governance.GovernanceDecision{}, errors.New("pdp unreachable")
	}
	return governance.LocalProvider{InstanceID: "release-test"}.Evaluate(ctx, req)
}

type svc struct {
	*world
	s   *release.Service
	pdp *pdp
}

func newSvc(t *testing.T) svc {
	t.Helper()
	w := newWorld(t)
	p := &pdp{}
	return svc{world: w, pdp: p, s: release.New(w.f.App, release.Options{Provider: p})}
}

func (v svc) principal(name string) registry.Actor {
	return registry.Actor{TenantID: v.f.Tenant, PrincipalID: v.f.P[name]}
}

func (v svc) candidate() release.Agent {
	return release.Agent{TenantID: v.f.Tenant, AgentID: v.stable.Agent, VersionID: v.world.candidate}
}

func (v svc) proposal(kind string, ref uuid.UUID, subject, tool string) release.Proposal {
	return release.Proposal{Kind: kind, ReferenceActionID: ref, Subject: subject + "@tenant-a.test",
		Operation: "purchase", Target: "erp", Tool: tool, ToolSchemaVersion: "1", Resource: "po",
		Payload: json.RawMessage(`{"currency":"THB","amount":1000000}`)}
}

func wantKind(t *testing.T, err error, kind error) {
	t.Helper()
	var re *registry.Error
	if !errors.As(err, &re) || re.Kind != kind {
		t.Fatalf("err = %v, want %v", err, kind)
	}
}

func intp(n int) *int { return &n }

func TestReleaseLifecycleThroughTheService(t *testing.T) {
	v := newSvc(t)
	ctx := context.Background()
	id := cohort(t, v.world)
	rel, err := v.s.Open(ctx, v.principal("erin"), release.OpenRequest{ID: id, CandidateVersionID: v.world.candidate,
		Reason: "model upgrade", Plan: release.Plan{RequiredSuites: []string{"accuracy"}, MinReplayCases: intp(1),
			MinShadowCases: intp(1), CanarySteps: []int{5000, 10000}, MinCanaryActions: intp(1)}})
	if err != nil || rel.State != "EVALUATING" || rel.StableVersionID != v.stable.Version || rel.MinReplayCases != 1 ||
		string(rel.MinShadowAgreement) != "0.9" || len(rel.CanarySteps) != 2 {
		t.Fatalf("open = %+v, %v", rel, err)
	}
	_, err = v.s.Open(ctx, v.principal("carol"), release.OpenRequest{CandidateVersionID: v.world.candidate, Reason: "x",
		Plan: release.Plan{RequiredSuites: []string{"accuracy"}}})
	wantKind(t, err, registry.ErrForbidden)

	ev, err := v.s.RecordEvaluation(ctx, v.principal("erin"), id, release.EvaluationInput{Suite: "accuracy",
		Score: "0.93", Threshold: "0.9", DatasetDigest: fill64("ab"), EvidenceRef: "ci://run/7"})
	if err != nil || !ev.Passed || ev.Score != "0.93" {
		t.Fatalf("evaluation = %+v, %v", ev, err)
	}
	_, err = v.s.RecordEvaluation(ctx, v.principal("erin"), id, release.EvaluationInput{Suite: "accuracy",
		Score: "NaN", Threshold: "0.9", DatasetDigest: fill64("ab"), EvidenceRef: "ci://run/7"})
	wantKind(t, err, registry.ErrInvalid)

	// Replay: the candidate proposes, the PDP decides, the database compares.
	ref := v.reference(t, v.stable.Version, "carol")
	obs, err := v.s.Observe(ctx, v.candidate(), v.proposal("replay", ref, "carol", "erp.purchase"))
	if err != nil || obs.Verdict == nil || *obs.Verdict != "allow" || !obs.Agrees || !obs.PayloadMatch ||
		obs.Recorded == nil || obs.ReleaseID != id {
		t.Fatalf("replay = %+v, %v", obs, err)
	}
	var recorded map[string]any
	_ = json.Unmarshal(obs.Recorded, &recorded)
	if recorded["state"] != "CANCELLED" {
		t.Fatalf("recorded = %s", obs.Recorded)
	}

	// Advancing names the state it expects.
	_, err = v.s.Advance(ctx, v.principal("ravi"), id, release.Advance{From: "SHADOW", Reason: "ok"})
	wantKind(t, err, registry.ErrConflict)
	rel, err = v.s.Advance(ctx, v.principal("ravi"), id, release.Advance{From: "EVALUATING", Reason: "replay agrees"})
	if err != nil || rel.State != "SHADOW" {
		t.Fatalf("advance = %+v, %v", rel, err)
	}

	// Shadow: paired with a live stable action.
	live := v.f.QueuedAction(t, v.stable.Version, "carol", "erp.purchase")
	if _, err := v.s.Observe(ctx, v.candidate(), v.proposal("shadow", live, "carol", "erp.purchase")); err != nil {
		t.Fatal(err)
	}
	rel, err = v.s.Advance(ctx, v.principal("ravi"), id, release.Advance{From: "SHADOW", Reason: "shadow agrees"})
	if err != nil || rel.State != "CANARY" || rel.CanaryBP == nil || *rel.CanaryBP != 5000 {
		t.Fatalf("canary = %+v, %v", rel, err)
	}
	_, err = v.s.Advance(ctx, v.principal("ravi"), id, release.Advance{From: "CANARY", Reason: "no bp"})
	wantKind(t, err, registry.ErrInvalid)

	// Routing follows the cohort.
	route, err := v.s.Route(ctx, v.candidate(), "carol@tenant-a.test")
	if err != nil || route.VersionID != v.world.candidate || route.Cohort != "canary" || route.ReleaseID == nil {
		t.Fatalf("carol route = %+v, %v", route, err)
	}
	route, err = v.s.Route(ctx, release.Agent{TenantID: v.f.Tenant, AgentID: v.stable.Agent, VersionID: v.stable.Version},
		"amy@tenant-a.test")
	if err != nil || route.VersionID != v.stable.Version || route.Cohort != "stable" {
		t.Fatalf("amy route = %+v, %v", route, err)
	}
	route, err = v.s.Route(ctx, v.candidate(), "nobody@tenant-a.test")
	if err != nil || route.VersionID != v.stable.Version {
		t.Fatalf("unknown subject route = %+v, %v", route, err)
	}

	// The detail shows the evidence of every stage.
	v.f.QueuedAction(t, v.world.candidate, "carol", "erp.purchase")
	d, err := v.s.Get(ctx, v.principal("audra"), id)
	if err != nil || len(d.Evaluations) != 1 || len(d.FailingSuites) != 0 || d.Canary == nil {
		t.Fatalf("detail = %+v, %v", d, err)
	}
	var replay, report map[string]any
	_ = json.Unmarshal(d.Replay, &replay)
	_ = json.Unmarshal(d.Canary, &report)
	if replay["cases"] != float64(1) || replay["agreement"] != float64(1) || report["sufficient"] != true {
		t.Fatalf("replay %s, canary %s", d.Replay, d.Canary)
	}

	five := 5000
	rel, err = v.s.Advance(ctx, v.principal("ravi"), id, release.Advance{From: "CANARY", FromCanaryBP: &five, Reason: "healthy"})
	if err != nil || *rel.CanaryBP != 10000 {
		t.Fatalf("step = %+v, %v", rel, err)
	}
	v.f.QueuedAction(t, v.world.candidate, "amy", "erp.purchase")
	full := 10000
	rel, err = v.s.Advance(ctx, v.principal("ravi"), id, release.Advance{From: "CANARY", FromCanaryBP: &full, Reason: "healthy"})
	if err != nil || rel.State != "PROMOTED" {
		t.Fatalf("promote = %+v, %v", rel, err)
	}
	list, err := v.s.List(ctx, v.principal("otto"), release.Filter{AgentID: v.stable.Agent})
	if err != nil || len(list) != 1 || list[0].State != "PROMOTED" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	_, err = v.s.Get(ctx, v.principal("audra"), uuid.New())
	wantKind(t, err, registry.ErrNotFound)
}

func TestObservationNeedsThePDPOnlyWhenTheCapabilityAllows(t *testing.T) {
	v := newSvc(t)
	ctx := context.Background()
	v.mustOpen(t, plan{replay: 5})
	ref := v.reference(t, v.stable.Version, "carol")

	// PDP down: nothing is recorded, and the caller can retry.
	v.pdp.down.Store(true)
	_, err := v.s.Observe(ctx, v.candidate(), v.proposal("replay", ref, "carol", "erp.purchase"))
	if !errors.Is(err, release.ErrGovernanceUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if n := v.count(t, `SELECT count(*) FROM eacp.agent_release_observations`); n != 0 {
		t.Fatalf("%d observations recorded without a decision", n)
	}
	// A proposal the candidate's allowlist denies needs no PDP.
	v.f.ActiveTool(t, "crm", "lookup")
	calls := v.pdp.calls.Load()
	obs, err := v.s.Observe(ctx, v.candidate(), v.proposal("replay", ref, "carol", "crm.lookup"))
	if err != nil || obs.CapabilityDenial == nil || *obs.CapabilityDenial != "tool_not_in_allowlist" ||
		obs.Verdict != nil || obs.Agrees || v.pdp.calls.Load() != calls {
		t.Fatalf("denied proposal = %+v, %v (pdp calls %d -> %d)", obs, err, calls, v.pdp.calls.Load())
	}
	v.pdp.down.Store(false)

	// Only the candidate of an open release observes; payloads are JSON.
	stable := release.Agent{TenantID: v.f.Tenant, AgentID: v.stable.Agent, VersionID: v.stable.Version}
	_, err = v.s.Observe(ctx, stable, v.proposal("replay", ref, "carol", "erp.purchase"))
	wantKind(t, err, registry.ErrForbidden)
	bad := v.proposal("replay", ref, "carol", "erp.purchase")
	bad.Payload = json.RawMessage(`{"amount":`)
	_, err = v.s.Observe(ctx, v.candidate(), bad)
	wantKind(t, err, registry.ErrInvalid)
	bad = v.proposal("rerun", ref, "carol", "erp.purchase")
	_, err = v.s.Observe(ctx, v.candidate(), bad)
	wantKind(t, err, registry.ErrInvalid)
}

func TestEvaluatorRollsBackOnlyBreachedCanaries(t *testing.T) {
	v := newSvc(t)
	ctx := context.Background()
	id := v.canary(t, cohort(t, v.world), plan{canaryActions: 2})
	if n, err := v.s.EvaluateAll(ctx); err != nil || n != 0 {
		t.Fatalf("evaluate = %d, %v", n, err)
	}
	v.f.QueuedAction(t, v.world.candidate, "carol", "erp.purchase")
	ok(t, v.f.ExecAgent(v.world.candidate, denySQL, v.f.ReceivedAction(t, v.world.candidate, "carol", "erp.purchase")))
	if n, err := v.s.EvaluateAll(ctx); err != nil || n != 1 {
		t.Fatalf("evaluate = %d, %v", n, err)
	}
	d, err := v.s.Get(ctx, v.principal("audra"), id)
	if err != nil || d.State != "ROLLED_BACK" || d.ChangedBy != nil || len(d.Breaches) == 0 ||
		d.ChangeReason != "guardrail breach: policy_violations" {
		t.Fatalf("after evaluation = %+v, %v", d.Release, err)
	}
	// A manual rollback of a closed release conflicts.
	_, err = v.s.Rollback(ctx, v.principal("otto"), id, "again")
	wantKind(t, err, registry.ErrConflict)
}

func fill64(pair string) string {
	out := ""
	for range 32 {
		out += pair
	}
	return out
}
