-- Slice A Phase 7: UNKNOWN_OUTCOME, reconciliation and human resolution
-- (MASTER_PLAN §20, §81; ADR-004 T28-T37, T29a).
--
-- A reconciler in the execution worker claims an UNKNOWN_OUTCOME action
-- under a fenced reconciler lease (T28, lease_generation + 1), looks its
-- operation key up with the worker-held credential and records what it saw
-- (eacp.reconciliation_checks) in the same transaction as the move it
-- drives: positive evidence succeeds (T30); only AUTHORITATIVE negative
-- evidence permits a retry or FAILED (T31, T32); anything else is still
-- unknown (T33) or goes to a human (T34). The sweeper routes contracts
-- without usable proof (T29), retries unknown reads (T29a), returns lapsed
-- reconciler leases (T33) and applies the reconciliation time limit (T34).
-- Operators resolve NEEDS_HUMAN_RESOLUTION through eacp.action_resolutions
-- (T35-T37); a retry needs a second, distinct operator.

-- +goose Up

-- ---------------------------------------------------- actor: the reconciler

-- +goose StatementBegin
-- eacp.actor_context() returns the transaction's single actor:
--   {"kind":"principal","id":...}  an enabled principal of the tenant
--   {"kind":"agent","id":...}      a non-terminal agent version of the tenant
--   {"kind":"system","id":<nil>,"component":"sweeper"|"owner"}
--   {"kind":"system","id":<nil>,"component":"worker"|"reconciler","worker":<id>}
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
        IF s IN ('worker', 'reconciler') THEN
            w := eacp.current_worker_id();
            IF w IS NULL THEN
                RAISE EXCEPTION 'the % actor needs a valid app.worker_id', s USING ERRCODE = '42501';
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

-- +goose StatementBegin
-- eacp.assert_lease_holder fails unless the actor holds action a's lease at
-- generation a.lease_generation: the reconciler while RECONCILING, the
-- execution worker otherwise.
CREATE OR REPLACE FUNCTION eacp.assert_lease_holder(who jsonb, a eacp.actions) RETURNS void
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF who->>'component' IS DISTINCT FROM (CASE WHEN a.state = 'RECONCILING' THEN 'reconciler' ELSE 'worker' END)
       OR who->>'worker' IS DISTINCT FROM a.worker_id
       OR eacp.current_lease_generation() IS DISTINCT FROM a.lease_generation THEN
        RAISE EXCEPTION 'only the lease holder at generation % may do this', a.lease_generation
            USING ERRCODE = '42501';
    END IF;
END
$$;
-- +goose StatementEnd

-- ------------------------------------------------------------------ tables

-- The reconciler lease reuses the execution lease columns (worker_id,
-- leased_until, lease_generation): generations stay monotonic across
-- workers and reconcilers, so every earlier holder is fenced out.
ALTER TABLE eacp.actions
    ADD COLUMN reconcile_attempts integer     NOT NULL DEFAULT 0 CHECK (reconcile_attempts >= 0),
    ADD COLUMN next_reconcile_at  timestamptz,
    ADD COLUMN outcome_unknown_at timestamptz,
    DROP CONSTRAINT lease_iff_leased,
    DROP CONSTRAINT leased_has_worker,
    ADD CONSTRAINT lease_iff_leased CHECK ((state IN ('LEASED', 'EXECUTING', 'RECONCILING')) = (leased_until IS NOT NULL)),
    ADD CONSTRAINT leased_has_worker CHECK (state NOT IN ('LEASED', 'EXECUTING', 'RECONCILING') OR worker_id IS NOT NULL),
    -- Actions already UNKNOWN_OUTCOME before this migration have no
    -- schedule and are simply due.
    ADD CONSTRAINT reconcile_only_unknown CHECK (next_reconcile_at IS NULL OR state = 'UNKNOWN_OUTCOME');

CREATE INDEX actions_reconcile ON eacp.actions (tenant_id, next_reconcile_at) WHERE state = 'UNKNOWN_OUTCOME';

-- What a reconciler saw, recorded in the transaction of the move it drives
-- (one per reconciler lease generation). Only the certified classification
-- and the external reference are stored, never a response body.
CREATE TABLE eacp.reconciliation_checks (
    tenant_id                  uuid        NOT NULL,
    action_id                  uuid        NOT NULL,
    lease_generation           bigint      NOT NULL CHECK (lease_generation >= 1),
    reconciler_id              text        NOT NULL,
    checked_at                 timestamptz NOT NULL,
    result                     text        NOT NULL CHECK (result IN ('found', 'absent', 'unknown', 'conflict')),
    external_reference         text        CHECK (btrim(external_reference) <> '' AND length(external_reference) <= 512),
    proof_standard             text        NOT NULL,
    connector_contract_id      uuid        NOT NULL,
    connector_contract_version integer     NOT NULL,
    PRIMARY KEY (tenant_id, action_id, lease_generation),
    CHECK ((result = 'found') = (external_reference IS NOT NULL)),
    FOREIGN KEY (tenant_id, action_id) REFERENCES eacp.actions (tenant_id, id),
    FOREIGN KEY (tenant_id, connector_contract_id) REFERENCES eacp.tool_contracts (tenant_id, id)
);

-- Operator resolutions of NEEDS_HUMAN_RESOLUTION (§20.3, §57). SUCCEEDED and
-- FAILED apply at once; a retry is PROPOSED by one operator and APPLIED by
-- another. Applying a resolution moves the action in the same transaction.
CREATE TABLE eacp.action_resolutions (
    tenant_id          uuid        NOT NULL,
    id                 uuid        NOT NULL DEFAULT gen_random_uuid(),
    action_id          uuid        NOT NULL,
    outcome            text        NOT NULL CHECK (outcome IN ('succeeded', 'failed', 'retry')),
    state              text        NOT NULL DEFAULT 'PROPOSED'
                                   CHECK (state IN ('PROPOSED', 'APPLIED', 'WITHDRAWN', 'VOIDED')),
    reason             text        NOT NULL CHECK (btrim(reason) <> '' AND length(reason) <= 1024),
    evidence           text        CHECK (btrim(evidence) <> '' AND length(evidence) <= 4096),
    external_reference text        CHECK (btrim(external_reference) <> '' AND length(external_reference) <= 512),
    proposed_by        uuid        NOT NULL,
    proposed_at        timestamptz NOT NULL,
    decided_by         uuid,
    decided_at         timestamptz,
    decision_reason    text        CHECK (length(decision_reason) <= 1024),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, action_id) REFERENCES eacp.actions (tenant_id, id),
    FOREIGN KEY (tenant_id, proposed_by) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, decided_by) REFERENCES eacp.principals (tenant_id, id),
    CHECK (outcome <> 'succeeded' OR (external_reference IS NOT NULL AND evidence IS NOT NULL)),
    CHECK (outcome <> 'failed' OR (evidence IS NOT NULL AND external_reference IS NULL)),
    CHECK (outcome <> 'retry' OR external_reference IS NULL),
    CHECK ((state = 'PROPOSED') = (decided_at IS NULL)),
    CHECK (state IN ('PROPOSED', 'VOIDED') OR decided_by IS NOT NULL)
);

CREATE UNIQUE INDEX action_resolutions_one_proposal ON eacp.action_resolutions (tenant_id, action_id)
    WHERE state = 'PROPOSED';

-- ------------------------------------------------------------- helpers

-- When every attempt's call has settled: its deadline plus one more call
-- budget (at least 1s), so a request still in flight cannot race a lookup
-- whose absence is taken as proof. NULL without attempts.
CREATE FUNCTION eacp.outcome_settled_at(a eacp.actions) RETURNS timestamptz
    LANGUAGE sql STABLE
    AS $$ SELECT max(call_deadline) + greatest(eacp.call_timeout(a.connector_contract_id), interval '1 second')
          FROM eacp.action_attempts WHERE tenant_id = a.tenant_id AND action_id = a.id $$;

-- +goose StatementBegin
-- eacp.assert_resolver fails unless principal p may resolve action a: an
-- enabled human operator who is neither its subject, nor the agent's owner,
-- nor a current member of the owner group (separation of duties, §20.3).
CREATE FUNCTION eacp.assert_resolver(p uuid, a eacp.actions) RETURNS void
    LANGUAGE plpgsql
    AS $$
DECLARE
    ag eacp.agents%ROWTYPE;
BEGIN
    IF p IS NULL THEN
        RAISE EXCEPTION 'only operators resolve actions' USING ERRCODE = '42501';
    END IF;
    PERFORM eacp.assert_role(p, 'operator');
    PERFORM 1 FROM eacp.principals
    WHERE tenant_id = a.tenant_id AND id = p AND kind = 'human' AND disabled_at IS NULL FOR SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'only an enabled human operator resolves actions' USING ERRCODE = '42501';
    END IF;
    -- Agents are immutable (migration 00003): no lock needed.
    SELECT * INTO ag FROM eacp.agents WHERE tenant_id = a.tenant_id AND id = a.agent_id;
    IF p = a.subject_principal_id OR p = ag.owner_principal_id THEN
        RAISE EXCEPTION 'the action''s subject or agent owner cannot resolve it' USING ERRCODE = '42501';
    END IF;
    IF ag.owner_group_id IS NOT NULL THEN
        PERFORM 1 FROM eacp.group_memberships gm
        WHERE gm.tenant_id = a.tenant_id AND gm.group_id = ag.owner_group_id
          AND gm.principal_id = p AND gm.removed_at IS NULL FOR SHARE;
        IF FOUND THEN
            RAISE EXCEPTION 'a member of the owner group cannot resolve the action' USING ERRCODE = '42501';
        END IF;
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- eacp.assert_retry_possible fails unless action a may be dispatched again
-- under its pinned contract: attempts remain, no cancel was requested and
-- not_after has not passed. A retry never extends either.
CREATE FUNCTION eacp.assert_retry_possible(a eacp.actions) RETURNS void
    LANGUAGE plpgsql
    AS $$
DECLARE
    ct eacp.tool_contracts%ROWTYPE;
BEGIN
    SELECT * INTO ct FROM eacp.tool_contracts
    WHERE tenant_id = a.tenant_id AND id = a.connector_contract_id FOR SHARE;
    IF a.cancel_requested_at IS NOT NULL OR a.not_after <= now() OR a.attempt_count >= ct.max_attempts THEN
        RAISE EXCEPTION 'no retry is possible (cancel requested, expired or attempts exhausted); resolve it as failed'
            USING ERRCODE = '55000';
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
    chk     eacp.reconciliation_checks%ROWTYPE;
    res     eacp.action_resolutions%ROWTYPE;
    retry   boolean;
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
        IF NEW.lease_generation <> 0 OR NEW.attempt_count <> 0 OR NEW.reconcile_attempts <> 0
           OR num_nonnulls(NEW.worker_id, NEW.leased_until, NEW.heartbeat_at, NEW.dispatch_intent_at,
               NEW.next_attempt_at, NEW.external_reference, NEW.cancel_requested_at, NEW.cancel_reason,
               NEW.next_reconcile_at, NEW.outcome_unknown_at) > 0 THEN
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

    -- The ADR-004 transition table. Anything else is forbidden.
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
        WHEN OLD.state = 'UNKNOWN_OUTCOME'  AND NEW.state = 'RECONCILING'      THEN 'T28'
        WHEN OLD.state = 'UNKNOWN_OUTCOME'  AND NEW.state = 'NEEDS_HUMAN_RESOLUTION' THEN 'T29'  -- or T34
        WHEN OLD.state = 'UNKNOWN_OUTCOME'  AND NEW.state = 'RETRY_WAIT'       THEN 'T29a'
        WHEN OLD.state = 'RECONCILING'      AND NEW.state = 'SUCCEEDED'        THEN 'T30'
        WHEN OLD.state = 'RECONCILING'      AND NEW.state = 'RETRY_WAIT'       THEN 'T31'
        WHEN OLD.state = 'RECONCILING'      AND NEW.state = 'FAILED'           THEN 'T32'
        WHEN OLD.state = 'RECONCILING'      AND NEW.state = 'UNKNOWN_OUTCOME'  THEN 'T33'
        WHEN OLD.state = 'RECONCILING'      AND NEW.state = 'NEEDS_HUMAN_RESOLUTION' THEN 'T34'
        WHEN OLD.state = 'NEEDS_HUMAN_RESOLUTION' AND NEW.state = 'SUCCEEDED'  THEN 'T35'
        WHEN OLD.state = 'NEEDS_HUMAN_RESOLUTION' AND NEW.state = 'FAILED'     THEN 'T36'
        WHEN OLD.state = 'NEEDS_HUMAN_RESOLUTION' AND NEW.state = 'RETRY_WAIT' THEN 'T37'
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
    IF OLD.state IN ('UNKNOWN_OUTCOME', 'RECONCILING', 'NEEDS_HUMAN_RESOLUTION') THEN
        SELECT * INTO ct FROM eacp.tool_contracts
        WHERE tenant_id = NEW.tenant_id AND id = NEW.connector_contract_id FOR SHARE;
        -- Without a usable lookup, or once the contract is revoked, an unknown
        -- outcome goes to a human (T29); with one, only exhaustion does (T34).
        IF move = 'T29' AND ct.reconciliation_lookup = 'by_operation_key' AND ct.proof_standard <> 'none'
           AND ct.revoked_at IS NULL THEN
            move := 'T34';
        END IF;
    END IF;
    IF OLD.state = 'RECONCILING' THEN
        -- The evidence this reconciler recorded in this transaction.
        SELECT * INTO chk FROM eacp.reconciliation_checks
        WHERE tenant_id = NEW.tenant_id AND action_id = NEW.id AND lease_generation = OLD.lease_generation
          AND checked_at = now();
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
    ELSIF move NOT IN ('T6', 'T7', 'T8', 'T35', 'T36', 'T37') AND akind = 'principal' THEN
        -- Governance, release, execution, reconciliation and expiry are agent
        -- or system work; approval outcomes (T6-T8) follow the approval rows
        -- and resolutions (T35-T37) the operator's resolution rows.
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
    IF comp = 'reconciler' AND move NOT IN ('T28', 'T30', 'T31', 'T32', 'T33', 'T34') THEN
        RAISE EXCEPTION 'the reconciler performs only reconciliation transitions' USING ERRCODE = '42501';
    END IF;
    IF (move IN ('T28', 'T30', 'T31', 'T32') OR (move = 'T34' AND OLD.state = 'RECONCILING'))
       AND comp IS DISTINCT FROM 'reconciler' THEN
        RAISE EXCEPTION 'only a reconciler performs % from %', move, OLD.state USING ERRCODE = '42501';
    END IF;
    IF (move IN ('T29', 'T29a') OR (move = 'T34' AND OLD.state = 'UNKNOWN_OUTCOME'))
       AND comp IS DISTINCT FROM 'sweeper' THEN
        RAISE EXCEPTION 'only the sweeper performs % from %', move, OLD.state USING ERRCODE = '42501';
    END IF;
    IF move = 'T33' AND COALESCE(comp, '') NOT IN ('reconciler', 'sweeper') THEN
        RAISE EXCEPTION 'only the reconciler or the sweeper performs T33' USING ERRCODE = '42501';
    END IF;
    IF move IN ('T35', 'T36', 'T37') AND akind <> 'principal' THEN
        RAISE EXCEPTION 'only an operator resolves %', move USING ERRCODE = '42501';
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
        WHEN move = 'T28' THEN ARRAY['lease_generation', 'worker_id', 'leased_until', 'heartbeat_at']
        WHEN move IN ('T30', 'T32', 'T34') THEN ARRAY['leased_until', 'heartbeat_at']
        WHEN move = 'T31' THEN ARRAY['leased_until', 'heartbeat_at', 'next_attempt_at']
        WHEN move = 'T33' THEN ARRAY['leased_until', 'heartbeat_at', 'reconcile_attempts', 'next_reconcile_at']
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
    IF OLD.state IN ('LEASED', 'EXECUTING', 'RECONCILING') AND move <> 'T16' THEN
        -- Leaving the lease: the lease ends with the transition.
        NEW.leased_until := NULL;
        NEW.heartbeat_at := NULL;
    END IF;
    IF OLD.state = 'EXECUTING' AND NEW.state = 'UNKNOWN_OUTCOME' THEN
        -- T22, T23: reconcile once every call has settled.
        NEW.outcome_unknown_at := now();
        NEW.reconcile_attempts := 0;
        NEW.next_reconcile_at := greatest(now(), eacp.outcome_settled_at(NEW));
    ELSIF NEW.state <> 'UNKNOWN_OUTCOME' THEN
        NEW.next_reconcile_at := NULL;
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

    WHEN 'T28' THEN
        -- Reconciler claim: the next generation, bound to the claiming
        -- reconciler, once the outcome has settled.
        IF NEW.lease_generation <> OLD.lease_generation + 1
           OR eacp.current_lease_generation() IS DISTINCT FROM NEW.lease_generation
           OR NEW.worker_id IS DISTINCT FROM who->>'worker' THEN
            RAISE EXCEPTION 'a claim takes the next lease generation for the claiming reconciler' USING ERRCODE = '42501';
        END IF;
        IF NEW.leased_until <= now() OR NEW.leased_until > now() + interval '10 minutes' THEN
            RAISE EXCEPTION 'a lease lasts at most 10 minutes' USING ERRCODE = '23514';
        END IF;
        IF COALESCE(OLD.next_reconcile_at, '-infinity') > now() THEN
            RAISE EXCEPTION 'reconciliation is not due' USING ERRCODE = '55000';
        END IF;
        IF ct.reconciliation_lookup <> 'by_operation_key' OR ct.proof_standard = 'none'
           OR ct.revoked_at IS NOT NULL OR ct.side_effects = ARRAY['READ_ONLY'] THEN
            RAISE EXCEPTION 'the pinned contract has no usable reconciliation lookup' USING ERRCODE = '55000';
        END IF;
        NEW.heartbeat_at := now();

    WHEN 'T29' THEN
        IF ct.side_effects = ARRAY['READ_ONLY'] AND ct.revoked_at IS NULL THEN
            RAISE EXCEPTION 'an unknown read is retried (T29a)' USING ERRCODE = '55000';
        END IF;
        -- A human sees an unknown outcome only once every call has settled,
        -- so a late result is evidence before anyone resolves it.
        IF COALESCE(eacp.outcome_settled_at(OLD), '-infinity') > now() THEN
            RAISE EXCEPTION 'the outcome has not settled' USING ERRCODE = '55000';
        END IF;
        PERFORM eacp.require_reason(NEW.state_reason, 'a human resolution request');

    WHEN 'T29a' THEN
        IF ct.side_effects <> ARRAY['READ_ONLY'] OR ct.revoked_at IS NOT NULL THEN
            RAISE EXCEPTION 'only an unrevoked READ_ONLY contract retries an unknown outcome' USING ERRCODE = '55000';
        END IF;
        NEW.next_attempt_at := now();

    WHEN 'T30' THEN
        -- Positive evidence: a record carrying the operation key, not
        -- contradicted by a success the target reported for any attempt.
        PERFORM eacp.assert_lease_holder(who, OLD);
        IF chk.result IS DISTINCT FROM 'found' THEN
            RAISE EXCEPTION 'T30 requires positive evidence recorded in this transaction' USING ERRCODE = '55000';
        END IF;
        IF EXISTS (SELECT 1 FROM eacp.action_attempts x
                   WHERE x.tenant_id = NEW.tenant_id AND x.action_id = NEW.id AND x.outcome = 'succeeded'
                     AND x.external_reference <> chk.external_reference) THEN
            RAISE EXCEPTION 'a reported result contradicts the evidence' USING ERRCODE = '55000';
        END IF;
        NEW.external_reference := chk.external_reference;

    WHEN 'T31', 'T32' THEN
        -- Negative evidence proves nothing unless the pinned contract is
        -- AUTHORITATIVE (§20.2, invariant 13), no attempt reported success,
        -- and every call has settled.
        PERFORM eacp.assert_lease_holder(who, OLD);
        IF chk.result IS DISTINCT FROM 'absent' OR ct.proof_standard <> 'authoritative'
           OR ct.reconciliation_consistency <> 'strong' OR ct.reconciliation_lookup <> 'by_operation_key'
           OR ct.revoked_at IS NOT NULL THEN
            RAISE EXCEPTION 'only authoritative negative evidence proves an action did not execute' USING ERRCODE = '55000';
        END IF;
        IF EXISTS (SELECT 1 FROM eacp.action_attempts x
                   WHERE x.tenant_id = NEW.tenant_id AND x.action_id = NEW.id AND x.outcome = 'succeeded') THEN
            RAISE EXCEPTION 'a reported success contradicts the negative evidence' USING ERRCODE = '55000';
        END IF;
        IF eacp.outcome_settled_at(NEW) > now() THEN
            RAISE EXCEPTION 'negative evidence waits until every call has settled' USING ERRCODE = '55000';
        END IF;
        retry := OLD.cancel_requested_at IS NULL AND NEW.not_after > now() AND NEW.attempt_count < ct.max_attempts;
        IF move = 'T31' AND NOT retry THEN
            RAISE EXCEPTION 'T31 does not permit a retry' USING ERRCODE = '55000';
        ELSIF move = 'T32' AND retry THEN
            RAISE EXCEPTION 'a retry is permitted; T32 applies only when it is not' USING ERRCODE = '55000';
        END IF;
        IF move = 'T31' AND (NEW.next_attempt_at <= now() OR NEW.next_attempt_at > now() + interval '1 hour') THEN
            RAISE EXCEPTION 'a retry is scheduled within the next hour' USING ERRCODE = '23514';
        END IF;
        PERFORM eacp.require_reason(NEW.state_reason, 'a reconciliation outcome');

    WHEN 'T33' THEN
        -- Still unknown: an inconclusive check by the holder, or a lapsed
        -- reconciler lease. Counted, and backed off.
        IF comp = 'reconciler' THEN
            PERFORM eacp.assert_lease_holder(who, OLD);
            IF chk.result IS NULL OR chk.result NOT IN ('absent', 'unknown') THEN
                RAISE EXCEPTION 'T33 requires an inconclusive check recorded in this transaction' USING ERRCODE = '55000';
            END IF;
        ELSIF OLD.leased_until > now() THEN
            RAISE EXCEPTION 'the lease has not expired' USING ERRCODE = '55000';
        END IF;
        IF NEW.reconcile_attempts <> OLD.reconcile_attempts + 1 THEN
            RAISE EXCEPTION 'T33 counts the reconciliation attempt' USING ERRCODE = '55000';
        END IF;
        IF NEW.next_reconcile_at IS NULL OR NEW.next_reconcile_at < now()
           OR NEW.next_reconcile_at > now() + interval '1 hour' THEN
            RAISE EXCEPTION 'the next reconciliation is scheduled within the next hour' USING ERRCODE = '23514';
        END IF;
        PERFORM eacp.require_reason(NEW.state_reason, 'an unknown outcome');

    WHEN 'T34' THEN
        -- Conflict or exhaustion: a human decides. From RECONCILING the
        -- holder records what it saw.
        IF OLD.state = 'RECONCILING' THEN
            PERFORM eacp.assert_lease_holder(who, OLD);
            IF chk.result IS NULL THEN
                RAISE EXCEPTION 'T34 from RECONCILING requires a check recorded in this transaction' USING ERRCODE = '55000';
            END IF;
        ELSIF ct.side_effects = ARRAY['READ_ONLY'] AND ct.revoked_at IS NULL THEN
            RAISE EXCEPTION 'an unknown read is retried (T29a)' USING ERRCODE = '55000';
        ELSIF COALESCE(eacp.outcome_settled_at(OLD), '-infinity') > now() THEN
            RAISE EXCEPTION 'the outcome has not settled' USING ERRCODE = '55000';
        END IF;
        PERFORM eacp.require_reason(NEW.state_reason, 'a human resolution request');

    WHEN 'T35', 'T36', 'T37' THEN
        -- Operator resolution (§20.3): applied through an action_resolutions
        -- row decided by this operator in this transaction.
        SELECT * INTO res FROM eacp.action_resolutions
        WHERE tenant_id = NEW.tenant_id AND action_id = NEW.id AND state = 'APPLIED'
          AND decided_at = now() AND decided_by = who_id;
        IF NOT FOUND OR res.outcome IS DISTINCT FROM (CASE move WHEN 'T35' THEN 'succeeded'
                                                               WHEN 'T36' THEN 'failed' ELSE 'retry' END) THEN
            RAISE EXCEPTION '% requires an operator resolution applied in this transaction', move USING ERRCODE = '55000';
        END IF;
        PERFORM eacp.assert_resolver(who_id, NEW);
        PERFORM eacp.require_reason(NEW.state_reason, 'a resolution');
        IF move = 'T35' THEN
            NEW.external_reference := res.external_reference;
        ELSIF move = 'T37' THEN
            IF res.proposed_by = res.decided_by THEN
                RAISE EXCEPTION 'a retry needs a second, distinct operator' USING ERRCODE = '42501';
            END IF;
            PERFORM eacp.assert_retry_possible(NEW);
            NEW.next_attempt_at := now();
        END IF;
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
-- intent inserts its attempt row; a resolved action voids a live retry
-- proposal.
CREATE OR REPLACE FUNCTION eacp.actions_after() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.state IS NOT DISTINCT FROM OLD.state THEN
        RETURN NULL;  -- heartbeat or cancel request
    END IF;
    IF OLD.state = 'NEEDS_HUMAN_RESOLUTION' THEN
        -- Resolved: a live retry proposal no longer applies.
        UPDATE eacp.action_resolutions SET state = 'VOIDED'
        WHERE tenant_id = NEW.tenant_id AND action_id = NEW.id AND state = 'PROPOSED';
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
            'next_attempt_at', NEW.next_attempt_at,
            'reconcile_attempts', NULLIF(NEW.reconcile_attempts, 0),
            'next_reconcile_at', NEW.next_reconcile_at)))::text, 'UTF8'));
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- ------------------------------------------------- reconciliation checks

-- +goose StatementBegin
-- A check is recorded only by the reconciler holding the action's lease at
-- that generation. The database stamps who, when and the pinned proof
-- standard the check was made under.
CREATE FUNCTION eacp.reconciliation_checks_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    who jsonb := eacp.actor_context();
    a   eacp.actions%ROWTYPE;
    ct  eacp.tool_contracts%ROWTYPE;
BEGIN
    SELECT * INTO a FROM eacp.actions
    WHERE tenant_id = NEW.tenant_id AND id = NEW.action_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'a reconciliation check requires an action' USING ERRCODE = '23503';
    END IF;
    PERFORM eacp.assert_lease_holder(who, a);
    IF a.state <> 'RECONCILING' OR NEW.lease_generation IS DISTINCT FROM a.lease_generation THEN
        RAISE EXCEPTION 'a check is recorded under the current reconciler lease' USING ERRCODE = '55000';
    END IF;
    SELECT * INTO ct FROM eacp.tool_contracts
    WHERE tenant_id = a.tenant_id AND id = a.connector_contract_id FOR SHARE;
    NEW.reconciler_id := a.worker_id;
    NEW.checked_at := now();
    NEW.proof_standard := ct.proof_standard;
    NEW.connector_contract_id := ct.id;
    NEW.connector_contract_version := ct.version;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER reconciliation_checks_guard BEFORE INSERT ON eacp.reconciliation_checks
    FOR EACH ROW EXECUTE FUNCTION eacp.reconciliation_checks_guard();

-- ---------------------------------------------------- human resolutions

-- +goose StatementBegin
CREATE FUNCTION eacp.action_resolutions_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    who jsonb := eacp.actor_context();
    p   uuid  := CASE WHEN who->>'kind' = 'principal' THEN (who->>'id')::uuid END;
    a   eacp.actions%ROWTYPE;
BEGIN
    -- The action first (lock order, migration 00005).
    SELECT * INTO a FROM eacp.actions
    WHERE tenant_id = NEW.tenant_id AND id = NEW.action_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'a resolution requires an action' USING ERRCODE = '23503';
    END IF;

    IF TG_OP = 'UPDATE' AND OLD.state = 'PROPOSED' AND NEW.state = 'VOIDED' THEN
        -- The action left NEEDS_HUMAN_RESOLUTION (eacp.actions_after).
        IF a.state = 'NEEDS_HUMAN_RESOLUTION' THEN
            RAISE EXCEPTION 'a proposal is voided only when its action is resolved' USING ERRCODE = '55000';
        END IF;
        IF (to_jsonb(NEW) - 'state') IS DISTINCT FROM (to_jsonb(OLD) - 'state') THEN
            RAISE EXCEPTION 'voiding changes only the state' USING ERRCODE = '55000';
        END IF;
        NEW.decided_at := now();
        RETURN NEW;
    END IF;

    PERFORM eacp.assert_resolver(p, a);
    IF a.state <> 'NEEDS_HUMAN_RESOLUTION' THEN
        RAISE EXCEPTION 'action % is %, not NEEDS_HUMAN_RESOLUTION', a.id, a.state USING ERRCODE = '55000';
    END IF;

    IF TG_OP = 'INSERT' THEN
        NEW.proposed_by := p;
        NEW.proposed_at := now();
        IF NEW.outcome = 'retry' THEN
            -- A retry dispatches again: a second operator applies it (T37).
            PERFORM eacp.assert_retry_possible(a);
            NEW.state := 'PROPOSED';
            NEW.decided_by := NULL;
            NEW.decided_at := NULL;
            NEW.decision_reason := NULL;
        ELSE
            NEW.state := 'APPLIED';
            NEW.decided_by := p;
            NEW.decided_at := now();
            NEW.decision_reason := NEW.reason;
        END IF;
        RETURN NEW;
    END IF;

    -- A proposal is applied or withdrawn once.
    IF OLD.state <> 'PROPOSED' OR NEW.state NOT IN ('APPLIED', 'WITHDRAWN') THEN
        RAISE EXCEPTION 'resolution % is %', OLD.id, OLD.state USING ERRCODE = '55000';
    END IF;
    IF (to_jsonb(NEW) - ARRAY['state', 'decision_reason']) IS DISTINCT FROM
       (to_jsonb(OLD) - ARRAY['state', 'decision_reason']) THEN
        RAISE EXCEPTION 'a decision changes only the state and its reason' USING ERRCODE = '55000';
    END IF;
    PERFORM eacp.require_reason(NEW.decision_reason, 'a resolution decision');
    IF NEW.state = 'APPLIED' THEN
        IF p = OLD.proposed_by THEN
            RAISE EXCEPTION 'a retry needs a second, distinct operator' USING ERRCODE = '42501';
        END IF;
        PERFORM eacp.assert_retry_possible(a);
    END IF;
    NEW.decided_by := p;
    NEW.decided_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- An applied resolution moves its action (T35-T37) in the same transaction.
CREATE FUNCTION eacp.action_resolutions_apply() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    UPDATE eacp.actions
    SET state = CASE NEW.outcome WHEN 'succeeded' THEN 'SUCCEEDED' WHEN 'failed' THEN 'FAILED' ELSE 'RETRY_WAIT' END,
        state_reason = 'operator resolution: ' || left(NEW.reason, 990)
    WHERE tenant_id = NEW.tenant_id AND id = NEW.action_id;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Every resolution step is a privileged operator action (§57), journaled
-- with its actor and reason.
CREATE FUNCTION eacp.audit_resolution_change() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    who jsonb := eacp.actor_context();
BEGIN
    INSERT INTO eacp.audit_events (tenant_id, payload)
    VALUES (NEW.tenant_id, convert_to(jsonb_build_object(
        'v', 1,
        'actor', who - 'component',
        'action', 'action.resolution',
        'subject', jsonb_build_object('type', 'action', 'id', NEW.action_id),
        'reason', CASE WHEN TG_OP = 'INSERT' THEN NEW.reason
                       ELSE COALESCE(NEW.decision_reason, 'action resolved otherwise') END,
        'data', jsonb_strip_nulls(jsonb_build_object(
            'resolution_id', NEW.id,
            'outcome', NEW.outcome,
            'state', NEW.state,
            'evidence', CASE WHEN TG_OP = 'INSERT' THEN NEW.evidence END,
            'external_reference', CASE WHEN TG_OP = 'INSERT' THEN NEW.external_reference END,
            'proposed_by', NEW.proposed_by,
            'decided_by', NEW.decided_by)))::text, 'UTF8'));
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER action_resolutions_guard BEFORE INSERT OR UPDATE ON eacp.action_resolutions
    FOR EACH ROW EXECUTE FUNCTION eacp.action_resolutions_guard();
CREATE TRIGGER b_apply AFTER INSERT OR UPDATE ON eacp.action_resolutions
    FOR EACH ROW WHEN (NEW.state = 'APPLIED') EXECUTE FUNCTION eacp.action_resolutions_apply();
CREATE TRIGGER zz_audit AFTER INSERT OR UPDATE ON eacp.action_resolutions
    FOR EACH ROW EXECUTE FUNCTION eacp.audit_resolution_change();

-- ---------------------------------------------------- RLS and privileges

ALTER TABLE eacp.reconciliation_checks ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.reconciliation_checks FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.reconciliation_checks USING (tenant_id = eacp.current_tenant_id());
REVOKE UPDATE ON eacp.reconciliation_checks FROM eacp_app;

ALTER TABLE eacp.action_resolutions ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.action_resolutions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.action_resolutions USING (tenant_id = eacp.current_tenant_id());
REVOKE UPDATE ON eacp.action_resolutions FROM eacp_app;
GRANT UPDATE (state, decision_reason) ON eacp.action_resolutions TO eacp_app;

GRANT UPDATE (reconcile_attempts, next_reconcile_at) ON eacp.actions TO eacp_app;

-- Cross-tenant scans (as in 00005, 00006): the schema owner reads pinned
-- contracts to match reconciliation claims to a worker's credentials.
CREATE POLICY owner_scan ON eacp.tool_contracts FOR SELECT TO CURRENT_USER USING (true);

CREATE OR REPLACE FUNCTION eacp.tenants_with_open_actions(after_tenant uuid, lim integer) RETURNS TABLE (tenant_id uuid)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$ SELECT DISTINCT a.tenant_id FROM eacp.actions a
          WHERE a.state IN ('RECEIVED', 'PENDING_APPROVAL', 'AUTHORIZED', 'QUEUED', 'LEASED', 'EXECUTING',
                            'RETRY_WAIT', 'UNKNOWN_OUTCOME', 'RECONCILING')
            AND (after_tenant IS NULL OR a.tenant_id > after_tenant)
          ORDER BY a.tenant_id LIMIT least(greatest(lim, 1), 1000) $$;

-- The reconciliation claim hint (T28): UNKNOWN_OUTCOME actions that are due,
-- whose pinned, unrevoked, non-READ_ONLY contract has a lookup by operation
-- key with a proof standard, and whose connector protocol and (tenant,
-- secret_ref, endpoint host) credential the reconciler holds. Only tenant
-- and action ids leave the function.
CREATE FUNCTION eacp.reconcilable_actions(p_protocols text[], p_bindings jsonb, lim integer)
    RETURNS TABLE (tenant_id uuid, action_id uuid)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$ SELECT a.tenant_id, a.id FROM eacp.actions a
          JOIN eacp.tool_contracts k ON k.tenant_id = a.tenant_id AND k.id = a.connector_contract_id
          JOIN eacp.tools t ON t.tenant_id = a.tenant_id AND t.id = a.tool_id
          JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
          WHERE a.state = 'UNKNOWN_OUTCOME' AND COALESCE(a.next_reconcile_at, '-infinity') <= now()
            AND k.reconciliation_lookup = 'by_operation_key' AND k.proof_standard <> 'none'
            AND k.revoked_at IS NULL AND k.side_effects <> ARRAY['READ_ONLY']
            AND c.protocol = ANY (p_protocols)
            AND EXISTS (SELECT 1 FROM jsonb_to_recordset(p_bindings) AS b(tenant_id uuid, secret_ref text, host text)
                        WHERE b.tenant_id = a.tenant_id AND b.secret_ref = c.secret_ref
                          AND b.host = lower(substring(c.endpoint FROM '^https?://([^/?#]+)')))
          ORDER BY a.next_reconcile_at NULLS FIRST, a.id LIMIT least(greatest(lim, 1), 100) $$;

REVOKE ALL ON FUNCTION eacp.reconcilable_actions(text[], jsonb, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.reconcilable_actions(text[], jsonb, integer) TO eacp_app;

-- +goose Down
DROP FUNCTION eacp.reconcilable_actions(text[], jsonb, integer);
DROP POLICY owner_scan ON eacp.tool_contracts;
DROP TABLE eacp.action_resolutions;
DROP TABLE eacp.reconciliation_checks;
DROP FUNCTION eacp.audit_resolution_change();
DROP FUNCTION eacp.action_resolutions_apply();
DROP FUNCTION eacp.action_resolutions_guard();
DROP FUNCTION eacp.reconciliation_checks_guard();

-- Restore the 00006 function bodies (verbatim).
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

-- +goose StatementBegin
-- eacp.assert_lease_holder fails unless the actor is the worker holding
-- action a's lease at generation a.lease_generation.
CREATE OR REPLACE FUNCTION eacp.assert_lease_holder(who jsonb, a eacp.actions) RETURNS void
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

CREATE OR REPLACE FUNCTION eacp.tenants_with_open_actions(after_tenant uuid, lim integer) RETURNS TABLE (tenant_id uuid)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$ SELECT DISTINCT a.tenant_id FROM eacp.actions a
          WHERE a.state IN ('RECEIVED', 'PENDING_APPROVAL', 'AUTHORIZED', 'QUEUED', 'LEASED', 'EXECUTING', 'RETRY_WAIT')
            AND (after_tenant IS NULL OR a.tenant_id > after_tenant)
          ORDER BY a.tenant_id LIMIT least(greatest(lim, 1), 1000) $$;

DROP FUNCTION eacp.assert_retry_possible(eacp.actions);
DROP FUNCTION eacp.assert_resolver(uuid, eacp.actions);
DROP FUNCTION eacp.outcome_settled_at(eacp.actions);
DROP INDEX eacp.actions_reconcile;
ALTER TABLE eacp.actions
    DROP CONSTRAINT reconcile_only_unknown,
    DROP CONSTRAINT leased_has_worker,
    DROP CONSTRAINT lease_iff_leased,
    ADD CONSTRAINT lease_iff_leased CHECK ((state IN ('LEASED', 'EXECUTING')) = (leased_until IS NOT NULL)),
    ADD CONSTRAINT leased_has_worker CHECK (state NOT IN ('LEASED', 'EXECUTING') OR worker_id IS NOT NULL),
    DROP COLUMN outcome_unknown_at,
    DROP COLUMN next_reconcile_at,
    DROP COLUMN reconcile_attempts;
