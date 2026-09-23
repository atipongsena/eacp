-- Append-only, hash-chained audit journal (ADR-003 §7, invariant 19).
--
-- One chain per tenant. The application only supplies the payload; the
-- database assigns seq, recorded_at, prev_hash and hash in a trigger that
-- serialises on the tenant's chain head, so a client can neither forge nor
-- reorder links:
--
--   hash = sha256(prev_hash || uuid_send(tenant_id) || int8send(seq)
--                 || int8send(epoch_us(recorded_at)) || payload)
--
-- The genesis prev_hash is 32 zero bytes. The payload is stored verbatim so
-- the chain can be recomputed (internal/audit.Verify).
--
-- Convention: append audit events as the LAST statement of a transaction,
-- after the business change, so every transaction takes the chain-head lock
-- after its other row locks (consistent lock order, no deadlocks).

-- +goose Up
CREATE FUNCTION eacp.epoch_us(ts timestamptz) RETURNS bigint
    LANGUAGE sql IMMUTABLE STRICT
    AS $$ SELECT (extract(epoch FROM ts) * 1000000)::bigint $$;

-- +goose StatementBegin
CREATE FUNCTION eacp.reject_modification() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    RAISE EXCEPTION '% on %.% is not allowed: the table is append-only',
        TG_OP, TG_TABLE_SCHEMA, TG_TABLE_NAME;
END
$$;
-- +goose StatementEnd

CREATE TABLE eacp.audit_chain_heads (
    tenant_id uuid   PRIMARY KEY REFERENCES eacp.tenants (id),
    seq       bigint NOT NULL CHECK (seq >= 0),
    hash      bytea  NOT NULL CHECK (octet_length(hash) = 32)
);
ALTER TABLE eacp.audit_chain_heads ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.audit_chain_heads FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.audit_chain_heads
    USING (tenant_id = eacp.current_tenant_id());
-- Only the append trigger (running as the owner) moves the head.
REVOKE INSERT, UPDATE ON eacp.audit_chain_heads FROM eacp_app;

CREATE TABLE eacp.audit_events (
    tenant_id   uuid        NOT NULL REFERENCES eacp.tenants (id),
    seq         bigint      NOT NULL CHECK (seq > 0),
    recorded_at timestamptz NOT NULL,
    payload     bytea       NOT NULL,
    prev_hash   bytea       NOT NULL CHECK (octet_length(prev_hash) = 32),
    hash        bytea       NOT NULL CHECK (octet_length(hash) = 32),
    PRIMARY KEY (tenant_id, seq)
);
ALTER TABLE eacp.audit_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.audit_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.audit_events
    USING (tenant_id = eacp.current_tenant_id());
REVOKE UPDATE ON eacp.audit_events FROM eacp_app;

-- +goose StatementBegin
CREATE FUNCTION eacp.audit_chain_append() RETURNS trigger
    LANGUAGE plpgsql
    SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    head eacp.audit_chain_heads%ROWTYPE;
BEGIN
    IF NEW.tenant_id IS DISTINCT FROM eacp.current_tenant_id() THEN
        RAISE EXCEPTION 'audit event tenant does not match the transaction tenant context';
    END IF;

    INSERT INTO eacp.audit_chain_heads (tenant_id, seq, hash)
    VALUES (NEW.tenant_id, 0, decode(repeat('00', 32), 'hex'))
    ON CONFLICT (tenant_id) DO NOTHING;

    SELECT * INTO STRICT head
    FROM eacp.audit_chain_heads
    WHERE tenant_id = NEW.tenant_id
    FOR UPDATE;

    NEW.seq         := head.seq + 1;
    NEW.recorded_at := clock_timestamp();  -- taken under the head lock: monotonic along the chain
    NEW.prev_hash   := head.hash;
    NEW.hash        := sha256(head.hash || uuid_send(NEW.tenant_id) || int8send(NEW.seq)
                              || int8send(eacp.epoch_us(NEW.recorded_at)) || NEW.payload);

    UPDATE eacp.audit_chain_heads
    SET seq = NEW.seq, hash = NEW.hash
    WHERE tenant_id = NEW.tenant_id;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER audit_events_chain BEFORE INSERT ON eacp.audit_events
    FOR EACH ROW EXECUTE FUNCTION eacp.audit_chain_append();
CREATE TRIGGER audit_events_append_only BEFORE UPDATE OR DELETE ON eacp.audit_events
    FOR EACH ROW EXECUTE FUNCTION eacp.reject_modification();
CREATE TRIGGER audit_events_no_truncate BEFORE TRUNCATE ON eacp.audit_events
    FOR EACH STATEMENT EXECUTE FUNCTION eacp.reject_modification();

REVOKE ALL ON FUNCTION eacp.audit_chain_append() FROM PUBLIC;

-- +goose Down
DROP TABLE eacp.audit_events;
DROP FUNCTION eacp.audit_chain_append();
DROP TABLE eacp.audit_chain_heads;
DROP FUNCTION eacp.reject_modification();
DROP FUNCTION eacp.epoch_us(timestamptz);
