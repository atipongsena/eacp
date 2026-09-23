package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPDPDevCertsOnlyInDevelopment(t *testing.T) {
	dir := t.TempDir()
	for _, envName := range []string{"", "staging", "production"} {
		if _, err := runWith(t, map[string]string{"EACP_ENV": envName}, "pdp-dev-certs", "--dir", dir, "--name", "agt-pdp"); err == nil ||
			!strings.Contains(err.Error(), "refused") {
			t.Errorf("EACP_ENV=%q: err = %v, want refusal", envName, err)
		}
	}
	if _, err := runWith(t, map[string]string{"EACP_ENV": "development"}, "pdp-dev-certs", "--dir", dir); err == nil {
		t.Error("no --name accepted")
	}
	out, err := runWith(t, map[string]string{"EACP_ENV": "development"}, "pdp-dev-certs", "--dir", dir, "--name", "agt-pdp", "--name", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"ca.pem", "server.pem", "server-key.pem", "client.pem", "client-key.pem"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	if !strings.Contains(out, "development") {
		t.Errorf("output = %q", out)
	}
}
