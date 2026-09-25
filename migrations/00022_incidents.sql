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

-- +goose Down
DROP TABLE eacp.incident_events;
DROP TABLE eacp.incidents;
DROP FUNCTION eacp.incident_events_audit();
DROP FUNCTION eacp.incident_events_guard();
DROP FUNCTION eacp.incidents_timeline();
DROP FUNCTION eacp.incidents_guard();
DROP FUNCTION eacp.incident_object_exists(text, uuid);
