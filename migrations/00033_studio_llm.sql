-- Phase 27c: Studio LLM admission, private typed results and run containment.
-- +goose Up

ALTER TABLE eacp.llm_calls ADD COLUMN studio_run_id uuid;
ALTER TABLE eacp.llm_calls ADD COLUMN studio_node_id uuid;
ALTER TABLE eacp.llm_calls ADD CONSTRAINT llm_calls_studio_run_fkey FOREIGN KEY(tenant_id,studio_run_id) REFERENCES eacp.studio_runs(tenant_id,id);
ALTER TABLE eacp.llm_calls ADD CONSTRAINT llm_calls_studio_node_fkey FOREIGN KEY(tenant_id,studio_node_id) REFERENCES eacp.studio_run_nodes(tenant_id,id);
ALTER TABLE eacp.llm_calls ADD CONSTRAINT llm_calls_studio_pair CHECK((studio_run_id IS NULL)=(studio_node_id IS NULL));
CREATE UNIQUE INDEX llm_calls_studio_node ON eacp.llm_calls(tenant_id,studio_node_id) WHERE studio_node_id IS NOT NULL;

CREATE TABLE eacp.studio_node_results(
 tenant_id uuid NOT NULL, id uuid NOT NULL, call_id uuid NOT NULL,
 output jsonb CHECK(jsonb_typeof(output)='object'), digest text NOT NULL, byte_count integer NOT NULL CHECK(byte_count BETWEEN 2 AND 65536),
 expires_at timestamptz NOT NULL, pruned_at timestamptz,
 PRIMARY KEY(tenant_id,id), FOREIGN KEY(tenant_id,id) REFERENCES eacp.studio_run_nodes(tenant_id,id),
 FOREIGN KEY(tenant_id,call_id) REFERENCES eacp.llm_calls(tenant_id,id), CHECK(pruned_at IS NULL OR output IS NULL)
);
ALTER TABLE eacp.studio_node_results ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.studio_node_results FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.studio_node_results USING(tenant_id=eacp.current_tenant_id());
REVOKE ALL ON eacp.studio_node_results FROM eacp_app;
GRANT SELECT(tenant_id,id,call_id,digest,byte_count,expires_at,pruned_at) ON eacp.studio_node_results TO eacp_app;
-- No output-content audit trigger. Only metadata is retained after pruning.

-- +goose StatementBegin
CREATE FUNCTION eacp.llm_admit_unbound(p jsonb) RETURNS jsonb
    LANGUAGE plpgsql
    AS $$
<<admission>>
DECLARE
    tenant  uuid := eacp.current_tenant_id();
    who     jsonb := eacp.actor_context();
    ver     eacp.agent_versions%ROWTYPE;
    ag      eacp.agents%ROWTYPE;
    m       eacp.llm_models%ROWTYPE;
    pr      eacp.model_prices%ROWTYPE;
    acct    eacp.budget_accounts%ROWTYPE;
    dec     jsonb := p->'decision';
    subj    uuid;
    denial  text;
    req_out bigint;
    est     numeric;
    room    numeric;
    over    numeric;
    allowed boolean;
    call_id uuid := gen_random_uuid();
    due     timestamptz;
BEGIN
    IF who->>'kind' IS DISTINCT FROM 'agent' THEN
        RAISE EXCEPTION 'an LLM call is admitted for an authenticated agent' USING ERRCODE = '42501';
    END IF;
    IF jsonb_typeof(p->'model_name') IS DISTINCT FROM 'string' OR jsonb_typeof(dec) IS DISTINCT FROM 'object'
       OR p->>'provider' NOT IN ('anthropic', 'openai') OR jsonb_typeof(p->'stream') IS DISTINCT FROM 'boolean'
       OR jsonb_typeof(p->'request_bytes') IS DISTINCT FROM 'number' THEN
        RAISE EXCEPTION 'an admission names a model, provider, stream flag, request size and decision'
            USING ERRCODE = '22023';
    END IF;
    IF jsonb_typeof(p->'max_output_tokens') = 'number' THEN
        req_out := (p->>'max_output_tokens')::bigint;
    END IF;

    SELECT * INTO ver FROM eacp.agent_versions WHERE tenant_id = tenant AND id = (who->>'id')::uuid FOR SHARE;
    SELECT * INTO ag FROM eacp.agents WHERE tenant_id = tenant AND id = ver.agent_id;
    IF ver.state <> 'ACTIVE' OR ver.active_allowlist_id IS NULL THEN
        denial := 'agent_version_not_active';
    END IF;
    IF denial IS NULL AND COALESCE(p->>'subject', '') <> '' THEN
        SELECT id INTO subj FROM eacp.principals
        WHERE tenant_id = tenant AND subject = p->>'subject' AND kind = 'human' AND disabled_at IS NULL;
        IF NOT FOUND THEN
            denial := 'subject_invalid';
        END IF;
    END IF;
    SELECT * INTO m FROM eacp.llm_models WHERE tenant_id = tenant AND name = p->>'model_name';  -- immutable: no lock
    IF denial IS NULL AND m.id IS NULL THEN
        denial := 'unknown_model';
    ELSIF denial IS NULL AND m.provider <> p->>'provider' THEN
        denial := 'wrong_provider';
    END IF;
    IF denial IS NULL THEN
        SELECT m.id = ANY (model_ids) INTO allowed FROM eacp.agent_allowlists
        WHERE tenant_id = tenant AND id = ver.active_allowlist_id;
        IF NOT COALESCE(allowed, false) THEN
            denial := 'model_not_in_allowlist';
        END IF;
    END IF;
    IF denial IS NULL THEN
        denial := eacp.release_denial(ver.id, subj);
    END IF;
    IF denial IS NULL AND COALESCE(dec->>'denial', '') <> '' THEN
        denial := dec->>'denial';
    END IF;
    PERFORM pg_advisory_xact_lock(hashtextextended('eacp.kill:' || tenant::text, 0));
    IF denial IS NULL AND eacp.llm_killed(tenant, ag.owner_group_id, ag.id, ver.id, m.id) THEN
        denial := 'killed';
    END IF;
    IF denial IS NULL AND p->>'_studio_run' IS NOT NULL AND eacp.studio_run_killed(tenant,ag.id,ver.id,(p->>'_studio_run')::uuid) THEN denial:='killed'; END IF;
    IF denial IS NULL AND req_out IS NOT NULL AND req_out > m.max_output_tokens THEN
        denial := 'max_tokens_over_cap';
    END IF;
    IF denial IS NULL THEN
        req_out := COALESCE(req_out, m.max_output_tokens);
        SELECT * INTO pr FROM eacp.model_prices
        WHERE tenant_id = tenant AND provider = m.provider AND model = m.upstream_model AND effective_from <= now()
        ORDER BY effective_from DESC LIMIT 1;
        IF NOT FOUND THEN
            denial := 'model_unpriced';
        END IF;
    END IF;
    IF denial IS NULL THEN
        est := eacp.llm_estimate(pr, (p->>'request_bytes')::bigint, req_out);
        SELECT * INTO acct FROM eacp.budget_accounts
        WHERE tenant_id = tenant AND agent_id = ag.id AND unit = pr.unit FOR NO KEY UPDATE;
        IF FOUND THEN
            PERFORM eacp.budget_fold(acct.id);
            SELECT hard_limit - allocated - reserved - committed INTO room FROM eacp.budget_accounts
            WHERE tenant_id = tenant AND id = acct.id;
            -- A settled call's cost above its reservation was not committed
            -- (a reservation never commits more than it holds); it is real
            -- spend, so it counts against every later admission.
            SELECT COALESCE(sum(cost_amount - committed_amount), 0) INTO over FROM eacp.llm_calls
            WHERE tenant_id = tenant AND agent_id = ag.id AND cost_unit = pr.unit
              AND cost_amount > committed_amount;
            IF est + over > room THEN
                denial := 'budget_exceeded';
            END IF;
        ELSIF p->>'_studio_run' IS NOT NULL THEN denial:='budget_pending';
        END IF;
    END IF;

    PERFORM set_config('eacp.llm_call', call_id::text, true);
    IF denial IS NULL THEN
        due := now() + make_interval(secs => m.timeout_ms / 1000.0) + interval '60 seconds';
        IF p->>'_studio_run' IS NOT NULL THEN due:=LEAST(due,(p->>'_studio_deadline')::timestamptz); END IF;
    END IF;
    INSERT INTO eacp.llm_calls (tenant_id, id, agent_version_id, model_id, model_name, provider,
        subject_principal_id, stream, trace_id, request_bytes, max_output_tokens, decision_id, policy_bundle_id,
        policy_version, verdict, input_digest, price_id, cost_unit, state, denial, gateway_id, deadline, studio_run_id, studio_node_id)
    VALUES (tenant, call_id, ver.id, m.id, p->>'model_name', p->>'provider', subj, (p->>'stream')::boolean,
        NULLIF(p->>'trace_id', ''), (p->>'request_bytes')::bigint,
        CASE WHEN denial IS NULL THEN req_out
             WHEN req_out BETWEEN 1 AND 1000000000 THEN req_out END,
        (dec->>'id')::uuid, (dec->>'bundle_id')::uuid, (dec->>'version')::integer, dec->>'verdict',
        NULLIF(dec->>'input_digest', ''),
        CASE WHEN denial IS NULL THEN pr.id END, CASE WHEN denial IS NULL THEN pr.unit END,
        CASE WHEN denial IS NULL THEN 'ADMITTED' ELSE 'DENIED' END, denial, p->>'gateway_id', due, (p->>'_studio_run')::uuid, (p->>'_studio_node')::uuid);
    IF denial IS NULL AND acct.id IS NOT NULL THEN
        INSERT INTO eacp.budget_reservations (tenant_id, account_id, unit, llm_call_id, amount, expires_at)
        VALUES (tenant, acct.id, pr.unit, call_id, est, due);
    END IF;
    IF p->>'_studio_node' IS NOT NULL THEN
        UPDATE eacp.studio_run_nodes SET call_id=admission.call_id WHERE tenant_id=tenant AND id=(p->>'_studio_node')::uuid;
    END IF;
    INSERT INTO eacp.audit_events (tenant_id, payload)
    VALUES (tenant, convert_to(jsonb_build_object(
        'v', 1, 'actor', who,
        'action', CASE WHEN denial IS NULL THEN 'llm.admitted' ELSE 'llm.denied' END,
        'subject', jsonb_build_object('type', 'llm_call', 'id', call_id),
        'reason', COALESCE(denial, 'admitted'),
        'data', jsonb_strip_nulls(jsonb_build_object(
            'model', p->>'model_name', 'model_id', m.id, 'provider', p->>'provider', 'stream', p->'stream',
            'request_bytes', p->'request_bytes', 'max_output_tokens', req_out, 'decision_id', dec->>'id',
            'verdict', dec->>'verdict', 'reservation', est, 'unit', pr.unit, 'deadline', due,
            'gateway_id', p->>'gateway_id')))::text, 'UTF8'));
    IF denial IS NOT NULL THEN
        RETURN jsonb_build_object('call_id', call_id, 'denial', denial);
    END IF;
    RETURN jsonb_build_object('call_id', call_id, 'denial', '', 'model_id', m.id, 'upstream_model', m.upstream_model,
        'base_url', m.base_url, 'secret_ref', m.secret_ref, 'timeout_ms', CASE WHEN p->>'_studio_run' IS NULL THEN m.timeout_ms ELSE LEAST(m.timeout_ms,GREATEST(1,ceil(extract(epoch FROM due-now())*1000)::integer)) END,
        'max_output_tokens', req_out, 'deadline', due);
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.llm_settle_unbound(p_call uuid, p_outcome text, p_status integer, p_input bigint,
                                p_cache_read bigint, p_cache_write bigint, p_output bigint, p_known boolean)
    RETURNS jsonb
    LANGUAGE plpgsql
    AS $$
DECLARE
    c eacp.llm_calls%ROWTYPE;
BEGIN
    IF eacp.current_system_actor() IS DISTINCT FROM 'llm_gateway' OR eacp.current_actor_id() IS NOT NULL
       OR eacp.current_agent_version_id() IS NOT NULL THEN
        RAISE EXCEPTION 'a call is settled by the LLM gateway' USING ERRCODE = '42501';
    END IF;
    IF p_outcome = 'abandoned' THEN
        RAISE EXCEPTION 'only the sweeper abandons a call' USING ERRCODE = '42501';
    END IF;
    IF p_outcome IS NULL OR p_outcome NOT IN ('succeeded', 'provider_error', 'usage_unknown', 'killed') THEN
        RAISE EXCEPTION 'unknown outcome %', p_outcome USING ERRCODE = '22023';
    END IF;
    SELECT * INTO c FROM eacp.llm_calls WHERE tenant_id = eacp.current_tenant_id() AND id = p_call FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'unknown call %', p_call USING ERRCODE = '23503';
    END IF;
    IF c.state <> 'ADMITTED' THEN
        RAISE EXCEPTION 'call % is %', p_call, c.state USING ERRCODE = '55000';
    END IF;
    RETURN eacp.llm_finish(c, p_outcome, p_status, p_input, p_cache_read, p_cache_write, p_output, p_known);
END
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION eacp.llm_admit_unbound(jsonb),eacp.llm_settle_unbound(uuid,text,integer,bigint,bigint,bigint,bigint,boolean) FROM PUBLIC,eacp_app;

-- +goose StatementBegin
CREATE FUNCTION eacp.studio_output_matches(s jsonb,v jsonb,depth integer DEFAULT 0) RETURNS boolean
LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE typ text:=s->>'type'; k text; item jsonb; n numeric;
BEGIN
    IF depth>4 OR v IS NULL OR jsonb_typeof(v) IS DISTINCT FROM (CASE WHEN typ='integer' THEN 'number' ELSE typ END) THEN RETURN false; END IF;
    IF s ? 'enum' AND NOT EXISTS(SELECT 1 FROM jsonb_array_elements(s->'enum') x WHERE x=v) THEN RETURN false; END IF;
    IF typ='object' THEN
        IF EXISTS(SELECT 1 FROM jsonb_object_keys(v) x WHERE NOT (s->'properties') ? x)
        OR EXISTS(SELECT 1 FROM jsonb_array_elements_text(COALESCE(s->'required','[]')) x WHERE NOT v ? x) THEN RETURN false; END IF;
        FOR k,item IN SELECT * FROM jsonb_each(v) LOOP
            IF NOT eacp.studio_output_matches(s->'properties'->k,item,depth+1) THEN RETURN false; END IF;
        END LOOP;
    ELSIF typ='array' THEN
        IF jsonb_array_length(v)> (s->>'maxItems')::integer OR jsonb_array_length(v)<COALESCE((s->>'minItems')::integer,0) THEN RETURN false; END IF;
        FOR item IN SELECT jsonb_array_elements(v) LOOP
            IF NOT eacp.studio_output_matches(s->'items',item,depth+1) THEN RETURN false; END IF;
        END LOOP;
    ELSIF typ='string' THEN
        IF length(v#>>'{}')>(s->>'maxLength')::integer OR length(v#>>'{}')<COALESCE((s->>'minLength')::integer,0) THEN RETURN false; END IF;
    ELSIF typ IN ('number','integer') THEN
        n:=(v::text)::numeric;
        IF (typ='integer' AND n<>trunc(n)) OR (s ? 'minimum' AND n<(s->>'minimum')::numeric)
        OR (s ? 'maximum' AND n>(s->>'maximum')::numeric) THEN RETURN false; END IF;
    END IF;
    RETURN true;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.llm_admit(p jsonb) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,pg_temp AS $$
DECLARE tenant uuid:=eacp.current_tenant_id(); bound_version uuid:=eacp.current_agent_version_id();
    node eacp.studio_run_nodes%ROWTYPE; r eacp.studio_runs%ROWTYPE; s jsonb; out jsonb; run uuid;
BEGIN
    IF NOT EXISTS(SELECT 1 FROM eacp.studio_versions WHERE tenant_id=tenant AND id=bound_version) THEN
        IF p->>'studio_intent_id' IS NOT NULL THEN RAISE EXCEPTION 'a Studio intent belongs to a Studio bound_version' USING ERRCODE='42501'; END IF;
        RETURN eacp.llm_admit_unbound(p-ARRAY['_studio_run','_studio_node','_studio_deadline']);
    END IF;
    SELECT run_id INTO run FROM eacp.studio_run_nodes WHERE tenant_id=tenant AND id=(p->>'studio_intent_id')::uuid;
    IF NOT FOUND THEN RAISE EXCEPTION 'a Studio key requires its current intent' USING ERRCODE='42501'; END IF;
    SELECT * INTO r FROM eacp.studio_runs WHERE tenant_id=tenant AND id=run FOR UPDATE;
    SELECT * INTO node FROM eacp.studio_run_nodes WHERE tenant_id=tenant AND id=(p->>'studio_intent_id')::uuid FOR UPDATE;
    IF r.version_id IS DISTINCT FROM bound_version OR r.state<>'RUNNING' OR r.current_index<>node.step_index
    OR r.leased_until<=now() OR r.deadline<=now() OR node.kind<>'llm' OR node.state<>'pending'
    OR r.runtime_id IS DISTINCT FROM p->>'studio_runtime_id' OR r.lease_generation IS DISTINCT FROM (p->>'studio_generation')::bigint
    OR node.runtime_id IS DISTINCT FROM r.runtime_id OR node.lease_generation IS DISTINCT FROM r.lease_generation THEN
        RAISE EXCEPTION 'intent requires its live run, node and lease' USING ERRCODE='42501'; END IF;
    IF node.call_id IS NOT NULL THEN RAISE EXCEPTION 'intent is already consumed' USING ERRCODE='55000'; END IF;
    SELECT definition::jsonb->'steps'->node.step_index INTO s FROM eacp.studio_versions WHERE tenant_id=tenant AND id=bound_version;
    IF p->>'model_name' IS DISTINCT FROM s->>'model' OR (p->>'max_output_tokens')::bigint IS DISTINCT FROM (s->>'max_output_tokens')::bigint
    OR p->'stream' IS DISTINCT FROM 'false'::jsonb
    OR NOT EXISTS(SELECT 1 FROM eacp.llm_models WHERE tenant_id=tenant AND name=s->>'model' AND provider=p->>'provider')
    OR NOT EXISTS(SELECT 1 FROM eacp.principals WHERE tenant_id=tenant AND id=r.requested_by AND subject=p->>'subject' AND disabled_at IS NULL) THEN
        RAISE EXCEPTION 'intent fixes the requester, model, provider and non-streaming cap' USING ERRCODE='42501'; END IF;
    -- Registry precedes the kill advisory lock; the private admission then reserves the budget.
    PERFORM 1 FROM eacp.agent_versions WHERE tenant_id=tenant AND id=bound_version FOR SHARE;
    out:=eacp.llm_admit_unbound(p||jsonb_build_object('_studio_run',r.id,'_studio_node',node.id,'_studio_deadline',r.deadline));
    RETURN out||jsonb_build_object('studio',jsonb_build_object('run_id',r.id,'index',node.step_index,'output_schema',s->'output_schema'));
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.studio_llm_settle(p_call uuid,p_outcome text,p_status integer,p_input bigint,
 p_cache_read bigint,p_cache_write bigint,p_output bigint,p_known boolean,p_value jsonb,p_failure text) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,pg_temp AS $$
DECLARE tenant uuid:=eacp.current_tenant_id(); c eacp.llm_calls%ROWTYPE; r eacp.studio_runs%ROWTYPE;
    node eacp.studio_run_nodes%ROWTYPE; run uuid; schema jsonb; eligible boolean; killed boolean; failure text;
BEGIN
    IF eacp.current_system_actor() IS DISTINCT FROM 'llm_gateway' OR eacp.current_actor_id() IS NOT NULL OR eacp.current_agent_version_id() IS NOT NULL THEN
        RAISE EXCEPTION 'only the gateway settles calls' USING ERRCODE='42501'; END IF;
    IF p_outcome='abandoned' THEN RAISE EXCEPTION 'only the sweeper abandons a call' USING ERRCODE='42501'; END IF;
    IF p_outcome IS NULL OR p_outcome NOT IN ('succeeded','provider_error','usage_unknown','killed') THEN
        RAISE EXCEPTION 'unknown gateway outcome' USING ERRCODE='22023'; END IF;
    SELECT studio_run_id INTO run FROM eacp.llm_calls WHERE tenant_id=tenant AND id=p_call;
    IF run IS NULL THEN RETURN eacp.llm_settle_unbound(p_call,p_outcome,p_status,p_input,p_cache_read,p_cache_write,p_output,p_known); END IF;
    SELECT * INTO r FROM eacp.studio_runs WHERE tenant_id=tenant AND id=run FOR UPDATE;
    SELECT * INTO node FROM eacp.studio_run_nodes WHERE tenant_id=tenant AND run_id=run AND call_id=p_call FOR UPDATE;
    SELECT * INTO c FROM eacp.llm_calls WHERE tenant_id=tenant AND id=p_call FOR UPDATE;
    IF c.state<>'ADMITTED' THEN RAISE EXCEPTION 'call is already settled' USING ERRCODE='55000'; END IF;
    PERFORM 1 FROM eacp.agent_versions WHERE tenant_id=tenant AND id=r.version_id AND state='ACTIVE' FOR SHARE;
    eligible:=FOUND AND r.state='RUNNING' AND r.deadline>now() AND r.current_index=node.step_index;
    PERFORM pg_advisory_xact_lock(hashtextextended('eacp.kill:'||tenant::text,0));
    killed:=eacp.llm_call_killed(c.id);
    eligible:=eligible AND NOT killed;
    -- Acquire the reservation before any audited run/node change. The
    -- ledger's finish later reuses this lock and appends its audit last.
    PERFORM 1 FROM eacp.budget_reservations WHERE tenant_id=tenant AND llm_call_id=c.id AND state='ACTIVE' FOR UPDATE;
    IF killed AND r.state IN ('QUEUED','RUNNING') THEN
        UPDATE eacp.studio_runs SET state='FAILED',failure_reason='killed',inputs=NULL,leased_until=NULL,finished_at=now()
        WHERE tenant_id=tenant AND id=r.id;
    END IF;
    IF p_failure IS NOT NULL AND p_failure NOT IN ('llm_invalid_output','llm_output_contains_credential','llm_output_unavailable') THEN
        RAISE EXCEPTION 'unknown typed output failure' USING ERRCODE='23514'; END IF;
    failure:=p_failure;
    IF eligible AND p_outcome='succeeded' AND p_value IS NOT NULL AND failure IS NULL THEN
        SELECT definition::jsonb->'steps'->node.step_index->'output_schema' INTO schema FROM eacp.studio_versions WHERE tenant_id=tenant AND id=r.version_id;
        IF octet_length(p_value::text)>65536 OR NOT eacp.studio_output_matches(schema,p_value) THEN
            RAISE EXCEPTION 'typed output does not match the immutable schema' USING ERRCODE='23514'; END IF;
        INSERT INTO eacp.studio_node_results(tenant_id,id,call_id,output,digest,byte_count,expires_at)
        VALUES(tenant,node.id,c.id,p_value,encode(sha256(convert_to(p_value::text,'UTF8')),'hex'),octet_length(p_value::text),r.deadline);
    ELSIF eligible AND p_outcome='succeeded' AND failure IS NULL THEN failure:='llm_output_unavailable';
    END IF;
    IF failure IS NOT NULL THEN UPDATE eacp.studio_run_nodes SET failure_reason=failure WHERE tenant_id=tenant AND id=node.id; END IF;
    -- The ledger's settlement and final audit are after all private writes.
    RETURN eacp.llm_settle_unbound(p_call,p_outcome,p_status,p_input,p_cache_read,p_cache_write,p_output,p_known);
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.llm_settle(p_call uuid,p_outcome text,p_status integer,p_input bigint,
 p_cache_read bigint,p_cache_write bigint,p_output bigint,p_known boolean) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,pg_temp AS $$
BEGIN RETURN eacp.studio_llm_settle(p_call,p_outcome,p_status,p_input,p_cache_read,p_cache_write,p_output,p_known,NULL,NULL); END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.llm_call_killed(p_call uuid) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT eacp.llm_killed(c.tenant_id,a.owner_group_id,c.agent_id,c.agent_version_id,c.model_id)
    OR (c.studio_run_id IS NOT NULL AND (eacp.studio_run_killed(c.tenant_id,c.agent_id,c.agent_version_id,c.studio_run_id)
        OR EXISTS(SELECT 1 FROM eacp.studio_runs r WHERE r.tenant_id=c.tenant_id AND r.id=c.studio_run_id AND r.failure_reason='killed')))
 FROM eacp.llm_calls c JOIN eacp.agents a ON a.tenant_id=c.tenant_id AND a.id=c.agent_id
 WHERE c.tenant_id=eacp.current_tenant_id() AND c.id=p_call
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.studio_run_results_prune() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.state IN ('SUCCEEDED','FAILED') OR NEW.deadline<=now() THEN
    UPDATE eacp.studio_node_results SET output=NULL,pruned_at=now() WHERE tenant_id=NEW.tenant_id AND output IS NOT NULL
    AND id IN (SELECT id FROM eacp.studio_run_nodes WHERE tenant_id=NEW.tenant_id AND run_id=NEW.id);
 END IF;
 RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER studio_run_results_prune AFTER UPDATE ON eacp.studio_runs FOR EACH ROW EXECUTE FUNCTION eacp.studio_run_results_prune();

REVOKE ALL ON FUNCTION eacp.studio_llm_settle(uuid,text,integer,bigint,bigint,bigint,bigint,boolean,jsonb,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.studio_llm_settle(uuid,text,integer,bigint,bigint,bigint,bigint,boolean,jsonb,text) TO eacp_app;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.llm_calls_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    comp text := eacp.current_system_actor();
BEGIN
    IF NOT eacp.llm_ledger_context(NEW.id) THEN
        RAISE EXCEPTION 'the ledger changes only through eacp.llm_admit, llm_settle and llm_sweep'
            USING ERRCODE = '42501';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF eacp.current_agent_version_id() IS DISTINCT FROM NEW.agent_version_id
           OR eacp.current_actor_id() IS NOT NULL OR comp IS NOT NULL THEN
            RAISE EXCEPTION 'an LLM call is recorded for the agent that made it' USING ERRCODE = '42501';
        END IF;
        IF NEW.state NOT IN ('DENIED', 'ADMITTED') OR num_nonnulls(NEW.outcome, NEW.provider_status, NEW.cost_amount,
                                                                   NEW.committed_amount, NEW.settled_at,
                                                                   NEW.input_tokens) > 0 THEN
            RAISE EXCEPTION 'an LLM call starts denied or admitted' USING ERRCODE = '23514';
        END IF;
        NEW.agent_id := (SELECT agent_id FROM eacp.agent_versions
                         WHERE tenant_id = NEW.tenant_id AND id = NEW.agent_version_id);
        NEW.created_at := now();
        RETURN NEW;
    END IF;
    IF NEW.studio_run_id IS DISTINCT FROM OLD.studio_run_id OR NEW.studio_node_id IS DISTINCT FROM OLD.studio_node_id THEN
        RAISE EXCEPTION 'call binding is immutable' USING ERRCODE='55000'; END IF;
    IF OLD.state <> 'ADMITTED' OR NEW.state <> 'SETTLED' THEN
        RAISE EXCEPTION 'call % is %', OLD.id, OLD.state USING ERRCODE = '55000';
    END IF;
    IF eacp.current_actor_id() IS NOT NULL OR eacp.current_agent_version_id() IS NOT NULL
       OR comp IS DISTINCT FROM (CASE WHEN NEW.outcome = 'abandoned' THEN 'llm_sweeper' ELSE 'llm_gateway' END)
       OR (NEW.outcome = 'abandoned' AND OLD.deadline >= now()) THEN
        RAISE EXCEPTION 'a call is settled by the gateway, or abandoned by the sweeper once overdue'
            USING ERRCODE = '42501';
    END IF;
    NEW.settled_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.studio_node_complete(p_run uuid,p_runtime text,p_generation bigint,p_index integer,p_result jsonb)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,pg_temp AS $$
DECLARE r eacp.studio_runs%ROWTYPE; s jsonb; def jsonb; node eacp.studio_run_nodes%ROWTYPE;
    next_id text; dest integer; act eacp.actions%ROWTYPE;
BEGIN
    r:=eacp.studio_node_held(p_run,p_runtime,p_generation,p_index);
    SELECT definition::jsonb INTO def FROM eacp.studio_versions WHERE tenant_id=r.tenant_id AND id=r.version_id;
    s:=def->'steps'->p_index;
    SELECT * INTO node FROM eacp.studio_run_nodes WHERE tenant_id=r.tenant_id AND run_id=r.id AND step_index=p_index FOR UPDATE;
    IF NOT FOUND OR node.state<>'pending' OR jsonb_typeof(p_result) IS DISTINCT FROM 'object' THEN
        RAISE EXCEPTION 'node was not begun or is complete' USING ERRCODE='55000'; END IF;
    IF node.kind='branch' THEN
        IF jsonb_typeof(p_result->'choice') IS DISTINCT FROM 'boolean' OR (SELECT count(*) FROM jsonb_object_keys(p_result))<>1 THEN
            RAISE EXCEPTION 'branch completion is one fixed choice' USING ERRCODE='23514'; END IF;
        next_id:=CASE WHEN (p_result->>'choice')::boolean THEN s->>'then' ELSE s->>'else' END;
    ELSIF node.kind='tool_call' THEN
        IF r.mode='preview' THEN
            IF NOT COALESCE(r.preview_samples,'{}') ? node.step_id OR p_result<>'{}'::jsonb THEN
                RAISE EXCEPTION 'preview needs its tool sample' USING ERRCODE='23514'; END IF;
        ELSE
            SELECT * INTO act FROM eacp.actions WHERE tenant_id=r.tenant_id AND id=(p_result->>'action_id')::uuid;
            IF NOT FOUND OR act.studio_run_id IS DISTINCT FROM r.id OR act.agent_version_id<>r.version_id
            OR act.idempotency_key<>format('studio:%s:%s',r.id,p_index) OR act.state<>'SUCCEEDED'
            OR act.tool IS DISTINCT FROM s->>'tool' OR (SELECT count(*) FROM jsonb_object_keys(p_result))<>1 THEN
                RAISE EXCEPTION 'node needs its own succeeded action' USING ERRCODE='55000'; END IF;
            UPDATE eacp.studio_run_nodes SET action_id=act.id WHERE tenant_id=r.tenant_id AND id=node.id;
        END IF;
        next_id:=s->>'next';
    ELSIF node.kind='llm' THEN
        IF p_result<>'{}'::jsonb OR node.failure_reason IS NOT NULL OR NOT EXISTS(
            SELECT 1 FROM eacp.studio_node_results res JOIN eacp.llm_calls c ON c.tenant_id=res.tenant_id AND c.id=res.call_id
            WHERE res.tenant_id=r.tenant_id AND res.id=node.id AND res.output IS NOT NULL AND res.expires_at>now()
                AND c.state='SETTLED' AND c.outcome='succeeded') THEN
            RAISE EXCEPTION 'LLM completion requires a recorded typed output' USING ERRCODE='55000'; END IF;
        next_id:=s->>'next';
    ELSE RAISE EXCEPTION 'respond finishes the run' USING ERRCODE='55000'; END IF;
    IF def->'schema_version'='1'::jsonb THEN dest:=p_index+1;
    ELSE SELECT ordinality::integer-1 INTO dest FROM jsonb_array_elements(def->'steps') WITH ORDINALITY e(value,ordinality) WHERE value->>'id'=next_id; END IF;
    IF dest IS NULL OR dest<=p_index THEN RAISE EXCEPTION 'invalid successor' USING ERRCODE='23514'; END IF;
    UPDATE eacp.studio_run_nodes SET state='completed',next_index=dest,completed_at=now() WHERE tenant_id=r.tenant_id AND id=node.id;
    UPDATE eacp.studio_runs SET current_index=dest WHERE tenant_id=r.tenant_id AND id=r.id;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.studio_node_output(p_run uuid,p_runtime text,p_generation bigint,p_index integer)
RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,pg_temp AS $$
DECLARE r eacp.studio_runs%ROWTYPE; s jsonb; node eacp.studio_run_nodes%ROWTYPE; c eacp.llm_calls%ROWTYPE; value jsonb;
BEGIN
    r:=eacp.studio_run_held(p_run,p_runtime,p_generation);
    r:=eacp.studio_node_held(p_run,p_runtime,p_generation,r.current_index);
    IF p_index IS NULL OR p_index>r.current_index OR p_index<0 THEN
        RAISE EXCEPTION 'output is not from a visited node' USING ERRCODE='55000'; END IF;
    IF p_index<>r.current_index AND NOT EXISTS(SELECT 1 FROM eacp.studio_run_nodes
        WHERE tenant_id=r.tenant_id AND run_id=r.id AND step_index=p_index AND state='completed') THEN
        RAISE EXCEPTION 'output is not from a visited node' USING ERRCODE='55000'; END IF;
    SELECT definition::jsonb->'steps'->p_index INTO s FROM eacp.studio_versions WHERE tenant_id=r.tenant_id AND id=r.version_id;
    IF r.mode='preview' AND s->>'kind'='tool_call' AND r.preview_samples ? (s->>'id') THEN
        RETURN jsonb_build_object('state','succeeded','output',r.preview_samples->(s->>'id'));
    END IF;
    SELECT * INTO node FROM eacp.studio_run_nodes WHERE tenant_id=r.tenant_id AND run_id=r.id AND step_index=p_index;
    IF node.call_id IS NOT NULL THEN
        SELECT * INTO c FROM eacp.llm_calls WHERE tenant_id=r.tenant_id AND id=node.call_id;
        IF c.state='DENIED' THEN RETURN jsonb_build_object('state','failed','failure','llm_denied','call_id',c.id); END IF;
        IF c.state='SETTLED' THEN
            SELECT output INTO value FROM eacp.studio_node_results WHERE tenant_id=r.tenant_id AND id=node.id AND expires_at>now();
            IF c.outcome='succeeded' AND value IS NOT NULL AND node.failure_reason IS NULL THEN
                RETURN jsonb_build_object('state','succeeded','output',value,'call_id',c.id); END IF;
            RETURN jsonb_build_object('state','failed','failure',COALESCE(node.failure_reason,
                CASE WHEN c.outcome='provider_error' THEN 'llm_failed' WHEN c.outcome='killed' THEN 'killed'
                    WHEN c.outcome='succeeded' THEN 'llm_output_unavailable' ELSE 'llm_unknown' END),'call_id',c.id);
        END IF;
    END IF;
    RETURN jsonb_build_object('state','pending','call_id',node.call_id);
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.studio_runs_expire(p_batch integer) RETURNS integer
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    tenant uuid := eacp.current_tenant_id();
    n integer;
    extra integer;
    m integer;
BEGIN
    IF eacp.current_system_actor() IS DISTINCT FROM 'sweeper' OR eacp.current_actor_id() IS NOT NULL
       OR eacp.current_agent_version_id() IS NOT NULL THEN
        RAISE EXCEPTION 'only the sweeper expires runs' USING ERRCODE = '42501';
    END IF;
    WITH due AS (
        SELECT id FROM eacp.studio_runs
        WHERE tenant_id = tenant AND deadline <= now()
          AND (state = 'QUEUED' OR (state = 'RUNNING' AND leased_until <= now()))
        ORDER BY deadline LIMIT greatest(p_batch, 1)
        FOR UPDATE SKIP LOCKED)
    UPDATE eacp.studio_runs s SET state = 'FAILED', failure_reason = 'deadline_exceeded', inputs = NULL,
           leased_until = NULL, finished_at = now()
    FROM due WHERE s.tenant_id = tenant AND s.id = due.id;
    GET DIAGNOSTICS n = ROW_COUNT;
    WITH due AS (
        SELECT id FROM eacp.studio_runs
        WHERE tenant_id = tenant AND answer IS NOT NULL AND answer_expires_at <= now()
        ORDER BY answer_expires_at LIMIT greatest(p_batch, 1)
        FOR UPDATE SKIP LOCKED)
    UPDATE eacp.studio_runs s SET answer = NULL, answer_pruned_at = now()
    FROM due WHERE s.tenant_id = tenant AND s.id = due.id;
    GET DIAGNOSTICS m = ROW_COUNT;
    n:=n+m;
    WITH due AS(SELECT id FROM eacp.studio_runs WHERE tenant_id=tenant AND deadline<=now()
        AND preview_samples IS NOT NULL LIMIT greatest(p_batch,1) FOR UPDATE SKIP LOCKED)
    UPDATE eacp.studio_runs r SET preview_samples=NULL FROM due WHERE r.tenant_id=tenant AND r.id=due.id;
    GET DIAGNOSTICS extra=ROW_COUNT;
    UPDATE eacp.studio_node_results SET output=NULL,pruned_at=now() WHERE tenant_id=tenant AND output IS NOT NULL AND expires_at<=now();
    GET DIAGNOSTICS m=ROW_COUNT;
    RETURN n + extra + m;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.studio_run_tenants() RETURNS SETOF uuid
    LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT DISTINCT tenant_id FROM eacp.studio_runs
    WHERE (deadline <= now() AND (state = 'QUEUED' OR (state = 'RUNNING' AND leased_until <= now())))
       OR (deadline<=now() AND (preview_samples IS NOT NULL OR EXISTS(SELECT 1 FROM eacp.studio_node_results res JOIN eacp.studio_run_nodes nd ON nd.tenant_id=res.tenant_id AND nd.id=res.id WHERE nd.tenant_id=eacp.studio_runs.tenant_id AND nd.run_id=eacp.studio_runs.id AND res.output IS NOT NULL)))
       OR (answer IS NOT NULL AND answer_expires_at <= now())
$$;
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.studio_run_tenants() RETURNS SETOF uuid
    LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT DISTINCT tenant_id FROM eacp.studio_runs
    WHERE (deadline <= now() AND (state = 'QUEUED' OR (state = 'RUNNING' AND leased_until <= now())))
       OR (answer IS NOT NULL AND answer_expires_at <= now())
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.studio_runs_expire(p_batch integer) RETURNS integer
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    tenant uuid := eacp.current_tenant_id();
    n integer;
    m integer;
BEGIN
    IF eacp.current_system_actor() IS DISTINCT FROM 'sweeper' OR eacp.current_actor_id() IS NOT NULL
       OR eacp.current_agent_version_id() IS NOT NULL THEN
        RAISE EXCEPTION 'only the sweeper expires runs' USING ERRCODE = '42501';
    END IF;
    WITH due AS (
        SELECT id FROM eacp.studio_runs
        WHERE tenant_id = tenant AND deadline <= now()
          AND (state = 'QUEUED' OR (state = 'RUNNING' AND leased_until <= now()))
        ORDER BY deadline LIMIT greatest(p_batch, 1)
        FOR UPDATE SKIP LOCKED)
    UPDATE eacp.studio_runs s SET state = 'FAILED', failure_reason = 'deadline_exceeded', inputs = NULL,
           leased_until = NULL, finished_at = now()
    FROM due WHERE s.tenant_id = tenant AND s.id = due.id;
    GET DIAGNOSTICS n = ROW_COUNT;
    WITH due AS (
        SELECT id FROM eacp.studio_runs
        WHERE tenant_id = tenant AND answer IS NOT NULL AND answer_expires_at <= now()
        ORDER BY answer_expires_at LIMIT greatest(p_batch, 1)
        FOR UPDATE SKIP LOCKED)
    UPDATE eacp.studio_runs s SET answer = NULL, answer_pruned_at = now()
    FROM due WHERE s.tenant_id = tenant AND s.id = due.id;
    GET DIAGNOSTICS m = ROW_COUNT;
    RETURN n + m;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.studio_node_output(p_run uuid,p_runtime text,p_generation bigint,p_index integer)
RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,pg_temp AS $$
DECLARE r eacp.studio_runs%ROWTYPE; s jsonb;
BEGIN
    r:=eacp.studio_run_held(p_run,p_runtime,p_generation);
    r:=eacp.studio_node_held(p_run,p_runtime,p_generation,r.current_index);
    IF p_index IS NULL OR p_index>r.current_index OR p_index<0 THEN
        RAISE EXCEPTION 'output is not from a visited node' USING ERRCODE='55000'; END IF;
    IF p_index<>r.current_index AND NOT EXISTS(SELECT 1 FROM eacp.studio_run_nodes
        WHERE tenant_id=r.tenant_id AND run_id=r.id AND step_index=p_index AND state='completed') THEN
        RAISE EXCEPTION 'output is not from a visited node' USING ERRCODE='55000'; END IF;
    SELECT definition::jsonb->'steps'->p_index INTO s FROM eacp.studio_versions WHERE tenant_id=r.tenant_id AND id=r.version_id;
    IF r.mode='preview' AND s->>'kind'='tool_call' AND r.preview_samples ? (s->>'id') THEN
        RETURN jsonb_build_object('state','succeeded','output',r.preview_samples->(s->>'id'));
    END IF;
    RETURN jsonb_build_object('state','pending');
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.studio_node_complete(p_run uuid,p_runtime text,p_generation bigint,p_index integer,p_result jsonb)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,pg_temp AS $$
DECLARE r eacp.studio_runs%ROWTYPE; s jsonb; def jsonb; node eacp.studio_run_nodes%ROWTYPE;
    next_id text; dest integer; act eacp.actions%ROWTYPE;
BEGIN
    r:=eacp.studio_node_held(p_run,p_runtime,p_generation,p_index);
    SELECT definition::jsonb INTO def FROM eacp.studio_versions WHERE tenant_id=r.tenant_id AND id=r.version_id;
    s:=def->'steps'->p_index;
    SELECT * INTO node FROM eacp.studio_run_nodes WHERE tenant_id=r.tenant_id AND run_id=r.id AND step_index=p_index FOR UPDATE;
    IF NOT FOUND OR node.state<>'pending' OR jsonb_typeof(p_result) IS DISTINCT FROM 'object' THEN
        RAISE EXCEPTION 'node was not begun or is complete' USING ERRCODE='55000'; END IF;
    IF node.kind='branch' THEN
        IF jsonb_typeof(p_result->'choice') IS DISTINCT FROM 'boolean' OR (SELECT count(*) FROM jsonb_object_keys(p_result))<>1 THEN
            RAISE EXCEPTION 'branch completion is one fixed choice' USING ERRCODE='23514'; END IF;
        next_id:=CASE WHEN (p_result->>'choice')::boolean THEN s->>'then' ELSE s->>'else' END;
    ELSIF node.kind='tool_call' THEN
        IF r.mode='preview' THEN
            IF NOT COALESCE(r.preview_samples,'{}') ? node.step_id OR p_result<>'{}'::jsonb THEN
                RAISE EXCEPTION 'preview needs its tool sample' USING ERRCODE='23514'; END IF;
        ELSE
            SELECT * INTO act FROM eacp.actions WHERE tenant_id=r.tenant_id AND id=(p_result->>'action_id')::uuid;
            IF NOT FOUND OR act.studio_run_id IS DISTINCT FROM r.id OR act.agent_version_id<>r.version_id
            OR act.idempotency_key<>format('studio:%s:%s',r.id,p_index) OR act.state<>'SUCCEEDED'
            OR act.tool IS DISTINCT FROM s->>'tool' OR (SELECT count(*) FROM jsonb_object_keys(p_result))<>1 THEN
                RAISE EXCEPTION 'node needs its own succeeded action' USING ERRCODE='55000'; END IF;
            UPDATE eacp.studio_run_nodes SET action_id=act.id WHERE tenant_id=r.tenant_id AND id=node.id;
        END IF;
        next_id:=s->>'next';
    ELSIF node.kind='llm' THEN
        -- Added by the Studio gateway migration; no caller can advance it yet.
        RAISE EXCEPTION 'LLM completion requires a recorded typed output' USING ERRCODE='55000';
    ELSE RAISE EXCEPTION 'respond finishes the run' USING ERRCODE='55000'; END IF;
    IF def->'schema_version'='1'::jsonb THEN dest:=p_index+1;
    ELSE SELECT ordinality::integer-1 INTO dest FROM jsonb_array_elements(def->'steps') WITH ORDINALITY e(value,ordinality) WHERE value->>'id'=next_id; END IF;
    IF dest IS NULL OR dest<=p_index THEN RAISE EXCEPTION 'invalid successor' USING ERRCODE='23514'; END IF;
    UPDATE eacp.studio_run_nodes SET state='completed',next_index=dest,completed_at=now() WHERE tenant_id=r.tenant_id AND id=node.id;
    UPDATE eacp.studio_runs SET current_index=dest WHERE tenant_id=r.tenant_id AND id=r.id;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.llm_calls_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    comp text := eacp.current_system_actor();
BEGIN
    IF NOT eacp.llm_ledger_context(NEW.id) THEN
        RAISE EXCEPTION 'the ledger changes only through eacp.llm_admit, llm_settle and llm_sweep'
            USING ERRCODE = '42501';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF eacp.current_agent_version_id() IS DISTINCT FROM NEW.agent_version_id
           OR eacp.current_actor_id() IS NOT NULL OR comp IS NOT NULL THEN
            RAISE EXCEPTION 'an LLM call is recorded for the agent that made it' USING ERRCODE = '42501';
        END IF;
        IF NEW.state NOT IN ('DENIED', 'ADMITTED') OR num_nonnulls(NEW.outcome, NEW.provider_status, NEW.cost_amount,
                                                                   NEW.committed_amount, NEW.settled_at,
                                                                   NEW.input_tokens) > 0 THEN
            RAISE EXCEPTION 'an LLM call starts denied or admitted' USING ERRCODE = '23514';
        END IF;
        NEW.agent_id := (SELECT agent_id FROM eacp.agent_versions
                         WHERE tenant_id = NEW.tenant_id AND id = NEW.agent_version_id);
        NEW.created_at := now();
        RETURN NEW;
    END IF;
    IF OLD.state <> 'ADMITTED' OR NEW.state <> 'SETTLED' THEN
        RAISE EXCEPTION 'call % is %', OLD.id, OLD.state USING ERRCODE = '55000';
    END IF;
    IF eacp.current_actor_id() IS NOT NULL OR eacp.current_agent_version_id() IS NOT NULL
       OR comp IS DISTINCT FROM (CASE WHEN NEW.outcome = 'abandoned' THEN 'llm_sweeper' ELSE 'llm_gateway' END)
       OR (NEW.outcome = 'abandoned' AND OLD.deadline >= now()) THEN
        RAISE EXCEPTION 'a call is settled by the gateway, or abandoned by the sweeper once overdue'
            USING ERRCODE = '42501';
    END IF;
    NEW.settled_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.llm_call_killed(p_call uuid) RETURNS boolean
    LANGUAGE sql STABLE
    AS $$
    SELECT eacp.llm_killed(c.tenant_id, a.owner_group_id, c.agent_id, c.agent_version_id, c.model_id)
    FROM eacp.llm_calls c
    JOIN eacp.agents a ON a.tenant_id = c.tenant_id AND a.id = c.agent_id
    WHERE c.tenant_id = eacp.current_tenant_id() AND c.id = p_call
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.llm_settle(p_call uuid, p_outcome text, p_status integer, p_input bigint,
                                p_cache_read bigint, p_cache_write bigint, p_output bigint, p_known boolean)
    RETURNS jsonb
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    c eacp.llm_calls%ROWTYPE;
BEGIN
    IF eacp.current_system_actor() IS DISTINCT FROM 'llm_gateway' OR eacp.current_actor_id() IS NOT NULL
       OR eacp.current_agent_version_id() IS NOT NULL THEN
        RAISE EXCEPTION 'a call is settled by the LLM gateway' USING ERRCODE = '42501';
    END IF;
    IF p_outcome = 'abandoned' THEN
        RAISE EXCEPTION 'only the sweeper abandons a call' USING ERRCODE = '42501';
    END IF;
    IF p_outcome IS NULL OR p_outcome NOT IN ('succeeded', 'provider_error', 'usage_unknown', 'killed') THEN
        RAISE EXCEPTION 'unknown outcome %', p_outcome USING ERRCODE = '22023';
    END IF;
    SELECT * INTO c FROM eacp.llm_calls WHERE tenant_id = eacp.current_tenant_id() AND id = p_call FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'unknown call %', p_call USING ERRCODE = '23503';
    END IF;
    IF c.state <> 'ADMITTED' THEN
        RAISE EXCEPTION 'call % is %', p_call, c.state USING ERRCODE = '55000';
    END IF;
    RETURN eacp.llm_finish(c, p_outcome, p_status, p_input, p_cache_read, p_cache_write, p_output, p_known);
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION eacp.llm_admit(p jsonb) RETURNS jsonb
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    tenant  uuid := eacp.current_tenant_id();
    who     jsonb := eacp.actor_context();
    ver     eacp.agent_versions%ROWTYPE;
    ag      eacp.agents%ROWTYPE;
    m       eacp.llm_models%ROWTYPE;
    pr      eacp.model_prices%ROWTYPE;
    acct    eacp.budget_accounts%ROWTYPE;
    dec     jsonb := p->'decision';
    subj    uuid;
    denial  text;
    req_out bigint;
    est     numeric;
    room    numeric;
    over    numeric;
    allowed boolean;
    call_id uuid := gen_random_uuid();
    due     timestamptz;
BEGIN
    IF who->>'kind' IS DISTINCT FROM 'agent' THEN
        RAISE EXCEPTION 'an LLM call is admitted for an authenticated agent' USING ERRCODE = '42501';
    END IF;
    IF jsonb_typeof(p->'model_name') IS DISTINCT FROM 'string' OR jsonb_typeof(dec) IS DISTINCT FROM 'object'
       OR p->>'provider' NOT IN ('anthropic', 'openai') OR jsonb_typeof(p->'stream') IS DISTINCT FROM 'boolean'
       OR jsonb_typeof(p->'request_bytes') IS DISTINCT FROM 'number' THEN
        RAISE EXCEPTION 'an admission names a model, provider, stream flag, request size and decision'
            USING ERRCODE = '22023';
    END IF;
    IF jsonb_typeof(p->'max_output_tokens') = 'number' THEN
        req_out := (p->>'max_output_tokens')::bigint;
    END IF;

    SELECT * INTO ver FROM eacp.agent_versions WHERE tenant_id = tenant AND id = (who->>'id')::uuid FOR SHARE;
    SELECT * INTO ag FROM eacp.agents WHERE tenant_id = tenant AND id = ver.agent_id;
    IF ver.state <> 'ACTIVE' OR ver.active_allowlist_id IS NULL THEN
        denial := 'agent_version_not_active';
    END IF;
    IF denial IS NULL AND COALESCE(p->>'subject', '') <> '' THEN
        SELECT id INTO subj FROM eacp.principals
        WHERE tenant_id = tenant AND subject = p->>'subject' AND kind = 'human' AND disabled_at IS NULL;
        IF NOT FOUND THEN
            denial := 'subject_invalid';
        END IF;
    END IF;
    SELECT * INTO m FROM eacp.llm_models WHERE tenant_id = tenant AND name = p->>'model_name';  -- immutable: no lock
    IF denial IS NULL AND m.id IS NULL THEN
        denial := 'unknown_model';
    ELSIF denial IS NULL AND m.provider <> p->>'provider' THEN
        denial := 'wrong_provider';
    END IF;
    IF denial IS NULL THEN
        SELECT m.id = ANY (model_ids) INTO allowed FROM eacp.agent_allowlists
        WHERE tenant_id = tenant AND id = ver.active_allowlist_id;
        IF NOT COALESCE(allowed, false) THEN
            denial := 'model_not_in_allowlist';
        END IF;
    END IF;
    IF denial IS NULL THEN
        denial := eacp.release_denial(ver.id, subj);
    END IF;
    IF denial IS NULL AND COALESCE(dec->>'denial', '') <> '' THEN
        denial := dec->>'denial';
    END IF;
    PERFORM pg_advisory_xact_lock(hashtextextended('eacp.kill:' || tenant::text, 0));
    IF denial IS NULL AND eacp.llm_killed(tenant, ag.owner_group_id, ag.id, ver.id, m.id) THEN
        denial := 'killed';
    END IF;
    IF denial IS NULL AND req_out IS NOT NULL AND req_out > m.max_output_tokens THEN
        denial := 'max_tokens_over_cap';
    END IF;
    IF denial IS NULL THEN
        req_out := COALESCE(req_out, m.max_output_tokens);
        SELECT * INTO pr FROM eacp.model_prices
        WHERE tenant_id = tenant AND provider = m.provider AND model = m.upstream_model AND effective_from <= now()
        ORDER BY effective_from DESC LIMIT 1;
        IF NOT FOUND THEN
            denial := 'model_unpriced';
        END IF;
    END IF;
    IF denial IS NULL THEN
        est := eacp.llm_estimate(pr, (p->>'request_bytes')::bigint, req_out);
        SELECT * INTO acct FROM eacp.budget_accounts
        WHERE tenant_id = tenant AND agent_id = ag.id AND unit = pr.unit FOR NO KEY UPDATE;
        IF FOUND THEN
            PERFORM eacp.budget_fold(acct.id);
            SELECT hard_limit - allocated - reserved - committed INTO room FROM eacp.budget_accounts
            WHERE tenant_id = tenant AND id = acct.id;
            -- A settled call's cost above its reservation was not committed
            -- (a reservation never commits more than it holds); it is real
            -- spend, so it counts against every later admission.
            SELECT COALESCE(sum(cost_amount - committed_amount), 0) INTO over FROM eacp.llm_calls
            WHERE tenant_id = tenant AND agent_id = ag.id AND cost_unit = pr.unit
              AND cost_amount > committed_amount;
            IF est + over > room THEN
                denial := 'budget_exceeded';
            END IF;
        END IF;
    END IF;

    PERFORM set_config('eacp.llm_call', call_id::text, true);
    IF denial IS NULL THEN
        due := now() + make_interval(secs => m.timeout_ms / 1000.0) + interval '60 seconds';
    END IF;
    INSERT INTO eacp.llm_calls (tenant_id, id, agent_version_id, model_id, model_name, provider,
        subject_principal_id, stream, trace_id, request_bytes, max_output_tokens, decision_id, policy_bundle_id,
        policy_version, verdict, input_digest, price_id, cost_unit, state, denial, gateway_id, deadline)
    VALUES (tenant, call_id, ver.id, m.id, p->>'model_name', p->>'provider', subj, (p->>'stream')::boolean,
        NULLIF(p->>'trace_id', ''), (p->>'request_bytes')::bigint,
        CASE WHEN denial IS NULL THEN req_out
             WHEN req_out BETWEEN 1 AND 1000000000 THEN req_out END,
        (dec->>'id')::uuid, (dec->>'bundle_id')::uuid, (dec->>'version')::integer, dec->>'verdict',
        NULLIF(dec->>'input_digest', ''),
        CASE WHEN denial IS NULL THEN pr.id END, CASE WHEN denial IS NULL THEN pr.unit END,
        CASE WHEN denial IS NULL THEN 'ADMITTED' ELSE 'DENIED' END, denial, p->>'gateway_id', due);
    IF denial IS NULL AND acct.id IS NOT NULL THEN
        INSERT INTO eacp.budget_reservations (tenant_id, account_id, unit, llm_call_id, amount, expires_at)
        VALUES (tenant, acct.id, pr.unit, call_id, est, due);
    END IF;
    INSERT INTO eacp.audit_events (tenant_id, payload)
    VALUES (tenant, convert_to(jsonb_build_object(
        'v', 1, 'actor', who,
        'action', CASE WHEN denial IS NULL THEN 'llm.admitted' ELSE 'llm.denied' END,
        'subject', jsonb_build_object('type', 'llm_call', 'id', call_id),
        'reason', COALESCE(denial, 'admitted'),
        'data', jsonb_strip_nulls(jsonb_build_object(
            'model', p->>'model_name', 'model_id', m.id, 'provider', p->>'provider', 'stream', p->'stream',
            'request_bytes', p->'request_bytes', 'max_output_tokens', req_out, 'decision_id', dec->>'id',
            'verdict', dec->>'verdict', 'reservation', est, 'unit', pr.unit, 'deadline', due,
            'gateway_id', p->>'gateway_id')))::text, 'UTF8'));
    IF denial IS NOT NULL THEN
        RETURN jsonb_build_object('call_id', call_id, 'denial', denial);
    END IF;
    RETURN jsonb_build_object('call_id', call_id, 'denial', '', 'model_id', m.id, 'upstream_model', m.upstream_model,
        'base_url', m.base_url, 'secret_ref', m.secret_ref, 'timeout_ms', m.timeout_ms,
        'max_output_tokens', req_out, 'deadline', due);
END
$$;
-- +goose StatementEnd

DROP FUNCTION eacp.studio_llm_settle(uuid,text,integer,bigint,bigint,bigint,bigint,boolean,jsonb,text);
DROP FUNCTION eacp.llm_settle_unbound(uuid,text,integer,bigint,bigint,bigint,bigint,boolean);
DROP FUNCTION eacp.llm_admit_unbound(jsonb);
DROP TRIGGER studio_run_results_prune ON eacp.studio_runs;
DROP FUNCTION eacp.studio_run_results_prune();
DROP FUNCTION eacp.studio_output_matches(jsonb,jsonb,integer);
DROP TABLE eacp.studio_node_results;
ALTER TABLE eacp.llm_calls DROP COLUMN studio_run_id,DROP COLUMN studio_node_id;
