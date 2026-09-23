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
package main

import (
	"fmt"
	"net/http"
	"os"

	"eacp/internal/config"
	"eacp/internal/connector"
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
				if secrets, err = worker.LoadSecrets(path); err != nil {
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
			connectors := map[string]worker.Connector{"http": connector.NewHTTP()}
			w, err := worker.New(d.DB, worker.Options{
				ID: id, Lease: d.Config.WorkerLease, Concurrency: d.Config.WorkerConcurrency,
				PollInterval: d.Config.WorkerPollInterval, Secrets: secrets, Connectors: connectors, Log: d.Log,
			})
			if err != nil {
				return err
			}
			r, err := worker.NewReconciler(d.DB, worker.ReconcilerOptions{
				ID: id, Lease: d.Config.WorkerLease, Concurrency: d.Config.WorkerConcurrency,
				PollInterval: 2 * d.Config.WorkerPollInterval, MaxAttempts: d.Config.ReconcileMaxAttempts,
				MaxAge: d.Config.ReconcileMaxAge, Secrets: secrets, Connectors: connectors, Log: d.Log,
			})
			if err != nil {
				return err
			}
			d.Log.Info("worker ready", "worker_id", id, "bindings", len(secrets.Bindings()), "protocols", len(connectors))
			d.Background(w.Run)
			d.Background(r.Run)
			return nil
		})
}
