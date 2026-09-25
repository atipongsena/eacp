// Package config loads service configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Options controls which settings a given service requires.
type Options struct {
	// RequireDatabase makes EACP_DATABASE_URL mandatory.
	RequireDatabase bool
	// DefaultHTTPAddr is used when EACP_HTTP_ADDR is unset.
	DefaultHTTPAddr string
	// AllowConnectorSecrets marks the execution worker: only it may hold
	// connector credentials (ADR-001 §3). Any other service refuses to
	// start when EACP_CONNECTOR_SECRETS_FILE is set.
	AllowConnectorSecrets bool
}

// Config is the process configuration shared by EACP services.
type Config struct {
	Environment     string
	HTTPAddr        string
	DatabaseURL     string
	LogLevel        slog.Level
	LogFormat       string
	OTelExporter    string
	OTelEndpoint    string
	ShutdownTimeout time.Duration

	// Action API (controlplane-api): admission limits on released and on
	// unreleased unfinished actions (MASTER_PLAN §26, ADR-022 §1), the
	// sweeper interval and the bound on one governance (PDP) call.
	MaxQueuedPerTenant  int64
	MaxQueuedGlobal     int64
	MaxPendingPerTenant int64
	SweepInterval       time.Duration
	PDPTimeout          time.Duration
	// FinOpsInterval is how often controlplane-api runs the FinOps alert
	// evaluator (ADR-025 §8).
	FinOpsInterval time.Duration
	// ReleaseInterval is how often controlplane-api checks canary guardrails
	// and rolls back breached canaries (ADR-018 §6).
	ReleaseInterval time.Duration

	// IncidentInterval is how often controlplane-api runs the incident
	// evaluator (ADR-027 §4).
	IncidentInterval time.Duration

	// UI serves the operator console at /ui/ (controlplane-api, ADR-028):
	// EACP_UI is "on" (default) or "off".
	UI bool

	// Governance provider (controlplane-api, ADR-002 §8): "local" or
	// "microsoft-agt", the AGT sidecar PDP at AGTPDPURL. Plain http must
	// name a loopback host; https needs the mutual-TLS files. The client
	// checks both when the service starts.
	GovernanceProvider string
	AGTPDPURL          string
	AGTPDPCAFile       string
	AGTPDPCertFile     string
	AGTPDPKeyFile      string

	// Messaging (ADR-014): the optional NATS JetStream server for work hints
	// and dashboard events. NATS carries signals only; without it the
	// system is correct, only slower. Plain nats:// is for loopback hosts or
	// development and test; the URL's password is redacted from logs.
	NATSURL            string
	NATSCAFile         string
	NATSPublishTimeout time.Duration

	// Reconciliation (ADR-004 T33/T34): how many inconclusive lookups, and
	// how long an unknown outcome may last, before a human must resolve it.
	// The reconciler (execution-worker) applies both; the sweeper
	// (controlplane-api) also applies the age, for connectors no reconciler
	// serves.
	ReconcileMaxAttempts int
	ReconcileMaxAge      time.Duration

	// Execution worker (AllowConnectorSecrets only). WorkerID defaults to
	// the host name at startup; ConnectorSecretsFile is optional, and a
	// worker without credentials claims nothing.
	WorkerID             string
	WorkerLease          time.Duration
	WorkerConcurrency    int
	WorkerPollInterval   time.Duration
	ConnectorSecretsFile string

	// Worker bulkhead and circuit breaker (ADR-022 §2, §3): in-flight
	// actions per tenant and capacity group (default half the concurrency),
	// consecutive failures that open a connector's breaker, and its first
	// cooldown.
	WorkerGroupConcurrency int
	WorkerBreakerFailures  int
	WorkerBreakerCooldown  time.Duration

	// MCP scanner (ADR-023 §2): how often each MCP server's tools are
	// listed and how long one discovery may take.
	MCPScanInterval time.Duration
	MCPScanTimeout  time.Duration
}

var (
	environments = map[string]bool{"development": true, "test": true, "staging": true, "production": true}
	logFormats   = map[string]bool{"json": true, "text": true}
	exporters    = map[string]bool{"none": true, "stdout": true, "otlp": true}
	workerID     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

// Load reads configuration using getenv (normally os.Getenv).
// It returns every validation problem at once rather than the first one.
func Load(getenv func(string) string, opts Options) (Config, error) {
	get := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}

	cfg := Config{
		Environment:  get("EACP_ENV", "development"),
		HTTPAddr:     get("EACP_HTTP_ADDR", opts.DefaultHTTPAddr),
		DatabaseURL:  get("EACP_DATABASE_URL", ""),
		LogFormat:    get("EACP_LOG_FORMAT", "json"),
		OTelExporter: get("EACP_OTEL_EXPORTER", "none"),
		OTelEndpoint: get("EACP_OTEL_ENDPOINT", ""),
	}

	var errs []error
	if !environments[cfg.Environment] {
		errs = append(errs, fmt.Errorf("EACP_ENV: unknown environment %q", cfg.Environment))
	}
	if opts.RequireDatabase && cfg.DatabaseURL == "" {
		errs = append(errs, errors.New("EACP_DATABASE_URL: required"))
	}
	if cfg.DatabaseURL != "" {
		// Only URL-form DSNs are accepted: keyword DSNs ("host=... password=...")
		// cannot be reliably redacted from logs.
		u, err := url.Parse(cfg.DatabaseURL)
		if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
			errs = append(errs, errors.New("EACP_DATABASE_URL: must be a postgres:// or postgresql:// URL"))
		}
	}
	if err := cfg.LogLevel.UnmarshalText([]byte(get("EACP_LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("EACP_LOG_LEVEL: %w", err))
	}
	if !logFormats[cfg.LogFormat] {
		errs = append(errs, fmt.Errorf("EACP_LOG_FORMAT: unknown format %q", cfg.LogFormat))
	}
	if !exporters[cfg.OTelExporter] {
		errs = append(errs, fmt.Errorf("EACP_OTEL_EXPORTER: unknown exporter %q", cfg.OTelExporter))
	}
	if cfg.OTelExporter == "otlp" && cfg.OTelEndpoint == "" {
		errs = append(errs, errors.New("EACP_OTEL_ENDPOINT: required when EACP_OTEL_EXPORTER=otlp"))
	}
	timeout, err := time.ParseDuration(get("EACP_SHUTDOWN_TIMEOUT", "15s"))
	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("EACP_SHUTDOWN_TIMEOUT: %w", err))
	case timeout <= 0:
		errs = append(errs, errors.New("EACP_SHUTDOWN_TIMEOUT: must be positive"))
	}
	cfg.ShutdownTimeout = timeout

	limit := func(key, def string) int64 {
		n, err := strconv.ParseInt(get(key, def), 10, 64)
		if err != nil || n <= 0 {
			errs = append(errs, fmt.Errorf("%s: must be a positive integer", key))
		}
		return n
	}
	cfg.MaxQueuedPerTenant = limit("EACP_ACTION_MAX_QUEUED_PER_TENANT", "1000")
	cfg.MaxQueuedGlobal = limit("EACP_ACTION_MAX_QUEUED_GLOBAL", "10000")
	if cfg.MaxQueuedPerTenant > cfg.MaxQueuedGlobal && cfg.MaxQueuedGlobal > 0 {
		errs = append(errs, errors.New("EACP_ACTION_MAX_QUEUED_PER_TENANT: must not exceed EACP_ACTION_MAX_QUEUED_GLOBAL"))
	}
	cfg.MaxPendingPerTenant = limit("EACP_ACTION_MAX_PENDING_PER_TENANT", "1000")
	duration := func(key, def string, max time.Duration) time.Duration {
		d, err := time.ParseDuration(get(key, def))
		if err != nil || d <= 0 || d > max {
			errs = append(errs, fmt.Errorf("%s: must be a duration in (0, %v]", key, max))
		}
		return d
	}
	cfg.SweepInterval = duration("EACP_ACTION_SWEEP_INTERVAL", "1s", time.Hour)
	cfg.PDPTimeout = duration("EACP_PDP_TIMEOUT", "5s", time.Minute)
	cfg.FinOpsInterval = duration("EACP_FINOPS_INTERVAL", "1m", time.Hour)
	if cfg.FinOpsInterval > 0 && cfg.FinOpsInterval < 10*time.Second {
		errs = append(errs, errors.New("EACP_FINOPS_INTERVAL: must be at least 10s"))
	}
	cfg.ReleaseInterval = duration("EACP_RELEASE_INTERVAL", "30s", time.Hour)
	if cfg.ReleaseInterval > 0 && cfg.ReleaseInterval < 10*time.Second {
		errs = append(errs, errors.New("EACP_RELEASE_INTERVAL: must be at least 10s"))
	}
	cfg.IncidentInterval = duration("EACP_INCIDENT_INTERVAL", "15s", time.Hour)
	if cfg.IncidentInterval > 0 && cfg.IncidentInterval < 5*time.Second {
		errs = append(errs, errors.New("EACP_INCIDENT_INTERVAL: must be at least 5s"))
	}
	switch get("EACP_UI", "on") {
	case "on":
		cfg.UI = true
	case "off":
		cfg.UI = false
	default:
		errs = append(errs, errors.New(`EACP_UI: must be "on" or "off"`))
	}
	cfg.GovernanceProvider = get("EACP_GOVERNANCE_PROVIDER", "local")
	cfg.AGTPDPURL = get("EACP_AGT_PDP_URL", "")
	cfg.AGTPDPCAFile = get("EACP_AGT_PDP_CA_FILE", "")
	cfg.AGTPDPCertFile = get("EACP_AGT_PDP_CERT_FILE", "")
	cfg.AGTPDPKeyFile = get("EACP_AGT_PDP_KEY_FILE", "")
	switch cfg.GovernanceProvider {
	case "local":
		if cfg.AGTPDPURL != "" {
			errs = append(errs, errors.New("EACP_AGT_PDP_URL: set, but EACP_GOVERNANCE_PROVIDER is not microsoft-agt"))
		}
	case "microsoft-agt":
		if cfg.AGTPDPURL == "" {
			errs = append(errs, errors.New("EACP_AGT_PDP_URL: required when EACP_GOVERNANCE_PROVIDER=microsoft-agt"))
		}
	default:
		errs = append(errs, fmt.Errorf("EACP_GOVERNANCE_PROVIDER: unknown provider %q (local or microsoft-agt)", cfg.GovernanceProvider))
	}
	cfg.NATSURL = get("EACP_NATS_URL", "")
	cfg.NATSCAFile = get("EACP_NATS_CA_FILE", "")
	cfg.NATSPublishTimeout = duration("EACP_NATS_PUBLISH_TIMEOUT", "5s", time.Minute)
	if cfg.NATSURL != "" {
		errs = append(errs, checkNATSURL(cfg.NATSURL, cfg.NATSCAFile, cfg.Environment)...)
	} else if cfg.NATSCAFile != "" {
		errs = append(errs, errors.New("EACP_NATS_CA_FILE: set, but EACP_NATS_URL is not"))
	}
	attempts, err := strconv.Atoi(get("EACP_RECONCILE_MAX_ATTEMPTS", "10"))
	if err != nil || attempts < 1 || attempts > 50 {
		errs = append(errs, errors.New("EACP_RECONCILE_MAX_ATTEMPTS: must be an integer in [1, 50]"))
	}
	cfg.ReconcileMaxAttempts = attempts
	cfg.ReconcileMaxAge = duration("EACP_RECONCILE_MAX_AGE", "1h", 24*time.Hour)

	if opts.AllowConnectorSecrets {
		cfg.WorkerID = get("EACP_WORKER_ID", "")
		if cfg.WorkerID != "" && !workerID.MatchString(cfg.WorkerID) {
			errs = append(errs, errors.New("EACP_WORKER_ID: 1-128 characters of [A-Za-z0-9._:-]"))
		}
		cfg.WorkerLease = duration("EACP_WORKER_LEASE", "30s", 5*time.Minute)
		if cfg.WorkerLease > 0 && cfg.WorkerLease < 5*time.Second {
			errs = append(errs, errors.New("EACP_WORKER_LEASE: must be at least 5s"))
		}
		n, err := strconv.Atoi(get("EACP_WORKER_CONCURRENCY", "4"))
		if err != nil || n <= 0 || n > 256 {
			errs = append(errs, errors.New("EACP_WORKER_CONCURRENCY: must be an integer in [1, 256]"))
		}
		cfg.WorkerConcurrency = n
		g, err := strconv.Atoi(get("EACP_WORKER_GROUP_CONCURRENCY", strconv.Itoa(max(n/2, 1))))
		if err != nil || g <= 0 || g > n {
			errs = append(errs, errors.New("EACP_WORKER_GROUP_CONCURRENCY: must be an integer in [1, EACP_WORKER_CONCURRENCY]"))
		}
		cfg.WorkerGroupConcurrency = g
		f, err := strconv.Atoi(get("EACP_WORKER_BREAKER_FAILURES", "5"))
		if err != nil || f < 1 || f > 100 {
			errs = append(errs, errors.New("EACP_WORKER_BREAKER_FAILURES: must be an integer in [1, 100]"))
		}
		cfg.WorkerBreakerFailures = f
		cfg.WorkerBreakerCooldown = duration("EACP_WORKER_BREAKER_COOLDOWN", "30s", 10*time.Minute)
		if cfg.WorkerBreakerCooldown > 0 && cfg.WorkerBreakerCooldown < time.Second {
			errs = append(errs, errors.New("EACP_WORKER_BREAKER_COOLDOWN: must be at least 1s"))
		}
		cfg.WorkerPollInterval = duration("EACP_WORKER_POLL_INTERVAL", "500ms", time.Minute)
		cfg.MCPScanInterval = duration("EACP_MCP_SCAN_INTERVAL", "15m", 24*time.Hour)
		if cfg.MCPScanInterval > 0 && cfg.MCPScanInterval < time.Minute {
			errs = append(errs, errors.New("EACP_MCP_SCAN_INTERVAL: must be at least 1m"))
		}
		cfg.MCPScanTimeout = duration("EACP_MCP_SCAN_TIMEOUT", "30s", 5*time.Minute)
		if cfg.MCPScanTimeout > 0 && cfg.MCPScanTimeout < time.Second {
			errs = append(errs, errors.New("EACP_MCP_SCAN_TIMEOUT: must be at least 1s"))
		}
		cfg.ConnectorSecretsFile = get("EACP_CONNECTOR_SECRETS_FILE", "")
	} else if get("EACP_CONNECTOR_SECRETS_FILE", "") != "" {
		errs = append(errs, errors.New("EACP_CONNECTOR_SECRETS_FILE: only the execution worker may hold connector credentials (ADR-001 §3)"))
	}

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return cfg, nil
}

// LogValue implements slog.LogValuer so that logging a Config never leaks
// the database password.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("environment", c.Environment),
		slog.String("http_addr", c.HTTPAddr),
		slog.String("database_url", RedactURL(c.DatabaseURL)),
		slog.String("log_level", c.LogLevel.String()),
		slog.String("log_format", c.LogFormat),
		slog.String("otel_exporter", c.OTelExporter),
		slog.String("otel_endpoint", c.OTelEndpoint),
		slog.Duration("shutdown_timeout", c.ShutdownTimeout),
		slog.Int64("action_max_queued_per_tenant", c.MaxQueuedPerTenant),
		slog.Int64("action_max_queued_global", c.MaxQueuedGlobal),
		slog.Int64("action_max_pending_per_tenant", c.MaxPendingPerTenant),
		slog.Duration("action_sweep_interval", c.SweepInterval),
		slog.Duration("pdp_timeout", c.PDPTimeout),
		slog.Duration("finops_interval", c.FinOpsInterval),
		slog.Duration("release_interval", c.ReleaseInterval),
		slog.Duration("incident_interval", c.IncidentInterval),
		slog.Bool("ui", c.UI),
		slog.String("governance_provider", c.GovernanceProvider),
		slog.String("agt_pdp_url", RedactURL(c.AGTPDPURL)),
		slog.String("agt_pdp_cert_file", c.AGTPDPCertFile),
		slog.String("nats_url", RedactURL(c.NATSURL)),
		slog.String("nats_ca_file", c.NATSCAFile),
		slog.Duration("nats_publish_timeout", c.NATSPublishTimeout),
		slog.Int("reconcile_max_attempts", c.ReconcileMaxAttempts),
		slog.Duration("reconcile_max_age", c.ReconcileMaxAge),
		slog.String("worker_id", c.WorkerID),
		slog.Duration("worker_lease", c.WorkerLease),
		slog.Int("worker_concurrency", c.WorkerConcurrency),
		slog.Int("worker_group_concurrency", c.WorkerGroupConcurrency),
		slog.Int("worker_breaker_failures", c.WorkerBreakerFailures),
		slog.Duration("worker_breaker_cooldown", c.WorkerBreakerCooldown),
		slog.Duration("worker_poll_interval", c.WorkerPollInterval),
		slog.Duration("mcp_scan_interval", c.MCPScanInterval),
		slog.Duration("mcp_scan_timeout", c.MCPScanTimeout),
		slog.String("connector_secrets_file", c.ConnectorSecretsFile),
	)
}

// checkNATSURL accepts one nats:// or tls:// server URL (ADR-014 §6). Plain
// nats:// must name a loopback host unless the environment is development
// or test; a CA file needs tls://.
func checkNATSURL(raw, caFile, environment string) []error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "nats" && u.Scheme != "tls") || u.Hostname() == "" ||
		strings.Contains(raw, ",") || (u.Path != "" && u.Path != "/") {
		return []error{errors.New("EACP_NATS_URL: must be one nats:// or tls:// server URL")}
	}
	var errs []error
	if u.Scheme == "nats" {
		if caFile != "" {
			errs = append(errs, errors.New("EACP_NATS_CA_FILE: needs a tls:// EACP_NATS_URL"))
		}
		if environment != "development" && environment != "test" && !isLoopback(u.Hostname()) {
			errs = append(errs, fmt.Errorf("EACP_NATS_URL: plain nats:// is allowed only for a loopback host in %s; use tls://", environment))
		}
	}
	return errs
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// RedactURL removes the password from a URL-form DSN. Unparsable input is
// fully redacted, because it may still contain a secret.
func RedactURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "[REDACTED]"
	}
	return u.Redacted()
}
