package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/audit"
	"github.com/atipongsena/eacp/internal/identity"
	"github.com/atipongsena/eacp/internal/storage"
)

func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// runKey generates a key locally. Only the credential id and hash are ever
// registered with EACP (bring your own key, ADR-003 §5); the key stays with
// whoever ran this command.
func runKey(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "generate" {
		return errors.New("usage: eacpctl key generate --kind agent|principal --tenant <uuid>")
	}
	fs := newFlags("key generate")
	kind := fs.String("kind", "", "agent or principal")
	tenant := fs.String("tenant", "", "tenant id")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	k := map[string]identity.Kind{"agent": identity.KindAgent, "principal": identity.KindPrincipal}[*kind]
	if k == "" {
		return fmt.Errorf("--kind must be agent or principal, not %q", *kind)
	}
	tid, err := uuid.Parse(*tenant)
	if err != nil || tid == uuid.Nil {
		return fmt.Errorf("--tenant must be a tenant UUID")
	}
	credID := uuid.New()
	key, hash, err := identity.NewKey(k, tid, credID)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "key:           %s\ncredential_id: %s\nhash:          %s\n\n", key, credID, hex.EncodeToString(hash))
	fmt.Fprintln(out, "Keep the key secret; it is not stored anywhere. Register only credential_id and hash.")
	return nil
}

type adminSpec struct {
	name, subject string
	credential    uuid.UUID
	hash          []byte
}

type adminFlags []adminSpec

func (a *adminFlags) String() string { return fmt.Sprint(len(*a)) }

func (a *adminFlags) Set(v string) error {
	var s adminSpec
	for _, kv := range strings.Split(v, ",") {
		k, val, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("--admin %q: want name=..,subject=..,credential=..,hash=..", v)
		}
		switch k {
		case "name":
			s.name = val
		case "subject":
			s.subject = strings.ToLower(strings.TrimSpace(val))
		case "credential":
			id, err := uuid.Parse(val)
			if err != nil {
				return fmt.Errorf("--admin credential: %w", err)
			}
			s.credential = id
		case "hash":
			h, err := hex.DecodeString(val)
			if err != nil || len(h) != 32 {
				return fmt.Errorf("--admin hash must be 64 hex characters")
			}
			s.hash = h
		default:
			return fmt.Errorf("--admin: unknown field %q", k)
		}
	}
	if s.name == "" || s.subject == "" || s.credential == uuid.Nil || s.hash == nil {
		return fmt.Errorf("--admin %q: name, subject, credential and hash are all required", v)
	}
	*a = append(*a, s)
	return nil
}

// runTenant bootstraps a tenant with at least two human admins as the schema
// owner, the only path that may create tenants and pre-approved grants.
func runTenant(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	if len(args) == 0 || args[0] != "create" {
		return errors.New("usage: eacpctl tenant create --slug <slug> --name <name> --admin <spec> --admin <spec>")
	}
	fs := newFlags("tenant create")
	slug := fs.String("slug", "", "tenant slug")
	name := fs.String("name", "", "display name")
	id := fs.String("id", "", "tenant id (default: random)")
	var admins adminFlags
	fs.Var(&admins, "admin", "name=..,subject=..,credential=..,hash=.. (repeat; at least two)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *slug == "" || *name == "" {
		return errors.New("--slug and --name are required")
	}
	if len(admins) < 2 {
		return errors.New("at least two --admin are required: every role grant needs two admins")
	}
	tenantID := uuid.New()
	if *id != "" {
		var err error
		if tenantID, err = uuid.Parse(*id); err != nil {
			return fmt.Errorf("--id: %w", err)
		}
	}
	dsn := getenv("EACP_DATABASE_URL")
	if dsn == "" {
		return errors.New("EACP_DATABASE_URL: required (schema owner DSN)")
	}
	pool, err := storage.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Tenants are not tenant-scoped rows' children: insert the tenant first
	// under its own context, then everything else in the same transaction.
	principals := map[string]uuid.UUID{}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID.String()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO eacp.tenants (id, slug, display_name) VALUES ($1, $2, $3)`,
			tenantID, *slug, *name); err != nil {
			return err
		}
		for _, a := range admins {
			var pid uuid.UUID
			if err := tx.QueryRow(ctx, `
				INSERT INTO eacp.principals (tenant_id, kind, name, subject, display_name)
				VALUES ($1, 'human', $2, $3, $2) RETURNING id`, tenantID, a.name, a.subject).Scan(&pid); err != nil {
				return fmt.Errorf("admin %s: %w", a.name, err)
			}
			principals[a.name] = pid
			if _, err := tx.Exec(ctx, `
				INSERT INTO eacp.role_grants (tenant_id, principal_id, role, approved_at)
				VALUES ($1, $2, 'admin', now())`, tenantID, pid); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO eacp.credentials (tenant_id, id, kind, principal_id, secret_hash, expires_at, approved_at)
				VALUES ($1, $2, 'pk', $3, $4, now() + interval '90 days' - interval '1 minute', now())`,
				tenantID, a.credential, pid, a.hash); err != nil {
				return err
			}
		}
		_, err := audit.Append(ctx, tx, audit.Event{
			ActorKind: audit.ActorSystem, Action: "tenant.bootstrapped", SubjectType: "tenant", SubjectID: tenantID,
			Reason: "tenant created by the schema owner (eacpctl tenant create)",
			Data:   map[string]any{"slug": *slug, "admins": principals},
		})
		return err
	})
	if err != nil {
		return fmt.Errorf("tenant create: %w", err)
	}
	fmt.Fprintf(out, "tenant %s (%s) created\n", tenantID, *slug)
	for _, a := range admins {
		fmt.Fprintf(out, "admin  %-20s principal %s\n", a.name, principals[a.name])
	}
	return nil
}
