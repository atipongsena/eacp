package registry_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/registry"
	"github.com/atipongsena/eacp/internal/registry/registrytest"
	"github.com/atipongsena/eacp/internal/storage"
	"github.com/atipongsena/eacp/internal/storage/pgtest"
)

func hasVersion(items []registry.DependencyImpact, id uuid.UUID) bool {
	for _, item := range items {
		if item.VersionID == id {
			return true
		}
	}
	return false
}

func TestBlastRadiusTraversesRegistryAndDeclaredDependencies(t *testing.T) {
	f := registrytest.New(t)
	s := registry.New(f.App)
	ctx := context.Background()
	actor := registry.Actor{TenantID: f.Tenant, PrincipalID: f.P["erin"]}
	mcp := f.MCPConnector(t, "sap-mcp")
	f.Scan(t, mcp, scanResult(getPO))
	tool := toolID(t, f, mcp, "get_po")
	certify(t, f, tool)
	buyer := f.ActiveAgent(t, "buyer", tool)
	delegate := f.ActiveAgent(t, "delegate")
	unrelated := f.ActiveAgent(t, "unrelated")

	_, err := s.RecordDependency(ctx, actor, registry.DependencyInput{
		FromKind: "agent_version", FromID: delegate.Version, ToKind: "agent_version", ToID: buyer.Version,
		Source: "deployment_manifest", Confidence: "high", ObservedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	// A legitimate delegation cycle must terminate in the recursive query.
	_, err = s.RecordDependency(ctx, actor, registry.DependencyInput{
		FromKind: "agent_version", FromID: buyer.Version, ToKind: "agent_version", ToID: delegate.Version,
		Source: "deployment_manifest", Confidence: "high", ObservedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.RecordDependency(ctx, actor, registry.DependencyInput{
		FromKind: "tool", FromID: tool, ToKind: "system", ToName: "sap-production",
		Source: "operator_inventory", Confidence: "high", ObservedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []registry.DependencyTarget{
		{Kind: "mcp", ID: mcp}, {Kind: "tool", ID: tool}, {Kind: "system", Name: "sap-production"},
	} {
		report, err := s.BlastRadius(ctx, actor, target)
		if err != nil {
			t.Fatal(err)
		}
		if !hasVersion(report.Confirmed, buyer.Version) || !hasVersion(report.Confirmed, delegate.Version) || hasVersion(report.Confirmed, unrelated.Version) {
			t.Fatalf("blast radius of %+v: %+v", target, report)
		}
		if len(report.AffectedTeams) != 0 {
			t.Fatalf("principal owner is not a team: %+v", report.AffectedTeams)
		}
	}
}

func TestBlastRadiusWidensForUnknownOrStaleEvidence(t *testing.T) {
	f := registrytest.New(t)
	s := registry.New(f.App)
	ctx := context.Background()
	actor := registry.Actor{TenantID: f.Tenant, PrincipalID: f.P["erin"]}
	mcp := f.MCPConnector(t, "sap-mcp")
	stale := f.ActiveAgent(t, "stale")
	unknown := f.ActiveAgent(t, "unknown")
	future := f.ActiveAgent(t, "future")
	for _, in := range []registry.DependencyInput{
		{FromKind: "agent_version", FromID: stale.Version, ToKind: "mcp", ToID: mcp,
			Source: "inventory", Confidence: "high", ObservedAt: time.Now().Add(-2 * time.Hour), ExpiresAt: time.Now().Add(-time.Hour)},
		{FromKind: "agent_version", FromID: unknown.Version, ToKind: "mcp",
			Source: "inventory", Confidence: "unknown", ObservedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)},
		{FromKind: "agent_version", FromID: future.Version, ToKind: "mcp", ToID: mcp,
			Source: "inventory", Confidence: "high", ObservedAt: time.Now().Add(2 * time.Minute), ExpiresAt: time.Now().Add(time.Hour)},
	} {
		if _, err := s.RecordDependency(ctx, actor, in); err != nil {
			t.Fatal(err)
		}
	}
	report, err := s.BlastRadius(ctx, actor, registry.DependencyTarget{Kind: "mcp", ID: mcp})
	if err != nil {
		t.Fatal(err)
	}
	if !hasVersion(report.Possible, stale.Version) || !hasVersion(report.Possible, unknown.Version) || !hasVersion(report.Possible, future.Version) ||
		hasVersion(report.Confirmed, stale.Version) || hasVersion(report.Confirmed, unknown.Version) || hasVersion(report.Confirmed, future.Version) {
		t.Fatalf("uncertain dependencies must widen the possible set: %+v", report)
	}
}

func TestDependencyWritesAreGuardedAndTenantScoped(t *testing.T) {
	f := registrytest.New(t)
	other := f.ForTenant(t, pgtest.TenantB)
	a := f.NewAgent(t, "buyer")
	foreign := other.NewAgent(t, "foreign")
	insert := `INSERT INTO eacp.dependency_edges
		(tenant_id, from_kind, from_id, to_kind, to_id, source, confidence, observed_at, expires_at)
		VALUES (eacp.current_tenant_id(), 'agent_version', $1, 'agent_version', $2,
		'inventory', 'high', now(), now() + interval '1 hour')`
	wantState(t, f.Exec("carol", insert, a.Version, a.Version), sqlForbidden)
	wantState(t, f.Exec("erin", insert, a.Version, foreign.Version), sqlBadState, sqlForeignKey)
	wantState(t, f.Exec("erin", insert, a.Version, a.Version), sqlCheck)
	wantState(t, f.Exec("", insert, a.Version, foreign.Version), sqlForbidden)

	id := f.ID(t, "erin", `INSERT INTO eacp.dependency_edges
		(tenant_id, from_kind, from_id, to_kind, to_name, source, confidence, observed_at, expires_at, created_by)
		VALUES (eacp.current_tenant_id(), 'agent_version', $1, 'model', 'gpt-production',
		'inventory', 'high', now(), now() + interval '1 hour', $2) RETURNING id`, a.Version, f.P["carol"])
	var by uuid.UUID
	ownerRow(t, f, `SELECT created_by FROM eacp.dependency_edges WHERE id = $1`, []any{id}, &by)
	if by != f.P["erin"] {
		t.Fatalf("forged creator survived: %s", by)
	}
	wantState(t, f.Exec("erin", `UPDATE eacp.dependency_edges SET expires_at = now() + interval '2 hours' WHERE id = $1`, id), sqlForbidden)
	wantState(t, f.Exec("carol", `UPDATE eacp.dependency_edges SET revoked_at = now(), revoke_reason = 'retired'
		WHERE id = $1`, id), sqlForbidden)
	ok(t, f.Exec("erin", `UPDATE eacp.dependency_edges SET revoked_at = now(), revoke_reason = 'retired'
		WHERE id = $1`, id))
	var count int
	ownerRow(t, f, `SELECT count(*) FROM eacp.audit_events WHERE convert_from(payload, 'UTF8') LIKE '%dependency_edges%'`, nil, &count)
	if count < 2 {
		t.Fatalf("expected insert and revocation in audit, got %d", count)
	}
	err := storage.InTenantTx(context.Background(), f.App, other.Tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM eacp.dependency_edges`).Scan(&count)
	})
	if err != nil || count != 0 {
		t.Fatalf("other tenant sees %d dependency edges: %v", count, err)
	}
}
