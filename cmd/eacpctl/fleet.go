package main

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strings"

	"github.com/google/uuid"
)

const fleetUsage = `usage:
  eacpctl fleet status|list [--environment E] [--risk R] [--owner-group UUID] [--health H] [--window 24h]
  eacpctl fleet operation <operation-id>
  eacpctl fleet pause|quarantine [--agent NAME]... [--environment E] [--risk R] [--owner-group UUID]
                                 [--tool CONNECTOR.TOOL] [--all] --reason TEXT [--dry-run]
  eacpctl fleet resume|release <operation-id> --reason TEXT [--dry-run]
  eacpctl fleet rollback <agent> [--to <version-id>] --reason TEXT [--dry-run]`

// listFlag collects a repeated flag.
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

func runFleet(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(fleetUsage)
	}
	switch args[0] {
	case "status", "list":
		return fleetView(ctx, args, getenv, out)
	case "operation":
		if len(args) != 2 {
			return errors.New(fleetUsage)
		}
		if _, err := uuid.Parse(args[1]); err != nil {
			return errors.New(fleetUsage)
		}
		return call(ctx, getenv, out, "GET", "/v1/fleet/operations/"+args[1], nil)
	case "pause", "quarantine", "resume", "release", "rollback":
		return fleetOperation(ctx, args, getenv, out)
	default:
		return errors.New(fleetUsage)
	}
}

func fleetView(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	fs := newFlags("fleet " + args[0])
	q := url.Values{}
	for _, name := range []string{"environment", "risk", "owner-group", "health", "window"} {
		fs.Func(name, "filter", func(v string) error {
			key := map[string]string{"risk": "risk_class", "owner-group": "owner_group_id"}[name]
			if key == "" {
				key = name
			}
			q.Set(key, v)
			return nil
		})
	}
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return errors.New(fleetUsage)
	}
	path := map[string]string{"status": "/v1/fleet/health", "list": "/v1/fleet/agents"}[args[0]]
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return call(ctx, getenv, out, "GET", path, nil)
}

func fleetOperation(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	kind, rest := args[0], args[1:]
	body := map[string]any{"kind": kind}
	var positional string
	if kind == "resume" || kind == "release" || kind == "rollback" {
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
			return errors.New(fleetUsage)
		}
		positional, rest = rest[0], rest[1:]
	}
	fs := newFlags("fleet " + kind)
	reason := fs.String("reason", "", "operator reason")
	dryRun := fs.Bool("dry-run", false, "return the plan without changing anything")
	var agents listFlag
	sel := map[string]any{}
	var to string
	if kind == "pause" || kind == "quarantine" {
		fs.Var(&agents, "agent", "agent slug (repeatable)")
		for _, name := range []string{"environment", "risk", "owner-group", "tool"} {
			fs.Func(name, "selector", func(v string) error {
				key := map[string]string{"risk": "risk_class", "owner-group": "owner_group_id"}[name]
				if key == "" {
					key = name
				}
				sel[key] = v
				return nil
			})
		}
		fs.BoolFunc("all", "select every agent", func(string) error { sel["all"] = true; return nil })
	}
	if kind == "rollback" {
		fs.StringVar(&to, "to", "", "version to roll back to")
	}
	if err := fs.Parse(rest); err != nil || fs.NArg() != 0 || *reason == "" {
		return errors.New(fleetUsage)
	}
	switch kind {
	case "pause", "quarantine":
		if len(agents) > 0 {
			sel["agents"] = []string(agents)
		}
		if len(sel) == 0 {
			return errors.New(fleetUsage)
		}
		body["selector"] = sel
	case "resume", "release":
		if _, err := uuid.Parse(positional); err != nil {
			return errors.New(fleetUsage)
		}
		body["source_operation_id"] = positional
	case "rollback":
		body["selector"] = map[string]any{"agents": []string{positional}}
		if to != "" {
			if _, err := uuid.Parse(to); err != nil {
				return errors.New(fleetUsage)
			}
			body["to_version_id"] = to
		}
	}
	body["reason"] = *reason
	if *dryRun {
		body["dry_run"] = true
	}
	return call(ctx, getenv, out, "POST", "/v1/fleet/operations", body)
}
