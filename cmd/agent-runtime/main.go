// Command agent-runtime runs approved Agent Studio agents (ADR-033, Phase
// 27a-2). It claims runs through the EACP API with its own principal key
// (one per tenant, EACP_RUNTIME_KEY_FILE), acts as each run's agent version
// with a key derived from the Studio master (EACP_STUDIO_MASTER_FILE) and
// sends every tool call through the ordinary action path. It proposes each
// version's key, and its successor 30 days before expiry, for a
// registry_approver to approve; it never approves anything.
//
// It has no database connection, no connector secret and no provider key:
// only this service accepts the Studio master, and it refuses
// EACP_DATABASE_URL and every other secret file. The master and every
// derived key are held in memory only and redacted from its logs.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"github.com/atipongsena/eacp/internal/config"
	"github.com/atipongsena/eacp/internal/service"
	"github.com/atipongsena/eacp/internal/studioruntime"
)

func main() {
	service.Main("agent-runtime", config.Options{StudioRuntime: true, DefaultHTTPAddr: ":8084"},
		func(d *service.Deps, _ *http.ServeMux) error {
			master, err := studioruntime.LoadMaster(d.Config.StudioMasterFile, d.Config.StudioMasterVersion)
			if err != nil {
				return err
			}
			d.RedactSecrets(master.Redactions()...)
			keys, err := studioruntime.LoadKeys(d.Config.RuntimeKeyFile)
			if err != nil {
				return err
			}
			for _, k := range keys {
				d.RedactSecrets(k)
			}
			id := d.Config.RuntimeID
			if id == "" {
				host, err := os.Hostname()
				if err != nil {
					return fmt.Errorf("EACP_RUNTIME_ID is unset and the host name is unavailable: %w", err)
				}
				id = host
			}
			rt, err := studioruntime.New(studioruntime.Options{
				API: d.Config.APIURL, Keys: keys, Master: master, ID: id, Lease: d.Config.RuntimeLease,
				Gateway:     d.Config.RuntimeLLMURL,
				Concurrency: d.Config.RuntimeConcurrency, Poll: d.Config.RuntimePollInterval,
				Log: d.Log, Redact: d.RedactSecrets,
			})
			if err != nil {
				return err
			}
			d.Log.Info("agent runtime ready", "runtime_id", id, "tenants", len(keys),
				"master_version", master.Version())
			d.Background(rt.Run)
			d.Background(func(ctx context.Context) { rt.Rotate(ctx, d.Config.RuntimeRotateInterval) })
			return nil
		})
}
