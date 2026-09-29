-- Phase 26a (ADR-032): the worker calls certified MCP tools. A tool of an
-- `mcp` connector is called at most once, even READ_ONLY (the protocol has no
-- idempotency key and every annotation is an untrusted hint), and only the
-- classes the worker reports before or instead of a tool run are certifiable
-- as no-effect. HTTP and A2A contracts are untouched. The redefined function
-- is its latest definition (00023) with the MCP rules added; the Down section
-- restores it.
-- +goose Up

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
                            WHERE e !~ '^(a2a_rejected|connection_refused_before_send|unauthorized|invalid_payload|a2a_rpc_(32700|32600|32601|32602|32004|32005|32008|32009))$') THEN
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
            -- An MCP tool is called at most once (ADR-032 S3.2): the protocol
            -- has no idempotency key and every annotation is an untrusted
            -- hint, so not even a READ_ONLY contract permits a retry, and only
            -- the classes the worker reports before or instead of a tool run
            -- are certifiable as no-effect.
            IF proto = 'mcp' THEN
                IF NEW.idempotency_mode <> 'none' OR NEW.max_attempts <> 1 THEN
                    RAISE EXCEPTION 'an MCP tool is called at most once: idempotency none and one attempt'
                        USING ERRCODE = '23514';
                END IF;
                IF EXISTS (SELECT 1 FROM unnest(NEW.no_effect_errors) e
                            WHERE e !~ '^(connection_refused_before_send|unauthorized|invalid_payload|definition_changed|tool_missing|definition_unverified|unsupported_header_mirroring|mcp_tool_error|mcp_rpc_(32700|32600|32601|32602))$') THEN
                    RAISE EXCEPTION 'an MCP contract certifies as no-effect only what the worker proves'
                        USING ERRCODE = '23514';
                END IF;
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

-- +goose Down

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
                            WHERE e !~ '^(a2a_rejected|connection_refused_before_send|unauthorized|invalid_payload|a2a_rpc_(32700|32600|32601|32602|32004|32005|32008|32009))$') THEN
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
