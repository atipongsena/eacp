package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"eacp/integrations/governance/microsoftagt"
	"eacp/internal/config"
	"eacp/internal/governance"
)

// governanceProvider builds the configured PDP (ADR-002 §8). For the AGT
// sidecar it checks the version pins at startup: a reachable sidecar with
// another protocol or engine stack aborts startup (fail closed), while an
// unreachable one only warns. Every decision is checked against the pins
// again, and the PDP is deliberately not a readiness check: cancel,
// settlement, the sweeper and kills never consult it and must stay available
// during a PDP outage (§6, ADR-031).
func governanceProvider(ctx context.Context, cfg config.Config, log *slog.Logger, instance string) (governance.GovernanceProvider, error) {
	if cfg.GovernanceProvider != "microsoft-agt" {
		return governance.LocalProvider{InstanceID: "llm-gateway/" + instance}, nil
	}
	p, err := microsoftagt.New(microsoftagt.Config{URL: cfg.AGTPDPURL, CAFile: cfg.AGTPDPCAFile,
		CertFile: cfg.AGTPDPCertFile, KeyFile: cfg.AGTPDPKeyFile})
	if err != nil {
		return nil, err
	}
	hctx, cancel := context.WithTimeout(ctx, cfg.PDPTimeout)
	defer cancel()
	switch err := p.Health(hctx); {
	case errors.Is(err, microsoftagt.ErrVersionMismatch):
		return nil, fmt.Errorf("refusing the AGT sidecar: %w", err)
	case err != nil:
		log.Warn("AGT sidecar unreachable at startup; LLM calls are refused until it answers", "err", err)
	default:
		log.Info("AGT sidecar ready", "url", cfg.AGTPDPURL, "agt", microsoftagt.Pinned.AGT,
			"acs", microsoftagt.Pinned.ACS, "opa", microsoftagt.Pinned.OPA)
	}
	return p, nil
}
