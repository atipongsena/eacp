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
