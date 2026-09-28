package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/atipongsena/eacp/integrations/governance/microsoftagt"
	"github.com/atipongsena/eacp/internal/config"
	"github.com/atipongsena/eacp/internal/governance"
)

func health(versions microsoftagt.Versions) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "protocol": microsoftagt.Protocol,
			"versions": versions, "provider_instance_id": "t"})
	})
}

// ADR-002 §7/§8: a reachable sidecar with other versions aborts startup; an
// unreachable one only warns, because cancel and containment never need
// the PDP and must stay available (§6).
func TestGatewayGovernanceProviderSelection(t *testing.T) {
	ctx := context.Background()
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	agt := func(url string) config.Config {
		return config.Config{GovernanceProvider: "microsoft-agt", AGTPDPURL: url, PDPTimeout: time.Second}
	}

	p, err := governanceProvider(ctx, config.Config{GovernanceProvider: "local"}, log, "host")
	if l, ok := p.(governance.LocalProvider); err != nil || !ok || l.InstanceID != "llm-gateway/host" {
		t.Fatalf("local = %#v, %v", p, err)
	}

	good := httptest.NewServer(health(microsoftagt.Pinned))
	defer good.Close()
	if p, err := governanceProvider(ctx, agt(good.URL), log, "host"); err != nil {
		t.Fatal(err)
	} else if _, ok := p.(*microsoftagt.Provider); !ok {
		t.Fatalf("agt = %#v", p)
	}

	wrong := microsoftagt.Pinned
	wrong.ACS = "0.4.0"
	bad := httptest.NewServer(health(wrong))
	defer bad.Close()
	if _, err := governanceProvider(ctx, agt(bad.URL), log, "host"); !errors.Is(err, microsoftagt.ErrVersionMismatch) {
		t.Fatalf("mismatched sidecar = %v", err)
	}

	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	if _, err := governanceProvider(ctx, agt(down.URL), log, "host"); err != nil {
		t.Fatalf("unreachable sidecar aborted startup: %v", err)
	}
	if !strings.Contains(logs.String(), "unreachable") {
		t.Fatalf("no warning logged: %s", logs.String())
	}

	if _, err := governanceProvider(ctx, agt("http://agt-pdp:8181"), log, "host"); err == nil {
		t.Fatal("plain http to a non-loopback sidecar accepted")
	}
}
