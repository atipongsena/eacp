package bundle_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"eacp/internal/registry"
)

// A bundle does not declare models (ADR-031): a new allowlist it proposes for
// an existing version keeps the model grants of the version's active one.
func TestBundleKeepsModelGrants(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()
	apply(t, f, s, ledgerDoc)
	reg := registry.New(f.App)
	m, err := reg.RegisterLLMModel(ctx, as(f, "erin"), registry.NewLLMModel{Name: "sonnet", Provider: "anthropic",
		BaseURL: "https://api.anthropic.test", UpstreamModel: "claude-x", SecretRef: "llm", MaxOutputTokens: 4096})
	ok(t, err)
	d, err := reg.GetAgent(ctx, as(f, "erin"), "ledger-bot")
	ok(t, err)
	version := d.Versions[0].ID
	al, err := reg.ProposeAllowlist(ctx, as(f, "erin"), version, []string{"ledger.post_entry"}, []string{"sonnet"})
	ok(t, err)
	ok(t, reg.ActivateAllowlist(ctx, as(f, "rita"), version, al))

	doc := strings.Replace(ledgerDoc, `"tools": {"post_entry": {`, `"tools": {"get_entry": {"contract": {
     "side_effects": ["READ_ONLY"], "idempotency_mode": "none", "reconciliation_lookup": "none",
     "reconciliation_consistency": "none", "proof_standard": "none", "max_attempts": 3}}, "post_entry": {`, 1)
	doc = strings.Replace(doc, `"allowlist": ["ledger.post_entry"]`, `"allowlist": ["ledger.post_entry", "ledger.get_entry"]`, 1)
	p, err := s.Plan(ctx, as(f, "erin"), req(doc))
	ok(t, err)
	var models []string
	for _, st := range p.Steps {
		var pl struct {
			Models []string `json:"models"`
		}
		raw, _ := json.Marshal(st.Payload)
		_ = json.Unmarshal(raw, &pl)
		if len(pl.Models) > 0 {
			models = pl.Models
		}
	}
	if len(models) != 1 || models[0] != "sonnet" {
		t.Fatalf("the plan does not carry the model grants: %+v", p.Steps)
	}
	_, err = s.Submit(ctx, as(f, "erin"), p.ID)
	ok(t, err)
	_, err = s.Approve(ctx, as(f, "rita"), p.ID)
	ok(t, err)
	if n := count(t, f, `SELECT count(*) FROM eacp.agent_versions v JOIN eacp.agent_allowlists al
		ON al.id = v.active_allowlist_id WHERE v.id = $1 AND al.model_ids = ARRAY[$2]::uuid[]
		AND cardinality(al.tool_ids) = 2`, version, m.ID); n != 1 {
		t.Fatal("the new allowlist lost the model grant")
	}
}
