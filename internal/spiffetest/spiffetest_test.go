package spiffetest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/proto/spiffe/workload"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func TestTheFakeAgentServesGoSpiffe(t *testing.T) {
	a := New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := workloadapi.New(ctx, workloadapi.WithAddr(a.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	id := spiffeid.RequireFromString("spiffe://eacp.test/w")
	svid, err := c.FetchJWTSVID(ctx, jwtsvid.Params{Audience: "aud-1", Subject: id})
	if err != nil {
		t.Fatal(err)
	}
	if svid.ID != id || len(svid.Audience) != 1 || svid.Audience[0] != "aud-1" || time.Until(svid.Expiry) < 50*time.Minute {
		t.Fatalf("svid = %v %v %v", svid.ID, svid.Audience, svid.Expiry)
	}
	if n := a.Fetches("aud-1"); n != 1 {
		t.Fatalf("%d fetches for aud-1", n)
	}

	a.Handle(func(*workload.JWTSVIDRequest) (string, error) {
		return "", status.Error(codes.PermissionDenied, "no identity issued")
	})
	if _, err := c.FetchJWTSVID(ctx, jwtsvid.Params{Audience: "aud-1", Subject: id}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("err = %v, want PermissionDenied", err)
	}
	a.Handle(func(r *workload.JWTSVIDRequest) (string, error) {
		return a.SVID("spiffe://eacp.test/other", r.Audience, time.Minute), nil
	})
	svid, err = c.FetchJWTSVID(ctx, jwtsvid.Params{Audience: "aud-2", Subject: id})
	if err != nil || svid.ID.String() != "spiffe://eacp.test/other" {
		t.Fatalf("scripted svid = %v, %v", svid, err)
	}
	if n := a.Fetches("aud-1"); n != 2 {
		t.Fatalf("%d fetches for aud-1, want 2", n)
	}
}

func TestTheFakeAgentRequiresTheHeader(t *testing.T) {
	a := New(t)
	conn, err := grpc.NewClient(strings.TrimPrefix(a.Addr(), "tcp://"),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = workload.NewSpiffeWorkloadAPIClient(conn).FetchJWTSVID(context.Background(),
		&workload.JWTSVIDRequest{Audience: []string{"aud-1"}, SpiffeId: "spiffe://eacp.test/w"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("err = %v, want InvalidArgument", err)
	}
	if n := a.Fetches("aud-1"); n != 0 {
		t.Fatalf("a call without the header was counted (%d)", n)
	}
}
