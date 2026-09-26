// Package service holds the startup sequence shared by every EACP binary:
// configuration, redacting logger, telemetry, database pool with safety
// checks, health probes and graceful HTTP serving.
package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"eacp/internal/config"
	"eacp/internal/health"
	"eacp/internal/httpserver"
	"eacp/internal/logging"
	"eacp/internal/storage"
	"eacp/internal/telemetry"
	"eacp/migrations"
)

// readinessTimeout bounds each /readyz evaluation.
const readinessTimeout = 2 * time.Second

// Deps are the initialised dependencies of a running service.
type Deps struct {
	Name   string
	Config config.Config
	Log    *slog.Logger
	// DB is nil for services started without RequireDatabase.
	DB *pgxpool.Pool

	out     io.Writer
	secrets []string

	bgCtx    context.Context
	bgCancel context.CancelFunc
	bg       sync.WaitGroup

	// draining is set when shutdown starts; /readyz then fails.
	draining atomic.Bool
}

// Background runs fn in its own goroutine until the service stops or the
// context given to Start ends. The stop function returned by Start cancels
// fn's context and waits for fn to return before it releases the database
// pool and telemetry.
func (d *Deps) Background(fn func(context.Context)) {
	d.bg.Go(func() { fn(d.bgCtx) })
}

// RedactSecrets replaces Log with a logger that also redacts values, such
// as connector credentials loaded after startup. Call it before handing Log
// to any component.
func (d *Deps) RedactSecrets(values ...string) {
	d.secrets = append(d.secrets, values...)
	d.Log = logging.New(d.out, d.Config.LogLevel, d.Config.LogFormat, d.secrets...).With("service", d.Name)
}

// Start initialises a service. It fails closed: invalid configuration, an
// unreachable database, a database role able to bypass Row-Level Security,
// or a schema older than this binary all abort startup. The returned stop
// function releases resources.
func Start(ctx context.Context, name string, getenv func(string) string, opts config.Options, out io.Writer) (*Deps, func(), error) {
	cfg, err := config.Load(getenv, opts)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: config: %w", name, err)
	}
	secrets := []string{dsnPassword(cfg.DatabaseURL), dsnPassword(cfg.NATSURL)}
	log := logging.New(out, cfg.LogLevel, cfg.LogFormat, secrets...).With("service", name)

	shutdownTelemetry, err := telemetry.Setup(ctx, telemetry.Options{
		ServiceName: name, Environment: cfg.Environment,
		Exporter: cfg.OTelExporter, Endpoint: cfg.OTelEndpoint,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("%s: telemetry: %w", name, err)
	}
	stopTelemetry := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTelemetry(ctx); err != nil {
			log.Warn("telemetry shutdown", "err", err)
		}
	}

	deps := &Deps{Name: name, Config: cfg, Log: log, out: out, secrets: secrets}
	deps.bgCtx, deps.bgCancel = context.WithCancel(ctx)
	if cfg.DatabaseURL != "" {
		pool, err := storage.Open(ctx, cfg.DatabaseURL)
		if err != nil {
			deps.bgCancel()
			stopTelemetry()
			return nil, nil, fmt.Errorf("%s: %w", name, err)
		}
		if err := storage.CheckRoleSafety(ctx, pool); err != nil {
			deps.bgCancel()
			pool.Close()
			stopTelemetry()
			return nil, nil, fmt.Errorf("%s: %w", name, err)
		}
		if err := storage.CheckSchemaVersion(ctx, pool, migrations.Latest()); err != nil {
			deps.bgCancel()
			pool.Close()
			stopTelemetry()
			return nil, nil, fmt.Errorf("%s: %w", name, err)
		}
		deps.DB = pool
	}

	log.Info("service started", "config", cfg)
	stop := func() {
		deps.bgCancel()
		deps.bg.Wait()
		if deps.DB != nil {
			deps.DB.Close()
		}
		stopTelemetry()
	}
	return deps, stop, nil
}

// Handler registers /healthz and /readyz on mux and returns it wrapped with
// OpenTelemetry HTTP instrumentation.
func (d *Deps) Handler(mux *http.ServeMux) http.Handler {
	health.Register(mux, d.Log, readinessTimeout, d.readinessChecks()...)
	return otelhttp.NewHandler(mux, d.Name)
}

// errDraining fails readiness once shutdown has started.
var errDraining = errors.New("service is shutting down")

func (d *Deps) readinessChecks() []health.Check {
	checks := []health.Check{{Name: "draining", Fn: func(context.Context) error {
		if d.draining.Load() {
			return errDraining
		}
		return nil
	}}}
	if d.DB == nil {
		return checks
	}
	return append(checks,
		health.Check{Name: "database", Fn: d.DB.Ping},
		health.Check{Name: "role_safety", Fn: func(ctx context.Context) error { return storage.CheckRoleSafety(ctx, d.DB) }},
		health.Check{Name: "schema_version", Fn: func(ctx context.Context) error {
			return storage.CheckSchemaVersion(ctx, d.DB, migrations.Latest())
		}},
	)
}

// Serve listens on the configured address and serves h until ctx is
// cancelled (see ServeOn).
func (d *Deps) Serve(ctx context.Context, h http.Handler) error {
	ln, err := net.Listen("tcp", d.Config.HTTPAddr)
	if err != nil {
		return fmt.Errorf("%s: listen: %w", d.Name, err)
	}
	return d.ServeOn(ctx, ln, h)
}

// ServeOn serves h on ln until ctx is cancelled, then drains (ADR-029):
// /readyz fails at once, responses close their connections, background
// tasks are cancelled, the listener keeps
// serving for EACP_SHUTDOWN_DELAY so a load balancer can stop routing here,
// and the graceful shutdown then waits up to EACP_SHUTDOWN_TIMEOUT for
// in-flight requests. A listener failure returns at once.
func (d *Deps) ServeOn(ctx context.Context, ln net.Listener, h http.Handler) error {
	d.Log.Info("http listening", "addr", ln.Addr().String())
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(d.Log.Handler(), slog.LevelWarn),
	}
	serving, stopServing := context.WithCancel(context.WithoutCancel(ctx))
	defer stopServing()
	go func() {
		select {
		case <-ctx.Done():
		case <-serving.Done(): // the server failed first
			return
		}
		d.draining.Store(true)
		// Responses now say Connection: close, so clients leave kept-alive
		// connections before the shutdown closes them under a request.
		srv.SetKeepAlivesEnabled(false)
		d.bgCancel()
		if delay := d.Config.ShutdownDelay; delay > 0 {
			d.Log.Info("draining: not ready, still serving", "delay", delay)
			t := time.NewTimer(delay)
			select {
			case <-t.C:
			case <-serving.Done():
				t.Stop()
			}
		}
		stopServing()
	}()
	err := httpserver.Serve(serving, srv, ln, d.Config.ShutdownTimeout)
	if err == nil {
		d.Log.Info("http stopped cleanly")
	} else if errors.Is(err, context.DeadlineExceeded) {
		d.Log.Warn("http shutdown timed out; remaining connections closed")
	}
	return err
}

// dsnPassword extracts the password from a URL-form DSN so the logger can
// redact it wherever it appears.
func dsnPassword(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		return ""
	}
	p, _ := u.User.Password()
	return p
}

// Main runs a probe-serving service process until SIGINT/SIGTERM and exits
// non-zero on startup or serving failure. It is the whole main() of every
// binary: register mounts the service's routes and may start background
// tasks with Deps.Background, which stop before the pool is closed. An
// error from register aborts startup (fail closed).
func Main(name string, opts config.Options, register func(*Deps, *http.ServeMux) error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	deps, cleanup, err := Start(ctx, name, os.Getenv, opts, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	mux := http.NewServeMux()
	if register != nil {
		if err := register(deps, mux); err != nil {
			cleanup()
			deps.Log.Error("startup", "err", err)
			os.Exit(1)
		}
	}
	err = deps.Serve(ctx, deps.Handler(mux))
	cleanup()
	if err != nil {
		deps.Log.Error("serve", "err", err)
		os.Exit(1)
	}
}
