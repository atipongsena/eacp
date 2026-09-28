package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/atipongsena/eacp/internal/config"
	"github.com/atipongsena/eacp/internal/service"
	"github.com/atipongsena/eacp/internal/version"
)

func TestStartLogsTheBuildVersion(t *testing.T) {
	var out bytes.Buffer
	_, stop, err := service.Start(context.Background(), "fakeerp", envFrom(nil), config.Options{RequireDatabase: false}, &out)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	stop()
	for _, line := range strings.Split(out.String(), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil || rec["msg"] != "service started" {
			continue
		}
		if rec["version"] != version.Version {
			t.Fatalf("service started logs version %v, want %q", rec["version"], version.Version)
		}
		return
	}
	t.Fatalf("no service started record in:\n%s", out.String())
}
