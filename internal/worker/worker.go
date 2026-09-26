package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"eacp/internal/governance"
)

// Options configure a Worker.
type Options struct {
	// ID names this worker in leases, attempts and the journal.
	ID string
	// Lease is how long a claim or heartbeat holds an action (default 30s,
	// 5s-5m). Heartbeats run every Lease/3 while a call is in flight.
	Lease time.Duration
	// Concurrency bounds the actions executed at once (default 4).
	Concurrency int
	// GroupConcurrency bounds the actions executed at once per tenant and
	// capacity group (the contract's concurrency_group, else its connector):
	// the worker's bulkhead (ADR-022 §2). Default: half of Concurrency, at
	// least 1; at most Concurrency.
	GroupConcurrency int
	// BreakerFailures consecutive failures of a connector open its breaker
	// for BreakerCooldown, doubling on each consecutive trip up to 10m, and
	// open its shared circuit as long (ADR-022 §3). Defaults 5 and 30s.
	BreakerFailures int
	BreakerCooldown time.Duration
	// PollInterval is the idle wait between claims (default 500ms).
	PollInterval time.Duration
	// KillPollInterval checks PostgreSQL during an external call even when
	// NATS drops a kill signal (default 1s).
	KillPollInterval time.Duration
	// Wake, when set, ends an idle wait early: a NATS work hint (ADR-014)
	// makes the loop claim at once. A wake-up only triggers the ordinary
	// fenced claim through PostgreSQL; polling continues as the backstop.
	Wake <-chan struct{}
	// Backoff is the delay before retry attempt n+1 after attempt n
	// (default 1s doubling, at most 5m, with equal jitter).
	Backoff func(attempt int) time.Duration
	// Connectors by protocol. The worker claims only actions whose
	// connector protocol is listed here.
	Connectors map[string]Connector
	// Secrets are the connector credentials. The worker claims only
	// actions whose tenant, secret reference and endpoint host it holds.
	Secrets *SecretStore
	Log     *slog.Logger

	// AfterIntent is a test hook run after the dispatch intent commits and
	// before the external call.
	AfterIntent func(context.Context, Lease)
}

var workerIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
var errorClassPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)

// Worker claims and executes actions (ADR-004 T14, T16-T22a).
type Worker struct {
	store     *Store
	o         Options
	protocols []string
	breaker   *breaker

	mu       sync.Mutex
	inflight map[Skip]int // leased actions per tenant and capacity group
	killMu   sync.Mutex
	killWake chan struct{}
}

// New returns a Worker over pool (the application role).
func New(pool *pgxpool.Pool, o Options) (*Worker, error) {
	if !workerIDPattern.MatchString(o.ID) {
		return nil, fmt.Errorf("worker: invalid worker id %q", o.ID)
	}
	if o.Lease == 0 {
		o.Lease = 30 * time.Second
	}
	if o.Lease < 5*time.Second || o.Lease > 5*time.Minute {
		return nil, errors.New("worker: lease must be between 5s and 5m")
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 4
	}
	if o.GroupConcurrency <= 0 {
		o.GroupConcurrency = max(o.Concurrency/2, 1)
	}
	if o.GroupConcurrency > o.Concurrency {
		return nil, errors.New("worker: group concurrency must not exceed concurrency")
	}
	if o.BreakerFailures <= 0 {
		o.BreakerFailures = 5
	}
	if o.BreakerCooldown <= 0 {
		o.BreakerCooldown = 30 * time.Second
	}
	if o.BreakerCooldown < time.Second || o.BreakerCooldown > maxBreakerCooldown {
		return nil, errors.New("worker: breaker cooldown must be between 1s and 10m")
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 500 * time.Millisecond
	}
	if o.KillPollInterval <= 0 {
		o.KillPollInterval = time.Second
	}
	if o.Backoff == nil {
		o.Backoff = jitteredBackoff
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	w := &Worker{store: NewStore(pool, o.ID), o: o, inflight: map[Skip]int{}, killWake: make(chan struct{}),
		breaker: newBreaker(o.BreakerFailures, o.BreakerCooldown, time.Now)}
	for p := range o.Connectors {
		w.protocols = append(w.protocols, p)
	}
	sort.Strings(w.protocols)
	return w, nil
}

// Store returns the worker's fenced store.
func (w *Worker) Store() *Store { return w.store }

// NotifyKill wakes every in-flight call's database check. A message alone
// never cancels a call; PostgreSQL remains the only authority.
func (w *Worker) NotifyKill() {
	w.killMu.Lock()
	close(w.killWake)
	w.killWake = make(chan struct{})
	w.killMu.Unlock()
}

func (w *Worker) killSignal() <-chan struct{} {
	w.killMu.Lock()
	defer w.killMu.Unlock()
	return w.killWake
}

// skipList is what the claim hint must leave out for this worker: capacity
// groups whose bulkhead is full and connectors whose breaker is open.
func (w *Worker) skipList() []Skip {
	w.mu.Lock()
	defer w.mu.Unlock()
	var skip []Skip
	for k, n := range w.inflight {
		if n >= w.o.GroupConcurrency {
			skip = append(skip, k)
		}
	}
	for _, k := range w.breaker.blockedKeys() {
		skip = append(skip, Skip{TenantID: k.tenant, Key: "connector:" + k.connector.String()})
	}
	return skip
}

// hold counts a lease against its bulkhead until the returned func runs.
func (w *Worker) hold(l Lease) (free func()) {
	k := Skip{TenantID: l.TenantID, Key: l.Capacity}
	w.mu.Lock()
	w.inflight[k]++
	w.mu.Unlock()
	return func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.inflight[k]--; w.inflight[k] <= 0 {
			delete(w.inflight, k)
		}
	}
}

// claimed is a lease counted against its bulkhead.
type claimed struct {
	Lease
	free func()
}

// claim leases up to n claimable actions, keeping each capacity group within
// the worker's bulkhead and skipping connectors whose breaker is open.
func (w *Worker) claim(ctx context.Context, n int) ([]claimed, error) {
	var leases []claimed
	attempted := make(map[Candidate]bool)
	for len(leases) < n {
		cands, err := w.store.Claimable(ctx, w.protocols, w.o.Secrets.Available(), 100, w.skipList())
		if err != nil {
			return leases, err
		}
		var next Candidate
		found := false
		for _, c := range cands {
			if !attempted[c] {
				next, found = c, true
				break
			}
		}
		if !found {
			break
		}
		l, ok, err := w.store.Claim(ctx, next, w.o.Lease)
		if err != nil {
			return leases, err
		}
		if ok {
			leases = append(leases, claimed{Lease: l, free: w.hold(l)})
			clear(attempted) // The successful claim advanced the database turn.
		} else {
			attempted[next] = true
		}
	}
	return leases, nil
}

// RunOnce claims up to Concurrency actions, executes them concurrently and
// returns how many it executed.
func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	leases, err := w.claim(ctx, w.o.Concurrency)
	var wg sync.WaitGroup
	for _, l := range leases {
		wg.Go(func() {
			defer l.free()
			w.execute(ctx, l.Lease)
		})
	}
	wg.Wait()
	return len(leases), err
}

// Run claims and executes actions until ctx is cancelled, then stops
// claiming and waits for in-flight calls (bounded by their call deadline).
// Leased actions not yet dispatched are released.
func (w *Worker) Run(ctx context.Context) {
	slots := make(chan struct{}, w.o.Concurrency)
	freed := make(chan struct{}, w.o.Concurrency)
	var wg sync.WaitGroup
	defer wg.Wait()
	for ctx.Err() == nil {
		if free := cap(slots) - len(slots); free > 0 {
			leases, err := w.claim(ctx, free)
			if err != nil && ctx.Err() == nil {
				w.o.Log.ErrorContext(ctx, "claim failed", "err", err)
			}
			for _, l := range leases {
				slots <- struct{}{}
				wg.Go(func() {
					defer func() {
						l.free()
						<-slots
						select { // wake the loop without ever blocking on it
						case freed <- struct{}{}:
						default:
						}
					}()
					w.execute(ctx, l.Lease)
				})
			}
			if len(leases) > 0 {
				continue
			}
		}
		select {
		case <-ctx.Done():
		case <-freed:
		case <-w.o.Wake: // a nil channel never fires
		case <-time.After(w.o.PollInterval):
		}
	}
}

// execute runs one leased action through dispatch intent, call and result.
// ctx is the worker's lifetime: once it ends, nothing new is dispatched,
// but a call already in flight completes and records its result.
func (w *Worker) execute(ctx context.Context, l Lease) {
	exec := context.WithoutCancel(ctx)
	log := w.o.Log.With("tenant", l.TenantID.String(), "action", l.ActionID.String(), "generation", l.Generation)
	job, err := w.store.Load(exec, l)
	if err != nil {
		log.WarnContext(exec, "cannot load leased action", "err", err)
		return
	}
	conn := w.o.Connectors[job.Protocol]
	if conn == nil {
		log.WarnContext(exec, "worker cannot serve this connector; releasing", "protocol", job.Protocol)
		w.release(exec, l, log, "worker cannot serve this connector")
		return
	}
	// ADR-019: the credential must outlive the whole call, so a token that
	// could expire mid-call is never sent. Nothing is dispatched without one.
	secret, err := w.o.Secrets.Credential(exec, l.TenantID, job.SecretRef, job.Endpoint, job.CallTimeout+CredentialSkew)
	if err != nil {
		reason := "worker cannot serve this connector"
		if errors.Is(err, ErrCredentialUnavailable) {
			reason = "credential unavailable"
		}
		log.WarnContext(exec, "no credential for this call; releasing", "reason", reason)
		w.release(exec, l, log, reason)
		return
	}
	// Invariant 14: dispatch only the enforced payload whose digest the
	// governance decision and grant were bound to.
	_, enforced, err := governance.Digests(governance.Binding{
		TenantID: l.TenantID, AgentID: job.AgentID, AgentVersionID: job.AgentVersionID, Subject: job.Subject,
		Operation: job.Operation, Target: job.Target, Tool: job.Tool, ToolSchemaVersion: job.ToolSchemaVersion,
		Resource: job.Resource, Payload: job.InputPayload,
	}, job.EnforcedPayload)
	if err != nil || !bytes.Equal(enforced[:], job.EnforcedDigest) {
		log.ErrorContext(exec, "security alert", "alert", "worker.enforced_digest_mismatch")
		if err := w.store.Deny(exec, l, "enforced_digest_mismatch"); err != nil {
			log.ErrorContext(exec, "cannot deny action", "err", err)
		}
		return
	}
	if ctx.Err() != nil {
		w.release(exec, l, log, "worker shutting down")
		return
	}
	// ADR-022 §3: an open breaker withholds the call; a half-open one lets
	// this call through as its probe.
	key := breakerKey{l.TenantID, job.ConnectorID}
	admitted, isProbe := w.breaker.start(key)
	if !admitted {
		w.release(exec, l, log, "connector circuit open")
		return
	}
	probe := neutral // until a call is made
	defer func() {
		if cooldown := w.breaker.done(key, probe, isProbe); cooldown > 0 {
			log.WarnContext(exec, "connector circuit breaker opened", "connector", job.ConnectorID.String(),
				"cooldown", cooldown)
			if err := w.store.TripCircuit(exec, l, job.ConnectorID, cooldown,
				fmt.Sprintf("worker %s: %d consecutive failures", w.o.ID, w.o.BreakerFailures)); err != nil {
				log.ErrorContext(exec, "cannot open the shared circuit", "err", err)
			}
		}
	}()

	decision, d, err := w.store.Intent(exec, l, w.o.Lease)
	if err != nil {
		log.WarnContext(exec, "dispatch intent failed; not dispatching", "err", err)
		return
	}
	if decision != Dispatched {
		log.InfoContext(exec, "not dispatched", "decision", string(decision))
		return
	}
	if w.o.AfterIntent != nil {
		w.o.AfterIntent(exec, l)
	}
	killed, err := w.store.CheckKill(exec, l)
	if err != nil || killed {
		// An intent committed, so absence of a call is still recorded as an
		// ambiguous attempt. Reconciliation, not this worker, proves effect.
		if _, completeErr := w.store.Complete(exec, l,
			Result{Outcome: Ambiguous, ErrorClass: "kill_before_call"}, 0); completeErr != nil {
			log.WarnContext(exec, "cannot record withheld call", "err", completeErr)
		}
		return
	}

	callCtx, cancel := context.WithTimeout(exec, d.Timeout)
	var cancelled atomic.Bool // set when the worker itself cancels the call
	stop := w.heartbeat(exec, l, func() { cancelled.Store(true); cancel() }, log)
	res := safeExecute(callCtx, conn, Call{
		TenantID: l.TenantID, ActionID: l.ActionID, OperationKey: job.OperationKey, Attempt: d.Attempt,
		Generation: l.Generation, Tool: job.Tool, Endpoint: job.Endpoint, Payload: job.EnforcedPayload,
		Contract: job.Contract, Secret: secret,
	})
	cancel()
	stop()
	res = classify(scrub(res, w.o.Secrets.Values()), job.Contract)
	if res.ErrorClass == "unauthorized" {
		// The target refused the credential: a minted token is dropped so
		// the next attempt mints another. The outcome stays as classified.
		w.o.Secrets.Rejected(l.TenantID, job.SecretRef, secret)
	}
	probe = signalOf(res, cancelled.Load())

	var comp Completion
	for try := 0; ; try++ {
		comp, err = w.store.Complete(exec, l, res, w.o.Backoff(d.Attempt))
		if err == nil || errors.Is(err, ErrLeaseLost) || try == 2 {
			break
		}
		time.Sleep(time.Duration(try+1) * 200 * time.Millisecond)
	}
	switch {
	case err != nil:
		// The lease will expire and the sweeper applies T23/T24; the
		// outcome of this attempt stays unknown (conservative).
		log.ErrorContext(exec, "cannot record result", "outcome", string(res.Outcome), "err", err)
	case comp.Late:
		log.WarnContext(exec, "late result recorded as evidence", "outcome", string(res.Outcome))
	default:
		log.InfoContext(exec, "attempt completed", "attempt", d.Attempt, "outcome", string(res.Outcome),
			"state", comp.State)
	}
}

func (w *Worker) release(ctx context.Context, l Lease, log *slog.Logger, reason string) {
	if err := w.store.Release(ctx, l, reason); err != nil {
		log.WarnContext(ctx, "cannot release lease; it will expire", "err", err)
	}
}

// heartbeat extends the lease every Lease/3 and reads the kill state every
// KillPollInterval (or on a kill signal) until stopped. A lost lease, a
// cancellation request or a kill cancels the in-flight call.
func (w *Worker) heartbeat(ctx context.Context, l Lease, cancelCall context.CancelFunc, log *slog.Logger) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		t := time.NewTicker(w.o.Lease / 3)
		kt := time.NewTicker(w.o.KillPollInterval)
		defer t.Stop()
		defer kt.Stop()
		for {
			var cancelRequested bool
			var err error
			select {
			case <-done:
				return
			case <-t.C:
				cancelRequested, err = w.store.Heartbeat(ctx, l, w.o.Lease)
			case <-kt.C:
				// The frequent kill check only reads; the lease has its own tick.
				cancelRequested, err = w.store.CheckKill(ctx, l)
			case <-w.killSignal():
				cancelRequested, err = w.store.CheckKill(ctx, l)
			}
			switch {
			case errors.Is(err, ErrLeaseLost):
				log.WarnContext(ctx, "lease lost during the call; cancelling it")
				cancelCall()
				return
			case err != nil:
				log.WarnContext(ctx, "heartbeat failed", "err", err)
			case cancelRequested:
				cancelCall()
			}
		}
	})
	return func() { close(done); wg.Wait() }
}

// safeExecute calls the connector; a panic is an ambiguous result.
func safeExecute(ctx context.Context, c Connector, call Call) (r Result) {
	defer func() {
		if recover() != nil {
			r = Result{Outcome: Ambiguous, ErrorClass: "connector_panic"}
		}
	}()
	return c.Execute(ctx, call)
}

// scrub drops any connector-returned field that contains a credential
// (ADR-001 §3): results are stored in attempts, the journal and the
// outbox. A success whose reference is dropped becomes ambiguous.
func scrub(r Result, secrets []string) Result {
	for _, s := range secrets {
		if s == "" {
			continue
		}
		if strings.Contains(r.ExternalReference, s) {
			r.ExternalReference = ""
			r.Outcome = Ambiguous
		}
		if strings.Contains(r.ErrorClass, s) {
			r.ErrorClass = "redacted"
		}
	}
	return r
}

// classify enforces the definitive-result rules: a success needs an
// external reference, a no-effect needs a certified error class; anything
// else is ambiguous.
func classify(r Result, c Contract) Result {
	class := r.ErrorClass
	if !errorClassPattern.MatchString(class) {
		class = ""
	}
	ref := strings.TrimSpace(r.ExternalReference)
	switch {
	case r.Outcome == Succeeded && ref != "" && len(ref) <= 512:
		return Result{Outcome: Succeeded, ExternalReference: ref}
	case r.Outcome == NoEffect && ref == "" && class != "" && slices.Contains(c.NoEffectErrors, class):
		return Result{Outcome: NoEffect, ErrorClass: class}
	default:
		return Result{Outcome: Ambiguous, ErrorClass: class}
	}
}
