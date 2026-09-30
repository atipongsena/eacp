package studioruntime_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/atipongsena/eacp/internal/studioruntime"
)

func TestRenderSubstitutesInputsAndOutputs(t *testing.T) {
	var out any
	d := json.NewDecoder(strings.NewReader(`{"structuredContent": {"days": 12, "ok": true, "list": ["a", {"b": 2}], "none": null}}`))
	d.UseNumber()
	if err := d.Decode(&out); err != nil {
		t.Fatal(err)
	}
	env := studioruntime.Env{Inputs: map[string]string{"employee_id": "E-7"}, Outputs: map[string]any{"lookup": out}}

	var payload any
	if err := json.Unmarshal([]byte(`{
		"id": "{{inputs.employee_id}}",
		"days": "{{steps.lookup.output.structuredContent.days}}",
		"ok": "{{steps.lookup.output.structuredContent.ok}}",
		"whole": "{{steps.lookup.output.structuredContent}}",
		"second": "{{steps.lookup.output.structuredContent.list.1.b}}",
		"text": "E {{inputs.employee_id}} has {{steps.lookup.output.structuredContent.days}} days: {{steps.lookup.output.structuredContent.list}}",
		"nested": ["{{inputs.employee_id}}", {"deep": "x{{inputs.employee_id}}"}],
		"plain": "no placeholder", "n": 3, "b": false}`), &payload); err != nil {
		t.Fatal(err)
	}
	got, err := studioruntime.Render(payload, env)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got)
	var have, want any
	_ = json.Unmarshal(raw, &have)
	_ = json.Unmarshal([]byte(`{"id": "E-7", "days": 12, "ok": true, "whole": {"days": 12, "ok": true, "list": ["a", {"b": 2}], "none": null},
		"second": 2, "text": "E E-7 has 12 days: [\"a\",{\"b\":2}]", "nested": ["E-7", {"deep": "xE-7"}],
		"plain": "no placeholder", "n": 3, "b": false}`), &want)
	if !reflect.DeepEqual(have, want) {
		t.Fatalf("rendered %s", raw)
	}

	// A missing value is never sent as the literal placeholder.
	for _, s := range []string{
		"{{steps.lookup.output.structuredContent.weeks}}", "{{steps.other.output}}", "{{inputs.nobody}}",
		"x {{steps.lookup.output.structuredContent.none}}", "{{steps.lookup.output.structuredContent.list.9}}",
		"{{steps.lookup.output.structuredContent.days.x}}",
	} {
		if v, err := studioruntime.Render(map[string]any{"v": s}, env); !errors.Is(err, studioruntime.ErrResultUnavailable) {
			t.Fatalf("%s rendered %v, %v", s, v, err)
		}
	}
}
