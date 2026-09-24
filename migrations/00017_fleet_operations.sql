-- Phase 17 (ADR-024): fleet operations. An operation is a set of ADR-003
-- lifecycle transitions made atomically; the version guard still authorizes
-- each one. Both tables are insert-only evidence.
-- +goose Up

CREATE TABLE eacp.fleet_operations (
    tenant_id           uuid        NOT NULL REFERENCES eacp.tenants (id),
    id                  uuid        NOT NULL DEFAULT gen_random_uuid(),
    kind                text        NOT NULL CHECK (kind IN ('pause', 'resume', 'quarantine', 'release', 'rollback')),
    selector            jsonb       NOT NULL CHECK (jsonb_typeof(selector) = 'object'),
    reason              text        NOT NULL CHECK (btrim(reason) <> '' AND length(reason) <= 1024),
    source_operation_id uuid,
    created_by          uuid        NOT NULL,
    created_at          timestamptz NOT NULL,
    created_xact        xid8        NOT NULL,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, created_by) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, source_operation_id) REFERENCES eacp.fleet_operations (tenant_id, id)
);

CREATE TABLE eacp.fleet_operation_targets (
    tenant_id    uuid    NOT NULL,
    operation_id uuid    NOT NULL,
    version_id   uuid    NOT NULL,
    agent_id     uuid    NOT NULL,
    ordinal      integer NOT NULL CHECK (ordinal BETWEEN 1 AND 500),
    from_state   text    NOT NULL,
    to_state     text    NOT NULL,
    PRIMARY KEY (tenant_id, operation_id, version_id),
    UNIQUE (tenant_id, operation_id, ordinal),
    FOREIGN KEY (tenant_id, operation_id) REFERENCES eacp.fleet_operations (tenant_id, id),
    FOREIGN KEY (tenant_id, version_id) REFERENCES eacp.agent_versions (tenant_id, id),
    FOREIGN KEY (tenant_id, agent_id) REFERENCES eacp.agents (tenant_id, id)
);
CREATE INDEX fleet_operation_targets_version ON eacp.fleet_operation_targets (tenant_id, version_id);

-- +goose StatementBegin
-- An operation is created by a principal holding a role that may make its
-- transitions; the version guard checks that role again for every target.
CREATE FUNCTION eacp.fleet_operations_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.current_actor_id();
    source_kind text;
BEGIN
    IF a IS NULL THEN
        RAISE EXCEPTION 'a fleet operation needs a principal' USING ERRCODE = '42501';
    END IF;
    IF NEW.kind IN ('pause', 'quarantine') THEN
        PERFORM eacp.assert_role(a, 'operator', 'registry_approver');
    ELSE
        PERFORM eacp.assert_role(a, 'registry_approver');
    END IF;
    PERFORM eacp.require_reason(NEW.reason, 'a fleet operation');
    IF NEW.kind IN ('resume', 'release') THEN
        SELECT kind INTO source_kind FROM eacp.fleet_operations
         WHERE tenant_id = NEW.tenant_id AND id = NEW.source_operation_id;
        IF source_kind IS DISTINCT FROM (CASE NEW.kind WHEN 'resume' THEN 'pause' ELSE 'quarantine' END) THEN
            RAISE EXCEPTION 'a fleet % undoes a %', NEW.kind,
                CASE NEW.kind WHEN 'resume' THEN 'pause' ELSE 'quarantine' END USING ERRCODE = '23514';
        END IF;
    ELSIF NEW.source_operation_id IS NOT NULL THEN
        RAISE EXCEPTION 'only resume and release name a source operation' USING ERRCODE = '23514';
    END IF;
    NEW.created_by := a;
    NEW.created_at := now();
    NEW.created_xact := pg_current_xact_id();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Inserting a target performs its transition. PostgreSQL reads the from
-- state, checks the pair for the operation's kind, and updates the version
-- under the version guard, whose audit trigger journals it.
CREATE FUNCTION eacp.fleet_operation_targets_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.current_actor_id();
    op eacp.fleet_operations%ROWTYPE;
    v eacp.agent_versions%ROWTYPE;
    first eacp.fleet_operation_targets%ROWTYPE;
    replaced integer;
    ok boolean;
BEGIN
    SELECT * INTO op FROM eacp.fleet_operations
     WHERE tenant_id = NEW.tenant_id AND id = NEW.operation_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'no such fleet operation' USING ERRCODE = '23503';
    END IF;
    IF a IS NULL OR op.created_by IS DISTINCT FROM a OR op.created_xact <> pg_current_xact_id() THEN
        RAISE EXCEPTION 'targets are added only by the transaction that created the operation'
            USING ERRCODE = '55000';
    END IF;
    SELECT * INTO v FROM eacp.agent_versions
     WHERE tenant_id = NEW.tenant_id AND id = NEW.version_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'no such agent version in this tenant' USING ERRCODE = '23503';
    END IF;
    IF EXISTS (SELECT 1 FROM eacp.fleet_operation_targets
               WHERE tenant_id = NEW.tenant_id AND operation_id = op.id AND version_id = v.id) THEN
        RAISE EXCEPTION 'version % is already a target of this operation', v.id USING ERRCODE = '55000';
    END IF;
    NEW.agent_id := v.agent_id;
    NEW.from_state := v.state;
    SELECT COALESCE(max(ordinal), 0) + 1 INTO NEW.ordinal FROM eacp.fleet_operation_targets
     WHERE tenant_id = NEW.tenant_id AND operation_id = op.id;
    IF NEW.ordinal > 500 THEN
        RAISE EXCEPTION 'a fleet operation changes at most 500 versions' USING ERRCODE = '23514';
    END IF;

    ok := CASE op.kind
        WHEN 'pause' THEN v.state = 'ACTIVE' AND NEW.to_state = 'SUSPENDED'
        WHEN 'resume' THEN v.state = 'SUSPENDED' AND NEW.to_state = 'ACTIVE'
        WHEN 'quarantine' THEN v.state IN ('REGISTERED', 'ACTIVE', 'SUSPENDED') AND NEW.to_state = 'QUARANTINED'
        WHEN 'release' THEN v.state = 'QUARANTINED' AND NEW.to_state = 'SUSPENDED'
        WHEN 'rollback' THEN (v.state = 'ACTIVE' AND NEW.to_state = 'SUSPENDED')
                          OR (v.state = 'SUSPENDED' AND NEW.to_state = 'ACTIVE')
    END;
    IF NOT ok THEN
        RAISE EXCEPTION 'a fleet % cannot move version % from % to %', op.kind, v.id, v.state, NEW.to_state
            USING ERRCODE = '55000';
    END IF;

    -- resume and release undo only what their source operation did.
    IF op.kind IN ('resume', 'release') AND NOT EXISTS (
        SELECT 1 FROM eacp.fleet_operation_targets
         WHERE tenant_id = NEW.tenant_id AND operation_id = op.source_operation_id AND version_id = v.id
           AND to_state = NEW.from_state) THEN
        RAISE EXCEPTION 'version % was not changed by the source operation', v.id USING ERRCODE = '55000';
    END IF;

    -- A rollback touches one agent: it may first suspend the ACTIVE version,
    -- then activates one version older than the one it replaces (or, with
    -- nothing to replace, older than the agent's newest version).
    IF op.kind = 'rollback' THEN
        SELECT * INTO first FROM eacp.fleet_operation_targets
         WHERE tenant_id = NEW.tenant_id AND operation_id = op.id ORDER BY ordinal LIMIT 1;
        IF FOUND AND first.agent_id <> v.agent_id THEN
            RAISE EXCEPTION 'a rollback changes one agent' USING ERRCODE = '55000';
        END IF;
        IF NEW.to_state = 'SUSPENDED' AND NEW.ordinal <> 1 THEN
            RAISE EXCEPTION 'a rollback suspends the active version first' USING ERRCODE = '55000';
        END IF;
        IF NEW.to_state = 'ACTIVE' THEN
            IF NEW.ordinal > 2 OR (FOUND AND first.to_state <> 'SUSPENDED') THEN
                RAISE EXCEPTION 'a rollback activates one version' USING ERRCODE = '55000';
            END IF;
            IF FOUND THEN
                SELECT version INTO replaced FROM eacp.agent_versions
                 WHERE tenant_id = NEW.tenant_id AND id = first.version_id;
            ELSE
                SELECT max(version) INTO replaced FROM eacp.agent_versions
                 WHERE tenant_id = NEW.tenant_id AND agent_id = v.agent_id;
            END IF;
            IF v.version >= replaced THEN
                RAISE EXCEPTION 'a rollback activates an older version' USING ERRCODE = '55000';
            END IF;
        END IF;
    END IF;

    UPDATE eacp.agent_versions
       SET state = NEW.to_state, state_reason = 'fleet ' || op.kind || ' ' || op.id || ': ' || op.reason
     WHERE tenant_id = NEW.tenant_id AND id = v.id;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- At commit: an operation changed at least one version (a rollback exactly
-- one activation), and its journal entry, listing the targets, comes last.
CREATE FUNCTION eacp.fleet_operations_commit() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    targets jsonb;
    activations integer;
BEGIN
    SELECT jsonb_agg(jsonb_build_object('version_id', version_id, 'agent_id', agent_id,
                                        'from', from_state, 'to', to_state) ORDER BY ordinal),
           count(*) FILTER (WHERE to_state = 'ACTIVE')
      INTO targets, activations
      FROM eacp.fleet_operation_targets
     WHERE tenant_id = NEW.tenant_id AND operation_id = NEW.id;
    IF targets IS NULL THEN
        RAISE EXCEPTION 'a fleet operation changes at least one version' USING ERRCODE = '23514';
    END IF;
    IF NEW.kind = 'rollback' AND activations <> 1 THEN
        RAISE EXCEPTION 'a rollback activates exactly one version' USING ERRCODE = '23514';
    END IF;
    INSERT INTO eacp.audit_events (tenant_id, payload)
    VALUES (NEW.tenant_id, convert_to(jsonb_build_object(
        'v', 1, 'actor', jsonb_build_object('kind', 'principal', 'id', NEW.created_by),
        'action', 'fleet.' || NEW.kind,
        'subject', jsonb_build_object('type', 'fleet_operation', 'id', NEW.id),
        'reason', NEW.reason,
        'data', jsonb_build_object('selector', NEW.selector, 'source_operation_id', NEW.source_operation_id,
                                   'targets', targets))::text, 'UTF8'));
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER fleet_operations_guard BEFORE INSERT ON eacp.fleet_operations
    FOR EACH ROW EXECUTE FUNCTION eacp.fleet_operations_guard();
CREATE CONSTRAINT TRIGGER fleet_operations_commit AFTER INSERT ON eacp.fleet_operations
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION eacp.fleet_operations_commit();
CREATE TRIGGER fleet_operation_targets_guard BEFORE INSERT ON eacp.fleet_operation_targets
    FOR EACH ROW EXECUTE FUNCTION eacp.fleet_operation_targets_guard();

ALTER TABLE eacp.fleet_operations ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.fleet_operations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.fleet_operations USING (tenant_id = eacp.current_tenant_id());
ALTER TABLE eacp.fleet_operation_targets ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.fleet_operation_targets FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.fleet_operation_targets USING (tenant_id = eacp.current_tenant_id());
REVOKE UPDATE ON eacp.fleet_operations FROM eacp_app;
REVOKE UPDATE ON eacp.fleet_operation_targets FROM eacp_app;

-- +goose Down
DROP TABLE eacp.fleet_operation_targets;
DROP TABLE eacp.fleet_operations;
DROP FUNCTION eacp.fleet_operations_commit();
DROP FUNCTION eacp.fleet_operation_targets_guard();
DROP FUNCTION eacp.fleet_operations_guard();
