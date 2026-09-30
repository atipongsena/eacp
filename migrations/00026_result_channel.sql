-- Phase 26b (ADR-034): the result channel. A contract may keep a successful
-- call's output for a limited time (result_retention_seconds, off when NULL).
-- Only the worker holding the lease records it, inside the transaction that
-- moves the action to SUCCEEDED; only an ACTIVE version of the action's agent
-- reads it; eacp_app cannot select the content; the sweeper clears it once it
-- expires. No audit trigger is attached: the content never reaches the journal.
-- +goose Up

ALTER TABLE eacp.tool_contracts
    ADD COLUMN result_retention_seconds integer
        CHECK (result_retention_seconds BETWEEN 60 AND 86400);

CREATE TABLE eacp.action_results (
    tenant_id        uuid        NOT NULL REFERENCES eacp.tenants (id),
    action_id        uuid        NOT NULL,
    agent_id         uuid        NOT NULL,
    contract_id      uuid        NOT NULL,
    lease_generation bigint      NOT NULL,
    -- RFC 8785 JSON, canonicalized by the worker; NULL when withheld or pruned.
    output           text        CHECK (output IS JSON AND octet_length(output) <= 65536),
    withheld         text        CHECK (withheld IN ('too_large', 'contains_credential', 'invalid_output')),
    sha256           text        CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    bytes            integer     CHECK (bytes BETWEEN 0 AND 65536),
    created_at       timestamptz NOT NULL DEFAULT now(),
    expires_at       timestamptz NOT NULL,
    pruned_at        timestamptz,
    PRIMARY KEY (tenant_id, action_id),
    FOREIGN KEY (tenant_id, action_id) REFERENCES eacp.actions (tenant_id, id),
    CHECK (output IS NULL OR withheld IS NULL),
    CHECK (pruned_at IS NOT NULL OR num_nonnulls(output, withheld) = 1),
    CHECK (output IS NULL OR pruned_at IS NULL),
    CHECK ((sha256 IS NULL) = (bytes IS NULL) AND (withheld IS NULL OR sha256 IS NULL))
);
CREATE INDEX action_results_due ON eacp.action_results (tenant_id, expires_at) WHERE pruned_at IS NULL;

ALTER TABLE eacp.action_results ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.action_results FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.action_results USING (tenant_id = eacp.current_tenant_id());
-- The schema owner's read-only scan behind eacp.action_result_tenants().
CREATE POLICY owner_scan ON eacp.action_results FOR SELECT TO CURRENT_USER USING (true);
-- The application sees the metadata; the content and every write go through
-- the SECURITY DEFINER functions below.
REVOKE ALL ON eacp.action_results FROM eacp_app;
GRANT SELECT (tenant_id, action_id, agent_id, contract_id, lease_generation, withheld, sha256, bytes,
              created_at, expires_at, pruned_at) ON eacp.action_results TO eacp_app;

-- +goose StatementBegin
CREATE FUNCTION eacp.action_result_record(p_action uuid, p_output text, p_withheld text) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    who  jsonb := eacp.actor_context();
    a    eacp.actions%ROWTYPE;
    att  eacp.action_attempts%ROWTYPE;
    keep integer;
BEGIN
    SELECT * INTO a FROM eacp.actions
    WHERE tenant_id = eacp.current_tenant_id() AND id = p_action FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'action % not found', p_action USING ERRCODE = 'P0002';
    END IF;
    PERFORM eacp.assert_lease_holder(who, a);
    IF a.state IS DISTINCT FROM 'EXECUTING' THEN
        RAISE EXCEPTION 'a result is recorded only while the action executes' USING ERRCODE = '55000';
    END IF;
    SELECT * INTO att FROM eacp.action_attempts
    WHERE tenant_id = a.tenant_id AND action_id = a.id AND lease_generation = a.lease_generation;
    IF NOT FOUND OR att.outcome IS DISTINCT FROM 'succeeded' OR att.late THEN
        RAISE EXCEPTION 'only a succeeded attempt keeps a result' USING ERRCODE = '55000';
    END IF;
    IF num_nonnulls(p_output, p_withheld) <> 1 THEN
        RAISE EXCEPTION 'a result is either output or a withheld reason' USING ERRCODE = '22023';
    END IF;
    SELECT result_retention_seconds INTO keep FROM eacp.tool_contracts
    WHERE tenant_id = a.tenant_id AND id = a.connector_contract_id;
    IF keep IS NULL THEN
        RETURN false; -- the contract keeps no output
    END IF;
    INSERT INTO eacp.action_results
        (tenant_id, action_id, agent_id, contract_id, lease_generation, output, withheld, sha256, bytes, expires_at)
    VALUES (a.tenant_id, a.id, a.agent_id, a.connector_contract_id, a.lease_generation, p_output, p_withheld,
            CASE WHEN p_output IS NOT NULL THEN encode(sha256(convert_to(p_output, 'UTF8')), 'hex') END,
            octet_length(p_output), now() + make_interval(secs => keep));
    RETURN true;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.action_result(p_action uuid)
    RETURNS TABLE (output text, sha256 text, bytes integer, expires_at timestamptz, withheld text)
    LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
#variable_conflict use_column
DECLARE
    who   jsonb := eacp.actor_context();
    agent uuid;
BEGIN
    IF who->>'kind' IS DISTINCT FROM 'agent' THEN
        RAISE EXCEPTION 'only the calling agent reads a result' USING ERRCODE = '42501';
    END IF;
    SELECT v.agent_id INTO agent FROM eacp.agent_versions v
    WHERE v.tenant_id = eacp.current_tenant_id() AND v.id = (who->>'id')::uuid AND v.state = 'ACTIVE';
    IF agent IS NULL THEN
        RAISE EXCEPTION 'only an ACTIVE agent version reads a result' USING ERRCODE = '42501';
    END IF;
    RETURN QUERY
    SELECT r.output, r.sha256, r.bytes, r.expires_at, r.withheld FROM eacp.action_results r
    WHERE r.tenant_id = eacp.current_tenant_id() AND r.action_id = p_action AND r.agent_id = agent
      AND r.expires_at > now() AND r.pruned_at IS NULL;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.action_results_prune(p_batch integer) RETURNS integer
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    n integer;
BEGIN
    IF eacp.current_system_actor() IS DISTINCT FROM 'sweeper' OR eacp.current_actor_id() IS NOT NULL
       OR eacp.current_agent_version_id() IS NOT NULL THEN
        RAISE EXCEPTION 'only the sweeper prunes results' USING ERRCODE = '42501';
    END IF;
    WITH due AS (
        SELECT action_id FROM eacp.action_results
        WHERE tenant_id = eacp.current_tenant_id() AND expires_at <= now() AND pruned_at IS NULL
        ORDER BY expires_at LIMIT greatest(p_batch, 1)
        FOR UPDATE SKIP LOCKED)
    UPDATE eacp.action_results r SET output = NULL, pruned_at = now()
    FROM due WHERE r.tenant_id = eacp.current_tenant_id() AND r.action_id = due.action_id;
    GET DIAGNOSTICS n = ROW_COUNT;
    RETURN n;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.action_result_tenants() RETURNS SETOF uuid
    LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT DISTINCT tenant_id FROM eacp.action_results WHERE expires_at <= now() AND pruned_at IS NULL
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION eacp.action_result_record(uuid, text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.action_result_record(uuid, text, text) TO eacp_app;
REVOKE ALL ON FUNCTION eacp.action_result(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.action_result(uuid) TO eacp_app;
REVOKE ALL ON FUNCTION eacp.action_results_prune(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.action_results_prune(integer) TO eacp_app;
REVOKE ALL ON FUNCTION eacp.action_result_tenants() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.action_result_tenants() TO eacp_app;

-- +goose Down

DROP FUNCTION eacp.action_result_tenants();
DROP FUNCTION eacp.action_results_prune(integer);
DROP FUNCTION eacp.action_result(uuid);
DROP FUNCTION eacp.action_result_record(uuid, text, text);
DROP TABLE eacp.action_results;
ALTER TABLE eacp.tool_contracts DROP COLUMN result_retention_seconds;
