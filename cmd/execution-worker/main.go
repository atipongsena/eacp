// Command execution-worker executes queued actions.
//
// Phase 1 (Platform Foundation) starts, verifies its database role and serves
// health probes only. The claim / lease / fenced-dispatch loop arrives in
// Phase 5 (MASTER_PLAN §79, ADR-004). Connector credentials will be loaded
// here and nowhere else (ADR-001 §3).
package main

import (
	"eacp/internal/config"
	"eacp/internal/service"
)

func main() {
	service.Main("execution-worker", config.Options{RequireDatabase: true, DefaultHTTPAddr: ":8081"}, nil)
}
