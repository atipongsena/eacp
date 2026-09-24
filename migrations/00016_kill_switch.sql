-- Distributed, tenant-scoped execution kills. PostgreSQL is authoritative;
-- NATS is a signal only (ADR-016). No kill changes an external outcome.
-- +goose Up

CREATE TABLE eacp.kill_tenant_epochs (
    tenant_id uuid PRIMARY KEY REFERENCES eacp.tenants(id),
    epoch bigint NOT NULL CHECK (epoch >= 0)
);
ALTER TABLE eacp.kill_tenant_epochs ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.kill_tenant_epochs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.kill_tenant_epochs USING (tenant_id = eacp.current_tenant_id());
CREATE POLICY owner_scan ON eacp.kill_tenant_epochs FOR SELECT TO CURRENT_USER USING (true);
REVOKE INSERT, UPDATE, DELETE ON eacp.kill_tenant_epochs FROM eacp_app;

CREATE TABLE eacp.kill_states (
    tenant_id uuid NOT NULL REFERENCES eacp.tenants(id),
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    scope text NOT NULL CHECK (scope IN ('tenant', 'team', 'agent', 'agent_version', 'action', 'connector', 'tool')),
    target_id uuid NOT NULL,
    killed boolean NOT NULL,
    epoch bigint NOT NULL CHECK (epoch > 0),
    reason_code text NOT NULL CHECK (reason_code IN
        ('policy_violation', 'security_incident', 'operator_request', 'error_budget_exhausted')),
    reason text NOT NULL CHECK (btrim(reason) <> '' AND length(reason) <= 1024),
    changed_by uuid NOT NULL,
    changed_at timestamptz NOT NULL DEFAULT now(),
    killed_by uuid NOT NULL,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, scope, target_id),
    FOREIGN KEY (tenant_id, changed_by) REFERENCES eacp.principals(tenant_id, id),
    FOREIGN KEY (tenant_id, killed_by) REFERENCES eacp.principals(tenant_id, id)
);
CREATE INDEX kill_states_active ON eacp.kill_states(tenant_id, scope, target_id) WHERE killed;
ALTER TABLE eacp.kill_states ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.kill_states FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.kill_states USING (tenant_id = eacp.current_tenant_id());
CREATE POLICY owner_scan ON eacp.kill_states FOR SELECT TO CURRENT_USER USING (true);
REVOKE INSERT, UPDATE, DELETE ON eacp.kill_states FROM eacp_app;

-- The outbox also carries kill state changes. Its existing aggregate FK was
-- action-only; the guard below keeps each topic tied to its source row.
ALTER TABLE eacp.outbox_events DROP CONSTRAINT outbox_events_tenant_id_aggregate_id_fkey;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.outbox_events_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF pg_trigger_depth() < 2 THEN
        RAISE EXCEPTION 'outbox events are written only by source triggers' USING ERRCODE = '42501';
    END IF;
    PERFORM eacp.actor_context();
    IF NEW.published_at IS NOT NULL THEN
        RAISE EXCEPTION 'an outbox event starts unpublished' USING ERRCODE = '23514';
    END IF;
    IF NEW.topic IN ('action.queued', 'action.transition') THEN
        PERFORM 1 FROM eacp.actions WHERE tenant_id = NEW.tenant_id AND id = NEW.aggregate_id;
        IF NOT FOUND THEN RAISE EXCEPTION 'action outbox aggregate is absent' USING ERRCODE = '23503'; END IF;
    ELSIF NEW.topic = 'kill.changed' THEN
        PERFORM 1 FROM eacp.kill_states k WHERE k.tenant_id = NEW.tenant_id AND k.id = NEW.aggregate_id
          AND NEW.payload = jsonb_build_object('scope', k.scope, 'target_id', k.target_id, 'epoch', k.epoch);
        IF NOT FOUND THEN RAISE EXCEPTION 'kill outbox does not match state' USING ERRCODE = '23514'; END IF;
    ELSE
        RAISE EXCEPTION 'unrecognized outbox topic' USING ERRCODE = '23514';
    END IF;
    NEW.created_at := now();
    RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.kill_outbox() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO eacp.outbox_events(tenant_id, topic, aggregate_id, payload)
    VALUES (NEW.tenant_id, 'kill.changed', NEW.id,
            jsonb_build_object('scope', NEW.scope, 'target_id', NEW.target_id, 'epoch', NEW.epoch));
    RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER kill_outbox AFTER INSERT OR UPDATE ON eacp.kill_states
    FOR EACH ROW EXECUTE FUNCTION eacp.kill_outbox();

-- +goose StatementBegin
-- The operator's one entry point. The advisory lock serializes a missing-row
-- insert with every T14/T16 check for this tenant. The journal is last.
CREATE FUNCTION eacp.set_kill(p_scope text, p_target uuid, p_killed boolean, p_reason text,
                              p_code text DEFAULT 'operator_request') RETURNS jsonb
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    tenant uuid := eacp.current_tenant_id();
    actor uuid := eacp.current_actor_id();
    prior eacp.kill_states%ROWTYPE;
    changed eacp.kill_states%ROWTYPE;
    next_epoch bigint;
    valid_target boolean;
BEGIN
    IF tenant IS NULL OR actor IS NULL THEN
        RAISE EXCEPTION 'operator and tenant context are required' USING ERRCODE = '42501';
    END IF;
    PERFORM eacp.assert_role(actor, 'operator');
    IF p_scope IS NULL OR p_target IS NULL OR p_killed IS NULL OR p_reason IS NULL
       OR btrim(p_reason) = '' OR length(p_reason) > 1024 THEN
        RAISE EXCEPTION 'scope, target, state and reason are required' USING ERRCODE = '23514';
    END IF;
    IF p_code IS NULL OR p_code NOT IN
       ('policy_violation', 'security_incident', 'operator_request', 'error_budget_exhausted') THEN
        RAISE EXCEPTION 'unsupported kill reason code' USING ERRCODE = '23514';
    END IF;
    PERFORM pg_advisory_xact_lock(hashtextextended('eacp.kill:' || tenant::text, 0));
    CASE p_scope
    WHEN 'tenant' THEN valid_target := p_target = tenant;
    WHEN 'team' THEN SELECT EXISTS(SELECT 1 FROM eacp.groups WHERE tenant_id = tenant AND id = p_target) INTO valid_target;
    WHEN 'agent' THEN SELECT EXISTS(SELECT 1 FROM eacp.agents WHERE tenant_id = tenant AND id = p_target) INTO valid_target;
    WHEN 'agent_version' THEN SELECT EXISTS(SELECT 1 FROM eacp.agent_versions WHERE tenant_id = tenant AND id = p_target) INTO valid_target;
    WHEN 'action' THEN SELECT EXISTS(SELECT 1 FROM eacp.actions WHERE tenant_id = tenant AND id = p_target) INTO valid_target;
    WHEN 'connector' THEN SELECT EXISTS(SELECT 1 FROM eacp.connectors WHERE tenant_id = tenant AND id = p_target) INTO valid_target;
    WHEN 'tool' THEN SELECT EXISTS(SELECT 1 FROM eacp.tools WHERE tenant_id = tenant AND id = p_target) INTO valid_target;
    ELSE RAISE EXCEPTION 'unsupported kill scope' USING ERRCODE = '23514';
    END CASE;
    IF NOT valid_target THEN RAISE EXCEPTION 'kill target is not in this tenant' USING ERRCODE = '23503'; END IF;
    SELECT * INTO prior FROM eacp.kill_states
     WHERE tenant_id = tenant AND scope = p_scope AND target_id = p_target FOR UPDATE;
    IF p_killed IS FALSE AND (NOT FOUND OR prior.killed IS FALSE) THEN
        RAISE EXCEPTION 'scope is not killed' USING ERRCODE = '55000';
    END IF;
    IF p_killed IS FALSE AND prior.killed_by = actor THEN
        RAISE EXCEPTION 'the operator who killed the scope cannot resume it' USING ERRCODE = '42501';
    END IF;
    INSERT INTO eacp.kill_states(tenant_id, scope, target_id, killed, epoch, reason_code, reason, changed_by, killed_by)
    VALUES (tenant, p_scope, p_target, p_killed, 1, p_code, p_reason, actor, actor)
    ON CONFLICT (tenant_id, scope, target_id) DO UPDATE SET
       killed = EXCLUDED.killed, epoch = eacp.kill_states.epoch + 1,
       reason_code = EXCLUDED.reason_code, reason = EXCLUDED.reason, changed_by = actor, changed_at = now(),
       killed_by = CASE WHEN EXCLUDED.killed THEN actor ELSE eacp.kill_states.killed_by END
    RETURNING * INTO changed;
    INSERT INTO eacp.kill_tenant_epochs(tenant_id, epoch) VALUES (tenant, 1)
    ON CONFLICT (tenant_id) DO UPDATE SET epoch = eacp.kill_tenant_epochs.epoch + 1
    RETURNING epoch INTO next_epoch;
    -- This is the last statement after state and outbox, as for registry changes.
    INSERT INTO eacp.audit_events(tenant_id, payload)
    VALUES (tenant, convert_to(jsonb_build_object(
        'v', 1, 'actor', jsonb_build_object('kind', 'principal', 'id', actor),
        'action', CASE WHEN p_killed THEN 'kill.activated' ELSE 'kill.resumed' END,
        'subject', jsonb_build_object('type', p_scope, 'id', p_target),
        'reason', p_reason, 'data', jsonb_build_object('reason_code', p_code,
            'scope_epoch', changed.epoch, 'tenant_epoch', next_epoch)
    )::text, 'UTF8'));
    RETURN to_jsonb(changed);
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION eacp.set_kill(text, uuid, boolean, text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.set_kill(text, uuid, boolean, text, text) TO eacp_app;

-- +goose StatementBegin
-- A scope with no stable action binding cannot silently match nothing.
CREATE FUNCTION eacp.action_killed(a eacp.actions) RETURNS boolean LANGUAGE sql STABLE AS $$
    SELECT EXISTS (
        SELECT 1 FROM eacp.kill_states k
        LEFT JOIN eacp.tools t ON t.tenant_id = a.tenant_id AND t.id = a.tool_id
        WHERE k.tenant_id = a.tenant_id AND k.killed AND (
            (k.scope = 'tenant' AND k.target_id = a.tenant_id) OR
            (k.scope = 'team' AND a.schedule_team_key = 'group:' || k.target_id::text) OR
            (k.scope = 'agent' AND k.target_id = a.agent_id) OR
            (k.scope = 'agent_version' AND k.target_id = a.agent_version_id) OR
            (k.scope = 'action' AND k.target_id = a.id) OR
            (k.scope = 'connector' AND k.target_id = t.connector_id) OR
            (k.scope = 'tool' AND k.target_id = a.tool_id))) $$;
-- +goose StatementEnd

ALTER TABLE eacp.actions ADD COLUMN dispatch_kill_epoch bigint NOT NULL DEFAULT 0 CHECK (dispatch_kill_epoch >= 0);

-- +goose StatementBegin
CREATE FUNCTION eacp.actions_kill_before() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (OLD.state = 'QUEUED' AND NEW.state = 'LEASED') OR
       (OLD.state = 'LEASED' AND NEW.state = 'EXECUTING') THEN
        PERFORM pg_advisory_xact_lock(hashtextextended('eacp.kill:' || NEW.tenant_id::text, 0));
        IF eacp.action_killed(NEW) THEN
            RAISE EXCEPTION 'action scope is killed' USING ERRCODE = '53300';
        END IF;
        IF NEW.state = 'EXECUTING' THEN
            NEW.dispatch_kill_epoch := COALESCE(
                (SELECT epoch FROM eacp.kill_tenant_epochs WHERE tenant_id = NEW.tenant_id), 0);
        END IF;
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER actions_kill_before BEFORE UPDATE ON eacp.actions
    FOR EACH ROW EXECUTE FUNCTION eacp.actions_kill_before();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.claimable_actions(p_protocols text[], p_bindings jsonb, lim integer, p_skip jsonb)
    RETURNS TABLE (tenant_id uuid, action_id uuid)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    WITH active_capacity AS MATERIALIZED (
        SELECT x.tenant_id,
               COALESCE('group:' || ac.concurrency_group, 'connector:' || xc.id::text) AS capacity_key,
               count(*)::integer AS active_count, min(ac.max_inflight) AS active_cap
          FROM eacp.actions x
          JOIN eacp.tool_contracts ac ON ac.tenant_id = x.tenant_id AND ac.id = x.connector_contract_id
          JOIN eacp.tools xt ON xt.tenant_id = ac.tenant_id AND xt.id = ac.tool_id
          JOIN eacp.connectors xc ON xc.tenant_id = xt.tenant_id AND xc.id = xt.connector_id
         WHERE x.state IN ('LEASED', 'EXECUTING')
         GROUP BY x.tenant_id, COALESCE('group:' || ac.concurrency_group, 'connector:' || xc.id::text)
    ), skip AS MATERIALIZED (
        SELECT s.tenant_id, s.key FROM jsonb_to_recordset(COALESCE(p_skip, '[]'::jsonb)) AS s(tenant_id uuid, key text)
    )
    SELECT a.tenant_id, a.id
      FROM eacp.actions a
      JOIN eacp.tools t ON t.tenant_id = a.tenant_id AND t.id = a.tool_id
      JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
      JOIN eacp.tool_contracts ct ON ct.tenant_id = a.tenant_id AND ct.id = a.connector_contract_id
      LEFT JOIN eacp.scheduler_tenant_state st ON st.tenant_id = a.tenant_id
      LEFT JOIN eacp.scheduler_team_state ss ON ss.tenant_id = a.tenant_id
        AND ss.team_key = COALESCE(a.schedule_team_key, 'agent:' || a.agent_id::text)
      LEFT JOIN active_capacity used ON used.tenant_id = a.tenant_id
        AND used.capacity_key = COALESCE('group:' || ct.concurrency_group, 'connector:' || c.id::text)
     WHERE a.state = 'QUEUED' AND a.not_after > now() AND c.protocol = ANY (p_protocols)
       AND EXISTS (SELECT 1 FROM jsonb_to_recordset(p_bindings) AS b(tenant_id uuid, secret_ref text, host text)
                   WHERE b.tenant_id = a.tenant_id AND b.secret_ref = c.secret_ref
                     AND b.host = lower(substring(c.endpoint FROM '^https?://([^/?#]+)')))
       AND (COALESCE(LEAST(ct.max_inflight, used.active_cap), ct.max_inflight, used.active_cap) IS NULL
            OR COALESCE(used.active_count, 0)
               < COALESCE(LEAST(ct.max_inflight, used.active_cap), ct.max_inflight, used.active_cap))
       AND NOT EXISTS (SELECT 1 FROM eacp.connector_circuits cc
                       WHERE cc.tenant_id = a.tenant_id AND cc.connector_id = c.id
                         AND (cc.disabled OR cc.open_until > now()))
       AND NOT EXISTS (SELECT 1 FROM skip s
                       WHERE s.tenant_id = a.tenant_id
                         AND s.key IN (COALESCE('group:' || ct.concurrency_group, 'connector:' || c.id::text),
                                       'connector:' || c.id::text))
       AND NOT eacp.action_killed(a)
     ORDER BY COALESCE(st.last_turn, 0), a.tenant_id,
              COALESCE(ss.last_turn, 0), COALESCE(a.schedule_team_key, 'agent:' || a.agent_id::text),
              CASE WHEN a.not_after <= now() + interval '5 minutes' THEN 9
                   ELSE LEAST(9, COALESCE(a.schedule_priority, 0)
                        + floor(extract(epoch FROM (now() - a.state_changed_at)) / 300)::integer)
                   END DESC,
              a.not_after, a.state_changed_at, a.id
     LIMIT least(greatest(lim, 1), 100)
    $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.claimable_actions(p_protocols text[], p_bindings jsonb, lim integer, p_skip jsonb)
    RETURNS TABLE (tenant_id uuid, action_id uuid)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    WITH active_capacity AS MATERIALIZED (
        SELECT x.tenant_id,
               COALESCE('group:' || ac.concurrency_group, 'connector:' || xc.id::text) AS capacity_key,
               count(*)::integer AS active_count, min(ac.max_inflight) AS active_cap
          FROM eacp.actions x
          JOIN eacp.tool_contracts ac ON ac.tenant_id = x.tenant_id AND ac.id = x.connector_contract_id
          JOIN eacp.tools xt ON xt.tenant_id = ac.tenant_id AND xt.id = ac.tool_id
          JOIN eacp.connectors xc ON xc.tenant_id = xt.tenant_id AND xc.id = xt.connector_id
         WHERE x.state IN ('LEASED', 'EXECUTING')
         GROUP BY x.tenant_id, COALESCE('group:' || ac.concurrency_group, 'connector:' || xc.id::text)
    ), skip AS MATERIALIZED (
        SELECT s.tenant_id, s.key FROM jsonb_to_recordset(COALESCE(p_skip, '[]'::jsonb)) AS s(tenant_id uuid, key text)
    )
    SELECT a.tenant_id, a.id
      FROM eacp.actions a
      JOIN eacp.tools t ON t.tenant_id = a.tenant_id AND t.id = a.tool_id
      JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
      JOIN eacp.tool_contracts ct ON ct.tenant_id = a.tenant_id AND ct.id = a.connector_contract_id
      LEFT JOIN eacp.scheduler_tenant_state st ON st.tenant_id = a.tenant_id
      LEFT JOIN eacp.scheduler_team_state ss ON ss.tenant_id = a.tenant_id
        AND ss.team_key = COALESCE(a.schedule_team_key, 'agent:' || a.agent_id::text)
      LEFT JOIN active_capacity used ON used.tenant_id = a.tenant_id
        AND used.capacity_key = COALESCE('group:' || ct.concurrency_group, 'connector:' || c.id::text)
     WHERE a.state = 'QUEUED' AND a.not_after > now() AND c.protocol = ANY (p_protocols)
       AND EXISTS (SELECT 1 FROM jsonb_to_recordset(p_bindings) AS b(tenant_id uuid, secret_ref text, host text)
                   WHERE b.tenant_id = a.tenant_id AND b.secret_ref = c.secret_ref
                     AND b.host = lower(substring(c.endpoint FROM '^https?://([^/?#]+)')))
       AND (COALESCE(LEAST(ct.max_inflight, used.active_cap), ct.max_inflight, used.active_cap) IS NULL
            OR COALESCE(used.active_count, 0)
               < COALESCE(LEAST(ct.max_inflight, used.active_cap), ct.max_inflight, used.active_cap))
       AND NOT EXISTS (SELECT 1 FROM eacp.connector_circuits cc
                       WHERE cc.tenant_id = a.tenant_id AND cc.connector_id = c.id
                         AND (cc.disabled OR cc.open_until > now()))
       AND NOT EXISTS (SELECT 1 FROM skip s
                       WHERE s.tenant_id = a.tenant_id
                         AND s.key IN (COALESCE('group:' || ct.concurrency_group, 'connector:' || c.id::text),
                                       'connector:' || c.id::text))
     ORDER BY COALESCE(st.last_turn, 0), a.tenant_id,
              COALESCE(ss.last_turn, 0), COALESCE(a.schedule_team_key, 'agent:' || a.agent_id::text),
              CASE WHEN a.not_after <= now() + interval '5 minutes' THEN 9
                   ELSE LEAST(9, COALESCE(a.schedule_priority, 0)
                        + floor(extract(epoch FROM (now() - a.state_changed_at)) / 300)::integer)
                   END DESC,
              a.not_after, a.state_changed_at, a.id
     LIMIT least(greatest(lim, 1), 100)
    $$;
-- +goose StatementEnd
DROP TRIGGER actions_kill_before ON eacp.actions;
DROP FUNCTION eacp.actions_kill_before();
ALTER TABLE eacp.actions DROP COLUMN dispatch_kill_epoch;
DROP FUNCTION eacp.action_killed(eacp.actions);
DROP FUNCTION eacp.set_kill(text, uuid, boolean, text, text);
DROP TRIGGER kill_outbox ON eacp.kill_states;
DROP FUNCTION eacp.kill_outbox();
DROP TABLE eacp.kill_states;
DROP TABLE eacp.kill_tenant_epochs;
ALTER TABLE eacp.outbox_events ADD FOREIGN KEY (tenant_id, aggregate_id) REFERENCES eacp.actions(tenant_id, id);
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.outbox_events_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF pg_trigger_depth() < 2 THEN
        RAISE EXCEPTION 'outbox events are written only by the action triggers' USING ERRCODE = '42501';
    END IF;
    PERFORM eacp.actor_context();
    IF NEW.published_at IS NOT NULL THEN
        RAISE EXCEPTION 'an outbox event starts unpublished' USING ERRCODE = '23514';
    END IF;
    NEW.created_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd
