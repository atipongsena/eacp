// Command eacpctl is the EACP operator CLI.
//
// Phase 1 provides schema migrations only:
//
//	eacpctl migrate up        apply pending migrations (as the schema owner)
//	eacpctl migrate status    print current and latest versions
//	eacpctl migrate down-all --yes-destroy-all-data
//	                          roll back everything; requires EACP_ENV=development|test
//
// EACP_DATABASE_URL must be the schema owner's DSN.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"eacp/internal/storage"
)

const (
	usage       = "usage: eacpctl migrate up|status|down-all --yes-destroy-all-data"
	confirmFlag = "--yes-destroy-all-data"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "eacpctl:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	if len(args) < 2 || len(args) > 3 || args[0] != "migrate" {
		return errors.New(usage)
	}
	dsn := getenv("EACP_DATABASE_URL")
	if dsn == "" {
		return errors.New("EACP_DATABASE_URL: required (schema owner DSN)")
	}

	if args[1] != "down-all" && len(args) != 2 {
		return errors.New(usage)
	}

	switch args[1] {
	case "up":
		if err := storage.MigrateUp(ctx, dsn); err != nil {
			return err
		}
		fmt.Fprintln(out, "migrations applied")
		return nil
	case "status":
		current, latest, err := storage.MigrationStatus(ctx, dsn)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "current=%d latest=%d\n", current, latest)
		return nil
	case "down-all":
		// Destructive: requires an explicit non-production environment AND an
		// explicit flag. An unset EACP_ENV is refused (fail closed).
		switch env := getenv("EACP_ENV"); env {
		case "development", "test":
		default:
			return fmt.Errorf("down-all refused in EACP_ENV=%q: it destroys all data; only development or test are allowed", env)
		}
		if len(args) != 3 || args[2] != confirmFlag {
			return fmt.Errorf("down-all destroys all data; re-run with %s", confirmFlag)
		}
		if err := storage.MigrateDownAll(ctx, dsn); err != nil {
			return err
		}
		fmt.Fprintln(out, "all migrations rolled back")
		return nil
	default:
		return errors.New(usage)
	}
}
