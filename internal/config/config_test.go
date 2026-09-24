package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"EACP_DATABASE_URL": "postgres://app@localhost/eacp",
	}), Options{RequireDatabase: true, DefaultHTTPAddr: ":8080"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Environment != "development" {
		t.Errorf("Environment = %q, want development", cfg.Environment)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want :8080", cfg.HTTPAddr)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want INFO", cfg.LogLevel)
	}
	if cfg.LogFormat != "json" {
		t.Errorf("LogFormat = %q, want json", cfg.LogFormat)
	}
	if cfg.OTelExporter != "none" {
		t.Errorf("OTelExporter = %q, want none", cfg.OTelExporter)
	}
	if cfg.ShutdownTimeout != 15*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 15s", cfg.ShutdownTimeout)
	}
	if cfg.MaxQueuedPerTenant != 1000 || cfg.MaxQueuedGlobal != 10000 || cfg.MaxPendingPerTenant != 1000 {
		t.Errorf("admission limits = %d/%d/%d, want 1000/10000/1000",
			cfg.MaxQueuedPerTenant, cfg.MaxQueuedGlobal, cfg.MaxPendingPerTenant)
	}
	if cfg.SweepInterval != time.Second || cfg.PDPTimeout != 5*time.Second {
		t.Errorf("SweepInterval = %v, PDPTimeout = %v, want 1s and 5s", cfg.SweepInterval, cfg.PDPTimeout)
	}
	if cfg.ReconcileMaxAttempts != 10 || cfg.ReconcileMaxAge != time.Hour {
		t.Errorf("reconcile limits = %d %v, want 10 and 1h", cfg.ReconcileMaxAttempts, cfg.ReconcileMaxAge)
	}
}

func TestLoadReadsOverrides(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"EACP_ENV":              "production",
		"EACP_HTTP_ADDR":        "127.0.0.1:9000",
		"EACP_DATABASE_URL":     "postgres://app@db/eacp",
		"EACP_LOG_LEVEL":        "debug",
		"EACP_LOG_FORMAT":       "text",
		"EACP_OTEL_EXPORTER":    "otlp",
		"EACP_OTEL_ENDPOINT":    "collector:4318",
		"EACP_SHUTDOWN_TIMEOUT": "3s",

		"EACP_ACTION_MAX_QUEUED_PER_TENANT": "5",
		"EACP_ACTION_MAX_QUEUED_GLOBAL":     "50",
		"EACP_ACTION_SWEEP_INTERVAL":        "250ms",
		"EACP_PDP_TIMEOUT":                  "2s",
		"EACP_RECONCILE_MAX_ATTEMPTS":       "3",
		"EACP_RECONCILE_MAX_AGE":            "20m",
	}), Options{RequireDatabase: true, DefaultHTTPAddr: ":8080"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Environment != "production" || cfg.HTTPAddr != "127.0.0.1:9000" ||
		cfg.LogLevel != slog.LevelDebug || cfg.LogFormat != "text" ||
		cfg.OTelExporter != "otlp" || cfg.OTelEndpoint != "collector:4318" ||
		cfg.ShutdownTimeout != 3*time.Second || cfg.MaxQueuedPerTenant != 5 || cfg.MaxQueuedGlobal != 50 ||
		cfg.SweepInterval != 250*time.Millisecond || cfg.PDPTimeout != 2*time.Second ||
		cfg.ReconcileMaxAttempts != 3 || cfg.ReconcileMaxAge != 20*time.Minute {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
}

func TestLoadRequiresDatabaseURLWhenRequested(t *testing.T) {
	_, err := Load(env(nil), Options{RequireDatabase: true})
	if err == nil || !strings.Contains(err.Error(), "EACP_DATABASE_URL") {
		t.Fatalf("err = %v, want error naming EACP_DATABASE_URL", err)
	}
}

func TestLoadAllowsMissingDatabaseURLWhenNotRequired(t *testing.T) {
	if _, err := Load(env(nil), Options{RequireDatabase: false}); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	cases := map[string]map[string]string{
		"unknown environment": {"EACP_ENV": "prod-ish"},
		"unknown log level":   {"EACP_LOG_LEVEL": "loud"},
		"unknown log format":  {"EACP_LOG_FORMAT": "xml"},
		"unknown exporter":    {"EACP_OTEL_EXPORTER": "zipkin"},
		"otlp without endpoint": {
			"EACP_OTEL_EXPORTER": "otlp",
		},
		"unparsable timeout": {"EACP_SHUTDOWN_TIMEOUT": "soon"},
		// Keyword DSNs cannot be reliably redacted; only URL form is accepted.
		"keyword dsn":      {"EACP_DATABASE_URL": "host=db user=eacp_app password=hunter2 dbname=eacp"},
		"non-postgres url": {"EACP_DATABASE_URL": "mysql://u:p@db/eacp"},
		"zero timeout":     {"EACP_SHUTDOWN_TIMEOUT": "0s"},
		// Admission is never unbounded (MASTER_PLAN §26).
		"zero tenant limit":         {"EACP_ACTION_MAX_QUEUED_PER_TENANT": "0"},
		"negative global limit":     {"EACP_ACTION_MAX_QUEUED_GLOBAL": "-1"},
		"tenant above global":       {"EACP_ACTION_MAX_QUEUED_PER_TENANT": "20", "EACP_ACTION_MAX_QUEUED_GLOBAL": "10"},
		"unparsable limit":          {"EACP_ACTION_MAX_QUEUED_GLOBAL": "lots"},
		"zero pending limit":        {"EACP_ACTION_MAX_PENDING_PER_TENANT": "0"},
		"zero sweep interval":       {"EACP_ACTION_SWEEP_INTERVAL": "0s"},
		"unparsable pdp timeout":    {"EACP_PDP_TIMEOUT": "later"},
		"pdp timeout over a minute": {"EACP_PDP_TIMEOUT": "2m"},
		"zero reconcile attempts":   {"EACP_RECONCILE_MAX_ATTEMPTS": "0"},
		"too many reconciles":       {"EACP_RECONCILE_MAX_ATTEMPTS": "51"},
		"reconcile age over a day":  {"EACP_RECONCILE_MAX_AGE": "25h"},
	}
	for name, vars := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(env(vars), Options{}); err == nil {
				t.Fatalf("Load accepted invalid config %v", vars)
			}
		})
	}
}

func TestLogValueRedactsDatabasePassword(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"EACP_DATABASE_URL": "postgres://eacp_app:s3cr3t-canary@db:5432/eacp?sslmode=disable",
	}), Options{RequireDatabase: true})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("config", "config", cfg)
	out := buf.String()
	if strings.Contains(out, "s3cr3t-canary") {
		t.Fatalf("password leaked in log output: %s", out)
	}
	if !strings.Contains(out, "db:5432") {
		t.Fatalf("expected host to remain visible, got: %s", out)
	}
}

// ADR-001 §3: connector credentials live only in the execution worker.
func TestConnectorSecretsOnlyInTheWorker(t *testing.T) {
	vars := map[string]string{"EACP_CONNECTOR_SECRETS_FILE": "/run/secrets/connector_secrets"}
	_, err := Load(env(vars), Options{})
	if err == nil || !strings.Contains(err.Error(), "EACP_CONNECTOR_SECRETS_FILE") {
		t.Fatalf("a service other than the worker accepted connector secrets: %v", err)
	}
	cfg, err := Load(env(vars), Options{AllowConnectorSecrets: true})
	if err != nil || cfg.ConnectorSecretsFile != "/run/secrets/connector_secrets" {
		t.Fatalf("worker config = %+v, %v", cfg, err)
	}
}

func TestWorkerSettings(t *testing.T) {
	cfg, err := Load(env(nil), Options{AllowConnectorSecrets: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkerID != "" || cfg.WorkerLease != 30*time.Second || cfg.WorkerConcurrency != 4 ||
		cfg.WorkerPollInterval != 500*time.Millisecond || cfg.ConnectorSecretsFile != "" ||
		cfg.WorkerGroupConcurrency != 2 || cfg.WorkerBreakerFailures != 5 || cfg.WorkerBreakerCooldown != 30*time.Second {
		t.Fatalf("worker defaults = %+v", cfg)
	}
	cfg, err = Load(env(map[string]string{
		"EACP_WORKER_ID": "worker-a", "EACP_WORKER_LEASE": "10s",
		"EACP_WORKER_CONCURRENCY": "16", "EACP_WORKER_POLL_INTERVAL": "2s",
		"EACP_WORKER_GROUP_CONCURRENCY": "3", "EACP_WORKER_BREAKER_FAILURES": "8",
		"EACP_WORKER_BREAKER_COOLDOWN": "1m",
	}), Options{AllowConnectorSecrets: true})
	if err != nil || cfg.WorkerID != "worker-a" || cfg.WorkerLease != 10*time.Second ||
		cfg.WorkerConcurrency != 16 || cfg.WorkerPollInterval != 2*time.Second ||
		cfg.WorkerGroupConcurrency != 3 || cfg.WorkerBreakerFailures != 8 || cfg.WorkerBreakerCooldown != time.Minute {
		t.Fatalf("worker overrides = %+v, %v", cfg, err)
	}
	// A one-slot worker cannot split its slot; the bulkhead is that slot.
	if cfg, err := Load(env(map[string]string{"EACP_WORKER_CONCURRENCY": "1"}), Options{AllowConnectorSecrets: true}); err != nil || cfg.WorkerGroupConcurrency != 1 {
		t.Fatalf("one-slot worker = %+v, %v", cfg, err)
	}
	for name, vars := range map[string]map[string]string{
		"short lease":        {"EACP_WORKER_LEASE": "1s"},
		"long lease":         {"EACP_WORKER_LEASE": "10m"},
		"zero concurrency":   {"EACP_WORKER_CONCURRENCY": "0"},
		"huge concurrency":   {"EACP_WORKER_CONCURRENCY": "100000"},
		"zero poll interval": {"EACP_WORKER_POLL_INTERVAL": "0s"},
		"bad worker id":      {"EACP_WORKER_ID": "no spaces allowed"},
		"bulkhead too wide":  {"EACP_WORKER_CONCURRENCY": "4", "EACP_WORKER_GROUP_CONCURRENCY": "5"},
		"zero bulkhead":      {"EACP_WORKER_GROUP_CONCURRENCY": "0"},
		"zero failures":      {"EACP_WORKER_BREAKER_FAILURES": "0"},
		"too many failures":  {"EACP_WORKER_BREAKER_FAILURES": "101"},
		"short cooldown":     {"EACP_WORKER_BREAKER_COOLDOWN": "100ms"},
		"long cooldown":      {"EACP_WORKER_BREAKER_COOLDOWN": "11m"},
	} {
		if _, err := Load(env(vars), Options{AllowConnectorSecrets: true}); err == nil {
			t.Errorf("%s: accepted %v", name, vars)
		}
	}
}

// ADR-002 §8: the provider is local unless microsoft-agt is chosen, which
// needs the sidecar URL. Transport safety (loopback or mutual TLS) is
// checked by the client when controlplane-api starts.
func TestGovernanceProvider(t *testing.T) {
	cfg, err := Load(env(nil), Options{})
	if err != nil || cfg.GovernanceProvider != "local" || cfg.AGTPDPURL != "" {
		t.Fatalf("default provider = %q %q, %v", cfg.GovernanceProvider, cfg.AGTPDPURL, err)
	}
	cfg, err = Load(env(map[string]string{
		"EACP_GOVERNANCE_PROVIDER": "microsoft-agt", "EACP_AGT_PDP_URL": "https://agt-pdp:8443",
		"EACP_AGT_PDP_CA_FILE": "/pki/ca.pem", "EACP_AGT_PDP_CERT_FILE": "/pki/client.pem", "EACP_AGT_PDP_KEY_FILE": "/pki/client-key.pem",
	}), Options{})
	if err != nil || cfg.GovernanceProvider != "microsoft-agt" || cfg.AGTPDPURL != "https://agt-pdp:8443" ||
		cfg.AGTPDPCAFile != "/pki/ca.pem" || cfg.AGTPDPCertFile != "/pki/client.pem" || cfg.AGTPDPKeyFile != "/pki/client-key.pem" {
		t.Fatalf("agt config = %+v, %v", cfg, err)
	}
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("x", "config", cfg)
	if !strings.Contains(buf.String(), "governance_provider=microsoft-agt") || !strings.Contains(buf.String(), "agt_pdp_url=https://agt-pdp:8443") {
		t.Fatalf("log value = %s", buf.String())
	}
	for name, vars := range map[string]map[string]string{
		"unknown provider": {"EACP_GOVERNANCE_PROVIDER": "opa"},
		"agt without url":  {"EACP_GOVERNANCE_PROVIDER": "microsoft-agt"},
		"url without agt":  {"EACP_AGT_PDP_URL": "http://127.0.0.1:8181"},
	} {
		if _, err := Load(env(vars), Options{}); err == nil {
			t.Errorf("%s: accepted %v", name, vars)
		}
	}
}

// ADR-014 §6: NATS is optional. Plain nats:// is for loopback hosts or
// development and test; staging and production need tls://. The password
// in the URL never reaches the logged configuration.
func TestNATSSettings(t *testing.T) {
	cfg, err := Load(env(nil), Options{})
	if err != nil || cfg.NATSURL != "" || cfg.NATSPublishTimeout != 5*time.Second {
		t.Fatalf("NATS defaults = %q %v, %v", cfg.NATSURL, cfg.NATSPublishTimeout, err)
	}
	cfg, err = Load(env(map[string]string{
		"EACP_NATS_URL": "nats://relay:n4ts-canary@nats:4222", "EACP_NATS_PUBLISH_TIMEOUT": "2s",
	}), Options{})
	if err != nil || cfg.NATSURL != "nats://relay:n4ts-canary@nats:4222" || cfg.NATSPublishTimeout != 2*time.Second {
		t.Fatalf("NATS config = %+v, %v", cfg, err)
	}
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("x", "config", cfg)
	if strings.Contains(buf.String(), "n4ts-canary") || !strings.Contains(buf.String(), "nats:4222") {
		t.Fatalf("log value = %s", buf.String())
	}
	for name, vars := range map[string]string{
		"loopback in production": "nats://127.0.0.1:4222",
		"tls in production":      "tls://nats.internal:4222",
	} {
		if _, err := Load(env(map[string]string{"EACP_ENV": "production", "EACP_NATS_URL": vars}), Options{}); err != nil {
			t.Errorf("%s: rejected: %v", name, err)
		}
	}
	for name, vars := range map[string]map[string]string{
		"plain nats in production": {"EACP_ENV": "production", "EACP_NATS_URL": "nats://nats:4222"},
		"plain nats in staging":    {"EACP_ENV": "staging", "EACP_NATS_URL": "nats://nats:4222"},
		"other scheme":             {"EACP_NATS_URL": "http://nats:4222"},
		"no host":                  {"EACP_NATS_URL": "nats://"},
		"several servers":          {"EACP_NATS_URL": "nats://a:4222,nats://b:4222"},
		"ca without tls":           {"EACP_NATS_URL": "nats://nats:4222", "EACP_NATS_CA_FILE": "/pki/ca.pem"},
		"ca without url":           {"EACP_NATS_CA_FILE": "/pki/ca.pem"},
		"zero publish timeout":     {"EACP_NATS_URL": "nats://nats:4222", "EACP_NATS_PUBLISH_TIMEOUT": "0s"},
	} {
		if _, err := Load(env(vars), Options{}); err == nil {
			t.Errorf("%s: accepted %v", name, vars)
		}
	}
}
