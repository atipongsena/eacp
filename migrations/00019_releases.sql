-- Phase 19 (ADR-018): agent releases. A release moves an agent from its
-- stable ACTIVE version to a candidate through evaluation, replay, shadow
-- and a canary cohort, with guardrails and rollback. PostgreSQL enforces
-- every rule; replay and shadow are observations that never execute.
-- +goose Up

CREATE TABLE eacp.agent_releases (
    tenant_id            uuid          NOT NULL REFERENCES eacp.tenants (id),
    id                   uuid          NOT NULL DEFAULT gen_random_uuid(),
    agent_id             uuid          NOT NULL,
    stable_version_id    uuid          NOT NULL,
    candidate_version_id uuid          NOT NULL,
    state                text          NOT NULL DEFAULT 'EVALUATING'
                             CHECK (state IN ('EVALUATING', 'SHADOW', 'CANARY', 'PROMOTED', 'ROLLED_BACK')),
    -- The plan, fixed at opening.
    required_suites      text[]        NOT NULL,
    min_replay_cases     integer       NOT NULL DEFAULT 20 CHECK (min_replay_cases BETWEEN 0 AND 100000),
    min_replay_agreement numeric(5,4)  NOT NULL DEFAULT 0.9 CHECK (min_replay_agreement BETWEEN 0 AND 1),
    min_shadow_cases     integer       NOT NULL DEFAULT 20 CHECK (min_shadow_cases BETWEEN 0 AND 100000),
    min_shadow_agreement numeric(5,4)  NOT NULL DEFAULT 0.9 CHECK (min_shadow_agreement BETWEEN 0 AND 1),
    canary_steps         integer[]     NOT NULL DEFAULT '{100,500,2500,5000,10000}',
    min_canary_actions   integer       NOT NULL DEFAULT 20 CHECK (min_canary_actions BETWEEN 1 AND 100000),
    max_denial_increase  numeric(5,4)  NOT NULL DEFAULT 0.05 CHECK (max_denial_increase BETWEEN 0 AND 1),
    max_failure_increase numeric(5,4)  NOT NULL DEFAULT 0.05 CHECK (max_failure_increase BETWEEN 0 AND 1),
    max_unknown_increase numeric(5,4)  NOT NULL DEFAULT 0.01 CHECK (max_unknown_increase BETWEEN 0 AND 1),
    max_latency_ratio    numeric(6,3)  NOT NULL DEFAULT 1.5 CHECK (max_latency_ratio BETWEEN 1 AND 100),
    max_cost_ratio       numeric(6,3)  NOT NULL DEFAULT 1.5 CHECK (max_cost_ratio BETWEEN 1 AND 100),
    reason               text          NOT NULL CHECK (length(reason) <= 1024),
    -- Progress, set by the database.
    canary_bp            integer       CHECK (canary_bp BETWEEN 1 AND 10000),
    stage_started_at     timestamptz   NOT NULL,
    created_by           uuid          NOT NULL,
    created_at           timestamptz   NOT NULL,
    changed_by           uuid,
    changed_at           timestamptz,
    change_reason        text          CHECK (length(change_reason) <= 1024),
    closed_xact          xid8,
    breaches             jsonb,
    PRIMARY KEY (tenant_id, id),
    CHECK (candidate_version_id <> stable_version_id),
    CHECK ((state IN ('PROMOTED', 'ROLLED_BACK')) = (closed_xact IS NOT NULL)),
    CHECK (state NOT IN ('EVALUATING', 'SHADOW') OR canary_bp IS NULL),
    CHECK (state NOT IN ('CANARY', 'PROMOTED') OR canary_bp IS NOT NULL),
    FOREIGN KEY (tenant_id, agent_id) REFERENCES eacp.agents (tenant_id, id),
    FOREIGN KEY (tenant_id, stable_version_id) REFERENCES eacp.agent_versions (tenant_id, id),
    FOREIGN KEY (tenant_id, candidate_version_id) REFERENCES eacp.agent_versions (tenant_id, id),
    FOREIGN KEY (tenant_id, created_by) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, changed_by) REFERENCES eacp.principals (tenant_id, id)
);
CREATE UNIQUE INDEX agent_releases_one_open ON eacp.agent_releases (tenant_id, agent_id)
    WHERE state IN ('EVALUATING', 'SHADOW', 'CANARY');
CREATE INDEX agent_releases_candidate ON eacp.agent_releases (tenant_id, candidate_version_id);
CREATE INDEX agent_releases_canary ON eacp.agent_releases (state) WHERE state = 'CANARY';

CREATE TABLE eacp.agent_release_evaluations (
    tenant_id      uuid          NOT NULL,
    id             uuid          NOT NULL DEFAULT gen_random_uuid(),
    seq            bigint        GENERATED ALWAYS AS IDENTITY,
    release_id     uuid          NOT NULL,
    suite          text          NOT NULL CHECK (suite ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
    score          numeric(12,6) NOT NULL,
    threshold      numeric(12,6) NOT NULL,
    passed         boolean       GENERATED ALWAYS AS (score >= threshold) STORED,
    dataset_digest text          NOT NULL CHECK (dataset_digest ~ '^[0-9a-f]{64}$'),
    evidence_ref   text          NOT NULL CHECK (btrim(evidence_ref) <> '' AND length(evidence_ref) <= 1024
                                                 AND evidence_ref !~ '[[:cntrl:]]'),
    recorded_by    uuid          NOT NULL,
    recorded_at    timestamptz   NOT NULL,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, release_id) REFERENCES eacp.agent_releases (tenant_id, id),
    FOREIGN KEY (tenant_id, recorded_by) REFERENCES eacp.principals (tenant_id, id)
);
CREATE INDEX agent_release_evaluations_release ON eacp.agent_release_evaluations (tenant_id, release_id, suite, seq);

CREATE TABLE eacp.agent_release_observations (
    tenant_id            uuid        NOT NULL,
    id                   uuid        NOT NULL DEFAULT gen_random_uuid(),
    release_id           uuid        NOT NULL,
    kind                 text        NOT NULL CHECK (kind IN ('replay', 'shadow')),
    candidate_version_id uuid        NOT NULL,
    reference_action_id  uuid        NOT NULL,
    -- The candidate's proposal (ADR-005 §2 binding; the payload is JCS text).
    subject              text        NOT NULL CHECK (length(subject) BETWEEN 1 AND 320),
    operation            text        NOT NULL CHECK (btrim(operation) <> '' AND length(operation) <= 256),
    target               text        NOT NULL CHECK (btrim(target) <> '' AND length(target) <= 256),
    tool                 text        NOT NULL CHECK (btrim(tool) <> '' AND length(tool) <= 128),
    tool_schema_version  text        NOT NULL CHECK (btrim(tool_schema_version) <> '' AND length(tool_schema_version) <= 64),
    resource             text        NOT NULL CHECK (btrim(resource) <> '' AND length(resource) <= 1024),
    input_payload        json        NOT NULL CHECK (octet_length(input_payload::text) <= 1048576),
    tool_id              uuid,
    capability_denial    text,
    verdict              text        CHECK (verdict IN ('allow', 'warn', 'deny', 'escalate', 'transform')),
    policy_version       integer,
    -- The comparison, computed by the database.
    candidate_outcome    text        NOT NULL,
    reference_version_id uuid        NOT NULL,
    reference_state      text        NOT NULL,
    reference_outcome    text,
    tool_match           boolean     NOT NULL,
    outcome_match        boolean     NOT NULL,
    payload_match        boolean     NOT NULL,
    agrees               boolean     NOT NULL,
    recorded             jsonb,
    recorded_at          timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, release_id, kind, reference_action_id),
    CHECK ((verdict IS NULL) = (policy_version IS NULL)),
    CHECK (kind = 'replay' OR recorded IS NULL),
    FOREIGN KEY (tenant_id, release_id) REFERENCES eacp.agent_releases (tenant_id, id),
    FOREIGN KEY (tenant_id, candidate_version_id) REFERENCES eacp.agent_versions (tenant_id, id),
    FOREIGN KEY (tenant_id, reference_action_id) REFERENCES eacp.actions (tenant_id, id),
    FOREIGN KEY (tenant_id, reference_version_id) REFERENCES eacp.agent_versions (tenant_id, id),
    FOREIGN KEY (tenant_id, tool_id) REFERENCES eacp.tools (tenant_id, id)
);

-- ------------------------------------------------------ shared tool rules

-- +goose StatementBegin
-- eacp.allowlist_tool_denial returns NULL when allowlist p_allowlist lets
-- its version call tool p_tool now, or the registry.Denial code. It holds
-- the tool rules of eacp.action_capability_denial (ADR-023 §7 order), and
-- reads the tool and contract FOR SHARE.
CREATE FUNCTION eacp.allowlist_tool_denial(p_allowlist uuid, p_tool uuid) RETURNS text
    LANGUAGE plpgsql
    AS $$
DECLARE
    tl eacp.tools%ROWTYPE;
    ct eacp.tool_contracts%ROWTYPE;
    allowed boolean;
BEGIN
    IF p_tool IS NULL THEN
        RETURN 'unknown_tool';
    END IF;
    SELECT p_tool = ANY (tool_ids) INTO allowed FROM eacp.agent_allowlists
    WHERE tenant_id = eacp.current_tenant_id() AND id = p_allowlist;
    IF NOT COALESCE(allowed, false) THEN
        RETURN 'tool_not_in_allowlist';
    END IF;
    SELECT * INTO tl FROM eacp.tools
    WHERE tenant_id = eacp.current_tenant_id() AND id = p_tool FOR SHARE;
    IF tl.quarantined_at IS NOT NULL THEN
        RETURN 'tool_quarantined';
    END IF;
    IF tl.active_contract_id IS NULL THEN
        RETURN 'no_active_contract';
    END IF;
    SELECT * INTO ct FROM eacp.tool_contracts
    WHERE tenant_id = eacp.current_tenant_id() AND id = tl.active_contract_id FOR SHARE;
    IF ct.revoked_at IS NOT NULL THEN
        RETURN 'contract_revoked';
    END IF;
    IF ct.fingerprint IS DISTINCT FROM eacp.tool_fingerprint(ct.tenant_id, p_tool) THEN
        RETURN 'contract_fingerprint_mismatch';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- eacp.action_capability_denial returns NULL when agent version p_version
-- may call tool p_tool now, or the registry.Denial code otherwise. Rows are
-- read FOR SHARE (ADR-004 principle 7). A quarantined tool is denied before
-- its contract is considered (ADR-023 §7).
CREATE OR REPLACE FUNCTION eacp.action_capability_denial(p_version uuid, p_tool uuid) RETURNS text
    LANGUAGE plpgsql
    AS $$
DECLARE
    v eacp.agent_versions%ROWTYPE;
BEGIN
    SELECT * INTO v FROM eacp.agent_versions
    WHERE tenant_id = eacp.current_tenant_id() AND id = p_version FOR SHARE;
    IF NOT FOUND OR v.state <> 'ACTIVE' OR v.active_allowlist_id IS NULL THEN
        RETURN 'agent_version_not_active';
    END IF;
    RETURN eacp.allowlist_tool_denial(v.active_allowlist_id, p_tool);
END
$$;
-- +goose StatementEnd

-- ------------------------------------------------------------- the cohort

-- +goose StatementBegin
-- The canary bucket of a subject under a release: the first 32 bits of
-- SHA-256 over "<release>/<subject>", modulo 10 000 (ADR-018 §4).
CREATE FUNCTION eacp.release_bucket(p_release uuid, p_subject uuid) RETURNS integer
    LANGUAGE sql IMMUTABLE STRICT
    AS $$
    SELECT (('x' || encode(substring(sha256(convert_to(p_release::text || '/' || p_subject::text, 'UTF8'))
                                     FROM 1 FOR 4), 'hex'))::bit(32)::bigint % 10000)::integer
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- eacp.release_denial returns NULL when version p_version may act for
-- subject principal p_subject under its release, or a denial code. Only the
-- candidate of an open release is restricted: to its canary cohort.
CREATE FUNCTION eacp.release_denial(p_version uuid, p_subject uuid) RETURNS text
    LANGUAGE plpgsql
    AS $$
DECLARE
    r eacp.agent_releases%ROWTYPE;
BEGIN
    SELECT * INTO r FROM eacp.agent_releases
    WHERE tenant_id = eacp.current_tenant_id() AND candidate_version_id = p_version
      AND state IN ('EVALUATING', 'SHADOW', 'CANARY') FOR SHARE;
    IF NOT FOUND THEN
        RETURN NULL;
    END IF;
    IF r.state <> 'CANARY' THEN
        RETURN 'release_not_in_canary';
    END IF;
    IF p_subject IS NULL OR eacp.release_bucket(r.id, p_subject) >= r.canary_bp THEN
        RETURN 'canary_cohort';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The version that serves subject principal p_subject of agent p_agent: the
-- canary candidate inside its cohort, otherwise the other ACTIVE version.
-- NULL when no version can act. Advisory: admission is release_denial's.
CREATE FUNCTION eacp.release_route(p_agent uuid, p_subject uuid) RETURNS uuid
    LANGUAGE plpgsql STABLE
    AS $$
DECLARE
    r eacp.agent_releases%ROWTYPE;
    v uuid;
BEGIN
    SELECT * INTO r FROM eacp.agent_releases
    WHERE tenant_id = eacp.current_tenant_id() AND agent_id = p_agent AND state = 'CANARY';
    IF FOUND AND p_subject IS NOT NULL AND eacp.release_bucket(r.id, p_subject) < r.canary_bp
       AND EXISTS (SELECT 1 FROM eacp.agent_versions
                   WHERE tenant_id = r.tenant_id AND id = r.candidate_version_id AND state = 'ACTIVE') THEN
        RETURN r.candidate_version_id;
    END IF;
    SELECT id INTO v FROM eacp.agent_versions
    WHERE tenant_id = eacp.current_tenant_id() AND agent_id = p_agent AND state = 'ACTIVE'
      AND id IS DISTINCT FROM r.candidate_version_id;
    RETURN v;
END
$$;
-- +goose StatementEnd

-- ------------------------------------------------------------- guardrails

-- +goose StatementBegin
-- What version p_version did with actions created since p_from (ADR-018 §6).
CREATE FUNCTION eacp.release_version_metrics(p_version uuid, p_from timestamptz) RETURNS jsonb
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
-- The guardrails release r breaches when candidate metrics c are compared
-- with stable metrics s (ADR-018 §6), in a fixed order. A stable rate with
-- no data counts as zero; latency is compared only when both have one.
CREATE FUNCTION eacp.release_breaches(r eacp.agent_releases, c jsonb, s jsonb) RETURNS jsonb
    LANGUAGE plpgsql IMMUTABLE
    AS $$
DECLARE
    out jsonb := '[]';
    g record;
    cr numeric;
    sr numeric;
    unit text;
    cv numeric;
    sv numeric;
BEGIN
    FOR g IN SELECT * FROM (VALUES
            (1, 'policy_violations', 'denied', 'actions', NULL::text, r.max_denial_increase),
            (2, 'failures', 'failed', 'succeeded', 'failed', r.max_failure_increase),
            (3, 'unknown_outcomes', 'unknown', 'dispatched', NULL, r.max_unknown_increase))
            AS x(ord, name, num, den, den2, tolerance) ORDER BY ord LOOP
        cr := (c->>g.num)::numeric / NULLIF((c->>g.den)::numeric + COALESCE((c->>g.den2)::numeric, 0), 0);
        sr := COALESCE((s->>g.num)::numeric / NULLIF((s->>g.den)::numeric + COALESCE((s->>g.den2)::numeric, 0), 0), 0);
        IF cr IS NOT NULL AND cr > sr + g.tolerance THEN
            out := out || jsonb_build_object('guardrail', g.name, 'candidate', round(cr, 6), 'stable', round(sr, 6),
                                             'limit', round(sr + g.tolerance, 6));
        END IF;
    END LOOP;
    cv := (c->>'latency_p95_ms')::numeric;
    sv := (s->>'latency_p95_ms')::numeric;
    IF cv IS NOT NULL AND sv IS NOT NULL AND cv > sv * r.max_latency_ratio THEN
        out := out || jsonb_build_object('guardrail', 'latency', 'candidate', cv, 'stable', sv,
                                         'limit', round(sv * r.max_latency_ratio, 3));
    END IF;
    FOR unit IN SELECT key FROM jsonb_each(COALESCE(c->'cost_per_action', '{}')) ORDER BY key LOOP
        cv := (c->'cost_per_action'->>unit)::numeric;
        sv := (s->'cost_per_action'->>unit)::numeric;
        IF cv > 0 AND (sv IS NULL OR sv = 0 OR cv > sv * r.max_cost_ratio) THEN
            out := out || jsonb_build_object('guardrail', 'cost:' || unit, 'candidate', cv, 'stable', COALESCE(sv, 0),
                                             'limit', round(COALESCE(sv, 0) * r.max_cost_ratio, 6));
        END IF;
    END LOOP;
    RETURN out;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The canary report of release r over its current step: both versions'
-- metrics, whether the candidate has enough actions, and the breaches.
CREATE FUNCTION eacp.release_report_of(r eacp.agent_releases) RETURNS jsonb
    LANGUAGE plpgsql STABLE
    AS $$
DECLARE
    c jsonb := eacp.release_version_metrics(r.candidate_version_id, r.stage_started_at);
    s jsonb := eacp.release_version_metrics(r.stable_version_id, r.stage_started_at);
BEGIN
    RETURN jsonb_build_object('since', r.stage_started_at, 'canary_bp', r.canary_bp,
        'candidate', c, 'stable', s,
        'sufficient', (c->>'actions')::integer >= r.min_canary_actions,
        'breaches', eacp.release_breaches(r, c, s));
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.release_canary_report(p_release uuid) RETURNS jsonb
    LANGUAGE sql STABLE
    AS $$
    SELECT eacp.release_report_of(r) FROM eacp.agent_releases r
    WHERE r.tenant_id = eacp.current_tenant_id() AND r.id = p_release AND r.state = 'CANARY'
$$;
-- +goose StatementEnd

-- ------------------------------------------------------------------ gates

-- +goose StatementBegin
-- The required suites of release r whose latest result did not pass.
CREATE FUNCTION eacp.release_failing_suites(r eacp.agent_releases) RETURNS text[]
    LANGUAGE sql STABLE
    AS $$
    SELECT COALESCE(array_agg(s ORDER BY s), '{}') FROM unnest(r.required_suites) s
    WHERE NOT COALESCE((SELECT e.passed FROM eacp.agent_release_evaluations e
                        WHERE e.tenant_id = r.tenant_id AND e.release_id = r.id AND e.suite = s
                        ORDER BY e.seq DESC LIMIT 1), false)
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Replay or shadow observations of a release, as rates and distributions.
CREATE FUNCTION eacp.release_observation_summary(p_release uuid, p_kind text) RETURNS jsonb
    LANGUAGE sql STABLE
    AS $$
    WITH o AS (
        SELECT * FROM eacp.agent_release_observations
        WHERE tenant_id = eacp.current_tenant_id() AND release_id = p_release AND kind = p_kind
    )
    SELECT jsonb_build_object(
        'cases', count(*),
        'agreeing', count(*) FILTER (WHERE agrees),
        'agreement', round(avg(agrees::integer), 4),
        'tool_match', round(avg(tool_match::integer), 4),
        'outcome_match', round(avg(outcome_match::integer), 4),
        'payload_match', round(avg(payload_match::integer), 4),
        'capability_denied', count(*) FILTER (WHERE capability_denial IS NOT NULL),
        'replay_misses', count(*) FILTER (WHERE kind = 'replay' AND recorded IS NULL),
        'candidate_outcomes', COALESCE((SELECT jsonb_object_agg(candidate_outcome, n) FROM (
            SELECT candidate_outcome, count(*) AS n FROM o GROUP BY 1) x), '{}'),
        'reference_outcomes', COALESCE((SELECT jsonb_object_agg(COALESCE(reference_outcome, 'none'), n) FROM (
            SELECT reference_outcome, count(*) AS n FROM o GROUP BY 1) x), '{}'))
    FROM o
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.release_gate_open(p_release uuid, p_kind text, p_min integer, p_agreement numeric) RETURNS boolean
    LANGUAGE sql STABLE
    AS $$
    SELECT p_min = 0 OR ((x->>'cases')::integer >= p_min AND (x->>'agreement')::numeric >= p_agreement)
    FROM (SELECT eacp.release_observation_summary(p_release, p_kind) AS x) s
$$;
-- +goose StatementEnd

-- ---------------------------------------------------------- the lifecycle

-- +goose StatementBegin
-- Opening fixes the plan and reads the stable version; every change after
-- that is one of the ADR-018 §1 transitions, made by the roles of §5 or,
-- for an automatic rollback, by the release system actor on a breach (§7).
CREATE FUNCTION eacp.agent_releases_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid;
    cand eacp.agent_versions%ROWTYPE;
    author uuid;
    report jsonb;
    step integer;
BEGIN
    IF TG_OP = 'INSERT' THEN
        a := eacp.actor();
        PERFORM eacp.assert_role(a, 'registry_editor', 'registry_approver');
        IF NEW.state <> 'EVALUATING' OR num_nonnulls(NEW.canary_bp, NEW.changed_by, NEW.changed_at,
               NEW.change_reason, NEW.closed_xact, NEW.breaches) > 0 THEN
            RAISE EXCEPTION 'a release opens EVALUATING' USING ERRCODE = '55000';
        END IF;
        PERFORM eacp.require_reason(NEW.reason, 'a release');
        IF cardinality(NEW.required_suites) IS NULL OR cardinality(NEW.required_suites) < 1
           OR EXISTS (SELECT 1 FROM unnest(NEW.required_suites) s WHERE s IS NULL OR s !~ '^[a-z0-9][a-z0-9._-]{0,63}$')
           OR (SELECT count(DISTINCT s) FROM unnest(NEW.required_suites) s) <> cardinality(NEW.required_suites) THEN
            RAISE EXCEPTION 'a release requires at least one distinct, well-named evaluation suite' USING ERRCODE = '23514';
        END IF;
        IF cardinality(NEW.canary_steps) IS NULL OR cardinality(NEW.canary_steps) < 1
           OR NEW.canary_steps[cardinality(NEW.canary_steps)] IS DISTINCT FROM 10000
           OR EXISTS (SELECT 1 FROM unnest(NEW.canary_steps) WITH ORDINALITY x(bp, i)
                      WHERE bp IS NULL OR bp < 1 OR bp > 10000
                         OR (i > 1 AND bp <= NEW.canary_steps[i - 1])) THEN
            RAISE EXCEPTION 'canary steps increase strictly from 1 to 10000 basis points' USING ERRCODE = '23514';
        END IF;
        SELECT * INTO cand FROM eacp.agent_versions
        WHERE tenant_id = NEW.tenant_id AND id = NEW.candidate_version_id FOR SHARE;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'no such candidate version' USING ERRCODE = '23503';
        END IF;
        IF cand.state NOT IN ('REGISTERED', 'SUSPENDED') OR cand.active_allowlist_id IS NULL THEN
            RAISE EXCEPTION 'the candidate is a REGISTERED or SUSPENDED version with an allowlist'
                USING ERRCODE = '55000';
        END IF;
        NEW.agent_id := cand.agent_id;
        SELECT id INTO NEW.stable_version_id FROM eacp.agent_versions
        WHERE tenant_id = NEW.tenant_id AND agent_id = cand.agent_id AND state = 'ACTIVE' FOR SHARE;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'agent % has no ACTIVE version to release from; activate the version instead',
                cand.agent_id USING ERRCODE = '55000';
        END IF;
        NEW.created_by := a;
        NEW.created_at := now();
        NEW.stage_started_at := now();
        RETURN NEW;
    END IF;

    IF OLD.state IN ('PROMOTED', 'ROLLED_BACK') THEN
        RAISE EXCEPTION 'release % is % (terminal)', OLD.id, OLD.state USING ERRCODE = '55000';
    END IF;

    -- Automatic rollback (ADR-018 §7): the release system actor withdraws a
    -- canary whose sufficient report shows a breach, and does nothing else.
    IF eacp.current_actor_id() IS NULL AND eacp.current_agent_version_id() IS NULL
       AND eacp.current_system_actor() = 'release' THEN
        IF OLD.state <> 'CANARY' OR NEW.state <> 'ROLLED_BACK' THEN
            RAISE EXCEPTION 'the release evaluator only rolls back a canary' USING ERRCODE = '42501';
        END IF;
        IF NEW.canary_bp IS DISTINCT FROM OLD.canary_bp THEN
            RAISE EXCEPTION 'a rollback keeps the step' USING ERRCODE = '55000';
        END IF;
        report := eacp.release_report_of(OLD);
        IF NOT (report->>'sufficient')::boolean OR jsonb_array_length(report->'breaches') = 0 THEN
            RAISE EXCEPTION 'release % breaches no guardrail', OLD.id USING ERRCODE = '55000';
        END IF;
        NEW.breaches := report->'breaches';
        NEW.change_reason := 'guardrail breach: ' ||
            (SELECT string_agg(b->>'guardrail', ', ') FROM jsonb_array_elements(report->'breaches') b);
        NEW.changed_by := NULL;
        NEW.changed_at := now();
        NEW.stage_started_at := now();
        NEW.closed_xact := pg_current_xact_id();
        RETURN NEW;
    END IF;

    a := eacp.actor();
    PERFORM eacp.require_reason(NEW.change_reason, 'a release change');
    IF NEW.breaches IS DISTINCT FROM OLD.breaches THEN
        RAISE EXCEPTION 'breaches are recorded by the release evaluator' USING ERRCODE = '42501';
    END IF;
    IF NEW.state = 'ROLLED_BACK' THEN
        -- Containment: single-person, from any open state.
        PERFORM eacp.assert_role(a, 'operator', 'registry_approver');
        IF NEW.canary_bp IS DISTINCT FROM OLD.canary_bp THEN
            RAISE EXCEPTION 'a rollback keeps the step' USING ERRCODE = '55000';
        END IF;
    ELSE
        -- Every forward move is a second person's (ADR-018 §5).
        PERFORM eacp.assert_role(a, 'registry_approver');
        PERFORM eacp.assert_distinct(a, OLD.created_by, 'the release opener');
        IF EXISTS (SELECT 1 FROM eacp.agent_release_evaluations
                   WHERE tenant_id = OLD.tenant_id AND release_id = OLD.id AND recorded_by = a) THEN
            RAISE EXCEPTION 'two-person rule: the actor cannot also be a recorder of the release''s evaluations'
                USING ERRCODE = '42501';
        END IF;
        SELECT * INTO cand FROM eacp.agent_versions
        WHERE tenant_id = OLD.tenant_id AND id = OLD.candidate_version_id FOR SHARE;
        IF OLD.state = 'CANARY' THEN
            -- Widening a canary grants capability: the ADR-003 grant rules.
            PERFORM eacp.assert_distinct(a, cand.created_by, 'the version creator');
            SELECT created_by INTO author FROM eacp.agent_allowlists
            WHERE tenant_id = cand.tenant_id AND id = cand.active_allowlist_id;
            PERFORM eacp.assert_distinct(a, author, 'the allowlist author');
        END IF;

        IF OLD.state = 'EVALUATING' AND NEW.state = 'SHADOW' THEN
            IF NEW.canary_bp IS NOT NULL THEN
                RAISE EXCEPTION 'a shadow has no canary step' USING ERRCODE = '55000';
            END IF;
            IF cardinality(eacp.release_failing_suites(OLD)) > 0 THEN
                RAISE EXCEPTION 'evaluation suites have not passed: %', eacp.release_failing_suites(OLD)
                    USING ERRCODE = '55000';
            END IF;
            IF NOT eacp.release_gate_open(OLD.id, 'replay', OLD.min_replay_cases, OLD.min_replay_agreement) THEN
                RAISE EXCEPTION 'the replay gate is closed' USING ERRCODE = '55000';
            END IF;
        ELSIF OLD.state = 'SHADOW' AND NEW.state = 'CANARY' THEN
            IF NEW.canary_bp IS DISTINCT FROM OLD.canary_steps[1] THEN
                RAISE EXCEPTION 'a canary starts at its first step (% basis points)', OLD.canary_steps[1]
                    USING ERRCODE = '55000';
            END IF;
            IF cardinality(eacp.release_failing_suites(OLD)) > 0 THEN
                RAISE EXCEPTION 'evaluation suites have not passed: %', eacp.release_failing_suites(OLD)
                    USING ERRCODE = '55000';
            END IF;
            IF NOT eacp.release_gate_open(OLD.id, 'shadow', OLD.min_shadow_cases, OLD.min_shadow_agreement) THEN
                RAISE EXCEPTION 'the shadow gate is closed' USING ERRCODE = '55000';
            END IF;
            PERFORM 1 FROM eacp.agent_versions
            WHERE tenant_id = OLD.tenant_id AND id = OLD.stable_version_id AND state = 'ACTIVE' FOR SHARE;
            IF NOT FOUND THEN
                RAISE EXCEPTION 'the stable version is not ACTIVE' USING ERRCODE = '55000';
            END IF;
        ELSIF OLD.state = 'CANARY' AND NEW.state IN ('CANARY', 'PROMOTED') THEN
            IF NEW.state = 'CANARY' THEN
                step := OLD.canary_steps[array_position(OLD.canary_steps, OLD.canary_bp) + 1];
                IF step IS NULL OR NEW.canary_bp IS DISTINCT FROM step THEN
                    RAISE EXCEPTION 'the next canary step is % basis points', step USING ERRCODE = '55000';
                END IF;
            ELSIF OLD.canary_bp <> 10000 OR NEW.canary_bp IS DISTINCT FROM OLD.canary_bp THEN
                RAISE EXCEPTION 'a release is promoted from its last step' USING ERRCODE = '55000';
            END IF;
            IF cand.state <> 'ACTIVE' THEN
                RAISE EXCEPTION 'the candidate is not ACTIVE' USING ERRCODE = '55000';
            END IF;
            report := eacp.release_report_of(OLD);
            IF NOT (report->>'sufficient')::boolean THEN
                RAISE EXCEPTION 'the candidate has fewer than % actions in this step', OLD.min_canary_actions
                    USING ERRCODE = '55000';
            END IF;
            IF jsonb_array_length(report->'breaches') > 0 THEN
                RAISE EXCEPTION 'the canary breaches its guardrails: %', report->'breaches' USING ERRCODE = '55000';
            END IF;
        ELSE
            RAISE EXCEPTION 'illegal release transition % -> %', OLD.state, NEW.state USING ERRCODE = '55000';
        END IF;
    END IF;

    NEW.changed_by := a;
    NEW.changed_at := now();
    NEW.stage_started_at := now();
    IF NEW.state IN ('PROMOTED', 'ROLLED_BACK') THEN
        NEW.closed_xact := pg_current_xact_id();
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The version changes a release makes, then its journal entry (last).
CREATE FUNCTION eacp.agent_releases_effects() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    event text;
    actor jsonb;
BEGIN
    IF TG_OP = 'INSERT' THEN
        event := 'release.opened';
    ELSE
        IF NEW.state = 'CANARY' AND OLD.state = 'SHADOW' THEN
            UPDATE eacp.agent_versions
               SET state = 'ACTIVE', state_reason = 'release ' || NEW.id || ' canary: ' || NEW.change_reason
             WHERE tenant_id = NEW.tenant_id AND id = NEW.candidate_version_id;
        ELSIF NEW.state = 'PROMOTED' THEN
            UPDATE eacp.agent_versions
               SET state = 'SUSPENDED', state_reason = 'release ' || NEW.id || ' promoted: ' || NEW.change_reason
             WHERE tenant_id = NEW.tenant_id AND id = NEW.stable_version_id AND state = 'ACTIVE';
        ELSIF NEW.state = 'ROLLED_BACK' THEN
            UPDATE eacp.agent_versions
               SET state = 'SUSPENDED', state_reason = 'release ' || NEW.id || ' rolled back: ' || NEW.change_reason
             WHERE tenant_id = NEW.tenant_id AND id = NEW.candidate_version_id AND state = 'ACTIVE';
        END IF;
        event := CASE
            WHEN NEW.state = 'CANARY' AND OLD.state = 'CANARY' THEN 'release.canary_step'
            ELSE 'release.' || lower(NEW.state) END;
    END IF;
    actor := CASE
        WHEN TG_OP = 'INSERT' THEN jsonb_build_object('kind', 'principal', 'id', NEW.created_by)
        WHEN NEW.changed_by IS NULL THEN jsonb_build_object('kind', 'system',
            'id', '00000000-0000-0000-0000-000000000000'::uuid, 'component', 'release')
        ELSE jsonb_build_object('kind', 'principal', 'id', NEW.changed_by) END;
    INSERT INTO eacp.audit_events (tenant_id, payload)
    VALUES (NEW.tenant_id, convert_to(jsonb_build_object(
        'v', 1, 'actor', actor, 'action', event,
        'subject', jsonb_build_object('type', 'agent_release', 'id', NEW.id),
        'reason', CASE WHEN TG_OP = 'INSERT' THEN NEW.reason ELSE NEW.change_reason END,
        'data', jsonb_build_object(
            'agent_id', NEW.agent_id, 'stable_version_id', NEW.stable_version_id,
            'candidate_version_id', NEW.candidate_version_id,
            'from', CASE WHEN TG_OP = 'UPDATE' THEN OLD.state END, 'to', NEW.state,
            'canary_bp', NEW.canary_bp, 'breaches', NEW.breaches))::text, 'UTF8'));
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER agent_releases_guard BEFORE INSERT OR UPDATE ON eacp.agent_releases
    FOR EACH ROW EXECUTE FUNCTION eacp.agent_releases_guard();
CREATE TRIGGER agent_releases_effects AFTER INSERT OR UPDATE ON eacp.agent_releases
    FOR EACH ROW EXECUTE FUNCTION eacp.agent_releases_effects();

-- ------------------------------------------------------------ evaluations

-- +goose StatementBegin
CREATE FUNCTION eacp.agent_release_evaluations_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.actor();
    st text;
BEGIN
    PERFORM eacp.assert_role(a, 'registry_editor', 'registry_approver');
    SELECT state INTO st FROM eacp.agent_releases
    WHERE tenant_id = NEW.tenant_id AND id = NEW.release_id FOR SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'no such release' USING ERRCODE = '23503';
    END IF;
    IF st NOT IN ('EVALUATING', 'SHADOW') THEN
        RAISE EXCEPTION 'evaluations are recorded before the canary' USING ERRCODE = '55000';
    END IF;
    NEW.recorded_by := a;
    NEW.recorded_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.agent_release_evaluations_audit() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    INSERT INTO eacp.audit_events (tenant_id, payload)
    VALUES (NEW.tenant_id, convert_to(jsonb_build_object(
        'v', 1, 'actor', jsonb_build_object('kind', 'principal', 'id', NEW.recorded_by),
        'action', 'release.evaluation_recorded',
        'subject', jsonb_build_object('type', 'agent_release', 'id', NEW.release_id),
        'reason', NEW.suite || CASE WHEN NEW.passed THEN ' passed' ELSE ' failed' END,
        'data', to_jsonb(NEW))::text, 'UTF8'));
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER agent_release_evaluations_guard BEFORE INSERT ON eacp.agent_release_evaluations
    FOR EACH ROW EXECUTE FUNCTION eacp.agent_release_evaluations_guard();
CREATE TRIGGER agent_release_evaluations_audit AFTER INSERT ON eacp.agent_release_evaluations
    FOR EACH ROW EXECUTE FUNCTION eacp.agent_release_evaluations_audit();

-- ----------------------------------------------------------- observations

-- +goose StatementBegin
-- The candidate records what it would do; the database checks the pairing
-- with the reference action and computes the comparison (ADR-018 §3).
CREATE FUNCTION eacp.agent_release_observations_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    who jsonb := eacp.actor_context();
    v uuid;
    r eacp.agent_releases%ROWTYPE;
    ref eacp.actions%ROWTYPE;
    allowlist uuid;
    ev_verdict text;
BEGIN
    IF who->>'kind' IS DISTINCT FROM 'agent' THEN
        RAISE EXCEPTION 'the candidate records its own observations' USING ERRCODE = '42501';
    END IF;
    v := (who->>'id')::uuid;
    SELECT * INTO r FROM eacp.agent_releases
    WHERE tenant_id = NEW.tenant_id AND candidate_version_id = v
      AND state IN ('EVALUATING', 'SHADOW', 'CANARY') FOR SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'only the candidate of an open release observes' USING ERRCODE = '42501';
    END IF;
    NEW.release_id := r.id;
    NEW.candidate_version_id := v;
    IF (NEW.kind = 'replay' AND r.state <> 'EVALUATING') OR (NEW.kind = 'shadow' AND r.state <> 'SHADOW') THEN
        RAISE EXCEPTION 'a % observation is not recorded while the release is %', NEW.kind, r.state
            USING ERRCODE = '55000';
    END IF;

    SELECT * INTO ref FROM eacp.actions WHERE tenant_id = NEW.tenant_id AND id = NEW.reference_action_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'no such reference action' USING ERRCODE = '23503';
    END IF;
    IF ref.agent_id <> r.agent_id OR ref.agent_version_id = v THEN
        RAISE EXCEPTION 'the reference is an action of another version of this agent' USING ERRCODE = '55000';
    END IF;
    IF NEW.kind = 'replay' AND ref.state NOT IN ('SUCCEEDED', 'FAILED', 'DENIED', 'CANCELLED', 'EXPIRED') THEN
        RAISE EXCEPTION 'a replay reference is a terminal action' USING ERRCODE = '55000';
    END IF;
    IF NEW.kind = 'shadow' AND (ref.agent_version_id <> r.stable_version_id OR ref.created_at < r.stage_started_at) THEN
        RAISE EXCEPTION 'a shadow reference is an action of the stable version since the shadow began'
            USING ERRCODE = '55000';
    END IF;
    IF NEW.subject IS DISTINCT FROM ref.subject THEN
        RAISE EXCEPTION 'an observation answers the reference''s request (the same subject)' USING ERRCODE = '55000';
    END IF;

    SELECT t.id INTO NEW.tool_id FROM eacp.tools t
    JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
    WHERE t.tenant_id = NEW.tenant_id AND c.name || '.' || t.name = NEW.tool;
    SELECT active_allowlist_id INTO allowlist FROM eacp.agent_versions
    WHERE tenant_id = NEW.tenant_id AND id = v FOR SHARE;
    NEW.capability_denial := eacp.allowlist_tool_denial(allowlist, NEW.tool_id);
    IF NEW.capability_denial IS NULL THEN
        IF NEW.verdict IS NULL THEN
            RAISE EXCEPTION 'an allowed proposal carries the PDP''s verdict' USING ERRCODE = '23514';
        END IF;
        PERFORM 1 FROM eacp.tenant_policy_pointer
        WHERE tenant_id = NEW.tenant_id AND current_version = NEW.policy_version FOR SHARE;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'the verdict was made under a policy that is no longer active' USING ERRCODE = '55000';
        END IF;
    ELSIF NEW.verdict IS NOT NULL OR NEW.policy_version IS NOT NULL THEN
        RAISE EXCEPTION 'a proposal denied by capability has no verdict' USING ERRCODE = '23514';
    END IF;

    SELECT verdict INTO ev_verdict FROM eacp.decision_evidence
    WHERE tenant_id = ref.tenant_id AND id = ref.decision_evidence_id;
    NEW.candidate_outcome := COALESCE('denied:' || NEW.capability_denial, NEW.verdict);
    NEW.reference_version_id := ref.agent_version_id;
    NEW.reference_state := ref.state;
    NEW.reference_outcome := CASE
        WHEN ref.decision_evidence_id IS NOT NULL THEN ev_verdict
        WHEN ref.state = 'DENIED' THEN 'denied:' || COALESCE(ref.state_reason, '') END;
    NEW.tool_match := NEW.tool = ref.tool;
    NEW.outcome_match := NEW.candidate_outcome IS NOT DISTINCT FROM NEW.reference_outcome;
    NEW.payload_match := NEW.input_payload::text = ref.input_payload::text;
    NEW.agrees := NEW.tool_match AND NEW.outcome_match AND NEW.capability_denial IS NULL;
    NEW.recorded := NULL;
    IF NEW.kind = 'replay' AND NEW.tool_match AND NEW.payload_match THEN
        -- The record/replay connector: EACP's own record of the reference.
        NEW.recorded := jsonb_build_object('state', ref.state, 'external_reference', ref.external_reference,
            'attempt', (SELECT jsonb_build_object('outcome', outcome, 'error_class', error_class)
                        FROM eacp.action_attempts
                        WHERE tenant_id = ref.tenant_id AND action_id = ref.id
                        ORDER BY attempt_no DESC LIMIT 1));
    END IF;
    NEW.recorded_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.agent_release_observations_audit() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    INSERT INTO eacp.audit_events (tenant_id, payload)
    VALUES (NEW.tenant_id, convert_to(jsonb_build_object(
        'v', 1, 'actor', jsonb_build_object('kind', 'agent', 'id', NEW.candidate_version_id),
        'action', 'release.observation_recorded',
        'subject', jsonb_build_object('type', 'agent_release', 'id', NEW.release_id),
        'reason', NEW.kind || CASE WHEN NEW.agrees THEN ' agrees' ELSE ' differs' END,
        'data', to_jsonb(NEW) - 'input_payload')::text, 'UTF8'));
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER agent_release_observations_guard BEFORE INSERT ON eacp.agent_release_observations
    FOR EACH ROW EXECUTE FUNCTION eacp.agent_release_observations_guard();
CREATE TRIGGER agent_release_observations_audit AFTER INSERT ON eacp.agent_release_observations
    FOR EACH ROW EXECUTE FUNCTION eacp.agent_release_observations_audit();

-- ------------------------------------------------- versions: two ACTIVE

DROP INDEX eacp.agent_versions_one_active;
CREATE INDEX agent_versions_active ON eacp.agent_versions (tenant_id, agent_id) WHERE state = 'ACTIVE';

-- +goose StatementBegin
-- At most one ACTIVE version per agent, except the candidate of the agent's
-- release in CANARY next to that release's stable version (ADR-018 §4).
-- Activations of an agent are serialised by the version-numbering lock.
CREATE FUNCTION eacp.agent_versions_release_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    r eacp.agent_releases%ROWTYPE;
    open_release boolean;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended(NEW.tenant_id::text || '/' || NEW.agent_id::text, 0));
    SELECT * INTO r FROM eacp.agent_releases
    WHERE tenant_id = NEW.tenant_id AND agent_id = NEW.agent_id
      AND state IN ('EVALUATING', 'SHADOW', 'CANARY') FOR SHARE;
    open_release := FOUND;
    IF open_release AND r.candidate_version_id = NEW.id THEN
        IF r.state <> 'CANARY' THEN
            RAISE EXCEPTION 'the candidate of an open release is activated only by its canary' USING ERRCODE = '55000';
        END IF;
        IF EXISTS (SELECT 1 FROM eacp.agent_versions
                   WHERE tenant_id = NEW.tenant_id AND agent_id = NEW.agent_id AND state = 'ACTIVE'
                     AND id NOT IN (NEW.id, r.stable_version_id)) THEN
            RAISE EXCEPTION 'at most one ACTIVE version per agent besides its canary' USING ERRCODE = '23505';
        END IF;
    ELSIF EXISTS (SELECT 1 FROM eacp.agent_versions
                  WHERE tenant_id = NEW.tenant_id AND agent_id = NEW.agent_id AND state = 'ACTIVE' AND id <> NEW.id
                    AND NOT (open_release AND r.state = 'CANARY' AND id = r.candidate_version_id
                             AND NEW.id = r.stable_version_id)) THEN
        RAISE EXCEPTION 'at most one ACTIVE version per agent outside a canary' USING ERRCODE = '23505';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER agent_versions_release_guard BEFORE UPDATE ON eacp.agent_versions
    FOR EACH ROW WHEN (NEW.state = 'ACTIVE' AND OLD.state IS DISTINCT FROM 'ACTIVE')
    EXECUTE FUNCTION eacp.agent_versions_release_guard();

-- +goose StatementBegin
-- ADR-003 §2 version guard, plus one system branch (ADR-018 §7): the release
-- evaluator suspends the candidate of a release it rolled back in this
-- transaction, and nothing else.
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

-- ------------------------------------------------- actions: the cohort

-- +goose StatementBegin
-- Backstop for T2, T4, T10 and every other move toward execution: a
-- candidate acts only inside its canary cohort (ADR-018 §4). The engine
-- denies such actions first; this refuses anything that slips past it.
CREATE FUNCTION eacp.actions_release_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    denial text := eacp.release_denial(NEW.agent_version_id, NEW.subject_principal_id);
BEGIN
    IF denial IS NOT NULL THEN
        RAISE EXCEPTION 'release check failed: %', denial USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER actions_release_guard BEFORE UPDATE ON eacp.actions
    FOR EACH ROW WHEN (NEW.state IN ('PENDING_APPROVAL', 'AUTHORIZED', 'QUEUED') AND NEW.state IS DISTINCT FROM OLD.state)
    EXECUTE FUNCTION eacp.actions_release_guard();

-- +goose StatementBegin
-- eacp.dispatch_drift returns NULL when action a may be dispatched under its
-- pinned facts, 'policy_changed' when only the policy pointer moved (T16a),
-- or a denial reason (T16b). Registry rows are read FOR SHARE (ADR-004
-- principle 7); denials win over policy drift. A candidate outside its
-- canary cohort is denied (ADR-018 §4).
CREATE OR REPLACE FUNCTION eacp.dispatch_drift(a eacp.actions) RETURNS text
    LANGUAGE plpgsql
    AS $$
DECLARE
    denial text;
    active uuid;
    p eacp.tenant_policy_pointer%ROWTYPE;
BEGIN
    PERFORM 1 FROM eacp.principals
    WHERE tenant_id = a.tenant_id AND id = a.subject_principal_id
      AND kind = 'human' AND disabled_at IS NULL FOR SHARE;
    IF NOT FOUND THEN
        RETURN 'subject_invalid';
    END IF;
    denial := eacp.action_capability_denial(a.agent_version_id, a.tool_id);
    IF denial IS NOT NULL THEN
        RETURN denial;
    END IF;
    denial := eacp.release_denial(a.agent_version_id, a.subject_principal_id);
    IF denial IS NOT NULL THEN
        RETURN denial;
    END IF;
    SELECT active_contract_id INTO active FROM eacp.tools
    WHERE tenant_id = a.tenant_id AND id = a.tool_id FOR SHARE;
    IF active IS DISTINCT FROM a.connector_contract_id THEN
        RETURN 'contract_superseded';
    END IF;
    SELECT * INTO p FROM eacp.tenant_policy_pointer WHERE tenant_id = a.tenant_id FOR SHARE;
    IF p.current_bundle_id IS DISTINCT FROM a.policy_bundle_id
       OR p.current_version IS DISTINCT FROM a.policy_version THEN
        RETURN 'policy_changed';
    END IF;
    PERFORM 1 FROM eacp.policy_bundles
    WHERE tenant_id = a.tenant_id AND id = a.policy_bundle_id AND revoked_at IS NULL FOR SHARE;
    IF NOT FOUND THEN
        RETURN 'policy_changed';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- -------------------------------------------------------------- evaluator

-- +goose StatementBegin
-- The automatic rollback (ADR-018 §7): every CANARY release of the tenant
-- whose sufficient report breaches a guardrail is rolled back. It returns
-- the number rolled back.
CREATE FUNCTION eacp.release_evaluate() RETURNS integer
    LANGUAGE plpgsql
    AS $$
DECLARE
    r eacp.agent_releases%ROWTYPE;
    report jsonb;
    n integer := 0;
BEGIN
    IF eacp.current_system_actor() IS DISTINCT FROM 'release' OR eacp.current_actor_id() IS NOT NULL
       OR eacp.current_agent_version_id() IS NOT NULL THEN
        RAISE EXCEPTION 'the release evaluator runs as the release system actor' USING ERRCODE = '42501';
    END IF;
    FOR r IN SELECT * FROM eacp.agent_releases
             WHERE tenant_id = eacp.current_tenant_id() AND state = 'CANARY' ORDER BY id FOR UPDATE LOOP
        report := eacp.release_report_of(r);
        IF (report->>'sufficient')::boolean AND jsonb_array_length(report->'breaches') > 0 THEN
            UPDATE eacp.agent_releases SET state = 'ROLLED_BACK' WHERE tenant_id = r.tenant_id AND id = r.id;
            n := n + 1;
        END IF;
    END LOOP;
    RETURN n;
END
$$;
-- +goose StatementEnd

-- ------------------------------------------------------------------ access

ALTER TABLE eacp.agent_releases ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.agent_releases FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.agent_releases USING (tenant_id = eacp.current_tenant_id());
CREATE POLICY owner_scan ON eacp.agent_releases FOR SELECT TO CURRENT_USER USING (true);
ALTER TABLE eacp.agent_release_evaluations ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.agent_release_evaluations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.agent_release_evaluations USING (tenant_id = eacp.current_tenant_id());
ALTER TABLE eacp.agent_release_observations ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.agent_release_observations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.agent_release_observations USING (tenant_id = eacp.current_tenant_id());

REVOKE UPDATE ON eacp.agent_releases FROM eacp_app;
GRANT UPDATE (state, canary_bp, change_reason) ON eacp.agent_releases TO eacp_app;
REVOKE UPDATE ON eacp.agent_release_evaluations FROM eacp_app;
REVOKE UPDATE ON eacp.agent_release_observations FROM eacp_app;

-- +goose StatementBegin
-- The evaluator's cross-tenant hint: tenant ids with a release in CANARY.
CREATE FUNCTION eacp.release_tenants() RETURNS TABLE (tenant_id uuid)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT DISTINCT r.tenant_id FROM eacp.agent_releases r WHERE r.state = 'CANARY' ORDER BY 1
    $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION eacp.release_tenants() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.release_tenants() TO eacp_app;

-- +goose Down
DROP FUNCTION eacp.release_tenants();
DROP FUNCTION eacp.release_evaluate();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.dispatch_drift(a eacp.actions) RETURNS text
    LANGUAGE plpgsql
    AS $$
DECLARE
    denial text;
    active uuid;
    p eacp.tenant_policy_pointer%ROWTYPE;
BEGIN
    PERFORM 1 FROM eacp.principals
    WHERE tenant_id = a.tenant_id AND id = a.subject_principal_id
      AND kind = 'human' AND disabled_at IS NULL FOR SHARE;
    IF NOT FOUND THEN
        RETURN 'subject_invalid';
    END IF;
    denial := eacp.action_capability_denial(a.agent_version_id, a.tool_id);
    IF denial IS NOT NULL THEN
        RETURN denial;
    END IF;
    SELECT active_contract_id INTO active FROM eacp.tools
    WHERE tenant_id = a.tenant_id AND id = a.tool_id FOR SHARE;
    IF active IS DISTINCT FROM a.connector_contract_id THEN
        RETURN 'contract_superseded';
    END IF;
    SELECT * INTO p FROM eacp.tenant_policy_pointer WHERE tenant_id = a.tenant_id FOR SHARE;
    IF p.current_bundle_id IS DISTINCT FROM a.policy_bundle_id
       OR p.current_version IS DISTINCT FROM a.policy_version THEN
        RETURN 'policy_changed';
    END IF;
    PERFORM 1 FROM eacp.policy_bundles
    WHERE tenant_id = a.tenant_id AND id = a.policy_bundle_id AND revoked_at IS NULL FOR SHARE;
    IF NOT FOUND THEN
        RETURN 'policy_changed';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

DROP TRIGGER actions_release_guard ON eacp.actions;
DROP FUNCTION eacp.actions_release_guard();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.agent_versions_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.actor();
    author uuid;
    move text;
BEGIN
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

DROP TRIGGER agent_versions_release_guard ON eacp.agent_versions;
DROP FUNCTION eacp.agent_versions_release_guard();
DROP INDEX eacp.agent_versions_active;
CREATE UNIQUE INDEX agent_versions_one_active ON eacp.agent_versions (tenant_id, agent_id)
    WHERE state = 'ACTIVE';

DROP FUNCTION eacp.release_gate_open(uuid, text, integer, numeric);
DROP FUNCTION eacp.release_observation_summary(uuid, text);
DROP FUNCTION eacp.release_failing_suites(eacp.agent_releases);
DROP FUNCTION eacp.release_canary_report(uuid);
DROP FUNCTION eacp.release_report_of(eacp.agent_releases);
DROP FUNCTION eacp.release_breaches(eacp.agent_releases, jsonb, jsonb);
DROP FUNCTION eacp.release_version_metrics(uuid, timestamptz);
DROP FUNCTION eacp.release_route(uuid, uuid);
DROP FUNCTION eacp.release_denial(uuid, uuid);
DROP FUNCTION eacp.release_bucket(uuid, uuid);
DROP TABLE eacp.agent_release_observations;
DROP TABLE eacp.agent_release_evaluations;
DROP TABLE eacp.agent_releases;
DROP FUNCTION eacp.agent_release_observations_audit();
DROP FUNCTION eacp.agent_release_observations_guard();
DROP FUNCTION eacp.agent_release_evaluations_audit();
DROP FUNCTION eacp.agent_release_evaluations_guard();
DROP FUNCTION eacp.agent_releases_effects();
DROP FUNCTION eacp.agent_releases_guard();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.action_capability_denial(p_version uuid, p_tool uuid) RETURNS text
    LANGUAGE plpgsql
    AS $$
DECLARE
    v eacp.agent_versions%ROWTYPE;
    tl eacp.tools%ROWTYPE;
    ct eacp.tool_contracts%ROWTYPE;
    allowed boolean;
BEGIN
    SELECT * INTO v FROM eacp.agent_versions
    WHERE tenant_id = eacp.current_tenant_id() AND id = p_version FOR SHARE;
    IF NOT FOUND OR v.state <> 'ACTIVE' OR v.active_allowlist_id IS NULL THEN
        RETURN 'agent_version_not_active';
    END IF;
    IF p_tool IS NULL THEN
        RETURN 'unknown_tool';
    END IF;
    SELECT p_tool = ANY (tool_ids) INTO allowed FROM eacp.agent_allowlists
    WHERE tenant_id = eacp.current_tenant_id() AND id = v.active_allowlist_id;
    IF NOT COALESCE(allowed, false) THEN
        RETURN 'tool_not_in_allowlist';
    END IF;
    SELECT * INTO tl FROM eacp.tools
    WHERE tenant_id = eacp.current_tenant_id() AND id = p_tool FOR SHARE;
    IF tl.quarantined_at IS NOT NULL THEN
        RETURN 'tool_quarantined';
    END IF;
    IF tl.active_contract_id IS NULL THEN
        RETURN 'no_active_contract';
    END IF;
    SELECT * INTO ct FROM eacp.tool_contracts
    WHERE tenant_id = eacp.current_tenant_id() AND id = tl.active_contract_id FOR SHARE;
    IF ct.revoked_at IS NOT NULL THEN
        RETURN 'contract_revoked';
    END IF;
    IF ct.fingerprint IS DISTINCT FROM eacp.tool_fingerprint(ct.tenant_id, p_tool) THEN
        RETURN 'contract_fingerprint_mismatch';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

DROP FUNCTION eacp.allowlist_tool_denial(uuid, uuid);
