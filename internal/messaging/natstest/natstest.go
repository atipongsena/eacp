// Package natstest runs an embedded NATS JetStream server for tests. Unlike
// the PostgreSQL integration tests, NATS tests never skip: the server runs
// in the test process with its store in a temporary directory.
package natstest

import (
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Server is an embedded JetStream server that can be stopped and restarted
// on the same port and store, to simulate an outage.
type Server struct {
	t    testing.TB
	dir  string
	port int
	srv  *server.Server
}

// Start runs a server on a free loopback port until the test ends.
func Start(t testing.TB) *Server {
	t.Helper()
	s := &Server{t: t, dir: t.TempDir(), port: -1}
	s.start()
	s.port = s.srv.Addr().(*net.TCPAddr).Port
	t.Cleanup(s.Stop)
	return s
}

func (s *Server) start() {
	s.t.Helper()
	srv, err := server.NewServer(&server.Options{
		Host: "127.0.0.1", Port: s.port, JetStream: true, StoreDir: s.dir,
		NoLog: true, NoSigs: true,
	})
	if err != nil {
		s.t.Fatalf("natstest: %v", err)
	}
	srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		s.t.Fatal("natstest: server not ready")
	}
	s.srv = srv
}

// URL is the server's client URL.
func (s *Server) URL() string { return "nats://127.0.0.1:" + strconv.Itoa(s.port) }

// Stop shuts the server down (an outage). It is idempotent.
func (s *Server) Stop() {
	if s.srv != nil {
		s.srv.Shutdown()
		s.srv.WaitForShutdown()
		s.srv = nil
	}
}

// Restart starts a stopped server again with the same port and store.
func (s *Server) Restart() {
	s.t.Helper()
	s.Stop()
	s.start()
}

// Connect returns a client connection that reconnects forever and its
// JetStream context; both close when the test ends.
func (s *Server) Connect(t testing.TB) (*nats.Conn, jetstream.JetStream) {
	t.Helper()
	nc, err := nats.Connect(s.URL(), nats.MaxReconnects(-1), nats.ReconnectWait(50*time.Millisecond),
		nats.RetryOnFailedConnect(true))
	if err != nil {
		t.Fatalf("natstest: connect: %v", err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("natstest: jetstream: %v", err)
	}
	return nc, js
}
