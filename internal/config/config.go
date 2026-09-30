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
	// AllowProviderSecrets marks the LLM gateway: only it may hold LLM
	// provider credentials (ADR-031). It requires EACP_LLM_SECRETS_FILE and
	// refuses connector secrets; any other service refuses to start when
	// EACP_LLM_SECRETS_FILE is set.
	AllowProviderSecrets bool
	// StudioRuntime marks agent-runtime (ADR-033 §4): only it may hold the
	// Studio master (EACP_STUDIO_MASTER_FILE). It reaches EACP only through
	// the API and refuses a database URL and every other secret file.
	StudioRuntime bool
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
	// ShutdownDelay is how long a stopping service keeps serving, reporting
	// not-ready, before its graceful HTTP shutdown (EACP_SHUTDOWN_DELAY,
	// 0s-60s, default 0s), so a load balancer stops routing to it first.
	ShutdownDelay time.Duration

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

	// LLM gateway (AllowProviderSecrets only, ADR-031): the provider
	// credentials, how often a running call re-checks the kill epoch, how
	// often overdue calls are abandoned, the request body bound and this
	// replica's id (default the host name at startup).
	LLMSecretsFile     string
	LLMKillPoll        time.Duration
	LLMSweepInterval   time.Duration
	LLMMaxRequestBytes int64
	LLMID              string

	// Agent runtime (StudioRuntime only, ADR-033 §4): the API it calls, its
	// principal keys (one per tenant), the Studio master and its version,
	// this replica's id (default the host name at startup), the run lease,
	// how many runs it drives at once, how often it claims and how often it
	// proposes due keys.
	APIURL                string
	RuntimeKeyFile        string
	StudioMasterFile      string
	StudioMasterVersion   string
	RuntimeID             string
	RuntimeLease          time.Duration
	RuntimeConcurrency    int
	RuntimePollInterval   time.Duration
	RuntimeRotateInterval time.Duration
}

var (
	environments = map[string]bool{"development": true, "test": true, "staging": true, "production": true}
	logFormats   = map[string]bool{"json": true, "text": true}
	exporters    = map[string]bool{"none": true, "stdout": true, "otlp": true}
	workerID     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	masterVer    = regexp.MustCompile(`^v[0-9]{1,4}$`)
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
	delay, err := time.ParseDuration(get("EACP_SHUTDOWN_DELAY", "0s"))
	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("EACP_SHUTDOWN_DELAY: %w", err))
	case delay < 0 || delay > time.Minute:
		errs = append(errs, errors.New("EACP_SHUTDOWN_DELAY: must be between 0s and 60s"))
	}
	cfg.ShutdownDelay = delay

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

	if opts.AllowProviderSecrets {
		if opts.AllowConnectorSecrets {
			errs = append(errs, errors.New("a service holds connector or provider credentials, never both (ADR-031)"))
		}
		cfg.LLMSecretsFile = get("EACP_LLM_SECRETS_FILE", "")
		if cfg.LLMSecretsFile == "" {
			errs = append(errs, errors.New("EACP_LLM_SECRETS_FILE: required by the LLM gateway"))
		}
		cfg.LLMKillPoll = duration("EACP_LLM_KILL_POLL", "2s", 30*time.Second)
		if cfg.LLMKillPoll > 0 && cfg.LLMKillPoll < 500*time.Millisecond {
			errs = append(errs, errors.New("EACP_LLM_KILL_POLL: must be at least 500ms"))
		}
		cfg.LLMSweepInterval = duration("EACP_LLM_SWEEP_INTERVAL", "30s", time.Hour)
		if cfg.LLMSweepInterval > 0 && cfg.LLMSweepInterval < 5*time.Second {
			errs = append(errs, errors.New("EACP_LLM_SWEEP_INTERVAL: must be at least 5s"))
		}
		n, err := strconv.ParseInt(get("EACP_LLM_MAX_REQUEST_BYTES", "4194304"), 10, 64)
		if err != nil || n < 1 || n > 32<<20 {
			errs = append(errs, errors.New("EACP_LLM_MAX_REQUEST_BYTES: must be an integer in [1, 33554432]"))
		}
		cfg.LLMMaxRequestBytes = n
		cfg.LLMID = get("EACP_LLM_ID", "")
		if cfg.LLMID != "" && !workerID.MatchString(cfg.LLMID) {
			errs = append(errs, errors.New("EACP_LLM_ID: 1-128 characters of [A-Za-z0-9._:-]"))
		}
	} else if get("EACP_LLM_SECRETS_FILE", "") != "" {
		errs = append(errs, errors.New("EACP_LLM_SECRETS_FILE: only the LLM gateway may hold provider credentials (ADR-031)"))
	}

	if opts.StudioRuntime {
		if opts.RequireDatabase || opts.AllowConnectorSecrets || opts.AllowProviderSecrets {
			errs = append(errs, errors.New("agent-runtime holds no database, connector or provider access (ADR-033 §4)"))
		}
		if cfg.DatabaseURL != "" {
			errs = append(errs, errors.New("EACP_DATABASE_URL: agent-runtime reaches EACP only through the API (ADR-033 §4)"))
		}
		cfg.APIURL = get("EACP_API_URL", "")
		if u, err := url.Parse(cfg.APIURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") ||
			u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			errs = append(errs, errors.New("EACP_API_URL: required, an http:// or https:// origin"))
		}
		cfg.APIURL = strings.TrimSuffix(cfg.APIURL, "/")
		cfg.RuntimeKeyFile = get("EACP_RUNTIME_KEY_FILE", "")
		if cfg.RuntimeKeyFile == "" {
			errs = append(errs, errors.New("EACP_RUNTIME_KEY_FILE: required by agent-runtime"))
		}
		cfg.StudioMasterFile = get("EACP_STUDIO_MASTER_FILE", "")
		if cfg.StudioMasterFile == "" {
			errs = append(errs, errors.New("EACP_STUDIO_MASTER_FILE: required by agent-runtime"))
		}
		cfg.StudioMasterVersion = get("EACP_STUDIO_MASTER_VERSION", "v1")
		if !masterVer.MatchString(cfg.StudioMasterVersion) {
			errs = append(errs, errors.New("EACP_STUDIO_MASTER_VERSION: v followed by 1-4 digits"))
		}
		cfg.RuntimeID = get("EACP_RUNTIME_ID", "")
		if cfg.RuntimeID != "" && !workerID.MatchString(cfg.RuntimeID) {
			errs = append(errs, errors.New("EACP_RUNTIME_ID: 1-128 characters of [A-Za-z0-9._:-]"))
		}
		cfg.RuntimeLease = duration("EACP_RUNTIME_LEASE", "30s", 5*time.Minute)
		if cfg.RuntimeLease > 0 && cfg.RuntimeLease < 5*time.Second {
			errs = append(errs, errors.New("EACP_RUNTIME_LEASE: must be at least 5s"))
		}
		n, err := strconv.Atoi(get("EACP_RUNTIME_CONCURRENCY", "4"))
		if err != nil || n <= 0 || n > 64 {
			errs = append(errs, errors.New("EACP_RUNTIME_CONCURRENCY: must be an integer in [1, 64]"))
		}
		cfg.RuntimeConcurrency = n
		cfg.RuntimePollInterval = duration("EACP_RUNTIME_POLL_INTERVAL", "1s", time.Minute)
		cfg.RuntimeRotateInterval = duration("EACP_RUNTIME_ROTATE_INTERVAL", "1m", time.Hour)
		if cfg.RuntimeRotateInterval > 0 && cfg.RuntimeRotateInterval < 5*time.Second {
			errs = append(errs, errors.New("EACP_RUNTIME_ROTATE_INTERVAL: must be at least 5s"))
		}
	} else if get("EACP_STUDIO_MASTER_FILE", "") != "" {
		errs = append(errs, errors.New("EACP_STUDIO_MASTER_FILE: only agent-runtime may hold the Studio master (ADR-033 §4)"))
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
		slog.Duration("shutdown_delay", c.ShutdownDelay),
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
		slog.String("llm_secrets_file", c.LLMSecretsFile),
		slog.Duration("llm_kill_poll", c.LLMKillPoll),
		slog.Duration("llm_sweep_interval", c.LLMSweepInterval),
		slog.Int64("llm_max_request_bytes", c.LLMMaxRequestBytes),
		slog.String("llm_id", c.LLMID),
		slog.String("api_url", RedactURL(c.APIURL)),
		slog.String("runtime_key_file", c.RuntimeKeyFile),
		slog.String("studio_master_file", c.StudioMasterFile),
		slog.String("studio_master_version", c.StudioMasterVersion),
		slog.String("runtime_id", c.RuntimeID),
		slog.Duration("runtime_lease", c.RuntimeLease),
		slog.Int("runtime_concurrency", c.RuntimeConcurrency),
		slog.Duration("runtime_poll_interval", c.RuntimePollInterval),
		slog.Duration("runtime_rotate_interval", c.RuntimeRotateInterval),
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
