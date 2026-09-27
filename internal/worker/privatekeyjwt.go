package worker

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"regexp"
	"strings"
	"time"

	"eacp/internal/logging"
)

const (
	// assertionLifetime: Entra ID advises 5-10 minutes; Okta allows at most
	// an hour (ADR-019 §3b).
	assertionLifetime = 5 * time.Minute
	maxKeyPEM         = 16 << 10
	minRSABits        = 2048
)

var keyIDPattern = regexp.MustCompile(`^[\x21-\x7e]{1,256}$`)

// privateKeyJWTEntry is the "private_key_jwt" object of an oauth2 entry: the
// worker signs its own client assertion (OpenID Connect Core §9, RFC 7523).
type privateKeyJWTEntry struct {
	Alg             string  `json:"alg"`
	KeyFile         *string `json:"key_file"`
	Key             *string `json:"key"`
	CertificateFile *string `json:"certificate_file"`
	Certificate     *string `json:"certificate"`
	KeyID           *string `json:"key_id"`
	// The key and certificate may also come from Vault (ADR-019 §3c).
	KeyVault         *vaultRef `json:"key_vault"`
	CertificateVault *vaultRef `json:"certificate_vault"`
}

// assertionSigner signs private_key_jwt client assertions with the worker's
// key. The key is read once, at load.
type assertionSigner struct {
	alg      string
	key      crypto.Signer
	kid, x5t string
	pem      string // the key's PEM text, redacted permanently and never sent
	// The certificate's validity, checked at every mint (zero without one).
	notBefore, notAfter time.Time
}

// newAssertionSigner validates e. Its errors name the field at fault, never
// a value from it. A certificate outside its validity is accepted here and
// refused at each mint: the clock, not the file, is at fault.
func newAssertionSigner(e privateKeyJWTEntry) (*assertionSigner, error) {
	if err := checkAlgKeyID(e); err != nil {
		return nil, err
	}
	keyPEM, err := oneOf(e.Key, e.KeyFile, "key", true)
	if err != nil {
		return nil, err
	}
	block, err := singlePEM(keyPEM, "key")
	if err != nil {
		return nil, err
	}
	var parsed any
	switch block.Type {
	case "PRIVATE KEY":
		parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		parsed, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		parsed, err = x509.ParseECPrivateKey(block.Bytes)
	default:
		err = errors.New("unsupported")
	}
	if err != nil {
		return nil, errors.New("private_key_jwt key must be an unencrypted PKCS #8, PKCS #1 or SEC 1 private key")
	}
	s := &assertionSigner{alg: e.Alg, pem: keyPEM}
	if e.KeyID != nil {
		s.kid = *e.KeyID
	}
	switch k := parsed.(type) {
	case *rsa.PrivateKey:
		if e.Alg == "ES256" || k.N.BitLen() < minRSABits {
			return nil, errors.New("private_key_jwt RS256 and PS256 need an RSA key of at least 2048 bits")
		}
		s.key = k
	case *ecdsa.PrivateKey:
		if e.Alg != "ES256" || k.Curve != elliptic.P256() {
			return nil, errors.New("private_key_jwt ES256 needs an ECDSA P-256 key")
		}
		s.key = k
	default:
		return nil, errors.New("private_key_jwt key must be RSA or ECDSA P-256")
	}
	if e.Certificate == nil && e.CertificateFile == nil {
		return s, nil
	}
	certPEM, err := oneOf(e.Certificate, e.CertificateFile, "certificate", false)
	if err != nil {
		return nil, err
	}
	cblock, err := singlePEM(certPEM, "certificate")
	if err != nil || cblock.Type != "CERTIFICATE" {
		return nil, errors.New("private_key_jwt certificate must be one PEM CERTIFICATE block")
	}
	cert, err := x509.ParseCertificate(cblock.Bytes)
	if err != nil {
		return nil, errors.New("private_key_jwt certificate cannot be parsed")
	}
	if pub, ok := cert.PublicKey.(interface{ Equal(crypto.PublicKey) bool }); !ok || !pub.Equal(s.key.Public()) {
		return nil, errors.New("private_key_jwt certificate is not the key's")
	}
	s.notBefore, s.notAfter = cert.NotBefore, cert.NotAfter
	sum := sha256.Sum256(cert.Raw)
	s.x5t = base64.RawURLEncoding.EncodeToString(sum[:])
	return s, nil
}

// checkAlgKeyID checks the entry's alg and key_id.
func checkAlgKeyID(e privateKeyJWTEntry) error {
	if e.Alg != "RS256" && e.Alg != "PS256" && e.Alg != "ES256" {
		return errors.New("private_key_jwt alg must be RS256, PS256 or ES256")
	}
	if e.KeyID != nil && !keyIDPattern.MatchString(*e.KeyID) {
		return errors.New("private_key_jwt key_id must be 1-256 printable characters without spaces")
	}
	return nil
}

// redactPEM adds a PEM, and each base64 line of it, to set permanently, so a
// partial leak is redacted too.
func redactPEM(set *logging.SecretSet, text string) {
	if set == nil || text == "" {
		return
	}
	set.AddPermanent(text)
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if len(line) >= 16 && !strings.HasPrefix(line, "-----") {
			set.AddPermanent(line)
		}
	}
}

// vaultSigner is a private_key_jwt signer whose key or certificate comes
// from Vault: it is rebuilt, under the rules of newAssertionSigner, each
// time their text changes (ADR-019 §3c).
type vaultSigner struct {
	client          *vaultClient
	alg             string
	keyID           *string
	keyRef, certRef *vaultRef
	keyText         string  // the key when it is not in Vault
	certText        *string // the certificate when it is not in Vault
	redact          *logging.SecretSet

	signer *assertionSigner // the caller holds the provider's mint lock
	last   string
}

func countSet(ptrs ...bool) int {
	n := 0
	for _, p := range ptrs {
		if p {
			n++
		}
	}
	return n
}

func newVaultSigner(e privateKeyJWTEntry, c loadConfig) (*vaultSigner, error) {
	if err := checkAlgKeyID(e); err != nil {
		return nil, err
	}
	if countSet(e.Key != nil, e.KeyFile != nil, e.KeyVault != nil) != 1 {
		return nil, errors.New("private_key_jwt needs exactly one of key, key_file and key_vault")
	}
	if countSet(e.Certificate != nil, e.CertificateFile != nil, e.CertificateVault != nil) > 1 {
		return nil, errors.New("private_key_jwt takes at most one of certificate, certificate_file and certificate_vault")
	}
	if c.vault == nil {
		return nil, errors.New("private_key_jwt key_vault and certificate_vault need the file's vault object")
	}
	s := &vaultSigner{client: c.vault, alg: e.Alg, keyID: e.KeyID, redact: c.redact}
	if e.KeyVault != nil {
		r, err := e.KeyVault.validate(c.vault.kvMount)
		if err != nil {
			return nil, err
		}
		s.keyRef = &r
	} else {
		text, err := oneOf(e.Key, e.KeyFile, "key", true)
		if err != nil {
			return nil, err
		}
		if _, err := newAssertionSigner(privateKeyJWTEntry{Alg: e.Alg, Key: &text, KeyID: e.KeyID}); err != nil {
			return nil, err
		}
		s.keyText = text
		redactPEM(c.redact, text)
	}
	switch {
	case e.CertificateVault != nil:
		r, err := e.CertificateVault.validate(c.vault.kvMount)
		if err != nil {
			return nil, err
		}
		s.certRef = &r
	case e.Certificate != nil || e.CertificateFile != nil:
		text, err := oneOf(e.Certificate, e.CertificateFile, "certificate", false)
		if err != nil {
			return nil, err
		}
		s.certText = &text
	}
	return s, nil
}

// current returns the signer for the key and certificate Vault holds now,
// or a failure class.
func (s *vaultSigner) current(ctx context.Context) (*assertionSigner, string) {
	keyPEM := s.keyText
	if s.keyRef != nil {
		v, class := s.client.value(ctx, *s.keyRef)
		if class != "" {
			return nil, class
		}
		keyPEM = v.v
	}
	certPEM := s.certText
	if s.certRef != nil {
		v, class := s.client.value(ctx, *s.certRef)
		if class != "" {
			return nil, class
		}
		certPEM = &v.v
	}
	combined := keyPEM
	if certPEM != nil {
		combined += "\x00" + *certPEM
	}
	if s.signer != nil && combined == s.last {
		return s.signer, ""
	}
	signer, err := newAssertionSigner(privateKeyJWTEntry{Alg: s.alg, Key: &keyPEM, Certificate: certPEM, KeyID: s.keyID})
	if err != nil {
		return nil, "vault_invalid"
	}
	redactPEM(s.redact, keyPEM)
	s.signer, s.last = signer, combined
	return signer, ""
}

// drop forgets the cached Vault paths, so the next mint reads them again.
func (s *vaultSigner) drop() {
	for _, r := range []*vaultRef{s.keyRef, s.certRef} {
		if r != nil {
			s.client.drop(*r)
		}
	}
}

// oneOf returns the inline value or the file's content; exactly one is
// required when required, at most one otherwise.
func oneOf(inline, file *string, name string, required bool) (string, error) {
	switch {
	case inline != nil && file != nil:
		return "", errors.New("private_key_jwt takes at most one of " + name + " and " + name + "_file")
	case inline != nil:
		return *inline, nil
	case file != nil:
		b, err := os.ReadFile(*file)
		if err != nil {
			return "", errors.New("private_key_jwt " + name + "_file cannot be read")
		}
		return string(b), nil
	case required:
		return "", errors.New("private_key_jwt needs one of " + name + " and " + name + "_file")
	}
	return "", nil
}

// singlePEM decodes text as exactly one unencrypted PEM block.
func singlePEM(text, name string) (*pem.Block, error) {
	bad := errors.New("private_key_jwt " + name + " must be exactly one unencrypted PEM block of at most 16 KiB")
	if len(text) > maxKeyPEM {
		return nil, bad
	}
	block, rest := pem.Decode([]byte(strings.ReplaceAll(text, "\r\n", "\n")))
	if block == nil || len(bytes.TrimSpace(rest)) != 0 || len(block.Headers) != 0 {
		return nil, bad
	}
	return block, nil
}

// validAt reports whether the certificate, if any, is valid at now.
func (s *assertionSigner) validAt(now time.Time) bool {
	return s.notAfter.IsZero() || (!now.Before(s.notBefore) && !now.After(s.notAfter))
}

// sign returns a fresh client assertion for clientID at audience (the token
// URL) and its expiry: a new jti, valid for assertionLifetime.
func (s *assertionSigner) sign(clientID, audience string, now time.Time) (Secret, time.Time, error) {
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return Secret{}, time.Time{}, err
	}
	header := struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
		Kid string `json:"kid,omitempty"`
		X5t string `json:"x5t#S256,omitempty"`
	}{s.alg, "JWT", s.kid, s.x5t}
	exp := now.Add(assertionLifetime)
	claims := struct {
		Iss string `json:"iss"`
		Sub string `json:"sub"`
		Aud string `json:"aud"`
		Jti string `json:"jti"`
		Iat int64  `json:"iat"`
		Nbf int64  `json:"nbf"`
		Exp int64  `json:"exp"`
	}{clientID, clientID, audience, base64.RawURLEncoding.EncodeToString(jti), now.Unix(), now.Unix(), exp.Unix()}
	h, _ := json.Marshal(header)
	c, _ := json.Marshal(claims)
	input := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
	sum := sha256.Sum256([]byte(input))
	var sig []byte
	var err error
	switch k := s.key.(type) {
	case *rsa.PrivateKey:
		if s.alg == "PS256" {
			sig, err = rsa.SignPSS(rand.Reader, k, crypto.SHA256, sum[:], &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
		} else {
			sig, err = rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, sum[:])
		}
	case *ecdsa.PrivateKey:
		r, ss, e := ecdsa.Sign(rand.Reader, k, sum[:])
		if err = e; err == nil {
			sig = make([]byte, 64) // RFC 7518 §3.4: the raw R || S, 32 bytes each
			r.FillBytes(sig[:32])
			ss.FillBytes(sig[32:])
		}
	}
	if err != nil {
		return Secret{}, time.Time{}, err
	}
	return Secret{v: input + "." + base64.RawURLEncoding.EncodeToString(sig)}, time.Unix(exp.Unix(), 0), nil
}
