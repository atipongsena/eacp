package worker

import (
	"context"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/proto/spiffe/workload"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/atipongsena/eacp/internal/logging"
	"github.com/atipongsena/eacp/internal/spiffetest"
)

// svidClient is a spiffeClient for agent a (tcp, development) on clock c.
func svidClient(t *testing.T, a *spiffetest.Agent, c *vclock, set *logging.SecretSet) *spiffeClient {
	t.Helper()
	s, err := newSpiffeClient(spiffeEntry{Endpoint: a.Addr(), SPIFFEID: workerID},
		loadConfig{allowPlain: true, now: c.now, redact: set, log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAnSVIDIsFetchedOnceAndCached(t *testing.T) {
	a := spiffetest.New(t)
	s := svidClient(t, a, &vclock{t: time.Now()}, nil)
	ctx := context.Background()
	first, exp, class := s.svid(ctx, "a", 30*time.Second)
	again, _, class2 := s.svid(ctx, "a", 30*time.Second)
	if class != "" || class2 != "" || first.v == "" || again.v != first.v || time.Until(exp) < 50*time.Minute {
		t.Fatalf("svids %q/%q (%s %s), exp %v", first.v, again.v, class, class2, exp)
	}
	if n := a.Fetches("a"); n != 1 {
		t.Fatalf("%d fetches for a", n)
	}
	if b, _, _ := s.svid(ctx, "b", 30*time.Second); b.v == first.v || a.Fetches("b") != 1 {
		t.Fatal("audience b shared audience a's SVID")
	}
}

func TestAShortRemainingLifeFetchesAgain(t *testing.T) {
	a := spiffetest.New(t)
	a.Handle(func(r *workload.JWTSVIDRequest) (string, error) {
		return a.SVID(r.SpiffeId, r.Audience, 40*time.Second), nil
	})
	c := &vclock{t: time.Now()}
	s := svidClient(t, a, c, nil)
	if _, _, class := s.svid(context.Background(), "a", 30*time.Second); class != "" {
		t.Fatal(class)
	}
	c.add(15 * time.Second)
	if _, _, class := s.svid(context.Background(), "a", 30*time.Second); class != "" || a.Fetches("a") != 2 {
		t.Fatalf("class %q, %d fetches: an SVID that would expire during the call was reused", class, a.Fetches("a"))
	}
}

func TestAResponseMustBeTheWorkersSVIDForTheAudience(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stopped := "tcp://" + closed.Addr().String()
	closed.Close()
	for name, c := range map[string]struct {
		handle func(a *spiffetest.Agent, r *workload.JWTSVIDRequest) (string, error)
		addr   string
		want   string
	}{
		"another subject": {handle: func(a *spiffetest.Agent, r *workload.JWTSVIDRequest) (string, error) {
			return a.SVID("spiffe://eacp.test/ns/eacp/sa/other", r.Audience, time.Hour), nil
		}, want: "spiffe_invalid"},
		"another audience": {handle: func(a *spiffetest.Agent, r *workload.JWTSVIDRequest) (string, error) {
			return a.SVID(r.SpiffeId, []string{"other"}, time.Hour), nil
		}, want: "spiffe_invalid"},
		"a space in the token": {handle: func(a *spiffetest.Agent, r *workload.JWTSVIDRequest) (string, error) {
			return a.SVID(r.SpiffeId, r.Audience, time.Hour) + " x", nil
		}, want: "spiffe_invalid"},
		"not a JWT": {handle: func(*spiffetest.Agent, *workload.JWTSVIDRequest) (string, error) {
			return "not-a-jwt", nil
		}, want: "spiffe_invalid"},
		"too long": {handle: func(a *spiffetest.Agent, r *workload.JWTSVIDRequest) (string, error) {
			return a.Signer.Sign(map[string]any{"sub": r.SpiffeId, "aud": r.Audience,
				"exp": time.Now().Add(time.Hour).Unix(), "pad": strings.Repeat("p", 17<<10)}), nil
		}, want: "spiffe_invalid"},
		"no registration entry": {handle: func(*spiffetest.Agent, *workload.JWTSVIDRequest) (string, error) {
			return "", status.Error(codes.PermissionDenied, "no identity issued")
		}, want: "spiffe_denied"},
		"agent unavailable": {handle: func(*spiffetest.Agent, *workload.JWTSVIDRequest) (string, error) {
			return "", status.Error(codes.Unavailable, "not ready")
		}, want: "spiffe_unavailable"},
		"agent stopped": {addr: stopped, want: "spiffe_unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			a := spiffetest.New(t)
			if c.handle != nil {
				a.Handle(func(r *workload.JWTSVIDRequest) (string, error) { return c.handle(a, r) })
			}
			s := svidClient(t, a, &vclock{t: time.Now()}, nil)
			if c.addr != "" {
				s.endpoint = c.addr
			}
			if v, _, class := s.svid(context.Background(), "erp-api", 30*time.Second); class != c.want || v.v != "" {
				t.Fatalf("class = %q (svid %t), want %s", class, v.v != "", c.want)
			}
		})
	}
}

func TestQueuedCallersShareAFailedFetch(t *testing.T) {
	a := spiffetest.New(t)
	release := make(chan struct{})
	a.Handle(func(*workload.JWTSVIDRequest) (string, error) {
		<-release
		return "", status.Error(codes.Unavailable, "not ready")
	})
	s := svidClient(t, a, &vclock{t: time.Now()}, nil)
	var wg sync.WaitGroup
	classes := make([]string, 20)
	for i := range classes {
		wg.Go(func() { _, _, classes[i] = s.svid(context.Background(), "a", 30*time.Second) })
	}
	time.Sleep(300 * time.Millisecond) // every caller is queued behind the first fetch
	close(release)
	wg.Wait()
	for i, c := range classes {
		if c != "spiffe_unavailable" {
			t.Fatalf("caller %d: class %q", i, c)
		}
	}
	if n := a.Fetches("a"); n != 1 {
		t.Fatalf("%d fetches: queued callers fetched again", n)
	}
}

func TestEverySVIDIsRedacted(t *testing.T) {
	a := spiffetest.New(t)
	set := logging.NewSecretSet()
	s := svidClient(t, a, &vclock{t: time.Now()}, set)
	v, _, class := s.svid(context.Background(), "a", 30*time.Second)
	if class != "" || !slices.Contains(set.Values(), v.v) {
		t.Fatalf("the SVID is not redacted (class %q)", class)
	}
}
