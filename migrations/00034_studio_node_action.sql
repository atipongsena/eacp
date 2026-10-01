-- Phase 27c: record a pending tool action before waiting, without moving
-- the graph cursor. Recovery waits on this id and never resubmits it.
-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION eacp.studio_node_action(p_run uuid,p_runtime text,p_generation bigint,p_index integer,p_action uuid)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,pg_temp AS $$
DECLARE r eacp.studio_runs%ROWTYPE; node eacp.studio_run_nodes%ROWTYPE; s jsonb;
BEGIN
    r:=eacp.studio_node_held(p_run,p_runtime,p_generation,p_index);
    SELECT * INTO node FROM eacp.studio_run_nodes WHERE tenant_id=r.tenant_id AND run_id=r.id AND step_index=p_index FOR UPDATE;
    IF NOT FOUND OR node.kind<>'tool_call' OR node.state<>'pending' OR r.mode<>'run' THEN
        RAISE EXCEPTION 'only a pending ordinary tool node records an action' USING ERRCODE='55000'; END IF;
    SELECT definition::jsonb->'steps'->p_index INTO s FROM eacp.studio_versions WHERE tenant_id=r.tenant_id AND id=r.version_id;
    -- Read the action binding, without acquiring an action lock after a run.
    IF NOT EXISTS(SELECT 1 FROM eacp.actions WHERE tenant_id=r.tenant_id AND id=p_action
        AND studio_run_id=r.id AND agent_version_id=r.version_id
        AND idempotency_key=format('studio:%s:%s',r.id,p_index) AND tool=s->>'tool') THEN
        RAISE EXCEPTION 'node records its own action only' USING ERRCODE='42501'; END IF;
    IF node.action_id IS NOT NULL THEN
        IF node.action_id<>p_action THEN RAISE EXCEPTION 'recorded action is immutable' USING ERRCODE='55000'; END IF;
        RETURN;
    END IF;
    UPDATE eacp.studio_run_nodes SET action_id=p_action WHERE tenant_id=r.tenant_id AND id=node.id;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION eacp.studio_node_action(uuid,text,bigint,integer,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION eacp.studio_node_action(uuid,text,bigint,integer,uuid) TO eacp_app;
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
    IF r.mode='preview' AND s->>'kind'='tool_call' THEN
        IF NOT COALESCE(r.preview_samples,'{}') ? (s->>'id') THEN
            RETURN jsonb_build_object('state','failed','failure','result_unavailable'); END IF;
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
-- +goose Down
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
DROP FUNCTION eacp.studio_node_action(uuid,text,bigint,integer,uuid);
