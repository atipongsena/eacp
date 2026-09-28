package worker_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/atipongsena/eacp/internal/fakeerp"
	"github.com/atipongsena/eacp/internal/jwttest"
	"github.com/atipongsena/eacp/internal/storage/pgtest"
)

const exchangedAccount = "eacp-erp@eacp-demo.iam.gserviceaccount.com"

// executePrincipals is the principal of every execute in the Fake ERP audit.
func (v *jitEnv) executePrincipals() []any {
	v.t.Helper()
	var out []any
	for _, e := range v.audit() {
		if e["path"] == "/v1/execute" {
			out = append(out, e["principal"])
		}
	}
	return out
}

// TestTheWorkerExecutesWithAnExchangedToken: the binding holds no client
// credential; the worker trades its platform token (a file) at the ERP's STS
// for an access token of its own subject (ADR-019 Rev 1.5).
func TestTheWorkerExecutesWithAnExchangedToken(t *testing.T) {
	signer := jwttest.New(t)
	file := subjectFile(t, signer, time.Hour)
	v := newJITFile(t, func(string) fakeerp.Options {
		return fakeerp.Options{TokenTTL: 10 * time.Minute, Exchange: &fakeerp.TokenExchange{Audience: "fakeerp-sts",
			Subjects: []fakeerp.ExchangeSubject{{Issuer: "https://issuer.test", Audience: "idp", Subject: stsSubject,
				Keys: []fakeerp.PublicKey{{KID: signer.KID, Key: &signer.Key.PublicKey}}}}}}
	}, func(tokenURL, host string) string {
		return fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"erp-jit","host":%q,
			"oauth2":{"grant":"token_exchange","token_url":%q,"subject_token":{"file":%q},"audience":"fakeerp-sts"}}]}`,
			pgtest.TenantA, host, tokenURL, file)
	}, 100)
	view := v.submit("")
	v.run(1)
	if got := v.get(view.ID); got.State != "SUCCEEDED" || got.AttemptCount != 1 {
		t.Fatalf("action = %+v", got)
	}
	if got := v.executePrincipals(); fmt.Sprint(got) != fmt.Sprint([]any{"sts:" + stsSubject}) {
		t.Fatalf("execute principals = %v, want one exchanged token", got)
	}
	v.mu.Lock()
	values := append([]string{readFile(t, file)}, v.tokens...)
	v.mu.Unlock()
	if len(values) != 2 {
		t.Fatalf("issued %d tokens, want 1", len(values)-1)
	}
	v.assertNotPersisted(values...)
}

// TestTheWorkerExecutesWithAnImpersonatedToken: the subject is the worker's
// JWT-SVID; the exchanged (federated) token only buys a service account's
// token, and the connector is called with that one.
func TestTheWorkerExecutesWithAnImpersonatedToken(t *testing.T) {
	a := newSVIDAgent(t)
	v := newJITFile(t, func(string) fakeerp.Options {
		return fakeerp.Options{TokenTTL: 10 * time.Minute, Exchange: &fakeerp.TokenExchange{Audience: "fakeerp-sts",
			Subjects: []fakeerp.ExchangeSubject{{Issuer: "https://spire.test", Audience: "fakeerp-sts", Subject: spiffeWorker,
				Keys: a.keys()}}},
			Impersonation: &fakeerp.Impersonation{Accounts: []string{exchangedAccount}}}
	}, func(tokenURL, host string) string {
		return fmt.Sprintf(`{"spiffe":{"endpoint":%q,"spiffe_id":%q},"secrets":[{"tenant_id":%q,"secret_ref":"erp-jit",
			"host":%q,"oauth2":{"grant":"token_exchange","token_url":%q,"subject_token":{"spiffe":{"audience":"fakeerp-sts"}},
			"audience":"fakeerp-sts","impersonate":{"url":"http://%s/v1/projects/-/serviceAccounts/%s:generateAccessToken",
			"scope":["erp.purchase"]}}}]}`, a.Addr(), spiffeWorker, pgtest.TenantA, host, tokenURL, host, exchangedAccount)
	}, 100)
	view := v.submit("")
	v.run(1)
	if got := v.get(view.ID); got.State != "SUCCEEDED" || got.AttemptCount != 1 {
		t.Fatalf("action = %+v", got)
	}
	if got := v.executePrincipals(); fmt.Sprint(got) != fmt.Sprint([]any{"sa:" + exchangedAccount}) {
		t.Fatalf("execute principals = %v, want one impersonated token", got)
	}
	v.mu.Lock()
	values := append(a.svids(), v.tokens...)
	v.mu.Unlock()
	values = append(values, v.secrets.Values()...) // the service account's token, still live
	if len(values) < 3 {
		t.Fatalf("%d SVIDs and tokens, want the SVID, the federated and the final token", len(values))
	}
	v.assertNotPersisted(values...)
}
