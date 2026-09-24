-- Phase 14 (ADR-023): MCP registry, tool fingerprint and certification.
--
-- MCP servers are connectors (protocol 'mcp') whose tools are discovered by
-- the scanner in the execution worker, never declared. Each discovered tool
-- keeps an insert-only history of its canonical definitions, fingerprinted
-- by PostgreSQL. A contract pins the definition it certifies; a high-risk
-- change invalidates it (the tool fingerprint moves) and quarantines a
-- certified tool. Quarantine blocks the capability everywhere.

-- +goose Up

-- ---------------------------------------------------------- the scanner

-- +goose StatementBegin
-- eacp.registry_actor() returns the single actor of a registry change:
--   {"kind":"principal","id":...}
--   {"kind":"scanner","worker":...,"generation":...}  (storage.SetScanner)
--   {"kind":"owner"}                                   (the schema owner, no setting)
-- eacp.actor_context() deliberately rejects the scanner, so a scanner can
-- never change an action.
CREATE FUNCTION eacp.registry_actor() RETURNS jsonb
    LANGUAGE plpgsql STABLE
    AS $$
DECLARE
    p uuid := eacp.current_actor_id();
    g uuid := eacp.current_agent_version_id();
    s text := eacp.current_system_actor();
BEGIN
    IF num_nonnulls(p, g, s) > 1 THEN
        RAISE EXCEPTION 'a transaction has exactly one actor' USING ERRCODE = '42501';
    END IF;
    IF s = 'scanner' THEN
        IF eacp.current_worker_id() IS NULL OR eacp.current_lease_generation() IS NULL THEN
            RAISE EXCEPTION 'the scanner needs a valid app.worker_id and lease generation' USING ERRCODE = '42501';
        END IF;
        RETURN jsonb_build_object('kind', 'scanner', 'worker', eacp.current_worker_id(),
                                  'generation', eacp.current_lease_generation());
    END IF;
    IF p IS NOT NULL THEN
        RETURN jsonb_build_object('kind', 'principal', 'id', p);
    END IF;
    IF g IS NULL AND s IS NULL AND eacp.is_schema_owner() THEN
        RETURN jsonb_build_object('kind', 'owner');
    END IF;
    RAISE EXCEPTION 'no registry actor set for this transaction' USING ERRCODE = '42501';
END
$$;
-- +goose StatementEnd

-- The EACP name of a discovered tool: its MCP name when that is already a
-- tool slug, otherwise a sanitized prefix plus 8 hex digits of its SHA-256.
CREATE FUNCTION eacp.mcp_tool_name(remote text) RETURNS text
    LANGUAGE sql STABLE STRICT
    AS $$
    SELECT CASE WHEN remote ~ '^[a-z0-9][a-z0-9_-]{0,62}$' THEN remote
           ELSE COALESCE(NULLIF(left(regexp_replace(lower(regexp_replace(remote, '[^A-Za-z0-9_-]+', '_', 'g')),
                                                    '^[^a-z0-9]+', ''), 54), ''), 'tool')
                || '-' || left(encode(sha256(convert_to(remote, 'UTF8')), 'hex'), 8)
           END
    $$;

ALTER TABLE eacp.connectors DROP CONSTRAINT connectors_protocol_check;
ALTER TABLE eacp.connectors ADD CONSTRAINT connectors_protocol_check CHECK (protocol IN ('http', 'mcp'));

-- One row per MCP connector: its scan schedule, the operator's rescan
-- request and the scan lease (ADR-023 §2).
CREATE TABLE eacp.mcp_servers (
    tenant_id            uuid        NOT NULL,
    connector_id         uuid        NOT NULL,
    next_scan_at         timestamptz NOT NULL DEFAULT now(),
    requested_at         timestamptz,
    requested_by         uuid,
    request_reason       text,
    lease_worker         text,
    lease_until          timestamptz,
    lease_claimed_at     timestamptz,
    lease_generation     bigint      NOT NULL DEFAULT 0 CHECK (lease_generation >= 0),
    last_scan_id         uuid,
    last_scan_at         timestamptz,
    last_outcome         text        CHECK (last_outcome IN ('ok', 'failed')),
    consecutive_failures integer     NOT NULL DEFAULT 0 CHECK (consecutive_failures >= 0),
    PRIMARY KEY (tenant_id, connector_id),
    FOREIGN KEY (tenant_id, connector_id) REFERENCES eacp.connectors (tenant_id, id),
    FOREIGN KEY (tenant_id, requested_by) REFERENCES eacp.principals (tenant_id, id),
    CHECK ((lease_worker IS NULL) = (lease_until IS NULL)),
    CHECK ((lease_worker IS NULL) = (lease_claimed_at IS NULL)),
    CHECK ((requested_at IS NULL) = (request_reason IS NULL))
);

-- Every scan, successful or not (ADR-023 §2). Insert-only.
CREATE TABLE eacp.mcp_scans (
    tenant_id           uuid        NOT NULL,
    id                  uuid        NOT NULL DEFAULT gen_random_uuid(),
    connector_id        uuid        NOT NULL,
    worker_id           text        NOT NULL,
    lease_generation    bigint      NOT NULL,
    completed_at        timestamptz NOT NULL DEFAULT now(),
    outcome             text        NOT NULL CHECK (outcome IN ('ok', 'failed')),
    error_class         text        CHECK (error_class ~ '^[a-z0-9_]{1,64}$'),
    protocol_version    text        CHECK (protocol_version ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}$'),
    -- Self-reported by the server: for display only, never for decisions.
    server_info         jsonb       CHECK (jsonb_typeof(server_info) = 'object' AND octet_length(server_info::text) <= 4096),
    tools_listed        integer     NOT NULL DEFAULT 0 CHECK (tools_listed >= 0),
    tools_added         integer     NOT NULL DEFAULT 0 CHECK (tools_added >= 0),
    definitions_changed integer     NOT NULL DEFAULT 0 CHECK (definitions_changed >= 0),
    metadata_changed    integer     NOT NULL DEFAULT 0 CHECK (metadata_changed >= 0),
    tools_missing       integer     NOT NULL DEFAULT 0 CHECK (tools_missing >= 0),
    rejected            jsonb       NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(rejected) = 'array'
                                        AND jsonb_array_length(rejected) <= 500 AND octet_length(rejected::text) <= 65536),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, connector_id) REFERENCES eacp.connectors (tenant_id, id),
    CHECK ((outcome = 'failed') = (error_class IS NOT NULL)),
    CHECK (outcome = 'failed' OR protocol_version IS NOT NULL)
);

ALTER TABLE eacp.mcp_servers
    ADD FOREIGN KEY (tenant_id, last_scan_id) REFERENCES eacp.mcp_scans (tenant_id, id);

-- Discovered tools (ADR-023 §1, §4, §7).
ALTER TABLE eacp.tools
    ADD COLUMN remote_name                  text CHECK (remote_name ~ '^[A-Za-z0-9_.-]{1,128}$'),
    ADD COLUMN origin                       text NOT NULL DEFAULT 'registered' CHECK (origin IN ('registered', 'discovered')),
    ADD COLUMN definition_id                uuid,
    ADD COLUMN missing_since                timestamptz,
    ADD COLUMN quarantined_at               timestamptz,
    ADD COLUMN quarantine_reason            text,
    ADD COLUMN quarantine_changed_at        timestamptz,
    ADD COLUMN quarantine_changed_by        uuid,
    ADD COLUMN quarantine_changed_by_worker text,
    ADD CONSTRAINT discovered_tools_are_named CHECK ((origin = 'discovered') = (remote_name IS NOT NULL)),
    ADD CONSTRAINT remote_name_unique UNIQUE (tenant_id, connector_id, remote_name),
    ADD FOREIGN KEY (tenant_id, quarantine_changed_by) REFERENCES eacp.principals (tenant_id, id);

-- The definition history of a discovered tool (ADR-023 §3, §4). Insert-only.
-- The database computes the digests, the risk of the change and the hints.
CREATE TABLE eacp.tool_definitions (
    tenant_id      uuid        NOT NULL,
    id             uuid        NOT NULL DEFAULT gen_random_uuid(),
    tool_id        uuid        NOT NULL,
    seq            integer     NOT NULL CHECK (seq >= 1),
    scan_id        uuid        NOT NULL,
    -- RFC 8785 text of the tool object minus its display-only fields.
    definition     text        NOT NULL CHECK (octet_length(definition) <= 65536),
    -- RFC 8785 text of the display-only fields (title, icons, annotations.title).
    display        text        NOT NULL CHECK (octet_length(display) <= 16384),
    fingerprint    bytea       NOT NULL CHECK (octet_length(fingerprint) = 32),
    display_digest bytea       NOT NULL CHECK (octet_length(display_digest) = 32),
    risk           text        NOT NULL CHECK (risk IN ('initial', 'low', 'high')),
    changes        text[]      NOT NULL DEFAULT '{}',
    -- Self-described behaviour with the specification's defaults: untrusted.
    read_only      boolean     NOT NULL,
    destructive    boolean     NOT NULL,
    idempotent     boolean     NOT NULL,
    open_world     boolean     NOT NULL,
    observed_at    timestamptz NOT NULL DEFAULT now(),
    observed_by    text        NOT NULL,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, tool_id, seq),
    FOREIGN KEY (tenant_id, tool_id) REFERENCES eacp.tools (tenant_id, id),
    -- A scan row is written after the definitions it recorded.
    FOREIGN KEY (tenant_id, scan_id) REFERENCES eacp.mcp_scans (tenant_id, id) DEFERRABLE INITIALLY DEFERRED
);

ALTER TABLE eacp.tools
    ADD FOREIGN KEY (tenant_id, definition_id) REFERENCES eacp.tool_definitions (tenant_id, id);

-- A contract of an MCP tool pins the definition it certifies (ADR-023 §6).
ALTER TABLE eacp.tool_contracts
    ADD COLUMN definition_id uuid,
    ADD FOREIGN KEY (tenant_id, definition_id) REFERENCES eacp.tool_definitions (tenant_id, id);

-- +goose StatementBegin
-- eacp.assert_scan_lease fails unless the transaction's scanner holds the
-- unexpired scan lease of the connector at its bound generation. It locks
-- the server row, so a record and a claim serialize.
CREATE FUNCTION eacp.assert_scan_lease(p_tenant uuid, p_connector uuid) RETURNS void
    LANGUAGE plpgsql
    AS $$
DECLARE
    s eacp.mcp_servers%ROWTYPE;
BEGIN
    SELECT * INTO s FROM eacp.mcp_servers
    WHERE tenant_id = p_tenant AND connector_id = p_connector FOR UPDATE;
    IF NOT FOUND OR eacp.current_system_actor() IS DISTINCT FROM 'scanner'
       OR s.lease_worker IS DISTINCT FROM eacp.current_worker_id()
       OR s.lease_generation IS DISTINCT FROM eacp.current_lease_generation()
       OR NOT COALESCE(s.lease_until > now(), false) THEN
        RAISE EXCEPTION 'only the scanner holding the scan lease at generation % may record', s.lease_generation
            USING ERRCODE = '42501';
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- An MCP server row is created with its connector. Afterwards:
--   a principal (operator or registry_editor) only requests a rescan;
--   a scanner claims a free or expired lease, bumping the generation by one,
--     for at most 10 minutes;
--   the lease holder records its bookkeeping and releases the lease.
CREATE FUNCTION eacp.mcp_servers_guard() RETURNS trigger
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
CREATE FUNCTION eacp.audit_mcp_server() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.requested_at IS NOT NULL AND NEW.requested_at IS DISTINCT FROM OLD.requested_at THEN
        INSERT INTO eacp.audit_events (tenant_id, payload)
        VALUES (NEW.tenant_id, convert_to(jsonb_build_object(
            'v', 1,
            'actor', jsonb_build_object('kind', 'principal', 'id', NEW.requested_by),
            'action', 'mcp.rescan_requested',
            'subject', jsonb_build_object('type', 'connector', 'id', NEW.connector_id),
            'reason', NEW.request_reason,
            'data', jsonb_build_object('requested_at', NEW.requested_at))::text, 'UTF8'));
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER mcp_servers_guard BEFORE INSERT OR UPDATE ON eacp.mcp_servers
    FOR EACH ROW EXECUTE FUNCTION eacp.mcp_servers_guard();
CREATE TRIGGER zz_audit AFTER UPDATE ON eacp.mcp_servers
    FOR EACH ROW EXECUTE FUNCTION eacp.audit_mcp_server();

-- +goose StatementBegin
CREATE FUNCTION eacp.connectors_mcp_server() RETURNS trigger
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
CREATE TRIGGER connectors_mcp_server AFTER INSERT ON eacp.connectors
    FOR EACH ROW EXECUTE FUNCTION eacp.connectors_mcp_server();

-- +goose StatementBegin
-- A scan is recorded only by the scanner holding the connector's lease; the
-- database stamps who recorded it and when.
CREATE FUNCTION eacp.mcp_scans_guard() RETURNS trigger
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
CREATE FUNCTION eacp.audit_mcp_scan() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    INSERT INTO eacp.audit_events (tenant_id, payload)
    VALUES (NEW.tenant_id, convert_to(jsonb_build_object(
        'v', 1,
        'actor', jsonb_build_object('kind', 'system', 'id', '00000000-0000-0000-0000-000000000000'::uuid,
                                    'component', 'scanner', 'worker', NEW.worker_id),
        'action', 'mcp.scan_recorded',
        'subject', jsonb_build_object('type', 'connector', 'id', NEW.connector_id),
        'reason', CASE WHEN NEW.outcome = 'ok' THEN 'scan completed' ELSE 'scan failed: ' || NEW.error_class END,
        'data', jsonb_strip_nulls(jsonb_build_object(
            'scan_id', NEW.id,
            'outcome', NEW.outcome,
            'protocol_version', NEW.protocol_version,
            'lease_generation', NEW.lease_generation,
            'tools_listed', NEW.tools_listed,
            'tools_added', NEW.tools_added,
            'definitions_changed', NEW.definitions_changed,
            'metadata_changed', NEW.metadata_changed,
            'tools_missing', NEW.tools_missing,
            'tools_rejected', jsonb_array_length(NEW.rejected))))::text, 'UTF8'));
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER mcp_scans_guard BEFORE INSERT ON eacp.mcp_scans
    FOR EACH ROW EXECUTE FUNCTION eacp.mcp_scans_guard();
CREATE TRIGGER zz_audit AFTER INSERT ON eacp.mcp_scans
    FOR EACH ROW EXECUTE FUNCTION eacp.audit_mcp_scan();

-- +goose StatementBegin
-- The scanner records definitions of discovered tools only, under the scan
-- lease. The database parses the canonical text, computes both digests,
-- classifies the change against the tool's current definition and derives
-- the behaviour hints with the specification's defaults.
CREATE FUNCTION eacp.tool_definitions_guard() RETURNS trigger
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
-- A recorded definition becomes the tool's current one. A high-risk change
-- of a certified tool (one with an active contract) quarantines it.
CREATE FUNCTION eacp.tool_definitions_apply() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    tl eacp.tools%ROWTYPE;
BEGIN
    SELECT * INTO tl FROM eacp.tools WHERE tenant_id = NEW.tenant_id AND id = NEW.tool_id;
    IF NEW.risk = 'high' AND tl.active_contract_id IS NOT NULL AND tl.quarantined_at IS NULL THEN
        UPDATE eacp.tools SET definition_id = NEW.id, quarantined_at = now(),
               quarantine_reason = format('definition %s changed: %s', NEW.seq, array_to_string(NEW.changes, ', '))
         WHERE tenant_id = NEW.tenant_id AND id = NEW.tool_id;
    ELSE
        UPDATE eacp.tools SET definition_id = NEW.id WHERE tenant_id = NEW.tenant_id AND id = NEW.tool_id;
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.audit_tool_definition() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    INSERT INTO eacp.audit_events (tenant_id, payload)
    VALUES (NEW.tenant_id, convert_to(jsonb_build_object(
        'v', 1,
        'actor', CASE WHEN NEW.observed_by = 'owner'
                      THEN jsonb_build_object('kind', 'system', 'id', '00000000-0000-0000-0000-000000000000'::uuid)
                      ELSE jsonb_build_object('kind', 'system', 'id', '00000000-0000-0000-0000-000000000000'::uuid,
                                              'component', 'scanner', 'worker', NEW.observed_by) END,
        'action', 'tool.definition_recorded',
        'subject', jsonb_build_object('type', 'tool', 'id', NEW.tool_id),
        'reason', format('definition %s: %s risk', NEW.seq, NEW.risk),
        'data', jsonb_build_object(
            'definition_id', NEW.id,
            'seq', NEW.seq,
            'scan_id', NEW.scan_id,
            'fingerprint', encode(NEW.fingerprint, 'hex'),
            'display_digest', encode(NEW.display_digest, 'hex'),
            'risk', NEW.risk,
            'changes', to_jsonb(NEW.changes),
            'read_only', NEW.read_only,
            'destructive', NEW.destructive,
            'idempotent', NEW.idempotent,
            'open_world', NEW.open_world))::text, 'UTF8'));
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER tool_definitions_guard BEFORE INSERT ON eacp.tool_definitions
    FOR EACH ROW EXECUTE FUNCTION eacp.tool_definitions_guard();
CREATE TRIGGER tool_definitions_apply AFTER INSERT ON eacp.tool_definitions
    FOR EACH ROW EXECUTE FUNCTION eacp.tool_definitions_apply();
CREATE TRIGGER zz_audit AFTER INSERT ON eacp.tool_definitions
    FOR EACH ROW EXECUTE FUNCTION eacp.audit_tool_definition();

-- ------------------------------------------------ the tool fingerprint

-- +goose StatementBegin
-- The identity a contract certifies (ADR-003 §4, ADR-023 §3). An HTTP tool
-- keeps the v1 hash. An MCP tool adds its names and the fingerprint of its
-- current definition (v2), and has no fingerprint before its first one.
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

-- ------------------------------------------------- tools and contracts

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

GRANT UPDATE (definition_id, missing_since, quarantined_at, quarantine_reason) ON eacp.tools TO eacp_app;

-- +goose StatementBegin
-- eacp.action_capability_denial returns NULL when agent version p_version
-- may call tool p_tool now, or the registry.Denial code otherwise. Rows are
-- read FOR SHARE (ADR-004 principle 7). A quarantined tool is denied before
-- its contract is considered (ADR-023 §7).
CREATE OR REPLACE FUNCTION eacp.action_capability_denial(p_version uuid, p_tool uuid) RETURNS text
    LANGUAGE plpgsql
    AS $$
DECLARE
    v eacp.agent_versions%ROWTYPE;
    tl eacp.tools%ROWTYPE;
    ct eacp.tool_contracts%ROWTYPE;
    allowed boolean;
BEGIN
    SELECT * INTO v FROM eacp.agent_versions
    WHERE tenant_id = eacp.current_tenant_id() AND id = p_version FOR SHARE;
    IF NOT FOUND OR v.state <> 'ACTIVE' OR v.active_allowlist_id IS NULL THEN
        RETURN 'agent_version_not_active';
    END IF;
    IF p_tool IS NULL THEN
        RETURN 'unknown_tool';
    END IF;
    SELECT p_tool = ANY (tool_ids) INTO allowed FROM eacp.agent_allowlists
    WHERE tenant_id = eacp.current_tenant_id() AND id = v.active_allowlist_id;
    IF NOT COALESCE(allowed, false) THEN
        RETURN 'tool_not_in_allowlist';
    END IF;
    SELECT * INTO tl FROM eacp.tools
    WHERE tenant_id = eacp.current_tenant_id() AND id = p_tool FOR SHARE;
    IF tl.quarantined_at IS NOT NULL THEN
        RETURN 'tool_quarantined';
    END IF;
    IF tl.active_contract_id IS NULL THEN
        RETURN 'no_active_contract';
    END IF;
    SELECT * INTO ct FROM eacp.tool_contracts
    WHERE tenant_id = eacp.current_tenant_id() AND id = tl.active_contract_id FOR SHARE;
    IF ct.revoked_at IS NOT NULL THEN
        RETURN 'contract_revoked';
    END IF;
    IF ct.fingerprint IS DISTINCT FROM eacp.tool_fingerprint(ct.tenant_id, p_tool) THEN
        RETURN 'contract_fingerprint_mismatch';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Every registry change is appended to the audit journal by the database
-- itself, in the same transaction (ADR-003 §7). Credential hashes are never
-- recorded. A scanner's change names the scanner (ADR-023 §7).
CREATE OR REPLACE FUNCTION eacp.audit_row_change() RETURNS trigger
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
        CASE WHEN TG_OP = 'UPDATE' AND changed ? 'quarantined_at' THEN row_new ->> 'quarantine_reason' END,
        TG_TABLE_NAME || ' ' || lower(TG_OP));

    INSERT INTO eacp.audit_events (tenant_id, payload)
    VALUES (NEW.tenant_id, convert_to(jsonb_build_object(
        'v', 1,
        'actor', CASE WHEN a IS NULL AND eacp.current_system_actor() = 'scanner' THEN
                           jsonb_build_object('kind', 'system', 'id', '00000000-0000-0000-0000-000000000000'::uuid,
                                              'component', 'scanner', 'worker', eacp.current_worker_id())
                      ELSE jsonb_build_object(
                           'kind', CASE WHEN a IS NULL THEN 'system' ELSE 'principal' END,
                           'id', COALESCE(a, '00000000-0000-0000-0000-000000000000'::uuid)) END,
        'action', TG_TABLE_NAME || '.' || lower(TG_OP),
        'subject', jsonb_build_object('type', TG_TABLE_NAME, 'id', NEW.id),
        'reason', why,
        'data', changed)::text, 'UTF8'));
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- ------------------------------------------------ recording a scan

-- +goose StatementBegin
-- eacp.mcp_record_scan records one scan of MCP connector p_connector by the
-- scanner holding its lease, and releases the lease. p_result:
--   {"outcome":"ok"|"failed", "error_class":..., "protocol_version":...,
--    "server_info":{...}, "interval_s":900,
--    "tools":[{"remote_name":..., "definition":<RFC 8785 text>, "display":<RFC 8785 text>}],
--    "rejected":[{"remote_name":..., "reason":...}]}
-- A successful scan is a complete listing: a tool it does not list is
-- missing. A failed scan changes no tool. Every write is checked by the
-- triggers above; this function only sequences them.
CREATE FUNCTION eacp.mcp_record_scan(p_connector uuid, p_result jsonb) RETURNS uuid
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

-- ------------------------------------------------ RLS and privileges

-- Existing MCP connectors: none (the protocol is new).
ALTER TABLE eacp.mcp_servers ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.mcp_servers FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.mcp_servers USING (tenant_id = eacp.current_tenant_id());
-- The SECURITY DEFINER scan hint finds due servers across tenants.
CREATE POLICY owner_scan ON eacp.mcp_servers FOR SELECT TO CURRENT_USER USING (true);

ALTER TABLE eacp.mcp_scans ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.mcp_scans FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.mcp_scans USING (tenant_id = eacp.current_tenant_id());
REVOKE UPDATE ON eacp.mcp_scans FROM eacp_app;

ALTER TABLE eacp.tool_definitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.tool_definitions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.tool_definitions USING (tenant_id = eacp.current_tenant_id());
REVOKE UPDATE ON eacp.tool_definitions FROM eacp_app;

-- +goose StatementBegin
-- eacp.mcp_scans_due is the scanner's hint: MCP servers across tenants that
-- are due (requested, or past next_scan_at), whose lease is free or expired
-- and whose secret the worker holds for the endpoint host. It returns only
-- ids and the lease generation; the claim re-checks everything under RLS.
CREATE FUNCTION eacp.mcp_scans_due(p_bindings jsonb, lim integer)
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
REVOKE ALL ON FUNCTION eacp.mcp_scans_due(jsonb, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.mcp_scans_due(jsonb, integer) TO eacp_app;

-- +goose Down

DROP FUNCTION eacp.mcp_scans_due(jsonb, integer);
DROP FUNCTION eacp.mcp_record_scan(uuid, jsonb);
REVOKE UPDATE (definition_id, missing_since, quarantined_at, quarantine_reason) ON eacp.tools FROM eacp_app;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.tool_fingerprint(p_tenant uuid, p_tool uuid) RETURNS bytea
    LANGUAGE sql STABLE
    AS $$
    SELECT sha256(convert_to(concat_ws(E'\n', 'eacp-tool-v1',
                  c.protocol, c.endpoint, c.secret_ref, c.name, t.name), 'UTF8'))
    FROM eacp.tools t
    JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
    WHERE t.tenant_id = p_tenant AND t.id = p_tool
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.tools_guard() RETURNS trigger
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
CREATE OR REPLACE FUNCTION eacp.tool_contracts_guard() RETURNS trigger
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

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.audit_row_change() RETURNS trigger
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

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.action_capability_denial(p_version uuid, p_tool uuid) RETURNS text
    LANGUAGE plpgsql
    AS $$
DECLARE
    v eacp.agent_versions%ROWTYPE;
    tl eacp.tools%ROWTYPE;
    ct eacp.tool_contracts%ROWTYPE;
    allowed boolean;
BEGIN
    SELECT * INTO v FROM eacp.agent_versions
    WHERE tenant_id = eacp.current_tenant_id() AND id = p_version FOR SHARE;
    IF NOT FOUND OR v.state <> 'ACTIVE' OR v.active_allowlist_id IS NULL THEN
        RETURN 'agent_version_not_active';
    END IF;
    IF p_tool IS NULL THEN
        RETURN 'unknown_tool';
    END IF;
    SELECT p_tool = ANY (tool_ids) INTO allowed FROM eacp.agent_allowlists
    WHERE tenant_id = eacp.current_tenant_id() AND id = v.active_allowlist_id;
    IF NOT COALESCE(allowed, false) THEN
        RETURN 'tool_not_in_allowlist';
    END IF;
    SELECT * INTO tl FROM eacp.tools
    WHERE tenant_id = eacp.current_tenant_id() AND id = p_tool FOR SHARE;
    IF tl.active_contract_id IS NULL THEN
        RETURN 'no_active_contract';
    END IF;
    SELECT * INTO ct FROM eacp.tool_contracts
    WHERE tenant_id = eacp.current_tenant_id() AND id = tl.active_contract_id FOR SHARE;
    IF ct.revoked_at IS NOT NULL THEN
        RETURN 'contract_revoked';
    END IF;
    IF ct.fingerprint IS DISTINCT FROM eacp.tool_fingerprint(ct.tenant_id, p_tool) THEN
        RETURN 'contract_fingerprint_mismatch';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

DROP TRIGGER connectors_mcp_server ON eacp.connectors;
DROP FUNCTION eacp.connectors_mcp_server();
ALTER TABLE eacp.tool_contracts DROP COLUMN definition_id;
ALTER TABLE eacp.tools DROP COLUMN definition_id;
DROP TABLE eacp.tool_definitions;
ALTER TABLE eacp.tools
    DROP CONSTRAINT remote_name_unique,
    DROP CONSTRAINT discovered_tools_are_named,
    DROP COLUMN quarantine_changed_by_worker,
    DROP COLUMN quarantine_changed_by,
    DROP COLUMN quarantine_changed_at,
    DROP COLUMN quarantine_reason,
    DROP COLUMN quarantined_at,
    DROP COLUMN missing_since,
    DROP COLUMN origin,
    DROP COLUMN remote_name;
DROP TABLE eacp.mcp_servers;
DROP TABLE eacp.mcp_scans;
DROP FUNCTION eacp.audit_tool_definition();
DROP FUNCTION eacp.tool_definitions_apply();
DROP FUNCTION eacp.tool_definitions_guard();
DROP FUNCTION eacp.audit_mcp_scan();
DROP FUNCTION eacp.mcp_scans_guard();
DROP FUNCTION eacp.audit_mcp_server();
DROP FUNCTION eacp.mcp_servers_guard();
DROP FUNCTION eacp.assert_scan_lease(uuid, uuid);
ALTER TABLE eacp.connectors DROP CONSTRAINT connectors_protocol_check;
ALTER TABLE eacp.connectors ADD CONSTRAINT connectors_protocol_check CHECK (protocol IN ('http'));
DROP FUNCTION eacp.mcp_tool_name(text);
DROP FUNCTION eacp.registry_actor();
