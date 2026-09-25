# Phase 22a Incidents and the Agent SOC Read Model — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a PostgreSQL incident domain (automatic incidents from existing signals, an operator lifecycle and an insert-only journaled timeline) and a read-only SOC summary, exposed through the API and eacpctl.

**Architecture:** Migration 00022 adds `eacp.incidents` and `eacp.incident_events` with guards, a timeline trigger and an audit trigger. It also adds `eacp.incident_evaluate()`, which the `incident` system actor runs per tenant from controlplane-api, and moves the ADR-015 blast-radius CTE into `eacp.blast_radius_versions()`, which `registry.BlastRadius` and the evaluator share. `internal/incident` wraps it (service, evaluator loop, summary); `internal/api` and `cmd/eacpctl` expose it.

**Tech Stack:** Go 1.27, pgx v5, PostgreSQL 16 (goose migrations).

**Spec:** `docs/superpowers/specs/2026-09-25-agent-soc-incidents-design.md`

## Global Constraints

- An incident never blocks, contains, releases or decides; responses stay with the existing APIs (spec §2.1).
- PostgreSQL computes severity, title, detail and the affected snapshot of every automatic incident; Go sends none of them (§2.2).
- Every incident event is journaled in the same transaction, last (§2.3).
- `detail` carries ids, states, counts, epochs and DB-generated text only: no action payload, `state_reason`, kill reason text or secret (§2.4).
- Nothing is added to action, kill, circuit or scan transactions (§2.5).
- `critical` incidents are resolved by a principal other than the acknowledger.
- Tests first; run with `-race` and `EACP_TEST_ADMIN_DSN="postgres://postgres:postgres@127.0.0.1:55432/postgres?sslmode=disable"`.
- Never weaken a test or a fail-closed rule. Commit as the user only, with no Co-Authored-By trailer.

## Review Focus

1. **The same signal must never open two incidents**, including across two evaluators and after resolution. Tests: `TestMCPDriftOpensOneCriticalIncidentWithItsBlastRadius` (re-evaluation and a resolved occurrence) and the `ON CONFLICT` path.
2. **The resolver of a critical incident must not also be its acknowledger.** A severity changed after opening could bypass this, so severity is immutable. Tests: `TestACriticalIncidentIsResolvedByASecondOperator` and `TestTheIncidentLifecycle` (immutability).
3. **Lifecycle timeline events cannot be forged** by a direct insert. Test: `TestTheTimelineIsAppendOnlyAndLifecycleEventsComeFromTheIncident`.
4. **Operator containment is not an incident.** An operator-disabled circuit or an operator-made quarantine opens nothing. Test: `TestOperatorContainmentOpensNoIncident`.
5. **The affected snapshot equals what the blast-radius API reports** (one rule). Test: `TestTheAffectedSnapshotAgreesWithTheBlastRadius`.

---

## File Structure

| File | Responsibility |
|---|---|
| `migrations/00022_incidents.sql` (create) | tables, guards, timeline, audit, blast radius function, evaluator, tenant hint, owner scan on finops_alerts |
| `internal/registry/dependency.go` (modify) | `BlastRadius` calls `eacp.blast_radius_versions` |
| `internal/incident/incident.go` (create) | types, service (open, lifecycle, notes, links, list, get), error mapping |
| `internal/incident/evaluate.go` (create) | `Evaluate`, `EvaluateAll`, `Run` |
| `internal/incident/summary.go` (create) | the SOC read model |
| `internal/incident/*_test.go` (create) | schema, evaluator, service and summary tests |
| `internal/config/config.go` (modify), `cmd/controlplane-api/main.go` (modify) | `EACP_INCIDENT_INTERVAL`, evaluator loop |
| `internal/api/incident.go` (create), `internal/api/api.go` (modify) | routes |
| `cmd/eacpctl/incident.go` (create), `cmd/eacpctl/main.go` (modify) | `incident …`, `soc summary` |
| `internal/storage/rls_catalog_test.go`, `internal/worker/isolation_integration_test.go` (modify) | reviewed catalog, isolation sweep |
| `test/demo/slice_c_test.go`, docs (modify) | demo step C7, ADR-027, INVARIANTS, AGENTS, MASTER_PLAN, DEMO |

---

### Task 1: Incident tables, lifecycle guards, timeline and audit

**Files:**
- Create: `migrations/00022_incidents.sql`
- Test: `internal/incident/schema_test.go`

**Interfaces:**
- Produces: tables `eacp.incidents` and `eacp.incident_events`; functions `eacp.incident_object_exists(text, uuid)`, `eacp.incidents_guard()`, `eacp.incidents_timeline()`, `eacp.incident_events_guard()`, `eacp.incident_events_audit()`. `eacp_app` may update only `state, ack_reason, assignee_id, resolution, resolution_reason`.

- [ ] **Step 1: Write the failing tests**

`internal/incident/schema_test.go`:
```go
package incident_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"eacp/internal/registry/registrytest"
	"eacp/internal/storage"
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
	other := f.ForTenant(t, "b")
	var n int
	ok(t, storage.InTenantTx(context.Background(), other.App, other.Tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM eacp.incidents WHERE id = $1`, id).Scan(&n)
	}))
	if n != 0 {
		t.Fatal("tenant b sees tenant a's incident")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 ./internal/incident/`
Expected: FAIL: `relation "eacp.incidents" does not exist` (SQLSTATE 42P01) in every test.

- [ ] **Step 3: Write the migration**

`migrations/00022_incidents.sql`:
```sql
-- Phase 22a (ADR-027): incidents and the Agent SOC read model. An incident
-- observes and never decides: the incident system actor opens automatic
-- incidents from signals PostgreSQL already holds, and operators work them
-- through OPEN -> ACKNOWLEDGED -> RESOLVED. Every event is journaled.
-- +goose Up

CREATE TABLE eacp.incidents (
    tenant_id         uuid        NOT NULL REFERENCES eacp.tenants (id),
    id                uuid        NOT NULL DEFAULT gen_random_uuid(),
    kind              text        NOT NULL CHECK (kind IN ('mcp_drift', 'kill', 'circuit_open', 'unknown_outcome',
                                                           'canary_rollback', 'finops', 'manual')),
    source_key        text        CHECK (length(source_key) <= 200),
    severity          text        NOT NULL CHECK (severity IN ('low', 'medium', 'high', 'critical')),
    state             text        NOT NULL DEFAULT 'OPEN' CHECK (state IN ('OPEN', 'ACKNOWLEDGED', 'RESOLVED')),
    title             text        NOT NULL CHECK (btrim(title) <> '' AND length(title) <= 200),
    subject_type      text        CHECK (subject_type IN ('tool', 'kill_state', 'connector', 'action', 'release',
                                                          'finops_alert', 'agent', 'agent_version')),
    subject_id        uuid,
    detail            jsonb       NOT NULL DEFAULT '{}'
                                  CHECK (jsonb_typeof(detail) = 'object' AND octet_length(detail::text) <= 16384),
    affected          jsonb       NOT NULL DEFAULT '{}'
                                  CHECK (jsonb_typeof(affected) = 'object' AND octet_length(affected::text) <= 65536),
    opened_by         uuid,
    opened_at         timestamptz NOT NULL DEFAULT now(),
    acknowledged_by   uuid,
    acknowledged_at   timestamptz,
    ack_reason        text        CHECK (length(ack_reason) <= 1024),
    assignee_id       uuid,
    resolved_by       uuid,
    resolved_at       timestamptz,
    resolution        text        CHECK (resolution IN ('contained', 'false_positive', 'accepted_risk', 'duplicate')),
    resolution_reason text        CHECK (length(resolution_reason) <= 1024),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, kind, source_key),
    CHECK ((kind = 'manual') = (source_key IS NULL)),
    CHECK ((subject_type IS NULL) = (subject_id IS NULL)),
    CHECK ((acknowledged_at IS NULL) = (acknowledged_by IS NULL) AND (acknowledged_at IS NULL) = (ack_reason IS NULL)),
    CHECK ((resolved_at IS NULL) = (resolved_by IS NULL) AND (resolved_at IS NULL) = (resolution IS NULL)
           AND (resolved_at IS NULL) = (resolution_reason IS NULL)),
    CHECK (state <> 'OPEN' OR (acknowledged_at IS NULL AND resolved_at IS NULL)),
    CHECK (state <> 'ACKNOWLEDGED' OR (acknowledged_at IS NOT NULL AND resolved_at IS NULL)),
    CHECK (state <> 'RESOLVED' OR (acknowledged_at IS NOT NULL AND resolved_at IS NOT NULL)),
    FOREIGN KEY (tenant_id, opened_by) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, acknowledged_by) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, assignee_id) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, resolved_by) REFERENCES eacp.principals (tenant_id, id)
);
CREATE INDEX incidents_open ON eacp.incidents (tenant_id, opened_at) WHERE state <> 'RESOLVED';

CREATE TABLE eacp.incident_events (
    tenant_id   uuid        NOT NULL,
    id          uuid        NOT NULL DEFAULT gen_random_uuid(),
    incident_id uuid        NOT NULL,
    seq         integer     NOT NULL CHECK (seq > 0),
    kind        text        NOT NULL CHECK (kind IN ('opened', 'acknowledged', 'assigned', 'resolved', 'note', 'linked')),
    actor_id    uuid,
    at          timestamptz NOT NULL DEFAULT now(),
    note        text        CHECK (length(note) <= 4096),
    link_kind   text        CHECK (link_kind IN ('action', 'kill_state', 'fleet_operation', 'release', 'change_set',
                                                 'tool', 'connector', 'agent', 'agent_version', 'finops_alert')),
    link_id     uuid,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, incident_id, seq),
    FOREIGN KEY (tenant_id, incident_id) REFERENCES eacp.incidents (tenant_id, id),
    FOREIGN KEY (tenant_id, actor_id) REFERENCES eacp.principals (tenant_id, id),
    CHECK ((link_kind IS NULL) = (link_id IS NULL)),
    CHECK ((kind = 'linked') = (link_kind IS NOT NULL)),
    CHECK (kind <> 'note' OR btrim(note) <> '')
);

-- +goose StatementBegin
-- Whether a registry or execution object of kind exists in this tenant.
CREATE FUNCTION eacp.incident_object_exists(p_kind text, p_id uuid) RETURNS boolean
    LANGUAGE plpgsql STABLE
    AS $$
BEGIN
    RETURN CASE p_kind
        WHEN 'action' THEN EXISTS (SELECT 1 FROM eacp.actions WHERE id = p_id)
        WHEN 'kill_state' THEN EXISTS (SELECT 1 FROM eacp.kill_states WHERE id = p_id)
        WHEN 'fleet_operation' THEN EXISTS (SELECT 1 FROM eacp.fleet_operations WHERE id = p_id)
        WHEN 'release' THEN EXISTS (SELECT 1 FROM eacp.agent_releases WHERE id = p_id)
        WHEN 'change_set' THEN EXISTS (SELECT 1 FROM eacp.change_sets WHERE id = p_id)
        WHEN 'tool' THEN EXISTS (SELECT 1 FROM eacp.tools WHERE id = p_id)
        WHEN 'connector' THEN EXISTS (SELECT 1 FROM eacp.connectors WHERE id = p_id)
        WHEN 'agent' THEN EXISTS (SELECT 1 FROM eacp.agents WHERE id = p_id)
        WHEN 'agent_version' THEN EXISTS (SELECT 1 FROM eacp.agent_versions WHERE id = p_id)
        WHEN 'finops_alert' THEN EXISTS (SELECT 1 FROM eacp.finops_alerts WHERE id = p_id)
        ELSE false
    END;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Automatic incidents are opened only by the incident system actor; manual
-- ones by an operator or admin with a reason. An incident then moves OPEN ->
-- ACKNOWLEDGED -> RESOLVED or is (re)assigned, one move per statement, by an
-- operator or admin; a critical one is resolved by someone other than its
-- acknowledger. Everything else is immutable.
CREATE FUNCTION eacp.incidents_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.current_actor_id();
    changed text[];
BEGIN
    IF eacp.current_agent_version_id() IS NOT NULL THEN
        RAISE EXCEPTION 'agents do not work incidents' USING ERRCODE = '42501';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.kind = 'manual' THEN
            IF a IS NULL THEN
                RAISE EXCEPTION 'a manual incident is opened by a principal' USING ERRCODE = '42501';
            END IF;
            PERFORM eacp.assert_role(a, 'operator', 'admin');
            PERFORM eacp.require_reason(NEW.detail ->> 'reason', 'a manual incident');
            IF NEW.detail - 'reason' <> '{}'::jsonb THEN
                RAISE EXCEPTION 'a manual incident carries only its reason' USING ERRCODE = '23514';
            END IF;
            IF NEW.subject_id IS NOT NULL AND NOT eacp.incident_object_exists(NEW.subject_type, NEW.subject_id) THEN
                RAISE EXCEPTION 'no such % in this tenant', NEW.subject_type USING ERRCODE = '23503';
            END IF;
            NEW.affected := '{}';
        ELSIF eacp.current_system_actor() IS DISTINCT FROM 'incident' OR a IS NOT NULL THEN
            RAISE EXCEPTION 'automatic incidents are opened by the incident evaluator' USING ERRCODE = '42501';
        END IF;
        IF NEW.assignee_id IS NOT NULL OR NEW.state <> 'OPEN' THEN
            RAISE EXCEPTION 'an incident opens OPEN and unassigned' USING ERRCODE = '23514';
        END IF;
        NEW.opened_by := a;
        NEW.opened_at := now();
        RETURN NEW;
    END IF;

    IF a IS NULL THEN
        RAISE EXCEPTION 'incidents are worked by a principal' USING ERRCODE = '42501';
    END IF;
    PERFORM eacp.assert_role(a, 'operator', 'admin');
    IF OLD.state = 'RESOLVED' THEN
        RAISE EXCEPTION 'incident % is resolved', OLD.id USING ERRCODE = '55000';
    END IF;
    changed := ARRAY(SELECT e.key FROM jsonb_each(to_jsonb(NEW)) e
                      WHERE to_jsonb(OLD) -> e.key IS DISTINCT FROM e.value ORDER BY 1);
    IF OLD.state = 'OPEN' AND NEW.state = 'ACKNOWLEDGED'
       AND changed <@ ARRAY['state', 'ack_reason', 'acknowledged_by', 'acknowledged_at'] THEN
        PERFORM eacp.require_reason(NEW.ack_reason, 'an acknowledgement');
        NEW.acknowledged_by := a;
        NEW.acknowledged_at := now();
    ELSIF OLD.state = 'ACKNOWLEDGED' AND NEW.state = 'RESOLVED'
          AND changed <@ ARRAY['state', 'resolution', 'resolution_reason', 'resolved_by', 'resolved_at'] THEN
        IF NEW.resolution IS NULL THEN
            RAISE EXCEPTION 'a resolution needs a code' USING ERRCODE = '23514';
        END IF;
        PERFORM eacp.require_reason(NEW.resolution_reason, 'a resolution');
        IF OLD.severity = 'critical' THEN
            PERFORM eacp.assert_distinct(a, OLD.acknowledged_by, 'the acknowledger of a critical incident');
        END IF;
        NEW.resolved_by := a;
        NEW.resolved_at := now();
    ELSIF changed = ARRAY['assignee_id'] THEN
        IF NEW.assignee_id IS NOT NULL AND NOT EXISTS (
            SELECT 1 FROM eacp.principals p
              JOIN eacp.role_grants g ON g.tenant_id = p.tenant_id AND g.principal_id = p.id
             WHERE p.id = NEW.assignee_id AND p.disabled_at IS NULL AND g.role IN ('operator', 'admin')
               AND g.approved_at IS NOT NULL AND g.revoked_at IS NULL) THEN
            RAISE EXCEPTION 'an incident is assigned to an enabled operator or admin' USING ERRCODE = '23514';
        END IF;
    ELSE
        RAISE EXCEPTION 'an incident moves OPEN -> ACKNOWLEDGED -> RESOLVED or is assigned; nothing else changes'
            USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Every opening and move of an incident appends its timeline event.
CREATE FUNCTION eacp.incidents_timeline() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    INSERT INTO eacp.incident_events (tenant_id, incident_id, kind, actor_id, note)
    VALUES (NEW.tenant_id, NEW.id,
            CASE WHEN TG_OP = 'INSERT' THEN 'opened'
                 WHEN NEW.state <> OLD.state THEN lower(NEW.state)
                 ELSE 'assigned' END,
            CASE WHEN TG_OP = 'INSERT' THEN NEW.opened_by ELSE eacp.current_actor_id() END,
            CASE WHEN TG_OP = 'INSERT' THEN COALESCE(NEW.detail ->> 'reason', NEW.title)
                 WHEN NEW.state = 'ACKNOWLEDGED' AND OLD.state = 'OPEN' THEN NEW.ack_reason
                 WHEN NEW.state = 'RESOLVED' THEN NEW.resolution || ': ' || NEW.resolution_reason
                 ELSE COALESCE('assigned to ' || NEW.assignee_id::text, 'unassigned') END);
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The timeline is insert-only. Lifecycle events come only from the incident
-- itself; notes and links from an operator or admin, in any state. Each
-- event takes the next sequence number under the incident's row lock.
CREATE FUNCTION eacp.incident_events_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.current_actor_id();
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'the incident timeline is append-only' USING ERRCODE = '42501';
    END IF;
    PERFORM 1 FROM eacp.incidents WHERE tenant_id = NEW.tenant_id AND id = NEW.incident_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'no such incident' USING ERRCODE = '23503';
    END IF;
    IF NEW.kind IN ('note', 'linked') THEN
        IF a IS NULL OR eacp.current_agent_version_id() IS NOT NULL THEN
            RAISE EXCEPTION 'notes and links are added by a principal' USING ERRCODE = '42501';
        END IF;
        PERFORM eacp.assert_role(a, 'operator', 'admin');
        IF NEW.kind = 'linked' AND NOT eacp.incident_object_exists(NEW.link_kind, NEW.link_id) THEN
            RAISE EXCEPTION 'no such % in this tenant', NEW.link_kind USING ERRCODE = '23503';
        END IF;
        NEW.actor_id := a;
    ELSIF pg_trigger_depth() < 2 THEN
        RAISE EXCEPTION 'lifecycle events are written by the incident itself' USING ERRCODE = '42501';
    END IF;
    NEW.seq := COALESCE((SELECT max(seq) FROM eacp.incident_events
                          WHERE tenant_id = NEW.tenant_id AND incident_id = NEW.incident_id), 0) + 1;
    NEW.at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.incident_events_audit() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    INSERT INTO eacp.audit_events (tenant_id, payload)
    VALUES (NEW.tenant_id, convert_to(jsonb_build_object(
        'v', 1,
        'actor', CASE WHEN NEW.actor_id IS NULL
                      THEN jsonb_build_object('kind', 'system', 'id', '00000000-0000-0000-0000-000000000000'::uuid,
                                              'component', 'incident')
                      ELSE jsonb_build_object('kind', 'principal', 'id', NEW.actor_id) END,
        'action', 'incident.' || NEW.kind,
        'subject', jsonb_build_object('type', 'incident', 'id', NEW.incident_id),
        'reason', COALESCE(NEW.note, NEW.kind),
        'data', to_jsonb(NEW) || CASE WHEN NEW.kind = 'opened' THEN
                   (SELECT jsonb_build_object('incident_kind', i.kind, 'severity', i.severity, 'title', i.title,
                                              'source_key', i.source_key)
                      FROM eacp.incidents i WHERE i.tenant_id = NEW.tenant_id AND i.id = NEW.incident_id)
                 ELSE '{}'::jsonb END)::text, 'UTF8'));
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER incidents_guard BEFORE INSERT OR UPDATE ON eacp.incidents
    FOR EACH ROW EXECUTE FUNCTION eacp.incidents_guard();
CREATE TRIGGER incidents_timeline AFTER INSERT OR UPDATE ON eacp.incidents
    FOR EACH ROW EXECUTE FUNCTION eacp.incidents_timeline();
CREATE TRIGGER incident_events_guard BEFORE INSERT OR UPDATE OR DELETE ON eacp.incident_events
    FOR EACH ROW EXECUTE FUNCTION eacp.incident_events_guard();
CREATE TRIGGER zz_audit AFTER INSERT ON eacp.incident_events
    FOR EACH ROW EXECUTE FUNCTION eacp.incident_events_audit();

ALTER TABLE eacp.incidents ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.incidents FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.incidents USING (tenant_id = eacp.current_tenant_id());
ALTER TABLE eacp.incident_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.incident_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.incident_events USING (tenant_id = eacp.current_tenant_id());

REVOKE UPDATE ON eacp.incidents FROM eacp_app;
GRANT UPDATE (state, ack_reason, assignee_id, resolution, resolution_reason) ON eacp.incidents TO eacp_app;
REVOKE UPDATE ON eacp.incident_events FROM eacp_app;

-- +goose Down
DROP TABLE eacp.incident_events;
DROP TABLE eacp.incidents;
DROP FUNCTION eacp.incident_events_audit();
DROP FUNCTION eacp.incident_events_guard();
DROP FUNCTION eacp.incidents_timeline();
DROP FUNCTION eacp.incidents_guard();
DROP FUNCTION eacp.incident_object_exists(text, uuid);
```

- [ ] **Step 4: Run the tests**

Run: `go test -race -count=1 ./internal/incident/ ./internal/storage/`
Expected: `internal/incident` PASS. `internal/storage` FAILS only in the RLS catalog test, which reports the two new tables. That test is updated in Task 6, and the ledger records a ruling that the failure is expected until then.

- [ ] **Step 5: Commit**

```bash
git add migrations/00022_incidents.sql internal/incident/schema_test.go
git commit -m "feat(incident): incident tables, lifecycle guards, journaled timeline (migration 00022)"
```

---

### Task 2: One blast-radius rule and the incident evaluator

**Files:**
- Modify: `migrations/00022_incidents.sql` (Up, before `-- +goose Down`; and Down)
- Modify: `internal/registry/dependency.go`
- Test: `internal/incident/evaluate_test.go`

**Interfaces:**
- Consumes: Task 1 tables.
- Produces: `eacp.blast_radius_versions(text)`, `eacp.incident_scope_nodes(text, uuid)`, `eacp.incident_affected(text[])`, `eacp.incident_severity(text, jsonb)`, `eacp.incident_micros(timestamptz)`, `eacp.incident_open(...)`, `eacp.incident_evaluate()`, `eacp.incident_tenants()`, and the `finops_alerts owner_scan` policy.

- [ ] **Step 1: Write the failing tests**

`internal/incident/evaluate_test.go`:
```go
package incident_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/registry"
	"eacp/internal/registry/registrytest"
	"eacp/internal/storage"
)

// replica runs sql as the superuser with triggers and foreign keys off, to
// stage a signal row the evaluator only reads.
func replica(t *testing.T, f *registrytest.Fixture, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, f.DB.AdminDSN)
	ok(t, err)
	defer admin.Close(ctx)
	_, err = admin.Exec(ctx, `SET session_replication_role = replica`)
	ok(t, err)
	_, err = admin.Exec(ctx, sql, args...)
	ok(t, err)
}

// evaluate runs the evaluator as the incident system actor.
func evaluate(t *testing.T, f *registrytest.Fixture) int {
	t.Helper()
	var n int
	ok(t, storage.InTenantTx(context.Background(), f.App, f.Tenant.String(), func(tx pgx.Tx) error {
		if err := storage.SetSystem(context.Background(), tx, "incident"); err != nil {
			return err
		}
		return tx.QueryRow(context.Background(), `SELECT eacp.incident_evaluate()`).Scan(&n)
	}))
	return n
}

type row struct {
	ID          uuid.UUID
	Kind        string
	Severity    string
	SubjectType string
	SubjectID   uuid.UUID
	Affected    map[string]any
}

func incidents(t *testing.T, f *registrytest.Fixture, kind string) []row {
	t.Helper()
	var out []row
	ok(t, storage.InTenantTx(context.Background(), f.Owner, f.Tenant.String(), func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), `SELECT id, kind, severity, subject_type, subject_id, affected::text
			FROM eacp.incidents WHERE kind = $1 ORDER BY opened_at, id`, kind)
		if err != nil {
			return err
		}
		for rows.Next() {
			var r row
			var aff string
			if err := rows.Scan(&r.ID, &r.Kind, &r.Severity, &r.SubjectType, &r.SubjectID, &aff); err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(aff), &r.Affected); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	}))
	return out
}

func confirmedVersions(r row) []string {
	var out []string
	for _, x := range r.Affected["confirmed"].([]any) {
		out = append(out, x.(map[string]any)["version_id"].(string))
	}
	return out
}

const scannerQuarantine = `UPDATE eacp.tools SET quarantined_at = now() + $2::interval,
	quarantine_reason = 'definition 2 changed: input_schema', quarantine_changed_at = now(),
	quarantine_changed_by = NULL, quarantine_changed_by_worker = 'worker-1' WHERE id = $1`

func TestTheEvaluatorRunsOnlyAsTheIncidentSystemActor(t *testing.T) {
	f := registrytest.New(t)
	wantCode(t, f.Exec("otto", `SELECT eacp.incident_evaluate()`), "42501")
	wantCode(t, f.ExecSystem("finops", `SELECT eacp.incident_evaluate()`), "42501")
	if n := evaluate(t, f); n != 0 {
		t.Fatalf("an empty tenant opened %d incidents", n)
	}
}

func TestMCPDriftOpensOneCriticalIncidentWithItsBlastRadius(t *testing.T) {
	f := registrytest.New(t)
	tool := f.ActiveTool(t, "erp", "po")
	agent := f.ActiveAgent(t, "buyer", tool.Tool)
	replica(t, f, scannerQuarantine, tool.Tool, "0 seconds")
	if n := evaluate(t, f); n != 1 {
		t.Fatalf("opened %d, want 1", n)
	}
	if n := evaluate(t, f); n != 0 {
		t.Fatalf("re-evaluation opened %d", n)
	}
	got := incidents(t, f, "mcp_drift")
	if len(got) != 1 || got[0].Severity != "critical" || got[0].SubjectType != "tool" || got[0].SubjectID != tool.Tool ||
		!slices.Contains(confirmedVersions(got[0]), agent.Version.String()) || got[0].Affected["production_active"] != 1.0 {
		t.Fatalf("incident = %+v", got)
	}
	id := got[0].ID
	ok(t, f.Exec("otto", `UPDATE eacp.incidents SET state = 'ACKNOWLEDGED', ack_reason = 'killing' WHERE id = $1`, id))
	ok(t, f.Exec("opal", `UPDATE eacp.incidents SET state = 'RESOLVED', resolution = 'contained',
		resolution_reason = 'tool recertified' WHERE id = $1`, id))
	if n := evaluate(t, f); n != 0 {
		t.Fatalf("a resolved occurrence reopened: %d", n)
	}
	replica(t, f, scannerQuarantine, tool.Tool, "1 second")
	if n := evaluate(t, f); n != 1 {
		t.Fatalf("a new occurrence opened %d, want 1", n)
	}
}

func TestKillsCircuitsUnknownOutcomesRollbacksAndFinOpsOpenIncidents(t *testing.T) {
	f := registrytest.New(t)
	tool := f.ActiveTool(t, "erp", "purchase")
	agent := f.ActiveAgent(t, "buyer", tool.Tool)
	ok(t, f.Exec("otto", `SELECT eacp.set_kill('tool', $1, true, 'contain the buyer', 'security_incident')`, tool.Tool))
	replica(t, f, `UPDATE eacp.connector_circuits SET open_until = now() + interval '5 minutes', disabled = false,
		changed_by = NULL, changed_by_worker = 'worker-1', changed_at = now(), reason = 'breaker opened'
		WHERE connector_id = $1`, tool.Connector)
	action := f.ReceivedAction(t, agent.Version, "carol", "erp.purchase")
	replica(t, f, `UPDATE eacp.actions SET state = 'NEEDS_HUMAN_RESOLUTION' WHERE id = $1`, action)
	candidate := f.ID(t, "erin", `INSERT INTO eacp.agent_versions (tenant_id, agent_id, runtime, code_ref)
		VALUES (eacp.current_tenant_id(), $1, 'python', 'git:def456') RETURNING id`, agent.Agent)
	replica(t, f, `INSERT INTO eacp.agent_releases (tenant_id, agent_id, stable_version_id, candidate_version_id, state,
		required_suites, reason, stage_started_at, created_by, created_at, changed_at, closed_xact, breaches)
		VALUES ($1, $2, $3, $4, 'ROLLED_BACK', '{}', 'canary', now(), $5, now(), now(), pg_current_xact_id(),
		'{"failure_rate": 0.3}')`, f.Tenant, agent.Agent, agent.Version, candidate, f.P["ravi"])
	replica(t, f, `INSERT INTO eacp.finops_alerts (tenant_id, kind, subject_type, subject_id, unit, period_start)
		VALUES ($1, 'spend_anomaly', 'agent', $2, 'USD', now())`, f.Tenant, agent.Agent)
	if n := evaluate(t, f); n != 5 {
		t.Fatalf("opened %d, want 5", n)
	}
	if n := evaluate(t, f); n != 0 {
		t.Fatalf("re-evaluation opened %d", n)
	}
	for kind, severity := range map[string]string{"kill": "critical", "circuit_open": "medium",
		"unknown_outcome": "high", "canary_rollback": "critical", "finops": "medium"} {
		if got := incidents(t, f, kind); len(got) != 1 || got[0].Severity != severity {
			t.Errorf("%s = %+v, want one %s", kind, got, severity)
		}
	}
	if got := incidents(t, f, "unknown_outcome"); len(got) == 1 && got[0].SubjectID != action {
		t.Errorf("unknown outcome subject = %s", got[0].SubjectID)
	}
}

func TestOperatorContainmentOpensNoIncident(t *testing.T) {
	f := registrytest.New(t)
	tool := f.ActiveTool(t, "erp", "po")
	f.ActiveAgent(t, "buyer", tool.Tool)
	replica(t, f, `UPDATE eacp.tools SET quarantined_at = now(), quarantine_reason = 'operator hold',
		quarantine_changed_at = now(), quarantine_changed_by = $2, quarantine_changed_by_worker = NULL
		WHERE id = $1`, tool.Tool, f.P["rita"])
	replica(t, f, `UPDATE eacp.connector_circuits SET disabled = true, changed_by = $2, changed_by_worker = NULL,
		changed_at = now(), reason = 'maintenance' WHERE connector_id = $1`, tool.Connector, f.P["otto"])
	if n := evaluate(t, f); n != 0 {
		t.Fatalf("operator containment opened %d incidents", n)
	}
}

func TestTheAffectedSnapshotAgreesWithTheBlastRadius(t *testing.T) {
	f := registrytest.New(t)
	tool := f.ActiveTool(t, "erp", "po")
	f.ActiveAgent(t, "buyer", tool.Tool)
	f.ActiveAgent(t, "seller", tool.Tool)
	replica(t, f, scannerQuarantine, tool.Tool, "0 seconds")
	evaluate(t, f)
	report, err := registry.New(f.App).BlastRadius(context.Background(),
		registry.Actor{TenantID: f.Tenant, PrincipalID: f.P["otto"]}, registry.DependencyTarget{Kind: "tool", ID: tool.Tool})
	ok(t, err)
	var want []string
	for _, c := range report.Confirmed {
		want = append(want, c.VersionID.String())
	}
	got := confirmedVersions(incidents(t, f, "mcp_drift")[0])
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) || len(got) != 2 {
		t.Fatalf("affected %v, blast radius %v", got, want)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 ./internal/incident/ -run 'Evaluator|Drift|Kills|Containment|Affected'`
Expected: FAIL: `function eacp.incident_evaluate() does not exist` (42883).

- [ ] **Step 3: Add the evaluator to the migration**

Insert this block before `-- +goose Down` in `migrations/00022_incidents.sql`:
```sql
-- +goose StatementBegin
-- The ADR-015 blast radius of one graph node ('tool:<id>', 'mcp:<id>',
-- 'agent_version:<id>', 'model:<name>', 'system:<name>') for every agent
-- version: confirmed through trusted edges, possible through any edge, with
-- every stale, low-confidence or unknown edge widening possible impact.
-- registry.BlastRadius and the incident evaluator share it.
CREATE FUNCTION eacp.blast_radius_versions(p_node text)
    RETURNS TABLE (version_id uuid, confirmed boolean, possible boolean)
    LANGUAGE sql STABLE
    AS $$
    WITH RECURSIVE edges(src, dst, trusted) AS (
        SELECT 'agent_version:' || v.id::text, 'tool:' || t.id::text, true
        FROM eacp.agent_versions v
        JOIN eacp.agent_allowlists al ON al.tenant_id = v.tenant_id AND al.id = v.active_allowlist_id
        JOIN eacp.tools t ON t.tenant_id = al.tenant_id AND t.id = ANY(al.tool_ids)
        UNION ALL
        SELECT 'agent_version:' || v.id::text, 'mcp:' || c.id::text, true
        FROM eacp.agent_versions v
        JOIN eacp.agent_allowlists al ON al.tenant_id = v.tenant_id AND al.id = v.active_allowlist_id
        JOIN eacp.tools t ON t.tenant_id = al.tenant_id AND t.id = ANY(al.tool_ids)
        JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id AND c.protocol = 'mcp'
        UNION ALL
        SELECT d.from_kind || ':' || d.from_id::text,
               d.to_kind || ':' || COALESCE(d.to_id::text, d.to_name),
               d.confidence = 'high' AND d.observed_at <= now() AND d.expires_at > now()
        FROM eacp.dependency_edges d WHERE d.revoked_at IS NULL AND (d.to_id IS NOT NULL OR d.to_name IS NOT NULL)
    ), uncertain(node) AS (
        SELECT d.from_kind || ':' || d.from_id::text FROM eacp.dependency_edges d
        WHERE d.revoked_at IS NULL AND (d.confidence <> 'high' OR d.observed_at > now() OR d.expires_at <= now()
            OR (d.to_id IS NULL AND d.to_name IS NULL))
    ), confirmed(node) AS (
        SELECT p_node UNION SELECT e.src FROM edges e JOIN confirmed c ON e.dst = c.node WHERE e.trusted
    ), possible(node) AS (
        SELECT p_node UNION SELECT node FROM uncertain
        UNION SELECT e.src FROM edges e JOIN possible p ON e.dst = p.node
    )
    SELECT v.id, EXISTS (SELECT 1 FROM confirmed c WHERE c.node = 'agent_version:' || v.id::text),
           EXISTS (SELECT 1 FROM possible p WHERE p.node = 'agent_version:' || v.id::text)
    FROM eacp.agent_versions v
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The graph nodes a kill scope, a connector or an agent covers.
CREATE FUNCTION eacp.incident_scope_nodes(p_scope text, p_id uuid) RETURNS text[]
    LANGUAGE sql STABLE
    AS $$
    SELECT CASE p_scope
        WHEN 'agent_version' THEN ARRAY['agent_version:' || p_id::text]
        WHEN 'tool' THEN ARRAY['tool:' || p_id::text]
        WHEN 'agent' THEN ARRAY(SELECT 'agent_version:' || v.id::text FROM eacp.agent_versions v
                                 WHERE v.agent_id = p_id ORDER BY v.version)
        WHEN 'connector' THEN ARRAY(SELECT n FROM (
                                  SELECT 'mcp:' || c.id::text AS n FROM eacp.connectors c
                                   WHERE c.id = p_id AND c.protocol = 'mcp'
                                  UNION ALL
                                  SELECT 'tool:' || t.id::text FROM eacp.tools t WHERE t.connector_id = p_id) x
                                ORDER BY n)
        ELSE ARRAY[]::text[]
    END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The affected snapshot of an incident: the union of the nodes' blast radii.
CREATE FUNCTION eacp.incident_affected(p_nodes text[]) RETURNS jsonb
    LANGUAGE sql STABLE
    AS $$
    WITH r AS (
        SELECT b.version_id, bool_or(b.confirmed) AS confirmed, bool_or(b.possible) AS possible
          FROM unnest(COALESCE(p_nodes, ARRAY[]::text[])) AS n(node)
          CROSS JOIN LATERAL eacp.blast_radius_versions(n.node) b
         GROUP BY b.version_id
    ), c AS (
        SELECT v.id AS version_id, v.version, v.state, ag.id AS agent_id, ag.name AS agent, ag.environment
          FROM r JOIN eacp.agent_versions v ON v.id = r.version_id
          JOIN eacp.agents ag ON ag.tenant_id = v.tenant_id AND ag.id = v.agent_id
         WHERE r.confirmed
    )
    SELECT jsonb_build_object(
        'nodes', to_jsonb(COALESCE(p_nodes, ARRAY[]::text[])),
        'confirmed', COALESCE((SELECT jsonb_agg(jsonb_build_object('agent_id', x.agent_id, 'agent', x.agent,
                                   'version_id', x.version_id, 'version', x.version, 'environment', x.environment,
                                   'state', x.state) ORDER BY x.agent, x.version)
                                 FROM (SELECT * FROM c ORDER BY agent, version LIMIT 200) x), '[]'::jsonb),
        'confirmed_count', (SELECT count(*) FROM c),
        'possible_count', (SELECT count(*) FROM r WHERE r.possible AND NOT r.confirmed),
        'production_active', (SELECT count(*) FROM c WHERE c.environment = 'production' AND c.state = 'ACTIVE'))
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- critical when a confirmed affected version is ACTIVE on a production agent.
CREATE FUNCTION eacp.incident_severity(p_base text, p_affected jsonb) RETURNS text
    LANGUAGE sql IMMUTABLE
    AS $$
    SELECT CASE WHEN COALESCE((p_affected ->> 'production_active')::int, 0) > 0 THEN 'critical' ELSE p_base END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.incident_micros(p timestamptz) RETURNS text
    LANGUAGE sql STABLE
    AS $$ SELECT (extract(epoch FROM p) * 1000000)::bigint::text $$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Opens one automatic incident unless its occurrence already has one.
CREATE FUNCTION eacp.incident_open(p_kind text, p_key text, p_severity text, p_title text, p_subject_type text,
                                   p_subject_id uuid, p_detail jsonb, p_affected jsonb) RETURNS integer
    LANGUAGE plpgsql
    AS $$
DECLARE
    k integer;
BEGIN
    INSERT INTO eacp.incidents (tenant_id, kind, source_key, severity, title, subject_type, subject_id, detail, affected)
    VALUES (eacp.current_tenant_id(), p_kind, p_key, p_severity, left(p_title, 200), p_subject_type, p_subject_id,
            p_detail, p_affected)
    ON CONFLICT (tenant_id, kind, source_key) DO NOTHING;
    GET DIAGNOSTICS k = ROW_COUNT;
    RETURN k;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The evaluator (ADR-027 §4): one incident per current signal occurrence.
-- It reads the signal tables without locks and writes only incidents, so it
-- never touches an action, kill, circuit or scan transaction. It returns the
-- number of incidents opened.
CREATE FUNCTION eacp.incident_evaluate() RETURNS integer
    LANGUAGE plpgsql
    AS $$
DECLARE
    opened integer := 0;
    s record;
    key text;
    aff jsonb;
BEGIN
    IF eacp.current_system_actor() IS DISTINCT FROM 'incident' OR eacp.current_actor_id() IS NOT NULL THEN
        RAISE EXCEPTION 'the incident evaluator runs as the incident system actor' USING ERRCODE = '42501';
    END IF;

    -- A tool the scanner quarantined: its certified definition changed.
    FOR s IN SELECT t.id, t.name, c.name AS connector, t.quarantined_at, t.quarantine_reason
               FROM eacp.tools t JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
              WHERE t.quarantined_at IS NOT NULL AND t.quarantine_changed_by_worker IS NOT NULL ORDER BY t.id LOOP
        key := format('tool:%s:%s', s.id, eacp.incident_micros(s.quarantined_at));
        CONTINUE WHEN EXISTS (SELECT 1 FROM eacp.incidents WHERE kind = 'mcp_drift' AND source_key = key);
        aff := eacp.incident_affected(ARRAY['tool:' || s.id::text]);
        opened := opened + eacp.incident_open('mcp_drift', key, eacp.incident_severity('high', aff),
            format('MCP tool %s.%s quarantined: its definition changed', s.connector, s.name), 'tool', s.id,
            jsonb_build_object('connector', s.connector, 'tool', s.name, 'quarantined_at', s.quarantined_at,
                               'quarantine_reason', s.quarantine_reason), aff);
    END LOOP;

    -- An active kill scope (reason code and epoch only, never the reason).
    FOR s IN SELECT k.id, k.scope, k.target_id, k.epoch, k.reason_code FROM eacp.kill_states k
              WHERE k.killed ORDER BY k.id LOOP
        key := format('%s:%s:%s', s.scope, s.target_id, s.epoch);
        CONTINUE WHEN EXISTS (SELECT 1 FROM eacp.incidents WHERE kind = 'kill' AND source_key = key);
        aff := eacp.incident_affected(eacp.incident_scope_nodes(s.scope, s.target_id));
        opened := opened + eacp.incident_open('kill', key,
            CASE WHEN s.scope = 'tenant' THEN 'critical' ELSE eacp.incident_severity('high', aff) END,
            format('Kill switch active on %s %s (%s)', s.scope, s.target_id, s.reason_code), 'kill_state', s.id,
            jsonb_build_object('scope', s.scope, 'target_id', s.target_id, 'epoch', s.epoch,
                               'reason_code', s.reason_code), aff);
    END LOOP;

    -- A circuit a worker's breaker opened (an operator disabling one is not).
    FOR s IN SELECT cc.connector_id, c.name, cc.open_until, cc.changed_at FROM eacp.connector_circuits cc
               JOIN eacp.connectors c ON c.tenant_id = cc.tenant_id AND c.id = cc.connector_id
              WHERE cc.open_until > now() AND cc.changed_by_worker IS NOT NULL AND NOT cc.disabled
              ORDER BY cc.connector_id LOOP
        key := format('connector:%s:%s', s.connector_id, eacp.incident_micros(s.changed_at));
        CONTINUE WHEN EXISTS (SELECT 1 FROM eacp.incidents WHERE kind = 'circuit_open' AND source_key = key);
        aff := eacp.incident_affected(eacp.incident_scope_nodes('connector', s.connector_id));
        opened := opened + eacp.incident_open('circuit_open', key, 'medium',
            format('Circuit open on connector %s', s.name), 'connector', s.connector_id,
            jsonb_build_object('connector', s.name, 'open_until', s.open_until, 'changed_at', s.changed_at), aff);
    END LOOP;

    -- An action whose outcome only a human can settle.
    FOR s IN SELECT a.id, a.agent_version_id, a.tool FROM eacp.actions a
              WHERE a.state = 'NEEDS_HUMAN_RESOLUTION' ORDER BY a.id LOOP
        key := 'action:' || s.id::text;
        CONTINUE WHEN EXISTS (SELECT 1 FROM eacp.incidents WHERE kind = 'unknown_outcome' AND source_key = key);
        aff := eacp.incident_affected(ARRAY['agent_version:' || s.agent_version_id::text]);
        opened := opened + eacp.incident_open('unknown_outcome', key, 'high',
            format('Action %s (%s) needs a human: its outcome is unknown', s.id, s.tool), 'action', s.id,
            jsonb_build_object('action_id', s.id, 'tool', s.tool, 'agent_version_id', s.agent_version_id), aff);
    END LOOP;

    -- A canary the release system actor rolled back in the last 7 days.
    FOR s IN SELECT r.id, r.candidate_version_id, r.stable_version_id, r.breaches, ag.name, ag.environment
               FROM eacp.agent_releases r JOIN eacp.agents ag ON ag.tenant_id = r.tenant_id AND ag.id = r.agent_id
              WHERE r.state = 'ROLLED_BACK' AND r.changed_by IS NULL AND r.changed_at >= now() - interval '7 days'
              ORDER BY r.id LOOP
        key := 'release:' || s.id::text;
        CONTINUE WHEN EXISTS (SELECT 1 FROM eacp.incidents WHERE kind = 'canary_rollback' AND source_key = key);
        aff := eacp.incident_affected(ARRAY['agent_version:' || s.candidate_version_id::text]);
        opened := opened + eacp.incident_open('canary_rollback', key,
            CASE WHEN s.environment = 'production' THEN 'critical' ELSE 'high' END,
            format('Canary of %s rolled back: a guardrail was breached', s.name), 'release', s.id,
            jsonb_build_object('agent', s.name, 'candidate_version_id', s.candidate_version_id,
                               'stable_version_id', s.stable_version_id, 'breaches', s.breaches), aff);
    END LOOP;

    -- Spend over its soft limit, or an anomaly, that nobody acknowledged.
    FOR s IN SELECT fa.id, fa.kind, fa.subject_type, fa.subject_id, fa.unit, fa.observed, fa.threshold
               FROM eacp.finops_alerts fa
              WHERE fa.acknowledged_at IS NULL AND fa.kind IN ('soft_limit_exceeded', 'spend_anomaly') ORDER BY fa.id LOOP
        key := 'finops_alert:' || s.id::text;
        CONTINUE WHEN EXISTS (SELECT 1 FROM eacp.incidents WHERE kind = 'finops' AND source_key = key);
        aff := eacp.incident_affected(CASE WHEN s.subject_type = 'agent'
                                           THEN eacp.incident_scope_nodes('agent', s.subject_id) END);
        opened := opened + eacp.incident_open('finops', key, 'medium',
            format('FinOps %s on %s %s', replace(s.kind, '_', ' '), replace(s.subject_type, '_', ' '), s.subject_id),
            'finops_alert', s.id,
            jsonb_build_object('alert_kind', s.kind, 'subject_type', s.subject_type, 'subject_id', s.subject_id,
                               'unit', s.unit, 'observed', s.observed, 'threshold', s.threshold), aff);
    END LOOP;
    RETURN opened;
END
$$;
-- +goose StatementEnd

-- The incident evaluator's cross-tenant hint reads open FinOps alerts.
CREATE POLICY owner_scan ON eacp.finops_alerts FOR SELECT TO CURRENT_USER USING (true);

-- +goose StatementBegin
-- The evaluator's cross-tenant hint: tenant ids only, for tenants with a
-- current signal.
CREATE FUNCTION eacp.incident_tenants() RETURNS TABLE (tenant_id uuid)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT DISTINCT t FROM (
        SELECT tl.tenant_id FROM eacp.tools tl WHERE tl.quarantined_at IS NOT NULL
        UNION SELECT k.tenant_id FROM eacp.kill_states k WHERE k.killed
        UNION SELECT c.tenant_id FROM eacp.connector_circuits c WHERE c.open_until > now()
        UNION SELECT a.tenant_id FROM eacp.actions a WHERE a.state = 'NEEDS_HUMAN_RESOLUTION'
        UNION SELECT r.tenant_id FROM eacp.agent_releases r
               WHERE r.state = 'ROLLED_BACK' AND r.changed_at >= now() - interval '7 days'
        UNION SELECT f.tenant_id FROM eacp.finops_alerts f WHERE f.acknowledged_at IS NULL
    ) x(t) ORDER BY 1
    $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION eacp.incident_tenants() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.incident_tenants() TO eacp_app;
```

Replace the Down section with:
```sql
-- +goose Down
DROP FUNCTION eacp.incident_tenants();
DROP POLICY owner_scan ON eacp.finops_alerts;
DROP FUNCTION eacp.incident_evaluate();
DROP FUNCTION eacp.incident_open(text, text, text, text, text, uuid, jsonb, jsonb);
DROP FUNCTION eacp.incident_micros(timestamptz);
DROP FUNCTION eacp.incident_severity(text, jsonb);
DROP FUNCTION eacp.incident_affected(text[]);
DROP FUNCTION eacp.incident_scope_nodes(text, uuid);
DROP FUNCTION eacp.blast_radius_versions(text);
DROP TABLE eacp.incident_events;
DROP TABLE eacp.incidents;
DROP FUNCTION eacp.incident_events_audit();
DROP FUNCTION eacp.incident_events_guard();
DROP FUNCTION eacp.incidents_timeline();
DROP FUNCTION eacp.incidents_guard();
DROP FUNCTION eacp.incident_object_exists(text, uuid);
```

- [ ] **Step 4: One rule in Go**

In `internal/registry/dependency.go`, replace the recursive query (the comment `// PostgreSQL expands both reachable sets…` and the `tx.Query(ctx, \`WITH RECURSIVE …\`, key)` call) with:
```go
		// PostgreSQL expands both reachable sets with recursive CTEs
		// (eacp.blast_radius_versions, shared with the incident evaluator). A
		// stale, low-confidence or unknown edge seeds possible impact
		// regardless of where its target was last reported.
		rows, err = tx.Query(ctx, `SELECT version_id, confirmed, possible FROM eacp.blast_radius_versions($1)`, key)
```

- [ ] **Step 5: Run the tests**

Run: `go test -race -count=1 ./internal/incident/ ./internal/registry/ -run 'Evaluator|Drift|Kills|Containment|Affected|BlastRadius|Lifecycle|Incident|Timeline|Assignee|Manual|Critical'`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add migrations/00022_incidents.sql internal/incident/evaluate_test.go internal/registry/dependency.go
git commit -m "feat(incident): the incident evaluator and one blast-radius rule in PostgreSQL"
```

---

### Task 3: The incident service, evaluator loop and SOC summary

**Files:**
- Create: `internal/incident/incident.go`, `internal/incident/evaluate.go`, `internal/incident/summary.go`
- Test: `internal/incident/service_test.go`

**Interfaces:**
- Consumes: Tasks 1–2 schema.
- Produces: `incident.New(pool) *Service`; `Service.{Open(ctx, registry.Actor, NewIncident) (Incident, error), Get(ctx, Actor, uuid) (Incident, error), List(ctx, Actor, Filter) ([]Incident, error), Acknowledge(ctx, Actor, id, reason), Assign(ctx, Actor, id, *uuid.UUID), Resolve(ctx, Actor, id, resolution, reason), Note(ctx, Actor, id, text), Link(ctx, Actor, id, kind string, target uuid.UUID)` (each returning `(Incident, error)`), `Evaluate(ctx, tenant) (int, error)`, `EvaluateAll(ctx) (int, error)`, `Run(ctx, interval, *slog.Logger)`, `Summary(ctx, Actor) (Summary, error)}`; types `Incident`, `Event`, `NewIncident{Title, Severity, SubjectType string; SubjectID *uuid.UUID; Reason string}`, `Filter{State, Severity, Kind string; Limit int}`, `Summary`.

- [ ] **Step 1: Write the failing tests**

`internal/incident/service_test.go`:
```go
package incident_test

import (
	"context"
	"errors"
	"testing"

	"eacp/internal/incident"
	"eacp/internal/registry"
	"eacp/internal/registry/registrytest"
)

func as(f *registrytest.Fixture, name string) registry.Actor {
	return registry.Actor{TenantID: f.Tenant, PrincipalID: f.P[name]}
}

func wantKind(t *testing.T, err error, kind error) {
	t.Helper()
	if !errors.Is(err, kind) {
		t.Fatalf("err = %v, want %v", err, kind)
	}
}

func TestAnIncidentIsWorkedThroughTheService(t *testing.T) {
	f := registrytest.New(t)
	s := incident.New(f.App)
	ctx := context.Background()
	tool := f.ActiveTool(t, "erp", "po")

	_, err := s.Open(ctx, as(f, "carol"), incident.NewIncident{Title: "x", Severity: "low", Reason: "r"})
	wantKind(t, err, registry.ErrForbidden)
	_, err = s.Open(ctx, as(f, "otto"), incident.NewIncident{Title: "x", Severity: "low"})
	wantKind(t, err, registry.ErrInvalid)
	in, err := s.Open(ctx, as(f, "otto"), incident.NewIncident{Title: "odd purchases", Severity: "critical",
		SubjectType: "tool", SubjectID: &tool.Tool, Reason: "reported by finance"})
	ok(t, err)
	if in.Kind != "manual" || in.State != "OPEN" || len(in.Events) != 1 || in.OpenedBy == nil || *in.OpenedBy != f.P["otto"] {
		t.Fatalf("opened = %+v", in)
	}
	_, err = s.Resolve(ctx, as(f, "otto"), in.ID, "contained", "done")
	wantKind(t, err, registry.ErrConflict)
	opal := f.P["opal"]
	_, err = s.Assign(ctx, as(f, "otto"), in.ID, &opal)
	ok(t, err)
	_, err = s.Note(ctx, as(f, "opal"), in.ID, "checking the ERP log")
	ok(t, err)
	_, err = s.Link(ctx, as(f, "opal"), in.ID, "tool", tool.Tool)
	ok(t, err)
	_, err = s.Acknowledge(ctx, as(f, "opal"), in.ID, "mine")
	ok(t, err)
	_, err = s.Resolve(ctx, as(f, "opal"), in.ID, "contained", "done")
	wantKind(t, err, registry.ErrForbidden)
	done, err := s.Resolve(ctx, as(f, "otto"), in.ID, "false_positive", "a scheduled batch")
	ok(t, err)
	if done.State != "RESOLVED" || len(done.Events) != 6 || done.Events[5].Kind != "resolved" {
		t.Fatalf("resolved = %+v", done)
	}
	_, err = s.Get(ctx, as(f, "audra"), in.ID)
	ok(t, err)
	list, err := s.List(ctx, as(f, "audra"), incident.Filter{State: "RESOLVED"})
	ok(t, err)
	if len(list) != 1 || list[0].ID != in.ID || list[0].Events != nil {
		t.Fatalf("list = %+v", list)
	}
	_, err = s.List(ctx, as(f, "audra"), incident.Filter{Limit: 501})
	wantKind(t, err, registry.ErrInvalid)
	_, err = s.Acknowledge(ctx, as(f, "otto"), tool.Tool, "no such incident")
	wantKind(t, err, registry.ErrNotFound)
}

func TestEvaluateAllOpensIncidentsForTenantsWithSignals(t *testing.T) {
	f := registrytest.New(t)
	s := incident.New(f.App)
	tool := f.ActiveTool(t, "erp", "po")
	f.ActiveAgent(t, "buyer", tool.Tool)
	ok(t, f.Exec("otto", `SELECT eacp.set_kill('tool', $1, true, 'contain', 'security_incident')`, tool.Tool))
	n, err := s.EvaluateAll(context.Background())
	ok(t, err)
	if n < 1 {
		t.Fatalf("EvaluateAll opened %d", n)
	}
	list, err := s.List(context.Background(), as(f, "otto"), incident.Filter{Kind: "kill"})
	ok(t, err)
	if len(list) != 1 || list[0].Severity != "critical" {
		t.Fatalf("list = %+v", list)
	}
}

func TestTheSummaryCountsTheTenant(t *testing.T) {
	f := registrytest.New(t)
	s := incident.New(f.App)
	ctx := context.Background()
	tool := f.ActiveTool(t, "erp", "purchase")
	agent := f.ActiveAgent(t, "buyer", tool.Tool)
	replica(t, f, scannerQuarantine, tool.Tool, "0 seconds")
	ok(t, f.Exec("otto", `SELECT eacp.set_kill('tool', $1, true, 'contain', 'security_incident')`, tool.Tool))
	action := f.ReceivedAction(t, agent.Version, "carol", "erp.purchase")
	replica(t, f, `UPDATE eacp.actions SET state = 'NEEDS_HUMAN_RESOLUTION' WHERE id = $1`, action)
	if _, err := s.Evaluate(ctx, f.Tenant); err != nil {
		t.Fatal(err)
	}
	sum, err := s.Summary(ctx, as(f, "audra"))
	ok(t, err)
	if sum.Agents.Registered != 1 || sum.Agents.Production != 1 || sum.Agents.HighRisk != 1 ||
		sum.Agents.Versions.Active != 1 || sum.Security.QuarantinedTools != 1 || sum.Security.ActiveKills != 1 ||
		sum.Security.OpenIncidents["critical"] != 2 || sum.Security.OpenIncidents["high"] != 1 ||
		sum.Security.Unacknowledged != 3 || sum.Execution.NeedsHuman != 1 || sum.AsOf.IsZero() ||
		sum.FinOps.SpendToday == nil {
		t.Fatalf("summary = %+v", sum)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 ./internal/incident/ -run 'Service|EvaluateAll|Summary'`
Expected: FAIL to compile: `undefined: incident.New`.

- [ ] **Step 3: Implement the service**

`internal/incident/incident.go`:
```go
// Package incident is the Agent SOC's incident domain and read model
// (ADR-027). An incident observes and never decides: PostgreSQL opens
// automatic incidents from signals it already holds, and operators
// acknowledge, assign, annotate, link and resolve them under the database's
// rules. Every event is journaled.
package incident

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"eacp/internal/registry"
	"eacp/internal/storage"
)

// Incident is an incident and, from Get, its timeline.
type Incident struct {
	ID               uuid.UUID       `json:"id"`
	Kind             string          `json:"kind"`
	SourceKey        *string         `json:"source_key,omitempty"`
	Severity         string          `json:"severity"`
	State            string          `json:"state"`
	Title            string          `json:"title"`
	SubjectType      *string         `json:"subject_type,omitempty"`
	SubjectID        *uuid.UUID      `json:"subject_id,omitempty"`
	Detail           json.RawMessage `json:"detail"`
	Affected         json.RawMessage `json:"affected"`
	OpenedBy         *uuid.UUID      `json:"opened_by,omitempty"`
	OpenedAt         time.Time       `json:"opened_at"`
	AcknowledgedBy   *uuid.UUID      `json:"acknowledged_by,omitempty"`
	AcknowledgedAt   *time.Time      `json:"acknowledged_at,omitempty"`
	AckReason        *string         `json:"ack_reason,omitempty"`
	AssigneeID       *uuid.UUID      `json:"assignee_id,omitempty"`
	ResolvedBy       *uuid.UUID      `json:"resolved_by,omitempty"`
	ResolvedAt       *time.Time      `json:"resolved_at,omitempty"`
	Resolution       *string         `json:"resolution,omitempty"`
	ResolutionReason *string         `json:"resolution_reason,omitempty"`
	Events           []Event         `json:"events,omitempty"`
}

// Event is one entry of an incident's timeline.
type Event struct {
	Seq      int        `json:"seq"`
	Kind     string     `json:"kind"`
	ActorID  *uuid.UUID `json:"actor_id,omitempty"`
	At       time.Time  `json:"at"`
	Note     *string    `json:"note,omitempty"`
	LinkKind *string    `json:"link_kind,omitempty"`
	LinkID   *uuid.UUID `json:"link_id,omitempty"`
}

// NewIncident is a manual incident.
type NewIncident struct {
	Title       string     `json:"title"`
	Severity    string     `json:"severity"`
	SubjectType string     `json:"subject_type,omitempty"`
	SubjectID   *uuid.UUID `json:"subject_id,omitempty"`
	Reason      string     `json:"reason"`
}

// Filter narrows List; empty fields match everything.
type Filter struct {
	State, Severity, Kind string
	Limit                 int
}

// Service works incidents over a pool connected as the application role.
type Service struct{ pool *pgxpool.Pool }

// New returns a Service.
func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

const columns = `id, kind, source_key, severity, state, title, subject_type, subject_id, detail::text, affected::text,
	opened_by, opened_at, acknowledged_by, acknowledged_at, ack_reason, assignee_id, resolved_by, resolved_at,
	resolution, resolution_reason`

func scan(r pgx.Row) (Incident, error) {
	var i Incident
	var detail, affected string
	err := r.Scan(&i.ID, &i.Kind, &i.SourceKey, &i.Severity, &i.State, &i.Title, &i.SubjectType, &i.SubjectID,
		&detail, &affected, &i.OpenedBy, &i.OpenedAt, &i.AcknowledgedBy, &i.AcknowledgedAt, &i.AckReason,
		&i.AssigneeID, &i.ResolvedBy, &i.ResolvedAt, &i.Resolution, &i.ResolutionReason)
	i.Detail, i.Affected = json.RawMessage(detail), json.RawMessage(affected)
	return i, err
}

// Open opens a manual incident (operator or admin, with a reason).
func (s *Service) Open(ctx context.Context, a registry.Actor, n NewIncident) (Incident, error) {
	var id uuid.UUID
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO eacp.incidents (tenant_id, kind, severity, title, subject_type, subject_id, detail)
			VALUES (eacp.current_tenant_id(), 'manual', $1, $2, NULLIF($3, ''), $4, jsonb_build_object('reason', $5::text))
			RETURNING id`, n.Severity, n.Title, n.SubjectType, n.SubjectID, n.Reason).Scan(&id)
	})
	if err != nil {
		return Incident{}, err
	}
	return s.Get(ctx, a, id)
}

// Acknowledge takes an OPEN incident, with a reason.
func (s *Service) Acknowledge(ctx context.Context, a registry.Actor, id uuid.UUID, reason string) (Incident, error) {
	return s.update(ctx, a, id, `UPDATE eacp.incidents SET state = 'ACKNOWLEDGED', ack_reason = $2 WHERE id = $1`, reason)
}

// Assign sets (or, with nil, clears) the assignee of an unresolved incident.
func (s *Service) Assign(ctx context.Context, a registry.Actor, id uuid.UUID, assignee *uuid.UUID) (Incident, error) {
	return s.update(ctx, a, id, `UPDATE eacp.incidents SET assignee_id = $2 WHERE id = $1`, assignee)
}

// Resolve closes an ACKNOWLEDGED incident with a code and a reason; a
// critical one by someone other than its acknowledger.
func (s *Service) Resolve(ctx context.Context, a registry.Actor, id uuid.UUID, resolution, reason string) (Incident, error) {
	return s.update(ctx, a, id, `UPDATE eacp.incidents SET state = 'RESOLVED', resolution = $2, resolution_reason = $3
		WHERE id = $1`, resolution, reason)
}

// Note adds a note to the timeline.
func (s *Service) Note(ctx context.Context, a registry.Actor, id uuid.UUID, text string) (Incident, error) {
	return s.event(ctx, a, id, `INSERT INTO eacp.incident_events (tenant_id, incident_id, kind, note)
		VALUES (eacp.current_tenant_id(), $1, 'note', $2)`, text)
}

// Link records the evidence of a response (a kill, an action, a release…).
func (s *Service) Link(ctx context.Context, a registry.Actor, id uuid.UUID, kind string, target uuid.UUID) (Incident, error) {
	return s.event(ctx, a, id, `INSERT INTO eacp.incident_events (tenant_id, incident_id, kind, link_kind, link_id)
		VALUES (eacp.current_tenant_id(), $1, 'linked', $2, $3)`, kind, target)
}

// Get returns an incident and its timeline.
func (s *Service) Get(ctx context.Context, a registry.Actor, id uuid.UUID) (Incident, error) {
	var out Incident
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		var err error
		if out, err = scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM eacp.incidents WHERE id = $1`, id)); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT seq, kind, actor_id, at, note, link_kind, link_id FROM eacp.incident_events
			WHERE incident_id = $1 ORDER BY seq`, id)
		if err != nil {
			return err
		}
		out.Events, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Event, error) {
			var e Event
			err := r.Scan(&e.Seq, &e.Kind, &e.ActorID, &e.At, &e.Note, &e.LinkKind, &e.LinkID)
			return e, err
		})
		return err
	})
	return out, err
}

// List returns incidents, most severe and newest first (without timelines).
func (s *Service) List(ctx context.Context, a registry.Actor, f Filter) ([]Incident, error) {
	if f.Limit == 0 {
		f.Limit = 100
	}
	if f.Limit < 1 || f.Limit > 500 {
		return nil, &registry.Error{Kind: registry.ErrInvalid, Msg: "limit must be between 1 and 500"}
	}
	out := []Incident{}
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+columns+` FROM eacp.incidents
			WHERE ($1 = '' OR state = $1) AND ($2 = '' OR severity = $2) AND ($3 = '' OR kind = $3)
			ORDER BY CASE severity WHEN 'critical' THEN 0 WHEN 'high' THEN 1 WHEN 'medium' THEN 2 ELSE 3 END,
			         opened_at DESC, id
			LIMIT $4`, f.State, f.Severity, f.Kind, f.Limit)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Incident, error) { return scan(r) })
		return err
	})
	return out, err
}

func (s *Service) update(ctx context.Context, a registry.Actor, id uuid.UUID, sql string, args ...any) (Incident, error) {
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, sql, append([]any{id}, args...)...)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return &registry.Error{Kind: registry.ErrNotFound, Msg: "no such incident"}
		}
		return nil
	})
	if err != nil {
		return Incident{}, err
	}
	return s.Get(ctx, a, id)
}

func (s *Service) event(ctx context.Context, a registry.Actor, id uuid.UUID, sql string, args ...any) (Incident, error) {
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, append([]any{id}, args...)...)
		return err
	})
	if err != nil {
		return Incident{}, err
	}
	return s.Get(ctx, a, id)
}

// change runs fn as the principal in one transaction; the database
// authorises every write and journals it in the same transaction.
func (s *Service) change(ctx context.Context, a registry.Actor, fn func(pgx.Tx) error) error {
	if a.TenantID == uuid.Nil || a.PrincipalID == uuid.Nil {
		return &registry.Error{Kind: registry.ErrForbidden, Msg: "no actor"}
	}
	return classify(storage.InTenantTx(ctx, s.pool, a.TenantID.String(), func(tx pgx.Tx) error {
		if err := storage.SetActor(ctx, tx, a.PrincipalID); err != nil {
			return err
		}
		return fn(tx)
	}))
}

func (s *Service) read(ctx context.Context, a registry.Actor, fn func(pgx.Tx) error) error {
	if a.TenantID == uuid.Nil {
		return &registry.Error{Kind: registry.ErrForbidden, Msg: "no tenant"}
	}
	return classify(storage.InTenantReadTx(ctx, s.pool, a.TenantID.String(), fn))
}

func classify(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return &registry.Error{Kind: registry.ErrNotFound, Msg: "no such incident"}
	}
	var p *pgconn.PgError
	if !errors.As(err, &p) {
		return err
	}
	switch p.Code {
	case "42501":
		return &registry.Error{Kind: registry.ErrForbidden, Msg: p.Message}
	case "23514", "22P02", "23502":
		return &registry.Error{Kind: registry.ErrInvalid, Msg: p.Message}
	case "23503":
		return &registry.Error{Kind: registry.ErrNotFound, Msg: p.Message}
	case "55000", "23505":
		return &registry.Error{Kind: registry.ErrConflict, Msg: p.Message}
	default:
		return err
	}
}
```

`internal/incident/evaluate.go`:
```go
package incident

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/storage"
)

// Evaluate runs eacp.incident_evaluate() for tenant as the incident system
// actor and returns the number of incidents it opened.
func (s *Service) Evaluate(ctx context.Context, tenant uuid.UUID) (int, error) {
	var n int
	err := storage.InTenantTx(ctx, s.pool, tenant.String(), func(tx pgx.Tx) error {
		if err := storage.SetSystem(ctx, tx, "incident"); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT eacp.incident_evaluate()`).Scan(&n)
	})
	return n, err
}

// EvaluateAll evaluates every tenant with a current signal.
func (s *Service) EvaluateAll(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT tenant_id FROM eacp.incident_tenants()`)
	if err != nil {
		return 0, err
	}
	tenants, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return 0, err
	}
	total := 0
	var first error
	for _, t := range tenants {
		n, err := s.Evaluate(ctx, t)
		total += n
		if err != nil && first == nil {
			first = fmt.Errorf("incident: evaluate tenant %s: %w", t, err)
		}
	}
	return total, first
}

// Run evaluates every interval until ctx ends.
func (s *Service) Run(ctx context.Context, interval time.Duration, log *slog.Logger) {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		n, err := s.EvaluateAll(ctx)
		if err != nil && ctx.Err() == nil {
			log.ErrorContext(ctx, "incident evaluation failed", "err", err)
		} else if n > 0 {
			log.InfoContext(ctx, "incidents opened", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
```

`internal/incident/summary.go`:
```go
package incident

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"eacp/internal/registry"
)

// Summary is the Agent SOC dashboard (MASTER_PLAN §55): counters read in
// one tenant snapshot. It observes and never decides.
type Summary struct {
	AsOf      time.Time       `json:"as_of"`
	Agents    AgentCounts     `json:"agents"`
	Security  SecurityCounts  `json:"security"`
	Execution ExecutionCounts `json:"execution"`
	FinOps    FinOpsCounts    `json:"finops"`
}

type AgentCounts struct {
	Registered    int           `json:"registered"`
	Production    int           `json:"production"`
	HighRisk      int           `json:"high_risk"`
	OwnerDisabled int           `json:"owner_disabled"`
	Versions      VersionCounts `json:"versions"`
}

type VersionCounts struct {
	Active      int `json:"active"`
	Suspended   int `json:"suspended"`
	Quarantined int `json:"quarantined"`
}

type SecurityCounts struct {
	OpenIncidents    map[string]int `json:"open_incidents"`
	Unacknowledged   int            `json:"unacknowledged"`
	QuarantinedTools int            `json:"quarantined_tools"`
	ActiveKills      int            `json:"active_kills"`
	OpenCircuits     int            `json:"open_circuits"`
	DisabledCircuits int            `json:"disabled_circuits"`
	PendingApprovals int            `json:"pending_approvals"`
}

type ExecutionCounts struct {
	Queued         int `json:"queued"`
	Running        int `json:"running"`
	RetryWait      int `json:"retry_wait"`
	UnknownOutcome int `json:"unknown_outcome"`
	NeedsHuman     int `json:"needs_human"`
}

type FinOpsCounts struct {
	OpenAlerts int          `json:"open_alerts"`
	SpendToday []UnitAmount `json:"spend_today"`
}

// UnitAmount is spend in one unit; today's spend is what the FinOps
// dashboard counts (committed and held tool cost plus effective LLM cost).
type UnitAmount struct {
	Unit   string      `json:"unit"`
	Amount json.Number `json:"amount"`
}

const summarySQL = `SELECT jsonb_build_object(
	'as_of', now(),
	'agents', (SELECT jsonb_build_object(
		'registered', count(*),
		'production', count(*) FILTER (WHERE ag.environment = 'production'),
		'high_risk', count(*) FILTER (WHERE ag.risk_class IN ('high', 'critical')),
		'owner_disabled', count(*) FILTER (WHERE EXISTS (SELECT 1 FROM eacp.principals p
			WHERE p.tenant_id = ag.tenant_id AND p.id = ag.owner_principal_id AND p.disabled_at IS NOT NULL)),
		'versions', (SELECT jsonb_build_object(
			'active', count(*) FILTER (WHERE v.state = 'ACTIVE'),
			'suspended', count(*) FILTER (WHERE v.state = 'SUSPENDED'),
			'quarantined', count(*) FILTER (WHERE v.state = 'QUARANTINED')) FROM eacp.agent_versions v))
		FROM eacp.agents ag),
	'security', jsonb_build_object(
		'open_incidents', (SELECT jsonb_build_object(
			'critical', count(*) FILTER (WHERE severity = 'critical'), 'high', count(*) FILTER (WHERE severity = 'high'),
			'medium', count(*) FILTER (WHERE severity = 'medium'), 'low', count(*) FILTER (WHERE severity = 'low'))
			FROM eacp.incidents WHERE state <> 'RESOLVED'),
		'unacknowledged', (SELECT count(*) FROM eacp.incidents WHERE state = 'OPEN'),
		'quarantined_tools', (SELECT count(*) FROM eacp.tools WHERE quarantined_at IS NOT NULL),
		'active_kills', (SELECT count(*) FROM eacp.kill_states WHERE killed),
		'open_circuits', (SELECT count(*) FROM eacp.connector_circuits WHERE open_until > now() AND NOT disabled),
		'disabled_circuits', (SELECT count(*) FROM eacp.connector_circuits WHERE disabled),
		'pending_approvals', (SELECT count(*) FROM eacp.approval_requests WHERE state = 'PENDING')),
	'execution', (SELECT jsonb_build_object(
		'queued', count(*) FILTER (WHERE state = 'QUEUED'),
		'running', count(*) FILTER (WHERE state IN ('LEASED', 'EXECUTING')),
		'retry_wait', count(*) FILTER (WHERE state = 'RETRY_WAIT'),
		'unknown_outcome', count(*) FILTER (WHERE state IN ('UNKNOWN_OUTCOME', 'RECONCILING')),
		'needs_human', count(*) FILTER (WHERE state = 'NEEDS_HUMAN_RESOLUTION'))
		FROM eacp.actions WHERE state NOT IN ('SUCCEEDED', 'FAILED', 'DENIED', 'CANCELLED', 'EXPIRED')),
	'finops', jsonb_build_object(
		'open_alerts', (SELECT count(*) FROM eacp.finops_alerts WHERE acknowledged_at IS NULL),
		'spend_today', COALESCE((SELECT jsonb_agg(jsonb_build_object('unit', s.unit, 'amount', s.amount) ORDER BY s.unit)
			FROM (SELECT unit, trim_scale(sum(tool_committed + tool_held + llm_effective)) AS amount
			        FROM eacp.finops_agent_spend(date_trunc('day', now(), 'UTC'), now()) GROUP BY unit) s), '[]'::jsonb))
)::text`

// Summary reads the SOC counters of the actor's tenant.
func (s *Service) Summary(ctx context.Context, a registry.Actor) (Summary, error) {
	var out Summary
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		var raw string
		if err := tx.QueryRow(ctx, summarySQL).Scan(&raw); err != nil {
			return err
		}
		return json.Unmarshal([]byte(raw), &out)
	})
	return out, err
}
```

- [ ] **Step 4: Run the tests**

Run: `go vet ./internal/incident/ && go test -race -count=1 ./internal/incident/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/incident
git commit -m "feat(incident): incident service, evaluator loop and SOC summary"
```

---

### Task 4: API routes and the controlplane-api evaluator loop

**Files:**
- Create: `internal/api/incident.go`
- Modify: `internal/api/api.go`, `internal/config/config.go`, `cmd/controlplane-api/main.go`
- Test: `internal/api/incident_test.go`, `internal/config/config_test.go` (only if it pins the defaults)

**Interfaces:**
- Consumes: Task 3 `incident.Service`.
- Produces: the routes in spec §5; `config.Options.IncidentInterval` (`EACP_INCIDENT_INTERVAL`, default 15s, at least 5s).

- [ ] **Step 1: Write the failing test**

`internal/api/incident_test.go`:
```go
package api_test

import (
	"testing"
)

func TestIncidentsThroughTheAPI(t *testing.T) {
	h := newHarness(t)
	code, body := h.as("carol", "POST", "/v1/incidents", map[string]any{"title": "x", "severity": "low", "reason": "r"})
	h.want(403, code, body)
	code, body = h.as("otto", "POST", "/v1/incidents", map[string]any{"title": "odd purchases",
		"severity": "critical", "reason": "reported by finance"})
	h.want(201, code, body)
	id, _ := body["id"].(string)
	code, body = h.as("otto", "POST", "/v1/incidents/"+id+"/resolve", map[string]any{"resolution": "contained", "reason": "done"})
	h.want(409, code, body)
	code, body = h.as("otto", "POST", "/v1/incidents/"+id+"/notes", map[string]any{"text": "checking"})
	h.want(201, code, body)
	code, body = h.as("otto", "POST", "/v1/incidents/"+id+"/assign", map[string]any{"assignee_id": nil})
	h.want(200, code, body)
	code, body = h.as("otto", "POST", "/v1/incidents/"+id+"/acknowledge", map[string]any{"reason": "mine"})
	h.want(200, code, body)
	code, body = h.as("otto", "POST", "/v1/incidents/"+id+"/resolve", map[string]any{"resolution": "contained", "reason": "done"})
	h.want(403, code, body)
	code, body = h.as("opal", "POST", "/v1/incidents/"+id+"/resolve", map[string]any{"resolution": "contained", "reason": "done"})
	h.want(200, code, body)
	if body["state"] != "RESOLVED" || len(body["events"].([]any)) != 5 {
		t.Fatalf("resolved = %v", body)
	}
	code, body = h.as("audra", "GET", "/v1/incidents?state=RESOLVED", nil)
	h.want(200, code, body)
	if list, _ := body["incidents"].([]any); len(list) != 1 {
		t.Fatalf("list = %v", body)
	}
	code, body = h.as("audra", "GET", "/v1/incidents/"+id, nil)
	h.want(200, code, body)
	code, body = h.as("audra", "GET", "/v1/incidents?limit=0", nil)
	h.want(400, code, body)
	code, body = h.as("audra", "POST", "/v1/incidents/"+id+"/notes", map[string]any{"text": "auditors read"})
	h.want(403, code, body)
	code, body = h.as("carol", "GET", "/v1/soc/summary", nil)
	h.want(403, code, body)
	code, body = h.as("audra", "GET", "/v1/soc/summary", nil)
	h.want(200, code, body)
	if sec, _ := body["security"].(map[string]any); sec == nil || sec["open_incidents"] == nil {
		t.Fatalf("summary = %v", body)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -count=1 ./internal/api/ -run TestIncidentsThroughTheAPI`
Expected: FAIL: `status 404 (body map[]) , want 403` or similar: the routes do not exist.

- [ ] **Step 3: Implement the routes**

`internal/api/incident.go`:
```go
package api

import (
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"eacp/internal/identity"
	"eacp/internal/incident"
)

var (
	incidentWorker = []string{"operator", "admin"}
	incidentReader = []string{"operator", "auditor", "admin"}
)

func (s *Server) registerIncidents(mux *http.ServeMux) {
	p := s.principal
	mux.Handle("GET /v1/incidents", p(incidentReader, s.listIncidents))
	mux.Handle("GET /v1/incidents/{id}", p(incidentReader, s.getIncident))
	mux.Handle("POST /v1/incidents", p(incidentWorker, s.openIncident))
	mux.Handle("POST /v1/incidents/{id}/acknowledge", p(incidentWorker, s.acknowledgeIncident))
	mux.Handle("POST /v1/incidents/{id}/assign", p(incidentWorker, s.assignIncident))
	mux.Handle("POST /v1/incidents/{id}/notes", p(incidentWorker, s.noteIncident))
	mux.Handle("POST /v1/incidents/{id}/links", p(incidentWorker, s.linkIncident))
	mux.Handle("POST /v1/incidents/{id}/resolve", p(incidentWorker, s.resolveIncident))
	mux.Handle("GET /v1/soc/summary", p(incidentReader, s.socSummary))
}

func (s *Server) listIncidents(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	q := r.URL.Query()
	f := incident.Filter{State: q.Get("state"), Severity: q.Get("severity"), Kind: q.Get("kind")}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 500 {
			return badRequest{"limit must be between 1 and 500"}
		}
		f.Limit = n
	}
	list, err := s.incidents.List(r.Context(), actor(c), f)
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"incidents": list})
	}
	return err
}

func (s *Server) getIncident(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	in, err := s.incidents.Get(r.Context(), actor(c), id)
	if err == nil {
		writeJSON(w, http.StatusOK, in)
	}
	return err
}

func (s *Server) openIncident(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in incident.NewIncident
	if err := decode(r, &in); err != nil {
		return err
	}
	out, err := s.incidents.Open(r.Context(), actor(c), in)
	return created(w, out, err)
}

// incidentMove decodes the body into v, then runs fn on the path's incident.
func (s *Server) incidentMove(w http.ResponseWriter, r *http.Request, status int, v any,
	fn func(id uuid.UUID) (incident.Incident, error)) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if err := decode(r, v); err != nil {
		return err
	}
	out, err := fn(id)
	if err == nil {
		writeJSON(w, status, out)
	}
	return err
}

func (s *Server) acknowledgeIncident(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in struct {
		Reason string `json:"reason"`
	}
	return s.incidentMove(w, r, http.StatusOK, &in, func(id uuid.UUID) (incident.Incident, error) {
		return s.incidents.Acknowledge(r.Context(), actor(c), id, in.Reason)
	})
}

func (s *Server) assignIncident(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in struct {
		AssigneeID *uuid.UUID `json:"assignee_id"`
	}
	return s.incidentMove(w, r, http.StatusOK, &in, func(id uuid.UUID) (incident.Incident, error) {
		return s.incidents.Assign(r.Context(), actor(c), id, in.AssigneeID)
	})
}

func (s *Server) noteIncident(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in struct {
		Text string `json:"text"`
	}
	return s.incidentMove(w, r, http.StatusCreated, &in, func(id uuid.UUID) (incident.Incident, error) {
		return s.incidents.Note(r.Context(), actor(c), id, in.Text)
	})
}

func (s *Server) linkIncident(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in struct {
		Kind string    `json:"kind"`
		ID   uuid.UUID `json:"id"`
	}
	return s.incidentMove(w, r, http.StatusCreated, &in, func(id uuid.UUID) (incident.Incident, error) {
		return s.incidents.Link(r.Context(), actor(c), id, in.Kind, in.ID)
	})
}

func (s *Server) resolveIncident(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in struct {
		Resolution string `json:"resolution"`
		Reason     string `json:"reason"`
	}
	return s.incidentMove(w, r, http.StatusOK, &in, func(id uuid.UUID) (incident.Incident, error) {
		return s.incidents.Resolve(r.Context(), actor(c), id, in.Resolution, in.Reason)
	})
}

func (s *Server) socSummary(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	sum, err := s.incidents.Summary(r.Context(), actor(c))
	if err == nil {
		writeJSON(w, http.StatusOK, sum)
	}
	return err
}
```

In `internal/api/api.go`: import `"eacp/internal/incident"`; add `incidents *incident.Service` to `Server` (after `finops`); add `incidents: incident.New(pool),` to `New`; call `s.registerIncidents(mux)` after `s.registerRelease(mux)`.

In `internal/config/config.go`: add after `ReleaseInterval`:
```go
	// IncidentInterval is how often controlplane-api runs the incident
	// evaluator (ADR-027 §4).
	IncidentInterval time.Duration
```
after the `EACP_RELEASE_INTERVAL` check:
```go
	cfg.IncidentInterval = duration("EACP_INCIDENT_INTERVAL", "15s", time.Hour)
	if cfg.IncidentInterval > 0 && cfg.IncidentInterval < 5*time.Second {
		errs = append(errs, errors.New("EACP_INCIDENT_INTERVAL: must be at least 5s"))
	}
```
and in the log attributes after `release_interval`:
```go
		slog.Duration("incident_interval", c.IncidentInterval),
```

In `cmd/controlplane-api/main.go`: import `"eacp/internal/incident"` and add after the releases loop:
```go
			d.Background(func(ctx context.Context) { incident.New(d.DB).Run(ctx, d.Config.IncidentInterval, d.Log) })
```

- [ ] **Step 4: Run the tests**

Run: `go vet ./... && go test -race -count=1 ./internal/api/ ./internal/config/ ./cmd/controlplane-api/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api internal/config cmd/controlplane-api
git commit -m "feat(api): incident routes, SOC summary and the evaluator loop"
```

---

### Task 5: eacpctl incident and soc

**Files:**
- Create: `cmd/eacpctl/incident.go`, `cmd/eacpctl/incident_test.go`
- Modify: `cmd/eacpctl/main.go`

**Interfaces:**
- Consumes: the Task 4 routes.

- [ ] **Step 1: Write the failing test**

`cmd/eacpctl/incident_test.go`:
```go
package main

import (
	"strings"
	"testing"
)

func TestIncidentCommands(t *testing.T) {
	env, got := recordingAPI(t)
	const id = "0b9f0e6a-4c8e-4a55-9d1e-4f2f6f1f9a01"
	const other = "0b9f0e6a-4c8e-4a55-9d1e-4f2f6f1f9a02"
	for _, tc := range []struct {
		args   []string
		method string
		uri    string
		body   map[string]any
	}{
		{[]string{"incident", "list"}, "GET", "/v1/incidents", nil},
		{[]string{"incident", "list", "--state", "OPEN", "--severity", "critical", "--limit", "20"}, "GET",
			"/v1/incidents?limit=20&severity=critical&state=OPEN", nil},
		{[]string{"incident", "show", id}, "GET", "/v1/incidents/" + id, nil},
		{[]string{"incident", "open", "--title", "odd", "--severity", "high", "--reason", "finance",
			"--subject-type", "tool", "--subject-id", other}, "POST", "/v1/incidents",
			map[string]any{"title": "odd", "severity": "high", "reason": "finance", "subject_type": "tool", "subject_id": other}},
		{[]string{"incident", "ack", id, "--reason", "mine"}, "POST", "/v1/incidents/" + id + "/acknowledge",
			map[string]any{"reason": "mine"}},
		{[]string{"incident", "assign", id, other}, "POST", "/v1/incidents/" + id + "/assign",
			map[string]any{"assignee_id": other}},
		{[]string{"incident", "assign", id, "none"}, "POST", "/v1/incidents/" + id + "/assign",
			map[string]any{"assignee_id": nil}},
		{[]string{"incident", "note", id, "--text", "checking"}, "POST", "/v1/incidents/" + id + "/notes",
			map[string]any{"text": "checking"}},
		{[]string{"incident", "link", id, "kill_state", other}, "POST", "/v1/incidents/" + id + "/links",
			map[string]any{"kind": "kill_state", "id": other}},
		{[]string{"incident", "resolve", id, "--code", "contained", "--reason", "done"}, "POST",
			"/v1/incidents/" + id + "/resolve", map[string]any{"resolution": "contained", "reason": "done"}},
		{[]string{"soc", "summary"}, "GET", "/v1/soc/summary", nil},
	} {
		*got = nil
		out, err := runWith(t, env, tc.args...)
		if err != nil || !strings.Contains(out, `"ok": true`) {
			t.Fatalf("%v: %v %s", tc.args, err, out)
		}
		if len(*got) != 1 || (*got)[0].method != tc.method || (*got)[0].uri != tc.uri {
			t.Fatalf("%v sent %+v", tc.args, *got)
		}
		for k, v := range tc.body {
			if (*got)[0].body[k] != v {
				t.Fatalf("%v body = %v", tc.args, (*got)[0].body)
			}
		}
	}
	for _, bad := range [][]string{{"incident"}, {"incident", "show", "x"}, {"incident", "ack", id},
		{"incident", "open", "--title", "t"}, {"incident", "open", "--title", "t", "--severity", "low", "--reason", "r",
			"--subject-type", "tool"}, {"incident", "resolve", id, "--reason", "r"}, {"incident", "link", id, "tool"},
		{"soc"}} {
		if _, err := runWith(t, env, bad...); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -count=1 ./cmd/eacpctl/ -run TestIncidentCommands`
Expected: FAIL: `[incident list]: usage: …` (unknown command).

- [ ] **Step 3: Implement the commands**

`cmd/eacpctl/incident.go`:
```go
package main

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strconv"

	"github.com/google/uuid"
)

const incidentUsage = `usage: eacpctl incident list [--state S] [--severity S] [--kind K] [--limit N]
  | show <id> | open --title T --severity S --reason R [--subject-type K --subject-id <uuid>]
  | ack <id> --reason R | assign <id> <principal-uuid|none> | note <id> --text T
  | link <id> <kind> <uuid> | resolve <id> --code <contained|false_positive|accepted_risk|duplicate> --reason R`

const socUsage = "usage: eacpctl soc summary"

func runSOC(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	if len(args) != 1 || args[0] != "summary" {
		return errors.New(socUsage)
	}
	return call(ctx, getenv, out, "GET", "/v1/soc/summary", nil)
}

func runIncident(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(incidentUsage)
	}
	switch args[0] {
	case "list":
		fs := newFlags("incident list")
		state := fs.String("state", "", "OPEN, ACKNOWLEDGED or RESOLVED")
		severity := fs.String("severity", "", "low, medium, high or critical")
		kind := fs.String("kind", "", "incident kind")
		limit := fs.Int("limit", 0, "at most 500")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if len(fs.Args()) != 0 {
			return errors.New(incidentUsage)
		}
		q := url.Values{}
		for k, v := range map[string]string{"state": *state, "severity": *severity, "kind": *kind} {
			if v != "" {
				q.Set(k, v)
			}
		}
		if *limit != 0 {
			q.Set("limit", strconv.Itoa(*limit))
		}
		path := "/v1/incidents"
		if enc := q.Encode(); enc != "" {
			path += "?" + enc
		}
		return call(ctx, getenv, out, "GET", path, nil)
	case "open":
		fs := newFlags("incident open")
		title := fs.String("title", "", "what is wrong")
		severity := fs.String("severity", "", "low, medium, high or critical")
		reason := fs.String("reason", "", "why it is an incident")
		subjectType := fs.String("subject-type", "", "tool, connector, agent, agent_version, action, …")
		subjectID := fs.String("subject-id", "", "the subject's uuid")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *title == "" || *severity == "" || *reason == "" || len(fs.Args()) != 0 ||
			(*subjectType == "") != (*subjectID == "") {
			return errors.New(incidentUsage)
		}
		body := map[string]any{"title": *title, "severity": *severity, "reason": *reason}
		if *subjectType != "" {
			id, err := uuid.Parse(*subjectID)
			if err != nil {
				return errors.New(incidentUsage)
			}
			body["subject_type"], body["subject_id"] = *subjectType, id
		}
		return call(ctx, getenv, out, "POST", "/v1/incidents", body)
	}
	if len(args) < 2 {
		return errors.New(incidentUsage)
	}
	id, err := uuid.Parse(args[1])
	if err != nil {
		return errors.New(incidentUsage)
	}
	path := "/v1/incidents/" + id.String()
	switch args[0] {
	case "show":
		if len(args) != 2 {
			return errors.New(incidentUsage)
		}
		return call(ctx, getenv, out, "GET", path, nil)
	case "assign":
		if len(args) != 3 {
			return errors.New(incidentUsage)
		}
		var assignee any
		if args[2] != "none" {
			p, err := uuid.Parse(args[2])
			if err != nil {
				return errors.New(incidentUsage)
			}
			assignee = p
		}
		return call(ctx, getenv, out, "POST", path+"/assign", map[string]any{"assignee_id": assignee})
	case "link":
		if len(args) != 4 {
			return errors.New(incidentUsage)
		}
		target, err := uuid.Parse(args[3])
		if err != nil {
			return errors.New(incidentUsage)
		}
		return call(ctx, getenv, out, "POST", path+"/links", map[string]any{"kind": args[2], "id": target})
	case "ack", "note", "resolve":
		fs := newFlags("incident " + args[0])
		reason := fs.String("reason", "", "reason")
		text := fs.String("text", "", "note text")
		code := fs.String("code", "", "resolution code")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if len(fs.Args()) != 0 {
			return errors.New(incidentUsage)
		}
		switch {
		case args[0] == "ack" && *reason != "":
			return call(ctx, getenv, out, "POST", path+"/acknowledge", map[string]any{"reason": *reason})
		case args[0] == "note" && *text != "":
			return call(ctx, getenv, out, "POST", path+"/notes", map[string]any{"text": *text})
		case args[0] == "resolve" && *code != "" && *reason != "":
			return call(ctx, getenv, out, "POST", path+"/resolve", map[string]any{"resolution": *code, "reason": *reason})
		}
	}
	return errors.New(incidentUsage)
}
```

In `cmd/eacpctl/main.go`, add to `usage` after the release line:
```
  eacpctl incident list|show|open|ack|assign|note|link|resolve ...
  eacpctl soc summary
```
and to `run` after `case "release":`:
```go
	case "incident":
		return runIncident(ctx, args[1:], getenv, out)
	case "soc":
		return runSOC(ctx, args[1:], getenv, out)
```

- [ ] **Step 4: Run the tests**

Run: `go vet ./cmd/eacpctl/ && go test -race -count=1 ./cmd/eacpctl/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/eacpctl
git commit -m "feat(eacpctl): incident and soc commands"
```

---

### Task 6: Catalog, isolation, demo, documents and the full suite

**Files:**
- Modify: `internal/storage/rls_catalog_test.go`, `internal/worker/isolation_integration_test.go`, `test/demo/slice_c_test.go`, `docs/DEMO.md`, `docs/INVARIANTS.md`, `docs/MASTER_PLAN.md`, `docs/adr/README.md`, `AGENTS.md`
- Create: `docs/adr/ADR-027-incidents-and-agent-soc.md`

- [ ] **Step 1: The reviewed catalog**

In `internal/storage/rls_catalog_test.go`, after the bundle tables check:
```go
	incidents := []string{"incident_events", "incidents"}
	if got := strs(`SELECT relname FROM pg_class WHERE relnamespace = 'eacp'::regnamespace
		AND relkind = 'r' AND relname IN ('incident_events', 'incidents') ORDER BY relname`); !slices.Equal(got, incidents) {
		t.Errorf("reviewed incident tables missing: %v", got)
	}
```
add `"finops_alerts owner_scan PERMISSIVE SELECT {eacp_owner} true",` to `reviewedPolicies` (sorted, after `connectors`), `00022` to the comment above it, and `"eacp.incident_tenants()",` to `reviewedDefiners` (after `finops_tenants`).

- [ ] **Step 2: The isolation sweep**

In `internal/worker/isolation_integration_test.go`, import `"eacp/internal/incident"` and add after the Phase 20 bundle block:
```go
	// Phase 22a: a manual incident with a note (ADR-027).
	inc := incident.New(v.f.App)
	otto := registry.Actor{TenantID: v.f.Tenant, PrincipalID: v.f.P["otto"]}
	opened, err := inc.Open(ctx, otto, incident.NewIncident{Title: "odd purchases", Severity: "medium", Reason: "triage"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inc.Note(ctx, otto, opened.ID, "looking into it"); err != nil {
		t.Fatal(err)
	}
```

- [ ] **Step 3: Run the catalog, isolation and invariants tests**

Run: `go test -race -count=1 ./internal/storage/ ./internal/worker/ -run 'RLS|Catalog|Isolation|AnotherTenant' && go test -count=1 ./test/invariants/`
Expected: PASS.

- [ ] **Step 4: The demo step**

In `test/demo/slice_c_test.go`, insert after `d.incidentEvidence(before, held)`:
```go

	d.step("C7. Incidents: the SOC shows the drift and the kill; otto acknowledges, opal resolves")
	d.incidents(version)
```
renumber `C7. Governance-as-Code…` to `C8.` and `C8. Scan responses…` to `C9.`, and add:
```go
// incidents shows the SOC's view of the drift and the kill (ADR-027). The
// evaluator opened both, with po-assistant in the drift's blast radius.
// otto acknowledges the critical drift incident and links his kill; he
// cannot resolve it himself (two-person), so opal does.
func (d *demo) incidents(version string) {
	d.t.Helper()
	var drift, killed map[string]any
	deadline := time.Now().Add(90 * time.Second)
	for drift == nil || killed == nil {
		if time.Now().After(deadline) {
			d.t.Fatalf("the evaluator opened no drift and kill incidents")
		}
		time.Sleep(2 * time.Second)
		list := d.must(200, "otto", "GET", "/v1/incidents?state=OPEN", nil)
		for _, x := range list["incidents"].([]any) {
			switch i := x.(map[string]any); i["kind"] {
			case "mcp_drift":
				drift = i
			case "kill":
				killed = i
			}
		}
	}
	confirmed := drift["affected"].(map[string]any)["confirmed"].([]any)
	if drift["severity"] != "critical" ||
		!slices.ContainsFunc(confirmed, func(x any) bool { return x.(map[string]any)["version_id"] == version }) {
		d.t.Fatalf("drift incident = %v", drift)
	}
	d.logf("incident %.8s [%v] %v: %d confirmed affected version(s)", drift["id"], drift["severity"], drift["title"],
		len(confirmed))
	d.logf("incident %.8s [%v] %v", killed["id"], killed["severity"], killed["title"])
	id := drift["id"].(string)
	d.must(200, "otto", "POST", "/v1/incidents/"+id+"/acknowledge", map[string]any{"reason": "po-assistant contained"})
	d.must(201, "otto", "POST", "/v1/incidents/"+id+"/links", map[string]any{"kind": "kill_state", "id": killed["subject_id"]})
	resolve := map[string]any{"resolution": "contained", "reason": "po-assistant killed; get_po stays quarantined"}
	if res := d.must(403, "otto", "POST", "/v1/incidents/"+id+"/resolve", resolve); res["error"] == nil {
		d.t.Fatalf("otto resolved his own critical incident: %v", res)
	}
	done := d.must(200, "opal", "POST", "/v1/incidents/"+id+"/resolve", resolve)
	d.logf("otto acknowledged and linked the kill, could not resolve it himself; opal resolved it: %v (%d events)",
		done["state"], len(done["events"].([]any)))
	sum := d.must(200, "audra", "GET", "/v1/soc/summary", nil)
	sec := sum["security"].(map[string]any)
	d.logf("SOC: open incidents %v, quarantined tools %v, active kills %v, queued actions %v",
		sec["open_incidents"], sec["quarantined_tools"], sec["active_kills"], sum["execution"].(map[string]any)["queued"])
}
```
In `docs/DEMO.md`, add a C7 row: "Otto sees the evaluator's MCP drift incident (critical, po-assistant affected) and the kill incident, acknowledges the drift and links his kill, cannot resolve it himself; opal resolves it. Audra reads the SOC summary (ADR-027) | Incidents observe; critical resolution is two-person". Renumber the existing C7 and C8 rows to C8 and C9.

- [ ] **Step 5: Run the demo**

Run (background, Git Bash): `MSYS_NO_PATHCONV=1 DEMO=C scripts/demo.sh > <workspace>/demo-c.log 2>&1; echo "exit $?" >> <workspace>/demo-c.log`
Expected: `--- PASS: TestSliceCDemo` and `exit 0`.

- [ ] **Step 6: The documents**

- `docs/adr/ADR-027-incidents-and-agent-soc.md`: `Status: Accepted (Rev 1.0) · Phase 22a · Date: 2026-09-25`. It has Context (§54–§56, signals without correlation), Decision (spec §2–§5 condensed: the model, lifecycle and two-person critical resolution, the evaluator and its signal table, the shared blast radius function, the summary, the routes), Consequences, and Unresolved assumptions (spec §8).
- `docs/adr/README.md`: add the ADR-027 row: `| [ADR-027](ADR-027-incidents-and-agent-soc.md) | Incidents and the Agent SOC Read Model | Accepted (Rev 1.0) | Phase 22a |`.
- `docs/MASTER_PLAN.md` §94: `> **Status (2026-09-25): 22a delivered.**`, then two sentences on incidents and the summary; 22b (operator web UI) follows.
- `docs/INVARIANTS.md`: add a Phase 22a paragraph after Phase 21. Under `## 17` add ``- `internal/incident` TestTheIncidentLifecycle — every incident event (opened, acknowledged, resolved) is journaled with its actor and reason``.
- `AGENTS.md`: status gets `Phase 22a (Incidents and the Agent SOC read model, ADR-027) is complete; 22b (operator web UI) awaits go-ahead.`; layout gets `internal/incident    incidents (evaluator, lifecycle, timeline) and the SOC summary (ADR-027)`; add the rule: `Incidents observe and never decide (ADR-027). Only the \`incident\` system actor opens automatic incidents, through \`eacp.incident_evaluate()\`, one per signal occurrence; PostgreSQL computes severity, title, detail and the affected snapshot (\`eacp.blast_radius_versions\`, shared with \`registry.BlastRadius\`). Operators and admins acknowledge, assign, note, link and resolve; a critical incident is resolved by someone other than its acknowledger. The timeline is insert-only and journaled. Never add incident writes to action, kill, circuit or scan transactions.`

- [ ] **Step 7: The full suite**

Run: `go vet ./... && go test -race -count=1 ./...` with `EACP_TEST_ADMIN_DSN` set (redirect to a workspace log).
Expected: every package `ok`, exit 0.

- [ ] **Step 8: Commit**

```bash
git add internal/storage internal/worker test/demo docs AGENTS.md
git commit -m "docs: ADR-027 incidents and the Agent SOC read model; Phase 22a delivered"
```
