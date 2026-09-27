package config

import (
	"strings"
	"testing"
	"time"
)

func TestProviderSecretsOnlyInTheGateway(t *testing.T) {
	file := map[string]string{"EACP_LLM_SECRETS_FILE": "/run/secrets/llm_secrets"}
	for name, opts := range map[string]Options{"api": {}, "worker": {AllowConnectorSecrets: true}} {
		if _, err := Load(env(file), opts); err == nil || !strings.Contains(err.Error(), "EACP_LLM_SECRETS_FILE") {
			t.Fatalf("%s accepted provider secrets: %v", name, err)
		}
	}
	gw := Options{AllowProviderSecrets: true}
	if _, err := Load(env(nil), gw); err == nil || !strings.Contains(err.Error(), "EACP_LLM_SECRETS_FILE") {
		t.Fatalf("the gateway started without its secrets file: %v", err)
	}
	both := map[string]string{"EACP_LLM_SECRETS_FILE": "/run/secrets/llm_secrets",
		"EACP_CONNECTOR_SECRETS_FILE": "/run/secrets/connector_secrets"}
	if _, err := Load(env(both), gw); err == nil || !strings.Contains(err.Error(), "EACP_CONNECTOR_SECRETS_FILE") {
		t.Fatalf("the gateway accepted connector secrets: %v", err)
	}
	if _, err := Load(env(nil), Options{AllowProviderSecrets: true, AllowConnectorSecrets: true}); err == nil {
		t.Fatal("a service may not hold both connector and provider secrets")
	}

	cfg, err := Load(env(file), gw)
	if err != nil || cfg.LLMSecretsFile != "/run/secrets/llm_secrets" || cfg.LLMKillPoll != 2*time.Second ||
		cfg.LLMSweepInterval != 30*time.Second || cfg.LLMMaxRequestBytes != 4<<20 || cfg.LLMID != "" {
		t.Fatalf("gateway defaults = %+v, %v", cfg, err)
	}
	cfg, err = Load(env(map[string]string{"EACP_LLM_SECRETS_FILE": "/s", "EACP_LLM_KILL_POLL": "500ms",
		"EACP_LLM_SWEEP_INTERVAL": "5m", "EACP_LLM_MAX_REQUEST_BYTES": "33554432", "EACP_LLM_ID": "gw-a"}), gw)
	if err != nil || cfg.LLMKillPoll != 500*time.Millisecond || cfg.LLMSweepInterval != 5*time.Minute ||
		cfg.LLMMaxRequestBytes != 32<<20 || cfg.LLMID != "gw-a" {
		t.Fatalf("gateway overrides = %+v, %v", cfg, err)
	}
	for name, v := range map[string]map[string]string{
		"fast poll":      {"EACP_LLM_KILL_POLL": "100ms"},
		"slow poll":      {"EACP_LLM_KILL_POLL": "31s"},
		"fast sweep":     {"EACP_LLM_SWEEP_INTERVAL": "1s"},
		"huge body":      {"EACP_LLM_MAX_REQUEST_BYTES": "33554433"},
		"no body":        {"EACP_LLM_MAX_REQUEST_BYTES": "0"},
		"bad gateway id": {"EACP_LLM_ID": "has spaces"},
	} {
		v["EACP_LLM_SECRETS_FILE"] = "/s"
		if _, err := Load(env(v), gw); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// Other services ignore the gateway's settings.
	if cfg, err := Load(env(map[string]string{"EACP_LLM_KILL_POLL": "2s"}), Options{}); err != nil || cfg.LLMKillPoll != 0 {
		t.Fatalf("api = %+v, %v", cfg, err)
	}
}
