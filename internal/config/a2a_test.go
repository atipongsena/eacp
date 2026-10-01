package config

import (
	"strings"
	"testing"
)

func TestA2AIngressIsOptInAndValidatesThePublicURL(t *testing.T) {
	for _, tc := range []struct {
		name, environment, url string
		valid                  bool
	}{
		{"disabled", "production", "", true},
		{"production", "production", "https://eacp.example.test/a2a", true},
		{"development", "development", "http://localhost:8080/a2a", true},
		{"test", "test", "http://api:8080/a2a", true},
		{"staging HTTP", "staging", "http://eacp.example.test/a2a", false},
		{"production HTTP", "production", "http://eacp.example.test/a2a", false},
		{"relative", "test", "/a2a", false},
		{"wrong path", "test", "https://api.test/other", false},
		{"trailing slash", "test", "https://api.test/a2a/", false},
		{"userinfo", "test", "https://someone@api.test/a2a", false},
		{"query", "test", "https://api.test/a2a?tenant=x", false},
		{"empty query", "test", "https://api.test/a2a?", false},
		{"fragment", "test", "https://api.test/a2a#x", false},
		{"empty fragment", "test", "https://api.test/a2a#", false},
		{"zero port", "test", "https://api.test:0/a2a", false},
		{"large port", "test", "https://api.test:65536/a2a", false},
		{"escaped path", "test", "https://api.test/%612a", false},
		{"non HTTP", "test", "file://api.test/a2a", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(env(map[string]string{"EACP_ENV": tc.environment, "EACP_A2A_PUBLIC_URL": tc.url}), Options{DefaultHTTPAddr: ":8080"})
			if (err == nil) != tc.valid {
				t.Fatalf("URL validation valid=%v, error=%v", tc.valid, err)
			}
			if err != nil && !strings.Contains(err.Error(), "EACP_A2A_PUBLIC_URL") {
				t.Fatalf("wrong error: %v", err)
			}
			if tc.valid && cfg.A2APublicURL != tc.url {
				t.Fatal("public URL changed")
			}
		})
	}
}
