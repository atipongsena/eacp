// Package jwttest signs RS256 JSON Web Tokens and publishes their key as a
// JWKS, for tests of workload identity federation (ADR-019). It is imported
// by tests only.
package jwttest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"testing"
)

// Signer holds an RSA key and the key id it signs under.
type Signer struct {
	Key *rsa.PrivateKey
	KID string
}

// New returns a signer with a fresh 2048-bit key and key id "test-key".
func New(t testing.TB) *Signer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &Signer{Key: key, KID: "test-key"}
}

// Sign returns claims as a compact RS256 JWS under the signer's key id.
func (s *Signer) Sign(claims map[string]any) string {
	return s.SignWith(map[string]any{"alg": "RS256", "kid": s.KID, "typ": "JWT"}, claims)
}

// SignWith returns claims with any header, signed RS256 whatever the header
// says, for tests of refused algorithms.
func (s *Signer) SignWith(header, claims map[string]any) string {
	input := segment(header) + "." + segment(claims)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.Key, crypto.SHA256, sum[:])
	if err != nil {
		panic(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// JWKS returns the signer's public key as a JSON Web Key Set.
func (s *Signer) JWKS() []byte {
	pub := s.Key.PublicKey
	b, _ := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "kid": s.KID, "use": "sig", "alg": "RS256",
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}})
	return b
}

func segment(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
