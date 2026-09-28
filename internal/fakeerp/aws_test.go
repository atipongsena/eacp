package fakeerp_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/fakeerp"
)

const (
	awsRole       = "arn:aws:iam::000000000000:role/eacp-erp"
	awsAssumed    = "arn:aws:sts::000000000000:assumed-role/eacp-erp/"
	awsStsAud     = "sts.amazonaws.com"
	awsRegion     = "us-east-1"
	awsService    = "execute-api"
	awsVersionSTS = "2011-06-15"
)

type awsKeys struct {
	AccessKeyID, SecretAccessKey, SessionToken, Expiration, Arn string
}

// awsERP trusts a Kubernetes token (audience fakeerp) and a JWT-SVID
// (audience sts.amazonaws.com) for role eacp-erp; path is its log.
func awsERP(t *testing.T, path string, k8s, spire *keyPair) *httptest.Server {
	t.Helper()
	h, err := fakeerp.NewWithOptions(credential, path, fakeerp.Options{AWS: &fakeerp.AWS{RoleARN: awsRole, Region: awsRegion,
		Service: awsService, Subjects: []fakeerp.ExchangeSubject{
			{Issuer: k8sIssuer, Audience: "fakeerp", Subject: k8sSubject, Keys: []fakeerp.PublicKey{{KID: "k8s-rsa", Key: &k8s.rsa.PublicKey}}},
			{Issuer: spiffeIssuer, Audience: awsStsAud, Subject: spiffeWorker, Keys: []fakeerp.PublicKey{{KID: "spire-ec", Key: &spire.ec.PublicKey}}},
		}}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func assumeForm(token, session string) url.Values {
	return url.Values{"Action": {"AssumeRoleWithWebIdentity"}, "Version": {awsVersionSTS}, "RoleArn": {awsRole},
		"RoleSessionName": {session}, "WebIdentityToken": {token}}
}

// assume posts form to the STS and parses the keys of a 200.
func assume(t *testing.T, srv *httptest.Server, form url.Values) (int, awsKeys, string) {
	t.Helper()
	resp, err := http.PostForm(srv.URL+"/aws/sts", form)
	if err != nil {
		t.Fatal(err)
	}
	raw := body(t, resp)
	var r struct {
		Result struct {
			User        struct{ Arn string } `xml:"AssumedRoleUser"`
			Credentials struct {
				AccessKeyID     string `xml:"AccessKeyId"`
				SecretAccessKey string
				SessionToken    string
				Expiration      string
			}
		} `xml:"AssumeRoleWithWebIdentityResult"`
	}
	_ = xml.Unmarshal(raw, &r)
	c := r.Result.Credentials
	return resp.StatusCode, awsKeys{c.AccessKeyID, c.SecretAccessKey, c.SessionToken, c.Expiration, r.Result.User.Arn}, string(raw)
}

// signedExecute executes a purchase signed with k for region and service.
func signedExecute(t *testing.T, srv *httptest.Server, k awsKeys, region, service string) int {
	t.Helper()
	tenant := uuid.New()
	payload := []byte(`{"tool":"erp.create_po","payload":{"amount":1}}`)
	req, _ := http.NewRequest("POST", srv.URL+"/v1/execute", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-EACP-Tenant-ID", tenant.String())
	req.Header.Set("Idempotency-Key", "eacp:"+tenant.String()+":"+uuid.NewString())
	sum := sha256.Sum256(payload)
	if err := v4.NewSigner().SignHTTP(context.Background(), aws.Credentials{AccessKeyID: k.AccessKeyID,
		SecretAccessKey: k.SecretAccessKey, SessionToken: k.SessionToken}, req, hex.EncodeToString(sum[:]), service, region, time.Now()); err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestAssumeRoleIssuesKeysForTheSubject(t *testing.T) {
	k8s, spire := newKeyPair(t, "k8s"), newKeyPair(t, "spire")
	path := filepath.Join(t.TempDir(), "erp.log")
	srv := awsERP(t, path, k8s, spire)
	svid := spire.sign(map[string]any{"alg": "ES256", "kid": "spire-ec"}, svidClaims(awsStsAud))
	issued := map[string]awsKeys{}
	for session, token := range map[string]string{"eacp-worker-k8s": k8sToken(k8s, nil), "eacp-worker-spiffe": svid} {
		status, k, raw := assume(t, srv, assumeForm(token, session))
		exp, err := time.Parse(time.RFC3339, k.Expiration)
		if status != 200 || len(k.AccessKeyID) != 20 || !strings.HasPrefix(k.AccessKeyID, "ASIA") || k.SecretAccessKey == "" ||
			len(k.SessionToken) != 43 || err != nil || exp.Sub(time.Now()) < 59*time.Minute || k.Arn != awsAssumed+session {
			t.Fatalf("%s: %d %s", session, status, raw)
		}
		if code := signedExecute(t, srv, k, awsRegion, awsService); code != 200 {
			t.Fatalf("%s: a signed execute answered %d", session, code)
		}
		issued[session] = k
	}
	_, raw := auditOf(t, srv)
	var entries []map[string]any
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		t.Fatal(err)
	}
	executes := 0
	for _, e := range entries {
		if e["path"] == "/v1/execute" {
			executes++
			p, _ := e["principal"].(string)
			session := strings.TrimPrefix(p, "aws:"+awsAssumed)
			if k, ok := issued[session]; !ok || e["aws_access_key_id"] != k.AccessKeyID {
				t.Fatalf("execute audit %v", e)
			}
		}
	}
	for _, k := range issued {
		if strings.Contains(raw, k.SecretAccessKey) || strings.Contains(raw, k.SessionToken) {
			t.Fatal("the audit holds a secret key or session token")
		}
	}
	if executes != 2 {
		t.Fatalf("%d executes", executes)
	}
	srv.Close()
	again := awsERP(t, path, k8s, spire) // the keys survive a restart
	if code := signedExecute(t, again, issued["eacp-worker-k8s"], awsRegion, awsService); code != 200 {
		t.Fatalf("after a restart: %d", code)
	}
}

func TestAssumeRoleRefusesBadRequests(t *testing.T) {
	k8s, spire := newKeyPair(t, "k8s"), newKeyPair(t, "spire")
	srv := awsERP(t, filepath.Join(t.TempDir(), "erp.log"), k8s, spire)
	good := k8sToken(k8s, nil)
	with := func(k, v string) url.Values { f := assumeForm(good, "s1"); f.Set(k, v); return f }
	without := func(k string) url.Values { f := assumeForm(good, "s1"); f.Del(k); return f }
	for name, c := range map[string]struct {
		form   url.Values
		status int
		code   string
	}{
		"wrong action":     {with("Action", "AssumeRole"), 400, "InvalidAction"},
		"wrong version":    {with("Version", "2012-01-01"), 400, "InvalidParameterValue"},
		"no token":         {without("WebIdentityToken"), 400, "MissingParameter"},
		"no role":          {without("RoleArn"), 400, "MissingParameter"},
		"no session":       {without("RoleSessionName"), 400, "MissingParameter"},
		"bad session":      {with("RoleSessionName", "a b"), 400, "InvalidParameterValue"},
		"short duration":   {with("DurationSeconds", "899"), 400, "InvalidParameterValue"},
		"long duration":    {with("DurationSeconds", "3601"), 400, "InvalidParameterValue"},
		"other role":       {with("RoleArn", "arn:aws:iam::000000000000:role/other"), 403, "AccessDenied"},
		"bad token":        {with("WebIdentityToken", good+"x"), 400, "InvalidIdentityToken"},
		"expired token":    {with("WebIdentityToken", k8sToken(k8s, func(c map[string]any) { c["exp"] = time.Now().Unix() - 60 })), 400, "InvalidIdentityToken"},
		"wrong audience":   {with("WebIdentityToken", k8sToken(k8s, func(c map[string]any) { c["aud"] = []string{"other"} })), 400, "InvalidIdentityToken"},
		"svid for fakeerp": {with("WebIdentityToken", spire.sign(map[string]any{"alg": "ES256", "kid": "spire-ec"}, svidClaims("fakeerp"))), 400, "InvalidIdentityToken"},
	} {
		status, k, raw := assume(t, srv, c.form)
		if status != c.status || k.AccessKeyID != "" || !strings.Contains(raw, "<Code>"+c.code+"</Code>") {
			t.Errorf("%s: %d %s", name, status, raw)
		}
	}
}

func TestTheERPVerifiesSigV4(t *testing.T) {
	k8s, spire := newKeyPair(t, "k8s"), newKeyPair(t, "spire")
	srv := awsERP(t, filepath.Join(t.TempDir(), "erp.log"), k8s, spire)
	_, k, _ := assume(t, srv, assumeForm(k8sToken(k8s, nil), "s1"))
	for name, c := range map[string]struct {
		keys            awsKeys
		region, service string
	}{
		"wrong region":        {k, "eu-west-1", awsService},
		"wrong service":       {k, awsRegion, "lambda"},
		"wrong secret":        {awsKeys{AccessKeyID: k.AccessKeyID, SecretAccessKey: "x", SessionToken: k.SessionToken}, awsRegion, awsService},
		"wrong session token": {awsKeys{AccessKeyID: k.AccessKeyID, SecretAccessKey: k.SecretAccessKey, SessionToken: "x"}, awsRegion, awsService},
		"unknown key":         {awsKeys{AccessKeyID: "ASIAUNKNOWNKEY000001", SecretAccessKey: k.SecretAccessKey, SessionToken: k.SessionToken}, awsRegion, awsService},
	} {
		if code := signedExecute(t, srv, c.keys, c.region, c.service); code != 401 {
			t.Errorf("%s: %d", name, code)
		}
	}
}

func TestAWSOptionsFailClosed(t *testing.T) {
	k := newKeyPair(t, "k8s")
	subject := fakeerp.ExchangeSubject{Issuer: k8sIssuer, Audience: "fakeerp", Subject: k8sSubject,
		Keys: []fakeerp.PublicKey{{KID: "k8s-rsa", Key: &k.rsa.PublicKey}}}
	for name, a := range map[string]*fakeerp.AWS{
		"no role":     {Region: awsRegion, Service: awsService, Subjects: []fakeerp.ExchangeSubject{subject}},
		"bad role":    {RoleARN: "arn:aws:iam::0:role/x", Region: awsRegion, Service: awsService, Subjects: []fakeerp.ExchangeSubject{subject}},
		"no region":   {RoleARN: awsRole, Service: awsService, Subjects: []fakeerp.ExchangeSubject{subject}},
		"no service":  {RoleARN: awsRole, Region: awsRegion, Subjects: []fakeerp.ExchangeSubject{subject}},
		"no subjects": {RoleARN: awsRole, Region: awsRegion, Service: awsService},
		"bad subject": {RoleARN: awsRole, Region: awsRegion, Service: awsService, Subjects: []fakeerp.ExchangeSubject{{Subject: "s"}}},
	} {
		if _, err := fakeerp.NewWithOptions(credential, filepath.Join(t.TempDir(), "erp.log"), fakeerp.Options{AWS: a}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
