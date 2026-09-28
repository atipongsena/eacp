package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/atipongsena/eacp/internal/storage"
)

const (
	migrateUsage = "usage: eacpctl migrate up|status|down-all --yes-destroy-all-data"
	confirmFlag  = "--yes-destroy-all-data"
)

func runMigrate(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	if len(args) < 1 || len(args) > 2 {
		return errors.New(migrateUsage)
	}
	dsn := getenv("EACP_DATABASE_URL")
	if dsn == "" {
		return errors.New("EACP_DATABASE_URL: required (schema owner DSN)")
	}
	if args[0] != "down-all" && len(args) != 1 {
		return errors.New(migrateUsage)
	}

	switch args[0] {
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
		if len(args) != 2 || args[1] != confirmFlag {
			return fmt.Errorf("down-all destroys all data; re-run with %s", confirmFlag)
		}
		if err := storage.MigrateDownAll(ctx, dsn); err != nil {
			return err
		}
		fmt.Fprintln(out, "all migrations rolled back")
		return nil
	default:
		return errors.New(migrateUsage)
	}
}
