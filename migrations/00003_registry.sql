-- Registry, identity and capability (ADR-003, Slice A Phase 2).
--
-- PostgreSQL is the backstop for every registry rule (ADR-003 §8):
--   * RLS (ENABLE + FORCE) on every table, per the 00001 convention.
--   * Composite foreign keys (tenant_id, id): no row can reference another
--     tenant's row, even with a leaked id.
--   * Column-level UPDATE grants: only lifecycle, pointer, approval and
--     revocation columns are writable by eacp_app; nothing is deletable.
--   * Triggers attribute and authorise every change using the transaction's
--     actor (app.actor_id, set by storage.SetActor). They overwrite every
--     *_by / *_at column, so a client cannot misattribute a change.
--   * A missing actor is accepted only from the schema owner (bootstrap,
--     the break-glass trust root).
--
-- Error codes raised by triggers:
--   42501 insufficient_privilege   missing role, two-person or SoD violation
--   55000 object_not_in_prerequisite_state   illegal transition, frozen row
--   23514 check_violation          invalid value (e.g. missing reason)
--   23503 foreign_key_violation    reference to a missing row

-- +goose Up

-- ---------------------------------------------------------------- helpers

CREATE FUNCTION eacp.current_actor_id() RETURNS uuid
    LANGUAGE sql STABLE
    AS $$ SELECT NULLIF(current_setting('app.actor_id', true), '')::uuid $$;

CREATE FUNCTION eacp.is_schema_owner() RETURNS boolean
    LANGUAGE sql STABLE
    AS $$ SELECT pg_has_role(current_user, n.nspowner, 'MEMBER')
          FROM pg_namespace n WHERE n.nspname = 'eacp' $$;

-- +goose StatementBegin
-- eacp.actor() returns the transaction's actor, or NULL for the schema owner
-- (bootstrap). Anyone else without an actor is refused.
CREATE FUNCTION eacp.actor() RETURNS uuid
    LANGUAGE plpgsql STABLE
    AS $$
DECLARE
    a uuid := eacp.current_actor_id();
BEGIN
    IF a IS NULL AND NOT eacp.is_schema_owner() THEN
        RAISE EXCEPTION 'no actor set for this transaction' USING ERRCODE = '42501';
    END IF;
    RETURN a;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- eacp.assert_role fails unless actor is an enabled principal of the current
-- tenant holding an approved, unrevoked grant of one of roles. The rows are
-- read FOR SHARE, so a concurrent revocation or disable serialises with the
-- change being authorised. A NULL actor is the schema owner (see actor()).
CREATE FUNCTION eacp.assert_role(actor uuid, VARIADIC roles text[]) RETURNS void
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF actor IS NULL THEN
        RETURN;
    END IF;
    PERFORM 1
    FROM eacp.principals p
    JOIN eacp.role_grants g ON g.tenant_id = p.tenant_id AND g.principal_id = p.id
    WHERE p.tenant_id = eacp.current_tenant_id()
      AND p.id = actor
      AND p.disabled_at IS NULL
      AND g.role = ANY (roles)
      AND g.approved_at IS NOT NULL
      AND g.revoked_at IS NULL
    FOR SHARE OF p, g;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'principal % has no effective role in %', actor, roles USING ERRCODE = '42501';
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.assert_distinct(actor uuid, other uuid, what text) RETURNS void
    LANGUAGE plpgsql IMMUTABLE
    AS $$
BEGIN
    IF actor IS NOT NULL AND actor = other THEN
        RAISE EXCEPTION 'two-person rule: the actor cannot also be %', what USING ERRCODE = '42501';
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.require_reason(reason text, what text) RETURNS void
    LANGUAGE plpgsql IMMUTABLE
    AS $$
BEGIN
    IF reason IS NULL OR btrim(reason) = '' THEN
        RAISE EXCEPTION '% requires a reason', what USING ERRCODE = '23514';
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Revocation-style columns (revoked_at/removed_at/disabled_at + reason) may be
-- set once and never changed again. Returns true when this UPDATE sets them.
CREATE FUNCTION eacp.set_once(old_at timestamptz, new_at timestamptz, old_reason text, new_reason text, what text)
    RETURNS boolean
    LANGUAGE plpgsql IMMUTABLE
    AS $$
BEGIN
    IF old_at IS NOT NULL THEN
        IF new_at IS DISTINCT FROM old_at OR new_reason IS DISTINCT FROM old_reason THEN
            RAISE EXCEPTION '% is permanent', what USING ERRCODE = '55000';
        END IF;
        RETURN false;
    END IF;
    IF new_at IS NULL THEN
        IF new_reason IS NOT NULL THEN
            RAISE EXCEPTION '% reason given without %', what, what USING ERRCODE = '23514';
        END IF;
        RETURN false;
    END IF;
    PERFORM eacp.require_reason(new_reason, what);
    RETURN true;
END
$$;
-- +goose StatementEnd

-- ------------------------------------------------------------- principals

CREATE TABLE eacp.principals (
    tenant_id      uuid        NOT NULL REFERENCES eacp.tenants (id),
    id             uuid        NOT NULL DEFAULT gen_random_uuid(),
    kind           text        NOT NULL CHECK (kind IN ('human', 'service')),
    name           text        NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9-]{1,62}$'),
    subject        text        CHECK (subject = lower(subject) AND length(subject) BETWEEN 3 AND 320),
    display_name   text        NOT NULL CHECK (btrim(display_name) <> ''),
    created_by     uuid,
    created_at     timestamptz NOT NULL DEFAULT now(),
    disabled_by    uuid,
    disabled_at    timestamptz,
    disable_reason text,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, name),
    UNIQUE (tenant_id, subject),
    CHECK ((kind = 'human') = (subject IS NOT NULL)),
    FOREIGN KEY (tenant_id, created_by) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, disabled_by) REFERENCES eacp.principals (tenant_id, id)
);

CREATE TABLE eacp.role_grants (
    tenant_id     uuid        NOT NULL,
    id            uuid        NOT NULL DEFAULT gen_random_uuid(),
    principal_id  uuid        NOT NULL,
    role          text        NOT NULL CHECK (role IN
                      ('admin', 'registry_editor', 'registry_approver', 'operator', 'approver', 'auditor')),
    proposed_by   uuid,
    proposed_at   timestamptz NOT NULL DEFAULT now(),
    approved_by   uuid,
    approved_at   timestamptz,
    revoked_by    uuid,
    revoked_at    timestamptz,
    revoke_reason text,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, principal_id) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, proposed_by) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, approved_by) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, revoked_by) REFERENCES eacp.principals (tenant_id, id)
);
CREATE UNIQUE INDEX role_grants_one_live ON eacp.role_grants (tenant_id, principal_id, role)
    WHERE revoked_at IS NULL;

-- +goose StatementBegin
CREATE FUNCTION eacp.principals_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.actor();
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM eacp.assert_role(a, 'admin');
        NEW.created_by := a;
        NEW.created_at := now();
        IF NEW.disabled_at IS NOT NULL OR NEW.disabled_by IS NOT NULL OR NEW.disable_reason IS NOT NULL THEN
            RAISE EXCEPTION 'a principal cannot be created disabled' USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;
    IF eacp.set_once(OLD.disabled_at, NEW.disabled_at, OLD.disable_reason, NEW.disable_reason, 'disabling a principal') THEN
        PERFORM eacp.assert_role(a, 'admin');
        NEW.disabled_by := a;
        NEW.disabled_at := now();
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.role_grants_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.actor();
    grantee eacp.principals%ROWTYPE;
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM eacp.assert_role(a, 'admin');
        PERFORM eacp.assert_distinct(a, NEW.principal_id, 'the grantee');
        SELECT * INTO grantee FROM eacp.principals
        WHERE tenant_id = NEW.tenant_id AND id = NEW.principal_id FOR SHARE;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'unknown principal %', NEW.principal_id USING ERRCODE = '23503';
        END IF;
        IF grantee.disabled_at IS NOT NULL THEN
            RAISE EXCEPTION 'principal % is disabled', NEW.principal_id USING ERRCODE = '55000';
        END IF;
        -- Slice A: a service principal may only read the audit journal. A
        -- human who controls a service principal must not be able to author
        -- as the service and approve as themselves (ADR-003 §1).
        IF grantee.kind = 'service' AND NEW.role <> 'auditor' THEN
            RAISE EXCEPTION 'role % is human-only', NEW.role USING ERRCODE = '42501';
        END IF;
        IF a IS NOT NULL AND (NEW.approved_at IS NOT NULL OR NEW.approved_by IS NOT NULL) THEN
            RAISE EXCEPTION 'a grant must be approved by a second admin' USING ERRCODE = '42501';
        END IF;
        IF NEW.revoked_at IS NOT NULL OR NEW.revoked_by IS NOT NULL OR NEW.revoke_reason IS NOT NULL THEN
            RAISE EXCEPTION 'a grant cannot be created revoked' USING ERRCODE = '23514';
        END IF;
        NEW.proposed_by := a;
        NEW.proposed_at := now();
        IF NEW.approved_at IS NOT NULL THEN  -- bootstrap only
            NEW.approved_at := now();
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.principal_id IS DISTINCT FROM OLD.principal_id OR NEW.role IS DISTINCT FROM OLD.role
       OR NEW.proposed_by IS DISTINCT FROM OLD.proposed_by OR NEW.proposed_at IS DISTINCT FROM OLD.proposed_at THEN
        RAISE EXCEPTION 'a grant''s subject is immutable' USING ERRCODE = '55000';
    END IF;

    IF eacp.set_once(OLD.revoked_at, NEW.revoked_at, OLD.revoke_reason, NEW.revoke_reason, 'revocation') THEN
        IF NEW.approved_at IS DISTINCT FROM OLD.approved_at THEN
            RAISE EXCEPTION 'approve and revoke separately' USING ERRCODE = '55000';
        END IF;
        PERFORM eacp.assert_role(a, 'admin');
        NEW.revoked_by := a;
        NEW.revoked_at := now();
        RETURN NEW;
    END IF;

    IF NEW.approved_at IS DISTINCT FROM OLD.approved_at OR NEW.approved_by IS DISTINCT FROM OLD.approved_by THEN
        IF OLD.approved_at IS NOT NULL THEN
            RAISE EXCEPTION 'grant already approved' USING ERRCODE = '55000';
        END IF;
        IF OLD.revoked_at IS NOT NULL THEN
            RAISE EXCEPTION 'grant is revoked' USING ERRCODE = '55000';
        END IF;
        IF NEW.approved_at IS NULL THEN
            RAISE EXCEPTION 'approval requires approved_at' USING ERRCODE = '23514';
        END IF;
        PERFORM eacp.assert_role(a, 'admin');
        PERFORM eacp.assert_distinct(a, OLD.proposed_by, 'the proposer');
        PERFORM eacp.assert_distinct(a, OLD.principal_id, 'the grantee');
        -- Re-check at approval time: the grantee may have been disabled.
        PERFORM 1 FROM eacp.principals
        WHERE tenant_id = NEW.tenant_id AND id = NEW.principal_id AND disabled_at IS NULL FOR SHARE;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'principal % is disabled', NEW.principal_id USING ERRCODE = '55000';
        END IF;
        NEW.approved_by := a;
        NEW.approved_at := now();
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER principals_guard BEFORE INSERT OR UPDATE ON eacp.principals
    FOR EACH ROW EXECUTE FUNCTION eacp.principals_guard();
CREATE TRIGGER role_grants_guard BEFORE INSERT OR UPDATE ON eacp.role_grants
    FOR EACH ROW EXECUTE FUNCTION eacp.role_grants_guard();

-- ----------------------------------------------------------------- groups

CREATE TABLE eacp.groups (
    tenant_id    uuid        NOT NULL REFERENCES eacp.tenants (id),
    id           uuid        NOT NULL DEFAULT gen_random_uuid(),
    name         text        NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9-]{1,62}$'),
    display_name text        NOT NULL CHECK (btrim(display_name) <> ''),
    created_by   uuid,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, name),
    FOREIGN KEY (tenant_id, created_by) REFERENCES eacp.principals (tenant_id, id)
);

CREATE TABLE eacp.group_memberships (
    tenant_id     uuid        NOT NULL,
    id            uuid        NOT NULL DEFAULT gen_random_uuid(),
    group_id      uuid        NOT NULL,
    principal_id  uuid        NOT NULL,
    added_by      uuid,
    added_at      timestamptz NOT NULL DEFAULT now(),
    removed_by    uuid,
    removed_at    timestamptz,
    remove_reason text,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, group_id) REFERENCES eacp.groups (tenant_id, id),
    FOREIGN KEY (tenant_id, principal_id) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, added_by) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, removed_by) REFERENCES eacp.principals (tenant_id, id)
);
CREATE UNIQUE INDEX group_memberships_one_live ON eacp.group_memberships (tenant_id, group_id, principal_id)
    WHERE removed_at IS NULL;

-- +goose StatementBegin
CREATE FUNCTION eacp.groups_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.actor();
BEGIN
    PERFORM eacp.assert_role(a, 'admin');
    NEW.created_by := a;
    NEW.created_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.group_memberships_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.actor();
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM eacp.assert_role(a, 'admin');
        IF NEW.removed_at IS NOT NULL OR NEW.removed_by IS NOT NULL OR NEW.remove_reason IS NOT NULL THEN
            RAISE EXCEPTION 'a membership cannot be created removed' USING ERRCODE = '23514';
        END IF;
        NEW.added_by := a;
        NEW.added_at := now();
        RETURN NEW;
    END IF;
    IF eacp.set_once(OLD.removed_at, NEW.removed_at, OLD.remove_reason, NEW.remove_reason, 'removal') THEN
        PERFORM eacp.assert_role(a, 'admin');
        NEW.removed_by := a;
        NEW.removed_at := now();
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER groups_guard BEFORE INSERT ON eacp.groups
    FOR EACH ROW EXECUTE FUNCTION eacp.groups_guard();
CREATE TRIGGER group_memberships_guard BEFORE INSERT OR UPDATE ON eacp.group_memberships
    FOR EACH ROW EXECUTE FUNCTION eacp.group_memberships_guard();

-- ------------------------------------------------- connectors and tools

CREATE TABLE eacp.connectors (
    tenant_id  uuid        NOT NULL REFERENCES eacp.tenants (id),
    id         uuid        NOT NULL DEFAULT gen_random_uuid(),
    name       text        NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9-]{1,62}$'),
    protocol   text        NOT NULL CHECK (protocol IN ('http')),
    endpoint   text        NOT NULL CHECK (endpoint ~ '^https?://[^[:space:]]+$' AND length(endpoint) <= 2048),
    -- A name the execution worker resolves together with the tenant id;
    -- never a secret value (ADR-003 §4).
    secret_ref text        NOT NULL CHECK (secret_ref ~ '^[a-z0-9][a-z0-9._/-]{0,127}$'),
    created_by uuid,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, name),
    FOREIGN KEY (tenant_id, created_by) REFERENCES eacp.principals (tenant_id, id)
);

CREATE TABLE eacp.tools (
    tenant_id           uuid        NOT NULL,
    id                  uuid        NOT NULL DEFAULT gen_random_uuid(),
    connector_id        uuid        NOT NULL,
    name                text        NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9_-]{0,62}$'),
    active_contract_id  uuid,
    contract_changed_by uuid,
    contract_changed_at timestamptz,
    created_by          uuid,
    created_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, connector_id, name),
    FOREIGN KEY (tenant_id, connector_id) REFERENCES eacp.connectors (tenant_id, id),
    FOREIGN KEY (tenant_id, created_by) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, contract_changed_by) REFERENCES eacp.principals (tenant_id, id)
);

CREATE TABLE eacp.tool_contracts (
    tenant_id                  uuid        NOT NULL,
    id                         uuid        NOT NULL DEFAULT gen_random_uuid(),
    tool_id                    uuid        NOT NULL,
    version                    integer     NOT NULL,
    side_effects               text[]      NOT NULL,
    idempotency_mode           text        NOT NULL,
    idempotency_key_field      text,
    correlation_field          text,
    reconciliation_lookup      text        NOT NULL,
    reconciliation_consistency text        NOT NULL,
    proof_standard             text        NOT NULL,
    no_effect_errors           text[]      NOT NULL DEFAULT '{}',
    credential_custody         text        NOT NULL DEFAULT 'worker',
    max_attempts               integer     NOT NULL,
    timeout_ms                 integer     CHECK (timeout_ms > 0),
    concurrency_group          text        CHECK (concurrency_group ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    max_inflight               integer     CHECK (max_inflight > 0),
    data_sensitivity           text        CHECK (data_sensitivity IN ('public', 'internal', 'confidential', 'restricted')),
    fingerprint                bytea       NOT NULL,
    created_by                 uuid,
    created_at                 timestamptz NOT NULL DEFAULT now(),
    revoked_by                 uuid,
    revoked_at                 timestamptz,
    revoke_reason              text,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, tool_id, version),
    FOREIGN KEY (tenant_id, tool_id) REFERENCES eacp.tools (tenant_id, id),
    FOREIGN KEY (tenant_id, created_by) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, revoked_by) REFERENCES eacp.principals (tenant_id, id),

    -- §31/§32 and ADR-003 §4.
    CONSTRAINT side_effects_known CHECK (
        cardinality(side_effects) >= 1
        AND side_effects <@ ARRAY['READ_ONLY', 'REVERSIBLE_WRITE', 'IRREVERSIBLE_WRITE',
                                  'EXTERNAL_COMMUNICATION', 'FINANCIAL', 'ADMINISTRATIVE']),
    CONSTRAINT read_only_alone CHECK (NOT ('READ_ONLY' = ANY (side_effects)) OR cardinality(side_effects) = 1),
    CONSTRAINT idempotency_mode_known CHECK (idempotency_mode IN ('native', 'correlation_only', 'none')),
    CONSTRAINT native_needs_key CHECK (idempotency_mode <> 'native' OR idempotency_key_field IS NOT NULL),
    CONSTRAINT correlation_needs_field CHECK (idempotency_mode <> 'correlation_only' OR correlation_field IS NOT NULL),
    CONSTRAINT field_names_not_blank CHECK (
        (idempotency_key_field IS NULL OR btrim(idempotency_key_field) <> '')
        AND (correlation_field IS NULL OR btrim(correlation_field) <> '')),
    CONSTRAINT lookup_known CHECK (reconciliation_lookup IN ('by_operation_key', 'none')),
    CONSTRAINT consistency_known CHECK (reconciliation_consistency IN ('strong', 'eventual', 'none')),
    CONSTRAINT consistency_iff_lookup CHECK ((reconciliation_lookup = 'none') = (reconciliation_consistency = 'none')),
    CONSTRAINT proof_known CHECK (proof_standard IN ('authoritative', 'best_effort', 'none')),
    CONSTRAINT authoritative_needs_strong_lookup CHECK (
        proof_standard <> 'authoritative'
        OR (reconciliation_lookup = 'by_operation_key' AND reconciliation_consistency = 'strong')),
    CONSTRAINT no_lookup_no_proof CHECK (reconciliation_lookup <> 'none' OR proof_standard = 'none'),
    CONSTRAINT worker_custody CHECK (credential_custody = 'worker'),
    CONSTRAINT attempts_bounded CHECK (max_attempts BETWEEN 1 AND 10),
    CONSTRAINT unsafe_writes_single_attempt CHECK (
        side_effects = ARRAY['READ_ONLY'] OR idempotency_mode <> 'none' OR max_attempts = 1),
    CONSTRAINT fingerprint_sha256 CHECK (octet_length(fingerprint) = 32)
);

ALTER TABLE eacp.tools
    ADD FOREIGN KEY (tenant_id, active_contract_id) REFERENCES eacp.tool_contracts (tenant_id, id);

-- The identity a contract certifies (ADR-003 §4). Connectors and tools are
-- immutable, so this only changes if someone edits them as the owner.
CREATE FUNCTION eacp.tool_fingerprint(p_tenant uuid, p_tool uuid) RETURNS bytea
    LANGUAGE sql STABLE
    AS $$
    SELECT sha256(convert_to(concat_ws(E'\n', 'eacp-tool-v1',
                  c.protocol, c.endpoint, c.secret_ref, c.name, t.name), 'UTF8'))
    FROM eacp.tools t
    JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
    WHERE t.tenant_id = p_tenant AND t.id = p_tool
$$;

-- +goose StatementBegin
CREATE FUNCTION eacp.connectors_guard() RETURNS trigger
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

-- +goose StatementBegin
CREATE FUNCTION eacp.tools_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.actor();
    c eacp.tool_contracts%ROWTYPE;
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM eacp.assert_role(a, 'registry_editor');
        IF NEW.active_contract_id IS NOT NULL THEN
            RAISE EXCEPTION 'a tool is registered without a contract' USING ERRCODE = '55000';
        END IF;
        NEW.created_by := a;
        NEW.created_at := now();
        NEW.contract_changed_by := NULL;
        NEW.contract_changed_at := NULL;
        RETURN NEW;
    END IF;

    IF NEW.active_contract_id IS NOT DISTINCT FROM OLD.active_contract_id THEN
        RETURN NEW;
    END IF;
    IF NEW.active_contract_id IS NULL THEN
        RAISE EXCEPTION 'the active contract cannot be cleared; revoke it instead' USING ERRCODE = '55000';
    END IF;
    SELECT * INTO c FROM eacp.tool_contracts
    WHERE tenant_id = NEW.tenant_id AND id = NEW.active_contract_id FOR SHARE;
    IF NOT FOUND OR c.tool_id <> NEW.id THEN
        RAISE EXCEPTION 'contract % does not belong to tool %', NEW.active_contract_id, NEW.id USING ERRCODE = '55000';
    END IF;
    IF c.revoked_at IS NOT NULL THEN
        RAISE EXCEPTION 'contract % is revoked', c.id USING ERRCODE = '55000';
    END IF;
    PERFORM eacp.assert_role(a, 'registry_approver');
    PERFORM eacp.assert_distinct(a, c.created_by, 'the contract author');
    NEW.contract_changed_by := a;
    NEW.contract_changed_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.tool_contracts_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.actor();
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM eacp.assert_role(a, 'registry_editor', 'registry_approver');
        IF NEW.revoked_at IS NOT NULL OR NEW.revoked_by IS NOT NULL OR NEW.revoke_reason IS NOT NULL THEN
            RAISE EXCEPTION 'a contract cannot be created revoked' USING ERRCODE = '23514';
        END IF;
        -- Serialise version numbering per tool on the tool row.
        PERFORM 1 FROM eacp.tools WHERE tenant_id = NEW.tenant_id AND id = NEW.tool_id FOR NO KEY UPDATE;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'unknown tool %', NEW.tool_id USING ERRCODE = '23503';
        END IF;
        SELECT COALESCE(max(version), 0) + 1 INTO NEW.version
        FROM eacp.tool_contracts WHERE tenant_id = NEW.tenant_id AND tool_id = NEW.tool_id;
        NEW.side_effects     := ARRAY(SELECT DISTINCT e FROM unnest(NEW.side_effects) e ORDER BY e);
        NEW.no_effect_errors := ARRAY(SELECT DISTINCT e FROM unnest(NEW.no_effect_errors) e ORDER BY e);
        NEW.fingerprint      := eacp.tool_fingerprint(NEW.tenant_id, NEW.tool_id);
        NEW.created_by       := a;
        NEW.created_at       := now();
        RETURN NEW;
    END IF;

    IF eacp.set_once(OLD.revoked_at, NEW.revoked_at, OLD.revoke_reason, NEW.revoke_reason, 'contract revocation') THEN
        PERFORM eacp.assert_role(a, 'registry_approver', 'operator');
        NEW.revoked_by := a;
        NEW.revoked_at := now();
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER connectors_guard BEFORE INSERT ON eacp.connectors
    FOR EACH ROW EXECUTE FUNCTION eacp.connectors_guard();
CREATE TRIGGER tools_guard BEFORE INSERT OR UPDATE ON eacp.tools
    FOR EACH ROW EXECUTE FUNCTION eacp.tools_guard();
CREATE TRIGGER tool_contracts_guard BEFORE INSERT OR UPDATE ON eacp.tool_contracts
    FOR EACH ROW EXECUTE FUNCTION eacp.tool_contracts_guard();

-- ------------------------------------------------------ agents and versions

CREATE TABLE eacp.agents (
    tenant_id          uuid        NOT NULL REFERENCES eacp.tenants (id),
    id                 uuid        NOT NULL DEFAULT gen_random_uuid(),
    name               text        NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9-]{1,62}$'),
    display_name       text        NOT NULL CHECK (btrim(display_name) <> ''),
    environment        text        NOT NULL CHECK (environment IN ('development', 'staging', 'production')),
    risk_class         text        NOT NULL CHECK (risk_class IN ('low', 'medium', 'high', 'critical')),
    owner_principal_id uuid,
    owner_group_id     uuid,
    created_by         uuid,
    created_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, name),
    -- Every agent has exactly one owner, in every environment (ADR-003 §2).
    CONSTRAINT exactly_one_owner CHECK (num_nonnulls(owner_principal_id, owner_group_id) = 1),
    FOREIGN KEY (tenant_id, owner_principal_id) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, owner_group_id) REFERENCES eacp.groups (tenant_id, id),
    FOREIGN KEY (tenant_id, created_by) REFERENCES eacp.principals (tenant_id, id)
);

CREATE TABLE eacp.agent_versions (
    tenant_id            uuid        NOT NULL,
    id                   uuid        NOT NULL DEFAULT gen_random_uuid(),
    agent_id             uuid        NOT NULL,
    version              integer     NOT NULL,
    runtime              text        NOT NULL CHECK (btrim(runtime) <> ''),
    code_ref             text        NOT NULL CHECK (btrim(code_ref) <> ''),
    state                text        NOT NULL DEFAULT 'REGISTERED' CHECK (state IN
                             ('REGISTERED', 'ACTIVE', 'SUSPENDED', 'QUARANTINED', 'RETIRED', 'REVOKED')),
    active_allowlist_id  uuid,
    allowlist_changed_by uuid,
    allowlist_changed_at timestamptz,
    created_by           uuid,
    created_at           timestamptz NOT NULL DEFAULT now(),
    state_changed_by     uuid,
    state_changed_at     timestamptz,
    state_reason         text,
    quarantined_by       uuid,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, agent_id, version),
    FOREIGN KEY (tenant_id, agent_id) REFERENCES eacp.agents (tenant_id, id),
    FOREIGN KEY (tenant_id, created_by) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, state_changed_by) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, allowlist_changed_by) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, quarantined_by) REFERENCES eacp.principals (tenant_id, id)
);
CREATE UNIQUE INDEX agent_versions_one_active ON eacp.agent_versions (tenant_id, agent_id)
    WHERE state = 'ACTIVE';

CREATE TABLE eacp.agent_allowlists (
    tenant_id        uuid        NOT NULL,
    id               uuid        NOT NULL DEFAULT gen_random_uuid(),
    agent_version_id uuid        NOT NULL,
    tool_ids         uuid[]      NOT NULL,
    created_by       uuid,
    created_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, agent_version_id) REFERENCES eacp.agent_versions (tenant_id, id),
    FOREIGN KEY (tenant_id, created_by) REFERENCES eacp.principals (tenant_id, id)
);

ALTER TABLE eacp.agent_versions
    ADD FOREIGN KEY (tenant_id, active_allowlist_id) REFERENCES eacp.agent_allowlists (tenant_id, id);

-- +goose StatementBegin
CREATE FUNCTION eacp.agents_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.actor();
BEGIN
    PERFORM eacp.assert_role(a, 'registry_editor');
    IF NEW.owner_principal_id IS NOT NULL THEN
        PERFORM 1 FROM eacp.principals
        WHERE tenant_id = NEW.tenant_id AND id = NEW.owner_principal_id AND disabled_at IS NULL FOR SHARE;
        IF NOT FOUND AND EXISTS (SELECT 1 FROM eacp.principals
                                 WHERE tenant_id = NEW.tenant_id AND id = NEW.owner_principal_id) THEN
            RAISE EXCEPTION 'owner % is disabled', NEW.owner_principal_id USING ERRCODE = '55000';
        END IF;
    END IF;
    NEW.created_by := a;
    NEW.created_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.agent_versions_guard() RETURNS trigger
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

-- +goose StatementBegin
CREATE FUNCTION eacp.agent_allowlists_guard() RETURNS trigger
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

CREATE TRIGGER agents_guard BEFORE INSERT ON eacp.agents
    FOR EACH ROW EXECUTE FUNCTION eacp.agents_guard();
CREATE TRIGGER agent_versions_guard BEFORE INSERT OR UPDATE ON eacp.agent_versions
    FOR EACH ROW EXECUTE FUNCTION eacp.agent_versions_guard();
CREATE TRIGGER agent_allowlists_guard BEFORE INSERT ON eacp.agent_allowlists
    FOR EACH ROW EXECUTE FUNCTION eacp.agent_allowlists_guard();

-- ------------------------------------------------------------ credentials

CREATE TABLE eacp.credentials (
    tenant_id        uuid        NOT NULL,
    id               uuid        NOT NULL,  -- chosen by the key holder (bring your own key)
    kind             text        NOT NULL CHECK (kind IN ('ak', 'pk')),
    principal_id     uuid,
    agent_version_id uuid,
    secret_hash      bytea       NOT NULL CHECK (octet_length(secret_hash) = 32),
    proposed_by      uuid,
    proposed_at      timestamptz NOT NULL DEFAULT now(),
    approved_by      uuid,
    approved_at      timestamptz,
    expires_at       timestamptz NOT NULL,
    revoked_by       uuid,
    revoked_at       timestamptz,
    revoke_reason    text,
    PRIMARY KEY (tenant_id, id),
    CONSTRAINT subject_matches_kind CHECK (
        (kind = 'pk' AND principal_id IS NOT NULL AND agent_version_id IS NULL)
        OR (kind = 'ak' AND agent_version_id IS NOT NULL AND principal_id IS NULL)),
    FOREIGN KEY (tenant_id, principal_id) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, agent_version_id) REFERENCES eacp.agent_versions (tenant_id, id),
    FOREIGN KEY (tenant_id, proposed_by) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, approved_by) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, revoked_by) REFERENCES eacp.principals (tenant_id, id)
);

-- +goose StatementBegin
CREATE FUNCTION eacp.credentials_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.actor();
    v_state text;
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.kind = 'pk' THEN
            PERFORM eacp.assert_role(a, 'admin');
        ELSE
            PERFORM eacp.assert_role(a, 'registry_editor', 'registry_approver');
            SELECT state INTO v_state FROM eacp.agent_versions
            WHERE tenant_id = NEW.tenant_id AND id = NEW.agent_version_id FOR SHARE;
            IF v_state IN ('RETIRED', 'REVOKED') THEN
                RAISE EXCEPTION 'version % is % (terminal)', NEW.agent_version_id, v_state USING ERRCODE = '55000';
            END IF;
        END IF;
        IF a IS NOT NULL AND (NEW.approved_at IS NOT NULL OR NEW.approved_by IS NOT NULL) THEN
            RAISE EXCEPTION 'a credential must be approved by a second person' USING ERRCODE = '42501';
        END IF;
        IF NEW.revoked_at IS NOT NULL OR NEW.revoked_by IS NOT NULL OR NEW.revoke_reason IS NOT NULL THEN
            RAISE EXCEPTION 'a credential cannot be created revoked' USING ERRCODE = '23514';
        END IF;
        IF NEW.expires_at <= now() OR NEW.expires_at > now() + interval '90 days' THEN
            RAISE EXCEPTION 'credential expiry must be in the future and at most 90 days away' USING ERRCODE = '23514';
        END IF;
        NEW.proposed_by := a;
        NEW.proposed_at := now();
        IF NEW.approved_at IS NOT NULL THEN  -- bootstrap only
            NEW.approved_at := now();
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.kind IS DISTINCT FROM OLD.kind OR NEW.principal_id IS DISTINCT FROM OLD.principal_id
       OR NEW.agent_version_id IS DISTINCT FROM OLD.agent_version_id OR NEW.secret_hash IS DISTINCT FROM OLD.secret_hash
       OR NEW.expires_at IS DISTINCT FROM OLD.expires_at OR NEW.proposed_by IS DISTINCT FROM OLD.proposed_by THEN
        RAISE EXCEPTION 'a credential''s subject, hash and expiry are immutable' USING ERRCODE = '55000';
    END IF;

    IF eacp.set_once(OLD.revoked_at, NEW.revoked_at, OLD.revoke_reason, NEW.revoke_reason, 'credential revocation') THEN
        IF NEW.approved_at IS DISTINCT FROM OLD.approved_at THEN
            RAISE EXCEPTION 'approve and revoke separately' USING ERRCODE = '55000';
        END IF;
        IF NEW.kind = 'pk' THEN
            PERFORM eacp.assert_role(a, 'admin');
        ELSE
            PERFORM eacp.assert_role(a, 'registry_approver', 'operator');
        END IF;
        NEW.revoked_by := a;
        NEW.revoked_at := now();
        RETURN NEW;
    END IF;

    IF NEW.approved_at IS DISTINCT FROM OLD.approved_at OR NEW.approved_by IS DISTINCT FROM OLD.approved_by THEN
        IF OLD.approved_at IS NOT NULL THEN
            RAISE EXCEPTION 'credential already approved' USING ERRCODE = '55000';
        END IF;
        IF OLD.revoked_at IS NOT NULL OR OLD.expires_at <= now() THEN
            RAISE EXCEPTION 'credential is revoked or expired' USING ERRCODE = '55000';
        END IF;
        IF NEW.approved_at IS NULL THEN
            RAISE EXCEPTION 'approval requires approved_at' USING ERRCODE = '23514';
        END IF;
        IF NEW.kind = 'pk' THEN
            PERFORM eacp.assert_role(a, 'admin');
            PERFORM eacp.assert_distinct(a, OLD.principal_id, 'the credential holder');
        ELSE
            PERFORM eacp.assert_role(a, 'registry_approver');
        END IF;
        PERFORM eacp.assert_distinct(a, OLD.proposed_by, 'the proposer');
        NEW.approved_by := a;
        NEW.approved_at := now();
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER credentials_guard BEFORE INSERT OR UPDATE ON eacp.credentials
    FOR EACH ROW EXECUTE FUNCTION eacp.credentials_guard();

-- ------------------------------------------------------------------ audit

-- +goose StatementBegin
-- Every registry change is appended to the audit journal by the database
-- itself, in the same transaction, so no code path can change the registry
-- without an audit event (ADR-003 §7). Credential hashes are never recorded.
CREATE FUNCTION eacp.audit_row_change() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a       uuid  := eacp.current_actor_id();
    row_new jsonb := to_jsonb(NEW) - 'secret_hash';
    row_old jsonb;
    changed jsonb;
    why     text;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        row_old := to_jsonb(OLD) - 'secret_hash';
        SELECT COALESCE(jsonb_object_agg(e.key, jsonb_build_object('from', row_old -> e.key, 'to', e.value)), '{}')
        INTO changed
        FROM jsonb_each(row_new) AS e
        WHERE row_old -> e.key IS DISTINCT FROM e.value;
        IF changed = '{}'::jsonb THEN
            RETURN NULL;
        END IF;
    ELSE
        changed := row_new;
    END IF;

    why := COALESCE(
        CASE WHEN changed ? 'revoked_at' THEN row_new ->> 'revoke_reason' END,
        CASE WHEN changed ? 'disabled_at' THEN row_new ->> 'disable_reason' END,
        CASE WHEN changed ? 'removed_at' THEN row_new ->> 'remove_reason' END,
        CASE WHEN changed ? 'state' THEN row_new ->> 'state_reason' END,
        TG_TABLE_NAME || ' ' || lower(TG_OP));

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

-- ------------------------------------------------ RLS and privileges

-- +goose StatementBegin
DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['principals', 'role_grants', 'groups', 'group_memberships', 'connectors',
                             'tools', 'tool_contracts', 'agents', 'agent_versions', 'agent_allowlists',
                             'credentials']
    LOOP
        EXECUTE format('ALTER TABLE eacp.%I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE eacp.%I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('CREATE POLICY tenant_isolation ON eacp.%I USING (tenant_id = eacp.current_tenant_id())', t);
        EXECUTE format('REVOKE UPDATE ON eacp.%I FROM eacp_app', t);
        EXECUTE format('CREATE TRIGGER zz_audit AFTER INSERT OR UPDATE ON eacp.%I '
                       'FOR EACH ROW EXECUTE FUNCTION eacp.audit_row_change()', t);
    END LOOP;
END
$$;
-- +goose StatementEnd

-- Only lifecycle, pointer, approval and revocation columns are writable.
-- (Triggers fill the matching *_by / *_at columns themselves.)
GRANT UPDATE (disabled_at, disable_reason)           ON eacp.principals        TO eacp_app;
GRANT UPDATE (approved_at, revoked_at, revoke_reason) ON eacp.role_grants       TO eacp_app;
GRANT UPDATE (removed_at, remove_reason)             ON eacp.group_memberships TO eacp_app;
GRANT UPDATE (active_contract_id)                    ON eacp.tools             TO eacp_app;
GRANT UPDATE (revoked_at, revoke_reason)             ON eacp.tool_contracts    TO eacp_app;
GRANT UPDATE (state, state_reason, active_allowlist_id) ON eacp.agent_versions TO eacp_app;
GRANT UPDATE (approved_at, revoked_at, revoke_reason) ON eacp.credentials       TO eacp_app;

-- +goose Down
DROP TABLE eacp.credentials;
ALTER TABLE eacp.agent_versions DROP CONSTRAINT agent_versions_tenant_id_active_allowlist_id_fkey;
DROP TABLE eacp.agent_allowlists;
DROP TABLE eacp.agent_versions;
DROP TABLE eacp.agents;
ALTER TABLE eacp.tools DROP CONSTRAINT tools_tenant_id_active_contract_id_fkey;
DROP TABLE eacp.tool_contracts;
DROP TABLE eacp.tools;
DROP TABLE eacp.connectors;
DROP TABLE eacp.group_memberships;
DROP TABLE eacp.groups;
DROP TABLE eacp.role_grants;
DROP TABLE eacp.principals;
DROP FUNCTION eacp.audit_row_change();
DROP FUNCTION eacp.credentials_guard();
DROP FUNCTION eacp.agent_allowlists_guard();
DROP FUNCTION eacp.agent_versions_guard();
DROP FUNCTION eacp.agents_guard();
DROP FUNCTION eacp.tool_contracts_guard();
DROP FUNCTION eacp.tools_guard();
DROP FUNCTION eacp.connectors_guard();
DROP FUNCTION eacp.tool_fingerprint(uuid, uuid);
DROP FUNCTION eacp.group_memberships_guard();
DROP FUNCTION eacp.groups_guard();
DROP FUNCTION eacp.role_grants_guard();
DROP FUNCTION eacp.principals_guard();
DROP FUNCTION eacp.set_once(timestamptz, timestamptz, text, text, text);
DROP FUNCTION eacp.require_reason(text, text);
DROP FUNCTION eacp.assert_distinct(uuid, uuid, text);
DROP FUNCTION eacp.assert_role(uuid, text[]);
DROP FUNCTION eacp.actor();
DROP FUNCTION eacp.is_schema_owner();
DROP FUNCTION eacp.current_actor_id();
