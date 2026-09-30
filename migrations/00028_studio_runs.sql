-- Phase 27a-2 (ADR-033): Studio runs and the runtime's keys. A member of a
-- Studio agent's department starts a run; agent-runtime (a studio_runtime
-- principal) claims it under a lease, submits each tool_call through the
-- action API as the agent with the requester as subject, records each step
-- and finishes the run. The requester reads the answer for an hour. The
-- runtime proposes derived keys, recording each key's master version; an
-- operator revokes every Studio key at once. Inputs and answers never reach
-- the journal.
-- +goose Up

-- ----------------------------------------------------------------- tables

CREATE TABLE eacp.studio_runs (
    tenant_id         uuid        NOT NULL REFERENCES eacp.tenants (id),
    id                uuid        NOT NULL DEFAULT gen_random_uuid(),
    agent_id          uuid        NOT NULL,
    version_id        uuid        NOT NULL,
    requested_by      uuid        NOT NULL,
    inputs            jsonb       CHECK (jsonb_typeof(inputs) = 'object'),
    state             text        NOT NULL DEFAULT 'QUEUED' CHECK (state IN ('QUEUED', 'RUNNING', 'SUCCEEDED', 'FAILED')),
    failure_reason    text        CHECK (failure_reason IN ('credential_pending', 'credential_expired', 'action_denied',
                                        'action_failed', 'action_unknown', 'action_cancelled', 'result_unavailable',
                                        'answer_too_large', 'deadline_exceeded', 'version_replaced',
                                        'credential_revoked')),
    deadline          timestamptz NOT NULL,
    runtime_id        text        CHECK (runtime_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'),
    lease_generation  bigint      NOT NULL DEFAULT 0,
    leased_until      timestamptz,
    answer            text        CHECK (octet_length(answer) <= 65536),
    answer_expires_at timestamptz,
    answer_pruned_at  timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    finished_at       timestamptz,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, agent_id) REFERENCES eacp.studio_agents (tenant_id, id),
    FOREIGN KEY (tenant_id, version_id) REFERENCES eacp.studio_versions (tenant_id, id),
    FOREIGN KEY (tenant_id, requested_by) REFERENCES eacp.principals (tenant_id, id),
    CHECK ((state = 'FAILED') = (failure_reason IS NOT NULL)),
    CHECK (answer IS NULL OR state = 'SUCCEEDED'),
    CHECK ((state IN ('SUCCEEDED', 'FAILED')) = (finished_at IS NOT NULL)),
    CHECK (state IN ('QUEUED', 'RUNNING') OR inputs IS NULL),
    CHECK ((state = 'SUCCEEDED') = (answer_expires_at IS NOT NULL)),
    CHECK (answer_pruned_at IS NULL OR answer IS NULL)
);
CREATE INDEX studio_runs_claimable ON eacp.studio_runs (tenant_id, created_at) WHERE state IN ('QUEUED', 'RUNNING');
CREATE INDEX studio_runs_answers ON eacp.studio_runs (tenant_id, answer_expires_at) WHERE answer IS NOT NULL;

CREATE TABLE eacp.studio_run_steps (
    tenant_id   uuid        NOT NULL,
    id          uuid        NOT NULL DEFAULT gen_random_uuid(),
    run_id      uuid        NOT NULL,
    step_index  integer     NOT NULL CHECK (step_index BETWEEN 0 AND 19),
    step_id     text        NOT NULL,
    action_id   uuid        NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, run_id, step_index),
    FOREIGN KEY (tenant_id, run_id) REFERENCES eacp.studio_runs (tenant_id, id),
    FOREIGN KEY (tenant_id, action_id) REFERENCES eacp.actions (tenant_id, id)
);

-- The master version behind each derived Studio key (ADR-033 §2).
CREATE TABLE eacp.studio_credentials (
    tenant_id      uuid        NOT NULL,
    id             uuid        NOT NULL,
    version_id     uuid        NOT NULL,
    master_version text        NOT NULL CHECK (master_version ~ '^v[0-9]{1,4}$'),
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, id) REFERENCES eacp.credentials (tenant_id, id),
    FOREIGN KEY (tenant_id, version_id) REFERENCES eacp.studio_versions (tenant_id, id)
);

-- +goose StatementBegin
-- eacp.audit_row_change without the columns named in the trigger's
-- arguments: a run's inputs and answer never reach the journal.
CREATE FUNCTION eacp.audit_row_change_redacted() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a       uuid  := eacp.current_actor_id();
    row_new jsonb := to_jsonb(NEW) - 'secret_hash' - TG_ARGV;
    row_old jsonb;
    changed jsonb;
    why     text;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        row_old := to_jsonb(OLD) - 'secret_hash' - TG_ARGV;
        SELECT COALESCE(jsonb_object_agg(e.key, jsonb_build_object('from', row_old -> e.key, 'to', e.value)), '{}')
        INTO changed
        FROM jsonb_each(row_new) AS e
        WHERE row_old -> e.key IS DISTINCT FROM e.value;
        IF changed = '{}'::jsonb THEN
            RETURN NULL;
        END IF;
    ELSE
        changed := row_new;
    END IF;
    why := COALESCE(CASE WHEN changed ? 'failure_reason' THEN row_new ->> 'failure_reason' END,
                    TG_TABLE_NAME || ' ' || lower(TG_OP));
    INSERT INTO eacp.audit_events (tenant_id, payload)
    VALUES (NEW.tenant_id, convert_to(jsonb_build_object(
        'v', 1,
        'actor', jsonb_build_object(
            'kind', CASE WHEN a IS NULL THEN 'system' ELSE 'principal' END,
            'id', COALESCE(a, '00000000-0000-0000-0000-000000000000'::uuid)),
        'action', TG_TABLE_NAME || '.' || lower(TG_OP),
        'subject', jsonb_build_object('type', TG_TABLE_NAME, 'id', NEW.id),
        'reason', why,
        'data', changed)::text, 'UTF8'));
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.studio_immutable() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    RAISE EXCEPTION '% is insert-only', TG_TABLE_NAME USING ERRCODE = '55000';
END
$$;
-- +goose StatementEnd

CREATE TRIGGER studio_run_steps_immutable BEFORE UPDATE ON eacp.studio_run_steps
    FOR EACH ROW EXECUTE FUNCTION eacp.studio_immutable();
CREATE TRIGGER studio_credentials_immutable BEFORE UPDATE ON eacp.studio_credentials
    FOR EACH ROW EXECUTE FUNCTION eacp.studio_immutable();

-- +goose StatementBegin
DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['studio_runs', 'studio_run_steps', 'studio_credentials']
    LOOP
        EXECUTE format('ALTER TABLE eacp.%I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE eacp.%I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('CREATE POLICY tenant_isolation ON eacp.%I USING (tenant_id = eacp.current_tenant_id())', t);
        EXECUTE format('REVOKE ALL ON eacp.%I FROM eacp_app', t);
    END LOOP;
END
$$;
-- +goose StatementEnd

-- The schema owner's read-only scan behind eacp.studio_run_tenants().
CREATE POLICY owner_scan ON eacp.studio_runs FOR SELECT TO CURRENT_USER USING (true);
GRANT SELECT ON eacp.studio_run_steps, eacp.studio_credentials TO eacp_app;
-- Neither the inputs nor the answer are selectable by the application.
GRANT SELECT (tenant_id, id, agent_id, version_id, requested_by, state, failure_reason, deadline, runtime_id,
              lease_generation, leased_until, answer_expires_at, answer_pruned_at, created_at, finished_at)
    ON eacp.studio_runs TO eacp_app;

CREATE TRIGGER zz_audit AFTER INSERT OR UPDATE ON eacp.studio_runs
    FOR EACH ROW EXECUTE FUNCTION eacp.audit_row_change_redacted('inputs', 'answer');
CREATE TRIGGER zz_audit AFTER INSERT OR UPDATE ON eacp.studio_run_steps
    FOR EACH ROW EXECUTE FUNCTION eacp.audit_row_change();
CREATE TRIGGER zz_audit AFTER INSERT OR UPDATE ON eacp.studio_credentials
    FOR EACH ROW EXECUTE FUNCTION eacp.audit_row_change();

-- ----------------------------------------------------------------- helpers

-- +goose StatementBegin
-- The runtime's principal: an enabled principal holding studio_runtime.
CREATE FUNCTION eacp.studio_runtime_actor() RETURNS uuid
    LANGUAGE plpgsql
    AS $$
DECLARE
    who jsonb := eacp.actor_context();
    a   uuid;
BEGIN
    IF who->>'kind' IS DISTINCT FROM 'principal' THEN
        RAISE EXCEPTION 'only the Studio runtime does this' USING ERRCODE = '42501';
    END IF;
    a := (who->>'id')::uuid;
    PERFORM eacp.assert_role(a, 'studio_runtime');
    RETURN a;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Locks run p_run for its lease holder: the runtime principal, runtime id
-- and generation of a live lease. Anything else is refused.
CREATE FUNCTION eacp.studio_run_held(p_run uuid, p_runtime text, p_generation bigint) RETURNS eacp.studio_runs
    LANGUAGE plpgsql
    AS $$
DECLARE
    r eacp.studio_runs%ROWTYPE;
BEGIN
    PERFORM eacp.studio_runtime_actor();
    SELECT * INTO r FROM eacp.studio_runs WHERE tenant_id = eacp.current_tenant_id() AND id = p_run FOR UPDATE;
    IF NOT FOUND OR r.state <> 'RUNNING' OR r.runtime_id IS DISTINCT FROM p_runtime
       OR r.lease_generation <> p_generation OR r.leased_until <= now() THEN
        RAISE EXCEPTION 'run % is not leased to % at generation %', p_run, p_runtime, p_generation
            USING ERRCODE = '42501';
    END IF;
    RETURN r;
END
$$;
-- +goose StatementEnd

-- ------------------------------------------------------------------ runs

-- +goose StatementBegin
-- A member of the agent's department starts a run of its ACTIVE, approved
-- version with exactly the declared inputs.
CREATE FUNCTION eacp.studio_run_start(p_agent uuid, p_inputs jsonb) RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    who    jsonb := eacp.actor_context();
    tenant uuid := eacp.current_tenant_id();
    a      uuid;
    sa     eacp.studio_agents%ROWTYPE;
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
    PERFORM 1 FROM eacp.group_memberships m JOIN eacp.principals p ON p.tenant_id = m.tenant_id AND p.id = m.principal_id
    WHERE m.tenant_id = tenant AND m.group_id = sa.department_group_id AND m.principal_id = a
      AND m.removed_at IS NULL AND p.disabled_at IS NULL AND p.kind = 'human' FOR SHARE OF m;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'only a member of the agent''s department runs it' USING ERRCODE = '42501';
    END IF;
    SELECT v2.* INTO v FROM eacp.agent_versions v2
    JOIN eacp.studio_versions s ON s.tenant_id = v2.tenant_id AND s.id = v2.id
    WHERE v2.tenant_id = tenant AND v2.agent_id = p_agent AND v2.state = 'ACTIVE' AND s.decision = 'approved'
    FOR SHARE OF v2;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'agent % has no approved ACTIVE version', p_agent USING ERRCODE = '55000';
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
CREATE FUNCTION eacp.studio_run_claim(p_runtime text, p_master text, p_lease integer, p_limit integer)
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

-- +goose StatementBegin
-- Records the action of the run's next tool_call step: the run's own
-- version, its requester as subject and the step's idempotency key.
CREATE FUNCTION eacp.studio_run_step(p_run uuid, p_runtime text, p_generation bigint, p_index integer, p_action uuid)
    RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    tenant uuid := eacp.current_tenant_id();
    r      eacp.studio_runs%ROWTYPE;
    step   jsonb;
    act    eacp.actions%ROWTYPE;
    done   integer;
BEGIN
    r := eacp.studio_run_held(p_run, p_runtime, p_generation);
    SELECT definition::jsonb->'steps'->p_index INTO step FROM eacp.studio_versions
    WHERE tenant_id = tenant AND id = r.version_id;
    IF step IS NULL OR step->>'kind' <> 'tool_call' THEN
        RAISE EXCEPTION 'step % of this run is not a tool_call', p_index USING ERRCODE = '23514';
    END IF;
    SELECT count(*) INTO done FROM eacp.studio_run_steps WHERE tenant_id = tenant AND run_id = p_run;
    IF p_index <> done THEN
        RAISE EXCEPTION 'step % is not the run''s next step (%)', p_index, done USING ERRCODE = '55000';
    END IF;
    SELECT * INTO act FROM eacp.actions WHERE tenant_id = tenant AND id = p_action;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'no action %', p_action USING ERRCODE = '23503';
    END IF;
    IF act.agent_version_id <> r.version_id OR act.subject_principal_id IS DISTINCT FROM r.requested_by
       OR act.idempotency_key <> format('studio:%s:%s', p_run, p_index) OR act.tool <> step->>'tool' THEN
        RAISE EXCEPTION 'action % is not this run''s step %', p_action, p_index USING ERRCODE = '42501';
    END IF;
    INSERT INTO eacp.studio_run_steps (tenant_id, run_id, step_index, step_id, action_id)
    VALUES (tenant, p_run, p_index, step->>'id', p_action);
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.studio_run_finish(p_run uuid, p_runtime text, p_generation bigint, p_state text,
                                       p_answer text, p_reason text) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    tenant uuid := eacp.current_tenant_id();
    r      eacp.studio_runs%ROWTYPE;
    calls  integer;
    done   integer;
BEGIN
    r := eacp.studio_run_held(p_run, p_runtime, p_generation);
    IF p_state = 'SUCCEEDED' THEN
        IF p_answer IS NULL OR p_reason IS NOT NULL OR octet_length(p_answer) > 65536 THEN
            RAISE EXCEPTION 'a succeeded run has an answer of at most 65536 bytes and no reason' USING ERRCODE = '23514';
        END IF;
        SELECT count(*) INTO calls FROM eacp.studio_versions s,
            jsonb_array_elements(s.definition::jsonb->'steps') AS e(step)
        WHERE s.tenant_id = tenant AND s.id = r.version_id AND e.step->>'kind' = 'tool_call';
        SELECT count(*) INTO done FROM eacp.studio_run_steps WHERE tenant_id = tenant AND run_id = p_run;
        IF done <> calls THEN
            RAISE EXCEPTION 'a run succeeds only once every step is recorded (% of %)', done, calls USING ERRCODE = '55000';
        END IF;
        UPDATE eacp.studio_runs SET state = 'SUCCEEDED', answer = p_answer, answer_expires_at = now() + interval '1 hour',
               inputs = NULL, leased_until = NULL, finished_at = now()
        WHERE tenant_id = tenant AND id = p_run;
    ELSIF p_state = 'FAILED' THEN
        IF p_answer IS NOT NULL OR p_reason IS NULL THEN
            RAISE EXCEPTION 'a failed run has a reason and no answer' USING ERRCODE = '23514';
        END IF;
        UPDATE eacp.studio_runs SET state = 'FAILED', failure_reason = p_reason, inputs = NULL,
               leased_until = NULL, finished_at = now()
        WHERE tenant_id = tenant AND id = p_run;
    ELSE
        RAISE EXCEPTION 'a run finishes SUCCEEDED or FAILED' USING ERRCODE = '23514';
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The requester reads the answer until it expires; anyone else reads NULL.
CREATE FUNCTION eacp.studio_run_answer(p_run uuid) RETURNS text
    LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    who jsonb := eacp.actor_context();
    out text;
BEGIN
    IF who->>'kind' IS DISTINCT FROM 'principal' THEN
        RETURN NULL;
    END IF;
    SELECT answer INTO out FROM eacp.studio_runs
    WHERE tenant_id = eacp.current_tenant_id() AND id = p_run AND requested_by = (who->>'id')::uuid
      AND state = 'SUCCEEDED' AND answer_expires_at > now();
    RETURN out;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The sweeper fails runs past their deadline that nobody holds, and clears
-- expired answers. Replicas race harmlessly: locked rows are skipped.
CREATE FUNCTION eacp.studio_runs_expire(p_batch integer) RETURNS integer
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    tenant uuid := eacp.current_tenant_id();
    n integer;
    m integer;
BEGIN
    IF eacp.current_system_actor() IS DISTINCT FROM 'sweeper' OR eacp.current_actor_id() IS NOT NULL
       OR eacp.current_agent_version_id() IS NOT NULL THEN
        RAISE EXCEPTION 'only the sweeper expires runs' USING ERRCODE = '42501';
    END IF;
    WITH due AS (
        SELECT id FROM eacp.studio_runs
        WHERE tenant_id = tenant AND deadline <= now()
          AND (state = 'QUEUED' OR (state = 'RUNNING' AND leased_until <= now()))
        ORDER BY deadline LIMIT greatest(p_batch, 1)
        FOR UPDATE SKIP LOCKED)
    UPDATE eacp.studio_runs s SET state = 'FAILED', failure_reason = 'deadline_exceeded', inputs = NULL,
           leased_until = NULL, finished_at = now()
    FROM due WHERE s.tenant_id = tenant AND s.id = due.id;
    GET DIAGNOSTICS n = ROW_COUNT;
    WITH due AS (
        SELECT id FROM eacp.studio_runs
        WHERE tenant_id = tenant AND answer IS NOT NULL AND answer_expires_at <= now()
        ORDER BY answer_expires_at LIMIT greatest(p_batch, 1)
        FOR UPDATE SKIP LOCKED)
    UPDATE eacp.studio_runs s SET answer = NULL, answer_pruned_at = now()
    FROM due WHERE s.tenant_id = tenant AND s.id = due.id;
    GET DIAGNOSTICS m = ROW_COUNT;
    RETURN n + m;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.studio_run_tenants() RETURNS SETOF uuid
    LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT DISTINCT tenant_id FROM eacp.studio_runs
    WHERE (deadline <= now() AND (state = 'QUEUED' OR (state = 'RUNNING' AND leased_until <= now())))
       OR (answer IS NOT NULL AND answer_expires_at <= now())
$$;
-- +goose StatementEnd

-- ------------------------------------------------------------------ keys

-- +goose StatementBegin
-- ACTIVE, approved Studio versions needing a key under master version
-- p_master: no approved, unrevoked key with more than 30 days left, and no
-- pending proposal (ADR-033 §5).
CREATE FUNCTION eacp.studio_credentials_due(p_master text) RETURNS SETOF uuid
    LANGUAGE plpgsql STABLE
    AS $$
BEGIN
    PERFORM eacp.studio_runtime_actor();
    RETURN QUERY
    SELECT v.id FROM eacp.agent_versions v
    JOIN eacp.studio_versions s ON s.tenant_id = v.tenant_id AND s.id = v.id
    WHERE v.tenant_id = eacp.current_tenant_id() AND v.state = 'ACTIVE' AND s.decision = 'approved'
      AND NOT EXISTS (
          SELECT 1 FROM eacp.credentials c
          JOIN eacp.studio_credentials sc ON sc.tenant_id = c.tenant_id AND sc.id = c.id
          WHERE c.tenant_id = v.tenant_id AND c.agent_version_id = v.id AND sc.master_version = p_master
            AND c.revoked_at IS NULL
            AND ((c.approved_at IS NOT NULL AND c.expires_at > now() + interval '30 days')
                 OR (c.approved_at IS NULL AND c.expires_at > now())))
    ORDER BY v.id;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The runtime proposes a derived key (only its hash) for a version that is
-- due, and records its master version. The credentials guard decides.
CREATE FUNCTION eacp.studio_credential_propose(p_version uuid, p_id uuid, p_hash bytea, p_master text)
    RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    tenant uuid := eacp.current_tenant_id();
BEGIN
    PERFORM eacp.studio_runtime_actor();
    IF p_master IS NULL OR p_master !~ '^v[0-9]{1,4}$' THEN
        RAISE EXCEPTION 'a master version is v followed by 1 to 4 digits' USING ERRCODE = '23514';
    END IF;
    -- Serialise proposals for one version.
    PERFORM pg_advisory_xact_lock(hashtextextended('studio_credentials/' || tenant::text || '/' || p_version::text, 0));
    IF NOT EXISTS (SELECT 1 FROM eacp.studio_credentials_due(p_master) AS d(id) WHERE d.id = p_version) THEN
        RAISE EXCEPTION 'no key is due for version % under %', p_version, p_master USING ERRCODE = '55000';
    END IF;
    INSERT INTO eacp.credentials (tenant_id, id, kind, agent_version_id, secret_hash, expires_at)
    VALUES (tenant, p_id, 'ak', p_version, p_hash, now() + interval '90 days');
    INSERT INTO eacp.studio_credentials (tenant_id, id, version_id, master_version)
    VALUES (tenant, p_id, p_version, p_master);
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- An operator revokes every live key of every Studio version, and no other,
-- through the credentials guard (ADR-033 §6). Returns how many.
CREATE FUNCTION eacp.studio_credentials_revoke_all(p_reason text) RETURNS integer
    LANGUAGE plpgsql
    AS $$
DECLARE
    n integer;
BEGIN
    PERFORM eacp.assert_role(eacp.actor(), 'operator');
    PERFORM eacp.require_reason(p_reason, 'revoking every Studio key');
    UPDATE eacp.credentials c SET revoked_at = now(), revoke_reason = p_reason
    WHERE c.tenant_id = eacp.current_tenant_id() AND c.kind = 'ak' AND c.revoked_at IS NULL
      AND EXISTS (SELECT 1 FROM eacp.studio_versions s WHERE s.tenant_id = c.tenant_id AND s.id = c.agent_version_id);
    GET DIAGNOSTICS n = ROW_COUNT;
    RETURN n;
END
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION eacp.studio_run_start(uuid, jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.studio_run_start(uuid, jsonb) TO eacp_app;
REVOKE ALL ON FUNCTION eacp.studio_run_claim(text, text, integer, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.studio_run_claim(text, text, integer, integer) TO eacp_app;
REVOKE ALL ON FUNCTION eacp.studio_run_heartbeat(uuid, text, bigint, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.studio_run_heartbeat(uuid, text, bigint, integer) TO eacp_app;
REVOKE ALL ON FUNCTION eacp.studio_run_step(uuid, text, bigint, integer, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.studio_run_step(uuid, text, bigint, integer, uuid) TO eacp_app;
REVOKE ALL ON FUNCTION eacp.studio_run_finish(uuid, text, bigint, text, text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.studio_run_finish(uuid, text, bigint, text, text, text) TO eacp_app;
REVOKE ALL ON FUNCTION eacp.studio_run_answer(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.studio_run_answer(uuid) TO eacp_app;
REVOKE ALL ON FUNCTION eacp.studio_runs_expire(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.studio_runs_expire(integer) TO eacp_app;
REVOKE ALL ON FUNCTION eacp.studio_run_tenants() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.studio_run_tenants() TO eacp_app;
REVOKE ALL ON FUNCTION eacp.studio_credential_propose(uuid, uuid, bytea, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.studio_credential_propose(uuid, uuid, bytea, text) TO eacp_app;

-- -------------------------------------------------------------- incidents

ALTER TABLE eacp.incidents DROP CONSTRAINT incidents_kind_check;
ALTER TABLE eacp.incidents ADD CONSTRAINT incidents_kind_check CHECK (kind IN ('mcp_drift', 'kill', 'circuit_open',
    'unknown_outcome', 'canary_rollback', 'finops', 'manual', 'studio_credential'));

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.incident_evaluate() RETURNS integer
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

    -- A Studio version whose last approved key expires within 7 days with no
    -- approved successor (ADR-033 §5): runs will fail credential_expired.
    FOR s IN SELECT v.id, ag.name, max(c.expires_at) AS expires_at
               FROM eacp.agent_versions v
               JOIN eacp.studio_versions sv ON sv.tenant_id = v.tenant_id AND sv.id = v.id
               JOIN eacp.agents ag ON ag.tenant_id = v.tenant_id AND ag.id = v.agent_id
               JOIN eacp.credentials c ON c.tenant_id = v.tenant_id AND c.agent_version_id = v.id
                                      AND c.approved_at IS NOT NULL AND c.revoked_at IS NULL
              WHERE v.state = 'ACTIVE' AND sv.decision = 'approved'
              GROUP BY v.id, ag.name
             HAVING max(c.expires_at) <= now() + interval '7 days' ORDER BY v.id LOOP
        key := format('agent_version:%s:%s', s.id, eacp.incident_micros(s.expires_at));
        CONTINUE WHEN EXISTS (SELECT 1 FROM eacp.incidents WHERE kind = 'studio_credential' AND source_key = key);
        aff := eacp.incident_affected(ARRAY['agent_version:' || s.id::text]);
        opened := opened + eacp.incident_open('studio_credential', key, 'high',
            format('Studio agent %s: its key expires at %s with no approved successor', s.name, s.expires_at),
            'agent_version', s.id, jsonb_build_object('agent', s.name, 'expires_at', s.expires_at), aff);
    END LOOP;
    RETURN opened;
END
$$;
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.incident_evaluate() RETURNS integer
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

ALTER TABLE eacp.incidents DROP CONSTRAINT incidents_kind_check;
ALTER TABLE eacp.incidents ADD CONSTRAINT incidents_kind_check CHECK (kind IN ('mcp_drift', 'kill', 'circuit_open',
    'unknown_outcome', 'canary_rollback', 'finops', 'manual'));
DROP FUNCTION eacp.studio_credentials_revoke_all(text);
DROP FUNCTION eacp.studio_credential_propose(uuid, uuid, bytea, text);
DROP FUNCTION eacp.studio_credentials_due(text);
DROP FUNCTION eacp.studio_run_tenants();
DROP FUNCTION eacp.studio_runs_expire(integer);
DROP FUNCTION eacp.studio_run_answer(uuid);
DROP FUNCTION eacp.studio_run_finish(uuid, text, bigint, text, text, text);
DROP FUNCTION eacp.studio_run_step(uuid, text, bigint, integer, uuid);
DROP FUNCTION eacp.studio_run_heartbeat(uuid, text, bigint, integer);
DROP FUNCTION eacp.studio_run_claim(text, text, integer, integer);
DROP FUNCTION eacp.studio_run_start(uuid, jsonb);
DROP FUNCTION eacp.studio_run_held(uuid, text, bigint);
DROP TABLE eacp.studio_credentials;
DROP TABLE eacp.studio_run_steps;
DROP TABLE eacp.studio_runs;
DROP FUNCTION eacp.studio_runtime_actor();
DROP FUNCTION eacp.studio_immutable();
DROP FUNCTION eacp.audit_row_change_redacted();
