package main

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strconv"
)

const actionUsage = `usage:
  eacpctl action list --state <STATE> [--limit N]
  eacpctl action get|evidence <action-id>
  eacpctl action resolve <action-id> --outcome succeeded|failed|retry --reason <text>
          [--evidence <text>] [--external-reference <ref>]
  eacpctl action confirm|withdraw <action-id> <resolution-id> --reason <text>`

// runAction is the operator's view of actions and human resolution
// (ADR-004 T35-T37). The API enforces the operator role and separation of
// duties; a retry is only proposed until a second operator confirms it.
func runAction(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(actionUsage)
	}
	switch args[0] {
	case "list":
		fs := newFlags("action list")
		state := fs.String("state", "", "action state, e.g. NEEDS_HUMAN_RESOLUTION")
		limit := fs.Int("limit", 0, "at most this many (1-200)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *state == "" || fs.NArg() != 0 {
			return errors.New(actionUsage)
		}
		q := url.Values{"state": {*state}}
		if *limit != 0 {
			q.Set("limit", strconv.Itoa(*limit))
		}
		return call(ctx, getenv, out, "GET", "/v1/actions?"+q.Encode(), nil)
	case "get", "evidence":
		if len(args) != 2 {
			return errors.New(actionUsage)
		}
		path := "/v1/actions/" + url.PathEscape(args[1])
		if args[0] == "evidence" {
			path += "/evidence"
		}
		return call(ctx, getenv, out, "GET", path, nil)
	case "resolve":
		if len(args) < 2 {
			return errors.New(actionUsage)
		}
		fs := newFlags("action resolve")
		outcome := fs.String("outcome", "", "succeeded|failed|retry")
		reason := fs.String("reason", "", "why (journaled)")
		evidence := fs.String("evidence", "", "what shows the outcome (required for succeeded and failed)")
		ref := fs.String("external-reference", "", "the target's reference (required for succeeded)")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if *outcome == "" || *reason == "" || fs.NArg() != 0 {
			return errors.New(actionUsage)
		}
		return call(ctx, getenv, out, "POST", "/v1/actions/"+url.PathEscape(args[1])+"/resolutions", map[string]any{
			"outcome": *outcome, "reason": *reason, "evidence": *evidence, "external_reference": *ref})
	case "confirm", "withdraw":
		if len(args) < 3 {
			return errors.New(actionUsage)
		}
		fs := newFlags("action " + args[0])
		reason := fs.String("reason", "", "why (journaled)")
		if err := fs.Parse(args[3:]); err != nil {
			return err
		}
		if *reason == "" || fs.NArg() != 0 {
			return errors.New(actionUsage)
		}
		return call(ctx, getenv, out, "POST", "/v1/actions/"+url.PathEscape(args[1])+"/resolutions/"+
			url.PathEscape(args[2])+"/"+args[0], map[string]any{"reason": *reason})
	default:
		return errors.New(actionUsage)
	}
}
