-- Phase 25b (ADR-031): the LLM gateway. Agents call Anthropic Messages and
-- OpenAI Chat Completions through llm-gateway, which decides every call
-- here: registry models, a per-version model allowlist, the model kill
-- scope, the LLM-call ledger and budget reservations for LLM calls.
-- Every redefined function is its latest definition (00003, 00016) with
-- the LLM rules added; the Down section restores those definitions.
-- +goose Up

-- --------------------------------------------------------------- models

CREATE TABLE eacp.llm_models (
    tenant_id         uuid        NOT NULL REFERENCES eacp.tenants (id),
    id                uuid        NOT NULL DEFAULT gen_random_uuid(),
    name              text        NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9._-]{0,62}$'),
    provider          text        NOT NULL CHECK (provider IN ('anthropic', 'openai')),
    -- The gateway appends /v1/messages or /v1/chat/completions.
    base_url          text        NOT NULL CHECK (base_url ~ '^https?://[^[:space:]/?#@]+(/[^[:space:]?#]*)?$'
                                                  AND length(base_url) <= 2048),
    upstream_model    text        NOT NULL CHECK (length(upstream_model) BETWEEN 1 AND 256
                                                  AND upstream_model !~ '[[:cntrl:]]'),
    -- A name the gateway resolves with the tenant id; never a secret value.
    secret_ref        text        NOT NULL CHECK (secret_ref ~ '^[a-z0-9][a-z0-9._/-]{0,127}$'),
    max_output_tokens integer     NOT NULL CHECK (max_output_tokens BETWEEN 1 AND 1000000),
    timeout_ms        integer     NOT NULL DEFAULT 600000 CHECK (timeout_ms BETWEEN 1000 AND 3600000),
    created_by        uuid,
    created_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, name),
    FOREIGN KEY (tenant_id, created_by) REFERENCES eacp.principals (tenant_id, id)
);

-- +goose StatementBegin
CREATE FUNCTION eacp.llm_models_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.actor();
BEGIN
    PERFORM eacp.assert_role(a, 'registry_editor');
    NEW.created_by := a;
    NEW.created_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER llm_models_guard BEFORE INSERT ON eacp.llm_models
    FOR EACH ROW EXECUTE FUNCTION eacp.llm_models_guard();
ALTER TABLE eacp.llm_models ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.llm_models FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.llm_models USING (tenant_id = eacp.current_tenant_id());
REVOKE UPDATE ON eacp.llm_models FROM eacp_app;
CREATE TRIGGER zz_audit AFTER INSERT OR UPDATE ON eacp.llm_models
    FOR EACH ROW EXECUTE FUNCTION eacp.audit_row_change();

-- ------------------------------------------------------ model allowlists

ALTER TABLE eacp.agent_allowlists ADD COLUMN model_ids uuid[] NOT NULL DEFAULT '{}';

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

-- ---------------------------------------------------- the model kill scope

ALTER TABLE eacp.kill_states DROP CONSTRAINT kill_states_scope_check;
ALTER TABLE eacp.kill_states ADD CONSTRAINT kill_states_scope_check
    CHECK (scope IN ('tenant', 'team', 'agent', 'agent_version', 'action', 'connector', 'tool', 'model'));

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

-- --------------------------------------------------- cache-write prices

ALTER TABLE eacp.model_prices ADD COLUMN cache_write_per_mtok numeric(21,6) CHECK (cache_write_per_mtok >= 0);

-- +goose Down

ALTER TABLE eacp.model_prices DROP COLUMN cache_write_per_mtok;

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
    CHECK (scope IN ('tenant', 'team', 'agent', 'agent_version', 'action', 'connector', 'tool'));

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
    NEW.created_by := a;
    NEW.created_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

ALTER TABLE eacp.agent_allowlists DROP COLUMN model_ids;
DROP TABLE eacp.llm_models;
DROP FUNCTION eacp.llm_models_guard();
