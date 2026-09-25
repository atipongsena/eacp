-- Phase 20 (ADR-026): Governance-as-Code. A bundle's desired state is
-- planned into a change set. Submit and approve run its steps as the same
-- registry writes the API makes, so every registry trigger still decides
-- each one. PostgreSQL computes every digest; a change set whose digest no
-- longer matches the registry is stale and runs nothing. Nothing is deleted.
-- +goose Up

CREATE TABLE eacp.bundles (
    tenant_id  uuid        NOT NULL REFERENCES eacp.tenants (id),
    id         uuid        NOT NULL DEFAULT gen_random_uuid(),
    name       text        NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9-]{1,62}$'),
    created_by uuid        NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, name),
    FOREIGN KEY (tenant_id, created_by) REFERENCES eacp.principals (tenant_id, id)
);

CREATE TABLE eacp.change_sets (
    tenant_id      uuid        NOT NULL,
    id             uuid        NOT NULL,
    bundle_id      uuid        NOT NULL,
    state          text        NOT NULL CHECK (state IN ('PLANNED', 'SUBMITTED', 'APPLIED', 'REJECTED', 'SUPERSEDED')),
    desired        jsonb       NOT NULL CHECK (jsonb_typeof(desired) = 'object' AND octet_length(desired::text) <= 1048576),
    desired_digest bytea       NOT NULL CHECK (octet_length(desired_digest) = 32),
    prune          boolean     NOT NULL,
    base_digest    bytea       CHECK (octet_length(base_digest) = 32),
    sealed_digest  bytea       CHECK (octet_length(sealed_digest) = 32),
    planned_by     uuid        NOT NULL,
    planned_at     timestamptz NOT NULL,
    planned_xact   xid8        NOT NULL,
    submitted_by   uuid,
    submitted_at   timestamptz,
    submitted_xact xid8,
    approved_by    uuid,
    approved_at    timestamptz,
    approved_xact  xid8,
    closed_by      uuid,
    closed_at      timestamptz,
    close_reason   text        CHECK (length(close_reason) <= 1024),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, bundle_id) REFERENCES eacp.bundles (tenant_id, id),
    FOREIGN KEY (tenant_id, planned_by) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, submitted_by) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, approved_by) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, closed_by) REFERENCES eacp.principals (tenant_id, id)
);
-- One open change set per bundle: the lock Terraform keeps in a state file.
CREATE UNIQUE INDEX change_sets_one_open ON eacp.change_sets (tenant_id, bundle_id)
    WHERE state IN ('PLANNED', 'SUBMITTED');
CREATE INDEX change_sets_by_bundle ON eacp.change_sets (tenant_id, bundle_id, planned_at DESC);

CREATE TABLE eacp.change_set_steps (
    tenant_id     uuid    NOT NULL,
    change_set_id uuid    NOT NULL,
    ordinal       integer NOT NULL CHECK (ordinal BETWEEN 1 AND 2000),
    address       text    NOT NULL CHECK (address ~
        '^(connector|tool|contract|agent|version|allowlist)\.[a-z0-9][a-z0-9_-]{0,62}(\.[a-z0-9][a-z0-9_-]{0,62})?$'),
    op            text    NOT NULL CHECK (op IN ('create', 'propose', 'activate', 'transition', 'revoke', 'import')),
    stage         text    NOT NULL CHECK (stage IN ('submit', 'approve')),
    payload       jsonb   NOT NULL CHECK (jsonb_typeof(payload) = 'object' AND octet_length(payload::text) <= 65536),
    object_id     uuid,
    executed_xact xid8,
    PRIMARY KEY (tenant_id, change_set_id, ordinal),
    FOREIGN KEY (tenant_id, change_set_id) REFERENCES eacp.change_sets (tenant_id, id),
    CHECK ((object_id IS NULL) = (executed_xact IS NULL))
);

CREATE TABLE eacp.change_set_refs (
    tenant_id     uuid NOT NULL,
    change_set_id uuid NOT NULL,
    kind          text NOT NULL CHECK (kind IN ('connector', 'tool', 'contract', 'agent', 'version', 'allowlist',
                                                'principal', 'group')),
    object_id     uuid NOT NULL,
    PRIMARY KEY (tenant_id, change_set_id, kind, object_id),
    FOREIGN KEY (tenant_id, change_set_id) REFERENCES eacp.change_sets (tenant_id, id)
);

-- The state: which registry object an address of a bundle manages.
CREATE TABLE eacp.bundle_resources (
    tenant_id     uuid        NOT NULL,
    bundle_id     uuid        NOT NULL,
    address       text        NOT NULL,
    kind          text        NOT NULL CHECK (kind IN ('connector', 'tool', 'agent', 'version')),
    object_id     uuid        NOT NULL,
    change_set_id uuid        NOT NULL,
    managed_at    timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, bundle_id, address),
    UNIQUE (tenant_id, kind, object_id),
    FOREIGN KEY (tenant_id, bundle_id) REFERENCES eacp.bundles (tenant_id, id),
    FOREIGN KEY (tenant_id, change_set_id) REFERENCES eacp.change_sets (tenant_id, id)
);

-- +goose StatementBegin
CREATE FUNCTION eacp.assert_bundle_role(a uuid) RETURNS void
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF a IS NULL THEN
        RAISE EXCEPTION 'a change set needs a principal' USING ERRCODE = '42501';
    END IF;
    PERFORM eacp.assert_role(a, 'registry_editor', 'registry_approver', 'admin');
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.bundles_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.current_actor_id();
BEGIN
    PERFORM eacp.assert_bundle_role(a);
    NEW.created_by := a;
    NEW.created_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The part of a registry object a plan depends on, as text, or 'absent'.
-- Under RLS it sees only the current tenant.
CREATE FUNCTION eacp.change_set_ref_row(p_kind text, p_id uuid) RETURNS text
    LANGUAGE plpgsql STABLE
    AS $$
DECLARE
    r text;
BEGIN
    CASE p_kind
    WHEN 'connector' THEN
        SELECT jsonb_build_array(c.id, c.name, c.protocol, c.endpoint, c.secret_ref,
               (SELECT COALESCE(jsonb_agg(jsonb_build_array(t.id, t.name) ORDER BY t.id), '[]'::jsonb)
                  FROM eacp.tools t WHERE t.tenant_id = c.tenant_id AND t.connector_id = c.id))::text
          INTO r FROM eacp.connectors c WHERE c.id = p_id;
    WHEN 'tool' THEN
        SELECT jsonb_build_array(t.id, t.connector_id, t.name, t.origin, t.active_contract_id, t.definition_id,
               t.quarantined_at IS NULL)::text
          INTO r FROM eacp.tools t WHERE t.id = p_id;
    WHEN 'contract' THEN
        SELECT jsonb_build_array(ct.id, ct.tool_id, ct.revoked_at IS NULL)::text
          INTO r FROM eacp.tool_contracts ct WHERE ct.id = p_id;
    WHEN 'agent' THEN
        SELECT jsonb_build_array(a.id, a.name, a.display_name, a.environment, a.risk_class,
               a.owner_principal_id, a.owner_group_id,
               (SELECT COALESCE(jsonb_agg(jsonb_build_array(v.id, v.state, v.active_allowlist_id) ORDER BY v.version),
                                '[]'::jsonb)
                  FROM eacp.agent_versions v WHERE v.tenant_id = a.tenant_id AND v.agent_id = a.id))::text
          INTO r FROM eacp.agents a WHERE a.id = p_id;
    WHEN 'version' THEN
        SELECT jsonb_build_array(v.id, v.agent_id, v.version, v.runtime, v.code_ref, v.state,
               v.active_allowlist_id)::text
          INTO r FROM eacp.agent_versions v WHERE v.id = p_id;
    WHEN 'allowlist' THEN
        SELECT jsonb_build_array(al.id, al.agent_version_id, al.tool_ids)::text
          INTO r FROM eacp.agent_allowlists al WHERE al.id = p_id;
    WHEN 'principal' THEN
        SELECT jsonb_build_array(p.id, p.name, p.disabled_at IS NULL)::text
          INTO r FROM eacp.principals p WHERE p.id = p_id;
    WHEN 'group' THEN
        SELECT jsonb_build_array(g.id, g.name)::text
          INTO r FROM eacp.groups g WHERE g.id = p_id;
    ELSE
        RAISE EXCEPTION 'unknown ref kind %', p_kind USING ERRCODE = '23514';
    END CASE;
    RETURN COALESCE(r, 'absent');
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The digest of what a change set depends on: its refs and the objects its
-- bundle manages, each with its current row (ids, fields and states).
CREATE FUNCTION eacp.change_set_digest(p_change_set uuid) RETURNS bytea
    LANGUAGE sql STABLE
    AS $$
    SELECT sha256(convert_to(COALESCE(string_agg(x.line, E'\n' ORDER BY x.line), ''), 'UTF8'))
      FROM (SELECT r.kind || ' ' || r.object_id || ' ' || eacp.change_set_ref_row(r.kind, r.object_id) AS line
              FROM eacp.change_set_refs r WHERE r.change_set_id = p_change_set
            UNION ALL
            SELECT 'managed ' || br.address || ' ' || br.object_id || ' ' || eacp.change_set_ref_row(br.kind, br.object_id)
              FROM eacp.bundle_resources br
              JOIN eacp.change_sets cs ON cs.tenant_id = br.tenant_id AND cs.bundle_id = br.bundle_id
             WHERE cs.id = p_change_set) x
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.change_sets_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.current_actor_id();
    has_approve boolean;
BEGIN
    PERFORM eacp.assert_bundle_role(a);
    IF TG_OP = 'INSERT' THEN
        IF NEW.state <> 'PLANNED' THEN
            RAISE EXCEPTION 'a change set starts PLANNED' USING ERRCODE = '23514';
        END IF;
        NEW.desired_digest := sha256(convert_to(NEW.desired::text, 'UTF8'));
        NEW.base_digest := NULL;
        NEW.sealed_digest := NULL;
        NEW.planned_by := a;
        NEW.planned_at := now();
        NEW.planned_xact := pg_current_xact_id();
        NEW.submitted_by := NULL; NEW.submitted_at := NULL; NEW.submitted_xact := NULL;
        NEW.approved_by := NULL; NEW.approved_at := NULL; NEW.approved_xact := NULL;
        NEW.closed_by := NULL; NEW.closed_at := NULL; NEW.close_reason := NULL;
        RETURN NEW;
    END IF;

    IF (NEW.tenant_id, NEW.id, NEW.bundle_id, NEW.desired, NEW.desired_digest, NEW.prune,
        NEW.planned_by, NEW.planned_at, NEW.planned_xact)
       IS DISTINCT FROM (OLD.tenant_id, OLD.id, OLD.bundle_id, OLD.desired, OLD.desired_digest, OLD.prune,
        OLD.planned_by, OLD.planned_at, OLD.planned_xact) THEN
        RAISE EXCEPTION 'a change set''s plan is immutable' USING ERRCODE = '55000';
    END IF;

    IF NEW.state = OLD.state THEN
        -- A seal: a digest written once, equal to PostgreSQL's own.
        IF (NEW.submitted_by, NEW.submitted_at, NEW.submitted_xact, NEW.approved_by, NEW.approved_at,
            NEW.approved_xact, NEW.closed_by, NEW.closed_at, NEW.close_reason)
           IS DISTINCT FROM (OLD.submitted_by, OLD.submitted_at, OLD.submitted_xact, OLD.approved_by,
            OLD.approved_at, OLD.approved_xact, OLD.closed_by, OLD.closed_at, OLD.close_reason) THEN
            RAISE EXCEPTION 'only a state change records who moved a change set' USING ERRCODE = '55000';
        END IF;
        IF NEW.base_digest IS DISTINCT FROM OLD.base_digest AND (
               OLD.base_digest IS NOT NULL OR OLD.state <> 'PLANNED'
               OR OLD.planned_xact <> pg_current_xact_id() OR OLD.planned_by <> a
               OR NEW.base_digest IS DISTINCT FROM eacp.change_set_digest(OLD.id)) THEN
            RAISE EXCEPTION 'a plan is sealed once, by the transaction that planned it' USING ERRCODE = '55000';
        END IF;
        IF NEW.sealed_digest IS DISTINCT FROM OLD.sealed_digest THEN
            IF OLD.sealed_digest IS NOT NULL OR OLD.state <> 'SUBMITTED'
               OR OLD.submitted_xact <> pg_current_xact_id() OR OLD.submitted_by <> a
               OR NEW.sealed_digest IS DISTINCT FROM eacp.change_set_digest(OLD.id) THEN
                RAISE EXCEPTION 'a submission is sealed once, by the transaction that submitted it'
                    USING ERRCODE = '55000';
            END IF;
            IF EXISTS (SELECT 1 FROM eacp.change_set_steps s
                        WHERE s.tenant_id = OLD.tenant_id AND s.change_set_id = OLD.id AND s.stage = 'submit'
                          AND (s.object_id IS NULL OR NOT EXISTS (
                               SELECT 1 FROM eacp.change_set_refs r
                                WHERE r.tenant_id = s.tenant_id AND r.change_set_id = s.change_set_id
                                  AND r.kind = split_part(s.address, '.', 1) AND r.object_id = s.object_id))) THEN
                RAISE EXCEPTION 'every submit step runs, and its object joins the refs, before the seal'
                    USING ERRCODE = '55000';
            END IF;
        END IF;
        RETURN NEW;
    END IF;

    -- A state change. Only the guard writes the who/when/xact columns.
    NEW.base_digest := OLD.base_digest;
    NEW.sealed_digest := OLD.sealed_digest;
    NEW.submitted_by := OLD.submitted_by; NEW.submitted_at := OLD.submitted_at; NEW.submitted_xact := OLD.submitted_xact;
    NEW.approved_by := OLD.approved_by; NEW.approved_at := OLD.approved_at; NEW.approved_xact := OLD.approved_xact;
    NEW.closed_by := OLD.closed_by; NEW.closed_at := OLD.closed_at;
    SELECT EXISTS (SELECT 1 FROM eacp.change_set_steps
                    WHERE tenant_id = OLD.tenant_id AND change_set_id = OLD.id AND stage = 'approve')
      INTO has_approve;

    IF OLD.state = 'PLANNED' AND NEW.state IN ('SUBMITTED', 'APPLIED') THEN
        IF OLD.base_digest IS NULL THEN
            RAISE EXCEPTION 'an unsealed plan cannot be submitted' USING ERRCODE = '55000';
        END IF;
        IF has_approve AND NEW.state = 'APPLIED' THEN
            RAISE EXCEPTION 'change set % has approve-stage steps: a second person applies it', OLD.id
                USING ERRCODE = '55000';
        ELSIF NOT has_approve AND NEW.state = 'SUBMITTED' THEN
            RAISE EXCEPTION 'change set % has no approve-stage steps: submitting applies it', OLD.id
                USING ERRCODE = '55000';
        END IF;
        IF eacp.change_set_digest(OLD.id) <> OLD.base_digest THEN
            RAISE EXCEPTION 'change set % is stale: the registry changed since it was planned', OLD.id
                USING ERRCODE = '40001';
        END IF;
        NEW.submitted_by := a;
        NEW.submitted_at := now();
        NEW.submitted_xact := pg_current_xact_id();
        NEW.close_reason := NULL;
    ELSIF OLD.state = 'SUBMITTED' AND NEW.state = 'APPLIED' THEN
        PERFORM eacp.assert_role(a, 'registry_approver', 'admin');
        PERFORM eacp.assert_distinct(a, OLD.submitted_by, 'the submitter of the change set');
        IF OLD.sealed_digest IS NULL THEN
            RAISE EXCEPTION 'an unsealed submission cannot be approved' USING ERRCODE = '55000';
        END IF;
        IF eacp.change_set_digest(OLD.id) <> OLD.sealed_digest THEN
            RAISE EXCEPTION 'change set % is stale: the registry changed since it was submitted', OLD.id
                USING ERRCODE = '40001';
        END IF;
        NEW.approved_by := a;
        NEW.approved_at := now();
        NEW.approved_xact := pg_current_xact_id();
        NEW.close_reason := NULL;
    ELSIF OLD.state IN ('PLANNED', 'SUBMITTED') AND NEW.state = 'REJECTED' THEN
        PERFORM eacp.require_reason(NEW.close_reason, 'rejecting a change set');
        NEW.closed_by := a;
        NEW.closed_at := now();
    ELSIF OLD.state = 'PLANNED' AND NEW.state = 'SUPERSEDED' THEN
        PERFORM eacp.require_reason(NEW.close_reason, 'superseding a change set');
        NEW.closed_by := a;
        NEW.closed_at := now();
    ELSE
        RAISE EXCEPTION 'a change set cannot move from % to %', OLD.state, NEW.state USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Steps are written by the planning transaction before its seal, and each
-- is run once, in order, by the transaction that submits or approves its
-- stage.
CREATE FUNCTION eacp.change_set_steps_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.current_actor_id();
    cs eacp.change_sets%ROWTYPE;
    ok boolean;
BEGIN
    SELECT * INTO cs FROM eacp.change_sets WHERE tenant_id = NEW.tenant_id AND id = NEW.change_set_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'no such change set' USING ERRCODE = '23503';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF a IS NULL OR cs.state <> 'PLANNED' OR cs.planned_by <> a OR cs.planned_xact <> pg_current_xact_id()
           OR cs.base_digest IS NOT NULL THEN
            RAISE EXCEPTION 'steps are added only by the transaction that plans the change set, before its seal'
                USING ERRCODE = '55000';
        END IF;
        NEW.object_id := NULL;
        NEW.executed_xact := NULL;
        RETURN NEW;
    END IF;
    IF (NEW.tenant_id, NEW.change_set_id, NEW.ordinal, NEW.address, NEW.op, NEW.stage, NEW.payload)
       IS DISTINCT FROM (OLD.tenant_id, OLD.change_set_id, OLD.ordinal, OLD.address, OLD.op, OLD.stage, OLD.payload) THEN
        RAISE EXCEPTION 'a planned step is immutable' USING ERRCODE = '55000';
    END IF;
    IF OLD.object_id IS NOT NULL OR NEW.object_id IS NULL THEN
        RAISE EXCEPTION 'a step runs once' USING ERRCODE = '55000';
    END IF;
    ok := CASE OLD.stage
        WHEN 'submit' THEN cs.state IN ('SUBMITTED', 'APPLIED') AND cs.submitted_by = a
                           AND cs.submitted_xact = pg_current_xact_id() AND cs.sealed_digest IS NULL
                           AND cs.approved_xact IS NULL
        WHEN 'approve' THEN cs.state = 'APPLIED' AND cs.approved_by = a
                            AND cs.approved_xact = pg_current_xact_id()
    END;
    IF NOT COALESCE(ok, false) THEN
        RAISE EXCEPTION 'a step runs only in the transaction that submits or approves its stage'
            USING ERRCODE = '55000';
    END IF;
    IF EXISTS (SELECT 1 FROM eacp.change_set_steps
                WHERE tenant_id = OLD.tenant_id AND change_set_id = OLD.change_set_id AND stage = OLD.stage
                  AND ordinal < OLD.ordinal AND object_id IS NULL) THEN
        RAISE EXCEPTION 'steps run in order' USING ERRCODE = '55000';
    END IF;
    NEW.executed_xact := pg_current_xact_id();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.change_set_refs_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.current_actor_id();
    cs eacp.change_sets%ROWTYPE;
BEGIN
    SELECT * INTO cs FROM eacp.change_sets WHERE tenant_id = NEW.tenant_id AND id = NEW.change_set_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'no such change set' USING ERRCODE = '23503';
    END IF;
    IF a IS NULL OR NOT (
          (cs.state = 'PLANNED' AND cs.planned_by = a AND cs.planned_xact = pg_current_xact_id()
           AND cs.base_digest IS NULL)
       OR (cs.state = 'SUBMITTED' AND cs.submitted_by = a AND cs.submitted_xact = pg_current_xact_id()
           AND cs.sealed_digest IS NULL)) THEN
        RAISE EXCEPTION 'refs are recorded only before the plan or its submission is sealed'
            USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Seals a change set: the planning transaction records base_digest; the
-- submitting transaction adds the objects its steps produced to the refs and
-- records sealed_digest, which the approval must match.
CREATE FUNCTION eacp.change_set_seal(p_change_set uuid) RETURNS bytea
    LANGUAGE plpgsql
    AS $$
DECLARE
    cs eacp.change_sets%ROWTYPE;
    d bytea;
BEGIN
    SELECT * INTO cs FROM eacp.change_sets WHERE id = p_change_set FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'no such change set' USING ERRCODE = '23503';
    END IF;
    IF NOT ((cs.state = 'PLANNED' AND cs.base_digest IS NULL)
            OR (cs.state = 'SUBMITTED' AND cs.sealed_digest IS NULL)) THEN
        RAISE EXCEPTION 'change set % is % and already sealed', cs.id, cs.state USING ERRCODE = '55000';
    END IF;
    IF cs.state = 'SUBMITTED' THEN
        INSERT INTO eacp.change_set_refs (tenant_id, change_set_id, kind, object_id)
        SELECT tenant_id, change_set_id, split_part(address, '.', 1), object_id
          FROM eacp.change_set_steps
         WHERE tenant_id = cs.tenant_id AND change_set_id = cs.id AND stage = 'submit' AND object_id IS NOT NULL
        ON CONFLICT DO NOTHING;
        d := eacp.change_set_digest(cs.id);
        UPDATE eacp.change_sets SET sealed_digest = d WHERE tenant_id = cs.tenant_id AND id = cs.id;
    ELSE
        d := eacp.change_set_digest(cs.id);
        UPDATE eacp.change_sets SET base_digest = d WHERE tenant_id = cs.tenant_id AND id = cs.id;
    END IF;
    RETURN d;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- An address manages an object only when the executing change set of its
-- bundle created or imported that object in this transaction.
CREATE FUNCTION eacp.bundle_resources_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.current_actor_id();
    cs eacp.change_sets%ROWTYPE;
BEGIN
    IF TG_OP = 'UPDATE' AND (NEW.tenant_id, NEW.bundle_id, NEW.address)
                            IS DISTINCT FROM (OLD.tenant_id, OLD.bundle_id, OLD.address) THEN
        RAISE EXCEPTION 'a managed address is permanent' USING ERRCODE = '55000';
    END IF;
    SELECT * INTO cs FROM eacp.change_sets WHERE tenant_id = NEW.tenant_id AND id = NEW.change_set_id;
    IF NOT FOUND OR cs.bundle_id <> NEW.bundle_id OR a IS NULL OR NOT (
          (cs.state IN ('SUBMITTED', 'APPLIED') AND cs.submitted_by = a AND cs.submitted_xact = pg_current_xact_id()
           AND cs.approved_xact IS NULL AND cs.sealed_digest IS NULL)
       OR (cs.state = 'APPLIED' AND cs.approved_by = a AND cs.approved_xact = pg_current_xact_id())) THEN
        RAISE EXCEPTION 'only the executing change set of a bundle manages its objects' USING ERRCODE = '55000';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM eacp.change_set_steps
                    WHERE tenant_id = NEW.tenant_id AND change_set_id = cs.id AND address = NEW.address
                      AND op IN ('create', 'import') AND object_id = NEW.object_id
                      AND executed_xact = pg_current_xact_id()) THEN
        RAISE EXCEPTION 'a managed object is one this change set created or imported' USING ERRCODE = '55000';
    END IF;
    NEW.kind := split_part(NEW.address, '.', 1);
    IF NEW.kind NOT IN ('connector', 'tool', 'agent', 'version') THEN
        RAISE EXCEPTION 'a bundle manages connectors, tools, agents and versions' USING ERRCODE = '23514';
    END IF;
    IF eacp.change_set_ref_row(NEW.kind, NEW.object_id) = 'absent' THEN
        RAISE EXCEPTION 'no such % in this tenant', NEW.kind USING ERRCODE = '23503';
    END IF;
    NEW.managed_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- At commit: a plan has steps and is sealed; an executed stage ran every
-- step; a submission is sealed. The journal entry comes last.
CREATE FUNCTION eacp.change_sets_commit() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    cs eacp.change_sets%ROWTYPE;
    bundle_name text;
    run_stage text;
    what text;
    who uuid;
    step_list jsonb;
BEGIN
    SELECT * INTO cs FROM eacp.change_sets WHERE tenant_id = NEW.tenant_id AND id = NEW.id;
    SELECT name INTO bundle_name FROM eacp.bundles WHERE tenant_id = cs.tenant_id AND id = cs.bundle_id;
    IF TG_OP = 'INSERT' THEN
        IF NOT EXISTS (SELECT 1 FROM eacp.change_set_steps WHERE tenant_id = cs.tenant_id AND change_set_id = cs.id) THEN
            RAISE EXCEPTION 'a change set has at least one step' USING ERRCODE = '23514';
        END IF;
        IF cs.base_digest IS NULL THEN
            RAISE EXCEPTION 'a plan is sealed before it commits' USING ERRCODE = '23514';
        END IF;
        what := 'change_set.planned';
        who := cs.planned_by;
    ELSE
        what := 'change_set.' || lower(NEW.state);
        CASE NEW.state
        WHEN 'SUBMITTED' THEN
            who := cs.submitted_by;
            run_stage := 'submit';
            IF cs.sealed_digest IS NULL THEN
                RAISE EXCEPTION 'a submission is sealed before it commits' USING ERRCODE = '23514';
            END IF;
        WHEN 'APPLIED' THEN
            IF cs.approved_xact IS NOT NULL THEN
                who := cs.approved_by;
                run_stage := 'approve';
            ELSE
                who := cs.submitted_by;
                run_stage := 'submit';
            END IF;
        ELSE
            who := cs.closed_by;
        END CASE;
        IF run_stage IS NOT NULL AND EXISTS (
            SELECT 1 FROM eacp.change_set_steps s
             WHERE s.tenant_id = cs.tenant_id AND s.change_set_id = cs.id AND s.stage = run_stage
               AND s.object_id IS NULL) THEN
            RAISE EXCEPTION 'every % step of a change set runs before it commits', run_stage USING ERRCODE = '23514';
        END IF;
    END IF;
    SELECT jsonb_agg(jsonb_build_object('ordinal', s.ordinal, 'address', s.address, 'op', s.op, 'stage', s.stage,
                                        'object_id', s.object_id) ORDER BY s.ordinal)
      INTO step_list FROM eacp.change_set_steps s WHERE s.tenant_id = cs.tenant_id AND s.change_set_id = cs.id;
    INSERT INTO eacp.audit_events (tenant_id, payload)
    VALUES (cs.tenant_id, convert_to(jsonb_build_object(
        'v', 1, 'actor', jsonb_build_object('kind', 'principal', 'id', who),
        'action', what,
        'subject', jsonb_build_object('type', 'change_set', 'id', cs.id),
        'reason', COALESCE(cs.close_reason, ''),
        'data', jsonb_build_object('bundle', bundle_name, 'desired_digest', encode(cs.desired_digest, 'hex'),
                                   'base_digest', encode(cs.base_digest, 'hex'),
                                   'sealed_digest', encode(cs.sealed_digest, 'hex'),
                                   'steps', COALESCE(step_list, '[]'::jsonb)))::text, 'UTF8'));
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER bundles_guard BEFORE INSERT ON eacp.bundles
    FOR EACH ROW EXECUTE FUNCTION eacp.bundles_guard();
CREATE TRIGGER change_sets_guard BEFORE INSERT OR UPDATE ON eacp.change_sets
    FOR EACH ROW EXECUTE FUNCTION eacp.change_sets_guard();
CREATE CONSTRAINT TRIGGER change_sets_commit_insert AFTER INSERT ON eacp.change_sets
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION eacp.change_sets_commit();
CREATE CONSTRAINT TRIGGER change_sets_commit_update AFTER UPDATE ON eacp.change_sets
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (OLD.state IS DISTINCT FROM NEW.state)
    EXECUTE FUNCTION eacp.change_sets_commit();
CREATE TRIGGER change_set_steps_guard BEFORE INSERT OR UPDATE ON eacp.change_set_steps
    FOR EACH ROW EXECUTE FUNCTION eacp.change_set_steps_guard();
CREATE TRIGGER change_set_refs_guard BEFORE INSERT ON eacp.change_set_refs
    FOR EACH ROW EXECUTE FUNCTION eacp.change_set_refs_guard();
CREATE TRIGGER bundle_resources_guard BEFORE INSERT OR UPDATE ON eacp.bundle_resources
    FOR EACH ROW EXECUTE FUNCTION eacp.bundle_resources_guard();

ALTER TABLE eacp.bundles ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.bundles FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.bundles USING (tenant_id = eacp.current_tenant_id());
ALTER TABLE eacp.change_sets ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.change_sets FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.change_sets USING (tenant_id = eacp.current_tenant_id());
ALTER TABLE eacp.change_set_steps ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.change_set_steps FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.change_set_steps USING (tenant_id = eacp.current_tenant_id());
ALTER TABLE eacp.change_set_refs ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.change_set_refs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.change_set_refs USING (tenant_id = eacp.current_tenant_id());
ALTER TABLE eacp.bundle_resources ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.bundle_resources FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.bundle_resources USING (tenant_id = eacp.current_tenant_id());
REVOKE UPDATE ON eacp.bundles FROM eacp_app;
REVOKE UPDATE ON eacp.change_set_refs FROM eacp_app;

-- +goose Down
DROP TABLE eacp.bundle_resources;
DROP TABLE eacp.change_set_refs;
DROP TABLE eacp.change_set_steps;
DROP TABLE eacp.change_sets;
DROP TABLE eacp.bundles;
DROP FUNCTION eacp.change_sets_commit();
DROP FUNCTION eacp.bundle_resources_guard();
DROP FUNCTION eacp.change_set_seal(uuid);
DROP FUNCTION eacp.change_set_refs_guard();
DROP FUNCTION eacp.change_set_steps_guard();
DROP FUNCTION eacp.change_sets_guard();
DROP FUNCTION eacp.change_set_digest(uuid);
DROP FUNCTION eacp.change_set_ref_row(text, uuid);
DROP FUNCTION eacp.bundles_guard();
DROP FUNCTION eacp.assert_bundle_role(uuid);
