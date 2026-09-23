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
	if cfg.MaxQueuedPerTenant != 1000 || cfg.MaxQueuedGlobal != 10000 {
		t.Errorf("admission limits = %d/%d, want 1000/10000", cfg.MaxQueuedPerTenant, cfg.MaxQueuedGlobal)
	}
	if cfg.SweepInterval != time.Second || cfg.PDPTimeout != 5*time.Second {
		t.Errorf("SweepInterval = %v, PDPTimeout = %v, want 1s and 5s", cfg.SweepInterval, cfg.PDPTimeout)
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
	}), Options{RequireDatabase: true, DefaultHTTPAddr: ":8080"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Environment != "production" || cfg.HTTPAddr != "127.0.0.1:9000" ||
		cfg.LogLevel != slog.LevelDebug || cfg.LogFormat != "text" ||
		cfg.OTelExporter != "otlp" || cfg.OTelEndpoint != "collector:4318" ||
		cfg.ShutdownTimeout != 3*time.Second || cfg.MaxQueuedPerTenant != 5 || cfg.MaxQueuedGlobal != 50 ||
		cfg.SweepInterval != 250*time.Millisecond || cfg.PDPTimeout != 2*time.Second {
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
		"zero sweep interval":       {"EACP_ACTION_SWEEP_INTERVAL": "0s"},
		"unparsable pdp timeout":    {"EACP_PDP_TIMEOUT": "later"},
		"pdp timeout over a minute": {"EACP_PDP_TIMEOUT": "2m"},
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
