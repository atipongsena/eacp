package registry_test

// Schema-level tests of the LLM gateway's registry objects (ADR-031,
// migration 00024): models, model allowlists and the model kill scope.

import (
	"testing"

	"github.com/google/uuid"

	"eacp/internal/registry/registrytest"
	"eacp/internal/storage/pgtest"
)

const llmModelSQL = `INSERT INTO eacp.llm_models (tenant_id, name, provider, base_url, upstream_model, secret_ref,
	max_output_tokens) VALUES (eacp.current_tenant_id(), $1, $2, $3, $4, 'llm', 4096) RETURNING id`

func TestLLMModelsAreDeclaredByEditorsAndImmutable(t *testing.T) {
	f := registrytest.New(t)
	for _, who := range []string{"alice", "otto", "rita", "carol"} {
		_, err := f.TryID(who, llmModelSQL, "sonnet", "anthropic", "https://api.anthropic.test", "claude-x")
		wantState(t, err, sqlForbidden)
	}
	id := f.ID(t, "erin", llmModelSQL, "sonnet", "anthropic", "https://api.anthropic.test", "claude-x")
	var createdBy uuid.UUID
	var timeout int
	ownerRow(t, f, `SELECT created_by, timeout_ms FROM eacp.llm_models WHERE id = $1`, []any{id}, &createdBy, &timeout)
	if createdBy != f.P["erin"] || timeout != 600000 {
		t.Fatalf("created_by %v, timeout %d", createdBy, timeout)
	}
	var audited int
	ownerRow(t, f, `SELECT count(*) FROM eacp.audit_events
		WHERE convert_from(payload, 'UTF8')::jsonb->>'action' = 'llm_models.insert'`, nil, &audited)
	if audited != 1 {
		t.Fatalf("%d audit events", audited)
	}
	wantState(t, f.Exec("erin", `UPDATE eacp.llm_models SET upstream_model = 'other' WHERE id = $1`, id), sqlForbidden)
	wantState(t, f.Exec("alice", `DELETE FROM eacp.llm_models WHERE id = $1`, id), sqlForbidden)

	for name, args := range map[string][]any{
		"duplicate name":      {"sonnet", "anthropic", "https://api.anthropic.test", "claude-y"},
		"bad name":            {"Sonnet!", "anthropic", "https://api.anthropic.test", "claude-x"},
		"unknown provider":    {"gem", "gemini", "https://api.gemini.test", "g"},
		"credentials in url":  {"a1", "openai", "https://user:pw@api.openai.test", "gpt"},
		"query in url":        {"a2", "openai", "https://api.openai.test?x=1", "gpt"},
		"fragment in url":     {"a3", "openai", "https://api.openai.test#x", "gpt"},
		"not http":            {"a4", "openai", "ftp://api.openai.test", "gpt"},
		"control in upstream": {"a5", "openai", "https://api.openai.test", "gpt\n4"},
		"empty upstream":      {"a6", "openai", "https://api.openai.test", ""},
	} {
		_, err := f.TryID("erin", llmModelSQL, args...)
		if name == "duplicate name" {
			wantState(t, err, sqlUnique)
			continue
		}
		wantState(t, err, sqlCheck)
	}
	_, err := f.TryID("erin", `INSERT INTO eacp.llm_models (tenant_id, name, provider, base_url, upstream_model,
		secret_ref, max_output_tokens) VALUES (eacp.current_tenant_id(), 'big', 'openai', 'https://x.test', 'g', 'llm',
		1000001) RETURNING id`)
	wantState(t, err, sqlCheck)
}

func TestAllowlistModelIDs(t *testing.T) {
	f := registrytest.New(t)
	a := f.NewAgent(t, "buyer")
	m1 := f.LLMModel(t, "sonnet", "anthropic", "https://api.anthropic.test", "claude-x")
	m2 := f.LLMModel(t, "gpt", "openai", "https://api.openai.test", "gpt-x")
	al := f.ID(t, "erin", `INSERT INTO eacp.agent_allowlists (tenant_id, agent_version_id, tool_ids, model_ids)
		VALUES (eacp.current_tenant_id(), $1, '{}', $2) RETURNING id`, a.Version, []uuid.UUID{m2, m1, m2})
	var got []uuid.UUID
	ownerRow(t, f, `SELECT model_ids FROM eacp.agent_allowlists WHERE id = $1`, []any{al}, &got)
	want := []uuid.UUID{m1, m2}
	if m2.String() < m1.String() {
		want = []uuid.UUID{m2, m1}
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("model_ids %v, want %v", got, want)
	}
	def := f.ID(t, "erin", `INSERT INTO eacp.agent_allowlists (tenant_id, agent_version_id, tool_ids)
		VALUES (eacp.current_tenant_id(), $1, '{}') RETURNING id`, a.Version)
	ownerRow(t, f, `SELECT model_ids FROM eacp.agent_allowlists WHERE id = $1`, []any{def}, &got)
	if len(got) != 0 {
		t.Fatalf("default model_ids %v", got)
	}
	other := f.ForTenant(t, pgtest.TenantB)
	foreign := other.LLMModel(t, "sonnet", "anthropic", "https://api.anthropic.test", "claude-x")
	for _, ids := range [][]uuid.UUID{{foreign}, {uuid.New()}} {
		_, err := f.TryID("erin", `INSERT INTO eacp.agent_allowlists (tenant_id, agent_version_id, tool_ids, model_ids)
			VALUES (eacp.current_tenant_id(), $1, '{}', $2) RETURNING id`, a.Version, ids)
		wantState(t, err, sqlForeignKey)
	}
	_, err := f.TryID("erin", `INSERT INTO eacp.agent_allowlists (tenant_id, agent_version_id, tool_ids, model_ids)
		VALUES (eacp.current_tenant_id(), $1, '{}', ARRAY[NULL]::uuid[]) RETURNING id`, a.Version)
	wantState(t, err, sqlCheck)
}

func TestKillScopeModel(t *testing.T) {
	f := registrytest.New(t)
	m := f.LLMModel(t, "sonnet", "anthropic", "https://api.anthropic.test", "claude-x")
	if err := f.Exec("otto", `SELECT eacp.set_kill('model', $1, true, 'provider incident')`, m); err != nil {
		t.Fatal(err)
	}
	var killed bool
	var epoch int64
	ownerRow(t, f, `SELECT k.killed, e.epoch FROM eacp.kill_states k
		JOIN eacp.kill_tenant_epochs e ON e.tenant_id = k.tenant_id WHERE k.scope = 'model' AND k.target_id = $1`,
		[]any{m}, &killed, &epoch)
	if !killed || epoch < 1 {
		t.Fatalf("killed %v epoch %d", killed, epoch)
	}
	wantState(t, f.Exec("otto", `SELECT eacp.set_kill('model', $1, true, 'unknown model')`, uuid.New()), sqlForeignKey)
	// Resuming takes a second operator, as for every scope.
	wantState(t, f.Exec("otto", `SELECT eacp.set_kill('model', $1, false, 'resolved')`, m), sqlForbidden)
	if err := f.Exec("opal", `SELECT eacp.set_kill('model', $1, false, 'resolved')`, m); err != nil {
		t.Fatal(err)
	}
}

func TestModelPricesCacheWrite(t *testing.T) {
	f := registrytest.New(t)
	price := `INSERT INTO eacp.model_prices (tenant_id, provider, model, unit, input_per_mtok, output_per_mtok,
		cache_write_per_mtok, reason) VALUES (eacp.current_tenant_id(), 'anthropic', $1, 'USD', 3, 15, $2, 'list price')
		RETURNING id`
	f.ID(t, "alice", price, "claude-x", "3.75")
	f.ID(t, "alice", price, "claude-y", nil)
	_, err := f.TryID("alice", price, "claude-z", "-1")
	wantState(t, err, sqlCheck)
}
