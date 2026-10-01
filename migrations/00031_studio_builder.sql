-- Phase 27c: bounded Studio graphs and derived model capabilities (ADR-033 Rev 1.4).
-- +goose Up

ALTER TABLE eacp.studio_versions ADD COLUMN model_capability uuid[] NOT NULL DEFAULT '{}';

-- +goose StatementBegin
CREATE FUNCTION eacp.studio_definition_v1(p_definition text) RETURNS text[]
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

-- +goose StatementBegin
-- Closed schemas cannot resolve references or run arbitrary expressions.
CREATE FUNCTION eacp.studio_output_schema_valid(s jsonb, depth integer DEFAULT 0) RETURNS boolean
LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE k text; item jsonb; typ text; props jsonb; req jsonb;
BEGIN
    IF depth > 4 OR jsonb_typeof(s) IS DISTINCT FROM 'object' THEN RETURN false; END IF;
    typ := s->>'type';
    IF typ IS NULL OR typ NOT IN ('object','array','string','number','integer','boolean') THEN RETURN false; END IF;
    IF EXISTS (SELECT 1 FROM jsonb_object_keys(s) x WHERE x NOT IN
        ('type','properties','required','additionalProperties','items','enum','minLength','maxLength',
         'minimum','maximum','minItems','maxItems')) THEN RETURN false; END IF;
    FOR k IN SELECT jsonb_object_keys(s) LOOP
        IF (k IN ('properties','required','additionalProperties') AND typ<>'object')
        OR (k IN ('items','minItems','maxItems') AND typ<>'array')
        OR (k IN ('minLength','maxLength') AND typ<>'string')
        OR (k IN ('minimum','maximum') AND typ NOT IN ('number','integer')) THEN RETURN false; END IF;
    END LOOP;
    IF typ='object' THEN
        props := s->'properties'; req := COALESCE(s->'required','[]'::jsonb);
        IF s->'additionalProperties' IS DISTINCT FROM 'false'::jsonb
           OR jsonb_typeof(props) IS DISTINCT FROM 'object' OR jsonb_typeof(req) IS DISTINCT FROM 'array'
           OR (SELECT count(*) FROM jsonb_object_keys(props))>20 THEN RETURN false; END IF;
        FOR k,item IN SELECT * FROM jsonb_each(props) LOOP
            IF k !~ '^[A-Za-z_][A-Za-z0-9_]{0,63}$' OR NOT eacp.studio_output_schema_valid(item,depth+1) THEN RETURN false; END IF;
        END LOOP;
        IF EXISTS (SELECT 1 FROM jsonb_array_elements(req) v WHERE jsonb_typeof(v)<>'string' OR NOT props ? (v#>>'{}'))
        OR (SELECT count(*) FROM jsonb_array_elements(req))<>(SELECT count(DISTINCT v) FROM jsonb_array_elements(req) v)
        THEN RETURN false; END IF;
    ELSIF typ='array' THEN
        IF NOT eacp.studio_int(s->'maxItems',0,1000) OR NOT eacp.studio_output_schema_valid(s->'items',depth+1) THEN RETURN false; END IF;
        IF s ? 'minItems' AND (NOT eacp.studio_int(s->'minItems',0,1000) OR (s->>'minItems')::integer>(s->>'maxItems')::integer) THEN RETURN false; END IF;
    ELSIF typ='string' THEN
        IF NOT eacp.studio_int(s->'maxLength',0,65536) THEN RETURN false; END IF;
        IF s ? 'minLength' AND (NOT eacp.studio_int(s->'minLength',0,65536) OR (s->>'minLength')::integer>(s->>'maxLength')::integer) THEN RETURN false; END IF;
    END IF;
    IF s ? 'minimum' AND jsonb_typeof(s->'minimum')<>'number' THEN RETURN false; END IF;
    IF s ? 'maximum' AND jsonb_typeof(s->'maximum')<>'number' THEN RETURN false; END IF;
    IF s ? 'minimum' AND s ? 'maximum' AND (s->>'minimum')::numeric>(s->>'maximum')::numeric THEN RETURN false; END IF;
    IF s ? 'enum' THEN
        IF typ IN ('object','array') OR jsonb_typeof(s->'enum') IS DISTINCT FROM 'array' THEN RETURN false; END IF;
        IF jsonb_array_length(s->'enum') NOT BETWEEN 1 AND 20 THEN RETURN false; END IF;
        FOR item IN SELECT jsonb_array_elements(s->'enum') LOOP
            IF jsonb_typeof(item)<>(CASE WHEN typ='integer' THEN 'number' ELSE typ END) THEN RETURN false; END IF;
            IF typ='integer' AND (item::text)::numeric<>trunc((item::text)::numeric) THEN RETURN false; END IF;
        END LOOP;
    END IF;
    RETURN true;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.studio_definition_capability(p_definition text) RETURNS text[]
LANGUAGE plpgsql STABLE AS $$
DECLARE d jsonb; s jsonb; prior jsonb; lim jsonb; k text; id text; n integer; dest integer;
    ids text[]:='{}'; inputs text[]:='{}'; tools text[]:='{}'; dom text[]; other text[];
    doms jsonb:='{}'; output_ids text[]:='{}'; refs text[]; strs text[]; allowed integer:=0; total integer;
    caps bigint:=0; cap integer; model eacp.llm_models%ROWTYPE;
BEGIN
    IF p_definition IS NULL OR octet_length(p_definition)>65536 OR NOT (p_definition IS JSON OBJECT WITH UNIQUE KEYS) THEN
        RAISE EXCEPTION 'definition: a unique-key JSON object of at most 65536 bytes' USING ERRCODE='23514';
    END IF;
    d:=p_definition::jsonb;
    IF d->'schema_version'='1'::jsonb THEN RETURN eacp.studio_definition_v1(p_definition); END IF;
    IF d->'schema_version' IS DISTINCT FROM '2'::jsonb OR d->'kind' IS DISTINCT FROM '"agent"'::jsonb
       OR EXISTS (SELECT 1 FROM jsonb_object_keys(d) x WHERE x NOT IN ('schema_version','kind','inputs','steps','limits')) THEN
        RAISE EXCEPTION 'definition: schema_version is 1 or 2 with known fields' USING ERRCODE='23514';
    END IF;
    IF EXISTS (SELECT 1 FROM jsonb_path_query(d,'strict $.** ? (@.type() == "string")') v
               WHERE (v#>>'{}') ~ '(eacp_[ap]k_|-----BEGIN|AKIA[0-9A-Z]{16}|Bearer |ghp_|xox[a-z]-|sk-[A-Za-z0-9_-]{20,})')
       OR EXISTS (SELECT 1 FROM jsonb_path_query(d,'strict $.** ? (@.type() == "object").keyvalue().key') v
               WHERE (v#>>'{}') ~ '(eacp_[ap]k_|-----BEGIN|AKIA[0-9A-Z]{16}|Bearer |ghp_|xox[a-z]-|sk-[A-Za-z0-9_-]{20,})') THEN
        RAISE EXCEPTION 'definition_contains_secret' USING ERRCODE='23514';
    END IF;
    IF jsonb_typeof(COALESCE(d->'inputs','{}'))<>'object' OR (SELECT count(*) FROM jsonb_object_keys(COALESCE(d->'inputs','{}')))>10 THEN
        RAISE EXCEPTION 'definition: at most 10 string inputs' USING ERRCODE='23514';
    END IF;
    FOR k,s IN SELECT * FROM jsonb_each(COALESCE(d->'inputs','{}')) LOOP
        IF k !~ '^[a-z][a-z0-9_]{0,31}$' OR jsonb_typeof(s)<>'object' OR s->>'type' IS DISTINCT FROM 'string'
        OR NOT eacp.studio_int(s->'max_length',1,1024)
        OR EXISTS (SELECT 1 FROM jsonb_object_keys(s) x WHERE x NOT IN ('type','max_length')) THEN
            RAISE EXCEPTION 'definition: invalid input' USING ERRCODE='23514';
        END IF;
        inputs:=inputs||k;
    END LOOP;
    lim:=d->'limits';
    IF jsonb_typeof(lim) IS DISTINCT FROM 'object' OR NOT eacp.studio_int(lim->'timeout_seconds',10,3600)
    OR EXISTS (SELECT 1 FROM jsonb_object_keys(lim) x WHERE x NOT IN ('timeout_seconds','max_output_tokens')) THEN
        RAISE EXCEPTION 'definition: invalid limits' USING ERRCODE='23514';
    END IF;
    IF jsonb_typeof(d->'steps') IS DISTINCT FROM 'array' OR jsonb_array_length(d->'steps') NOT BETWEEN 1 AND 20 THEN
        RAISE EXCEPTION 'definition: 1 to 20 steps' USING ERRCODE='23514';
    END IF;
    n:=jsonb_array_length(d->'steps');
    FOR i IN 0..n-1 LOOP
        s:=d->'steps'->i; id:=s->>'id';
        IF jsonb_typeof(s) IS DISTINCT FROM 'object' OR jsonb_typeof(s->'id') IS DISTINCT FROM 'string'
        OR id !~ '^[a-z][a-z0-9_]{0,31}$' OR id=ANY(ids) THEN
            RAISE EXCEPTION 'definition: unique valid step ids' USING ERRCODE='23514';
        END IF;
        ids:=ids||id;
    END LOOP;
    FOR i IN 0..n-1 LOOP
        s:=d->'steps'->i; id:=s->>'id'; strs:='{}'; dom:=NULL;
        IF i=0 THEN dom:='{}'; ELSE
            FOR j IN 0..i-1 LOOP
                prior:=d->'steps'->j;
                IF prior->>'next'=id OR prior->>'then'=id OR prior->>'else'=id THEN
                    other:=ARRAY(SELECT jsonb_array_elements_text(doms->(prior->>'id')));
                    IF dom IS NULL THEN dom:=other; ELSE dom:=ARRAY(SELECT unnest(dom) INTERSECT SELECT unnest(other)); END IF;
                END IF;
            END LOOP;
            IF dom IS NULL THEN RAISE EXCEPTION 'definition: unreachable step %',id USING ERRCODE='23514'; END IF;
        END IF;
        refs:=ARRAY(SELECT unnest(dom) INTERSECT SELECT unnest(output_ids));
        IF s->>'kind'='tool_call' THEN
            IF EXISTS (SELECT 1 FROM jsonb_object_keys(s) x WHERE x NOT IN ('id','kind','tool','tool_schema_version','operation','target','resource','payload','next')) THEN
                RAISE EXCEPTION 'definition: unknown tool field' USING ERRCODE='23514'; END IF;
            IF jsonb_typeof(s->'tool') IS DISTINCT FROM 'string' OR s->>'tool' !~ '^[a-z0-9][a-z0-9-]{1,62}\.[a-z0-9][a-z0-9_-]{0,62}$' THEN
                RAISE EXCEPTION 'definition: invalid tool' USING ERRCODE='23514'; END IF;
            PERFORM 1 FROM eacp.tools t JOIN eacp.connectors c ON c.tenant_id=t.tenant_id AND c.id=t.connector_id
            WHERE t.tenant_id=eacp.current_tenant_id() AND c.name||'.'||t.name=s->>'tool';
            IF NOT FOUND THEN RAISE EXCEPTION 'definition: unknown tool' USING ERRCODE='23514'; END IF;
            FOREACH k IN ARRAY ARRAY['tool_schema_version','operation','target','resource'] LOOP
                IF jsonb_typeof(s->k) IS DISTINCT FROM 'string' OR btrim(s->>k)='' OR length(s->>k)>128 THEN
                    RAISE EXCEPTION 'definition: invalid tool field %',k USING ERRCODE='23514'; END IF;
            END LOOP;
            IF jsonb_typeof(s->'payload') IS DISTINCT FROM 'object' THEN RAISE EXCEPTION 'definition: payload is an object' USING ERRCODE='23514'; END IF;
            strs:=ARRAY(SELECT v#>>'{}' FROM jsonb_path_query(s->'payload','strict $.** ? (@.type() == "string")') v);
            tools:=tools||(s->>'tool'); output_ids:=output_ids||id;
        ELSIF s->>'kind'='llm' THEN
            IF EXISTS (SELECT 1 FROM jsonb_object_keys(s) x WHERE x NOT IN ('id','kind','model','instruction','input','max_output_tokens','output_schema','next')) THEN
                RAISE EXCEPTION 'definition: unknown model field' USING ERRCODE='23514'; END IF;
            SELECT * INTO model FROM eacp.llm_models WHERE tenant_id=eacp.current_tenant_id() AND name=s->>'model';
            IF NOT FOUND OR jsonb_typeof(s->'model') IS DISTINCT FROM 'string' THEN RAISE EXCEPTION 'definition: unknown model' USING ERRCODE='23514'; END IF;
            IF jsonb_typeof(s->'instruction') IS DISTINCT FROM 'string' OR length(s->>'instruction')>4096
            OR jsonb_typeof(s->'input') IS DISTINCT FROM 'object' OR NOT eacp.studio_int(s->'max_output_tokens',1,model.max_output_tokens::integer)
            OR s->'output_schema'->>'type' IS DISTINCT FROM 'object' OR NOT eacp.studio_output_schema_valid(s->'output_schema') THEN
                RAISE EXCEPTION 'definition: invalid model step or output schema' USING ERRCODE='23514'; END IF;
            caps:=caps+(s->>'max_output_tokens')::integer;
            strs:=ARRAY[s->>'instruction']||ARRAY(SELECT v#>>'{}' FROM jsonb_path_query(s->'input','strict $.** ? (@.type() == "string")') v);
            output_ids:=output_ids||id;
        ELSIF s->>'kind'='branch' THEN
            IF EXISTS (SELECT 1 FROM jsonb_object_keys(s) x WHERE x NOT IN ('id','kind','condition','then','else'))
            OR jsonb_typeof(s->'condition') IS DISTINCT FROM 'object'
            OR EXISTS (SELECT 1 FROM jsonb_object_keys(s->'condition') x WHERE x NOT IN ('left','operator','right'))
            OR s->'condition'->>'operator' IS NULL OR s->'condition'->>'operator' NOT IN ('eq','ne','lt','le','gt','ge') THEN
                RAISE EXCEPTION 'definition: invalid branch' USING ERRCODE='23514'; END IF;
            FOREACH k IN ARRAY ARRAY['left','right'] LOOP
                IF jsonb_typeof(s->'condition'->k) IS NULL OR jsonb_typeof(s->'condition'->k) NOT IN ('string','number','boolean') THEN
                    RAISE EXCEPTION 'definition: branch operands are non-null scalars' USING ERRCODE='23514'; END IF;
                IF jsonb_typeof(s->'condition'->k)='string' THEN
                    IF position('{{' IN (s->'condition'->>k))>0 AND (s->'condition'->>k) !~ '^\{\{[^{}]+\}\}$' THEN
                        RAISE EXCEPTION 'definition: a branch reference is exact' USING ERRCODE='23514'; END IF;
                    strs:=strs||(s->'condition'->>k);
                END IF;
            END LOOP;
        ELSIF s->>'kind'='respond' THEN
            IF EXISTS (SELECT 1 FROM jsonb_object_keys(s) x WHERE x NOT IN ('id','kind','text'))
            OR jsonb_typeof(s->'text') IS DISTINCT FROM 'string' OR length(s->>'text')>4096 THEN
                RAISE EXCEPTION 'definition: invalid respond' USING ERRCODE='23514'; END IF;
            strs:=ARRAY[s->>'text'];
        ELSE RAISE EXCEPTION 'definition: unknown step kind' USING ERRCODE='23514'; END IF;
        IF s->>'kind'<>'respond' THEN
            FOREACH k IN ARRAY (CASE WHEN s->>'kind'='branch' THEN ARRAY['then','else'] ELSE ARRAY['next'] END) LOOP
                dest:=array_position(ids,s->>k);
                IF jsonb_typeof(s->k) IS DISTINCT FROM 'string' OR dest IS NULL OR dest<=i+1 THEN
                    RAISE EXCEPTION 'definition: destinations must be later steps' USING ERRCODE='23514'; END IF;
            END LOOP;
        END IF;
        allowed:=allowed+eacp.studio_placeholders(strs,inputs,refs);
        doms:=jsonb_set(doms,ARRAY[id],to_jsonb(dom||id));
    END LOOP;
    SELECT COALESCE(sum(regexp_count(v#>>'{}','\{\{')),0) INTO total FROM (
      SELECT v FROM jsonb_path_query(d,'strict $.** ? (@.type() == "string")') v UNION ALL
      SELECT v FROM jsonb_path_query(d,'strict $.** ? (@.type() == "object").keyvalue().key') v) x;
    IF total<>allowed THEN RAISE EXCEPTION 'definition: placeholder outside template fields' USING ERRCODE='23514'; END IF;
    IF caps>0 AND (NOT eacp.studio_int(lim->'max_output_tokens',1,20000000) OR caps>(lim->>'max_output_tokens')::bigint) THEN
        RAISE EXCEPTION 'definition: total output cap exceeded' USING ERRCODE='23514'; END IF;
    IF caps=0 AND lim ? 'max_output_tokens' AND NOT eacp.studio_int(lim->'max_output_tokens',0,20000000) THEN
        RAISE EXCEPTION 'definition: invalid output cap' USING ERRCODE='23514'; END IF;
    RETURN ARRAY(SELECT DISTINCT t COLLATE "C" FROM unnest(tools) t ORDER BY 1);
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.studio_definition_models(p_definition text) RETURNS uuid[]
LANGUAGE plpgsql STABLE AS $$
BEGIN
    PERFORM eacp.studio_definition_capability(p_definition);
    RETURN ARRAY(SELECT DISTINCT m.id FROM jsonb_array_elements(p_definition::jsonb->'steps') s
        JOIN eacp.llm_models m ON m.tenant_id=eacp.current_tenant_id() AND m.name=s->>'model'
        WHERE s->>'kind'='llm' ORDER BY m.id);
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.studio_save_as(p_agent uuid, p_name text, p_display_name text, p_description text,
                                    p_department uuid, p_definition text, p_cloned_from uuid) RETURNS uuid
    LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    who      jsonb := eacp.actor_context();
    tenant   uuid := eacp.current_tenant_id();
    a        uuid;
    cap      text[];
    models   uuid[];
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
    models := eacp.studio_definition_models(p_definition);
    digest := encode(sha256(convert_to(p_definition, 'UTF8')), 'hex');
    SELECT COALESCE(array_agg(t.id), '{}') INTO tool_ids FROM eacp.tools t
    JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
    WHERE t.tenant_id = tenant AND c.name || '.' || t.name = ANY (cap);

    INSERT INTO eacp.studio_save_marks (tenant_id, xact, open) VALUES (tenant, pg_current_xact_id(), true)
    ON CONFLICT (tenant_id, xact) DO UPDATE SET open = true;
    IF p_agent IS NULL THEN
        INSERT INTO eacp.agents (tenant_id, name, display_name, environment, risk_class, owner_principal_id)
        VALUES (tenant, p_name, p_display_name, 'production', 'high', a) RETURNING id INTO p_agent;
        INSERT INTO eacp.studio_agents (tenant_id, id, department_group_id, description, created_by, cloned_from_version)
        VALUES (tenant, p_agent, p_department, COALESCE(p_description, ''), a, p_cloned_from);
    END IF;
    INSERT INTO eacp.agent_versions (tenant_id, agent_id, runtime, code_ref)
    VALUES (tenant, p_agent, 'studio', 'studio:' || digest) RETURNING id INTO version;
    INSERT INTO eacp.studio_versions (tenant_id, id, agent_id, definition, digest, capability, model_capability, created_by)
    VALUES (tenant, version, p_agent, p_definition, digest, cap, models, a);
    INSERT INTO eacp.agent_allowlists (tenant_id, agent_version_id, tool_ids, model_ids) VALUES (tenant, version, tool_ids, models);
    UPDATE eacp.studio_save_marks SET open = false WHERE tenant_id = tenant AND xact = pg_current_xact_id();
    RETURN version;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.studio_versions_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF (NEW.tenant_id, NEW.id, NEW.agent_id, NEW.definition, NEW.digest, NEW.capability, NEW.model_capability, NEW.created_by, NEW.created_at)
       IS DISTINCT FROM
       (OLD.tenant_id, OLD.id, OLD.agent_id, OLD.definition, OLD.digest, OLD.capability, OLD.model_capability, OLD.created_by, OLD.created_at) THEN
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
        IF NEW.model_ids IS DISTINCT FROM (SELECT model_capability FROM eacp.studio_versions
            WHERE tenant_id=NEW.tenant_id AND id=NEW.agent_version_id) THEN
            RAISE EXCEPTION 'a Studio allowlist contains exactly its derived models' USING ERRCODE='23514';
        END IF;
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
CREATE FUNCTION eacp.studio_save_checked(p_agent uuid,p_name text,p_display text,p_description text,
    p_department uuid,p_definition text,p_expected uuid) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,pg_temp AS $$
DECLARE latest uuid;
BEGIN
    IF p_agent IS NOT NULL THEN
        PERFORM 1 FROM eacp.agents WHERE tenant_id=eacp.current_tenant_id() AND id=p_agent FOR UPDATE;
        SELECT id INTO latest FROM eacp.agent_versions
        WHERE tenant_id=eacp.current_tenant_id() AND agent_id=p_agent ORDER BY version DESC LIMIT 1;
        IF p_expected IS NULL OR latest IS DISTINCT FROM p_expected THEN
            RAISE EXCEPTION 'studio_version_stale: reload or save as copy' USING ERRCODE='55000';
        END IF;
    ELSIF p_expected IS NOT NULL THEN
        RAISE EXCEPTION 'an initial save has no expected version' USING ERRCODE='23514';
    END IF;
    RETURN eacp.studio_save(p_agent,p_name,p_display,p_description,p_department,p_definition);
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION eacp.studio_save_checked(uuid,text,text,text,uuid,text,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.studio_save_checked(uuid,text,text,text,uuid,text,uuid) TO eacp_app;

-- +goose Down

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
CREATE OR REPLACE FUNCTION eacp.studio_versions_guard() RETURNS trigger
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
CREATE OR REPLACE FUNCTION eacp.studio_save_as(p_agent uuid, p_name text, p_display_name text, p_description text,
                                    p_department uuid, p_definition text, p_cloned_from uuid) RETURNS uuid
    LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp
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
        INSERT INTO eacp.studio_agents (tenant_id, id, department_group_id, description, created_by, cloned_from_version)
        VALUES (tenant, p_agent, p_department, COALESCE(p_description, ''), a, p_cloned_from);
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

DROP FUNCTION eacp.studio_save_checked(uuid,text,text,text,uuid,text,uuid);
DROP FUNCTION eacp.studio_definition_models(text);
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.studio_definition_capability(p_definition text) RETURNS text[]
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

DROP FUNCTION eacp.studio_definition_v1(text);
DROP FUNCTION eacp.studio_output_schema_valid(jsonb,integer);
ALTER TABLE eacp.studio_versions DROP COLUMN model_capability;
