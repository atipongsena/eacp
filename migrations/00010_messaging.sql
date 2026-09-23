-- EACP Phase 10: NATS JetStream signals (ADR-014; MASTER_PLAN §60, §62, §63, §84).
--
-- PostgreSQL stays the only execution authority. This migration adds what the
-- outbox relay and the inbox need, and nothing an action transition could use:
--
--   * action.transition outbox rows: one per action state change, for the
--     dashboard event stream. Only ids, states and the time; never a reason,
--     payload, digest, operation key or external reference.
--   * Outbox rows come only from the action triggers.
--   * Publishing: only the "outbox" messaging actor sets published_at, once,
--     to the database's time; nothing else about a row changes.
--   * Pruning: the "outbox" actor deletes rows published an hour ago, or never
--     published for a day (a lost hint, which §60 allows).
--   * eacp.inbox_messages: the consumer-side dedup record (§63), written and
--     pruned only by the "inbox" messaging actor.
--   * Two reviewed SECURITY DEFINER hints returning (tenant_id, id) only:
--     eacp.outbox_pending and eacp.outbox_prunable.
--
-- The messaging actors are bound with storage.SetSystem("outbox"|"inbox"),
-- but eacp.actor_context() does not accept them: they can never change an
-- action, an approval or anything journaled.

-- +goose Up

-- ---------------------------------------------------- messaging actors

-- +goose StatementBegin
-- eacp.assert_messaging_actor fails unless the transaction's only actor is
-- the named messaging component.
CREATE FUNCTION eacp.assert_messaging_actor(want text) RETURNS void
    LANGUAGE plpgsql STABLE
    AS $$
BEGIN
    IF eacp.current_system_actor() IS DISTINCT FROM want
       OR eacp.current_actor_id() IS NOT NULL OR eacp.current_agent_version_id() IS NOT NULL THEN
        RAISE EXCEPTION 'only the % messaging actor may do this', want USING ERRCODE = '42501';
    END IF;
END
$$;
-- +goose StatementEnd

-- ---------------------------------------------------- dashboard events

-- +goose StatementBegin
-- One action.transition outbox row per state change, including the initial
-- state, in the transaction that made it. It sorts before zz_audit, so the
-- journal append stays the last statement.
CREATE FUNCTION eacp.actions_outbox_events() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF TG_OP = 'UPDATE' AND NEW.state IS NOT DISTINCT FROM OLD.state THEN
        RETURN NULL;
    END IF;
    INSERT INTO eacp.outbox_events (tenant_id, topic, aggregate_id, payload, traceparent)
    VALUES (NEW.tenant_id, 'action.transition', NEW.id,
            jsonb_build_object('action_id', NEW.id,
                               'from', CASE WHEN TG_OP = 'UPDATE' THEN OLD.state END,
                               'to', NEW.state,
                               'at', now()),
            COALESCE(NEW.traceparent, eacp.current_traceparent()));
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER actions_outbox_events AFTER INSERT OR UPDATE ON eacp.actions
    FOR EACH ROW EXECUTE FUNCTION eacp.actions_outbox_events();

-- ---------------------------------------------------- outbox guards

-- +goose StatementBegin
-- Rows come only from the action triggers (a direct INSERT runs this guard
-- at trigger depth 1), bound to the action transaction's actor, unpublished.
CREATE OR REPLACE FUNCTION eacp.outbox_events_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF pg_trigger_depth() < 2 THEN
        RAISE EXCEPTION 'outbox events are written only by the action triggers' USING ERRCODE = '42501';
    END IF;
    PERFORM eacp.actor_context();
    IF NEW.published_at IS NOT NULL THEN
        RAISE EXCEPTION 'an outbox event starts unpublished' USING ERRCODE = '23514';
    END IF;
    NEW.created_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- eacp.outbox_expired is the retention rule: published an hour ago, or never
-- published for a day.
CREATE FUNCTION eacp.outbox_expired(published timestamptz, created timestamptz) RETURNS boolean
    LANGUAGE sql STABLE
    AS $$ SELECT (published IS NOT NULL AND published <= now() - interval '1 hour')
              OR (published IS NULL AND created <= now() - interval '24 hours') $$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The relay marks a row published after the stream's PubAck (ADR-014 §3):
-- once, with the database's time, and nothing else changes. Pruning deletes
-- only expired rows.
CREATE FUNCTION eacp.outbox_events_relay() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM eacp.assert_messaging_actor('outbox');
    IF TG_OP = 'DELETE' THEN
        IF NOT eacp.outbox_expired(OLD.published_at, OLD.created_at) THEN
            RAISE EXCEPTION 'outbox event % is kept until it expires', OLD.id USING ERRCODE = '55000';
        END IF;
        RETURN OLD;
    END IF;
    IF NEW.published_at IS NULL THEN
        RAISE EXCEPTION 'publishing sets published_at' USING ERRCODE = '23514';
    END IF;
    IF OLD.published_at IS NOT NULL THEN
        RAISE EXCEPTION 'outbox event % is already published', OLD.id USING ERRCODE = '55000';
    END IF;
    IF (to_jsonb(NEW) - 'published_at') IS DISTINCT FROM (to_jsonb(OLD) - 'published_at') THEN
        RAISE EXCEPTION 'publishing changes only published_at' USING ERRCODE = '55000';
    END IF;
    NEW.published_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER outbox_events_relay BEFORE UPDATE OR DELETE ON eacp.outbox_events
    FOR EACH ROW EXECUTE FUNCTION eacp.outbox_events_relay();

CREATE INDEX outbox_published ON eacp.outbox_events (published_at) WHERE published_at IS NOT NULL;

GRANT UPDATE (published_at) ON eacp.outbox_events TO eacp_app;
GRANT DELETE ON eacp.outbox_events TO eacp_app;

-- ---------------------------------------------------- inbox (§63)

CREATE TABLE eacp.inbox_messages (
    tenant_id    uuid        NOT NULL REFERENCES eacp.tenants (id),
    consumer     text        NOT NULL CHECK (consumer ~ '^[a-z][a-z0-9_-]{0,63}$'),
    message_id   uuid        NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, consumer, message_id)
);
CREATE INDEX inbox_messages_processed ON eacp.inbox_messages (tenant_id, consumer, processed_at);

-- +goose StatementBegin
-- Only the inbox actor records a message (at the database's time) and
-- prunes records an hour or more old; a record never changes.
CREATE FUNCTION eacp.inbox_messages_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM eacp.assert_messaging_actor('inbox');
    IF TG_OP = 'INSERT' THEN
        NEW.processed_at := now();
        RETURN NEW;
    ELSIF TG_OP = 'UPDATE' THEN
        RAISE EXCEPTION 'an inbox record never changes' USING ERRCODE = '55000';
    END IF;
    IF OLD.processed_at > now() - interval '1 hour' THEN
        RAISE EXCEPTION 'an inbox record is kept for at least an hour' USING ERRCODE = '55000';
    END IF;
    RETURN OLD;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER inbox_messages_guard BEFORE INSERT OR UPDATE OR DELETE ON eacp.inbox_messages
    FOR EACH ROW EXECUTE FUNCTION eacp.inbox_messages_guard();

ALTER TABLE eacp.inbox_messages ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.inbox_messages FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.inbox_messages USING (tenant_id = eacp.current_tenant_id());
REVOKE UPDATE ON eacp.inbox_messages FROM eacp_app;
GRANT DELETE ON eacp.inbox_messages TO eacp_app;

-- ---------------------------------------------------- cross-tenant hints

-- As in 00005-00007: the schema owner may read every outbox row, and only
-- these narrow SECURITY DEFINER functions expose that view. They return
-- (tenant_id, id) pairs; the relay and the pruner lock and act on each row
-- in its own tenant transaction, under RLS and the guards above.
CREATE POLICY owner_scan ON eacp.outbox_events FOR SELECT TO CURRENT_USER USING (true);

CREATE FUNCTION eacp.outbox_pending(p_topics text[], lim integer) RETURNS TABLE (tenant_id uuid, id uuid)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$ SELECT o.tenant_id, o.id FROM eacp.outbox_events o
          WHERE o.published_at IS NULL AND o.topic = ANY (p_topics)
          ORDER BY o.created_at LIMIT least(greatest(lim, 1), 1000) $$;

CREATE FUNCTION eacp.outbox_prunable(lim integer) RETURNS TABLE (tenant_id uuid, id uuid)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$ SELECT o.tenant_id, o.id FROM eacp.outbox_events o
          WHERE (o.published_at IS NOT NULL AND o.published_at <= now() - interval '1 hour')
             OR (o.published_at IS NULL AND o.created_at <= now() - interval '24 hours')
          LIMIT least(greatest(lim, 1), 1000) $$;

REVOKE ALL ON FUNCTION eacp.outbox_pending(text[], integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION eacp.outbox_prunable(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.outbox_pending(text[], integer) TO eacp_app;
GRANT EXECUTE ON FUNCTION eacp.outbox_prunable(integer) TO eacp_app;

-- +goose Down

DROP FUNCTION eacp.outbox_prunable(integer);
DROP FUNCTION eacp.outbox_pending(text[], integer);
DROP POLICY owner_scan ON eacp.outbox_events;

DROP TABLE eacp.inbox_messages;
DROP FUNCTION eacp.inbox_messages_guard();

REVOKE DELETE ON eacp.outbox_events FROM eacp_app;
REVOKE UPDATE (published_at) ON eacp.outbox_events FROM eacp_app;
DROP INDEX eacp.outbox_published;
DROP TRIGGER outbox_events_relay ON eacp.outbox_events;
DROP FUNCTION eacp.outbox_events_relay();
DROP FUNCTION eacp.outbox_expired(timestamptz, timestamptz);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.outbox_events_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM eacp.actor_context();
    IF NEW.published_at IS NOT NULL THEN
        RAISE EXCEPTION 'an outbox event starts unpublished' USING ERRCODE = '23514';
    END IF;
    NEW.created_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

DROP TRIGGER actions_outbox_events ON eacp.actions;
DROP FUNCTION eacp.actions_outbox_events();
DROP FUNCTION eacp.assert_messaging_actor(text);
