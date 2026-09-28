package incident_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/atipongsena/eacp/internal/registry/registrytest"
	"github.com/atipongsena/eacp/internal/storage"
	"github.com/atipongsena/eacp/internal/storage/pgtest"
)

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var p *pgconn.PgError
	if !errors.As(err, &p) || p.Code != code {
		t.Fatalf("err = %v, want SQLSTATE %s", err, code)
	}
}

func ok(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// manual opens a manual incident as actor and returns its id.
func manual(t *testing.T, f *registrytest.Fixture, actor, severity string) uuid.UUID {
	t.Helper()
	return f.ID(t, actor, `INSERT INTO eacp.incidents (tenant_id, kind, severity, title, detail)
		VALUES (eacp.current_tenant_id(), 'manual', $1, 'suspicious purchases', '{"reason": "triage"}') RETURNING id`,
		severity)
}

func timeline(t *testing.T, f *registrytest.Fixture, id uuid.UUID) []string {
	t.Helper()
	var out []string
	ok(t, storage.InTenantTx(context.Background(), f.Owner, f.Tenant.String(), func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), `SELECT kind FROM eacp.incident_events
			WHERE incident_id = $1 ORDER BY seq`, id)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	}))
	return out
}

func audited(t *testing.T, f *registrytest.Fixture, id uuid.UUID) int {
	t.Helper()
	var n int
	ok(t, storage.InTenantTx(context.Background(), f.Owner, f.Tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM eacp.audit_events
			WHERE convert_from(payload, 'UTF8')::jsonb #>> '{subject,type}' = 'incident'
			  AND convert_from(payload, 'UTF8')::jsonb #>> '{subject,id}' = $1::text`, id).Scan(&n)
	}))
	return n
}

func TestOnlyTheIncidentSystemActorOpensAutomaticIncidents(t *testing.T) {
	f := registrytest.New(t)
	insert := `INSERT INTO eacp.incidents (tenant_id, kind, source_key, severity, title)
		VALUES (eacp.current_tenant_id(), 'kill', 'agent:x:1', 'high', 'kill')`
	wantCode(t, f.Exec("otto", insert), "42501")
	wantCode(t, f.ExecSystem("finops", insert), "42501")
	ok(t, f.ExecSystem("incident", insert))
	wantCode(t, f.ExecSystem("incident", insert), "23505")
	wantCode(t, f.ExecSystem("incident", `INSERT INTO eacp.incidents (tenant_id, kind, severity, title)
		VALUES (eacp.current_tenant_id(), 'kill', 'high', 'no key')`), "23514")
}

func TestAManualIncidentNeedsAnOperatorAndAReason(t *testing.T) {
	f := registrytest.New(t)
	wantCode(t, f.Exec("carol", `INSERT INTO eacp.incidents (tenant_id, kind, severity, title, detail)
		VALUES (eacp.current_tenant_id(), 'manual', 'low', 'x', '{"reason": "r"}')`), "42501")
	wantCode(t, f.Exec("otto", `INSERT INTO eacp.incidents (tenant_id, kind, severity, title)
		VALUES (eacp.current_tenant_id(), 'manual', 'low', 'x')`), "23514")
	wantCode(t, f.Exec("otto", `INSERT INTO eacp.incidents (tenant_id, kind, severity, title, detail)
		VALUES (eacp.current_tenant_id(), 'manual', 'low', 'x', '{"reason": "r", "payload": "no"}')`), "23514")
	wantCode(t, f.Exec("otto", `INSERT INTO eacp.incidents (tenant_id, kind, severity, title, detail, subject_type, subject_id)
		VALUES (eacp.current_tenant_id(), 'manual', 'low', 'x', '{"reason": "r"}', 'tool', gen_random_uuid())`), "23503")
	id := manual(t, f, "alice", "low")
	if got := timeline(t, f, id); len(got) != 1 || got[0] != "opened" {
		t.Fatalf("timeline = %v", got)
	}
	if audited(t, f, id) != 1 {
		t.Fatal("opening is journaled")
	}
}

func TestTheIncidentLifecycle(t *testing.T) {
	f := registrytest.New(t)
	id := manual(t, f, "otto", "high")
	resolve := `UPDATE eacp.incidents SET state = 'RESOLVED', resolution = 'contained', resolution_reason = 'done' WHERE id = $1`
	wantCode(t, f.Exec("otto", resolve, id), "55000")
	wantCode(t, f.Exec("otto", `UPDATE eacp.incidents SET state = 'ACKNOWLEDGED' WHERE id = $1`, id), "23514")
	wantCode(t, f.Exec("carol", `UPDATE eacp.incidents SET state = 'ACKNOWLEDGED', ack_reason = 'mine' WHERE id = $1`, id), "42501")
	ok(t, f.Exec("otto", `UPDATE eacp.incidents SET state = 'ACKNOWLEDGED', ack_reason = 'mine' WHERE id = $1`, id))
	wantCode(t, f.Exec("otto", `UPDATE eacp.incidents SET severity = 'low' WHERE id = $1`, id), "42501")
	wantCode(t, f.Exec("otto", `UPDATE eacp.incidents SET state = 'RESOLVED', resolution_reason = 'done' WHERE id = $1`, id), "23514")
	ok(t, f.Exec("otto", resolve, id))
	wantCode(t, f.Exec("otto", `UPDATE eacp.incidents SET assignee_id = $2 WHERE id = $1`, id, f.P["opal"]), "55000")
	if got := timeline(t, f, id); len(got) != 3 || got[1] != "acknowledged" || got[2] != "resolved" {
		t.Fatalf("timeline = %v", got)
	}
	if audited(t, f, id) != 3 {
		t.Fatal("every move is journaled")
	}
}

func TestACriticalIncidentIsResolvedByASecondOperator(t *testing.T) {
	f := registrytest.New(t)
	id := manual(t, f, "otto", "critical")
	ok(t, f.Exec("otto", `UPDATE eacp.incidents SET state = 'ACKNOWLEDGED', ack_reason = 'mine' WHERE id = $1`, id))
	resolve := `UPDATE eacp.incidents SET state = 'RESOLVED', resolution = 'contained', resolution_reason = 'done' WHERE id = $1`
	wantCode(t, f.Exec("otto", resolve, id), "42501")
	ok(t, f.Exec("opal", resolve, id))
}

func TestAssigneesAreOperatorsOrAdmins(t *testing.T) {
	f := registrytest.New(t)
	id := manual(t, f, "otto", "medium")
	wantCode(t, f.Exec("otto", `UPDATE eacp.incidents SET assignee_id = $2 WHERE id = $1`, id, f.P["carol"]), "23514")
	ok(t, f.Exec("otto", `UPDATE eacp.incidents SET assignee_id = $2 WHERE id = $1`, id, f.P["opal"]))
	ok(t, f.Exec("opal", `UPDATE eacp.incidents SET assignee_id = NULL WHERE id = $1`, id))
	if got := timeline(t, f, id); len(got) != 3 || got[1] != "assigned" || got[2] != "assigned" {
		t.Fatalf("timeline = %v", got)
	}
}

func TestTheTimelineIsAppendOnlyAndLifecycleEventsComeFromTheIncident(t *testing.T) {
	f := registrytest.New(t)
	id := manual(t, f, "otto", "medium")
	wantCode(t, f.Exec("otto", `INSERT INTO eacp.incident_events (tenant_id, incident_id, kind, note)
		VALUES (eacp.current_tenant_id(), $1, 'acknowledged', 'forged')`, id), "42501")
	wantCode(t, f.Exec("carol", `INSERT INTO eacp.incident_events (tenant_id, incident_id, kind, note)
		VALUES (eacp.current_tenant_id(), $1, 'note', 'hi')`, id), "42501")
	wantCode(t, f.Exec("otto", `INSERT INTO eacp.incident_events (tenant_id, incident_id, kind, note)
		VALUES (eacp.current_tenant_id(), gen_random_uuid(), 'note', 'hi')`), "23503")
	ok(t, f.Exec("otto", `INSERT INTO eacp.incident_events (tenant_id, incident_id, kind, note)
		VALUES (eacp.current_tenant_id(), $1, 'note', 'looking into it')`, id))
	tool := f.ActiveTool(t, "erp", "po")
	wantCode(t, f.Exec("otto", `INSERT INTO eacp.incident_events (tenant_id, incident_id, kind, link_kind, link_id)
		VALUES (eacp.current_tenant_id(), $1, 'linked', 'tool', gen_random_uuid())`, id), "23503")
	ok(t, f.Exec("otto", `INSERT INTO eacp.incident_events (tenant_id, incident_id, kind, link_kind, link_id)
		VALUES (eacp.current_tenant_id(), $1, 'linked', 'tool', $2)`, id, tool.Tool))
	wantCode(t, f.Exec("otto", `UPDATE eacp.incident_events SET note = 'edited' WHERE incident_id = $1`, id), "42501")
	if got := timeline(t, f, id); len(got) != 3 || got[1] != "note" || got[2] != "linked" {
		t.Fatalf("timeline = %v", got)
	}
	if audited(t, f, id) != 3 {
		t.Fatal("notes and links are journaled")
	}
}

func TestIncidentsAreTenantIsolated(t *testing.T) {
	f := registrytest.New(t)
	id := manual(t, f, "otto", "low")
	other := f.ForTenant(t, pgtest.TenantB)
	var n int
	ok(t, storage.InTenantTx(context.Background(), other.App, other.Tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM eacp.incidents WHERE id = $1`, id).Scan(&n)
	}))
	if n != 0 {
		t.Fatal("tenant b sees tenant a's incident")
	}
}
