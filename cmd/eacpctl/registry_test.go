package main

import (
	"context"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/api"
	"eacp/internal/audit"
	"eacp/internal/identity"
	"eacp/internal/registry/registrytest"
	"eacp/internal/storage"
	"eacp/internal/storage/pgtest"
)

var (
	keyLine  = regexp.MustCompile(`(?m)^key:\s+(\S+)$`)
	credLine = regexp.MustCompile(`(?m)^credential_id:\s+(\S+)$`)
	hashLine = regexp.MustCompile(`(?m)^hash:\s+(\S+)$`)
)

type generated struct{ key, cred, hash string }

func generate(t *testing.T, kind, tenant string) generated {
	t.Helper()
	out, err := runWith(t, nil, "key", "generate", "--kind", kind, "--tenant", tenant)
	if err != nil {
		t.Fatalf("key generate: %v", err)
	}
	g := generated{}
	for _, m := range []struct {
		re  *regexp.Regexp
		dst *string
	}{{keyLine, &g.key}, {credLine, &g.cred}, {hashLine, &g.hash}} {
		sub := m.re.FindStringSubmatch(out)
		if sub == nil {
			t.Fatalf("key generate output lacks %v:\n%s", m.re, out)
		}
		*m.dst = sub[1]
	}
	return g
}

func TestKeyGenerateOutputsMatchingKeyIdAndHash(t *testing.T) {
	tenant := uuid.NewString()
	g := generate(t, "principal", tenant)
	p, err := identity.ParseKey(g.key)
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := hex.DecodeString(g.hash)
	if p.TenantID.String() != tenant || p.CredentialID.String() != g.cred || !p.Matches(hash) || p.Kind != identity.KindPrincipal {
		t.Fatalf("inconsistent output: %+v", g)
	}
	if _, err := runWith(t, nil, "key", "generate", "--kind", "root", "--tenant", tenant); err == nil {
		t.Fatal("unknown kind accepted")
	}
	if _, err := runWith(t, nil, "key", "generate", "--kind", "agent", "--tenant", "nope"); err == nil {
		t.Fatal("invalid tenant accepted")
	}
}

func TestTenantCreateBootstrapsTwoAdmins(t *testing.T) {
	db := pgtest.New(t)
	env := map[string]string{"EACP_DATABASE_URL": db.OwnerDSN}
	if _, err := runWith(t, env, "migrate", "up"); err != nil {
		t.Fatal(err)
	}
	tenant := uuid.NewString()
	a, b := generate(t, "principal", tenant), generate(t, "principal", tenant)
	admin := func(name string, g generated) string {
		return "name=" + name + ",subject=" + name + "@acme.test,credential=" + g.cred + ",hash=" + g.hash
	}

	if _, err := runWith(t, env, "tenant", "create", "--id", tenant, "--slug", "acme", "--name", "Acme",
		"--admin", admin("alice", a)); err == nil || !strings.Contains(err.Error(), "two") {
		t.Fatalf("single admin: err = %v, want refusal", err)
	}
	if _, err := runWith(t, env, "tenant", "create", "--id", tenant, "--slug", "acme", "--name", "Acme",
		"--admin", admin("alice", a), "--admin", "name=alice2,subject=alice@acme.test,credential="+b.cred+",hash="+b.hash); err == nil {
		t.Fatal("duplicate subject accepted")
	}
	out, err := runWith(t, env, "tenant", "create", "--id", tenant, "--slug", "acme", "--name", "Acme",
		"--admin", admin("alice", a), "--admin", admin("bob", b))
	if err != nil {
		t.Fatalf("tenant create: %v", err)
	}
	if !strings.Contains(out, tenant) {
		t.Fatalf("output lacks tenant id: %s", out)
	}

	app := pgtest.Pool(t, db.AppDSN)
	for _, g := range []generated{a, b} {
		c, err := identity.Authenticate(context.Background(), app, g.key)
		if err != nil || !c.HasRole("admin") {
			t.Fatalf("bootstrapped admin key: %+v %v", c, err)
		}
	}
	var res audit.Result
	err = storage.InTenantReadTx(context.Background(), app, tenant, func(tx pgx.Tx) error {
		var err error
		res, err = audit.Verify(context.Background(), tx)
		return err
	})
	// Per admin: principal, grant and credential rows (audited by the
	// database), plus the tenant.bootstrapped event.
	if err != nil || res.Count != 7 {
		t.Fatalf("bootstrap audit: count=%d err=%v, want 7 verified events", res.Count, err)
	}
}

func apiEnv(t *testing.T) (*registrytest.Fixture, func(who string) map[string]string) {
	t.Helper()
	f := registrytest.New(t)
	mux := http.NewServeMux()
	api.New(f.App, slog.New(slog.NewTextHandler(io.Discard, nil))).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, func(who string) map[string]string {
		credID := uuid.New()
		key, hash, _ := identity.NewKey(identity.KindPrincipal, f.Tenant, credID)
		f.ID(t, "alice", `INSERT INTO eacp.credentials (tenant_id, id, kind, principal_id, secret_hash, expires_at)
			VALUES (eacp.current_tenant_id(), $1, 'pk', $2, $3, now() + interval '1 day') RETURNING id`, credID, f.P[who], hash)
		if err := f.Exec("bob", `UPDATE eacp.credentials SET approved_at = now() WHERE id = $1`, credID); err != nil {
			t.Fatal(err)
		}
		return map[string]string{"EACP_API_URL": srv.URL, "EACP_API_KEY": key}
	}
}

func TestAgentAndConnectorCommands(t *testing.T) {
	f, keyFor := apiEnv(t)
	erin := keyFor("erin")

	out, err := runWith(t, erin, "connector", "register", "--name", "erp", "--endpoint", "http://fakeerp:8090", "--secret-ref", "erp")
	if err != nil || !strings.Contains(out, `"name": "erp"`) {
		t.Fatalf("connector register: %v\n%s", err, out)
	}
	out, err = runWith(t, erin, "agent", "register", "--name", "procurement-bot", "--display-name", "Procurement bot",
		"--env", "production", "--risk", "high", "--owner-principal", f.P["carol"].String())
	if err != nil || !strings.Contains(out, `"name": "procurement-bot"`) {
		t.Fatalf("agent register: %v\n%s", err, out)
	}
	out, err = runWith(t, erin, "agent", "list")
	if err != nil || !strings.Contains(out, "procurement-bot") {
		t.Fatalf("agent list: %v\n%s", err, out)
	}
	out, err = runWith(t, erin, "agent", "inspect", "procurement-bot")
	if err != nil || !strings.Contains(out, `"versions"`) {
		t.Fatalf("agent inspect: %v\n%s", err, out)
	}
	out, err = runWith(t, erin, "api", "GET", "/v1/connectors")
	if err != nil || !strings.Contains(out, "fakeerp") {
		t.Fatalf("api GET: %v\n%s", err, out)
	}
}

func TestCommandsSurfaceHTTPErrors(t *testing.T) {
	f, keyFor := apiEnv(t)
	_, err := runWith(t, keyFor("carol"), "agent", "register", "--name", "x1", "--display-name", "x",
		"--env", "production", "--risk", "low", "--owner-principal", f.P["carol"].String())
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v, want HTTP 403", err)
	}
	if _, err := runWith(t, map[string]string{}, "agent", "list"); err == nil || !strings.Contains(err.Error(), "EACP_API_KEY") {
		t.Fatalf("err = %v, want missing EACP_API_KEY", err)
	}
	if _, err := runWith(t, keyFor("erin"), "agent", "register", "--name", "x2"); err == nil {
		t.Fatal("missing owner accepted by the CLI")
	}
}
