-- Phase 22a (ADR-027): incidents and the Agent SOC read model. An incident
-- observes and never decides: the incident system actor opens automatic
-- incidents from signals PostgreSQL already holds, and operators work them
-- through OPEN -> ACKNOWLEDGED -> RESOLVED. Every event is journaled.
-- +goose Up

CREATE TABLE eacp.incidents (
    tenant_id         uuid        NOT NULL REFERENCES eacp.tenants (id),
    id                uuid        NOT NULL DEFAULT gen_random_uuid(),
    kind              text        NOT NULL CHECK (kind IN ('mcp_drift', 'kill', 'circuit_open', 'unknown_outcome',
                                                           'canary_rollback', 'finops', 'manual')),
    source_key        text        CHECK (length(source_key) <= 200),
    severity          text        NOT NULL CHECK (severity IN ('low', 'medium', 'high', 'critical')),
    state             text        NOT NULL DEFAULT 'OPEN' CHECK (state IN ('OPEN', 'ACKNOWLEDGED', 'RESOLVED')),
    title             text        NOT NULL CHECK (btrim(title) <> '' AND length(title) <= 200),
    subject_type      text        CHECK (subject_type IN ('tool', 'kill_state', 'connector', 'action', 'release',
                                                          'finops_alert', 'agent', 'agent_version')),
    subject_id        uuid,
    detail            jsonb       NOT NULL DEFAULT '{}'
                                  CHECK (jsonb_typeof(detail) = 'object' AND octet_length(detail::text) <= 16384),
    affected          jsonb       NOT NULL DEFAULT '{}'
                                  CHECK (jsonb_typeof(affected) = 'object' AND octet_length(affected::text) <= 65536),
    opened_by         uuid,
    opened_at         timestamptz NOT NULL DEFAULT now(),
    acknowledged_by   uuid,
    acknowledged_at   timestamptz,
    ack_reason        text        CHECK (length(ack_reason) <= 1024),
    assignee_id       uuid,
    resolved_by       uuid,
    resolved_at       timestamptz,
    resolution        text        CHECK (resolution IN ('contained', 'false_positive', 'accepted_risk', 'duplicate')),
    resolution_reason text        CHECK (length(resolution_reason) <= 1024),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, kind, source_key),
    CHECK ((kind = 'manual') = (source_key IS NULL)),
    CHECK ((subject_type IS NULL) = (subject_id IS NULL)),
    CHECK ((acknowledged_at IS NULL) = (acknowledged_by IS NULL) AND (acknowledged_at IS NULL) = (ack_reason IS NULL)),
    CHECK ((resolved_at IS NULL) = (resolved_by IS NULL) AND (resolved_at IS NULL) = (resolution IS NULL)
           AND (resolved_at IS NULL) = (resolution_reason IS NULL)),
    CHECK (state <> 'OPEN' OR (acknowledged_at IS NULL AND resolved_at IS NULL)),
    CHECK (state <> 'ACKNOWLEDGED' OR (acknowledged_at IS NOT NULL AND resolved_at IS NULL)),
    CHECK (state <> 'RESOLVED' OR (acknowledged_at IS NOT NULL AND resolved_at IS NOT NULL)),
    FOREIGN KEY (tenant_id, opened_by) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, acknowledged_by) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, assignee_id) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, resolved_by) REFERENCES eacp.principals (tenant_id, id)
);
CREATE INDEX incidents_open ON eacp.incidents (tenant_id, opened_at) WHERE state <> 'RESOLVED';

CREATE TABLE eacp.incident_events (
    tenant_id   uuid        NOT NULL,
    id          uuid        NOT NULL DEFAULT gen_random_uuid(),
    incident_id uuid        NOT NULL,
    seq         integer     NOT NULL CHECK (seq > 0),
    kind        text        NOT NULL CHECK (kind IN ('opened', 'acknowledged', 'assigned', 'resolved', 'note', 'linked')),
    actor_id    uuid,
    at          timestamptz NOT NULL DEFAULT now(),
    note        text        CHECK (length(note) <= 4096),
    link_kind   text        CHECK (link_kind IN ('action', 'kill_state', 'fleet_operation', 'release', 'change_set',
                                                 'tool', 'connector', 'agent', 'agent_version', 'finops_alert')),
    link_id     uuid,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, incident_id, seq),
    FOREIGN KEY (tenant_id, incident_id) REFERENCES eacp.incidents (tenant_id, id),
    FOREIGN KEY (tenant_id, actor_id) REFERENCES eacp.principals (tenant_id, id),
    CHECK ((link_kind IS NULL) = (link_id IS NULL)),
    CHECK ((kind = 'linked') = (link_kind IS NOT NULL)),
    CHECK (kind <> 'note' OR btrim(note) <> '')
);

-- +goose StatementBegin
-- Whether a registry or execution object of kind exists in this tenant.
CREATE FUNCTION eacp.incident_object_exists(p_kind text, p_id uuid) RETURNS boolean
    LANGUAGE plpgsql STABLE
    AS $$
BEGIN
    RETURN CASE p_kind
        WHEN 'action' THEN EXISTS (SELECT 1 FROM eacp.actions WHERE id = p_id)
        WHEN 'kill_state' THEN EXISTS (SELECT 1 FROM eacp.kill_states WHERE id = p_id)
        WHEN 'fleet_operation' THEN EXISTS (SELECT 1 FROM eacp.fleet_operations WHERE id = p_id)
        WHEN 'release' THEN EXISTS (SELECT 1 FROM eacp.agent_releases WHERE id = p_id)
        WHEN 'change_set' THEN EXISTS (SELECT 1 FROM eacp.change_sets WHERE id = p_id)
        WHEN 'tool' THEN EXISTS (SELECT 1 FROM eacp.tools WHERE id = p_id)
        WHEN 'connector' THEN EXISTS (SELECT 1 FROM eacp.connectors WHERE id = p_id)
        WHEN 'agent' THEN EXISTS (SELECT 1 FROM eacp.agents WHERE id = p_id)
        WHEN 'agent_version' THEN EXISTS (SELECT 1 FROM eacp.agent_versions WHERE id = p_id)
        WHEN 'finops_alert' THEN EXISTS (SELECT 1 FROM eacp.finops_alerts WHERE id = p_id)
        ELSE false
    END;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Automatic incidents are opened only by the incident system actor; manual
-- ones by an operator or admin with a reason. An incident then moves OPEN ->
-- ACKNOWLEDGED -> RESOLVED or is (re)assigned, one move per statement, by an
-- operator or admin; a critical one is resolved by someone other than its
-- acknowledger. Everything else is immutable.
CREATE FUNCTION eacp.incidents_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.current_actor_id();
    changed text[];
BEGIN
    IF eacp.current_agent_version_id() IS NOT NULL THEN
        RAISE EXCEPTION 'agents do not work incidents' USING ERRCODE = '42501';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.kind = 'manual' THEN
            IF a IS NULL THEN
                RAISE EXCEPTION 'a manual incident is opened by a principal' USING ERRCODE = '42501';
            END IF;
            PERFORM eacp.assert_role(a, 'operator', 'admin');
            PERFORM eacp.require_reason(NEW.detail ->> 'reason', 'a manual incident');
            IF NEW.detail - 'reason' <> '{}'::jsonb THEN
                RAISE EXCEPTION 'a manual incident carries only its reason' USING ERRCODE = '23514';
            END IF;
            IF NEW.subject_id IS NOT NULL AND NOT eacp.incident_object_exists(NEW.subject_type, NEW.subject_id) THEN
                RAISE EXCEPTION 'no such % in this tenant', NEW.subject_type USING ERRCODE = '23503';
            END IF;
            NEW.affected := '{}';
        ELSIF eacp.current_system_actor() IS DISTINCT FROM 'incident' OR a IS NOT NULL THEN
            RAISE EXCEPTION 'automatic incidents are opened by the incident evaluator' USING ERRCODE = '42501';
        END IF;
        IF NEW.assignee_id IS NOT NULL OR NEW.state <> 'OPEN' THEN
            RAISE EXCEPTION 'an incident opens OPEN and unassigned' USING ERRCODE = '23514';
        END IF;
        NEW.opened_by := a;
        NEW.opened_at := now();
        RETURN NEW;
    END IF;

    IF a IS NULL THEN
        RAISE EXCEPTION 'incidents are worked by a principal' USING ERRCODE = '42501';
    END IF;
    PERFORM eacp.assert_role(a, 'operator', 'admin');
    IF OLD.state = 'RESOLVED' THEN
        RAISE EXCEPTION 'incident % is resolved', OLD.id USING ERRCODE = '55000';
    END IF;
    changed := ARRAY(SELECT e.key FROM jsonb_each(to_jsonb(NEW)) e
                      WHERE to_jsonb(OLD) -> e.key IS DISTINCT FROM e.value ORDER BY 1);
    IF OLD.state = 'OPEN' AND NEW.state = 'ACKNOWLEDGED'
       AND changed <@ ARRAY['state', 'ack_reason', 'acknowledged_by', 'acknowledged_at'] THEN
        PERFORM eacp.require_reason(NEW.ack_reason, 'an acknowledgement');
        NEW.acknowledged_by := a;
        NEW.acknowledged_at := now();
    ELSIF OLD.state = 'ACKNOWLEDGED' AND NEW.state = 'RESOLVED'
          AND changed <@ ARRAY['state', 'resolution', 'resolution_reason', 'resolved_by', 'resolved_at'] THEN
        IF NEW.resolution IS NULL THEN
            RAISE EXCEPTION 'a resolution needs a code' USING ERRCODE = '23514';
        END IF;
        PERFORM eacp.require_reason(NEW.resolution_reason, 'a resolution');
        IF OLD.severity = 'critical' THEN
            PERFORM eacp.assert_distinct(a, OLD.acknowledged_by, 'the acknowledger of a critical incident');
        END IF;
        NEW.resolved_by := a;
        NEW.resolved_at := now();
    ELSIF changed = ARRAY['assignee_id'] THEN
        IF NEW.assignee_id IS NOT NULL AND NOT EXISTS (
            SELECT 1 FROM eacp.principals p
              JOIN eacp.role_grants g ON g.tenant_id = p.tenant_id AND g.principal_id = p.id
             WHERE p.id = NEW.assignee_id AND p.disabled_at IS NULL AND g.role IN ('operator', 'admin')
               AND g.approved_at IS NOT NULL AND g.revoked_at IS NULL) THEN
            RAISE EXCEPTION 'an incident is assigned to an enabled operator or admin' USING ERRCODE = '23514';
        END IF;
    ELSE
        RAISE EXCEPTION 'an incident moves OPEN -> ACKNOWLEDGED -> RESOLVED or is assigned; nothing else changes'
            USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Every opening and move of an incident appends its timeline event.
CREATE FUNCTION eacp.incidents_timeline() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    INSERT INTO eacp.incident_events (tenant_id, incident_id, kind, actor_id, note)
    VALUES (NEW.tenant_id, NEW.id,
            CASE WHEN TG_OP = 'INSERT' THEN 'opened'
                 WHEN NEW.state <> OLD.state THEN lower(NEW.state)
                 ELSE 'assigned' END,
            CASE WHEN TG_OP = 'INSERT' THEN NEW.opened_by ELSE eacp.current_actor_id() END,
            CASE WHEN TG_OP = 'INSERT' THEN COALESCE(NEW.detail ->> 'reason', NEW.title)
                 WHEN NEW.state = 'ACKNOWLEDGED' AND OLD.state = 'OPEN' THEN NEW.ack_reason
                 WHEN NEW.state = 'RESOLVED' THEN NEW.resolution || ': ' || NEW.resolution_reason
                 ELSE COALESCE('assigned to ' || NEW.assignee_id::text, 'unassigned') END);
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The timeline is insert-only. Lifecycle events come only from the incident
-- itself; notes and links from an operator or admin, in any state. Each
-- event takes the next sequence number under the incident's row lock.
CREATE FUNCTION eacp.incident_events_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.current_actor_id();
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'the incident timeline is append-only' USING ERRCODE = '42501';
    END IF;
    PERFORM 1 FROM eacp.incidents WHERE tenant_id = NEW.tenant_id AND id = NEW.incident_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'no such incident' USING ERRCODE = '23503';
    END IF;
    IF NEW.kind IN ('note', 'linked') THEN
        IF a IS NULL OR eacp.current_agent_version_id() IS NOT NULL THEN
            RAISE EXCEPTION 'notes and links are added by a principal' USING ERRCODE = '42501';
        END IF;
        PERFORM eacp.assert_role(a, 'operator', 'admin');
        IF NEW.kind = 'linked' AND NOT eacp.incident_object_exists(NEW.link_kind, NEW.link_id) THEN
            RAISE EXCEPTION 'no such % in this tenant', NEW.link_kind USING ERRCODE = '23503';
        END IF;
        NEW.actor_id := a;
    ELSIF pg_trigger_depth() < 2 THEN
        RAISE EXCEPTION 'lifecycle events are written by the incident itself' USING ERRCODE = '42501';
    END IF;
    NEW.seq := COALESCE((SELECT max(seq) FROM eacp.incident_events
                          WHERE tenant_id = NEW.tenant_id AND incident_id = NEW.incident_id), 0) + 1;
    NEW.at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.incident_events_audit() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    INSERT INTO eacp.audit_events (tenant_id, payload)
    VALUES (NEW.tenant_id, convert_to(jsonb_build_object(
        'v', 1,
        'actor', CASE WHEN NEW.actor_id IS NULL
                      THEN jsonb_build_object('kind', 'system', 'id', '00000000-0000-0000-0000-000000000000'::uuid,
                                              'component', 'incident')
                      ELSE jsonb_build_object('kind', 'principal', 'id', NEW.actor_id) END,
        'action', 'incident.' || NEW.kind,
        'subject', jsonb_build_object('type', 'incident', 'id', NEW.incident_id),
        'reason', COALESCE(NEW.note, NEW.kind),
        'data', to_jsonb(NEW) || CASE WHEN NEW.kind = 'opened' THEN
                   (SELECT jsonb_build_object('incident_kind', i.kind, 'severity', i.severity, 'title', i.title,
                                              'source_key', i.source_key)
                      FROM eacp.incidents i WHERE i.tenant_id = NEW.tenant_id AND i.id = NEW.incident_id)
                 ELSE '{}'::jsonb END)::text, 'UTF8'));
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER incidents_guard BEFORE INSERT OR UPDATE ON eacp.incidents
    FOR EACH ROW EXECUTE FUNCTION eacp.incidents_guard();
CREATE TRIGGER incidents_timeline AFTER INSERT OR UPDATE ON eacp.incidents
    FOR EACH ROW EXECUTE FUNCTION eacp.incidents_timeline();
CREATE TRIGGER incident_events_guard BEFORE INSERT OR UPDATE OR DELETE ON eacp.incident_events
    FOR EACH ROW EXECUTE FUNCTION eacp.incident_events_guard();
CREATE TRIGGER zz_audit AFTER INSERT ON eacp.incident_events
    FOR EACH ROW EXECUTE FUNCTION eacp.incident_events_audit();

ALTER TABLE eacp.incidents ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.incidents FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.incidents USING (tenant_id = eacp.current_tenant_id());
ALTER TABLE eacp.incident_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.incident_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.incident_events USING (tenant_id = eacp.current_tenant_id());

REVOKE UPDATE ON eacp.incidents FROM eacp_app;
GRANT UPDATE (state, ack_reason, assignee_id, resolution, resolution_reason) ON eacp.incidents TO eacp_app;
REVOKE UPDATE ON eacp.incident_events FROM eacp_app;

-- +goose StatementBegin
-- The ADR-015 blast radius of one graph node ('tool:<id>', 'mcp:<id>',
-- 'agent_version:<id>', 'model:<name>', 'system:<name>') for every agent
-- version: confirmed through trusted edges, possible through any edge, with
-- every stale, low-confidence or unknown edge widening possible impact.
-- registry.BlastRadius and the incident evaluator share it.
CREATE FUNCTION eacp.blast_radius_versions(p_node text)
    RETURNS TABLE (version_id uuid, confirmed boolean, possible boolean)
    LANGUAGE sql STABLE
    AS $$
    WITH RECURSIVE edges(src, dst, trusted) AS (
        SELECT 'agent_version:' || v.id::text, 'tool:' || t.id::text, true
        FROM eacp.agent_versions v
        JOIN eacp.agent_allowlists al ON al.tenant_id = v.tenant_id AND al.id = v.active_allowlist_id
        JOIN eacp.tools t ON t.tenant_id = al.tenant_id AND t.id = ANY(al.tool_ids)
        UNION ALL
        SELECT 'agent_version:' || v.id::text, 'mcp:' || c.id::text, true
        FROM eacp.agent_versions v
        JOIN eacp.agent_allowlists al ON al.tenant_id = v.tenant_id AND al.id = v.active_allowlist_id
        JOIN eacp.tools t ON t.tenant_id = al.tenant_id AND t.id = ANY(al.tool_ids)
        JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id AND c.protocol = 'mcp'
        UNION ALL
        SELECT d.from_kind || ':' || d.from_id::text,
               d.to_kind || ':' || COALESCE(d.to_id::text, d.to_name),
               d.confidence = 'high' AND d.observed_at <= now() AND d.expires_at > now()
        FROM eacp.dependency_edges d WHERE d.revoked_at IS NULL AND (d.to_id IS NOT NULL OR d.to_name IS NOT NULL)
    ), uncertain(node) AS (
        SELECT d.from_kind || ':' || d.from_id::text FROM eacp.dependency_edges d
        WHERE d.revoked_at IS NULL AND (d.confidence <> 'high' OR d.observed_at > now() OR d.expires_at <= now()
            OR (d.to_id IS NULL AND d.to_name IS NULL))
    ), confirmed(node) AS (
        SELECT p_node UNION SELECT e.src FROM edges e JOIN confirmed c ON e.dst = c.node WHERE e.trusted
    ), possible(node) AS (
        SELECT p_node UNION SELECT node FROM uncertain
        UNION SELECT e.src FROM edges e JOIN possible p ON e.dst = p.node
    )
    SELECT v.id, EXISTS (SELECT 1 FROM confirmed c WHERE c.node = 'agent_version:' || v.id::text),
           EXISTS (SELECT 1 FROM possible p WHERE p.node = 'agent_version:' || v.id::text)
    FROM eacp.agent_versions v
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The graph nodes a kill scope, a connector or an agent covers.
CREATE FUNCTION eacp.incident_scope_nodes(p_scope text, p_id uuid) RETURNS text[]
    LANGUAGE sql STABLE
    AS $$
    SELECT CASE p_scope
        WHEN 'agent_version' THEN ARRAY['agent_version:' || p_id::text]
        WHEN 'tool' THEN ARRAY['tool:' || p_id::text]
        WHEN 'agent' THEN ARRAY(SELECT 'agent_version:' || v.id::text FROM eacp.agent_versions v
                                 WHERE v.agent_id = p_id ORDER BY v.version)
        WHEN 'connector' THEN ARRAY(SELECT n FROM (
                                  SELECT 'mcp:' || c.id::text AS n FROM eacp.connectors c
                                   WHERE c.id = p_id AND c.protocol = 'mcp'
                                  UNION ALL
                                  SELECT 'tool:' || t.id::text FROM eacp.tools t WHERE t.connector_id = p_id) x
                                ORDER BY n)
        ELSE ARRAY[]::text[]
    END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The affected snapshot of an incident: the union of the nodes' blast radii.
CREATE FUNCTION eacp.incident_affected(p_nodes text[]) RETURNS jsonb
    LANGUAGE sql STABLE
    AS $$
    WITH r AS (
        SELECT b.version_id, bool_or(b.confirmed) AS confirmed, bool_or(b.possible) AS possible
          FROM unnest(COALESCE(p_nodes, ARRAY[]::text[])) AS n(node)
          CROSS JOIN LATERAL eacp.blast_radius_versions(n.node) b
         GROUP BY b.version_id
    ), c AS (
        SELECT v.id AS version_id, v.version, v.state, ag.id AS agent_id, ag.name AS agent, ag.environment
          FROM r JOIN eacp.agent_versions v ON v.id = r.version_id
          JOIN eacp.agents ag ON ag.tenant_id = v.tenant_id AND ag.id = v.agent_id
         WHERE r.confirmed
    )
    SELECT jsonb_build_object(
        'nodes', to_jsonb(COALESCE(p_nodes, ARRAY[]::text[])),
        'confirmed', COALESCE((SELECT jsonb_agg(jsonb_build_object('agent_id', x.agent_id, 'agent', x.agent,
                                   'version_id', x.version_id, 'version', x.version, 'environment', x.environment,
                                   'state', x.state) ORDER BY x.agent, x.version)
                                 FROM (SELECT * FROM c ORDER BY agent, version LIMIT 200) x), '[]'::jsonb),
        'confirmed_count', (SELECT count(*) FROM c),
        'possible_count', (SELECT count(*) FROM r WHERE r.possible AND NOT r.confirmed),
        'production_active', (SELECT count(*) FROM c WHERE c.environment = 'production' AND c.state = 'ACTIVE'))
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- critical when a confirmed affected version is ACTIVE on a production agent.
CREATE FUNCTION eacp.incident_severity(p_base text, p_affected jsonb) RETURNS text
    LANGUAGE sql IMMUTABLE
    AS $$
    SELECT CASE WHEN COALESCE((p_affected ->> 'production_active')::int, 0) > 0 THEN 'critical' ELSE p_base END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.incident_micros(p timestamptz) RETURNS text
    LANGUAGE sql STABLE
    AS $$ SELECT (extract(epoch FROM p) * 1000000)::bigint::text $$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Opens one automatic incident unless its occurrence already has one.
CREATE FUNCTION eacp.incident_open(p_kind text, p_key text, p_severity text, p_title text, p_subject_type text,
                                   p_subject_id uuid, p_detail jsonb, p_affected jsonb) RETURNS integer
    LANGUAGE plpgsql
    AS $$
DECLARE
    k integer;
BEGIN
    INSERT INTO eacp.incidents (tenant_id, kind, source_key, severity, title, subject_type, subject_id, detail, affected)
    VALUES (eacp.current_tenant_id(), p_kind, p_key, p_severity, left(p_title, 200), p_subject_type, p_subject_id,
            p_detail, p_affected)
    ON CONFLICT (tenant_id, kind, source_key) DO NOTHING;
    GET DIAGNOSTICS k = ROW_COUNT;
    RETURN k;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The evaluator (ADR-027 §4): one incident per current signal occurrence.
-- It reads the signal tables without locks and writes only incidents, so it
-- never touches an action, kill, circuit or scan transaction. It returns the
-- number of incidents opened.
CREATE FUNCTION eacp.incident_evaluate() RETURNS integer
    LANGUAGE plpgsql
    AS $$
DECLARE
    opened integer := 0;
    s record;
    key text;
    aff jsonb;
BEGIN
    IF eacp.current_system_actor() IS DISTINCT FROM 'incident' OR eacp.current_actor_id() IS NOT NULL THEN
        RAISE EXCEPTION 'the incident evaluator runs as the incident system actor' USING ERRCODE = '42501';
    END IF;

    -- A tool the scanner quarantined: its certified definition changed.
    FOR s IN SELECT t.id, t.name, c.name AS connector, t.quarantined_at, t.quarantine_reason
               FROM eacp.tools t JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
              WHERE t.quarantined_at IS NOT NULL AND t.quarantine_changed_by_worker IS NOT NULL ORDER BY t.id LOOP
        key := format('tool:%s:%s', s.id, eacp.incident_micros(s.quarantined_at));
        CONTINUE WHEN EXISTS (SELECT 1 FROM eacp.incidents WHERE kind = 'mcp_drift' AND source_key = key);
        aff := eacp.incident_affected(ARRAY['tool:' || s.id::text]);
        opened := opened + eacp.incident_open('mcp_drift', key, eacp.incident_severity('high', aff),
            format('MCP tool %s.%s quarantined: its definition changed', s.connector, s.name), 'tool', s.id,
            jsonb_build_object('connector', s.connector, 'tool', s.name, 'quarantined_at', s.quarantined_at,
                               'quarantine_reason', s.quarantine_reason), aff);
    END LOOP;

    -- An active kill scope (reason code and epoch only, never the reason).
    FOR s IN SELECT k.id, k.scope, k.target_id, k.epoch, k.reason_code FROM eacp.kill_states k
              WHERE k.killed ORDER BY k.id LOOP
        key := format('%s:%s:%s', s.scope, s.target_id, s.epoch);
        CONTINUE WHEN EXISTS (SELECT 1 FROM eacp.incidents WHERE kind = 'kill' AND source_key = key);
        aff := eacp.incident_affected(eacp.incident_scope_nodes(s.scope, s.target_id));
        opened := opened + eacp.incident_open('kill', key,
            CASE WHEN s.scope = 'tenant' THEN 'critical' ELSE eacp.incident_severity('high', aff) END,
            format('Kill switch active on %s %s (%s)', s.scope, s.target_id, s.reason_code), 'kill_state', s.id,
            jsonb_build_object('scope', s.scope, 'target_id', s.target_id, 'epoch', s.epoch,
                               'reason_code', s.reason_code), aff);
    END LOOP;

    -- A circuit a worker's breaker opened (an operator disabling one is not).
    FOR s IN SELECT cc.connector_id, c.name, cc.open_until, cc.changed_at FROM eacp.connector_circuits cc
               JOIN eacp.connectors c ON c.tenant_id = cc.tenant_id AND c.id = cc.connector_id
              WHERE cc.open_until > now() AND cc.changed_by_worker IS NOT NULL AND NOT cc.disabled
              ORDER BY cc.connector_id LOOP
        key := format('connector:%s:%s', s.connector_id, eacp.incident_micros(s.changed_at));
        CONTINUE WHEN EXISTS (SELECT 1 FROM eacp.incidents WHERE kind = 'circuit_open' AND source_key = key);
        aff := eacp.incident_affected(eacp.incident_scope_nodes('connector', s.connector_id));
        opened := opened + eacp.incident_open('circuit_open', key, 'medium',
            format('Circuit open on connector %s', s.name), 'connector', s.connector_id,
            jsonb_build_object('connector', s.name, 'open_until', s.open_until, 'changed_at', s.changed_at), aff);
    END LOOP;

    -- An action whose outcome only a human can settle.
    FOR s IN SELECT a.id, a.agent_version_id, a.tool FROM eacp.actions a
              WHERE a.state = 'NEEDS_HUMAN_RESOLUTION' ORDER BY a.id LOOP
        key := 'action:' || s.id::text;
        CONTINUE WHEN EXISTS (SELECT 1 FROM eacp.incidents WHERE kind = 'unknown_outcome' AND source_key = key);
        aff := eacp.incident_affected(ARRAY['agent_version:' || s.agent_version_id::text]);
        opened := opened + eacp.incident_open('unknown_outcome', key, 'high',
            format('Action %s (%s) needs a human: its outcome is unknown', s.id, s.tool), 'action', s.id,
            jsonb_build_object('action_id', s.id, 'tool', s.tool, 'agent_version_id', s.agent_version_id), aff);
    END LOOP;

    -- A canary the release system actor rolled back in the last 7 days.
    FOR s IN SELECT r.id, r.candidate_version_id, r.stable_version_id, r.breaches, ag.name, ag.environment
               FROM eacp.agent_releases r JOIN eacp.agents ag ON ag.tenant_id = r.tenant_id AND ag.id = r.agent_id
              WHERE r.state = 'ROLLED_BACK' AND r.changed_by IS NULL AND r.changed_at >= now() - interval '7 days'
              ORDER BY r.id LOOP
        key := 'release:' || s.id::text;
        CONTINUE WHEN EXISTS (SELECT 1 FROM eacp.incidents WHERE kind = 'canary_rollback' AND source_key = key);
        aff := eacp.incident_affected(ARRAY['agent_version:' || s.candidate_version_id::text]);
        opened := opened + eacp.incident_open('canary_rollback', key,
            CASE WHEN s.environment = 'production' THEN 'critical' ELSE 'high' END,
            format('Canary of %s rolled back: a guardrail was breached', s.name), 'release', s.id,
            jsonb_build_object('agent', s.name, 'candidate_version_id', s.candidate_version_id,
                               'stable_version_id', s.stable_version_id, 'breaches', s.breaches), aff);
    END LOOP;

    -- Spend over its soft limit, or an anomaly, that nobody acknowledged.
    FOR s IN SELECT fa.id, fa.kind, fa.subject_type, fa.subject_id, fa.unit, fa.observed, fa.threshold
               FROM eacp.finops_alerts fa
              WHERE fa.acknowledged_at IS NULL AND fa.kind IN ('soft_limit_exceeded', 'spend_anomaly') ORDER BY fa.id LOOP
        key := 'finops_alert:' || s.id::text;
        CONTINUE WHEN EXISTS (SELECT 1 FROM eacp.incidents WHERE kind = 'finops' AND source_key = key);
        aff := eacp.incident_affected(CASE WHEN s.subject_type = 'agent'
                                           THEN eacp.incident_scope_nodes('agent', s.subject_id) END);
        opened := opened + eacp.incident_open('finops', key, 'medium',
            format('FinOps %s on %s %s', replace(s.kind, '_', ' '), replace(s.subject_type, '_', ' '), s.subject_id),
            'finops_alert', s.id,
            jsonb_build_object('alert_kind', s.kind, 'subject_type', s.subject_type, 'subject_id', s.subject_id,
                               'unit', s.unit, 'observed', s.observed, 'threshold', s.threshold), aff);
    END LOOP;
    RETURN opened;
END
$$;
-- +goose StatementEnd

-- The incident evaluator's cross-tenant hint reads open FinOps alerts.
CREATE POLICY owner_scan ON eacp.finops_alerts FOR SELECT TO CURRENT_USER USING (true);

-- +goose StatementBegin
-- The evaluator's cross-tenant hint: tenant ids only, for tenants with a
-- current signal.
CREATE FUNCTION eacp.incident_tenants() RETURNS TABLE (tenant_id uuid)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT DISTINCT t FROM (
        SELECT tl.tenant_id FROM eacp.tools tl WHERE tl.quarantined_at IS NOT NULL
        UNION SELECT k.tenant_id FROM eacp.kill_states k WHERE k.killed
        UNION SELECT c.tenant_id FROM eacp.connector_circuits c WHERE c.open_until > now()
        UNION SELECT a.tenant_id FROM eacp.actions a WHERE a.state = 'NEEDS_HUMAN_RESOLUTION'
        UNION SELECT r.tenant_id FROM eacp.agent_releases r
               WHERE r.state = 'ROLLED_BACK' AND r.changed_at >= now() - interval '7 days'
        UNION SELECT f.tenant_id FROM eacp.finops_alerts f WHERE f.acknowledged_at IS NULL
    ) x(t) ORDER BY 1
    $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION eacp.incident_tenants() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.incident_tenants() TO eacp_app;

-- +goose Down
DROP FUNCTION eacp.incident_tenants();
DROP POLICY owner_scan ON eacp.finops_alerts;
DROP FUNCTION eacp.incident_evaluate();
DROP FUNCTION eacp.incident_open(text, text, text, text, text, uuid, jsonb, jsonb);
DROP FUNCTION eacp.incident_micros(timestamptz);
DROP FUNCTION eacp.incident_severity(text, jsonb);
DROP FUNCTION eacp.incident_affected(text[]);
DROP FUNCTION eacp.incident_scope_nodes(text, uuid);
DROP FUNCTION eacp.blast_radius_versions(text);
DROP TABLE eacp.incident_events;
DROP TABLE eacp.incidents;
DROP FUNCTION eacp.incident_events_audit();
DROP FUNCTION eacp.incident_events_guard();
DROP FUNCTION eacp.incidents_timeline();
DROP FUNCTION eacp.incidents_guard();
DROP FUNCTION eacp.incident_object_exists(text, uuid);
