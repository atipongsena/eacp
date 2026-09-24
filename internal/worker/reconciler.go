package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ReconcilerOptions configure a Reconciler.
type ReconcilerOptions struct {
	// ID names this reconciler in leases, checks and the journal.
	ID string
	// Lease is how long a reconciler claim holds an action (default 30s,
	// 5s-5m). A lookup runs within half of it; there is no heartbeat.
	Lease time.Duration
	// Concurrency bounds the lookups made at once (default 4).
	Concurrency int
	// PollInterval is the idle wait between claims (default 1s).
	PollInterval time.Duration
	// MaxAttempts and MaxAge bound reconciliation (defaults 10 and 1h). Once
	// either is exhausted an inconclusive check goes to a human (T34),
	// never to FAILED.
	MaxAttempts int
	MaxAge      time.Duration
	// Backoff is the delay before reconciliation attempt n+1 after attempt
	// n (default 1s doubling, at most 5m, with equal jitter), and before a
	// retry permitted by authoritative negative evidence.
	Backoff func(attempt int) time.Duration
	// Connectors by protocol and the worker-held credentials (ADR-001 §3):
	// a lookup authenticates like the call it looks for.
	Connectors map[string]Connector
	Secrets    *SecretStore
	Log        *slog.Logger
}

// Reconciler resolves UNKNOWN_OUTCOME actions with evidence (ADR-004
// T28, T30-T34; MASTER_PLAN §20). It runs in the execution worker because
// a lookup needs the connector credential.
type Reconciler struct {
	store     *Store
	o         ReconcilerOptions
	protocols []string
}

// NewReconciler returns a Reconciler over pool (the application role).
func NewReconciler(pool *pgxpool.Pool, o ReconcilerOptions) (*Reconciler, error) {
	if !workerIDPattern.MatchString(o.ID) {
		return nil, fmt.Errorf("reconciler: invalid id %q", o.ID)
	}
	if o.Lease == 0 {
		o.Lease = 30 * time.Second
	}
	if o.Lease < 5*time.Second || o.Lease > 5*time.Minute {
		return nil, errors.New("reconciler: lease must be between 5s and 5m")
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 4
	}
	if o.PollInterval <= 0 {
		o.PollInterval = time.Second
	}
	if o.MaxAttempts == 0 {
		o.MaxAttempts = 10
	}
	if o.MaxAttempts < 1 || o.MaxAttempts > 50 {
		return nil, errors.New("reconciler: max attempts must be between 1 and 50")
	}
	if o.MaxAge <= 0 {
		o.MaxAge = time.Hour
	}
	if o.Backoff == nil {
		o.Backoff = jitteredBackoff
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	r := &Reconciler{store: NewStore(pool, o.ID), o: o}
	for p := range o.Connectors {
		r.protocols = append(r.protocols, p)
	}
	sort.Strings(r.protocols)
	return r, nil
}

// RunOnce claims up to Concurrency due actions, reconciles them
// concurrently and returns how many it reconciled.
func (r *Reconciler) RunOnce(ctx context.Context) (int, error) {
	cands, err := r.store.Reconcilable(ctx, r.protocols, r.o.Secrets.Bindings(), r.o.Concurrency*2)
	if err != nil {
		return 0, err
	}
	var wg sync.WaitGroup
	n := 0
	for _, c := range cands {
		if n == r.o.Concurrency {
			break
		}
		l, ok, err := r.store.ClaimReconcile(ctx, c, r.o.Lease)
		if err != nil {
			wg.Wait()
			return n, err
		}
		if ok {
			n++
			wg.Go(func() { r.reconcile(ctx, l) })
		}
	}
	wg.Wait()
	return n, nil
}

// Run reconciles until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context) {
	for ctx.Err() == nil {
		n, err := r.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			r.o.Log.ErrorContext(ctx, "reconciliation claim failed", "err", err)
		}
		if n > 0 {
			continue
		}
		select {
		case <-ctx.Done():
		case <-time.After(r.o.PollInterval):
		}
	}
}

// reconcile looks up one claimed action and commits the verdict. Nothing
// here dispatches: a permitted retry goes through RETRY_WAIT, the sweeper
// and a fresh fenced dispatch intent with the same operation key.
func (r *Reconciler) reconcile(ctx context.Context, l Lease) {
	ctx = context.WithoutCancel(ctx)
	log := r.o.Log.With("tenant", l.TenantID.String(), "action", l.ActionID.String(), "generation", l.Generation)
	job, err := r.store.loadReconcile(ctx, l)
	if err != nil {
		log.WarnContext(ctx, "cannot load reconciling action", "err", err)
		return
	}
	job.Facts.Exhausted = job.Attempts+1 >= r.o.MaxAttempts || job.age >= r.o.MaxAge

	res := LookupResult{Status: LookupUnknown}
	conn := r.o.Connectors[job.Protocol]
	secret, err := r.o.Secrets.Resolve(l.TenantID, job.SecretRef, job.Endpoint)
	if conn != nil && err == nil {
		lookupCtx, cancel := context.WithTimeout(ctx, min(job.Timeout, r.o.Lease/2))
		res = safeLookup(lookupCtx, conn, LookupCall{TenantID: l.TenantID, OperationKey: job.OperationKey,
			Endpoint: job.Endpoint, Secret: secret})
		cancel()
	} else {
		log.WarnContext(ctx, "reconciler cannot serve this connector", "protocol", job.Protocol)
	}
	res = scrubLookup(res, r.o.Secrets.Values())

	v := decide(job.Facts, res)
	// The database accepts a next retry or reconciliation within the hour.
	clamp := func(d time.Duration) time.Duration { return min(max(d, 100*time.Millisecond), time.Hour) }
	var next time.Duration
	switch v.To {
	case "RETRY_WAIT":
		next = clamp(r.o.Backoff(1))
	case "UNKNOWN_OUTCOME":
		next = clamp(r.o.Backoff(job.Attempts + 1))
	}
	if err := r.store.reconciled(ctx, l, v, next); err != nil {
		// The lease lapses and the sweeper returns the action to
		// UNKNOWN_OUTCOME (T33): nothing was decided.
		log.WarnContext(ctx, "cannot record reconciliation", "check", v.Check, "to", v.To, "err", err)
		return
	}
	log.InfoContext(ctx, "reconciled", "check", v.Check, "to", v.To, "attempt", job.Attempts+1)
}

// safeLookup calls the connector; a panic is an unknown result.
func safeLookup(ctx context.Context, c Connector, call LookupCall) (r LookupResult) {
	defer func() {
		if recover() != nil {
			r = LookupResult{Status: LookupUnknown}
		}
	}()
	return c.Lookup(ctx, call)
}

// scrubLookup drops a found reference that contains a credential (ADR-001
// §3); without its reference, found is unknown.
func scrubLookup(r LookupResult, secrets []string) LookupResult {
	ref := strings.TrimSpace(r.ExternalReference)
	for _, s := range secrets {
		if s != "" && strings.Contains(ref, s) {
			return LookupResult{Status: LookupUnknown}
		}
	}
	if r.Status == LookupFound && (ref == "" || len(ref) > 512) {
		return LookupResult{Status: LookupUnknown}
	}
	if r.Status != LookupFound {
		ref = ""
	}
	return LookupResult{Status: r.Status, ExternalReference: ref}
}

// reconcileFacts are the facts a verdict depends on, read under the lease.
type reconcileFacts struct {
	Proof        string   // the pinned proof standard: authoritative or best_effort
	Settled      bool     // every attempt's call deadline has settled
	RetryAllowed bool     // attempts remain, no cancel request, not_after ahead
	Reported     []string // external references of successes any attempt reported
	Exhausted    bool     // reconcile attempts or time used up
}

// verdict is the recorded check and the move it drives.
type verdict struct {
	Check             string // found, absent, unknown or conflict
	ExternalReference string // of a found record
	To                string // the action's next state
	Reason            string
}

// decide applies the proof standard (ADR-004 §20.2). Only a record found by
// operation key proves success; only AUTHORITATIVE absence, after every
// call settled and with no reported success, proves no effect. Everything
// else is still unknown, and once exhausted, a human decides.
func decide(f reconcileFacts, res LookupResult) verdict {
	still := func(check, why string) verdict {
		if f.Exhausted {
			return verdict{Check: check, To: "NEEDS_HUMAN_RESOLUTION", Reason: "reconciliation exhausted: " + why}
		}
		return verdict{Check: check, To: "UNKNOWN_OUTCOME", Reason: "still unknown: " + why}
	}
	switch res.Status {
	case LookupFound:
		if res.ExternalReference == "" {
			return still("unknown", "a record was reported without a reference")
		}
		for _, ref := range f.Reported {
			if ref != res.ExternalReference {
				return verdict{Check: "found", ExternalReference: res.ExternalReference, To: "NEEDS_HUMAN_RESOLUTION",
					Reason: "conflict: lookup found " + res.ExternalReference + " but an attempt reported " + ref}
			}
		}
		return verdict{Check: "found", ExternalReference: res.ExternalReference, To: "SUCCEEDED",
			Reason: "reconciled: found " + res.ExternalReference}
	case LookupConflict:
		return verdict{Check: "conflict", To: "NEEDS_HUMAN_RESOLUTION",
			Reason: "conflict: several records carry the operation key"}
	case LookupAbsent:
		switch {
		case f.Proof != "authoritative":
			return still("absent", "not found under a "+strings.ReplaceAll(f.Proof, "_", "-")+" proof standard")
		case len(f.Reported) > 0:
			return verdict{Check: "absent", To: "NEEDS_HUMAN_RESOLUTION",
				Reason: "conflict: not found, but an attempt reported " + f.Reported[0]}
		case !f.Settled:
			return still("absent", "not found before every call settled")
		case f.RetryAllowed:
			return verdict{Check: "absent", To: "RETRY_WAIT",
				Reason: "reconciled: authoritatively not executed; retry with the same operation key"}
		default:
			return verdict{Check: "absent", To: "FAILED", Reason: "reconciled: authoritatively not executed"}
		}
	}
	return still("unknown", "the lookup was inconclusive")
}

// clip bounds a state reason (at most 1024 bytes) without splitting a rune.
func clip(s string) string {
	const limit = 1000
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
