package connector_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/connector"
	"github.com/atipongsena/eacp/internal/fakeerp"
	"github.com/atipongsena/eacp/internal/jwttest"
	"github.com/atipongsena/eacp/internal/worker"
)

const token = "connector-test-canary"

func call(t *testing.T, endpoint, mode string) worker.Call {
	t.Helper()
	tenant := uuid.New()
	p := filepath.Join(t.TempDir(), "secrets.json")
	body := fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"erp","host":%q,"value":%q}]}`,
		tenant, strings.TrimPrefix(endpoint, "http://"), token)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := worker.LoadSecrets(p)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := store.Resolve(tenant, "erp", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	return worker.Call{
		TenantID: tenant, ActionID: uuid.New(), OperationKey: "eacp:" + tenant.String() + ":" + uuid.NewString(),
		Tool: "erp.create_po", Endpoint: endpoint, Payload: json.RawMessage(`{"amount":42,"currency":"THB"}`),
		Secret: secret, Contract: worker.Contract{IdempotencyMode: mode, IdempotencyKeyField: "Idempotency-Key",
			CorrelationField: "external_reference", NoEffectErrors: []string{"validation", "fakeerp_5xx_before_effect", "connection_refused_before_send"}},
	}
}

func TestHTTPExecutePreservesEnforcedPayloadAndNativeKey(t *testing.T) {
	var called atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Add(1)
		if r.URL.Path != "/v1/execute" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+token ||
			r.Header.Get("Idempotency-Key") == "" || r.Header.Get("X-EACP-Tenant-ID") == "" {
			t.Errorf("unexpected request path=%q method=%q", r.URL.Path, r.Method)
		}
		var body struct {
			Tool    string          `json:"tool"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Tool != "erp.create_po" ||
			string(body.Payload) != `{"amount":42,"currency":"THB"}` {
			t.Errorf("body = %+v, err = %v", body, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"external_reference":"PO-42"}`))
	}))
	defer srv.Close()
	c := call(t, srv.URL, "native")
	res := connector.NewHTTP().Execute(context.Background(), c)
	if res.Outcome != worker.Succeeded || res.ExternalReference != "PO-42" || called.Load() != 1 {
		t.Fatalf("result = %+v, calls = %d", res, called.Load())
	}
}

func TestHTTPCorrelationKeyUsesDeclaredField(t *testing.T) {
	var expectedKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Idempotency-Key") != "" {
			t.Error("correlation-only request carried native idempotency header")
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if string(body["external_reference"]) != `"`+expectedKey+`"` {
			t.Errorf("correlation field = %s", body["external_reference"])
		}
		_, _ = w.Write([]byte(`{"external_reference":"PO-1"}`))
	}))
	defer srv.Close()
	c := call(t, srv.URL, "correlation_only")
	expectedKey = c.OperationKey
	if got := connector.NewHTTP().Execute(context.Background(), c); got.Outcome != worker.Succeeded {
		t.Fatalf("result = %+v", got)
	}
}

func TestHTTPLookupRejectsNonCanonicalOperationKey(t *testing.T) {
	var called atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Add(1)
	}))
	defer srv.Close()
	c := call(t, srv.URL, "native")
	got := connector.NewHTTP().Lookup(context.Background(), worker.LookupCall{
		TenantID: c.TenantID, OperationKey: "../audit", Endpoint: c.Endpoint, Secret: c.Secret,
	})
	if got.Status != worker.LookupUnknown || called.Load() != 0 {
		t.Fatalf("invalid key lookup = %+v, requests = %d", got, called.Load())
	}
}

func TestHTTPClassifiesOnlyCertifiedNoEffectAndTreatsLostResponsesAsAmbiguous(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		status         int
		want           worker.Outcome
	}{
		{"validation", `{"error_class":"validation"}`, 422, worker.NoEffect},
		{"certified 5xx before effect", `{"error_class":"fakeerp_5xx_before_effect"}`, 503, worker.NoEffect},
		{"contradictory error with reference", `{"error_class":"validation","external_reference":"PO-created"}`, 422, worker.Ambiguous},
		{"rate limited", `{"error_class":"rate_limited"}`, 429, worker.Ambiguous},
		{"uncertified 5xx", `{"error_class":"after_effect"}`, 503, worker.Ambiguous},
		{"success without reference", `{}`, 200, worker.Ambiguous},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.response))
			}))
			defer srv.Close()
			got := connector.NewHTTP().Execute(context.Background(), call(t, srv.URL, "native"))
			if got.Outcome != tc.want {
				t.Fatalf("outcome = %s, want %s", got.Outcome, tc.want)
			}
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(80 * time.Millisecond)
		_, _ = w.Write([]byte(`{"external_reference":"PO-late"}`))
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if got := connector.NewHTTP().Execute(ctx, call(t, srv.URL, "native")); got.Outcome != worker.Ambiguous {
		t.Fatalf("timed out call = %+v", got)
	}
}

func TestHTTPDoesNotForwardCredentialOnRedirect(t *testing.T) {
	var forwarded atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("credential reached redirected target")
		}
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/stolen", http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	got := connector.NewHTTP().Execute(context.Background(), call(t, source.URL, "native"))
	if got.Outcome != worker.Ambiguous || forwarded.Load() != 0 {
		t.Fatalf("redirect result = %+v, forwarded = %d", got, forwarded.Load())
	}
}

func TestHTTPExecuteDoesNotReplayAfterAReusedConnectionLosesResponse(t *testing.T) {
	var requests atomic.Int32
	var firstRemote atomic.Value
	var reused atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch requests.Add(1) {
		case 1:
			firstRemote.Store(r.RemoteAddr)
			_, _ = w.Write([]byte(`{"external_reference":"PO-prime"}`))
		case 2:
			reused.Store(r.RemoteAddr == firstRemote.Load().(string))
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close() // the effect happened, but no response was sent
		default:
			_, _ = w.Write([]byte(`{"external_reference":"PO-replayed"}`))
		}
	}))
	defer srv.Close()
	h := connector.NewHTTP()
	if got := h.Execute(context.Background(), call(t, srv.URL, "native")); got.Outcome != worker.Succeeded {
		t.Fatalf("priming call = %+v", got)
	}
	got := h.Execute(context.Background(), call(t, srv.URL, "native"))
	if !reused.Load() {
		t.Fatal("test did not exercise a reused connection")
	}
	if got.Outcome != worker.Ambiguous || requests.Load() != 2 {
		t.Fatalf("lost response = %+v, network requests = %d", got, requests.Load())
	}
}

func TestHTTPLookupDistinguishesFoundAbsentAndUnknown(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		status         int
		want           worker.LookupStatus
	}{
		{"found", `{"external_reference":"PO-42"}`, 200, worker.LookupFound},
		{"absent", `{"error_class":"not_found"}`, 404, worker.LookupAbsent},
		{"router 404", `{}`, 404, worker.LookupUnknown},
		{"conflict", `{"error_class":"conflict"}`, 409, worker.LookupConflict},
		{"conflict with a reference", `{"error_class":"conflict","external_reference":"PO-1"}`, 409, worker.LookupUnknown},
		{"other 409", `{"error_class":"busy"}`, 409, worker.LookupUnknown},
		{"unavailable", `{}`, 503, worker.LookupUnknown},
		{"malformed", `{}`, 200, worker.LookupUnknown},
		{"credential echoed", `{"external_reference":"connector-test-canary"}`, 200, worker.LookupUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasPrefix(r.URL.Path, "/v1/operations/eacp:") || r.Header.Get("Authorization") != "Bearer "+token {
					t.Errorf("unexpected lookup request path = %s", r.URL.Path)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.response))
			}))
			defer srv.Close()
			c := call(t, srv.URL, "native")
			got := connector.NewHTTP().Lookup(context.Background(), worker.LookupCall{
				TenantID: c.TenantID, OperationKey: c.OperationKey, Endpoint: c.Endpoint, Secret: c.Secret,
			})
			if got.Status != tc.want || (tc.want == worker.LookupFound && got.ExternalReference != "PO-42") {
				t.Fatalf("lookup = %+v, want %s", got, tc.want)
			}
		})
	}
}

const (
	awsSecretKey = "connector-aws-secret-canary"
	awsSession   = "IQoJconnector//+session-canary=="
	awsKeyID     = "ASIACONNECTORTEST001"
)

// awsCall is call with temporary AWS keys minted from a fake STS for the
// endpoint (region us-east-1, service execute-api).
func awsCall(t *testing.T, endpoint string) worker.Call {
	t.Helper()
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprintf(w, `<AssumeRoleWithWebIdentityResponse><AssumeRoleWithWebIdentityResult><Credentials>`+
			`<AccessKeyId>%s</AccessKeyId><SecretAccessKey>%s</SecretAccessKey><SessionToken>%s</SessionToken>`+
			`<Expiration>%s</Expiration></Credentials></AssumeRoleWithWebIdentityResult></AssumeRoleWithWebIdentityResponse>`,
			awsKeyID, awsSecretKey, awsSession, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	}))
	t.Cleanup(sts.Close)
	now := time.Now()
	subject := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(subject, []byte(jwttest.New(t).Sign(map[string]any{"sub": "worker", "aud": "sts",
		"exp": now.Add(time.Hour).Unix()})), 0o600); err != nil {
		t.Fatal(err)
	}
	tenant := uuid.New()
	p := filepath.Join(t.TempDir(), "secrets.json")
	body := fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"erp","host":%q,"aws":{"role_arn":"arn:aws:iam::123456789012:role/eacp",
		"region":"us-east-1","service":"execute-api","sts_endpoint":%q,"subject_token":{"file":%q}}}]}`,
		tenant, strings.TrimPrefix(endpoint, "http://"), sts.URL, subject)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := worker.LoadSecrets(p, worker.AllowPlainTokenURL())
	if err != nil {
		t.Fatal(err)
	}
	secret, err := store.Credential(context.Background(), tenant, "erp", endpoint, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	c := call(t, endpoint, "native")
	c.TenantID, c.Secret = tenant, secret
	c.OperationKey = "eacp:" + tenant.String() + ":" + uuid.NewString()
	return c
}

// verified reports the SigV4 verification of r against the keys awsCall mints.
func verified(r *http.Request) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	_, err = fakeerp.VerifySigV4(r, body, "us-east-1", "execute-api", time.Now(), func(id string) (fakeerp.SigV4Key, bool) {
		return fakeerp.SigV4Key{SecretKey: awsSecretKey, SessionToken: awsSession}, id == awsKeyID
	})
	return err
}

func TestExecuteSignsAnAWSCredential(t *testing.T) {
	var signed []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := verified(r); err != nil {
			t.Errorf("execute: %v", err)
		}
		auth := r.Header.Get("Authorization")
		signed = strings.Split(strings.SplitN(strings.SplitN(auth, "SignedHeaders=", 2)[1], ",", 2)[0], ";")
		_, _ = w.Write([]byte(`{"external_reference":"PO-7"}`))
	}))
	defer srv.Close()
	if res := connector.NewHTTP().Execute(context.Background(), awsCall(t, srv.URL)); res.Outcome != worker.Succeeded {
		t.Fatalf("result = %+v", res)
	}
	for _, h := range []string{"content-type", "idempotency-key", "x-eacp-tenant-id", "x-amz-security-token"} {
		if !slices.Contains(signed, h) {
			t.Errorf("%s is not signed: %v", h, signed)
		}
	}
}

func TestLookupSignsAnAWSCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := verified(r); err != nil {
			t.Errorf("lookup: %v", err)
		}
		_, _ = w.Write([]byte(`{"external_reference":"PO-7"}`))
	}))
	defer srv.Close()
	c := awsCall(t, srv.URL)
	got := connector.NewHTTP().Lookup(context.Background(), worker.LookupCall{TenantID: c.TenantID, OperationKey: c.OperationKey,
		Endpoint: c.Endpoint, Secret: c.Secret})
	if got.Status != worker.LookupFound {
		t.Fatalf("lookup = %+v", got)
	}
}

func TestALookupReferenceWithTheSessionTokenIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"external_reference":"PO-` + awsSession + `"}`))
	}))
	defer srv.Close()
	c := awsCall(t, srv.URL)
	got := connector.NewHTTP().Lookup(context.Background(), worker.LookupCall{TenantID: c.TenantID, OperationKey: c.OperationKey,
		Endpoint: c.Endpoint, Secret: c.Secret})
	if got.Status != worker.LookupUnknown {
		t.Fatalf("a reference echoing the session token: %+v", got)
	}
}

// TestAKeyHeaderTheSignerOwnsIsRefused: SigV4 overwrites X-Amz-Date and
// X-Amz-Security-Token and leaves some headers unsigned, so a native key in
// one of them would be lost or unprotected; the execute is refused before
// anything is sent.
func TestAKeyHeaderTheSignerOwnsIsRefused(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"external_reference":"PO-7"}`))
	}))
	defer srv.Close()
	for _, name := range []string{"X-Amz-Date", "x-amz-security-token", "X-Amz-Content-Sha256", "User-Agent",
		"X-Amzn-Trace-Id", "Expect", "Transfer-Encoding"} {
		c := awsCall(t, srv.URL)
		c.Contract.IdempotencyKeyField = name
		if res := connector.NewHTTP().Execute(context.Background(), c); res.Outcome != worker.Ambiguous || res.ErrorClass != "invalid_contract" {
			t.Errorf("%s: result = %+v", name, res)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("the target was called %d times", calls.Load())
	}
}

// ADR-034: the execute response's result is a success's output; a failure
// carries none, and a response up to 128 KiB is read.
func TestHTTPReturnsASuccessesResult(t *testing.T) {
	big := strings.Repeat("x", 100<<10)
	for name, tc := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"object":      {200, `{"external_reference":"PO-1","result":{"po":"PO-1","lines":[1,2]}}`, `{"po":"PO-1","lines":[1,2]}`},
		"string":      {200, `{"external_reference":"PO-1","result":"ok"}`, `"ok"`},
		"no result":   {200, `{"external_reference":"PO-1"}`, ``},
		"null result": {200, `{"external_reference":"PO-1","result":null}`, ``},
		"large":       {200, `{"external_reference":"PO-1","result":"` + big + `"}`, `"` + big + `"`},
		"failure":     {400, `{"error_class":"validation","result":{"why":"x"}}`, ``},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		res := connector.NewHTTP().Execute(context.Background(), call(t, srv.URL, "native"))
		srv.Close()
		if string(res.Output) != tc.want {
			t.Errorf("%s: output = %.80q (outcome %s), want %.80q", name, res.Output, res.Outcome, tc.want)
		}
		if tc.status == 200 && res.Outcome != worker.Succeeded {
			t.Errorf("%s: outcome %s", name, res.Outcome)
		}
	}
}
