// Command controlplane-api serves the EACP control-plane API.
//
// Phase 2 serves the registry, identity and capability API (ADR-003) plus
// health probes. The Action API arrives in Phase 4 (MASTER_PLAN §78).
package main

import (
	"net/http"

	"eacp/internal/api"
	"eacp/internal/config"
	"eacp/internal/service"
)

func main() {
	service.Main("controlplane-api", config.Options{RequireDatabase: true, DefaultHTTPAddr: ":8080"},
		func(d *service.Deps, mux *http.ServeMux) {
			api.New(d.DB, d.Log).Register(mux)
		})
}
