-- Phase 18 (ADR-025): Agent FinOps. LLM usage is ingested (OTel GenAI spans
-- from the agent's own key, or provider billing imported by an admin),
-- priced here from an insert-only rate card, and aggregated with ADR-012
-- tool spend. Soft limits and alerts observe; only hard limits block.
-- +goose Up

CREATE TABLE eacp.model_prices (
    tenant_id             uuid          NOT NULL REFERENCES eacp.tenants (id),
    id                    uuid          NOT NULL DEFAULT gen_random_uuid(),
    provider              text          NOT NULL CHECK (provider ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
    model                 text          NOT NULL CHECK (length(model) BETWEEN 1 AND 256 AND model !~ '[[:cntrl:]]'),
    unit                  text          NOT NULL CHECK (unit ~ '^[A-Z][A-Z0-9_]{0,15}$'),
    input_per_mtok        numeric(21,6) NOT NULL CHECK (input_per_mtok >= 0),
    cached_input_per_mtok numeric(21,6) CHECK (cached_input_per_mtok >= 0),
    output_per_mtok       numeric(21,6) NOT NULL CHECK (output_per_mtok >= 0),
    effective_from        timestamptz   NOT NULL DEFAULT now(),
    reason                text          NOT NULL CHECK (length(reason) <= 1024),
    created_by            uuid,
    created_at            timestamptz   NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, provider, model, effective_from),
    FOREIGN KEY (tenant_id, created_by) REFERENCES eacp.principals (tenant_id, id)
);

CREATE TABLE eacp.usage_records (
    tenant_id         uuid          NOT NULL REFERENCES eacp.tenants (id),
    id                uuid          NOT NULL DEFAULT gen_random_uuid(),
    source            text          NOT NULL CHECK (source IN ('otel', 'provider_billing')),
    agent_id          uuid          NOT NULL,
    agent_version_id  uuid,
    trace_id          text          CHECK (trace_id ~ '^[0-9a-f]{32}$'),
    span_id           text          CHECK (span_id ~ '^[0-9a-f]{16}$'),
    external_id       text          CHECK (length(external_id) BETWEEN 1 AND 256 AND external_id !~ '[[:cntrl:]]'),
    provider          text          NOT NULL CHECK (provider ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
    operation         text          NOT NULL CHECK (operation ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
    model             text          CHECK (length(model) BETWEEN 1 AND 256 AND model !~ '[[:cntrl:]]'),
    input_tokens      bigint        NOT NULL DEFAULT 0 CHECK (input_tokens BETWEEN 0 AND 1000000000000),
    cache_read_tokens bigint        NOT NULL DEFAULT 0 CHECK (cache_read_tokens BETWEEN 0 AND 1000000000000),
    output_tokens     bigint        NOT NULL DEFAULT 0 CHECK (output_tokens BETWEEN 0 AND 1000000000000),
    billable          boolean       NOT NULL DEFAULT false,
    observed_at       timestamptz   NOT NULL,
    received_at       timestamptz   NOT NULL DEFAULT now(),
    price_id          uuid,
    cost_amount       numeric(21,6) CHECK (cost_amount >= 0),
    cost_unit         text          CHECK (cost_unit ~ '^[A-Z][A-Z0-9_]{0,15}$'),
    reported_by       uuid,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, agent_id) REFERENCES eacp.agents (tenant_id, id),
    FOREIGN KEY (tenant_id, agent_version_id) REFERENCES eacp.agent_versions (tenant_id, id),
    FOREIGN KEY (tenant_id, price_id) REFERENCES eacp.model_prices (tenant_id, id),
    FOREIGN KEY (tenant_id, reported_by) REFERENCES eacp.principals (tenant_id, id),
    CHECK ((source = 'otel') = (trace_id IS NOT NULL AND span_id IS NOT NULL AND agent_version_id IS NOT NULL)),
    CHECK ((source = 'provider_billing') = (external_id IS NOT NULL AND reported_by IS NOT NULL)),
    CHECK ((cost_amount IS NULL) = (cost_unit IS NULL))
);
CREATE UNIQUE INDEX usage_records_span ON eacp.usage_records (tenant_id, trace_id, span_id) WHERE source = 'otel';
CREATE UNIQUE INDEX usage_records_billing ON eacp.usage_records (tenant_id, external_id) WHERE source = 'provider_billing';
CREATE INDEX usage_records_agent_time ON eacp.usage_records (tenant_id, agent_id, observed_at);
CREATE INDEX usage_records_time ON eacp.usage_records (tenant_id, observed_at);

CREATE TABLE eacp.budget_soft_limits (
    tenant_id     uuid          NOT NULL REFERENCES eacp.tenants (id),
    id            uuid          NOT NULL DEFAULT gen_random_uuid(),
    account_id    uuid          NOT NULL,
    monthly_limit numeric(21,6) CHECK (monthly_limit > 0),
    reason        text          NOT NULL CHECK (length(reason) <= 1024),
    set_by        uuid,
    set_at        timestamptz   NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, account_id),
    FOREIGN KEY (tenant_id, account_id) REFERENCES eacp.budget_accounts (tenant_id, id),
    FOREIGN KEY (tenant_id, set_by) REFERENCES eacp.principals (tenant_id, id)
);

CREATE TABLE eacp.finops_alerts (
    tenant_id       uuid          NOT NULL REFERENCES eacp.tenants (id),
    id              uuid          NOT NULL DEFAULT gen_random_uuid(),
    kind            text          NOT NULL CHECK (kind IN
                        ('soft_limit_warning', 'soft_limit_exceeded', 'spend_anomaly', 'unpriced_usage')),
    subject_type    text          NOT NULL CHECK (subject_type IN ('budget_account', 'agent')),
    subject_id      uuid          NOT NULL,
    unit            text          NOT NULL DEFAULT '' CHECK (unit = '' OR unit ~ '^[A-Z][A-Z0-9_]{0,15}$'),
    period_start    timestamptz   NOT NULL,
    observed        numeric(21,6),
    threshold       numeric(21,6),
    detail          jsonb         NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(detail) = 'object'),
    created_at      timestamptz   NOT NULL DEFAULT now(),
    acknowledged_by uuid,
    acknowledged_at timestamptz,
    ack_reason      text          CHECK (length(ack_reason) <= 1024),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, kind, subject_id, unit, period_start),
    FOREIGN KEY (tenant_id, acknowledged_by) REFERENCES eacp.principals (tenant_id, id),
    CHECK ((acknowledged_at IS NULL) = (acknowledged_by IS NULL)),
    CHECK ((acknowledged_at IS NULL) = (ack_reason IS NULL))
);
CREATE INDEX finops_alerts_open ON eacp.finops_alerts (tenant_id, created_at) WHERE acknowledged_at IS NULL;

-- +goose StatementBegin
-- A price is set by an admin and takes effect no earlier than now: past
-- usage is never repriced. Only the schema owner (bootstrap) may seed a
-- rate card with an earlier effective time.
CREATE FUNCTION eacp.model_prices_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.actor();
BEGIN
    NEW.created_at := now();
    IF a IS NULL THEN
        NEW.created_by := NULL;
        RETURN NEW;
    END IF;
    PERFORM eacp.assert_role(a, 'admin');
    PERFORM eacp.require_reason(NEW.reason, 'a price');
    IF NEW.effective_from < now() THEN
        RAISE EXCEPTION 'a price takes effect from now on, never retroactively' USING ERRCODE = '23514';
    END IF;
    NEW.created_by := a;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The cost of billable usage under the price in effect at p_at, rounded up
-- to six decimals, or NULL when no price exists (ADR-025 §3).
CREATE FUNCTION eacp.usage_cost(p_provider text, p_model text, p_at timestamptz,
                                p_input bigint, p_cache bigint, p_output bigint,
                                OUT price_id uuid, OUT amount numeric, OUT unit text)
    LANGUAGE plpgsql STABLE
    AS $$
DECLARE
    p eacp.model_prices%ROWTYPE;
    raw numeric;
BEGIN
    SELECT * INTO p FROM eacp.model_prices
     WHERE tenant_id = eacp.current_tenant_id() AND provider = p_provider AND model = p_model
       AND effective_from <= p_at
     ORDER BY effective_from DESC LIMIT 1;
    IF NOT FOUND THEN
        RETURN;
    END IF;
    IF p_cache <= p_input THEN
        raw := (p_input - p_cache) * p.input_per_mtok + p_cache * COALESCE(p.cached_input_per_mtok, p.input_per_mtok);
    ELSE
        -- Only consistent if input excluded the cache: charge both.
        raw := p_input * p.input_per_mtok + p_cache * COALESCE(p.cached_input_per_mtok, p.input_per_mtok);
    END IF;
    raw := raw + p_output * p.output_per_mtok;
    price_id := p.id;
    amount := ceil(raw) / 1000000;
    unit := p.unit;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- OTel usage is recorded only by the agent whose key it came in with; its
-- identity and cost come from here, never from the caller. Billing lines are
-- imported by an admin with the provider's amount.
CREATE FUNCTION eacp.usage_records_guard() RETURNS trigger
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

-- +goose StatementBegin
CREATE FUNCTION eacp.budget_soft_limits_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.current_actor_id();
BEGIN
    IF a IS NULL THEN
        RAISE EXCEPTION 'a soft limit is set by a principal' USING ERRCODE = '42501';
    END IF;
    PERFORM eacp.assert_role(a, 'admin');
    PERFORM eacp.require_reason(NEW.reason, 'a soft limit');
    IF TG_OP = 'UPDATE' AND NEW.account_id IS DISTINCT FROM OLD.account_id THEN
        RAISE EXCEPTION 'a soft limit belongs to one account' USING ERRCODE = '55000';
    END IF;
    NEW.set_by := a;
    NEW.set_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Alerts are raised only by the finops evaluator and acknowledged once by
-- an operator or admin, with a reason.
CREATE FUNCTION eacp.finops_alerts_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.current_actor_id();
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF eacp.current_system_actor() IS DISTINCT FROM 'finops' OR a IS NOT NULL
           OR eacp.current_agent_version_id() IS NOT NULL THEN
            RAISE EXCEPTION 'alerts are raised by the finops evaluator' USING ERRCODE = '42501';
        END IF;
        IF NEW.acknowledged_at IS NOT NULL OR NEW.acknowledged_by IS NOT NULL OR NEW.ack_reason IS NOT NULL THEN
            RAISE EXCEPTION 'an alert starts unacknowledged' USING ERRCODE = '23514';
        END IF;
        NEW.created_at := now();
        RETURN NEW;
    END IF;
    IF a IS NULL THEN
        RAISE EXCEPTION 'an alert is acknowledged by a principal' USING ERRCODE = '42501';
    END IF;
    PERFORM eacp.assert_role(a, 'operator', 'admin');
    IF OLD.acknowledged_at IS NOT NULL THEN
        RAISE EXCEPTION 'alert % is already acknowledged', OLD.id USING ERRCODE = '55000';
    END IF;
    PERFORM eacp.require_reason(NEW.ack_reason, 'an acknowledgement');
    IF (to_jsonb(NEW) - 'acknowledged_by' - 'acknowledged_at' - 'ack_reason')
       IS DISTINCT FROM (to_jsonb(OLD) - 'acknowledged_by' - 'acknowledged_at' - 'ack_reason') THEN
        RAISE EXCEPTION 'an acknowledgement changes nothing else' USING ERRCODE = '55000';
    END IF;
    NEW.acknowledged_by := a;
    NEW.acknowledged_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.finops_alerts_audit() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    INSERT INTO eacp.audit_events (tenant_id, payload)
    VALUES (NEW.tenant_id, convert_to(jsonb_build_object(
        'v', 1,
        'actor', CASE WHEN TG_OP = 'INSERT'
                      THEN jsonb_build_object('kind', 'system', 'id', '00000000-0000-0000-0000-000000000000'::uuid,
                                              'component', 'finops')
                      ELSE jsonb_build_object('kind', 'principal', 'id', NEW.acknowledged_by) END,
        'action', CASE TG_OP WHEN 'INSERT' THEN 'finops_alert.raised' ELSE 'finops_alert.acknowledged' END,
        'subject', jsonb_build_object('type', 'finops_alert', 'id', NEW.id),
        'reason', CASE TG_OP WHEN 'INSERT' THEN NEW.kind ELSE NEW.ack_reason END,
        'data', to_jsonb(NEW))::text, 'UTF8'));
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The journal entry of a price, a billing line or a soft limit carries the
-- row's own reason, and its actor (the schema owner seeding shows as system).
CREATE FUNCTION eacp.finops_row_audit() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.current_actor_id();
    data jsonb := to_jsonb(NEW);
BEGIN
    IF TG_OP = 'UPDATE' THEN
        SELECT COALESCE(jsonb_object_agg(e.key, jsonb_build_object('from', to_jsonb(OLD) -> e.key, 'to', e.value)), '{}')
          INTO data
          FROM jsonb_each(to_jsonb(NEW)) AS e
         WHERE to_jsonb(OLD) -> e.key IS DISTINCT FROM e.value;
    END IF;
    INSERT INTO eacp.audit_events (tenant_id, payload)
    VALUES (NEW.tenant_id, convert_to(jsonb_build_object(
        'v', 1,
        'actor', jsonb_build_object(
            'kind', CASE WHEN a IS NULL THEN 'system' ELSE 'principal' END,
            'id', COALESCE(a, '00000000-0000-0000-0000-000000000000'::uuid)),
        'action', TG_TABLE_NAME || '.' || lower(TG_OP),
        'subject', jsonb_build_object('type', TG_TABLE_NAME, 'id', NEW.id),
        'reason', COALESCE(to_jsonb(NEW) ->> 'reason', 'billing import ' || (to_jsonb(NEW) ->> 'external_id')),
        'data', data)::text, 'UTF8'));
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER model_prices_guard BEFORE INSERT ON eacp.model_prices
    FOR EACH ROW EXECUTE FUNCTION eacp.model_prices_guard();
CREATE TRIGGER zz_audit AFTER INSERT ON eacp.model_prices
    FOR EACH ROW EXECUTE FUNCTION eacp.finops_row_audit();
CREATE TRIGGER usage_records_guard BEFORE INSERT ON eacp.usage_records
    FOR EACH ROW EXECUTE FUNCTION eacp.usage_records_guard();
CREATE TRIGGER zz_audit AFTER INSERT ON eacp.usage_records
    FOR EACH ROW WHEN (NEW.source = 'provider_billing') EXECUTE FUNCTION eacp.finops_row_audit();
CREATE TRIGGER budget_soft_limits_guard BEFORE INSERT OR UPDATE ON eacp.budget_soft_limits
    FOR EACH ROW EXECUTE FUNCTION eacp.budget_soft_limits_guard();
CREATE TRIGGER zz_audit AFTER INSERT OR UPDATE ON eacp.budget_soft_limits
    FOR EACH ROW EXECUTE FUNCTION eacp.finops_row_audit();
CREATE TRIGGER finops_alerts_guard BEFORE INSERT OR UPDATE ON eacp.finops_alerts
    FOR EACH ROW EXECUTE FUNCTION eacp.finops_alerts_guard();
CREATE TRIGGER zz_audit AFTER INSERT OR UPDATE ON eacp.finops_alerts
    FOR EACH ROW EXECUTE FUNCTION eacp.finops_alerts_audit();

-- ------------------------------------------------------------ spend

-- +goose StatementBegin
-- Priced, billable LLM spend per agent, unit and UTC day in [p_from, p_to).
-- Both sources may describe the same calls, so the effective spend is the
-- greater of the two (ADR-025 §4).
CREATE FUNCTION eacp.finops_llm_daily(p_from timestamptz, p_to timestamptz)
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
-- Spend per agent and unit in [p_from, p_to): committed tool spend (by
-- settlement), held tool spend (ACTIVE reservations, by creation) and LLM
-- spend (ADR-025 §5).
CREATE FUNCTION eacp.finops_agent_spend(p_from timestamptz, p_to timestamptz)
    RETURNS TABLE (agent_id uuid, unit text, tool_committed numeric, tool_held numeric,
                   llm_reported numeric, llm_billed numeric, llm_effective numeric)
    LANGUAGE sql STABLE
    AS $$
    WITH tool AS (
        SELECT a.agent_id, r.unit,
               COALESCE(sum(r.committed_amount) FILTER (WHERE r.state = 'COMMITTED'
                        AND r.settled_at >= p_from AND r.settled_at < p_to), 0) AS committed,
               COALESCE(sum(r.amount) FILTER (WHERE r.state = 'ACTIVE'
                        AND r.created_at >= p_from AND r.created_at < p_to), 0) AS held
          FROM eacp.budget_reservations r
          JOIN eacp.actions a ON a.tenant_id = r.tenant_id AND a.id = r.action_id
         WHERE (r.settled_at >= p_from AND r.settled_at < p_to)
            OR (r.state = 'ACTIVE' AND r.created_at >= p_from AND r.created_at < p_to)
         GROUP BY 1, 2
    ), llm AS (
        SELECT d.agent_id, d.unit, sum(d.reported) AS reported, sum(d.billed) AS billed, sum(d.effective) AS effective
          FROM eacp.finops_llm_daily(p_from, p_to) d
         GROUP BY 1, 2
    )
    SELECT COALESCE(t.agent_id, l.agent_id), COALESCE(t.unit, l.unit),
           COALESCE(t.committed, 0), COALESCE(t.held, 0),
           COALESCE(l.reported, 0), COALESCE(l.billed, 0), COALESCE(l.effective, 0)
      FROM tool t
      FULL JOIN llm l ON l.agent_id = t.agent_id AND l.unit = t.unit
    $$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Spend of an account's subtree in its unit: every agent leaf below it.
CREATE FUNCTION eacp.finops_account_spend(p_account uuid, p_from timestamptz, p_to timestamptz)
    RETURNS numeric
    LANGUAGE sql STABLE
    AS $$
    WITH RECURSIVE sub AS (
        SELECT id, unit, agent_id FROM eacp.budget_accounts
         WHERE tenant_id = eacp.current_tenant_id() AND id = p_account
        UNION ALL
        SELECT c.id, c.unit, c.agent_id FROM eacp.budget_accounts c
          JOIN sub ON c.tenant_id = eacp.current_tenant_id() AND c.parent_id = sub.id
    )
    SELECT COALESCE(sum(s.tool_committed + s.tool_held + s.llm_effective), 0)
      FROM sub
      JOIN eacp.finops_agent_spend(p_from, p_to) s ON s.agent_id = sub.agent_id AND s.unit = sub.unit
    $$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The evaluator (ADR-025 §8): soft limits for the month, spend anomalies for
-- the last complete hour and unpriced usage for the day. Each alert is
-- unique per kind, subject, unit and period, so re-evaluation is idempotent.
-- It returns the number of alerts raised.
CREATE FUNCTION eacp.finops_evaluate() RETURNS integer
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

-- ------------------------------------------------------------ access

ALTER TABLE eacp.model_prices ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.model_prices FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.model_prices USING (tenant_id = eacp.current_tenant_id());
ALTER TABLE eacp.usage_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.usage_records FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.usage_records USING (tenant_id = eacp.current_tenant_id());
CREATE POLICY owner_scan ON eacp.usage_records FOR SELECT TO CURRENT_USER USING (true);
ALTER TABLE eacp.budget_soft_limits ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.budget_soft_limits FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.budget_soft_limits USING (tenant_id = eacp.current_tenant_id());
CREATE POLICY owner_scan ON eacp.budget_soft_limits FOR SELECT TO CURRENT_USER USING (true);
ALTER TABLE eacp.finops_alerts ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.finops_alerts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.finops_alerts USING (tenant_id = eacp.current_tenant_id());

REVOKE UPDATE ON eacp.model_prices FROM eacp_app;
REVOKE UPDATE ON eacp.usage_records FROM eacp_app;
REVOKE UPDATE ON eacp.budget_soft_limits FROM eacp_app;
GRANT UPDATE (monthly_limit, reason) ON eacp.budget_soft_limits TO eacp_app;
REVOKE UPDATE ON eacp.finops_alerts FROM eacp_app;
GRANT UPDATE (ack_reason, acknowledged_at) ON eacp.finops_alerts TO eacp_app;

-- +goose StatementBegin
-- The evaluator's cross-tenant hint: tenant ids only, for tenants with
-- recent usage, a soft limit or recent actions (whose reservations count).
CREATE FUNCTION eacp.finops_tenants() RETURNS TABLE (tenant_id uuid)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT DISTINCT t FROM (
        SELECT u.tenant_id FROM eacp.usage_records u WHERE u.observed_at >= now() - interval '8 days'
        UNION SELECT l.tenant_id FROM eacp.budget_soft_limits l
        UNION SELECT a.tenant_id FROM eacp.actions a WHERE a.state_changed_at >= now() - interval '8 days'
    ) x(t) ORDER BY 1
    $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION eacp.finops_tenants() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.finops_tenants() TO eacp_app;

-- +goose Down
DROP FUNCTION eacp.finops_tenants();
DROP FUNCTION eacp.finops_evaluate();
DROP FUNCTION eacp.finops_account_spend(uuid, timestamptz, timestamptz);
DROP FUNCTION eacp.finops_agent_spend(timestamptz, timestamptz);
DROP FUNCTION eacp.finops_llm_daily(timestamptz, timestamptz);
DROP TABLE eacp.finops_alerts;
DROP TABLE eacp.budget_soft_limits;
DROP TABLE eacp.usage_records;
DROP TABLE eacp.model_prices;
DROP FUNCTION eacp.finops_alerts_audit();
DROP FUNCTION eacp.finops_row_audit();
DROP FUNCTION eacp.finops_alerts_guard();
DROP FUNCTION eacp.budget_soft_limits_guard();
DROP FUNCTION eacp.usage_records_guard();
DROP FUNCTION eacp.usage_cost(text, text, timestamptz, bigint, bigint, bigint);
DROP FUNCTION eacp.model_prices_guard();
