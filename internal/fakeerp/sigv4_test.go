package fakeerp_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"eacp/internal/fakeerp"
)

const (
	sigKeyID   = "ASIAEACPSIGV4TEST001"
	sigSecret  = "sigv4-secret-key"
	sigSession = "IQoJb3JpZ2luX2Vj//+session=="
)

// signed returns a POST signed as the worker signs it, and its body.
func signed(t *testing.T, at time.Time, mutate func(*http.Request)) (*http.Request, []byte) {
	t.Helper()
	body := []byte(`{"tool":"erp.create_po","payload":{"amount":7}}`)
	req, err := http.NewRequest(http.MethodPost, "http://fakeerp:8090/v1/execute", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-EACP-Tenant-ID", "00000000-0000-4000-8000-0000000000aa")
	req.Header.Set("Idempotency-Key", "eacp:k")
	sum := sha256.Sum256(body)
	if err := v4.NewSigner().SignHTTP(context.Background(), aws.Credentials{AccessKeyID: sigKeyID, SecretAccessKey: sigSecret,
		SessionToken: sigSession}, req, hex.EncodeToString(sum[:]), "execute-api", "us-east-1", at); err != nil {
		t.Fatal(err)
	}
	// As the server sees it: the Host header, and headers the transport adds unsigned.
	req.Host = req.URL.Host
	req.Header.Set("Accept-Encoding", "gzip")
	if mutate != nil {
		mutate(req)
	}
	return req, body
}

func lookupKey(id string) (fakeerp.SigV4Key, bool) {
	if id != sigKeyID {
		return fakeerp.SigV4Key{}, false
	}
	return fakeerp.SigV4Key{SecretKey: sigSecret, SessionToken: sigSession}, true
}

func TestSigV4IsVerified(t *testing.T) {
	now := time.Now().UTC()
	req, body := signed(t, now, nil)
	id, err := fakeerp.VerifySigV4(req, body, "us-east-1", "execute-api", now, lookupKey)
	if err != nil || id != sigKeyID {
		t.Fatalf("a good signature: %q %v", id, err)
	}
	get, err := http.NewRequest(http.MethodGet, "http://fakeerp:8090/v1/operations/eacp:k", nil)
	if err != nil {
		t.Fatal(err)
	}
	empty := sha256.Sum256(nil)
	if err := v4.NewSigner().SignHTTP(context.Background(), aws.Credentials{AccessKeyID: sigKeyID, SecretAccessKey: sigSecret,
		SessionToken: sigSession}, get, hex.EncodeToString(empty[:]), "execute-api", "us-east-1", now); err != nil {
		t.Fatal(err)
	}
	get.Host = get.URL.Host
	if _, err := fakeerp.VerifySigV4(get, nil, "us-east-1", "execute-api", now, lookupKey); err != nil {
		t.Fatalf("a signed GET: %v", err)
	}
}

func TestSigV4RefusesEveryMismatch(t *testing.T) {
	now := time.Now().UTC()
	for name, c := range map[string]struct {
		at              time.Time
		mutate          func(*http.Request)
		body            func([]byte) []byte
		region, service string
	}{
		"unknown key": {mutate: func(r *http.Request) {
			r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), sigKeyID, "ASIAEACPSIGV4TEST999", 1))
		}},
		"wrong session token":   {mutate: func(r *http.Request) { r.Header.Set("X-Amz-Security-Token", "other") }},
		"wrong region":          {region: "eu-west-1"},
		"wrong service":         {service: "lambda"},
		"six minutes old":       {at: now.Add(-6 * time.Minute)},
		"six minutes ahead":     {at: now.Add(6 * time.Minute)},
		"altered body":          {body: func(b []byte) []byte { return bytes.Replace(b, []byte("7"), []byte("8"), 1) }},
		"altered signed header": {mutate: func(r *http.Request) { r.Header.Set("Idempotency-Key", "eacp:other") }},
		"removed signed header": {mutate: func(r *http.Request) { r.Header.Del("X-Eacp-Tenant-Id") }},
		"altered date":          {mutate: func(r *http.Request) { r.Header.Set("X-Amz-Date", now.Add(time.Second).Format("20060102T150405Z")) }},
		"not sigv4":             {mutate: func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+sigSecret) }},
		"token not signed": {mutate: func(r *http.Request) {
			r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), ";x-amz-security-token", "", 1))
		}},
	} {
		t.Run(name, func(t *testing.T) {
			at := now
			if !c.at.IsZero() {
				at = c.at
			}
			req, body := signed(t, at, c.mutate)
			if c.body != nil {
				body = c.body(body)
			}
			region, service := "us-east-1", "execute-api"
			if c.region != "" {
				region = c.region
			}
			if c.service != "" {
				service = c.service
			}
			if id, err := fakeerp.VerifySigV4(req, body, region, service, now, lookupKey); err == nil {
				t.Fatalf("accepted as %q", id)
			}
		})
	}
}
