-- EACP Phase 13: backpressure, bulkheads, circuit breakers and retry budgets
-- (ADR-022, MASTER_PLAN §26-§30).
--
-- * tool_contracts gains max_queued (connector queue admission), and the
--   retry budget's retry_max_elapsed_ms and retry_max_cost.
-- * eacp.retry_budget_exhausted is the one answer to "may this action be
--   retried": eacp.actions_guard (T20, T22a, T25-T27, T31/T32) and
--   eacp.assert_retry_possible (T37) now ask it instead of comparing
--   attempt_count with max_attempts. The guard is otherwise unchanged from
--   migration 00008.
-- * eacp.connector_circuits holds one row per connector, created with it:
--   a worker's breaker opens it (open_until), an operator disables and
--   enables it. T14 refuses a claim and T16 a dispatch intent while it is
--   open; T16 reads the row FOR SHARE (principle 7), so no dispatch intent
--   commits after a disable does.
-- * eacp.claimable_actions takes the worker's skip list (full bulkheads,
--   open local breakers) and omits connectors whose circuit is open.
--
-- Down restores every replaced function verbatim.

-- +goose Up

-- ------------------------------------------------ contract limits (§26, §30)

ALTER TABLE eacp.tool_contracts
    ADD COLUMN max_queued           integer       CHECK (max_queued > 0),
    ADD COLUMN retry_max_elapsed_ms integer       CHECK (retry_max_elapsed_ms BETWEEN 1000 AND 86400000),
    ADD COLUMN retry_max_cost       numeric(21,6) CHECK (retry_max_cost >= 0),
    ADD CONSTRAINT retry_cost_needs_unit CHECK (retry_max_cost IS NULL OR cost_unit IS NOT NULL);

-- +goose StatementBegin
-- Which retry limit of the pinned contract ct action a has reached, or NULL
-- when another attempt is within the budget (ADR-022 §5):
--   attempts  attempt_count has reached max_attempts;
--   elapsed   retry_max_elapsed_ms has passed since the first dispatch intent;
--   cost      the next retry would take the cost of retries (every call after
--             the first, at the contract's cost) past retry_max_cost, or the
--             cost cannot be computed.
CREATE FUNCTION eacp.retry_budget_exhausted(a eacp.actions, ct eacp.tool_contracts) RETURNS text
    LANGUAGE sql STABLE
    AS $$
    SELECT CASE
        WHEN a.attempt_count >= ct.max_attempts THEN 'attempts'
        WHEN ct.retry_max_elapsed_ms IS NOT NULL
             AND now() >= COALESCE((SELECT min(x.dispatched_at) FROM eacp.action_attempts x
                                    WHERE x.tenant_id = a.tenant_id AND x.action_id = a.id), a.released_at)
                          + make_interval(secs => ct.retry_max_elapsed_ms / 1000.0) THEN 'elapsed'
        WHEN ct.retry_max_cost IS NOT NULL
             AND COALESCE(a.attempt_count * eacp.action_cost(ct, a.enforced_payload::jsonb) > ct.retry_max_cost,
                          true) THEN 'cost'
    END
    $$;
-- +goose StatementEnd

-- +goose StatementBegin
-- An operator retry (T37) is possible only within the retry budget
-- (ADR-022 §5); otherwise the operator resolves the action as failed.
CREATE OR REPLACE FUNCTION eacp.assert_retry_possible(a eacp.actions) RETURNS void
    LANGUAGE plpgsql
    AS $$
DECLARE
    ct eacp.tool_contracts%ROWTYPE;
BEGIN
    SELECT * INTO ct FROM eacp.tool_contracts
    WHERE tenant_id = a.tenant_id AND id = a.connector_contract_id FOR SHARE;
    IF a.cancel_requested_at IS NOT NULL OR a.not_after <= now() OR eacp.retry_budget_exhausted(a, ct) IS NOT NULL THEN
        RAISE EXCEPTION 'no retry is possible (cancel requested, expired or retry budget exhausted); resolve it as failed'
            USING ERRCODE = '55000';
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Connector queue admission (ADR-022 §1): whether the capacity group of the
-- tool named p_tool ("connector.tool") already holds as many released,
-- unfinished actions as the smallest max_queued among the tool's active
-- contract and the pinned contracts of those actions. Under RLS: the
-- caller's tenant only. Advisory: concurrent submissions may overshoot.
CREATE FUNCTION eacp.connector_queue_full(p_tool text) RETURNS boolean
    LANGUAGE sql STABLE
    AS $$
    WITH candidate AS (
        SELECT COALESCE('group:' || k.concurrency_group, 'connector:' || c.id::text) AS capacity_key, k.max_queued
          FROM eacp.tools t
          JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
          JOIN eacp.tool_contracts k ON k.tenant_id = t.tenant_id AND k.id = t.active_contract_id
         WHERE t.tenant_id = eacp.current_tenant_id() AND c.name || '.' || t.name = p_tool
    ), queued AS (
        SELECT count(*) AS n, min(ak.max_queued) AS cap
          FROM eacp.actions a
          JOIN eacp.tool_contracts ak ON ak.tenant_id = a.tenant_id AND ak.id = a.connector_contract_id
          JOIN eacp.tools at ON at.tenant_id = ak.tenant_id AND at.id = ak.tool_id
          JOIN eacp.connectors ac ON ac.tenant_id = at.tenant_id AND ac.id = at.connector_id
         WHERE a.state IN ('QUEUED', 'LEASED', 'EXECUTING', 'RETRY_WAIT')
           AND COALESCE('group:' || ak.concurrency_group, 'connector:' || ac.id::text)
               = (SELECT capacity_key FROM candidate)
    )
    SELECT COALESCE((SELECT n FROM queued) >= LEAST((SELECT max_queued FROM candidate), (SELECT cap FROM queued)),
                    false)
    $$;
-- +goose StatementEnd

-- ------------------------------------------------------ the retry budget

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
        WHEN OLD.state = 'RECEIVED'         AND NEW.state = 'CANCELLED'        THEN 'T5a'
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

    WHEN 'T5a', 'T9' THEN
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
               OR eacp.retry_budget_exhausted(NEW, ct) IS NOT NULL
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
                    OR eacp.retry_budget_exhausted(NEW, ct) IS NOT NULL) THEN
                RAISE EXCEPTION 'the retry budget is not exhausted' USING ERRCODE = '55000';
            END IF;
            PERFORM eacp.require_reason(NEW.state_reason, 'a failure');
        ELSE
            IF OLD.next_attempt_at > now() OR OLD.cancel_requested_at IS NOT NULL
               OR NEW.not_after <= now() OR eacp.retry_budget_exhausted(NEW, ct) IS NOT NULL THEN
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
        retry := OLD.cancel_requested_at IS NULL AND NEW.not_after > now()
                 AND eacp.retry_budget_exhausted(NEW, ct) IS NULL;
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

-- ---------------------------------------------- connector circuits (§28)

CREATE TABLE eacp.connector_circuits (
    tenant_id         uuid        NOT NULL,
    connector_id      uuid        NOT NULL,
    open_until        timestamptz,
    disabled          boolean     NOT NULL DEFAULT false,
    reason            text        NOT NULL,
    changed_at        timestamptz NOT NULL DEFAULT now(),
    changed_by        uuid,
    changed_by_worker text,
    PRIMARY KEY (tenant_id, connector_id),
    FOREIGN KEY (tenant_id, connector_id) REFERENCES eacp.connectors (tenant_id, id),
    FOREIGN KEY (tenant_id, changed_by) REFERENCES eacp.principals (tenant_id, id),
    CHECK (num_nonnulls(changed_by, changed_by_worker) <= 1)
);

-- +goose StatementBegin
-- The circuit state of a connector: 'disabled' (by an operator), 'open' (a
-- worker's breaker tripped and open_until lies ahead) or NULL (closed).
CREATE FUNCTION eacp.connector_circuit_state(p_tenant uuid, p_connector uuid) RETURNS text
    LANGUAGE sql STABLE
    AS $$
    SELECT CASE WHEN disabled THEN 'disabled' WHEN open_until > now() THEN 'open' END
      FROM eacp.connector_circuits WHERE tenant_id = p_tenant AND connector_id = p_connector
    $$;
-- +goose StatementEnd

-- +goose StatementBegin
-- A circuit is created closed with its connector. Afterwards only a worker
-- opens it, forward only and for at most 10 minutes, and only an operator
-- disables or enables it; enabling clears an automatic trip too. The
-- database stamps who changed it and when, and a reason is required.
CREATE FUNCTION eacp.connector_circuits_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    who   jsonb := eacp.actor_context();
    akind text  := who->>'kind';
    comp  text  := who->>'component';
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF pg_trigger_depth() < 2 AND comp IS DISTINCT FROM 'owner' THEN
            RAISE EXCEPTION 'a connector circuit is created with its connector' USING ERRCODE = '42501';
        END IF;
        NEW.open_until := NULL;
        NEW.disabled := false;
        NEW.reason := 'registered';
        NEW.changed_at := now();
        NEW.changed_by := CASE WHEN akind = 'principal' THEN (who->>'id')::uuid END;
        NEW.changed_by_worker := NULL;
        RETURN NEW;
    END IF;
    IF ROW(NEW.tenant_id, NEW.connector_id, NEW.changed_at, NEW.changed_by, NEW.changed_by_worker)
       IS DISTINCT FROM ROW(OLD.tenant_id, OLD.connector_id, OLD.changed_at, OLD.changed_by, OLD.changed_by_worker) THEN
        RAISE EXCEPTION 'a circuit''s identity and change stamps are set by the database' USING ERRCODE = '55000';
    END IF;
    PERFORM eacp.require_reason(NEW.reason, 'a circuit change');
    IF akind = 'system' AND comp = 'worker' THEN
        IF NEW.disabled IS DISTINCT FROM OLD.disabled THEN
            RAISE EXCEPTION 'only an operator disables or enables a connector' USING ERRCODE = '42501';
        END IF;
        IF NEW.open_until IS NULL OR NEW.open_until <= now() OR NEW.open_until > now() + interval '10 minutes' THEN
            RAISE EXCEPTION 'a worker opens a circuit for at most 10 minutes' USING ERRCODE = '23514';
        END IF;
        IF OLD.open_until > NEW.open_until THEN
            NEW.open_until := OLD.open_until;  -- never earlier
        END IF;
        NEW.changed_by := NULL;
        NEW.changed_by_worker := who->>'worker';
    ELSIF akind = 'principal' THEN
        PERFORM eacp.assert_role((who->>'id')::uuid, 'operator');
        IF NEW.open_until IS DISTINCT FROM OLD.open_until THEN
            RAISE EXCEPTION 'an operator disables or enables a connector; only a worker opens its circuit'
                USING ERRCODE = '42501';
        END IF;
        IF NOT NEW.disabled THEN
            NEW.open_until := NULL;
        END IF;
        NEW.changed_by := (who->>'id')::uuid;
        NEW.changed_by_worker := NULL;
    ELSE
        RAISE EXCEPTION 'only a worker or an operator changes a connector circuit' USING ERRCODE = '42501';
    END IF;
    NEW.changed_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.audit_connector_circuit() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    who jsonb := eacp.actor_context();
BEGIN
    INSERT INTO eacp.audit_events (tenant_id, payload)
    VALUES (NEW.tenant_id, convert_to(jsonb_build_object(
        'v', 1,
        'actor', who - 'component',
        'action', CASE WHEN NEW.changed_by_worker IS NOT NULL THEN 'connector.circuit_opened'
                       WHEN NEW.disabled THEN 'connector.circuit_disabled'
                       ELSE 'connector.circuit_enabled' END,
        'subject', jsonb_build_object('type', 'connector', 'id', NEW.connector_id),
        'reason', NEW.reason,
        'data', jsonb_strip_nulls(jsonb_build_object(
            'component', who->>'component',
            'open_until', NEW.open_until,
            'disabled', NEW.disabled)))::text, 'UTF8'));
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER connector_circuits_guard BEFORE INSERT OR UPDATE ON eacp.connector_circuits
    FOR EACH ROW EXECUTE FUNCTION eacp.connector_circuits_guard();
CREATE TRIGGER zz_audit AFTER UPDATE ON eacp.connector_circuits
    FOR EACH ROW EXECUTE FUNCTION eacp.audit_connector_circuit();

-- Existing connectors get their (closed) circuit before RLS applies to the
-- table; new ones get it from connectors_circuit.
INSERT INTO eacp.connector_circuits (tenant_id, connector_id, reason)
SELECT tenant_id, id, 'registered' FROM eacp.connectors;

ALTER TABLE eacp.connector_circuits ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.connector_circuits FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.connector_circuits USING (tenant_id = eacp.current_tenant_id());
-- The SECURITY DEFINER claim hint omits open circuits across tenants.
CREATE POLICY owner_scan ON eacp.connector_circuits FOR SELECT TO CURRENT_USER USING (true);

-- +goose StatementBegin
CREATE FUNCTION eacp.connectors_circuit() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    INSERT INTO eacp.connector_circuits (tenant_id, connector_id, reason) VALUES (NEW.tenant_id, NEW.id, 'registered');
    RETURN NULL;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER connectors_circuit AFTER INSERT ON eacp.connectors
    FOR EACH ROW EXECUTE FUNCTION eacp.connectors_circuit();

-- +goose StatementBegin
-- T16 (ADR-022 §3): no dispatch intent while the connector's circuit is open
-- or it is disabled. The row is read FOR SHARE, so an operator's disable
-- (FOR UPDATE) and a dispatch intent serialize: after the disable commits,
-- no dispatch intent for the connector commits. A connector without its
-- circuit row is refused (conservative).
CREATE FUNCTION eacp.actions_circuit_before() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    c eacp.connector_circuits%ROWTYPE;
BEGIN
    IF OLD.state = 'LEASED' AND NEW.state = 'EXECUTING' THEN
        SELECT cc.* INTO c FROM eacp.tools t
          JOIN eacp.connector_circuits cc ON cc.tenant_id = t.tenant_id AND cc.connector_id = t.connector_id
         WHERE t.tenant_id = NEW.tenant_id AND t.id = NEW.tool_id
           FOR SHARE OF cc;
        IF NOT FOUND OR c.disabled OR c.open_until > now() THEN
            RAISE EXCEPTION 'dispatch is not permitted: connector circuit open' USING ERRCODE = '55000';
        END IF;
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER actions_circuit_before BEFORE UPDATE ON eacp.actions
    FOR EACH ROW EXECUTE FUNCTION eacp.actions_circuit_before();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.actions_capacity_before() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    candidate_group text;
    candidate_cap integer;
    candidate_connector uuid;
    active_count integer;
    active_cap integer;
    effective_cap integer;
BEGIN
    IF TG_OP = 'UPDATE' AND OLD.state = 'QUEUED' AND NEW.state = 'LEASED' THEN
        SELECT COALESCE('group:' || ct.concurrency_group, 'connector:' || c.id::text), ct.max_inflight, c.id
          INTO candidate_group, candidate_cap, candidate_connector
          FROM eacp.tool_contracts ct
          JOIN eacp.tools t ON t.tenant_id = ct.tenant_id AND t.id = ct.tool_id
          JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
         WHERE ct.tenant_id = NEW.tenant_id AND ct.id = NEW.connector_contract_id;
        -- ADR-022 §3: no claim while the connector's circuit is open or it
        -- is disabled. T16 re-checks under a row lock.
        IF eacp.connector_circuit_state(NEW.tenant_id, candidate_connector) IS NOT NULL THEN
            RAISE EXCEPTION 'connector circuit open' USING ERRCODE = '53300';
        END IF;
        PERFORM pg_advisory_xact_lock(hashtextextended(NEW.tenant_id::text || ':' || candidate_group, 0));
        SELECT count(*)::integer, min(ac.max_inflight) INTO active_count, active_cap
          FROM eacp.actions a
          JOIN eacp.tool_contracts ac ON ac.tenant_id = a.tenant_id AND ac.id = a.connector_contract_id
          JOIN eacp.tools at ON at.tenant_id = ac.tenant_id AND at.id = ac.tool_id
          JOIN eacp.connectors cc ON cc.tenant_id = at.tenant_id AND cc.id = at.connector_id
         WHERE a.tenant_id = NEW.tenant_id AND a.state IN ('LEASED', 'EXECUTING')
           AND COALESCE('group:' || ac.concurrency_group, 'connector:' || cc.id::text) = candidate_group;
        effective_cap := LEAST(candidate_cap, active_cap);
        IF effective_cap IS NULL THEN
            effective_cap := COALESCE(candidate_cap, active_cap);
        END IF;
        IF effective_cap IS NOT NULL AND active_count >= effective_cap THEN
            RAISE EXCEPTION 'connector capacity exhausted' USING ERRCODE = '53300';
        END IF;
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- Like migration 00012's hint, plus: connectors whose circuit is open are
-- omitted, and so is anything in p_skip, the worker's list of
-- {tenant_id, key} with key a capacity group ("group:<name>" or
-- "connector:<id>") whose bulkhead is full or "connector:<id>" whose local
-- breaker is open.
DROP FUNCTION eacp.claimable_actions(text[], jsonb, integer);
CREATE FUNCTION eacp.claimable_actions(p_protocols text[], p_bindings jsonb, lim integer, p_skip jsonb)
    RETURNS TABLE (tenant_id uuid, action_id uuid)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    WITH active_capacity AS MATERIALIZED (
        SELECT x.tenant_id,
               COALESCE('group:' || ac.concurrency_group, 'connector:' || xc.id::text) AS capacity_key,
               count(*)::integer AS active_count, min(ac.max_inflight) AS active_cap
          FROM eacp.actions x
          JOIN eacp.tool_contracts ac ON ac.tenant_id = x.tenant_id AND ac.id = x.connector_contract_id
          JOIN eacp.tools xt ON xt.tenant_id = ac.tenant_id AND xt.id = ac.tool_id
          JOIN eacp.connectors xc ON xc.tenant_id = xt.tenant_id AND xc.id = xt.connector_id
         WHERE x.state IN ('LEASED', 'EXECUTING')
         GROUP BY x.tenant_id, COALESCE('group:' || ac.concurrency_group, 'connector:' || xc.id::text)
    ), skip AS MATERIALIZED (
        SELECT s.tenant_id, s.key FROM jsonb_to_recordset(COALESCE(p_skip, '[]'::jsonb)) AS s(tenant_id uuid, key text)
    )
    SELECT a.tenant_id, a.id
      FROM eacp.actions a
      JOIN eacp.tools t ON t.tenant_id = a.tenant_id AND t.id = a.tool_id
      JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
      JOIN eacp.tool_contracts ct ON ct.tenant_id = a.tenant_id AND ct.id = a.connector_contract_id
      LEFT JOIN eacp.scheduler_tenant_state st ON st.tenant_id = a.tenant_id
      LEFT JOIN eacp.scheduler_team_state ss ON ss.tenant_id = a.tenant_id
        AND ss.team_key = COALESCE(a.schedule_team_key, 'agent:' || a.agent_id::text)
      LEFT JOIN active_capacity used ON used.tenant_id = a.tenant_id
        AND used.capacity_key = COALESCE('group:' || ct.concurrency_group, 'connector:' || c.id::text)
     WHERE a.state = 'QUEUED' AND a.not_after > now() AND c.protocol = ANY (p_protocols)
       AND EXISTS (SELECT 1 FROM jsonb_to_recordset(p_bindings) AS b(tenant_id uuid, secret_ref text, host text)
                   WHERE b.tenant_id = a.tenant_id AND b.secret_ref = c.secret_ref
                     AND b.host = lower(substring(c.endpoint FROM '^https?://([^/?#]+)')))
       AND (COALESCE(LEAST(ct.max_inflight, used.active_cap), ct.max_inflight, used.active_cap) IS NULL
            OR COALESCE(used.active_count, 0)
               < COALESCE(LEAST(ct.max_inflight, used.active_cap), ct.max_inflight, used.active_cap))
       AND NOT EXISTS (SELECT 1 FROM eacp.connector_circuits cc
                       WHERE cc.tenant_id = a.tenant_id AND cc.connector_id = c.id
                         AND (cc.disabled OR cc.open_until > now()))
       AND NOT EXISTS (SELECT 1 FROM skip s
                       WHERE s.tenant_id = a.tenant_id
                         AND s.key IN (COALESCE('group:' || ct.concurrency_group, 'connector:' || c.id::text),
                                       'connector:' || c.id::text))
     ORDER BY COALESCE(st.last_turn, 0), a.tenant_id,
              COALESCE(ss.last_turn, 0), COALESCE(a.schedule_team_key, 'agent:' || a.agent_id::text),
              CASE WHEN a.not_after <= now() + interval '5 minutes' THEN 9
                   ELSE LEAST(9, COALESCE(a.schedule_priority, 0)
                        + floor(extract(epoch FROM (now() - a.state_changed_at)) / 300)::integer)
                   END DESC,
              a.not_after, a.state_changed_at, a.id
     LIMIT least(greatest(lim, 1), 100)
    $$;
REVOKE ALL ON FUNCTION eacp.claimable_actions(text[], jsonb, integer, jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.claimable_actions(text[], jsonb, integer, jsonb) TO eacp_app;

-- +goose Down

DROP FUNCTION eacp.claimable_actions(text[], jsonb, integer, jsonb);
CREATE OR REPLACE FUNCTION eacp.claimable_actions(p_protocols text[], p_bindings jsonb, lim integer)
    RETURNS TABLE (tenant_id uuid, action_id uuid)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    WITH active_capacity AS MATERIALIZED (
        SELECT x.tenant_id,
               COALESCE('group:' || ac.concurrency_group, 'connector:' || xc.id::text) AS capacity_key,
               count(*)::integer AS active_count, min(ac.max_inflight) AS active_cap
          FROM eacp.actions x
          JOIN eacp.tool_contracts ac ON ac.tenant_id = x.tenant_id AND ac.id = x.connector_contract_id
          JOIN eacp.tools xt ON xt.tenant_id = ac.tenant_id AND xt.id = ac.tool_id
          JOIN eacp.connectors xc ON xc.tenant_id = xt.tenant_id AND xc.id = xt.connector_id
         WHERE x.state IN ('LEASED', 'EXECUTING')
         GROUP BY x.tenant_id, COALESCE('group:' || ac.concurrency_group, 'connector:' || xc.id::text)
    )
    SELECT a.tenant_id, a.id
      FROM eacp.actions a
      JOIN eacp.tools t ON t.tenant_id = a.tenant_id AND t.id = a.tool_id
      JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
      JOIN eacp.tool_contracts ct ON ct.tenant_id = a.tenant_id AND ct.id = a.connector_contract_id
      LEFT JOIN eacp.scheduler_tenant_state st ON st.tenant_id = a.tenant_id
      LEFT JOIN eacp.scheduler_team_state ss ON ss.tenant_id = a.tenant_id
        AND ss.team_key = COALESCE(a.schedule_team_key, 'agent:' || a.agent_id::text)
      LEFT JOIN active_capacity used ON used.tenant_id = a.tenant_id
        AND used.capacity_key = COALESCE('group:' || ct.concurrency_group, 'connector:' || c.id::text)
     WHERE a.state = 'QUEUED' AND a.not_after > now() AND c.protocol = ANY (p_protocols)
       AND EXISTS (SELECT 1 FROM jsonb_to_recordset(p_bindings) AS b(tenant_id uuid, secret_ref text, host text)
                   WHERE b.tenant_id = a.tenant_id AND b.secret_ref = c.secret_ref
                     AND b.host = lower(substring(c.endpoint FROM '^https?://([^/?#]+)')))
       AND (COALESCE(LEAST(ct.max_inflight, used.active_cap), ct.max_inflight, used.active_cap) IS NULL
            OR COALESCE(used.active_count, 0)
               < COALESCE(LEAST(ct.max_inflight, used.active_cap), ct.max_inflight, used.active_cap))
     ORDER BY COALESCE(st.last_turn, 0), a.tenant_id,
              COALESCE(ss.last_turn, 0), COALESCE(a.schedule_team_key, 'agent:' || a.agent_id::text),
              CASE WHEN a.not_after <= now() + interval '5 minutes' THEN 9
                   ELSE LEAST(9, COALESCE(a.schedule_priority, 0)
                        + floor(extract(epoch FROM (now() - a.state_changed_at)) / 300)::integer)
                   END DESC,
              a.not_after, a.state_changed_at, a.id
     LIMIT least(greatest(lim, 1), 100)
    $$;
REVOKE ALL ON FUNCTION eacp.claimable_actions(text[], jsonb, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.claimable_actions(text[], jsonb, integer) TO eacp_app;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.actions_capacity_before() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    candidate_group text;
    candidate_cap integer;
    active_count integer;
    active_cap integer;
    effective_cap integer;
BEGIN
    IF TG_OP = 'UPDATE' AND OLD.state = 'QUEUED' AND NEW.state = 'LEASED' THEN
        SELECT COALESCE('group:' || ct.concurrency_group, 'connector:' || c.id::text), ct.max_inflight
          INTO candidate_group, candidate_cap
          FROM eacp.tool_contracts ct
          JOIN eacp.tools t ON t.tenant_id = ct.tenant_id AND t.id = ct.tool_id
          JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
         WHERE ct.tenant_id = NEW.tenant_id AND ct.id = NEW.connector_contract_id;
        PERFORM pg_advisory_xact_lock(hashtextextended(NEW.tenant_id::text || ':' || candidate_group, 0));
        SELECT count(*)::integer, min(ac.max_inflight) INTO active_count, active_cap
          FROM eacp.actions a
          JOIN eacp.tool_contracts ac ON ac.tenant_id = a.tenant_id AND ac.id = a.connector_contract_id
          JOIN eacp.tools at ON at.tenant_id = ac.tenant_id AND at.id = ac.tool_id
          JOIN eacp.connectors cc ON cc.tenant_id = at.tenant_id AND cc.id = at.connector_id
         WHERE a.tenant_id = NEW.tenant_id AND a.state IN ('LEASED', 'EXECUTING')
           AND COALESCE('group:' || ac.concurrency_group, 'connector:' || cc.id::text) = candidate_group;
        effective_cap := LEAST(candidate_cap, active_cap);
        IF effective_cap IS NULL THEN
            effective_cap := COALESCE(candidate_cap, active_cap);
        END IF;
        IF effective_cap IS NOT NULL AND active_count >= effective_cap THEN
            RAISE EXCEPTION 'connector capacity exhausted' USING ERRCODE = '53300';
        END IF;
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

DROP TRIGGER actions_circuit_before ON eacp.actions;
DROP FUNCTION eacp.actions_circuit_before();
DROP TRIGGER connectors_circuit ON eacp.connectors;
DROP FUNCTION eacp.connectors_circuit();
DROP TABLE eacp.connector_circuits;
DROP FUNCTION eacp.audit_connector_circuit();
DROP FUNCTION eacp.connector_circuits_guard();
DROP FUNCTION eacp.connector_circuit_state(uuid, uuid);

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
        WHEN OLD.state = 'RECEIVED'         AND NEW.state = 'CANCELLED'        THEN 'T5a'
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

    WHEN 'T5a', 'T9' THEN
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
-- eacp.assert_retry_possible fails unless action a may be dispatched again
-- under its pinned contract: attempts remain, no cancel was requested and
-- not_after has not passed. A retry never extends either.
CREATE OR REPLACE FUNCTION eacp.assert_retry_possible(a eacp.actions) RETURNS void
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

DROP FUNCTION eacp.connector_queue_full(text);
DROP FUNCTION eacp.retry_budget_exhausted(eacp.actions, eacp.tool_contracts);
ALTER TABLE eacp.tool_contracts
    DROP CONSTRAINT retry_cost_needs_unit,
    DROP COLUMN retry_max_cost,
    DROP COLUMN retry_max_elapsed_ms,
    DROP COLUMN max_queued;
