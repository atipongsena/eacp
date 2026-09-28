package registry_test

import (
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/registry"
)

var sonnet = registry.NewLLMModel{Name: "sonnet", Provider: "anthropic", BaseURL: "https://api.anthropic.test",
	UpstreamModel: "claude-x", SecretRef: "llm", MaxOutputTokens: 4096}

func TestRegisterAndListLLMModels(t *testing.T) {
	e := newEnv(t)
	_, err := e.svc.RegisterLLMModel(e.ctx, e.as("alice"), sonnet)
	wantErr(t, err, registry.ErrForbidden)
	m := must[registry.LLMModel](t)(e.svc.RegisterLLMModel(e.ctx, e.as("erin"), sonnet))
	if m.Name != "sonnet" || m.TimeoutMS != 600000 || m.MaxOutputTokens != 4096 || m.CreatedBy != e.f.P["erin"] {
		t.Fatalf("model %+v", m)
	}
	_, err = e.svc.RegisterLLMModel(e.ctx, e.as("erin"), sonnet)
	wantErr(t, err, registry.ErrConflict)
	bad := sonnet
	bad.Name, bad.BaseURL = "leaky", "https://user:pw@api.anthropic.test"
	_, err = e.svc.RegisterLLMModel(e.ctx, e.as("erin"), bad)
	wantErr(t, err, registry.ErrInvalid)
	gpt := registry.NewLLMModel{Name: "gpt", Provider: "openai", BaseURL: "https://api.openai.test",
		UpstreamModel: "gpt-x", SecretRef: "llm", MaxOutputTokens: 8192, TimeoutMS: 30000}
	must[registry.LLMModel](t)(e.svc.RegisterLLMModel(e.ctx, e.as("erin"), gpt))
	list := must[[]registry.LLMModel](t)(e.svc.ListLLMModels(e.ctx, e.as("carol")))
	if len(list) != 2 || list[0].Name != "gpt" || list[0].TimeoutMS != 30000 || list[1].Name != "sonnet" {
		t.Fatalf("list %+v", list)
	}
}

func TestProposeAllowlistWithModels(t *testing.T) {
	e := newEnv(t)
	w := e.wire(t)
	must[registry.LLMModel](t)(e.svc.RegisterLLMModel(e.ctx, e.as("erin"), sonnet))
	al := must[uuid.UUID](t)(e.svc.ProposeAllowlist(e.ctx, e.as("erin"), w.version, []string{"erp.create_po"},
		[]string{"sonnet", "sonnet"}))
	noErr(t, e.svc.ActivateAllowlist(e.ctx, e.as("rita"), w.version, al))
	d := must[registry.AgentDetail](t)(e.svc.GetAgent(e.ctx, e.as("carol"), "procurement-bot"))
	v := d.Versions[0]
	if !slices.Equal(v.AllowedTools, []string{"erp.create_po"}) || !slices.Equal(v.AllowedModels, []string{"sonnet"}) {
		t.Fatalf("version %+v", v)
	}
	_, err := e.svc.ProposeAllowlist(e.ctx, e.as("erin"), w.version, nil, []string{"ghost"})
	wantErr(t, err, registry.ErrNotFound)
	// A version without models shows an empty list, never null.
	d2 := must[registry.AgentDetail](t)(e.svc.GetAgent(e.ctx, e.as("carol"), "procurement-bot"))
	if d2.Versions[0].AllowedModels == nil {
		t.Fatal("allowed_models is null")
	}
}
