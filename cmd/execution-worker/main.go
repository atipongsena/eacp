// Command execution-worker executes queued actions (ADR-004, MASTER_PLAN
// §79): it claims actions from PostgreSQL under a fenced lease, commits a
// dispatch intent before any external call and records each result, or
// late-result evidence, under its lease generation.
//
// Connector credentials are loaded here and nowhere else (ADR-001 §3):
// only this service accepts EACP_CONNECTOR_SECRETS_FILE, and their values
// are redacted from its logs. Slice A Phase 5 registers no connector
// protocol yet (the HTTP connector is Phase 6), so the worker claims
// nothing until one is registered.
package main

import (
	"fmt"
	"net/http"
	"os"

	"eacp/internal/config"
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
			w, err := worker.New(d.DB, worker.Options{
				ID: id, Lease: d.Config.WorkerLease, Concurrency: d.Config.WorkerConcurrency,
				PollInterval: d.Config.WorkerPollInterval, Secrets: secrets,
				Connectors: map[string]worker.Connector{}, Log: d.Log,
			})
			if err != nil {
				return err
			}
			d.Log.Info("worker ready", "worker_id", id, "bindings", len(secrets.Bindings()), "protocols", 0)
			d.Background(w.Run)
			return nil
		})
}
