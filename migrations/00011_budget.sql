-- EACP Phase 11: hard budget reservation (ADR-012; MASTER_PLAN §45-§47, §57, §85).
--
--   * Connector contracts declare what a call costs: a unit, a fixed cost and
--     optionally a payload amount field (and a payload unit field that must
--     match). eacp.action_cost computes it from the enforced payload.
--   * eacp.budget_accounts: a tree of accounts per unit. A child's limit is
--     escrowed from its parent (parent.allocated), so a reservation locks only
--     its agent leaf. CHECK (allocated + reserved + committed <= hard_limit)
--     is the hard guarantee: no account can be stored oversubscribed.
--   * eacp.budget_limit_changes: lowering a limit applies at once; raising it
--     is two-person (§57). Applying locks parent -> account.
--   * eacp.budget_reservations: one ACTIVE reservation per action, made in the
--     release transaction (eacp.budget_reserve) before any audited write, and
--     required by T10. The action's own state change settles it (SUCCEEDED ->
--     COMMITTED; FAILED, CANCELLED, EXPIRED, DENIED -> RELEASED) without
--     touching the account. The next reservation or limit change on that
--     leaf folds settled rows into the counters, under the leaf lock.
--
-- Lock order: action -> approval rows -> registry (FOR SHARE) -> budget leaf
-- -> audit chain head. Limit changes: parent -> account -> audit chain head.
-- Account counters change only from these triggers (pg_trigger_depth() >= 2).

-- +goose Up

-- ---------------------------------------------------- contract cost (§45)

ALTER TABLE eacp.tool_contracts
    ADD COLUMN cost_unit         text          CHECK (cost_unit ~ '^[A-Z][A-Z0-9_]{0,15}$'),
    ADD COLUMN cost_fixed        numeric(21,6) NOT NULL DEFAULT 0 CHECK (cost_fixed >= 0),
    ADD COLUMN cost_amount_field text          CHECK (cost_amount_field ~ '^[A-Za-z_][A-Za-z0-9_.-]{0,63}$'),
    ADD COLUMN cost_unit_field   text          CHECK (cost_unit_field ~ '^[A-Za-z_][A-Za-z0-9_.-]{0,63}$'),
    ADD CONSTRAINT cost_declared CHECK (
        (cost_unit IS NULL AND cost_fixed = 0 AND cost_amount_field IS NULL AND cost_unit_field IS NULL)
        OR (cost_unit IS NOT NULL AND (cost_fixed > 0 OR cost_amount_field IS NOT NULL)));

-- +goose StatementBegin
-- eacp.action_cost is what a call under contract c costs for the enforced
-- payload: cost_fixed plus the payload's amount field. NULL when c is not
-- budgeted or the payload does not state a valid cost: the amount is missing,
-- not a JSON number, negative, too precise (more than 6 decimals) or at least
-- 10^15, or the unit field does not name the contract's unit.
CREATE FUNCTION eacp.action_cost(c eacp.tool_contracts, payload jsonb) RETURNS numeric
    LANGUAGE plpgsql IMMUTABLE
    AS $$
DECLARE
    v   jsonb;
    amt numeric := 0;
BEGIN
    IF c.cost_unit IS NULL OR payload IS NULL OR jsonb_typeof(payload) <> 'object' THEN
        RETURN NULL;
    END IF;
    IF c.cost_unit_field IS NOT NULL THEN
        v := payload -> c.cost_unit_field;
        IF v IS NULL OR jsonb_typeof(v) <> 'string' OR (v #>> '{}') <> c.cost_unit THEN
            RETURN NULL;
        END IF;
    END IF;
    IF c.cost_amount_field IS NOT NULL THEN
        v := payload -> c.cost_amount_field;
        IF v IS NULL OR jsonb_typeof(v) <> 'number' THEN
            RETURN NULL;
        END IF;
        amt := (v #>> '{}')::numeric;
        IF amt < 0 OR amt >= 1e15 OR scale(amt) > 6 THEN
            RETURN NULL;
        END IF;
    END IF;
    IF c.cost_fixed + amt >= 1e15 THEN
        RETURN NULL;
    END IF;
    RETURN c.cost_fixed + amt;
END
$$;
-- +goose StatementEnd

-- ---------------------------------------------------- accounts (§46)

CREATE TABLE eacp.budget_accounts (
    tenant_id  uuid          NOT NULL REFERENCES eacp.tenants (id),
    id         uuid          NOT NULL DEFAULT gen_random_uuid(),
    name       text          NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9._-]{0,62}$'),
    unit       text          NOT NULL CHECK (unit ~ '^[A-Z][A-Z0-9_]{0,15}$'),
    parent_id  uuid,
    agent_id   uuid,
    hard_limit numeric(21,6) NOT NULL DEFAULT 0 CHECK (hard_limit >= 0),
    allocated  numeric(21,6) NOT NULL DEFAULT 0 CHECK (allocated >= 0),
    reserved   numeric(21,6) NOT NULL DEFAULT 0 CHECK (reserved >= 0),
    committed  numeric(21,6) NOT NULL DEFAULT 0 CHECK (committed >= 0),
    created_by uuid,
    created_at timestamptz   NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, name),
    UNIQUE (tenant_id, id, unit),
    FOREIGN KEY (tenant_id, parent_id, unit) REFERENCES eacp.budget_accounts (tenant_id, id, unit),
    FOREIGN KEY (tenant_id, agent_id) REFERENCES eacp.agents (tenant_id, id),
    FOREIGN KEY (tenant_id, created_by) REFERENCES eacp.principals (tenant_id, id),
    -- The hard guarantee (§103 invariant 3).
    CONSTRAINT within_limit CHECK (allocated + reserved + committed <= hard_limit)
);
-- One agent leaf per agent and unit.
CREATE UNIQUE INDEX budget_accounts_agent_leaf ON eacp.budget_accounts (tenant_id, agent_id, unit)
    WHERE agent_id IS NOT NULL;

-- +goose StatementBegin
-- An admin creates an empty account (no limit: creating one grants nothing).
-- Afterwards only the limit-change and reservation triggers change its
-- counters and limit; its identity never changes.
CREATE FUNCTION eacp.budget_accounts_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid;
BEGIN
    IF TG_OP = 'INSERT' THEN
        a := eacp.actor();
        PERFORM eacp.assert_role(a, 'admin');
        IF NEW.hard_limit <> 0 OR NEW.allocated <> 0 OR NEW.reserved <> 0 OR NEW.committed <> 0 THEN
            RAISE EXCEPTION 'a budget account starts empty; a limit change sets its limit' USING ERRCODE = '23514';
        END IF;
        IF NEW.parent_id IS NOT NULL THEN
            PERFORM 1 FROM eacp.budget_accounts
            WHERE tenant_id = NEW.tenant_id AND id = NEW.parent_id AND agent_id IS NOT NULL;
            IF FOUND THEN
                RAISE EXCEPTION 'an agent budget account has no child accounts' USING ERRCODE = '55000';
            END IF;
        END IF;
        NEW.created_by := a;
        NEW.created_at := now();
        RETURN NEW;
    END IF;
    IF pg_trigger_depth() < 2 THEN
        RAISE EXCEPTION 'a budget account changes only through reservations and limit changes' USING ERRCODE = '42501';
    END IF;
    IF (to_jsonb(NEW) - ARRAY['hard_limit', 'allocated', 'reserved', 'committed'])
       IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['hard_limit', 'allocated', 'reserved', 'committed']) THEN
        RAISE EXCEPTION 'a budget account''s identity never changes' USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER budget_accounts_guard BEFORE INSERT OR UPDATE ON eacp.budget_accounts
    FOR EACH ROW EXECUTE FUNCTION eacp.budget_accounts_guard();
-- Creation is journaled like a registry row; limits through their changes.
CREATE TRIGGER zz_audit AFTER INSERT ON eacp.budget_accounts
    FOR EACH ROW EXECUTE FUNCTION eacp.audit_row_change();

-- +goose StatementBegin
-- eacp.budget_lock_account locks an account for a limit change, its parent
-- first (root -> leaf), and returns it.
CREATE FUNCTION eacp.budget_lock_account(p_account uuid) RETURNS eacp.budget_accounts
    LANGUAGE plpgsql
    AS $$
DECLARE
    par  uuid;
    acct eacp.budget_accounts%ROWTYPE;
BEGIN
    SELECT parent_id INTO par FROM eacp.budget_accounts
    WHERE tenant_id = eacp.current_tenant_id() AND id = p_account;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'unknown budget account %', p_account USING ERRCODE = '23503';
    END IF;
    IF par IS NOT NULL THEN
        PERFORM 1 FROM eacp.budget_accounts
        WHERE tenant_id = eacp.current_tenant_id() AND id = par FOR NO KEY UPDATE;
    END IF;
    SELECT * INTO STRICT acct FROM eacp.budget_accounts
    WHERE tenant_id = eacp.current_tenant_id() AND id = p_account FOR NO KEY UPDATE;
    RETURN acct;
END
$$;
-- +goose StatementEnd

-- ---------------------------------------------------- reservations (§47)

CREATE TABLE eacp.budget_reservations (
    tenant_id        uuid          NOT NULL REFERENCES eacp.tenants (id),
    id               uuid          NOT NULL DEFAULT gen_random_uuid(),
    account_id       uuid          NOT NULL,
    unit             text          NOT NULL,
    action_id        uuid          NOT NULL,
    contract_id      uuid          NOT NULL,
    amount           numeric(21,6) NOT NULL CHECK (amount >= 0),
    state            text          NOT NULL DEFAULT 'ACTIVE' CHECK (state IN ('ACTIVE', 'COMMITTED', 'RELEASED')),
    committed_amount numeric(21,6) CHECK (committed_amount >= 0 AND committed_amount <= amount),
    created_at       timestamptz   NOT NULL DEFAULT now(),
    expires_at       timestamptz   NOT NULL,
    settled_at       timestamptz,
    settle_reason    text          CHECK (length(settle_reason) <= 64),
    folded_at        timestamptz,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, account_id, unit) REFERENCES eacp.budget_accounts (tenant_id, id, unit),
    FOREIGN KEY (tenant_id, action_id) REFERENCES eacp.actions (tenant_id, id),
    FOREIGN KEY (tenant_id, contract_id) REFERENCES eacp.tool_contracts (tenant_id, id),
    CHECK ((state = 'ACTIVE') = (settled_at IS NULL)),
    CHECK ((state = 'ACTIVE') = (committed_amount IS NULL)),
    CHECK ((state = 'ACTIVE') = (settle_reason IS NULL)),
    CHECK (state <> 'RELEASED' OR committed_amount = 0),
    CHECK (folded_at IS NULL OR state <> 'ACTIVE')
);
CREATE UNIQUE INDEX budget_reservations_one_active ON eacp.budget_reservations (tenant_id, action_id)
    WHERE state = 'ACTIVE';
CREATE INDEX budget_reservations_unfolded ON eacp.budget_reservations (tenant_id, account_id)
    WHERE folded_at IS NULL AND state <> 'ACTIVE';
CREATE INDEX budget_reservations_action ON eacp.budget_reservations (tenant_id, action_id);

-- +goose StatementBegin
-- A reservation is made for an AUTHORIZED action by its release actor, of
-- exactly the action's cost under the tool's active contract, on the
-- agent's leaf account in that unit. It is settled only as its action's
-- state allows, and folded once settled. Nothing else changes.
CREATE FUNCTION eacp.budget_reservations_guard() RETURNS trigger
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

-- +goose StatementBegin
-- The account counters follow the reservation rows: reserving adds to
-- reserved (the account CHECK refuses oversubscription); folding a settled
-- reservation moves it from reserved to committed. Settling itself does not
-- touch the account (ADR-012 §5).
CREATE FUNCTION eacp.budget_reservations_apply() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        UPDATE eacp.budget_accounts SET reserved = reserved + NEW.amount
        WHERE tenant_id = NEW.tenant_id AND id = NEW.account_id;
    ELSIF OLD.folded_at IS NULL AND NEW.folded_at IS NOT NULL THEN
        UPDATE eacp.budget_accounts
        SET reserved = reserved - NEW.amount, committed = committed + NEW.committed_amount
        WHERE tenant_id = NEW.tenant_id AND id = NEW.account_id;
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- budget.reserved, budget.committed and budget.released are journaled with
-- the actor of the transaction that made them.
CREATE FUNCTION eacp.audit_budget_reservation() RETURNS trigger
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

CREATE TRIGGER budget_reservations_guard BEFORE INSERT OR UPDATE ON eacp.budget_reservations
    FOR EACH ROW EXECUTE FUNCTION eacp.budget_reservations_guard();
CREATE TRIGGER b_apply AFTER INSERT OR UPDATE ON eacp.budget_reservations
    FOR EACH ROW EXECUTE FUNCTION eacp.budget_reservations_apply();
CREATE TRIGGER zz_audit AFTER INSERT OR UPDATE OF state ON eacp.budget_reservations
    FOR EACH ROW EXECUTE FUNCTION eacp.audit_budget_reservation();

-- +goose StatementBegin
-- eacp.budget_fold folds the settled, unfolded reservations of an account
-- the caller has locked. Rows another transaction holds are skipped: they
-- still count as reserved, which over-counts and never under-counts.
CREATE FUNCTION eacp.budget_fold(p_account uuid) RETURNS void
    LANGUAGE sql
    AS $$
    UPDATE eacp.budget_reservations SET folded_at = now()
    WHERE (tenant_id, id) IN (
        SELECT tenant_id, id FROM eacp.budget_reservations
        WHERE tenant_id = eacp.current_tenant_id() AND account_id = p_account
          AND state <> 'ACTIVE' AND folded_at IS NULL
        FOR UPDATE SKIP LOCKED)
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- eacp.budget_reserve is the budget step of the release transaction (ADR-005
-- §5a R1, ADR-012 §4), called after the action, its approval rows and the
-- registry are locked and before any audited write. It returns:
--   'unbudgeted'   the contract has no cost_unit (a stale reservation is released)
--   'reserved'     an ACTIVE reservation of the action's cost exists now
--   'exceeded'     the agent's leaf account cannot take the cost
--   'no_account'   the agent has no account in the contract's unit
--   'invalid_cost' the enforced payload does not state a valid cost
-- Only 'reserved' and 'unbudgeted' permit T10.
CREATE FUNCTION eacp.budget_reserve(p_action uuid, p_contract uuid) RETURNS text
    LANGUAGE plpgsql
    AS $$
DECLARE
    act  eacp.actions%ROWTYPE;
    ct   eacp.tool_contracts%ROWTYPE;
    acct eacp.budget_accounts%ROWTYPE;
    cur  eacp.budget_reservations%ROWTYPE;
    cost numeric;
    room numeric;
BEGIN
    SELECT * INTO act FROM eacp.actions
    WHERE tenant_id = eacp.current_tenant_id() AND id = p_action FOR UPDATE;
    IF NOT FOUND OR act.state <> 'AUTHORIZED' THEN
        RAISE EXCEPTION 'budget is reserved only for an AUTHORIZED action' USING ERRCODE = '55000';
    END IF;
    SELECT * INTO ct FROM eacp.tool_contracts
    WHERE tenant_id = act.tenant_id AND id = p_contract AND tool_id = act.tool_id FOR SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'contract % is not a contract of the action''s tool', p_contract USING ERRCODE = '55000';
    END IF;
    SELECT * INTO cur FROM eacp.budget_reservations
    WHERE tenant_id = act.tenant_id AND action_id = act.id AND state = 'ACTIVE' FOR UPDATE;

    IF ct.cost_unit IS NULL THEN
        IF cur.id IS NOT NULL THEN
            UPDATE eacp.budget_reservations SET state = 'RELEASED', settle_reason = 'repriced'
            WHERE tenant_id = cur.tenant_id AND id = cur.id;
        END IF;
        RETURN 'unbudgeted';
    END IF;
    cost := eacp.action_cost(ct, act.enforced_payload::jsonb);
    IF cost IS NULL THEN
        RETURN 'invalid_cost';
    END IF;
    SELECT * INTO acct FROM eacp.budget_accounts
    WHERE tenant_id = act.tenant_id AND agent_id = act.agent_id AND unit = ct.cost_unit FOR NO KEY UPDATE;
    IF NOT FOUND THEN
        RETURN 'no_account';
    END IF;
    PERFORM eacp.budget_fold(acct.id);
    IF cur.id IS NOT NULL AND cur.account_id = acct.id AND cur.contract_id = ct.id AND cur.amount = cost THEN
        RETURN 'reserved';  -- a re-release keeps its reservation
    END IF;
    SELECT hard_limit - allocated - reserved - committed INTO room FROM eacp.budget_accounts
    WHERE tenant_id = acct.tenant_id AND id = acct.id;
    IF cur.id IS NOT NULL AND cur.account_id = acct.id THEN
        room := room + cur.amount;
    END IF;
    IF cost > room THEN
        RETURN 'exceeded';
    END IF;
    IF cur.id IS NOT NULL THEN
        -- Repriced (a new contract version or unit): release, and give the
        -- amount back at once when it is on this leaf.
        UPDATE eacp.budget_reservations SET state = 'RELEASED', settle_reason = 'repriced'
        WHERE tenant_id = cur.tenant_id AND id = cur.id;
        IF cur.account_id = acct.id THEN
            UPDATE eacp.budget_reservations SET folded_at = now()
            WHERE tenant_id = cur.tenant_id AND id = cur.id;
        END IF;
    END IF;
    INSERT INTO eacp.budget_reservations (tenant_id, account_id, unit, action_id, contract_id, amount, expires_at)
    VALUES (act.tenant_id, acct.id, ct.cost_unit, act.id, ct.id, cost, act.not_after);
    RETURN 'reserved';
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The action's state change settles its reservation in the same
-- transaction, touching only the reservation row (ADR-012 §5): SUCCEEDED
-- commits it; a terminal state without effect releases it; every other
-- state holds it (UNKNOWN_OUTCOME until reconciled or resolved). T10 of a
-- budgeted action requires its ACTIVE reservation under the pinned
-- contract, so no release skips the budget.
CREATE FUNCTION eacp.actions_budget() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    ct eacp.tool_contracts%ROWTYPE;
BEGIN
    IF NEW.state IS NOT DISTINCT FROM OLD.state THEN
        RETURN NULL;
    END IF;
    IF OLD.state = 'AUTHORIZED' AND NEW.state = 'QUEUED' THEN
        SELECT * INTO STRICT ct FROM eacp.tool_contracts
        WHERE tenant_id = NEW.tenant_id AND id = NEW.connector_contract_id;
        IF ct.cost_unit IS NULL THEN
            IF EXISTS (SELECT 1 FROM eacp.budget_reservations
                       WHERE tenant_id = NEW.tenant_id AND action_id = NEW.id AND state = 'ACTIVE') THEN
                RAISE EXCEPTION 'an unbudgeted release keeps no reservation' USING ERRCODE = '55000';
            END IF;
        ELSIF NOT EXISTS (
            SELECT 1 FROM eacp.budget_reservations r
            JOIN eacp.budget_accounts b ON b.tenant_id = r.tenant_id AND b.id = r.account_id
            WHERE r.tenant_id = NEW.tenant_id AND r.action_id = NEW.id AND r.state = 'ACTIVE'
              AND r.contract_id = ct.id AND r.unit = ct.cost_unit AND b.agent_id = NEW.agent_id
              AND r.amount = eacp.action_cost(ct, NEW.enforced_payload::jsonb)) THEN
            RAISE EXCEPTION 'release requires a budget reservation of the action''s cost' USING ERRCODE = '55000';
        END IF;
    ELSIF NEW.state = 'SUCCEEDED' THEN
        UPDATE eacp.budget_reservations SET state = 'COMMITTED', settle_reason = 'succeeded'
        WHERE tenant_id = NEW.tenant_id AND action_id = NEW.id AND state = 'ACTIVE';
    ELSIF NEW.state IN ('FAILED', 'CANCELLED', 'EXPIRED', 'DENIED') THEN
        UPDATE eacp.budget_reservations SET state = 'RELEASED', settle_reason = lower(NEW.state)
        WHERE tenant_id = NEW.tenant_id AND action_id = NEW.id AND state = 'ACTIVE';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- Sorts after actions_after and before actions_outbox_events and zz_audit.
CREATE TRIGGER actions_budget AFTER UPDATE ON eacp.actions
    FOR EACH ROW EXECUTE FUNCTION eacp.actions_budget();

-- ---------------------------------------------------- limit changes (§57)

CREATE TABLE eacp.budget_limit_changes (
    tenant_id       uuid          NOT NULL REFERENCES eacp.tenants (id),
    id              uuid          NOT NULL DEFAULT gen_random_uuid(),
    account_id      uuid          NOT NULL,
    old_limit       numeric(21,6) NOT NULL DEFAULT 0 CHECK (old_limit >= 0),
    new_limit       numeric(21,6) NOT NULL CHECK (new_limit >= 0),
    state           text          NOT NULL DEFAULT 'PROPOSED' CHECK (state IN ('PROPOSED', 'APPLIED', 'REJECTED')),
    reason          text          NOT NULL CHECK (length(reason) <= 1024),
    decision_reason text          CHECK (length(decision_reason) <= 1024),
    proposed_by     uuid,
    proposed_at     timestamptz   NOT NULL DEFAULT now(),
    decided_by      uuid,
    decided_at      timestamptz,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, account_id) REFERENCES eacp.budget_accounts (tenant_id, id),
    FOREIGN KEY (tenant_id, proposed_by) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, decided_by) REFERENCES eacp.principals (tenant_id, id),
    CHECK ((state = 'PROPOSED') = (decided_at IS NULL))
);
-- At most one open proposal per account.
CREATE UNIQUE INDEX budget_limit_changes_one_open ON eacp.budget_limit_changes (tenant_id, account_id)
    WHERE state = 'PROPOSED';

-- +goose StatementBegin
-- An admin changes a limit with a reason. A decrease (or no change)
-- applies at once; an increase is PROPOSED and applied by a different admin,
-- only while the limit is still the one proposed from. Any admin may reject
-- an open proposal. Accounts are locked parent first, before the journal.
CREATE FUNCTION eacp.budget_limit_changes_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a    uuid := eacp.actor();
    acct eacp.budget_accounts%ROWTYPE;
BEGIN
    PERFORM eacp.assert_role(a, 'admin');
    IF TG_OP = 'INSERT' THEN
        PERFORM eacp.require_reason(NEW.reason, 'a limit change');
        IF NEW.state <> 'PROPOSED' OR num_nonnulls(NEW.decided_by, NEW.decided_at, NEW.decision_reason) > 0 THEN
            RAISE EXCEPTION 'a limit change starts as a proposal' USING ERRCODE = '23514';
        END IF;
        acct := eacp.budget_lock_account(NEW.account_id);
        NEW.old_limit := acct.hard_limit;
        NEW.proposed_by := a;
        NEW.proposed_at := now();
        IF NEW.new_limit <= acct.hard_limit THEN
            -- Tightening needs one admin.
            NEW.state := 'APPLIED';
            NEW.decided_by := a;
            NEW.decided_at := now();
            NEW.decision_reason := NEW.reason;
        END IF;
        RETURN NEW;
    END IF;

    IF OLD.state <> 'PROPOSED' THEN
        RAISE EXCEPTION 'limit change % is %', OLD.id, OLD.state USING ERRCODE = '55000';
    END IF;
    IF (to_jsonb(NEW) - ARRAY['state', 'decision_reason', 'decided_by', 'decided_at'])
       IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['state', 'decision_reason', 'decided_by', 'decided_at']) THEN
        RAISE EXCEPTION 'deciding a limit change changes only the decision' USING ERRCODE = '55000';
    END IF;
    PERFORM eacp.require_reason(NEW.decision_reason, 'a limit decision');
    IF NEW.state = 'APPLIED' THEN
        PERFORM eacp.assert_distinct(a, OLD.proposed_by, 'the proposer');
        acct := eacp.budget_lock_account(OLD.account_id);
        IF acct.hard_limit <> OLD.old_limit THEN
            RAISE EXCEPTION 'the limit changed since this proposal' USING ERRCODE = '55000';
        END IF;
    ELSIF NEW.state <> 'REJECTED' THEN
        RAISE EXCEPTION 'a proposal is applied or rejected' USING ERRCODE = '23514';
    END IF;
    NEW.decided_by := a;
    NEW.decided_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Applying a limit (both accounts are locked, parent first): fold the
-- account, move the parent's escrow by the difference, then set the limit.
CREATE FUNCTION eacp.budget_limit_changes_apply() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    acct  eacp.budget_accounts%ROWTYPE;
    par   eacp.budget_accounts%ROWTYPE;
    delta numeric := NEW.new_limit - NEW.old_limit;
BEGIN
    IF NEW.state <> 'APPLIED' OR (TG_OP = 'UPDATE' AND OLD.state = 'APPLIED') THEN
        RETURN NULL;
    END IF;
    PERFORM eacp.budget_fold(NEW.account_id);
    SELECT * INTO STRICT acct FROM eacp.budget_accounts WHERE tenant_id = NEW.tenant_id AND id = NEW.account_id;
    IF acct.allocated + acct.reserved + acct.committed > NEW.new_limit THEN
        RAISE EXCEPTION 'the limit is below what the account has allocated, reserved and committed'
            USING ERRCODE = '55000';
    END IF;
    IF acct.parent_id IS NOT NULL THEN
        SELECT * INTO STRICT par FROM eacp.budget_accounts WHERE tenant_id = NEW.tenant_id AND id = acct.parent_id;
        IF par.allocated + par.reserved + par.committed + delta > par.hard_limit THEN
            RAISE EXCEPTION 'the parent account has no room for this limit' USING ERRCODE = '55000';
        END IF;
        UPDATE eacp.budget_accounts SET allocated = allocated + delta
        WHERE tenant_id = NEW.tenant_id AND id = acct.parent_id;
    END IF;
    UPDATE eacp.budget_accounts SET hard_limit = NEW.new_limit
    WHERE tenant_id = NEW.tenant_id AND id = NEW.account_id;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.audit_budget_limit_change() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.current_actor_id();
BEGIN
    IF TG_OP = 'UPDATE' AND NEW.state IS NOT DISTINCT FROM OLD.state THEN
        RETURN NULL;
    END IF;
    INSERT INTO eacp.audit_events (tenant_id, payload)
    VALUES (NEW.tenant_id, convert_to(jsonb_build_object(
        'v', 1,
        'actor', jsonb_build_object(
            'kind', CASE WHEN a IS NULL THEN 'system' ELSE 'principal' END,
            'id', COALESCE(a, '00000000-0000-0000-0000-000000000000'::uuid)),
        'action', 'budget.limit_' || lower(NEW.state),
        'subject', jsonb_build_object('type', 'budget_limit_change', 'id', NEW.id),
        'reason', COALESCE(NEW.decision_reason, NEW.reason),
        'data', jsonb_strip_nulls(jsonb_build_object(
            'account_id', NEW.account_id,
            'old_limit', NEW.old_limit,
            'new_limit', NEW.new_limit,
            'proposed_by', NEW.proposed_by,
            'decided_by', NEW.decided_by)))::text, 'UTF8'));
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER budget_limit_changes_guard BEFORE INSERT OR UPDATE ON eacp.budget_limit_changes
    FOR EACH ROW EXECUTE FUNCTION eacp.budget_limit_changes_guard();
CREATE TRIGGER b_apply AFTER INSERT OR UPDATE ON eacp.budget_limit_changes
    FOR EACH ROW EXECUTE FUNCTION eacp.budget_limit_changes_apply();
CREATE TRIGGER zz_audit AFTER INSERT OR UPDATE ON eacp.budget_limit_changes
    FOR EACH ROW EXECUTE FUNCTION eacp.audit_budget_limit_change();

-- ---------------------------------------------------- RLS and privileges

-- +goose StatementBegin
DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['budget_accounts', 'budget_reservations', 'budget_limit_changes']
    LOOP
        EXECUTE format('ALTER TABLE eacp.%I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE eacp.%I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('CREATE POLICY tenant_isolation ON eacp.%I USING (tenant_id = eacp.current_tenant_id())', t);
        EXECUTE format('REVOKE UPDATE ON eacp.%I FROM eacp_app', t);
    END LOOP;
END
$$;
-- +goose StatementEnd

GRANT UPDATE (hard_limit, allocated, reserved, committed) ON eacp.budget_accounts TO eacp_app;
GRANT UPDATE (state, committed_amount, settled_at, settle_reason, folded_at) ON eacp.budget_reservations TO eacp_app;
GRANT UPDATE (state, decision_reason, decided_by, decided_at) ON eacp.budget_limit_changes TO eacp_app;

-- +goose Down

DROP TABLE eacp.budget_limit_changes;
DROP FUNCTION eacp.audit_budget_limit_change();
DROP FUNCTION eacp.budget_limit_changes_apply();
DROP FUNCTION eacp.budget_limit_changes_guard();

DROP TRIGGER actions_budget ON eacp.actions;
DROP FUNCTION eacp.actions_budget();
DROP FUNCTION eacp.budget_reserve(uuid, uuid);
DROP FUNCTION eacp.budget_fold(uuid);

DROP TABLE eacp.budget_reservations;
DROP FUNCTION eacp.audit_budget_reservation();
DROP FUNCTION eacp.budget_reservations_apply();
DROP FUNCTION eacp.budget_reservations_guard();

DROP FUNCTION eacp.budget_lock_account(uuid);
DROP TABLE eacp.budget_accounts;
DROP FUNCTION eacp.budget_accounts_guard();

DROP FUNCTION eacp.action_cost(eacp.tool_contracts, jsonb);
ALTER TABLE eacp.tool_contracts
    DROP CONSTRAINT cost_declared,
    DROP COLUMN cost_unit_field,
    DROP COLUMN cost_amount_field,
    DROP COLUMN cost_fixed,
    DROP COLUMN cost_unit;
