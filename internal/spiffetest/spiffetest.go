// Package spiffetest serves a fake SPIFFE Workload API (the SPIRE agent's
// FetchJWTSVID) over loopback TCP, for tests of the worker's JWT-SVID
// credentials (ADR-019 Rev 1.4). It is imported by tests only.
package spiffetest

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/proto/spiffe/workload"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"eacp/internal/jwttest"
)

// Agent is a fake Workload API. By default it answers every FetchJWTSVID
// with an SVID for the requested SPIFFE ID and audiences, living an hour,
// signed by Signer.
type Agent struct {
	workload.UnimplementedSpiffeWorkloadAPIServer
	Signer *jwttest.Signer
	addr   string

	mu      sync.Mutex
	handle  func(*workload.JWTSVIDRequest) (string, error)
	fetches map[string]int
}

// New starts an agent on 127.0.0.1 that stops when the test ends.
func New(t testing.TB) *Agent {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{Signer: jwttest.New(t), addr: "tcp://" + l.Addr().String(), fetches: map[string]int{}}
	a.handle = func(r *workload.JWTSVIDRequest) (string, error) {
		return a.SVID(r.SpiffeId, r.Audience, time.Hour), nil
	}
	srv := grpc.NewServer()
	workload.RegisterSpiffeWorkloadAPIServer(srv, a)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(srv.Stop)
	return a
}

// Addr is the agent's Workload API address, "tcp://127.0.0.1:<port>".
func (a *Agent) Addr() string { return a.addr }

// Handle scripts FetchJWTSVID: f returns the SVID, or an error sent as is
// (use status.Error).
func (a *Agent) Handle(f func(*workload.JWTSVIDRequest) (string, error)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.handle = f
}

// SVID signs a JWT-SVID for sub and aud that expires ttl from now.
func (a *Agent) SVID(sub string, aud []string, ttl time.Duration) string {
	now := time.Now()
	return a.Signer.Sign(map[string]any{"sub": sub, "aud": aud, "iat": now.Unix(), "exp": now.Add(ttl).Unix()})
}

// Fetches counts the FetchJWTSVID calls whose first audience is audience.
func (a *Agent) Fetches(audience string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.fetches[audience]
}

// FetchJWTSVID requires the Workload API's security header, as SPIRE does.
func (a *Agent) FetchJWTSVID(ctx context.Context, r *workload.JWTSVIDRequest) (*workload.JWTSVIDResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	if v := md.Get("workload.spiffe.io"); len(v) != 1 || v[0] != "true" {
		return nil, status.Error(codes.InvalidArgument, "security header missing from request")
	}
	a.mu.Lock()
	if len(r.Audience) > 0 {
		a.fetches[r.Audience[0]]++
	}
	handle := a.handle
	a.mu.Unlock()
	svid, err := handle(r)
	if err != nil {
		return nil, err
	}
	return &workload.JWTSVIDResponse{Svids: []*workload.JWTSVID{{SpiffeId: r.SpiffeId, Svid: svid}}}, nil
}
