package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// Authorize authenticates req, whose body is body, with s: a Bearer
// credential sets the Authorization header; temporary AWS keys sign it with
// SigV4 for their region and service (ADR-019 §3f). Call it after every other
// header is set, so they are signed. It signs with the real clock: the target
// judges the date, whatever clock the provider was given.
func (s Secret) Authorize(req *http.Request, body []byte) error {
	if s.aws == nil {
		req.Header.Set("Authorization", "Bearer "+s.v)
		return nil
	}
	sum := sha256.Sum256(body)
	return v4.NewSigner().SignHTTP(req.Context(), aws.Credentials{AccessKeyID: s.aws.accessKeyID, SecretAccessKey: s.v,
		SessionToken: s.aws.sessionToken}, req, hex.EncodeToString(sum[:]), s.aws.service, s.aws.region, time.Now().UTC())
}

// Contains reports whether text contains any secret part of s.
func (s Secret) Contains(text string) bool {
	for _, v := range s.values() {
		if strings.Contains(text, v) {
			return true
		}
	}
	return false
}
