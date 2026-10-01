package studioruntime

import (
	"encoding/json"
	"testing"
)

func TestBranchPreservesJSONNumberPrecision(t *testing.T) {
	for _, c := range []struct {
		left  any
		op    string
		right any
		want  bool
	}{
		{json.Number("9007199254740993"), "gt", json.Number("9007199254740992"), true},
		{json.Number("9007199254740993"), "eq", json.Number("9007199254740992"), false},
		{json.Number("1e3"), "eq", json.Number("1000"), true},
		{json.Number("-0"), "eq", json.Number("0"), true},
		{json.Number("1.0000000000000000000001"), "gt", json.Number("1"), true},
		{json.Number("-2"), "lt", json.Number("-1"), true},
		{json.Number("2"), "le", json.Number("2"), true},
		{json.Number("2"), "ge", json.Number("2"), true},
		{true, "ne", false, true}, {"yes", "eq", "yes", true}, {"a", "ne", "b", true},
	} {
		got, err := evaluateBranch(c.left, c.op, c.right)
		if err != nil || got != c.want {
			t.Fatalf("branch %v %s %v = %v, %v", c.left, c.op, c.right, got, err)
		}
	}
}

func TestBranchRefusesCoercionAndNonScalars(t *testing.T) {
	for _, c := range []struct {
		left  any
		op    string
		right any
	}{
		{"1", "eq", json.Number("1")}, {nil, "eq", nil}, {true, "gt", false}, {"a", "lt", "b"},
		{[]any{}, "eq", []any{}}, {map[string]any{}, "ne", map[string]any{}},
		{json.Number("NaN"), "eq", json.Number("0")}, {true, "unknown", false},
	} {
		if _, err := evaluateBranch(c.left, c.op, c.right); err == nil {
			t.Fatalf("accepted %T %s %T", c.left, c.op, c.right)
		}
	}
}
