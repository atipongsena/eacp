package identity_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/identity"
	"github.com/atipongsena/eacp/internal/registry/registrytest"
	"github.com/atipongsena/eacp/internal/storage/pgtest"
)

// issue registers a credential the way ADR-003 §5 prescribes: the holder
// generates the key, a proposer registers only its hash, a second person
// approves. It returns the key.
func issue(t *testing.T, f *registrytest.Fixture, kind identity.Kind, subject uuid.UUID, proposer, approver string) (string, uuid.UUID) {
	t.Helper()
	credID := uuid.New()
	key, hash, err := identity.NewKey(kind, f.Tenant, credID)
	if err != nil {
		t.Fatal(err)
	}
	var principal, version any
	if kind == identity.KindPrincipal {
		principal = subject
	} else {
		version = subject
	}
	f.ID(t, proposer, `INSERT INTO eacp.credentials
		(tenant_id, id, kind, principal_id, agent_version_id, secret_hash, expires_at)
		VALUES (eacp.current_tenant_id(), $1, $2, $3, $4, $5, now() + interval '30 days') RETURNING id`,
		credID, string(kind), principal, version, hash)
	if approver != "" {
		if err := f.Exec(approver, `UPDATE eacp.credentials SET approved_at = now() WHERE id = $1`, credID); err != nil {
			t.Fatalf("approve: %v", err)
		}
	}
	return key, credID
}

func wantReason(t *testing.T, err error, reason string) {
	t.Helper()
	var ae *identity.AuthError
	if !errors.As(err, &ae) || !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("err = %v, want AuthError(%s)", err, reason)
	}
	if ae.Reason != reason {
		t.Fatalf("reason = %q, want %q", ae.Reason, reason)
	}
}

func TestAuthenticatePrincipal(t *testing.T) {
	f := registrytest.New(t)
	key, credID := issue(t, f, identity.KindPrincipal, f.P["rita"], "alice", "bob")
	c, err := identity.Authenticate(context.Background(), f.App, key)
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != identity.KindPrincipal || c.TenantID != f.Tenant || c.PrincipalID != f.P["rita"] || c.CredentialID != credID {
		t.Fatalf("caller = %+v", c)
	}
	if !slices.Equal(c.Roles, []string{"registry_approver"}) || !c.HasRole("admin", "registry_approver") || c.HasRole("admin") {
		t.Fatalf("roles = %v", c.Roles)
	}
}

func TestAuthenticateAgent(t *testing.T) {
	f := registrytest.New(t)
	tool := f.ActiveTool(t, "erp", "read")
	a := f.ActiveAgent(t, "a1", tool.Tool)
	key, _ := issue(t, f, identity.KindAgent, a.Version, "erin", "rita")
	c, err := identity.Authenticate(context.Background(), f.App, key)
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != identity.KindAgent || c.AgentVersionID != a.Version || c.AgentID != a.Agent || c.AgentState != "ACTIVE" {
		t.Fatalf("caller = %+v", c)
	}
	if len(c.Roles) != 0 || c.HasRole("admin") {
		t.Fatal("agents hold no roles")
	}
}

func TestSuspendedAgentStillAuthenticatesWithItsState(t *testing.T) {
	f := registrytest.New(t)
	tool := f.ActiveTool(t, "erp", "read")
	a := f.ActiveAgent(t, "a1", tool.Tool)
	key, _ := issue(t, f, identity.KindAgent, a.Version, "erin", "rita")
	if err := f.Exec("otto", `UPDATE eacp.agent_versions SET state = 'SUSPENDED', state_reason = 'x' WHERE id = $1`, a.Version); err != nil {
		t.Fatal(err)
	}
	c, err := identity.Authenticate(context.Background(), f.App, key)
	if err != nil || c.AgentState != "SUSPENDED" {
		t.Fatalf("caller = %+v, err = %v", c, err)
	}
}

func TestAuthenticationFailures(t *testing.T) {
	f := registrytest.New(t)
	ctx := context.Background()
	tool := f.ActiveTool(t, "erp", "read")
	a := f.ActiveAgent(t, "a1", tool.Tool)

	t.Run("malformed", func(t *testing.T) {
		_, err := identity.Authenticate(ctx, f.App, "Bearer nonsense")
		wantReason(t, err, "malformed")
	})
	t.Run("unknown credential", func(t *testing.T) {
		key, _, _ := identity.NewKey(identity.KindPrincipal, f.Tenant, uuid.New())
		_, err := identity.Authenticate(ctx, f.App, key)
		wantReason(t, err, "unknown_credential")
	})
	t.Run("wrong secret", func(t *testing.T) {
		_, credID := issue(t, f, identity.KindPrincipal, f.P["carol"], "alice", "bob")
		forged, _, _ := identity.NewKey(identity.KindPrincipal, f.Tenant, credID)
		_, err := identity.Authenticate(ctx, f.App, forged)
		wantReason(t, err, "secret_mismatch")
	})
	t.Run("wrong tenant", func(t *testing.T) {
		_, credID := issue(t, f, identity.KindPrincipal, f.P["carol"], "alice", "bob")
		other, _, _ := identity.NewKey(identity.KindPrincipal, uuid.MustParse(pgtest.TenantB), credID)
		_, err := identity.Authenticate(ctx, f.App, other)
		wantReason(t, err, "unknown_credential")
	})
	t.Run("kind mismatch", func(t *testing.T) {
		credID := uuid.New()
		key, hash, _ := identity.NewKey(identity.KindAgent, f.Tenant, credID)
		f.ID(t, "alice", `INSERT INTO eacp.credentials (tenant_id, id, kind, principal_id, secret_hash, expires_at)
			VALUES (eacp.current_tenant_id(), $1, 'pk', $2, $3, now() + interval '1 day') RETURNING id`, credID, f.P["carol"], hash)
		f.Exec("bob", `UPDATE eacp.credentials SET approved_at = now() WHERE id = $1`, credID)
		_, err := identity.Authenticate(ctx, f.App, key)
		wantReason(t, err, "kind_mismatch")
	})
	t.Run("unapproved", func(t *testing.T) {
		key, _ := issue(t, f, identity.KindPrincipal, f.P["carol"], "alice", "")
		_, err := identity.Authenticate(ctx, f.App, key)
		wantReason(t, err, "unapproved")
	})
	t.Run("revoked", func(t *testing.T) {
		key, credID := issue(t, f, identity.KindPrincipal, f.P["carol"], "alice", "bob")
		f.Exec("alice", `UPDATE eacp.credentials SET revoked_at = now(), revoke_reason = 'lost' WHERE id = $1`, credID)
		_, err := identity.Authenticate(ctx, f.App, key)
		wantReason(t, err, "revoked")
	})
	t.Run("expired", func(t *testing.T) {
		key, credID := issue(t, f, identity.KindPrincipal, f.P["carol"], "alice", "bob")
		admin, err := pgx.Connect(ctx, f.DB.AdminDSN)
		if err != nil {
			t.Fatal(err)
		}
		defer admin.Close(ctx)
		// Time travel: only a superuser with triggers disabled can do this.
		admin.Exec(ctx, "SET session_replication_role = replica")
		if _, err := admin.Exec(ctx, `UPDATE eacp.credentials SET expires_at = now() - interval '1 second' WHERE id = $1`, credID); err != nil {
			t.Fatal(err)
		}
		_, err = identity.Authenticate(ctx, f.App, key)
		wantReason(t, err, "expired")
	})
	t.Run("disabled principal", func(t *testing.T) {
		key, _ := issue(t, f, identity.KindPrincipal, f.P["audra"], "alice", "bob")
		f.Exec("alice", `UPDATE eacp.principals SET disabled_at = now(), disable_reason = 'left' WHERE id = $1`, f.P["audra"])
		_, err := identity.Authenticate(ctx, f.App, key)
		wantReason(t, err, "principal_disabled")
	})
	t.Run("terminal agent version", func(t *testing.T) {
		key, _ := issue(t, f, identity.KindAgent, a.Version, "erin", "rita")
		f.Exec("rita", `UPDATE eacp.agent_versions SET state = 'RETIRED', state_reason = 'eol' WHERE id = $1`, a.Version)
		_, err := identity.Authenticate(ctx, f.App, key)
		wantReason(t, err, "agent_version_terminal")
	})
}

func TestAuthErrorMessageIsGeneric(t *testing.T) {
	err := &identity.AuthError{Reason: "secret_mismatch"}
	if err.Error() != "identity: authentication failed" {
		t.Fatalf("Error() = %q leaks detail", err.Error())
	}
}
