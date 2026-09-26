package service_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"eacp/internal/config"
	"eacp/internal/service"
)

type served struct {
	base    string
	cancel  context.CancelFunc
	done    chan error
	stopped chan struct{} // closed when the background context ends
}

func serveWith(t *testing.T, delay string) served {
	t.Helper()
	env := map[string]string{}
	if delay != "" {
		env["EACP_SHUTDOWN_DELAY"] = delay
	}
	var out bytes.Buffer
	deps, stop, err := service.Start(context.Background(), "test", envFrom(env), config.Options{}, &out)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	s := served{done: make(chan error, 1), stopped: make(chan struct{})}
	deps.Background(func(ctx context.Context) {
		<-ctx.Done()
		close(s.stopped)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.base = "http://" + ln.Addr().String()
	var ctx context.Context
	ctx, s.cancel = context.WithCancel(context.Background())
	go func() { s.done <- deps.ServeOn(ctx, ln, deps.Handler(http.NewServeMux())) }()
	if code, _ := get(t, s.base+"/readyz"); code != http.StatusOK {
		t.Fatalf("/readyz before shutdown = %d", code)
	}
	return s
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	c := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := c.Get(url)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// closes reports whether a kept-alive request to url is answered with
// Connection: close.
func closes(t *testing.T, url string) bool {
	t.Helper()
	c := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{}}
	defer c.CloseIdleConnections()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Close
}

func TestDrainReportsNotReadyAndKeepsServing(t *testing.T) {
	s := serveWith(t, "600ms")
	if closes(t, s.base+"/healthz") {
		t.Fatal("a response before the signal closed its kept-alive connection")
	}
	start := time.Now()
	s.cancel()
	select {
	case <-s.stopped:
	case <-time.After(time.Second):
		t.Fatal("background loops kept running after the signal")
	}
	code, body := get(t, s.base+"/readyz")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, `"draining":"fail"`) {
		t.Fatalf("/readyz while draining = %d %s", code, body)
	}
	if code, _ := get(t, s.base+"/healthz"); code != http.StatusOK {
		t.Fatalf("/healthz while draining = %d", code)
	}
	// A client on a kept-alive connection is told to reconnect, so it moves
	// to a pod still in the Service before this one closes its connections.
	if !closes(t, s.base+"/healthz") {
		t.Fatal("a response while draining did not close its kept-alive connection")
	}
	select {
	case err := <-s.done:
		t.Fatalf("served only %s of the delay: %v", time.Since(start), err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := <-s.done; err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 600*time.Millisecond {
		t.Fatalf("shut down after %s, before the delay", elapsed)
	}
	if code, _ := get(t, s.base+"/healthz"); code != 0 {
		t.Fatalf("still serving after shutdown: %d", code)
	}
}

func TestNoDelayShutsDownAtOnce(t *testing.T) {
	s := serveWith(t, "")
	start := time.Now()
	s.cancel()
	if err := <-s.done; err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("shutdown took %s without a delay", elapsed)
	}
	select {
	case <-s.stopped:
	case <-time.After(time.Second):
		t.Fatal("background loops kept running")
	}
}

func TestListenerFailureReturnsWithoutDelay(t *testing.T) {
	var out bytes.Buffer
	deps, stop, err := service.Start(context.Background(), "test",
		envFrom(map[string]string{"EACP_SHUTDOWN_DELAY": "60s"}), config.Options{}, &out)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln.Close() // Serve fails at once
	start := time.Now()
	err = deps.ServeOn(context.Background(), ln, deps.Handler(http.NewServeMux()))
	if err == nil || errors.Is(err, context.Canceled) || time.Since(start) > 5*time.Second {
		t.Fatalf("ServeOn on a closed listener = %v after %s", err, time.Since(start))
	}
}
