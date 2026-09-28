package messaging_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/atipongsena/eacp/internal/action"
	"github.com/atipongsena/eacp/internal/governance"
	"github.com/atipongsena/eacp/internal/messaging"
	"github.com/atipongsena/eacp/internal/messaging/natstest"
	"github.com/atipongsena/eacp/internal/registry/registrytest"
	"github.com/atipongsena/eacp/internal/storage"
	"github.com/atipongsena/eacp/internal/storage/pgtest"
	"github.com/atipongsena/eacp/internal/worker"
)

const traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

// testTopology keeps the duplicate window and redelivery short so tests
// can step past them.
var testTopology = messaging.Topology{Duplicates: 200 * time.Millisecond, AckWait: time.Second}

type env struct {
	t     *testing.T
	f     *registrytest.Fixture
	e     *action.Engine
	agent registrytest.Agent
	nats  *natstest.Server
	js    jetstream.JetStream
	calls atomic.Int32
}

func newEnv(t *testing.T) *env {
	t.Helper()
	f := registrytest.New(t)
	tool := f.ActiveTool(t, "erp", "lookup")
	v := &env{t: t, f: f, agent: f.ActiveAgent(t, "buyer", tool.Tool), nats: natstest.Start(t)}
	f.ActivatePolicy(t, registrytest.AllowPolicy)
	v.e = action.New(f.App, action.Options{Provider: governance.LocalProvider{InstanceID: "messaging-test"}})
	_, v.js = v.nats.Connect(t)
	return v
}

// submit creates an allowed read action, which is QUEUED on return.
func (v *env) submit() action.View {
	v.t.Helper()
	got, err := v.e.Submit(context.Background(), action.Agent(v.f.Tenant, v.agent.Agent, v.agent.Version),
		action.Submission{IdempotencyKey: uuid.NewString(), Subject: "carol@tenant-a.test", Operation: "lookup",
			Target: "erp", Tool: "erp.lookup", ToolSchemaVersion: "1", Resource: "po",
			Payload: json.RawMessage(`{"po":"PO-1"}`), Traceparent: traceparent})
	if err != nil || got.State != "QUEUED" {
		v.t.Fatalf("submit = %+v, err = %v", got, err)
	}
	return got
}

func (v *env) get(id uuid.UUID) action.View {
	v.t.Helper()
	got, err := v.e.Get(context.Background(), v.f.Tenant, id)
	if err != nil {
		v.t.Fatal(err)
	}
	return got
}

func (v *env) relay(js jetstream.JetStream) *messaging.Relay {
	return messaging.NewRelay(v.f.App, js, messaging.RelayOptions{
		PublishTimeout: 500 * time.Millisecond, Topology: testTopology, Log: quiet(),
	})
}

func (v *env) count(sql string, args ...any) int {
	v.t.Helper()
	ctx := context.Background()
	var n int
	if err := storage.InTenantTx(ctx, v.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&n)
	}); err != nil {
		v.t.Fatal(err)
	}
	return n
}

func (v *env) streamMsgs(name string) uint64 {
	v.t.Helper()
	s, err := v.js.Stream(context.Background(), name)
	if err != nil {
		v.t.Fatal(err)
	}
	info, err := s.Info(context.Background())
	if err != nil {
		v.t.Fatal(err)
	}
	return info.State.Msgs
}

// worker returns an execution worker for the erp.lookup tool whose
// connector succeeds and counts its calls.
func (v *env) worker(poll time.Duration, wake <-chan struct{}) *worker.Worker {
	v.t.Helper()
	path := filepath.Join(v.t.TempDir(), "secrets.json")
	if err := os.WriteFile(path, []byte(`{"secrets":[{"tenant_id":"`+pgtest.TenantA+
		`","secret_ref":"erp","host":"fakeerp:8090","value":"messaging-test-secret"}]}`), 0o600); err != nil {
		v.t.Fatal(err)
	}
	secrets, err := worker.LoadSecrets(path)
	if err != nil {
		v.t.Fatal(err)
	}
	w, err := worker.New(v.f.App, worker.Options{
		ID: "hinted", Lease: 5 * time.Second, PollInterval: poll, Wake: wake, Secrets: secrets,
		Connectors: map[string]worker.Connector{"http": connector{&v.calls}}, Log: quiet(),
	})
	if err != nil {
		v.t.Fatal(err)
	}
	return w
}

type connector struct{ calls *atomic.Int32 }

func (c connector) Execute(_ context.Context, call worker.Call) worker.Result {
	c.calls.Add(1)
	return worker.Result{Outcome: worker.Succeeded, ExternalReference: "REF-" + call.ActionID.String()[:8]}
}

func (connector) Lookup(context.Context, worker.LookupCall) worker.LookupResult {
	return worker.LookupResult{Status: worker.LookupUnknown}
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// eventually polls cond every 20ms until it holds or d passes.
func eventually(d time.Duration, cond func() bool) bool {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return cond()
}
