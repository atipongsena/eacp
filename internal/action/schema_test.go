package action_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"eacp/internal/audit"
	"eacp/internal/registry/registrytest"
	"eacp/internal/storage"
	"eacp/internal/storage/pgtest"
)

const escalatePolicy = `{"format_version":1,"rules":[{"id":"high-risk","match":{"risk_class":"high"},"verdict":"escalate","reason":"high risk","approval":{"quorum":2,"eligible_roles":["approver"],"ttl_seconds":600}}]}`

type schemaSetup struct {
	f      *registrytest.Fixture
	tool   registrytest.Tooling
	agent  registrytest.Agent
	policy uuid.UUID
}

func newSchemaSetup(t *testing.T) schemaSetup {
	t.Helper()
	f := registrytest.New(t)
	tool := f.ActiveTool(t, "erp", "purchase")
	agent := f.ActiveAgent(t, "buyer", tool.Tool)
	policy := f.ID(t, "alice", `INSERT INTO eacp.policy_bundles (tenant_id, content)
		VALUES (eacp.current_tenant_id(), $1::jsonb) RETURNING id`, escalatePolicy)
	if err := f.Exec("bob", `UPDATE eacp.tenant_policy_pointer
		SET current_bundle_id = $1, activation_reason = 'reviewed'
		WHERE tenant_id = eacp.current_tenant_id()`, policy); err != nil {
		t.Fatal(err)
	}
	return schemaSetup{f: f, tool: tool, agent: agent, policy: policy}
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("error = %v, want SQLSTATE %s", err, code)
	}
}

// query runs fn in tenant A without an actor (reads only).
func (s schemaSetup) query(t *testing.T, fn func(pgx.Tx) error) {
	t.Helper()
	if err := storage.InTenantTx(context.Background(), s.f.App, pgtest.TenantA, fn); err != nil {
		t.Fatal(err)
	}
}

func (s schemaSetup) state(t *testing.T, action uuid.UUID) string {
	t.Helper()
	var state string
	s.query(t, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT state FROM eacp.actions WHERE id = $1`, action).Scan(&state)
	})
	return state
}

// allowEvidence records allow evidence for action under the fixture policy.
const allowEvidenceSQL = `INSERT INTO eacp.decision_evidence
	(tenant_id, action_id, policy_bundle_id, policy_version, provider, provider_instance_id,
	 decision_id, verdict, reasons, input_digest, enforced_digest, evaluated_at, enforced_payload)
	VALUES (eacp.current_tenant_id(), $1, $2, 1, 'local', 'local-test', gen_random_uuid(),
	 $3, ARRAY['ok'], decode(repeat('11', 32), 'hex'), decode(repeat('22', 32), 'hex'), now(),
	 $4::jsonb) RETURNING id`

const authorizeSQL = `UPDATE eacp.actions SET state = 'AUTHORIZED', state_reason = 'ok',
	decision_evidence_id = $2, enforced_payload = $3 WHERE id = $1`

func (s schemaSetup) authorized(t *testing.T) uuid.UUID {
	t.Helper()
	a := s.f.ReceivedAction(t, s.agent.Version, "carol", "erp.purchase")
	ev := s.f.AgentID(t, s.agent.Version, allowEvidenceSQL, a, s.policy, "allow", registrytest.ActionPayload)
	if err := s.f.ExecAgent(s.agent.Version, authorizeSQL, a, ev, registrytest.ActionPayload); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestActionInsertRequiresMatchingAgentAndDerivesIdentity(t *testing.T) {
	s := newSchemaSetup(t)
	other := s.f.ActiveAgent(t, "other", s.tool.Tool)
	key := func() string { return uuid.NewString() }

	// Principals and other agents cannot submit for this agent version.
	_, err := s.f.TryID("alice", registrytest.ReceivedActionSQL, s.agent.Version, key(), "carol@tenant-a.test", "erp.purchase")
	wantCode(t, err, "42501")
	_, err = s.f.TryAgentID(other.Version, registrytest.ReceivedActionSQL, s.agent.Version, key(), "carol@tenant-a.test", "erp.purchase")
	wantCode(t, err, "42501")
	// Actions are born RECEIVED without governance results.
	_, err = s.f.TryAgentID(s.agent.Version, `INSERT INTO eacp.actions
		(tenant_id, agent_version_id, idempotency_key, subject, operation, target, tool,
		 tool_schema_version, resource, input_payload, input_digest, not_after, state)
		VALUES (eacp.current_tenant_id(), $1, 'k-queued', 'carol@tenant-a.test', 'purchase', 'erp',
		 'erp.purchase', '1', 'po', '{}', decode(repeat('11', 32), 'hex'), now() + interval '1 hour', 'QUEUED')
		RETURNING id`, s.agent.Version)
	wantCode(t, err, "55000")
	// The lifetime is bounded by the database clock.
	for _, lifetime := range []string{"-1 second", "25 hours"} {
		_, err = s.f.TryAgentID(s.agent.Version, strings.Replace(registrytest.ReceivedActionSQL,
			"interval '1 hour'", "interval '"+lifetime+"'", 1), s.agent.Version, key(), "carol@tenant-a.test", "erp.purchase")
		wantCode(t, err, "23514")
	}

	id := s.f.ReceivedAction(t, s.agent.Version, "carol", "erp.purchase")
	unknown := s.f.AgentID(t, s.agent.Version, registrytest.ReceivedActionSQL, s.agent.Version, key(), "nobody@tenant-a.test", "erp.missing")
	s.query(t, func(tx pgx.Tx) error {
		var agent, subject, tool, actorID uuid.UUID
		var state, actorKind, opKey string
		err := tx.QueryRow(context.Background(), `SELECT agent_id, subject_principal_id, tool_id, state,
			state_actor_kind, state_actor_id, operation_key FROM eacp.actions WHERE id = $1`, id).
			Scan(&agent, &subject, &tool, &state, &actorKind, &actorID, &opKey)
		if err != nil {
			return err
		}
		if agent != s.agent.Agent || subject != s.f.P["carol"] || tool != s.tool.Tool || state != "RECEIVED" ||
			actorKind != "agent" || actorID != s.agent.Version || opKey != "eacp:"+pgtest.TenantA+":"+id.String() {
			t.Errorf("derived identity = %v %v %v %s %s %v %s", agent, subject, tool, state, actorKind, actorID, opKey)
		}
		var nullSubject, nullTool bool
		if err := tx.QueryRow(context.Background(), `SELECT subject_principal_id IS NULL, tool_id IS NULL
			FROM eacp.actions WHERE id = $1`, unknown).Scan(&nullSubject, &nullTool); err != nil {
			return err
		}
		if !nullSubject || !nullTool {
			t.Errorf("unknown subject/tool resolved: %v %v", nullSubject, nullTool)
		}
		return nil
	})
}

func TestIdempotencyKeyIsUniquePerAgent(t *testing.T) {
	s := newSchemaSetup(t)
	other := s.f.ActiveAgent(t, "other", s.tool.Tool)
	s.f.AgentID(t, s.agent.Version, registrytest.ReceivedActionSQL, s.agent.Version, "k1", "carol@tenant-a.test", "erp.purchase")
	_, err := s.f.TryAgentID(s.agent.Version, registrytest.ReceivedActionSQL, s.agent.Version, "k1", "carol@tenant-a.test", "erp.purchase")
	wantCode(t, err, "23505")
	s.f.AgentID(t, other.Version, registrytest.ReceivedActionSQL, other.Version, "k1", "carol@tenant-a.test", "erp.purchase")
}

func TestActionsAreTenantIsolatedAndUndeletable(t *testing.T) {
	s := newSchemaSetup(t)
	id := s.f.ReceivedAction(t, s.agent.Version, "carol", "erp.purchase")
	ctx := context.Background()
	var n int
	err := storage.InTenantTx(ctx, s.f.App, pgtest.TenantB, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM eacp.actions`).Scan(&n)
	})
	if err != nil || n != 0 {
		t.Fatalf("tenant B sees %d actions, err = %v", n, err)
	}
	if err := s.f.App.QueryRow(ctx, `SELECT count(*) FROM eacp.actions`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("no tenant context sees %d actions, err = %v", n, err)
	}
	wantCode(t, s.f.ExecAgent(s.agent.Version, `DELETE FROM eacp.actions WHERE id = $1`, id), "42501")
	wantCode(t, s.f.ExecAgent(s.agent.Version, `UPDATE eacp.actions SET input_payload = '{}' WHERE id = $1`, id), "42501")
	wantCode(t, s.f.ExecAgent(s.agent.Version, `DELETE FROM eacp.outbox_events`), "42501")
}

func TestIllegalAndTerminalTransitionsAreRejected(t *testing.T) {
	s := newSchemaSetup(t)
	id := s.f.ReceivedAction(t, s.agent.Version, "carol", "erp.purchase")
	for _, to := range []string{"QUEUED", "CANCELLED", "EXECUTING", "SUCCEEDED"} {
		wantCode(t, s.f.ExecAgent(s.agent.Version, `UPDATE eacp.actions SET state = $2, state_reason = 'x'
			WHERE id = $1`, id, to), "55000")
	}
	if err := s.f.ExecAgent(s.agent.Version, `UPDATE eacp.actions SET state = 'DENIED',
		state_reason = 'tool_not_in_allowlist' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	wantCode(t, s.f.ExecAgent(s.agent.Version, `UPDATE eacp.actions SET state = 'AUTHORIZED',
		state_reason = 'x' WHERE id = $1`, id), "55000")
	// A denial needs a reason.
	id2 := s.f.ReceivedAction(t, s.agent.Version, "carol", "erp.purchase")
	wantCode(t, s.f.ExecAgent(s.agent.Version, `UPDATE eacp.actions SET state = 'DENIED' WHERE id = $1`, id2), "23514")
	// Expiry is decided by the database clock.
	wantCode(t, s.f.ExecSystem("sweeper", `UPDATE eacp.actions SET state = 'EXPIRED', state_reason = 'late'
		WHERE id = $1`, id2), "55000")
}

func TestAuthorizeRequiresMatchingCurrentEvidence(t *testing.T) {
	s := newSchemaSetup(t)
	a := s.f.ReceivedAction(t, s.agent.Version, "carol", "erp.purchase")
	b := s.f.ReceivedAction(t, s.agent.Version, "carol", "erp.purchase")
	evB := s.f.AgentID(t, s.agent.Version, allowEvidenceSQL, b, s.policy, "allow", registrytest.ActionPayload)
	deny := s.f.AgentID(t, s.agent.Version, allowEvidenceSQL, a, s.policy, "deny", registrytest.ActionPayload)
	allow := s.f.AgentID(t, s.agent.Version, allowEvidenceSQL, a, s.policy, "allow", registrytest.ActionPayload)

	// Principals do not drive governance transitions.
	wantCode(t, s.f.Exec("alice", authorizeSQL, a, allow, registrytest.ActionPayload), "42501")
	wantCode(t, s.f.ExecAgent(s.agent.Version, authorizeSQL, a, evB, registrytest.ActionPayload), "55000")
	wantCode(t, s.f.ExecAgent(s.agent.Version, authorizeSQL, a, deny, registrytest.ActionPayload), "55000")
	wantCode(t, s.f.ExecAgent(s.agent.Version, authorizeSQL, a, allow, `{"amount":1,"currency":"THB"}`), "55000")
	if err := s.f.ExecAgent(s.agent.Version, authorizeSQL, a, allow, registrytest.ActionPayload); err != nil {
		t.Fatal(err)
	}
	s.query(t, func(tx pgx.Tx) error {
		var version int
		var bundle uuid.UUID
		var digest []byte
		if err := tx.QueryRow(context.Background(), `SELECT policy_version, policy_bundle_id, enforced_digest
			FROM eacp.actions WHERE id = $1`, a).Scan(&version, &bundle, &digest); err != nil {
			return err
		}
		if version != 1 || bundle != s.policy || len(digest) != 32 || digest[0] != 0x22 {
			t.Errorf("derived policy/digest = %d %v %x", version, bundle, digest)
		}
		return nil
	})
	// Evidence cannot be recorded for an action in another agent's name.
	other := s.f.ActiveAgent(t, "other", s.tool.Tool)
	_, err := s.f.TryAgentID(other.Version, allowEvidenceSQL, b, s.policy, "allow", registrytest.ActionPayload)
	wantCode(t, err, "42501")
	// A stale policy version cannot authorize.
	v2 := s.f.ID(t, "bob", `INSERT INTO eacp.policy_bundles (tenant_id, content)
		VALUES (eacp.current_tenant_id(), $1::jsonb) RETURNING id`, escalatePolicy)
	if err := s.f.Exec("alice", `UPDATE eacp.tenant_policy_pointer SET current_bundle_id = $1,
		activation_reason = 'v2' WHERE tenant_id = eacp.current_tenant_id()`, v2); err != nil {
		t.Fatal(err)
	}
	wantCode(t, s.f.ExecAgent(s.agent.Version, authorizeSQL, b, evB, registrytest.ActionPayload), "55000")
}

// releaseSQL runs the release as the agent in one transaction: fresh
// evidence, optional grant consumption, and T10 pinning the active contract.
func (s schemaSetup) release(action uuid.UUID, verdict string, consume bool) error {
	ctx := context.Background()
	return storage.InTenantTx(ctx, s.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		if err := storage.SetAgent(ctx, tx, s.agent.Version); err != nil {
			return err
		}
		if err := storage.SetTraceparent(ctx, tx, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"); err != nil {
			return err
		}
		var ev uuid.UUID
		if err := tx.QueryRow(ctx, registrytest.ReleaseEvidenceSQL, action, verdict).Scan(&ev); err != nil {
			return err
		}
		if consume {
			if _, err := tx.Exec(ctx, registrytest.ConsumeGrantSQL, action); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, registrytest.QueueSQL, action, ev)
		return err
	})
}

func TestReleaseRequiresFreshEvidenceAndConsumedGrant(t *testing.T) {
	s := newSchemaSetup(t)
	e := s.f.EscalatedAction(t, s.agent.Version, s.policy, "carol", "erp.purchase")
	vote := `INSERT INTO eacp.approval_votes (tenant_id, request_id, decision, reason)
		VALUES (eacp.current_tenant_id(), $1, 'APPROVE', 'reviewed')`
	for _, name := range []string{"amy", "ben"} {
		if err := s.f.Exec(name, vote, e.Request); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.state(t, e.Action); got != "AUTHORIZED" {
		t.Fatalf("quorum left action %s, want AUTHORIZED (T6)", got)
	}
	// Reusing the submission evidence is not a revalidation.
	wantCode(t, s.f.ExecAgent(s.agent.Version, registrytest.QueueSQL, e.Action, e.Evidence), "55000")
	// Escalation evidence without consuming the grant cannot release.
	wantCode(t, s.release(e.Action, "escalate", false), "55000")
	// A grant cannot be burned without queuing the action (checked at commit).
	err := storage.InTenantTx(context.Background(), s.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		if err := storage.SetAgent(context.Background(), tx, s.agent.Version); err != nil {
			return err
		}
		_, err := tx.Exec(context.Background(), registrytest.ConsumeGrantSQL, e.Action)
		return err
	})
	wantCode(t, err, "55000")
	if err := s.release(e.Action, "escalate", true); err != nil {
		t.Fatal(err)
	}
	if got := s.state(t, e.Action); got != "QUEUED" {
		t.Fatalf("release left action %s", got)
	}
	s.query(t, func(tx pgx.Tx) error {
		ctx := context.Background()
		var contract uuid.UUID
		var contractVersion int
		var consumedBy uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT a.connector_contract_id, a.connector_contract_version, g.consumed_by_action_id
			FROM eacp.actions a JOIN eacp.approval_grants g ON g.tenant_id = a.tenant_id AND g.action_id = a.id
			WHERE a.id = $1`, e.Action).Scan(&contract, &contractVersion, &consumedBy); err != nil {
			return err
		}
		if contract != s.tool.Contract || contractVersion != 1 || consumedBy != e.Action {
			t.Errorf("pinned contract %v v%d, grant consumed by %v", contract, contractVersion, consumedBy)
		}
		var topic, traceparent string
		var payload []byte
		if err := tx.QueryRow(ctx, `SELECT topic, payload::text, traceparent FROM eacp.outbox_events
			WHERE aggregate_id = $1`, e.Action).Scan(&topic, &payload, &traceparent); err != nil {
			return err
		}
		var body map[string]any
		if err := json.Unmarshal(payload, &body); err != nil {
			return err
		}
		if topic != "action.queued" || len(body) != 1 || body["action_id"] != e.Action.String() ||
			!strings.HasPrefix(traceparent, "00-4bf92f35") {
			t.Errorf("outbox = %s %s %s", topic, payload, traceparent)
		}
		return nil
	})
	// Terminal and executable states cannot be re-released or re-authorized.
	wantCode(t, s.release(e.Action, "escalate", false), "55000")
}

func TestAllowReleaseRequiresVoidedApprovalState(t *testing.T) {
	s := newSchemaSetup(t)
	e := s.f.EscalatedAction(t, s.agent.Version, s.policy, "carol", "erp.purchase")
	for _, name := range []string{"amy", "ben"} {
		if err := s.f.Exec(name, `INSERT INTO eacp.approval_votes (tenant_id, request_id, decision, reason)
			VALUES (eacp.current_tenant_id(), $1, 'APPROVE', 'reviewed')`, e.Request); err != nil {
			t.Fatal(err)
		}
	}
	// Policy v2 allows the action outright.
	v2 := s.f.ID(t, "bob", `INSERT INTO eacp.policy_bundles (tenant_id, content)
		VALUES (eacp.current_tenant_id(), $1::jsonb) RETURNING id`,
		`{"format_version":1,"rules":[{"id":"all","verdict":"allow","reason":"permitted"}]}`)
	if err := s.f.Exec("alice", `UPDATE eacp.tenant_policy_pointer SET current_bundle_id = $1,
		activation_reason = 'v2' WHERE tenant_id = eacp.current_tenant_id()`, v2); err != nil {
		t.Fatal(err)
	}
	// The granted request of v1 is still live: an allow release must not
	// leave it behind.
	wantCode(t, s.release(e.Action, "allow", false), "55000")
	if err := s.f.ExecAgent(s.agent.Version, `UPDATE eacp.approval_requests SET state = 'VOIDED'
		WHERE id = $1`, e.Request); err != nil {
		t.Fatal(err)
	}
	if err := s.release(e.Action, "allow", false); err != nil {
		t.Fatal(err)
	}
	if got := s.state(t, e.Action); got != "QUEUED" {
		t.Fatalf("state = %s", got)
	}
}

func TestAllowedActionReleasesWithoutGrantAndPinsCurrentPolicy(t *testing.T) {
	s := newSchemaSetup(t)
	a := s.authorized(t)
	// Revalidation evidence committed in an earlier transaction is stale:
	// the pointer could have moved since, so it cannot release.
	stale := s.f.AgentID(t, s.agent.Version, registrytest.ReleaseEvidenceSQL, a, "allow")
	wantCode(t, s.f.ExecAgent(s.agent.Version, registrytest.QueueSQL, a, stale), "55000")
	// The release pins this tool's active contract, not any other.
	other := s.f.ActiveTool(t, "crm", "read")
	ctx := context.Background()
	wantCode(t, storage.InTenantTx(ctx, s.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		if err := storage.SetAgent(ctx, tx, s.agent.Version); err != nil {
			return err
		}
		var ev uuid.UUID
		if err := tx.QueryRow(ctx, registrytest.ReleaseEvidenceSQL, a, "allow").Scan(&ev); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE eacp.actions SET state = 'QUEUED', state_reason = 'released',
			decision_evidence_id = $2, connector_contract_id = $3 WHERE id = $1`, a, ev, other.Contract)
		return err
	}), "55000")
	if err := s.release(a, "allow", false); err != nil {
		t.Fatal(err)
	}
	if got := s.state(t, a); got != "QUEUED" {
		t.Fatalf("state = %s", got)
	}
	// A suspended agent version cannot be released.
	b := s.authorized(t)
	if err := s.f.Exec("otto", `UPDATE eacp.agent_versions SET state = 'SUSPENDED',
		state_reason = 'incident' WHERE id = $1`, s.agent.Version); err != nil {
		t.Fatal(err)
	}
	wantCode(t, s.release(b, "allow", false), "55000")
}

func TestVoteDenyAndRequestExpiryCascadeToAction(t *testing.T) {
	s := newSchemaSetup(t)
	e := s.f.EscalatedAction(t, s.agent.Version, s.policy, "carol", "erp.purchase")
	if err := s.f.Exec("amy", `INSERT INTO eacp.approval_votes (tenant_id, request_id, decision, reason)
		VALUES (eacp.current_tenant_id(), $1, 'DENY', 'too large')`, e.Request); err != nil {
		t.Fatal(err)
	}
	if got := s.state(t, e.Action); got != "DENIED" {
		t.Fatalf("deny vote left action %s (T7)", got)
	}
	e2 := s.f.EscalatedAction(t, s.agent.Version, s.policy, "carol", "erp.purchase")
	if err := s.f.Exec("carol", `UPDATE eacp.approval_requests SET expires_at = now() - interval '1 second'
		WHERE id = $1`, e2.Request); err != nil {
		t.Fatal(err)
	}
	if err := s.f.ExecSystem("sweeper", `UPDATE eacp.approval_requests SET state = 'EXPIRED'
		WHERE id = $1`, e2.Request); err != nil {
		t.Fatal(err)
	}
	if got := s.state(t, e2.Action); got != "EXPIRED" {
		t.Fatalf("request expiry left action %s (T8)", got)
	}
}

func TestCancelActorRulesAndVoidedApproval(t *testing.T) {
	s := newSchemaSetup(t)
	other := s.f.ActiveAgent(t, "other", s.tool.Tool)
	cancel := `UPDATE eacp.actions SET state = 'CANCELLED', state_reason = 'no longer needed' WHERE id = $1`
	e := s.f.EscalatedAction(t, s.agent.Version, s.policy, "carol", "erp.purchase")
	wantCode(t, s.f.ExecAgent(other.Version, cancel, e.Action), "42501")
	wantCode(t, s.f.Exec("erin", cancel, e.Action), "42501")
	wantCode(t, s.f.ExecSystem("sweeper", cancel, e.Action), "42501")
	if err := s.f.Exec("otto", cancel, e.Action); err != nil {
		t.Fatal(err)
	}
	// Cancellation voids the outstanding approval in the same transaction.
	s.query(t, func(tx pgx.Tx) error {
		var state string
		if err := tx.QueryRow(context.Background(), `SELECT state FROM eacp.approval_requests WHERE id = $1`,
			e.Request).Scan(&state); err != nil {
			return err
		}
		if state != "VOIDED" {
			t.Errorf("request after cancel = %s, want VOIDED", state)
		}
		return nil
	})
	wantCode(t, s.f.Exec("amy", `INSERT INTO eacp.approval_votes (tenant_id, request_id, decision, reason)
		VALUES (eacp.current_tenant_id(), $1, 'APPROVE', 'late')`, e.Request), "55000")
	// The subject and the submitting agent may cancel too.
	e2 := s.f.EscalatedAction(t, s.agent.Version, s.policy, "carol", "erp.purchase")
	if err := s.f.Exec("carol", cancel, e2.Action); err != nil {
		t.Fatal(err)
	}
	a := s.authorized(t)
	if err := s.f.ExecAgent(s.agent.Version, cancel, a); err != nil {
		t.Fatal(err)
	}
	// A live request of a non-terminal action cannot be voided.
	e3 := s.f.EscalatedAction(t, s.agent.Version, s.policy, "carol", "erp.purchase")
	wantCode(t, s.f.Exec("otto", `UPDATE eacp.approval_requests SET state = 'VOIDED' WHERE id = $1`, e3.Request), "55000")
}

func TestActionTransitionsAreJournaledWithActorKinds(t *testing.T) {
	s := newSchemaSetup(t)
	e := s.f.EscalatedAction(t, s.agent.Version, s.policy, "carol", "erp.purchase")
	for _, name := range []string{"amy", "ben"} {
		if err := s.f.Exec(name, `INSERT INTO eacp.approval_votes (tenant_id, request_id, decision, reason)
			VALUES (eacp.current_tenant_id(), $1, 'APPROVE', 'reviewed')`, e.Request); err != nil {
			t.Fatal(err)
		}
	}
	s.query(t, func(tx pgx.Tx) error {
		ctx := context.Background()
		rows, err := tx.Query(ctx, `SELECT convert_from(payload, 'UTF8') FROM eacp.audit_events
			WHERE convert_from(payload, 'UTF8')::jsonb #>> '{subject,id}' = $1 ORDER BY seq`, e.Action.String())
		if err != nil {
			return err
		}
		defer rows.Close()
		var kinds, moves []string
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				return err
			}
			for _, forbidden := range []string{"input_digest", "enforced_digest", "input_payload", "enforced_payload"} {
				if strings.Contains(raw, forbidden) {
					t.Errorf("action audit contains %s: %s", forbidden, raw)
				}
			}
			var ev struct {
				Actor struct{ Kind string } `json:"actor"`
				Data  struct{ From, To string }
			}
			if err := json.Unmarshal([]byte(raw), &ev); err != nil {
				return err
			}
			kinds = append(kinds, ev.Actor.Kind)
			moves = append(moves, ev.Data.From+">"+ev.Data.To)
		}
		want := []string{">RECEIVED", "RECEIVED>PENDING_APPROVAL", "PENDING_APPROVAL>AUTHORIZED"}
		if strings.Join(moves, ",") != strings.Join(want, ",") ||
			strings.Join(kinds, ",") != "agent,agent,principal" {
			t.Errorf("journal moves %v actors %v", moves, kinds)
		}
		return rows.Err()
	})
	err := storage.InTenantReadTx(context.Background(), s.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		_, err := audit.Verify(context.Background(), tx)
		return err
	})
	if err != nil {
		t.Fatalf("audit chain: %v", err)
	}
}

func TestCrossTenantScansExposeOnlyCountsAndTenantIDs(t *testing.T) {
	s := newSchemaSetup(t)
	a := s.authorized(t)
	if err := s.release(a, "allow", false); err != nil {
		t.Fatal(err)
	}
	s.f.ReceivedAction(t, s.agent.Version, "carol", "erp.purchase")
	ctx := context.Background()
	var queued int64
	if err := s.f.App.QueryRow(ctx, `SELECT eacp.global_queued_count()`).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("global queued = %d, err = %v", queued, err)
	}
	rows, err := s.f.App.Query(ctx, `SELECT tenant_id FROM eacp.tenants_with_open_actions(NULL, 10)`)
	if err != nil {
		t.Fatal(err)
	}
	tenants, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil || len(tenants) != 1 || tenants[0] != s.f.Tenant {
		t.Fatalf("tenants = %v, err = %v", tenants, err)
	}
}
