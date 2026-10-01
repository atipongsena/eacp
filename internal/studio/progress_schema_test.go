package studio_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/atipongsena/eacp/internal/registry/registrytest"
	"github.com/atipongsena/eacp/internal/storage"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const branchDefinition = `{"schema_version":2,"kind":"agent","inputs":{"employee_id":{"type":"string","max_length":64}},"limits":{"timeout_seconds":120},"steps":[{"id":"choose","kind":"branch","condition":{"left":"{{inputs.employee_id}}","operator":"eq","right":"E-1"},"then":"lookup","else":"decline"},{"id":"lookup","kind":"tool_call","tool":"hr-mcp.get_leave_balance","tool_schema_version":"1","operation":"lookup","target":"hr","resource":"leave_balance","payload":{"employee_id":"{{inputs.employee_id}}"},"next":"answer"},{"id":"decline","kind":"respond","text":"Not eligible"},{"id":"answer","kind":"respond","text":"{{steps.lookup.output.days}}"}]}`
const llmDefinition = `{"schema_version":2,"kind":"agent","inputs":{},"limits":{"timeout_seconds":120,"max_output_tokens":100},"steps":[{"id":"classify","kind":"llm","model":"triage","instruction":"Return JSON only.","input":{},"max_output_tokens":100,"output_schema":{"type":"object","properties":{"eligible":{"type":"boolean"}},"required":["eligible"],"additionalProperties":false},"next":"answer"},{"id":"answer","kind":"respond","text":"{{steps.classify.output.eligible}}"}]}`

func approvedDefinition(t *testing.T, f *fix, name, def string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	v := f.mustSave("stella", nil, name, def)
	ok(t, f.decide("rita", v, true, "reviewed"))
	return v, f.agentOf(v)
}

func TestStudioIntentConcurrentBeginReturnsOneIntent(t *testing.T) {
	f, _ := builderFix(t)
	_, ag := approvedDefinition(t, f, "concurrent-intent", llmDefinition)
	run := f.ID(t, "stella", startSQL, ag, `{}`)
	c := f.mustClaim("r1")
	var wg sync.WaitGroup
	ids := make([]any, 2)
	errs := make([]error, 2)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			node, err := f.object("rt", `SELECT eacp.studio_llm_begin($1,$2,$3,0)`, run, "r1", c.Generation)
			errs[i], ids[i] = err, node["intent_id"]
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		ok(t, err)
	}
	if ids[0] == nil || ids[0] != ids[1] {
		t.Fatal("concurrent begin allocated two intents")
	}
}

func TestStudioPreviewOutputRefusesExpiredRun(t *testing.T) {
	f := newFix(t)
	v, _ := approvedDefinition(t, f, "expired-preview", example)
	run := f.ID(t, "stella", `SELECT eacp.studio_preview_start($1,$2::jsonb,$3::jsonb)`, v, inputs, `{"lookup":{"days":12}}`)
	c := f.mustClaim("r1")
	f.owner(func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `UPDATE eacp.studio_runs SET deadline=now()-interval '1 second' WHERE id=$1`, run)
		return err
	})
	_, err := f.object("rt", `SELECT eacp.studio_node_output($1,$2,$3,0)`, run, "r1", c.Generation)
	wantState(t, err, sqlBadState)
}

func (f *fix) object(actor, sql string, args ...any) (map[string]any, error) {
	var raw []byte
	err := storage.InTenantTx(context.Background(), f.App, f.Tenant.String(), func(tx pgx.Tx) error {
		if err := storage.SetActor(context.Background(), tx, f.P[actor]); err != nil {
			return err
		}
		return tx.QueryRow(context.Background(), sql, args...).Scan(&raw)
	})
	var result map[string]any
	if err == nil {
		err = json.Unmarshal(raw, &result)
	}
	return result, err
}

func TestStudioCursorRefusesUnchosenOrPrematureSteps(t *testing.T) {
	f := newFix(t)
	v, ag := approvedDefinition(t, f, "branch-cursor", branchDefinition)
	run := f.ID(t, "stella", startSQL, ag, inputs)
	c := f.mustClaim("r1")
	_, err := f.object("rt", `SELECT eacp.studio_node_begin($1,$2,$3,$4)`, run, "r1", c.Generation, 1)
	wantState(t, err, sqlBadState)
	_, err = f.object("rt", `SELECT eacp.studio_node_begin($1,$2,$3,$4)`, run, "r1", c.Generation, 0)
	ok(t, err)
	ok(t, f.Exec("rt", `SELECT eacp.studio_node_complete($1,$2,$3,$4,$5::jsonb)`, run, "r1", c.Generation, 0, `{"choice":false}`))
	if got := f.scalar(`SELECT current_index::text FROM eacp.studio_runs WHERE id=$1`, run); got != "2" {
		t.Fatalf("cursor=%s", got)
	}
	_, err = f.object("rt", `SELECT eacp.studio_node_begin($1,$2,$3,$4)`, run, "r1", c.Generation, 1)
	wantState(t, err, sqlBadState)
	err = f.ExecAgent(v, registrytest.ReceivedActionSQL, v, stepKey(run, 1), stella, "hr-mcp.get_leave_balance")
	wantState(t, err, sqlForbidden)
	ok(t, f.Exec("rt", finishSQL, run, "r1", c.Generation, "SUCCEEDED", "Not eligible", nil))
}

func TestStudioIntentIsUniqueAndFenced(t *testing.T) {
	f, _ := builderFix(t)
	_, ag := approvedDefinition(t, f, "model-intent", llmDefinition)
	run := f.ID(t, "stella", startSQL, ag, `{}`)
	c := f.mustClaim("r1")
	query := `SELECT eacp.studio_llm_begin($1,$2,$3,0)`
	first, err := f.object("rt", query, run, "r1", c.Generation)
	ok(t, err)
	second, err := f.object("rt", query, run, "r1", c.Generation)
	ok(t, err)
	if first["intent_id"] == nil || first["intent_id"] != second["intent_id"] {
		t.Fatalf("intent ids differ")
	}
	_, err = f.object("rt", query, run, "r2", c.Generation)
	wantState(t, err, sqlForbidden)
	_, err = f.object("stella", query, run, "r1", c.Generation)
	wantState(t, err, sqlForbidden)
	f.owner(func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `UPDATE eacp.studio_runs SET leased_until=now()-interval '1 second' WHERE id=$1`, run)
		return err
	})
	c2 := f.mustClaim("r2")
	_, err = f.object("rt", query, run, "r1", c.Generation)
	wantState(t, err, sqlForbidden)
	next, err := f.object("rt", query, run, "r2", c2.Generation)
	ok(t, err)
	if next["intent_id"] != first["intent_id"] {
		t.Fatal("takeover created another intent")
	}
}

func TestStudioPreviewCannotCreateAnAction(t *testing.T) {
	f := newFix(t)
	v, _ := approvedDefinition(t, f, "preview", example)
	run := f.ID(t, "stella", `SELECT eacp.studio_preview_start($1,$2::jsonb,$3::jsonb)`, v, inputs, `{"lookup":{"structuredContent":{"days":12,"private":"preview-content-canary-27c"}}}`)
	c := f.mustClaim("r1")
	err := f.ExecAgent(v, registrytest.ReceivedActionSQL, v, stepKey(run, 0), stella, "hr-mcp.get_leave_balance")
	wantState(t, err, sqlForbidden)
	_, err = f.TryID("abe", `SELECT eacp.studio_preview_start($1,$2::jsonb,$3::jsonb)`, v, inputs, `{}`)
	wantState(t, err, sqlForbidden)
	_, err = f.object("rt", `SELECT eacp.studio_node_output($1,$2,$3,0)`, run, "r1", c.Generation)
	ok(t, err)
	_, err = f.object("rt", `SELECT eacp.studio_node_output($1,$2,$3,0)`, run, "r1", c.Generation+1)
	wantState(t, err, sqlForbidden)
	if got := f.scalar(`SELECT count(*)::text FROM eacp.audit_events WHERE position('preview-content-canary-27c' IN convert_from(payload,'UTF8'))>0`); got != "0" {
		t.Fatal("preview samples were journaled")
	}
}

func TestStudioGraphCannotFinishBeforeItsResponse(t *testing.T) {
	f := newFix(t)
	_, ag := approvedDefinition(t, f, "response-guard", branchDefinition)
	run := f.ID(t, "stella", startSQL, ag, inputs)
	c := f.mustClaim("r1")
	wantState(t, f.Exec("rt", finishSQL, run, "r1", c.Generation, "SUCCEEDED", "premature", nil), sqlBadState)
	_, err := f.object("rt", `SELECT eacp.studio_node_begin($1,$2,$3,0)`, run, "r1", c.Generation)
	ok(t, err)
	wantState(t, f.Exec("rt", `SELECT eacp.studio_node_complete($1,$2,$3,0,$4::jsonb)`, run, "r1", c.Generation, `{"choice":"false"}`), sqlCheck)
	wantState(t, f.Exec("rt", `SELECT eacp.studio_node_complete($1,$2,$3,0,$4::jsonb)`, run, "r1", c.Generation, `{"choice":false,"next":"lookup"}`), sqlCheck)
	ok(t, f.Exec("rt", `SELECT eacp.studio_node_complete($1,$2,$3,0,$4::jsonb)`, run, "r1", c.Generation, `{"choice":false}`))
	ok(t, f.Exec("rt", finishSQL, run, "r1", c.Generation, "SUCCEEDED", "declined", nil))
	_, err = f.object("rt", `SELECT eacp.studio_node_begin($1,$2,$3,2)`, run, "r1", c.Generation)
	wantState(t, err, sqlForbidden)
}
