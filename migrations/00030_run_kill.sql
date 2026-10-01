-- Phase 28 (ADR-016 Rev 1.1): the run kill scope. PostgreSQL binds every
-- action of a Studio version to the running run its idempotency key names,
-- when the action is inserted, and never changes it; a run kill matches that
-- binding at T14, T16, the claim filter and the worker's checks. A kill that
-- matches a run (tenant, team, agent, agent version or run) stops new runs,
-- and PostgreSQL fails a due or held run killed at its claim or heartbeat.
-- +goose Up

-- ------------------------------------------------------------ the binding

ALTER TABLE eacp.actions ADD COLUMN studio_run_id uuid;
ALTER TABLE eacp.actions ADD CONSTRAINT actions_studio_run_fkey
    FOREIGN KEY (tenant_id, studio_run_id) REFERENCES eacp.studio_runs (tenant_id, id);
CREATE INDEX actions_studio_run ON eacp.actions (tenant_id, studio_run_id) WHERE studio_run_id IS NOT NULL;

-- +goose StatementBegin
-- An action of a Studio version is a step of its own running run: the key
-- studio:<run>:<index> names a RUNNING run of the same version, requested by
-- the action's subject, whose step <index> is a tool_call of the action's
-- tool. Anything else is refused, so a Studio key never acts outside a run.
-- Every other action names no run. The binding never changes.
CREATE FUNCTION eacp.actions_studio_run() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    m      text[];
    run    uuid;
    state  text;
    ver    uuid;
    req    uuid;
    step   jsonb;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF NEW.studio_run_id IS DISTINCT FROM OLD.studio_run_id THEN
            RAISE EXCEPTION 'an action''s run never changes' USING ERRCODE = '55000';
        END IF;
        RETURN NEW;
    END IF;
    NEW.studio_run_id := NULL;
    IF NOT EXISTS (SELECT 1 FROM eacp.studio_versions WHERE tenant_id = NEW.tenant_id AND id = NEW.agent_version_id) THEN
        RETURN NEW;
    END IF;
    m := regexp_match(NEW.idempotency_key,
        '^studio:([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}):(0|[1-9][0-9]?)$');
    IF m IS NOT NULL THEN
        SELECT r.id, r.state, r.version_id, r.requested_by INTO run, state, ver, req
        FROM eacp.studio_runs r WHERE r.tenant_id = NEW.tenant_id AND r.id = m[1]::uuid;
        SELECT s.definition::jsonb->'steps'->(m[2]::integer) INTO step
        FROM eacp.studio_versions s WHERE s.tenant_id = NEW.tenant_id AND s.id = ver;
    END IF;
    IF run IS NULL OR state <> 'RUNNING' OR ver <> NEW.agent_version_id
       OR req IS DISTINCT FROM NEW.subject_principal_id OR step IS NULL
       OR step->>'kind' IS DISTINCT FROM 'tool_call' OR step->>'tool' IS DISTINCT FROM NEW.tool THEN
        RAISE EXCEPTION 'a Studio agent acts only as a step of its own running run' USING ERRCODE = '42501';
    END IF;
    NEW.studio_run_id := run;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- After actions_guard, which resolves the subject.
CREATE TRIGGER actions_studio_run BEFORE INSERT OR UPDATE OF studio_run_id ON eacp.actions
    FOR EACH ROW EXECUTE FUNCTION eacp.actions_studio_run();

-- -------------------------------------------------------------- the scope

ALTER TABLE eacp.kill_states DROP CONSTRAINT kill_states_scope_check;
ALTER TABLE eacp.kill_states ADD CONSTRAINT kill_states_scope_check
    CHECK (scope IN ('tenant', 'team', 'agent', 'agent_version', 'action', 'connector', 'tool', 'model', 'run'));

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.set_kill(p_scope text, p_target uuid, p_killed boolean, p_reason text,
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
    WHEN 'model' THEN SELECT EXISTS(SELECT 1 FROM eacp.llm_models WHERE tenant_id = tenant AND id = p_target) INTO valid_target;
    WHEN 'run' THEN SELECT EXISTS(SELECT 1 FROM eacp.studio_runs WHERE tenant_id = tenant AND id = p_target) INTO valid_target;
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

-- +goose StatementBegin
-- A scope with no stable action binding cannot silently match nothing. A
-- run matches the Studio run PostgreSQL bound the action to (Phase 28), and
-- an action of a run that failed killed stays stopped after any resume.
CREATE OR REPLACE FUNCTION eacp.action_killed(a eacp.actions) RETURNS boolean LANGUAGE sql STABLE AS $$
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
            (k.scope = 'tool' AND k.target_id = a.tool_id) OR
            (k.scope = 'run' AND k.target_id = a.studio_run_id)))
    OR (a.studio_run_id IS NOT NULL AND EXISTS (
        SELECT 1 FROM eacp.studio_runs r
        WHERE r.tenant_id = a.tenant_id AND r.id = a.studio_run_id AND r.failure_reason = 'killed')) $$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The graph nodes a kill scope, a connector or an agent covers.
CREATE OR REPLACE FUNCTION eacp.incident_scope_nodes(p_scope text, p_id uuid) RETURNS text[]
    LANGUAGE sql STABLE
    AS $$
    SELECT CASE p_scope
        WHEN 'agent_version' THEN ARRAY['agent_version:' || p_id::text]
        WHEN 'tool' THEN ARRAY['tool:' || p_id::text]
        WHEN 'action' THEN ARRAY(SELECT 'agent_version:' || a.agent_version_id::text FROM eacp.actions a
                                  WHERE a.id = p_id)
        WHEN 'team' THEN ARRAY(SELECT 'agent_version:' || v.id::text FROM eacp.agent_versions v
                                 JOIN eacp.agents ag ON ag.tenant_id = v.tenant_id AND ag.id = v.agent_id
                                WHERE ag.owner_group_id = p_id ORDER BY 1)
        WHEN 'agent' THEN ARRAY(SELECT 'agent_version:' || v.id::text FROM eacp.agent_versions v
                                 WHERE v.agent_id = p_id ORDER BY v.version)
        WHEN 'connector' THEN ARRAY(SELECT n FROM (
                                  SELECT 'mcp:' || c.id::text AS n FROM eacp.connectors c
                                   WHERE c.id = p_id AND c.protocol = 'mcp'
                                  UNION ALL
                                  SELECT 'tool:' || t.id::text FROM eacp.tools t WHERE t.connector_id = p_id) x
                                ORDER BY n)
        WHEN 'run' THEN ARRAY(SELECT 'agent_version:' || r.version_id::text FROM eacp.studio_runs r
                               WHERE r.id = p_id)
        ELSE ARRAY[]::text[]
    END
$$;
-- +goose StatementEnd

-- ------------------------------------------------------------------ runs

ALTER TABLE eacp.studio_runs DROP CONSTRAINT studio_runs_failure_reason_check;
ALTER TABLE eacp.studio_runs ADD CONSTRAINT studio_runs_failure_reason_check
    CHECK (failure_reason IN ('credential_pending', 'credential_expired', 'action_denied', 'action_failed',
                              'action_unknown', 'action_cancelled', 'result_unavailable', 'answer_too_large',
                              'deadline_exceeded', 'version_replaced', 'credential_revoked', 'killed'));

-- +goose StatementBegin
-- Whether a kill stops a run of agent p_agent at version p_version (p_run is
-- NULL before the run exists). tool and connector kills stop its actions,
-- not the run. A team is the agent's owner group, as for actions.
CREATE FUNCTION eacp.studio_run_killed(p_tenant uuid, p_agent uuid, p_version uuid, p_run uuid) RETURNS boolean
    LANGUAGE sql STABLE
    AS $$
    SELECT EXISTS (
        SELECT 1 FROM eacp.kill_states k
        LEFT JOIN eacp.agents ag ON ag.tenant_id = p_tenant AND ag.id = p_agent
        WHERE k.tenant_id = p_tenant AND k.killed AND (
            (k.scope = 'tenant' AND k.target_id = p_tenant) OR
            (k.scope = 'team' AND k.target_id = ag.owner_group_id) OR
            (k.scope = 'agent' AND k.target_id = p_agent) OR
            (k.scope = 'agent_version' AND k.target_id = p_version) OR
            (k.scope = 'run' AND k.target_id = p_run)))
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The agent's owner, while a member of its department, or anyone its Hub
-- listing reaches starts a run of its ACTIVE, approved version with exactly
-- the declared inputs. A listing runs only the version it publishes. A kill
-- that matches the agent stops new runs (Phase 28).
CREATE OR REPLACE FUNCTION eacp.studio_run_start(p_agent uuid, p_inputs jsonb) RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    who    jsonb := eacp.actor_context();
    tenant uuid := eacp.current_tenant_id();
    a      uuid;
    sa     eacp.studio_agents%ROWTYPE;
    l      eacp.studio_listings%ROWTYPE;
    listed uuid;
    v      eacp.agent_versions%ROWTYPE;
    def    jsonb;
    k      text;
    spec   jsonb;
    run    uuid;
BEGIN
    IF who->>'kind' IS DISTINCT FROM 'principal' THEN
        RAISE EXCEPTION 'a run is started by a principal' USING ERRCODE = '42501';
    END IF;
    a := (who->>'id')::uuid;
    SELECT * INTO sa FROM eacp.studio_agents WHERE tenant_id = tenant AND id = p_agent;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'no Studio agent %', p_agent USING ERRCODE = '23503';
    END IF;
    PERFORM 1 FROM eacp.group_memberships m
    JOIN eacp.principals p ON p.tenant_id = m.tenant_id AND p.id = m.principal_id
    JOIN eacp.agents g ON g.tenant_id = m.tenant_id AND g.id = p_agent AND g.owner_principal_id = a
    WHERE m.tenant_id = tenant AND m.group_id = sa.department_group_id AND m.principal_id = a
      AND m.removed_at IS NULL AND p.disabled_at IS NULL AND p.kind = 'human' FOR SHARE OF m;
    IF NOT FOUND THEN
        SELECT * INTO l FROM eacp.studio_listings WHERE tenant_id = tenant AND agent_id = p_agent FOR SHARE;
        IF NOT FOUND OR NOT eacp.studio_listing_visible(a, l.scope, l.state, sa.department_group_id) THEN
            RAISE EXCEPTION 'only the agent''s owner, or someone its Hub listing reaches, runs it' USING ERRCODE = '42501';
        END IF;
        listed := l.published_version_id;
    END IF;
    SELECT v2.* INTO v FROM eacp.agent_versions v2
    JOIN eacp.studio_versions s ON s.tenant_id = v2.tenant_id AND s.id = v2.id
    WHERE v2.tenant_id = tenant AND v2.agent_id = p_agent AND v2.state = 'ACTIVE' AND s.decision = 'approved'
    FOR SHARE OF v2;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'agent % has no approved ACTIVE version', p_agent USING ERRCODE = '55000';
    END IF;
    IF listed IS NOT NULL AND listed <> v.id THEN
        RAISE EXCEPTION 'the Hub listing publishes version %, which is no longer ACTIVE', listed USING ERRCODE = '55000';
    END IF;
    IF eacp.studio_run_killed(tenant, p_agent, v.id, NULL) THEN
        RAISE EXCEPTION 'a kill switch stops agent %', p_agent USING ERRCODE = '55000';
    END IF;
    SELECT definition::jsonb INTO def FROM eacp.studio_versions WHERE tenant_id = tenant AND id = v.id;

    IF jsonb_typeof(p_inputs) IS DISTINCT FROM 'object' THEN
        RAISE EXCEPTION 'inputs must be an object' USING ERRCODE = '23514';
    END IF;
    SELECT key INTO k FROM jsonb_object_keys(p_inputs) AS o(key)
    WHERE NOT COALESCE(def->'inputs', '{}') ? key LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'input % is not declared', k USING ERRCODE = '23514';
    END IF;
    FOR k, spec IN SELECT key, value FROM jsonb_each(COALESCE(def->'inputs', '{}')) LOOP
        IF jsonb_typeof(p_inputs->k) IS DISTINCT FROM 'string' THEN
            RAISE EXCEPTION 'input % is a required string', k USING ERRCODE = '23514';
        END IF;
        IF length(p_inputs->>k) > (spec->>'max_length')::integer THEN
            RAISE EXCEPTION 'input % is longer than %', k, spec->>'max_length' USING ERRCODE = '23514';
        END IF;
    END LOOP;

    INSERT INTO eacp.studio_runs (tenant_id, agent_id, version_id, requested_by, inputs, deadline)
    VALUES (tenant, p_agent, v.id, a, p_inputs,
            now() + make_interval(secs => (def->'limits'->>'timeout_seconds')::integer))
    RETURNING id INTO run;
    RETURN run;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The runtime leases up to p_limit runs: QUEUED ones, and RUNNING ones whose
-- lease lapsed, before their deadline. Each comes with what the runtime
-- needs to execute it and the key PostgreSQL chose for its master version.
-- A due run a kill matches is failed killed instead (Phase 28).
CREATE OR REPLACE FUNCTION eacp.studio_run_claim(p_runtime text, p_master text, p_lease integer, p_limit integer)
    RETURNS SETOF jsonb
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    tenant uuid := eacp.current_tenant_id();
    r      eacp.studio_runs%ROWTYPE;
    cred   uuid;
    status text;
BEGIN
    PERFORM eacp.studio_runtime_actor();
    IF p_runtime !~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$' OR p_lease NOT BETWEEN 5 AND 300
       OR p_limit NOT BETWEEN 1 AND 100 OR p_master !~ '^v[0-9]{1,4}$' THEN
        RAISE EXCEPTION 'a claim names a runtime id, a master version, a lease of 5 to 300 s and 1 to 100 runs'
            USING ERRCODE = '23514';
    END IF;
    WITH killed AS (
        SELECT id FROM eacp.studio_runs
        WHERE tenant_id = tenant AND deadline > now()
          AND (state = 'QUEUED' OR (state = 'RUNNING' AND leased_until <= now()))
          AND eacp.studio_run_killed(tenant, agent_id, version_id, id)
        ORDER BY created_at, id LIMIT 100
        FOR UPDATE SKIP LOCKED)
    UPDATE eacp.studio_runs s SET state = 'FAILED', failure_reason = 'killed', inputs = NULL,
           leased_until = NULL, finished_at = now()
    FROM killed WHERE s.tenant_id = tenant AND s.id = killed.id;
    FOR r IN
        WITH due AS (
            SELECT id FROM eacp.studio_runs
            WHERE tenant_id = tenant AND deadline > now()
              AND (state = 'QUEUED' OR (state = 'RUNNING' AND leased_until <= now()))
              AND NOT eacp.studio_run_killed(tenant, agent_id, version_id, id)
            ORDER BY created_at, id LIMIT p_limit
            FOR UPDATE SKIP LOCKED)
        UPDATE eacp.studio_runs s SET state = 'RUNNING', runtime_id = p_runtime,
               lease_generation = s.lease_generation + 1, leased_until = now() + make_interval(secs => p_lease)
        FROM due WHERE s.tenant_id = tenant AND s.id = due.id
        RETURNING s.*
    LOOP
        SELECT c.id INTO cred FROM eacp.credentials c
        JOIN eacp.studio_credentials sc ON sc.tenant_id = c.tenant_id AND sc.id = c.id
        WHERE c.tenant_id = tenant AND c.agent_version_id = r.version_id AND sc.master_version = p_master
          AND c.approved_at IS NOT NULL AND c.revoked_at IS NULL AND c.expires_at > now()
        ORDER BY c.expires_at DESC LIMIT 1;
        status := CASE
            WHEN cred IS NOT NULL THEN 'ok'
            WHEN EXISTS (SELECT 1 FROM eacp.credentials c
                         JOIN eacp.studio_credentials sc ON sc.tenant_id = c.tenant_id AND sc.id = c.id
                         WHERE c.tenant_id = tenant AND c.agent_version_id = r.version_id AND sc.master_version = p_master
                           AND c.approved_at IS NOT NULL AND c.revoked_at IS NULL) THEN 'expired'
            ELSE 'pending' END;
        RETURN NEXT jsonb_build_object(
            'id', r.id, 'agent_id', r.agent_id, 'version_id', r.version_id, 'generation', r.lease_generation,
            'deadline', r.deadline, 'inputs', r.inputs,
            'definition', (SELECT definition::jsonb FROM eacp.studio_versions WHERE tenant_id = tenant AND id = r.version_id),
            'subject', (SELECT subject FROM eacp.principals WHERE tenant_id = tenant AND id = r.requested_by),
            'steps', COALESCE((SELECT jsonb_agg(jsonb_build_object('index', step_index, 'action_id', action_id)
                                                ORDER BY step_index)
                               FROM eacp.studio_run_steps WHERE tenant_id = tenant AND run_id = r.id), '[]'),
            'credential_id', cred, 'credential', status);
    END LOOP;
END
$$;
-- +goose StatementEnd

DROP FUNCTION eacp.studio_run_heartbeat(uuid, text, bigint, integer);

-- +goose StatementBegin
-- The lease holder extends its lease. It returns active, or replaced when the
-- run's version is no longer ACTIVE (the runtime sends nothing more and fails
-- the run version_replaced), or killed when a kill matches the run: then
-- PostgreSQL has failed it killed and the runtime holds nothing (Phase 28).
CREATE FUNCTION eacp.studio_run_heartbeat(p_run uuid, p_runtime text, p_generation bigint, p_lease integer)
    RETURNS text
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    r eacp.studio_runs%ROWTYPE;
BEGIN
    IF p_lease NOT BETWEEN 5 AND 300 THEN
        RAISE EXCEPTION 'a lease is 5 to 300 s' USING ERRCODE = '23514';
    END IF;
    r := eacp.studio_run_held(p_run, p_runtime, p_generation);
    IF eacp.studio_run_killed(r.tenant_id, r.agent_id, r.version_id, r.id) THEN
        UPDATE eacp.studio_runs SET state = 'FAILED', failure_reason = 'killed', inputs = NULL,
               leased_until = NULL, finished_at = now()
        WHERE tenant_id = r.tenant_id AND id = p_run;
        RETURN 'killed';
    END IF;
    UPDATE eacp.studio_runs SET leased_until = now() + make_interval(secs => p_lease)
    WHERE tenant_id = r.tenant_id AND id = p_run;
    RETURN CASE WHEN EXISTS (SELECT 1 FROM eacp.agent_versions
                             WHERE tenant_id = r.tenant_id AND id = r.version_id AND state = 'ACTIVE')
                THEN 'active' ELSE 'replaced' END;
END
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION eacp.studio_run_heartbeat(uuid, text, bigint, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.studio_run_heartbeat(uuid, text, bigint, integer) TO eacp_app;

-- +goose Down

DROP FUNCTION eacp.studio_run_heartbeat(uuid, text, bigint, integer);

-- +goose StatementBegin
-- The lease holder extends its lease. It returns whether the run's version
-- is still ACTIVE: when it is not, the runtime sends nothing more and fails
-- the run version_replaced.
CREATE FUNCTION eacp.studio_run_heartbeat(p_run uuid, p_runtime text, p_generation bigint, p_lease integer)
    RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    r eacp.studio_runs%ROWTYPE;
BEGIN
    IF p_lease NOT BETWEEN 5 AND 300 THEN
        RAISE EXCEPTION 'a lease is 5 to 300 s' USING ERRCODE = '23514';
    END IF;
    r := eacp.studio_run_held(p_run, p_runtime, p_generation);
    UPDATE eacp.studio_runs SET leased_until = now() + make_interval(secs => p_lease)
    WHERE tenant_id = eacp.current_tenant_id() AND id = p_run;
    RETURN EXISTS (SELECT 1 FROM eacp.agent_versions
                   WHERE tenant_id = r.tenant_id AND id = r.version_id AND state = 'ACTIVE');
END
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION eacp.studio_run_heartbeat(uuid, text, bigint, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.studio_run_heartbeat(uuid, text, bigint, integer) TO eacp_app;

-- +goose StatementBegin
-- The runtime leases up to p_limit runs: QUEUED ones, and RUNNING ones whose
-- lease lapsed, before their deadline. Each comes with what the runtime
-- needs to execute it and the key PostgreSQL chose for its master version.
CREATE OR REPLACE FUNCTION eacp.studio_run_claim(p_runtime text, p_master text, p_lease integer, p_limit integer)
    RETURNS SETOF jsonb
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    tenant uuid := eacp.current_tenant_id();
    r      eacp.studio_runs%ROWTYPE;
    cred   uuid;
    status text;
BEGIN
    PERFORM eacp.studio_runtime_actor();
    IF p_runtime !~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$' OR p_lease NOT BETWEEN 5 AND 300
       OR p_limit NOT BETWEEN 1 AND 100 OR p_master !~ '^v[0-9]{1,4}$' THEN
        RAISE EXCEPTION 'a claim names a runtime id, a master version, a lease of 5 to 300 s and 1 to 100 runs'
            USING ERRCODE = '23514';
    END IF;
    FOR r IN
        WITH due AS (
            SELECT id FROM eacp.studio_runs
            WHERE tenant_id = tenant AND deadline > now()
              AND (state = 'QUEUED' OR (state = 'RUNNING' AND leased_until <= now()))
            ORDER BY created_at, id LIMIT p_limit
            FOR UPDATE SKIP LOCKED)
        UPDATE eacp.studio_runs s SET state = 'RUNNING', runtime_id = p_runtime,
               lease_generation = s.lease_generation + 1, leased_until = now() + make_interval(secs => p_lease)
        FROM due WHERE s.tenant_id = tenant AND s.id = due.id
        RETURNING s.*
    LOOP
        SELECT c.id INTO cred FROM eacp.credentials c
        JOIN eacp.studio_credentials sc ON sc.tenant_id = c.tenant_id AND sc.id = c.id
        WHERE c.tenant_id = tenant AND c.agent_version_id = r.version_id AND sc.master_version = p_master
          AND c.approved_at IS NOT NULL AND c.revoked_at IS NULL AND c.expires_at > now()
        ORDER BY c.expires_at DESC LIMIT 1;
        status := CASE
            WHEN cred IS NOT NULL THEN 'ok'
            WHEN EXISTS (SELECT 1 FROM eacp.credentials c
                         JOIN eacp.studio_credentials sc ON sc.tenant_id = c.tenant_id AND sc.id = c.id
                         WHERE c.tenant_id = tenant AND c.agent_version_id = r.version_id AND sc.master_version = p_master
                           AND c.approved_at IS NOT NULL AND c.revoked_at IS NULL) THEN 'expired'
            ELSE 'pending' END;
        RETURN NEXT jsonb_build_object(
            'id', r.id, 'agent_id', r.agent_id, 'version_id', r.version_id, 'generation', r.lease_generation,
            'deadline', r.deadline, 'inputs', r.inputs,
            'definition', (SELECT definition::jsonb FROM eacp.studio_versions WHERE tenant_id = tenant AND id = r.version_id),
            'subject', (SELECT subject FROM eacp.principals WHERE tenant_id = tenant AND id = r.requested_by),
            'steps', COALESCE((SELECT jsonb_agg(jsonb_build_object('index', step_index, 'action_id', action_id)
                                                ORDER BY step_index)
                               FROM eacp.studio_run_steps WHERE tenant_id = tenant AND run_id = r.id), '[]'),
            'credential_id', cred, 'credential', status);
    END LOOP;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The agent's owner, while a member of its department, or anyone its Hub
-- listing reaches starts a run of its ACTIVE, approved version with exactly
-- the declared inputs. A listing runs only the version it publishes.
CREATE OR REPLACE FUNCTION eacp.studio_run_start(p_agent uuid, p_inputs jsonb) RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    who    jsonb := eacp.actor_context();
    tenant uuid := eacp.current_tenant_id();
    a      uuid;
    sa     eacp.studio_agents%ROWTYPE;
    l      eacp.studio_listings%ROWTYPE;
    listed uuid;
    v      eacp.agent_versions%ROWTYPE;
    def    jsonb;
    k      text;
    spec   jsonb;
    run    uuid;
BEGIN
    IF who->>'kind' IS DISTINCT FROM 'principal' THEN
        RAISE EXCEPTION 'a run is started by a principal' USING ERRCODE = '42501';
    END IF;
    a := (who->>'id')::uuid;
    SELECT * INTO sa FROM eacp.studio_agents WHERE tenant_id = tenant AND id = p_agent;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'no Studio agent %', p_agent USING ERRCODE = '23503';
    END IF;
    PERFORM 1 FROM eacp.group_memberships m
    JOIN eacp.principals p ON p.tenant_id = m.tenant_id AND p.id = m.principal_id
    JOIN eacp.agents g ON g.tenant_id = m.tenant_id AND g.id = p_agent AND g.owner_principal_id = a
    WHERE m.tenant_id = tenant AND m.group_id = sa.department_group_id AND m.principal_id = a
      AND m.removed_at IS NULL AND p.disabled_at IS NULL AND p.kind = 'human' FOR SHARE OF m;
    IF NOT FOUND THEN
        SELECT * INTO l FROM eacp.studio_listings WHERE tenant_id = tenant AND agent_id = p_agent FOR SHARE;
        IF NOT FOUND OR NOT eacp.studio_listing_visible(a, l.scope, l.state, sa.department_group_id) THEN
            RAISE EXCEPTION 'only the agent''s owner, or someone its Hub listing reaches, runs it' USING ERRCODE = '42501';
        END IF;
        listed := l.published_version_id;
    END IF;
    SELECT v2.* INTO v FROM eacp.agent_versions v2
    JOIN eacp.studio_versions s ON s.tenant_id = v2.tenant_id AND s.id = v2.id
    WHERE v2.tenant_id = tenant AND v2.agent_id = p_agent AND v2.state = 'ACTIVE' AND s.decision = 'approved'
    FOR SHARE OF v2;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'agent % has no approved ACTIVE version', p_agent USING ERRCODE = '55000';
    END IF;
    IF listed IS NOT NULL AND listed <> v.id THEN
        RAISE EXCEPTION 'the Hub listing publishes version %, which is no longer ACTIVE', listed USING ERRCODE = '55000';
    END IF;
    SELECT definition::jsonb INTO def FROM eacp.studio_versions WHERE tenant_id = tenant AND id = v.id;

    IF jsonb_typeof(p_inputs) IS DISTINCT FROM 'object' THEN
        RAISE EXCEPTION 'inputs must be an object' USING ERRCODE = '23514';
    END IF;
    SELECT key INTO k FROM jsonb_object_keys(p_inputs) AS o(key)
    WHERE NOT COALESCE(def->'inputs', '{}') ? key LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'input % is not declared', k USING ERRCODE = '23514';
    END IF;
    FOR k, spec IN SELECT key, value FROM jsonb_each(COALESCE(def->'inputs', '{}')) LOOP
        IF jsonb_typeof(p_inputs->k) IS DISTINCT FROM 'string' THEN
            RAISE EXCEPTION 'input % is a required string', k USING ERRCODE = '23514';
        END IF;
        IF length(p_inputs->>k) > (spec->>'max_length')::integer THEN
            RAISE EXCEPTION 'input % is longer than %', k, spec->>'max_length' USING ERRCODE = '23514';
        END IF;
    END LOOP;

    INSERT INTO eacp.studio_runs (tenant_id, agent_id, version_id, requested_by, inputs, deadline)
    VALUES (tenant, p_agent, v.id, a, p_inputs,
            now() + make_interval(secs => (def->'limits'->>'timeout_seconds')::integer))
    RETURNING id INTO run;
    RETURN run;
END
$$;
-- +goose StatementEnd

DROP FUNCTION eacp.studio_run_killed(uuid, uuid, uuid, uuid);

ALTER TABLE eacp.studio_runs DROP CONSTRAINT studio_runs_failure_reason_check;
ALTER TABLE eacp.studio_runs ADD CONSTRAINT studio_runs_failure_reason_check
    CHECK (failure_reason IN ('credential_pending', 'credential_expired', 'action_denied', 'action_failed',
                              'action_unknown', 'action_cancelled', 'result_unavailable', 'answer_too_large',
                              'deadline_exceeded', 'version_replaced', 'credential_revoked'));

-- +goose StatementBegin
-- The graph nodes a kill scope, a connector or an agent covers.
CREATE OR REPLACE FUNCTION eacp.incident_scope_nodes(p_scope text, p_id uuid) RETURNS text[]
    LANGUAGE sql STABLE
    AS $$
    SELECT CASE p_scope
        WHEN 'agent_version' THEN ARRAY['agent_version:' || p_id::text]
        WHEN 'tool' THEN ARRAY['tool:' || p_id::text]
        WHEN 'action' THEN ARRAY(SELECT 'agent_version:' || a.agent_version_id::text FROM eacp.actions a
                                  WHERE a.id = p_id)
        WHEN 'team' THEN ARRAY(SELECT 'agent_version:' || v.id::text FROM eacp.agent_versions v
                                 JOIN eacp.agents ag ON ag.tenant_id = v.tenant_id AND ag.id = v.agent_id
                                WHERE ag.owner_group_id = p_id ORDER BY 1)
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
-- A scope with no stable action binding cannot silently match nothing.
CREATE OR REPLACE FUNCTION eacp.action_killed(a eacp.actions) RETURNS boolean LANGUAGE sql STABLE AS $$
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

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.set_kill(p_scope text, p_target uuid, p_killed boolean, p_reason text,
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
    WHEN 'model' THEN SELECT EXISTS(SELECT 1 FROM eacp.llm_models WHERE tenant_id = tenant AND id = p_target) INTO valid_target;
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

ALTER TABLE eacp.kill_states DROP CONSTRAINT kill_states_scope_check;
ALTER TABLE eacp.kill_states ADD CONSTRAINT kill_states_scope_check
    CHECK (scope IN ('tenant', 'team', 'agent', 'agent_version', 'action', 'connector', 'tool', 'model'));

DROP TRIGGER actions_studio_run ON eacp.actions;
DROP FUNCTION eacp.actions_studio_run();
DROP INDEX eacp.actions_studio_run;
ALTER TABLE eacp.actions DROP COLUMN studio_run_id;
