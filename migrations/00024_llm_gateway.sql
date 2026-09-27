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

-- ------------------------------------------------------------ the ledger

CREATE TABLE eacp.llm_calls (
    tenant_id            uuid          NOT NULL REFERENCES eacp.tenants (id),
    id                   uuid          NOT NULL DEFAULT gen_random_uuid(),
    agent_id             uuid          NOT NULL,
    agent_version_id     uuid          NOT NULL,
    model_id             uuid,
    model_name           text          NOT NULL CHECK (length(model_name) BETWEEN 1 AND 256
                                                       AND model_name !~ '[[:cntrl:]]'),
    provider             text          NOT NULL CHECK (provider IN ('anthropic', 'openai')),
    subject_principal_id uuid,
    stream               boolean       NOT NULL,
    trace_id             text          CHECK (trace_id ~ '^[0-9a-f]{32}$'),
    request_bytes        bigint        NOT NULL CHECK (request_bytes BETWEEN 0 AND 1073741824),
    max_output_tokens    bigint        CHECK (max_output_tokens BETWEEN 1 AND 1000000000),
    decision_id          uuid,
    policy_bundle_id     uuid,
    policy_version       integer       CHECK (policy_version >= 1),
    verdict              text          CHECK (verdict IN ('allow', 'warn', 'deny', 'escalate', 'transform')),
    input_digest         text          CHECK (input_digest ~ '^([0-9a-f]{2}){1,32}$'),
    price_id             uuid,
    cost_unit            text          CHECK (cost_unit ~ '^[A-Z][A-Z0-9_]{0,15}$'),
    state                text          NOT NULL CHECK (state IN ('DENIED', 'ADMITTED', 'SETTLED')),
    denial               text          CHECK (denial ~ '^[a-z][a-z0-9_]{0,63}$'),
    outcome              text          CHECK (outcome IN ('succeeded', 'provider_error', 'usage_unknown', 'killed',
                                                          'abandoned')),
    provider_status      integer       CHECK (provider_status BETWEEN 0 AND 999),
    input_tokens         bigint        CHECK (input_tokens BETWEEN 0 AND 1000000000000),
    cache_read_tokens    bigint        CHECK (cache_read_tokens BETWEEN 0 AND 1000000000000),
    cache_write_tokens   bigint        CHECK (cache_write_tokens BETWEEN 0 AND 1000000000000),
    output_tokens        bigint        CHECK (output_tokens BETWEEN 0 AND 1000000000000),
    cost_amount          numeric(21,6) CHECK (cost_amount >= 0),
    committed_amount     numeric(21,6) CHECK (committed_amount >= 0),
    gateway_id           text          NOT NULL CHECK (gateway_id ~ '^[A-Za-z0-9._:-]{1,128}$'),
    created_at           timestamptz   NOT NULL DEFAULT now(),
    deadline             timestamptz,
    settled_at           timestamptz,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, agent_id) REFERENCES eacp.agents (tenant_id, id),
    FOREIGN KEY (tenant_id, agent_version_id) REFERENCES eacp.agent_versions (tenant_id, id),
    FOREIGN KEY (tenant_id, model_id) REFERENCES eacp.llm_models (tenant_id, id),
    FOREIGN KEY (tenant_id, subject_principal_id) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, price_id) REFERENCES eacp.model_prices (tenant_id, id),
    CHECK ((state = 'DENIED') = (denial IS NOT NULL)),
    CHECK ((state = 'SETTLED') = (outcome IS NOT NULL AND settled_at IS NOT NULL)),
    CHECK (state = 'DENIED' OR num_nonnulls(model_id, price_id, cost_unit, deadline, max_output_tokens) = 5),
    CHECK (state <> 'DENIED' OR num_nonnulls(price_id, cost_unit, deadline) = 0),
    CHECK (num_nonnulls(input_tokens, cache_read_tokens, cache_write_tokens, output_tokens) IN (0, 4))
);
CREATE INDEX llm_calls_agent_time ON eacp.llm_calls (tenant_id, agent_id, created_at);
CREATE INDEX llm_calls_time ON eacp.llm_calls (tenant_id, created_at);
CREATE INDEX llm_calls_overdue ON eacp.llm_calls (tenant_id, deadline) WHERE state = 'ADMITTED';

ALTER TABLE eacp.llm_calls ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.llm_calls FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.llm_calls USING (tenant_id = eacp.current_tenant_id());
-- The schema owner's read-only scan behind eacp.llm_sweep_tenants().
CREATE POLICY owner_scan ON eacp.llm_calls FOR SELECT TO CURRENT_USER USING (true);
-- The application reads the ledger; only the SECURITY DEFINER functions
-- llm_admit, llm_settle and llm_sweep write it.
REVOKE INSERT, UPDATE, DELETE ON eacp.llm_calls FROM eacp_app;

-- +goose StatementBegin
-- Whether this statement runs inside llm_admit, llm_settle or llm_sweep
-- for call p_call: the gate names the call and the statement runs as the
-- schema owner (a SECURITY DEFINER function). The setting alone can be set
-- by any transaction; the role cannot (ADR-031 §3).
CREATE FUNCTION eacp.llm_ledger_context(p_call uuid) RETURNS boolean
    LANGUAGE sql STABLE
    AS $$
    SELECT current_setting('eacp.llm_call', true) IS NOT DISTINCT FROM p_call::text
       AND current_user = pg_catalog.pg_get_userbyid(
               (SELECT c.relowner FROM pg_catalog.pg_class c WHERE c.oid = 'eacp.llm_calls'::pg_catalog.regclass))
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The ledger changes only through eacp.llm_admit, eacp.llm_settle and
-- eacp.llm_sweep, which name the call they write in eacp.llm_call. A row is
-- inserted for the agent that made the call and settled once: by the
-- gateway, or by the sweeper once it is overdue.
CREATE FUNCTION eacp.llm_calls_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    comp text := eacp.current_system_actor();
BEGIN
    IF NOT eacp.llm_ledger_context(NEW.id) THEN
        RAISE EXCEPTION 'the ledger changes only through eacp.llm_admit, llm_settle and llm_sweep'
            USING ERRCODE = '42501';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF eacp.current_agent_version_id() IS DISTINCT FROM NEW.agent_version_id
           OR eacp.current_actor_id() IS NOT NULL OR comp IS NOT NULL THEN
            RAISE EXCEPTION 'an LLM call is recorded for the agent that made it' USING ERRCODE = '42501';
        END IF;
        IF NEW.state NOT IN ('DENIED', 'ADMITTED') OR num_nonnulls(NEW.outcome, NEW.provider_status, NEW.cost_amount,
                                                                   NEW.committed_amount, NEW.settled_at,
                                                                   NEW.input_tokens) > 0 THEN
            RAISE EXCEPTION 'an LLM call starts denied or admitted' USING ERRCODE = '23514';
        END IF;
        NEW.agent_id := (SELECT agent_id FROM eacp.agent_versions
                         WHERE tenant_id = NEW.tenant_id AND id = NEW.agent_version_id);
        NEW.created_at := now();
        RETURN NEW;
    END IF;
    IF OLD.state <> 'ADMITTED' OR NEW.state <> 'SETTLED' THEN
        RAISE EXCEPTION 'call % is %', OLD.id, OLD.state USING ERRCODE = '55000';
    END IF;
    IF eacp.current_actor_id() IS NOT NULL OR eacp.current_agent_version_id() IS NOT NULL
       OR comp IS DISTINCT FROM (CASE WHEN NEW.outcome = 'abandoned' THEN 'llm_sweeper' ELSE 'llm_gateway' END)
       OR (NEW.outcome = 'abandoned' AND OLD.deadline >= now()) THEN
        RAISE EXCEPTION 'a call is settled by the gateway, or abandoned by the sweeper once overdue'
            USING ERRCODE = '42501';
    END IF;
    NEW.settled_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER llm_calls_guard BEFORE INSERT OR UPDATE ON eacp.llm_calls
    FOR EACH ROW EXECUTE FUNCTION eacp.llm_calls_guard();

-- ------------------------------------------------------------ pricing

-- +goose StatementBegin
-- The reservation for a call: request bytes bound the input tokens from
-- above, priced at the dearest input rate, plus the output cap (ADR-031).
CREATE FUNCTION eacp.llm_estimate(p eacp.model_prices, p_bytes bigint, p_output bigint) RETURNS numeric
    LANGUAGE sql IMMUTABLE
    AS $$
    SELECT ceil(p_bytes * GREATEST(p.input_per_mtok, COALESCE(p.cached_input_per_mtok, p.input_per_mtok),
                                   COALESCE(p.cache_write_per_mtok, p.input_per_mtok))
                + p_output * p.output_per_mtok) / 1000000
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The cost of a call's usage, rounded up to six decimals; NULL when a
-- cache write has no price (unpriced, ADR-025).
CREATE FUNCTION eacp.llm_cost(p eacp.model_prices, p_input bigint, p_cache_read bigint, p_cache_write bigint,
                              p_output bigint) RETURNS numeric
    LANGUAGE sql IMMUTABLE
    AS $$
    SELECT CASE WHEN p_cache_write > 0 AND p.cache_write_per_mtok IS NULL THEN NULL
                ELSE ceil(p_input * p.input_per_mtok
                          + p_cache_read * COALESCE(p.cached_input_per_mtok, p.input_per_mtok)
                          + p_cache_write * COALESCE(p.cache_write_per_mtok, 0)
                          + p_output * p.output_per_mtok) / 1000000 END
$$;
-- +goose StatementEnd

-- ------------------------------------------------------------ kills

-- +goose StatementBegin
-- Whether a kill scope stops an LLM call of this agent version and model.
CREATE FUNCTION eacp.llm_killed(p_tenant uuid, p_group uuid, p_agent uuid, p_version uuid, p_model uuid)
    RETURNS boolean
    LANGUAGE sql STABLE
    AS $$
    SELECT EXISTS (
        SELECT 1 FROM eacp.kill_states k
        WHERE k.tenant_id = p_tenant AND k.killed AND (
            (k.scope = 'tenant' AND k.target_id = p_tenant) OR
            (k.scope = 'team' AND k.target_id = p_group) OR
            (k.scope = 'agent' AND k.target_id = p_agent) OR
            (k.scope = 'agent_version' AND k.target_id = p_version) OR
            (k.scope = 'model' AND k.target_id = p_model)))
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.llm_call_killed(p_call uuid) RETURNS boolean
    LANGUAGE sql STABLE
    AS $$
    SELECT eacp.llm_killed(c.tenant_id, a.owner_group_id, c.agent_id, c.agent_version_id, c.model_id)
    FROM eacp.llm_calls c
    JOIN eacp.agents a ON a.tenant_id = c.tenant_id AND a.id = c.agent_id
    WHERE c.tenant_id = eacp.current_tenant_id() AND c.id = p_call
$$;
-- +goose StatementEnd

-- ------------------------------------------------ LLM reservations

ALTER TABLE eacp.budget_reservations ALTER COLUMN action_id DROP NOT NULL;
ALTER TABLE eacp.budget_reservations ALTER COLUMN contract_id DROP NOT NULL;
ALTER TABLE eacp.budget_reservations ADD COLUMN llm_call_id uuid;
ALTER TABLE eacp.budget_reservations ADD CONSTRAINT budget_reservations_llm_call_fkey
    FOREIGN KEY (tenant_id, llm_call_id) REFERENCES eacp.llm_calls (tenant_id, id);
ALTER TABLE eacp.budget_reservations ADD CONSTRAINT budget_reservations_subject
    CHECK (num_nonnulls(action_id, llm_call_id) = 1 AND (action_id IS NULL) = (contract_id IS NULL));
CREATE UNIQUE INDEX budget_reservations_llm_call ON eacp.budget_reservations (tenant_id, llm_call_id)
    WHERE llm_call_id IS NOT NULL;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.budget_reservations_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    who  jsonb;
    act  eacp.actions%ROWTYPE;
    call eacp.llm_calls%ROWTYPE;
    pr   eacp.model_prices%ROWTYPE;
    ct   eacp.tool_contracts%ROWTYPE;
    acct eacp.budget_accounts%ROWTYPE;
BEGIN
    -- An LLM call's reservation (ADR-031): made by eacp.llm_admit for the
    -- call's agent, settled by eacp.llm_settle or eacp.llm_sweep.
    IF TG_OP = 'INSERT' AND NEW.llm_call_id IS NOT NULL THEN
        IF NEW.action_id IS NOT NULL OR NEW.contract_id IS NOT NULL THEN
            RAISE EXCEPTION 'a reservation is for an action or an LLM call' USING ERRCODE = '23514';
        END IF;
        IF NOT eacp.llm_ledger_context(NEW.llm_call_id) OR eacp.current_agent_version_id() IS NULL THEN
            RAISE EXCEPTION 'an LLM call is reserved only by eacp.llm_admit' USING ERRCODE = '42501';
        END IF;
        SELECT * INTO call FROM eacp.llm_calls WHERE tenant_id = NEW.tenant_id AND id = NEW.llm_call_id;
        IF NOT FOUND OR call.state <> 'ADMITTED' OR call.agent_version_id <> eacp.current_agent_version_id() THEN
            RAISE EXCEPTION 'a reservation is for an admitted call of this agent' USING ERRCODE = '55000';
        END IF;
        SELECT * INTO pr FROM eacp.model_prices WHERE tenant_id = NEW.tenant_id AND id = call.price_id;
        SELECT * INTO acct FROM eacp.budget_accounts WHERE tenant_id = NEW.tenant_id AND id = NEW.account_id;
        IF NOT FOUND OR acct.agent_id IS DISTINCT FROM call.agent_id OR acct.unit <> pr.unit THEN
            RAISE EXCEPTION 'a reservation draws on the agent''s account in the price''s unit' USING ERRCODE = '55000';
        END IF;
        IF NEW.amount IS DISTINCT FROM eacp.llm_estimate(pr, call.request_bytes, call.max_output_tokens) THEN
            RAISE EXCEPTION 'a reservation is exactly the call''s estimate' USING ERRCODE = '55000';
        END IF;
        IF NEW.state <> 'ACTIVE' OR num_nonnulls(NEW.committed_amount, NEW.settled_at, NEW.settle_reason,
                                                 NEW.folded_at) > 0 THEN
            RAISE EXCEPTION 'a reservation starts ACTIVE' USING ERRCODE = '23514';
        END IF;
        NEW.unit := pr.unit;
        NEW.created_at := now();
        NEW.expires_at := call.deadline;
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' AND OLD.llm_call_id IS NOT NULL THEN
        IF NEW.state IS DISTINCT FROM OLD.state THEN
            IF (to_jsonb(NEW) - ARRAY['state', 'committed_amount', 'settled_at', 'settle_reason'])
               IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['state', 'committed_amount', 'settled_at', 'settle_reason']) THEN
                RAISE EXCEPTION 'settling a reservation changes only its settlement' USING ERRCODE = '55000';
            END IF;
            IF NOT eacp.llm_ledger_context(OLD.llm_call_id)
               OR eacp.current_system_actor() IS NULL
               OR eacp.current_system_actor() NOT IN ('llm_gateway', 'llm_sweeper')
               OR eacp.current_actor_id() IS NOT NULL OR eacp.current_agent_version_id() IS NOT NULL THEN
                RAISE EXCEPTION 'an LLM reservation is settled only with its call' USING ERRCODE = '42501';
            END IF;
            IF OLD.state <> 'ACTIVE' THEN
                RAISE EXCEPTION 'reservation % is already %', OLD.id, OLD.state USING ERRCODE = '55000';
            END IF;
            SELECT * INTO call FROM eacp.llm_calls WHERE tenant_id = NEW.tenant_id AND id = OLD.llm_call_id;
            IF call.state <> 'SETTLED' OR call.committed_amount IS NULL OR call.committed_amount > OLD.amount
               OR (NEW.state = 'RELEASED') <> (call.outcome = 'provider_error') THEN
                RAISE EXCEPTION 'a reservation follows its settled call' USING ERRCODE = '55000';
            END IF;
            NEW.committed_amount := call.committed_amount;
            PERFORM eacp.require_reason(NEW.settle_reason, 'a settlement');
            NEW.settled_at := now();
            RETURN NEW;
        END IF;
        IF OLD.folded_at IS NULL AND NEW.folded_at IS NOT NULL
           AND (to_jsonb(NEW) - 'folded_at') IS NOT DISTINCT FROM (to_jsonb(OLD) - 'folded_at') THEN
            NEW.folded_at := now();
            RETURN NEW;
        END IF;
        RAISE EXCEPTION 'a reservation changes only by settling, then folding' USING ERRCODE = '55000';
    END IF;
    who := eacp.actor_context();

    IF TG_OP = 'INSERT' THEN
        IF NEW.action_id IS NULL OR NEW.contract_id IS NULL THEN
            RAISE EXCEPTION 'a reservation is for an action or an LLM call' USING ERRCODE = '23514';
        END IF;
        IF who->>'kind' = 'principal' THEN
            RAISE EXCEPTION 'budget is reserved only by the release' USING ERRCODE = '42501';
        END IF;
        SELECT * INTO act FROM eacp.actions WHERE tenant_id = NEW.tenant_id AND id = NEW.action_id FOR UPDATE;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'unknown action %', NEW.action_id USING ERRCODE = '23503';
        END IF;
        PERFORM eacp.assert_action_agent(who, act.agent_id);
        IF act.state <> 'AUTHORIZED' OR act.not_after <= now() THEN
            RAISE EXCEPTION 'budget is reserved only for an AUTHORIZED, unexpired action' USING ERRCODE = '55000';
        END IF;
        SELECT c.* INTO ct FROM eacp.tools t
        JOIN eacp.tool_contracts c ON c.tenant_id = t.tenant_id AND c.id = t.active_contract_id
        WHERE t.tenant_id = NEW.tenant_id AND t.id = act.tool_id AND c.id = NEW.contract_id
        FOR SHARE OF t, c;
        IF NOT FOUND OR ct.revoked_at IS NOT NULL OR ct.cost_unit IS NULL THEN
            RAISE EXCEPTION 'a reservation is priced by the tool''s active, budgeted contract' USING ERRCODE = '55000';
        END IF;
        SELECT * INTO acct FROM eacp.budget_accounts WHERE tenant_id = NEW.tenant_id AND id = NEW.account_id;
        IF NOT FOUND OR acct.agent_id IS DISTINCT FROM act.agent_id OR acct.unit <> ct.cost_unit THEN
            RAISE EXCEPTION 'a reservation draws on the agent''s account in the contract''s unit' USING ERRCODE = '55000';
        END IF;
        IF NEW.amount IS DISTINCT FROM eacp.action_cost(ct, act.enforced_payload::jsonb) THEN
            RAISE EXCEPTION 'a reservation is exactly the action''s cost' USING ERRCODE = '55000';
        END IF;
        IF NEW.state <> 'ACTIVE' OR num_nonnulls(NEW.committed_amount, NEW.settled_at, NEW.settle_reason,
                                                 NEW.folded_at) > 0 THEN
            RAISE EXCEPTION 'a reservation starts ACTIVE' USING ERRCODE = '23514';
        END IF;
        NEW.unit := ct.cost_unit;
        NEW.created_at := now();
        NEW.expires_at := act.not_after;  -- the reservation TTL (§47)
        RETURN NEW;
    END IF;

    IF NEW.state IS DISTINCT FROM OLD.state THEN
        IF (to_jsonb(NEW) - ARRAY['state', 'committed_amount', 'settled_at', 'settle_reason'])
           IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['state', 'committed_amount', 'settled_at', 'settle_reason']) THEN
            RAISE EXCEPTION 'settling a reservation changes only its settlement' USING ERRCODE = '55000';
        END IF;
        IF OLD.state <> 'ACTIVE' THEN
            RAISE EXCEPTION 'reservation % is already %', OLD.id, OLD.state USING ERRCODE = '55000';
        END IF;
        SELECT * INTO act FROM eacp.actions WHERE tenant_id = NEW.tenant_id AND id = NEW.action_id;
        IF NEW.state = 'COMMITTED' THEN
            -- Commit the actual; no connector reports one yet, so the estimate.
            IF act.state <> 'SUCCEEDED' THEN
                RAISE EXCEPTION 'budget is committed only when its action succeeded' USING ERRCODE = '55000';
            END IF;
            NEW.committed_amount := COALESCE(NEW.committed_amount, OLD.amount);
        ELSE
            -- No effect happened (terminal without success), or the action
            -- is back at the release boundary and is being repriced.
            IF act.state NOT IN ('FAILED', 'CANCELLED', 'EXPIRED', 'DENIED', 'AUTHORIZED') THEN
                RAISE EXCEPTION 'budget is released only when its action had no effect' USING ERRCODE = '55000';
            END IF;
            NEW.committed_amount := 0;
        END IF;
        PERFORM eacp.require_reason(NEW.settle_reason, 'a settlement');
        NEW.settled_at := now();
        RETURN NEW;
    END IF;

    IF OLD.folded_at IS NULL AND NEW.folded_at IS NOT NULL
       AND (to_jsonb(NEW) - 'folded_at') IS NOT DISTINCT FROM (to_jsonb(OLD) - 'folded_at') THEN
        NEW.folded_at := now();
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'a reservation changes only by settling, then folding' USING ERRCODE = '55000';
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.audit_budget_reservation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    who jsonb;
BEGIN
    IF TG_OP = 'UPDATE' AND NEW.state IS NOT DISTINCT FROM OLD.state THEN
        RETURN NULL;
    END IF;
    IF NEW.llm_call_id IS NOT NULL AND eacp.current_system_actor() IN ('llm_gateway', 'llm_sweeper') THEN
        who := jsonb_build_object('kind', 'system', 'id', '00000000-0000-0000-0000-000000000000'::uuid,
                                  'component', eacp.current_system_actor());
    ELSE
        who := eacp.actor_context();
    END IF;
    INSERT INTO eacp.audit_events (tenant_id, payload)
    VALUES (NEW.tenant_id, convert_to(jsonb_build_object(
        'v', 1,
        'actor', who - 'component',
        'action', CASE NEW.state WHEN 'ACTIVE' THEN 'budget.reserved'
                                 WHEN 'COMMITTED' THEN 'budget.committed' ELSE 'budget.released' END,
        'subject', jsonb_build_object('type', 'budget_reservation', 'id', NEW.id),
        'reason', COALESCE(NEW.settle_reason, CASE WHEN NEW.llm_call_id IS NULL THEN 'reserved at release' ELSE 'reserved at admission' END),
        'data', jsonb_strip_nulls(jsonb_build_object(
            'component', who->>'component',
            'action_id', NEW.action_id,
            'llm_call_id', NEW.llm_call_id,
            'account_id', NEW.account_id,
            'contract_id', NEW.contract_id,
            'unit', NEW.unit,
            'amount', NEW.amount,
            'committed_amount', NEW.committed_amount,
            'expires_at', NEW.expires_at)))::text, 'UTF8'));
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- --------------------------------------------------------- gateway usage

ALTER TABLE eacp.usage_records DROP CONSTRAINT usage_records_source_check;
ALTER TABLE eacp.usage_records ADD CONSTRAINT usage_records_source_check
    CHECK (source IN ('otel', 'provider_billing', 'gateway'));
ALTER TABLE eacp.usage_records ADD COLUMN llm_call_id uuid;
ALTER TABLE eacp.usage_records ADD CONSTRAINT usage_records_llm_call_fkey
    FOREIGN KEY (tenant_id, llm_call_id) REFERENCES eacp.llm_calls (tenant_id, id);
ALTER TABLE eacp.usage_records ADD CONSTRAINT usage_records_gateway
    CHECK ((source = 'gateway') = (llm_call_id IS NOT NULL));
CREATE UNIQUE INDEX usage_records_llm_call ON eacp.usage_records (tenant_id, llm_call_id) WHERE source = 'gateway';

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.usage_records_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.current_actor_id();
    ver uuid := eacp.current_agent_version_id();
    v eacp.agent_versions%ROWTYPE;
    c record;
    call eacp.llm_calls%ROWTYPE;
BEGIN
    NEW.received_at := now();
    IF NEW.source = 'gateway' THEN
        -- Written by eacp.llm_settle for a succeeded call; every value comes
        -- from the call (ADR-031).
        IF eacp.current_system_actor() IS DISTINCT FROM 'llm_gateway' OR a IS NOT NULL OR ver IS NOT NULL
           OR NOT eacp.llm_ledger_context(NEW.llm_call_id) THEN
            RAISE EXCEPTION 'gateway usage is recorded by eacp.llm_settle' USING ERRCODE = '42501';
        END IF;
        SELECT * INTO call FROM eacp.llm_calls WHERE tenant_id = NEW.tenant_id AND id = NEW.llm_call_id;
        IF NOT FOUND OR call.state <> 'SETTLED' OR call.outcome <> 'succeeded' OR call.input_tokens IS NULL THEN
            RAISE EXCEPTION 'gateway usage is recorded for a succeeded call' USING ERRCODE = '55000';
        END IF;
        NEW.agent_id := call.agent_id;
        NEW.agent_version_id := call.agent_version_id;
        NEW.trace_id := NULL;
        NEW.span_id := NULL;
        NEW.external_id := NULL;
        NEW.reported_by := NULL;
        NEW.provider := call.provider;
        NEW.operation := 'chat';
        NEW.model := (SELECT upstream_model FROM eacp.llm_models WHERE tenant_id = call.tenant_id AND id = call.model_id);
        NEW.input_tokens := call.input_tokens + call.cache_read_tokens + call.cache_write_tokens;
        NEW.cache_read_tokens := call.cache_read_tokens;
        NEW.output_tokens := call.output_tokens;
        NEW.billable := true;
        NEW.observed_at := call.settled_at;
        NEW.price_id := call.price_id;
        NEW.cost_amount := call.cost_amount;
        NEW.cost_unit := CASE WHEN call.cost_amount IS NULL THEN NULL ELSE call.cost_unit END;
        RETURN NEW;
    END IF;
    NEW.llm_call_id := NULL;
    IF NEW.source = 'otel' THEN
        IF ver IS NULL OR a IS NOT NULL OR eacp.current_system_actor() IS NOT NULL THEN
            RAISE EXCEPTION 'usage spans are recorded by the agent that made them' USING ERRCODE = '42501';
        END IF;
        SELECT * INTO v FROM eacp.agent_versions WHERE tenant_id = NEW.tenant_id AND id = ver;
        IF NOT FOUND OR v.state IN ('RETIRED', 'REVOKED') THEN
            RAISE EXCEPTION 'agent version % cannot report usage', ver USING ERRCODE = '42501';
        END IF;
        IF NEW.observed_at < now() - interval '7 days' OR NEW.observed_at > now() + interval '5 minutes' THEN
            RAISE EXCEPTION 'usage must be observed within the last 7 days' USING ERRCODE = '23514';
        END IF;
        NEW.agent_version_id := v.id;
        NEW.agent_id := v.agent_id;
        NEW.external_id := NULL;
        NEW.reported_by := NULL;
        NEW.billable := NEW.operation IN ('chat', 'generate_content', 'text_completion', 'embeddings');
        NEW.price_id := NULL;
        NEW.cost_amount := NULL;
        NEW.cost_unit := NULL;
        IF NEW.model IS NOT NULL THEN
            SELECT * INTO c FROM eacp.usage_cost(NEW.provider, NEW.model, NEW.observed_at,
                                                 NEW.input_tokens, NEW.cache_read_tokens, NEW.output_tokens);
            NEW.price_id := c.price_id;
            NEW.cost_amount := c.amount;
            NEW.cost_unit := c.unit;
        END IF;
    ELSE
        IF a IS NULL OR ver IS NOT NULL THEN
            RAISE EXCEPTION 'billing lines are imported by an admin' USING ERRCODE = '42501';
        END IF;
        PERFORM eacp.assert_role(a, 'admin');
        IF NEW.cost_amount IS NULL OR NEW.cost_unit IS NULL THEN
            RAISE EXCEPTION 'a billing line carries the provider''s amount and unit' USING ERRCODE = '23514';
        END IF;
        IF NEW.observed_at > now() + interval '5 minutes' OR NEW.observed_at < now() - interval '400 days' THEN
            RAISE EXCEPTION 'a billing line is from the last 400 days' USING ERRCODE = '23514';
        END IF;
        NEW.agent_version_id := NULL;
        NEW.trace_id := NULL;
        NEW.span_id := NULL;
        NEW.price_id := NULL;
        NEW.billable := true;
        NEW.reported_by := a;
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.finops_llm_daily(p_from timestamptz, p_to timestamptz)
    RETURNS TABLE (agent_id uuid, unit text, day date, reported numeric, billed numeric, effective numeric)
    LANGUAGE sql STABLE
    AS $$
    SELECT u.agent_id, u.cost_unit, (u.observed_at AT TIME ZONE 'UTC')::date,
           COALESCE(sum(u.cost_amount) FILTER (WHERE u.source IN ('otel', 'gateway')), 0),
           COALESCE(sum(u.cost_amount) FILTER (WHERE u.source = 'provider_billing'), 0),
           GREATEST(COALESCE(sum(u.cost_amount) FILTER (WHERE u.source IN ('otel', 'gateway')), 0),
                    COALESCE(sum(u.cost_amount) FILTER (WHERE u.source = 'provider_billing'), 0))
      FROM eacp.usage_records u
     WHERE u.billable AND u.cost_amount IS NOT NULL AND u.observed_at >= p_from AND u.observed_at < p_to
     GROUP BY 1, 2, 3
    $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.finops_evaluate() RETURNS integer
    LANGUAGE plpgsql
    AS $$
DECLARE
    tenant uuid := eacp.current_tenant_id();
    month_start timestamptz := date_trunc('month', now(), 'UTC');
    day_start timestamptz := date_trunc('day', now(), 'UTC');
    hour_start timestamptz := date_trunc('hour', now(), 'UTC') - interval '1 hour';
    raised integer := 0;
    k integer;
BEGIN
    IF eacp.current_system_actor() IS DISTINCT FROM 'finops' OR eacp.current_actor_id() IS NOT NULL THEN
        RAISE EXCEPTION 'the finops evaluator runs as the finops system actor' USING ERRCODE = '42501';
    END IF;

    WITH s AS (
        SELECT l.account_id, l.monthly_limit, b.unit,
               eacp.finops_account_spend(l.account_id, month_start, now()) AS spent
          FROM eacp.budget_soft_limits l
          JOIN eacp.budget_accounts b ON b.tenant_id = l.tenant_id AND b.id = l.account_id
         WHERE l.monthly_limit IS NOT NULL
    )
    INSERT INTO eacp.finops_alerts (tenant_id, kind, subject_type, subject_id, unit, period_start,
                                    observed, threshold, detail)
    SELECT tenant, t.kind, 'budget_account', s.account_id, s.unit, month_start, s.spent, s.monthly_limit,
           jsonb_build_object('ratio', round(s.spent / s.monthly_limit, 4), 'at', t.ratio)
      FROM s CROSS JOIN (VALUES ('soft_limit_warning', 0.8), ('soft_limit_exceeded', 1.0)) AS t(kind, ratio)
     WHERE s.spent >= s.monthly_limit * t.ratio
    ON CONFLICT (tenant_id, kind, subject_id, unit, period_start) DO NOTHING;
    GET DIAGNOSTICS k = ROW_COUNT;
    raised := raised + k;

    WITH spend AS (
        SELECT u.agent_id, u.cost_unit AS unit, date_trunc('hour', u.observed_at, 'UTC') AS h, u.cost_amount AS amount
          FROM eacp.usage_records u
         WHERE u.source IN ('otel', 'gateway') AND u.billable AND u.cost_amount IS NOT NULL
           AND u.observed_at >= hour_start - interval '168 hours' AND u.observed_at < hour_start + interval '1 hour'
        UNION ALL
        SELECT a.agent_id, r.unit, date_trunc('hour', r.created_at, 'UTC'), r.amount
          FROM eacp.budget_reservations r
          JOIN eacp.actions a ON a.tenant_id = r.tenant_id AND a.id = r.action_id
         WHERE r.state <> 'RELEASED'
           AND r.created_at >= hour_start - interval '168 hours' AND r.created_at < hour_start + interval '1 hour'
    ), agg AS (
        SELECT agent_id, unit, COALESCE(sum(amount) FILTER (WHERE h = hour_start), 0) AS cur,
               COALESCE(sum(amount) FILTER (WHERE h < hour_start), 0) AS base
          FROM spend GROUP BY 1, 2
    )
    INSERT INTO eacp.finops_alerts (tenant_id, kind, subject_type, subject_id, unit, period_start,
                                    observed, threshold, detail)
    SELECT tenant, 'spend_anomaly', 'agent', g.agent_id, g.unit, hour_start, g.cur, 3 * g.base / 168,
           jsonb_build_object('baseline_hourly_mean', round(g.base / 168, 6), 'baseline_hours', 168, 'factor', 3)
      FROM agg g
     WHERE g.cur > 0 AND g.cur >= 3 * g.base / 168
       AND (EXISTS (SELECT 1 FROM eacp.usage_records u
                     WHERE u.agent_id = g.agent_id AND u.observed_at < hour_start - interval '24 hours')
            OR EXISTS (SELECT 1 FROM eacp.budget_reservations r
                         JOIN eacp.actions a ON a.tenant_id = r.tenant_id AND a.id = r.action_id
                        WHERE a.agent_id = g.agent_id AND r.created_at < hour_start - interval '24 hours'))
    ON CONFLICT (tenant_id, kind, subject_id, unit, period_start) DO NOTHING;
    GET DIAGNOSTICS k = ROW_COUNT;
    raised := raised + k;

    INSERT INTO eacp.finops_alerts (tenant_id, kind, subject_type, subject_id, unit, period_start, detail)
    SELECT tenant, 'unpriced_usage', 'agent', u.agent_id, '', day_start,
           jsonb_build_object('tokens', sum(u.input_tokens + u.output_tokens),
                              'models', jsonb_agg(DISTINCT COALESCE(u.model, '')))
      FROM eacp.usage_records u
     WHERE u.billable AND u.cost_amount IS NULL AND u.observed_at >= day_start
     GROUP BY u.agent_id
    ON CONFLICT (tenant_id, kind, subject_id, unit, period_start) DO NOTHING;
    GET DIAGNOSTICS k = ROW_COUNT;
    RETURN raised + k;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.release_version_metrics(p_version uuid, p_from timestamptz) RETURNS jsonb
    LANGUAGE sql STABLE
    AS $$
    WITH a AS (
        SELECT * FROM eacp.actions
        WHERE tenant_id = eacp.current_tenant_id() AND agent_version_id = p_version AND created_at >= p_from
    ), att AS (
        SELECT t.action_id, t.dispatched_at, t.completed_at, t.outcome
        FROM eacp.action_attempts t JOIN a ON a.tenant_id = t.tenant_id AND a.id = t.action_id
    ), cost AS (
        SELECT unit, sum(amount) AS total FROM (
            SELECT r.unit, COALESCE(r.committed_amount, r.amount) AS amount
            FROM eacp.budget_reservations r JOIN a ON a.tenant_id = r.tenant_id AND a.id = r.action_id
            WHERE r.state <> 'RELEASED'
            UNION ALL
            SELECT u.cost_unit, u.cost_amount FROM eacp.usage_records u
            WHERE u.tenant_id = eacp.current_tenant_id() AND u.agent_version_id = p_version
              AND u.source IN ('otel', 'gateway') AND u.cost_amount IS NOT NULL AND u.observed_at >= p_from
        ) x GROUP BY unit
    ), n AS (SELECT count(*) AS actions FROM a)
    SELECT jsonb_build_object(
        'actions', n.actions,
        'denied', (SELECT count(*) FROM a WHERE state = 'DENIED'),
        'succeeded', (SELECT count(*) FROM a WHERE state = 'SUCCEEDED'),
        'failed', (SELECT count(*) FROM a WHERE state = 'FAILED'),
        'dispatched', (SELECT count(*) FROM a WHERE attempt_count > 0),
        'unknown', (SELECT count(*) FROM a WHERE state IN ('UNKNOWN_OUTCOME', 'RECONCILING', 'NEEDS_HUMAN_RESOLUTION')
                        OR EXISTS (SELECT 1 FROM eacp.reconciliation_checks c
                                   WHERE c.tenant_id = a.tenant_id AND c.action_id = a.id)
                        OR EXISTS (SELECT 1 FROM att WHERE att.action_id = a.id AND att.outcome = 'ambiguous')),
        'latency_p95_ms', (SELECT round((percentile_cont(0.95) WITHIN GROUP (
                               ORDER BY extract(epoch FROM completed_at - dispatched_at) * 1000))::numeric, 3)
                           FROM att WHERE completed_at IS NOT NULL),
        'cost', COALESCE((SELECT jsonb_object_agg(unit, total) FROM cost), '{}'),
        'cost_per_action', COALESCE((SELECT jsonb_object_agg(unit, round(total / NULLIF(n.actions, 0), 6))
                                     FROM cost), '{}'))
    FROM n
$$;
-- +goose StatementEnd


-- --------------------------------------------------- admit, settle, sweep

-- +goose StatementBegin
-- eacp.llm_admit decides an LLM call for the transaction's agent and
-- records it (ADR-031 §3.5). Input: model_name, provider, subject,
-- stream, request_bytes, max_output_tokens (NULL: the model's cap),
-- trace_id, gateway_id and the PDP decision {id, bundle_id, version,
-- verdict, input_digest, denial}. Lock order: the version FOR SHARE (models are immutable),
-- the tenant kill lock, the budget leaf, then the call, its reservation
-- and the audit event, last. Returns {call_id, denial} or the admission.
CREATE FUNCTION eacp.llm_admit(p jsonb) RETURNS jsonb
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    tenant  uuid := eacp.current_tenant_id();
    who     jsonb := eacp.actor_context();
    ver     eacp.agent_versions%ROWTYPE;
    ag      eacp.agents%ROWTYPE;
    m       eacp.llm_models%ROWTYPE;
    pr      eacp.model_prices%ROWTYPE;
    acct    eacp.budget_accounts%ROWTYPE;
    dec     jsonb := p->'decision';
    subj    uuid;
    denial  text;
    req_out bigint;
    est     numeric;
    room    numeric;
    allowed boolean;
    call_id uuid := gen_random_uuid();
    due     timestamptz;
BEGIN
    IF who->>'kind' IS DISTINCT FROM 'agent' THEN
        RAISE EXCEPTION 'an LLM call is admitted for an authenticated agent' USING ERRCODE = '42501';
    END IF;
    IF jsonb_typeof(p->'model_name') IS DISTINCT FROM 'string' OR jsonb_typeof(dec) IS DISTINCT FROM 'object'
       OR p->>'provider' NOT IN ('anthropic', 'openai') OR jsonb_typeof(p->'stream') IS DISTINCT FROM 'boolean'
       OR jsonb_typeof(p->'request_bytes') IS DISTINCT FROM 'number' THEN
        RAISE EXCEPTION 'an admission names a model, provider, stream flag, request size and decision'
            USING ERRCODE = '22023';
    END IF;
    IF jsonb_typeof(p->'max_output_tokens') = 'number' THEN
        req_out := (p->>'max_output_tokens')::bigint;
    END IF;

    SELECT * INTO ver FROM eacp.agent_versions WHERE tenant_id = tenant AND id = (who->>'id')::uuid FOR SHARE;
    SELECT * INTO ag FROM eacp.agents WHERE tenant_id = tenant AND id = ver.agent_id;
    IF ver.state <> 'ACTIVE' OR ver.active_allowlist_id IS NULL THEN
        denial := 'agent_version_not_active';
    END IF;
    IF denial IS NULL AND COALESCE(p->>'subject', '') <> '' THEN
        SELECT id INTO subj FROM eacp.principals
        WHERE tenant_id = tenant AND subject = p->>'subject' AND kind = 'human' AND disabled_at IS NULL;
        IF NOT FOUND THEN
            denial := 'subject_invalid';
        END IF;
    END IF;
    SELECT * INTO m FROM eacp.llm_models WHERE tenant_id = tenant AND name = p->>'model_name';  -- immutable: no lock
    IF denial IS NULL AND m.id IS NULL THEN
        denial := 'unknown_model';
    ELSIF denial IS NULL AND m.provider <> p->>'provider' THEN
        denial := 'wrong_provider';
    END IF;
    IF denial IS NULL THEN
        SELECT m.id = ANY (model_ids) INTO allowed FROM eacp.agent_allowlists
        WHERE tenant_id = tenant AND id = ver.active_allowlist_id;
        IF NOT COALESCE(allowed, false) THEN
            denial := 'model_not_in_allowlist';
        END IF;
    END IF;
    IF denial IS NULL THEN
        denial := eacp.release_denial(ver.id, subj);
    END IF;
    IF denial IS NULL AND COALESCE(dec->>'denial', '') <> '' THEN
        denial := dec->>'denial';
    END IF;
    PERFORM pg_advisory_xact_lock(hashtextextended('eacp.kill:' || tenant::text, 0));
    IF denial IS NULL AND eacp.llm_killed(tenant, ag.owner_group_id, ag.id, ver.id, m.id) THEN
        denial := 'killed';
    END IF;
    IF denial IS NULL AND req_out IS NOT NULL AND req_out > m.max_output_tokens THEN
        denial := 'max_tokens_over_cap';
    END IF;
    IF denial IS NULL THEN
        req_out := COALESCE(req_out, m.max_output_tokens);
        SELECT * INTO pr FROM eacp.model_prices
        WHERE tenant_id = tenant AND provider = m.provider AND model = m.upstream_model AND effective_from <= now()
        ORDER BY effective_from DESC LIMIT 1;
        IF NOT FOUND THEN
            denial := 'model_unpriced';
        END IF;
    END IF;
    IF denial IS NULL THEN
        est := eacp.llm_estimate(pr, (p->>'request_bytes')::bigint, req_out);
        SELECT * INTO acct FROM eacp.budget_accounts
        WHERE tenant_id = tenant AND agent_id = ag.id AND unit = pr.unit FOR NO KEY UPDATE;
        IF FOUND THEN
            PERFORM eacp.budget_fold(acct.id);
            SELECT hard_limit - allocated - reserved - committed INTO room FROM eacp.budget_accounts
            WHERE tenant_id = tenant AND id = acct.id;
            IF est > room THEN
                denial := 'budget_exceeded';
            END IF;
        END IF;
    END IF;

    PERFORM set_config('eacp.llm_call', call_id::text, true);
    IF denial IS NULL THEN
        due := now() + make_interval(secs => m.timeout_ms / 1000.0) + interval '60 seconds';
    END IF;
    INSERT INTO eacp.llm_calls (tenant_id, id, agent_version_id, model_id, model_name, provider,
        subject_principal_id, stream, trace_id, request_bytes, max_output_tokens, decision_id, policy_bundle_id,
        policy_version, verdict, input_digest, price_id, cost_unit, state, denial, gateway_id, deadline)
    VALUES (tenant, call_id, ver.id, m.id, p->>'model_name', p->>'provider', subj, (p->>'stream')::boolean,
        NULLIF(p->>'trace_id', ''), (p->>'request_bytes')::bigint,
        CASE WHEN denial IS NULL THEN req_out
             WHEN req_out BETWEEN 1 AND 1000000000 THEN req_out END,
        (dec->>'id')::uuid, (dec->>'bundle_id')::uuid, (dec->>'version')::integer, dec->>'verdict',
        NULLIF(dec->>'input_digest', ''),
        CASE WHEN denial IS NULL THEN pr.id END, CASE WHEN denial IS NULL THEN pr.unit END,
        CASE WHEN denial IS NULL THEN 'ADMITTED' ELSE 'DENIED' END, denial, p->>'gateway_id', due);
    IF denial IS NULL AND acct.id IS NOT NULL THEN
        INSERT INTO eacp.budget_reservations (tenant_id, account_id, unit, llm_call_id, amount, expires_at)
        VALUES (tenant, acct.id, pr.unit, call_id, est, due);
    END IF;
    INSERT INTO eacp.audit_events (tenant_id, payload)
    VALUES (tenant, convert_to(jsonb_build_object(
        'v', 1, 'actor', who,
        'action', CASE WHEN denial IS NULL THEN 'llm.admitted' ELSE 'llm.denied' END,
        'subject', jsonb_build_object('type', 'llm_call', 'id', call_id),
        'reason', COALESCE(denial, 'admitted'),
        'data', jsonb_strip_nulls(jsonb_build_object(
            'model', p->>'model_name', 'model_id', m.id, 'provider', p->>'provider', 'stream', p->'stream',
            'request_bytes', p->'request_bytes', 'max_output_tokens', req_out, 'decision_id', dec->>'id',
            'verdict', dec->>'verdict', 'reservation', est, 'unit', pr.unit, 'deadline', due,
            'gateway_id', p->>'gateway_id')))::text, 'UTF8'));
    IF denial IS NOT NULL THEN
        RETURN jsonb_build_object('call_id', call_id, 'denial', denial);
    END IF;
    RETURN jsonb_build_object('call_id', call_id, 'denial', '', 'model_id', m.id, 'upstream_model', m.upstream_model,
        'base_url', m.base_url, 'secret_ref', m.secret_ref, 'timeout_ms', m.timeout_ms,
        'max_output_tokens', req_out, 'deadline', due);
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- eacp.llm_finish settles call c (locked by the caller) as outcome and
-- returns {cost_amount, cost_unit, committed_amount} as text. A succeeded
-- call with known usage commits min(cost, reservation) and records its
-- usage; a provider error commits nothing; anything else commits the full
-- reservation.
CREATE FUNCTION eacp.llm_finish(c eacp.llm_calls, p_outcome text, p_status integer, p_input bigint,
                                p_cache_read bigint, p_cache_write bigint, p_output bigint, p_known boolean)
    RETURNS jsonb
    LANGUAGE plpgsql
    AS $$
DECLARE
    pr     eacp.model_prices%ROWTYPE;
    r      eacp.budget_reservations%ROWTYPE;
    cost   numeric;
    amt    numeric;
    known  boolean := p_known AND p_outcome = 'succeeded';
    done   eacp.llm_calls%ROWTYPE;
    comp   text := eacp.current_system_actor();
BEGIN
    IF known AND (p_input < 0 OR p_cache_read < 0 OR p_cache_write < 0 OR p_output < 0) THEN
        RAISE EXCEPTION 'token counts are never negative' USING ERRCODE = '22023';
    END IF;
    SELECT * INTO pr FROM eacp.model_prices WHERE tenant_id = c.tenant_id AND id = c.price_id;
    cost := CASE WHEN known THEN eacp.llm_cost(pr, p_input, p_cache_read, p_cache_write, p_output)
                 WHEN p_outcome = 'provider_error' THEN 0 END;
    SELECT * INTO r FROM eacp.budget_reservations
    WHERE tenant_id = c.tenant_id AND llm_call_id = c.id AND state = 'ACTIVE' FOR UPDATE;
    IF r.id IS NOT NULL THEN
        amt := CASE WHEN p_outcome = 'provider_error' THEN 0
                       WHEN cost IS NOT NULL THEN LEAST(cost, r.amount)
                       ELSE r.amount END;
    END IF;
    PERFORM set_config('eacp.llm_call', c.id::text, true);
    UPDATE eacp.llm_calls SET state = 'SETTLED', outcome = p_outcome, provider_status = p_status,
        input_tokens = CASE WHEN known THEN p_input END, cache_read_tokens = CASE WHEN known THEN p_cache_read END,
        cache_write_tokens = CASE WHEN known THEN p_cache_write END, output_tokens = CASE WHEN known THEN p_output END,
        cost_amount = cost, committed_amount = amt
    WHERE tenant_id = c.tenant_id AND id = c.id
    RETURNING * INTO done;
    IF r.id IS NOT NULL THEN
        UPDATE eacp.budget_reservations
        SET state = CASE WHEN p_outcome = 'provider_error' THEN 'RELEASED' ELSE 'COMMITTED' END,
            settle_reason = 'llm call ' || p_outcome
        WHERE tenant_id = r.tenant_id AND id = r.id;
    END IF;
    IF known THEN
        INSERT INTO eacp.usage_records (tenant_id, source, agent_id, provider, operation, observed_at, llm_call_id)
        VALUES (c.tenant_id, 'gateway', c.agent_id, c.provider, 'chat', now(), c.id);
    END IF;
    INSERT INTO eacp.audit_events (tenant_id, payload)
    VALUES (c.tenant_id, convert_to(jsonb_build_object(
        'v', 1,
        'actor', jsonb_build_object('kind', 'system', 'id', '00000000-0000-0000-0000-000000000000'::uuid,
                                    'component', comp),
        'action', 'llm.settled',
        'subject', jsonb_build_object('type', 'llm_call', 'id', c.id),
        'reason', p_outcome,
        'data', jsonb_strip_nulls(jsonb_build_object(
            'provider_status', p_status, 'input_tokens', done.input_tokens,
            'cache_read_tokens', done.cache_read_tokens, 'cache_write_tokens', done.cache_write_tokens,
            'output_tokens', done.output_tokens, 'cost_amount', done.cost_amount, 'cost_unit', done.cost_unit,
            'committed_amount', done.committed_amount)))::text, 'UTF8'));
    RETURN jsonb_build_object('cost_amount', done.cost_amount::text, 'cost_unit', done.cost_unit,
                              'committed_amount', done.committed_amount::text);
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- eacp.llm_settle is the gateway's settlement of one call (ADR-031 §3.5).
CREATE FUNCTION eacp.llm_settle(p_call uuid, p_outcome text, p_status integer, p_input bigint,
                                p_cache_read bigint, p_cache_write bigint, p_output bigint, p_known boolean)
    RETURNS jsonb
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    c eacp.llm_calls%ROWTYPE;
BEGIN
    IF eacp.current_system_actor() IS DISTINCT FROM 'llm_gateway' OR eacp.current_actor_id() IS NOT NULL
       OR eacp.current_agent_version_id() IS NOT NULL THEN
        RAISE EXCEPTION 'a call is settled by the LLM gateway' USING ERRCODE = '42501';
    END IF;
    IF p_outcome = 'abandoned' THEN
        RAISE EXCEPTION 'only the sweeper abandons a call' USING ERRCODE = '42501';
    END IF;
    IF p_outcome IS NULL OR p_outcome NOT IN ('succeeded', 'provider_error', 'usage_unknown', 'killed') THEN
        RAISE EXCEPTION 'unknown outcome %', p_outcome USING ERRCODE = '22023';
    END IF;
    SELECT * INTO c FROM eacp.llm_calls WHERE tenant_id = eacp.current_tenant_id() AND id = p_call FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'unknown call %', p_call USING ERRCODE = '23503';
    END IF;
    IF c.state <> 'ADMITTED' THEN
        RAISE EXCEPTION 'call % is %', p_call, c.state USING ERRCODE = '55000';
    END IF;
    RETURN eacp.llm_finish(c, p_outcome, p_status, p_input, p_cache_read, p_cache_write, p_output, p_known);
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- eacp.llm_sweep settles the tenant's overdue admitted calls as abandoned,
-- committing their full reservations. Replicas share it: a row another
-- transaction holds is skipped (ADR-029).
CREATE FUNCTION eacp.llm_sweep() RETURNS integer
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    c eacp.llm_calls%ROWTYPE;
    n integer := 0;
BEGIN
    IF eacp.current_system_actor() IS DISTINCT FROM 'llm_sweeper' OR eacp.current_actor_id() IS NOT NULL
       OR eacp.current_agent_version_id() IS NOT NULL THEN
        RAISE EXCEPTION 'overdue calls are swept by the LLM sweeper' USING ERRCODE = '42501';
    END IF;
    FOR c IN SELECT * FROM eacp.llm_calls
             WHERE tenant_id = eacp.current_tenant_id() AND state = 'ADMITTED' AND deadline < now()
             ORDER BY deadline LIMIT 1000 FOR UPDATE SKIP LOCKED LOOP
        PERFORM eacp.llm_finish(c, 'abandoned', NULL, NULL, NULL, NULL, NULL, false);
        n := n + 1;
    END LOOP;
    RETURN n;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The tenants with overdue admitted calls: ids only; eacp.llm_sweep
-- re-checks everything under RLS.
CREATE FUNCTION eacp.llm_sweep_tenants() RETURNS SETOF uuid
    LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT DISTINCT tenant_id FROM eacp.llm_calls WHERE state = 'ADMITTED' AND deadline < now()
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION eacp.llm_sweep_tenants() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.llm_sweep_tenants() TO eacp_app;
-- The ledger's writers run as the schema owner: every statement in them
-- names the tenant, so RLS and these filters keep them tenant-local.
REVOKE ALL ON FUNCTION eacp.llm_admit(jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.llm_admit(jsonb) TO eacp_app;
REVOKE ALL ON FUNCTION eacp.llm_settle(uuid, text, integer, bigint, bigint, bigint, bigint, boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.llm_settle(uuid, text, integer, bigint, bigint, bigint, bigint, boolean) TO eacp_app;
REVOKE ALL ON FUNCTION eacp.llm_sweep() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.llm_sweep() TO eacp_app;
REVOKE ALL ON FUNCTION eacp.llm_finish(eacp.llm_calls, text, integer, bigint, bigint, bigint, bigint, boolean)
    FROM PUBLIC;


-- +goose Down

DROP FUNCTION eacp.llm_sweep_tenants();
DROP FUNCTION eacp.llm_sweep();
DROP FUNCTION eacp.llm_settle(uuid, text, integer, bigint, bigint, bigint, bigint, boolean);
DROP FUNCTION eacp.llm_finish(eacp.llm_calls, text, integer, bigint, bigint, bigint, bigint, boolean);
DROP FUNCTION eacp.llm_admit(jsonb);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.finops_llm_daily(p_from timestamptz, p_to timestamptz)
    RETURNS TABLE (agent_id uuid, unit text, day date, reported numeric, billed numeric, effective numeric)
    LANGUAGE sql STABLE
    AS $$
    SELECT u.agent_id, u.cost_unit, (u.observed_at AT TIME ZONE 'UTC')::date,
           COALESCE(sum(u.cost_amount) FILTER (WHERE u.source = 'otel'), 0),
           COALESCE(sum(u.cost_amount) FILTER (WHERE u.source = 'provider_billing'), 0),
           GREATEST(COALESCE(sum(u.cost_amount) FILTER (WHERE u.source = 'otel'), 0),
                    COALESCE(sum(u.cost_amount) FILTER (WHERE u.source = 'provider_billing'), 0))
      FROM eacp.usage_records u
     WHERE u.billable AND u.cost_amount IS NOT NULL AND u.observed_at >= p_from AND u.observed_at < p_to
     GROUP BY 1, 2, 3
    $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.finops_evaluate() RETURNS integer
    LANGUAGE plpgsql
    AS $$
DECLARE
    tenant uuid := eacp.current_tenant_id();
    month_start timestamptz := date_trunc('month', now(), 'UTC');
    day_start timestamptz := date_trunc('day', now(), 'UTC');
    hour_start timestamptz := date_trunc('hour', now(), 'UTC') - interval '1 hour';
    raised integer := 0;
    k integer;
BEGIN
    IF eacp.current_system_actor() IS DISTINCT FROM 'finops' OR eacp.current_actor_id() IS NOT NULL THEN
        RAISE EXCEPTION 'the finops evaluator runs as the finops system actor' USING ERRCODE = '42501';
    END IF;

    WITH s AS (
        SELECT l.account_id, l.monthly_limit, b.unit,
               eacp.finops_account_spend(l.account_id, month_start, now()) AS spent
          FROM eacp.budget_soft_limits l
          JOIN eacp.budget_accounts b ON b.tenant_id = l.tenant_id AND b.id = l.account_id
         WHERE l.monthly_limit IS NOT NULL
    )
    INSERT INTO eacp.finops_alerts (tenant_id, kind, subject_type, subject_id, unit, period_start,
                                    observed, threshold, detail)
    SELECT tenant, t.kind, 'budget_account', s.account_id, s.unit, month_start, s.spent, s.monthly_limit,
           jsonb_build_object('ratio', round(s.spent / s.monthly_limit, 4), 'at', t.ratio)
      FROM s CROSS JOIN (VALUES ('soft_limit_warning', 0.8), ('soft_limit_exceeded', 1.0)) AS t(kind, ratio)
     WHERE s.spent >= s.monthly_limit * t.ratio
    ON CONFLICT (tenant_id, kind, subject_id, unit, period_start) DO NOTHING;
    GET DIAGNOSTICS k = ROW_COUNT;
    raised := raised + k;

    WITH spend AS (
        SELECT u.agent_id, u.cost_unit AS unit, date_trunc('hour', u.observed_at, 'UTC') AS h, u.cost_amount AS amount
          FROM eacp.usage_records u
         WHERE u.source = 'otel' AND u.billable AND u.cost_amount IS NOT NULL
           AND u.observed_at >= hour_start - interval '168 hours' AND u.observed_at < hour_start + interval '1 hour'
        UNION ALL
        SELECT a.agent_id, r.unit, date_trunc('hour', r.created_at, 'UTC'), r.amount
          FROM eacp.budget_reservations r
          JOIN eacp.actions a ON a.tenant_id = r.tenant_id AND a.id = r.action_id
         WHERE r.state <> 'RELEASED'
           AND r.created_at >= hour_start - interval '168 hours' AND r.created_at < hour_start + interval '1 hour'
    ), agg AS (
        SELECT agent_id, unit, COALESCE(sum(amount) FILTER (WHERE h = hour_start), 0) AS cur,
               COALESCE(sum(amount) FILTER (WHERE h < hour_start), 0) AS base
          FROM spend GROUP BY 1, 2
    )
    INSERT INTO eacp.finops_alerts (tenant_id, kind, subject_type, subject_id, unit, period_start,
                                    observed, threshold, detail)
    SELECT tenant, 'spend_anomaly', 'agent', g.agent_id, g.unit, hour_start, g.cur, 3 * g.base / 168,
           jsonb_build_object('baseline_hourly_mean', round(g.base / 168, 6), 'baseline_hours', 168, 'factor', 3)
      FROM agg g
     WHERE g.cur > 0 AND g.cur >= 3 * g.base / 168
       AND (EXISTS (SELECT 1 FROM eacp.usage_records u
                     WHERE u.agent_id = g.agent_id AND u.observed_at < hour_start - interval '24 hours')
            OR EXISTS (SELECT 1 FROM eacp.budget_reservations r
                         JOIN eacp.actions a ON a.tenant_id = r.tenant_id AND a.id = r.action_id
                        WHERE a.agent_id = g.agent_id AND r.created_at < hour_start - interval '24 hours'))
    ON CONFLICT (tenant_id, kind, subject_id, unit, period_start) DO NOTHING;
    GET DIAGNOSTICS k = ROW_COUNT;
    raised := raised + k;

    INSERT INTO eacp.finops_alerts (tenant_id, kind, subject_type, subject_id, unit, period_start, detail)
    SELECT tenant, 'unpriced_usage', 'agent', u.agent_id, '', day_start,
           jsonb_build_object('tokens', sum(u.input_tokens + u.output_tokens),
                              'models', jsonb_agg(DISTINCT COALESCE(u.model, '')))
      FROM eacp.usage_records u
     WHERE u.billable AND u.cost_amount IS NULL AND u.observed_at >= day_start
     GROUP BY u.agent_id
    ON CONFLICT (tenant_id, kind, subject_id, unit, period_start) DO NOTHING;
    GET DIAGNOSTICS k = ROW_COUNT;
    RETURN raised + k;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.release_version_metrics(p_version uuid, p_from timestamptz) RETURNS jsonb
    LANGUAGE sql STABLE
    AS $$
    WITH a AS (
        SELECT * FROM eacp.actions
        WHERE tenant_id = eacp.current_tenant_id() AND agent_version_id = p_version AND created_at >= p_from
    ), att AS (
        SELECT t.action_id, t.dispatched_at, t.completed_at, t.outcome
        FROM eacp.action_attempts t JOIN a ON a.tenant_id = t.tenant_id AND a.id = t.action_id
    ), cost AS (
        SELECT unit, sum(amount) AS total FROM (
            SELECT r.unit, COALESCE(r.committed_amount, r.amount) AS amount
            FROM eacp.budget_reservations r JOIN a ON a.tenant_id = r.tenant_id AND a.id = r.action_id
            WHERE r.state <> 'RELEASED'
            UNION ALL
            SELECT u.cost_unit, u.cost_amount FROM eacp.usage_records u
            WHERE u.tenant_id = eacp.current_tenant_id() AND u.agent_version_id = p_version
              AND u.source = 'otel' AND u.cost_amount IS NOT NULL AND u.observed_at >= p_from
        ) x GROUP BY unit
    ), n AS (SELECT count(*) AS actions FROM a)
    SELECT jsonb_build_object(
        'actions', n.actions,
        'denied', (SELECT count(*) FROM a WHERE state = 'DENIED'),
        'succeeded', (SELECT count(*) FROM a WHERE state = 'SUCCEEDED'),
        'failed', (SELECT count(*) FROM a WHERE state = 'FAILED'),
        'dispatched', (SELECT count(*) FROM a WHERE attempt_count > 0),
        'unknown', (SELECT count(*) FROM a WHERE state IN ('UNKNOWN_OUTCOME', 'RECONCILING', 'NEEDS_HUMAN_RESOLUTION')
                        OR EXISTS (SELECT 1 FROM eacp.reconciliation_checks c
                                   WHERE c.tenant_id = a.tenant_id AND c.action_id = a.id)
                        OR EXISTS (SELECT 1 FROM att WHERE att.action_id = a.id AND att.outcome = 'ambiguous')),
        'latency_p95_ms', (SELECT round((percentile_cont(0.95) WITHIN GROUP (
                               ORDER BY extract(epoch FROM completed_at - dispatched_at) * 1000))::numeric, 3)
                           FROM att WHERE completed_at IS NOT NULL),
        'cost', COALESCE((SELECT jsonb_object_agg(unit, total) FROM cost), '{}'),
        'cost_per_action', COALESCE((SELECT jsonb_object_agg(unit, round(total / NULLIF(n.actions, 0), 6))
                                     FROM cost), '{}'))
    FROM n
$$;
-- +goose StatementEnd


-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.usage_records_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.current_actor_id();
    ver uuid := eacp.current_agent_version_id();
    v eacp.agent_versions%ROWTYPE;
    c record;
BEGIN
    NEW.received_at := now();
    IF NEW.source = 'otel' THEN
        IF ver IS NULL OR a IS NOT NULL OR eacp.current_system_actor() IS NOT NULL THEN
            RAISE EXCEPTION 'usage spans are recorded by the agent that made them' USING ERRCODE = '42501';
        END IF;
        SELECT * INTO v FROM eacp.agent_versions WHERE tenant_id = NEW.tenant_id AND id = ver;
        IF NOT FOUND OR v.state IN ('RETIRED', 'REVOKED') THEN
            RAISE EXCEPTION 'agent version % cannot report usage', ver USING ERRCODE = '42501';
        END IF;
        IF NEW.observed_at < now() - interval '7 days' OR NEW.observed_at > now() + interval '5 minutes' THEN
            RAISE EXCEPTION 'usage must be observed within the last 7 days' USING ERRCODE = '23514';
        END IF;
        NEW.agent_version_id := v.id;
        NEW.agent_id := v.agent_id;
        NEW.external_id := NULL;
        NEW.reported_by := NULL;
        NEW.billable := NEW.operation IN ('chat', 'generate_content', 'text_completion', 'embeddings');
        NEW.price_id := NULL;
        NEW.cost_amount := NULL;
        NEW.cost_unit := NULL;
        IF NEW.model IS NOT NULL THEN
            SELECT * INTO c FROM eacp.usage_cost(NEW.provider, NEW.model, NEW.observed_at,
                                                 NEW.input_tokens, NEW.cache_read_tokens, NEW.output_tokens);
            NEW.price_id := c.price_id;
            NEW.cost_amount := c.amount;
            NEW.cost_unit := c.unit;
        END IF;
    ELSE
        IF a IS NULL OR ver IS NOT NULL THEN
            RAISE EXCEPTION 'billing lines are imported by an admin' USING ERRCODE = '42501';
        END IF;
        PERFORM eacp.assert_role(a, 'admin');
        IF NEW.cost_amount IS NULL OR NEW.cost_unit IS NULL THEN
            RAISE EXCEPTION 'a billing line carries the provider''s amount and unit' USING ERRCODE = '23514';
        END IF;
        IF NEW.observed_at > now() + interval '5 minutes' OR NEW.observed_at < now() - interval '400 days' THEN
            RAISE EXCEPTION 'a billing line is from the last 400 days' USING ERRCODE = '23514';
        END IF;
        NEW.agent_version_id := NULL;
        NEW.trace_id := NULL;
        NEW.span_id := NULL;
        NEW.price_id := NULL;
        NEW.billable := true;
        NEW.reported_by := a;
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

DROP INDEX eacp.usage_records_llm_call;
ALTER TABLE eacp.usage_records DROP CONSTRAINT usage_records_gateway;
ALTER TABLE eacp.usage_records DROP CONSTRAINT usage_records_llm_call_fkey;
ALTER TABLE eacp.usage_records DROP COLUMN llm_call_id;
ALTER TABLE eacp.usage_records DROP CONSTRAINT usage_records_source_check;
ALTER TABLE eacp.usage_records ADD CONSTRAINT usage_records_source_check
    CHECK (source IN ('otel', 'provider_billing'));

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.audit_budget_reservation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    who jsonb := eacp.actor_context();
BEGIN
    IF TG_OP = 'UPDATE' AND NEW.state IS NOT DISTINCT FROM OLD.state THEN
        RETURN NULL;
    END IF;
    INSERT INTO eacp.audit_events (tenant_id, payload)
    VALUES (NEW.tenant_id, convert_to(jsonb_build_object(
        'v', 1,
        'actor', who - 'component',
        'action', CASE NEW.state WHEN 'ACTIVE' THEN 'budget.reserved'
                                 WHEN 'COMMITTED' THEN 'budget.committed' ELSE 'budget.released' END,
        'subject', jsonb_build_object('type', 'budget_reservation', 'id', NEW.id),
        'reason', COALESCE(NEW.settle_reason, 'reserved at release'),
        'data', jsonb_strip_nulls(jsonb_build_object(
            'component', who->>'component',
            'action_id', NEW.action_id,
            'account_id', NEW.account_id,
            'contract_id', NEW.contract_id,
            'unit', NEW.unit,
            'amount', NEW.amount,
            'committed_amount', NEW.committed_amount,
            'expires_at', NEW.expires_at)))::text, 'UTF8'));
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.budget_reservations_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    who  jsonb := eacp.actor_context();
    act  eacp.actions%ROWTYPE;
    ct   eacp.tool_contracts%ROWTYPE;
    acct eacp.budget_accounts%ROWTYPE;
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF who->>'kind' = 'principal' THEN
            RAISE EXCEPTION 'budget is reserved only by the release' USING ERRCODE = '42501';
        END IF;
        SELECT * INTO act FROM eacp.actions WHERE tenant_id = NEW.tenant_id AND id = NEW.action_id FOR UPDATE;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'unknown action %', NEW.action_id USING ERRCODE = '23503';
        END IF;
        PERFORM eacp.assert_action_agent(who, act.agent_id);
        IF act.state <> 'AUTHORIZED' OR act.not_after <= now() THEN
            RAISE EXCEPTION 'budget is reserved only for an AUTHORIZED, unexpired action' USING ERRCODE = '55000';
        END IF;
        SELECT c.* INTO ct FROM eacp.tools t
        JOIN eacp.tool_contracts c ON c.tenant_id = t.tenant_id AND c.id = t.active_contract_id
        WHERE t.tenant_id = NEW.tenant_id AND t.id = act.tool_id AND c.id = NEW.contract_id
        FOR SHARE OF t, c;
        IF NOT FOUND OR ct.revoked_at IS NOT NULL OR ct.cost_unit IS NULL THEN
            RAISE EXCEPTION 'a reservation is priced by the tool''s active, budgeted contract' USING ERRCODE = '55000';
        END IF;
        SELECT * INTO acct FROM eacp.budget_accounts WHERE tenant_id = NEW.tenant_id AND id = NEW.account_id;
        IF NOT FOUND OR acct.agent_id IS DISTINCT FROM act.agent_id OR acct.unit <> ct.cost_unit THEN
            RAISE EXCEPTION 'a reservation draws on the agent''s account in the contract''s unit' USING ERRCODE = '55000';
        END IF;
        IF NEW.amount IS DISTINCT FROM eacp.action_cost(ct, act.enforced_payload::jsonb) THEN
            RAISE EXCEPTION 'a reservation is exactly the action''s cost' USING ERRCODE = '55000';
        END IF;
        IF NEW.state <> 'ACTIVE' OR num_nonnulls(NEW.committed_amount, NEW.settled_at, NEW.settle_reason,
                                                 NEW.folded_at) > 0 THEN
            RAISE EXCEPTION 'a reservation starts ACTIVE' USING ERRCODE = '23514';
        END IF;
        NEW.unit := ct.cost_unit;
        NEW.created_at := now();
        NEW.expires_at := act.not_after;  -- the reservation TTL (§47)
        RETURN NEW;
    END IF;

    IF NEW.state IS DISTINCT FROM OLD.state THEN
        IF (to_jsonb(NEW) - ARRAY['state', 'committed_amount', 'settled_at', 'settle_reason'])
           IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['state', 'committed_amount', 'settled_at', 'settle_reason']) THEN
            RAISE EXCEPTION 'settling a reservation changes only its settlement' USING ERRCODE = '55000';
        END IF;
        IF OLD.state <> 'ACTIVE' THEN
            RAISE EXCEPTION 'reservation % is already %', OLD.id, OLD.state USING ERRCODE = '55000';
        END IF;
        SELECT * INTO act FROM eacp.actions WHERE tenant_id = NEW.tenant_id AND id = NEW.action_id;
        IF NEW.state = 'COMMITTED' THEN
            -- Commit the actual; no connector reports one yet, so the estimate.
            IF act.state <> 'SUCCEEDED' THEN
                RAISE EXCEPTION 'budget is committed only when its action succeeded' USING ERRCODE = '55000';
            END IF;
            NEW.committed_amount := COALESCE(NEW.committed_amount, OLD.amount);
        ELSE
            -- No effect happened (terminal without success), or the action
            -- is back at the release boundary and is being repriced.
            IF act.state NOT IN ('FAILED', 'CANCELLED', 'EXPIRED', 'DENIED', 'AUTHORIZED') THEN
                RAISE EXCEPTION 'budget is released only when its action had no effect' USING ERRCODE = '55000';
            END IF;
            NEW.committed_amount := 0;
        END IF;
        PERFORM eacp.require_reason(NEW.settle_reason, 'a settlement');
        NEW.settled_at := now();
        RETURN NEW;
    END IF;

    IF OLD.folded_at IS NULL AND NEW.folded_at IS NOT NULL
       AND (to_jsonb(NEW) - 'folded_at') IS NOT DISTINCT FROM (to_jsonb(OLD) - 'folded_at') THEN
        NEW.folded_at := now();
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'a reservation changes only by settling, then folding' USING ERRCODE = '55000';
END
$$;
-- +goose StatementEnd

DROP INDEX eacp.budget_reservations_llm_call;
ALTER TABLE eacp.budget_reservations DROP CONSTRAINT budget_reservations_subject;
ALTER TABLE eacp.budget_reservations DROP CONSTRAINT budget_reservations_llm_call_fkey;
ALTER TABLE eacp.budget_reservations DROP COLUMN llm_call_id;
ALTER TABLE eacp.budget_reservations ALTER COLUMN contract_id SET NOT NULL;
ALTER TABLE eacp.budget_reservations ALTER COLUMN action_id SET NOT NULL;

DROP FUNCTION eacp.llm_call_killed(uuid);
DROP FUNCTION eacp.llm_killed(uuid, uuid, uuid, uuid, uuid);
DROP FUNCTION eacp.llm_cost(eacp.model_prices, bigint, bigint, bigint, bigint);
DROP FUNCTION eacp.llm_estimate(eacp.model_prices, bigint, bigint);
DROP TABLE eacp.llm_calls;
DROP FUNCTION eacp.llm_calls_guard();
DROP FUNCTION eacp.llm_ledger_context(uuid);

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
