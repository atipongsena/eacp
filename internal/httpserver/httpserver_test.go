package httpserver

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// startServer runs Serve on a random local port with handler h.
func startServer(t *testing.T, h http.Handler, timeout time.Duration) (addr string, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan error, 1)
	go func() { ch <- Serve(ctx, &http.Server{Handler: h}, ln, timeout) }()
	return ln.Addr().String(), cancel, ch
}

func TestServeWaitsForInFlightRequestsOnShutdown(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_, _ = io.WriteString(w, "done")
	})
	addr, cancel, done := startServer(t, h, 5*time.Second)

	respCh := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/")
		if err != nil {
			respCh <- "error: " + err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		respCh <- string(b)
	}()

	<-entered
	cancel()

	select {
	case err := <-done:
		t.Fatalf("Serve returned (%v) while a request was still in flight", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	if got := <-respCh; got != "done" {
		t.Fatalf("in-flight request got %q, want %q", got, "done")
	}
	if err := <-done; err != nil {
		t.Fatalf("Serve returned %v, want nil after clean shutdown", err)
	}
}

func TestServeRefusesNewConnectionsAfterShutdown(t *testing.T) {
	addr, cancel, done := startServer(t, http.NotFoundHandler(), time.Second)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("server still accepting connections after shutdown")
	}
}

func TestServeReturnsErrorWhenShutdownTimesOut(t *testing.T) {
	entered := make(chan struct{})
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-block
	})
	addr, cancel, done := startServer(t, h, 100*time.Millisecond)
	go func() { _, _ = http.Get("http://" + addr + "/") }()
	<-entered
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Serve returned %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after the shutdown timeout")
	}
}

func TestServeReturnsListenerErrors(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln.Close() // Serve on a closed listener must fail, not hang.
	err = Serve(context.Background(), &http.Server{Handler: http.NotFoundHandler()}, ln, time.Second)
	if err == nil {
		t.Fatal("Serve returned nil for a closed listener")
	}
}
