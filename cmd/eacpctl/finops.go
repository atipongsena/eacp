package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"

	"github.com/google/uuid"
)

const finopsUsage = `usage:
  eacpctl finops dashboard
  eacpctl finops chargeback [--by agent|team|account] [--from RFC3339] [--to RFC3339]
  eacpctl finops usage [--agent UUID] [--from RFC3339] [--to RFC3339] [--limit N]
  eacpctl finops prices
  eacpctl finops price add --provider P --model M --unit U --input X --output Y [--cached Z]
                           [--effective-from RFC3339] --reason TEXT
  eacpctl finops billing import <file.json>     a JSON array of lines, or {"lines": [...]}
  eacpctl finops soft-limits
  eacpctl finops soft-limit <account-id> (--limit X | --clear) --reason TEXT
  eacpctl finops alerts [--open] [--limit N]
  eacpctl finops ack <alert-id> --reason TEXT`

func runFinOps(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(finopsUsage)
	}
	simple := map[string]string{"dashboard": "/v1/finops/dashboard", "prices": "/v1/finops/prices",
		"soft-limits": "/v1/finops/soft-limits"}
	if path, ok := simple[args[0]]; ok {
		if len(args) != 1 {
			return errors.New(finopsUsage)
		}
		return call(ctx, getenv, out, "GET", path, nil)
	}
	switch args[0] {
	case "chargeback", "usage", "alerts":
		return finopsQuery(ctx, args, getenv, out)
	case "price":
		return finopsAddPrice(ctx, args[1:], getenv, out)
	case "billing":
		if len(args) != 3 || args[1] != "import" {
			return errors.New(finopsUsage)
		}
		return finopsImport(ctx, args[2], getenv, out)
	case "soft-limit":
		return finopsSoftLimit(ctx, args[1:], getenv, out)
	case "ack":
		if len(args) < 2 {
			return errors.New(finopsUsage)
		}
		if _, err := uuid.Parse(args[1]); err != nil {
			return errors.New(finopsUsage)
		}
		fs := newFlags("finops ack")
		reason := fs.String("reason", "", "why")
		if err := fs.Parse(args[2:]); err != nil || fs.NArg() != 0 || *reason == "" {
			return errors.New(finopsUsage)
		}
		return call(ctx, getenv, out, "POST", "/v1/finops/alerts/"+args[1]+"/ack", map[string]any{"reason": *reason})
	default:
		return errors.New(finopsUsage)
	}
}

func finopsQuery(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	fs := newFlags("finops " + args[0])
	q := url.Values{}
	set := func(key string) func(string) error {
		return func(v string) error { q.Set(key, v); return nil }
	}
	switch args[0] {
	case "chargeback":
		fs.Func("by", "agent, team or account", set("group_by"))
	case "usage":
		fs.Func("agent", "agent id", set("agent_id"))
	case "alerts":
		fs.BoolFunc("open", "unacknowledged only", func(string) error { q.Set("open", "true"); return nil })
	}
	if args[0] != "alerts" {
		fs.Func("from", "period start", set("from"))
		fs.Func("to", "period end", set("to"))
	}
	if args[0] != "chargeback" {
		fs.Func("limit", "at most N", func(v string) error {
			if n, err := strconv.Atoi(v); err != nil || n < 1 {
				return errors.New("limit must be a positive integer")
			}
			q.Set("limit", v)
			return nil
		})
	}
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return errors.New(finopsUsage)
	}
	path := "/v1/finops/" + args[0]
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return call(ctx, getenv, out, "GET", path, nil)
}

func finopsAddPrice(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	if len(args) == 0 || args[0] != "add" {
		return errors.New(finopsUsage)
	}
	fs := newFlags("finops price add")
	body := map[string]any{}
	for flag, key := range map[string]string{"provider": "provider", "model": "model", "unit": "unit",
		"input": "input_per_mtok", "output": "output_per_mtok", "cached": "cached_input_per_mtok",
		"effective-from": "effective_from", "reason": "reason"} {
		fs.Func(flag, key, func(v string) error { body[key] = v; return nil })
	}
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return errors.New(finopsUsage)
	}
	for _, k := range []string{"provider", "model", "unit", "input_per_mtok", "output_per_mtok", "reason"} {
		if body[k] == nil || body[k] == "" {
			return errors.New(finopsUsage)
		}
	}
	return call(ctx, getenv, out, "POST", "/v1/finops/prices", body)
}

// finopsImport sends a billing file: a JSON array of lines or {"lines": [...]}.
func finopsImport(ctx context.Context, file string, getenv func(string) string, out io.Writer) error {
	raw, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	raw = bytes.TrimSpace(raw)
	if !json.Valid(raw) {
		return fmt.Errorf("%s: not JSON", file)
	}
	if len(raw) > 0 && raw[0] == '[' {
		raw = append(append([]byte(`{"lines":`), raw...), '}')
	}
	return call(ctx, getenv, out, "POST", "/v1/finops/billing", json.RawMessage(raw))
}

func finopsSoftLimit(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(finopsUsage)
	}
	if _, err := uuid.Parse(args[0]); err != nil {
		return errors.New(finopsUsage)
	}
	fs := newFlags("finops soft-limit")
	limit := fs.String("limit", "", "monthly limit")
	clear := fs.Bool("clear", false, "remove the limit")
	reason := fs.String("reason", "", "why")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || *reason == "" || (*limit == "") == !*clear {
		return errors.New(finopsUsage)
	}
	body := map[string]any{"reason": *reason, "monthly_limit": nil}
	if !*clear {
		body["monthly_limit"] = *limit
	}
	return call(ctx, getenv, out, "PUT", "/v1/finops/soft-limits/"+args[0], body)
}
