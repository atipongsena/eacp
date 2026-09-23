// Command controlplane-api serves the EACP control-plane API.
//
// Phase 1 (Platform Foundation) serves health probes only. The Action API
// arrives in Phase 4 (MASTER_PLAN §78).
package main

import (
	"eacp/internal/config"
	"eacp/internal/service"
)

func main() {
	service.Main("controlplane-api", config.Options{RequireDatabase: true, DefaultHTTPAddr: ":8080"}, nil)
}
