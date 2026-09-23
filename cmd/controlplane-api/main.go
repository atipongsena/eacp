// Command controlplane-api serves the EACP control-plane API.
//
// It serves the registry, identity and capability API (ADR-003), policies
// and approvals (ADR-002/005) and the Action API up to the release boundary
// (ADR-004), and runs the action sweeper, which also reclaims lapsed worker
// leases and schedules retries. The execution worker dispatches.
package main

import (
	"context"
	"net/http"
	"os"

	"eacp/internal/action"
	"eacp/internal/api"
	"eacp/internal/config"
	"eacp/internal/governance"
	"eacp/internal/service"
)

func main() {
	service.Main("controlplane-api", config.Options{RequireDatabase: true, DefaultHTTPAddr: ":8080"},
		func(d *service.Deps, mux *http.ServeMux) error {
			instance, _ := os.Hostname()
			engine := action.New(d.DB, action.Options{
				Provider: governance.LocalProvider{InstanceID: "controlplane-api/" + instance},
				Limits:   action.Limits{MaxQueuedPerTenant: d.Config.MaxQueuedPerTenant, MaxQueuedGlobal: d.Config.MaxQueuedGlobal},
				Log:      d.Log, EvaluationTimeout: d.Config.PDPTimeout,
			})
			api.New(d.DB, d.Log).WithActions(engine).Register(mux)
			d.Background(func(ctx context.Context) {
				action.NewSweeper(engine).Run(ctx, d.Config.SweepInterval)
			})
			return nil
		})
}
