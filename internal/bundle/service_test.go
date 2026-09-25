package bundle_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/bundle"
	"eacp/internal/registry"
	"eacp/internal/registry/registrytest"
	"eacp/internal/storage"
)

const ledgerDoc = `{
 "connectors": {"ledger": {"protocol": "http", "endpoint": "http://fakeerp:8090", "secret_ref": "ledger",
   "tools": {"post_entry": {"contract": {"side_effects": ["READ_ONLY"], "idempotency_mode": "none",
     "reconciliation_lookup": "none", "reconciliation_consistency": "none", "proof_standard": "none",
     "max_attempts": 3}}}}},
 "agents": {"ledger-bot": {"display_name": "Ledger bot", "environment": "production", "risk_class": "high",
   "owner": {"principal": "carol"}, "version": {"runtime": "python", "code_ref": "git:aaa111"},
   "allowlist": ["ledger.post_entry"], "state": "ACTIVE"}}}`

func setup(t *testing.T) (*registrytest.Fixture, *bundle.Service) {
	t.Helper()
	f := registrytest.New(t)
	return f, bundle.New(f.App)
}

func as(f *registrytest.Fixture, name string) registry.Actor {
	return registry.Actor{TenantID: f.Tenant, PrincipalID: f.P[name]}
}

func req(doc string) bundle.Request {
	return bundle.Request{Bundle: "ledger", Desired: json.RawMessage(doc)}
}

func count(t *testing.T, f *registrytest.Fixture, sql string, args ...any) int {
	t.Helper()
	var n int
	ok(t, storage.InTenantTx(context.Background(), f.Owner, f.Tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), sql, args...).Scan(&n)
	}))
	return n
}

func wantCode(t *testing.T, err error, code string) *bundle.Error {
	t.Helper()
	var be *bundle.Error
	if !errors.As(err, &be) || be.Code != code {
		t.Fatalf("err = %v, want %s", err, code)
	}
	return be
}

func TestPlanRecordsAChangeSetAndWritesNoRegistryObject(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()

	dry := req(ledgerDoc)
	dry.DryRun = true
	p, err := s.Plan(ctx, as(f, "erin"), dry)
	ok(t, err)
	if p.ID != uuid.Nil || len(p.Steps) != 9 || count(t, f, `SELECT count(*) FROM eacp.change_sets`) != 0 {
		t.Fatalf("dry run = %+v", p)
	}

	p, err = s.Plan(ctx, as(f, "erin"), req(ledgerDoc))
	ok(t, err)
	if p.ID == uuid.Nil || p.State != "PLANNED" || len(p.Steps) != 9 || len(p.BaseDigest) != 64 ||
		len(p.DesiredDigest) != 64 || p.PlannedBy != f.P["erin"] {
		t.Fatalf("plan = %+v", p)
	}
	if n := count(t, f, `SELECT count(*) FROM eacp.connectors WHERE name = 'ledger'`) +
		count(t, f, `SELECT count(*) FROM eacp.agents WHERE name = 'ledger-bot'`); n != 0 {
		t.Fatalf("a plan wrote %d registry objects", n)
	}
	got, err := s.Get(ctx, as(f, "audra"), p.ID)
	ok(t, err)
	if got.Bundle != "ledger" || len(got.Steps) != 9 || got.Steps[1].Payload.Parent != "connector.ledger" {
		t.Fatalf("get = %+v", got)
	}
	list, err := s.List(ctx, as(f, "audra"), "ledger", 10)
	ok(t, err)
	if len(list) != 1 || list[0].ID != p.ID {
		t.Fatalf("list = %+v", list)
	}
	bundles, err := s.Bundles(ctx, as(f, "audra"))
	ok(t, err)
	if len(bundles) != 1 || bundles[0].Name != "ledger" {
		t.Fatalf("bundles = %+v", bundles)
	}
}

func TestBlockedPlansAreNotRecorded(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()
	_, err := s.Plan(ctx, as(f, "erin"), req(strings.Replace(ledgerDoc, "carol", "nobody", 1)))
	be := wantCode(t, err, bundle.CodePlanBlocked)
	if len(be.Findings) == 0 || be.Findings[0].Kind != bundle.KindUnresolvedReference {
		t.Fatalf("findings = %+v", be.Findings)
	}
	_, err = s.Plan(ctx, as(f, "erin"), req(strings.Replace(ledgerDoc, "git:aaa111", "${var.ref}", 1)))
	wantCode(t, err, bundle.CodePlanBlocked)
	_, err = s.Plan(ctx, as(f, "erin"), bundle.Request{Bundle: "Bad Name", Desired: json.RawMessage(ledgerDoc)})
	var re *registry.Error
	if !errors.As(err, &re) || !errors.Is(err, registry.ErrInvalid) {
		t.Fatalf("bad bundle name: %v", err)
	}
	if n := count(t, f, `SELECT count(*) FROM eacp.change_sets`); n != 0 {
		t.Fatalf("%d change sets recorded", n)
	}
}

func TestReplanningSupersedesAPlannedChangeSet(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()
	first, err := s.Plan(ctx, as(f, "erin"), req(ledgerDoc))
	ok(t, err)
	second, err := s.Plan(ctx, as(f, "rita"), req(ledgerDoc))
	ok(t, err)
	got, err := s.Get(ctx, as(f, "erin"), first.ID)
	ok(t, err)
	if got.State != "SUPERSEDED" || !strings.Contains(got.CloseReason, second.ID.String()) {
		t.Fatalf("first = %+v", got)
	}
	_, err = s.Plan(ctx, as(f, "carol"), req(ledgerDoc))
	if !errors.Is(err, registry.ErrForbidden) {
		t.Fatalf("carol planned: %v", err)
	}
}

// apply plans ledgerDoc as erin, submits it as erin and approves it as rita.
func apply(t *testing.T, f *registrytest.Fixture, s *bundle.Service, doc string) bundle.ChangeSet {
	t.Helper()
	ctx := context.Background()
	p, err := s.Plan(ctx, as(f, "erin"), req(doc))
	ok(t, err)
	_, err = s.Submit(ctx, as(f, "erin"), p.ID)
	ok(t, err)
	cs, err := s.Approve(ctx, as(f, "rita"), p.ID)
	ok(t, err)
	return cs
}

func TestSubmitAndApproveApplyTheBundle(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()
	p, err := s.Plan(ctx, as(f, "erin"), req(ledgerDoc))
	ok(t, err)
	sub, err := s.Submit(ctx, as(f, "erin"), p.ID)
	ok(t, err)
	if sub.State != "SUBMITTED" || len(sub.SealedDigest) != 64 || *sub.SubmittedBy != f.P["erin"] {
		t.Fatalf("submitted = %+v", sub)
	}
	if n := count(t, f, `SELECT count(*) FROM eacp.agent_versions v JOIN eacp.agents a ON a.id = v.agent_id
		WHERE a.name = 'ledger-bot' AND v.state = 'REGISTERED' AND v.active_allowlist_id IS NULL`); n != 1 {
		t.Fatalf("after submit: %d registered versions without an allowlist", n)
	}
	_, err = s.Submit(ctx, as(f, "erin"), p.ID)
	if !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("second submit: %v", err)
	}

	cs, err := s.Approve(ctx, as(f, "rita"), p.ID)
	ok(t, err)
	if cs.State != "APPLIED" || *cs.ApprovedBy != f.P["rita"] {
		t.Fatalf("applied = %+v", cs)
	}
	for _, st := range cs.Steps {
		if st.ObjectID == nil {
			t.Fatalf("step %d did not run", st.Ordinal)
		}
	}
	if n := count(t, f, `SELECT count(*) FROM eacp.agent_versions v JOIN eacp.agents a ON a.id = v.agent_id
		WHERE a.name = 'ledger-bot' AND v.state = 'ACTIVE' AND v.active_allowlist_id IS NOT NULL`); n != 1 {
		t.Fatal("the version is not ACTIVE with an allowlist")
	}
	if n := count(t, f, `SELECT count(*) FROM eacp.bundle_resources`); n != 4 {
		t.Fatalf("managed objects = %d, want connector, tool, agent and version", n)
	}
	_, err = s.Approve(ctx, as(f, "ravi"), p.ID)
	if !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("second approve: %v", err)
	}
	again, err := s.Plan(ctx, as(f, "erin"), req(ledgerDoc))
	ok(t, err)
	if again.ID != uuid.Nil || len(again.Steps) != 0 {
		t.Fatalf("replan after apply = %+v", again)
	}
}

// grant gives name an approved role, as the schema owner (bootstrap path).
func grant(t *testing.T, f *registrytest.Fixture, name, role string) {
	t.Helper()
	ok(t, storage.InTenantTx(context.Background(), f.Owner, f.Tenant.String(), func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `INSERT INTO eacp.role_grants (tenant_id, principal_id, role, approved_at)
			VALUES (eacp.current_tenant_id(), $1, $2, now())`, f.P[name], role)
		return err
	}))
}

func TestTheSubmitterCannotApprove(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()
	grant(t, f, "rita", "registry_editor") // an approver who also writes the registry
	p, err := s.Plan(ctx, as(f, "rita"), req(ledgerDoc))
	ok(t, err)
	_, err = s.Submit(ctx, as(f, "rita"), p.ID)
	ok(t, err)
	_, err = s.Approve(ctx, as(f, "rita"), p.ID)
	wantCode(t, err, bundle.CodeSamePrincipal)
	_, err = s.Approve(ctx, as(f, "erin"), p.ID) // an editor is not an approver
	if !errors.Is(err, registry.ErrForbidden) {
		t.Fatalf("editor approve: %v", err)
	}
	_, err = s.Approve(ctx, as(f, "ravi"), p.ID)
	ok(t, err)
}

func TestAStaleChangeSetRunsNothing(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()
	p, err := s.Plan(ctx, as(f, "erin"), req(ledgerDoc))
	ok(t, err)
	ok(t, f.Exec("alice", `UPDATE eacp.principals SET disabled_at = now(), disable_reason = 'left'
		WHERE name = 'carol'`))
	_, err = s.Submit(ctx, as(f, "erin"), p.ID)
	wantCode(t, err, bundle.CodeStale)
	if n := count(t, f, `SELECT count(*) FROM eacp.connectors WHERE name = 'ledger'`); n != 0 {
		t.Fatal("a stale change set created the connector")
	}

	// Stale at approval: an operator quarantines the new version meanwhile.
	f2, s2 := setup(t)
	p, err = s2.Plan(ctx, as(f2, "erin"), req(ledgerDoc))
	ok(t, err)
	_, err = s2.Submit(ctx, as(f2, "erin"), p.ID)
	ok(t, err)
	ok(t, f2.Exec("otto", `UPDATE eacp.agent_versions SET state = 'QUARANTINED', state_reason = 'incident'`))
	_, err = s2.Approve(ctx, as(f2, "rita"), p.ID)
	wantCode(t, err, bundle.CodeStale)
}

func TestAFailingStepRollsBackTheStage(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()
	// Native idempotency without a key field: the contract CHECK refuses it.
	doc := strings.Replace(ledgerDoc, `"idempotency_mode": "none"`, `"idempotency_mode": "native"`, 1)
	p, err := s.Plan(ctx, as(f, "erin"), req(doc))
	ok(t, err)
	_, err = s.Submit(ctx, as(f, "erin"), p.ID)
	be := wantCode(t, err, bundle.CodeStepFailed)
	if be.Address != "contract.ledger.post_entry" || !errors.Is(err, registry.ErrInvalid) {
		t.Fatalf("step failure = %+v", be)
	}
	if n := count(t, f, `SELECT count(*) FROM eacp.connectors WHERE name = 'ledger'`); n != 0 {
		t.Fatal("the stage was not rolled back")
	}
	got, err := s.Get(ctx, as(f, "erin"), p.ID)
	ok(t, err)
	if got.State != "PLANNED" {
		t.Fatalf("state after a failed submit = %s", got.State)
	}
}

func TestRejectAndTheOpenChangeSetLock(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()
	p, err := s.Plan(ctx, as(f, "erin"), req(ledgerDoc))
	ok(t, err)
	_, err = s.Submit(ctx, as(f, "erin"), p.ID)
	ok(t, err)
	_, err = s.Plan(ctx, as(f, "erin"), req(ledgerDoc))
	wantCode(t, err, bundle.CodeOpen)
	_, err = s.Reject(ctx, as(f, "rita"), p.ID, " ")
	if !errors.Is(err, registry.ErrInvalid) {
		t.Fatalf("reject without reason: %v", err)
	}
	cs, err := s.Reject(ctx, as(f, "rita"), p.ID, "wrong owner")
	ok(t, err)
	if cs.State != "REJECTED" || cs.CloseReason != "wrong owner" {
		t.Fatalf("rejected = %+v", cs)
	}
	// The proposals stay inert; a new plan starts from what the submit created.
	next, err := s.Plan(ctx, as(f, "erin"), req(ledgerDoc))
	ok(t, err)
	if next.ID == uuid.Nil {
		t.Fatal("no plan after rejection")
	}
}

func TestAnObjectBelongsToOneBundle(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()
	apply(t, f, s, ledgerDoc)
	var conn uuid.UUID
	ok(t, storage.InTenantTx(ctx, f.Owner, f.Tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM eacp.connectors WHERE name = 'ledger'`).Scan(&conn)
	}))
	other := `{"connectors": {"ledger": {"protocol": "http", "endpoint": "http://fakeerp:8090", "secret_ref": "ledger"}},
		"imports": [{"to": "connector.ledger", "id": "` + conn.String() + `"}]}`
	_, err := s.Plan(ctx, as(f, "erin"), bundle.Request{Bundle: "finance", Desired: json.RawMessage(other)})
	be := wantCode(t, err, bundle.CodePlanBlocked)
	if be.Findings[0].Kind != bundle.KindUnmanaged || !strings.Contains(be.Findings[0].Detail, "ledger") {
		t.Fatalf("findings = %+v", be.Findings)
	}
}

func TestDriftObservesAndNeverChanges(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()
	_, err := s.Drift(ctx, as(f, "audra"), "ledger")
	if !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("drift before any apply: %v", err)
	}
	applied := apply(t, f, s, ledgerDoc)
	status := func() map[string]string {
		t.Helper()
		d, err := s.Drift(ctx, as(f, "audra"), "ledger")
		ok(t, err)
		if d.ChangeSetID != applied.ID {
			t.Fatalf("drift of %s, want %s", d.ChangeSetID, applied.ID)
		}
		out := map[string]string{}
		for _, e := range d.Entries {
			out[e.Address] = e.Status
		}
		return out
	}
	for addr, st := range status() {
		if st != bundle.DriftInSync {
			t.Fatalf("%s is %s right after apply", addr, st)
		}
	}
	ok(t, f.Exec("otto", `UPDATE eacp.agent_versions SET state = 'SUSPENDED', state_reason = 'paused'`))
	if got := status()["version.ledger-bot"]; got != bundle.DriftModified {
		t.Fatalf("suspended version drift = %q", got)
	}
	ok(t, f.Exec("rita", `UPDATE eacp.agent_versions SET state = 'RETIRED', state_reason = 'gone'`))
	// Drift only reads: it adds no audit event and changes no change set.
	before := count(t, f, `SELECT count(*) FROM eacp.audit_events`)
	if got := status()["version.ledger-bot"]; got != bundle.DriftMissing {
		t.Fatalf("retired version drift = %q", got)
	}
	if after := count(t, f, `SELECT count(*) FROM eacp.audit_events`); after != before {
		t.Fatalf("drift wrote %d audit events", after-before)
	}
	if n := count(t, f, `SELECT count(*) FROM eacp.change_sets WHERE state <> 'APPLIED'`); n != 0 {
		t.Fatalf("drift left %d other change sets", n)
	}
}

// submittedPrune applies ledgerDoc with a REGISTERED version, then plans
// and submits (erin) a prune that retires it.
func submittedPrune(t *testing.T) (*registrytest.Fixture, *bundle.Service, uuid.UUID, uuid.UUID) {
	t.Helper()
	f, s := setup(t)
	ctx := context.Background()
	apply(t, f, s, strings.Replace(ledgerDoc, `, "state": "ACTIVE"`, ``, 1))
	var version uuid.UUID
	ok(t, storage.InTenantTx(ctx, f.Owner, f.Tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT v.id FROM eacp.agent_versions v JOIN eacp.agents a ON a.id = v.agent_id
			WHERE a.name = 'ledger-bot' AND v.state = 'REGISTERED'`).Scan(&version)
	}))
	connectorOnly := ledgerDoc[:strings.Index(ledgerDoc, `,
 "agents"`)] + "}"
	r := req(connectorOnly)
	r.Prune = true
	p, err := s.Plan(ctx, as(f, "erin"), r)
	ok(t, err)
	if len(p.Steps) != 1 || p.Steps[0].Op != bundle.OpTransition || p.Steps[0].Payload.From != registry.StateRegistered {
		t.Fatalf("prune plan = %+v", p.Steps)
	}
	_, err = s.Submit(ctx, as(f, "erin"), p.ID)
	ok(t, err)
	return f, s, p.ID, version
}

func wantActive(t *testing.T, f *registrytest.Fixture, version uuid.UUID) {
	t.Helper()
	if n := count(t, f, `SELECT count(*) FROM eacp.agent_versions WHERE id = $1 AND state = 'ACTIVE'`, version); n != 1 {
		t.Fatal("an ACTIVE version was retired by a prune planned before it was activated")
	}
}

// A prune planned while a version was REGISTERED must not retire it after
// it became ACTIVE: the managed objects' states are part of the digest.
func TestPruneDoesNotRetireAVersionActivatedAfterThePlan(t *testing.T) {
	f, s, id, version := submittedPrune(t)
	ok(t, f.Exec("rita", `UPDATE eacp.agent_versions SET state = 'ACTIVE', state_reason = 'go live' WHERE id = $1`, version))
	_, err := s.Approve(context.Background(), as(f, "ravi"), id)
	wantCode(t, err, bundle.CodeStale)
	wantActive(t, f, version)
}

// The same move committed between the approval's digest check and its step:
// the step moves the version only from the state it was planned from.
func TestPruneDoesNotRetireAVersionActivatedDuringTheApproval(t *testing.T) {
	f, s, id, version := submittedPrune(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, f.DB.AdminDSN)
	ok(t, err)
	defer admin.Close(ctx)

	updated, release, activated := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		activated <- storage.InTenantTx(ctx, f.App, f.Tenant.String(), func(tx pgx.Tx) error {
			if err := storage.SetActor(ctx, tx, f.P["rita"]); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE eacp.agent_versions SET state = 'ACTIVE', state_reason = 'go live'
				WHERE id = $1`, version); err != nil {
				return err
			}
			close(updated)
			<-release
			return nil
		})
	}()
	select {
	case <-updated:
	case err := <-activated:
		t.Fatal(err)
	}
	approved := make(chan error, 1)
	go func() {
		_, err := s.Approve(ctx, as(f, "ravi"), id)
		approved <- err
	}()
	// Wait until the approval blocks on the version row, then commit.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		ok(t, admin.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting))
		if waiting > 0 {
			break
		}
		select {
		case err := <-approved:
			close(release)
			t.Fatalf("the approval finished before the activation committed: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("the approval never waited on the version")
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(release)
	ok(t, <-activated)
	wantCode(t, <-approved, bundle.CodeStale)
	wantActive(t, f, version)
}
