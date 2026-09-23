// Package config loads service configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"
)

// Options controls which settings a given service requires.
type Options struct {
	// RequireDatabase makes EACP_DATABASE_URL mandatory.
	RequireDatabase bool
	// DefaultHTTPAddr is used when EACP_HTTP_ADDR is unset.
	DefaultHTTPAddr string
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
}

var (
	environments = map[string]bool{"development": true, "test": true, "staging": true, "production": true}
	logFormats   = map[string]bool{"json": true, "text": true}
	exporters    = map[string]bool{"none": true, "stdout": true, "otlp": true}
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
	)
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
