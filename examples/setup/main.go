// Command setup prepares the examples tenant on a running `docker compose`
// stack: people with their roles, a policy, the Fake ERP connector, the
// fake LLM model, one agent and the Agent Studio cast. It writes their keys to examples/.env and
// never prints one. Run it through examples/setup.sh.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/identity"
)

// tenant has Fake ERP and Fake LLM credentials in the development secrets
// manifests (deployments/docker/secrets).
var tenant = uuid.MustParse("00000000-0000-4000-8000-0000000000e0")

const policy = `{"format_version": 1, "rules": [
	{"id": "high-value", "match": {"operation": "purchase_high_value"}, "verdict": "escalate",
	 "reason": "a high-value purchase needs two approvers",
	 "approval": {"quorum": 2, "eligible_roles": ["approver"], "ttl_seconds": 3600}},
	{"id": "routine", "match": {"target": "erp"}, "verdict": "allow", "reason": "a routine purchase"},
	{"id": "hr-lookups", "match": {"target": "hr"}, "verdict": "allow", "reason": "a read-only HR lookup"},
	{"id": "llm", "match": {"operation": "llm.generate"}, "verdict": "allow", "reason": "model use"}]}`

// keyDays is how long the people's and the agent's keys last: as long as
// the admins' keys from eacpctl tenant create.
const keyDays = 90

// people are the tenant's members after alice and bob, the admins.
var people = []struct{ name, role, env string }{
	{"erin", "registry_editor", "EDITOR_KEY"},
	{"rita", "registry_approver", "REGISTRY_APPROVER_KEY"},
	{"ravi", "registry_approver", ""},
	{"otto", "operator", "OPERATOR_KEY"},
	{"olga", "operator", "OPERATOR2_KEY"},
	{"amy", "approver", "APPROVER_KEY"},
	{"ben", "approver", "APPROVER2_KEY"},
	{"sam", "", ""},                                  // owns the agent and is every action's subject
	{"stella", "studio_author", "STUDIO_AUTHOR_KEY"}, // saves Studio agents in the HR group
}

// requiredKeys are the keys a reusable examples/.env must hold: one written
// before a newer setup step is refused with the reset instructions.
var requiredKeys = []string{"ADMIN_KEY", "AGENT_KEY", "STUDIO_AUTHOR_KEY", "STUDIO_RUNTIME_KEY"}

type setup struct {
	api   string
	http  *http.Client
	keys  map[string]string // who → key, never printed
	env   map[string]string // what examples/.env holds
	ids   map[string]string
	root  string
	stdio io.Writer
}

func main() {
	api := flag.String("api", envOr("EACP_API", "http://127.0.0.1:8080"), "the control plane's URL")
	gateway := flag.String("gateway", envOr("EACP_GATEWAY", "http://127.0.0.1:8083"), "the LLM gateway's URL")
	flag.Parse()
	root, err := repoRoot()
	if err != nil {
		fail(err)
	}
	s := &setup{api: strings.TrimRight(*api, "/"), http: &http.Client{Timeout: 30 * time.Second},
		keys: map[string]string{}, env: map[string]string{}, ids: map[string]string{}, root: root, stdio: os.Stdout}
	ctx := context.Background()
	envPath := filepath.Join(root, "examples", ".env")
	if done, err := s.alreadySetUp(ctx, envPath); err != nil {
		fail(err)
	} else if done {
		fmt.Println("examples: already set up (examples/.env works); nothing to do")
		return
	}
	s.env["EACP_API"] = s.api
	s.env["EACP_GATEWAY"] = strings.TrimRight(*gateway, "/")
	s.env["TENANT_ID"] = tenant.String()
	s.env["SUBJECT"] = "sam@examples.test"
	for _, step := range []struct {
		what string
		fn   func(context.Context) error
	}{
		{"tenant, admins and people", s.tenant},
		{"policy", s.policy},
		{"Fake ERP connector and fake LLM model", s.registry},
		{"agent procurement-bot", s.agent},
		{"Agent Studio: the HR group, the runtime's principal and the HR tool", s.studio},
	} {
		fmt.Printf("examples: %s\n", step.what)
		if err := step.fn(ctx); err != nil {
			fail(stepError(step.what, err))
		}
	}
	if err := writeEnv(envPath, s.env); err != nil {
		fail(err)
	}
	fmt.Println("examples: ready; keys are in examples/.env (git-ignored, never printed)")
}

// resetHint is how to start over: the admin keys exist only in
// examples/.env, so a tenant without a working .env cannot be reused.
const resetHint = "start over with `docker compose down -v && docker compose up -d --build --wait`, " +
	"delete examples/.env and run examples/setup.sh again"

// stepError reports a failed step. .env is written only once every step
// has succeeded, so the tenant a failed run leaves behind has no keys.
func stepError(what string, err error) error {
	return fmt.Errorf("%s: %w\n%s", what, err, resetHint)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "examples setup:", err)
	os.Exit(1)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("run from inside the repository")
		}
		dir = parent
	}
}

// alreadySetUp reports whether .env exists and every key in it still
// works: people and the agent sign in with /v1/me and /v1/agent/self.
func (s *setup) alreadySetUp(ctx context.Context, path string) (bool, error) {
	env, err := readEnv(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	names := make([]string, 0, len(env))
	for k := range env {
		if strings.HasSuffix(k, "_KEY") {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return false, fmt.Errorf("examples/.env holds no key; %s", resetHint)
	}
	for _, k := range requiredKeys {
		if env[k] == "" {
			return false, fmt.Errorf("examples/.env has no %s: it was written by an older setup; %s", k, resetHint)
		}
	}
	for _, k := range names {
		route := "/v1/me"
		if k == "AGENT_KEY" {
			route = "/v1/agent/self"
		}
		s.keys[k] = env[k]
		code, _, err := s.call(ctx, k, "GET", route, nil)
		if err != nil {
			return false, err
		}
		if code != http.StatusOK {
			return false, fmt.Errorf("examples/.env exists but its %s is not accepted (HTTP %d): the stack was reset "+
				"or the key expired; %s", k, code, resetHint)
		}
	}
	return true, nil
}

// newKey generates who's key and returns the credential id and the hash
// to register. The key itself stays in memory and .env.
func (s *setup) newKey(who string, kind identity.Kind) (uuid.UUID, string, error) {
	cred := uuid.New()
	key, hash, err := identity.NewKey(kind, tenant, cred)
	if err != nil {
		return uuid.Nil, "", err
	}
	s.keys[who] = key
	return cred, hex.EncodeToString(hash), nil
}

func (s *setup) tenant(ctx context.Context) error {
	args := []string{"compose", "run", "--rm", "migrate", "/eacpctl", "tenant", "create",
		"--id", tenant.String(), "--slug", "examples", "--name", "Examples"}
	for _, n := range []string{"alice", "bob"} {
		cred, hash, err := s.newKey(n, identity.KindPrincipal)
		if err != nil {
			return err
		}
		args = append(args, "--admin", fmt.Sprintf("name=%s,subject=%s@examples.test,credential=%s,hash=%s", n, n, cred, hash))
	}
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = s.root
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("eacpctl tenant create: %w\n%s", err, out)
	}
	s.env["ADMIN_KEY"], s.env["ADMIN2_KEY"] = s.keys["alice"], s.keys["bob"]
	for _, m := range people {
		p, err := s.must(ctx, 201, "alice", "POST", "/v1/principals", map[string]any{"kind": "human", "name": m.name,
			"subject": m.name + "@examples.test", "display_name": strings.ToUpper(m.name[:1]) + m.name[1:]})
		if err != nil {
			return err
		}
		s.ids[m.name] = id(p)
		if m.role == "" {
			continue
		}
		g, err := s.must(ctx, 201, "alice", "POST", "/v1/role-grants", map[string]any{"principal_id": id(p), "role": m.role})
		if err != nil {
			return err
		}
		if _, err := s.must(ctx, 204, "bob", "POST", "/v1/role-grants/"+id(g)+"/approve", nil); err != nil {
			return err
		}
		cred, hash, err := s.newKey(m.name, identity.KindPrincipal)
		if err != nil {
			return err
		}
		if _, err := s.must(ctx, 201, "alice", "POST", "/v1/credentials", map[string]any{"id": cred,
			"kind": identity.KindPrincipal, "principal_id": id(p), "hash": hash, "expires_in_days": keyDays}); err != nil {
			return err
		}
		if _, err := s.must(ctx, 204, "bob", "POST", "/v1/credentials/"+cred.String()+"/approve", nil); err != nil {
			return err
		}
		if m.env != "" {
			s.env[m.env] = s.keys[m.name]
		}
	}
	return nil
}

func (s *setup) policy(ctx context.Context) error {
	p, err := s.must(ctx, 201, "alice", "POST", "/v1/policies", map[string]any{"content": json.RawMessage(policy)})
	if err != nil {
		return err
	}
	_, err = s.must(ctx, 204, "bob", "POST", "/v1/policies/"+id(p)+"/activate", map[string]any{"reason": "examples"})
	return err
}

func (s *setup) registry(ctx context.Context) error {
	conn, err := s.must(ctx, 201, "erin", "POST", "/v1/connectors", map[string]any{"name": "erp", "protocol": "http",
		"endpoint": "http://fakeerp:8090", "secret_ref": "fakeerp"})
	if err != nil {
		return err
	}
	// create_po has an authoritative lookup (example 01). create_po_eventual
	// is visible late and only best effort, so an unknown outcome of it goes
	// to a human (scripts/screenshots.sh).
	cost := map[string]any{"cost_unit": "THB", "cost_amount_field": "amount", "cost_unit_field": "currency"}
	contracts := []struct {
		name     string
		contract map[string]any
	}{
		{"create_po", map[string]any{"side_effects": []string{"IRREVERSIBLE_WRITE", "FINANCIAL"},
			"idempotency_mode": "native", "idempotency_key_field": "Idempotency-Key",
			"reconciliation_lookup": "by_operation_key", "reconciliation_consistency": "strong",
			"proof_standard": "authoritative", "no_effect_errors": []string{"validation"}, "max_attempts": 2,
			"timeout_ms": 3000}},
		{"create_po_eventual", map[string]any{"side_effects": []string{"IRREVERSIBLE_WRITE", "FINANCIAL"},
			"idempotency_mode": "correlation_only", "correlation_field": "external_reference",
			"reconciliation_lookup": "by_operation_key", "reconciliation_consistency": "eventual",
			"proof_standard": "best_effort", "no_effect_errors": []string{"validation"}, "max_attempts": 1,
			"timeout_ms": 2000}},
	}
	for _, t := range contracts {
		tool, err := s.must(ctx, 201, "erin", "POST", "/v1/connectors/"+id(conn)+"/tools", map[string]any{"name": t.name})
		if err != nil {
			return err
		}
		for k, v := range cost {
			t.contract[k] = v
		}
		c, err := s.must(ctx, 201, "erin", "POST", "/v1/tools/"+id(tool)+"/contracts", t.contract)
		if err != nil {
			return err
		}
		if _, err := s.must(ctx, 204, "rita", "POST", "/v1/tools/"+id(tool)+"/contract", map[string]any{"contract_id": id(c)}); err != nil {
			return err
		}
		s.env["TOOL_"+strings.ToUpper(t.name)+"_ID"] = id(tool)
	}
	s.env["ERP_CONNECTOR_ID"] = id(conn)
	// Two models; the agent's allowlist names only sonnet (example 02).
	for _, name := range []string{"sonnet", "opus"} {
		if _, err := s.must(ctx, 201, "erin", "POST", "/v1/llm-models", map[string]any{"name": name,
			"provider": "anthropic", "base_url": "http://fakellm:8093", "upstream_model": "claude-fake",
			"secret_ref": "fakellm", "max_output_tokens": 4096, "timeout_ms": 60000}); err != nil {
			return err
		}
	}
	_, err = s.must(ctx, 201, "alice", "POST", "/v1/finops/prices", map[string]any{"provider": "anthropic",
		"model": "claude-fake", "unit": "USD", "input_per_mtok": "3", "output_per_mtok": "15", "reason": "examples"})
	return err
}

func (s *setup) agent(ctx context.Context) error {
	a, err := s.must(ctx, 201, "erin", "POST", "/v1/agents", map[string]any{"name": "procurement-bot",
		"display_name": "Procurement bot", "environment": "production", "risk_class": "high",
		"owner_principal_id": s.ids["sam"]})
	if err != nil {
		return err
	}
	v, err := s.must(ctx, 201, "erin", "POST", "/v1/agents/"+id(a)+"/versions",
		map[string]any{"runtime": "python", "code_ref": "git:examples"})
	if err != nil {
		return err
	}
	al, err := s.must(ctx, 201, "erin", "POST", "/v1/agent-versions/"+id(v)+"/allowlists",
		map[string]any{"tools": []string{"erp.create_po", "erp.create_po_eventual"}, "models": []string{"sonnet"}})
	if err != nil {
		return err
	}
	if _, err := s.must(ctx, 204, "rita", "POST", "/v1/agent-versions/"+id(v)+"/allowlist",
		map[string]any{"allowlist_id": id(al)}); err != nil {
		return err
	}
	if _, err := s.must(ctx, 204, "ravi", "POST", "/v1/agent-versions/"+id(v)+"/transitions",
		map[string]any{"to": "ACTIVE", "reason": "examples"}); err != nil {
		return err
	}
	cred, hash, err := s.newKey("agent", identity.KindAgent)
	if err != nil {
		return err
	}
	if _, err := s.must(ctx, 201, "erin", "POST", "/v1/credentials", map[string]any{"id": cred,
		"kind": identity.KindAgent, "agent_version_id": id(v), "hash": hash, "expires_in_days": keyDays}); err != nil {
		return err
	}
	if _, err := s.must(ctx, 204, "rita", "POST", "/v1/credentials/"+cred.String()+"/approve", nil); err != nil {
		return err
	}
	for _, b := range [][2]string{{"THB", "10000000"}, {"USD", "1000"}} {
		acct, err := s.must(ctx, 201, "alice", "POST", "/v1/budgets", map[string]any{
			"name": "procurement-bot-" + strings.ToLower(b[0]), "unit": b[0], "agent_id": id(a)})
		if err != nil {
			return err
		}
		raise, err := s.must(ctx, 201, "alice", "POST", "/v1/budgets/"+id(acct)+"/limit",
			map[string]any{"limit": b[1], "reason": "examples"})
		if err != nil {
			return err
		}
		if _, err := s.must(ctx, 200, "bob", "POST", "/v1/budget-limit-changes/"+id(raise)+"/approve",
			map[string]any{"reason": "examples"}); err != nil {
			return err
		}
	}
	s.env["AGENT_KEY"] = s.keys["agent"]
	s.env["AGENT_ID"] = id(a)
	s.env["AGENT_VERSION_ID"] = id(v)
	return nil
}

// studio sets up Agent Studio (ADR-033) for scripts/screenshots.sh: stella
// (studio_author) in the HR group, the agent runtime's service principal
// holding studio_runtime alone, and the HR MCP server's get_leave_balance,
// discovered by the worker and certified read-only with results kept for
// ten minutes. The runtime itself is started by the script that needs it.
func (s *setup) studio(ctx context.Context) error {
	g, err := s.must(ctx, 201, "alice", "POST", "/v1/groups", map[string]any{"name": "hr", "display_name": "HR"})
	if err != nil {
		return err
	}
	if _, err := s.must(ctx, 201, "alice", "POST", "/v1/groups/"+id(g)+"/members",
		map[string]any{"principal_id": s.ids["stella"]}); err != nil {
		return err
	}
	p, err := s.must(ctx, 201, "alice", "POST", "/v1/principals", map[string]any{"kind": "service",
		"name": "studio-runtime", "display_name": "Agent runtime"})
	if err != nil {
		return err
	}
	grant, err := s.must(ctx, 201, "alice", "POST", "/v1/role-grants", map[string]any{"principal_id": id(p), "role": "studio_runtime"})
	if err != nil {
		return err
	}
	if _, err := s.must(ctx, 204, "bob", "POST", "/v1/role-grants/"+id(grant)+"/approve", nil); err != nil {
		return err
	}
	cred, hash, err := s.newKey("studio-runtime", identity.KindPrincipal)
	if err != nil {
		return err
	}
	if _, err := s.must(ctx, 201, "alice", "POST", "/v1/credentials", map[string]any{"id": cred,
		"kind": identity.KindPrincipal, "principal_id": id(p), "hash": hash, "expires_in_days": keyDays}); err != nil {
		return err
	}
	if _, err := s.must(ctx, 204, "bob", "POST", "/v1/credentials/"+cred.String()+"/approve", nil); err != nil {
		return err
	}
	conn, err := s.must(ctx, 201, "erin", "POST", "/v1/connectors", map[string]any{"name": "hr-mcp", "protocol": "mcp",
		"endpoint": "http://fakemcp-hr:8091/mcp", "secret_ref": "hr-mcp"})
	if err != nil {
		return err
	}
	tool, err := s.discovered(ctx, id(conn))
	if err != nil {
		return err
	}
	def, _ := tool["definition"].(map[string]any)
	c, err := s.must(ctx, 201, "erin", "POST", "/v1/tools/"+id(tool)+"/contracts", map[string]any{"definition_id": def["id"],
		"side_effects": []string{"READ_ONLY"}, "idempotency_mode": "none", "reconciliation_lookup": "none",
		"reconciliation_consistency": "none", "proof_standard": "none", "no_effect_errors": []string{"definition_changed"},
		"max_attempts": 1, "result_retention_seconds": 600})
	if err != nil {
		return err
	}
	if _, err := s.must(ctx, 204, "rita", "POST", "/v1/tools/"+id(tool)+"/contract", map[string]any{"contract_id": id(c)}); err != nil {
		return err
	}
	s.env["STUDIO_RUNTIME_KEY"] = s.keys["studio-runtime"]
	s.env["HR_GROUP_ID"] = id(g)
	return nil
}

// discovered waits for the worker's scanner to record connector's tool.
func (s *setup) discovered(ctx context.Context, connector string) (map[string]any, error) {
	deadline := time.Now().Add(90 * time.Second)
	for {
		out, err := s.must(ctx, 200, "erin", "GET", "/v1/connectors/"+connector+"/tools", nil)
		if err != nil {
			return nil, err
		}
		if tools, _ := out["tools"].([]any); len(tools) == 1 {
			if t, _ := tools[0].(map[string]any); t != nil && t["definition"] != nil {
				return t, nil
			}
		}
		if time.Now().After(deadline) {
			return nil, errors.New("the worker did not discover the HR MCP tool within 90 s (is fakemcp-hr running?)")
		}
		time.Sleep(time.Second)
	}
}

// call sends one API request as who.
func (s *setup) call(ctx context.Context, who, method, path string, body any) (int, map[string]any, error) {
	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.api+path, r)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.keys[who])
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w (is `docker compose up -d --build --wait` running?)", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, nil
}

func (s *setup) must(ctx context.Context, want int, who, method, path string, body any) (map[string]any, error) {
	code, out, err := s.call(ctx, who, method, path, body)
	if err != nil {
		return nil, err
	}
	if code != want {
		return nil, fmt.Errorf("%s %s as %s = %d, want %d: %v", method, path, who, code, want, out["error"])
	}
	return out, nil
}

func id(m map[string]any) string { s, _ := m["id"].(string); return s }

func readEnv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	env := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "="); ok && !strings.HasPrefix(k, "#") {
			env[k] = v
		}
	}
	return env, sc.Err()
}

// writeEnv writes env as KEY=value lines, readable by the owner only
// where the file system allows it.
func writeEnv(path string, env map[string]string) error {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("# Written by examples/setup.sh. Holds keys: never commit or share it.\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, env[k])
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}
