-- Phase 27a-1 (ADR-033): Agent Studio's data and rules. An employee holding
-- studio_author saves an agent definition; PostgreSQL validates it, computes
-- its digest and the tools it needs, and creates the registry agent, a
-- REGISTERED version and an allowlist holding exactly those tools. A
-- registry_approver other than the author approves or rejects it through the
-- existing registry guards. The Studio runtime (studio_runtime, a service
-- principal holding no other role) may propose an agent key only for an
-- approved Studio version.
-- +goose Up

-- ----------------------------------------------------------------- roles

ALTER TABLE eacp.role_grants DROP CONSTRAINT role_grants_role_check;
ALTER TABLE eacp.role_grants ADD CONSTRAINT role_grants_role_check CHECK (role IN
    ('admin', 'registry_editor', 'registry_approver', 'operator', 'approver', 'auditor',
     'studio_author', 'studio_runtime'));

-- +goose StatementBegin
-- eacp.holds_role is assert_role as a test: whether actor is an enabled
-- principal of the current tenant with an approved, unrevoked grant of role.
CREATE FUNCTION eacp.holds_role(p_actor uuid, p_role text) RETURNS boolean
    LANGUAGE sql STABLE
    AS $$
    SELECT EXISTS (SELECT 1 FROM eacp.principals p
                   JOIN eacp.role_grants g ON g.tenant_id = p.tenant_id AND g.principal_id = p.id
                   WHERE p.tenant_id = eacp.current_tenant_id() AND p.id = p_actor AND p.disabled_at IS NULL
                     AND g.role = p_role AND g.approved_at IS NOT NULL AND g.revoked_at IS NULL)
$$;
-- +goose StatementEnd

-- ---------------------------------------------------------------- tables

-- One row per transaction that saved in Studio; open only while
-- eacp.studio_save runs (xid8 values never repeat). The application role
-- reads it and never writes it, so only eacp.studio_save can open a mark.
CREATE TABLE eacp.studio_save_marks (
    tenant_id uuid    NOT NULL REFERENCES eacp.tenants (id),
    xact      xid8    NOT NULL,
    open      boolean NOT NULL,
    PRIMARY KEY (tenant_id, xact)
);

-- A Studio agent: a registry agent created by eacp.studio_save.
CREATE TABLE eacp.studio_agents (
    tenant_id           uuid        NOT NULL,
    id                  uuid        NOT NULL,
    department_group_id uuid        NOT NULL,
    description         text        NOT NULL CHECK (length(description) <= 1000),
    created_by          uuid        NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, id) REFERENCES eacp.agents (tenant_id, id),
    FOREIGN KEY (tenant_id, department_group_id) REFERENCES eacp.groups (tenant_id, id),
    FOREIGN KEY (tenant_id, created_by) REFERENCES eacp.principals (tenant_id, id)
);

-- A Studio version: its definition, digest and derived capability, and the
-- one-time decision on its capability request.
CREATE TABLE eacp.studio_versions (
    tenant_id       uuid        NOT NULL,
    id              uuid        NOT NULL,
    agent_id        uuid        NOT NULL,
    definition      text        NOT NULL CHECK (definition IS JSON OBJECT AND octet_length(definition) <= 65536),
    digest          text        NOT NULL CHECK (digest ~ '^[0-9a-f]{64}$'),
    capability      text[]      NOT NULL,
    created_by      uuid        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    decision        text        CHECK (decision IN ('approved', 'rejected')),
    decided_by      uuid,
    decided_at      timestamptz,
    decision_reason text        CHECK (length(decision_reason) <= 1024),
    PRIMARY KEY (tenant_id, id),
    CHECK (num_nulls(decision, decided_by, decided_at, decision_reason) IN (0, 4)),
    FOREIGN KEY (tenant_id, id) REFERENCES eacp.agent_versions (tenant_id, id),
    FOREIGN KEY (tenant_id, agent_id) REFERENCES eacp.studio_agents (tenant_id, id),
    FOREIGN KEY (tenant_id, created_by) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, decided_by) REFERENCES eacp.principals (tenant_id, id)
);
CREATE INDEX studio_versions_agent ON eacp.studio_versions (tenant_id, agent_id);
CREATE INDEX studio_versions_waiting ON eacp.studio_versions (tenant_id, created_at) WHERE decision IS NULL;

-- +goose StatementBegin
CREATE FUNCTION eacp.studio_versions_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF (NEW.tenant_id, NEW.id, NEW.agent_id, NEW.definition, NEW.digest, NEW.capability, NEW.created_by, NEW.created_at)
       IS DISTINCT FROM
       (OLD.tenant_id, OLD.id, OLD.agent_id, OLD.definition, OLD.digest, OLD.capability, OLD.created_by, OLD.created_at) THEN
        RAISE EXCEPTION 'a Studio version is immutable' USING ERRCODE = '55000';
    END IF;
    IF OLD.decision IS NOT NULL THEN
        RAISE EXCEPTION 'this request is already decided' USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.studio_agents_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    RAISE EXCEPTION 'a Studio agent is immutable' USING ERRCODE = '55000';
END
$$;
-- +goose StatementEnd

CREATE TRIGGER studio_versions_guard BEFORE UPDATE ON eacp.studio_versions
    FOR EACH ROW EXECUTE FUNCTION eacp.studio_versions_guard();
CREATE TRIGGER studio_agents_guard BEFORE UPDATE ON eacp.studio_agents
    FOR EACH ROW EXECUTE FUNCTION eacp.studio_agents_guard();

-- +goose StatementBegin
DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['studio_save_marks', 'studio_agents', 'studio_versions']
    LOOP
        EXECUTE format('ALTER TABLE eacp.%I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE eacp.%I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('CREATE POLICY tenant_isolation ON eacp.%I USING (tenant_id = eacp.current_tenant_id())', t);
        -- Every write goes through eacp.studio_save and eacp.studio_decide.
        EXECUTE format('REVOKE ALL ON eacp.%I FROM eacp_app', t);
        EXECUTE format('GRANT SELECT ON eacp.%I TO eacp_app', t);
    END LOOP;
    FOREACH t IN ARRAY ARRAY['studio_agents', 'studio_versions']
    LOOP
        EXECUTE format('CREATE TRIGGER zz_audit AFTER INSERT OR UPDATE ON eacp.%I '
                       'FOR EACH ROW EXECUTE FUNCTION eacp.audit_row_change()', t);
    END LOOP;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- True only while eacp.studio_save runs in this transaction.
CREATE FUNCTION eacp.in_studio_save() RETURNS boolean
    LANGUAGE sql STABLE
    AS $$
    SELECT EXISTS (SELECT 1 FROM eacp.studio_save_marks
                   WHERE tenant_id = eacp.current_tenant_id() AND xact = pg_current_xact_id_if_assigned() AND open)
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- A Studio version becomes ACTIVE, or gets its allowlist, only once its
-- request is approved (eacp.studio_decide records the decision first).
CREATE FUNCTION eacp.studio_assert_approved(p_tenant uuid, p_version uuid) RETURNS void
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM eacp.studio_versions
               WHERE tenant_id = p_tenant AND id = p_version AND decision IS DISTINCT FROM 'approved') THEN
        RAISE EXCEPTION 'a Studio version is activated only by an approval of its request' USING ERRCODE = '42501';
    END IF;
END
$$;
-- +goose StatementEnd

-- ------------------------------------------------------------ definitions

-- +goose StatementBegin
CREATE FUNCTION eacp.studio_int(v jsonb, lo integer, hi integer) RETURNS boolean
    LANGUAGE sql IMMUTABLE
    AS $$
    SELECT COALESCE(jsonb_typeof(v) = 'number' AND (v::text)::numeric = trunc((v::text)::numeric)
                    AND (v::text)::numeric BETWEEN lo AND hi, false)
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- eacp.studio_placeholders checks every {{...}} in strs: an input declared
-- in inputs, or the output (at most 8 keys deep) of a step in ids. It
-- returns how many it found. Nothing is evaluated.
CREATE FUNCTION eacp.studio_placeholders(strs text[], inputs text[], ids text[]) RETURNS integer
    LANGUAGE plpgsql IMMUTABLE
    AS $$
DECLARE
    s text;
    m text[];
    found_here integer;
    total integer := 0;
BEGIN
    FOREACH s IN ARRAY COALESCE(strs, '{}') LOOP
        found_here := 0;
        FOR m IN SELECT regexp_matches(s, '\{\{([^{}]*)\}\}', 'g') LOOP
            IF NOT ((m[1] ~ '^inputs\.[a-z][a-z0-9_]{0,31}$' AND substr(m[1], 8) = ANY (inputs))
                    OR (m[1] ~ '^steps\.[a-z][a-z0-9_]{0,31}\.output(\.[A-Za-z0-9_]+){0,8}$'
                        AND split_part(m[1], '.', 2) = ANY (ids))) THEN
                RAISE EXCEPTION 'definition: placeholder {{%}} is not a declared input or an earlier step''s output', m[1]
                    USING ERRCODE = '23514';
            END IF;
            found_here := found_here + 1;
        END LOOP;
        IF found_here <> regexp_count(s, '\{\{') THEN
            RAISE EXCEPTION 'definition: a placeholder is not closed or not valid' USING ERRCODE = '23514';
        END IF;
        total := total + found_here;
    END LOOP;
    RETURN total;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- eacp.studio_definition_capability validates a definition (schema version 1,
-- spec 27a-1 section 3.2) and returns its capability: the sorted, distinct
-- tools of its tool_call steps. Every refusal is 23514 naming the rule.
CREATE FUNCTION eacp.studio_definition_capability(p_definition text) RETURNS text[]
    LANGUAGE plpgsql STABLE
    AS $$
DECLARE
    name_re   constant text := '^[a-z][a-z0-9_]{0,31}$';
    -- Best effort: a definition never needs a key or a secret (ADR-033 §7).
    secret_re constant text := '(eacp_[ap]k_|-----BEGIN|AKIA[0-9A-Z]{16}|Bearer |ghp_|xox[a-z]-|sk-[A-Za-z0-9_-]{20,})';
    d      jsonb;
    x      jsonb;
    s      jsonb;
    k      text;
    n      integer;
    inputs text[] := '{}';
    ids    text[] := '{}';
    tools  text[] := '{}';
    allowed integer := 0;
    total  integer;
BEGIN
    IF p_definition IS NULL OR octet_length(p_definition) > 65536 THEN
        RAISE EXCEPTION 'definition: at most 65536 bytes' USING ERRCODE = '23514';
    END IF;
    IF NOT (p_definition IS JSON OBJECT WITH UNIQUE KEYS) THEN
        RAISE EXCEPTION 'definition: must be a JSON object without duplicate keys' USING ERRCODE = '23514';
    END IF;
    d := p_definition::jsonb;

    IF EXISTS (SELECT 1 FROM jsonb_path_query(d, 'strict $.** ? (@.type() == "string")') AS j(v)
               WHERE (v #>> '{}') ~ secret_re)
       OR EXISTS (SELECT 1 FROM jsonb_path_query(d, 'strict $.** ? (@.type() == "object").keyvalue().key') AS j(v)
                  WHERE (v #>> '{}') ~ secret_re) THEN
        RAISE EXCEPTION 'definition_contains_secret: a definition never holds a key or a secret' USING ERRCODE = '23514';
    END IF;

    SELECT key INTO k FROM jsonb_object_keys(d) AS o(key)
    WHERE key NOT IN ('schema_version', 'kind', 'inputs', 'steps', 'limits') LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'definition: unknown key %', k USING ERRCODE = '23514';
    END IF;
    IF NOT eacp.studio_int(d->'schema_version', 1, 1) THEN
        RAISE EXCEPTION 'definition: schema_version must be 1' USING ERRCODE = '23514';
    END IF;
    IF d->'kind' IS DISTINCT FROM '"agent"'::jsonb THEN
        RAISE EXCEPTION 'definition: kind must be "agent"' USING ERRCODE = '23514';
    END IF;

    -- Inputs: at most 10 strings.
    x := COALESCE(d->'inputs', '{}'::jsonb);
    IF jsonb_typeof(x) <> 'object' THEN
        RAISE EXCEPTION 'definition: inputs must be an object' USING ERRCODE = '23514';
    END IF;
    SELECT count(*) INTO n FROM jsonb_object_keys(x);
    IF n > 10 THEN
        RAISE EXCEPTION 'definition: at most 10 inputs' USING ERRCODE = '23514';
    END IF;
    FOR k, s IN SELECT key, value FROM jsonb_each(x) LOOP
        IF k !~ name_re THEN
            RAISE EXCEPTION 'definition: input name % must match %', k, name_re USING ERRCODE = '23514';
        END IF;
        IF jsonb_typeof(s) <> 'object'
           OR EXISTS (SELECT 1 FROM jsonb_object_keys(s) AS o(key) WHERE key NOT IN ('type', 'max_length')) THEN
            RAISE EXCEPTION 'definition: input % has only type and max_length', k USING ERRCODE = '23514';
        END IF;
        IF s->'type' IS DISTINCT FROM '"string"'::jsonb THEN
            RAISE EXCEPTION 'definition: input % must be of type string', k USING ERRCODE = '23514';
        END IF;
        IF NOT eacp.studio_int(s->'max_length', 1, 1024) THEN
            RAISE EXCEPTION 'definition: input % max_length must be 1 to 1024', k USING ERRCODE = '23514';
        END IF;
        inputs := inputs || k;
    END LOOP;

    -- Steps: 1 to 20, tool_call or respond, respond last and only last.
    x := d->'steps';
    IF jsonb_typeof(x) IS DISTINCT FROM 'array' OR jsonb_array_length(x) NOT BETWEEN 1 AND 20 THEN
        RAISE EXCEPTION 'definition: steps must be an array of 1 to 20 steps' USING ERRCODE = '23514';
    END IF;
    n := jsonb_array_length(x);
    FOR i IN 0 .. n - 1 LOOP
        s := x->i;
        IF jsonb_typeof(s) <> 'object' THEN
            RAISE EXCEPTION 'definition: step % must be an object', i + 1 USING ERRCODE = '23514';
        END IF;
        IF jsonb_typeof(s->'id') IS DISTINCT FROM 'string' OR s->>'id' !~ name_re THEN
            RAISE EXCEPTION 'definition: step % id must match %', i + 1, name_re USING ERRCODE = '23514';
        END IF;
        IF s->>'id' = ANY (ids) THEN
            RAISE EXCEPTION 'definition: step ids must be unique (%)', s->>'id' USING ERRCODE = '23514';
        END IF;
        IF s->'kind' = '"respond"'::jsonb THEN
            IF i <> n - 1 THEN
                RAISE EXCEPTION 'definition: respond is the last step, and only the last' USING ERRCODE = '23514';
            END IF;
            SELECT key INTO k FROM jsonb_object_keys(s) AS o(key) WHERE key NOT IN ('id', 'kind', 'text') LIMIT 1;
            IF FOUND THEN
                RAISE EXCEPTION 'definition: step % has an unknown key %', i + 1, k USING ERRCODE = '23514';
            END IF;
            IF jsonb_typeof(s->'text') IS DISTINCT FROM 'string' OR length(s->>'text') > 4096 THEN
                RAISE EXCEPTION 'definition: respond text must be a string of at most 4096 characters'
                    USING ERRCODE = '23514';
            END IF;
            allowed := allowed + eacp.studio_placeholders(ARRAY[s->>'text'], inputs, ids);
        ELSIF s->'kind' = '"tool_call"'::jsonb THEN
            IF i = n - 1 THEN
                RAISE EXCEPTION 'definition: respond is the last step, and only the last' USING ERRCODE = '23514';
            END IF;
            SELECT key INTO k FROM jsonb_object_keys(s) AS o(key)
            WHERE key NOT IN ('id', 'kind', 'tool', 'tool_schema_version', 'operation', 'target', 'resource', 'payload')
            LIMIT 1;
            IF FOUND THEN
                RAISE EXCEPTION 'definition: step % has an unknown key %', i + 1, k USING ERRCODE = '23514';
            END IF;
            IF jsonb_typeof(s->'tool') IS DISTINCT FROM 'string'
               OR s->>'tool' !~ '^[a-z0-9][a-z0-9-]{1,62}\.[a-z0-9][a-z0-9_-]{0,62}$' THEN
                RAISE EXCEPTION 'definition: step % tool must name connector.tool', i + 1 USING ERRCODE = '23514';
            END IF;
            PERFORM 1 FROM eacp.tools t
            JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
            WHERE t.tenant_id = eacp.current_tenant_id() AND c.name || '.' || t.name = s->>'tool';
            IF NOT FOUND THEN
                RAISE EXCEPTION 'definition: unknown tool %', s->>'tool' USING ERRCODE = '23514';
            END IF;
            FOREACH k IN ARRAY ARRAY['tool_schema_version', 'operation', 'target', 'resource'] LOOP
                IF jsonb_typeof(s->k) IS DISTINCT FROM 'string' OR btrim(s->>k) = '' OR length(s->>k) > 128 THEN
                    RAISE EXCEPTION 'definition: step % % must be a non-empty string of at most 128 characters', i + 1, k
                        USING ERRCODE = '23514';
                END IF;
            END LOOP;
            IF jsonb_typeof(s->'payload') IS DISTINCT FROM 'object' THEN
                RAISE EXCEPTION 'definition: step % payload must be an object', i + 1 USING ERRCODE = '23514';
            END IF;
            allowed := allowed + eacp.studio_placeholders(ARRAY(
                SELECT v #>> '{}' FROM jsonb_path_query(s->'payload', 'strict $.** ? (@.type() == "string")') AS j(v)),
                inputs, ids);
            tools := tools || (s->>'tool');
        ELSE
            RAISE EXCEPTION 'definition: step % kind must be tool_call or respond', i + 1 USING ERRCODE = '23514';
        END IF;
        ids := ids || (s->>'id');
    END LOOP;

    -- Placeholders appear only in payload string values and respond text.
    SELECT COALESCE(sum(regexp_count(v #>> '{}', '\{\{')), 0) INTO total FROM (
        SELECT v FROM jsonb_path_query(d, 'strict $.** ? (@.type() == "string")') AS j(v)
        UNION ALL
        SELECT v FROM jsonb_path_query(d, 'strict $.** ? (@.type() == "object").keyvalue().key') AS j(v)) AS all_strings;
    IF total <> allowed THEN
        RAISE EXCEPTION 'definition: a placeholder appears only in a payload value or a respond text'
            USING ERRCODE = '23514';
    END IF;

    x := d->'limits';
    IF jsonb_typeof(x) IS DISTINCT FROM 'object'
       OR EXISTS (SELECT 1 FROM jsonb_object_keys(x) AS o(key) WHERE key <> 'timeout_seconds') THEN
        RAISE EXCEPTION 'definition: limits holds only timeout_seconds' USING ERRCODE = '23514';
    END IF;
    IF NOT eacp.studio_int(x->'timeout_seconds', 10, 3600) THEN
        RAISE EXCEPTION 'definition: limits.timeout_seconds must be 10 to 3600' USING ERRCODE = '23514';
    END IF;

    RETURN ARRAY(SELECT DISTINCT t COLLATE "C" FROM unnest(tools) AS u(t) ORDER BY 1);
END
$$;
-- +goose StatementEnd

-- ---------------------------------------------------------------- saving

-- +goose StatementBegin
-- eacp.studio_save saves a definition as the transaction's principal, who
-- must hold studio_author: a new agent (p_agent NULL; the author must be a
-- member of p_department) or the next version of the author's Studio agent.
-- It opens this transaction's save mark, so the registry guards accept the
-- author, and closes it before returning. Returns the version id.
CREATE FUNCTION eacp.studio_save(p_agent uuid, p_name text, p_display_name text, p_description text,
                                 p_department uuid, p_definition text) RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    who      jsonb := eacp.actor_context();
    tenant   uuid := eacp.current_tenant_id();
    a        uuid;
    cap      text[];
    digest   text;
    tool_ids uuid[];
    version  uuid;
BEGIN
    IF who->>'kind' IS DISTINCT FROM 'principal' THEN
        RAISE EXCEPTION 'a Studio save is made by a principal' USING ERRCODE = '42501';
    END IF;
    a := (who->>'id')::uuid;
    PERFORM eacp.assert_role(a, 'studio_author');
    IF p_agent IS NULL THEN
        IF p_department IS NULL THEN
            RAISE EXCEPTION 'a new Studio agent names its department' USING ERRCODE = '23514';
        END IF;
        PERFORM 1 FROM eacp.group_memberships
        WHERE tenant_id = tenant AND group_id = p_department AND principal_id = a AND removed_at IS NULL FOR SHARE;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'the author is not a member of department %', p_department USING ERRCODE = '42501';
        END IF;
    ELSE
        IF num_nonnulls(p_name, p_display_name, p_description, p_department) > 0 THEN
            RAISE EXCEPTION 'a new version carries only its definition' USING ERRCODE = '23514';
        END IF;
        PERFORM 1 FROM eacp.studio_agents s
        JOIN eacp.agents g ON g.tenant_id = s.tenant_id AND g.id = s.id
        WHERE s.tenant_id = tenant AND s.id = p_agent AND g.owner_principal_id = a;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'no Studio agent % of this author', p_agent USING ERRCODE = '23503';
        END IF;
    END IF;

    cap := eacp.studio_definition_capability(p_definition);
    digest := encode(sha256(convert_to(p_definition, 'UTF8')), 'hex');
    SELECT COALESCE(array_agg(t.id), '{}') INTO tool_ids FROM eacp.tools t
    JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
    WHERE t.tenant_id = tenant AND c.name || '.' || t.name = ANY (cap);

    INSERT INTO eacp.studio_save_marks (tenant_id, xact, open) VALUES (tenant, pg_current_xact_id(), true)
    ON CONFLICT (tenant_id, xact) DO UPDATE SET open = true;
    IF p_agent IS NULL THEN
        INSERT INTO eacp.agents (tenant_id, name, display_name, environment, risk_class, owner_principal_id)
        VALUES (tenant, p_name, p_display_name, 'production', 'high', a) RETURNING id INTO p_agent;
        INSERT INTO eacp.studio_agents (tenant_id, id, department_group_id, description, created_by)
        VALUES (tenant, p_agent, p_department, COALESCE(p_description, ''), a);
    END IF;
    INSERT INTO eacp.agent_versions (tenant_id, agent_id, runtime, code_ref)
    VALUES (tenant, p_agent, 'studio', 'studio:' || digest) RETURNING id INTO version;
    INSERT INTO eacp.studio_versions (tenant_id, id, agent_id, definition, digest, capability, created_by)
    VALUES (tenant, version, p_agent, p_definition, digest, cap, a);
    INSERT INTO eacp.agent_allowlists (tenant_id, agent_version_id, tool_ids) VALUES (tenant, version, tool_ids);
    UPDATE eacp.studio_save_marks SET open = false WHERE tenant_id = tenant AND xact = pg_current_xact_id();
    RETURN version;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- eacp.studio_decide approves or rejects a Studio version's capability
-- request, once, as a registry_approver who is not its author. It records the
-- decision first and then moves the version through the registry guards:
-- approval retires the agent's ACTIVE version, activates the version's
-- allowlist and makes it ACTIVE; rejection retires it. Returns the new state.
CREATE FUNCTION eacp.studio_decide(p_version uuid, p_approve boolean, p_reason text) RETURNS text
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    who    jsonb := eacp.actor_context();
    tenant uuid := eacp.current_tenant_id();
    a      uuid;
    v      eacp.agent_versions%ROWTYPE;
    sv     eacp.studio_versions%ROWTYPE;
    old_id uuid;
BEGIN
    IF who->>'kind' IS DISTINCT FROM 'principal' THEN
        RAISE EXCEPTION 'a Studio decision is made by a principal' USING ERRCODE = '42501';
    END IF;
    a := (who->>'id')::uuid;
    PERFORM eacp.assert_role(a, 'registry_approver');
    PERFORM eacp.require_reason(p_reason, 'a Studio decision');
    IF p_approve IS NULL THEN
        RAISE EXCEPTION 'a Studio decision approves or rejects' USING ERRCODE = '23514';
    END IF;
    SELECT * INTO v FROM eacp.agent_versions WHERE tenant_id = tenant AND id = p_version FOR UPDATE;
    SELECT * INTO sv FROM eacp.studio_versions WHERE tenant_id = tenant AND id = p_version FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'no Studio request for version %', p_version USING ERRCODE = '23503';
    END IF;
    IF sv.decision IS NOT NULL THEN
        RAISE EXCEPTION 'this request is already decided' USING ERRCODE = '55000';
    END IF;
    IF a = sv.created_by THEN
        RAISE EXCEPTION 'two-person rule: the author cannot decide their own request' USING ERRCODE = '55000';
    END IF;
    IF v.state <> 'REGISTERED' THEN
        RAISE EXCEPTION 'version % is %; only a waiting version is decided', p_version, v.state USING ERRCODE = '55000';
    END IF;
    IF p_approve AND EXISTS (SELECT 1 FROM eacp.agent_releases
                             WHERE tenant_id = tenant AND agent_id = v.agent_id
                               AND state IN ('EVALUATING', 'SHADOW', 'CANARY')) THEN
        RAISE EXCEPTION 'agent % has an open release', v.agent_id USING ERRCODE = '55000';
    END IF;

    UPDATE eacp.studio_versions
    SET decision = CASE WHEN p_approve THEN 'approved' ELSE 'rejected' END,
        decided_by = a, decided_at = now(), decision_reason = p_reason
    WHERE tenant_id = tenant AND id = p_version;
    IF NOT p_approve THEN
        UPDATE eacp.agent_versions SET state = 'RETIRED', state_reason = p_reason WHERE tenant_id = tenant AND id = p_version;
        RETURN 'RETIRED';
    END IF;
    FOR old_id IN SELECT id FROM eacp.agent_versions
                  WHERE tenant_id = tenant AND agent_id = v.agent_id AND state = 'ACTIVE' AND id <> p_version
                  ORDER BY id FOR UPDATE LOOP
        UPDATE eacp.agent_versions SET state = 'RETIRED', state_reason = format('replaced by version %s', v.version)
        WHERE tenant_id = tenant AND id = old_id;
    END LOOP;
    UPDATE eacp.agent_versions SET active_allowlist_id = l.id
    FROM eacp.agent_allowlists l
    WHERE eacp.agent_versions.tenant_id = tenant AND eacp.agent_versions.id = p_version
      AND l.tenant_id = tenant AND l.agent_version_id = p_version;
    UPDATE eacp.agent_versions SET state = 'ACTIVE', state_reason = p_reason WHERE tenant_id = tenant AND id = p_version;
    RETURN 'ACTIVE';
END
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION eacp.studio_save(uuid, text, text, text, uuid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.studio_save(uuid, text, text, text, uuid, text) TO eacp_app;
REVOKE ALL ON FUNCTION eacp.studio_decide(uuid, boolean, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.studio_decide(uuid, boolean, text) TO eacp_app;

-- ---------------------------------------------------------------- guards

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.role_grants_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.actor();
    grantee eacp.principals%ROWTYPE;
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM eacp.assert_role(a, 'admin');
        PERFORM eacp.assert_distinct(a, NEW.principal_id, 'the grantee');
        SELECT * INTO grantee FROM eacp.principals
        WHERE tenant_id = NEW.tenant_id AND id = NEW.principal_id FOR SHARE;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'unknown principal %', NEW.principal_id USING ERRCODE = '23503';
        END IF;
        IF grantee.disabled_at IS NOT NULL THEN
            RAISE EXCEPTION 'principal % is disabled', NEW.principal_id USING ERRCODE = '55000';
        END IF;
        -- A service principal may only read the audit journal or be the
        -- Studio runtime. A human who controls a service principal must not
        -- be able to author as the service and approve as themselves
        -- (ADR-003 §1, ADR-033 §1).
        IF grantee.kind = 'service' AND NEW.role NOT IN ('auditor', 'studio_runtime') THEN
            RAISE EXCEPTION 'role % is human-only', NEW.role USING ERRCODE = '42501';
        END IF;
        IF grantee.kind = 'human' AND NEW.role = 'studio_runtime' THEN
            RAISE EXCEPTION 'studio_runtime is held only by a service principal' USING ERRCODE = '42501';
        END IF;
        -- studio_runtime is held alone: serialise the principal's grants and
        -- count every live one, approved or pending.
        PERFORM pg_advisory_xact_lock(hashtextextended('role_grants/' || NEW.tenant_id::text || '/'
                                                       || NEW.principal_id::text, 0));
        IF EXISTS (SELECT 1 FROM eacp.role_grants
                   WHERE tenant_id = NEW.tenant_id AND principal_id = NEW.principal_id AND revoked_at IS NULL
                     AND (role = 'studio_runtime') <> (NEW.role = 'studio_runtime')) THEN
            RAISE EXCEPTION 'studio_runtime is held alone' USING ERRCODE = '42501';
        END IF;
        IF a IS NOT NULL AND (NEW.approved_at IS NOT NULL OR NEW.approved_by IS NOT NULL) THEN
            RAISE EXCEPTION 'a grant must be approved by a second admin' USING ERRCODE = '42501';
        END IF;
        IF NEW.revoked_at IS NOT NULL OR NEW.revoked_by IS NOT NULL OR NEW.revoke_reason IS NOT NULL THEN
            RAISE EXCEPTION 'a grant cannot be created revoked' USING ERRCODE = '23514';
        END IF;
        NEW.proposed_by := a;
        NEW.proposed_at := now();
        IF NEW.approved_at IS NOT NULL THEN  -- bootstrap only
            NEW.approved_at := now();
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.principal_id IS DISTINCT FROM OLD.principal_id OR NEW.role IS DISTINCT FROM OLD.role
       OR NEW.proposed_by IS DISTINCT FROM OLD.proposed_by OR NEW.proposed_at IS DISTINCT FROM OLD.proposed_at THEN
        RAISE EXCEPTION 'a grant''s subject is immutable' USING ERRCODE = '55000';
    END IF;

    IF eacp.set_once(OLD.revoked_at, NEW.revoked_at, OLD.revoke_reason, NEW.revoke_reason, 'revocation') THEN
        IF NEW.approved_at IS DISTINCT FROM OLD.approved_at THEN
            RAISE EXCEPTION 'approve and revoke separately' USING ERRCODE = '55000';
        END IF;
        PERFORM eacp.assert_role(a, 'admin');
        NEW.revoked_by := a;
        NEW.revoked_at := now();
        RETURN NEW;
    END IF;

    IF NEW.approved_at IS DISTINCT FROM OLD.approved_at OR NEW.approved_by IS DISTINCT FROM OLD.approved_by THEN
        IF OLD.approved_at IS NOT NULL THEN
            RAISE EXCEPTION 'grant already approved' USING ERRCODE = '55000';
        END IF;
        IF OLD.revoked_at IS NOT NULL THEN
            RAISE EXCEPTION 'grant is revoked' USING ERRCODE = '55000';
        END IF;
        IF NEW.approved_at IS NULL THEN
            RAISE EXCEPTION 'approval requires approved_at' USING ERRCODE = '23514';
        END IF;
        PERFORM eacp.assert_role(a, 'admin');
        PERFORM eacp.assert_distinct(a, OLD.proposed_by, 'the proposer');
        PERFORM eacp.assert_distinct(a, OLD.principal_id, 'the grantee');
        -- Re-check at approval time: the grantee may have been disabled.
        PERFORM 1 FROM eacp.principals
        WHERE tenant_id = NEW.tenant_id AND id = NEW.principal_id AND disabled_at IS NULL FOR SHARE;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'principal % is disabled', NEW.principal_id USING ERRCODE = '55000';
        END IF;
        NEW.approved_by := a;
        NEW.approved_at := now();
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.agents_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.actor();
BEGIN
    -- Agent Studio (ADR-033): a studio_author writes only inside eacp.studio_save.
    IF eacp.in_studio_save() THEN
        PERFORM eacp.assert_role(a, 'studio_author');
    ELSE
        PERFORM eacp.assert_role(a, 'registry_editor');
    END IF;
    IF NEW.owner_principal_id IS NOT NULL THEN
        PERFORM 1 FROM eacp.principals
        WHERE tenant_id = NEW.tenant_id AND id = NEW.owner_principal_id AND disabled_at IS NULL FOR SHARE;
        IF NOT FOUND AND EXISTS (SELECT 1 FROM eacp.principals
                                 WHERE tenant_id = NEW.tenant_id AND id = NEW.owner_principal_id) THEN
            RAISE EXCEPTION 'owner % is disabled', NEW.owner_principal_id USING ERRCODE = '55000';
        END IF;
    END IF;
    NEW.created_by := a;
    NEW.created_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.agent_versions_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid;
    author uuid;
    move text;
BEGIN
    IF TG_OP = 'UPDATE' AND eacp.current_actor_id() IS NULL AND eacp.current_agent_version_id() IS NULL
       AND eacp.current_system_actor() = 'release' THEN
        IF OLD.state <> 'ACTIVE' OR NEW.state <> 'SUSPENDED'
           OR NEW.active_allowlist_id IS DISTINCT FROM OLD.active_allowlist_id
           OR NOT EXISTS (SELECT 1 FROM eacp.agent_releases
                          WHERE tenant_id = OLD.tenant_id AND candidate_version_id = OLD.id
                            AND state = 'ROLLED_BACK' AND changed_by IS NULL
                            AND closed_xact = pg_current_xact_id()) THEN
            RAISE EXCEPTION 'the release evaluator only suspends the candidate it rolled back' USING ERRCODE = '42501';
        END IF;
        PERFORM eacp.require_reason(NEW.state_reason, 'a lifecycle transition');
        NEW.quarantined_by := OLD.quarantined_by;
        NEW.state_changed_by := NULL;
        NEW.state_changed_at := now();
        RETURN NEW;
    END IF;
    a := eacp.actor();

    IF TG_OP = 'INSERT' THEN
        -- Agent Studio (ADR-033): a Studio agent's versions come only from
        -- eacp.studio_save, which alone lets a studio_author in.
        IF eacp.in_studio_save() THEN
            PERFORM eacp.assert_role(a, 'studio_author');
        ELSE
            PERFORM eacp.assert_role(a, 'registry_editor');
            IF EXISTS (SELECT 1 FROM eacp.studio_agents WHERE tenant_id = NEW.tenant_id AND id = NEW.agent_id) THEN
                RAISE EXCEPTION 'a Studio agent''s versions are saved in Studio' USING ERRCODE = '42501';
            END IF;
        END IF;
        IF NEW.state <> 'REGISTERED' OR NEW.active_allowlist_id IS NOT NULL THEN
            RAISE EXCEPTION 'a version is registered in state REGISTERED without an allowlist' USING ERRCODE = '55000';
        END IF;
        -- Serialise version numbering per agent (agents are otherwise immutable).
        PERFORM pg_advisory_xact_lock(hashtextextended(NEW.tenant_id::text || '/' || NEW.agent_id::text, 0));
        SELECT COALESCE(max(version), 0) + 1 INTO NEW.version
        FROM eacp.agent_versions WHERE tenant_id = NEW.tenant_id AND agent_id = NEW.agent_id;
        NEW.created_by := a;
        NEW.created_at := now();
        NEW.state_changed_by := NULL;
        NEW.state_changed_at := NULL;
        NEW.state_reason := NULL;
        NEW.quarantined_by := NULL;
        NEW.allowlist_changed_by := NULL;
        NEW.allowlist_changed_at := NULL;
        RETURN NEW;
    END IF;

    IF OLD.state IN ('RETIRED', 'REVOKED') THEN
        RAISE EXCEPTION 'version % is % (terminal)', OLD.id, OLD.state USING ERRCODE = '55000';
    END IF;
    IF NEW.state IS DISTINCT FROM OLD.state AND NEW.active_allowlist_id IS DISTINCT FROM OLD.active_allowlist_id THEN
        RAISE EXCEPTION 'change the lifecycle state and the allowlist in separate updates' USING ERRCODE = '55000';
    END IF;

    -- Allowlist pointer (two-person: activator is not the allowlist author).
    IF NEW.active_allowlist_id IS DISTINCT FROM OLD.active_allowlist_id THEN
        IF NEW.active_allowlist_id IS NULL THEN
            RAISE EXCEPTION 'the active allowlist cannot be cleared; suspend the version instead' USING ERRCODE = '55000';
        END IF;
        SELECT created_by INTO author FROM eacp.agent_allowlists
        WHERE tenant_id = NEW.tenant_id AND id = NEW.active_allowlist_id AND agent_version_id = NEW.id;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'allowlist % does not belong to version %', NEW.active_allowlist_id, NEW.id USING ERRCODE = '55000';
        END IF;
        PERFORM eacp.assert_role(a, 'registry_approver');
        PERFORM eacp.assert_distinct(a, author, 'the allowlist author');
        PERFORM eacp.studio_assert_approved(NEW.tenant_id, NEW.id);
        NEW.allowlist_changed_by := a;
        NEW.allowlist_changed_at := now();
        RETURN NEW;
    END IF;

    IF NEW.state IS NOT DISTINCT FROM OLD.state THEN
        IF NEW.state_reason IS DISTINCT FROM OLD.state_reason THEN
            RAISE EXCEPTION 'a reason is recorded only with a transition' USING ERRCODE = '55000';
        END IF;
        RETURN NEW;
    END IF;

    -- Lifecycle transition table (ADR-003 §2).
    move := CASE
        WHEN OLD.state IN ('REGISTERED', 'SUSPENDED') AND NEW.state = 'ACTIVE' THEN 'grant'
        WHEN OLD.state = 'ACTIVE' AND NEW.state = 'SUSPENDED' THEN 'contain'
        WHEN OLD.state IN ('REGISTERED', 'ACTIVE', 'SUSPENDED') AND NEW.state = 'QUARANTINED' THEN 'quarantine'
        WHEN OLD.state = 'QUARANTINED' AND NEW.state = 'SUSPENDED' THEN 'release'
        WHEN OLD.state IN ('REGISTERED', 'ACTIVE', 'SUSPENDED') AND NEW.state = 'RETIRED' THEN 'retire'
        WHEN NEW.state = 'REVOKED' THEN 'contain'
    END;
    IF move IS NULL THEN
        RAISE EXCEPTION 'illegal transition % -> %', OLD.state, NEW.state USING ERRCODE = '55000';
    END IF;
    PERFORM eacp.require_reason(NEW.state_reason, 'a lifecycle transition');

    CASE move
    WHEN 'grant' THEN
        IF OLD.active_allowlist_id IS NULL THEN
            RAISE EXCEPTION 'version % has no active allowlist', OLD.id USING ERRCODE = '55000';
        END IF;
        PERFORM eacp.assert_role(a, 'registry_approver');
        PERFORM eacp.assert_distinct(a, OLD.created_by, 'the version creator');
        SELECT created_by INTO author FROM eacp.agent_allowlists
        WHERE tenant_id = OLD.tenant_id AND id = OLD.active_allowlist_id;
        PERFORM eacp.assert_distinct(a, author, 'the allowlist author');
        PERFORM eacp.studio_assert_approved(OLD.tenant_id, OLD.id);
    WHEN 'contain', 'quarantine' THEN
        PERFORM eacp.assert_role(a, 'operator', 'registry_approver');
    WHEN 'release' THEN
        PERFORM eacp.assert_role(a, 'registry_approver');
        PERFORM eacp.assert_distinct(a, OLD.quarantined_by, 'the principal who quarantined it');
    WHEN 'retire' THEN
        PERFORM eacp.assert_role(a, 'registry_approver');
    END CASE;

    IF NEW.state = 'QUARANTINED' THEN
        NEW.quarantined_by := a;
    ELSE
        NEW.quarantined_by := OLD.quarantined_by;
    END IF;
    NEW.state_changed_by := a;
    NEW.state_changed_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.agent_allowlists_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.actor();
    v_state text;
    known integer;
BEGIN
    -- Agent Studio (ADR-033): a Studio version's allowlist is its derived
    -- capability, written only by eacp.studio_save.
    IF eacp.in_studio_save() THEN
        PERFORM eacp.assert_role(a, 'studio_author');
    ELSE
        PERFORM eacp.assert_role(a, 'registry_editor', 'registry_approver');
        IF EXISTS (SELECT 1 FROM eacp.studio_versions WHERE tenant_id = NEW.tenant_id AND id = NEW.agent_version_id) THEN
            RAISE EXCEPTION 'a Studio version''s allowlist is its derived capability' USING ERRCODE = '42501';
        END IF;
    END IF;
    SELECT state INTO v_state FROM eacp.agent_versions
    WHERE tenant_id = NEW.tenant_id AND id = NEW.agent_version_id FOR SHARE;
    IF v_state IN ('RETIRED', 'REVOKED') THEN
        RAISE EXCEPTION 'version % is % (terminal)', NEW.agent_version_id, v_state USING ERRCODE = '55000';
    END IF;
    IF array_position(NEW.tool_ids, NULL) IS NOT NULL THEN
        RAISE EXCEPTION 'allowlist contains a null tool id' USING ERRCODE = '23514';
    END IF;
    NEW.tool_ids := ARRAY(SELECT DISTINCT x FROM unnest(NEW.tool_ids) x ORDER BY x);
    SELECT count(*) INTO known FROM eacp.tools
    WHERE tenant_id = NEW.tenant_id AND id = ANY (NEW.tool_ids);
    IF known <> cardinality(NEW.tool_ids) THEN
        RAISE EXCEPTION 'allowlist references unknown tools' USING ERRCODE = '23503';
    END IF;
    -- Models are granted exactly as tools are (ADR-031).
    IF array_position(NEW.model_ids, NULL) IS NOT NULL THEN
        RAISE EXCEPTION 'allowlist contains a null model id' USING ERRCODE = '23514';
    END IF;
    NEW.model_ids := ARRAY(SELECT DISTINCT x FROM unnest(NEW.model_ids) x ORDER BY x);
    SELECT count(*) INTO known FROM eacp.llm_models
    WHERE tenant_id = NEW.tenant_id AND id = ANY (NEW.model_ids);
    IF known <> cardinality(NEW.model_ids) THEN
        RAISE EXCEPTION 'allowlist references unknown models' USING ERRCODE = '23503';
    END IF;
    NEW.created_by := a;
    NEW.created_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.credentials_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.actor();
    v_state text;
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.kind = 'pk' THEN
            PERFORM eacp.assert_role(a, 'admin');
        ELSE
            -- ADR-033 §1: the Studio runtime proposes a key only for a Studio
            -- version whose capability request is approved.
            IF a IS NOT NULL AND eacp.holds_role(a, 'studio_runtime') THEN
                PERFORM 1 FROM eacp.studio_versions s
                JOIN eacp.agent_versions v ON v.tenant_id = s.tenant_id AND v.id = s.id
                WHERE s.tenant_id = NEW.tenant_id AND s.id = NEW.agent_version_id AND s.decision = 'approved'
                  AND v.active_allowlist_id IS NOT NULL
                FOR SHARE OF v;
                IF NOT FOUND THEN
                    RAISE EXCEPTION 'the Studio runtime proposes keys only for approved Studio versions'
                        USING ERRCODE = '42501';
                END IF;
            ELSE
                PERFORM eacp.assert_role(a, 'registry_editor', 'registry_approver');
            END IF;
            SELECT state INTO v_state FROM eacp.agent_versions
            WHERE tenant_id = NEW.tenant_id AND id = NEW.agent_version_id FOR SHARE;
            IF v_state IN ('RETIRED', 'REVOKED') THEN
                RAISE EXCEPTION 'version % is % (terminal)', NEW.agent_version_id, v_state USING ERRCODE = '55000';
            END IF;
        END IF;
        IF a IS NOT NULL AND (NEW.approved_at IS NOT NULL OR NEW.approved_by IS NOT NULL) THEN
            RAISE EXCEPTION 'a credential must be approved by a second person' USING ERRCODE = '42501';
        END IF;
        IF NEW.revoked_at IS NOT NULL OR NEW.revoked_by IS NOT NULL OR NEW.revoke_reason IS NOT NULL THEN
            RAISE EXCEPTION 'a credential cannot be created revoked' USING ERRCODE = '23514';
        END IF;
        IF NEW.expires_at <= now() OR NEW.expires_at > now() + interval '90 days' THEN
            RAISE EXCEPTION 'credential expiry must be in the future and at most 90 days away' USING ERRCODE = '23514';
        END IF;
        NEW.proposed_by := a;
        NEW.proposed_at := now();
        IF NEW.approved_at IS NOT NULL THEN  -- bootstrap only
            NEW.approved_at := now();
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.kind IS DISTINCT FROM OLD.kind OR NEW.principal_id IS DISTINCT FROM OLD.principal_id
       OR NEW.agent_version_id IS DISTINCT FROM OLD.agent_version_id OR NEW.secret_hash IS DISTINCT FROM OLD.secret_hash
       OR NEW.expires_at IS DISTINCT FROM OLD.expires_at OR NEW.proposed_by IS DISTINCT FROM OLD.proposed_by THEN
        RAISE EXCEPTION 'a credential''s subject, hash and expiry are immutable' USING ERRCODE = '55000';
    END IF;

    IF eacp.set_once(OLD.revoked_at, NEW.revoked_at, OLD.revoke_reason, NEW.revoke_reason, 'credential revocation') THEN
        IF NEW.approved_at IS DISTINCT FROM OLD.approved_at THEN
            RAISE EXCEPTION 'approve and revoke separately' USING ERRCODE = '55000';
        END IF;
        IF NEW.kind = 'pk' THEN
            PERFORM eacp.assert_role(a, 'admin');
        ELSE
            PERFORM eacp.assert_role(a, 'registry_approver', 'operator');
        END IF;
        NEW.revoked_by := a;
        NEW.revoked_at := now();
        RETURN NEW;
    END IF;

    IF NEW.approved_at IS DISTINCT FROM OLD.approved_at OR NEW.approved_by IS DISTINCT FROM OLD.approved_by THEN
        IF OLD.approved_at IS NOT NULL THEN
            RAISE EXCEPTION 'credential already approved' USING ERRCODE = '55000';
        END IF;
        IF OLD.revoked_at IS NOT NULL OR OLD.expires_at <= now() THEN
            RAISE EXCEPTION 'credential is revoked or expired' USING ERRCODE = '55000';
        END IF;
        IF NEW.approved_at IS NULL THEN
            RAISE EXCEPTION 'approval requires approved_at' USING ERRCODE = '23514';
        END IF;
        IF NEW.kind = 'pk' THEN
            PERFORM eacp.assert_role(a, 'admin');
            PERFORM eacp.assert_distinct(a, OLD.principal_id, 'the credential holder');
        ELSE
            PERFORM eacp.assert_role(a, 'registry_approver');
        END IF;
        PERFORM eacp.assert_distinct(a, OLD.proposed_by, 'the proposer');
        NEW.approved_by := a;
        NEW.approved_at := now();
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.role_grants_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.actor();
    grantee eacp.principals%ROWTYPE;
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM eacp.assert_role(a, 'admin');
        PERFORM eacp.assert_distinct(a, NEW.principal_id, 'the grantee');
        SELECT * INTO grantee FROM eacp.principals
        WHERE tenant_id = NEW.tenant_id AND id = NEW.principal_id FOR SHARE;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'unknown principal %', NEW.principal_id USING ERRCODE = '23503';
        END IF;
        IF grantee.disabled_at IS NOT NULL THEN
            RAISE EXCEPTION 'principal % is disabled', NEW.principal_id USING ERRCODE = '55000';
        END IF;
        -- Slice A: a service principal may only read the audit journal. A
        -- human who controls a service principal must not be able to author
        -- as the service and approve as themselves (ADR-003 §1).
        IF grantee.kind = 'service' AND NEW.role <> 'auditor' THEN
            RAISE EXCEPTION 'role % is human-only', NEW.role USING ERRCODE = '42501';
        END IF;
        IF a IS NOT NULL AND (NEW.approved_at IS NOT NULL OR NEW.approved_by IS NOT NULL) THEN
            RAISE EXCEPTION 'a grant must be approved by a second admin' USING ERRCODE = '42501';
        END IF;
        IF NEW.revoked_at IS NOT NULL OR NEW.revoked_by IS NOT NULL OR NEW.revoke_reason IS NOT NULL THEN
            RAISE EXCEPTION 'a grant cannot be created revoked' USING ERRCODE = '23514';
        END IF;
        NEW.proposed_by := a;
        NEW.proposed_at := now();
        IF NEW.approved_at IS NOT NULL THEN  -- bootstrap only
            NEW.approved_at := now();
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.principal_id IS DISTINCT FROM OLD.principal_id OR NEW.role IS DISTINCT FROM OLD.role
       OR NEW.proposed_by IS DISTINCT FROM OLD.proposed_by OR NEW.proposed_at IS DISTINCT FROM OLD.proposed_at THEN
        RAISE EXCEPTION 'a grant''s subject is immutable' USING ERRCODE = '55000';
    END IF;

    IF eacp.set_once(OLD.revoked_at, NEW.revoked_at, OLD.revoke_reason, NEW.revoke_reason, 'revocation') THEN
        IF NEW.approved_at IS DISTINCT FROM OLD.approved_at THEN
            RAISE EXCEPTION 'approve and revoke separately' USING ERRCODE = '55000';
        END IF;
        PERFORM eacp.assert_role(a, 'admin');
        NEW.revoked_by := a;
        NEW.revoked_at := now();
        RETURN NEW;
    END IF;

    IF NEW.approved_at IS DISTINCT FROM OLD.approved_at OR NEW.approved_by IS DISTINCT FROM OLD.approved_by THEN
        IF OLD.approved_at IS NOT NULL THEN
            RAISE EXCEPTION 'grant already approved' USING ERRCODE = '55000';
        END IF;
        IF OLD.revoked_at IS NOT NULL THEN
            RAISE EXCEPTION 'grant is revoked' USING ERRCODE = '55000';
        END IF;
        IF NEW.approved_at IS NULL THEN
            RAISE EXCEPTION 'approval requires approved_at' USING ERRCODE = '23514';
        END IF;
        PERFORM eacp.assert_role(a, 'admin');
        PERFORM eacp.assert_distinct(a, OLD.proposed_by, 'the proposer');
        PERFORM eacp.assert_distinct(a, OLD.principal_id, 'the grantee');
        -- Re-check at approval time: the grantee may have been disabled.
        PERFORM 1 FROM eacp.principals
        WHERE tenant_id = NEW.tenant_id AND id = NEW.principal_id AND disabled_at IS NULL FOR SHARE;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'principal % is disabled', NEW.principal_id USING ERRCODE = '55000';
        END IF;
        NEW.approved_by := a;
        NEW.approved_at := now();
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.agents_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.actor();
BEGIN
    PERFORM eacp.assert_role(a, 'registry_editor');
    IF NEW.owner_principal_id IS NOT NULL THEN
        PERFORM 1 FROM eacp.principals
        WHERE tenant_id = NEW.tenant_id AND id = NEW.owner_principal_id AND disabled_at IS NULL FOR SHARE;
        IF NOT FOUND AND EXISTS (SELECT 1 FROM eacp.principals
                                 WHERE tenant_id = NEW.tenant_id AND id = NEW.owner_principal_id) THEN
            RAISE EXCEPTION 'owner % is disabled', NEW.owner_principal_id USING ERRCODE = '55000';
        END IF;
    END IF;
    NEW.created_by := a;
    NEW.created_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.agent_versions_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid;
    author uuid;
    move text;
BEGIN
    IF TG_OP = 'UPDATE' AND eacp.current_actor_id() IS NULL AND eacp.current_agent_version_id() IS NULL
       AND eacp.current_system_actor() = 'release' THEN
        IF OLD.state <> 'ACTIVE' OR NEW.state <> 'SUSPENDED'
           OR NEW.active_allowlist_id IS DISTINCT FROM OLD.active_allowlist_id
           OR NOT EXISTS (SELECT 1 FROM eacp.agent_releases
                          WHERE tenant_id = OLD.tenant_id AND candidate_version_id = OLD.id
                            AND state = 'ROLLED_BACK' AND changed_by IS NULL
                            AND closed_xact = pg_current_xact_id()) THEN
            RAISE EXCEPTION 'the release evaluator only suspends the candidate it rolled back' USING ERRCODE = '42501';
        END IF;
        PERFORM eacp.require_reason(NEW.state_reason, 'a lifecycle transition');
        NEW.quarantined_by := OLD.quarantined_by;
        NEW.state_changed_by := NULL;
        NEW.state_changed_at := now();
        RETURN NEW;
    END IF;
    a := eacp.actor();

    IF TG_OP = 'INSERT' THEN
        PERFORM eacp.assert_role(a, 'registry_editor');
        IF NEW.state <> 'REGISTERED' OR NEW.active_allowlist_id IS NOT NULL THEN
            RAISE EXCEPTION 'a version is registered in state REGISTERED without an allowlist' USING ERRCODE = '55000';
        END IF;
        -- Serialise version numbering per agent (agents are otherwise immutable).
        PERFORM pg_advisory_xact_lock(hashtextextended(NEW.tenant_id::text || '/' || NEW.agent_id::text, 0));
        SELECT COALESCE(max(version), 0) + 1 INTO NEW.version
        FROM eacp.agent_versions WHERE tenant_id = NEW.tenant_id AND agent_id = NEW.agent_id;
        NEW.created_by := a;
        NEW.created_at := now();
        NEW.state_changed_by := NULL;
        NEW.state_changed_at := NULL;
        NEW.state_reason := NULL;
        NEW.quarantined_by := NULL;
        NEW.allowlist_changed_by := NULL;
        NEW.allowlist_changed_at := NULL;
        RETURN NEW;
    END IF;

    IF OLD.state IN ('RETIRED', 'REVOKED') THEN
        RAISE EXCEPTION 'version % is % (terminal)', OLD.id, OLD.state USING ERRCODE = '55000';
    END IF;
    IF NEW.state IS DISTINCT FROM OLD.state AND NEW.active_allowlist_id IS DISTINCT FROM OLD.active_allowlist_id THEN
        RAISE EXCEPTION 'change the lifecycle state and the allowlist in separate updates' USING ERRCODE = '55000';
    END IF;

    -- Allowlist pointer (two-person: activator is not the allowlist author).
    IF NEW.active_allowlist_id IS DISTINCT FROM OLD.active_allowlist_id THEN
        IF NEW.active_allowlist_id IS NULL THEN
            RAISE EXCEPTION 'the active allowlist cannot be cleared; suspend the version instead' USING ERRCODE = '55000';
        END IF;
        SELECT created_by INTO author FROM eacp.agent_allowlists
        WHERE tenant_id = NEW.tenant_id AND id = NEW.active_allowlist_id AND agent_version_id = NEW.id;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'allowlist % does not belong to version %', NEW.active_allowlist_id, NEW.id USING ERRCODE = '55000';
        END IF;
        PERFORM eacp.assert_role(a, 'registry_approver');
        PERFORM eacp.assert_distinct(a, author, 'the allowlist author');
        NEW.allowlist_changed_by := a;
        NEW.allowlist_changed_at := now();
        RETURN NEW;
    END IF;

    IF NEW.state IS NOT DISTINCT FROM OLD.state THEN
        IF NEW.state_reason IS DISTINCT FROM OLD.state_reason THEN
            RAISE EXCEPTION 'a reason is recorded only with a transition' USING ERRCODE = '55000';
        END IF;
        RETURN NEW;
    END IF;

    -- Lifecycle transition table (ADR-003 §2).
    move := CASE
        WHEN OLD.state IN ('REGISTERED', 'SUSPENDED') AND NEW.state = 'ACTIVE' THEN 'grant'
        WHEN OLD.state = 'ACTIVE' AND NEW.state = 'SUSPENDED' THEN 'contain'
        WHEN OLD.state IN ('REGISTERED', 'ACTIVE', 'SUSPENDED') AND NEW.state = 'QUARANTINED' THEN 'quarantine'
        WHEN OLD.state = 'QUARANTINED' AND NEW.state = 'SUSPENDED' THEN 'release'
        WHEN OLD.state IN ('REGISTERED', 'ACTIVE', 'SUSPENDED') AND NEW.state = 'RETIRED' THEN 'retire'
        WHEN NEW.state = 'REVOKED' THEN 'contain'
    END;
    IF move IS NULL THEN
        RAISE EXCEPTION 'illegal transition % -> %', OLD.state, NEW.state USING ERRCODE = '55000';
    END IF;
    PERFORM eacp.require_reason(NEW.state_reason, 'a lifecycle transition');

    CASE move
    WHEN 'grant' THEN
        IF OLD.active_allowlist_id IS NULL THEN
            RAISE EXCEPTION 'version % has no active allowlist', OLD.id USING ERRCODE = '55000';
        END IF;
        PERFORM eacp.assert_role(a, 'registry_approver');
        PERFORM eacp.assert_distinct(a, OLD.created_by, 'the version creator');
        SELECT created_by INTO author FROM eacp.agent_allowlists
        WHERE tenant_id = OLD.tenant_id AND id = OLD.active_allowlist_id;
        PERFORM eacp.assert_distinct(a, author, 'the allowlist author');
    WHEN 'contain', 'quarantine' THEN
        PERFORM eacp.assert_role(a, 'operator', 'registry_approver');
    WHEN 'release' THEN
        PERFORM eacp.assert_role(a, 'registry_approver');
        PERFORM eacp.assert_distinct(a, OLD.quarantined_by, 'the principal who quarantined it');
    WHEN 'retire' THEN
        PERFORM eacp.assert_role(a, 'registry_approver');
    END CASE;

    IF NEW.state = 'QUARANTINED' THEN
        NEW.quarantined_by := a;
    ELSE
        NEW.quarantined_by := OLD.quarantined_by;
    END IF;
    NEW.state_changed_by := a;
    NEW.state_changed_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.agent_allowlists_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.actor();
    v_state text;
    known integer;
BEGIN
    PERFORM eacp.assert_role(a, 'registry_editor', 'registry_approver');
    SELECT state INTO v_state FROM eacp.agent_versions
    WHERE tenant_id = NEW.tenant_id AND id = NEW.agent_version_id FOR SHARE;
    IF v_state IN ('RETIRED', 'REVOKED') THEN
        RAISE EXCEPTION 'version % is % (terminal)', NEW.agent_version_id, v_state USING ERRCODE = '55000';
    END IF;
    IF array_position(NEW.tool_ids, NULL) IS NOT NULL THEN
        RAISE EXCEPTION 'allowlist contains a null tool id' USING ERRCODE = '23514';
    END IF;
    NEW.tool_ids := ARRAY(SELECT DISTINCT x FROM unnest(NEW.tool_ids) x ORDER BY x);
    SELECT count(*) INTO known FROM eacp.tools
    WHERE tenant_id = NEW.tenant_id AND id = ANY (NEW.tool_ids);
    IF known <> cardinality(NEW.tool_ids) THEN
        RAISE EXCEPTION 'allowlist references unknown tools' USING ERRCODE = '23503';
    END IF;
    -- Models are granted exactly as tools are (ADR-031).
    IF array_position(NEW.model_ids, NULL) IS NOT NULL THEN
        RAISE EXCEPTION 'allowlist contains a null model id' USING ERRCODE = '23514';
    END IF;
    NEW.model_ids := ARRAY(SELECT DISTINCT x FROM unnest(NEW.model_ids) x ORDER BY x);
    SELECT count(*) INTO known FROM eacp.llm_models
    WHERE tenant_id = NEW.tenant_id AND id = ANY (NEW.model_ids);
    IF known <> cardinality(NEW.model_ids) THEN
        RAISE EXCEPTION 'allowlist references unknown models' USING ERRCODE = '23503';
    END IF;
    NEW.created_by := a;
    NEW.created_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.credentials_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.actor();
    v_state text;
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.kind = 'pk' THEN
            PERFORM eacp.assert_role(a, 'admin');
        ELSE
            PERFORM eacp.assert_role(a, 'registry_editor', 'registry_approver');
            SELECT state INTO v_state FROM eacp.agent_versions
            WHERE tenant_id = NEW.tenant_id AND id = NEW.agent_version_id FOR SHARE;
            IF v_state IN ('RETIRED', 'REVOKED') THEN
                RAISE EXCEPTION 'version % is % (terminal)', NEW.agent_version_id, v_state USING ERRCODE = '55000';
            END IF;
        END IF;
        IF a IS NOT NULL AND (NEW.approved_at IS NOT NULL OR NEW.approved_by IS NOT NULL) THEN
            RAISE EXCEPTION 'a credential must be approved by a second person' USING ERRCODE = '42501';
        END IF;
        IF NEW.revoked_at IS NOT NULL OR NEW.revoked_by IS NOT NULL OR NEW.revoke_reason IS NOT NULL THEN
            RAISE EXCEPTION 'a credential cannot be created revoked' USING ERRCODE = '23514';
        END IF;
        IF NEW.expires_at <= now() OR NEW.expires_at > now() + interval '90 days' THEN
            RAISE EXCEPTION 'credential expiry must be in the future and at most 90 days away' USING ERRCODE = '23514';
        END IF;
        NEW.proposed_by := a;
        NEW.proposed_at := now();
        IF NEW.approved_at IS NOT NULL THEN  -- bootstrap only
            NEW.approved_at := now();
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.kind IS DISTINCT FROM OLD.kind OR NEW.principal_id IS DISTINCT FROM OLD.principal_id
       OR NEW.agent_version_id IS DISTINCT FROM OLD.agent_version_id OR NEW.secret_hash IS DISTINCT FROM OLD.secret_hash
       OR NEW.expires_at IS DISTINCT FROM OLD.expires_at OR NEW.proposed_by IS DISTINCT FROM OLD.proposed_by THEN
        RAISE EXCEPTION 'a credential''s subject, hash and expiry are immutable' USING ERRCODE = '55000';
    END IF;

    IF eacp.set_once(OLD.revoked_at, NEW.revoked_at, OLD.revoke_reason, NEW.revoke_reason, 'credential revocation') THEN
        IF NEW.approved_at IS DISTINCT FROM OLD.approved_at THEN
            RAISE EXCEPTION 'approve and revoke separately' USING ERRCODE = '55000';
        END IF;
        IF NEW.kind = 'pk' THEN
            PERFORM eacp.assert_role(a, 'admin');
        ELSE
            PERFORM eacp.assert_role(a, 'registry_approver', 'operator');
        END IF;
        NEW.revoked_by := a;
        NEW.revoked_at := now();
        RETURN NEW;
    END IF;

    IF NEW.approved_at IS DISTINCT FROM OLD.approved_at OR NEW.approved_by IS DISTINCT FROM OLD.approved_by THEN
        IF OLD.approved_at IS NOT NULL THEN
            RAISE EXCEPTION 'credential already approved' USING ERRCODE = '55000';
        END IF;
        IF OLD.revoked_at IS NOT NULL OR OLD.expires_at <= now() THEN
            RAISE EXCEPTION 'credential is revoked or expired' USING ERRCODE = '55000';
        END IF;
        IF NEW.approved_at IS NULL THEN
            RAISE EXCEPTION 'approval requires approved_at' USING ERRCODE = '23514';
        END IF;
        IF NEW.kind = 'pk' THEN
            PERFORM eacp.assert_role(a, 'admin');
            PERFORM eacp.assert_distinct(a, OLD.principal_id, 'the credential holder');
        ELSE
            PERFORM eacp.assert_role(a, 'registry_approver');
        END IF;
        PERFORM eacp.assert_distinct(a, OLD.proposed_by, 'the proposer');
        NEW.approved_by := a;
        NEW.approved_at := now();
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

DROP FUNCTION eacp.studio_decide(uuid, boolean, text);
DROP FUNCTION eacp.studio_save(uuid, text, text, text, uuid, text);
DROP FUNCTION eacp.studio_definition_capability(text);
DROP FUNCTION eacp.studio_placeholders(text[], text[], text[]);
DROP FUNCTION eacp.studio_int(jsonb, integer, integer);
DROP FUNCTION eacp.studio_assert_approved(uuid, uuid);
DROP FUNCTION eacp.in_studio_save();
DROP TABLE eacp.studio_versions;
DROP TABLE eacp.studio_agents;
DROP TABLE eacp.studio_save_marks;
DROP FUNCTION eacp.studio_versions_guard();
DROP FUNCTION eacp.studio_agents_guard();
DROP FUNCTION eacp.holds_role(uuid, text);
ALTER TABLE eacp.role_grants DROP CONSTRAINT role_grants_role_check;
ALTER TABLE eacp.role_grants ADD CONSTRAINT role_grants_role_check CHECK (role IN
    ('admin', 'registry_editor', 'registry_approver', 'operator', 'approver', 'auditor'));
