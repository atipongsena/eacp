-- Slice A Phase 5: worker, lease, fencing and dispatch intent (MASTER_PLAN
-- §79, ADR-004 T14, T16-T27). Reconciliation and human resolution (T28-T37)
-- arrive in Phase 7.
--
-- A worker claims a QUEUED action (T14, lease_generation + 1), records a
-- fenced dispatch intent (T16, which inserts the attempt row) before any
-- external call, and commits the result fenced by its generation (T19-T22a).
-- The sweeper reclaims expired leases (T17, T18, T23, T24) and schedules
-- retries (T25-T27). Every worker write names its lease generation in the
-- transaction (app.lease_generation), so PostgreSQL rejects a stale worker
-- even for raw SQL.

-- +goose Up

-- ------------------------------------------------------- actor: the worker

CREATE FUNCTION eacp.current_worker_id() RETURNS text
    LANGUAGE sql STABLE
    AS $$ SELECT w FROM (SELECT current_setting('app.worker_id', true) AS w) s
          WHERE w ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$' $$;

CREATE FUNCTION eacp.current_lease_generation() RETURNS bigint
    LANGUAGE sql STABLE
    AS $$ SELECT g::bigint FROM (SELECT current_setting('app.lease_generation', true) AS g) s
          WHERE g ~ '^[0-9]{1,18}$' $$;

-- +goose StatementBegin
-- eacp.actor_context() returns the transaction's single actor:
--   {"kind":"principal","id":...}  an enabled principal of the tenant
--   {"kind":"agent","id":...}      a non-terminal agent version of the tenant
--   {"kind":"system","id":<nil>,"component":"sweeper"|"owner"}
--   {"kind":"system","id":<nil>,"component":"worker","worker":<worker id>}
-- The schema owner without any setting is the "owner" system actor.
CREATE OR REPLACE FUNCTION eacp.actor_context() RETURNS jsonb
    LANGUAGE plpgsql STABLE
    AS $$
DECLARE
    p uuid := eacp.current_actor_id();
    g uuid := eacp.current_agent_version_id();
    s text := eacp.current_system_actor();
    w text;
BEGIN
    IF num_nonnulls(p, g, s) > 1 THEN
        RAISE EXCEPTION 'a transaction has exactly one actor' USING ERRCODE = '42501';
    END IF;
    IF p IS NOT NULL THEN
        PERFORM 1 FROM eacp.principals
        WHERE tenant_id = eacp.current_tenant_id() AND id = p AND disabled_at IS NULL;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'actor % is not an enabled principal', p USING ERRCODE = '42501';
        END IF;
        RETURN jsonb_build_object('kind', 'principal', 'id', p);
    END IF;
    IF g IS NOT NULL THEN
        PERFORM 1 FROM eacp.agent_versions
        WHERE tenant_id = eacp.current_tenant_id() AND id = g AND state NOT IN ('RETIRED', 'REVOKED');
        IF NOT FOUND THEN
            RAISE EXCEPTION 'agent version % cannot act', g USING ERRCODE = '42501';
        END IF;
        RETURN jsonb_build_object('kind', 'agent', 'id', g);
    END IF;
    IF s IS NOT NULL THEN
        IF s = 'worker' THEN
            w := eacp.current_worker_id();
            IF w IS NULL THEN
                RAISE EXCEPTION 'the worker actor needs a valid app.worker_id' USING ERRCODE = '42501';
            END IF;
            RETURN jsonb_build_object('kind', 'system', 'id', '00000000-0000-0000-0000-000000000000'::uuid,
                                      'component', s, 'worker', w);
        END IF;
        IF s <> 'sweeper' THEN
            RAISE EXCEPTION 'unknown system actor %', s USING ERRCODE = '42501';
        END IF;
        RETURN jsonb_build_object('kind', 'system', 'id', '00000000-0000-0000-0000-000000000000'::uuid, 'component', s);
    END IF;
    IF eacp.is_schema_owner() THEN
        RETURN jsonb_build_object('kind', 'system', 'id', '00000000-0000-0000-0000-000000000000'::uuid, 'component', 'owner');
    END IF;
    RAISE EXCEPTION 'no actor set for this transaction' USING ERRCODE = '42501';
END
$$;
-- +goose StatementEnd

-- ------------------------------------------------------------------ tables

ALTER TABLE eacp.actions
    ADD COLUMN lease_generation    bigint      NOT NULL DEFAULT 0 CHECK (lease_generation >= 0),
    ADD COLUMN worker_id           text        CHECK (worker_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'),
    ADD COLUMN leased_until        timestamptz,
    ADD COLUMN heartbeat_at        timestamptz,
    ADD COLUMN attempt_count       integer     NOT NULL DEFAULT 0 CHECK (attempt_count BETWEEN 0 AND 10),
    ADD COLUMN dispatch_intent_at  timestamptz,
    ADD COLUMN next_attempt_at     timestamptz,
    ADD COLUMN external_reference  text        CHECK (btrim(external_reference) <> '' AND length(external_reference) <= 512),
    ADD COLUMN cancel_requested_at timestamptz,
    ADD COLUMN cancel_reason       text        CHECK (length(cancel_reason) <= 1024),
    ADD CONSTRAINT lease_iff_leased CHECK ((state IN ('LEASED', 'EXECUTING')) = (leased_until IS NOT NULL)),
    ADD CONSTRAINT leased_has_worker CHECK (state NOT IN ('LEASED', 'EXECUTING') OR worker_id IS NOT NULL),
    ADD CONSTRAINT retry_iff_scheduled CHECK ((state = 'RETRY_WAIT') = (next_attempt_at IS NOT NULL)),
    ADD CONSTRAINT success_has_reference CHECK (state <> 'SUCCEEDED' OR external_reference IS NOT NULL),
    ADD CONSTRAINT executing_has_intent CHECK (state <> 'EXECUTING' OR dispatch_intent_at IS NOT NULL),
    ADD CONSTRAINT cancel_request_complete CHECK ((cancel_requested_at IS NULL) = (cancel_reason IS NULL));

CREATE INDEX actions_claim ON eacp.actions (state_changed_at, id) WHERE state = 'QUEUED';
CREATE INDEX actions_leases ON eacp.actions (tenant_id, leased_until) WHERE state IN ('LEASED', 'EXECUTING');
CREATE INDEX actions_retry ON eacp.actions (tenant_id, next_attempt_at) WHERE state = 'RETRY_WAIT';

-- One row per dispatch intent (T16), inserted by the actions trigger in the
-- dispatch-intent transaction. The worker completes it once with the
-- classified outcome; a completion after the action left EXECUTING at that
-- generation is late-result evidence (§23.1 step 4). No secret, response
-- body or error text is stored: only the certified classification.
CREATE TABLE eacp.action_attempts (
    tenant_id                  uuid        NOT NULL,
    action_id                  uuid        NOT NULL,
    attempt_no                 integer     NOT NULL CHECK (attempt_no BETWEEN 1 AND 10),
    lease_generation           bigint      NOT NULL CHECK (lease_generation >= 1),
    worker_id                  text        NOT NULL,
    operation_key              text        NOT NULL,
    connector_contract_id      uuid        NOT NULL,
    connector_contract_version integer     NOT NULL,
    enforced_digest            bytea       NOT NULL CHECK (octet_length(enforced_digest) = 32),
    dispatched_at              timestamptz NOT NULL,
    call_deadline              timestamptz NOT NULL,
    completed_at               timestamptz,
    outcome                    text        CHECK (outcome IN ('succeeded', 'no_effect', 'ambiguous')),
    external_reference         text        CHECK (btrim(external_reference) <> '' AND length(external_reference) <= 512),
    error_class                text        CHECK (error_class ~ '^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$'),
    late                       boolean     NOT NULL DEFAULT false,
    PRIMARY KEY (tenant_id, action_id, attempt_no),
    UNIQUE (tenant_id, action_id, lease_generation),
    CHECK ((completed_at IS NULL) = (outcome IS NULL)),
    CHECK (outcome IS DISTINCT FROM 'succeeded' OR (external_reference IS NOT NULL AND error_class IS NULL)),
    CHECK (outcome IS DISTINCT FROM 'no_effect' OR (external_reference IS NULL AND error_class IS NOT NULL)),
    CHECK (outcome IS DISTINCT FROM 'ambiguous' OR external_reference IS NULL),
    FOREIGN KEY (tenant_id, action_id) REFERENCES eacp.actions (tenant_id, id),
    FOREIGN KEY (tenant_id, connector_contract_id) REFERENCES eacp.tool_contracts (tenant_id, id)
);

-- ------------------------------------------------------------- helpers

-- The call budget of a pinned contract: its timeout (default 30s), capped
-- at 5 minutes so a lease (at most 10 minutes) can always cover it.
CREATE FUNCTION eacp.call_timeout(p_contract uuid) RETURNS interval
    LANGUAGE sql STABLE
    AS $$ SELECT make_interval(secs => least(COALESCE(timeout_ms, 30000), 300000) / 1000.0)
          FROM eacp.tool_contracts
          WHERE tenant_id = eacp.current_tenant_id() AND id = p_contract $$;

-- +goose StatementBegin
-- eacp.dispatch_drift returns NULL when action a may be dispatched under its
-- pinned facts, 'policy_changed' when only the policy pointer moved (T16a),
-- or a denial reason (T16b). Registry rows are read FOR SHARE (ADR-004
-- principle 7); denials win over policy drift.
CREATE FUNCTION eacp.dispatch_drift(a eacp.actions) RETURNS text
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

-- +goose StatementBegin
-- eacp.assert_lease_holder fails unless the actor is the worker holding
-- action a's lease at generation a.lease_generation.
CREATE FUNCTION eacp.assert_lease_holder(who jsonb, a eacp.actions) RETURNS void
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF who->>'component' IS DISTINCT FROM 'worker' OR who->>'worker' IS DISTINCT FROM a.worker_id
       OR eacp.current_lease_generation() IS DISTINCT FROM a.lease_generation THEN
        RAISE EXCEPTION 'only the lease holder at generation % may do this', a.lease_generation
            USING ERRCODE = '42501';
    END IF;
END
$$;
-- +goose StatementEnd

-- ------------------------------------------------------- the state machine

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.actions_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    who     jsonb := eacp.actor_context();
    akind   text  := who->>'kind';
    who_id  uuid  := (who->>'id')::uuid;
    comp    text  := who->>'component';
    move    text;
    mutable text[] := ARRAY['state', 'state_reason'];
    ev      eacp.decision_evidence%ROWTYPE;
    req     eacp.approval_requests%ROWTYPE;
    att     eacp.action_attempts%ROWTYPE;
    ct      eacp.tool_contracts%ROWTYPE;
    p       eacp.tenant_policy_pointer%ROWTYPE;
    denial  text;
    outcome text;
    contract_version integer;
BEGIN
    IF TG_OP = 'INSERT' THEN
        -- T1: only the authenticated agent version submits its own action.
        IF akind <> 'agent' OR who_id IS DISTINCT FROM NEW.agent_version_id THEN
            RAISE EXCEPTION 'only the authenticated agent version submits its actions' USING ERRCODE = '42501';
        END IF;
        IF NEW.state <> 'RECEIVED' OR num_nonnulls(NEW.decision_evidence_id, NEW.policy_bundle_id,
               NEW.policy_version, NEW.enforced_payload, NEW.enforced_digest, NEW.approval_request_id,
               NEW.connector_contract_id, NEW.connector_contract_version, NEW.released_at, NEW.state_reason) > 0 THEN
            RAISE EXCEPTION 'an action is submitted RECEIVED without governance results' USING ERRCODE = '55000';
        END IF;
        IF NEW.lease_generation <> 0 OR NEW.attempt_count <> 0 OR num_nonnulls(NEW.worker_id, NEW.leased_until,
               NEW.heartbeat_at, NEW.dispatch_intent_at, NEW.next_attempt_at, NEW.external_reference,
               NEW.cancel_requested_at, NEW.cancel_reason) > 0 THEN
            RAISE EXCEPTION 'an action is submitted without execution state' USING ERRCODE = '55000';
        END IF;
        IF NEW.not_after <= now() OR NEW.not_after > now() + interval '24 hours' THEN
            RAISE EXCEPTION 'action lifetime must end within the next 24 hours' USING ERRCODE = '23514';
        END IF;
        SELECT agent_id INTO NEW.agent_id FROM eacp.agent_versions
        WHERE tenant_id = NEW.tenant_id AND id = NEW.agent_version_id;
        -- The database resolves the subject and tool; unknown ones stay NULL
        -- and the action is denied (T2) so the attempt remains auditable.
        SELECT id INTO NEW.subject_principal_id FROM eacp.principals
        WHERE tenant_id = NEW.tenant_id AND subject = NEW.subject AND kind = 'human' AND disabled_at IS NULL;
        SELECT t.id INTO NEW.tool_id FROM eacp.tools t
        JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
        WHERE t.tenant_id = NEW.tenant_id AND c.name || '.' || t.name = NEW.tool;
        NEW.created_at := now();
        NEW.state_changed_at := now();
        NEW.state_actor_kind := akind;
        NEW.state_actor_id := who_id;
        NEW.state_actor_component := NULL;
        RETURN NEW;
    END IF;

    IF OLD.state IN ('SUCCEEDED', 'FAILED', 'DENIED', 'CANCELLED', 'EXPIRED') THEN
        RAISE EXCEPTION 'action % is % (terminal)', OLD.id, OLD.state USING ERRCODE = '55000';
    END IF;

    IF NEW.state IS NOT DISTINCT FROM OLD.state THEN
        IF OLD.state IN ('LEASED', 'EXECUTING')
           AND NEW.cancel_requested_at IS NOT DISTINCT FROM OLD.cancel_requested_at THEN
            -- Heartbeat (§22): the lease holder extends a live lease. An
            -- expired lease belongs to the sweeper. Not a transition.
            IF (to_jsonb(NEW) - ARRAY['leased_until', 'heartbeat_at', 'operation_key'])
               IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['leased_until', 'heartbeat_at', 'operation_key']) THEN
                RAISE EXCEPTION 'a heartbeat changes only the lease' USING ERRCODE = '55000';
            END IF;
            PERFORM eacp.assert_lease_holder(who, OLD);
            IF OLD.leased_until <= now() THEN
                RAISE EXCEPTION 'the lease has expired' USING ERRCODE = '55000';
            END IF;
            IF NEW.leased_until <= now() OR NEW.leased_until > now() + interval '10 minutes' THEN
                RAISE EXCEPTION 'a lease lasts at most 10 minutes' USING ERRCODE = '23514';
            END IF;
            NEW.heartbeat_at := now();
            RETURN NEW;
        END IF;
        IF OLD.state IN ('EXECUTING', 'RETRY_WAIT') AND OLD.cancel_requested_at IS NULL
           AND NEW.cancel_requested_at IS NOT NULL THEN
            -- A cancel request after a dispatch intent (ADR-004 Rev 2.3): not
            -- a transition. The worker cancels the call (then T22, or T21 on a
            -- definitive no-effect); the sweeper fails a waiting retry (T27).
            IF (to_jsonb(NEW) - ARRAY['cancel_requested_at', 'cancel_reason', 'operation_key'])
               IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['cancel_requested_at', 'cancel_reason', 'operation_key']) THEN
                RAISE EXCEPTION 'a cancel request changes only the request' USING ERRCODE = '55000';
            END IF;
            PERFORM eacp.assert_action_agent(who, OLD.agent_id);
            IF akind = 'system' AND comp <> 'owner' THEN
                RAISE EXCEPTION 'the system does not cancel actions' USING ERRCODE = '42501';
            ELSIF akind = 'principal' AND who_id IS DISTINCT FROM OLD.subject_principal_id THEN
                PERFORM eacp.assert_role(who_id, 'operator');
            END IF;
            PERFORM eacp.require_reason(NEW.cancel_reason, 'a cancellation');
            NEW.cancel_requested_at := now();
            RETURN NEW;
        END IF;
        RAISE EXCEPTION 'an action changes only by a state transition' USING ERRCODE = '55000';
    END IF;

    -- The ADR-004 transition table up to UNKNOWN_OUTCOME. Anything else is
    -- forbidden; Phase 7 adds reconciliation and human resolution.
    move := CASE
        WHEN OLD.state = 'RECEIVED'         AND NEW.state = 'DENIED'           THEN 'T2'
        WHEN OLD.state = 'RECEIVED'         AND NEW.state = 'AUTHORIZED'       THEN 'T3'
        WHEN OLD.state = 'RECEIVED'         AND NEW.state = 'PENDING_APPROVAL' THEN 'T4'
        WHEN OLD.state = 'RECEIVED'         AND NEW.state = 'EXPIRED'          THEN 'T5'
        WHEN OLD.state = 'PENDING_APPROVAL' AND NEW.state = 'AUTHORIZED'       THEN 'T6'
        WHEN OLD.state = 'PENDING_APPROVAL' AND NEW.state = 'DENIED'           THEN 'T7'
        WHEN OLD.state = 'PENDING_APPROVAL' AND NEW.state = 'EXPIRED'          THEN 'T8'
        WHEN OLD.state = 'PENDING_APPROVAL' AND NEW.state = 'CANCELLED'        THEN 'T9'
        WHEN OLD.state = 'AUTHORIZED'       AND NEW.state = 'QUEUED'           THEN 'T10'
        WHEN OLD.state = 'AUTHORIZED'       AND NEW.state = 'PENDING_APPROVAL' THEN 'T11'
        WHEN OLD.state = 'AUTHORIZED'       AND NEW.state = 'DENIED'           THEN 'T12'
        WHEN OLD.state = 'AUTHORIZED'       AND NEW.state = 'EXPIRED'          THEN 'T13'
        WHEN OLD.state = 'AUTHORIZED'       AND NEW.state = 'CANCELLED'        THEN 'T13'
        WHEN OLD.state = 'QUEUED'           AND NEW.state = 'LEASED'           THEN 'T14'
        WHEN OLD.state = 'QUEUED'           AND NEW.state = 'EXPIRED'          THEN 'T15'
        WHEN OLD.state = 'QUEUED'           AND NEW.state = 'CANCELLED'        THEN 'T15'
        WHEN OLD.state = 'LEASED'           AND NEW.state = 'EXECUTING'        THEN 'T16'
        WHEN OLD.state = 'LEASED'           AND NEW.state = 'AUTHORIZED'       THEN 'T16a'
        WHEN OLD.state = 'LEASED'           AND NEW.state = 'DENIED'           THEN 'T16b'
        WHEN OLD.state = 'LEASED'           AND NEW.state = 'QUEUED'           THEN 'T17'
        WHEN OLD.state = 'LEASED'           AND NEW.state = 'CANCELLED'        THEN 'T18'
        WHEN OLD.state = 'LEASED'           AND NEW.state = 'EXPIRED'          THEN 'T18'
        WHEN OLD.state = 'EXECUTING'        AND NEW.state = 'SUCCEEDED'        THEN 'T19'
        WHEN OLD.state = 'EXECUTING'        AND NEW.state = 'RETRY_WAIT'       THEN 'T20'  -- or T22a / T24
        WHEN OLD.state = 'EXECUTING'        AND NEW.state = 'FAILED'           THEN 'T21'
        WHEN OLD.state = 'EXECUTING'        AND NEW.state = 'UNKNOWN_OUTCOME'  THEN 'T22'  -- or T23
        WHEN OLD.state = 'RETRY_WAIT'       AND NEW.state = 'QUEUED'           THEN 'T25'
        WHEN OLD.state = 'RETRY_WAIT'       AND NEW.state = 'AUTHORIZED'       THEN 'T26'
        WHEN OLD.state = 'RETRY_WAIT'       AND NEW.state = 'FAILED'           THEN 'T27'
    END;
    IF move IS NULL THEN
        RAISE EXCEPTION 'illegal action transition % -> %', OLD.state, NEW.state USING ERRCODE = '55000';
    END IF;
    IF OLD.state = 'EXECUTING' THEN
        SELECT * INTO att FROM eacp.action_attempts
        WHERE tenant_id = NEW.tenant_id AND action_id = NEW.id AND lease_generation = OLD.lease_generation;
        -- The sweeper acts on an expired lease (T23, T24); the worker on its
        -- classified result (T20 no-effect, T22a ambiguous read).
        IF comp = 'sweeper' AND move = 'T20' THEN
            move := 'T24';
        ELSIF comp = 'sweeper' AND move = 'T22' THEN
            move := 'T23';
        ELSIF move = 'T20' AND att.outcome = 'ambiguous' THEN
            move := 'T22a';
        END IF;
    END IF;

    -- Actors (ADR-004 actor column). Agents act only on their own actions.
    PERFORM eacp.assert_action_agent(who, OLD.agent_id);
    IF NEW.state = 'CANCELLED' THEN
        -- REQ (the agent or the subject) or an operator; never the system.
        IF akind = 'system' AND comp <> 'owner' THEN
            RAISE EXCEPTION 'the system does not cancel actions' USING ERRCODE = '42501';
        ELSIF akind = 'principal' AND who_id IS DISTINCT FROM OLD.subject_principal_id THEN
            PERFORM eacp.assert_role(who_id, 'operator');
        END IF;
        PERFORM eacp.require_reason(NEW.state_reason, 'a cancellation');
    ELSIF move NOT IN ('T6', 'T7', 'T8') AND akind = 'principal' THEN
        -- Governance, release, execution and expiry are agent or system work;
        -- approval outcomes (T6-T8) follow the approval rows.
        RAISE EXCEPTION 'principals do not drive governance transitions' USING ERRCODE = '42501';
    END IF;
    IF comp = 'worker' AND move NOT IN ('T14', 'T16', 'T16a', 'T16b', 'T17', 'T18', 'T19', 'T20', 'T21', 'T22', 'T22a') THEN
        RAISE EXCEPTION 'the worker performs only execution transitions' USING ERRCODE = '42501';
    END IF;
    IF move IN ('T14', 'T16', 'T16a', 'T16b', 'T19', 'T20', 'T21', 'T22', 'T22a') AND comp IS DISTINCT FROM 'worker' THEN
        RAISE EXCEPTION 'only a worker performs %', move USING ERRCODE = '42501';
    END IF;
    IF move IN ('T23', 'T24', 'T25', 'T26', 'T27') AND comp IS DISTINCT FROM 'sweeper' THEN
        RAISE EXCEPTION 'only the sweeper performs %', move USING ERRCODE = '42501';
    END IF;
    IF move IN ('T17', 'T18') AND NEW.state <> 'CANCELLED' AND COALESCE(comp, '') NOT IN ('worker', 'sweeper') THEN
        RAISE EXCEPTION 'only the lease holder or the sweeper performs %', move USING ERRCODE = '42501';
    END IF;

    mutable := mutable || CASE
        WHEN move IN ('T2', 'T12') THEN ARRAY['decision_evidence_id']
        WHEN move = 'T3'  THEN ARRAY['decision_evidence_id', 'enforced_payload']
        WHEN move = 'T4'  THEN ARRAY['decision_evidence_id', 'enforced_payload', 'approval_request_id']
        WHEN move = 'T10' THEN ARRAY['decision_evidence_id', 'connector_contract_id']
        WHEN move = 'T11' THEN ARRAY['decision_evidence_id', 'approval_request_id']
        WHEN move = 'T14' THEN ARRAY['lease_generation', 'worker_id', 'leased_until', 'heartbeat_at']
        WHEN move = 'T16' THEN ARRAY['leased_until', 'heartbeat_at', 'attempt_count', 'dispatch_intent_at']
        WHEN move IN ('T16a', 'T16b', 'T17', 'T18', 'T21', 'T22', 'T23') THEN ARRAY['leased_until', 'heartbeat_at']
        WHEN move = 'T19' THEN ARRAY['leased_until', 'heartbeat_at', 'external_reference']
        WHEN move IN ('T20', 'T22a', 'T24') THEN ARRAY['leased_until', 'heartbeat_at', 'next_attempt_at']
        WHEN move IN ('T25', 'T26', 'T27') THEN ARRAY['next_attempt_at']
        ELSE '{}'::text[] END;
    -- operation_key is generated after BEFORE triggers and is not comparable here.
    mutable := mutable || ARRAY['operation_key'];
    IF (to_jsonb(NEW) - mutable) IS DISTINCT FROM (to_jsonb(OLD) - mutable) THEN
        RAISE EXCEPTION 'transition % cannot change these action columns', move USING ERRCODE = '55000';
    END IF;

    IF NEW.decision_evidence_id IS DISTINCT FROM OLD.decision_evidence_id THEN
        SELECT * INTO ev FROM eacp.decision_evidence
        WHERE tenant_id = NEW.tenant_id AND id = NEW.decision_evidence_id;
        IF NOT FOUND OR ev.action_id <> NEW.id OR ev.input_digest <> NEW.input_digest THEN
            RAISE EXCEPTION 'decision evidence does not belong to this action' USING ERRCODE = '55000';
        END IF;
        NEW.policy_bundle_id := ev.policy_bundle_id;
        NEW.policy_version := ev.policy_version;
    END IF;
    IF OLD.state IN ('LEASED', 'EXECUTING') AND move <> 'T16' THEN
        -- Leaving the lease: the lease ends with the transition.
        NEW.leased_until := NULL;
        NEW.heartbeat_at := NULL;
    END IF;
    IF move IN ('T20', 'T21', 'T22', 'T22a', 'T24') OR move = 'T19' THEN
        SELECT * INTO ct FROM eacp.tool_contracts
        WHERE tenant_id = NEW.tenant_id AND id = NEW.connector_contract_id FOR SHARE;
    END IF;

    CASE move
    WHEN 'T2', 'T12' THEN
        PERFORM eacp.require_reason(NEW.state_reason, 'a denial');

    WHEN 'T3', 'T4' THEN
        PERFORM eacp.assert_governance_evidence(ev, NEW);
        IF (move = 'T3' AND ev.verdict NOT IN ('allow', 'warn', 'transform'))
           OR (move = 'T4' AND ev.verdict <> 'escalate') THEN
            RAISE EXCEPTION 'verdict % does not permit %', ev.verdict, move USING ERRCODE = '55000';
        END IF;
        IF NEW.enforced_payload IS NULL OR NEW.enforced_payload::jsonb IS DISTINCT FROM ev.enforced_payload THEN
            RAISE EXCEPTION 'enforced payload differs from the decision' USING ERRCODE = '55000';
        END IF;
        NEW.enforced_digest := ev.enforced_digest;
        IF NEW.not_after <= now() THEN
            RAISE EXCEPTION 'action has expired' USING ERRCODE = '55000';
        END IF;
        PERFORM 1 FROM eacp.principals
        WHERE tenant_id = NEW.tenant_id AND id = NEW.subject_principal_id
          AND kind = 'human' AND disabled_at IS NULL FOR SHARE;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'action subject is not an enabled human' USING ERRCODE = '55000';
        END IF;
        denial := eacp.action_capability_denial(NEW.agent_version_id, NEW.tool_id);
        IF denial IS NOT NULL THEN
            RAISE EXCEPTION 'capability check failed: %', denial USING ERRCODE = '55000';
        END IF;
        IF move = 'T4' THEN
            SELECT * INTO req FROM eacp.approval_requests
            WHERE tenant_id = NEW.tenant_id AND id = NEW.approval_request_id;
            IF NOT FOUND OR req.action_id <> NEW.id OR req.state <> 'PENDING'
               OR req.decision_evidence_id <> ev.id THEN
                RAISE EXCEPTION 'escalation requires a pending request bound to the decision' USING ERRCODE = '55000';
            END IF;
        END IF;

    WHEN 'T5', 'T13', 'T15' THEN
        IF NEW.state = 'EXPIRED' AND now() < NEW.not_after THEN
            -- An approval that expired before release expires the action.
            SELECT * INTO req FROM eacp.approval_requests
            WHERE tenant_id = NEW.tenant_id AND id = OLD.approval_request_id;
            IF move <> 'T13' OR NOT FOUND OR req.state <> 'EXPIRED' THEN
                RAISE EXCEPTION 'action has not expired' USING ERRCODE = '55000';
            END IF;
        END IF;

    WHEN 'T6', 'T7', 'T8' THEN
        SELECT * INTO req FROM eacp.approval_requests
        WHERE tenant_id = NEW.tenant_id AND id = OLD.approval_request_id;
        IF (move = 'T6' AND (req.state IS DISTINCT FROM 'GRANTED' OR NOT EXISTS (
                SELECT 1 FROM eacp.approval_grants g
                WHERE g.tenant_id = NEW.tenant_id AND g.request_id = req.id
                  AND g.consumed_at IS NULL AND g.expires_at > now())))
           OR (move = 'T7' AND req.state IS DISTINCT FROM 'DENIED')
           OR (move = 'T8' AND req.state IS DISTINCT FROM 'EXPIRED' AND now() < NEW.not_after) THEN
            RAISE EXCEPTION 'approval state does not permit %', move USING ERRCODE = '55000';
        END IF;

    WHEN 'T9' THEN
        NULL;  -- actor and reason checked above

    WHEN 'T10' THEN
        -- The release boundary (ADR-005 §5a R1). The revalidation decision
        -- must be recorded in this transaction under the locked pointer.
        IF NEW.decision_evidence_id IS NOT DISTINCT FROM OLD.decision_evidence_id OR ev.recorded_at <> now() THEN
            RAISE EXCEPTION 'release requires a revalidation recorded in this transaction' USING ERRCODE = '55000';
        END IF;
        PERFORM eacp.assert_governance_evidence(ev, NEW);
        IF ev.verdict NOT IN ('allow', 'warn', 'transform', 'escalate')
           OR ev.enforced_digest <> OLD.enforced_digest
           OR ev.enforced_payload IS DISTINCT FROM OLD.enforced_payload::jsonb THEN
            RAISE EXCEPTION 'revalidation does not permit release of this payload' USING ERRCODE = '55000';
        END IF;
        IF ev.verdict = 'escalate' THEN
            SELECT * INTO req FROM eacp.approval_requests
            WHERE tenant_id = NEW.tenant_id AND id = OLD.approval_request_id;
            IF NOT FOUND OR req.policy_version <> ev.policy_version OR NOT EXISTS (
                SELECT 1 FROM eacp.approval_grants g
                WHERE g.tenant_id = NEW.tenant_id AND g.request_id = req.id AND g.action_id = NEW.id
                  AND g.consumed_by_action_id = NEW.id AND g.consumed_at = now()
                  AND g.enforced_digest = OLD.enforced_digest AND g.policy_version = ev.policy_version) THEN
                RAISE EXCEPTION 'release requires consuming a grant for this digest and policy' USING ERRCODE = '55000';
            END IF;
        ELSIF EXISTS (SELECT 1 FROM eacp.approval_requests
                      WHERE tenant_id = NEW.tenant_id AND action_id = NEW.id AND state IN ('PENDING', 'GRANTED')) THEN
            RAISE EXCEPTION 'outstanding approval state must be voided before release' USING ERRCODE = '55000';
        END IF;
        IF NEW.not_after <= now() THEN
            RAISE EXCEPTION 'action has expired' USING ERRCODE = '55000';
        END IF;
        PERFORM 1 FROM eacp.principals
        WHERE tenant_id = NEW.tenant_id AND id = NEW.subject_principal_id
          AND kind = 'human' AND disabled_at IS NULL FOR SHARE;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'action subject is not an enabled human' USING ERRCODE = '55000';
        END IF;
        denial := eacp.action_capability_denial(NEW.agent_version_id, NEW.tool_id);
        IF denial IS NOT NULL THEN
            RAISE EXCEPTION 'capability check failed: %', denial USING ERRCODE = '55000';
        END IF;
        SELECT c.version INTO contract_version FROM eacp.tools t
        JOIN eacp.tool_contracts c ON c.tenant_id = t.tenant_id AND c.id = t.active_contract_id
        WHERE t.tenant_id = NEW.tenant_id AND t.id = NEW.tool_id AND c.id = NEW.connector_contract_id;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'release must pin the active contract' USING ERRCODE = '55000';
        END IF;
        NEW.connector_contract_version := contract_version;
        NEW.released_at := now();

    WHEN 'T11' THEN
        IF NEW.decision_evidence_id IS NOT DISTINCT FROM OLD.decision_evidence_id OR ev.recorded_at <> now()
           OR ev.verdict <> 'escalate' OR ev.enforced_digest <> OLD.enforced_digest
           OR ev.enforced_payload IS DISTINCT FROM OLD.enforced_payload::jsonb THEN
            RAISE EXCEPTION 're-approval requires a fresh escalation of the same payload' USING ERRCODE = '55000';
        END IF;
        PERFORM eacp.assert_governance_evidence(ev, NEW);
        SELECT * INTO req FROM eacp.approval_requests
        WHERE tenant_id = NEW.tenant_id AND id = NEW.approval_request_id;
        IF NOT FOUND OR req.id IS NOT DISTINCT FROM OLD.approval_request_id OR req.action_id <> NEW.id
           OR req.state <> 'PENDING' OR req.decision_evidence_id <> ev.id THEN
            RAISE EXCEPTION 're-approval requires a new pending request bound to the decision' USING ERRCODE = '55000';
        END IF;

    WHEN 'T14' THEN
        -- Claim (§22): the next generation, bound to the claiming worker.
        IF NEW.lease_generation <> OLD.lease_generation + 1
           OR eacp.current_lease_generation() IS DISTINCT FROM NEW.lease_generation
           OR NEW.worker_id IS DISTINCT FROM who->>'worker' THEN
            RAISE EXCEPTION 'a claim takes the next lease generation for the claiming worker' USING ERRCODE = '42501';
        END IF;
        IF NEW.leased_until <= now() OR NEW.leased_until > now() + interval '10 minutes' THEN
            RAISE EXCEPTION 'a lease lasts at most 10 minutes' USING ERRCODE = '23514';
        END IF;
        IF NEW.not_after <= now() THEN
            RAISE EXCEPTION 'action has expired' USING ERRCODE = '55000';
        END IF;
        NEW.heartbeat_at := now();

    WHEN 'T16' THEN
        -- The fenced dispatch intent (§23.1 step 1): its own transaction,
        -- before any external call; the attempt row is inserted by
        -- actions_after in this transaction.
        PERFORM eacp.assert_lease_holder(who, OLD);
        IF OLD.leased_until <= now() THEN
            RAISE EXCEPTION 'the lease has expired' USING ERRCODE = '55000';
        END IF;
        IF NEW.attempt_count <> OLD.attempt_count + 1 THEN
            RAISE EXCEPTION 'a dispatch intent is the next attempt' USING ERRCODE = '55000';
        END IF;
        IF OLD.cancel_requested_at IS NOT NULL OR NEW.not_after <= now() THEN
            RAISE EXCEPTION 'a cancelled or expired action is not dispatched' USING ERRCODE = '55000';
        END IF;
        denial := eacp.dispatch_drift(NEW);
        IF denial IS NOT NULL THEN
            RAISE EXCEPTION 'dispatch is not permitted: %', denial USING ERRCODE = '55000';
        END IF;
        IF NEW.leased_until <= now() + eacp.call_timeout(NEW.connector_contract_id) + interval '1 second'
           OR NEW.leased_until > now() + interval '10 minutes' THEN
            RAISE EXCEPTION 'the lease must outlive the call and last at most 10 minutes' USING ERRCODE = '23514';
        END IF;
        NEW.dispatch_intent_at := now();
        NEW.heartbeat_at := now();

    WHEN 'T16a' THEN
        -- Policy drift before dispatch: back through the release boundary.
        PERFORM eacp.assert_lease_holder(who, OLD);
        IF eacp.dispatch_drift(NEW) IS DISTINCT FROM 'policy_changed' THEN
            RAISE EXCEPTION 'T16a requires a changed policy pointer and no revocation' USING ERRCODE = '55000';
        END IF;
        PERFORM eacp.require_reason(NEW.state_reason, 'a return to the release boundary');

    WHEN 'T16b' THEN
        PERFORM eacp.assert_lease_holder(who, OLD);
        denial := eacp.dispatch_drift(NEW);
        IF denial IS NOT NULL AND denial <> 'policy_changed' THEN
            NEW.state_reason := 'revoked_before_dispatch: ' || denial;
        ELSIF NEW.state_reason IS DISTINCT FROM 'enforced_digest_mismatch' THEN
            -- The worker recomputes the enforced digest (JCS) before dispatch;
            -- a mismatch denies (invariant 14). Nothing else denies here.
            RAISE EXCEPTION 'T16b requires a revocation or an enforced digest mismatch' USING ERRCODE = '55000';
        END IF;

    WHEN 'T17' THEN
        IF comp = 'worker' THEN
            PERFORM eacp.assert_lease_holder(who, OLD);  -- voluntary release
        ELSIF OLD.leased_until > now() THEN
            RAISE EXCEPTION 'the lease has not expired' USING ERRCODE = '55000';
        END IF;
        PERFORM eacp.require_reason(NEW.state_reason, 'a lease release');

    WHEN 'T18' THEN
        IF NEW.state = 'EXPIRED' THEN
            IF comp = 'worker' THEN
                PERFORM eacp.assert_lease_holder(who, OLD);
            END IF;
            IF now() < NEW.not_after THEN
                RAISE EXCEPTION 'action has not expired' USING ERRCODE = '55000';
            END IF;
        END IF;
        -- CANCELLED: the requester or an operator (checked above). The row
        -- lock orders it against T16, which then no longer finds LEASED.

    WHEN 'T19', 'T20', 'T21', 'T22', 'T22a' THEN
        -- Fenced result commit (§23.1 step 3) with the attempt completed, not
        -- late, in this transaction.
        PERFORM eacp.assert_lease_holder(who, OLD);
        outcome := CASE move WHEN 'T19' THEN 'succeeded' WHEN 'T20' THEN 'no_effect'
                             WHEN 'T21' THEN 'no_effect' ELSE 'ambiguous' END;
        IF att.completed_at IS DISTINCT FROM now() OR att.late OR att.outcome IS DISTINCT FROM outcome THEN
            RAISE EXCEPTION '% requires this attempt''s matching outcome in this transaction', move
                USING ERRCODE = '55000';
        END IF;
        IF move = 'T19' THEN
            NEW.external_reference := att.external_reference;
        ELSIF move IN ('T20', 'T22a') THEN
            IF OLD.cancel_requested_at IS NOT NULL OR NEW.not_after <= now()
               OR NEW.attempt_count >= ct.max_attempts
               OR (move = 'T22a' AND (ct.side_effects <> ARRAY['READ_ONLY'] OR ct.revoked_at IS NOT NULL)) THEN
                RAISE EXCEPTION '% does not permit a retry', move USING ERRCODE = '55000';
            END IF;
            IF NEW.next_attempt_at <= now() OR NEW.next_attempt_at > now() + interval '1 hour' THEN
                RAISE EXCEPTION 'a retry is scheduled within the next hour' USING ERRCODE = '23514';
            END IF;
        END IF;

    WHEN 'T23' THEN
        -- Lease lost while EXECUTING: the effect may have happened.
        IF OLD.leased_until > now() THEN
            RAISE EXCEPTION 'the lease has not expired' USING ERRCODE = '55000';
        END IF;
        PERFORM eacp.require_reason(NEW.state_reason, 'an unknown outcome');

    WHEN 'T24' THEN
        -- Slice A narrows T24 to READ_ONLY (ADR-004 Rev 2.3).
        IF OLD.leased_until > now() THEN
            RAISE EXCEPTION 'the lease has not expired' USING ERRCODE = '55000';
        END IF;
        IF ct.side_effects <> ARRAY['READ_ONLY'] OR ct.revoked_at IS NOT NULL THEN
            RAISE EXCEPTION 'only an unrevoked READ_ONLY contract retries after a lost lease' USING ERRCODE = '55000';
        END IF;
        NEW.next_attempt_at := now();

    WHEN 'T25', 'T26', 'T27' THEN
        SELECT * INTO ct FROM eacp.tool_contracts
        WHERE tenant_id = NEW.tenant_id AND id = NEW.connector_contract_id FOR SHARE;
        IF move = 'T27' THEN
            IF NOT (OLD.cancel_requested_at IS NOT NULL OR NEW.not_after <= now()
                    OR NEW.attempt_count >= ct.max_attempts) THEN
                RAISE EXCEPTION 'the retry budget is not exhausted' USING ERRCODE = '55000';
            END IF;
            PERFORM eacp.require_reason(NEW.state_reason, 'a failure');
        ELSE
            IF OLD.next_attempt_at > now() OR OLD.cancel_requested_at IS NOT NULL
               OR NEW.not_after <= now() OR NEW.attempt_count >= ct.max_attempts THEN
                RAISE EXCEPTION 'the retry is not due' USING ERRCODE = '55000';
            END IF;
            SELECT * INTO p FROM eacp.tenant_policy_pointer WHERE tenant_id = NEW.tenant_id FOR SHARE;
            IF (move = 'T25') <> (p.current_bundle_id IS NOT DISTINCT FROM NEW.policy_bundle_id
                                  AND p.current_version IS NOT DISTINCT FROM NEW.policy_version) THEN
                RAISE EXCEPTION 'T25 retries under the pinned policy; T26 re-releases under a new one'
                    USING ERRCODE = '55000';
            END IF;
        END IF;
        NEW.next_attempt_at := NULL;
    END CASE;

    NEW.state_changed_at := now();
    NEW.state_actor_kind := akind;
    NEW.state_actor_id := who_id;
    NEW.state_actor_component := comp;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Terminal actions void their outstanding approval state; a QUEUED action
-- emits one outbox hint (on release and on every re-queue); a dispatch
-- intent inserts its attempt row.
CREATE OR REPLACE FUNCTION eacp.actions_after() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.state IS NOT DISTINCT FROM OLD.state THEN
        RETURN NULL;  -- heartbeat or cancel request
    END IF;
    IF NEW.state IN ('DENIED', 'CANCELLED', 'EXPIRED') THEN
        UPDATE eacp.approval_requests SET state = 'VOIDED'
        WHERE tenant_id = NEW.tenant_id AND action_id = NEW.id AND state IN ('PENDING', 'GRANTED');
        UPDATE eacp.approval_grants SET expires_at = now()
        WHERE tenant_id = NEW.tenant_id AND action_id = NEW.id AND consumed_at IS NULL AND expires_at > now();
    ELSIF NEW.state = 'QUEUED' THEN
        INSERT INTO eacp.outbox_events (tenant_id, topic, aggregate_id, payload, traceparent)
        VALUES (NEW.tenant_id, 'action.queued', NEW.id,
                jsonb_build_object('action_id', NEW.id),
                COALESCE(NEW.traceparent, eacp.current_traceparent()));
    ELSIF NEW.state = 'EXECUTING' THEN
        INSERT INTO eacp.action_attempts (tenant_id, action_id, attempt_no, lease_generation)
        VALUES (NEW.tenant_id, NEW.id, NEW.attempt_count, NEW.lease_generation);
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The action journal. Payloads and digests stay out of the audit chain,
-- heartbeats are not transitions and are not journaled.
CREATE OR REPLACE FUNCTION eacp.audit_action_change() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    who jsonb := eacp.actor_context();
    kind text;
BEGIN
    IF TG_OP = 'INSERT' THEN
        kind := 'action.received';
    ELSIF NEW.state IS DISTINCT FROM OLD.state THEN
        kind := 'action.transition';
    ELSIF NEW.cancel_requested_at IS DISTINCT FROM OLD.cancel_requested_at THEN
        kind := 'action.cancel_requested';
    ELSE
        RETURN NULL;
    END IF;
    INSERT INTO eacp.audit_events (tenant_id, payload)
    VALUES (NEW.tenant_id, convert_to(jsonb_build_object(
        'v', 1,
        'actor', who - 'component',
        'action', kind,
        'subject', jsonb_build_object('type', 'action', 'id', NEW.id),
        'reason', COALESCE(CASE WHEN kind = 'action.cancel_requested' THEN NEW.cancel_reason END,
                           NEW.state_reason, CASE WHEN TG_OP = 'INSERT' THEN 'submitted' ELSE 'transition' END),
        'data', jsonb_strip_nulls(jsonb_build_object(
            'from', CASE WHEN TG_OP = 'UPDATE' THEN OLD.state END,
            'to', NEW.state,
            'component', who->>'component',
            'agent_version_id', NEW.agent_version_id,
            'operation', NEW.operation,
            'tool', NEW.tool,
            'decision_evidence_id', NEW.decision_evidence_id,
            'policy_version', NEW.policy_version,
            'approval_request_id', NEW.approval_request_id,
            'connector_contract_version', NEW.connector_contract_version,
            'lease_generation', NULLIF(NEW.lease_generation, 0),
            'worker_id', NEW.worker_id,
            'attempt', NULLIF(NEW.attempt_count, 0),
            'external_reference', NEW.external_reference,
            'next_attempt_at', NEW.next_attempt_at)))::text, 'UTF8'));
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- ------------------------------------------------------------ attempts

-- +goose StatementBegin
CREATE FUNCTION eacp.action_attempts_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    who jsonb := eacp.actor_context();
    a   eacp.actions%ROWTYPE;
    ct  eacp.tool_contracts%ROWTYPE;
BEGIN
    SELECT * INTO a FROM eacp.actions
    WHERE tenant_id = NEW.tenant_id AND id = NEW.action_id FOR SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'an attempt requires an action' USING ERRCODE = '23503';
    END IF;
    IF TG_OP = 'INSERT' THEN
        -- Only the dispatch intent of this generation creates an attempt.
        PERFORM eacp.assert_lease_holder(who, a);
        IF a.state <> 'EXECUTING' OR a.dispatch_intent_at IS DISTINCT FROM now()
           OR NEW.lease_generation <> a.lease_generation OR NEW.attempt_no <> a.attempt_count
           OR num_nonnulls(NEW.completed_at, NEW.outcome, NEW.external_reference, NEW.error_class) > 0
           OR NEW.late THEN
            RAISE EXCEPTION 'an attempt is created by its dispatch intent' USING ERRCODE = '55000';
        END IF;
        NEW.worker_id := a.worker_id;
        NEW.operation_key := a.operation_key;
        NEW.connector_contract_id := a.connector_contract_id;
        NEW.connector_contract_version := a.connector_contract_version;
        NEW.enforced_digest := a.enforced_digest;
        NEW.dispatched_at := now();
        NEW.call_deadline := now() + eacp.call_timeout(a.connector_contract_id);
        RETURN NEW;
    END IF;

    -- Completion: once, by the worker of this attempt's generation, with a
    -- classification the pinned contract certifies.
    IF OLD.completed_at IS NOT NULL THEN
        RAISE EXCEPTION 'an attempt completes once' USING ERRCODE = '55000';
    END IF;
    IF (to_jsonb(NEW) - ARRAY['outcome', 'external_reference', 'error_class', 'completed_at', 'late'])
       IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['outcome', 'external_reference', 'error_class', 'completed_at', 'late']) THEN
        RAISE EXCEPTION 'an attempt records only its outcome' USING ERRCODE = '55000';
    END IF;
    IF who->>'component' IS DISTINCT FROM 'worker' OR who->>'worker' IS DISTINCT FROM OLD.worker_id
       OR eacp.current_lease_generation() IS DISTINCT FROM OLD.lease_generation THEN
        RAISE EXCEPTION 'only the worker of this attempt records its outcome' USING ERRCODE = '42501';
    END IF;
    IF NEW.outcome = 'no_effect' THEN
        SELECT * INTO ct FROM eacp.tool_contracts
        WHERE tenant_id = NEW.tenant_id AND id = OLD.connector_contract_id;
        IF NOT (NEW.error_class = ANY (ct.no_effect_errors)) THEN
            RAISE EXCEPTION 'error class % is not certified as no-effect', NEW.error_class USING ERRCODE = '23514';
        END IF;
    ELSIF NEW.outcome IS NULL THEN
        RAISE EXCEPTION 'an attempt completes with an outcome' USING ERRCODE = '23514';
    END IF;
    NEW.completed_at := now();
    NEW.late := NOT (a.state = 'EXECUTING' AND a.lease_generation = OLD.lease_generation);
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Late-result evidence (§23.1 step 4): no state change, journaled.
CREATE FUNCTION eacp.audit_late_result() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    who jsonb := eacp.actor_context();
BEGIN
    INSERT INTO eacp.audit_events (tenant_id, payload)
    VALUES (NEW.tenant_id, convert_to(jsonb_build_object(
        'v', 1,
        'actor', who - 'component',
        'action', 'action.late_result',
        'subject', jsonb_build_object('type', 'action', 'id', NEW.action_id),
        'reason', 'result arrived after the lease was lost',
        'data', jsonb_strip_nulls(jsonb_build_object(
            'component', who->>'component',
            'attempt', NEW.attempt_no,
            'lease_generation', NEW.lease_generation,
            'worker_id', NEW.worker_id,
            'outcome', NEW.outcome,
            'external_reference', NEW.external_reference,
            'error_class', NEW.error_class)))::text, 'UTF8'));
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- At commit, a completed attempt that was not late has moved its action out
-- of EXECUTING at its generation: a result is never recorded without the
-- fenced transition it drives.
CREATE FUNCTION eacp.action_attempts_result_check() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM 1 FROM eacp.actions
    WHERE tenant_id = NEW.tenant_id AND id = NEW.action_id
      AND state = 'EXECUTING' AND lease_generation = NEW.lease_generation;
    IF FOUND THEN
        RAISE EXCEPTION 'an attempt result was recorded without its transition' USING ERRCODE = '55000';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER action_attempts_guard BEFORE INSERT OR UPDATE ON eacp.action_attempts
    FOR EACH ROW EXECUTE FUNCTION eacp.action_attempts_guard();
CREATE TRIGGER zz_audit_late AFTER UPDATE ON eacp.action_attempts
    FOR EACH ROW WHEN (NEW.late) EXECUTE FUNCTION eacp.audit_late_result();
CREATE CONSTRAINT TRIGGER result_check AFTER UPDATE ON eacp.action_attempts
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW WHEN (NOT NEW.late AND NEW.completed_at IS NOT NULL)
    EXECUTE FUNCTION eacp.action_attempts_result_check();

-- ---------------------------------------------------- RLS and privileges

ALTER TABLE eacp.action_attempts ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.action_attempts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.action_attempts USING (tenant_id = eacp.current_tenant_id());
REVOKE UPDATE ON eacp.action_attempts FROM eacp_app;
GRANT UPDATE (outcome, external_reference, error_class) ON eacp.action_attempts TO eacp_app;

GRANT UPDATE (lease_generation, worker_id, leased_until, attempt_count, next_attempt_at,
              cancel_requested_at, cancel_reason) ON eacp.actions TO eacp_app;

-- Cross-tenant scans (as in 00005): the schema owner may read tools and
-- connectors to match claims to a worker's protocols and credentials.
CREATE POLICY owner_scan ON eacp.tools FOR SELECT TO CURRENT_USER USING (true);
CREATE POLICY owner_scan ON eacp.connectors FOR SELECT TO CURRENT_USER USING (true);

-- Admission counts every released, unfinished action (ADR-004 Rev 2.3), so
-- workers draining the queue do not disable the limit.
CREATE OR REPLACE FUNCTION eacp.global_queued_count() RETURNS bigint
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$ SELECT count(*) FROM eacp.actions WHERE state IN ('QUEUED', 'LEASED', 'EXECUTING', 'RETRY_WAIT') $$;

CREATE OR REPLACE FUNCTION eacp.tenants_with_open_actions(after_tenant uuid, lim integer) RETURNS TABLE (tenant_id uuid)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$ SELECT DISTINCT a.tenant_id FROM eacp.actions a
          WHERE a.state IN ('RECEIVED', 'PENDING_APPROVAL', 'AUTHORIZED', 'QUEUED', 'LEASED', 'EXECUTING', 'RETRY_WAIT')
            AND (after_tenant IS NULL OR a.tenant_id > after_tenant)
          ORDER BY a.tenant_id LIMIT least(greatest(lim, 1), 1000) $$;

-- The claim hint (§22): the oldest QUEUED actions whose connector protocol
-- the worker implements and whose (tenant, secret_ref, endpoint host) it
-- holds a credential for. Only tenant and action ids leave the function:
-- the worker claims each in its tenant with FOR UPDATE SKIP LOCKED.
CREATE FUNCTION eacp.claimable_actions(p_protocols text[], p_bindings jsonb, lim integer)
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

REVOKE ALL ON FUNCTION eacp.claimable_actions(text[], jsonb, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.claimable_actions(text[], jsonb, integer) TO eacp_app;

-- +goose Down
DROP FUNCTION eacp.claimable_actions(text[], jsonb, integer);
DROP POLICY owner_scan ON eacp.connectors;
DROP POLICY owner_scan ON eacp.tools;
DROP TABLE eacp.action_attempts;
DROP FUNCTION eacp.action_attempts_result_check();
DROP FUNCTION eacp.audit_late_result();
DROP FUNCTION eacp.action_attempts_guard();

-- Restore the 00005 function bodies (verbatim).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.actor_context() RETURNS jsonb
    LANGUAGE plpgsql STABLE
    AS $$
DECLARE
    p uuid := eacp.current_actor_id();
    g uuid := eacp.current_agent_version_id();
    s text := eacp.current_system_actor();
BEGIN
    IF num_nonnulls(p, g, s) > 1 THEN
        RAISE EXCEPTION 'a transaction has exactly one actor' USING ERRCODE = '42501';
    END IF;
    IF p IS NOT NULL THEN
        PERFORM 1 FROM eacp.principals
        WHERE tenant_id = eacp.current_tenant_id() AND id = p AND disabled_at IS NULL;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'actor % is not an enabled principal', p USING ERRCODE = '42501';
        END IF;
        RETURN jsonb_build_object('kind', 'principal', 'id', p);
    END IF;
    IF g IS NOT NULL THEN
        PERFORM 1 FROM eacp.agent_versions
        WHERE tenant_id = eacp.current_tenant_id() AND id = g AND state NOT IN ('RETIRED', 'REVOKED');
        IF NOT FOUND THEN
            RAISE EXCEPTION 'agent version % cannot act', g USING ERRCODE = '42501';
        END IF;
        RETURN jsonb_build_object('kind', 'agent', 'id', g);
    END IF;
    IF s IS NOT NULL THEN
        IF s <> 'sweeper' THEN
            RAISE EXCEPTION 'unknown system actor %', s USING ERRCODE = '42501';
        END IF;
        RETURN jsonb_build_object('kind', 'system', 'id', '00000000-0000-0000-0000-000000000000'::uuid, 'component', s);
    END IF;
    IF eacp.is_schema_owner() THEN
        RETURN jsonb_build_object('kind', 'system', 'id', '00000000-0000-0000-0000-000000000000'::uuid, 'component', 'owner');
    END IF;
    RAISE EXCEPTION 'no actor set for this transaction' USING ERRCODE = '42501';
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.actions_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    who     jsonb := eacp.actor_context();
    akind   text  := who->>'kind';
    who_id  uuid  := (who->>'id')::uuid;
    move    text;
    mutable text[] := ARRAY['state', 'state_reason'];
    ev      eacp.decision_evidence%ROWTYPE;
    req     eacp.approval_requests%ROWTYPE;
    denial  text;
    contract_version integer;
BEGIN
    IF TG_OP = 'INSERT' THEN
        -- T1: only the authenticated agent version submits its own action.
        IF akind <> 'agent' OR who_id IS DISTINCT FROM NEW.agent_version_id THEN
            RAISE EXCEPTION 'only the authenticated agent version submits its actions' USING ERRCODE = '42501';
        END IF;
        IF NEW.state <> 'RECEIVED' OR num_nonnulls(NEW.decision_evidence_id, NEW.policy_bundle_id,
               NEW.policy_version, NEW.enforced_payload, NEW.enforced_digest, NEW.approval_request_id,
               NEW.connector_contract_id, NEW.connector_contract_version, NEW.released_at, NEW.state_reason) > 0 THEN
            RAISE EXCEPTION 'an action is submitted RECEIVED without governance results' USING ERRCODE = '55000';
        END IF;
        IF NEW.not_after <= now() OR NEW.not_after > now() + interval '24 hours' THEN
            RAISE EXCEPTION 'action lifetime must end within the next 24 hours' USING ERRCODE = '23514';
        END IF;
        SELECT agent_id INTO NEW.agent_id FROM eacp.agent_versions
        WHERE tenant_id = NEW.tenant_id AND id = NEW.agent_version_id;
        -- The database resolves the subject and tool; unknown ones stay NULL
        -- and the action is denied (T2) so the attempt remains auditable.
        SELECT id INTO NEW.subject_principal_id FROM eacp.principals
        WHERE tenant_id = NEW.tenant_id AND subject = NEW.subject AND kind = 'human' AND disabled_at IS NULL;
        SELECT t.id INTO NEW.tool_id FROM eacp.tools t
        JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
        WHERE t.tenant_id = NEW.tenant_id AND c.name || '.' || t.name = NEW.tool;
        NEW.created_at := now();
        NEW.state_changed_at := now();
        NEW.state_actor_kind := akind;
        NEW.state_actor_id := who_id;
        NEW.state_actor_component := NULL;
        RETURN NEW;
    END IF;

    IF OLD.state IN ('SUCCEEDED', 'FAILED', 'DENIED', 'CANCELLED', 'EXPIRED') THEN
        RAISE EXCEPTION 'action % is % (terminal)', OLD.id, OLD.state USING ERRCODE = '55000';
    END IF;
    IF NEW.state IS NOT DISTINCT FROM OLD.state THEN
        RAISE EXCEPTION 'an action changes only by a state transition' USING ERRCODE = '55000';
    END IF;

    -- Phase 4 edges of the ADR-004 transition table. Anything else is
    -- forbidden; later phases extend this function.
    move := CASE
        WHEN OLD.state = 'RECEIVED'         AND NEW.state = 'DENIED'           THEN 'T2'
        WHEN OLD.state = 'RECEIVED'         AND NEW.state = 'AUTHORIZED'       THEN 'T3'
        WHEN OLD.state = 'RECEIVED'         AND NEW.state = 'PENDING_APPROVAL' THEN 'T4'
        WHEN OLD.state = 'RECEIVED'         AND NEW.state = 'EXPIRED'          THEN 'T5'
        WHEN OLD.state = 'PENDING_APPROVAL' AND NEW.state = 'AUTHORIZED'       THEN 'T6'
        WHEN OLD.state = 'PENDING_APPROVAL' AND NEW.state = 'DENIED'           THEN 'T7'
        WHEN OLD.state = 'PENDING_APPROVAL' AND NEW.state = 'EXPIRED'          THEN 'T8'
        WHEN OLD.state = 'PENDING_APPROVAL' AND NEW.state = 'CANCELLED'        THEN 'T9'
        WHEN OLD.state = 'AUTHORIZED'       AND NEW.state = 'QUEUED'           THEN 'T10'
        WHEN OLD.state = 'AUTHORIZED'       AND NEW.state = 'PENDING_APPROVAL' THEN 'T11'
        WHEN OLD.state = 'AUTHORIZED'       AND NEW.state = 'DENIED'           THEN 'T12'
        WHEN OLD.state = 'AUTHORIZED'       AND NEW.state = 'EXPIRED'          THEN 'T13'
        WHEN OLD.state = 'AUTHORIZED'       AND NEW.state = 'CANCELLED'        THEN 'T13'
        WHEN OLD.state = 'QUEUED'           AND NEW.state = 'EXPIRED'          THEN 'T15'
        WHEN OLD.state = 'QUEUED'           AND NEW.state = 'CANCELLED'        THEN 'T15'
    END;
    IF move IS NULL THEN
        RAISE EXCEPTION 'illegal action transition % -> %', OLD.state, NEW.state USING ERRCODE = '55000';
    END IF;

    -- Actors (ADR-004 actor column). Agents act only on their own actions.
    PERFORM eacp.assert_action_agent(who, OLD.agent_id);
    IF NEW.state = 'CANCELLED' THEN
        -- REQ (the agent or the subject) or an operator; never the system.
        IF akind = 'system' AND who->>'component' <> 'owner' THEN
            RAISE EXCEPTION 'the system does not cancel actions' USING ERRCODE = '42501';
        ELSIF akind = 'principal' AND who_id IS DISTINCT FROM OLD.subject_principal_id THEN
            PERFORM eacp.assert_role(who_id, 'operator');
        END IF;
        PERFORM eacp.require_reason(NEW.state_reason, 'a cancellation');
    ELSIF move NOT IN ('T6', 'T7', 'T8') AND akind = 'principal' THEN
        -- Governance, release and expiry are agent or system work; approval
        -- outcomes (T6-T8) follow the approval rows in any actor's transaction.
        RAISE EXCEPTION 'principals do not drive governance transitions' USING ERRCODE = '42501';
    END IF;

    mutable := mutable || CASE move
        WHEN 'T2'  THEN ARRAY['decision_evidence_id']
        WHEN 'T12' THEN ARRAY['decision_evidence_id']
        WHEN 'T3'  THEN ARRAY['decision_evidence_id', 'enforced_payload']
        WHEN 'T4'  THEN ARRAY['decision_evidence_id', 'enforced_payload', 'approval_request_id']
        WHEN 'T10' THEN ARRAY['decision_evidence_id', 'connector_contract_id']
        WHEN 'T11' THEN ARRAY['decision_evidence_id', 'approval_request_id']
        ELSE '{}'::text[] END;
    -- operation_key is generated after BEFORE triggers and is not comparable here.
    mutable := mutable || ARRAY['operation_key'];
    IF (to_jsonb(NEW) - mutable) IS DISTINCT FROM (to_jsonb(OLD) - mutable) THEN
        RAISE EXCEPTION 'transition % cannot change these action columns', move USING ERRCODE = '55000';
    END IF;

    IF NEW.decision_evidence_id IS DISTINCT FROM OLD.decision_evidence_id THEN
        SELECT * INTO ev FROM eacp.decision_evidence
        WHERE tenant_id = NEW.tenant_id AND id = NEW.decision_evidence_id;
        IF NOT FOUND OR ev.action_id <> NEW.id OR ev.input_digest <> NEW.input_digest THEN
            RAISE EXCEPTION 'decision evidence does not belong to this action' USING ERRCODE = '55000';
        END IF;
        NEW.policy_bundle_id := ev.policy_bundle_id;
        NEW.policy_version := ev.policy_version;
    END IF;

    CASE move
    WHEN 'T2', 'T12' THEN
        PERFORM eacp.require_reason(NEW.state_reason, 'a denial');

    WHEN 'T3', 'T4' THEN
        PERFORM eacp.assert_governance_evidence(ev, NEW);
        IF (move = 'T3' AND ev.verdict NOT IN ('allow', 'warn', 'transform'))
           OR (move = 'T4' AND ev.verdict <> 'escalate') THEN
            RAISE EXCEPTION 'verdict % does not permit %', ev.verdict, move USING ERRCODE = '55000';
        END IF;
        IF NEW.enforced_payload IS NULL OR NEW.enforced_payload::jsonb IS DISTINCT FROM ev.enforced_payload THEN
            RAISE EXCEPTION 'enforced payload differs from the decision' USING ERRCODE = '55000';
        END IF;
        NEW.enforced_digest := ev.enforced_digest;
        IF NEW.not_after <= now() THEN
            RAISE EXCEPTION 'action has expired' USING ERRCODE = '55000';
        END IF;
        PERFORM 1 FROM eacp.principals
        WHERE tenant_id = NEW.tenant_id AND id = NEW.subject_principal_id
          AND kind = 'human' AND disabled_at IS NULL FOR SHARE;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'action subject is not an enabled human' USING ERRCODE = '55000';
        END IF;
        denial := eacp.action_capability_denial(NEW.agent_version_id, NEW.tool_id);
        IF denial IS NOT NULL THEN
            RAISE EXCEPTION 'capability check failed: %', denial USING ERRCODE = '55000';
        END IF;
        IF move = 'T4' THEN
            SELECT * INTO req FROM eacp.approval_requests
            WHERE tenant_id = NEW.tenant_id AND id = NEW.approval_request_id;
            IF NOT FOUND OR req.action_id <> NEW.id OR req.state <> 'PENDING'
               OR req.decision_evidence_id <> ev.id THEN
                RAISE EXCEPTION 'escalation requires a pending request bound to the decision' USING ERRCODE = '55000';
            END IF;
        END IF;

    WHEN 'T5', 'T13', 'T15' THEN
        IF NEW.state = 'EXPIRED' AND now() < NEW.not_after THEN
            -- An approval that expired before release expires the action.
            SELECT * INTO req FROM eacp.approval_requests
            WHERE tenant_id = NEW.tenant_id AND id = OLD.approval_request_id;
            IF move <> 'T13' OR NOT FOUND OR req.state <> 'EXPIRED' THEN
                RAISE EXCEPTION 'action has not expired' USING ERRCODE = '55000';
            END IF;
        END IF;

    WHEN 'T6', 'T7', 'T8' THEN
        SELECT * INTO req FROM eacp.approval_requests
        WHERE tenant_id = NEW.tenant_id AND id = OLD.approval_request_id;
        IF (move = 'T6' AND (req.state IS DISTINCT FROM 'GRANTED' OR NOT EXISTS (
                SELECT 1 FROM eacp.approval_grants g
                WHERE g.tenant_id = NEW.tenant_id AND g.request_id = req.id
                  AND g.consumed_at IS NULL AND g.expires_at > now())))
           OR (move = 'T7' AND req.state IS DISTINCT FROM 'DENIED')
           OR (move = 'T8' AND req.state IS DISTINCT FROM 'EXPIRED' AND now() < NEW.not_after) THEN
            RAISE EXCEPTION 'approval state does not permit %', move USING ERRCODE = '55000';
        END IF;

    WHEN 'T9' THEN
        NULL;  -- actor and reason checked above

    WHEN 'T10' THEN
        -- The release boundary (ADR-005 §5a R1). The revalidation decision
        -- must be recorded in this transaction under the locked pointer.
        IF NEW.decision_evidence_id IS NOT DISTINCT FROM OLD.decision_evidence_id OR ev.recorded_at <> now() THEN
            RAISE EXCEPTION 'release requires a revalidation recorded in this transaction' USING ERRCODE = '55000';
        END IF;
        PERFORM eacp.assert_governance_evidence(ev, NEW);
        IF ev.verdict NOT IN ('allow', 'warn', 'transform', 'escalate')
           OR ev.enforced_digest <> OLD.enforced_digest
           OR ev.enforced_payload IS DISTINCT FROM OLD.enforced_payload::jsonb THEN
            RAISE EXCEPTION 'revalidation does not permit release of this payload' USING ERRCODE = '55000';
        END IF;
        IF ev.verdict = 'escalate' THEN
            SELECT * INTO req FROM eacp.approval_requests
            WHERE tenant_id = NEW.tenant_id AND id = OLD.approval_request_id;
            IF NOT FOUND OR req.policy_version <> ev.policy_version OR NOT EXISTS (
                SELECT 1 FROM eacp.approval_grants g
                WHERE g.tenant_id = NEW.tenant_id AND g.request_id = req.id AND g.action_id = NEW.id
                  AND g.consumed_by_action_id = NEW.id AND g.consumed_at = now()
                  AND g.enforced_digest = OLD.enforced_digest AND g.policy_version = ev.policy_version) THEN
                RAISE EXCEPTION 'release requires consuming a grant for this digest and policy' USING ERRCODE = '55000';
            END IF;
        ELSIF EXISTS (SELECT 1 FROM eacp.approval_requests
                      WHERE tenant_id = NEW.tenant_id AND action_id = NEW.id AND state IN ('PENDING', 'GRANTED')) THEN
            RAISE EXCEPTION 'outstanding approval state must be voided before release' USING ERRCODE = '55000';
        END IF;
        IF NEW.not_after <= now() THEN
            RAISE EXCEPTION 'action has expired' USING ERRCODE = '55000';
        END IF;
        PERFORM 1 FROM eacp.principals
        WHERE tenant_id = NEW.tenant_id AND id = NEW.subject_principal_id
          AND kind = 'human' AND disabled_at IS NULL FOR SHARE;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'action subject is not an enabled human' USING ERRCODE = '55000';
        END IF;
        denial := eacp.action_capability_denial(NEW.agent_version_id, NEW.tool_id);
        IF denial IS NOT NULL THEN
            RAISE EXCEPTION 'capability check failed: %', denial USING ERRCODE = '55000';
        END IF;
        SELECT c.version INTO contract_version FROM eacp.tools t
        JOIN eacp.tool_contracts c ON c.tenant_id = t.tenant_id AND c.id = t.active_contract_id
        WHERE t.tenant_id = NEW.tenant_id AND t.id = NEW.tool_id AND c.id = NEW.connector_contract_id;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'release must pin the active contract' USING ERRCODE = '55000';
        END IF;
        NEW.connector_contract_version := contract_version;
        NEW.released_at := now();

    WHEN 'T11' THEN
        IF NEW.decision_evidence_id IS NOT DISTINCT FROM OLD.decision_evidence_id OR ev.recorded_at <> now()
           OR ev.verdict <> 'escalate' OR ev.enforced_digest <> OLD.enforced_digest
           OR ev.enforced_payload IS DISTINCT FROM OLD.enforced_payload::jsonb THEN
            RAISE EXCEPTION 're-approval requires a fresh escalation of the same payload' USING ERRCODE = '55000';
        END IF;
        PERFORM eacp.assert_governance_evidence(ev, NEW);
        SELECT * INTO req FROM eacp.approval_requests
        WHERE tenant_id = NEW.tenant_id AND id = NEW.approval_request_id;
        IF NOT FOUND OR req.id IS NOT DISTINCT FROM OLD.approval_request_id OR req.action_id <> NEW.id
           OR req.state <> 'PENDING' OR req.decision_evidence_id <> ev.id THEN
            RAISE EXCEPTION 're-approval requires a new pending request bound to the decision' USING ERRCODE = '55000';
        END IF;
    END CASE;

    NEW.state_changed_at := now();
    NEW.state_actor_kind := akind;
    NEW.state_actor_id := who_id;
    NEW.state_actor_component := who->>'component';
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.actions_after() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.state IN ('DENIED', 'CANCELLED', 'EXPIRED') THEN
        UPDATE eacp.approval_requests SET state = 'VOIDED'
        WHERE tenant_id = NEW.tenant_id AND action_id = NEW.id AND state IN ('PENDING', 'GRANTED');
        UPDATE eacp.approval_grants SET expires_at = now()
        WHERE tenant_id = NEW.tenant_id AND action_id = NEW.id AND consumed_at IS NULL AND expires_at > now();
    ELSIF NEW.state = 'QUEUED' THEN
        INSERT INTO eacp.outbox_events (tenant_id, topic, aggregate_id, payload, traceparent)
        VALUES (NEW.tenant_id, 'action.queued', NEW.id,
                jsonb_build_object('action_id', NEW.id),
                COALESCE(NEW.traceparent, eacp.current_traceparent()));
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.audit_action_change() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    who jsonb := eacp.actor_context();
BEGIN
    INSERT INTO eacp.audit_events (tenant_id, payload)
    VALUES (NEW.tenant_id, convert_to(jsonb_build_object(
        'v', 1,
        'actor', who - 'component',
        'action', CASE WHEN TG_OP = 'INSERT' THEN 'action.received' ELSE 'action.transition' END,
        'subject', jsonb_build_object('type', 'action', 'id', NEW.id),
        'reason', COALESCE(NEW.state_reason, CASE WHEN TG_OP = 'INSERT' THEN 'submitted' ELSE 'transition' END),
        'data', jsonb_strip_nulls(jsonb_build_object(
            'from', CASE WHEN TG_OP = 'UPDATE' THEN OLD.state END,
            'to', NEW.state,
            'component', who->>'component',
            'agent_version_id', NEW.agent_version_id,
            'operation', NEW.operation,
            'tool', NEW.tool,
            'decision_evidence_id', NEW.decision_evidence_id,
            'policy_version', NEW.policy_version,
            'approval_request_id', NEW.approval_request_id,
            'connector_contract_version', NEW.connector_contract_version)))::text, 'UTF8'));
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE OR REPLACE FUNCTION eacp.global_queued_count() RETURNS bigint
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$ SELECT count(*) FROM eacp.actions WHERE state = 'QUEUED' $$;

CREATE OR REPLACE FUNCTION eacp.tenants_with_open_actions(after_tenant uuid, lim integer) RETURNS TABLE (tenant_id uuid)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$ SELECT DISTINCT a.tenant_id FROM eacp.actions a
          WHERE a.state IN ('RECEIVED', 'PENDING_APPROVAL', 'AUTHORIZED', 'QUEUED')
            AND (after_tenant IS NULL OR a.tenant_id > after_tenant)
          ORDER BY a.tenant_id LIMIT least(greatest(lim, 1), 1000) $$;

DROP FUNCTION eacp.assert_lease_holder(jsonb, eacp.actions);
DROP FUNCTION eacp.dispatch_drift(eacp.actions);
DROP FUNCTION eacp.call_timeout(uuid);
DROP INDEX eacp.actions_retry;
DROP INDEX eacp.actions_leases;
DROP INDEX eacp.actions_claim;
ALTER TABLE eacp.actions
    DROP CONSTRAINT cancel_request_complete,
    DROP CONSTRAINT executing_has_intent,
    DROP CONSTRAINT success_has_reference,
    DROP CONSTRAINT retry_iff_scheduled,
    DROP CONSTRAINT leased_has_worker,
    DROP CONSTRAINT lease_iff_leased,
    DROP COLUMN cancel_reason,
    DROP COLUMN cancel_requested_at,
    DROP COLUMN external_reference,
    DROP COLUMN next_attempt_at,
    DROP COLUMN dispatch_intent_at,
    DROP COLUMN attempt_count,
    DROP COLUMN heartbeat_at,
    DROP COLUMN leased_until,
    DROP COLUMN worker_id,
    DROP COLUMN lease_generation;
DROP FUNCTION eacp.current_lease_generation();
DROP FUNCTION eacp.current_worker_id();
