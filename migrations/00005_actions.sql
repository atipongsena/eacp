-- Slice A Phase 4: actions, the pre-dispatch state machine (ADR-004
-- T1-T15), the atomic release boundary (ADR-005 §5a) and the outbox.
--
-- PostgreSQL re-checks every transition guard, so raw SQL as eacp_app obeys
-- the same state machine as internal/action:
--   * actions_guard       BEFORE INSERT/UPDATE: actor, legal edge, guards,
--                         derived columns, attribution.
--   * actions_after       AFTER UPDATE: void approval state of a terminal
--                         action; outbox row when an action is QUEUED.
--   * zz_audit            AFTER INSERT/UPDATE: hash-chained journal (last).
--   * approval cascades   grant issued -> T6, request denied -> T7,
--                         request expired -> T8 / T13.
--   * release check       a consumed grant must be matched by a QUEUED
--                         action released in the same transaction (deferred).
--
-- Actor context. A transaction has exactly one actor: a principal
-- (app.actor_id), an authenticated agent version (app.agent_version_id) or a
-- named system component (app.system_actor). eacp.actor() is unchanged and
-- still refuses non-principals, so registry and policy writes stay
-- principal-only. Like app.actor_id, these settings are trusted inside the
-- eacp_app credential boundary (ADR-003 §8).
--
-- Lock order for every transaction that changes an action: the action row,
-- then approval rows, then registry rows (FOR SHARE), then the audit chain
-- head (taken by the first audited write). Votes lock the action first too.

-- +goose Up

-- ------------------------------------------------------------ actor context

CREATE FUNCTION eacp.current_agent_version_id() RETURNS uuid
    LANGUAGE sql STABLE
    AS $$ SELECT NULLIF(current_setting('app.agent_version_id', true), '')::uuid $$;

CREATE FUNCTION eacp.current_system_actor() RETURNS text
    LANGUAGE sql STABLE
    AS $$ SELECT NULLIF(current_setting('app.system_actor', true), '') $$;

CREATE FUNCTION eacp.current_traceparent() RETURNS text
    LANGUAGE sql STABLE
    AS $$ SELECT t FROM (SELECT current_setting('app.traceparent', true) AS t) s
          WHERE t ~ '^[0-9a-f]{2}-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$' $$;

-- +goose StatementBegin
-- eacp.actor_context() returns the transaction's single actor:
--   {"kind":"principal","id":...}  an enabled principal of the tenant
--   {"kind":"agent","id":...}      a non-terminal agent version of the tenant
--   {"kind":"system","id":<nil>,"component":"sweeper"|"owner"}
-- The schema owner without any setting is the "owner" system actor.
CREATE FUNCTION eacp.actor_context() RETURNS jsonb
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
-- eacp.assert_action_agent fails when the actor is an agent version of a
-- different agent than the action's. Principals and system actors pass.
CREATE FUNCTION eacp.assert_action_agent(who jsonb, action_agent uuid) RETURNS void
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF who->>'kind' = 'agent' AND NOT EXISTS (
        SELECT 1 FROM eacp.agent_versions
        WHERE tenant_id = eacp.current_tenant_id() AND id = (who->>'id')::uuid AND agent_id = action_agent) THEN
        RAISE EXCEPTION 'an agent may only act on its own actions' USING ERRCODE = '42501';
    END IF;
END
$$;
-- +goose StatementEnd

-- ------------------------------------------------------------------ tables

CREATE TABLE eacp.actions (
    tenant_id                  uuid        NOT NULL REFERENCES eacp.tenants (id),
    id                         uuid        NOT NULL DEFAULT gen_random_uuid(),
    agent_id                   uuid        NOT NULL,
    agent_version_id           uuid        NOT NULL,
    idempotency_key            text        NOT NULL CHECK (idempotency_key ~ '^[!-~]{1,255}$'),
    -- The action binding (ADR-005 §2). Payloads are RFC 8785 text stored
    -- verbatim (json, not jsonb) so a digest can be recomputed exactly.
    subject                    text        NOT NULL CHECK (length(subject) BETWEEN 1 AND 320),
    subject_principal_id       uuid,
    operation                  text        NOT NULL CHECK (btrim(operation) <> '' AND length(operation) <= 256),
    target                     text        NOT NULL CHECK (btrim(target) <> '' AND length(target) <= 256),
    tool                       text        NOT NULL CHECK (btrim(tool) <> '' AND length(tool) <= 128),
    tool_id                    uuid,
    tool_schema_version        text        NOT NULL CHECK (btrim(tool_schema_version) <> '' AND length(tool_schema_version) <= 64),
    resource                   text        NOT NULL CHECK (btrim(resource) <> '' AND length(resource) <= 1024),
    input_payload              json        NOT NULL CHECK (octet_length(input_payload::text) <= 1048576),
    input_digest               bytea       NOT NULL CHECK (octet_length(input_digest) = 32),
    not_after                  timestamptz NOT NULL,
    operation_key              text        GENERATED ALWAYS AS ('eacp:' || tenant_id::text || ':' || id::text) STORED,
    state                      text        NOT NULL DEFAULT 'RECEIVED' CHECK (state IN
                                   ('RECEIVED', 'PENDING_APPROVAL', 'AUTHORIZED', 'QUEUED', 'LEASED', 'EXECUTING',
                                    'RETRY_WAIT', 'UNKNOWN_OUTCOME', 'RECONCILING', 'NEEDS_HUMAN_RESOLUTION',
                                    'SUCCEEDED', 'FAILED', 'DENIED', 'CANCELLED', 'EXPIRED')),
    state_reason               text        CHECK (length(state_reason) <= 1024),
    -- Governance results: the latest decision that drove a transition.
    decision_evidence_id       uuid,
    policy_bundle_id           uuid,
    policy_version             integer,
    enforced_payload           json        CHECK (octet_length(enforced_payload::text) <= 1048576),
    enforced_digest            bytea       CHECK (octet_length(enforced_digest) = 32),
    approval_request_id        uuid,
    -- Pinned at the release boundary (ADR-004 principle 6).
    connector_contract_id      uuid,
    connector_contract_version integer,
    released_at                timestamptz,
    -- W3C trace context of the submitting request, carried to the outbox.
    traceparent                text        CHECK (traceparent ~ '^[0-9a-f]{2}-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$'),
    created_at                 timestamptz NOT NULL DEFAULT now(),
    state_changed_at           timestamptz NOT NULL DEFAULT now(),
    state_actor_kind           text        NOT NULL CHECK (state_actor_kind IN ('principal', 'agent', 'system')),
    state_actor_id             uuid        NOT NULL,
    state_actor_component      text,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, agent_id, idempotency_key),
    CHECK ((enforced_payload IS NULL) = (enforced_digest IS NULL)),
    CHECK ((policy_bundle_id IS NULL) = (policy_version IS NULL)),
    CHECK ((connector_contract_id IS NULL) = (connector_contract_version IS NULL)),
    CHECK ((connector_contract_id IS NULL) = (released_at IS NULL)),
    CHECK (state NOT IN ('PENDING_APPROVAL', 'AUTHORIZED', 'QUEUED')
           OR (decision_evidence_id IS NOT NULL AND enforced_digest IS NOT NULL AND policy_version IS NOT NULL)),
    CHECK (state <> 'PENDING_APPROVAL' OR approval_request_id IS NOT NULL),
    CHECK (state <> 'QUEUED' OR released_at IS NOT NULL),
    FOREIGN KEY (tenant_id, agent_id) REFERENCES eacp.agents (tenant_id, id),
    FOREIGN KEY (tenant_id, agent_version_id) REFERENCES eacp.agent_versions (tenant_id, id),
    FOREIGN KEY (tenant_id, subject_principal_id) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, tool_id) REFERENCES eacp.tools (tenant_id, id),
    FOREIGN KEY (tenant_id, decision_evidence_id) REFERENCES eacp.decision_evidence (tenant_id, id),
    FOREIGN KEY (tenant_id, policy_bundle_id) REFERENCES eacp.policy_bundles (tenant_id, id),
    FOREIGN KEY (tenant_id, policy_version) REFERENCES eacp.policy_bundles (tenant_id, version),
    FOREIGN KEY (tenant_id, approval_request_id) REFERENCES eacp.approval_requests (tenant_id, id),
    FOREIGN KEY (tenant_id, connector_contract_id) REFERENCES eacp.tool_contracts (tenant_id, id)
);
CREATE INDEX actions_open ON eacp.actions (tenant_id, created_at)
    WHERE state IN ('RECEIVED', 'PENDING_APPROVAL', 'AUTHORIZED', 'QUEUED');
CREATE INDEX actions_queued ON eacp.actions (tenant_id) WHERE state = 'QUEUED';

-- Phase 3 left action_id as a future reference; it now references actions.
ALTER TABLE eacp.decision_evidence
    ADD FOREIGN KEY (tenant_id, action_id) REFERENCES eacp.actions (tenant_id, id);
ALTER TABLE eacp.approval_requests
    ADD FOREIGN KEY (tenant_id, action_id) REFERENCES eacp.actions (tenant_id, id);
ALTER TABLE eacp.approval_grants
    ADD FOREIGN KEY (tenant_id, action_id) REFERENCES eacp.actions (tenant_id, id);
-- At most one live approval request per action.
CREATE UNIQUE INDEX approval_requests_one_live ON eacp.approval_requests (tenant_id, action_id)
    WHERE state IN ('PENDING', 'GRANTED');

-- Transactional outbox (MASTER_PLAN §62). A row is a hint carrying only the
-- action id; PostgreSQL stays the execution authority. Publishing (and the
-- published_at grant) arrives with the Phase 5 worker.
CREATE TABLE eacp.outbox_events (
    tenant_id    uuid        NOT NULL REFERENCES eacp.tenants (id),
    id           uuid        NOT NULL DEFAULT gen_random_uuid(),
    topic        text        NOT NULL CHECK (topic ~ '^[a-z][a-z0-9_.]{0,63}$'),
    aggregate_id uuid        NOT NULL,
    payload      jsonb       NOT NULL,
    traceparent  text        CHECK (traceparent ~ '^[0-9a-f]{2}-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$'),
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, aggregate_id) REFERENCES eacp.actions (tenant_id, id)
);
CREATE INDEX outbox_unpublished ON eacp.outbox_events (created_at) WHERE published_at IS NULL;

-- ------------------------------------------------------ capability backstop

-- +goose StatementBegin
-- eacp.action_capability_denial returns NULL when agent version p_version
-- may call tool p_tool now, or the registry.Denial code otherwise. Rows are
-- read FOR SHARE (ADR-004 principle 7).
CREATE FUNCTION eacp.action_capability_denial(p_version uuid, p_tool uuid) RETURNS text
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
-- eacp.assert_governance_evidence fails unless ev is complete evidence of
-- action a (same input) under the current, unrevoked policy.
CREATE FUNCTION eacp.assert_governance_evidence(ev eacp.decision_evidence, a eacp.actions) RETURNS void
    LANGUAGE plpgsql
    AS $$
DECLARE
    p eacp.tenant_policy_pointer%ROWTYPE;
BEGIN
    IF ev.id IS NULL OR ev.action_id <> a.id OR ev.input_digest <> a.input_digest THEN
        RAISE EXCEPTION 'decision evidence does not belong to this action' USING ERRCODE = '55000';
    END IF;
    SELECT * INTO p FROM eacp.tenant_policy_pointer
    WHERE tenant_id = a.tenant_id FOR SHARE;
    IF p.current_bundle_id IS DISTINCT FROM ev.policy_bundle_id
       OR p.current_version IS DISTINCT FROM ev.policy_version THEN
        RAISE EXCEPTION 'decision was made under a policy that is no longer active' USING ERRCODE = '55000';
    END IF;
    PERFORM 1 FROM eacp.policy_bundles
    WHERE tenant_id = a.tenant_id AND id = ev.policy_bundle_id AND revoked_at IS NULL FOR SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'decision policy is revoked' USING ERRCODE = '55000';
    END IF;
END
$$;
-- +goose StatementEnd

-- ------------------------------------------------------- the state machine

-- +goose StatementBegin
CREATE FUNCTION eacp.actions_guard() RETURNS trigger
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
-- Terminal actions void their outstanding approval state; a released action
-- emits one outbox hint in the release transaction.
CREATE FUNCTION eacp.actions_after() RETURNS trigger
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
CREATE FUNCTION eacp.outbox_events_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM eacp.actor_context();
    IF NEW.published_at IS NOT NULL THEN
        RAISE EXCEPTION 'an outbox event starts unpublished' USING ERRCODE = '23514';
    END IF;
    NEW.created_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The action journal. Payloads and digests stay out of the audit chain, as
-- for the governance tables (00004).
CREATE FUNCTION eacp.audit_action_change() RETURNS trigger
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

CREATE TRIGGER actions_guard BEFORE INSERT OR UPDATE ON eacp.actions
    FOR EACH ROW EXECUTE FUNCTION eacp.actions_guard();
CREATE TRIGGER actions_after AFTER UPDATE ON eacp.actions
    FOR EACH ROW EXECUTE FUNCTION eacp.actions_after();
CREATE TRIGGER zz_audit AFTER INSERT OR UPDATE ON eacp.actions
    FOR EACH ROW EXECUTE FUNCTION eacp.audit_action_change();
CREATE TRIGGER outbox_events_guard BEFORE INSERT ON eacp.outbox_events
    FOR EACH ROW EXECUTE FUNCTION eacp.outbox_events_guard();

-- ---------------------------------------- Phase 3 functions, action-aware

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.decision_evidence_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    who jsonb := eacp.actor_context();
    act eacp.actions%ROWTYPE;
    v integer;
BEGIN
    -- The action row is locked by the evaluating transaction (FOR UPDATE);
    -- FOR SHARE here keeps raw SQL in the same lock order.
    SELECT * INTO act FROM eacp.actions
    WHERE tenant_id = NEW.tenant_id AND id = NEW.action_id FOR SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'evidence requires an action' USING ERRCODE = '23503';
    END IF;
    IF who->>'kind' = 'principal' THEN
        RAISE EXCEPTION 'decision evidence is recorded by the governance path' USING ERRCODE = '42501';
    END IF;
    PERFORM eacp.assert_action_agent(who, act.agent_id);
    IF act.state NOT IN ('RECEIVED', 'AUTHORIZED') THEN
        RAISE EXCEPTION 'evidence is recorded only while a decision is pending' USING ERRCODE = '55000';
    END IF;
    IF NEW.input_digest <> act.input_digest THEN
        RAISE EXCEPTION 'evidence does not match the action input' USING ERRCODE = '55000';
    END IF;
    SELECT version INTO v FROM eacp.policy_bundles
    WHERE tenant_id = NEW.tenant_id AND id = NEW.policy_bundle_id FOR SHARE;
    IF NOT FOUND OR v <> NEW.policy_version THEN
        RAISE EXCEPTION 'evidence policy identity mismatch' USING ERRCODE = '23514';
    END IF;
    IF NEW.evaluated_at > now() + interval '1 minute' THEN
        RAISE EXCEPTION 'evidence evaluation time is in the future' USING ERRCODE = '23514';
    END IF;
    NEW.recorded_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.approval_requests_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    who jsonb := eacp.actor_context();
    act eacp.actions%ROWTYPE;
    ev eacp.decision_evidence%ROWTYPE;
    p eacp.tenant_policy_pointer%ROWTYPE;
    b eacp.policy_bundles%ROWTYPE;
    v eacp.agent_versions%ROWTYPE;
    ag eacp.agents%ROWTYPE;
    al eacp.agent_allowlists%ROWTYPE;
    tl eacp.tools%ROWTYPE;
    ct eacp.tool_contracts%ROWTYPE;
    subject eacp.principals%ROWTYPE;
    member uuid;
    members uuid[] := '{}';
    enabling uuid[];
    approvals integer;
BEGIN
    IF TG_OP = 'INSERT' THEN
        SELECT * INTO act FROM eacp.actions
        WHERE tenant_id = NEW.tenant_id AND id = NEW.action_id FOR SHARE;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'approval request requires an action' USING ERRCODE = '23503';
        END IF;
        IF who->>'kind' = 'principal' THEN
            RAISE EXCEPTION 'approval requests are opened by the governance path' USING ERRCODE = '42501';
        END IF;
        PERFORM eacp.assert_action_agent(who, act.agent_id);
        IF act.state NOT IN ('RECEIVED', 'AUTHORIZED') OR act.tool_id IS NULL OR act.subject_principal_id IS NULL THEN
            RAISE EXCEPTION 'action is not awaiting a governance decision' USING ERRCODE = '55000';
        END IF;
        -- The request binds the action; callers cannot supply these.
        NEW.agent_version_id := act.agent_version_id;
        NEW.tool_id := act.tool_id;
        NEW.requesting_subject_id := act.subject_principal_id;
        NEW.not_after := act.not_after;
        -- Evidence is immutable and the app role has no UPDATE grant, so a
        -- row lock is unnecessary (and would require UPDATE privilege).
        SELECT * INTO ev FROM eacp.decision_evidence
        WHERE tenant_id = NEW.tenant_id AND id = NEW.decision_evidence_id;
        IF NOT FOUND OR ev.verdict <> 'escalate' OR ev.action_id <> NEW.action_id THEN
            RAISE EXCEPTION 'request requires matching escalation evidence' USING ERRCODE = '55000';
        END IF;
        SELECT * INTO p FROM eacp.tenant_policy_pointer
        WHERE tenant_id = NEW.tenant_id FOR SHARE;
        IF NOT FOUND OR p.current_version IS DISTINCT FROM ev.policy_version
           OR p.current_bundle_id IS DISTINCT FROM ev.policy_bundle_id THEN
            RAISE EXCEPTION 'approval policy is no longer active' USING ERRCODE = '55000';
        END IF;
        SELECT * INTO b FROM eacp.policy_bundles
        WHERE tenant_id = NEW.tenant_id AND id = ev.policy_bundle_id FOR SHARE;
        IF NOT FOUND OR b.revoked_at IS NOT NULL THEN
            RAISE EXCEPTION 'approval policy is revoked' USING ERRCODE = '55000';
        END IF;
        SELECT * INTO subject FROM eacp.principals
        WHERE tenant_id = NEW.tenant_id AND id = NEW.requesting_subject_id FOR SHARE;
        IF NOT FOUND OR subject.disabled_at IS NOT NULL OR subject.kind <> 'human' THEN
            RAISE EXCEPTION 'requesting subject must be an enabled human' USING ERRCODE = '42501';
        END IF;
        SELECT * INTO v FROM eacp.agent_versions
        WHERE tenant_id = NEW.tenant_id AND id = NEW.agent_version_id FOR SHARE;
        IF NOT FOUND OR v.state <> 'ACTIVE' OR v.active_allowlist_id IS NULL THEN
            RAISE EXCEPTION 'agent version is not active' USING ERRCODE = '55000';
        END IF;
        SELECT * INTO ag FROM eacp.agents
        WHERE tenant_id = NEW.tenant_id AND id = v.agent_id;
        SELECT * INTO al FROM eacp.agent_allowlists
        WHERE tenant_id = NEW.tenant_id AND id = v.active_allowlist_id;
        IF NOT FOUND OR NOT NEW.tool_id = ANY (al.tool_ids) THEN
            RAISE EXCEPTION 'tool is not in active allowlist' USING ERRCODE = '55000';
        END IF;
        SELECT * INTO tl FROM eacp.tools
        WHERE tenant_id = NEW.tenant_id AND id = NEW.tool_id FOR SHARE;
        IF NOT FOUND OR tl.active_contract_id IS NULL THEN
            RAISE EXCEPTION 'tool has no active contract' USING ERRCODE = '55000';
        END IF;
        SELECT * INTO ct FROM eacp.tool_contracts
        WHERE tenant_id = NEW.tenant_id AND id = tl.active_contract_id FOR SHARE;
        IF NOT FOUND OR ct.revoked_at IS NOT NULL
           OR ct.fingerprint IS DISTINCT FROM eacp.tool_fingerprint(NEW.tenant_id, NEW.tool_id) THEN
            RAISE EXCEPTION 'active contract is unusable' USING ERRCODE = '55000';
        END IF;
        IF NEW.state <> 'PENDING' OR NEW.not_after <= now() OR NEW.expires_at <= now()
           OR NEW.expires_at > NEW.not_after
           OR NEW.expires_at > ev.evaluated_at + make_interval(secs => ev.approval_ttl_seconds) THEN
            RAISE EXCEPTION 'invalid approval lifetime or initial state' USING ERRCODE = '23514';
        END IF;
        IF ag.owner_group_id IS NOT NULL THEN
            FOR member IN SELECT gm.principal_id FROM eacp.group_memberships gm
                WHERE gm.tenant_id = NEW.tenant_id AND gm.group_id = ag.owner_group_id
                  AND gm.removed_at IS NULL FOR SHARE
            LOOP
                members := array_append(members, member);
            END LOOP;
        END IF;
        SELECT ARRAY(SELECT DISTINCT x FROM unnest(members) x ORDER BY x)
        INTO NEW.owner_member_ids_snapshot;
        enabling := array_remove(ARRAY[b.created_by, p.activated_by, al.created_by,
                                       v.allowlist_changed_by, ct.created_by,
                                       tl.contract_changed_by]::uuid[], NULL);
        SELECT ARRAY(SELECT DISTINCT x FROM unnest(enabling) x ORDER BY x)
        INTO NEW.enabling_actor_ids;
        NEW.policy_bundle_id := ev.policy_bundle_id;
        NEW.policy_version := ev.policy_version;
        NEW.enforced_digest := ev.enforced_digest;
        NEW.required_quorum := ev.required_quorum;
        NEW.eligible_roles := ev.eligible_roles;
        NEW.risk_class := ag.risk_class;
        NEW.active_allowlist_id := al.id;
        NEW.active_contract_id := ct.id;
        NEW.owner_principal_id := ag.owner_principal_id;
        NEW.owner_group_id := ag.owner_group_id;
        NEW.created_by := CASE WHEN who->>'kind' = 'principal' THEN (who->>'id')::uuid END;
        NEW.created_at := now();
        RETURN NEW;
    END IF;

    IF (to_jsonb(NEW) - 'state' - 'expires_at') IS DISTINCT FROM
       (to_jsonb(OLD) - 'state' - 'expires_at') THEN
        RAISE EXCEPTION 'approval request identity is immutable' USING ERRCODE = '55000';
    END IF;
    IF NEW.expires_at > OLD.expires_at THEN
        RAISE EXCEPTION 'approval expiry cannot be extended' USING ERRCODE = '55000';
    END IF;
    IF NEW.state IS NOT DISTINCT FROM OLD.state THEN RETURN NEW; END IF;
    IF OLD.state NOT IN ('PENDING', 'GRANTED') THEN
        RAISE EXCEPTION 'approval request is terminal' USING ERRCODE = '55000';
    END IF;
    CASE NEW.state
        WHEN 'GRANTED' THEN
            IF OLD.state <> 'PENDING' THEN
                RAISE EXCEPTION 'only pending requests can be granted' USING ERRCODE = '55000';
            END IF;
            SELECT count(*) INTO approvals FROM eacp.approval_votes
            WHERE tenant_id = NEW.tenant_id AND request_id = NEW.id AND decision = 'APPROVE';
            IF approvals < NEW.required_quorum THEN
                RAISE EXCEPTION 'approval quorum not reached' USING ERRCODE = '55000';
            END IF;
        WHEN 'DENIED' THEN
            IF OLD.state <> 'PENDING' OR NOT EXISTS (
                SELECT 1 FROM eacp.approval_votes WHERE tenant_id = NEW.tenant_id
                AND request_id = NEW.id AND decision = 'DENY') THEN
                RAISE EXCEPTION 'deny vote required' USING ERRCODE = '55000';
            END IF;
        WHEN 'EXPIRED' THEN
            IF now() < NEW.expires_at AND now() < NEW.not_after THEN
                RAISE EXCEPTION 'request has not expired' USING ERRCODE = '55000';
            END IF;
        WHEN 'VOIDED' THEN
            -- Void only when the action is terminal, or when a dependency the
            -- approval was bound to (policy, allowlist, contract) has changed.
            SELECT * INTO act FROM eacp.actions WHERE tenant_id = NEW.tenant_id AND id = OLD.action_id;
            SELECT * INTO p FROM eacp.tenant_policy_pointer
            WHERE tenant_id = NEW.tenant_id FOR SHARE;
            SELECT * INTO v FROM eacp.agent_versions
            WHERE tenant_id = NEW.tenant_id AND id = OLD.agent_version_id FOR SHARE;
            SELECT * INTO tl FROM eacp.tools
            WHERE tenant_id = NEW.tenant_id AND id = OLD.tool_id FOR SHARE;
            IF act.state NOT IN ('SUCCEEDED', 'FAILED', 'DENIED', 'CANCELLED', 'EXPIRED')
               AND p.current_version IS NOT DISTINCT FROM OLD.policy_version
               AND v.active_allowlist_id IS NOT DISTINCT FROM OLD.active_allowlist_id
               AND tl.active_contract_id IS NOT DISTINCT FROM OLD.active_contract_id THEN
                RAISE EXCEPTION 'approval dependencies have not changed' USING ERRCODE = '55000';
            END IF;
        ELSE
            RAISE EXCEPTION 'illegal approval request transition' USING ERRCODE = '55000';
    END CASE;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.approval_grants_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    who jsonb := eacp.actor_context();
    req eacp.approval_requests%ROWTYPE;
    p eacp.tenant_policy_pointer%ROWTYPE;
    act eacp.actions%ROWTYPE;
BEGIN
    IF TG_OP = 'INSERT' THEN
        SELECT * INTO req FROM eacp.approval_requests
        WHERE tenant_id = NEW.tenant_id AND id = NEW.request_id FOR SHARE;
        IF NOT FOUND OR req.state <> 'GRANTED' OR req.expires_at <= now() THEN
            RAISE EXCEPTION 'grant requires an unexpired granted request' USING ERRCODE = '55000';
        END IF;
        SELECT * INTO p FROM eacp.tenant_policy_pointer
        WHERE tenant_id = NEW.tenant_id FOR SHARE;
        IF p.current_version IS DISTINCT FROM req.policy_version THEN
            RAISE EXCEPTION 'grant policy changed' USING ERRCODE = '55000';
        END IF;
        IF NEW.consumed_at IS NOT NULL OR NEW.consumed_by_action_id IS NOT NULL THEN
            RAISE EXCEPTION 'a grant starts unconsumed' USING ERRCODE = '55000';
        END IF;
        NEW.action_id := req.action_id;
        NEW.enforced_digest := req.enforced_digest;
        NEW.policy_bundle_id := req.policy_bundle_id;
        NEW.policy_version := req.policy_version;
        NEW.expires_at := req.expires_at;
        NEW.created_at := now();
        RETURN NEW;
    END IF;
    IF (to_jsonb(NEW) - 'expires_at' - 'consumed_at' - 'consumed_by_action_id') IS DISTINCT FROM
       (to_jsonb(OLD) - 'expires_at' - 'consumed_at' - 'consumed_by_action_id') THEN
        RAISE EXCEPTION 'grant identity is immutable' USING ERRCODE = '55000';
    END IF;
    IF NEW.expires_at > OLD.expires_at THEN
        RAISE EXCEPTION 'grant expiry cannot be extended' USING ERRCODE = '55000';
    END IF;
    IF NEW.consumed_at IS NOT DISTINCT FROM OLD.consumed_at
       AND NEW.consumed_by_action_id IS NOT DISTINCT FROM OLD.consumed_by_action_id THEN
        RETURN NEW;
    END IF;
    IF OLD.consumed_at IS NOT NULL THEN
        RAISE EXCEPTION 'grant was already consumed' USING ERRCODE = '55000';
    END IF;
    -- Consumption is part of the release boundary: agent or system only.
    SELECT * INTO act FROM eacp.actions WHERE tenant_id = NEW.tenant_id AND id = OLD.action_id;
    IF who->>'kind' = 'principal' THEN
        RAISE EXCEPTION 'grants are consumed only by the release boundary' USING ERRCODE = '42501';
    END IF;
    PERFORM eacp.assert_action_agent(who, act.agent_id);
    IF NEW.consumed_at IS NULL OR NEW.consumed_by_action_id IS DISTINCT FROM OLD.action_id
       OR OLD.expires_at <= now() OR NEW.expires_at <= now() THEN
        RAISE EXCEPTION 'grant cannot be consumed' USING ERRCODE = '55000';
    END IF;
    SELECT * INTO req FROM eacp.approval_requests
    WHERE tenant_id = NEW.tenant_id AND id = OLD.request_id FOR SHARE;
    SELECT * INTO p FROM eacp.tenant_policy_pointer
    WHERE tenant_id = NEW.tenant_id FOR SHARE;
    IF req.state <> 'GRANTED' OR req.expires_at <= now() OR req.not_after <= now()
       OR p.current_version IS DISTINCT FROM OLD.policy_version
       OR p.current_bundle_id IS DISTINCT FROM OLD.policy_bundle_id THEN
        RAISE EXCEPTION 'grant no longer matches an active approval' USING ERRCODE = '55000';
    END IF;
    PERFORM 1 FROM eacp.policy_bundles
    WHERE tenant_id = NEW.tenant_id AND id = OLD.policy_bundle_id
      AND revoked_at IS NULL FOR SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'grant policy is revoked' USING ERRCODE = '55000';
    END IF;
    -- The approvers' enabling-change SoD was judged against this allowlist
    -- and contract; a grant does not survive a change to either.
    PERFORM 1 FROM eacp.agent_versions v
    JOIN eacp.tools t ON t.tenant_id = v.tenant_id AND t.id = req.tool_id
    WHERE v.tenant_id = NEW.tenant_id AND v.id = req.agent_version_id
      AND v.active_allowlist_id = req.active_allowlist_id
      AND t.active_contract_id = req.active_contract_id
    FOR SHARE OF v, t;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'grant dependencies changed since approval' USING ERRCODE = '55000';
    END IF;
    NEW.consumed_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Invariant 2 at commit: a consumed grant is matched by its action having
-- been released (T10) in the consuming transaction.
CREATE FUNCTION eacp.approval_grants_release_check() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM 1 FROM eacp.actions
    WHERE tenant_id = NEW.tenant_id AND id = NEW.consumed_by_action_id
      AND state = 'QUEUED' AND released_at = NEW.consumed_at;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'a grant was consumed without releasing its action' USING ERRCODE = '55000';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.audit_governance_change() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    who     jsonb := eacp.actor_context();
    row_new jsonb := to_jsonb(NEW) - 'content' - 'input_digest' - 'enforced_digest'
                     - 'enforced_payload' - 'owner_member_ids_snapshot' - 'snapshot_hash';
    row_old jsonb;
    changed jsonb;
    why text;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        row_old := to_jsonb(OLD) - 'content' - 'input_digest' - 'enforced_digest'
                   - 'enforced_payload' - 'owner_member_ids_snapshot' - 'snapshot_hash';
        SELECT COALESCE(jsonb_object_agg(e.key, jsonb_build_object('from', row_old -> e.key, 'to', e.value)), '{}')
        INTO changed FROM jsonb_each(row_new) AS e
        WHERE row_old -> e.key IS DISTINCT FROM e.value;
        IF changed = '{}'::jsonb THEN RETURN NULL; END IF;
    ELSE
        changed := row_new;
    END IF;
    IF who ? 'component' THEN
        changed := changed || jsonb_build_object('component', who->>'component');
    END IF;
    why := COALESCE(row_new ->> 'reason', row_new ->> 'activation_reason',
                    row_new ->> 'revoke_reason', TG_TABLE_NAME || ' ' || lower(TG_OP));
    INSERT INTO eacp.audit_events (tenant_id, payload)
    VALUES (NEW.tenant_id, convert_to(jsonb_build_object(
        'v', 1,
        'actor', who - 'component',
        'action', TG_TABLE_NAME || '.' || lower(TG_OP),
        'subject', jsonb_build_object('type', TG_TABLE_NAME, 'id', NEW.id),
        'reason', why,
        'data', changed)::text, 'UTF8'));
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- ------------------------------------------------- approval-driven cascades

-- +goose StatementBegin
-- T6: issuing the grant authorizes the pending action in the vote's
-- transaction.
CREATE FUNCTION eacp.approval_grants_authorize() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    UPDATE eacp.actions SET state = 'AUTHORIZED', state_reason = 'approval granted'
    WHERE tenant_id = NEW.tenant_id AND id = NEW.action_id
      AND state = 'PENDING_APPROVAL' AND approval_request_id = NEW.request_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'granted approval does not match a pending action' USING ERRCODE = '55000';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- T7 and T8/T13: a denied or expired request decides its action.
CREATE FUNCTION eacp.approval_requests_follow() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.state = 'DENIED' THEN
        UPDATE eacp.actions SET state = 'DENIED', state_reason = 'approval denied'
        WHERE tenant_id = NEW.tenant_id AND id = NEW.action_id
          AND state = 'PENDING_APPROVAL' AND approval_request_id = NEW.id;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'denied approval does not match a pending action' USING ERRCODE = '55000';
        END IF;
    ELSIF NEW.state = 'EXPIRED' THEN
        UPDATE eacp.actions SET state = 'EXPIRED', state_reason = 'approval expired'
        WHERE tenant_id = NEW.tenant_id AND id = NEW.action_id
          AND state IN ('PENDING_APPROVAL', 'AUTHORIZED') AND approval_request_id = NEW.id;
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Votes lock the action before the request so that votes, cancellation,
-- expiry and release take row locks in the same order.
CREATE FUNCTION eacp.approval_votes_lock_action() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM 1 FROM eacp.actions a
    JOIN eacp.approval_requests r ON r.tenant_id = a.tenant_id AND r.action_id = a.id
    WHERE r.tenant_id = NEW.tenant_id AND r.id = NEW.request_id
    FOR NO KEY UPDATE OF a;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER a_lock_action BEFORE INSERT ON eacp.approval_votes
    FOR EACH ROW EXECUTE FUNCTION eacp.approval_votes_lock_action();
CREATE TRIGGER action_authorize AFTER INSERT ON eacp.approval_grants
    FOR EACH ROW EXECUTE FUNCTION eacp.approval_grants_authorize();
CREATE TRIGGER action_follow AFTER UPDATE ON eacp.approval_requests
    FOR EACH ROW WHEN (OLD.state IS DISTINCT FROM NEW.state)
    EXECUTE FUNCTION eacp.approval_requests_follow();
CREATE CONSTRAINT TRIGGER release_check AFTER UPDATE ON eacp.approval_grants
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW WHEN (OLD.consumed_at IS NULL AND NEW.consumed_at IS NOT NULL)
    EXECUTE FUNCTION eacp.approval_grants_release_check();

-- ---------------------------------------------------- RLS and privileges

-- +goose StatementBegin
DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['actions', 'outbox_events'] LOOP
        EXECUTE format('ALTER TABLE eacp.%I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE eacp.%I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('CREATE POLICY tenant_isolation ON eacp.%I USING (tenant_id = eacp.current_tenant_id())', t);
        EXECUTE format('REVOKE UPDATE ON eacp.%I FROM eacp_app', t);
    END LOOP;
END
$$;
-- +goose StatementEnd

-- Transitions change only these columns; triggers derive the rest.
GRANT UPDATE (state, state_reason, decision_evidence_id, enforced_payload,
              approval_request_id, connector_contract_id) ON eacp.actions TO eacp_app;

-- Cross-tenant scans. The schema owner (the role running this migration)
-- may read every action; eacp_app keeps strict tenant RLS. Only these narrow
-- SECURITY DEFINER functions expose the owner's view, and they return a
-- count or tenant ids, never action data.
CREATE POLICY owner_scan ON eacp.actions FOR SELECT TO CURRENT_USER USING (true);

CREATE FUNCTION eacp.global_queued_count() RETURNS bigint
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$ SELECT count(*) FROM eacp.actions WHERE state = 'QUEUED' $$;

CREATE FUNCTION eacp.tenants_with_open_actions(after_tenant uuid, lim integer) RETURNS TABLE (tenant_id uuid)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$ SELECT DISTINCT a.tenant_id FROM eacp.actions a
          WHERE a.state IN ('RECEIVED', 'PENDING_APPROVAL', 'AUTHORIZED', 'QUEUED')
            AND (after_tenant IS NULL OR a.tenant_id > after_tenant)
          ORDER BY a.tenant_id LIMIT least(greatest(lim, 1), 1000) $$;

REVOKE ALL ON FUNCTION eacp.global_queued_count() FROM PUBLIC;
REVOKE ALL ON FUNCTION eacp.tenants_with_open_actions(uuid, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.global_queued_count() TO eacp_app;
GRANT EXECUTE ON FUNCTION eacp.tenants_with_open_actions(uuid, integer) TO eacp_app;

-- +goose Down
DROP TRIGGER release_check ON eacp.approval_grants;
DROP TRIGGER action_follow ON eacp.approval_requests;
DROP TRIGGER action_authorize ON eacp.approval_grants;
DROP TRIGGER a_lock_action ON eacp.approval_votes;
DROP FUNCTION eacp.approval_votes_lock_action();
DROP FUNCTION eacp.approval_requests_follow();
DROP FUNCTION eacp.approval_grants_authorize();
DROP FUNCTION eacp.approval_grants_release_check();
DROP FUNCTION eacp.tenants_with_open_actions(uuid, integer);
DROP FUNCTION eacp.global_queued_count();

-- Restore the Phase 3 function bodies (verbatim from 00004).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.decision_evidence_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    v integer;
BEGIN
    PERFORM eacp.actor();
    SELECT version INTO v FROM eacp.policy_bundles
    WHERE tenant_id = NEW.tenant_id AND id = NEW.policy_bundle_id FOR SHARE;
    IF NOT FOUND OR v <> NEW.policy_version THEN
        RAISE EXCEPTION 'evidence policy identity mismatch' USING ERRCODE = '23514';
    END IF;
    IF NEW.evaluated_at > now() + interval '1 minute' THEN
        RAISE EXCEPTION 'evidence evaluation time is in the future' USING ERRCODE = '23514';
    END IF;
    NEW.recorded_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.approval_requests_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.actor();
    ev eacp.decision_evidence%ROWTYPE;
    p eacp.tenant_policy_pointer%ROWTYPE;
    b eacp.policy_bundles%ROWTYPE;
    v eacp.agent_versions%ROWTYPE;
    ag eacp.agents%ROWTYPE;
    al eacp.agent_allowlists%ROWTYPE;
    tl eacp.tools%ROWTYPE;
    ct eacp.tool_contracts%ROWTYPE;
    subject eacp.principals%ROWTYPE;
    member uuid;
    members uuid[] := '{}';
    enabling uuid[];
    approvals integer;
BEGIN
    IF TG_OP = 'INSERT' THEN
        -- Evidence is immutable and the app role has no UPDATE grant, so a
        -- row lock is unnecessary (and would require UPDATE privilege).
        SELECT * INTO ev FROM eacp.decision_evidence
        WHERE tenant_id = NEW.tenant_id AND id = NEW.decision_evidence_id;
        IF NOT FOUND OR ev.verdict <> 'escalate' OR ev.action_id <> NEW.action_id THEN
            RAISE EXCEPTION 'request requires matching escalation evidence' USING ERRCODE = '55000';
        END IF;
        SELECT * INTO p FROM eacp.tenant_policy_pointer
        WHERE tenant_id = NEW.tenant_id FOR SHARE;
        IF NOT FOUND OR p.current_version IS DISTINCT FROM ev.policy_version
           OR p.current_bundle_id IS DISTINCT FROM ev.policy_bundle_id THEN
            RAISE EXCEPTION 'approval policy is no longer active' USING ERRCODE = '55000';
        END IF;
        SELECT * INTO b FROM eacp.policy_bundles
        WHERE tenant_id = NEW.tenant_id AND id = ev.policy_bundle_id FOR SHARE;
        IF NOT FOUND OR b.revoked_at IS NOT NULL THEN
            RAISE EXCEPTION 'approval policy is revoked' USING ERRCODE = '55000';
        END IF;
        SELECT * INTO subject FROM eacp.principals
        WHERE tenant_id = NEW.tenant_id AND id = NEW.requesting_subject_id FOR SHARE;
        IF NOT FOUND OR subject.disabled_at IS NOT NULL OR subject.kind <> 'human' THEN
            RAISE EXCEPTION 'requesting subject must be an enabled human' USING ERRCODE = '42501';
        END IF;
        SELECT * INTO v FROM eacp.agent_versions
        WHERE tenant_id = NEW.tenant_id AND id = NEW.agent_version_id FOR SHARE;
        IF NOT FOUND OR v.state <> 'ACTIVE' OR v.active_allowlist_id IS NULL THEN
            RAISE EXCEPTION 'agent version is not active' USING ERRCODE = '55000';
        END IF;
        SELECT * INTO ag FROM eacp.agents
        WHERE tenant_id = NEW.tenant_id AND id = v.agent_id;
        SELECT * INTO al FROM eacp.agent_allowlists
        WHERE tenant_id = NEW.tenant_id AND id = v.active_allowlist_id;
        IF NOT FOUND OR NOT NEW.tool_id = ANY (al.tool_ids) THEN
            RAISE EXCEPTION 'tool is not in active allowlist' USING ERRCODE = '55000';
        END IF;
        SELECT * INTO tl FROM eacp.tools
        WHERE tenant_id = NEW.tenant_id AND id = NEW.tool_id FOR SHARE;
        IF NOT FOUND OR tl.active_contract_id IS NULL THEN
            RAISE EXCEPTION 'tool has no active contract' USING ERRCODE = '55000';
        END IF;
        SELECT * INTO ct FROM eacp.tool_contracts
        WHERE tenant_id = NEW.tenant_id AND id = tl.active_contract_id FOR SHARE;
        IF NOT FOUND OR ct.revoked_at IS NOT NULL
           OR ct.fingerprint IS DISTINCT FROM eacp.tool_fingerprint(NEW.tenant_id, NEW.tool_id) THEN
            RAISE EXCEPTION 'active contract is unusable' USING ERRCODE = '55000';
        END IF;
        IF NEW.state <> 'PENDING' OR NEW.not_after <= now() OR NEW.expires_at <= now()
           OR NEW.expires_at > NEW.not_after
           OR NEW.expires_at > ev.evaluated_at + make_interval(secs => ev.approval_ttl_seconds) THEN
            RAISE EXCEPTION 'invalid approval lifetime or initial state' USING ERRCODE = '23514';
        END IF;
        IF ag.owner_group_id IS NOT NULL THEN
            FOR member IN SELECT gm.principal_id FROM eacp.group_memberships gm
                WHERE gm.tenant_id = NEW.tenant_id AND gm.group_id = ag.owner_group_id
                  AND gm.removed_at IS NULL FOR SHARE
            LOOP
                members := array_append(members, member);
            END LOOP;
        END IF;
        SELECT ARRAY(SELECT DISTINCT x FROM unnest(members) x ORDER BY x)
        INTO NEW.owner_member_ids_snapshot;
        enabling := array_remove(ARRAY[b.created_by, p.activated_by, al.created_by,
                                       v.allowlist_changed_by, ct.created_by,
                                       tl.contract_changed_by]::uuid[], NULL);
        SELECT ARRAY(SELECT DISTINCT x FROM unnest(enabling) x ORDER BY x)
        INTO NEW.enabling_actor_ids;
        NEW.policy_bundle_id := ev.policy_bundle_id;
        NEW.policy_version := ev.policy_version;
        NEW.enforced_digest := ev.enforced_digest;
        NEW.required_quorum := ev.required_quorum;
        NEW.eligible_roles := ev.eligible_roles;
        NEW.risk_class := ag.risk_class;
        NEW.active_allowlist_id := al.id;
        NEW.active_contract_id := ct.id;
        NEW.owner_principal_id := ag.owner_principal_id;
        NEW.owner_group_id := ag.owner_group_id;
        NEW.created_by := a;
        NEW.created_at := now();
        RETURN NEW;
    END IF;

    IF (to_jsonb(NEW) - 'state' - 'expires_at') IS DISTINCT FROM
       (to_jsonb(OLD) - 'state' - 'expires_at') THEN
        RAISE EXCEPTION 'approval request identity is immutable' USING ERRCODE = '55000';
    END IF;
    IF NEW.expires_at > OLD.expires_at THEN
        RAISE EXCEPTION 'approval expiry cannot be extended' USING ERRCODE = '55000';
    END IF;
    IF NEW.state IS NOT DISTINCT FROM OLD.state THEN RETURN NEW; END IF;
    IF OLD.state NOT IN ('PENDING', 'GRANTED') THEN
        RAISE EXCEPTION 'approval request is terminal' USING ERRCODE = '55000';
    END IF;
    CASE NEW.state
        WHEN 'GRANTED' THEN
            IF OLD.state <> 'PENDING' THEN
                RAISE EXCEPTION 'only pending requests can be granted' USING ERRCODE = '55000';
            END IF;
            SELECT count(*) INTO approvals FROM eacp.approval_votes
            WHERE tenant_id = NEW.tenant_id AND request_id = NEW.id AND decision = 'APPROVE';
            IF approvals < NEW.required_quorum THEN
                RAISE EXCEPTION 'approval quorum not reached' USING ERRCODE = '55000';
            END IF;
        WHEN 'DENIED' THEN
            IF OLD.state <> 'PENDING' OR NOT EXISTS (
                SELECT 1 FROM eacp.approval_votes WHERE tenant_id = NEW.tenant_id
                AND request_id = NEW.id AND decision = 'DENY') THEN
                RAISE EXCEPTION 'deny vote required' USING ERRCODE = '55000';
            END IF;
        WHEN 'EXPIRED' THEN
            IF now() < NEW.expires_at AND now() < NEW.not_after THEN
                RAISE EXCEPTION 'request has not expired' USING ERRCODE = '55000';
            END IF;
        WHEN 'VOIDED' THEN
            SELECT * INTO p FROM eacp.tenant_policy_pointer
            WHERE tenant_id = NEW.tenant_id FOR SHARE;
            IF p.current_version IS NOT DISTINCT FROM OLD.policy_version THEN
                RAISE EXCEPTION 'current policy has not changed' USING ERRCODE = '55000';
            END IF;
        ELSE
            RAISE EXCEPTION 'illegal approval request transition' USING ERRCODE = '55000';
    END CASE;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.approval_grants_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.actor();
    req eacp.approval_requests%ROWTYPE;
    p eacp.tenant_policy_pointer%ROWTYPE;
BEGIN
    IF TG_OP = 'INSERT' THEN
        SELECT * INTO req FROM eacp.approval_requests
        WHERE tenant_id = NEW.tenant_id AND id = NEW.request_id FOR SHARE;
        IF NOT FOUND OR req.state <> 'GRANTED' OR req.expires_at <= now() THEN
            RAISE EXCEPTION 'grant requires an unexpired granted request' USING ERRCODE = '55000';
        END IF;
        SELECT * INTO p FROM eacp.tenant_policy_pointer
        WHERE tenant_id = NEW.tenant_id FOR SHARE;
        IF p.current_version IS DISTINCT FROM req.policy_version THEN
            RAISE EXCEPTION 'grant policy changed' USING ERRCODE = '55000';
        END IF;
        IF NEW.consumed_at IS NOT NULL OR NEW.consumed_by_action_id IS NOT NULL THEN
            RAISE EXCEPTION 'a grant starts unconsumed' USING ERRCODE = '55000';
        END IF;
        NEW.action_id := req.action_id;
        NEW.enforced_digest := req.enforced_digest;
        NEW.policy_bundle_id := req.policy_bundle_id;
        NEW.policy_version := req.policy_version;
        NEW.expires_at := req.expires_at;
        NEW.created_at := now();
        RETURN NEW;
    END IF;
    IF (to_jsonb(NEW) - 'expires_at' - 'consumed_at' - 'consumed_by_action_id') IS DISTINCT FROM
       (to_jsonb(OLD) - 'expires_at' - 'consumed_at' - 'consumed_by_action_id') THEN
        RAISE EXCEPTION 'grant identity is immutable' USING ERRCODE = '55000';
    END IF;
    IF NEW.expires_at > OLD.expires_at THEN
        RAISE EXCEPTION 'grant expiry cannot be extended' USING ERRCODE = '55000';
    END IF;
    IF NEW.consumed_at IS NOT DISTINCT FROM OLD.consumed_at
       AND NEW.consumed_by_action_id IS NOT DISTINCT FROM OLD.consumed_by_action_id THEN
        RETURN NEW;
    END IF;
    IF OLD.consumed_at IS NOT NULL THEN
        RAISE EXCEPTION 'grant was already consumed' USING ERRCODE = '55000';
    END IF;
    IF NEW.consumed_at IS NULL OR NEW.consumed_by_action_id IS DISTINCT FROM OLD.action_id
       OR OLD.expires_at <= now() OR NEW.expires_at <= now() THEN
        RAISE EXCEPTION 'grant cannot be consumed' USING ERRCODE = '55000';
    END IF;
    SELECT * INTO req FROM eacp.approval_requests
    WHERE tenant_id = NEW.tenant_id AND id = OLD.request_id FOR SHARE;
    SELECT * INTO p FROM eacp.tenant_policy_pointer
    WHERE tenant_id = NEW.tenant_id FOR SHARE;
    IF req.state <> 'GRANTED' OR req.expires_at <= now() OR req.not_after <= now()
       OR p.current_version IS DISTINCT FROM OLD.policy_version
       OR p.current_bundle_id IS DISTINCT FROM OLD.policy_bundle_id THEN
        RAISE EXCEPTION 'grant no longer matches an active approval' USING ERRCODE = '55000';
    END IF;
    PERFORM 1 FROM eacp.policy_bundles
    WHERE tenant_id = NEW.tenant_id AND id = OLD.policy_bundle_id
      AND revoked_at IS NULL FOR SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'grant policy is revoked' USING ERRCODE = '55000';
    END IF;
    NEW.consumed_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Governance rows contain policy source and digest bytes that must not be
-- copied into the audit journal. The event retains row identity, actor,
-- state, and metadata needed to reconstruct the chain of decisions.
CREATE OR REPLACE FUNCTION eacp.audit_governance_change() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a       uuid := eacp.current_actor_id();
    row_new jsonb := to_jsonb(NEW) - 'content' - 'input_digest' - 'enforced_digest'
                     - 'enforced_payload' - 'owner_member_ids_snapshot' - 'snapshot_hash';
    row_old jsonb;
    changed jsonb;
    why text;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        row_old := to_jsonb(OLD) - 'content' - 'input_digest' - 'enforced_digest'
                   - 'enforced_payload' - 'owner_member_ids_snapshot' - 'snapshot_hash';
        SELECT COALESCE(jsonb_object_agg(e.key, jsonb_build_object('from', row_old -> e.key, 'to', e.value)), '{}')
        INTO changed FROM jsonb_each(row_new) AS e
        WHERE row_old -> e.key IS DISTINCT FROM e.value;
        IF changed = '{}'::jsonb THEN RETURN NULL; END IF;
    ELSE
        changed := row_new;
    END IF;
    why := COALESCE(row_new ->> 'reason', row_new ->> 'activation_reason',
                    row_new ->> 'revoke_reason', TG_TABLE_NAME || ' ' || lower(TG_OP));
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

DROP INDEX eacp.approval_requests_one_live;
ALTER TABLE eacp.approval_grants DROP CONSTRAINT approval_grants_tenant_id_action_id_fkey;
ALTER TABLE eacp.approval_requests DROP CONSTRAINT approval_requests_tenant_id_action_id_fkey;
ALTER TABLE eacp.decision_evidence DROP CONSTRAINT decision_evidence_tenant_id_action_id_fkey;
DROP TABLE eacp.outbox_events;
DROP FUNCTION eacp.assert_governance_evidence(eacp.decision_evidence, eacp.actions);
DROP TABLE eacp.actions;
DROP FUNCTION eacp.audit_action_change();
DROP FUNCTION eacp.outbox_events_guard();
DROP FUNCTION eacp.actions_after();
DROP FUNCTION eacp.actions_guard();
DROP FUNCTION eacp.action_capability_denial(uuid, uuid);
DROP FUNCTION eacp.assert_action_agent(jsonb, uuid);
DROP FUNCTION eacp.actor_context();
DROP FUNCTION eacp.current_traceparent();
DROP FUNCTION eacp.current_system_actor();
DROP FUNCTION eacp.current_agent_version_id();
