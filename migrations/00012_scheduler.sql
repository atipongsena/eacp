-- Phase 12: PostgreSQL fair claim order and connector capacity (ADR-011).
-- +goose Up

ALTER TABLE eacp.groups
    ADD COLUMN schedule_weight integer NOT NULL DEFAULT 1 CHECK (schedule_weight BETWEEN 1 AND 10);
ALTER TABLE eacp.tool_contracts
    ADD COLUMN schedule_priority integer NOT NULL DEFAULT 0 CHECK (schedule_priority BETWEEN 0 AND 9);
ALTER TABLE eacp.actions
    ADD COLUMN schedule_team_key text,
    ADD COLUMN schedule_weight integer CHECK (schedule_weight BETWEEN 1 AND 10),
    ADD COLUMN schedule_priority integer CHECK (schedule_priority BETWEEN 0 AND 9);

CREATE SEQUENCE eacp.scheduler_turn_seq;

CREATE TABLE eacp.scheduler_tenant_state (
    tenant_id uuid PRIMARY KEY REFERENCES eacp.tenants (id),
    last_turn bigint NOT NULL DEFAULT 0,
    credit integer NOT NULL DEFAULT 0 CHECK (credit BETWEEN 0 AND 10)
);
CREATE TABLE eacp.scheduler_team_state (
    tenant_id uuid NOT NULL REFERENCES eacp.tenants (id),
    team_key text NOT NULL,
    last_turn bigint NOT NULL DEFAULT 0,
    credit integer NOT NULL DEFAULT 0 CHECK (credit BETWEEN 0 AND 10),
    PRIMARY KEY (tenant_id, team_key)
);
ALTER TABLE eacp.scheduler_tenant_state ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.scheduler_tenant_state FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.scheduler_tenant_state USING (tenant_id = eacp.current_tenant_id());
CREATE POLICY owner_scan ON eacp.scheduler_tenant_state FOR SELECT TO CURRENT_USER USING (true);
ALTER TABLE eacp.scheduler_team_state ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.scheduler_team_state FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.scheduler_team_state USING (tenant_id = eacp.current_tenant_id());
CREATE POLICY owner_scan ON eacp.scheduler_team_state FOR SELECT TO CURRENT_USER USING (true);

-- Only the T14 action trigger may advance a turn. Application SQL cannot
-- fabricate scheduler history, including while bound as a worker.
-- +goose StatementBegin
CREATE FUNCTION eacp.scheduler_state_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF pg_trigger_depth() <> 2 OR eacp.actor_context()->>'component' IS DISTINCT FROM 'worker' THEN
        RAISE EXCEPTION 'scheduler state changes only on worker claim' USING ERRCODE = '42501';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER scheduler_tenant_guard BEFORE INSERT OR UPDATE ON eacp.scheduler_tenant_state
    FOR EACH ROW EXECUTE FUNCTION eacp.scheduler_state_guard();
CREATE TRIGGER scheduler_team_guard BEFORE INSERT OR UPDATE ON eacp.scheduler_team_state
    FOR EACH ROW EXECUTE FUNCTION eacp.scheduler_state_guard();

-- A release pins ownership, weight and contract priority. Existing queued
-- rows use the direct-agent / weight-one / priority-zero defaults in hints.
-- +goose StatementBegin
CREATE FUNCTION eacp.actions_schedule_before() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    owner_group uuid;
    weight integer;
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF num_nonnulls(NEW.schedule_team_key, NEW.schedule_weight, NEW.schedule_priority) <> 0 THEN
            RAISE EXCEPTION 'scheduling facts are pinned at release' USING ERRCODE = '55000';
        END IF;
        RETURN NEW;
    END IF;
    IF ROW(NEW.schedule_team_key, NEW.schedule_weight, NEW.schedule_priority)
       IS DISTINCT FROM ROW(OLD.schedule_team_key, OLD.schedule_weight, OLD.schedule_priority) THEN
        RAISE EXCEPTION 'scheduling facts cannot be supplied by a caller' USING ERRCODE = '55000';
    END IF;
    IF OLD.state = 'AUTHORIZED' AND NEW.state = 'QUEUED' THEN
        SELECT a.owner_group_id, COALESCE(g.schedule_weight, 1)
          INTO owner_group, weight
          FROM eacp.agents a LEFT JOIN eacp.groups g
            ON g.tenant_id = a.tenant_id AND g.id = a.owner_group_id
         WHERE a.tenant_id = NEW.tenant_id AND a.id = NEW.agent_id;
        NEW.schedule_team_key := CASE WHEN owner_group IS NULL THEN 'agent:' || NEW.agent_id::text
                                      ELSE 'group:' || owner_group::text END;
        NEW.schedule_weight := weight;
        SELECT c.schedule_priority INTO NEW.schedule_priority FROM eacp.tool_contracts c
         WHERE c.tenant_id = NEW.tenant_id AND c.id = NEW.connector_contract_id;
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER actions_schedule_before BEFORE INSERT OR UPDATE ON eacp.actions
    FOR EACH ROW EXECUTE FUNCTION eacp.actions_schedule_before();

-- The action row is already locked. The advisory lock serializes claims for
-- one connector capacity group; the latest committed active count is read
-- after acquiring it. A missing max_inflight is unbounded unless another
-- active contract in the same group specifies a smaller cap.
-- +goose StatementBegin
CREATE FUNCTION eacp.actions_capacity_before() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    candidate_group text;
    candidate_cap integer;
    active_count integer;
    active_cap integer;
    effective_cap integer;
BEGIN
    IF TG_OP = 'UPDATE' AND OLD.state = 'QUEUED' AND NEW.state = 'LEASED' THEN
        SELECT COALESCE('group:' || ct.concurrency_group, 'connector:' || c.id::text), ct.max_inflight
          INTO candidate_group, candidate_cap
          FROM eacp.tool_contracts ct
          JOIN eacp.tools t ON t.tenant_id = ct.tenant_id AND t.id = ct.tool_id
          JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
         WHERE ct.tenant_id = NEW.tenant_id AND ct.id = NEW.connector_contract_id;
        PERFORM pg_advisory_xact_lock(hashtextextended(NEW.tenant_id::text || ':' || candidate_group, 0));
        SELECT count(*)::integer, min(ac.max_inflight) INTO active_count, active_cap
          FROM eacp.actions a
          JOIN eacp.tool_contracts ac ON ac.tenant_id = a.tenant_id AND ac.id = a.connector_contract_id
          JOIN eacp.tools at ON at.tenant_id = ac.tenant_id AND at.id = ac.tool_id
          JOIN eacp.connectors cc ON cc.tenant_id = at.tenant_id AND cc.id = at.connector_id
         WHERE a.tenant_id = NEW.tenant_id AND a.state IN ('LEASED', 'EXECUTING')
           AND COALESCE('group:' || ac.concurrency_group, 'connector:' || cc.id::text) = candidate_group;
        effective_cap := LEAST(candidate_cap, active_cap);
        IF effective_cap IS NULL THEN
            effective_cap := COALESCE(candidate_cap, active_cap);
        END IF;
        IF effective_cap IS NOT NULL AND active_count >= effective_cap THEN
            RAISE EXCEPTION 'connector capacity exhausted' USING ERRCODE = '53300';
        END IF;
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER actions_capacity_before BEFORE UPDATE ON eacp.actions
    FOR EACH ROW EXECUTE FUNCTION eacp.actions_capacity_before();

-- A unit-cost weighted deficit turn: an eligible team keeps its turn for
-- weight claims, then moves behind other eligible teams. Tenant quantum is 1.
-- +goose StatementBegin
CREATE FUNCTION eacp.actions_scheduler_claim() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    team text := COALESCE(NEW.schedule_team_key, 'agent:' || NEW.agent_id::text);
    quantum integer := COALESCE(NEW.schedule_weight, 1);
BEGIN
    IF OLD.state <> 'QUEUED' OR NEW.state <> 'LEASED' THEN
        RETURN NULL;
    END IF;
    INSERT INTO eacp.scheduler_tenant_state (tenant_id, last_turn, credit)
    VALUES (NEW.tenant_id, nextval('eacp.scheduler_turn_seq'), 0)
    ON CONFLICT (tenant_id) DO UPDATE SET
        last_turn = nextval('eacp.scheduler_turn_seq'), credit = 0;
    INSERT INTO eacp.scheduler_team_state (tenant_id, team_key, last_turn, credit)
    VALUES (NEW.tenant_id, team,
            CASE WHEN quantum = 1 THEN nextval('eacp.scheduler_turn_seq') ELSE 0 END, quantum - 1)
    ON CONFLICT (tenant_id, team_key) DO UPDATE SET
        last_turn = CASE WHEN eacp.scheduler_team_state.credit = 1
                             OR (eacp.scheduler_team_state.credit = 0 AND quantum = 1)
                         THEN nextval('eacp.scheduler_turn_seq')
                         ELSE eacp.scheduler_team_state.last_turn END,
        credit = CASE WHEN eacp.scheduler_team_state.credit > 0
                       THEN eacp.scheduler_team_state.credit - 1 ELSE quantum - 1 END;
    RETURN NULL;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER actions_scheduler_claim AFTER UPDATE ON eacp.actions
    FOR EACH ROW EXECUTE FUNCTION eacp.actions_scheduler_claim();

-- Only IDs cross the RLS boundary. Within each tenant and team, effective
-- priority increases every five minutes, capped at nine, then deadline and
-- FIFO order decide. Capacity is a hint here; T14 enforces it under lock.
CREATE OR REPLACE FUNCTION eacp.claimable_actions(p_protocols text[], p_bindings jsonb, lim integer)
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
     ORDER BY COALESCE(st.last_turn, 0), a.tenant_id,
              COALESCE(ss.last_turn, 0), COALESCE(a.schedule_team_key, 'agent:' || a.agent_id::text),
              CASE WHEN a.not_after <= now() + interval '5 minutes' THEN 9
                   ELSE LEAST(9, COALESCE(a.schedule_priority, 0)
                        + floor(extract(epoch FROM (now() - a.state_changed_at)) / 300)::integer)
                   END DESC,
              a.not_after, a.state_changed_at, a.id
     LIMIT least(greatest(lim, 1), 100)
    $$;

-- +goose Down
CREATE OR REPLACE FUNCTION eacp.claimable_actions(p_protocols text[], p_bindings jsonb, lim integer)
    RETURNS TABLE (tenant_id uuid, action_id uuid)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$ SELECT a.tenant_id, a.id FROM eacp.actions a
          JOIN eacp.tools t ON t.tenant_id = a.tenant_id AND t.id = a.tool_id
          JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
          WHERE a.state = 'QUEUED' AND a.not_after > now() AND c.protocol = ANY (p_protocols)
            AND EXISTS (SELECT 1 FROM jsonb_to_recordset(p_bindings) AS b(tenant_id uuid, secret_ref text, host text)
                        WHERE b.tenant_id = a.tenant_id AND b.secret_ref = c.secret_ref
                          AND b.host = lower(substring(c.endpoint FROM '^https?://([^/?#]+)')))
          ORDER BY a.state_changed_at, a.id LIMIT least(greatest(lim, 1), 100) $$;
DROP TRIGGER actions_scheduler_claim ON eacp.actions;
DROP FUNCTION eacp.actions_scheduler_claim();
DROP TRIGGER actions_capacity_before ON eacp.actions;
DROP FUNCTION eacp.actions_capacity_before();
DROP TRIGGER actions_schedule_before ON eacp.actions;
DROP FUNCTION eacp.actions_schedule_before();
DROP TRIGGER scheduler_team_guard ON eacp.scheduler_team_state;
DROP TRIGGER scheduler_tenant_guard ON eacp.scheduler_tenant_state;
DROP FUNCTION eacp.scheduler_state_guard();
DROP TABLE eacp.scheduler_team_state;
DROP TABLE eacp.scheduler_tenant_state;
DROP SEQUENCE eacp.scheduler_turn_seq;
ALTER TABLE eacp.actions DROP COLUMN schedule_team_key, DROP COLUMN schedule_weight, DROP COLUMN schedule_priority;
ALTER TABLE eacp.tool_contracts DROP COLUMN schedule_priority;
ALTER TABLE eacp.groups DROP COLUMN schedule_weight;
