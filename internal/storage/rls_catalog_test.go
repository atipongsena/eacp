package storage_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"eacp/internal/storage/pgtest"
)

// Invariant 8 (MASTER_PLAN §103): tenant isolation cannot be bypassed.
// Every table in the eacp schema follows the RLS convention of migration
// 00001, and the only ways across tenants are the reviewed exceptions
// below. A new table, policy or SECURITY DEFINER function fails this test
// until it has been reviewed and added here.
func TestEveryTableFollowsTheRLSConventionAndCrossTenantPathsAreReviewed(t *testing.T) {
	db := pgtest.Migrated(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, db.AdminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	strs := func(sql string) []string {
		t.Helper()
		rows, err := conn.Query(ctx, sql)
		if err != nil {
			t.Fatal(err)
		}
		out, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	// Every table is tenant-scoped; only the tenants table keys on id.
	if got := strs(`SELECT c.relname FROM pg_class c
		WHERE c.relnamespace = 'eacp'::regnamespace AND c.relkind IN ('r', 'p')
		  AND NOT EXISTS (SELECT 1 FROM pg_attribute a
		                  WHERE a.attrelid = c.oid AND a.attname = 'tenant_id' AND NOT a.attisdropped)
		ORDER BY 1`); !slices.Equal(got, []string{"tenants"}) {
		t.Errorf("tables without tenant_id = %v, want [tenants]", got)
	}
	// RLS is enabled and forced (it applies to the owner too).
	if got := strs(`SELECT relname FROM pg_class
		WHERE relnamespace = 'eacp'::regnamespace AND relkind IN ('r', 'p')
		  AND NOT (relrowsecurity AND relforcerowsecurity) ORDER BY 1`); len(got) != 0 {
		t.Errorf("tables without enabled and forced RLS: %v", got)
	}
	// Every tenant table has the standard policy for every role and command.
	if got := strs(`SELECT c.relname FROM pg_class c
		WHERE c.relnamespace = 'eacp'::regnamespace AND c.relkind IN ('r', 'p') AND c.relname <> 'tenants'
		  AND NOT EXISTS (SELECT 1 FROM pg_policies p
		                  WHERE p.schemaname = 'eacp' AND p.tablename = c.relname AND p.policyname = 'tenant_isolation'
		                    AND p.permissive = 'PERMISSIVE' AND p.cmd = 'ALL' AND p.roles = '{public}'
		                    AND p.qual = '(tenant_id = eacp.current_tenant_id())' AND p.with_check IS NULL)
		ORDER BY 1`); len(got) != 0 {
		t.Errorf("tables without the tenant_isolation policy: %v", got)
	}
	// Phase 15's dependency evidence uses the standard tenant policy and no
	// cross-tenant exception. The checks above inspect it with every table.
	if got := strs(`SELECT relname FROM pg_class WHERE relnamespace = 'eacp'::regnamespace
		AND relkind = 'r' AND relname = 'dependency_edges'`); !slices.Equal(got, []string{"dependency_edges"}) {
		t.Errorf("reviewed dependency table missing: %v", got)
	}
	if got := strs(`SELECT relname FROM pg_class WHERE relnamespace = 'eacp'::regnamespace
		AND relkind = 'r' AND relname IN ('kill_states', 'kill_tenant_epochs') ORDER BY relname`); !slices.Equal(got, []string{"kill_states", "kill_tenant_epochs"}) {
		t.Errorf("reviewed kill tables missing: %v", got)
	}
	if got := strs(`SELECT relname FROM pg_class WHERE relnamespace = 'eacp'::regnamespace
		AND relkind = 'r' AND relname IN ('fleet_operation_targets', 'fleet_operations') ORDER BY relname`); !slices.Equal(got, []string{"fleet_operation_targets", "fleet_operations"}) {
		t.Errorf("reviewed fleet tables missing: %v", got)
	}
	// Any other policy is a reviewed exception: the schema owner's read-only
	// scans behind the SECURITY DEFINER claim and outbox hints (migrations
	// 00005-00007, 00010, 00012, 00013, 00014).
	reviewedPolicies := []string{
		"actions owner_scan PERMISSIVE SELECT {eacp_owner} true",
		"connector_circuits owner_scan PERMISSIVE SELECT {eacp_owner} true",
		"connectors owner_scan PERMISSIVE SELECT {eacp_owner} true",
		"kill_states owner_scan PERMISSIVE SELECT {eacp_owner} true",
		"kill_tenant_epochs owner_scan PERMISSIVE SELECT {eacp_owner} true",
		"mcp_servers owner_scan PERMISSIVE SELECT {eacp_owner} true",
		"outbox_events owner_scan PERMISSIVE SELECT {eacp_owner} true",
		"scheduler_team_state owner_scan PERMISSIVE SELECT {eacp_owner} true",
		"scheduler_tenant_state owner_scan PERMISSIVE SELECT {eacp_owner} true",
		"tenants tenant_isolation PERMISSIVE ALL {public} (id = eacp.current_tenant_id())",
		"tool_contracts owner_scan PERMISSIVE SELECT {eacp_owner} true",
		"tools owner_scan PERMISSIVE SELECT {eacp_owner} true",
	}
	if got := strs(`SELECT concat_ws(' ', tablename, policyname, permissive, cmd, roles::text, qual, with_check)
		FROM pg_policies WHERE schemaname = 'eacp'
		  AND NOT (policyname = 'tenant_isolation' AND permissive = 'PERMISSIVE' AND cmd = 'ALL'
		           AND roles = '{public}' AND qual = '(tenant_id = eacp.current_tenant_id())' AND with_check IS NULL)
		ORDER BY 1`); !slices.Equal(got, reviewedPolicies) {
		t.Errorf("policies outside the convention:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(reviewedPolicies, "\n"))
	}

	// SECURITY DEFINER functions run as the owner and so cross tenants. Each
	// is reviewed: the journal chain append, and claim, count and outbox
	// hints that return only ids and counts; the caller re-checks everything
	// under RLS (the relay locks and publishes each row in its tenant).
	reviewedDefiners := []string{
		"eacp.audit_chain_append()",
		"eacp.claimable_actions(text[],jsonb,integer,jsonb)",
		"eacp.global_queued_count()",
		"eacp.mcp_scans_due(jsonb,integer)",
		"eacp.outbox_pending(text[],integer)",
		"eacp.outbox_prunable(integer)",
		"eacp.reconcilable_actions(text[],jsonb,integer)",
		"eacp.set_kill(text,uuid,boolean,text,text)",
		"eacp.tenants_with_open_actions(uuid,integer)",
	}
	if got := strs(`SELECT p.oid::regprocedure::text FROM pg_proc p
		WHERE p.pronamespace = 'eacp'::regnamespace AND p.prosecdef ORDER BY 1`); !slices.Equal(got, reviewedDefiners) {
		t.Errorf("SECURITY DEFINER functions = %v, want %v", got, reviewedDefiners)
	}
	// Each pins its search_path and is executable only by the owner and the
	// application role, never by PUBLIC.
	if got := strs(`SELECT p.oid::regprocedure::text FROM pg_proc p
		WHERE p.pronamespace = 'eacp'::regnamespace AND p.prosecdef
		  AND (p.proconfig IS DISTINCT FROM ARRAY['search_path=pg_catalog, pg_temp']
		       OR p.proacl IS NULL
		       OR EXISTS (SELECT 1 FROM aclexplode(p.proacl) x
		                  WHERE x.privilege_type = 'EXECUTE'
		                    AND (x.grantee = 0 OR pg_get_userbyid(x.grantee) NOT IN ('eacp_owner', 'eacp_app'))))
		ORDER BY 1`); len(got) != 0 {
		t.Errorf("SECURITY DEFINER functions without a pinned search_path or with wider EXECUTE: %v", got)
	}

	// Neither EACP role can bypass RLS.
	if got := strs(`SELECT rolname FROM pg_roles
		WHERE rolname IN ('eacp_owner', 'eacp_app') AND (rolbypassrls OR rolsuper) ORDER BY 1`); len(got) != 0 {
		t.Errorf("roles that bypass RLS: %v", got)
	}
}
