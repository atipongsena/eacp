package main

import (
	"context"
	"errors"
	"io"

	"github.com/google/uuid"
)

const killUsage = "usage: eacpctl kill activate|resume <tenant|team|agent|agent_version|action|connector|tool|model|run> <uuid> --reason <text> [--code <AGT reason>] | list"

func runKill(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	if len(args) == 1 && args[0] == "list" {
		return call(ctx, getenv, out, "GET", "/v1/killswitch", nil)
	}
	if len(args) < 3 || (args[0] != "activate" && args[0] != "resume") {
		return errors.New(killUsage)
	}
	switch args[1] {
	case "tenant", "team", "agent", "agent_version", "action", "connector", "tool", "model", "run":
	default:
		return errors.New(killUsage)
	}
	id, err := uuid.Parse(args[2])
	if err != nil {
		return errors.New(killUsage)
	}
	fs := newFlags("kill " + args[0])
	reason := fs.String("reason", "", "operator reason")
	code := fs.String("code", "operator_request", "AGT reason code")
	if err := fs.Parse(args[3:]); err != nil {
		return err
	}
	if *reason == "" || len(fs.Args()) != 0 {
		return errors.New(killUsage)
	}
	switch *code {
	case "policy_violation", "security_incident", "operator_request", "error_budget_exhausted":
	default:
		return errors.New(killUsage)
	}
	return call(ctx, getenv, out, "POST", "/v1/killswitch", map[string]any{
		"scope": args[1], "target_id": id, "killed": args[0] == "activate", "reason": *reason,
		"reason_code": *code,
	})
}
