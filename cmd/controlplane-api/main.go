// Command controlplane-api serves the EACP control-plane API.
//
// It serves the registry, identity and capability API (ADR-003), policies
// and approvals (ADR-002/005) and the Action API up to the release boundary
// (ADR-004), operator resolution of actions that need a human, and runs the
// action sweeper, which also reclaims lapsed worker and reconciler leases,
// schedules retries and sends unknown outcomes without proof to a human.
// The execution worker dispatches and reconciles. Decisions come from the
// local provider or, with EACP_GOVERNANCE_PROVIDER=microsoft-agt, from the
// AGT sidecar PDP (ADR-002 §8). It prunes the transactional outbox and, with
// EACP_NATS_URL set, relays it to NATS JetStream as work hints and dashboard
// events (ADR-014); NATS is never an authority or a readiness dependency.
// It runs the FinOps alert evaluator every EACP_FINOPS_INTERVAL (ADR-025).
package main

import (
	"context"
	"net/http"
	"os"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"eacp/internal/action"
	"eacp/internal/api"
	"eacp/internal/config"
	"eacp/internal/finops"
	"eacp/internal/messaging"
	"eacp/internal/release"
	"eacp/internal/service"
)

// relayInterval is the idle wait between outbox relay passes.
const relayInterval = 200 * time.Millisecond

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
				Limits: action.Limits{MaxQueuedPerTenant: d.Config.MaxQueuedPerTenant, MaxQueuedGlobal: d.Config.MaxQueuedGlobal,
					MaxPendingPerTenant: d.Config.MaxPendingPerTenant},
				Log: d.Log, EvaluationTimeout: d.Config.PDPTimeout,
			})
			releases := release.New(d.DB, release.Options{Provider: provider, Log: d.Log,
				EvaluationTimeout: d.Config.PDPTimeout})
			api.New(d.DB, d.Log).WithActions(engine).WithReleases(releases).Register(mux)
			sweeper := action.NewSweeper(engine)
			sweeper.ReconcileMaxAge = d.Config.ReconcileMaxAge
			d.Background(func(ctx context.Context) { sweeper.Run(ctx, d.Config.SweepInterval) })
			d.Background(func(ctx context.Context) { messaging.RunPruner(ctx, d.DB, time.Minute, d.Log) })
			d.Background(func(ctx context.Context) { finops.New(d.DB).Run(ctx, d.Config.FinOpsInterval, d.Log) })
			d.Background(func(ctx context.Context) { releases.Run(ctx, d.Config.ReleaseInterval) })
			return startRelay(d)
		})
}

// startRelay runs the outbox relay when EACP_NATS_URL is set. An
// unreachable NATS server is not a startup failure: the client reconnects
// and the rows wait in the outbox.
func startRelay(d *service.Deps) error {
	if d.Config.NATSURL == "" {
		d.Log.Info("NATS not configured; work hints off, workers poll")
		return nil
	}
	nc, err := messaging.Connect(d.Config.NATSURL, d.Config.NATSCAFile, "controlplane-api relay", d.Log)
	if err != nil {
		return err
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return err
	}
	relay := messaging.NewRelay(d.DB, js, messaging.RelayOptions{PublishTimeout: d.Config.NATSPublishTimeout, Log: d.Log})
	d.Background(func(ctx context.Context) {
		defer nc.Close()
		relay.Run(ctx, relayInterval)
	})
	d.Log.Info("outbox relay started", "nats", config.RedactURL(d.Config.NATSURL))
	return nil
}
