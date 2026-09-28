package worker_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/proto/spiffe/workload"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/atipongsena/eacp/internal/fakeerp"
	"github.com/atipongsena/eacp/internal/spiffetest"
	"github.com/atipongsena/eacp/internal/storage/pgtest"
)

// svidAgent is a fake SPIRE agent whose SVIDs name issuer https://spire.test
// and are kept, so a test can look for them afterwards.
type svidAgent struct {
	*spiffetest.Agent
	mu     sync.Mutex
	issued []string
	refuse bool
}

func newSVIDAgent(t *testing.T) *svidAgent {
	a := &svidAgent{Agent: spiffetest.New(t)}
	a.Handle(func(r *workload.JWTSVIDRequest) (string, error) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.refuse {
			return "", status.Error(codes.PermissionDenied, "no identity issued")
		}
		now := time.Now()
		svid := a.Signer.Sign(map[string]any{"iss": "https://spire.test", "sub": r.SpiffeId, "aud": r.Audience,
			"iat": now.Unix(), "exp": now.Add(time.Hour).Unix()})
		a.issued = append(a.issued, svid)
		return svid, nil
	})
	return a
}

func (a *svidAgent) keys() []fakeerp.PublicKey {
	return []fakeerp.PublicKey{{KID: a.Signer.KID, Key: &a.Signer.Key.PublicKey}}
}

func (a *svidAgent) svids() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.issued...)
}

func (a *svidAgent) setRefuse(r bool) { a.mu.Lock(); a.refuse = r; a.mu.Unlock() }

// svidBearerEnv is the worker with one value_spiffe binding for audience
// fakeerp-api, against a Fake ERP that accepts the agent's SVIDs.
func svidBearerEnv(t *testing.T, a *svidAgent) *jitEnv {
	return newJITFile(t, func(string) fakeerp.Options {
		return fakeerp.Options{SPIFFEBearer: &fakeerp.SPIFFEBearer{Audience: "fakeerp-api", Subject: spiffeWorker, Keys: a.keys()}}
	}, func(_, host string) string {
		return fmt.Sprintf(`{"spiffe":{"endpoint":%q,"spiffe_id":%q},"secrets":[{"tenant_id":%q,"secret_ref":"erp-jit",
			"host":%q,"value_spiffe":{"audience":"fakeerp-api"}}]}`, a.Addr(), spiffeWorker, pgtest.TenantA, host)
	}, 100)
}

// TestTheWorkerExecutesWithAnSVID: the binding's credential is the worker's
// JWT-SVID for the ERP's audience, from the SPIRE agent (ADR-019 Rev 1.4).
func TestTheWorkerExecutesWithAnSVID(t *testing.T) {
	a := newSVIDAgent(t)
	v := svidBearerEnv(t, a)
	view := v.submit("")
	v.run(1)
	if got := v.get(view.ID); got.State != "SUCCEEDED" || got.AttemptCount != 1 {
		t.Fatalf("action = %+v", got)
	}
	svids := a.svids()
	if len(svids) != 1 {
		t.Fatalf("%d SVIDs fetched, want 1", len(svids))
	}
	sum := sha256.Sum256([]byte(svids[0]))
	executes := 0
	for _, e := range v.audit() {
		if e["path"] == "/v1/execute" {
			executes++
			if e["principal"] != "spiffe:"+spiffeWorker || e["svid_sha256"] != hex.EncodeToString(sum[:]) {
				t.Fatalf("execute audit = %v", e)
			}
		}
	}
	if executes != 1 {
		t.Fatalf("%d executes, want 1", executes)
	}
	v.assertNotPersisted(svids...)
}

// TestTheWorkerMintsWithAnSVIDAssertion: the OAuth client holds no secret;
// each mint presents the worker's JWT-SVID for the IdP's audience.
func TestTheWorkerMintsWithAnSVIDAssertion(t *testing.T) {
	a := newSVIDAgent(t)
	v := newJITFile(t, func(string) fakeerp.Options {
		return fakeerp.Options{TokenTTL: 10 * time.Minute, SPIFFEClient: &fakeerp.SPIFFEClient{ClientID: "eacp-worker-spiffe",
			Issuer: "https://spire.test", Audience: "fakeerp-token", Subject: spiffeWorker, Keys: a.keys()}}
	}, func(tokenURL, host string) string {
		return fmt.Sprintf(`{"spiffe":{"endpoint":%q,"spiffe_id":%q},"secrets":[{"tenant_id":%q,"secret_ref":"erp-jit",
			"host":%q,"oauth2":{"token_url":%q,"client_id":"eacp-worker-spiffe",
			"client_assertion_spiffe":{"audience":"fakeerp-token"}}}]}`, a.Addr(), spiffeWorker, pgtest.TenantA, host, tokenURL)
	}, 100)
	view := v.submit("")
	v.run(1)
	if got := v.get(view.ID); got.State != "SUCCEEDED" || got.AttemptCount != 1 {
		t.Fatalf("action = %+v", got)
	}
	for _, e := range v.audit() {
		if e["path"] == "/v1/execute" && e["principal"] != "oauth:eacp-worker-spiffe" {
			t.Fatalf("an execute used principal %v, not a token minted with an SVID", e["principal"])
		}
	}
	v.mu.Lock()
	values := append(a.svids(), v.tokens...)
	v.mu.Unlock()
	if len(values) != 2 {
		t.Fatalf("%d SVIDs and tokens, want one of each", len(values))
	}
	v.assertNotPersisted(values...)
}

// TestAnAgentRefusalWithholdsWork: while the agent has no identity for the
// worker, nothing is dispatched and the binding gets no work until its
// back-off passes; then the action runs.
func TestAnAgentRefusalWithholdsWork(t *testing.T) {
	a := newSVIDAgent(t)
	a.setRefuse(true)
	v := svidBearerEnv(t, a)
	view := v.submit("")
	v.run(1)
	if got := v.get(view.ID); got.State != "QUEUED" || got.StateReason != "credential unavailable" || got.AttemptCount != 0 {
		t.Fatalf("action = %+v", got)
	}
	fetches := a.Fetches("fakeerp-api")
	for range 3 {
		v.run(0)
	}
	if a.Fetches("fakeerp-api") != fetches {
		t.Fatal("the agent was asked again during the back-off")
	}
	a.setRefuse(false)
	v.clock.add(2 * time.Second)
	v.run(1)
	if got := v.get(view.ID); got.State != "SUCCEEDED" || got.AttemptCount != 1 {
		t.Fatalf("action = %+v", got)
	}
}
