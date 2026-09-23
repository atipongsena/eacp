// Command eacpctl is the EACP operator CLI.
//
// Schema (EACP_DATABASE_URL must be the schema owner's DSN):
//
//	eacpctl migrate up        apply pending migrations
//	eacpctl migrate status    print current and latest versions
//	eacpctl migrate down-all --yes-destroy-all-data
//	                          roll back everything; requires EACP_ENV=development|test
//
// Bootstrap (schema owner DSN; the break-glass trust root, ADR-003):
//
//	eacpctl key generate --kind agent|principal --tenant <uuid>
//	eacpctl tenant create --slug <slug> --name <name> [--id <uuid>]
//	        --admin name=<n>,subject=<s>,credential=<uuid>,hash=<hex>   (at least two)
//
// Registry, through the API (EACP_API_URL, EACP_API_KEY):
//
//	eacpctl agent register --name --display-name --env --risk (--owner-principal|--owner-group) <uuid>
//	eacpctl agent list
//	eacpctl agent inspect <id|name>
//	eacpctl connector register --name --endpoint --secret-ref [--protocol http]
//	eacpctl api <METHOD> <PATH> [JSON]      any other API call
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

const usage = `usage:
  eacpctl migrate up|status|down-all --yes-destroy-all-data
  eacpctl key generate --kind agent|principal --tenant <uuid>
  eacpctl tenant create --slug <slug> --name <name> --admin <spec> --admin <spec>
  eacpctl agent register|list|inspect ...
  eacpctl connector register ...
  eacpctl api <METHOD> <PATH> [JSON]`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "eacpctl:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "migrate":
		return runMigrate(ctx, args[1:], getenv, out)
	case "key":
		return runKey(args[1:], out)
	case "tenant":
		return runTenant(ctx, args[1:], getenv, out)
	case "agent":
		return runAgent(ctx, args[1:], getenv, out)
	case "connector":
		return runConnector(ctx, args[1:], getenv, out)
	case "api":
		return runAPI(ctx, args[1:], getenv, out)
	default:
		return errors.New(usage)
	}
}
