// Command llm-gateway governs and meters agents' LLM calls (ADR-031). An
// agent sends an Anthropic Messages or OpenAI Chat Completions request with
// its own EACP key; the gateway authenticates it, asks the PDP, admits the
// call in PostgreSQL (allowlist, kill scopes and a hard budget reservation
// at the model's pinned price), forwards it with the provider credential and
// relays the answer, streaming included, then settles the call and its
// reservation from the reported usage.
//
// Provider credentials are loaded here and nowhere else: only this service
// accepts EACP_LLM_SECRETS_FILE, it refuses EACP_CONNECTOR_SECRETS_FILE, and
// every value is redacted from its logs. A sweeper settles calls a replica
// abandoned, every EACP_LLM_SWEEP_INTERVAL.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"eacp/internal/config"
	"eacp/internal/governance"
	"eacp/internal/identity"
	"eacp/internal/llm"
	"eacp/internal/llmgateway"
	"eacp/internal/service"
	"eacp/internal/worker"
)

func main() {
	service.Main("llm-gateway",
		config.Options{RequireDatabase: true, DefaultHTTPAddr: ":8083", AllowProviderSecrets: true},
		func(d *service.Deps, mux *http.ServeMux) error {
			// Minted tokens join the redaction set as they are issued (ADR-019).
			// The gateway sends Bearer or API-key credentials only: an aws
			// entry would need request signing and fails the load.
			opts := []worker.LoadOption{worker.WithRedaction(d.Redaction()), worker.WithLogger(d.Log),
				worker.RefuseSigningCredentials()}
			if env := d.Config.Environment; env == "development" || env == "test" {
				opts = append(opts, worker.AllowPlainTokenURL())
			}
			secrets, err := worker.LoadSecrets(d.Config.LLMSecretsFile, opts...)
			if err != nil {
				return err
			}
			d.RedactSecrets(secrets.Values()...)
			id := d.Config.LLMID
			if id == "" {
				host, err := os.Hostname()
				if err != nil {
					return fmt.Errorf("EACP_LLM_ID is unset and the host name is unavailable: %w", err)
				}
				id = host
			}
			pdp, err := governanceProvider(context.Background(), d.Config, d.Log, id)
			if err != nil {
				return err
			}
			store := llm.New(d.DB)
			gw, err := llmgateway.New(llmgateway.Options{
				ID: id,
				Auth: func(ctx context.Context, key string) (identity.Caller, error) {
					return identity.Authenticate(ctx, d.DB, key)
				},
				Ledger: store, Policies: governance.NewStore(d.DB), PDP: pdp, Secrets: secrets,
				AgentRisk: store.AgentRisk, MaxRequestBytes: d.Config.LLMMaxRequestBytes,
				KillPoll: d.Config.LLMKillPoll, Log: d.Log,
			})
			if err != nil {
				return err
			}
			mux.Handle("POST /v1/messages", gw)
			mux.Handle("POST /v1/chat/completions", gw)
			d.Log.Info("llm gateway ready", "gateway_id", id, "bindings", len(secrets.Bindings()))
			d.Background(func(ctx context.Context) {
				llmgateway.RunSweeper(ctx, d.Config.LLMSweepInterval, store, d.Log)
			})
			return nil
		})
}
