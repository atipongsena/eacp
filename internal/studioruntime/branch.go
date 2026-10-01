package studioruntime

import (
	"encoding/json"
	"errors"
	"math/big"
)

func evaluateBranch(left any, operator string, right any) (bool, error) {
	invalid := errors.New("branch_invalid")
	if operator != "eq" && operator != "ne" && operator != "lt" && operator != "le" && operator != "gt" && operator != "ge" {
		return false, invalid
	}
	var cmp int
	switch l := left.(type) {
	case json.Number:
		r, ok := right.(json.Number)
		if !ok {
			return false, invalid
		}
		a, aok := new(big.Rat).SetString(string(l))
		b, bok := new(big.Rat).SetString(string(r))
		if !aok || !bok {
			return false, invalid
		}
		cmp = a.Cmp(b)
	case string:
		r, ok := right.(string)
		if !ok || (operator != "eq" && operator != "ne") {
			return false, invalid
		}
		if l != r {
			cmp = 1
		}
	case bool:
		r, ok := right.(bool)
		if !ok || (operator != "eq" && operator != "ne") {
			return false, invalid
		}
		if l != r {
			cmp = 1
		}
	default:
		return false, invalid
	}
	switch operator {
	case "eq":
		return cmp == 0, nil
	case "ne":
		return cmp != 0, nil
	case "lt":
		return cmp < 0, nil
	case "le":
		return cmp <= 0, nil
	case "gt":
		return cmp > 0, nil
	case "ge":
		return cmp >= 0, nil
	}
	return false, invalid
}
