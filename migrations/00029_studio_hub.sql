-- Phase 27b (ADR-033 Rev 1.3): the Agent Hub. A department lead is a flag an
-- admin sets when adding a membership. An agent's owner proposes its ACTIVE,
-- approved version to the Hub at a scope; a lead of its department approves a
-- DEPARTMENT listing, an admin or registry approver an ORG one, and nobody
-- decides their own. PostgreSQL decides who sees a listing, who runs an agent
-- through it and who clones it; a clone is a new, unapproved Studio agent.
-- +goose Up

-- ----------------------------------------------------------------- leads

ALTER TABLE eacp.group_memberships ADD COLUMN lead boolean NOT NULL DEFAULT false;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.group_memberships_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.actor();
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM eacp.assert_role(a, 'admin');
        IF NEW.removed_at IS NOT NULL OR NEW.removed_by IS NOT NULL OR NEW.remove_reason IS NOT NULL THEN
            RAISE EXCEPTION 'a membership cannot be created removed' USING ERRCODE = '23514';
        END IF;
        -- A lead decides the group's Hub listings (ADR-033 Rev 1.3): a human only.
        IF NEW.lead AND NOT EXISTS (SELECT 1 FROM eacp.principals
                                    WHERE tenant_id = NEW.tenant_id AND id = NEW.principal_id AND kind = 'human') THEN
            RAISE EXCEPTION 'only a human leads a group' USING ERRCODE = '42501';
        END IF;
        NEW.added_by := a;
        NEW.added_at := now();
        RETURN NEW;
    END IF;
    IF NEW.lead IS DISTINCT FROM OLD.lead THEN
        RAISE EXCEPTION 'a lead is set only when an admin adds the membership' USING ERRCODE = '55000';
    END IF;
    IF eacp.set_once(OLD.removed_at, NEW.removed_at, OLD.remove_reason, NEW.remove_reason, 'removal') THEN
        PERFORM eacp.assert_role(a, 'admin');
        NEW.removed_by := a;
        NEW.removed_at := now();
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- ---------------------------------------------------------------- tables

-- The Hub listing a clone was copied from (set only by eacp.studio_clone).
ALTER TABLE eacp.studio_agents ADD COLUMN cloned_from_version uuid;
ALTER TABLE eacp.studio_agents ADD FOREIGN KEY (tenant_id, cloned_from_version)
    REFERENCES eacp.studio_versions (tenant_id, id);

-- A proposal to list an agent's version in the Hub at a scope, decided once
-- by that scope's approver or cancelled by its proposer.
CREATE TABLE eacp.studio_listing_proposals (
    tenant_id       uuid        NOT NULL REFERENCES eacp.tenants (id),
    id              uuid        NOT NULL DEFAULT gen_random_uuid(),
    agent_id        uuid        NOT NULL,
    version_id      uuid        NOT NULL,
    scope           text        NOT NULL CHECK (scope IN ('DEPARTMENT', 'ORG')),
    tags            text[]      NOT NULL,
    note            text        CHECK (length(note) <= 500),
    proposed_by     uuid        NOT NULL,
    proposed_at     timestamptz NOT NULL DEFAULT now(),
    decision        text        CHECK (decision IN ('approved', 'rejected', 'cancelled')),
    decided_by      uuid,
    decided_at      timestamptz,
    decision_reason text        CHECK (length(decision_reason) <= 1024),
    PRIMARY KEY (tenant_id, id),
    CHECK (num_nulls(decision, decided_by, decided_at, decision_reason) IN (0, 4)),
    FOREIGN KEY (tenant_id, agent_id) REFERENCES eacp.studio_agents (tenant_id, id),
    FOREIGN KEY (tenant_id, version_id) REFERENCES eacp.studio_versions (tenant_id, id),
    FOREIGN KEY (tenant_id, proposed_by) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, decided_by) REFERENCES eacp.principals (tenant_id, id)
);
CREATE UNIQUE INDEX studio_listing_proposals_one_open ON eacp.studio_listing_proposals (tenant_id, agent_id)
    WHERE decision IS NULL;
CREATE INDEX studio_listing_proposals_open ON eacp.studio_listing_proposals (tenant_id, proposed_at)
    WHERE decision IS NULL;

-- An agent's one Hub listing: what its last approved proposal published,
-- and whether it was deprecated or withdrawn since.
CREATE TABLE eacp.studio_listings (
    tenant_id            uuid        NOT NULL REFERENCES eacp.tenants (id),
    id                   uuid        NOT NULL DEFAULT gen_random_uuid(),
    agent_id             uuid        NOT NULL,
    scope                text        NOT NULL CHECK (scope IN ('DEPARTMENT', 'ORG')),
    state                text        NOT NULL CHECK (state IN ('PUBLISHED', 'DEPRECATED', 'WITHDRAWN')),
    published_version_id uuid        NOT NULL,
    tags                 text[]      NOT NULL,
    proposal_id          uuid        NOT NULL,
    published_by         uuid        NOT NULL,
    published_at         timestamptz NOT NULL DEFAULT now(),
    state_changed_by     uuid,
    state_changed_at     timestamptz,
    state_reason         text        CHECK (length(state_reason) <= 1024),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, agent_id),
    CHECK (num_nulls(state_changed_by, state_changed_at, state_reason) IN (0, 3)),
    CHECK ((state = 'PUBLISHED') = (state_reason IS NULL)),
    FOREIGN KEY (tenant_id, agent_id) REFERENCES eacp.studio_agents (tenant_id, id),
    FOREIGN KEY (tenant_id, published_version_id) REFERENCES eacp.studio_versions (tenant_id, id),
    FOREIGN KEY (tenant_id, proposal_id) REFERENCES eacp.studio_listing_proposals (tenant_id, id),
    FOREIGN KEY (tenant_id, published_by) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, state_changed_by) REFERENCES eacp.principals (tenant_id, id)
);

-- +goose StatementBegin
CREATE FUNCTION eacp.studio_listing_proposals_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF (NEW.tenant_id, NEW.id, NEW.agent_id, NEW.version_id, NEW.scope, NEW.tags, NEW.note, NEW.proposed_by, NEW.proposed_at)
       IS DISTINCT FROM
       (OLD.tenant_id, OLD.id, OLD.agent_id, OLD.version_id, OLD.scope, OLD.tags, OLD.note, OLD.proposed_by, OLD.proposed_at) THEN
        RAISE EXCEPTION 'a Hub proposal is immutable' USING ERRCODE = '55000';
    END IF;
    IF OLD.decision IS NOT NULL THEN
        RAISE EXCEPTION 'this proposal is already decided' USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER studio_listing_proposals_guard BEFORE UPDATE ON eacp.studio_listing_proposals
    FOR EACH ROW EXECUTE FUNCTION eacp.studio_listing_proposals_guard();

-- +goose StatementBegin
DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['studio_listing_proposals', 'studio_listings']
    LOOP
        EXECUTE format('ALTER TABLE eacp.%I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE eacp.%I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('CREATE POLICY tenant_isolation ON eacp.%I USING (tenant_id = eacp.current_tenant_id())', t);
        -- Every write goes through the Hub's functions.
        EXECUTE format('REVOKE ALL ON eacp.%I FROM eacp_app', t);
        EXECUTE format('GRANT SELECT ON eacp.%I TO eacp_app', t);
        EXECUTE format('CREATE TRIGGER zz_audit AFTER INSERT OR UPDATE ON eacp.%I '
                       'FOR EACH ROW EXECUTE FUNCTION eacp.audit_row_change()', t);
    END LOOP;
END
$$;
-- +goose StatementEnd

-- --------------------------------------------------------------- helpers

-- +goose StatementBegin
-- Whether p_actor is an enabled human with a live membership of p_group.
CREATE FUNCTION eacp.studio_is_member(p_actor uuid, p_group uuid) RETURNS boolean
    LANGUAGE sql STABLE
    AS $$
    SELECT EXISTS (SELECT 1 FROM eacp.group_memberships m
                   JOIN eacp.principals p ON p.tenant_id = m.tenant_id AND p.id = m.principal_id
                   WHERE m.tenant_id = eacp.current_tenant_id() AND m.group_id = p_group AND m.principal_id = p_actor
                     AND m.removed_at IS NULL AND p.disabled_at IS NULL AND p.kind = 'human')
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Whether p_actor is an enabled human leading p_group through a live membership.
CREATE FUNCTION eacp.studio_is_lead(p_actor uuid, p_group uuid) RETURNS boolean
    LANGUAGE sql STABLE
    AS $$
    SELECT EXISTS (SELECT 1 FROM eacp.group_memberships m
                   JOIN eacp.principals p ON p.tenant_id = m.tenant_id AND p.id = m.principal_id
                   WHERE m.tenant_id = eacp.current_tenant_id() AND m.group_id = p_group AND m.principal_id = p_actor
                     AND m.lead AND m.removed_at IS NULL AND p.disabled_at IS NULL AND p.kind = 'human')
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Whether p_actor approves a listing at p_scope of an agent of department
-- p_group: a lead of that department, or an admin or registry approver for
-- the organisation.
CREATE FUNCTION eacp.studio_listing_approver(p_actor uuid, p_scope text, p_group uuid) RETURNS boolean
    LANGUAGE sql STABLE
    AS $$
    SELECT CASE p_scope
        WHEN 'DEPARTMENT' THEN eacp.studio_is_lead(p_actor, p_group)
        WHEN 'ORG' THEN eacp.holds_role(p_actor, 'admin') OR eacp.holds_role(p_actor, 'registry_approver')
        ELSE false
    END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Whether a listing at p_scope and p_state of an agent of department p_group
-- reaches p_actor: a published or deprecated listing, and a member of the
-- department or, for the organisation, any enabled human of the tenant.
CREATE FUNCTION eacp.studio_listing_visible(p_actor uuid, p_scope text, p_state text, p_group uuid) RETURNS boolean
    LANGUAGE sql STABLE
    AS $$
    SELECT p_state IN ('PUBLISHED', 'DEPRECATED') AND CASE p_scope
        WHEN 'DEPARTMENT' THEN eacp.studio_is_member(p_actor, p_group)
        WHEN 'ORG' THEN EXISTS (SELECT 1 FROM eacp.principals
                                WHERE tenant_id = eacp.current_tenant_id() AND id = p_actor
                                  AND disabled_at IS NULL AND kind = 'human')
        ELSE false
    END
$$;
-- +goose StatementEnd

-- ------------------------------------------------------------- proposals

-- +goose StatementBegin
-- The agent's owner, holding studio_author, proposes its ACTIVE, approved
-- version to the Hub at a scope with tags. One proposal per agent is open.
CREATE FUNCTION eacp.studio_listing_propose(p_agent uuid, p_version uuid, p_scope text, p_tags text[], p_note text)
    RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    who    jsonb := eacp.actor_context();
    tenant uuid := eacp.current_tenant_id();
    a      uuid;
    v      eacp.agent_versions%ROWTYPE;
    tag    text;
    made   uuid;
BEGIN
    IF who->>'kind' IS DISTINCT FROM 'principal' THEN
        RAISE EXCEPTION 'a Hub proposal is made by a principal' USING ERRCODE = '42501';
    END IF;
    a := (who->>'id')::uuid;
    PERFORM eacp.assert_role(a, 'studio_author');
    PERFORM 1 FROM eacp.studio_agents WHERE tenant_id = tenant AND studio_agents.id = p_agent;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'no Studio agent %', p_agent USING ERRCODE = '23503';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM eacp.agents WHERE tenant_id = tenant AND agents.id = p_agent AND owner_principal_id = a) THEN
        RAISE EXCEPTION 'only the agent''s owner proposes it to the Hub' USING ERRCODE = '42501';
    END IF;
    IF p_scope IS NULL OR p_scope NOT IN ('DEPARTMENT', 'ORG') THEN
        RAISE EXCEPTION 'a Hub scope is DEPARTMENT or ORG' USING ERRCODE = '23514';
    END IF;
    IF p_tags IS NULL OR cardinality(p_tags) > 8
       OR cardinality(p_tags) <> (SELECT count(DISTINCT t) FROM unnest(p_tags) AS u(t)) THEN
        RAISE EXCEPTION 'at most 8 distinct tags' USING ERRCODE = '23514';
    END IF;
    FOREACH tag IN ARRAY p_tags LOOP
        IF tag IS NULL OR tag !~ '^[a-z0-9][a-z0-9-]{0,31}$' THEN
            RAISE EXCEPTION 'tag % is not lower case letters, digits and hyphens', tag USING ERRCODE = '23514';
        END IF;
    END LOOP;
    IF 'template' = ANY (p_tags) AND p_scope <> 'ORG' THEN
        RAISE EXCEPTION 'only an ORG listing is a template' USING ERRCODE = '23514';
    END IF;
    p_tags := ARRAY(SELECT t FROM unnest(p_tags) AS u(t) ORDER BY t);
    IF p_note IS NOT NULL AND length(p_note) > 500 THEN
        RAISE EXCEPTION 'a note is at most 500 characters' USING ERRCODE = '23514';
    END IF;
    SELECT v2.* INTO v FROM eacp.agent_versions v2
    WHERE v2.tenant_id = tenant AND v2.id = p_version AND v2.agent_id = p_agent FOR SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'no version % of agent %', p_version, p_agent USING ERRCODE = '23503';
    END IF;
    IF v.state <> 'ACTIVE' OR NOT EXISTS (SELECT 1 FROM eacp.studio_versions
                                          WHERE tenant_id = tenant AND studio_versions.id = p_version
                                            AND decision = 'approved') THEN
        RAISE EXCEPTION 'only an ACTIVE, approved version is proposed to the Hub' USING ERRCODE = '55000';
    END IF;
    IF EXISTS (SELECT 1 FROM eacp.studio_listing_proposals
               WHERE tenant_id = tenant AND agent_id = p_agent AND decision IS NULL) THEN
        RAISE EXCEPTION 'agent % already has an open Hub proposal', p_agent USING ERRCODE = '55000';
    END IF;
    INSERT INTO eacp.studio_listing_proposals (tenant_id, agent_id, version_id, scope, tags, note, proposed_by)
    VALUES (tenant, p_agent, p_version, p_scope, p_tags, NULLIF(btrim(p_note), ''), a)
    RETURNING studio_listing_proposals.id INTO made;
    RETURN made;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The scope's approver decides a proposal once, never their own and never
-- one for an agent they own. The template tag needs an admin. Approval
-- needs the version still ACTIVE and approved, then publishes it.
CREATE FUNCTION eacp.studio_listing_decide(p_proposal uuid, p_approve boolean, p_reason text) RETURNS text
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    who    jsonb := eacp.actor_context();
    tenant uuid := eacp.current_tenant_id();
    a      uuid;
    p      eacp.studio_listing_proposals%ROWTYPE;
    dept   uuid;
    owner  uuid;
BEGIN
    IF who->>'kind' IS DISTINCT FROM 'principal' THEN
        RAISE EXCEPTION 'a Hub decision is made by a principal' USING ERRCODE = '42501';
    END IF;
    a := (who->>'id')::uuid;
    PERFORM eacp.require_reason(p_reason, 'a Hub decision');
    IF p_approve IS NULL THEN
        RAISE EXCEPTION 'a Hub decision approves or rejects' USING ERRCODE = '23514';
    END IF;
    SELECT * INTO p FROM eacp.studio_listing_proposals WHERE tenant_id = tenant AND id = p_proposal FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'no Hub proposal %', p_proposal USING ERRCODE = '23503';
    END IF;
    IF p.decision IS NOT NULL THEN
        RAISE EXCEPTION 'this proposal is already decided' USING ERRCODE = '55000';
    END IF;
    SELECT s.department_group_id, g.owner_principal_id INTO dept, owner
    FROM eacp.studio_agents s JOIN eacp.agents g ON g.tenant_id = s.tenant_id AND g.id = s.id
    WHERE s.tenant_id = tenant AND s.id = p.agent_id;
    IF a = p.proposed_by OR a = owner THEN
        RAISE EXCEPTION 'two-person rule: nobody decides a Hub proposal for their own agent' USING ERRCODE = '55000';
    END IF;
    IF NOT eacp.studio_listing_approver(a, p.scope, dept) THEN
        RAISE EXCEPTION 'only % decides this proposal',
            CASE p.scope WHEN 'DEPARTMENT' THEN 'a lead of the agent''s department' ELSE 'an admin or a registry approver' END
            USING ERRCODE = '42501';
    END IF;
    IF 'template' = ANY (p.tags) AND NOT eacp.holds_role(a, 'admin') THEN
        RAISE EXCEPTION 'only an admin publishes a template' USING ERRCODE = '42501';
    END IF;
    IF p_approve THEN
        PERFORM 1 FROM eacp.agent_versions v JOIN eacp.studio_versions s ON s.tenant_id = v.tenant_id AND s.id = v.id
        WHERE v.tenant_id = tenant AND v.id = p.version_id AND v.state = 'ACTIVE' AND s.decision = 'approved'
        FOR SHARE OF v;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'version % is no longer ACTIVE; the owner proposes the current one', p.version_id
                USING ERRCODE = '55000';
        END IF;
    END IF;

    UPDATE eacp.studio_listing_proposals
    SET decision = CASE WHEN p_approve THEN 'approved' ELSE 'rejected' END,
        decided_by = a, decided_at = now(), decision_reason = p_reason
    WHERE tenant_id = tenant AND id = p_proposal;
    IF NOT p_approve THEN
        RETURN 'rejected';
    END IF;
    INSERT INTO eacp.studio_listings (tenant_id, agent_id, scope, state, published_version_id, tags, proposal_id, published_by)
    VALUES (tenant, p.agent_id, p.scope, 'PUBLISHED', p.version_id, p.tags, p.id, a)
    ON CONFLICT (tenant_id, agent_id) DO UPDATE
    SET scope = EXCLUDED.scope, state = 'PUBLISHED', published_version_id = EXCLUDED.published_version_id,
        tags = EXCLUDED.tags, proposal_id = EXCLUDED.proposal_id, published_by = EXCLUDED.published_by,
        published_at = now(), state_changed_by = NULL, state_changed_at = NULL, state_reason = NULL;
    RETURN 'PUBLISHED';
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The proposer cancels their open proposal.
CREATE FUNCTION eacp.studio_listing_cancel(p_proposal uuid, p_reason text) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    who    jsonb := eacp.actor_context();
    tenant uuid := eacp.current_tenant_id();
    a      uuid;
    p      eacp.studio_listing_proposals%ROWTYPE;
BEGIN
    IF who->>'kind' IS DISTINCT FROM 'principal' THEN
        RAISE EXCEPTION 'a Hub proposal is cancelled by a principal' USING ERRCODE = '42501';
    END IF;
    a := (who->>'id')::uuid;
    PERFORM eacp.require_reason(p_reason, 'a cancellation');
    SELECT * INTO p FROM eacp.studio_listing_proposals WHERE tenant_id = tenant AND id = p_proposal FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'no Hub proposal %', p_proposal USING ERRCODE = '23503';
    END IF;
    IF a <> p.proposed_by THEN
        RAISE EXCEPTION 'only the proposer cancels a Hub proposal' USING ERRCODE = '42501';
    END IF;
    IF p.decision IS NOT NULL THEN
        RAISE EXCEPTION 'this proposal is already decided' USING ERRCODE = '55000';
    END IF;
    UPDATE eacp.studio_listing_proposals
    SET decision = 'cancelled', decided_by = a, decided_at = now(), decision_reason = p_reason
    WHERE tenant_id = tenant AND id = p_proposal;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The owner, an admin or the approver of the listing's scope deprecates
-- (PUBLISHED -> DEPRECATED) or withdraws (PUBLISHED | DEPRECATED ->
-- WITHDRAWN) a listing with a reason. Both only narrow.
CREATE FUNCTION eacp.studio_listing_retire(p_listing uuid, p_state text, p_reason text) RETURNS text
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    who    jsonb := eacp.actor_context();
    tenant uuid := eacp.current_tenant_id();
    a      uuid;
    l      eacp.studio_listings%ROWTYPE;
    dept   uuid;
    owner  uuid;
BEGIN
    IF who->>'kind' IS DISTINCT FROM 'principal' THEN
        RAISE EXCEPTION 'a listing is retired by a principal' USING ERRCODE = '42501';
    END IF;
    a := (who->>'id')::uuid;
    IF p_state IS NULL OR p_state NOT IN ('DEPRECATED', 'WITHDRAWN') THEN
        RAISE EXCEPTION 'a listing is deprecated or withdrawn' USING ERRCODE = '23514';
    END IF;
    PERFORM eacp.require_reason(p_reason, 'a listing change');
    SELECT * INTO l FROM eacp.studio_listings WHERE tenant_id = tenant AND id = p_listing FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'no Hub listing %', p_listing USING ERRCODE = '23503';
    END IF;
    SELECT s.department_group_id, g.owner_principal_id INTO dept, owner
    FROM eacp.studio_agents s JOIN eacp.agents g ON g.tenant_id = s.tenant_id AND g.id = s.id
    WHERE s.tenant_id = tenant AND s.id = l.agent_id;
    IF NOT (a = owner OR eacp.holds_role(a, 'admin') OR eacp.studio_listing_approver(a, l.scope, dept)) THEN
        RAISE EXCEPTION 'only the owner, an admin or the listing''s approver changes it' USING ERRCODE = '42501';
    END IF;
    IF NOT (l.state = 'PUBLISHED' OR (l.state = 'DEPRECATED' AND p_state = 'WITHDRAWN')) THEN
        RAISE EXCEPTION 'a % listing cannot become %', l.state, p_state USING ERRCODE = '55000';
    END IF;
    UPDATE eacp.studio_listings
    SET state = p_state, state_changed_by = a, state_changed_at = now(), state_reason = p_reason
    WHERE tenant_id = tenant AND id = p_listing;
    RETURN p_state;
END
$$;
-- +goose StatementEnd

-- ---------------------------------------------------------------- reads

-- +goose StatementBegin
-- The listings the transaction's principal sees: the ones that reach them,
-- and every listing of an agent they own.
CREATE FUNCTION eacp.studio_hub_listings()
    RETURNS TABLE (id uuid, agent_id uuid, name text, display_name text, description text, department_id uuid,
                   department_name text, owner_id uuid, scope text, state text, published_version_id uuid,
                   version integer, tags text[], runnable boolean, run_count bigint, published_at timestamptz,
                   cloned_from_version uuid)
    LANGUAGE sql STABLE
    AS $$
    SELECT l.id, l.agent_id, g.name, g.display_name, sa.description, sa.department_group_id, grp.display_name,
           g.owner_principal_id, l.scope, l.state, l.published_version_id, v.version, l.tags,
           l.state IN ('PUBLISHED', 'DEPRECATED') AND v.state = 'ACTIVE',
           (SELECT count(r.id) FROM eacp.studio_runs r WHERE r.tenant_id = l.tenant_id AND r.agent_id = l.agent_id),
           l.published_at, sa.cloned_from_version
    FROM eacp.studio_listings l
    JOIN eacp.studio_agents sa ON sa.tenant_id = l.tenant_id AND sa.id = l.agent_id
    JOIN eacp.agents g ON g.tenant_id = l.tenant_id AND g.id = l.agent_id
    JOIN eacp.groups grp ON grp.tenant_id = sa.tenant_id AND grp.id = sa.department_group_id
    JOIN eacp.agent_versions v ON v.tenant_id = l.tenant_id AND v.id = l.published_version_id
    WHERE l.tenant_id = eacp.current_tenant_id()
      AND (g.owner_principal_id = eacp.actor()
           OR eacp.studio_listing_visible(eacp.actor(), l.scope, l.state, sa.department_group_id))
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The published definition of a listing the transaction's principal sees;
-- any other listing is not found.
CREATE FUNCTION eacp.studio_hub_definition(p_listing uuid) RETURNS text
    LANGUAGE plpgsql STABLE
    AS $$
DECLARE
    def text;
BEGIN
    SELECT s.definition INTO def FROM eacp.studio_hub_listings() h
    JOIN eacp.studio_versions s ON s.tenant_id = eacp.current_tenant_id() AND s.id = h.published_version_id
    WHERE h.id = p_listing;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'no Hub listing %', p_listing USING ERRCODE = '23503';
    END IF;
    RETURN def;
END
$$;
-- +goose StatementEnd

-- ------------------------------------------------------------------ runs

-- +goose StatementBegin
-- The agent's owner, while a member of its department, or anyone its Hub
-- listing reaches starts a run of its ACTIVE, approved version with exactly
-- the declared inputs. A listing runs only the version it publishes.
CREATE OR REPLACE FUNCTION eacp.studio_run_start(p_agent uuid, p_inputs jsonb) RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    who    jsonb := eacp.actor_context();
    tenant uuid := eacp.current_tenant_id();
    a      uuid;
    sa     eacp.studio_agents%ROWTYPE;
    l      eacp.studio_listings%ROWTYPE;
    listed uuid;
    v      eacp.agent_versions%ROWTYPE;
    def    jsonb;
    k      text;
    spec   jsonb;
    run    uuid;
BEGIN
    IF who->>'kind' IS DISTINCT FROM 'principal' THEN
        RAISE EXCEPTION 'a run is started by a principal' USING ERRCODE = '42501';
    END IF;
    a := (who->>'id')::uuid;
    SELECT * INTO sa FROM eacp.studio_agents WHERE tenant_id = tenant AND id = p_agent;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'no Studio agent %', p_agent USING ERRCODE = '23503';
    END IF;
    PERFORM 1 FROM eacp.group_memberships m
    JOIN eacp.principals p ON p.tenant_id = m.tenant_id AND p.id = m.principal_id
    JOIN eacp.agents g ON g.tenant_id = m.tenant_id AND g.id = p_agent AND g.owner_principal_id = a
    WHERE m.tenant_id = tenant AND m.group_id = sa.department_group_id AND m.principal_id = a
      AND m.removed_at IS NULL AND p.disabled_at IS NULL AND p.kind = 'human' FOR SHARE OF m;
    IF NOT FOUND THEN
        SELECT * INTO l FROM eacp.studio_listings WHERE tenant_id = tenant AND agent_id = p_agent FOR SHARE;
        IF NOT FOUND OR NOT eacp.studio_listing_visible(a, l.scope, l.state, sa.department_group_id) THEN
            RAISE EXCEPTION 'only the agent''s owner, or someone its Hub listing reaches, runs it' USING ERRCODE = '42501';
        END IF;
        listed := l.published_version_id;
    END IF;
    SELECT v2.* INTO v FROM eacp.agent_versions v2
    JOIN eacp.studio_versions s ON s.tenant_id = v2.tenant_id AND s.id = v2.id
    WHERE v2.tenant_id = tenant AND v2.agent_id = p_agent AND v2.state = 'ACTIVE' AND s.decision = 'approved'
    FOR SHARE OF v2;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'agent % has no approved ACTIVE version', p_agent USING ERRCODE = '55000';
    END IF;
    IF listed IS NOT NULL AND listed <> v.id THEN
        RAISE EXCEPTION 'the Hub listing publishes version %, which is no longer ACTIVE', listed USING ERRCODE = '55000';
    END IF;
    SELECT definition::jsonb INTO def FROM eacp.studio_versions WHERE tenant_id = tenant AND id = v.id;

    IF jsonb_typeof(p_inputs) IS DISTINCT FROM 'object' THEN
        RAISE EXCEPTION 'inputs must be an object' USING ERRCODE = '23514';
    END IF;
    SELECT key INTO k FROM jsonb_object_keys(p_inputs) AS o(key)
    WHERE NOT COALESCE(def->'inputs', '{}') ? key LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'input % is not declared', k USING ERRCODE = '23514';
    END IF;
    FOR k, spec IN SELECT key, value FROM jsonb_each(COALESCE(def->'inputs', '{}')) LOOP
        IF jsonb_typeof(p_inputs->k) IS DISTINCT FROM 'string' THEN
            RAISE EXCEPTION 'input % is a required string', k USING ERRCODE = '23514';
        END IF;
        IF length(p_inputs->>k) > (spec->>'max_length')::integer THEN
            RAISE EXCEPTION 'input % is longer than %', k, spec->>'max_length' USING ERRCODE = '23514';
        END IF;
    END LOOP;

    INSERT INTO eacp.studio_runs (tenant_id, agent_id, version_id, requested_by, inputs, deadline)
    VALUES (tenant, p_agent, v.id, a, p_inputs,
            now() + make_interval(secs => (def->'limits'->>'timeout_seconds')::integer))
    RETURNING id INTO run;
    RETURN run;
END
$$;
-- +goose StatementEnd

-- ---------------------------------------------------------------- clones

-- +goose StatementBegin
-- eacp.studio_save_as is eacp.studio_save's body (migration 00027), with the
-- version a clone was copied from. It is not SECURITY DEFINER and the
-- application role cannot execute it: only eacp.studio_save and
-- eacp.studio_clone, running as the owner, call it.
CREATE FUNCTION eacp.studio_save_as(p_agent uuid, p_name text, p_display_name text, p_description text,
                                    p_department uuid, p_definition text, p_cloned_from uuid) RETURNS uuid
    LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    who      jsonb := eacp.actor_context();
    tenant   uuid := eacp.current_tenant_id();
    a        uuid;
    cap      text[];
    digest   text;
    tool_ids uuid[];
    version  uuid;
BEGIN
    IF who->>'kind' IS DISTINCT FROM 'principal' THEN
        RAISE EXCEPTION 'a Studio save is made by a principal' USING ERRCODE = '42501';
    END IF;
    a := (who->>'id')::uuid;
    PERFORM eacp.assert_role(a, 'studio_author');
    IF p_agent IS NULL THEN
        IF p_department IS NULL THEN
            RAISE EXCEPTION 'a new Studio agent names its department' USING ERRCODE = '23514';
        END IF;
        PERFORM 1 FROM eacp.group_memberships
        WHERE tenant_id = tenant AND group_id = p_department AND principal_id = a AND removed_at IS NULL FOR SHARE;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'the author is not a member of department %', p_department USING ERRCODE = '42501';
        END IF;
    ELSE
        IF num_nonnulls(p_name, p_display_name, p_description, p_department) > 0 THEN
            RAISE EXCEPTION 'a new version carries only its definition' USING ERRCODE = '23514';
        END IF;
        PERFORM 1 FROM eacp.studio_agents s
        JOIN eacp.agents g ON g.tenant_id = s.tenant_id AND g.id = s.id
        WHERE s.tenant_id = tenant AND s.id = p_agent AND g.owner_principal_id = a;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'no Studio agent % of this author', p_agent USING ERRCODE = '23503';
        END IF;
    END IF;

    cap := eacp.studio_definition_capability(p_definition);
    digest := encode(sha256(convert_to(p_definition, 'UTF8')), 'hex');
    SELECT COALESCE(array_agg(t.id), '{}') INTO tool_ids FROM eacp.tools t
    JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
    WHERE t.tenant_id = tenant AND c.name || '.' || t.name = ANY (cap);

    INSERT INTO eacp.studio_save_marks (tenant_id, xact, open) VALUES (tenant, pg_current_xact_id(), true)
    ON CONFLICT (tenant_id, xact) DO UPDATE SET open = true;
    IF p_agent IS NULL THEN
        INSERT INTO eacp.agents (tenant_id, name, display_name, environment, risk_class, owner_principal_id)
        VALUES (tenant, p_name, p_display_name, 'production', 'high', a) RETURNING id INTO p_agent;
        INSERT INTO eacp.studio_agents (tenant_id, id, department_group_id, description, created_by, cloned_from_version)
        VALUES (tenant, p_agent, p_department, COALESCE(p_description, ''), a, p_cloned_from);
    END IF;
    INSERT INTO eacp.agent_versions (tenant_id, agent_id, runtime, code_ref)
    VALUES (tenant, p_agent, 'studio', 'studio:' || digest) RETURNING id INTO version;
    INSERT INTO eacp.studio_versions (tenant_id, id, agent_id, definition, digest, capability, created_by)
    VALUES (tenant, version, p_agent, p_definition, digest, cap, a);
    INSERT INTO eacp.agent_allowlists (tenant_id, agent_version_id, tool_ids) VALUES (tenant, version, tool_ids);
    UPDATE eacp.studio_save_marks SET open = false WHERE tenant_id = tenant AND xact = pg_current_xact_id();
    RETURN version;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- eacp.studio_save saves a definition as the transaction's principal
-- (migration 00027); its body is eacp.studio_save_as.
CREATE OR REPLACE FUNCTION eacp.studio_save(p_agent uuid, p_name text, p_display_name text, p_description text,
                                            p_department uuid, p_definition text) RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    RETURN eacp.studio_save_as(p_agent, p_name, p_display_name, p_description, p_department, p_definition, NULL);
END
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION eacp.studio_save_as(uuid, text, text, text, uuid, text, uuid) FROM PUBLIC;

-- +goose StatementBegin
-- A studio_author who sees a PUBLISHED listing copies its definition into a
-- new agent of their own department: an ordinary save, so the copy waits
-- for its own approval and carries no permission, key or listing.
CREATE FUNCTION eacp.studio_clone(p_listing uuid, p_name text, p_display_name text, p_department uuid) RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    who    jsonb := eacp.actor_context();
    tenant uuid := eacp.current_tenant_id();
    a      uuid;
    l      eacp.studio_listings%ROWTYPE;
    sa     eacp.studio_agents%ROWTYPE;
    def    text;
BEGIN
    IF who->>'kind' IS DISTINCT FROM 'principal' THEN
        RAISE EXCEPTION 'a clone is made by a principal' USING ERRCODE = '42501';
    END IF;
    a := (who->>'id')::uuid;
    PERFORM eacp.assert_role(a, 'studio_author');
    SELECT * INTO l FROM eacp.studio_listings WHERE tenant_id = tenant AND id = p_listing FOR SHARE;
    IF FOUND THEN
        SELECT * INTO sa FROM eacp.studio_agents WHERE tenant_id = tenant AND id = l.agent_id;
    END IF;
    IF l.id IS NULL OR NOT eacp.studio_listing_visible(a, l.scope, l.state, sa.department_group_id) THEN
        RAISE EXCEPTION 'no Hub listing %', p_listing USING ERRCODE = '23503';
    END IF;
    IF l.state <> 'PUBLISHED' THEN
        RAISE EXCEPTION 'a % listing is not cloned', l.state USING ERRCODE = '55000';
    END IF;
    SELECT definition INTO def FROM eacp.studio_versions WHERE tenant_id = tenant AND id = l.published_version_id;
    RETURN eacp.studio_save_as(NULL, p_name, p_display_name, sa.description, p_department, def, l.published_version_id);
END
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION eacp.studio_listing_propose(uuid, uuid, text, text[], text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.studio_listing_propose(uuid, uuid, text, text[], text) TO eacp_app;
REVOKE ALL ON FUNCTION eacp.studio_listing_decide(uuid, boolean, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.studio_listing_decide(uuid, boolean, text) TO eacp_app;
REVOKE ALL ON FUNCTION eacp.studio_listing_cancel(uuid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.studio_listing_cancel(uuid, text) TO eacp_app;
REVOKE ALL ON FUNCTION eacp.studio_listing_retire(uuid, text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.studio_listing_retire(uuid, text, text) TO eacp_app;
REVOKE ALL ON FUNCTION eacp.studio_clone(uuid, text, text, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.studio_clone(uuid, text, text, uuid) TO eacp_app;

-- +goose Down

DROP FUNCTION eacp.studio_clone(uuid, text, text, uuid);
DROP FUNCTION eacp.studio_hub_definition(uuid);
DROP FUNCTION eacp.studio_hub_listings();
DROP FUNCTION eacp.studio_listing_retire(uuid, text, text);
DROP FUNCTION eacp.studio_listing_cancel(uuid, text);
DROP FUNCTION eacp.studio_listing_decide(uuid, boolean, text);
DROP FUNCTION eacp.studio_listing_propose(uuid, uuid, text, text[], text);
DROP FUNCTION eacp.studio_listing_visible(uuid, text, text, uuid);
DROP FUNCTION eacp.studio_listing_approver(uuid, text, uuid);
DROP FUNCTION eacp.studio_is_lead(uuid, uuid);
DROP FUNCTION eacp.studio_is_member(uuid, uuid);
-- +goose StatementBegin
-- A member of the agent's department starts a run of its ACTIVE, approved
-- version with exactly the declared inputs.
CREATE OR REPLACE FUNCTION eacp.studio_run_start(p_agent uuid, p_inputs jsonb) RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    who    jsonb := eacp.actor_context();
    tenant uuid := eacp.current_tenant_id();
    a      uuid;
    sa     eacp.studio_agents%ROWTYPE;
    v      eacp.agent_versions%ROWTYPE;
    def    jsonb;
    k      text;
    spec   jsonb;
    run    uuid;
BEGIN
    IF who->>'kind' IS DISTINCT FROM 'principal' THEN
        RAISE EXCEPTION 'a run is started by a principal' USING ERRCODE = '42501';
    END IF;
    a := (who->>'id')::uuid;
    SELECT * INTO sa FROM eacp.studio_agents WHERE tenant_id = tenant AND id = p_agent;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'no Studio agent %', p_agent USING ERRCODE = '23503';
    END IF;
    PERFORM 1 FROM eacp.group_memberships m JOIN eacp.principals p ON p.tenant_id = m.tenant_id AND p.id = m.principal_id
    WHERE m.tenant_id = tenant AND m.group_id = sa.department_group_id AND m.principal_id = a
      AND m.removed_at IS NULL AND p.disabled_at IS NULL AND p.kind = 'human' FOR SHARE OF m;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'only a member of the agent''s department runs it' USING ERRCODE = '42501';
    END IF;
    SELECT v2.* INTO v FROM eacp.agent_versions v2
    JOIN eacp.studio_versions s ON s.tenant_id = v2.tenant_id AND s.id = v2.id
    WHERE v2.tenant_id = tenant AND v2.agent_id = p_agent AND v2.state = 'ACTIVE' AND s.decision = 'approved'
    FOR SHARE OF v2;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'agent % has no approved ACTIVE version', p_agent USING ERRCODE = '55000';
    END IF;
    SELECT definition::jsonb INTO def FROM eacp.studio_versions WHERE tenant_id = tenant AND id = v.id;

    IF jsonb_typeof(p_inputs) IS DISTINCT FROM 'object' THEN
        RAISE EXCEPTION 'inputs must be an object' USING ERRCODE = '23514';
    END IF;
    SELECT key INTO k FROM jsonb_object_keys(p_inputs) AS o(key)
    WHERE NOT COALESCE(def->'inputs', '{}') ? key LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'input % is not declared', k USING ERRCODE = '23514';
    END IF;
    FOR k, spec IN SELECT key, value FROM jsonb_each(COALESCE(def->'inputs', '{}')) LOOP
        IF jsonb_typeof(p_inputs->k) IS DISTINCT FROM 'string' THEN
            RAISE EXCEPTION 'input % is a required string', k USING ERRCODE = '23514';
        END IF;
        IF length(p_inputs->>k) > (spec->>'max_length')::integer THEN
            RAISE EXCEPTION 'input % is longer than %', k, spec->>'max_length' USING ERRCODE = '23514';
        END IF;
    END LOOP;

    INSERT INTO eacp.studio_runs (tenant_id, agent_id, version_id, requested_by, inputs, deadline)
    VALUES (tenant, p_agent, v.id, a, p_inputs,
            now() + make_interval(secs => (def->'limits'->>'timeout_seconds')::integer))
    RETURNING id INTO run;
    RETURN run;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- eacp.studio_save saves a definition as the transaction's principal, who
-- must hold studio_author: a new agent (p_agent NULL; the author must be a
-- member of p_department) or the next version of the author's Studio agent.
-- It opens this transaction's save mark, so the registry guards accept the
-- author, and closes it before returning. Returns the version id.
CREATE OR REPLACE FUNCTION eacp.studio_save(p_agent uuid, p_name text, p_display_name text, p_description text,
                                 p_department uuid, p_definition text) RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    who      jsonb := eacp.actor_context();
    tenant   uuid := eacp.current_tenant_id();
    a        uuid;
    cap      text[];
    digest   text;
    tool_ids uuid[];
    version  uuid;
BEGIN
    IF who->>'kind' IS DISTINCT FROM 'principal' THEN
        RAISE EXCEPTION 'a Studio save is made by a principal' USING ERRCODE = '42501';
    END IF;
    a := (who->>'id')::uuid;
    PERFORM eacp.assert_role(a, 'studio_author');
    IF p_agent IS NULL THEN
        IF p_department IS NULL THEN
            RAISE EXCEPTION 'a new Studio agent names its department' USING ERRCODE = '23514';
        END IF;
        PERFORM 1 FROM eacp.group_memberships
        WHERE tenant_id = tenant AND group_id = p_department AND principal_id = a AND removed_at IS NULL FOR SHARE;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'the author is not a member of department %', p_department USING ERRCODE = '42501';
        END IF;
    ELSE
        IF num_nonnulls(p_name, p_display_name, p_description, p_department) > 0 THEN
            RAISE EXCEPTION 'a new version carries only its definition' USING ERRCODE = '23514';
        END IF;
        PERFORM 1 FROM eacp.studio_agents s
        JOIN eacp.agents g ON g.tenant_id = s.tenant_id AND g.id = s.id
        WHERE s.tenant_id = tenant AND s.id = p_agent AND g.owner_principal_id = a;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'no Studio agent % of this author', p_agent USING ERRCODE = '23503';
        END IF;
    END IF;

    cap := eacp.studio_definition_capability(p_definition);
    digest := encode(sha256(convert_to(p_definition, 'UTF8')), 'hex');
    SELECT COALESCE(array_agg(t.id), '{}') INTO tool_ids FROM eacp.tools t
    JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
    WHERE t.tenant_id = tenant AND c.name || '.' || t.name = ANY (cap);

    INSERT INTO eacp.studio_save_marks (tenant_id, xact, open) VALUES (tenant, pg_current_xact_id(), true)
    ON CONFLICT (tenant_id, xact) DO UPDATE SET open = true;
    IF p_agent IS NULL THEN
        INSERT INTO eacp.agents (tenant_id, name, display_name, environment, risk_class, owner_principal_id)
        VALUES (tenant, p_name, p_display_name, 'production', 'high', a) RETURNING id INTO p_agent;
        INSERT INTO eacp.studio_agents (tenant_id, id, department_group_id, description, created_by)
        VALUES (tenant, p_agent, p_department, COALESCE(p_description, ''), a);
    END IF;
    INSERT INTO eacp.agent_versions (tenant_id, agent_id, runtime, code_ref)
    VALUES (tenant, p_agent, 'studio', 'studio:' || digest) RETURNING id INTO version;
    INSERT INTO eacp.studio_versions (tenant_id, id, agent_id, definition, digest, capability, created_by)
    VALUES (tenant, version, p_agent, p_definition, digest, cap, a);
    INSERT INTO eacp.agent_allowlists (tenant_id, agent_version_id, tool_ids) VALUES (tenant, version, tool_ids);
    UPDATE eacp.studio_save_marks SET open = false WHERE tenant_id = tenant AND xact = pg_current_xact_id();
    RETURN version;
END
$$;
-- +goose StatementEnd

DROP FUNCTION eacp.studio_save_as(uuid, text, text, text, uuid, text, uuid);
DROP TABLE eacp.studio_listings;
DROP TABLE eacp.studio_listing_proposals;
DROP FUNCTION eacp.studio_listing_proposals_guard();
ALTER TABLE eacp.studio_agents DROP COLUMN cloned_from_version;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.group_memberships_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.actor();
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM eacp.assert_role(a, 'admin');
        IF NEW.removed_at IS NOT NULL OR NEW.removed_by IS NOT NULL OR NEW.remove_reason IS NOT NULL THEN
            RAISE EXCEPTION 'a membership cannot be created removed' USING ERRCODE = '23514';
        END IF;
        NEW.added_by := a;
        NEW.added_at := now();
        RETURN NEW;
    END IF;
    IF eacp.set_once(OLD.removed_at, NEW.removed_at, OLD.remove_reason, NEW.remove_reason, 'removal') THEN
        PERFORM eacp.assert_role(a, 'admin');
        NEW.removed_by := a;
        NEW.removed_at := now();
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

ALTER TABLE eacp.group_memberships DROP COLUMN lead;
