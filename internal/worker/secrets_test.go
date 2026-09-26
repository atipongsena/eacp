package worker_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"eacp/internal/worker"
)

const canary = "s3cret-canary-4f1d"

var tenant = uuid.MustParse("00000000-0000-4000-8000-00000000000a")

func secretsFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secrets.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSecretsResolveByTenantRefAndBoundHost(t *testing.T) {
	valueFile := filepath.Join(t.TempDir(), "erp-token")
	if err := os.WriteFile(valueFile, []byte(canary+"-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := worker.LoadSecrets(secretsFile(t, fmt.Sprintf(`{"secrets":[
		{"tenant_id":%q,"secret_ref":"erp","host":"fakeerp:8090","value":%q},
		{"tenant_id":%q,"secret_ref":"bank","host":"bank.example","value_file":%q}]}`,
		tenant, canary, tenant, valueFile)))
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Resolve(tenant, "erp", "http://FakeERP:8090/v1/po")
	if err != nil || s.Reveal() != canary {
		t.Fatalf("resolve = %v", err)
	}
	if s, err := store.Resolve(tenant, "bank", "https://bank.example/pay"); err != nil || s.Reveal() != canary+"-file" {
		t.Fatalf("value_file secret = %v (trailing newline must be trimmed)", err)
	}
	for name, c := range map[string]struct {
		tenant   uuid.UUID
		ref, url string
	}{
		"other host":   {tenant, "erp", "http://evil.example:8090/v1/po"},
		"other port":   {tenant, "erp", "http://fakeerp:9999/v1/po"},
		"other tenant": {uuid.New(), "erp", "http://fakeerp:8090/v1/po"},
		"unknown ref":  {tenant, "crm", "http://fakeerp:8090/v1/po"},
		"userinfo":     {tenant, "erp", "http://x@fakeerp:8090/v1/po"},
		"not a url":    {tenant, "erp", "::"},
	} {
		if _, err := store.Resolve(c.tenant, c.ref, c.url); !errors.Is(err, worker.ErrNoCredential) {
			t.Errorf("%s: err = %v, want ErrNoCredential", name, err)
		}
	}
	b := store.Bindings()
	if len(b) != 2 || b[0].Host != "bank.example" || b[1].Ref != "erp" {
		t.Fatalf("bindings = %+v", b)
	}
}

func TestSecretsFileIsValidatedFailClosed(t *testing.T) {
	for name, body := range map[string]string{
		"not json":      `{`,
		"unknown field": fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"erp","host":"h","value":"v","x":1}]}`, tenant),
		"bad tenant":    `{"secrets":[{"tenant_id":"nope","secret_ref":"erp","host":"h","value":"v"}]}`,
		"bad ref":       fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"ERP!","host":"h","value":"v"}]}`, tenant),
		"bad host":      fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"erp","host":"http://h","value":"v"}]}`, tenant),
		"no value":      fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"erp","host":"h"}]}`, tenant),
		"both values":   fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"erp","host":"h","value":"v","value_file":"/x"}]}`, tenant),
		"missing file":  fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"erp","host":"h","value_file":"/no/such/file"}]}`, tenant),
		"duplicate":     fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"erp","host":"h","value":"v"},{"tenant_id":%q,"secret_ref":"erp","host":"g","value":"w"}]}`, tenant, tenant),
		"empty":         `{"secrets":[]}`,
	} {
		_, err := worker.LoadSecrets(secretsFile(t, body))
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), `"v"`) || strings.Contains(err.Error(), "value\":") {
			t.Errorf("%s: error echoes the file: %v", name, err)
		}
	}
	if _, err := worker.LoadSecrets(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Error("a missing secrets file was accepted")
	}
}

func TestSecretNeverPrintsLogsOrMarshals(t *testing.T) {
	store, err := worker.LoadSecrets(secretsFile(t, fmt.Sprintf(`{"secrets":[
		{"tenant_id":%q,"secret_ref":"erp","host":"fakeerp:8090","value":%q}]}`, tenant, canary)))
	if err != nil {
		t.Fatal(err)
	}
	s, _ := store.Resolve(tenant, "erp", "http://fakeerp:8090")
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	log.Info("call", "secret", s, "struct", struct{ S worker.Secret }{s})
	j, _ := json.Marshal(map[string]any{"s": s})
	out := strings.Join([]string{buf.String(), string(j), fmt.Sprint(s), fmt.Sprintf("%v %+v %#v %s %q", s, s, s, s, s),
		fmt.Sprintf("%+v", store)}, "\n")
	if strings.Contains(out, canary) {
		t.Fatalf("secret leaked: %s", out)
	}
	if v := store.Values(); len(v) != 1 || v[0] != canary {
		t.Fatal("Values must return the raw values for the log redactor")
	}
}

func TestInvalidOAuthEntriesRejectTheWholeFile(t *testing.T) {
	good := `"token_url":"https://idp.example/token","client_id":"eacp","client_secret":"s"`
	for name, oauth := range map[string]string{
		"plain http":        `"token_url":"http://idp.example/token","client_id":"eacp","client_secret":"s"`,
		"user info":         `"token_url":"https://u:p@idp.example/token","client_id":"eacp","client_secret":"s"`,
		"query":             `"token_url":"https://idp.example/token?x=1","client_id":"eacp","client_secret":"s"`,
		"fragment":          `"token_url":"https://idp.example/token#x","client_id":"eacp","client_secret":"s"`,
		"relative":          `"token_url":"/token","client_id":"eacp","client_secret":"s"`,
		"other scheme":      `"token_url":"ftp://idp.example/token","client_id":"eacp","client_secret":"s"`,
		"upper-case host":   `"token_url":"https://IDP.example/token","client_id":"eacp","client_secret":"s"`,
		"client id colon":   `"token_url":"https://idp.example/token","client_id":"a:b","client_secret":"s"`,
		"client id space":   `"token_url":"https://idp.example/token","client_id":"a b","client_secret":"s"`,
		"no client id":      `"token_url":"https://idp.example/token","client_secret":"s"`,
		"no client secret":  `"token_url":"https://idp.example/token","client_id":"eacp"`,
		"empty secret":      `"token_url":"https://idp.example/token","client_id":"eacp","client_secret":""`,
		"both secrets":      good + `,"client_secret_file":"/x"`,
		"unreadable file":   `"token_url":"https://idp.example/token","client_id":"eacp","client_secret_file":"/absent/file"`,
		"double space":      good + `,"scope":"a  b"`,
		"quoted scope":      good + `,"scope":"a\"b"`,
		"resource fragment": good + `,"resource":"https://erp.example#x"`,
		"relative resource": good + `,"resource":"erp"`,
		"unknown field":     good + `,"audience":"x"`,
	} {
		body := fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"erp","host":"erp.example","oauth2":{%s}}]}`, tenant, oauth)
		if _, err := worker.LoadSecrets(secretsFile(t, body)); err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), "idp.example") {
			t.Errorf("%s: error repeats the configuration: %v", name, err)
		}
	}
	dir := t.TempDir()
	jwtFile := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	seg := func(v string) string { return base64.RawURLEncoding.EncodeToString([]byte(v)) }
	header := seg(`{"alg":"RS256"}`)
	for name, file := range map[string]string{
		"unreadable assertion":  "/absent/file",
		"empty assertion":       jwtFile("empty", "\n"),
		"oversized assertion":   jwtFile("big", header+"."+seg(`{"exp":1}`)+"."+strings.Repeat("a", 16<<10)),
		"two segments":          jwtFile("two", header+"."+seg(`{"exp":1}`)),
		"empty signature":       jwtFile("nosig", header+"."+seg(`{"exp":1}`)+"."),
		"padded segment":        jwtFile("pad", header+"."+seg(`{"exp":1}`)+"=.c2ln"),
		"payload not an object": jwtFile("array", header+"."+seg(`[]`)+".c2ln"),
		"header not JSON":       jwtFile("hdr", seg(`alg`)+"."+seg(`{"exp":1}`)+".c2ln"),
		"no exp":                jwtFile("noexp", header+"."+seg(`{"sub":"`+canary+`"}`)+".c2ln"),
		"exp as a string":       jwtFile("strexp", header+"."+seg(`{"exp":"1"}`)+".c2ln"),
	} {
		body := fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"erp","host":"erp.example","oauth2":{"token_url":"https://idp.example/token","client_id":"eacp","client_assertion_file":%q}}]}`, tenant, file)
		if _, err := worker.LoadSecrets(secretsFile(t, body)); err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), canary) || strings.Contains(err.Error(), "c2ln") {
			t.Errorf("%s: error repeats the assertion: %v", name, err)
		}
	}
	valid := jwtFile("valid", header+"."+seg(`{"exp":1}`)+".c2ln")
	secretAndAssertion := fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"erp","host":"erp.example","oauth2":{%s,"client_assertion_file":%q}}]}`, tenant, good, valid)
	if _, err := worker.LoadSecrets(secretsFile(t, secretAndAssertion)); err == nil {
		t.Error("an entry with a client secret and an assertion was accepted")
	}
	both := fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"erp","host":"erp.example","value":"v","oauth2":{%s}}]}`, tenant, good)
	if _, err := worker.LoadSecrets(secretsFile(t, both)); err == nil {
		t.Error("an entry with a value and oauth2 was accepted")
	}
	ok := fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"erp","host":"erp.example","oauth2":{%s,"scope":"a b","resource":"https://erp.example/api"}}]}`, tenant, good)
	if _, err := worker.LoadSecrets(secretsFile(t, ok)); err != nil {
		t.Errorf("a valid entry was refused: %v", err)
	}
	file := filepath.Join(t.TempDir(), "client")
	if err := os.WriteFile(file, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fromFile := fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"erp","host":"erp.example","oauth2":{"token_url":"https://idp.example/token","client_id":"eacp","client_secret_file":%q}}]}`, tenant, file)
	s, err := worker.LoadSecrets(secretsFile(t, fromFile))
	if err != nil {
		t.Fatalf("client_secret_file: %v", err)
	}
	if v := strings.Join(s.Values(), ","); !strings.Contains(v, "from-file") || strings.Contains(v, "from-file\n") {
		t.Errorf("client_secret_file value not trimmed or missing")
	}
}
