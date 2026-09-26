package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ScannerOptions configure a Scanner.
type ScannerOptions struct {
	// ID names this scanner in scan leases and the journal (the worker id).
	ID string
	// Interval is the time between two scans of a server (default 15m,
	// 1m-24h). A failed scan is retried sooner, with backoff.
	Interval time.Duration
	// Timeout bounds one discovery (default 30s, 1s-5m). The scan lease
	// lasts twice as long.
	Timeout time.Duration
	// Concurrency bounds the scans made at once (default 2).
	Concurrency int
	// PollInterval is the idle wait between looking for due servers
	// (default 10s).
	PollInterval time.Duration
	// Secrets are the worker-held credentials: a server is scanned only
	// with the token bound to its tenant, secret reference and host.
	Secrets    *SecretStore
	Discoverer Discoverer
	Log        *slog.Logger
}

// Scanner discovers the tools of MCP servers and records every scan
// (ADR-023 §2). It runs in the execution worker because discovery needs the
// server's credential. PostgreSQL fingerprints and classifies what it
// records; the scanner never decides trust.
type Scanner struct {
	store *Store
	o     ScannerOptions
}

// NewScanner returns a Scanner over pool (the application role).
func NewScanner(pool *pgxpool.Pool, o ScannerOptions) (*Scanner, error) {
	if !workerIDPattern.MatchString(o.ID) {
		return nil, fmt.Errorf("scanner: invalid id %q", o.ID)
	}
	if o.Interval == 0 {
		o.Interval = 15 * time.Minute
	}
	if o.Interval < time.Minute || o.Interval > 24*time.Hour {
		return nil, errors.New("scanner: interval must be between 1m and 24h")
	}
	if o.Timeout == 0 {
		o.Timeout = 30 * time.Second
	}
	if o.Timeout < time.Second || o.Timeout > 5*time.Minute {
		return nil, errors.New("scanner: timeout must be between 1s and 5m")
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 2
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 10 * time.Second
	}
	if o.Discoverer == nil {
		return nil, errors.New("scanner: a discoverer is required")
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Scanner{store: NewStore(pool, o.ID), o: o}, nil
}

// RunOnce claims up to Concurrency due servers, scans them concurrently and
// returns how many it scanned.
func (s *Scanner) RunOnce(ctx context.Context) (int, error) {
	cands, err := s.store.ScansDue(ctx, s.o.Secrets.Available(), s.o.Concurrency*2)
	if err != nil {
		return 0, err
	}
	var wg sync.WaitGroup
	n := 0
	for _, c := range cands {
		if n == s.o.Concurrency {
			break
		}
		l, ok, err := s.store.ClaimScan(ctx, c, 2*s.o.Timeout)
		if err != nil {
			wg.Wait()
			return n, err
		}
		if ok {
			n++
			wg.Go(func() { s.scan(ctx, l) })
		}
	}
	wg.Wait()
	return n, nil
}

// Run scans until ctx is cancelled.
func (s *Scanner) Run(ctx context.Context) {
	for ctx.Err() == nil {
		n, err := s.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			s.o.Log.ErrorContext(ctx, "mcp scan claim failed", "err", err)
		}
		if n > 0 {
			continue
		}
		select {
		case <-ctx.Done():
		case <-time.After(s.o.PollInterval):
		}
	}
}

// scan discovers one claimed server with no transaction open and records
// the result under the lease. A scan interrupted by shutdown is not
// recorded: its lease expires and another scanner retries.
func (s *Scanner) scan(ctx context.Context, l ScanLease) {
	log := s.o.Log.With("tenant_id", l.TenantID, "connector_id", l.ConnectorID, "generation", l.Generation)
	var d Discovery
	secret, err := s.o.Secrets.Credential(ctx, l.TenantID, l.SecretRef, l.Endpoint, s.o.Timeout+CredentialSkew)
	if errors.Is(err, ErrCredentialUnavailable) {
		// Not recorded: the scan lease expires and the scan is retried once
		// the credential is available again (ADR-019).
		log.WarnContext(ctx, "credential unavailable; mcp scan not recorded")
		return
	}
	if err != nil {
		err = &DiscoveryError{Class: "no_credential", Err: errors.New("no credential bound to this server")}
	} else {
		dctx, cancel := context.WithTimeout(ctx, s.o.Timeout)
		d, err = s.o.Discoverer.Discover(dctx, l.Endpoint, secret)
		cancel()
	}
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		log.WarnContext(ctx, "mcp scan failed", "class", DiscoveryClass(err))
		if DiscoveryClass(err) == "unauthorized" {
			s.o.Secrets.Rejected(l.TenantID, l.SecretRef, secret)
		}
	}
	if rerr := s.store.RecordScan(ctx, l, d, err, s.o.Interval); rerr != nil && ctx.Err() == nil {
		log.ErrorContext(ctx, "mcp scan not recorded", "err", rerr)
		return
	}
	if err == nil {
		log.InfoContext(ctx, "mcp scan recorded", "tools", len(d.Tools), "rejected", len(d.Rejected),
			"protocol_version", d.ProtocolVersion)
	}
}
