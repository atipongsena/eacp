-- Phase 21 (ADR-026 Rev 1.1): Governance-as-Code for identity, the tenant
-- policy, budgets and prices. No new table: change sets learn the new step
-- and ref kinds, one bundle may own the tenant policy, the digest learns the
-- new objects (never the budget counters, which move with every action), and
-- a stage that revoked an admin grant cannot commit below two admins.
-- +goose Up

ALTER TABLE eacp.change_set_steps DROP CONSTRAINT change_set_steps_address_check;
ALTER TABLE eacp.change_set_steps ADD CONSTRAINT change_set_steps_address_check CHECK (address ~
    '^(connector|tool|contract|agent|version|allowlist|principal|group|member|role|policy|budget|price)\.[a-z0-9][a-z0-9_-]{0,62}(\.[a-z0-9][a-z0-9_-]{0,62})?$');
ALTER TABLE eacp.change_set_steps DROP CONSTRAINT change_set_steps_op_check;
ALTER TABLE eacp.change_set_steps ADD CONSTRAINT change_set_steps_op_check
    CHECK (op IN ('create', 'propose', 'activate', 'transition', 'revoke', 'import', 'set'));
ALTER TABLE eacp.change_set_refs DROP CONSTRAINT change_set_refs_kind_check;
ALTER TABLE eacp.change_set_refs ADD CONSTRAINT change_set_refs_kind_check CHECK (kind IN ('connector', 'tool',
    'contract', 'agent', 'version', 'allowlist', 'principal', 'group', 'member', 'role', 'policy', 'budget', 'price'));
ALTER TABLE eacp.bundle_resources DROP CONSTRAINT bundle_resources_kind_check;
ALTER TABLE eacp.bundle_resources ADD CONSTRAINT bundle_resources_kind_check CHECK (kind IN ('connector', 'tool',
    'agent', 'version', 'principal', 'group', 'policy', 'budget', 'price'));
-- One bundle owns the tenant policy.
CREATE UNIQUE INDEX bundle_resources_one_policy ON eacp.bundle_resources (tenant_id) WHERE kind = 'policy';

-- +goose StatementBegin
-- The part of a registry object a plan depends on, as text, or 'absent'.
-- Under RLS it sees only the current tenant.
CREATE OR REPLACE FUNCTION eacp.change_set_ref_row(p_kind text, p_id uuid) RETURNS text
    LANGUAGE plpgsql STABLE
    AS $$
DECLARE
    r text;
BEGIN
    CASE p_kind
    WHEN 'connector' THEN
        SELECT jsonb_build_array(c.id, c.name, c.protocol, c.endpoint, c.secret_ref,
               (SELECT COALESCE(jsonb_agg(jsonb_build_array(t.id, t.name) ORDER BY t.id), '[]'::jsonb)
                  FROM eacp.tools t WHERE t.tenant_id = c.tenant_id AND t.connector_id = c.id))::text
          INTO r FROM eacp.connectors c WHERE c.id = p_id;
    WHEN 'tool' THEN
        SELECT jsonb_build_array(t.id, t.connector_id, t.name, t.origin, t.active_contract_id, t.definition_id,
               t.quarantined_at IS NULL)::text
          INTO r FROM eacp.tools t WHERE t.id = p_id;
    WHEN 'contract' THEN
        SELECT jsonb_build_array(ct.id, ct.tool_id, ct.revoked_at IS NULL)::text
          INTO r FROM eacp.tool_contracts ct WHERE ct.id = p_id;
    WHEN 'agent' THEN
        SELECT jsonb_build_array(a.id, a.name, a.display_name, a.environment, a.risk_class,
               a.owner_principal_id, a.owner_group_id,
               (SELECT COALESCE(jsonb_agg(jsonb_build_array(v.id, v.state, v.active_allowlist_id) ORDER BY v.version),
                                '[]'::jsonb)
                  FROM eacp.agent_versions v WHERE v.tenant_id = a.tenant_id AND v.agent_id = a.id))::text
          INTO r FROM eacp.agents a WHERE a.id = p_id;
    WHEN 'version' THEN
        SELECT jsonb_build_array(v.id, v.agent_id, v.version, v.runtime, v.code_ref, v.state,
               v.active_allowlist_id)::text
          INTO r FROM eacp.agent_versions v WHERE v.id = p_id;
    WHEN 'allowlist' THEN
        SELECT jsonb_build_array(al.id, al.agent_version_id, al.tool_ids)::text
          INTO r FROM eacp.agent_allowlists al WHERE al.id = p_id;
    WHEN 'principal' THEN
        SELECT jsonb_build_array(p.id, p.name, p.kind, p.subject, p.display_name, p.disabled_at IS NULL,
               (SELECT COALESCE(jsonb_agg(jsonb_build_array(g.id, g.role, g.approved_at IS NOT NULL) ORDER BY g.role),
                                '[]'::jsonb)
                  FROM eacp.role_grants g
                 WHERE g.tenant_id = p.tenant_id AND g.principal_id = p.id AND g.revoked_at IS NULL))::text
          INTO r FROM eacp.principals p WHERE p.id = p_id;
    WHEN 'group' THEN
        SELECT jsonb_build_array(g.id, g.name, g.display_name, g.schedule_weight,
               (SELECT COALESCE(jsonb_agg(jsonb_build_array(m.id, m.principal_id) ORDER BY m.id), '[]'::jsonb)
                  FROM eacp.group_memberships m
                 WHERE m.tenant_id = g.tenant_id AND m.group_id = g.id AND m.removed_at IS NULL))::text
          INTO r FROM eacp.groups g WHERE g.id = p_id;
    WHEN 'member' THEN
        SELECT jsonb_build_array(m.id, m.group_id, m.principal_id, m.removed_at IS NULL)::text
          INTO r FROM eacp.group_memberships m WHERE m.id = p_id;
    WHEN 'role' THEN
        SELECT jsonb_build_array(g.id, g.principal_id, g.role, g.approved_at IS NOT NULL, g.revoked_at IS NULL)::text
          INTO r FROM eacp.role_grants g WHERE g.id = p_id;
    WHEN 'policy' THEN
        -- A policy version, or the tenant itself when it has none: either way
        -- the pointer is part of the row, so every activation moves it.
        SELECT jsonb_build_array(p_id, pb.version, pb.revoked_at IS NULL, ptr.current_bundle_id)::text
          INTO r FROM eacp.tenant_policy_pointer ptr
          LEFT JOIN eacp.policy_bundles pb ON pb.tenant_id = ptr.tenant_id AND pb.id = p_id
         WHERE ptr.tenant_id = eacp.current_tenant_id() AND (pb.id IS NOT NULL OR p_id = ptr.tenant_id);
    WHEN 'budget' THEN
        -- Never allocated, reserved or committed: they move with every action.
        SELECT jsonb_build_array(b.id, b.name, b.unit, b.parent_id, b.agent_id, trim_scale(b.hard_limit)::text,
               (SELECT trim_scale(l.monthly_limit)::text FROM eacp.budget_soft_limits l
                 WHERE l.tenant_id = b.tenant_id AND l.account_id = b.id),
               (SELECT c.id FROM eacp.budget_limit_changes c
                 WHERE c.tenant_id = b.tenant_id AND c.account_id = b.id AND c.state = 'PROPOSED'
                 ORDER BY c.proposed_at DESC LIMIT 1))::text
          INTO r FROM eacp.budget_accounts b WHERE b.id = p_id;
    WHEN 'price' THEN
        SELECT jsonb_build_array(mp.id, mp.provider, mp.model,
               (SELECT cur.id FROM eacp.model_prices cur
                 WHERE cur.tenant_id = mp.tenant_id AND cur.provider = mp.provider AND cur.model = mp.model
                   AND cur.effective_from <= now()
                 ORDER BY cur.effective_from DESC LIMIT 1))::text
          INTO r FROM eacp.model_prices mp WHERE mp.id = p_id;
    ELSE
        RAISE EXCEPTION 'unknown ref kind %', p_kind USING ERRCODE = '23514';
    END CASE;
    RETURN COALESCE(r, 'absent');
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- An address manages an object only when the executing change set of its
-- bundle created or imported that object in this transaction.
CREATE OR REPLACE FUNCTION eacp.bundle_resources_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.current_actor_id();
    cs eacp.change_sets%ROWTYPE;
BEGIN
    IF TG_OP = 'UPDATE' AND (NEW.tenant_id, NEW.bundle_id, NEW.address)
                            IS DISTINCT FROM (OLD.tenant_id, OLD.bundle_id, OLD.address) THEN
        RAISE EXCEPTION 'a managed address is permanent' USING ERRCODE = '55000';
    END IF;
    SELECT * INTO cs FROM eacp.change_sets WHERE tenant_id = NEW.tenant_id AND id = NEW.change_set_id;
    IF NOT FOUND OR cs.bundle_id <> NEW.bundle_id OR a IS NULL OR NOT (
          (cs.state IN ('SUBMITTED', 'APPLIED') AND cs.submitted_by = a AND cs.submitted_xact = pg_current_xact_id()
           AND cs.approved_xact IS NULL AND cs.sealed_digest IS NULL)
       OR (cs.state = 'APPLIED' AND cs.approved_by = a AND cs.approved_xact = pg_current_xact_id())) THEN
        RAISE EXCEPTION 'only the executing change set of a bundle manages its objects' USING ERRCODE = '55000';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM eacp.change_set_steps
                    WHERE tenant_id = NEW.tenant_id AND change_set_id = cs.id AND address = NEW.address
                      AND op IN ('create', 'import') AND object_id = NEW.object_id
                      AND executed_xact = pg_current_xact_id()) THEN
        RAISE EXCEPTION 'a managed object is one this change set created or imported' USING ERRCODE = '55000';
    END IF;
    NEW.kind := split_part(NEW.address, '.', 1);
    IF NEW.kind NOT IN ('connector', 'tool', 'agent', 'version', 'principal', 'group', 'policy', 'budget', 'price') THEN
        RAISE EXCEPTION 'a bundle manages connectors, tools, agents, versions, principals, groups, the policy, budgets and prices'
            USING ERRCODE = '23514';
    END IF;
    IF eacp.change_set_ref_row(NEW.kind, NEW.object_id) = 'absent' THEN
        RAISE EXCEPTION 'no such % in this tenant', NEW.kind USING ERRCODE = '23503';
    END IF;
    NEW.managed_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- At commit: a plan has steps and is sealed; an executed stage ran every
-- step and, if it revoked an admin grant, leaves at least two admins; a
-- submission is sealed. The journal entry comes last.
CREATE OR REPLACE FUNCTION eacp.change_sets_commit() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    cs eacp.change_sets%ROWTYPE;
    bundle_name text;
    run_stage text;
    what text;
    who uuid;
    step_list jsonb;
BEGIN
    SELECT * INTO cs FROM eacp.change_sets WHERE tenant_id = NEW.tenant_id AND id = NEW.id;
    SELECT name INTO bundle_name FROM eacp.bundles WHERE tenant_id = cs.tenant_id AND id = cs.bundle_id;
    IF TG_OP = 'INSERT' THEN
        IF NOT EXISTS (SELECT 1 FROM eacp.change_set_steps WHERE tenant_id = cs.tenant_id AND change_set_id = cs.id) THEN
            RAISE EXCEPTION 'a change set has at least one step' USING ERRCODE = '23514';
        END IF;
        IF cs.base_digest IS NULL THEN
            RAISE EXCEPTION 'a plan is sealed before it commits' USING ERRCODE = '23514';
        END IF;
        what := 'change_set.planned';
        who := cs.planned_by;
    ELSE
        what := 'change_set.' || lower(NEW.state);
        CASE NEW.state
        WHEN 'SUBMITTED' THEN
            who := cs.submitted_by;
            run_stage := 'submit';
            IF cs.sealed_digest IS NULL THEN
                RAISE EXCEPTION 'a submission is sealed before it commits' USING ERRCODE = '23514';
            END IF;
        WHEN 'APPLIED' THEN
            IF cs.approved_xact IS NOT NULL THEN
                who := cs.approved_by;
                run_stage := 'approve';
            ELSE
                who := cs.submitted_by;
                run_stage := 'submit';
            END IF;
        ELSE
            who := cs.closed_by;
        END CASE;
        IF run_stage IS NOT NULL AND EXISTS (
            SELECT 1 FROM eacp.change_set_steps s
             WHERE s.tenant_id = cs.tenant_id AND s.change_set_id = cs.id AND s.stage = run_stage
               AND s.object_id IS NULL) THEN
            RAISE EXCEPTION 'every % step of a change set runs before it commits', run_stage USING ERRCODE = '23514';
        END IF;
        IF run_stage IS NOT NULL AND EXISTS (
            SELECT 1 FROM eacp.change_set_steps s
             WHERE s.tenant_id = cs.tenant_id AND s.change_set_id = cs.id AND s.stage = run_stage
               AND s.op = 'revoke' AND s.address ~ '^role\.[^.]+\.admin$')
           AND (SELECT count(*) FROM eacp.role_grants g
                  JOIN eacp.principals p ON p.tenant_id = g.tenant_id AND p.id = g.principal_id
                 WHERE g.tenant_id = cs.tenant_id AND g.role = 'admin' AND g.approved_at IS NOT NULL
                   AND g.revoked_at IS NULL AND p.kind = 'human' AND p.disabled_at IS NULL) < 2 THEN
            RAISE EXCEPTION 'change set % would leave fewer than two admins', cs.id
                USING ERRCODE = '23514', HINT = 'admin_floor';
        END IF;
    END IF;
    SELECT jsonb_agg(jsonb_build_object('ordinal', s.ordinal, 'address', s.address, 'op', s.op, 'stage', s.stage,
                                        'object_id', s.object_id) ORDER BY s.ordinal)
      INTO step_list FROM eacp.change_set_steps s WHERE s.tenant_id = cs.tenant_id AND s.change_set_id = cs.id;
    INSERT INTO eacp.audit_events (tenant_id, payload)
    VALUES (cs.tenant_id, convert_to(jsonb_build_object(
        'v', 1, 'actor', jsonb_build_object('kind', 'principal', 'id', who),
        'action', what,
        'subject', jsonb_build_object('type', 'change_set', 'id', cs.id),
        'reason', COALESCE(cs.close_reason, ''),
        'data', jsonb_build_object('bundle', bundle_name, 'desired_digest', encode(cs.desired_digest, 'hex'),
                                   'base_digest', encode(cs.base_digest, 'hex'),
                                   'sealed_digest', encode(cs.sealed_digest, 'hex'),
                                   'steps', COALESCE(step_list, '[]'::jsonb)))::text, 'UTF8'));
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose Down
DROP INDEX eacp.bundle_resources_one_policy;
ALTER TABLE eacp.bundle_resources DROP CONSTRAINT bundle_resources_kind_check;
ALTER TABLE eacp.bundle_resources ADD CONSTRAINT bundle_resources_kind_check
    CHECK (kind IN ('connector', 'tool', 'agent', 'version'));
ALTER TABLE eacp.change_set_refs DROP CONSTRAINT change_set_refs_kind_check;
ALTER TABLE eacp.change_set_refs ADD CONSTRAINT change_set_refs_kind_check CHECK (kind IN ('connector', 'tool',
    'contract', 'agent', 'version', 'allowlist', 'principal', 'group'));
ALTER TABLE eacp.change_set_steps DROP CONSTRAINT change_set_steps_op_check;
ALTER TABLE eacp.change_set_steps ADD CONSTRAINT change_set_steps_op_check
    CHECK (op IN ('create', 'propose', 'activate', 'transition', 'revoke', 'import'));
ALTER TABLE eacp.change_set_steps DROP CONSTRAINT change_set_steps_address_check;
ALTER TABLE eacp.change_set_steps ADD CONSTRAINT change_set_steps_address_check CHECK (address ~
    '^(connector|tool|contract|agent|version|allowlist)\.[a-z0-9][a-z0-9_-]{0,62}(\.[a-z0-9][a-z0-9_-]{0,62})?$');

-- +goose StatementBegin
-- The part of a registry object a plan depends on, as text, or 'absent'.
-- Under RLS it sees only the current tenant.
CREATE OR REPLACE FUNCTION eacp.change_set_ref_row(p_kind text, p_id uuid) RETURNS text
    LANGUAGE plpgsql STABLE
    AS $$
DECLARE
    r text;
BEGIN
    CASE p_kind
    WHEN 'connector' THEN
        SELECT jsonb_build_array(c.id, c.name, c.protocol, c.endpoint, c.secret_ref,
               (SELECT COALESCE(jsonb_agg(jsonb_build_array(t.id, t.name) ORDER BY t.id), '[]'::jsonb)
                  FROM eacp.tools t WHERE t.tenant_id = c.tenant_id AND t.connector_id = c.id))::text
          INTO r FROM eacp.connectors c WHERE c.id = p_id;
    WHEN 'tool' THEN
        SELECT jsonb_build_array(t.id, t.connector_id, t.name, t.origin, t.active_contract_id, t.definition_id,
               t.quarantined_at IS NULL)::text
          INTO r FROM eacp.tools t WHERE t.id = p_id;
    WHEN 'contract' THEN
        SELECT jsonb_build_array(ct.id, ct.tool_id, ct.revoked_at IS NULL)::text
          INTO r FROM eacp.tool_contracts ct WHERE ct.id = p_id;
    WHEN 'agent' THEN
        SELECT jsonb_build_array(a.id, a.name, a.display_name, a.environment, a.risk_class,
               a.owner_principal_id, a.owner_group_id,
               (SELECT COALESCE(jsonb_agg(jsonb_build_array(v.id, v.state, v.active_allowlist_id) ORDER BY v.version),
                                '[]'::jsonb)
                  FROM eacp.agent_versions v WHERE v.tenant_id = a.tenant_id AND v.agent_id = a.id))::text
          INTO r FROM eacp.agents a WHERE a.id = p_id;
    WHEN 'version' THEN
        SELECT jsonb_build_array(v.id, v.agent_id, v.version, v.runtime, v.code_ref, v.state,
               v.active_allowlist_id)::text
          INTO r FROM eacp.agent_versions v WHERE v.id = p_id;
    WHEN 'allowlist' THEN
        SELECT jsonb_build_array(al.id, al.agent_version_id, al.tool_ids)::text
          INTO r FROM eacp.agent_allowlists al WHERE al.id = p_id;
    WHEN 'principal' THEN
        SELECT jsonb_build_array(p.id, p.name, p.disabled_at IS NULL)::text
          INTO r FROM eacp.principals p WHERE p.id = p_id;
    WHEN 'group' THEN
        SELECT jsonb_build_array(g.id, g.name)::text
          INTO r FROM eacp.groups g WHERE g.id = p_id;
    ELSE
        RAISE EXCEPTION 'unknown ref kind %', p_kind USING ERRCODE = '23514';
    END CASE;
    RETURN COALESCE(r, 'absent');
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- An address manages an object only when the executing change set of its
-- bundle created or imported that object in this transaction.
CREATE OR REPLACE FUNCTION eacp.bundle_resources_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.current_actor_id();
    cs eacp.change_sets%ROWTYPE;
BEGIN
    IF TG_OP = 'UPDATE' AND (NEW.tenant_id, NEW.bundle_id, NEW.address)
                            IS DISTINCT FROM (OLD.tenant_id, OLD.bundle_id, OLD.address) THEN
        RAISE EXCEPTION 'a managed address is permanent' USING ERRCODE = '55000';
    END IF;
    SELECT * INTO cs FROM eacp.change_sets WHERE tenant_id = NEW.tenant_id AND id = NEW.change_set_id;
    IF NOT FOUND OR cs.bundle_id <> NEW.bundle_id OR a IS NULL OR NOT (
          (cs.state IN ('SUBMITTED', 'APPLIED') AND cs.submitted_by = a AND cs.submitted_xact = pg_current_xact_id()
           AND cs.approved_xact IS NULL AND cs.sealed_digest IS NULL)
       OR (cs.state = 'APPLIED' AND cs.approved_by = a AND cs.approved_xact = pg_current_xact_id())) THEN
        RAISE EXCEPTION 'only the executing change set of a bundle manages its objects' USING ERRCODE = '55000';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM eacp.change_set_steps
                    WHERE tenant_id = NEW.tenant_id AND change_set_id = cs.id AND address = NEW.address
                      AND op IN ('create', 'import') AND object_id = NEW.object_id
                      AND executed_xact = pg_current_xact_id()) THEN
        RAISE EXCEPTION 'a managed object is one this change set created or imported' USING ERRCODE = '55000';
    END IF;
    NEW.kind := split_part(NEW.address, '.', 1);
    IF NEW.kind NOT IN ('connector', 'tool', 'agent', 'version') THEN
        RAISE EXCEPTION 'a bundle manages connectors, tools, agents and versions' USING ERRCODE = '23514';
    END IF;
    IF eacp.change_set_ref_row(NEW.kind, NEW.object_id) = 'absent' THEN
        RAISE EXCEPTION 'no such % in this tenant', NEW.kind USING ERRCODE = '23503';
    END IF;
    NEW.managed_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- At commit: a plan has steps and is sealed; an executed stage ran every
-- step; a submission is sealed. The journal entry comes last.
CREATE OR REPLACE FUNCTION eacp.change_sets_commit() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    cs eacp.change_sets%ROWTYPE;
    bundle_name text;
    run_stage text;
    what text;
    who uuid;
    step_list jsonb;
BEGIN
    SELECT * INTO cs FROM eacp.change_sets WHERE tenant_id = NEW.tenant_id AND id = NEW.id;
    SELECT name INTO bundle_name FROM eacp.bundles WHERE tenant_id = cs.tenant_id AND id = cs.bundle_id;
    IF TG_OP = 'INSERT' THEN
        IF NOT EXISTS (SELECT 1 FROM eacp.change_set_steps WHERE tenant_id = cs.tenant_id AND change_set_id = cs.id) THEN
            RAISE EXCEPTION 'a change set has at least one step' USING ERRCODE = '23514';
        END IF;
        IF cs.base_digest IS NULL THEN
            RAISE EXCEPTION 'a plan is sealed before it commits' USING ERRCODE = '23514';
        END IF;
        what := 'change_set.planned';
        who := cs.planned_by;
    ELSE
        what := 'change_set.' || lower(NEW.state);
        CASE NEW.state
        WHEN 'SUBMITTED' THEN
            who := cs.submitted_by;
            run_stage := 'submit';
            IF cs.sealed_digest IS NULL THEN
                RAISE EXCEPTION 'a submission is sealed before it commits' USING ERRCODE = '23514';
            END IF;
        WHEN 'APPLIED' THEN
            IF cs.approved_xact IS NOT NULL THEN
                who := cs.approved_by;
                run_stage := 'approve';
            ELSE
                who := cs.submitted_by;
                run_stage := 'submit';
            END IF;
        ELSE
            who := cs.closed_by;
        END CASE;
        IF run_stage IS NOT NULL AND EXISTS (
            SELECT 1 FROM eacp.change_set_steps s
             WHERE s.tenant_id = cs.tenant_id AND s.change_set_id = cs.id AND s.stage = run_stage
               AND s.object_id IS NULL) THEN
            RAISE EXCEPTION 'every % step of a change set runs before it commits', run_stage USING ERRCODE = '23514';
        END IF;
    END IF;
    SELECT jsonb_agg(jsonb_build_object('ordinal', s.ordinal, 'address', s.address, 'op', s.op, 'stage', s.stage,
                                        'object_id', s.object_id) ORDER BY s.ordinal)
      INTO step_list FROM eacp.change_set_steps s WHERE s.tenant_id = cs.tenant_id AND s.change_set_id = cs.id;
    INSERT INTO eacp.audit_events (tenant_id, payload)
    VALUES (cs.tenant_id, convert_to(jsonb_build_object(
        'v', 1, 'actor', jsonb_build_object('kind', 'principal', 'id', who),
        'action', what,
        'subject', jsonb_build_object('type', 'change_set', 'id', cs.id),
        'reason', COALESCE(cs.close_reason, ''),
        'data', jsonb_build_object('bundle', bundle_name, 'desired_digest', encode(cs.desired_digest, 'hex'),
                                   'base_digest', encode(cs.base_digest, 'hex'),
                                   'sealed_digest', encode(cs.sealed_digest, 'hex'),
                                   'steps', COALESCE(step_list, '[]'::jsonb)))::text, 'UTF8'));
    RETURN NULL;
END
$$;
-- +goose StatementEnd
