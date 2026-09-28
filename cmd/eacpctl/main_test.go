package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/atipongsena/eacp/internal/storage/pgtest"
	"github.com/atipongsena/eacp/migrations"
)

func runWith(t *testing.T, env map[string]string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := run(context.Background(), args, func(k string) string { return env[k] }, &out)
	return out.String(), err
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{nil, {"migrate"}, {"migrate", "sideways"}, {"agent"}, {"tool"}, {"tool", "get"}, {"tool", "get", "x", "y"}, {"tool", "release", "x"}, {"connector", "scan", "x"}} {
		if _, err := runWith(t, map[string]string{"EACP_DATABASE_URL": "postgres://x@y/z"}, args...); err == nil {
			t.Errorf("run(%v) succeeded, want usage error", args)
		}
	}
}

func TestMigrateRequiresDatabaseURL(t *testing.T) {
	if _, err := runWith(t, nil, "migrate", "up"); err == nil || !strings.Contains(err.Error(), "EACP_DATABASE_URL") {
		t.Fatalf("err = %v, want missing EACP_DATABASE_URL", err)
	}
}

func TestDownAllIsRefusedOutsideDevelopment(t *testing.T) {
	for _, envName := range []string{"", "staging", "production"} {
		_, err := runWith(t, map[string]string{
			"EACP_ENV": envName, "EACP_DATABASE_URL": "postgres://x@127.0.0.1:1/z",
		}, "migrate", "down-all")
		if err == nil || !strings.Contains(err.Error(), "refused") {
			t.Errorf("EACP_ENV=%s: err = %v, want refusal", envName, err)
		}
	}
}

func TestDownAllRequiresExplicitConfirmation(t *testing.T) {
	_, err := runWith(t, map[string]string{
		"EACP_ENV": "development", "EACP_DATABASE_URL": "postgres://x@127.0.0.1:1/z",
	}, "migrate", "down-all")
	if err == nil || !strings.Contains(err.Error(), "--yes-destroy-all-data") {
		t.Fatalf("err = %v, want refusal without confirmation flag", err)
	}
}

func TestDownAllWithConfirmationInDevelopment(t *testing.T) {
	db := pgtest.New(t)
	env := map[string]string{"EACP_ENV": "test", "EACP_DATABASE_URL": db.OwnerDSN}
	if _, err := runWith(t, env, "migrate", "up"); err != nil {
		t.Fatalf("up: %v", err)
	}
	if _, err := runWith(t, env, "migrate", "down-all", "--yes-destroy-all-data"); err != nil {
		t.Fatalf("down-all: %v", err)
	}
	out, err := runWith(t, env, "migrate", "status")
	if err != nil || !strings.Contains(out, "current=0") {
		t.Fatalf("status after down-all = %q, %v", out, err)
	}
}

func TestMigrateUpThenStatus(t *testing.T) {
	db := pgtest.New(t)
	env := map[string]string{"EACP_DATABASE_URL": db.OwnerDSN}
	if _, err := runWith(t, env, "migrate", "up"); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	out, err := runWith(t, env, "migrate", "status")
	if err != nil {
		t.Fatalf("migrate status: %v", err)
	}
	want := fmt.Sprintf("current=%d latest=%d", migrations.Latest(), migrations.Latest())
	if !strings.Contains(out, want) {
		t.Fatalf("status output = %q", out)
	}
}
