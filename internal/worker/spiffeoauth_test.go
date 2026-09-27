package worker_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/proto/spiffe/workload"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"eacp/internal/logging"
	"eacp/internal/spiffetest"
	"eacp/internal/worker"
)

const spiffeWorker = "spiffe://eacp.test/ns/eacp/sa/eacp-worker"

// spiffeOAuthStore binds tenant's "erp" to an OAuth client that authenticates
// with an SVID for audience fakeerp-token from agent a.
func spiffeOAuthStore(t *testing.T, a *spiffetest.Agent, tokenURL string, c *clock, opts ...worker.LoadOption) *worker.SecretStore {
	t.Helper()
	body := fmt.Sprintf(`{"spiffe":{"endpoint":%q,"spiffe_id":%q},"secrets":[{"tenant_id":%q,"secret_ref":"erp",
		"host":"erp.internal:8443","oauth2":{"token_url":%q,"client_id":"eacp-worker",
		"client_assertion_spiffe":{"audience":"fakeerp-token"}}}]}`, a.Addr(), spiffeWorker, tenant, tokenURL)
	opts = append([]worker.LoadOption{worker.AllowPlainTokenURL(), worker.WithClock(c.now)}, opts...)
	s, err := worker.LoadSecrets(secretsFile(t, body), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// ttls makes the agent's n-th SVID live ttl(n), each with its own jti.
func ttls(a *spiffetest.Agent, ttl func(n int64) time.Duration) {
	var n atomic.Int64
	a.Handle(func(r *workload.JWTSVIDRequest) (string, error) {
		i := n.Add(1)
		now := time.Now()
		return a.Signer.Sign(map[string]any{"sub": r.SpiffeId, "aud": r.Audience, "jti": fmt.Sprint(i),
			"iat": now.Unix(), "exp": now.Add(ttl(i)).Unix()}), nil
	})
}

func TestASPIFFEAssertionAuthenticatesTheMint(t *testing.T) {
	a, p := spiffetest.New(t), newIDP(t)
	s := spiffeOAuthStore(t, a, p.srv.URL+"/token", &clock{t: time.Now()})
	tok, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, 33*time.Second)
	if err != nil || !strings.HasPrefix(tok.Reveal(), "tok-1-") {
		t.Fatalf("token %v, %v", tok.Reveal(), err)
	}
	form, auth := p.last()
	if auth != [2]string{} || form.Get("client_id") != "eacp-worker" || form.Get("grant_type") != "client_credentials" ||
		form.Get("client_assertion_type") != "urn:ietf:params:oauth:client-assertion-type:jwt-bearer" {
		t.Fatalf("form %v, basic auth %v", form, auth)
	}
	parts := strings.Split(form.Get("client_assertion"), ".")
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Sub string   `json:"sub"`
		Aud []string `json:"aud"`
	}
	if len(parts) != 3 || json.Unmarshal(payload, &claims) != nil || claims.Sub != spiffeWorker ||
		!slices.Equal(claims.Aud, []string{"fakeerp-token"}) {
		t.Fatalf("assertion claims = %+v", claims)
	}
}

func TestEveryMintFetchesAnAssertionOnlyWhenNeeded(t *testing.T) {
	a, p := spiffetest.New(t), newIDP(t)
	p.set(func(n int64) (int, any) {
		return 200, map[string]any{"access_token": fmt.Sprintf("tok-%d", n), "token_type": "Bearer", "expires_in": 60}
	})
	ttls(a, func(n int64) time.Duration {
		if n == 1 {
			return 50 * time.Second
		}
		return 2 * time.Hour
	})
	c := &clock{t: time.Now()}
	s := spiffeOAuthStore(t, a, p.srv.URL+"/token", c)
	ctx := context.Background()
	if _, err := s.Credential(ctx, tenant, "erp", erpEndpoint, 33*time.Second); err != nil {
		t.Fatal(err)
	}
	c.add(30 * time.Second) // the token has 30 s left: mint again; the SVID has 20 s, enough for the request
	if _, err := s.Credential(ctx, tenant, "erp", erpEndpoint, 33*time.Second); err != nil {
		t.Fatal(err)
	}
	if p.mints.Load() != 2 || a.Fetches("fakeerp-token") != 1 {
		t.Fatalf("%d mints, %d fetches: want 2 mints with one SVID", p.mints.Load(), a.Fetches("fakeerp-token"))
	}
	c.add(15 * time.Second) // the SVID has 5 s left: the next mint fetches another
	c.add(30 * time.Second)
	if _, err := s.Credential(ctx, tenant, "erp", erpEndpoint, 33*time.Second); err != nil {
		t.Fatal(err)
	}
	if p.mints.Load() != 3 || a.Fetches("fakeerp-token") != 2 {
		t.Fatalf("%d mints, %d fetches: an assertion about to expire was sent", p.mints.Load(), a.Fetches("fakeerp-token"))
	}
}

type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *safeBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestASPIFFEFailureIsAFailedMint(t *testing.T) {
	a, p := spiffetest.New(t), newIDP(t)
	a.Handle(func(*workload.JWTSVIDRequest) (string, error) {
		return "", status.Error(codes.PermissionDenied, "no identity issued")
	})
	c := &clock{t: time.Now()}
	buf := &safeBuffer{}
	s := spiffeOAuthStore(t, a, p.srv.URL+"/token", c, worker.WithLogger(slog.New(slog.NewJSONHandler(buf, nil))))
	ctx := context.Background()
	held := func() bool {
		return !slices.ContainsFunc(s.Available(), func(b worker.Binding) bool { return b.Ref == "erp" })
	}
	if _, err := s.Credential(ctx, tenant, "erp", erpEndpoint, 33*time.Second); !errors.Is(err, worker.ErrCredentialUnavailable) ||
		p.mints.Load() != 0 || !held() || !strings.Contains(buf.String(), "spiffe_denied") {
		t.Fatalf("err %v, %d token requests, withheld %t, log %s", err, p.mints.Load(), held(), buf.String())
	}
	c.add(time.Second)
	if held() {
		t.Fatal("still withheld after the back-off")
	}
	a.Handle(func(r *workload.JWTSVIDRequest) (string, error) {
		return a.SVID(r.SpiffeId, r.Audience, 5*time.Second), nil
	})
	if _, err := s.Credential(ctx, tenant, "erp", erpEndpoint, 33*time.Second); !errors.Is(err, worker.ErrCredentialUnavailable) ||
		p.mints.Load() != 0 || !strings.Contains(buf.String(), "assertion_expired") {
		t.Fatalf("an SVID with 5 s left: err %v, %d token requests", err, p.mints.Load())
	}
}

func TestTheSVIDAssertionIsRedactedAndScrubbed(t *testing.T) {
	a, p := spiffetest.New(t), newIDP(t)
	set := logging.NewSecretSet()
	c := &clock{t: time.Now()}
	s := spiffeOAuthStore(t, a, p.srv.URL+"/token", c, worker.WithRedaction(set))
	if _, err := s.Credential(context.Background(), tenant, "erp", erpEndpoint, 33*time.Second); err != nil {
		t.Fatal(err)
	}
	form, _ := p.last()
	svid := form.Get("client_assertion")
	if svid == "" || !slices.Contains(set.Values(), svid) || !slices.Contains(s.Values(), svid) {
		t.Fatal("the SVID assertion is not redacted or not scrubbed")
	}
	c.add(2 * time.Hour)
	if slices.Contains(s.Values(), svid) {
		t.Fatal("an expired assertion is still scrubbed")
	}
}
