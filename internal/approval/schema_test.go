package approval_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"eacp/internal/registry/registrytest"
	"eacp/internal/storage"
	"eacp/internal/storage/pgtest"
)

const policyJSON = `{"format_version":1,"rules":[{"id":"high-risk","match":{"risk_class":"high"},"verdict":"escalate","reason":"high risk","approval":{"quorum":2,"eligible_roles":["approver"],"ttl_seconds":600}}]}`

type approvalSetup struct {
	f       *registrytest.Fixture
	action  uuid.UUID
	request uuid.UUID
	policy  uuid.UUID
	tool    registrytest.Tooling
	agent   registrytest.Agent
}

func setupApproval(t *testing.T, subject string) approvalSetup {
	t.Helper()
	f := registrytest.New(t)
	tool := f.ActiveTool(t, "erp", "purchase")
	agent := f.ActiveAgent(t, "buyer", tool.Tool)
	return finishApprovalSetup(t, f, tool, agent, subject)
}

func finishApprovalSetup(t *testing.T, f *registrytest.Fixture, tool registrytest.Tooling,
	agent registrytest.Agent, subject string) approvalSetup {
	t.Helper()
	policy := f.ID(t, "alice", `INSERT INTO eacp.policy_bundles (tenant_id, content)
		VALUES (eacp.current_tenant_id(), $1::jsonb) RETURNING id`, policyJSON)
	if err := f.Exec("bob", `UPDATE eacp.tenant_policy_pointer
		SET current_bundle_id = $1, activation_reason = 'high-risk policy'
		WHERE tenant_id = eacp.current_tenant_id()`, policy); err != nil {
		t.Fatal(err)
	}
	action := uuid.New()
	evidence := f.ID(t, subject, `INSERT INTO eacp.decision_evidence
		(tenant_id, action_id, policy_bundle_id, policy_version, provider, provider_instance_id,
		 decision_id, verdict, reasons, input_digest, enforced_digest, evaluated_at,
		 required_quorum, eligible_roles, approval_ttl_seconds, enforced_payload)
		VALUES (eacp.current_tenant_id(), $1, $2, 1, 'local', 'local-test', $3,
		 'escalate', ARRAY['high risk'], decode(repeat('11', 32), 'hex'),
		 decode(repeat('22', 32), 'hex'), now(), 2, ARRAY['approver'], 600,
		 '{"amount":1000000,"currency":"THB"}'::jsonb)
		RETURNING id`, action, policy, uuid.New())
	request := f.ID(t, subject, `INSERT INTO eacp.approval_requests
		(tenant_id, action_id, agent_version_id, tool_id, requesting_subject_id,
		 decision_evidence_id, not_after, expires_at)
		VALUES (eacp.current_tenant_id(), $1, $2, $3, $4, $5,
		 now() + interval '1 hour', now() + interval '9 minutes') RETURNING id`,
		action, agent.Version, tool.Tool, f.P[subject], evidence)
	return approvalSetup{f: f, action: action, request: request, policy: policy, tool: tool, agent: agent}
}

func ownerGrantApprover(t *testing.T, f *registrytest.Fixture, principal string) {
	t.Helper()
	err := storage.InTenantTx(context.Background(), f.Owner, pgtest.TenantA, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `INSERT INTO eacp.role_grants
			(tenant_id, principal_id, role, approved_at)
			VALUES (eacp.current_tenant_id(), $1, 'approver', now())`, f.P[principal])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func setupGroupApproval(t *testing.T, member string) (approvalSetup, uuid.UUID, uuid.UUID) {
	t.Helper()
	f := registrytest.New(t)
	group := f.ID(t, "alice", `INSERT INTO eacp.groups (tenant_id, name, display_name)
		VALUES (eacp.current_tenant_id(), 'owners', 'Owners') RETURNING id`)
	membership := f.ID(t, "alice", `INSERT INTO eacp.group_memberships (tenant_id, group_id, principal_id)
		VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, group, f.P[member])
	tool := f.ActiveTool(t, "erp", "purchase")
	agent := registrytest.Agent{}
	agent.Agent = f.ID(t, "erin", `INSERT INTO eacp.agents
		(tenant_id, name, display_name, environment, risk_class, owner_group_id)
		VALUES (eacp.current_tenant_id(), 'group-buyer', 'Group buyer', 'production', 'high', $1)
		RETURNING id`, group)
	agent.Version = f.ID(t, "erin", `INSERT INTO eacp.agent_versions (tenant_id, agent_id, runtime, code_ref)
		VALUES (eacp.current_tenant_id(), $1, 'python', 'git:group') RETURNING id`, agent.Agent)
	allowlist := f.ID(t, "erin", `INSERT INTO eacp.agent_allowlists
		(tenant_id, agent_version_id, tool_ids)
		VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, agent.Version, []uuid.UUID{tool.Tool})
	if err := f.Exec("rita", `UPDATE eacp.agent_versions SET active_allowlist_id = $1 WHERE id = $2`, allowlist, agent.Version); err != nil {
		t.Fatal(err)
	}
	if err := f.Exec("ravi", `UPDATE eacp.agent_versions SET state = 'ACTIVE', state_reason = 'go live' WHERE id = $1`, agent.Version); err != nil {
		t.Fatal(err)
	}
	return finishApprovalSetup(t, f, tool, agent, "carol"), group, membership
}

func wantState(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("error = %v, want SQLSTATE %s", err, code)
	}
}

func requestState(t *testing.T, s approvalSetup) string {
	t.Helper()
	var state string
	err := storage.InTenantTx(context.Background(), s.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT state FROM eacp.approval_requests WHERE id = $1`, s.request).Scan(&state)
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestApprovalQuorumAndDenyShortCircuit(t *testing.T) {
	s := setupApproval(t, "carol")
	vote := `INSERT INTO eacp.approval_votes (tenant_id, request_id, decision, reason)
		VALUES (eacp.current_tenant_id(), $1, $2, 'reviewed')`
	if err := s.f.Exec("amy", vote, s.request, "APPROVE"); err != nil {
		t.Fatal(err)
	}
	if state := requestState(t, s); state != "PENDING" {
		t.Fatalf("one of two votes granted request: %s", state)
	}
	wantState(t, s.f.Exec("amy", vote, s.request, "APPROVE"), "23505")
	if err := s.f.Exec("ben", vote, s.request, "APPROVE"); err != nil {
		t.Fatal(err)
	}
	if state := requestState(t, s); state != "GRANTED" {
		t.Fatalf("quorum did not grant request: %s", state)
	}
	var n int
	err := storage.InTenantTx(context.Background(), s.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM eacp.approval_grants WHERE request_id = $1`, s.request).Scan(&n)
	})
	if err != nil || n != 1 {
		t.Fatalf("grants = %d, err = %v, want one", n, err)
	}
	wantState(t, s.f.Exec("cy", vote, s.request, "DENY"), "55000")

	s2 := setupApproval(t, "carol")
	if err := s2.f.Exec("amy", vote, s2.request, "DENY"); err != nil {
		t.Fatal(err)
	}
	if state := requestState(t, s2); state != "DENIED" {
		t.Fatalf("deny vote left request %s", state)
	}
	wantState(t, s2.f.Exec("ben", vote, s2.request, "APPROVE"), "55000")
}

func TestSelfAndCrossTenantApprovalRejected(t *testing.T) {
	s := setupApproval(t, "amy")
	vote := `INSERT INTO eacp.approval_votes (tenant_id, request_id, decision, reason)
		VALUES (eacp.current_tenant_id(), $1, 'APPROVE', 'reviewed')`
	wantState(t, s.f.Exec("amy", vote, s.request), "42501")
	wantState(t, s.f.ExecIn(pgtest.TenantB, "ben", vote, s.request), "42501")
}

func TestOwnerAndEnablingActorsCannotApprove(t *testing.T) {
	s := setupApproval(t, "amy")
	ownerGrantApprover(t, s.f, "carol")
	for _, name := range []string{"alice", "bob", "erin", "rita"} {
		ownerGrantApprover(t, s.f, name)
	}
	vote := `INSERT INTO eacp.approval_votes (tenant_id, request_id, decision, reason)
		VALUES (eacp.current_tenant_id(), $1, 'APPROVE', 'reviewed')`
	for _, name := range []string{"carol", "alice", "bob", "erin", "rita"} {
		wantState(t, s.f.Exec(name, vote, s.request), "42501")
	}
}

func TestOwnerGroupMembershipAtRequestOrVoteBlocksApproval(t *testing.T) {
	s, group, membership := setupGroupApproval(t, "amy")
	if err := s.f.Exec("alice", `UPDATE eacp.group_memberships
		SET removed_at = now(), remove_reason = 'left group' WHERE id = $1`, membership); err != nil {
		t.Fatal(err)
	}
	vote := `INSERT INTO eacp.approval_votes (tenant_id, request_id, decision, reason)
		VALUES (eacp.current_tenant_id(), $1, 'APPROVE', 'reviewed')`
	wantState(t, s.f.Exec("amy", vote, s.request), "42501")
	if err := s.f.Exec("ben", vote, s.request); err != nil {
		t.Fatal(err)
	}
	s.f.ID(t, "alice", `INSERT INTO eacp.group_memberships (tenant_id, group_id, principal_id)
		VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, group, s.f.P["cy"])
	wantState(t, s.f.Exec("cy", vote, s.request), "42501")
}

func TestGrantCannotBeConsumedTwiceOrAfterPolicyChange(t *testing.T) {
	s := setupApproval(t, "carol")
	vote := `INSERT INTO eacp.approval_votes (tenant_id, request_id, decision, reason)
		VALUES (eacp.current_tenant_id(), $1, 'APPROVE', 'reviewed')`
	for _, name := range []string{"amy", "ben"} {
		if err := s.f.Exec(name, vote, s.request); err != nil {
			t.Fatal(err)
		}
	}
	consume := `UPDATE eacp.approval_grants
		SET consumed_at = now(), consumed_by_action_id = $2
		WHERE request_id = $1 AND enforced_digest = decode(repeat('22', 32), 'hex')
		AND policy_version = 1 AND consumed_at IS NULL AND expires_at > now()`
	if err := s.f.Exec("carol", consume, s.request, s.action); err != nil {
		t.Fatal(err)
	}
	// A direct UPDATE without the conditional WHERE is rejected by the trigger.
	wantState(t, s.f.Exec("carol", `UPDATE eacp.approval_grants
		SET consumed_at = now(), consumed_by_action_id = $2 WHERE request_id = $1`, s.request, s.action), "55000")

	s2 := setupApproval(t, "carol")
	for _, name := range []string{"amy", "ben"} {
		if err := s2.f.Exec(name, vote, s2.request); err != nil {
			t.Fatal(err)
		}
	}
	v2 := s2.f.ID(t, "bob", `INSERT INTO eacp.policy_bundles (tenant_id, content)
		VALUES (eacp.current_tenant_id(), $1::jsonb) RETURNING id`, policyJSON)
	if err := s2.f.Exec("alice", `UPDATE eacp.tenant_policy_pointer
		SET current_bundle_id = $1, activation_reason = 'new policy'
		WHERE tenant_id = eacp.current_tenant_id()`, v2); err != nil {
		t.Fatal(err)
	}
	wantState(t, s2.f.Exec("carol", consume, s2.request, s2.action), "55000")
}

func TestGrantCannotBeConsumedAfterRequestExpiryIsShortened(t *testing.T) {
	s := setupApproval(t, "carol")
	vote := `INSERT INTO eacp.approval_votes (tenant_id, request_id, decision, reason)
		VALUES (eacp.current_tenant_id(), $1, 'APPROVE', 'reviewed')`
	for _, name := range []string{"amy", "ben"} {
		if err := s.f.Exec(name, vote, s.request); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.f.Exec("carol", `UPDATE eacp.approval_requests
		SET expires_at = now() - interval '1 second' WHERE id = $1`, s.request); err != nil {
		t.Fatal(err)
	}
	wantState(t, s.f.Exec("carol", `UPDATE eacp.approval_grants
		SET consumed_at = now(), consumed_by_action_id = $2
		WHERE request_id = $1`, s.request, s.action), "55000")
	wantState(t, s.f.Exec("carol", `UPDATE eacp.approval_grants
		SET expires_at = now() - interval '1 second', consumed_at = now(),
		consumed_by_action_id = $2 WHERE request_id = $1`, s.request, s.action), "55000")
}

func TestRevokedActivePolicyInvalidatesVotesAndGrants(t *testing.T) {
	vote := `INSERT INTO eacp.approval_votes (tenant_id, request_id, decision, reason)
		VALUES (eacp.current_tenant_id(), $1, 'APPROVE', 'reviewed')`
	s := setupApproval(t, "carol")
	if err := s.f.Exec("bob", `UPDATE eacp.policy_bundles
		SET revoked_at = now(), revoke_reason = 'unsafe' WHERE id = $1`, s.policy); err != nil {
		t.Fatal(err)
	}
	wantState(t, s.f.Exec("amy", vote, s.request), "55000")

	s2 := setupApproval(t, "carol")
	for _, name := range []string{"amy", "ben"} {
		if err := s2.f.Exec(name, vote, s2.request); err != nil {
			t.Fatal(err)
		}
	}
	if err := s2.f.Exec("bob", `UPDATE eacp.policy_bundles
		SET revoked_at = now(), revoke_reason = 'unsafe' WHERE id = $1`, s2.policy); err != nil {
		t.Fatal(err)
	}
	wantState(t, s2.f.Exec("carol", `UPDATE eacp.approval_grants
		SET consumed_at = now(), consumed_by_action_id = $2
		WHERE request_id = $1`, s2.request, s2.action), "55000")
}

func TestVoteRequiresCurrentApproverRoleAndUnexpiredRequest(t *testing.T) {
	s := setupApproval(t, "carol")
	vote := `INSERT INTO eacp.approval_votes (tenant_id, request_id, decision, reason)
		VALUES (eacp.current_tenant_id(), $1, 'APPROVE', 'reviewed')`
	if err := s.f.Exec("alice", `UPDATE eacp.role_grants SET revoked_at = now(), revoke_reason = 'left role'
		WHERE principal_id = $1 AND role = 'approver'`, s.f.P["amy"]); err != nil {
		t.Fatal(err)
	}
	wantState(t, s.f.Exec("amy", vote, s.request), "42501")
	// The database clock, not a client-supplied vote timestamp, decides expiry.
	s2 := setupApproval(t, "carol")
	if err := s2.f.Exec("carol", `UPDATE eacp.approval_requests
		SET expires_at = now() + interval '100 milliseconds' WHERE id = $1`, s2.request); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	wantState(t, s2.f.Exec("amy", vote, s2.request), "55000")
}

func TestGovernanceAuditOmitsPolicySourcePayloadAndDigests(t *testing.T) {
	s := setupApproval(t, "carol")
	if err := s.f.Exec("amy", `INSERT INTO eacp.approval_votes
		(tenant_id, request_id, decision, reason)
		VALUES (eacp.current_tenant_id(), $1, 'APPROVE', 'reviewed')`, s.request); err != nil {
		t.Fatal(err)
	}
	err := storage.InTenantTx(context.Background(), s.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), `SELECT convert_from(payload, 'UTF8')
			FROM eacp.audit_events WHERE convert_from(payload, 'UTF8') LIKE '%approval_%'
			   OR convert_from(payload, 'UTF8') LIKE '%policy_bundles%'`)
		if err != nil {
			return err
		}
		defer rows.Close()
		count := 0
		for rows.Next() {
			var payload string
			if err := rows.Scan(&payload); err != nil {
				return err
			}
			count++
			for _, forbidden := range []string{"\"content\"", "\"enforced_payload\"", "\"input_digest\"", "\"enforced_digest\"", "\"snapshot_hash\""} {
				if strings.Contains(payload, forbidden) {
					t.Errorf("audit payload contains %s: %s", forbidden, payload)
				}
			}
		}
		if count == 0 {
			t.Error("no governance audit events found")
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
}
