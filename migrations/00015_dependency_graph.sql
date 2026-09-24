-- Phase 15 (ADR-015): tenant-scoped dependency evidence. Registry-derived
-- Agent->MCP and MCP->Tool edges stay authoritative in their existing tables.

-- +goose Up

CREATE TABLE eacp.dependency_edges (
    tenant_id   uuid NOT NULL REFERENCES eacp.tenants (id),
    id          uuid NOT NULL DEFAULT gen_random_uuid(),
    from_kind   text NOT NULL CHECK (from_kind IN ('agent_version', 'tool')),
    from_id     uuid NOT NULL,
    to_kind     text NOT NULL CHECK (to_kind IN ('model', 'mcp', 'agent_version', 'system')),
    to_id       uuid,
    to_name     text CHECK (to_name ~ '^[a-z0-9][a-z0-9._/-]{0,127}$'),
    source      text NOT NULL CHECK (source ~ '^[a-z0-9][a-z0-9._/-]{0,127}$'),
    confidence  text NOT NULL CHECK (confidence IN ('high', 'medium', 'low', 'unknown')),
    observed_at timestamptz NOT NULL,
    expires_at  timestamptz NOT NULL,
    created_by  uuid,
    created_at  timestamptz NOT NULL DEFAULT now(),
    revoked_by  uuid,
    revoked_at  timestamptz,
    revoke_reason text CHECK (length(btrim(revoke_reason)) BETWEEN 1 AND 500),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, created_by) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, revoked_by) REFERENCES eacp.principals (tenant_id, id),
    CHECK (expires_at > observed_at),
    CHECK ((to_id IS NULL) OR (to_name IS NULL)),
    CHECK ((revoked_at IS NULL) = (revoke_reason IS NULL)),
    CHECK ((revoked_at IS NULL) = (revoked_by IS NULL)),
    CHECK ((from_kind = 'agent_version' AND to_kind IN ('model', 'mcp', 'agent_version'))
        OR (from_kind = 'tool' AND to_kind = 'system')),
    CHECK ((to_kind IN ('model', 'system') AND to_id IS NULL)
        OR (to_kind IN ('mcp', 'agent_version') AND to_name IS NULL)),
    CHECK (confidence = 'unknown' OR num_nonnulls(to_id, to_name) = 1)
);
CREATE INDEX dependency_edges_from ON eacp.dependency_edges (tenant_id, from_kind, from_id) WHERE revoked_at IS NULL;
CREATE INDEX dependency_edges_target_id ON eacp.dependency_edges (tenant_id, to_kind, to_id) WHERE revoked_at IS NULL;
CREATE INDEX dependency_edges_target_name ON eacp.dependency_edges (tenant_id, to_kind, to_name) WHERE revoked_at IS NULL;

-- +goose StatementBegin
CREATE FUNCTION eacp.dependency_edges_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.actor();
BEGIN
    PERFORM eacp.assert_role(a, 'registry_editor');
    IF TG_OP = 'UPDATE' THEN
        IF NEW IS NOT DISTINCT FROM OLD THEN
            RETURN NEW;
        END IF;
        IF OLD.revoked_at IS NOT NULL OR NEW.revoked_at IS NULL OR NEW.revoke_reason IS NULL
           OR (to_jsonb(NEW) - 'revoked_at' - 'revoked_by' - 'revoke_reason')
              IS DISTINCT FROM (to_jsonb(OLD) - 'revoked_at' - 'revoked_by' - 'revoke_reason') THEN
            RAISE EXCEPTION 'dependency evidence is immutable; only revocation is allowed' USING ERRCODE = '55000';
        END IF;
        NEW.revoked_at := now();
        NEW.revoked_by := a;
        RETURN NEW;
    END IF;
    IF NEW.revoked_at IS NOT NULL OR NEW.revoked_by IS NOT NULL OR NEW.revoke_reason IS NOT NULL THEN
        RAISE EXCEPTION 'dependency evidence cannot start revoked' USING ERRCODE = '23514';
    END IF;
    IF NEW.observed_at > now() + interval '5 minutes' THEN
        RAISE EXCEPTION 'observation is in the future' USING ERRCODE = '23514';
    END IF;
    IF NEW.from_kind = 'agent_version' THEN
        PERFORM 1 FROM eacp.agent_versions WHERE tenant_id = NEW.tenant_id AND id = NEW.from_id FOR SHARE;
    ELSE
        PERFORM 1 FROM eacp.tools WHERE tenant_id = NEW.tenant_id AND id = NEW.from_id FOR SHARE;
    END IF;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'dependency source does not exist in tenant' USING ERRCODE = '23503';
    END IF;
    IF NEW.to_kind = 'agent_version' AND NEW.to_id IS NOT NULL THEN
        IF NEW.to_id = NEW.from_id THEN
            RAISE EXCEPTION 'self-delegation is invalid' USING ERRCODE = '23514';
        END IF;
        PERFORM 1 FROM eacp.agent_versions WHERE tenant_id = NEW.tenant_id AND id = NEW.to_id FOR SHARE;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'dependency target does not exist in tenant' USING ERRCODE = '23503';
        END IF;
    ELSIF NEW.to_kind = 'mcp' AND NEW.to_id IS NOT NULL THEN
        -- Connector identity and protocol are immutable. A row lock would
        -- require table UPDATE privilege that eacp_app intentionally lacks.
        PERFORM 1 FROM eacp.connectors WHERE tenant_id = NEW.tenant_id AND id = NEW.to_id AND protocol = 'mcp';
        IF NOT FOUND THEN
            RAISE EXCEPTION 'dependency target is not a tenant MCP server' USING ERRCODE = '23503';
        END IF;
    END IF;
    NEW.created_by := a;
    NEW.created_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER dependency_edges_guard BEFORE INSERT OR UPDATE ON eacp.dependency_edges
    FOR EACH ROW EXECUTE FUNCTION eacp.dependency_edges_guard();
CREATE TRIGGER zz_audit AFTER INSERT OR UPDATE ON eacp.dependency_edges
    FOR EACH ROW EXECUTE FUNCTION eacp.audit_row_change();

ALTER TABLE eacp.dependency_edges ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.dependency_edges FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.dependency_edges USING (tenant_id = eacp.current_tenant_id());
REVOKE UPDATE ON eacp.dependency_edges FROM eacp_app;
GRANT UPDATE (revoked_at, revoke_reason) ON eacp.dependency_edges TO eacp_app;

-- +goose Down
DROP TABLE eacp.dependency_edges;
DROP FUNCTION eacp.dependency_edges_guard();
