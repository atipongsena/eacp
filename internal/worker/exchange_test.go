package worker_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"eacp/internal/jwttest"
	"eacp/internal/spiffetest"
	"eacp/internal/worker"
)

const (
	tokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"
	accessToken   = "urn:ietf:params:oauth:token-type:access_token"
	jwtTokenType  = "urn:ietf:params:oauth:token-type:jwt"
	stsSubject    = "system:serviceaccount:eacp:eacp-worker"
	impersonation = "/v1/projects/-/serviceAccounts/eacp-erp@eacp-demo.iam.gserviceaccount.com:generateAccessToken"
)

// subjectFile writes a platform-style JWT (the worker's projected token)
// living ttl and returns its path.
func subjectFile(t *testing.T, s *jwttest.Signer, ttl time.Duration) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	writeAssertion(t, p, assertion(s, time.Now(), ttl, stsSubject))
	return p
}

// exchangeFile is a secrets file with one token-exchange binding "erp";
// oauth is the body of its oauth2 object after the grant.
func exchangeFile(t *testing.T, spiffe, oauth string) string {
	t.Helper()
	if spiffe != "" {
		spiffe = `"spiffe":` + spiffe + `,`
	}
	return secretsFile(t, fmt.Sprintf(`{%s"secrets":[{"tenant_id":%q,"secret_ref":"erp","host":"erp.internal:8443",
		"oauth2":{"grant":"token_exchange",%s}}]}`, spiffe, tenant, oauth))
}

func TestATokenExchangeEntryLoads(t *testing.T) {
	file := subjectFile(t, jwttest.New(t), time.Hour)
	base := fmt.Sprintf(`"token_url":"http://127.0.0.1:9/token","subject_token":{"file":%q}`, file)
	imp := `"impersonate":{"url":"http://127.0.0.1:9` + impersonation + `","scope":["https://www.googleapis.com/auth/cloud-platform"]}`
	for name, oauth := range map[string]string{
		"no client authentication": base + `,"audience":"//iam.googleapis.com/projects/1/x","scope":"a b",` + imp,
		"public client":            base + `,"client_id":"eacp"`,
		"client secret":            base + `,"client_id":"eacp","client_secret":"s"`,
		"id_token subject":         base + `,"subject_token_type":"urn:ietf:params:oauth:token-type:id_token"`,
		"id-token subject":         base + `,"subject_token_type":"urn:ietf:params:oauth:token-type:id-token"`,
		"a lifetime":               base + `,"impersonate":{"url":"http://127.0.0.1:9` + impersonation + `","scope":["s"],"lifetime_seconds":300}`,
	} {
		if _, err := worker.LoadSecrets(exchangeFile(t, "", oauth), worker.AllowPlainTokenURL()); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	a := spiffetest.New(t)
	spiffe := fmt.Sprintf(`{"endpoint":%q,"spiffe_id":%q}`, a.Addr(), spiffeWorker)
	if _, err := worker.LoadSecrets(exchangeFile(t, spiffe, `"token_url":"http://127.0.0.1:9/token","subject_token":{"spiffe":{"audience":"sts"}}`),
		worker.AllowPlainTokenURL()); err != nil {
		t.Fatal(err)
	}
	if n := a.Fetches("sts"); n != 0 {
		t.Fatalf("the agent was asked %d times at load", n)
	}
}

func TestInvalidTokenExchangeEntriesRejectTheWholeFile(t *testing.T) {
	file := subjectFile(t, jwttest.New(t), time.Hour)
	subject := fmt.Sprintf(`"subject_token":{"file":%q}`, file)
	base := `"token_url":"https://sts.example/token",` + subject
	secret := `"token_url":"https://idp.example/token","client_id":"eacp","client_secret":"s"`
	imp := func(url, rest string) string {
		return base + `,"impersonate":{"url":"` + url + `","scope":["s"]` + rest + `}`
	}
	good := "https://iam.example" + impersonation
	scopes := make([]string, 33)
	for i := range scopes {
		scopes[i] = fmt.Sprintf("%q", fmt.Sprint("s", i))
	}
	for name, body := range map[string]string{
		"unknown grant":                     `"grant":"password",` + secret,
		"subject without grant":             secret + `,` + subject,
		"audience without grant":            secret + `,"audience":"x"`,
		"type without grant":                secret + `,"subject_token_type":"` + jwtTokenType + `"`,
		"impersonate without grant":         secret + `,"impersonate":{"url":"` + good + `","scope":["s"]}`,
		"client credentials without a form": `"token_url":"https://idp.example/token","client_id":"eacp"`,
	} {
		b := fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"erp","host":"erp.example","oauth2":{%s}}]}`, tenant, body)
		if _, err := worker.LoadSecrets(secretsFile(t, b)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for name, oauth := range map[string]string{
		"no subject":            `"token_url":"https://sts.example/token"`,
		"both subjects":         `"token_url":"https://sts.example/token","subject_token":{"file":"` + filepath.ToSlash(file) + `","spiffe":{"audience":"a"}}`,
		"empty subject":         `"token_url":"https://sts.example/token","subject_token":{}`,
		"spiffe without block":  `"token_url":"https://sts.example/token","subject_token":{"spiffe":{"audience":"a"}}`,
		"unreadable subject":    `"token_url":"https://sts.example/token","subject_token":{"file":"/absent/token"}`,
		"bad subject type":      base + `,"subject_token_type":"urn:ietf:params:oauth:token-type:saml2"`,
		"audience with a space": base + `,"audience":"a b"`,
		"audience too long":     base + `,"audience":"` + strings.Repeat("a", 1025) + `"`,
		"secret without id":     base + `,"client_secret":"s"`,
		"two forms":             base + `,"client_id":"eacp","client_secret":"s","client_secret_file":"/x"`,
		"plain http sts":        `"token_url":"http://sts.example/token",` + subject,
		"imp query":             imp(good+"?x=1", ""),
		"imp user info":         imp("https://u:p@iam.example"+impersonation, ""),
		"imp wrong path":        imp("https://iam.example/v1/projects/p/serviceAccounts/a@b.c:generateAccessToken", ""),
		"imp no at":             imp("https://iam.example/v1/projects/-/serviceAccounts/nobody:generateAccessToken", ""),
		"imp wrong verb":        imp("https://iam.example/v1/projects/-/serviceAccounts/a@b.c:generateIdToken", ""),
		"imp plain http":        imp("http://iam.example"+impersonation, ""),
		"imp no scope":          base + `,"impersonate":{"url":"` + good + `","scope":[]}`,
		"imp 33 scopes":         base + `,"impersonate":{"url":"` + good + `","scope":[` + strings.Join(scopes, ",") + `]}`,
		"imp scope with space":  base + `,"impersonate":{"url":"` + good + `","scope":["a b"]}`,
		"imp lifetime 299":      imp(good, `,"lifetime_seconds":299`),
		"imp lifetime 3601":     imp(good, `,"lifetime_seconds":3601`),
		"imp unknown field":     imp(good, `,"delegates":["x"]`),
	} {
		if _, err := worker.LoadSecrets(exchangeFile(t, "", oauth)); err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), "example") || strings.Contains(err.Error(), "eacp-erp") {
			t.Errorf("%s: error repeats the configuration: %v", name, err)
		} else if name != "imp unknown field" && strings.Contains(err.Error(), "expected shape") {
			t.Errorf("%s: refused as an unknown field, not by validation: %v", name, err)
		}
	}
}
