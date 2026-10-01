package config

import "testing"

func TestRuntimeLLMOriginIsOptionalAndBounded(t *testing.T) {
	base := map[string]string{"EACP_API_URL": "http://api:8080", "EACP_RUNTIME_KEY_FILE": "/keys", "EACP_STUDIO_MASTER_FILE": "/master"}
	for _, origin := range []string{"", "http://llm-gateway:8083", "https://gateway.example.test/"} {
		base["EACP_RUNTIME_LLM_URL"] = origin
		cfg, err := Load(env(base), Options{StudioRuntime: true})
		if err != nil || (origin != "" && cfg.RuntimeLLMURL == "") {
			t.Fatalf("origin %q: %v", origin, err)
		}
	}
	for _, origin := range []string{"ftp://gateway", "http://gateway/v1", "http://u:p@gateway", "http://gateway?token=x", "http://gateway#x"} {
		base["EACP_RUNTIME_LLM_URL"] = origin
		if _, err := Load(env(base), Options{StudioRuntime: true}); err == nil {
			t.Fatalf("accepted %q", origin)
		}
	}
}
