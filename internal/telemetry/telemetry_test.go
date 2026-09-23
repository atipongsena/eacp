package telemetry

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

func TestStdoutExporterWritesSpansWithServiceName(t *testing.T) {
	var buf bytes.Buffer
	shutdown, err := Setup(context.Background(), Options{
		ServiceName: "eacp-test", Exporter: "stdout", Writer: &buf,
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	_, span := otel.Tracer("test").Start(context.Background(), "phase1-span")
	span.End()
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "phase1-span") || !strings.Contains(out, "eacp-test") {
		t.Fatalf("span or service name missing from exporter output: %s", out)
	}
}

func TestNoneExporterStillPropagatesTraceContext(t *testing.T) {
	shutdown, err := Setup(context.Background(), Options{ServiceName: "eacp-test", Exporter: "none"})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	defer shutdown(context.Background())

	ctx, span := otel.Tracer("test").Start(context.Background(), "s")
	defer span.End()
	h := http.Header{}
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(h))
	if h.Get("traceparent") == "" {
		t.Fatal("traceparent not injected: W3C trace context must propagate even without an exporter")
	}
}

func TestOTLPExporterRequiresEndpoint(t *testing.T) {
	if _, err := Setup(context.Background(), Options{ServiceName: "x", Exporter: "otlp"}); err == nil {
		t.Fatal("Setup accepted otlp exporter without endpoint")
	}
}

func TestOTLPExporterConstructsWithoutNetwork(t *testing.T) {
	shutdown, err := Setup(context.Background(), Options{
		ServiceName: "x", Exporter: "otlp", Endpoint: "127.0.0.1:4318",
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // nothing to flush; shutdown must not block on the network
	_ = shutdown(ctx)
}

func TestUnknownExporterIsRejected(t *testing.T) {
	if _, err := Setup(context.Background(), Options{ServiceName: "x", Exporter: "zipkin"}); err == nil {
		t.Fatal("Setup accepted unknown exporter")
	}
}
