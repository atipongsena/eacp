-- Slice A Phase 3: immutable local policy versions, decision evidence and
-- durable approval state (ADR-002, ADR-003, ADR-005).
-- +goose Up

CREATE TABLE eacp.policy_bundles (
    tenant_id      uuid        NOT NULL REFERENCES eacp.tenants (id),
    id             uuid        NOT NULL DEFAULT gen_random_uuid(),
    version        integer     NOT NULL CHECK (version > 0),
    content        jsonb       NOT NULL,
    created_by     uuid,
    created_at     timestamptz NOT NULL DEFAULT now(),
    revoked_by     uuid,
    revoked_at     timestamptz,
    revoke_reason  text,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, version),
    FOREIGN KEY (tenant_id, created_by) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, revoked_by) REFERENCES eacp.principals (tenant_id, id)
);

CREATE TABLE eacp.tenant_policy_pointer (
    tenant_id         uuid        PRIMARY KEY REFERENCES eacp.tenants (id),
    id                uuid        NOT NULL DEFAULT gen_random_uuid(),
    current_bundle_id uuid,
    current_version   integer,
    activated_by      uuid,
    activated_at      timestamptz,
    activation_reason text,
    UNIQUE (tenant_id, id),
    CHECK ((current_bundle_id IS NULL) = (current_version IS NULL)),
    FOREIGN KEY (tenant_id, current_bundle_id) REFERENCES eacp.policy_bundles (tenant_id, id),
    FOREIGN KEY (tenant_id, current_version) REFERENCES eacp.policy_bundles (tenant_id, version),
    FOREIGN KEY (tenant_id, activated_by) REFERENCES eacp.principals (tenant_id, id)
);

CREATE TABLE eacp.decision_evidence (
    tenant_id            uuid        NOT NULL REFERENCES eacp.tenants (id),
    id                   uuid        NOT NULL DEFAULT gen_random_uuid(),
    action_id            uuid        NOT NULL,
    policy_bundle_id     uuid        NOT NULL,
    policy_version       integer     NOT NULL,
    provider             text        NOT NULL CHECK (btrim(provider) <> ''),
    provider_instance_id text        NOT NULL CHECK (btrim(provider_instance_id) <> ''),
    decision_id          uuid        NOT NULL,
    verdict              text        NOT NULL CHECK (verdict IN ('allow', 'warn', 'deny', 'escalate', 'transform')),
    reasons              text[]      NOT NULL CHECK (cardinality(reasons) > 0 AND array_position(reasons, NULL) IS NULL),
    input_digest         bytea       NOT NULL CHECK (octet_length(input_digest) = 32),
    enforced_digest      bytea       NOT NULL CHECK (octet_length(enforced_digest) = 32),
    enforced_payload     jsonb       NOT NULL,
    required_quorum      integer     CHECK (required_quorum BETWEEN 1 AND 5),
    eligible_roles       text[]      CHECK (eligible_roles = ARRAY['approver']),
    approval_ttl_seconds integer     CHECK (approval_ttl_seconds BETWEEN 1 AND 86400),
    evaluated_at         timestamptz NOT NULL,
    recorded_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, decision_id),
    CHECK ((verdict = 'escalate') =
           (required_quorum IS NOT NULL AND eligible_roles IS NOT NULL AND approval_ttl_seconds IS NOT NULL)),
    FOREIGN KEY (tenant_id, policy_bundle_id) REFERENCES eacp.policy_bundles (tenant_id, id),
    FOREIGN KEY (tenant_id, policy_version) REFERENCES eacp.policy_bundles (tenant_id, version)
);

-- +goose StatementBegin
CREATE FUNCTION eacp.decision_evidence_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    v integer;
BEGIN
    PERFORM eacp.actor();
    SELECT version INTO v FROM eacp.policy_bundles
    WHERE tenant_id = NEW.tenant_id AND id = NEW.policy_bundle_id FOR SHARE;
    IF NOT FOUND OR v <> NEW.policy_version THEN
        RAISE EXCEPTION 'evidence policy identity mismatch' USING ERRCODE = '23514';
    END IF;
    IF NEW.evaluated_at > now() + interval '1 minute' THEN
        RAISE EXCEPTION 'evidence evaluation time is in the future' USING ERRCODE = '23514';
    END IF;
    NEW.recorded_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TABLE eacp.approval_requests (
    tenant_id                 uuid        NOT NULL REFERENCES eacp.tenants (id),
    id                        uuid        NOT NULL DEFAULT gen_random_uuid(),
    action_id                 uuid        NOT NULL,
    agent_version_id          uuid        NOT NULL,
    tool_id                   uuid        NOT NULL,
    requesting_subject_id     uuid        NOT NULL,
    decision_evidence_id      uuid        NOT NULL,
    policy_bundle_id          uuid        NOT NULL,
    policy_version            integer     NOT NULL,
    enforced_digest           bytea       NOT NULL CHECK (octet_length(enforced_digest) = 32),
    required_quorum           integer     NOT NULL CHECK (required_quorum BETWEEN 1 AND 5),
    eligible_roles            text[]      NOT NULL CHECK (eligible_roles = ARRAY['approver']),
    risk_class                text        NOT NULL,
    active_allowlist_id       uuid        NOT NULL,
    active_contract_id        uuid        NOT NULL,
    owner_principal_id        uuid,
    owner_group_id            uuid,
    owner_member_ids_snapshot uuid[]      NOT NULL DEFAULT '{}',
    enabling_actor_ids        uuid[]      NOT NULL DEFAULT '{}',
    state                     text        NOT NULL DEFAULT 'PENDING'
                                       CHECK (state IN ('PENDING', 'GRANTED', 'DENIED', 'EXPIRED', 'VOIDED')),
    not_after                 timestamptz NOT NULL,
    expires_at                timestamptz NOT NULL,
    created_by                uuid,
    created_at                timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, decision_evidence_id),
    FOREIGN KEY (tenant_id, agent_version_id) REFERENCES eacp.agent_versions (tenant_id, id),
    FOREIGN KEY (tenant_id, tool_id) REFERENCES eacp.tools (tenant_id, id),
    FOREIGN KEY (tenant_id, requesting_subject_id) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, decision_evidence_id) REFERENCES eacp.decision_evidence (tenant_id, id),
    FOREIGN KEY (tenant_id, policy_bundle_id) REFERENCES eacp.policy_bundles (tenant_id, id),
    FOREIGN KEY (tenant_id, policy_version) REFERENCES eacp.policy_bundles (tenant_id, version),
    FOREIGN KEY (tenant_id, active_allowlist_id) REFERENCES eacp.agent_allowlists (tenant_id, id),
    FOREIGN KEY (tenant_id, active_contract_id) REFERENCES eacp.tool_contracts (tenant_id, id),
    FOREIGN KEY (tenant_id, owner_principal_id) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, owner_group_id) REFERENCES eacp.groups (tenant_id, id),
    FOREIGN KEY (tenant_id, created_by) REFERENCES eacp.principals (tenant_id, id)
);

CREATE TABLE eacp.approval_votes (
    tenant_id             uuid        NOT NULL,
    id                    uuid        NOT NULL DEFAULT gen_random_uuid(),
    request_id            uuid        NOT NULL,
    approver_principal_id uuid        NOT NULL,
    decision              text        NOT NULL CHECK (decision IN ('APPROVE', 'DENY')),
    reason                text        NOT NULL CHECK (btrim(reason) <> ''),
    authorization_basis   jsonb       NOT NULL,
    snapshot_hash         bytea       NOT NULL CHECK (octet_length(snapshot_hash) = 32),
    voted_at              timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, request_id, approver_principal_id),
    FOREIGN KEY (tenant_id, request_id) REFERENCES eacp.approval_requests (tenant_id, id),
    FOREIGN KEY (tenant_id, approver_principal_id) REFERENCES eacp.principals (tenant_id, id)
);

CREATE TABLE eacp.approval_grants (
    tenant_id             uuid        NOT NULL,
    id                    uuid        NOT NULL DEFAULT gen_random_uuid(),
    request_id            uuid        NOT NULL,
    action_id             uuid        NOT NULL,
    enforced_digest       bytea       NOT NULL CHECK (octet_length(enforced_digest) = 32),
    policy_bundle_id      uuid        NOT NULL,
    policy_version        integer     NOT NULL,
    expires_at            timestamptz NOT NULL,
    created_at            timestamptz NOT NULL DEFAULT now(),
    consumed_at           timestamptz,
    consumed_by_action_id uuid,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, request_id),
    CHECK ((consumed_at IS NULL) = (consumed_by_action_id IS NULL)),
    FOREIGN KEY (tenant_id, request_id) REFERENCES eacp.approval_requests (tenant_id, id),
    FOREIGN KEY (tenant_id, policy_bundle_id) REFERENCES eacp.policy_bundles (tenant_id, id),
    FOREIGN KEY (tenant_id, policy_version) REFERENCES eacp.policy_bundles (tenant_id, version)
);

-- +goose StatementBegin
CREATE FUNCTION eacp.local_policy_valid(content jsonb) RETURNS boolean
    LANGUAGE plpgsql IMMUTABLE
    AS $$
DECLARE
    r jsonb;
    seen text[] := '{}';
    v text;
    approval jsonb;
BEGIN
    IF jsonb_typeof(content) <> 'object'
       OR content->>'format_version' <> '1'
       OR jsonb_typeof(content->'rules') <> 'array'
       OR jsonb_array_length(content->'rules') = 0
       OR (SELECT count(*) FROM jsonb_object_keys(content) k WHERE k NOT IN ('format_version', 'rules')) > 0 THEN
        RETURN false;
    END IF;
    FOR r IN SELECT value FROM jsonb_array_elements(content->'rules') LOOP
        IF jsonb_typeof(r) <> 'object' OR jsonb_typeof(r->'id') <> 'string'
           OR jsonb_typeof(r->'reason') <> 'string'
           OR NULLIF(btrim(r->>'id'), '') IS NULL
           OR NULLIF(btrim(r->>'reason'), '') IS NULL
           OR (SELECT count(*) FROM jsonb_object_keys(r) k
               WHERE k NOT IN ('id', 'match', 'verdict', 'reason', 'set', 'approval')) > 0 THEN
            RETURN false;
        END IF;
        IF r->>'id' = ANY (seen) THEN RETURN false; END IF;
        seen := array_append(seen, r->>'id');
        IF r ? 'match' AND jsonb_typeof(r->'match') <> 'object' THEN RETURN false; END IF;
        IF r ? 'set' AND jsonb_typeof(r->'set') <> 'object' THEN RETURN false; END IF;
        IF r ? 'match' AND EXISTS (
            SELECT 1 FROM jsonb_each(r->'match') m
            WHERE m.key NOT IN ('subject', 'operation', 'target', 'tool', 'risk_class', 'side_effect_class')
               OR jsonb_typeof(m.value) <> 'string'
        ) THEN RETURN false; END IF;
        IF r ? 'set' AND r->'set' ? '' THEN RETURN false; END IF;
        IF r ? 'set' AND EXISTS (
            SELECT 1 FROM jsonb_path_query(r->'set', '$.**') AS j
            WHERE CASE WHEN jsonb_typeof(j) = 'number'
                THEN abs((j::text)::numeric) > 9007199254740992
                ELSE false END
        ) THEN RETURN false; END IF;
        v := r->>'verdict';
        IF v NOT IN ('allow', 'warn', 'deny', 'escalate', 'transform') OR v IS NULL THEN RETURN false; END IF;
        IF v IN ('allow', 'warn', 'deny') AND (r ? 'set' OR r ? 'approval') THEN RETURN false; END IF;
        IF v = 'transform' AND (NOT (r ? 'set') OR r ? 'approval' OR r->'set' = '{}'::jsonb) THEN RETURN false; END IF;
        IF v = 'escalate' THEN
            approval := r->'approval';
            IF jsonb_typeof(approval) <> 'object'
               OR NOT (approval ? 'quorum' AND approval ? 'ttl_seconds' AND approval ? 'eligible_roles')
               OR (SELECT count(*) FROM jsonb_object_keys(approval) k
                   WHERE k NOT IN ('quorum', 'ttl_seconds', 'eligible_roles')) > 0
               OR jsonb_typeof(approval->'quorum') <> 'number'
               OR jsonb_typeof(approval->'ttl_seconds') <> 'number'
               OR jsonb_typeof(approval->'eligible_roles') <> 'array'
               OR approval->'eligible_roles' <> '["approver"]'::jsonb
               OR (approval->>'quorum')::integer NOT BETWEEN 1 AND 5
               OR (approval->>'ttl_seconds')::integer NOT BETWEEN 1 AND 86400 THEN
                RETURN false;
            END IF;
        END IF;
    END LOOP;
    RETURN true;
EXCEPTION WHEN invalid_text_representation OR numeric_value_out_of_range THEN
    RETURN false;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.policy_bundles_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.actor();
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM eacp.assert_role(a, 'admin');
        IF NOT eacp.local_policy_valid(NEW.content) THEN
            RAISE EXCEPTION 'invalid local policy bundle' USING ERRCODE = '23514';
        END IF;
        IF NEW.revoked_at IS NOT NULL OR NEW.revoked_by IS NOT NULL OR NEW.revoke_reason IS NOT NULL THEN
            RAISE EXCEPTION 'a policy cannot be created revoked' USING ERRCODE = '23514';
        END IF;
        -- The pointer row is the per-tenant serialization point for version numbering.
        PERFORM 1 FROM eacp.tenant_policy_pointer WHERE tenant_id = NEW.tenant_id FOR UPDATE;
        IF NOT FOUND THEN RAISE EXCEPTION 'missing policy pointer' USING ERRCODE = '55000'; END IF;
        SELECT COALESCE(max(version), 0) + 1 INTO NEW.version
        FROM eacp.policy_bundles WHERE tenant_id = NEW.tenant_id;
        NEW.created_by := a;
        NEW.created_at := now();
        RETURN NEW;
    END IF;
    IF NEW.content IS DISTINCT FROM OLD.content OR NEW.version IS DISTINCT FROM OLD.version
       OR NEW.created_by IS DISTINCT FROM OLD.created_by OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'policy versions are immutable' USING ERRCODE = '55000';
    END IF;
    IF eacp.set_once(OLD.revoked_at, NEW.revoked_at, OLD.revoke_reason, NEW.revoke_reason, 'policy revocation') THEN
        PERFORM eacp.assert_role(a, 'admin', 'operator');
        NEW.revoked_by := a;
        NEW.revoked_at := now();
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.policy_pointer_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.actor();
    bundle eacp.policy_bundles%ROWTYPE;
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF a IS NOT NULL OR NEW.current_bundle_id IS NOT NULL OR NEW.current_version IS NOT NULL THEN
            RAISE EXCEPTION 'policy pointers are seeded by the schema owner' USING ERRCODE = '42501';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id OR NEW.id IS DISTINCT FROM OLD.id THEN
        RAISE EXCEPTION 'policy pointer identity is immutable' USING ERRCODE = '55000';
    END IF;
    IF NEW.current_bundle_id IS NOT DISTINCT FROM OLD.current_bundle_id THEN
        RAISE EXCEPTION 'policy activation must change the bundle' USING ERRCODE = '55000';
    END IF;
    PERFORM eacp.assert_role(a, 'admin');
    PERFORM eacp.require_reason(NEW.activation_reason, 'policy activation');
    SELECT * INTO bundle FROM eacp.policy_bundles
    WHERE tenant_id = NEW.tenant_id AND id = NEW.current_bundle_id FOR SHARE;
    IF NOT FOUND OR bundle.revoked_at IS NOT NULL THEN
        RAISE EXCEPTION 'policy bundle is missing or revoked' USING ERRCODE = '55000';
    END IF;
    IF OLD.current_version IS NOT NULL AND bundle.version <= OLD.current_version THEN
        RAISE EXCEPTION 'policy activation must advance the version' USING ERRCODE = '55000';
    END IF;
    PERFORM eacp.assert_distinct(a, bundle.created_by, 'the policy author');
    NEW.current_version := bundle.version;
    NEW.activated_by := a;
    NEW.activated_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- A pointer is born with each new tenant. This runs in the tenant-creation
-- transaction and carries its tenant context. Existing tenants are seeded
-- below while the migration owns an exclusive lock on the tenants table.
-- +goose StatementBegin
CREATE FUNCTION eacp.seed_policy_pointer() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    INSERT INTO eacp.tenant_policy_pointer (tenant_id) VALUES (NEW.id);
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER policy_bundles_guard BEFORE INSERT OR UPDATE ON eacp.policy_bundles
    FOR EACH ROW EXECUTE FUNCTION eacp.policy_bundles_guard();
CREATE TRIGGER policy_pointer_guard BEFORE INSERT OR UPDATE ON eacp.tenant_policy_pointer
    FOR EACH ROW EXECUTE FUNCTION eacp.policy_pointer_guard();
CREATE TRIGGER decision_evidence_guard BEFORE INSERT ON eacp.decision_evidence
    FOR EACH ROW EXECUTE FUNCTION eacp.decision_evidence_guard();
CREATE TRIGGER zz_policy_pointer_seed AFTER INSERT ON eacp.tenants
    FOR EACH ROW EXECUTE FUNCTION eacp.seed_policy_pointer();

-- +goose StatementBegin
CREATE FUNCTION eacp.approval_requests_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.actor();
    ev eacp.decision_evidence%ROWTYPE;
    p eacp.tenant_policy_pointer%ROWTYPE;
    b eacp.policy_bundles%ROWTYPE;
    v eacp.agent_versions%ROWTYPE;
    ag eacp.agents%ROWTYPE;
    al eacp.agent_allowlists%ROWTYPE;
    tl eacp.tools%ROWTYPE;
    ct eacp.tool_contracts%ROWTYPE;
    subject eacp.principals%ROWTYPE;
    member uuid;
    members uuid[] := '{}';
    enabling uuid[];
    approvals integer;
BEGIN
    IF TG_OP = 'INSERT' THEN
        -- Evidence is immutable and the app role has no UPDATE grant, so a
        -- row lock is unnecessary (and would require UPDATE privilege).
        SELECT * INTO ev FROM eacp.decision_evidence
        WHERE tenant_id = NEW.tenant_id AND id = NEW.decision_evidence_id;
        IF NOT FOUND OR ev.verdict <> 'escalate' OR ev.action_id <> NEW.action_id THEN
            RAISE EXCEPTION 'request requires matching escalation evidence' USING ERRCODE = '55000';
        END IF;
        SELECT * INTO p FROM eacp.tenant_policy_pointer
        WHERE tenant_id = NEW.tenant_id FOR SHARE;
        IF NOT FOUND OR p.current_version IS DISTINCT FROM ev.policy_version
           OR p.current_bundle_id IS DISTINCT FROM ev.policy_bundle_id THEN
            RAISE EXCEPTION 'approval policy is no longer active' USING ERRCODE = '55000';
        END IF;
        SELECT * INTO b FROM eacp.policy_bundles
        WHERE tenant_id = NEW.tenant_id AND id = ev.policy_bundle_id FOR SHARE;
        IF NOT FOUND OR b.revoked_at IS NOT NULL THEN
            RAISE EXCEPTION 'approval policy is revoked' USING ERRCODE = '55000';
        END IF;
        SELECT * INTO subject FROM eacp.principals
        WHERE tenant_id = NEW.tenant_id AND id = NEW.requesting_subject_id FOR SHARE;
        IF NOT FOUND OR subject.disabled_at IS NOT NULL OR subject.kind <> 'human' THEN
            RAISE EXCEPTION 'requesting subject must be an enabled human' USING ERRCODE = '42501';
        END IF;
        SELECT * INTO v FROM eacp.agent_versions
        WHERE tenant_id = NEW.tenant_id AND id = NEW.agent_version_id FOR SHARE;
        IF NOT FOUND OR v.state <> 'ACTIVE' OR v.active_allowlist_id IS NULL THEN
            RAISE EXCEPTION 'agent version is not active' USING ERRCODE = '55000';
        END IF;
        SELECT * INTO ag FROM eacp.agents
        WHERE tenant_id = NEW.tenant_id AND id = v.agent_id;
        SELECT * INTO al FROM eacp.agent_allowlists
        WHERE tenant_id = NEW.tenant_id AND id = v.active_allowlist_id;
        IF NOT FOUND OR NOT NEW.tool_id = ANY (al.tool_ids) THEN
            RAISE EXCEPTION 'tool is not in active allowlist' USING ERRCODE = '55000';
        END IF;
        SELECT * INTO tl FROM eacp.tools
        WHERE tenant_id = NEW.tenant_id AND id = NEW.tool_id FOR SHARE;
        IF NOT FOUND OR tl.active_contract_id IS NULL THEN
            RAISE EXCEPTION 'tool has no active contract' USING ERRCODE = '55000';
        END IF;
        SELECT * INTO ct FROM eacp.tool_contracts
        WHERE tenant_id = NEW.tenant_id AND id = tl.active_contract_id FOR SHARE;
        IF NOT FOUND OR ct.revoked_at IS NOT NULL
           OR ct.fingerprint IS DISTINCT FROM eacp.tool_fingerprint(NEW.tenant_id, NEW.tool_id) THEN
            RAISE EXCEPTION 'active contract is unusable' USING ERRCODE = '55000';
        END IF;
        IF NEW.state <> 'PENDING' OR NEW.not_after <= now() OR NEW.expires_at <= now()
           OR NEW.expires_at > NEW.not_after
           OR NEW.expires_at > ev.evaluated_at + make_interval(secs => ev.approval_ttl_seconds) THEN
            RAISE EXCEPTION 'invalid approval lifetime or initial state' USING ERRCODE = '23514';
        END IF;
        IF ag.owner_group_id IS NOT NULL THEN
            FOR member IN SELECT gm.principal_id FROM eacp.group_memberships gm
                WHERE gm.tenant_id = NEW.tenant_id AND gm.group_id = ag.owner_group_id
                  AND gm.removed_at IS NULL FOR SHARE
            LOOP
                members := array_append(members, member);
            END LOOP;
        END IF;
        SELECT ARRAY(SELECT DISTINCT x FROM unnest(members) x ORDER BY x)
        INTO NEW.owner_member_ids_snapshot;
        enabling := array_remove(ARRAY[b.created_by, p.activated_by, al.created_by,
                                       v.allowlist_changed_by, ct.created_by,
                                       tl.contract_changed_by]::uuid[], NULL);
        SELECT ARRAY(SELECT DISTINCT x FROM unnest(enabling) x ORDER BY x)
        INTO NEW.enabling_actor_ids;
        NEW.policy_bundle_id := ev.policy_bundle_id;
        NEW.policy_version := ev.policy_version;
        NEW.enforced_digest := ev.enforced_digest;
        NEW.required_quorum := ev.required_quorum;
        NEW.eligible_roles := ev.eligible_roles;
        NEW.risk_class := ag.risk_class;
        NEW.active_allowlist_id := al.id;
        NEW.active_contract_id := ct.id;
        NEW.owner_principal_id := ag.owner_principal_id;
        NEW.owner_group_id := ag.owner_group_id;
        NEW.created_by := a;
        NEW.created_at := now();
        RETURN NEW;
    END IF;

    IF (to_jsonb(NEW) - 'state' - 'expires_at') IS DISTINCT FROM
       (to_jsonb(OLD) - 'state' - 'expires_at') THEN
        RAISE EXCEPTION 'approval request identity is immutable' USING ERRCODE = '55000';
    END IF;
    IF NEW.expires_at > OLD.expires_at THEN
        RAISE EXCEPTION 'approval expiry cannot be extended' USING ERRCODE = '55000';
    END IF;
    IF NEW.state IS NOT DISTINCT FROM OLD.state THEN RETURN NEW; END IF;
    IF OLD.state NOT IN ('PENDING', 'GRANTED') THEN
        RAISE EXCEPTION 'approval request is terminal' USING ERRCODE = '55000';
    END IF;
    CASE NEW.state
        WHEN 'GRANTED' THEN
            IF OLD.state <> 'PENDING' THEN
                RAISE EXCEPTION 'only pending requests can be granted' USING ERRCODE = '55000';
            END IF;
            SELECT count(*) INTO approvals FROM eacp.approval_votes
            WHERE tenant_id = NEW.tenant_id AND request_id = NEW.id AND decision = 'APPROVE';
            IF approvals < NEW.required_quorum THEN
                RAISE EXCEPTION 'approval quorum not reached' USING ERRCODE = '55000';
            END IF;
        WHEN 'DENIED' THEN
            IF OLD.state <> 'PENDING' OR NOT EXISTS (
                SELECT 1 FROM eacp.approval_votes WHERE tenant_id = NEW.tenant_id
                AND request_id = NEW.id AND decision = 'DENY') THEN
                RAISE EXCEPTION 'deny vote required' USING ERRCODE = '55000';
            END IF;
        WHEN 'EXPIRED' THEN
            IF now() < NEW.expires_at AND now() < NEW.not_after THEN
                RAISE EXCEPTION 'request has not expired' USING ERRCODE = '55000';
            END IF;
        WHEN 'VOIDED' THEN
            SELECT * INTO p FROM eacp.tenant_policy_pointer
            WHERE tenant_id = NEW.tenant_id FOR SHARE;
            IF p.current_version IS NOT DISTINCT FROM OLD.policy_version THEN
                RAISE EXCEPTION 'current policy has not changed' USING ERRCODE = '55000';
            END IF;
        ELSE
            RAISE EXCEPTION 'illegal approval request transition' USING ERRCODE = '55000';
    END CASE;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.approval_votes_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.actor();
    req eacp.approval_requests%ROWTYPE;
    principal eacp.principals%ROWTYPE;
    version integer;
    role_grant_id uuid;
BEGIN
    PERFORM eacp.assert_role(a, 'approver');
    SELECT * INTO req FROM eacp.approval_requests
    WHERE tenant_id = NEW.tenant_id AND id = NEW.request_id FOR UPDATE;
    IF NOT FOUND OR req.state <> 'PENDING' OR req.expires_at <= now() OR req.not_after <= now() THEN
        RAISE EXCEPTION 'approval request is not pending and unexpired' USING ERRCODE = '55000';
    END IF;
    SELECT current_version INTO version FROM eacp.tenant_policy_pointer
    WHERE tenant_id = NEW.tenant_id FOR SHARE;
    IF version IS DISTINCT FROM req.policy_version THEN
        RAISE EXCEPTION 'approval policy changed' USING ERRCODE = '55000';
    END IF;
    PERFORM 1 FROM eacp.policy_bundles
    WHERE tenant_id = NEW.tenant_id AND id = req.policy_bundle_id
      AND revoked_at IS NULL FOR SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'approval policy is revoked' USING ERRCODE = '55000';
    END IF;
    SELECT * INTO principal FROM eacp.principals
    WHERE tenant_id = NEW.tenant_id AND id = a FOR SHARE;
    IF NOT FOUND OR principal.kind <> 'human' OR principal.disabled_at IS NOT NULL
       OR a = req.requesting_subject_id OR a = req.owner_principal_id
       OR a = ANY (req.owner_member_ids_snapshot)
       OR a = ANY (req.enabling_actor_ids) THEN
        RAISE EXCEPTION 'approver is ineligible by separation of duties' USING ERRCODE = '42501';
    END IF;
    IF req.owner_group_id IS NOT NULL THEN
        PERFORM 1 FROM eacp.group_memberships gm
        WHERE gm.tenant_id = NEW.tenant_id AND gm.group_id = req.owner_group_id
          AND gm.principal_id = a AND gm.removed_at IS NULL FOR SHARE;
        IF FOUND THEN
            RAISE EXCEPTION 'approver currently belongs to the owner group' USING ERRCODE = '42501';
        END IF;
    END IF;
    PERFORM eacp.require_reason(NEW.reason, 'an approval vote');
    SELECT id INTO role_grant_id FROM eacp.role_grants
    WHERE tenant_id = NEW.tenant_id AND principal_id = a AND role = 'approver'
      AND approved_at IS NOT NULL AND revoked_at IS NULL FOR SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'approver role is no longer effective' USING ERRCODE = '42501';
    END IF;
    NEW.approver_principal_id := a;
    NEW.voted_at := now();
    NEW.authorization_basis := jsonb_build_object(
        'role', 'approver', 'role_grant_id', role_grant_id,
        'policy_bundle_id', req.policy_bundle_id, 'policy_version', req.policy_version);
    NEW.snapshot_hash := sha256(convert_to(jsonb_build_object(
        'principal_id', a, 'role_grant_id', role_grant_id,
        'owner_group_id', req.owner_group_id,
        'owner_member_ids_at_request', req.owner_member_ids_snapshot,
        'enabling_actor_ids', req.enabling_actor_ids)::text, 'UTF8'));
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.approval_votes_after() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    req eacp.approval_requests%ROWTYPE;
    approvals integer;
BEGIN
    IF NEW.decision = 'DENY' THEN
        UPDATE eacp.approval_requests SET state = 'DENIED'
        WHERE tenant_id = NEW.tenant_id AND id = NEW.request_id;
        RETURN NULL;
    END IF;
    SELECT * INTO req FROM eacp.approval_requests
    WHERE tenant_id = NEW.tenant_id AND id = NEW.request_id;
    SELECT count(*) INTO approvals FROM eacp.approval_votes
    WHERE tenant_id = NEW.tenant_id AND request_id = NEW.request_id AND decision = 'APPROVE';
    IF approvals >= req.required_quorum THEN
        UPDATE eacp.approval_requests SET state = 'GRANTED'
        WHERE tenant_id = NEW.tenant_id AND id = NEW.request_id;
        INSERT INTO eacp.approval_grants (tenant_id, request_id)
        VALUES (NEW.tenant_id, NEW.request_id);
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.approval_grants_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.actor();
    req eacp.approval_requests%ROWTYPE;
    p eacp.tenant_policy_pointer%ROWTYPE;
BEGIN
    IF TG_OP = 'INSERT' THEN
        SELECT * INTO req FROM eacp.approval_requests
        WHERE tenant_id = NEW.tenant_id AND id = NEW.request_id FOR SHARE;
        IF NOT FOUND OR req.state <> 'GRANTED' OR req.expires_at <= now() THEN
            RAISE EXCEPTION 'grant requires an unexpired granted request' USING ERRCODE = '55000';
        END IF;
        SELECT * INTO p FROM eacp.tenant_policy_pointer
        WHERE tenant_id = NEW.tenant_id FOR SHARE;
        IF p.current_version IS DISTINCT FROM req.policy_version THEN
            RAISE EXCEPTION 'grant policy changed' USING ERRCODE = '55000';
        END IF;
        IF NEW.consumed_at IS NOT NULL OR NEW.consumed_by_action_id IS NOT NULL THEN
            RAISE EXCEPTION 'a grant starts unconsumed' USING ERRCODE = '55000';
        END IF;
        NEW.action_id := req.action_id;
        NEW.enforced_digest := req.enforced_digest;
        NEW.policy_bundle_id := req.policy_bundle_id;
        NEW.policy_version := req.policy_version;
        NEW.expires_at := req.expires_at;
        NEW.created_at := now();
        RETURN NEW;
    END IF;
    IF (to_jsonb(NEW) - 'expires_at' - 'consumed_at' - 'consumed_by_action_id') IS DISTINCT FROM
       (to_jsonb(OLD) - 'expires_at' - 'consumed_at' - 'consumed_by_action_id') THEN
        RAISE EXCEPTION 'grant identity is immutable' USING ERRCODE = '55000';
    END IF;
    IF NEW.expires_at > OLD.expires_at THEN
        RAISE EXCEPTION 'grant expiry cannot be extended' USING ERRCODE = '55000';
    END IF;
    IF NEW.consumed_at IS NOT DISTINCT FROM OLD.consumed_at
       AND NEW.consumed_by_action_id IS NOT DISTINCT FROM OLD.consumed_by_action_id THEN
        RETURN NEW;
    END IF;
    IF OLD.consumed_at IS NOT NULL THEN
        RAISE EXCEPTION 'grant was already consumed' USING ERRCODE = '55000';
    END IF;
    IF NEW.consumed_at IS NULL OR NEW.consumed_by_action_id IS DISTINCT FROM OLD.action_id
       OR OLD.expires_at <= now() OR NEW.expires_at <= now() THEN
        RAISE EXCEPTION 'grant cannot be consumed' USING ERRCODE = '55000';
    END IF;
    SELECT * INTO req FROM eacp.approval_requests
    WHERE tenant_id = NEW.tenant_id AND id = OLD.request_id FOR SHARE;
    SELECT * INTO p FROM eacp.tenant_policy_pointer
    WHERE tenant_id = NEW.tenant_id FOR SHARE;
    IF req.state <> 'GRANTED' OR req.expires_at <= now() OR req.not_after <= now()
       OR p.current_version IS DISTINCT FROM OLD.policy_version
       OR p.current_bundle_id IS DISTINCT FROM OLD.policy_bundle_id THEN
        RAISE EXCEPTION 'grant no longer matches an active approval' USING ERRCODE = '55000';
    END IF;
    PERFORM 1 FROM eacp.policy_bundles
    WHERE tenant_id = NEW.tenant_id AND id = OLD.policy_bundle_id
      AND revoked_at IS NULL FOR SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'grant policy is revoked' USING ERRCODE = '55000';
    END IF;
    NEW.consumed_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER approval_requests_guard BEFORE INSERT OR UPDATE ON eacp.approval_requests
    FOR EACH ROW EXECUTE FUNCTION eacp.approval_requests_guard();
CREATE TRIGGER approval_votes_guard BEFORE INSERT ON eacp.approval_votes
    FOR EACH ROW EXECUTE FUNCTION eacp.approval_votes_guard();
CREATE TRIGGER approval_votes_after AFTER INSERT ON eacp.approval_votes
    FOR EACH ROW EXECUTE FUNCTION eacp.approval_votes_after();
CREATE TRIGGER approval_grants_guard BEFORE INSERT OR UPDATE ON eacp.approval_grants
    FOR EACH ROW EXECUTE FUNCTION eacp.approval_grants_guard();

-- +goose StatementBegin
-- Governance rows contain policy source and digest bytes that must not be
-- copied into the audit journal. The event retains row identity, actor,
-- state, and metadata needed to reconstruct the chain of decisions.
CREATE FUNCTION eacp.audit_governance_change() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a       uuid := eacp.current_actor_id();
    row_new jsonb := to_jsonb(NEW) - 'content' - 'input_digest' - 'enforced_digest'
                     - 'enforced_payload' - 'owner_member_ids_snapshot' - 'snapshot_hash';
    row_old jsonb;
    changed jsonb;
    why text;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        row_old := to_jsonb(OLD) - 'content' - 'input_digest' - 'enforced_digest'
                   - 'enforced_payload' - 'owner_member_ids_snapshot' - 'snapshot_hash';
        SELECT COALESCE(jsonb_object_agg(e.key, jsonb_build_object('from', row_old -> e.key, 'to', e.value)), '{}')
        INTO changed FROM jsonb_each(row_new) AS e
        WHERE row_old -> e.key IS DISTINCT FROM e.value;
        IF changed = '{}'::jsonb THEN RETURN NULL; END IF;
    ELSE
        changed := row_new;
    END IF;
    why := COALESCE(row_new ->> 'reason', row_new ->> 'activation_reason',
                    row_new ->> 'revoke_reason', TG_TABLE_NAME || ' ' || lower(TG_OP));
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
DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['policy_bundles', 'tenant_policy_pointer', 'decision_evidence',
                             'approval_requests', 'approval_votes', 'approval_grants'] LOOP
        EXECUTE format('ALTER TABLE eacp.%I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE eacp.%I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('CREATE POLICY tenant_isolation ON eacp.%I USING (tenant_id = eacp.current_tenant_id())', t);
        EXECUTE format('REVOKE UPDATE ON eacp.%I FROM eacp_app', t);
        EXECUTE format('CREATE TRIGGER zz_audit AFTER INSERT OR UPDATE ON eacp.%I '
                       'FOR EACH ROW EXECUTE FUNCTION eacp.audit_governance_change()', t);
    END LOOP;
END
$$;
-- +goose StatementEnd

REVOKE INSERT ON eacp.tenant_policy_pointer FROM eacp_app;
GRANT UPDATE (revoked_at, revoke_reason) ON eacp.policy_bundles TO eacp_app;
GRANT UPDATE (current_bundle_id, activation_reason) ON eacp.tenant_policy_pointer TO eacp_app;
GRANT UPDATE (state, expires_at) ON eacp.approval_requests TO eacp_app;
GRANT UPDATE (expires_at, consumed_at, consumed_by_action_id) ON eacp.approval_grants TO eacp_app;

-- Backfill existing tenants after audit triggers exist, with each insert
-- attributed to the schema owner in that tenant's own audit chain. The
-- migration is atomic; the temporary NO FORCE window holds an exclusive lock.
ALTER TABLE eacp.tenants NO FORCE ROW LEVEL SECURITY;
-- +goose StatementBegin
DO $$
DECLARE tenant uuid;
BEGIN
    FOR tenant IN SELECT id FROM eacp.tenants LOOP
        PERFORM set_config('app.tenant_id', tenant::text, true);
        INSERT INTO eacp.tenant_policy_pointer (tenant_id) VALUES (tenant);
    END LOOP;
END
$$;
-- +goose StatementEnd
ALTER TABLE eacp.tenants FORCE ROW LEVEL SECURITY;

-- +goose Down
DROP TRIGGER zz_policy_pointer_seed ON eacp.tenants;
DROP TABLE eacp.approval_grants;
DROP TABLE eacp.approval_votes;
DROP TABLE eacp.approval_requests;
DROP TABLE eacp.decision_evidence;
DROP TABLE eacp.tenant_policy_pointer;
DROP TABLE eacp.policy_bundles;
DROP FUNCTION eacp.audit_governance_change();
DROP FUNCTION eacp.seed_policy_pointer();
DROP FUNCTION eacp.approval_grants_guard();
DROP FUNCTION eacp.approval_votes_after();
DROP FUNCTION eacp.approval_votes_guard();
DROP FUNCTION eacp.approval_requests_guard();
DROP FUNCTION eacp.decision_evidence_guard();
DROP FUNCTION eacp.policy_pointer_guard();
DROP FUNCTION eacp.policy_bundles_guard();
DROP FUNCTION eacp.local_policy_valid(jsonb);
