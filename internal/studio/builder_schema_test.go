package studio_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

const builderDefinition = `{"schema_version":2,"kind":"agent","inputs":{"employee_id":{"type":"string","max_length":64}},"limits":{"timeout_seconds":120,"max_output_tokens":100},"steps":[{"id":"lookup","kind":"tool_call","tool":"hr-mcp.get_leave_balance","tool_schema_version":"1","operation":"lookup","target":"hr","resource":"leave_balance","payload":{"employee_id":"{{inputs.employee_id}}"},"next":"classify"},{"id":"classify","kind":"llm","model":"triage","instruction":"Return JSON only.","input":{"balance":"{{steps.lookup.output.days}}"},"max_output_tokens":100,"output_schema":{"type":"object","properties":{"eligible":{"type":"boolean"}},"required":["eligible"],"additionalProperties":false},"next":"choose"},{"id":"choose","kind":"branch","condition":{"left":"{{steps.classify.output.eligible}}","operator":"eq","right":true},"then":"yes","else":"no"},{"id":"yes","kind":"respond","text":"Eligible"},{"id":"no","kind":"respond","text":"Not eligible"}]}`

func builderFix(t *testing.T) (*fix, uuid.UUID) {
	t.Helper()
	f := newFix(t)
	m := f.LLMModel(t, "triage", "openai", "https://llm.example.test", "triage-upstream")
	return f, m
}

func TestStudioV2DerivesExactlyToolsAndModels(t *testing.T) {
	f, m := builderFix(t)
	v := f.mustSave("stella", nil, "triage-agent", builderDefinition)
	if got := f.scalar(`SELECT model_capability::text FROM eacp.studio_versions WHERE id=$1`, v); got != "{"+m.String()+"}" {
		t.Fatalf("model capability = %s", got)
	}
	if got := f.scalar(`SELECT tool_ids::text FROM eacp.agent_allowlists WHERE agent_version_id=$1`, v); got != "{"+f.balance.Tool.String()+"}" {
		t.Fatalf("tool capability = %s", got)
	}
	if got := f.scalar(`SELECT model_ids::text FROM eacp.agent_allowlists WHERE agent_version_id=$1`, v); got != "{"+m.String()+"}" {
		t.Fatalf("model allowlist = %s", got)
	}
	wantState(t, f.decide("stella", v, true, "self"), sqlForbidden, sqlBadState)
	ok(t, f.decide("rita", v, true, "reviewed"))
	wantState(t, f.Exec("erin", `INSERT INTO eacp.agent_allowlists (tenant_id,agent_version_id,model_ids) VALUES(eacp.current_tenant_id(),$1,'{}')`, v), sqlForbidden)
}

func TestStudioV2RejectsInvalidGraphs(t *testing.T) {
	f, _ := builderFix(t)
	f.mustSave("stella", nil, "valid-builder", builderDefinition)
	cases := map[string]string{
		"backward":            strings.Replace(builderDefinition, `"next":"classify"`, `"next":"lookup"`, 1),
		"unknown destination": strings.Replace(builderDefinition, `"else":"no"`, `"else":"absent"`, 1),
		"unreachable":         strings.Replace(builderDefinition, `"else":"no"`, `"else":"yes"`, 1),
		"model placeholder":   strings.Replace(builderDefinition, `"model":"triage"`, `"model":"{{inputs.employee_id}}"`, 1),
		"unknown model":       strings.Replace(builderDefinition, `"model":"triage"`, `"model":"absent"`, 1),
		"operator":            strings.Replace(builderDefinition, `"operator":"eq"`, `"operator":"eval"`, 1),
		"reference schema":    strings.Replace(builderDefinition, `"type":"object","properties"`, `"$ref":"https://schema.example.test","type":"object","properties"`, 1),
		"total token cap":     strings.Replace(builderDefinition, `"max_output_tokens":100`, `"max_output_tokens":99`, 1),
		"model cap":           strings.ReplaceAll(builderDefinition, `"max_output_tokens":100`, `"max_output_tokens":999999`),
		"duplicate id":        strings.Replace(builderDefinition, `"id":"no"`, `"id":"yes"`, 1),
		"missing input type":  strings.Replace(builderDefinition, `"type":"string",`, ``, 1),
		"null input type":     strings.Replace(builderDefinition, `"type":"string"`, `"type":null`, 1),
	}
	for name, def := range cases {
		t.Run(name, func(t *testing.T) {
			wantState(t, f.Exec("stella", `SELECT eacp.studio_definition_capability($1)`, def), sqlCheck)
		})
	}
}

func TestStudioV2OutputReferencesMustDominate(t *testing.T) {
	f, _ := builderFix(t)
	var d map[string]any
	ok(t, json.Unmarshal([]byte(builderDefinition), &d))
	steps := d["steps"].([]any)
	// A fixed branch skips the model on one path, then joins at its consumer.
	steps[0] = map[string]any{"id": "entry", "kind": "branch", "condition": map[string]any{"left": "{{inputs.employee_id}}", "operator": "eq", "right": "x"}, "then": "classify", "else": "choose"}
	steps[1].(map[string]any)["input"] = map[string]any{}
	b, err := json.Marshal(d)
	ok(t, err)
	wantState(t, f.Exec("stella", `SELECT eacp.studio_definition_capability($1)`, string(b)), sqlCheck)
}

func TestStudioCheckedSaveRefusesAStaleVersion(t *testing.T) {
	f := newFix(t)
	v := f.mustSave("stella", nil, "checked-save", example)
	agent := f.agentOf(v)
	_, err := f.TryID("stella", `SELECT eacp.studio_save_checked($1,NULL,NULL,NULL,NULL,$2,$3)`, agent, example, v)
	ok(t, err)
	_, err = f.TryID("stella", `SELECT eacp.studio_save_checked($1,NULL,NULL,NULL,NULL,$2,$3)`, agent, example, v)
	p := wantState(t, err, sqlBadState)
	if !strings.Contains(p.Message, "studio_version_stale") {
		t.Fatalf("reason = %s", p.Message)
	}
}
