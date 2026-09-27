-- Phase 25a (ADR-030): A2A delegation. An A2A connector is a remote agent
-- reached through its JSON-RPC interface. It reuses the MCP scan machinery
-- (ADR-023): the scanner fetches the Agent Card and records one discovered
-- tool, delegate, whose definition carries the card; a contract certifies
-- that definition, and a changed card quarantines a certified delegate.
-- A2A has no idempotency key and no lookup, so a delegation is sent at
-- most once and an unknown outcome goes to a human. An attempt may keep
-- the remote task id (remote_reference) even when its outcome is not a
-- success. Every function below is its latest definition (00014, 00006)
-- with the A2A rules added; the Down section restores those definitions.
-- +goose Up

ALTER TABLE eacp.connectors DROP CONSTRAINT connectors_protocol_check;
ALTER TABLE eacp.connectors ADD CONSTRAINT connectors_protocol_check CHECK (protocol IN ('http', 'mcp', 'a2a'));

ALTER TABLE eacp.mcp_scans DROP CONSTRAINT mcp_scans_protocol_version_check;
ALTER TABLE eacp.mcp_scans ADD CONSTRAINT mcp_scans_protocol_version_check
    CHECK (protocol_version ~ '^([0-9]{4}-[0-9]{2}-[0-9]{2}|1\.0)$');

-- The remote agent's task id: evidence for the human who settles an
-- unknown outcome, never a proof of success (ADR-030 §6).
ALTER TABLE eacp.action_attempts
    ADD COLUMN remote_reference text CHECK (remote_reference ~ '^[A-Za-z0-9._:-]+$' AND length(remote_reference) <= 512),
    ADD CONSTRAINT remote_reference_not_on_success
        CHECK (remote_reference IS NULL OR outcome IN ('ambiguous', 'no_effect'));
GRANT UPDATE (remote_reference) ON eacp.action_attempts TO eacp_app;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.connectors_mcp_server() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.protocol IN ('mcp', 'a2a') THEN
        INSERT INTO eacp.mcp_servers (tenant_id, connector_id) VALUES (NEW.tenant_id, NEW.id);
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.mcp_servers_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    who  jsonb := eacp.registry_actor();
    kind text  := who->>'kind';
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF pg_trigger_depth() < 2 AND kind <> 'owner' THEN
            RAISE EXCEPTION 'an MCP server row is created with its connector' USING ERRCODE = '42501';
        END IF;
        PERFORM 1 FROM eacp.connectors WHERE tenant_id = NEW.tenant_id AND id = NEW.connector_id
            AND protocol IN ('mcp', 'a2a');
        IF NOT FOUND THEN
            RAISE EXCEPTION 'connector % is not an MCP server', NEW.connector_id USING ERRCODE = '23514';
        END IF;
        NEW.next_scan_at := now();
        NEW.requested_at := NULL;
        NEW.requested_by := NULL;
        NEW.request_reason := NULL;
        NEW.lease_worker := NULL;
        NEW.lease_until := NULL;
        NEW.lease_claimed_at := NULL;
        NEW.lease_generation := 0;
        NEW.last_scan_id := NULL;
        NEW.last_scan_at := NULL;
        NEW.last_outcome := NULL;
        NEW.consecutive_failures := 0;
        RETURN NEW;
    END IF;
    IF kind = 'owner' THEN
        RETURN NEW;
    END IF;
    IF ROW(NEW.tenant_id, NEW.connector_id) IS DISTINCT FROM ROW(OLD.tenant_id, OLD.connector_id) THEN
        RAISE EXCEPTION 'an MCP server row keeps its connector' USING ERRCODE = '55000';
    END IF;

    IF kind = 'principal' THEN
        IF ROW(NEW.next_scan_at, NEW.lease_worker, NEW.lease_until, NEW.lease_claimed_at, NEW.lease_generation,
               NEW.last_scan_id, NEW.last_scan_at, NEW.last_outcome, NEW.consecutive_failures)
           IS DISTINCT FROM
           ROW(OLD.next_scan_at, OLD.lease_worker, OLD.lease_until, OLD.lease_claimed_at, OLD.lease_generation,
               OLD.last_scan_id, OLD.last_scan_at, OLD.last_outcome, OLD.consecutive_failures)
           OR NEW.requested_at IS NULL THEN
            RAISE EXCEPTION 'a principal only requests a rescan' USING ERRCODE = '42501';
        END IF;
        PERFORM eacp.assert_role((who->>'id')::uuid, 'operator', 'registry_editor');
        PERFORM eacp.require_reason(NEW.request_reason, 'a rescan request');
        NEW.requested_at := now();
        NEW.requested_by := (who->>'id')::uuid;
        RETURN NEW;
    END IF;

    -- The scanner.
    IF ROW(NEW.requested_by, NEW.request_reason) IS DISTINCT FROM ROW(OLD.requested_by, OLD.request_reason)
       AND NEW.requested_at IS NOT NULL THEN
        RAISE EXCEPTION 'the scanner does not request rescans' USING ERRCODE = '42501';
    END IF;
    IF NEW.lease_generation IS DISTINCT FROM OLD.lease_generation THEN
        -- A claim.
        IF NEW.lease_generation IS DISTINCT FROM OLD.lease_generation + 1
           OR NEW.lease_generation IS DISTINCT FROM (who->>'generation')::bigint
           OR NEW.lease_worker IS DISTINCT FROM who->>'worker' THEN
            RAISE EXCEPTION 'a scanner claims the next lease generation for itself' USING ERRCODE = '42501';
        END IF;
        IF ROW(NEW.next_scan_at, NEW.requested_at, NEW.last_scan_id, NEW.last_scan_at, NEW.last_outcome,
               NEW.consecutive_failures)
           IS DISTINCT FROM
           ROW(OLD.next_scan_at, OLD.requested_at, OLD.last_scan_id, OLD.last_scan_at, OLD.last_outcome,
               OLD.consecutive_failures) THEN
            RAISE EXCEPTION 'a claim changes only the lease' USING ERRCODE = '42501';
        END IF;
        IF COALESCE(OLD.lease_until > now(), false) THEN
            RAISE EXCEPTION 'the scan lease is held until %', OLD.lease_until USING ERRCODE = '55000';
        END IF;
        IF NOT COALESCE(NEW.lease_until > now() AND NEW.lease_until <= now() + interval '10 minutes', false) THEN
            RAISE EXCEPTION 'a scan lease lasts at most 10 minutes' USING ERRCODE = '23514';
        END IF;
        NEW.lease_claimed_at := now();
        RETURN NEW;
    END IF;

    -- The holder's bookkeeping and release.
    IF OLD.lease_worker IS DISTINCT FROM who->>'worker' OR OLD.lease_generation IS DISTINCT FROM (who->>'generation')::bigint
       OR NOT COALESCE(OLD.lease_until > now(), false) THEN
        RAISE EXCEPTION 'only the scanner holding the scan lease may record' USING ERRCODE = '42501';
    END IF;
    IF NEW.lease_worker IS NOT NULL OR NEW.lease_until IS NOT NULL THEN
        RAISE EXCEPTION 'a lease holder only releases its lease' USING ERRCODE = '42501';
    END IF;
    NEW.lease_claimed_at := NULL;
    IF NEW.next_scan_at < now() OR NEW.next_scan_at > now() + interval '1 day' THEN
        RAISE EXCEPTION 'the next scan is due within a day' USING ERRCODE = '23514';
    END IF;
    IF NEW.last_scan_id IS DISTINCT FROM OLD.last_scan_id THEN
        PERFORM 1 FROM eacp.mcp_scans
        WHERE tenant_id = NEW.tenant_id AND id = NEW.last_scan_id AND connector_id = NEW.connector_id
          AND lease_generation = OLD.lease_generation;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'the last scan must be recorded under this lease' USING ERRCODE = '55000';
        END IF;
        NEW.last_scan_at := now();
    END IF;
    -- A request made after the lease was claimed is not satisfied by it.
    IF NEW.requested_at IS NULL AND OLD.requested_at > OLD.lease_claimed_at THEN
        NEW.requested_at := OLD.requested_at;
        NEW.requested_by := OLD.requested_by;
        NEW.request_reason := OLD.request_reason;
    ELSIF NEW.requested_at IS NULL THEN
        NEW.requested_by := NULL;
        NEW.request_reason := NULL;
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.mcp_scans_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    who   jsonb := eacp.registry_actor();
    proto text;
BEGIN
    IF who->>'kind' = 'owner' THEN
        RETURN NEW;
    END IF;
    IF who->>'kind' <> 'scanner' THEN
        RAISE EXCEPTION 'only a scanner records a scan' USING ERRCODE = '42501';
    END IF;
    PERFORM eacp.assert_scan_lease(NEW.tenant_id, NEW.connector_id);
    -- MCP names a dated revision, A2A its protocol version (ADR-030).
    IF NEW.protocol_version IS NOT NULL THEN
        SELECT protocol INTO proto FROM eacp.connectors WHERE tenant_id = NEW.tenant_id AND id = NEW.connector_id;
        IF (proto = 'a2a') IS DISTINCT FROM (NEW.protocol_version = '1.0') THEN
            RAISE EXCEPTION 'protocol version % is not one of a % connector', NEW.protocol_version, proto
                USING ERRCODE = '23514';
        END IF;
    END IF;
    NEW.worker_id := who->>'worker';
    NEW.lease_generation := (who->>'generation')::bigint;
    NEW.completed_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.mcp_scans_due(p_bindings jsonb, lim integer)
    RETURNS TABLE (tenant_id uuid, connector_id uuid, lease_generation bigint)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT s.tenant_id, s.connector_id, s.lease_generation
      FROM eacp.mcp_servers s
      JOIN eacp.connectors c ON c.tenant_id = s.tenant_id AND c.id = s.connector_id
     WHERE c.protocol IN ('mcp', 'a2a')
       AND (s.requested_at IS NOT NULL OR s.next_scan_at <= now())
       AND (s.lease_until IS NULL OR s.lease_until <= now())
       AND EXISTS (SELECT 1 FROM jsonb_to_recordset(p_bindings) AS b(tenant_id uuid, secret_ref text, host text)
                   WHERE b.tenant_id = s.tenant_id AND b.secret_ref = c.secret_ref
                     AND b.host = lower(substring(c.endpoint FROM '^https?://([^/?#]+)')))
     ORDER BY s.requested_at NULLS LAST, s.next_scan_at, s.tenant_id, s.connector_id
     LIMIT least(greatest(lim, 1), 100)
    $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.mcp_record_scan(p_connector uuid, p_result jsonb) RETURNS uuid
    LANGUAGE plpgsql
    AS $$
DECLARE
    tenant   uuid := eacp.current_tenant_id();
    s        eacp.mcp_servers%ROWTYPE;
    sid      uuid := gen_random_uuid();
    ok       boolean;
    t        jsonb;
    remote   text;
    tl       eacp.tools%ROWTYPE;
    cur      eacp.tool_definitions%ROWTYPE;
    r        text;
    listed   text[] := '{}';
    n_added  integer := 0;
    n_high   integer := 0;
    n_low    integer := 0;
    n_miss   integer := 0;
    every    integer;
BEGIN
    IF eacp.registry_actor()->>'kind' <> 'scanner' THEN
        RAISE EXCEPTION 'only a scanner records a scan' USING ERRCODE = '42501';
    END IF;
    PERFORM eacp.assert_scan_lease(tenant, p_connector);
    SELECT * INTO s FROM eacp.mcp_servers WHERE tenant_id = tenant AND connector_id = p_connector;
    every := least(greatest(COALESCE((p_result->>'interval_s')::integer, 900), 60), 86400);
    ok := p_result->>'outcome' = 'ok';
    IF ok THEN
        IF jsonb_typeof(p_result->'tools') IS DISTINCT FROM 'array' THEN
            RAISE EXCEPTION 'a successful scan lists its tools' USING ERRCODE = '23514';
        END IF;
        -- An A2A agent is one tool, delegate, whatever its card lists (ADR-030 §1).
        IF (SELECT protocol FROM eacp.connectors WHERE tenant_id = tenant AND id = p_connector) = 'a2a'
           AND (jsonb_array_length(p_result->'tools') <> 1
                OR p_result->'tools'->0->>'remote_name' IS DISTINCT FROM 'delegate'
                OR COALESCE(p_result->'rejected', '[]'::jsonb) <> '[]'::jsonb) THEN
            RAISE EXCEPTION 'an A2A scan lists exactly one tool, delegate' USING ERRCODE = '23514';
        END IF;
        FOR t IN SELECT value FROM jsonb_array_elements(p_result->'tools') LOOP
            remote := t->>'remote_name';
            IF remote IS NULL OR remote = ANY (listed) THEN
                RAISE EXCEPTION 'tool % is unnamed or listed twice', remote USING ERRCODE = '23514';
            END IF;
            listed := listed || remote;
            SELECT * INTO tl FROM eacp.tools
            WHERE tenant_id = tenant AND connector_id = p_connector AND remote_name = remote FOR UPDATE;
            IF NOT FOUND THEN
                INSERT INTO eacp.tools (tenant_id, connector_id, name, remote_name)
                VALUES (tenant, p_connector, 'discovered', remote) RETURNING * INTO tl;
                n_added := n_added + 1;
            END IF;
            cur := NULL;
            IF tl.definition_id IS NOT NULL THEN
                SELECT * INTO cur FROM eacp.tool_definitions WHERE tenant_id = tenant AND id = tl.definition_id;
            END IF;
            IF cur.id IS NULL
               OR cur.fingerprint IS DISTINCT FROM sha256(convert_to(t->>'definition', 'UTF8'))
               OR cur.display_digest IS DISTINCT FROM sha256(convert_to(t->>'display', 'UTF8')) THEN
                INSERT INTO eacp.tool_definitions (tenant_id, tool_id, scan_id, definition, display)
                VALUES (tenant, tl.id, sid, t->>'definition', t->>'display') RETURNING risk INTO r;
                n_high := n_high + (r = 'high')::integer;
                n_low := n_low + (r = 'low')::integer;
            END IF;
            IF tl.missing_since IS NOT NULL THEN
                UPDATE eacp.tools SET missing_since = NULL WHERE tenant_id = tenant AND id = tl.id;
            END IF;
        END LOOP;
        FOR tl IN SELECT * FROM eacp.tools
                  WHERE tenant_id = tenant AND connector_id = p_connector AND missing_since IS NULL
                    AND NOT (remote_name = ANY (listed))
                  ORDER BY id FOR UPDATE LOOP
            UPDATE eacp.tools SET missing_since = now() WHERE tenant_id = tenant AND id = tl.id;
            n_miss := n_miss + 1;
        END LOOP;
    ELSIF p_result->>'outcome' IS DISTINCT FROM 'failed' THEN
        RAISE EXCEPTION 'a scan outcome is ok or failed' USING ERRCODE = '23514';
    END IF;

    INSERT INTO eacp.mcp_scans (tenant_id, id, connector_id, worker_id, lease_generation, outcome, error_class,
                                protocol_version, server_info, tools_listed, tools_added, definitions_changed,
                                metadata_changed, tools_missing, rejected)
    VALUES (tenant, sid, p_connector, '', 0, p_result->>'outcome', CASE WHEN NOT ok THEN p_result->>'error_class' END,
            CASE WHEN ok THEN p_result->>'protocol_version' END,
            CASE WHEN ok AND jsonb_typeof(p_result->'server_info') = 'object' THEN p_result->'server_info' END,
            cardinality(listed), n_added, n_high, n_low, n_miss,
            CASE WHEN ok THEN COALESCE(p_result->'rejected', '[]'::jsonb) ELSE '[]'::jsonb END);

    UPDATE eacp.mcp_servers
       SET lease_worker = NULL, lease_until = NULL, last_scan_id = sid,
           last_outcome = p_result->>'outcome',
           consecutive_failures = CASE WHEN ok THEN 0 ELSE s.consecutive_failures + 1 END,
           next_scan_at = now() + make_interval(secs => CASE WHEN ok THEN every
                              ELSE least(every, 30 * power(2, least(s.consecutive_failures, 10))) END),
           requested_at = NULL
     WHERE tenant_id = tenant AND connector_id = p_connector;
    RETURN sid;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.tool_definitions_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    who  jsonb := eacp.registry_actor();
    tl   eacp.tools%ROWTYPE;
    prev eacp.tool_definitions%ROWTYPE;
    d    jsonb;
    disp jsonb;
    pd   jsonb;
    pdisp jsonb;
    a    jsonb;
BEGIN
    IF who->>'kind' NOT IN ('scanner', 'owner') THEN
        RAISE EXCEPTION 'only a scanner records a tool definition' USING ERRCODE = '42501';
    END IF;
    IF NEW.fingerprint IS NOT NULL OR NEW.display_digest IS NOT NULL OR NEW.risk IS NOT NULL
       OR NEW.read_only IS NOT NULL OR NEW.destructive IS NOT NULL OR NEW.idempotent IS NOT NULL
       OR NEW.open_world IS NOT NULL OR NEW.seq IS NOT NULL OR NEW.changes <> '{}' THEN
        RAISE EXCEPTION 'digests, risk and hints are computed by the database' USING ERRCODE = '42501';
    END IF;
    SELECT * INTO tl FROM eacp.tools WHERE tenant_id = NEW.tenant_id AND id = NEW.tool_id FOR UPDATE;
    IF NOT FOUND OR tl.origin <> 'discovered' THEN
        RAISE EXCEPTION 'tool % is not a discovered MCP tool', NEW.tool_id USING ERRCODE = '55000';
    END IF;
    IF who->>'kind' = 'scanner' THEN
        PERFORM eacp.assert_scan_lease(NEW.tenant_id, tl.connector_id);
    END IF;

    d := NEW.definition::jsonb;
    disp := NEW.display::jsonb;
    IF jsonb_typeof(d) <> 'object' OR d->>'name' IS DISTINCT FROM tl.remote_name
       OR jsonb_typeof(d->'inputSchema') IS DISTINCT FROM 'object'
       OR d->'inputSchema'->>'type' IS DISTINCT FROM 'object' THEN
        RAISE EXCEPTION 'a definition is an object named % with an object inputSchema', tl.remote_name
            USING ERRCODE = '23514';
    END IF;
    -- An A2A delegate's definition carries the Agent Card it certifies (ADR-030).
    IF jsonb_typeof(d->'agentCard') IS DISTINCT FROM 'object'
       AND (SELECT protocol FROM eacp.connectors WHERE tenant_id = tl.tenant_id AND id = tl.connector_id) = 'a2a' THEN
        RAISE EXCEPTION 'an A2A delegate definition carries its Agent Card' USING ERRCODE = '23514';
    END IF;
    IF jsonb_typeof(disp) <> 'object' THEN
        RAISE EXCEPTION 'display metadata is an object' USING ERRCODE = '23514';
    END IF;

    NEW.fingerprint := sha256(convert_to(NEW.definition, 'UTF8'));
    NEW.display_digest := sha256(convert_to(NEW.display, 'UTF8'));
    SELECT * INTO prev FROM eacp.tool_definitions WHERE tenant_id = NEW.tenant_id AND id = tl.definition_id;
    IF prev.id IS NULL THEN
        NEW.seq := 1;
        NEW.risk := 'initial';
        NEW.changes := '{}';
    ELSE
        IF prev.fingerprint = NEW.fingerprint AND prev.display_digest = NEW.display_digest THEN
            RAISE EXCEPTION 'the definition of tool % did not change', tl.id USING ERRCODE = '55000';
        END IF;
        pd := prev.definition::jsonb;
        pdisp := prev.display::jsonb;
        NEW.seq := prev.seq + 1;
        NEW.risk := CASE WHEN prev.fingerprint = NEW.fingerprint THEN 'low' ELSE 'high' END;
        NEW.changes := ARRAY(
            SELECT k FROM (SELECT jsonb_object_keys(pd) UNION SELECT jsonb_object_keys(d)) AS x(k)
             WHERE pd->k IS DISTINCT FROM d->k
            UNION
            SELECT k FROM (SELECT jsonb_object_keys(pdisp) UNION SELECT jsonb_object_keys(disp)) AS y(k)
             WHERE pdisp->k IS DISTINCT FROM disp->k
            ORDER BY 1);
    END IF;

    a := CASE WHEN jsonb_typeof(d->'annotations') = 'object' THEN d->'annotations' ELSE '{}'::jsonb END;
    NEW.read_only := CASE WHEN jsonb_typeof(a->'readOnlyHint') = 'boolean' THEN (a->'readOnlyHint')::boolean ELSE false END;
    NEW.destructive := CASE WHEN NEW.read_only THEN false
                            WHEN jsonb_typeof(a->'destructiveHint') = 'boolean' THEN (a->'destructiveHint')::boolean
                            ELSE true END;
    NEW.idempotent := CASE WHEN NEW.read_only THEN true
                           WHEN jsonb_typeof(a->'idempotentHint') = 'boolean' THEN (a->'idempotentHint')::boolean
                           ELSE false END;
    NEW.open_world := CASE WHEN jsonb_typeof(a->'openWorldHint') = 'boolean' THEN (a->'openWorldHint')::boolean ELSE true END;
    NEW.observed_at := now();
    NEW.observed_by := COALESCE(who->>'worker', 'owner');
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.tool_fingerprint(p_tenant uuid, p_tool uuid) RETURNS bytea
    LANGUAGE sql STABLE
    AS $$
    SELECT CASE WHEN c.protocol IN ('mcp', 'a2a') THEN
                CASE WHEN d.id IS NOT NULL THEN
                     sha256(convert_to(concat_ws(E'\n', 'eacp-tool-v2', c.protocol, c.endpoint, c.secret_ref,
                                                 c.name, t.name, t.remote_name, encode(d.fingerprint, 'hex')), 'UTF8'))
                END
           ELSE sha256(convert_to(concat_ws(E'\n', 'eacp-tool-v1',
                       c.protocol, c.endpoint, c.secret_ref, c.name, t.name), 'UTF8'))
           END
    FROM eacp.tools t
    JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
    LEFT JOIN eacp.tool_definitions d ON d.tenant_id = t.tenant_id AND d.id = t.definition_id
    WHERE t.tenant_id = p_tenant AND t.id = p_tool
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.tools_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    who   jsonb := eacp.registry_actor();
    kind  text  := who->>'kind';
    a     uuid;
    c     eacp.tool_contracts%ROWTYPE;
    proto text;
BEGIN
    IF TG_OP = 'INSERT' THEN
        SELECT protocol INTO proto FROM eacp.connectors WHERE tenant_id = NEW.tenant_id AND id = NEW.connector_id;
        NEW.definition_id := NULL;
        NEW.missing_since := NULL;
        NEW.quarantined_at := NULL;
        NEW.quarantine_reason := NULL;
        NEW.quarantine_changed_at := NULL;
        NEW.quarantine_changed_by := NULL;
        NEW.quarantine_changed_by_worker := NULL;
        NEW.contract_changed_by := NULL;
        NEW.contract_changed_at := NULL;
        NEW.created_at := now();
        IF NEW.active_contract_id IS NOT NULL THEN
            RAISE EXCEPTION 'a tool is registered without a contract' USING ERRCODE = '55000';
        END IF;
        IF kind = 'scanner' THEN
            IF proto IS NULL OR proto NOT IN ('mcp', 'a2a') THEN
                RAISE EXCEPTION 'the scanner discovers MCP and A2A tools only' USING ERRCODE = '42501';
            END IF;
            PERFORM eacp.assert_scan_lease(NEW.tenant_id, NEW.connector_id);
            IF NEW.remote_name IS NULL THEN
                RAISE EXCEPTION 'a discovered tool keeps its MCP name' USING ERRCODE = '23514';
            END IF;
            IF proto = 'a2a' AND NEW.remote_name <> 'delegate' THEN
                RAISE EXCEPTION 'an A2A agent has one tool, delegate' USING ERRCODE = '23514';
            END IF;
            NEW.name := eacp.mcp_tool_name(NEW.remote_name);
            NEW.origin := 'discovered';
            NEW.created_by := NULL;
            RETURN NEW;
        END IF;
        a := eacp.actor();
        PERFORM eacp.assert_role(a, 'registry_editor');
        IF proto IN ('mcp', 'a2a') AND kind <> 'owner' THEN
            RAISE EXCEPTION 'MCP and A2A tools are discovered, not declared' USING ERRCODE = '55000';
        END IF;
        IF kind <> 'owner' THEN
            NEW.origin := 'registered';
            NEW.remote_name := NULL;
        END IF;
        NEW.created_by := a;
        RETURN NEW;
    END IF;

    -- The current definition moves only when the scanner records one.
    IF NEW.definition_id IS DISTINCT FROM OLD.definition_id THEN
        IF pg_trigger_depth() < 2 AND kind <> 'owner' THEN
            RAISE EXCEPTION 'a tool''s definition changes only when one is recorded' USING ERRCODE = '42501';
        END IF;
    END IF;

    -- Listed or not (the scanner holding the lease).
    IF NEW.missing_since IS DISTINCT FROM OLD.missing_since AND kind <> 'owner' THEN
        IF kind <> 'scanner' THEN
            RAISE EXCEPTION 'only the scanner marks a tool as missing' USING ERRCODE = '42501';
        END IF;
        PERFORM eacp.assert_scan_lease(NEW.tenant_id, NEW.connector_id);
        IF NEW.missing_since IS NOT NULL THEN
            NEW.missing_since := COALESCE(OLD.missing_since, now());
            IF OLD.active_contract_id IS NOT NULL AND OLD.quarantined_at IS NULL AND NEW.quarantined_at IS NULL THEN
                NEW.quarantined_at := now();
                NEW.quarantine_reason := 'no longer listed by the server';
            END IF;
        END IF;
    END IF;

    -- Quarantine and release.
    IF kind <> 'owner'
       AND ROW(NEW.quarantined_at IS NULL, NEW.quarantine_reason, NEW.quarantine_changed_at, NEW.quarantine_changed_by,
               NEW.quarantine_changed_by_worker)
           IS DISTINCT FROM
           ROW(OLD.quarantined_at IS NULL, OLD.quarantine_reason, OLD.quarantine_changed_at, OLD.quarantine_changed_by,
               OLD.quarantine_changed_by_worker) THEN
        IF (NEW.quarantined_at IS NULL) = (OLD.quarantined_at IS NULL) THEN
            RAISE EXCEPTION 'quarantine fields change only when a tool is quarantined or released'
                USING ERRCODE = '55000';
        END IF;
        PERFORM eacp.require_reason(NEW.quarantine_reason, 'a quarantine change');
        IF NEW.quarantined_at IS NOT NULL THEN
            IF kind = 'scanner' THEN
                IF pg_trigger_depth() < 2 AND NEW.missing_since IS NOT DISTINCT FROM OLD.missing_since THEN
                    RAISE EXCEPTION 'the scanner quarantines a tool only on drift' USING ERRCODE = '42501';
                END IF;
                NEW.quarantine_changed_by := NULL;
                NEW.quarantine_changed_by_worker := who->>'worker';
            ELSE
                PERFORM eacp.assert_role((who->>'id')::uuid, 'operator', 'registry_approver');
                NEW.quarantine_changed_by := (who->>'id')::uuid;
                NEW.quarantine_changed_by_worker := NULL;
            END IF;
            NEW.quarantined_at := now();
        ELSE
            IF kind <> 'principal' THEN
                RAISE EXCEPTION 'only a registry approver releases a quarantine' USING ERRCODE = '42501';
            END IF;
            PERFORM eacp.assert_role((who->>'id')::uuid, 'registry_approver');
            PERFORM eacp.assert_distinct((who->>'id')::uuid, OLD.quarantine_changed_by, 'the principal who quarantined it');
            NEW.quarantine_changed_by := (who->>'id')::uuid;
            NEW.quarantine_changed_by_worker := NULL;
        END IF;
        NEW.quarantine_changed_at := now();
    ELSIF NEW.quarantined_at IS DISTINCT FROM OLD.quarantined_at AND kind <> 'owner' THEN
        NEW.quarantined_at := OLD.quarantined_at;  -- the timestamp is the database's
    END IF;

    IF NEW.active_contract_id IS NOT DISTINCT FROM OLD.active_contract_id THEN
        RETURN NEW;
    END IF;
    a := eacp.actor();
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
    IF c.fingerprint IS DISTINCT FROM eacp.tool_fingerprint(NEW.tenant_id, NEW.id) THEN
        RAISE EXCEPTION 'contract % does not match the tool''s current definition', c.id USING ERRCODE = '55000';
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
CREATE OR REPLACE FUNCTION eacp.tool_contracts_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a     uuid := eacp.actor();
    tl    eacp.tools%ROWTYPE;
    proto text;
    d     eacp.tool_definitions%ROWTYPE;
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM eacp.assert_role(a, 'registry_editor', 'registry_approver');
        IF NEW.revoked_at IS NOT NULL OR NEW.revoked_by IS NOT NULL OR NEW.revoke_reason IS NOT NULL THEN
            RAISE EXCEPTION 'a contract cannot be created revoked' USING ERRCODE = '23514';
        END IF;
        -- Serialise version numbering per tool on the tool row.
        SELECT * INTO tl FROM eacp.tools WHERE tenant_id = NEW.tenant_id AND id = NEW.tool_id FOR NO KEY UPDATE;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'unknown tool %', NEW.tool_id USING ERRCODE = '23503';
        END IF;
        SELECT COALESCE(max(version), 0) + 1 INTO NEW.version
        FROM eacp.tool_contracts WHERE tenant_id = NEW.tenant_id AND tool_id = NEW.tool_id;
        NEW.side_effects     := ARRAY(SELECT DISTINCT e FROM unnest(NEW.side_effects) e ORDER BY e);
        NEW.no_effect_errors := ARRAY(SELECT DISTINCT e FROM unnest(NEW.no_effect_errors) e ORDER BY e);
        -- An MCP tool's contract certifies the definition its proposer
        -- reviewed, which must still be current (ADR-023 §5, §6).
        SELECT protocol INTO proto FROM eacp.connectors WHERE tenant_id = tl.tenant_id AND id = tl.connector_id;
        IF proto IN ('mcp', 'a2a') THEN
            IF NEW.definition_id IS NULL OR NEW.definition_id IS DISTINCT FROM tl.definition_id THEN
                RAISE EXCEPTION 'a contract must pin tool %''s current definition; review it again', tl.id
                    USING ERRCODE = '55000';
            END IF;
            -- A2A has no idempotency key and no lookup: a delegation is sent
            -- at most once, and only what the connector proves is no-effect
            -- (ADR-030 §5).
            IF proto = 'a2a' THEN
                IF 'READ_ONLY' = ANY (NEW.side_effects) THEN
                    RAISE EXCEPTION 'a delegation is never read-only' USING ERRCODE = '23514';
                END IF;
                IF NEW.idempotency_mode <> 'none' THEN
                    RAISE EXCEPTION 'A2A defines no idempotency key: a delegation is sent at most once'
                        USING ERRCODE = '23514';
                END IF;
                IF EXISTS (SELECT 1 FROM unnest(NEW.no_effect_errors) e
                            WHERE e !~ '^(a2a_rejected|connection_refused_before_send|unauthorized|a2a_rpc_[0-9]{1,6})$') THEN
                    RAISE EXCEPTION 'an A2A contract certifies as no-effect only what the connector proves'
                        USING ERRCODE = '23514';
                END IF;
            END IF;
            SELECT * INTO d FROM eacp.tool_definitions WHERE tenant_id = NEW.tenant_id AND id = NEW.definition_id;
            IF NEW.side_effects = ARRAY['READ_ONLY'] AND NOT d.read_only THEN
                RAISE EXCEPTION 'the server does not declare tool % read-only', tl.id USING ERRCODE = '23514';
            END IF;
            IF NEW.reconciliation_lookup <> 'none' THEN
                RAISE EXCEPTION 'MCP and A2A define no lookup by operation key' USING ERRCODE = '23514';
            END IF;
        ELSIF NEW.definition_id IS NOT NULL THEN
            RAISE EXCEPTION 'only a discovered tool''s contract pins a definition' USING ERRCODE = '23514';
        END IF;
        NEW.fingerprint      := eacp.tool_fingerprint(NEW.tenant_id, NEW.tool_id);
        NEW.created_by       := a;
        NEW.created_at       := now();
        RETURN NEW;
    END IF;

    IF NEW.definition_id IS DISTINCT FROM OLD.definition_id THEN
        RAISE EXCEPTION 'a contract''s definition is immutable' USING ERRCODE = '55000';
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

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.action_attempts_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    who jsonb := eacp.actor_context();
    a   eacp.actions%ROWTYPE;
    ct  eacp.tool_contracts%ROWTYPE;
BEGIN
    SELECT * INTO a FROM eacp.actions
    WHERE tenant_id = NEW.tenant_id AND id = NEW.action_id FOR SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'an attempt requires an action' USING ERRCODE = '23503';
    END IF;
    IF TG_OP = 'INSERT' THEN
        -- Only the dispatch intent of this generation creates an attempt.
        PERFORM eacp.assert_lease_holder(who, a);
        IF a.state <> 'EXECUTING' OR a.dispatch_intent_at IS DISTINCT FROM now()
           OR NEW.lease_generation <> a.lease_generation OR NEW.attempt_no <> a.attempt_count
           OR num_nonnulls(NEW.completed_at, NEW.outcome, NEW.external_reference, NEW.error_class,
                         NEW.remote_reference) > 0
           OR NEW.late THEN
            RAISE EXCEPTION 'an attempt is created by its dispatch intent' USING ERRCODE = '55000';
        END IF;
        NEW.worker_id := a.worker_id;
        NEW.operation_key := a.operation_key;
        NEW.connector_contract_id := a.connector_contract_id;
        NEW.connector_contract_version := a.connector_contract_version;
        NEW.enforced_digest := a.enforced_digest;
        NEW.dispatched_at := now();
        NEW.call_deadline := now() + eacp.call_timeout(a.connector_contract_id);
        RETURN NEW;
    END IF;

    -- Completion: once, by the worker of this attempt's generation, with a
    -- classification the pinned contract certifies.
    IF OLD.completed_at IS NOT NULL THEN
        RAISE EXCEPTION 'an attempt completes once' USING ERRCODE = '55000';
    END IF;
    IF (to_jsonb(NEW) - ARRAY['outcome', 'external_reference', 'error_class', 'remote_reference', 'completed_at', 'late'])
       IS DISTINCT FROM
       (to_jsonb(OLD) - ARRAY['outcome', 'external_reference', 'error_class', 'remote_reference', 'completed_at', 'late']) THEN
        RAISE EXCEPTION 'an attempt records only its outcome' USING ERRCODE = '55000';
    END IF;
    IF who->>'component' IS DISTINCT FROM 'worker' OR who->>'worker' IS DISTINCT FROM OLD.worker_id
       OR eacp.current_lease_generation() IS DISTINCT FROM OLD.lease_generation THEN
        RAISE EXCEPTION 'only the worker of this attempt records its outcome' USING ERRCODE = '42501';
    END IF;
    IF NEW.outcome = 'no_effect' THEN
        SELECT * INTO ct FROM eacp.tool_contracts
        WHERE tenant_id = NEW.tenant_id AND id = OLD.connector_contract_id;
        IF NOT (NEW.error_class = ANY (ct.no_effect_errors)) THEN
            RAISE EXCEPTION 'error class % is not certified as no-effect', NEW.error_class USING ERRCODE = '23514';
        END IF;
    ELSIF NEW.outcome IS NULL THEN
        RAISE EXCEPTION 'an attempt completes with an outcome' USING ERRCODE = '23514';
    END IF;
    NEW.completed_at := now();
    NEW.late := NOT (a.state = 'EXECUTING' AND a.lease_generation = OLD.lease_generation);
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.connectors_mcp_server() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.protocol = 'mcp' THEN
        INSERT INTO eacp.mcp_servers (tenant_id, connector_id) VALUES (NEW.tenant_id, NEW.id);
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.mcp_servers_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    who  jsonb := eacp.registry_actor();
    kind text  := who->>'kind';
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF pg_trigger_depth() < 2 AND kind <> 'owner' THEN
            RAISE EXCEPTION 'an MCP server row is created with its connector' USING ERRCODE = '42501';
        END IF;
        PERFORM 1 FROM eacp.connectors WHERE tenant_id = NEW.tenant_id AND id = NEW.connector_id AND protocol = 'mcp';
        IF NOT FOUND THEN
            RAISE EXCEPTION 'connector % is not an MCP server', NEW.connector_id USING ERRCODE = '23514';
        END IF;
        NEW.next_scan_at := now();
        NEW.requested_at := NULL;
        NEW.requested_by := NULL;
        NEW.request_reason := NULL;
        NEW.lease_worker := NULL;
        NEW.lease_until := NULL;
        NEW.lease_claimed_at := NULL;
        NEW.lease_generation := 0;
        NEW.last_scan_id := NULL;
        NEW.last_scan_at := NULL;
        NEW.last_outcome := NULL;
        NEW.consecutive_failures := 0;
        RETURN NEW;
    END IF;
    IF kind = 'owner' THEN
        RETURN NEW;
    END IF;
    IF ROW(NEW.tenant_id, NEW.connector_id) IS DISTINCT FROM ROW(OLD.tenant_id, OLD.connector_id) THEN
        RAISE EXCEPTION 'an MCP server row keeps its connector' USING ERRCODE = '55000';
    END IF;

    IF kind = 'principal' THEN
        IF ROW(NEW.next_scan_at, NEW.lease_worker, NEW.lease_until, NEW.lease_claimed_at, NEW.lease_generation,
               NEW.last_scan_id, NEW.last_scan_at, NEW.last_outcome, NEW.consecutive_failures)
           IS DISTINCT FROM
           ROW(OLD.next_scan_at, OLD.lease_worker, OLD.lease_until, OLD.lease_claimed_at, OLD.lease_generation,
               OLD.last_scan_id, OLD.last_scan_at, OLD.last_outcome, OLD.consecutive_failures)
           OR NEW.requested_at IS NULL THEN
            RAISE EXCEPTION 'a principal only requests a rescan' USING ERRCODE = '42501';
        END IF;
        PERFORM eacp.assert_role((who->>'id')::uuid, 'operator', 'registry_editor');
        PERFORM eacp.require_reason(NEW.request_reason, 'a rescan request');
        NEW.requested_at := now();
        NEW.requested_by := (who->>'id')::uuid;
        RETURN NEW;
    END IF;

    -- The scanner.
    IF ROW(NEW.requested_by, NEW.request_reason) IS DISTINCT FROM ROW(OLD.requested_by, OLD.request_reason)
       AND NEW.requested_at IS NOT NULL THEN
        RAISE EXCEPTION 'the scanner does not request rescans' USING ERRCODE = '42501';
    END IF;
    IF NEW.lease_generation IS DISTINCT FROM OLD.lease_generation THEN
        -- A claim.
        IF NEW.lease_generation IS DISTINCT FROM OLD.lease_generation + 1
           OR NEW.lease_generation IS DISTINCT FROM (who->>'generation')::bigint
           OR NEW.lease_worker IS DISTINCT FROM who->>'worker' THEN
            RAISE EXCEPTION 'a scanner claims the next lease generation for itself' USING ERRCODE = '42501';
        END IF;
        IF ROW(NEW.next_scan_at, NEW.requested_at, NEW.last_scan_id, NEW.last_scan_at, NEW.last_outcome,
               NEW.consecutive_failures)
           IS DISTINCT FROM
           ROW(OLD.next_scan_at, OLD.requested_at, OLD.last_scan_id, OLD.last_scan_at, OLD.last_outcome,
               OLD.consecutive_failures) THEN
            RAISE EXCEPTION 'a claim changes only the lease' USING ERRCODE = '42501';
        END IF;
        IF COALESCE(OLD.lease_until > now(), false) THEN
            RAISE EXCEPTION 'the scan lease is held until %', OLD.lease_until USING ERRCODE = '55000';
        END IF;
        IF NOT COALESCE(NEW.lease_until > now() AND NEW.lease_until <= now() + interval '10 minutes', false) THEN
            RAISE EXCEPTION 'a scan lease lasts at most 10 minutes' USING ERRCODE = '23514';
        END IF;
        NEW.lease_claimed_at := now();
        RETURN NEW;
    END IF;

    -- The holder's bookkeeping and release.
    IF OLD.lease_worker IS DISTINCT FROM who->>'worker' OR OLD.lease_generation IS DISTINCT FROM (who->>'generation')::bigint
       OR NOT COALESCE(OLD.lease_until > now(), false) THEN
        RAISE EXCEPTION 'only the scanner holding the scan lease may record' USING ERRCODE = '42501';
    END IF;
    IF NEW.lease_worker IS NOT NULL OR NEW.lease_until IS NOT NULL THEN
        RAISE EXCEPTION 'a lease holder only releases its lease' USING ERRCODE = '42501';
    END IF;
    NEW.lease_claimed_at := NULL;
    IF NEW.next_scan_at < now() OR NEW.next_scan_at > now() + interval '1 day' THEN
        RAISE EXCEPTION 'the next scan is due within a day' USING ERRCODE = '23514';
    END IF;
    IF NEW.last_scan_id IS DISTINCT FROM OLD.last_scan_id THEN
        PERFORM 1 FROM eacp.mcp_scans
        WHERE tenant_id = NEW.tenant_id AND id = NEW.last_scan_id AND connector_id = NEW.connector_id
          AND lease_generation = OLD.lease_generation;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'the last scan must be recorded under this lease' USING ERRCODE = '55000';
        END IF;
        NEW.last_scan_at := now();
    END IF;
    -- A request made after the lease was claimed is not satisfied by it.
    IF NEW.requested_at IS NULL AND OLD.requested_at > OLD.lease_claimed_at THEN
        NEW.requested_at := OLD.requested_at;
        NEW.requested_by := OLD.requested_by;
        NEW.request_reason := OLD.request_reason;
    ELSIF NEW.requested_at IS NULL THEN
        NEW.requested_by := NULL;
        NEW.request_reason := NULL;
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.mcp_scans_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    who jsonb := eacp.registry_actor();
BEGIN
    IF who->>'kind' = 'owner' THEN
        RETURN NEW;
    END IF;
    IF who->>'kind' <> 'scanner' THEN
        RAISE EXCEPTION 'only a scanner records a scan' USING ERRCODE = '42501';
    END IF;
    PERFORM eacp.assert_scan_lease(NEW.tenant_id, NEW.connector_id);
    NEW.worker_id := who->>'worker';
    NEW.lease_generation := (who->>'generation')::bigint;
    NEW.completed_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.mcp_scans_due(p_bindings jsonb, lim integer)
    RETURNS TABLE (tenant_id uuid, connector_id uuid, lease_generation bigint)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT s.tenant_id, s.connector_id, s.lease_generation
      FROM eacp.mcp_servers s
      JOIN eacp.connectors c ON c.tenant_id = s.tenant_id AND c.id = s.connector_id
     WHERE c.protocol = 'mcp'
       AND (s.requested_at IS NOT NULL OR s.next_scan_at <= now())
       AND (s.lease_until IS NULL OR s.lease_until <= now())
       AND EXISTS (SELECT 1 FROM jsonb_to_recordset(p_bindings) AS b(tenant_id uuid, secret_ref text, host text)
                   WHERE b.tenant_id = s.tenant_id AND b.secret_ref = c.secret_ref
                     AND b.host = lower(substring(c.endpoint FROM '^https?://([^/?#]+)')))
     ORDER BY s.requested_at NULLS LAST, s.next_scan_at, s.tenant_id, s.connector_id
     LIMIT least(greatest(lim, 1), 100)
    $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.mcp_record_scan(p_connector uuid, p_result jsonb) RETURNS uuid
    LANGUAGE plpgsql
    AS $$
DECLARE
    tenant   uuid := eacp.current_tenant_id();
    s        eacp.mcp_servers%ROWTYPE;
    sid      uuid := gen_random_uuid();
    ok       boolean;
    t        jsonb;
    remote   text;
    tl       eacp.tools%ROWTYPE;
    cur      eacp.tool_definitions%ROWTYPE;
    r        text;
    listed   text[] := '{}';
    n_added  integer := 0;
    n_high   integer := 0;
    n_low    integer := 0;
    n_miss   integer := 0;
    every    integer;
BEGIN
    IF eacp.registry_actor()->>'kind' <> 'scanner' THEN
        RAISE EXCEPTION 'only a scanner records a scan' USING ERRCODE = '42501';
    END IF;
    PERFORM eacp.assert_scan_lease(tenant, p_connector);
    SELECT * INTO s FROM eacp.mcp_servers WHERE tenant_id = tenant AND connector_id = p_connector;
    every := least(greatest(COALESCE((p_result->>'interval_s')::integer, 900), 60), 86400);
    ok := p_result->>'outcome' = 'ok';
    IF ok THEN
        IF jsonb_typeof(p_result->'tools') IS DISTINCT FROM 'array' THEN
            RAISE EXCEPTION 'a successful scan lists its tools' USING ERRCODE = '23514';
        END IF;
        FOR t IN SELECT value FROM jsonb_array_elements(p_result->'tools') LOOP
            remote := t->>'remote_name';
            IF remote IS NULL OR remote = ANY (listed) THEN
                RAISE EXCEPTION 'tool % is unnamed or listed twice', remote USING ERRCODE = '23514';
            END IF;
            listed := listed || remote;
            SELECT * INTO tl FROM eacp.tools
            WHERE tenant_id = tenant AND connector_id = p_connector AND remote_name = remote FOR UPDATE;
            IF NOT FOUND THEN
                INSERT INTO eacp.tools (tenant_id, connector_id, name, remote_name)
                VALUES (tenant, p_connector, 'discovered', remote) RETURNING * INTO tl;
                n_added := n_added + 1;
            END IF;
            cur := NULL;
            IF tl.definition_id IS NOT NULL THEN
                SELECT * INTO cur FROM eacp.tool_definitions WHERE tenant_id = tenant AND id = tl.definition_id;
            END IF;
            IF cur.id IS NULL
               OR cur.fingerprint IS DISTINCT FROM sha256(convert_to(t->>'definition', 'UTF8'))
               OR cur.display_digest IS DISTINCT FROM sha256(convert_to(t->>'display', 'UTF8')) THEN
                INSERT INTO eacp.tool_definitions (tenant_id, tool_id, scan_id, definition, display)
                VALUES (tenant, tl.id, sid, t->>'definition', t->>'display') RETURNING risk INTO r;
                n_high := n_high + (r = 'high')::integer;
                n_low := n_low + (r = 'low')::integer;
            END IF;
            IF tl.missing_since IS NOT NULL THEN
                UPDATE eacp.tools SET missing_since = NULL WHERE tenant_id = tenant AND id = tl.id;
            END IF;
        END LOOP;
        FOR tl IN SELECT * FROM eacp.tools
                  WHERE tenant_id = tenant AND connector_id = p_connector AND missing_since IS NULL
                    AND NOT (remote_name = ANY (listed))
                  ORDER BY id FOR UPDATE LOOP
            UPDATE eacp.tools SET missing_since = now() WHERE tenant_id = tenant AND id = tl.id;
            n_miss := n_miss + 1;
        END LOOP;
    ELSIF p_result->>'outcome' IS DISTINCT FROM 'failed' THEN
        RAISE EXCEPTION 'a scan outcome is ok or failed' USING ERRCODE = '23514';
    END IF;

    INSERT INTO eacp.mcp_scans (tenant_id, id, connector_id, worker_id, lease_generation, outcome, error_class,
                                protocol_version, server_info, tools_listed, tools_added, definitions_changed,
                                metadata_changed, tools_missing, rejected)
    VALUES (tenant, sid, p_connector, '', 0, p_result->>'outcome', CASE WHEN NOT ok THEN p_result->>'error_class' END,
            CASE WHEN ok THEN p_result->>'protocol_version' END,
            CASE WHEN ok AND jsonb_typeof(p_result->'server_info') = 'object' THEN p_result->'server_info' END,
            cardinality(listed), n_added, n_high, n_low, n_miss,
            CASE WHEN ok THEN COALESCE(p_result->'rejected', '[]'::jsonb) ELSE '[]'::jsonb END);

    UPDATE eacp.mcp_servers
       SET lease_worker = NULL, lease_until = NULL, last_scan_id = sid,
           last_outcome = p_result->>'outcome',
           consecutive_failures = CASE WHEN ok THEN 0 ELSE s.consecutive_failures + 1 END,
           next_scan_at = now() + make_interval(secs => CASE WHEN ok THEN every
                              ELSE least(every, 30 * power(2, least(s.consecutive_failures, 10))) END),
           requested_at = NULL
     WHERE tenant_id = tenant AND connector_id = p_connector;
    RETURN sid;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.tool_definitions_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    who  jsonb := eacp.registry_actor();
    tl   eacp.tools%ROWTYPE;
    prev eacp.tool_definitions%ROWTYPE;
    d    jsonb;
    disp jsonb;
    pd   jsonb;
    pdisp jsonb;
    a    jsonb;
BEGIN
    IF who->>'kind' NOT IN ('scanner', 'owner') THEN
        RAISE EXCEPTION 'only a scanner records a tool definition' USING ERRCODE = '42501';
    END IF;
    IF NEW.fingerprint IS NOT NULL OR NEW.display_digest IS NOT NULL OR NEW.risk IS NOT NULL
       OR NEW.read_only IS NOT NULL OR NEW.destructive IS NOT NULL OR NEW.idempotent IS NOT NULL
       OR NEW.open_world IS NOT NULL OR NEW.seq IS NOT NULL OR NEW.changes <> '{}' THEN
        RAISE EXCEPTION 'digests, risk and hints are computed by the database' USING ERRCODE = '42501';
    END IF;
    SELECT * INTO tl FROM eacp.tools WHERE tenant_id = NEW.tenant_id AND id = NEW.tool_id FOR UPDATE;
    IF NOT FOUND OR tl.origin <> 'discovered' THEN
        RAISE EXCEPTION 'tool % is not a discovered MCP tool', NEW.tool_id USING ERRCODE = '55000';
    END IF;
    IF who->>'kind' = 'scanner' THEN
        PERFORM eacp.assert_scan_lease(NEW.tenant_id, tl.connector_id);
    END IF;

    d := NEW.definition::jsonb;
    disp := NEW.display::jsonb;
    IF jsonb_typeof(d) <> 'object' OR d->>'name' IS DISTINCT FROM tl.remote_name
       OR jsonb_typeof(d->'inputSchema') IS DISTINCT FROM 'object'
       OR d->'inputSchema'->>'type' IS DISTINCT FROM 'object' THEN
        RAISE EXCEPTION 'a definition is an object named % with an object inputSchema', tl.remote_name
            USING ERRCODE = '23514';
    END IF;
    IF jsonb_typeof(disp) <> 'object' THEN
        RAISE EXCEPTION 'display metadata is an object' USING ERRCODE = '23514';
    END IF;

    NEW.fingerprint := sha256(convert_to(NEW.definition, 'UTF8'));
    NEW.display_digest := sha256(convert_to(NEW.display, 'UTF8'));
    SELECT * INTO prev FROM eacp.tool_definitions WHERE tenant_id = NEW.tenant_id AND id = tl.definition_id;
    IF prev.id IS NULL THEN
        NEW.seq := 1;
        NEW.risk := 'initial';
        NEW.changes := '{}';
    ELSE
        IF prev.fingerprint = NEW.fingerprint AND prev.display_digest = NEW.display_digest THEN
            RAISE EXCEPTION 'the definition of tool % did not change', tl.id USING ERRCODE = '55000';
        END IF;
        pd := prev.definition::jsonb;
        pdisp := prev.display::jsonb;
        NEW.seq := prev.seq + 1;
        NEW.risk := CASE WHEN prev.fingerprint = NEW.fingerprint THEN 'low' ELSE 'high' END;
        NEW.changes := ARRAY(
            SELECT k FROM (SELECT jsonb_object_keys(pd) UNION SELECT jsonb_object_keys(d)) AS x(k)
             WHERE pd->k IS DISTINCT FROM d->k
            UNION
            SELECT k FROM (SELECT jsonb_object_keys(pdisp) UNION SELECT jsonb_object_keys(disp)) AS y(k)
             WHERE pdisp->k IS DISTINCT FROM disp->k
            ORDER BY 1);
    END IF;

    a := CASE WHEN jsonb_typeof(d->'annotations') = 'object' THEN d->'annotations' ELSE '{}'::jsonb END;
    NEW.read_only := CASE WHEN jsonb_typeof(a->'readOnlyHint') = 'boolean' THEN (a->'readOnlyHint')::boolean ELSE false END;
    NEW.destructive := CASE WHEN NEW.read_only THEN false
                            WHEN jsonb_typeof(a->'destructiveHint') = 'boolean' THEN (a->'destructiveHint')::boolean
                            ELSE true END;
    NEW.idempotent := CASE WHEN NEW.read_only THEN true
                           WHEN jsonb_typeof(a->'idempotentHint') = 'boolean' THEN (a->'idempotentHint')::boolean
                           ELSE false END;
    NEW.open_world := CASE WHEN jsonb_typeof(a->'openWorldHint') = 'boolean' THEN (a->'openWorldHint')::boolean ELSE true END;
    NEW.observed_at := now();
    NEW.observed_by := COALESCE(who->>'worker', 'owner');
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.tool_fingerprint(p_tenant uuid, p_tool uuid) RETURNS bytea
    LANGUAGE sql STABLE
    AS $$
    SELECT CASE WHEN c.protocol = 'mcp' THEN
                CASE WHEN d.id IS NOT NULL THEN
                     sha256(convert_to(concat_ws(E'\n', 'eacp-tool-v2', c.protocol, c.endpoint, c.secret_ref,
                                                 c.name, t.name, t.remote_name, encode(d.fingerprint, 'hex')), 'UTF8'))
                END
           ELSE sha256(convert_to(concat_ws(E'\n', 'eacp-tool-v1',
                       c.protocol, c.endpoint, c.secret_ref, c.name, t.name), 'UTF8'))
           END
    FROM eacp.tools t
    JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
    LEFT JOIN eacp.tool_definitions d ON d.tenant_id = t.tenant_id AND d.id = t.definition_id
    WHERE t.tenant_id = p_tenant AND t.id = p_tool
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.tools_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    who   jsonb := eacp.registry_actor();
    kind  text  := who->>'kind';
    a     uuid;
    c     eacp.tool_contracts%ROWTYPE;
    proto text;
BEGIN
    IF TG_OP = 'INSERT' THEN
        SELECT protocol INTO proto FROM eacp.connectors WHERE tenant_id = NEW.tenant_id AND id = NEW.connector_id;
        NEW.definition_id := NULL;
        NEW.missing_since := NULL;
        NEW.quarantined_at := NULL;
        NEW.quarantine_reason := NULL;
        NEW.quarantine_changed_at := NULL;
        NEW.quarantine_changed_by := NULL;
        NEW.quarantine_changed_by_worker := NULL;
        NEW.contract_changed_by := NULL;
        NEW.contract_changed_at := NULL;
        NEW.created_at := now();
        IF NEW.active_contract_id IS NOT NULL THEN
            RAISE EXCEPTION 'a tool is registered without a contract' USING ERRCODE = '55000';
        END IF;
        IF kind = 'scanner' THEN
            IF proto IS DISTINCT FROM 'mcp' THEN
                RAISE EXCEPTION 'the scanner discovers MCP tools only' USING ERRCODE = '42501';
            END IF;
            PERFORM eacp.assert_scan_lease(NEW.tenant_id, NEW.connector_id);
            IF NEW.remote_name IS NULL THEN
                RAISE EXCEPTION 'a discovered tool keeps its MCP name' USING ERRCODE = '23514';
            END IF;
            NEW.name := eacp.mcp_tool_name(NEW.remote_name);
            NEW.origin := 'discovered';
            NEW.created_by := NULL;
            RETURN NEW;
        END IF;
        a := eacp.actor();
        PERFORM eacp.assert_role(a, 'registry_editor');
        IF proto = 'mcp' AND kind <> 'owner' THEN
            RAISE EXCEPTION 'MCP tools are discovered, not declared' USING ERRCODE = '55000';
        END IF;
        IF kind <> 'owner' THEN
            NEW.origin := 'registered';
            NEW.remote_name := NULL;
        END IF;
        NEW.created_by := a;
        RETURN NEW;
    END IF;

    -- The current definition moves only when the scanner records one.
    IF NEW.definition_id IS DISTINCT FROM OLD.definition_id THEN
        IF pg_trigger_depth() < 2 AND kind <> 'owner' THEN
            RAISE EXCEPTION 'a tool''s definition changes only when one is recorded' USING ERRCODE = '42501';
        END IF;
    END IF;

    -- Listed or not (the scanner holding the lease).
    IF NEW.missing_since IS DISTINCT FROM OLD.missing_since AND kind <> 'owner' THEN
        IF kind <> 'scanner' THEN
            RAISE EXCEPTION 'only the scanner marks a tool as missing' USING ERRCODE = '42501';
        END IF;
        PERFORM eacp.assert_scan_lease(NEW.tenant_id, NEW.connector_id);
        IF NEW.missing_since IS NOT NULL THEN
            NEW.missing_since := COALESCE(OLD.missing_since, now());
            IF OLD.active_contract_id IS NOT NULL AND OLD.quarantined_at IS NULL AND NEW.quarantined_at IS NULL THEN
                NEW.quarantined_at := now();
                NEW.quarantine_reason := 'no longer listed by the server';
            END IF;
        END IF;
    END IF;

    -- Quarantine and release.
    IF kind <> 'owner'
       AND ROW(NEW.quarantined_at IS NULL, NEW.quarantine_reason, NEW.quarantine_changed_at, NEW.quarantine_changed_by,
               NEW.quarantine_changed_by_worker)
           IS DISTINCT FROM
           ROW(OLD.quarantined_at IS NULL, OLD.quarantine_reason, OLD.quarantine_changed_at, OLD.quarantine_changed_by,
               OLD.quarantine_changed_by_worker) THEN
        IF (NEW.quarantined_at IS NULL) = (OLD.quarantined_at IS NULL) THEN
            RAISE EXCEPTION 'quarantine fields change only when a tool is quarantined or released'
                USING ERRCODE = '55000';
        END IF;
        PERFORM eacp.require_reason(NEW.quarantine_reason, 'a quarantine change');
        IF NEW.quarantined_at IS NOT NULL THEN
            IF kind = 'scanner' THEN
                IF pg_trigger_depth() < 2 AND NEW.missing_since IS NOT DISTINCT FROM OLD.missing_since THEN
                    RAISE EXCEPTION 'the scanner quarantines a tool only on drift' USING ERRCODE = '42501';
                END IF;
                NEW.quarantine_changed_by := NULL;
                NEW.quarantine_changed_by_worker := who->>'worker';
            ELSE
                PERFORM eacp.assert_role((who->>'id')::uuid, 'operator', 'registry_approver');
                NEW.quarantine_changed_by := (who->>'id')::uuid;
                NEW.quarantine_changed_by_worker := NULL;
            END IF;
            NEW.quarantined_at := now();
        ELSE
            IF kind <> 'principal' THEN
                RAISE EXCEPTION 'only a registry approver releases a quarantine' USING ERRCODE = '42501';
            END IF;
            PERFORM eacp.assert_role((who->>'id')::uuid, 'registry_approver');
            PERFORM eacp.assert_distinct((who->>'id')::uuid, OLD.quarantine_changed_by, 'the principal who quarantined it');
            NEW.quarantine_changed_by := (who->>'id')::uuid;
            NEW.quarantine_changed_by_worker := NULL;
        END IF;
        NEW.quarantine_changed_at := now();
    ELSIF NEW.quarantined_at IS DISTINCT FROM OLD.quarantined_at AND kind <> 'owner' THEN
        NEW.quarantined_at := OLD.quarantined_at;  -- the timestamp is the database's
    END IF;

    IF NEW.active_contract_id IS NOT DISTINCT FROM OLD.active_contract_id THEN
        RETURN NEW;
    END IF;
    a := eacp.actor();
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
    IF c.fingerprint IS DISTINCT FROM eacp.tool_fingerprint(NEW.tenant_id, NEW.id) THEN
        RAISE EXCEPTION 'contract % does not match the tool''s current definition', c.id USING ERRCODE = '55000';
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
CREATE OR REPLACE FUNCTION eacp.tool_contracts_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a     uuid := eacp.actor();
    tl    eacp.tools%ROWTYPE;
    proto text;
    d     eacp.tool_definitions%ROWTYPE;
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM eacp.assert_role(a, 'registry_editor', 'registry_approver');
        IF NEW.revoked_at IS NOT NULL OR NEW.revoked_by IS NOT NULL OR NEW.revoke_reason IS NOT NULL THEN
            RAISE EXCEPTION 'a contract cannot be created revoked' USING ERRCODE = '23514';
        END IF;
        -- Serialise version numbering per tool on the tool row.
        SELECT * INTO tl FROM eacp.tools WHERE tenant_id = NEW.tenant_id AND id = NEW.tool_id FOR NO KEY UPDATE;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'unknown tool %', NEW.tool_id USING ERRCODE = '23503';
        END IF;
        SELECT COALESCE(max(version), 0) + 1 INTO NEW.version
        FROM eacp.tool_contracts WHERE tenant_id = NEW.tenant_id AND tool_id = NEW.tool_id;
        NEW.side_effects     := ARRAY(SELECT DISTINCT e FROM unnest(NEW.side_effects) e ORDER BY e);
        NEW.no_effect_errors := ARRAY(SELECT DISTINCT e FROM unnest(NEW.no_effect_errors) e ORDER BY e);
        -- An MCP tool's contract certifies the definition its proposer
        -- reviewed, which must still be current (ADR-023 §5, §6).
        SELECT protocol INTO proto FROM eacp.connectors WHERE tenant_id = tl.tenant_id AND id = tl.connector_id;
        IF proto = 'mcp' THEN
            IF NEW.definition_id IS NULL OR NEW.definition_id IS DISTINCT FROM tl.definition_id THEN
                RAISE EXCEPTION 'a contract must pin tool %''s current definition; review it again', tl.id
                    USING ERRCODE = '55000';
            END IF;
            SELECT * INTO d FROM eacp.tool_definitions WHERE tenant_id = NEW.tenant_id AND id = NEW.definition_id;
            IF NEW.side_effects = ARRAY['READ_ONLY'] AND NOT d.read_only THEN
                RAISE EXCEPTION 'the server does not declare tool % read-only', tl.id USING ERRCODE = '23514';
            END IF;
            IF NEW.reconciliation_lookup <> 'none' THEN
                RAISE EXCEPTION 'MCP defines no lookup by operation key' USING ERRCODE = '23514';
            END IF;
        ELSIF NEW.definition_id IS NOT NULL THEN
            RAISE EXCEPTION 'only an MCP tool''s contract pins a definition' USING ERRCODE = '23514';
        END IF;
        NEW.fingerprint      := eacp.tool_fingerprint(NEW.tenant_id, NEW.tool_id);
        NEW.created_by       := a;
        NEW.created_at       := now();
        RETURN NEW;
    END IF;

    IF NEW.definition_id IS DISTINCT FROM OLD.definition_id THEN
        RAISE EXCEPTION 'a contract''s definition is immutable' USING ERRCODE = '55000';
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

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.action_attempts_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    who jsonb := eacp.actor_context();
    a   eacp.actions%ROWTYPE;
    ct  eacp.tool_contracts%ROWTYPE;
BEGIN
    SELECT * INTO a FROM eacp.actions
    WHERE tenant_id = NEW.tenant_id AND id = NEW.action_id FOR SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'an attempt requires an action' USING ERRCODE = '23503';
    END IF;
    IF TG_OP = 'INSERT' THEN
        -- Only the dispatch intent of this generation creates an attempt.
        PERFORM eacp.assert_lease_holder(who, a);
        IF a.state <> 'EXECUTING' OR a.dispatch_intent_at IS DISTINCT FROM now()
           OR NEW.lease_generation <> a.lease_generation OR NEW.attempt_no <> a.attempt_count
           OR num_nonnulls(NEW.completed_at, NEW.outcome, NEW.external_reference, NEW.error_class) > 0
           OR NEW.late THEN
            RAISE EXCEPTION 'an attempt is created by its dispatch intent' USING ERRCODE = '55000';
        END IF;
        NEW.worker_id := a.worker_id;
        NEW.operation_key := a.operation_key;
        NEW.connector_contract_id := a.connector_contract_id;
        NEW.connector_contract_version := a.connector_contract_version;
        NEW.enforced_digest := a.enforced_digest;
        NEW.dispatched_at := now();
        NEW.call_deadline := now() + eacp.call_timeout(a.connector_contract_id);
        RETURN NEW;
    END IF;

    -- Completion: once, by the worker of this attempt's generation, with a
    -- classification the pinned contract certifies.
    IF OLD.completed_at IS NOT NULL THEN
        RAISE EXCEPTION 'an attempt completes once' USING ERRCODE = '55000';
    END IF;
    IF (to_jsonb(NEW) - ARRAY['outcome', 'external_reference', 'error_class', 'completed_at', 'late'])
       IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['outcome', 'external_reference', 'error_class', 'completed_at', 'late']) THEN
        RAISE EXCEPTION 'an attempt records only its outcome' USING ERRCODE = '55000';
    END IF;
    IF who->>'component' IS DISTINCT FROM 'worker' OR who->>'worker' IS DISTINCT FROM OLD.worker_id
       OR eacp.current_lease_generation() IS DISTINCT FROM OLD.lease_generation THEN
        RAISE EXCEPTION 'only the worker of this attempt records its outcome' USING ERRCODE = '42501';
    END IF;
    IF NEW.outcome = 'no_effect' THEN
        SELECT * INTO ct FROM eacp.tool_contracts
        WHERE tenant_id = NEW.tenant_id AND id = OLD.connector_contract_id;
        IF NOT (NEW.error_class = ANY (ct.no_effect_errors)) THEN
            RAISE EXCEPTION 'error class % is not certified as no-effect', NEW.error_class USING ERRCODE = '23514';
        END IF;
    ELSIF NEW.outcome IS NULL THEN
        RAISE EXCEPTION 'an attempt completes with an outcome' USING ERRCODE = '23514';
    END IF;
    NEW.completed_at := now();
    NEW.late := NOT (a.state = 'EXECUTING' AND a.lease_generation = OLD.lease_generation);
    RETURN NEW;
END
$$;
-- +goose StatementEnd

REVOKE UPDATE (remote_reference) ON eacp.action_attempts FROM eacp_app;
ALTER TABLE eacp.action_attempts DROP CONSTRAINT remote_reference_not_on_success, DROP COLUMN remote_reference;
ALTER TABLE eacp.mcp_scans DROP CONSTRAINT mcp_scans_protocol_version_check;
ALTER TABLE eacp.mcp_scans ADD CONSTRAINT mcp_scans_protocol_version_check
    CHECK (protocol_version ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}$');
ALTER TABLE eacp.connectors DROP CONSTRAINT connectors_protocol_check;
ALTER TABLE eacp.connectors ADD CONSTRAINT connectors_protocol_check CHECK (protocol IN ('http', 'mcp'));
