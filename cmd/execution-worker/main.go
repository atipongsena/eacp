// Command execution-worker executes queued actions (ADR-004, MASTER_PLAN
// §79): it claims actions from PostgreSQL under a fenced lease, commits a
// dispatch intent before any external call and records each result, or
// late-result evidence, under its lease generation. Its reconciler resolves
// UNKNOWN_OUTCOME actions by looking their operation key up under a fenced
// reconciler lease (ADR-004 T28-T34).
//
// Connector credentials are loaded here and nowhere else (ADR-001 §3):
// only this service accepts EACP_CONNECTOR_SECRETS_FILE, and their values
// are redacted from its logs. The HTTP connector is registered.
//
// With EACP_NATS_URL set, a work hint (ADR-014) wakes the claim loop at once;
// the hint grants nothing, and polling stays on as the backstop.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"eacp/internal/config"
	"eacp/internal/connector"
	"eacp/internal/connector/mcp"
	"eacp/internal/messaging"
	"eacp/internal/service"
	"eacp/internal/worker"
)

func main() {
	service.Main("execution-worker",
		config.Options{RequireDatabase: true, DefaultHTTPAddr: ":8081", AllowConnectorSecrets: true},
		func(d *service.Deps, _ *http.ServeMux) error {
			var secrets *worker.SecretStore
			if path := d.Config.ConnectorSecretsFile; path != "" {
				var err error
				// Minted tokens join the redaction set as they are issued (ADR-019).
				opts := []worker.LoadOption{worker.WithRedaction(d.Redaction()), worker.WithLogger(d.Log)}
				if env := d.Config.Environment; env == "development" || env == "test" {
					opts = append(opts, worker.AllowPlainTokenURL())
				}
				if secrets, err = worker.LoadSecrets(path, opts...); err != nil {
					return err
				}
				d.RedactSecrets(secrets.Values()...)
			}
			id := d.Config.WorkerID
			if id == "" {
				host, err := os.Hostname()
				if err != nil {
					return fmt.Errorf("EACP_WORKER_ID is unset and the host name is unavailable: %w", err)
				}
				id = host
			}
			wake, nc, err := startHints(d)
			if err != nil {
				return err
			}
			connectors := map[string]worker.Connector{"http": connector.NewHTTP()}
			w, err := worker.New(d.DB, worker.Options{
				ID: id, Lease: d.Config.WorkerLease, Concurrency: d.Config.WorkerConcurrency,
				GroupConcurrency: d.Config.WorkerGroupConcurrency, BreakerFailures: d.Config.WorkerBreakerFailures,
				BreakerCooldown: d.Config.WorkerBreakerCooldown,
				PollInterval:    d.Config.WorkerPollInterval, Wake: wake, Secrets: secrets, Connectors: connectors, Log: d.Log,
			})
			if err != nil {
				return err
			}
			if nc != nil {
				// This is a broadcast latency hint. Each in-flight call reads
				// PostgreSQL before deciding whether to cancel.
				if _, err := nc.Subscribe("eacp.events.*.kill.changed", func(*nats.Msg) { w.NotifyKill() }); err != nil {
					return err
				}
			}
			r, err := worker.NewReconciler(d.DB, worker.ReconcilerOptions{
				ID: id, Lease: d.Config.WorkerLease, Concurrency: d.Config.WorkerConcurrency,
				PollInterval: 2 * d.Config.WorkerPollInterval, MaxAttempts: d.Config.ReconcileMaxAttempts,
				MaxAge: d.Config.ReconcileMaxAge, Secrets: secrets, Connectors: connectors, Log: d.Log,
			})
			if err != nil {
				return err
			}
			scanner, err := worker.NewScanner(d.DB, worker.ScannerOptions{
				ID: id, Interval: d.Config.MCPScanInterval, Timeout: d.Config.MCPScanTimeout,
				Secrets: secrets, Discoverer: mcp.New(), Log: d.Log,
			})
			if err != nil {
				return err
			}
			d.Log.Info("worker ready", "worker_id", id, "bindings", len(secrets.Bindings()), "protocols", len(connectors))
			d.Background(w.Run)
			d.Background(r.Run)
			d.Background(scanner.Run)
			return nil
		})
}

// startHints runs the work-hint consumer when EACP_NATS_URL is set and
// returns its wake-up channel (nil otherwise: the worker only polls).
func startHints(d *service.Deps) (<-chan struct{}, *nats.Conn, error) {
	if d.Config.NATSURL == "" {
		return nil, nil, nil
	}
	nc, err := messaging.Connect(d.Config.NATSURL, d.Config.NATSCAFile, "execution-worker hints", d.Log)
	if err != nil {
		return nil, nil, err
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, nil, err
	}
	hints, err := messaging.NewHints(d.DB, js, messaging.HintOptions{Log: d.Log})
	if err != nil {
		nc.Close()
		return nil, nil, err
	}
	d.Background(func(ctx context.Context) {
		defer nc.Close()
		hints.Run(ctx)
	})
	d.Log.Info("work hints on", "nats", config.RedactURL(d.Config.NATSURL))
	return hints.Wake(), nc, nil
}
