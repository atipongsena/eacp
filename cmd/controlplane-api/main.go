// Command controlplane-api serves the EACP control-plane API.
//
// It serves the registry, identity and capability API (ADR-003), policies
// and approvals (ADR-002/005) and the Action API up to the release boundary
// (ADR-004), operator resolution of actions that need a human, and runs the
// action sweeper, which also reclaims lapsed worker and reconciler leases,
// schedules retries and sends unknown outcomes without proof to a human.
// The execution worker dispatches and reconciles. Decisions come from the
// local provider or, with EACP_GOVERNANCE_PROVIDER=microsoft-agt, from the
// AGT sidecar PDP (ADR-002 §8).
package main

import (
	"context"
	"net/http"
	"os"

	"eacp/internal/action"
	"eacp/internal/api"
	"eacp/internal/config"
	"eacp/internal/service"
)

func main() {
	service.Main("controlplane-api", config.Options{RequireDatabase: true, DefaultHTTPAddr: ":8080"},
		func(d *service.Deps, mux *http.ServeMux) error {
			instance, _ := os.Hostname()
			provider, err := governanceProvider(context.Background(), d.Config, d.Log, instance)
			if err != nil {
				return err
			}
			engine := action.New(d.DB, action.Options{
				Provider: provider,
				Limits:   action.Limits{MaxQueuedPerTenant: d.Config.MaxQueuedPerTenant, MaxQueuedGlobal: d.Config.MaxQueuedGlobal},
				Log:      d.Log, EvaluationTimeout: d.Config.PDPTimeout,
			})
			api.New(d.DB, d.Log).WithActions(engine).Register(mux)
			sweeper := action.NewSweeper(engine)
			sweeper.ReconcileMaxAge = d.Config.ReconcileMaxAge
			d.Background(func(ctx context.Context) { sweeper.Run(ctx, d.Config.SweepInterval) })
			return nil
		})
}
