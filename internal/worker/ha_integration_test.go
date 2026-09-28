package worker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atipongsena/eacp/internal/action"
	"github.com/atipongsena/eacp/internal/audit"
	"github.com/atipongsena/eacp/internal/connector"
	"github.com/atipongsena/eacp/internal/finops"
	"github.com/atipongsena/eacp/internal/governance"
	"github.com/atipongsena/eacp/internal/incident"
	"github.com/atipongsena/eacp/internal/messaging"
	"github.com/atipongsena/eacp/internal/release"
	"github.com/atipongsena/eacp/internal/storage"
	"github.com/atipongsena/eacp/internal/worker"
)

// syncBuffer is a log sink shared by goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// replica is one process's loops on its own pool; stop cancels them as
// SIGTERM does, waits for them, then closes the pool.
type replica struct {
	engine *action.Engine
	stop   func()
}

func (v *erpEnv) pool() *pgxpool.Pool {
	v.t.Helper()
	p, err := pgxpool.New(context.Background(), v.f.DB.AppDSN)
	if err != nil {
		v.t.Fatal(err)
	}
	return p
}

func start(pool *pgxpool.Pool, loops ...func(context.Context)) func() {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for _, fn := range loops {
		wg.Go(func() { fn(ctx) })
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			wg.Wait()
			pool.Close()
		})
	}
}

func (v *erpEnv) apiReplica(n int, log *slog.Logger) replica {
	pool := v.pool()
	provider := governance.LocalProvider{InstanceID: fmt.Sprintf("api-%d", n)}
	e := action.New(pool, action.Options{Provider: provider, Log: log})
	s := action.NewSweeper(e)
	s.Grace = 0
	rel := release.New(pool, release.Options{Provider: provider, Log: log})
	return replica{engine: e, stop: start(pool,
		func(ctx context.Context) { s.Run(ctx, 100*time.Millisecond) },
		func(ctx context.Context) { incident.New(pool).Run(ctx, 100*time.Millisecond, log) },
		func(ctx context.Context) { finops.New(pool).Run(ctx, 100*time.Millisecond, log) },
		func(ctx context.Context) { rel.Run(ctx, 100*time.Millisecond) },
		func(ctx context.Context) { messaging.RunPruner(ctx, pool, 100*time.Millisecond, log) },
	)}
}

func (v *erpEnv) workerReplica(n int, log *slog.Logger) replica {
	v.t.Helper()
	pool := v.pool()
	conns := map[string]worker.Connector{"http": connector.NewHTTP()}
	// Most scenarios fail at the ERP on purpose, so a breaker would open the
	// shared circuit and its doubling cooldowns would outlast the test; the
	// breaker is ADR-022's subject, not this test's.
	w, err := worker.New(pool, worker.Options{ID: fmt.Sprintf("w-ha-%d", n), Lease: 5 * time.Second,
		BreakerFailures: 1000, PollInterval: 50 * time.Millisecond, Connectors: conns, Secrets: v.secrets, Log: log,
		Backoff: func(int) time.Duration { return 100 * time.Millisecond }})
	if err != nil {
		v.t.Fatal(err)
	}
	r, err := worker.NewReconciler(pool, worker.ReconcilerOptions{ID: fmt.Sprintf("r-ha-%d", n), Lease: 5 * time.Second,
		PollInterval: 50 * time.Millisecond, Connectors: conns, Secrets: v.secrets, Log: log,
		Backoff: func(int) time.Duration { return 100 * time.Millisecond }})
	if err != nil {
		v.t.Fatal(err)
	}
	return replica{stop: start(pool, w.Run, r.Run)}
}

// Three API replicas (sweeper, incident, FinOps and release evaluators,
// outbox pruner) and three workers (worker and reconciler) share one
// database. One of each stops a third of the way through. Every action
// still ends SUCCEEDED with one ERP record per operation key (effectively
// once where reconcilable), a standing kill opens one incident although
// three evaluators saw it, no loop reports a failure, the audit chain
// verifies, and more than one worker did the work.
func TestReplicasShareTheWorkAndSurviveLosingOne(t *testing.T) {
	v := newERPEnv(t)
	ctx := context.Background()
	idle := v.f.ActiveAgent(t, "idle")
	if err := v.f.Exec("otto", `SELECT eacp.set_kill('agent', $1, true, 'ha test', 'security_incident')`, idle.Agent); err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))

	var apis, workers []replica
	for n := range 3 {
		apis = append(apis, v.apiReplica(n, log))
		workers = append(workers, v.workerReplica(n, log))
	}
	defer func() {
		for _, r := range append(apis, workers...) {
			r.stop()
		}
	}()

	scenarios := []map[string]any{
		{}, {"scenario": "execute_then_timeout", "delay_ms": 300}, {"scenario": "execute_then_reset"},
		{"scenario": "5xx_after_effect"}, {"scenario": "slow_response", "delay_ms": 50},
	}
	actor := action.Agent(v.f.Tenant, v.agent.Agent, v.agent.Version)
	var ids []uuid.UUID
	keys := map[uuid.UUID]string{}
	const stopAt = 10
	for i := range 30 {
		if i == stopAt {
			apis[0].stop()
			workers[0].stop()
		}
		payload := map[string]any{"amount": 42, "currency": "THB"}
		for k, val := range scenarios[i%len(scenarios)] {
			payload[k] = val
		}
		b, _ := json.Marshal(payload)
		via := apis[i%3]
		if i >= stopAt {
			via = apis[1+i%2]
		}
		a, err := via.engine.Submit(ctx, actor, action.Submission{IdempotencyKey: uuid.NewString(),
			Subject: "carol@tenant-a.test", Operation: "post", Target: "erp", Tool: "erp.create_po",
			ToolSchemaVersion: "1", Resource: "po", Payload: b})
		if err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
		ids = append(ids, a.ID)
		keys[a.ID] = a.OperationKey
		time.Sleep(30 * time.Millisecond)
	}

	for _, got := range v.settled(ids, 2*time.Minute) {
		if got.State != "SUCCEEDED" || got.ExternalReference != "PO-"+got.ID.String() {
			t.Errorf("%s = %s (%s)", got.ID, got.State, got.StateReason)
		}
		if n := v.effects(keys[got.ID]); n != 1 {
			t.Errorf("%s: %d ERP records", got.ID, n)
		}
	}
	// Let every evaluator run a few more passes over the standing kill.
	time.Sleep(500 * time.Millisecond)

	var kills, workersUsed, stoppedAttempts int
	err := storage.InTenantTx(ctx, v.f.Owner, v.f.Tenant.String(), func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM eacp.incidents WHERE kind = 'kill'`).Scan(&kills); err != nil {
			return err
		}
		// The stopped worker was really working when it stopped.
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM eacp.action_attempts WHERE worker_id = 'w-ha-0'`).Scan(&stoppedAttempts); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(DISTINCT worker_id) FROM eacp.actions WHERE id = ANY($1)`, ids).Scan(&workersUsed)
	})
	if err != nil {
		t.Fatal(err)
	}
	if stoppedAttempts == 0 {
		t.Error("the stopped worker made no attempt: losing it proved nothing")
	}
	if kills != 1 {
		t.Errorf("kill incidents = %d, want 1", kills)
	}
	if workersUsed < 2 {
		t.Errorf("%d worker ids executed the actions, want the work shared", workersUsed)
	}
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, `"level":"ERROR"`) {
			t.Errorf("a replica logged an error: %s", line)
		}
	}
	if err := storage.InTenantReadTx(ctx, v.f.App, v.f.Tenant.String(), func(tx pgx.Tx) error {
		_, err := audit.Verify(ctx, tx)
		return err
	}); err != nil {
		t.Fatalf("audit chain: %v", err)
	}
}
