package fakeerp

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// SigV4 verification (ADR-019 §3f) for the demo and tests: a request is
// re-signed from exactly its signed headers with the pinned aws-sdk-go-v2
// signer, and the signatures are compared.

const (
	amzDate    = "20060102T150405Z"
	maxSkewV4  = 5 * time.Minute
	sigV4Label = "AWS4-HMAC-SHA256 "
)

var sigV4Authorization = regexp.MustCompile(`^AWS4-HMAC-SHA256 Credential=([A-Z0-9]{16,128})/([0-9]{8})/([a-z0-9-]+)/([a-z0-9-]+)/aws4_request, ?SignedHeaders=([a-z0-9;-]+), ?Signature=([0-9a-f]{64})$`)

// SigV4Key is what a verifier knows about an access key id.
type SigV4Key struct {
	SecretKey, SessionToken string
}

// VerifySigV4 verifies r, whose body is body, as signed for region and
// service within five minutes of now by a key lookup knows, with its session
// token. It returns the access key id. Its errors never contain a secret.
func VerifySigV4(r *http.Request, body []byte, region, service string, now time.Time,
	lookup func(keyID string) (SigV4Key, bool)) (string, error) {
	auth := r.Header.Get("Authorization")
	m := sigV4Authorization.FindStringSubmatch(auth)
	if m == nil {
		return "", errors.New("sigv4: malformed authorization")
	}
	keyID, date, signedHeaders := m[1], m[2], strings.Split(m[5], ";")
	if m[3] != region || m[4] != service {
		return "", errors.New("sigv4: wrong credential scope")
	}
	at, err := time.Parse(amzDate, r.Header.Get("X-Amz-Date"))
	if err != nil || at.Format("20060102") != date || at.Sub(now) > maxSkewV4 || now.Sub(at) > maxSkewV4 {
		return "", errors.New("sigv4: stale or malformed date")
	}
	for _, need := range []string{"host", "x-amz-date", "x-amz-security-token"} {
		if !slices.Contains(signedHeaders, need) {
			return "", errors.New("sigv4: " + need + " is not signed")
		}
	}
	key, ok := lookup(keyID)
	if !ok || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Amz-Security-Token")), []byte(key.SessionToken)) != 1 {
		return "", errors.New("sigv4: unknown key or session token")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, scheme+"://"+r.Host+r.URL.RequestURI(), bytes.NewReader(body))
	if err != nil {
		return "", errors.New("sigv4: cannot rebuild the request")
	}
	for _, name := range signedHeaders {
		if name == "host" || name == "content-length" {
			continue // the signer takes both from the request itself
		}
		values := r.Header.Values(name)
		if len(values) == 0 {
			return "", errors.New("sigv4: a signed header is missing")
		}
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	sum := sha256.Sum256(body)
	if err := v4.NewSigner().SignHTTP(r.Context(), aws.Credentials{AccessKeyID: keyID, SecretAccessKey: key.SecretKey,
		SessionToken: key.SessionToken}, req, hex.EncodeToString(sum[:]), service, region, at); err != nil {
		return "", errors.New("sigv4: cannot sign")
	}
	if subtle.ConstantTimeCompare([]byte(req.Header.Get("Authorization")), []byte(auth)) != 1 {
		return "", errors.New("sigv4: signature mismatch")
	}
	return keyID, nil
}
