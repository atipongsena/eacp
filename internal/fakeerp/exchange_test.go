package fakeerp_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/atipongsena/eacp/internal/fakeerp"
)

const (
	grantExchange  = "urn:ietf:params:oauth:grant-type:token-exchange"
	typeAccess     = "urn:ietf:params:oauth:token-type:access_token"
	typeJWT        = "urn:ietf:params:oauth:token-type:jwt"
	stsAudience    = "fakeerp-sts"
	k8sIssuer      = "https://kubernetes.default.svc.cluster.local"
	k8sSubject     = "system:serviceaccount:eacp:eacp-worker"
	serviceAccount = "eacp-erp@eacp-demo.iam.gserviceaccount.com"
)

// exchangeERP trusts two subjects: a Kubernetes service-account token (RS256
// by k8s) and a JWT-SVID (ES256 by spire), and impersonates one account.
func exchangeERP(t *testing.T) (*httptest.Server, *keyPair, *keyPair) {
	t.Helper()
	k8s, spire := newKeyPair(t, "k8s"), newKeyPair(t, "spire")
	h, err := fakeerp.NewWithOptions(credential, filepath.Join(t.TempDir(), "erp.log"), fakeerp.Options{
		OAuthClientID: oauthClient, OAuthClientSecret: oauthSecret,
		Exchange: &fakeerp.TokenExchange{Audience: stsAudience, Subjects: []fakeerp.ExchangeSubject{
			{Issuer: k8sIssuer, Audience: "fakeerp", Subject: k8sSubject,
				Keys: []fakeerp.PublicKey{{KID: "k8s-rsa", Key: &k8s.rsa.PublicKey}}},
			{Issuer: spiffeIssuer, Audience: stsAudience, Subject: spiffeWorker,
				Keys: []fakeerp.PublicKey{{KID: "spire-ec", Key: &spire.ec.PublicKey}}},
		}},
		Impersonation: &fakeerp.Impersonation{Accounts: []string{serviceAccount}},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, k8s, spire
}

func k8sToken(k *keyPair, mutate func(map[string]any)) string {
	now := time.Now().Unix()
	c := map[string]any{"iss": k8sIssuer, "sub": k8sSubject, "aud": []string{"fakeerp"}, "iat": now, "nbf": now, "exp": now + 600}
	if mutate != nil {
		mutate(c)
	}
	return k.sign(map[string]any{"alg": "RS256", "kid": "k8s-rsa"}, c)
}

func exchangeForm(subject string) url.Values {
	return url.Values{"grant_type": {grantExchange}, "subject_token": {subject}, "subject_token_type": {typeJWT},
		"requested_token_type": {typeAccess}, "audience": {stsAudience}}
}

func impersonate(t *testing.T, srv *httptest.Server, bearer, account, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/projects/-/serviceAccounts/"+account+":generateAccessToken", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestAnExchangeIssuesATokenForTheSubject(t *testing.T) {
	srv, k8s, spire := exchangeERP(t)
	svid := spire.sign(map[string]any{"alg": "ES256", "kid": "spire-ec"}, svidClaims(stsAudience))
	subjects := map[string]string{"sts:" + k8sSubject: k8sToken(k8s, nil), "sts:" + spiffeWorker: svid}
	for principal, subject := range subjects {
		status, got := postToken(t, srv, exchangeForm(subject), false)
		if status != 200 || got["issued_token_type"] != typeAccess || got["token_type"] != "Bearer" || got["expires_in"] == nil {
			t.Fatalf("%s: %d %v", principal, status, got)
		}
		if code := executeWith(t, srv, got["access_token"].(string)); code != 200 {
			t.Fatalf("%s: the exchanged token executes with %d", principal, code)
		}
	}
	if status, got := postToken(t, srv, exchangeForm(k8sToken(k8s, nil)), true); status != 200 {
		t.Fatalf("with a valid client: %d %v", status, got)
	}
	entries, raw := auditOf(t, srv)
	seen := map[string]bool{}
	for _, e := range entries {
		if e.Outcome == "token_issued" {
			seen[e.Principal] = true
		}
	}
	sum := sha256.Sum256([]byte(svid))
	if !seen["sts:"+k8sSubject] || !seen["sts:"+spiffeWorker] || !strings.Contains(raw, hex.EncodeToString(sum[:])) {
		t.Fatalf("audit: %v", seen)
	}
	if strings.Contains(raw, svid) {
		t.Fatal("the audit holds a subject token")
	}
}

func TestTheExchangeRefusesBadRequests(t *testing.T) {
	srv, k8s, spire := exchangeERP(t)
	good := k8sToken(k8s, nil)
	with := func(k string, v any) string { return k8sToken(k8s, func(c map[string]any) { c[k] = v }) }
	for name, c := range map[string]struct {
		form   url.Values
		status int
	}{
		"another audience": {func() url.Values { f := exchangeForm(good); f.Set("audience", "other"); return f }(), 400},
		"no audience":      {func() url.Values { f := exchangeForm(good); f.Del("audience"); return f }(), 400},
		"a saml subject": {func() url.Values {
			f := exchangeForm(good)
			f.Set("subject_token_type", "urn:ietf:params:oauth:token-type:saml2")
			return f
		}(), 400},
		"a refresh token wanted": {func() url.Values {
			f := exchangeForm(good)
			f.Set("requested_token_type", "urn:ietf:params:oauth:token-type:refresh_token")
			return f
		}(), 400},
		"wrong issuer":         {exchangeForm(with("iss", "https://other")), 400},
		"wrong subject":        {exchangeForm(with("sub", "system:serviceaccount:eacp:other")), 400},
		"wrong token audience": {exchangeForm(with("aud", []string{"other"})), 400},
		"expired":              {exchangeForm(with("exp", time.Now().Unix()-120)), 400},
		"unknown kid":          {exchangeForm(k8s.sign(map[string]any{"alg": "RS256", "kid": "nope"}, map[string]any{"iss": k8sIssuer, "sub": k8sSubject, "aud": "fakeerp", "exp": time.Now().Unix() + 60})), 400},
		"bad signature":        {exchangeForm(good[:len(good)-4] + "AAAA"), 400},
		"another key":          {exchangeForm(spire.sign(map[string]any{"alg": "RS256", "kid": "k8s-rsa"}, map[string]any{"iss": k8sIssuer, "sub": k8sSubject, "aud": "fakeerp", "exp": time.Now().Unix() + 60})), 400},
		"no subject":           {func() url.Values { f := exchangeForm(good); f.Del("subject_token"); return f }(), 400},
	} {
		status, got := postToken(t, srv, c.form, false)
		if status != c.status || got["access_token"] != nil {
			t.Errorf("%s: %d %v", name, status, got)
		}
	}
	req, _ := http.NewRequest("POST", srv.URL+"/oauth/token", strings.NewReader(exchangeForm(good).Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(url.QueryEscape(oauthClient), "wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("a bad client credential: %d", resp.StatusCode)
	}
}

func TestImpersonationIssuesAServiceAccountToken(t *testing.T) {
	srv, k8s, _ := exchangeERP(t)
	_, got := postToken(t, srv, exchangeForm(k8sToken(k8s, nil)), false)
	status, sa := impersonate(t, srv, got["access_token"].(string), serviceAccount, `{"scope":["https://www.googleapis.com/auth/cloud-platform"],"lifetime":"600s"}`)
	if status != 200 || sa["accessToken"] == nil {
		t.Fatalf("%d %v", status, sa)
	}
	exp, err := time.Parse(time.RFC3339, sa["expireTime"].(string))
	if err != nil || exp.Sub(time.Now()) < 590*time.Second || exp.Sub(time.Now()) > 610*time.Second {
		t.Fatalf("expireTime %v, %v", sa["expireTime"], err)
	}
	if code := executeWith(t, srv, sa["accessToken"].(string)); code != 200 {
		t.Fatalf("the impersonated token executes with %d", code)
	}
	entries, _ := auditOf(t, srv)
	found := false
	for _, e := range entries {
		found = found || (e.Outcome == "token_issued" && e.Principal == "sa:"+serviceAccount)
	}
	if !found {
		t.Fatal("no issuance to the service account in the audit")
	}
}

func TestImpersonationRefusesBadRequests(t *testing.T) {
	srv, k8s, _ := exchangeERP(t)
	_, got := postToken(t, srv, exchangeForm(k8sToken(k8s, nil)), false)
	sts := got["access_token"].(string)
	_, basic := postToken(t, srv, url.Values{"grant_type": {"client_credentials"}}, true)
	body := `{"scope":["s"],"lifetime":"600s"}`
	for name, c := range map[string]struct {
		bearer, account, body string
		status                int
	}{
		"no token":        {"", serviceAccount, body, 401},
		"static token":    {credential, serviceAccount, body, 401},
		"an oauth token":  {basic["access_token"].(string), serviceAccount, body, 401},
		"unknown account": {sts, "other@eacp-demo.iam.gserviceaccount.com", body, 403},
		"no scope":        {sts, serviceAccount, `{"scope":[],"lifetime":"600s"}`, 400},
		"33 scopes":       {sts, serviceAccount, `{"scope":["a","a","a","a","a","a","a","a","a","a","a","a","a","a","a","a","a","a","a","a","a","a","a","a","a","a","a","a","a","a","a","a","a"],"lifetime":"600s"}`, 400},
		"zero lifetime":   {sts, serviceAccount, `{"scope":["s"],"lifetime":"0s"}`, 400},
		"long lifetime":   {sts, serviceAccount, `{"scope":["s"],"lifetime":"3601s"}`, 400},
		"hours lifetime":  {sts, serviceAccount, `{"scope":["s"],"lifetime":"1h"}`, 400},
		"not json":        {sts, serviceAccount, `scope=s`, 400},
	} {
		if status, out := impersonate(t, srv, c.bearer, c.account, c.body); status != c.status || out["accessToken"] != nil {
			t.Errorf("%s: %d %v", name, status, out)
		}
	}
}

func TestExchangeOptionsFailClosed(t *testing.T) {
	k := newKeyPair(t, "k8s")
	subject := fakeerp.ExchangeSubject{Issuer: k8sIssuer, Audience: "fakeerp", Subject: k8sSubject,
		Keys: []fakeerp.PublicKey{{KID: "k8s-rsa", Key: &k.rsa.PublicKey}}}
	for name, o := range map[string]fakeerp.Options{
		"no audience":     {Exchange: &fakeerp.TokenExchange{Subjects: []fakeerp.ExchangeSubject{subject}}},
		"no subjects":     {Exchange: &fakeerp.TokenExchange{Audience: stsAudience}},
		"subject no sub":  {Exchange: &fakeerp.TokenExchange{Audience: stsAudience, Subjects: []fakeerp.ExchangeSubject{{Audience: "a", Keys: subject.Keys}}}},
		"subject no aud":  {Exchange: &fakeerp.TokenExchange{Audience: stsAudience, Subjects: []fakeerp.ExchangeSubject{{Subject: "s", Keys: subject.Keys}}}},
		"subject no keys": {Exchange: &fakeerp.TokenExchange{Audience: stsAudience, Subjects: []fakeerp.ExchangeSubject{{Subject: "s", Audience: "a"}}}},
		"no exchange":     {Impersonation: &fakeerp.Impersonation{Accounts: []string{serviceAccount}}},
		"no accounts":     {Exchange: &fakeerp.TokenExchange{Audience: stsAudience, Subjects: []fakeerp.ExchangeSubject{subject}}, Impersonation: &fakeerp.Impersonation{}},
		"not an email":    {Exchange: &fakeerp.TokenExchange{Audience: stsAudience, Subjects: []fakeerp.ExchangeSubject{subject}}, Impersonation: &fakeerp.Impersonation{Accounts: []string{"nobody"}}},
	} {
		if _, err := fakeerp.NewWithOptions(credential, filepath.Join(t.TempDir(), "erp.log"), o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestAnExchangeAuditKeepsTheClient: an exchange that authenticates a
// client records it beside the subject's principal.
func TestAnExchangeAuditKeepsTheClient(t *testing.T) {
	srv, k8s, _ := exchangeERP(t)
	if status, got := postToken(t, srv, exchangeForm(k8sToken(k8s, nil)), true); status != 200 {
		t.Fatalf("%d %v", status, got)
	}
	_, raw := auditOf(t, srv)
	var entries []map[string]any
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e["outcome"] == "token_issued" && e["principal"] == "sts:"+k8sSubject {
			if e["client"] != oauthClient {
				t.Fatalf("the exchange audit lost its client: %v", e)
			}
			return
		}
	}
	t.Fatalf("no exchange issuance in %s", raw)
}

// TestAMissingSubjectIsNotDigested: a refusal without a subject token
// records no digest (the digest of nothing is not evidence).
func TestAMissingSubjectIsNotDigested(t *testing.T) {
	srv, _, _ := exchangeERP(t)
	f := exchangeForm("x")
	f.Del("subject_token")
	if status, _ := postToken(t, srv, f, false); status != 400 {
		t.Fatalf("status %d", status)
	}
	if _, raw := auditOf(t, srv); strings.Contains(raw, "subject_sha256") {
		t.Fatalf("a missing subject was digested: %s", raw)
	}
}

func TestAPercentAccountIsRefused(t *testing.T) {
	k := newKeyPair(t, "k8s")
	subject := fakeerp.ExchangeSubject{Issuer: k8sIssuer, Audience: "fakeerp", Subject: k8sSubject,
		Keys: []fakeerp.PublicKey{{KID: "k8s-rsa", Key: &k.rsa.PublicKey}}}
	o := fakeerp.Options{Exchange: &fakeerp.TokenExchange{Audience: stsAudience, Subjects: []fakeerp.ExchangeSubject{subject}},
		Impersonation: &fakeerp.Impersonation{Accounts: []string{"a%2Fb@x.y"}}}
	if _, err := fakeerp.NewWithOptions(credential, filepath.Join(t.TempDir(), "erp.log"), o); err == nil {
		t.Fatal("a percent-escaped account was accepted")
	}
}
