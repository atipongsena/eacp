-- Phase 27c: fenced graph progress, one-use intents and tool-free previews.
-- +goose Up

ALTER TABLE eacp.studio_runs ADD COLUMN mode text NOT NULL DEFAULT 'run' CHECK(mode IN ('run','preview'));
ALTER TABLE eacp.studio_runs ADD COLUMN current_index integer NOT NULL DEFAULT 0 CHECK(current_index BETWEEN 0 AND 19);
ALTER TABLE eacp.studio_runs ADD COLUMN preview_samples jsonb;
GRANT SELECT(mode,current_index) ON eacp.studio_runs TO eacp_app;
ALTER TABLE eacp.studio_runs DROP CONSTRAINT studio_runs_failure_reason_check;
ALTER TABLE eacp.studio_runs ADD CONSTRAINT studio_runs_failure_reason_check CHECK(failure_reason IN
 ('credential_pending','credential_expired','action_denied','action_failed','action_unknown','action_cancelled',
  'result_unavailable','answer_too_large','deadline_exceeded','version_replaced','credential_revoked','killed',
  'branch_invalid','llm_denied','llm_failed','llm_unknown','llm_invalid_output','llm_output_unavailable',
  'llm_output_contains_credential','budget_pending'));

CREATE TABLE eacp.studio_run_nodes(
 tenant_id uuid NOT NULL, id uuid NOT NULL DEFAULT gen_random_uuid(), run_id uuid NOT NULL,
 step_index integer NOT NULL CHECK(step_index BETWEEN 0 AND 19), step_id text NOT NULL,
 kind text NOT NULL CHECK(kind IN ('tool_call','llm','branch','respond')),
 state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','completed')),
 next_index integer CHECK(next_index BETWEEN 0 AND 19),
 action_id uuid, call_id uuid, runtime_id text NOT NULL, lease_generation bigint NOT NULL,
 failure_reason text, created_at timestamptz NOT NULL DEFAULT now(), completed_at timestamptz,
 PRIMARY KEY(tenant_id,id), UNIQUE(tenant_id,run_id,step_index), UNIQUE(tenant_id,call_id),
 FOREIGN KEY(tenant_id,run_id) REFERENCES eacp.studio_runs(tenant_id,id),
 FOREIGN KEY(tenant_id,action_id) REFERENCES eacp.actions(tenant_id,id),
 FOREIGN KEY(tenant_id,call_id) REFERENCES eacp.llm_calls(tenant_id,id),
 CHECK((state='completed')=(completed_at IS NOT NULL))
);
ALTER TABLE eacp.studio_run_nodes ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.studio_run_nodes FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.studio_run_nodes USING(tenant_id=eacp.current_tenant_id());
REVOKE ALL ON eacp.studio_run_nodes FROM eacp_app;
GRANT SELECT ON eacp.studio_run_nodes TO eacp_app;
CREATE TRIGGER zz_audit AFTER INSERT OR UPDATE ON eacp.studio_run_nodes FOR EACH ROW EXECUTE FUNCTION eacp.audit_row_change();
DROP TRIGGER zz_audit ON eacp.studio_runs;
CREATE TRIGGER zz_audit AFTER INSERT OR UPDATE ON eacp.studio_runs
 FOR EACH ROW EXECUTE FUNCTION eacp.audit_row_change_redacted('inputs','answer','preview_samples');

-- +goose StatementBegin
CREATE FUNCTION eacp.studio_run_start_as(p_agent uuid, p_inputs jsonb, p_mode text, p_samples jsonb) RETURNS uuid
    LANGUAGE plpgsql
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
    IF current_user <> 'eacp_owner' OR p_mode IS NULL OR p_mode NOT IN ('run','preview') THEN
        RAISE EXCEPTION 'private Studio run writer' USING ERRCODE='42501'; END IF;
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

    INSERT INTO eacp.studio_runs (tenant_id, agent_id, version_id, requested_by, inputs, deadline, mode, preview_samples)
    VALUES (tenant, p_agent, v.id, a, p_inputs,
            now() + make_interval(secs => (def->'limits'->>'timeout_seconds')::integer), p_mode, p_samples)
    RETURNING id INTO run;
    RETURN run;
END
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION eacp.studio_run_start_as(uuid,jsonb,text,jsonb) FROM PUBLIC,eacp_app;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.studio_run_start(p_agent uuid,p_inputs jsonb) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,pg_temp AS $$
BEGIN RETURN eacp.studio_run_start_as(p_agent,p_inputs,'run',NULL); END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.studio_run_private_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.mode IS DISTINCT FROM OLD.mode THEN RAISE EXCEPTION 'run mode is immutable' USING ERRCODE='55000'; END IF;
    IF NEW.state IN ('SUCCEEDED','FAILED') THEN NEW.preview_samples:=NULL; END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER studio_run_private_guard BEFORE UPDATE ON eacp.studio_runs FOR EACH ROW EXECUTE FUNCTION eacp.studio_run_private_guard();

-- +goose StatementBegin
-- Run-first locking; this function never locks an action or calls the PDP.
CREATE FUNCTION eacp.studio_node_held(p_run uuid,p_runtime text,p_generation bigint,p_index integer)
RETURNS eacp.studio_runs LANGUAGE plpgsql AS $$
DECLARE r eacp.studio_runs%ROWTYPE; def jsonb;
BEGIN
    r:=eacp.studio_run_held(p_run,p_runtime,p_generation);
    SELECT definition::jsonb INTO def FROM eacp.studio_versions WHERE tenant_id=r.tenant_id AND id=r.version_id;
    IF def->'schema_version'='1'::jsonb AND r.mode<>'preview' THEN
        RAISE EXCEPTION 'schema v1 uses its legacy step path' USING ERRCODE='55000'; END IF;
    IF p_index IS NULL OR p_index IS DISTINCT FROM r.current_index THEN
        RAISE EXCEPTION 'node is not the current chosen step' USING ERRCODE='55000'; END IF;
    PERFORM 1 FROM eacp.agent_versions WHERE tenant_id=r.tenant_id AND id=r.version_id AND state='ACTIVE' FOR SHARE;
    IF NOT FOUND THEN RAISE EXCEPTION 'version_replaced' USING ERRCODE='55000'; END IF;
    PERFORM pg_advisory_xact_lock(hashtextextended('eacp.kill:'||r.tenant_id::text,0));
    IF r.deadline<=now() OR eacp.studio_run_killed(r.tenant_id,r.agent_id,r.version_id,r.id) THEN
        RAISE EXCEPTION 'run is expired or killed' USING ERRCODE='55000'; END IF;
    RETURN r;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.studio_node_begin(p_run uuid,p_runtime text,p_generation bigint,p_index integer)
RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,pg_temp AS $$
DECLARE r eacp.studio_runs%ROWTYPE; s jsonb; node eacp.studio_run_nodes%ROWTYPE;
BEGIN
    r:=eacp.studio_node_held(p_run,p_runtime,p_generation,p_index);
    SELECT definition::jsonb->'steps'->p_index INTO s FROM eacp.studio_versions WHERE tenant_id=r.tenant_id AND id=r.version_id;
    IF s IS NULL THEN RAISE EXCEPTION 'unknown node' USING ERRCODE='23514'; END IF;
    INSERT INTO eacp.studio_run_nodes(tenant_id,run_id,step_index,step_id,kind,runtime_id,lease_generation)
    VALUES(r.tenant_id,r.id,p_index,s->>'id',s->>'kind',p_runtime,p_generation) ON CONFLICT(tenant_id,run_id,step_index) DO NOTHING;
    SELECT * INTO node FROM eacp.studio_run_nodes WHERE tenant_id=r.tenant_id AND run_id=r.id AND step_index=p_index;
    IF node.call_id IS NULL AND (node.runtime_id<>p_runtime OR node.lease_generation<>p_generation) THEN
        UPDATE eacp.studio_run_nodes SET runtime_id=p_runtime,lease_generation=p_generation
        WHERE tenant_id=r.tenant_id AND id=node.id RETURNING * INTO node;
    END IF;
    RETURN jsonb_build_object('intent_id',node.id,'call_id',node.call_id,'state',node.state,'kind',node.kind,
        'action_id',node.action_id,'failure_reason',node.failure_reason);
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.studio_llm_begin(p_run uuid,p_runtime text,p_generation bigint,p_index integer)
RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,pg_temp AS $$
DECLARE node jsonb;
BEGIN
    node:=eacp.studio_node_begin(p_run,p_runtime,p_generation,p_index);
    IF node->>'kind'<>'llm' THEN RAISE EXCEPTION 'node is not an LLM step' USING ERRCODE='23514'; END IF;
    RETURN node;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.studio_node_complete(p_run uuid,p_runtime text,p_generation bigint,p_index integer,p_result jsonb)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,pg_temp AS $$
DECLARE r eacp.studio_runs%ROWTYPE; s jsonb; def jsonb; node eacp.studio_run_nodes%ROWTYPE;
    next_id text; dest integer; act eacp.actions%ROWTYPE;
BEGIN
    r:=eacp.studio_node_held(p_run,p_runtime,p_generation,p_index);
    SELECT definition::jsonb INTO def FROM eacp.studio_versions WHERE tenant_id=r.tenant_id AND id=r.version_id;
    s:=def->'steps'->p_index;
    SELECT * INTO node FROM eacp.studio_run_nodes WHERE tenant_id=r.tenant_id AND run_id=r.id AND step_index=p_index FOR UPDATE;
    IF NOT FOUND OR node.state<>'pending' OR jsonb_typeof(p_result) IS DISTINCT FROM 'object' THEN
        RAISE EXCEPTION 'node was not begun or is complete' USING ERRCODE='55000'; END IF;
    IF node.kind='branch' THEN
        IF jsonb_typeof(p_result->'choice') IS DISTINCT FROM 'boolean' OR (SELECT count(*) FROM jsonb_object_keys(p_result))<>1 THEN
            RAISE EXCEPTION 'branch completion is one fixed choice' USING ERRCODE='23514'; END IF;
        next_id:=CASE WHEN (p_result->>'choice')::boolean THEN s->>'then' ELSE s->>'else' END;
    ELSIF node.kind='tool_call' THEN
        IF r.mode='preview' THEN
            IF NOT COALESCE(r.preview_samples,'{}') ? node.step_id OR p_result<>'{}'::jsonb THEN
                RAISE EXCEPTION 'preview needs its tool sample' USING ERRCODE='23514'; END IF;
        ELSE
            SELECT * INTO act FROM eacp.actions WHERE tenant_id=r.tenant_id AND id=(p_result->>'action_id')::uuid;
            IF NOT FOUND OR act.studio_run_id IS DISTINCT FROM r.id OR act.agent_version_id<>r.version_id
            OR act.idempotency_key<>format('studio:%s:%s',r.id,p_index) OR act.state<>'SUCCEEDED'
            OR act.tool IS DISTINCT FROM s->>'tool' OR (SELECT count(*) FROM jsonb_object_keys(p_result))<>1 THEN
                RAISE EXCEPTION 'node needs its own succeeded action' USING ERRCODE='55000'; END IF;
            UPDATE eacp.studio_run_nodes SET action_id=act.id WHERE tenant_id=r.tenant_id AND id=node.id;
        END IF;
        next_id:=s->>'next';
    ELSIF node.kind='llm' THEN
        -- Added by the Studio gateway migration; no caller can advance it yet.
        RAISE EXCEPTION 'LLM completion requires a recorded typed output' USING ERRCODE='55000';
    ELSE RAISE EXCEPTION 'respond finishes the run' USING ERRCODE='55000'; END IF;
    IF def->'schema_version'='1'::jsonb THEN dest:=p_index+1;
    ELSE SELECT ordinality::integer-1 INTO dest FROM jsonb_array_elements(def->'steps') WITH ORDINALITY e(value,ordinality) WHERE value->>'id'=next_id; END IF;
    IF dest IS NULL OR dest<=p_index THEN RAISE EXCEPTION 'invalid successor' USING ERRCODE='23514'; END IF;
    UPDATE eacp.studio_run_nodes SET state='completed',next_index=dest,completed_at=now() WHERE tenant_id=r.tenant_id AND id=node.id;
    UPDATE eacp.studio_runs SET current_index=dest WHERE tenant_id=r.tenant_id AND id=r.id;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.studio_preview_start(p_version uuid,p_inputs jsonb,p_samples jsonb) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,pg_temp AS $$
DECLARE tenant uuid:=eacp.current_tenant_id(); actor uuid:=eacp.actor(); ag uuid; def jsonb; run uuid; k text;
BEGIN
    SELECT s.agent_id,s.definition::jsonb INTO ag,def FROM eacp.studio_versions s
    JOIN eacp.agents g ON g.tenant_id=s.tenant_id AND g.id=s.agent_id
    JOIN eacp.agent_versions v ON v.tenant_id=s.tenant_id AND v.id=s.id
    WHERE s.tenant_id=tenant AND s.id=p_version AND g.owner_principal_id=actor AND v.state='ACTIVE' AND s.decision='approved';
    IF NOT FOUND THEN RAISE EXCEPTION 'only an approved version owner starts a preview' USING ERRCODE='42501'; END IF;
    IF jsonb_typeof(p_samples) IS DISTINCT FROM 'object' OR octet_length(p_samples::text)>65536 THEN
        RAISE EXCEPTION 'preview samples are an object of at most 65536 bytes' USING ERRCODE='23514'; END IF;
    FOR k IN SELECT jsonb_object_keys(p_samples) LOOP
        IF NOT EXISTS(SELECT 1 FROM jsonb_array_elements(def->'steps') s WHERE s->>'id'=k AND s->>'kind'='tool_call') THEN
            RAISE EXCEPTION 'sample is not for a tool node' USING ERRCODE='23514'; END IF;
    END LOOP;
    run:=eacp.studio_run_start_as(ag,p_inputs,'preview',p_samples);
    IF NOT EXISTS(SELECT 1 FROM eacp.studio_runs WHERE tenant_id=tenant AND id=run AND version_id=p_version) THEN
        RAISE EXCEPTION 'preview version is no longer active' USING ERRCODE='55000'; END IF;
    RETURN run;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.studio_node_output(p_run uuid,p_runtime text,p_generation bigint,p_index integer)
RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,pg_temp AS $$
DECLARE r eacp.studio_runs%ROWTYPE; s jsonb;
BEGIN
    r:=eacp.studio_run_held(p_run,p_runtime,p_generation);
    r:=eacp.studio_node_held(p_run,p_runtime,p_generation,r.current_index);
    IF p_index IS NULL OR p_index>r.current_index OR p_index<0 THEN
        RAISE EXCEPTION 'output is not from a visited node' USING ERRCODE='55000'; END IF;
    IF p_index<>r.current_index AND NOT EXISTS(SELECT 1 FROM eacp.studio_run_nodes
        WHERE tenant_id=r.tenant_id AND run_id=r.id AND step_index=p_index AND state='completed') THEN
        RAISE EXCEPTION 'output is not from a visited node' USING ERRCODE='55000'; END IF;
    SELECT definition::jsonb->'steps'->p_index INTO s FROM eacp.studio_versions WHERE tenant_id=r.tenant_id AND id=r.version_id;
    IF r.mode='preview' AND s->>'kind'='tool_call' AND r.preview_samples ? (s->>'id') THEN
        RETURN jsonb_build_object('state','succeeded','output',r.preview_samples->(s->>'id'));
    END IF;
    RETURN jsonb_build_object('state','pending');
END
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION eacp.studio_node_begin(uuid,text,bigint,integer),eacp.studio_node_complete(uuid,text,bigint,integer,jsonb),
 eacp.studio_llm_begin(uuid,text,bigint,integer),eacp.studio_node_output(uuid,text,bigint,integer),eacp.studio_preview_start(uuid,jsonb,jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.studio_node_begin(uuid,text,bigint,integer),eacp.studio_node_complete(uuid,text,bigint,integer,jsonb),
 eacp.studio_llm_begin(uuid,text,bigint,integer),eacp.studio_node_output(uuid,text,bigint,integer),eacp.studio_preview_start(uuid,jsonb,jsonb) TO eacp_app;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.actions_studio_run() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    m      text[];
    run    uuid;
    state  text;
    ver    uuid;
    req    uuid;
    step   jsonb;
    mode text;
    cursor integer;
    schema integer;
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
        SELECT r.id, r.state, r.version_id, r.requested_by, r.mode, r.current_index INTO run, state, ver, req, mode, cursor
        FROM eacp.studio_runs r WHERE r.tenant_id = NEW.tenant_id AND r.id = m[1]::uuid;
        SELECT s.definition::jsonb->'steps'->(m[2]::integer), (s.definition::jsonb->>'schema_version')::integer INTO step, schema
        FROM eacp.studio_versions s WHERE s.tenant_id = NEW.tenant_id AND s.id = ver;
    END IF;
    IF run IS NULL OR mode IS DISTINCT FROM 'run' OR (schema=2 AND cursor<>m[2]::integer) OR state <> 'RUNNING' OR ver <> NEW.agent_version_id
       OR req IS DISTINCT FROM NEW.subject_principal_id OR step IS NULL
       OR step->>'kind' IS DISTINCT FROM 'tool_call' OR step->>'tool' IS DISTINCT FROM NEW.tool THEN
        RAISE EXCEPTION 'a Studio agent acts only as a step of its own running run' USING ERRCODE = '42501';
    END IF;
    NEW.studio_run_id := run;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
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
            'deadline', r.deadline, 'inputs', r.inputs, 'mode',r.mode, 'current_index',r.current_index,
            'schema_version',(SELECT (definition::jsonb->>'schema_version')::integer FROM eacp.studio_versions WHERE tenant_id=tenant AND id=r.version_id),
            'nodes',COALESCE((SELECT jsonb_agg(jsonb_build_object('index',step_index,'kind',kind,'state',state,
                'intent_id',id,'call_id',call_id,'action_id',action_id,'next_index',next_index,'failure_reason',failure_reason) ORDER BY step_index)
                FROM eacp.studio_run_nodes WHERE tenant_id=tenant AND run_id=r.id),'[]'),
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
CREATE OR REPLACE FUNCTION eacp.studio_run_finish(p_run uuid, p_runtime text, p_generation bigint, p_state text,
                                       p_answer text, p_reason text) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    tenant uuid := eacp.current_tenant_id();
    r      eacp.studio_runs%ROWTYPE;
    calls  integer;
    done   integer;
    def jsonb;
BEGIN
    r := eacp.studio_run_held(p_run, p_runtime, p_generation);
    IF p_state = 'SUCCEEDED' THEN
        IF p_answer IS NULL OR p_reason IS NOT NULL OR octet_length(p_answer) > 65536 THEN
            RAISE EXCEPTION 'a succeeded run has an answer of at most 65536 bytes and no reason' USING ERRCODE = '23514';
        END IF;
        SELECT definition::jsonb INTO def FROM eacp.studio_versions WHERE tenant_id=tenant AND id=r.version_id;
        IF def->'schema_version'='2'::jsonb OR r.mode='preview' THEN
            r:=eacp.studio_node_held(p_run,p_runtime,p_generation,r.current_index);
            IF def->'steps'->r.current_index->>'kind' IS DISTINCT FROM 'respond' THEN
                RAISE EXCEPTION 'run has not reached its chosen response' USING ERRCODE='55000'; END IF;
        ELSE
        SELECT count(*) INTO calls FROM eacp.studio_versions s,
            jsonb_array_elements(s.definition::jsonb->'steps') AS e(step)
        WHERE s.tenant_id = tenant AND s.id = r.version_id AND e.step->>'kind' = 'tool_call';
        SELECT count(*) INTO done FROM eacp.studio_run_steps WHERE tenant_id = tenant AND run_id = p_run;
        IF done <> calls THEN
            RAISE EXCEPTION 'a run succeeds only once every step is recorded (% of %)', done, calls USING ERRCODE = '55000';
        END IF;
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
CREATE OR REPLACE FUNCTION eacp.studio_run_step(p_run uuid, p_runtime text, p_generation bigint, p_index integer, p_action uuid)
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
    IF r.mode='preview' OR EXISTS(SELECT 1 FROM eacp.studio_versions WHERE tenant_id=r.tenant_id AND id=r.version_id AND definition::jsonb->'schema_version'='2'::jsonb) THEN
        RAISE EXCEPTION 'this run uses fenced graph nodes' USING ERRCODE='55000'; END IF;
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

-- +goose Down

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.studio_run_step(p_run uuid, p_runtime text, p_generation bigint, p_index integer, p_action uuid)
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
CREATE OR REPLACE FUNCTION eacp.studio_run_finish(p_run uuid, p_runtime text, p_generation bigint, p_state text,
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

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.actions_studio_run() RETURNS trigger
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

-- +goose StatementBegin
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

DROP FUNCTION eacp.studio_preview_start(uuid,jsonb,jsonb);
DROP FUNCTION eacp.studio_node_output(uuid,text,bigint,integer);
DROP FUNCTION eacp.studio_llm_begin(uuid,text,bigint,integer);
DROP FUNCTION eacp.studio_node_complete(uuid,text,bigint,integer,jsonb);
DROP FUNCTION eacp.studio_node_begin(uuid,text,bigint,integer);
DROP FUNCTION eacp.studio_node_held(uuid,text,bigint,integer);
DROP FUNCTION eacp.studio_run_start_as(uuid,jsonb,text,jsonb);
DROP TRIGGER studio_run_private_guard ON eacp.studio_runs;
DROP FUNCTION eacp.studio_run_private_guard();
DROP TABLE eacp.studio_run_nodes;
DROP TRIGGER zz_audit ON eacp.studio_runs;
CREATE TRIGGER zz_audit AFTER INSERT OR UPDATE ON eacp.studio_runs FOR EACH ROW EXECUTE FUNCTION eacp.audit_row_change_redacted('inputs','answer');
ALTER TABLE eacp.studio_runs DROP COLUMN mode,DROP COLUMN current_index,DROP COLUMN preview_samples;
ALTER TABLE eacp.studio_runs DROP CONSTRAINT studio_runs_failure_reason_check;
ALTER TABLE eacp.studio_runs ADD CONSTRAINT studio_runs_failure_reason_check CHECK(failure_reason IN
 ('credential_pending','credential_expired','action_denied','action_failed','action_unknown','action_cancelled',
 'result_unavailable','answer_too_large','deadline_exceeded','version_replaced','credential_revoked','killed'));
