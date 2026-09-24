package main

import (
	"context"
	"errors"
	"io"
	"net/url"

	"github.com/google/uuid"
)

const dependencyUsage = "usage: eacpctl dependency blast-radius mcp|tool|agent_version <uuid> | model|system <name>"

func runDependency(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	if len(args) != 3 || args[0] != "blast-radius" {
		return errors.New(dependencyUsage)
	}
	q := url.Values{"kind": {args[1]}}
	switch args[1] {
	case "mcp", "tool", "agent_version":
		if _, err := uuid.Parse(args[2]); err != nil {
			return errors.New(dependencyUsage)
		}
		q.Set("id", args[2])
	case "model", "system":
		if args[2] == "" {
			return errors.New(dependencyUsage)
		}
		q.Set("name", args[2])
	default:
		return errors.New(dependencyUsage)
	}
	return call(ctx, getenv, out, "GET", "/v1/dependencies/blast-radius?"+q.Encode(), nil)
}
