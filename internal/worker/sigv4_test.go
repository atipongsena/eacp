package worker

import (
	"bytes"
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"eacp/internal/fakeerp"
)

func TestABearerSecretAuthorizes(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://erp.example/v1", nil)
	if err := (Secret{v: "tok-" + awsCanary}).Authorize(req, nil); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Authorization") != "Bearer tok-"+awsCanary || len(req.Header) != 1 {
		t.Fatalf("headers %v", req.Header)
	}
}

// TestAnAWSSecretSignsSigV4: the credential from a mint whose provider clock
// is two hours behind signs with the real clock; every header set before
// Authorize is signed, and the signature verifies.
func TestAnAWSSecretSignsSigV4(t *testing.T) {
	f := newFakeSTS(t)
	s := awsStore(t, f, &vclock{t: time.Now().Add(-2 * time.Hour)}, "", fileSubjectOf(awsSubject(t)))
	tok, err := s.Credential(context.Background(), awsTenant, "erp", awsEndpoint, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"tool":"erp.create_po"}`)
	req, _ := http.NewRequest(http.MethodPost, awsEndpoint, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-EACP-Tenant-ID", awsTenant.String())
	req.Header.Set("Idempotency-Key", "eacp:k")
	if err := tok.Authorize(req, body); err != nil {
		t.Fatal(err)
	}
	auth := req.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential="+awsKeyID+"/") || !strings.Contains(auth, "/eu-west-1/execute-api/aws4_request") ||
		req.Header.Get("X-Amz-Security-Token") != awsSession {
		t.Fatalf("headers %v", req.Header)
	}
	signed := strings.Split(strings.SplitN(strings.SplitN(auth, "SignedHeaders=", 2)[1], ",", 2)[0], ";")
	for _, h := range []string{"content-type", "host", "idempotency-key", "x-amz-date", "x-amz-security-token", "x-eacp-tenant-id"} {
		if !slices.Contains(signed, h) {
			t.Errorf("%s is not signed: %v", h, signed)
		}
	}
	at, err := time.Parse("20060102T150405Z", req.Header.Get("X-Amz-Date"))
	if err != nil || time.Since(at).Abs() > 5*time.Second {
		t.Fatalf("signed at %v, not the real clock", at)
	}
	if req.Host == "" { // what the server sees: the signer may have set Host without a default port
		req.Host = req.URL.Host
	}
	lookup := func(id string) (fakeerp.SigV4Key, bool) {
		return fakeerp.SigV4Key{SecretKey: tok.Reveal(), SessionToken: awsSession}, id == awsKeyID
	}
	if _, err := fakeerp.VerifySigV4(req, body, "eu-west-1", "execute-api", time.Now(), lookup); err != nil {
		t.Fatalf("the signature does not verify: %v", err)
	}
}

func TestContainsLooksForEverySecretPart(t *testing.T) {
	s := Secret{v: "secret-key", aws: &awsKeys{accessKeyID: awsKeyID, sessionToken: "session-token"}}
	for text, want := range map[string]bool{"a secret-key b": true, "x session-token": true, awsKeyID: false, "": false} {
		if s.Contains(text) != want {
			t.Errorf("Contains(%q) = %v", text, !want)
		}
	}
	if (Secret{}).Contains("anything") {
		t.Fatal("an empty secret is in everything")
	}
}
