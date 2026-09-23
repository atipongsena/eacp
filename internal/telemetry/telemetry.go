// Package telemetry configures OpenTelemetry tracing and W3C propagation.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Options selects the trace exporter.
type Options struct {
	ServiceName string
	Environment string
	// Exporter is "none", "stdout" or "otlp" (OTLP over HTTP).
	Exporter string
	// Endpoint is the OTLP/HTTP collector host:port; required for "otlp".
	Endpoint string
	// Writer receives "stdout" exporter output; defaults to os.Stdout.
	Writer io.Writer
}

// Setup installs a global tracer provider and the W3C TraceContext + Baggage
// propagator. Trace context is always propagated, even with Exporter "none",
// so that trace IDs survive across services and the outbox (MASTER_PLAN §42).
// The returned function flushes and shuts down the provider.
func Setup(ctx context.Context, opts Options) (func(context.Context) error, error) {
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		attribute.String("service.name", opts.ServiceName),
		attribute.String("deployment.environment.name", opts.Environment),
	))
	if err != nil {
		return nil, fmt.Errorf("telemetry resource: %w", err)
	}

	tpOpts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	switch opts.Exporter {
	case "none":
		// Spans are created (so context propagates) but never exported.
	case "stdout":
		w := opts.Writer
		if w == nil {
			w = os.Stdout
		}
		exp, err := stdouttrace.New(stdouttrace.WithWriter(w))
		if err != nil {
			return nil, fmt.Errorf("stdout exporter: %w", err)
		}
		tpOpts = append(tpOpts, sdktrace.WithBatcher(exp))
	case "otlp":
		if opts.Endpoint == "" {
			return nil, errors.New("otlp exporter: endpoint required")
		}
		exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpoint(opts.Endpoint), otlptracehttp.WithInsecure())
		if err != nil {
			return nil, fmt.Errorf("otlp exporter: %w", err)
		}
		tpOpts = append(tpOpts, sdktrace.WithBatcher(exp))
	default:
		return nil, fmt.Errorf("unknown trace exporter %q", opts.Exporter)
	}

	tp := sdktrace.NewTracerProvider(tpOpts...)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	return tp.Shutdown, nil
}
