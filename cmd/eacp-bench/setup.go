package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/identity"
)

// benchTenant has the ERP and LLM entries in the development secrets
// manifests (deployments/docker/secrets).
var benchTenant = uuid.MustParse("00000000-0000-4000-8000-0000000000be")

const (
	subject       = "sam@bench.test"
	setupWorkers  = 16
	thbLimit      = "1000000000"
	usdLimit      = "1000000"
	benchPolicyV1 = `{"format_version": 1, "rules": [
		{"id": "erp", "match": {"target": "erp"}, "verdict": "allow", "reason": "benchmark"},
		{"id": "llm", "match": {"operation": "llm.generate"}, "verdict": "allow", "reason": "benchmark"}]}`
)

// agent is one registered benchmark agent; its key is in the client only.
type agent struct {
	name string
	id   string
}

type setup struct {
	o      options
	api    *client
	sam    string // sam's principal id: the agents' owner and every action's subject
	agents []agent
	mu     sync.Mutex
}

// newKey generates a key for who, keeps it in the client and returns the
// credential id and the hash to register.
func (s *setup) newKey(who string, kind identity.Kind) (uuid.UUID, string, error) {
	cred := uuid.New()
	key, hash, err := identity.NewKey(kind, benchTenant, cred)
	if err != nil {
		return uuid.Nil, "", err
	}
	s.api.setKey(who, key)
	return cred, hex.EncodeToString(hash), nil
}

// compose runs docker compose on the bench project.
func (s *setup) compose(ctx context.Context, args ...string) ([]byte, error) {
	base := []string{"compose", "-p", s.o.project}
	for _, f := range s.o.composeFiles {
		base = append(base, "-f", f)
	}
	return exec.CommandContext(ctx, "docker", append(base, args...)...).CombinedOutput()
}

// tenant creates the bench tenant with admins alice and bob (eacpctl, the
// owner's path), then erin, rita, ravi and sam; every grant and key is
// approved by the other admin.
func (s *setup) tenant(ctx context.Context) error {
	args := []string{"run", "--rm", "migrate", "/eacpctl", "tenant", "create",
		"--id", benchTenant.String(), "--slug", "bench", "--name", "Bench"}
	for _, n := range []string{"alice", "bob"} {
		cred, hash, err := s.newKey(n, identity.KindPrincipal)
		if err != nil {
			return err
		}
		args = append(args, "--admin", fmt.Sprintf("name=%s,subject=%s@bench.test,credential=%s,hash=%s", n, n, cred, hash))
	}
	if out, err := s.compose(ctx, args...); err != nil {
		return fmt.Errorf("eacpctl tenant create: %w\n%s", err, out)
	}
	for _, m := range []struct{ name, role string }{{"erin", "registry_editor"}, {"rita", "registry_approver"},
		{"ravi", "registry_approver"}, {"sam", ""}} {
		p, err := s.api.must(ctx, 201, "alice", "POST", "/v1/principals", map[string]any{"kind": "human", "name": m.name,
			"subject": m.name + "@bench.test", "display_name": m.name})
		if err != nil {
			return err
		}
		if m.role == "" {
			s.sam = id(p)
			continue
		}
		g, err := s.api.must(ctx, 201, "alice", "POST", "/v1/role-grants", map[string]any{"principal_id": id(p), "role": m.role})
		if err != nil {
			return err
		}
		if _, err := s.api.must(ctx, 204, "bob", "POST", "/v1/role-grants/"+id(g)+"/approve", nil); err != nil {
			return err
		}
		cred, hash, err := s.newKey(m.name, identity.KindPrincipal)
		if err != nil {
			return err
		}
		if _, err := s.api.must(ctx, 201, "alice", "POST", "/v1/credentials", map[string]any{"id": cred,
			"kind": identity.KindPrincipal, "principal_id": id(p), "hash": hash, "expires_in_days": 1}); err != nil {
			return err
		}
		if _, err := s.api.must(ctx, 204, "bob", "POST", "/v1/credentials/"+cred.String()+"/approve", nil); err != nil {
			return err
		}
	}
	return nil
}

// registry declares the allow policy, the ERP tool with a costed contract,
// and the fake LLM model with its price.
func (s *setup) registry(ctx context.Context) error {
	p, err := s.api.must(ctx, 201, "alice", "POST", "/v1/policies", map[string]any{"content": json.RawMessage(benchPolicyV1)})
	if err != nil {
		return err
	}
	if _, err := s.api.must(ctx, 204, "bob", "POST", "/v1/policies/"+id(p)+"/activate", map[string]any{"reason": "benchmark"}); err != nil {
		return err
	}
	conn, err := s.api.must(ctx, 201, "erin", "POST", "/v1/connectors", map[string]any{"name": "erp", "protocol": "http",
		"endpoint": "http://fakeerp:8090", "secret_ref": "fakeerp"})
	if err != nil {
		return err
	}
	tool, err := s.api.must(ctx, 201, "erin", "POST", "/v1/connectors/"+id(conn)+"/tools", map[string]any{"name": "create_po"})
	if err != nil {
		return err
	}
	c, err := s.api.must(ctx, 201, "erin", "POST", "/v1/tools/"+id(tool)+"/contracts", map[string]any{
		"side_effects": []string{"IRREVERSIBLE_WRITE", "FINANCIAL"}, "idempotency_mode": "native",
		"idempotency_key_field": "Idempotency-Key", "reconciliation_lookup": "by_operation_key",
		"reconciliation_consistency": "strong", "proof_standard": "authoritative",
		"no_effect_errors": []string{"validation"}, "max_attempts": 2, "timeout_ms": 3000,
		"cost_unit": "THB", "cost_amount_field": "amount", "cost_unit_field": "currency"})
	if err != nil {
		return err
	}
	if _, err := s.api.must(ctx, 204, "rita", "POST", "/v1/tools/"+id(tool)+"/contract", map[string]any{"contract_id": id(c)}); err != nil {
		return err
	}
	if _, err := s.api.must(ctx, 201, "erin", "POST", "/v1/llm-models", map[string]any{"name": "sonnet",
		"provider": "anthropic", "base_url": "http://fakellm:8093", "upstream_model": "claude-fake",
		"secret_ref": "fakellm", "max_output_tokens": 100000, "timeout_ms": 90000}); err != nil {
		return err
	}
	_, err = s.api.must(ctx, 201, "alice", "POST", "/v1/finops/prices", map[string]any{"provider": "anthropic",
		"model": "claude-fake", "unit": "USD", "input_per_mtok": "3", "output_per_mtok": "15", "reason": "benchmark"})
	return err
}

// grow registers agents until there are n, setupWorkers at a time, and
// stops at the first failure.
func (s *setup) grow(ctx context.Context, n int) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	names := make(chan string)
	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
	)
	for range setupWorkers {
		wg.Go(func() {
			for name := range names {
				a, err := s.register(ctx, name)
				if err != nil {
					errOnce.Do(func() { firstErr = fmt.Errorf("agent %s: %w", name, err); cancel() })
					continue
				}
				s.mu.Lock()
				s.agents = append(s.agents, a)
				s.mu.Unlock()
			}
		})
	}
	for i := len(s.agents); i < n && ctx.Err() == nil; i++ {
		names <- fmt.Sprintf("bench-%05d", i+1)
	}
	close(names)
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

// register creates one ACTIVE agent version allowed erp.create_po and the
// sonnet model, its key, and funded THB and USD budget accounts.
func (s *setup) register(ctx context.Context, name string) (agent, error) {
	a, err := s.api.must(ctx, 201, "erin", "POST", "/v1/agents", map[string]any{"name": name, "display_name": name,
		"environment": "production", "risk_class": "medium", "owner_principal_id": s.sam})
	if err != nil {
		return agent{}, err
	}
	v, err := s.api.must(ctx, 201, "erin", "POST", "/v1/agents/"+id(a)+"/versions",
		map[string]any{"runtime": "python", "code_ref": "git:bench"})
	if err != nil {
		return agent{}, err
	}
	al, err := s.api.must(ctx, 201, "erin", "POST", "/v1/agent-versions/"+id(v)+"/allowlists",
		map[string]any{"tools": []string{"erp.create_po"}, "models": []string{"sonnet"}})
	if err != nil {
		return agent{}, err
	}
	if _, err := s.api.must(ctx, 204, "rita", "POST", "/v1/agent-versions/"+id(v)+"/allowlist",
		map[string]any{"allowlist_id": id(al)}); err != nil {
		return agent{}, err
	}
	if _, err := s.api.must(ctx, 204, "ravi", "POST", "/v1/agent-versions/"+id(v)+"/transitions",
		map[string]any{"to": "ACTIVE", "reason": "benchmark"}); err != nil {
		return agent{}, err
	}
	cred, hash, err := s.newKey(name, identity.KindAgent)
	if err != nil {
		return agent{}, err
	}
	if _, err := s.api.must(ctx, 201, "erin", "POST", "/v1/credentials", map[string]any{"id": cred,
		"kind": identity.KindAgent, "agent_version_id": id(v), "hash": hash, "expires_in_days": 1}); err != nil {
		return agent{}, err
	}
	if _, err := s.api.must(ctx, 204, "rita", "POST", "/v1/credentials/"+cred.String()+"/approve", nil); err != nil {
		return agent{}, err
	}
	for _, b := range [][2]string{{"THB", thbLimit}, {"USD", usdLimit}} {
		acct, err := s.api.must(ctx, 201, "alice", "POST", "/v1/budgets", map[string]any{
			"name": name + "-" + strings.ToLower(b[0]), "unit": b[0], "agent_id": id(a)})
		if err != nil {
			return agent{}, err
		}
		raise, err := s.api.must(ctx, 201, "alice", "POST", "/v1/budgets/"+id(acct)+"/limit",
			map[string]any{"limit": b[1], "reason": "benchmark"})
		if err != nil {
			return agent{}, err
		}
		if _, err := s.api.must(ctx, 200, "bob", "POST", "/v1/budget-limit-changes/"+id(raise)+"/approve",
			map[string]any{"reason": "benchmark"}); err != nil {
			return agent{}, err
		}
	}
	return agent{name: name, id: id(a)}, nil
}
