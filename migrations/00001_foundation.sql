-- Phase 1 foundation: schema, tenant context and the Row-Level Security
-- convention every tenant-scoped table follows (ADR-021).
--
-- Convention for tenant-scoped tables:
--   * column   tenant_id uuid NOT NULL
--   * ALTER TABLE ... ENABLE ROW LEVEL SECURITY;
--   * ALTER TABLE ... FORCE ROW LEVEL SECURITY;   -- applies to the owner too
--   * CREATE POLICY tenant_isolation ... USING (tenant_id = eacp.current_tenant_id())
--   * unique keys include tenant_id
-- Without a tenant context, eacp.current_tenant_id() is NULL, so no row
-- matches: a query that forgets the tenant fails closed.
--
-- The application role (eacp_app) gets SELECT/INSERT/UPDATE by default but
-- never DELETE: audit and execution records are append-or-transition only.
-- Roles are created by deployment bootstrap (deployments/docker/postgres).

-- +goose Up
CREATE SCHEMA eacp;
GRANT USAGE ON SCHEMA eacp TO eacp_app;

-- Readiness checks read the migration version as the application role.
GRANT SELECT ON public.goose_db_version TO eacp_app;

CREATE FUNCTION eacp.current_tenant_id() RETURNS uuid
    LANGUAGE sql STABLE
    AS $$ SELECT NULLIF(current_setting('app.tenant_id', true), '')::uuid $$;

ALTER DEFAULT PRIVILEGES IN SCHEMA eacp GRANT SELECT, INSERT, UPDATE ON TABLES TO eacp_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA eacp GRANT USAGE, SELECT ON SEQUENCES TO eacp_app;

CREATE TABLE eacp.tenants (
    id           uuid        PRIMARY KEY,
    slug         text        NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,62}$'),
    display_name text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE eacp.tenants ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.tenants FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.tenants
    USING (id = eacp.current_tenant_id());

-- Tenants are provisioned by operators, not by services.
REVOKE INSERT, UPDATE ON eacp.tenants FROM eacp_app;

-- +goose Down
DROP TABLE eacp.tenants;
DROP FUNCTION eacp.current_tenant_id();
ALTER DEFAULT PRIVILEGES IN SCHEMA eacp REVOKE USAGE, SELECT ON SEQUENCES FROM eacp_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA eacp REVOKE SELECT, INSERT, UPDATE ON TABLES FROM eacp_app;
REVOKE SELECT ON public.goose_db_version FROM eacp_app;
REVOKE USAGE ON SCHEMA eacp FROM eacp_app;
DROP SCHEMA eacp;
