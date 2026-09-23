// Package registrytest provides a migrated database with a bootstrapped
// tenant and a cast of principals for registry, identity and API tests.
package registrytest

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"eacp/internal/storage"
	"eacp/internal/storage/pgtest"
)

// Cast of tenant A principals and their (approved) roles.
//
//	alice, bob   human admin
//	erin         human registry_editor
//	rita, ravi   human registry_approver
//	otto         human operator
//	audra        human auditor
//	carol        human, no roles
//	amy, ben, cy human approver
//	ci           service auditor (the only role a service may hold)
var cast = []struct {
	name, kind string
	roles      []string
}{
	{"alice", "human", []string{"admin"}},
	{"bob", "human", []string{"admin"}},
	{"erin", "human", []string{"registry_editor"}},
	{"rita", "human", []string{"registry_approver"}},
	{"ravi", "human", []string{"registry_approver"}},
	{"otto", "human", []string{"operator"}},
	{"audra", "human", []string{"auditor"}},
	{"carol", "human", nil},
	{"amy", "human", []string{"approver"}},
	{"ben", "human", []string{"approver"}},
	{"cy", "human", []string{"approver"}},
	{"ci", "service", []string{"auditor"}},
}

// Fixture is a migrated database whose tenant A has been bootstrapped by the
// schema owner (the break-glass path) with the cast above.
type Fixture struct {
	DB     pgtest.DB
	App    *pgxpool.Pool // application role
	Owner  *pgxpool.Pool // schema owner
	Tenant uuid.UUID     // tenant A
	P      map[string]uuid.UUID
}

// New bootstraps the fixture.
func New(t testing.TB) *Fixture {
	t.Helper()
	db := pgtest.Migrated(t)
	f := &Fixture{
		DB: db, App: pgtest.Pool(t, db.AppDSN), Owner: pgtest.Pool(t, db.OwnerDSN),
		Tenant: uuid.MustParse(pgtest.TenantA), P: map[string]uuid.UUID{},
	}
	ctx := context.Background()
	err := storage.InTenantTx(ctx, f.Owner, pgtest.TenantA, func(tx pgx.Tx) error {
		for _, c := range cast {
			var subject *string
			if c.kind == "human" {
				s := c.name + "@tenant-a.test"
				subject = &s
			}
			var id uuid.UUID
			if err := tx.QueryRow(ctx, `
				INSERT INTO eacp.principals (tenant_id, kind, name, subject, display_name)
				VALUES (eacp.current_tenant_id(), $1, $2, $3, $2) RETURNING id`,
				c.kind, c.name, subject).Scan(&id); err != nil {
				return err
			}
			f.P[c.name] = id
			for _, r := range c.roles {
				if _, err := tx.Exec(ctx, `
					INSERT INTO eacp.role_grants (tenant_id, principal_id, role, approved_at)
					VALUES (eacp.current_tenant_id(), $1, $2, now())`, id, r); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("registrytest: bootstrap: %v", err)
	}
	return f
}

// Exec runs sql as the application role in tenant A with the named actor
// ("" for none).
func (f *Fixture) Exec(actor, sql string, args ...any) error {
	return f.ExecIn(pgtest.TenantA, actor, sql, args...)
}

// ExecIn is Exec in the given tenant.
func (f *Fixture) ExecIn(tenant, actor, sql string, args ...any) error {
	ctx := context.Background()
	return storage.InTenantTx(ctx, f.App, tenant, func(tx pgx.Tx) error {
		if err := f.setActor(ctx, tx, actor); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, sql, args...)
		return err
	})
}

// ID runs an INSERT ... RETURNING id as the application role in tenant A and
// fails the test on error.
func (f *Fixture) ID(t testing.TB, actor, sql string, args ...any) uuid.UUID {
	t.Helper()
	id, err := f.TryID(actor, sql, args...)
	if err != nil {
		t.Fatalf("registrytest: %v\nSQL: %s", err, sql)
	}
	return id
}

// TryID is ID returning the error.
func (f *Fixture) TryID(actor, sql string, args ...any) (uuid.UUID, error) {
	ctx := context.Background()
	var id uuid.UUID
	err := storage.InTenantTx(ctx, f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		if err := f.setActor(ctx, tx, actor); err != nil {
			return err
		}
		return tx.QueryRow(ctx, sql, args...).Scan(&id)
	})
	return id, err
}

// ExecAgent runs sql as the application role in tenant A with the agent
// version as the transaction's actor (an authenticated agent key).
func (f *Fixture) ExecAgent(version uuid.UUID, sql string, args ...any) error {
	ctx := context.Background()
	return storage.InTenantTx(ctx, f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		if err := storage.SetAgent(ctx, tx, version); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, sql, args...)
		return err
	})
}

// AgentID is ID with the agent version as the transaction's actor.
func (f *Fixture) AgentID(t testing.TB, version uuid.UUID, sql string, args ...any) uuid.UUID {
	t.Helper()
	id, err := f.TryAgentID(version, sql, args...)
	if err != nil {
		t.Fatalf("registrytest: %v\nSQL: %s", err, sql)
	}
	return id
}

// TryAgentID is AgentID returning the error.
func (f *Fixture) TryAgentID(version uuid.UUID, sql string, args ...any) (uuid.UUID, error) {
	ctx := context.Background()
	var id uuid.UUID
	err := storage.InTenantTx(ctx, f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		if err := storage.SetAgent(ctx, tx, version); err != nil {
			return err
		}
		return tx.QueryRow(ctx, sql, args...).Scan(&id)
	})
	return id, err
}

// ExecSystem runs sql in tenant A as the named system component.
func (f *Fixture) ExecSystem(component, sql string, args ...any) error {
	ctx := context.Background()
	return storage.InTenantTx(ctx, f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		if err := storage.SetSystem(ctx, tx, component); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, sql, args...)
		return err
	})
}

// ActionPayload is the canonical payload of fixture actions. Fixture
// digests are placeholders: 0x11 repeated (input) and 0x22 (enforced).
const ActionPayload = `{"amount":1000000,"currency":"THB"}`

// ReceivedActionSQL submits an action as raw SQL: $1 agent version, $2
// idempotency key, $3 subject, $4 tool reference.
const ReceivedActionSQL = `INSERT INTO eacp.actions
	(tenant_id, agent_version_id, idempotency_key, subject, operation, target, tool,
	 tool_schema_version, resource, input_payload, input_digest, not_after)
	VALUES (eacp.current_tenant_id(), $1, $2, $3, 'purchase', 'erp', $4, '1', 'po',
	 '` + ActionPayload + `', decode(repeat('11', 32), 'hex'), now() + interval '1 hour')
	RETURNING id`

// ReceivedAction submits a RECEIVED action by agent version for the named
// human subject (a cast name) and tool reference ("connector.tool").
func (f *Fixture) ReceivedAction(t testing.TB, version uuid.UUID, subject, tool string) uuid.UUID {
	t.Helper()
	return f.AgentID(t, version, ReceivedActionSQL, version, uuid.NewString(), subject+"@tenant-a.test", tool)
}

// EscalationEvidenceSQL records escalate evidence for action $1 under
// policy bundle $2 (version 1): quorum 2, TTL 600 s.
const EscalationEvidenceSQL = `INSERT INTO eacp.decision_evidence
	(tenant_id, action_id, policy_bundle_id, policy_version, provider, provider_instance_id,
	 decision_id, verdict, reasons, input_digest, enforced_digest, evaluated_at,
	 required_quorum, eligible_roles, approval_ttl_seconds, enforced_payload)
	VALUES (eacp.current_tenant_id(), $1, $2, 1, 'local', 'local-test', gen_random_uuid(),
	 'escalate', ARRAY['high risk'], decode(repeat('11', 32), 'hex'),
	 decode(repeat('22', 32), 'hex'), now(), 2, ARRAY['approver'], 600,
	 '` + ActionPayload + `'::jsonb)
	RETURNING id`

// ApprovalRequestSQL opens a request for action $1 bound to evidence $2
// expiring in 9 minutes. The database derives the rest from the action.
const ApprovalRequestSQL = `INSERT INTO eacp.approval_requests
	(tenant_id, action_id, decision_evidence_id, expires_at)
	VALUES (eacp.current_tenant_id(), $1, $2, now() + interval '9 minutes') RETURNING id`

// EscalateSQL is transition T4 for action $1 with evidence $2 and request $3.
const EscalateSQL = `UPDATE eacp.actions
	SET state = 'PENDING_APPROVAL', state_reason = 'high risk',
	    decision_evidence_id = $2, approval_request_id = $3,
	    enforced_payload = '` + ActionPayload + `'
	WHERE id = $1`

// ReleaseEvidenceSQL records a revalidation of action $1 with verdict $2
// under the current policy, repeating the action's digests and payload.
const ReleaseEvidenceSQL = `INSERT INTO eacp.decision_evidence
	(tenant_id, action_id, policy_bundle_id, policy_version, provider, provider_instance_id,
	 decision_id, verdict, reasons, input_digest, enforced_digest, evaluated_at, enforced_payload,
	 required_quorum, eligible_roles, approval_ttl_seconds)
	SELECT a.tenant_id, a.id, p.current_bundle_id, p.current_version, 'local', 'local-test',
	 gen_random_uuid(), $2::text, ARRAY['revalidated'], a.input_digest, a.enforced_digest, now(),
	 a.enforced_payload::jsonb,
	 CASE WHEN $2::text = 'escalate' THEN 2 END,
	 CASE WHEN $2::text = 'escalate' THEN ARRAY['approver'] END,
	 CASE WHEN $2::text = 'escalate' THEN 600 END
	FROM eacp.actions a JOIN eacp.tenant_policy_pointer p ON p.tenant_id = a.tenant_id
	WHERE a.id = $1 RETURNING id`

// ConsumeGrantSQL consumes action $1's grant for its digest and policy.
const ConsumeGrantSQL = `UPDATE eacp.approval_grants g
	SET consumed_at = now(), consumed_by_action_id = a.id
	FROM eacp.actions a
	WHERE a.id = $1 AND g.tenant_id = a.tenant_id AND g.action_id = a.id
	  AND g.enforced_digest = a.enforced_digest AND g.policy_version = a.policy_version
	  AND g.consumed_at IS NULL AND g.expires_at > now()`

// QueueSQL is transition T10 for action $1 with release evidence $2,
// pinning the tool's active contract.
const QueueSQL = `UPDATE eacp.actions a
	SET state = 'QUEUED', state_reason = 'released', decision_evidence_id = $2,
	    connector_contract_id = t.active_contract_id
	FROM eacp.tools t
	WHERE a.id = $1 AND t.tenant_id = a.tenant_id AND t.id = a.tool_id`

// Escalated is an action in PENDING_APPROVAL with its evidence and request.
type Escalated struct {
	Action, Evidence, Request uuid.UUID
}

// EscalatedAction submits an action and moves it to PENDING_APPROVAL (T4)
// under the active policy bundle (which must be version 1), as the agent.
func (f *Fixture) EscalatedAction(t testing.TB, version, policy uuid.UUID, subject, tool string) Escalated {
	t.Helper()
	var e Escalated
	e.Action = f.ReceivedAction(t, version, subject, tool)
	e.Evidence = f.AgentID(t, version, EscalationEvidenceSQL, e.Action, policy)
	e.Request = f.AgentID(t, version, ApprovalRequestSQL, e.Action, e.Evidence)
	if err := f.ExecAgent(version, EscalateSQL, e.Action, e.Evidence, e.Request); err != nil {
		t.Fatalf("registrytest: escalate: %v", err)
	}
	return e
}

func (f *Fixture) setActor(ctx context.Context, tx pgx.Tx, actor string) error {
	if actor == "" {
		return nil
	}
	id, ok := f.P[actor]
	if !ok {
		id = uuid.MustParse(actor)
	}
	return storage.SetActor(ctx, tx, id)
}

// Tooling is a connector with one tool whose contract is active.
type Tooling struct {
	Connector, Tool, Contract uuid.UUID
}

// ActiveTool registers connector/tool (by erin) with a safe read-only
// contract (by erin) activated by rita.
func (f *Fixture) ActiveTool(t testing.TB, connector, tool string) Tooling {
	t.Helper()
	var r Tooling
	r.Connector = f.ID(t, "erin", `INSERT INTO eacp.connectors (tenant_id, name, protocol, endpoint, secret_ref)
		VALUES (eacp.current_tenant_id(), $1, 'http', 'http://fakeerp:8090', $1) RETURNING id`, connector)
	r.Tool = f.ID(t, "erin", `INSERT INTO eacp.tools (tenant_id, connector_id, name)
		VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, r.Connector, tool)
	r.Contract = f.ID(t, "erin", SafeContractSQL, r.Tool)
	if err := f.Exec("rita", `UPDATE eacp.tools SET active_contract_id = $1 WHERE id = $2`, r.Contract, r.Tool); err != nil {
		t.Fatalf("registrytest: activate contract: %v", err)
	}
	return r
}

// SafeContractSQL inserts a valid read-only contract for tool $1.
const SafeContractSQL = `INSERT INTO eacp.tool_contracts
	(tenant_id, tool_id, side_effects, idempotency_mode, reconciliation_lookup,
	 reconciliation_consistency, proof_standard, max_attempts)
	VALUES (eacp.current_tenant_id(), $1, '{READ_ONLY}', 'none', 'none', 'none', 'none', 3)
	RETURNING id`

// Agent is an agent with one version.
type Agent struct {
	Agent, Version uuid.UUID
}

// NewAgent registers an agent owned by carol and a version, both by erin.
func (f *Fixture) NewAgent(t testing.TB, name string) Agent {
	t.Helper()
	var a Agent
	a.Agent = f.ID(t, "erin", `INSERT INTO eacp.agents
		(tenant_id, name, display_name, environment, risk_class, owner_principal_id)
		VALUES (eacp.current_tenant_id(), $1, $1, 'production', 'high', $2) RETURNING id`, name, f.P["carol"])
	a.Version = f.ID(t, "erin", `INSERT INTO eacp.agent_versions (tenant_id, agent_id, runtime, code_ref)
		VALUES (eacp.current_tenant_id(), $1, 'python', 'git:abc123') RETURNING id`, a.Agent)
	return a
}

// ActiveAgent is NewAgent plus an allowlist of tools (proposed by erin,
// activated by rita) and activation of the version by ravi.
func (f *Fixture) ActiveAgent(t testing.TB, name string, tools ...uuid.UUID) Agent {
	t.Helper()
	a := f.NewAgent(t, name)
	al := f.ID(t, "erin", `INSERT INTO eacp.agent_allowlists (tenant_id, agent_version_id, tool_ids)
		VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, a.Version, tools)
	if err := f.Exec("rita", `UPDATE eacp.agent_versions SET active_allowlist_id = $1 WHERE id = $2`, al, a.Version); err != nil {
		t.Fatalf("registrytest: activate allowlist: %v", err)
	}
	if err := f.Exec("ravi", `UPDATE eacp.agent_versions SET state = 'ACTIVE', state_reason = 'go live' WHERE id = $1`, a.Version); err != nil {
		t.Fatalf("registrytest: activate version: %v", err)
	}
	return a
}
