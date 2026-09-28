package worker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/proto/spiffe/workload"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/atipongsena/eacp/internal/spiffetest"
)

// spiffeStore loads bindings against agent a on clock c, logging to buf.
func spiffeStore(t *testing.T, a *spiffetest.Agent, c *vclock, buf *lockedBuffer, entries ...string) *SecretStore {
	t.Helper()
	opts := []LoadOption{AllowPlainTokenURL(), WithClock(c.now)}
	if buf != nil {
		opts = append(opts, WithLogger(slog.New(slog.NewJSONHandler(buf, nil))))
	}
	s, err := LoadSecrets(writeFile(t, "secrets.json", spiffeFile(a.Addr(), workerID, entries...)), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func claims(t *testing.T, jwt string) map[string]any {
	t.Helper()
	parts := strings.Split(jwt, ".")
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	var out map[string]any
	if err != nil || json.Unmarshal(b, &out) != nil {
		t.Fatalf("not a JWT: %v", err)
	}
	return out
}

func available(s *SecretStore, ref string) bool {
	return slices.ContainsFunc(s.Available(), func(b Binding) bool { return b.Ref == ref })
}

// counting makes a's SVIDs distinct (a jti per fetch) and lets ttl vary per fetch.
func counting(a *spiffetest.Agent, ttl func(n int64) time.Duration) {
	var n atomic.Int64
	a.Handle(func(r *workload.JWTSVIDRequest) (string, error) {
		i := n.Add(1)
		now := time.Now()
		return a.Signer.Sign(map[string]any{"sub": r.SpiffeId, "aud": r.Audience, "jti": fmt.Sprint(i),
			"iat": now.Unix(), "exp": now.Add(ttl(i)).Unix()}), nil
	})
}

func TestAValueSPIFFEBindingServesAnSVIDForItsAudience(t *testing.T) {
	a := spiffetest.New(t)
	s := spiffeStore(t, a, &vclock{t: time.Now()}, nil, valueSPIFFE)
	v, err := s.Credential(context.Background(), spiffeTenant, "erp-spiffe", spiffeEndpoint, 33*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c := claims(t, v.Reveal())
	if c["sub"] != workerID || fmt.Sprint(c["aud"]) != "[erp-api]" {
		t.Fatalf("claims = %v", c)
	}
	if _, err := s.Resolve(spiffeTenant, "erp-spiffe", "https://other.internal/v1"); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("another host: %v", err)
	}
}

func TestAnAgentFailureWithholdsTheBinding(t *testing.T) {
	a := spiffetest.New(t)
	down := func(*workload.JWTSVIDRequest) (string, error) {
		return "", status.Error(codes.Unavailable, "not ready")
	}
	a.Handle(down)
	c := &vclock{t: time.Now()}
	s := spiffeStore(t, a, c, nil, valueSPIFFE)
	ctx := context.Background()
	ask := func() error {
		_, err := s.Credential(ctx, spiffeTenant, "erp-spiffe", spiffeEndpoint, 33*time.Second)
		return err
	}
	if err := ask(); !errors.Is(err, ErrCredentialUnavailable) || available(s, "erp-spiffe") {
		t.Fatalf("err %v, available %t", err, available(s, "erp-spiffe"))
	}
	if err := ask(); !errors.Is(err, ErrCredentialUnavailable) || a.Fetches("erp-api") != 1 {
		t.Fatalf("during the back-off: err %v, %d fetches", err, a.Fetches("erp-api"))
	}
	c.add(time.Second)
	if !available(s, "erp-spiffe") {
		t.Fatal("still withheld after 1 s")
	}
	_ = ask() // fails again: 2 s
	c.add(time.Second)
	if available(s, "erp-spiffe") {
		t.Fatal("the back-off did not double")
	}
	c.add(time.Second)
	a.Handle(func(r *workload.JWTSVIDRequest) (string, error) {
		return a.SVID(r.SpiffeId, r.Audience, time.Hour), nil
	})
	if err := ask(); err != nil {
		t.Fatal(err)
	}
	c.add(2 * time.Hour) // the SVID is gone by the worker's clock
	a.Handle(down)
	_ = ask()
	c.add(time.Second)
	if !available(s, "erp-spiffe") {
		t.Fatal("a success did not reset the back-off")
	}
}

func TestAnSVIDShorterThanTheCallIsTooShort(t *testing.T) {
	a := spiffetest.New(t)
	a.Handle(func(r *workload.JWTSVIDRequest) (string, error) {
		return a.SVID(r.SpiffeId, r.Audience, time.Minute), nil
	})
	s := spiffeStore(t, a, &vclock{t: time.Now()}, nil, valueSPIFFE)
	ctx := context.Background()
	if _, err := s.Credential(ctx, spiffeTenant, "erp-spiffe", spiffeEndpoint, 90*time.Second); !errors.Is(err, ErrCredentialTooShort) {
		t.Fatalf("90 s call: %v", err)
	}
	if !available(s, "erp-spiffe") {
		t.Fatal("a short SVID withheld the binding")
	}
	if _, err := s.Credential(ctx, spiffeTenant, "erp-spiffe", spiffeEndpoint, 20*time.Second); err != nil {
		t.Fatalf("20 s call: %v", err)
	}
	if _, err := s.Credential(ctx, spiffeTenant, "erp-spiffe", spiffeEndpoint, 90*time.Second); !errors.Is(err, ErrCredentialTooShort) {
		t.Fatalf("second 90 s call: %v", err)
	}
	if n := a.Fetches("erp-api"); n != 1 {
		t.Fatalf("%d fetches, want 1", n)
	}
}

// TestATooShortSVIDRecoversWhenItExpires: a short SVID (the agent's cached
// copy while its server was down) refuses longer calls only while it lives;
// once it has expired the next call fetches again and gets a fresh SVID.
func TestATooShortSVIDRecoversWhenItExpires(t *testing.T) {
	a := spiffetest.New(t)
	counting(a, func(n int64) time.Duration {
		if n == 1 {
			return 50 * time.Second
		}
		return time.Hour
	})
	c := &vclock{t: time.Now()}
	s := spiffeStore(t, a, c, nil, valueSPIFFE)
	ctx := context.Background()
	if _, err := s.Credential(ctx, spiffeTenant, "erp-spiffe", spiffeEndpoint, time.Minute); !errors.Is(err, ErrCredentialTooShort) {
		t.Fatalf("first call: %v", err)
	}
	c.add(2 * time.Minute) // past the short SVID's exp; the agent now issues hour-long SVIDs
	if _, err := s.Credential(ctx, spiffeTenant, "erp-spiffe", spiffeEndpoint, time.Minute); err != nil {
		t.Fatalf("after the short SVID expired: %v (%d fetches)", err, a.Fetches("erp-api"))
	}
	if n := a.Fetches("erp-api"); n != 2 {
		t.Fatalf("%d fetches, want 2", n)
	}
}

// TestAShortSharedSVIDDoesNotWedgeTheSecondBinding: the second binding's
// short call is served from the shared cache; once that SVID expires, its
// longer calls fetch a fresh one.
func TestAShortSharedSVIDDoesNotWedgeTheSecondBinding(t *testing.T) {
	a := spiffetest.New(t)
	counting(a, func(n int64) time.Duration {
		if n == 1 {
			return 45 * time.Second
		}
		return time.Hour
	})
	c := &vclock{t: time.Now()}
	second := strings.Replace(valueSPIFFE, `"erp-spiffe"`, `"erp-spiffe-2"`, 1)
	s := spiffeStore(t, a, c, nil, valueSPIFFE, second)
	ctx := context.Background()
	for _, ref := range []string{"erp-spiffe", "erp-spiffe-2"} {
		if _, err := s.Credential(ctx, spiffeTenant, ref, spiffeEndpoint, 20*time.Second); err != nil {
			t.Fatalf("%s short call: %v", ref, err)
		}
	}
	if _, err := s.Credential(ctx, spiffeTenant, "erp-spiffe-2", spiffeEndpoint, time.Minute); !errors.Is(err, ErrCredentialTooShort) {
		t.Fatalf("long call while the shared SVID lives: %v", err)
	}
	c.add(time.Minute) // past the shared SVID's exp
	if _, err := s.Credential(ctx, spiffeTenant, "erp-spiffe-2", spiffeEndpoint, time.Minute); err != nil {
		t.Fatalf("long call after the shared SVID expired: %v (%d fetches)", err, a.Fetches("erp-api"))
	}
}

func TestAnSVIDAboutToExpireIsNeverSent(t *testing.T) {
	a := spiffetest.New(t)
	a.Handle(func(r *workload.JWTSVIDRequest) (string, error) {
		return a.SVID(r.SpiffeId, r.Audience, 5*time.Second), nil
	})
	buf := &lockedBuffer{}
	s := spiffeStore(t, a, &vclock{t: time.Now()}, buf, valueSPIFFE)
	v, err := s.Credential(context.Background(), spiffeTenant, "erp-spiffe", spiffeEndpoint, time.Second)
	if !errors.Is(err, ErrCredentialUnavailable) || errors.Is(err, ErrCredentialTooShort) || v.Reveal() != "" {
		t.Fatalf("err = %v", err)
	}
	if available(s, "erp-spiffe") || !strings.Contains(buf.String(), "spiffe_expiring") {
		t.Fatalf("not withheld with spiffe_expiring: %s", buf.String())
	}
}

func TestBindingsSharingAnAudienceFetchOnce(t *testing.T) {
	a := spiffetest.New(t)
	c := &vclock{t: time.Now()}
	second := strings.Replace(valueSPIFFE, `"erp-spiffe"`, `"erp-spiffe-2"`, 1)
	s := spiffeStore(t, a, c, nil, valueSPIFFE, second)
	ctx := context.Background()
	one, err1 := s.Credential(ctx, spiffeTenant, "erp-spiffe", spiffeEndpoint, 33*time.Second)
	two, err2 := s.Credential(ctx, spiffeTenant, "erp-spiffe-2", spiffeEndpoint, 33*time.Second)
	if err1 != nil || err2 != nil || one.Reveal() != two.Reveal() || a.Fetches("erp-api") != 1 {
		t.Fatalf("%v %v, %d fetches", err1, err2, a.Fetches("erp-api"))
	}
	c.add(2 * time.Hour)
	a.Handle(func(*workload.JWTSVIDRequest) (string, error) { return "", status.Error(codes.Unavailable, "down") })
	if _, err := s.Credential(ctx, spiffeTenant, "erp-spiffe", spiffeEndpoint, 33*time.Second); err == nil {
		t.Fatal("served an expired SVID")
	}
	if available(s, "erp-spiffe") || !available(s, "erp-spiffe-2") {
		t.Fatal("back-offs are not per binding")
	}
}

func TestRejectedDropsOnlyTheCurrentSVID(t *testing.T) {
	a := spiffetest.New(t)
	counting(a, func(int64) time.Duration { return time.Hour })
	s := spiffeStore(t, a, &vclock{t: time.Now()}, nil, valueSPIFFE)
	ctx := context.Background()
	first, err := s.Credential(ctx, spiffeTenant, "erp-spiffe", spiffeEndpoint, 33*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	s.Rejected(spiffeTenant, "erp-spiffe", Secret{v: "an-old-svid"})
	if again, _ := s.Credential(ctx, spiffeTenant, "erp-spiffe", spiffeEndpoint, 33*time.Second); again.Reveal() != first.Reveal() ||
		a.Fetches("erp-api") != 1 {
		t.Fatal("rejecting an old SVID dropped the current one")
	}
	s.Rejected(spiffeTenant, "erp-spiffe", first)
	next, err := s.Credential(ctx, spiffeTenant, "erp-spiffe", spiffeEndpoint, 33*time.Second)
	if err != nil || next.Reveal() == first.Reveal() || a.Fetches("erp-api") != 2 {
		t.Fatalf("after the rejection: %v, %d fetches", err, a.Fetches("erp-api"))
	}
}

func TestValuesHoldTheSVIDsUntilTheyExpire(t *testing.T) {
	a := spiffetest.New(t)
	counting(a, func(n int64) time.Duration {
		if n == 1 {
			return 40 * time.Second
		}
		return 2 * time.Hour
	})
	c := &vclock{t: time.Now()}
	s := spiffeStore(t, a, c, nil, valueSPIFFE)
	ctx := context.Background()
	first, err := s.Credential(ctx, spiffeTenant, "erp-spiffe", spiffeEndpoint, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.add(31 * time.Second)
	second, err := s.Credential(ctx, spiffeTenant, "erp-spiffe", spiffeEndpoint, 30*time.Second)
	if err != nil || second.Reveal() == first.Reveal() {
		t.Fatalf("no new SVID: %v", err)
	}
	if v := s.Values(); !slices.Contains(v, first.Reveal()) || !slices.Contains(v, second.Reveal()) {
		t.Fatal("Values lacks the current or the replaced SVID")
	}
	c.add(10 * time.Second)
	if v := s.Values(); slices.Contains(v, first.Reveal()) || !slices.Contains(v, second.Reveal()) {
		t.Fatal("Values keeps an expired SVID or lost the current one")
	}
}
