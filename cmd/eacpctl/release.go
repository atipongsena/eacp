package main

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

const releaseUsage = `usage:
  eacpctl release list [--agent UUID] [--state S]
  eacpctl release show <release-id>
  eacpctl release open --candidate <version-id> --suite NAME... --reason TEXT
                       [--min-replay N] [--min-shadow N] [--steps BP,BP,...] [--min-canary-actions N]
  eacpctl release evaluation <release-id> --suite NAME --score X --threshold Y
                             --dataset-digest SHA256 --evidence REF
  eacpctl release advance <release-id> --from STATE [--from-bp N] --reason TEXT
  eacpctl release rollback <release-id> --reason TEXT`

func runRelease(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(releaseUsage)
	}
	switch args[0] {
	case "list":
		fs := newFlags("release list")
		q := url.Values{}
		fs.Func("agent", "agent id", func(v string) error {
			if _, err := uuid.Parse(v); err != nil {
				return err
			}
			q.Set("agent_id", v)
			return nil
		})
		fs.Func("state", "release state", func(v string) error { q.Set("state", v); return nil })
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
			return errors.New(releaseUsage)
		}
		path := "/v1/releases"
		if len(q) > 0 {
			path += "?" + q.Encode()
		}
		return call(ctx, getenv, out, "GET", path, nil)
	case "show":
		if len(args) != 2 {
			return errors.New(releaseUsage)
		}
		if _, err := uuid.Parse(args[1]); err != nil {
			return errors.New(releaseUsage)
		}
		return call(ctx, getenv, out, "GET", "/v1/releases/"+args[1], nil)
	case "open":
		return releaseOpen(ctx, args[1:], getenv, out)
	case "evaluation", "advance", "rollback":
		return releaseChange(ctx, args, getenv, out)
	default:
		return errors.New(releaseUsage)
	}
}

func releaseOpen(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	fs := newFlags("release open")
	candidate := fs.String("candidate", "", "candidate version id")
	reason := fs.String("reason", "", "why")
	var suites listFlag
	fs.Var(&suites, "suite", "required evaluation suite (repeatable)")
	body := map[string]any{}
	for _, name := range []string{"min-replay", "min-shadow", "min-canary-actions"} {
		fs.Func(name, "count", func(v string) error {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return errors.New("a non-negative integer")
			}
			body[map[string]string{"min-replay": "min_replay_cases", "min-shadow": "min_shadow_cases",
				"min-canary-actions": "min_canary_actions"}[name]] = n
			return nil
		})
	}
	fs.Func("steps", "canary steps in basis points", func(v string) error {
		var steps []int
		for _, s := range strings.Split(v, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(s))
			if err != nil {
				return err
			}
			steps = append(steps, n)
		}
		body["canary_steps"] = steps
		return nil
	})
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *reason == "" || len(suites) == 0 {
		return errors.New(releaseUsage)
	}
	if _, err := uuid.Parse(*candidate); err != nil {
		return errors.New(releaseUsage)
	}
	body["candidate_version_id"], body["reason"], body["required_suites"] = *candidate, *reason, []string(suites)
	return call(ctx, getenv, out, "POST", "/v1/releases", body)
}

func releaseChange(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	kind := args[0]
	if len(args) < 2 {
		return errors.New(releaseUsage)
	}
	if _, err := uuid.Parse(args[1]); err != nil {
		return errors.New(releaseUsage)
	}
	fs := newFlags("release " + kind)
	body := map[string]any{}
	var required []string
	str := func(flag, key string) {
		required = append(required, key)
		fs.Func(flag, key, func(v string) error { body[key] = v; return nil })
	}
	switch kind {
	case "evaluation":
		str("suite", "suite")
		str("score", "score")
		str("threshold", "threshold")
		str("dataset-digest", "dataset_digest")
		str("evidence", "evidence_ref")
	case "advance":
		str("from", "from")
		str("reason", "reason")
		fs.Func("from-bp", "the canary step reviewed", func(v string) error {
			n, err := strconv.Atoi(v)
			if err != nil {
				return err
			}
			body["from_canary_bp"] = n
			return nil
		})
	case "rollback":
		str("reason", "reason")
	}
	if err := fs.Parse(args[2:]); err != nil || fs.NArg() != 0 {
		return errors.New(releaseUsage)
	}
	for _, key := range required {
		if v, _ := body[key].(string); v == "" {
			return errors.New(releaseUsage)
		}
	}
	path := "/v1/releases/" + args[1] + "/" + kind
	if kind == "evaluation" {
		path += "s"
	}
	return call(ctx, getenv, out, "POST", path, body)
}
