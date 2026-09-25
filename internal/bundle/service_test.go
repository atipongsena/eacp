package bundle_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

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
