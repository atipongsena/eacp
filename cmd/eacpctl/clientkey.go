package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// runDevClientKey writes a development-only private_key_jwt client key
// (ADR-019 Rev 1.2): <dir>/key.pem (PKCS #8, 0600), a self-signed
// <dir>/cert.pem and the public JWKS the relying party registers. It keeps an
// existing complete set and refuses a partial one.
func runDevClientKey(args []string, getenv func(string) string, out io.Writer) error {
	if env := getenv("EACP_ENV"); env != "development" && env != "test" {
		return errors.New("dev-client-key refused: requires EACP_ENV=development or test; production keys come from the enterprise's own PKI")
	}
	fs := flag.NewFlagSet("dev-client-key", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("dir", "", "directory for key.pem and cert.pem (the worker's only)")
	jwks := fs.String("jwks", "", "file for the public JWKS (the relying party's)")
	kid := fs.String("kid", "eacp-dev-pkjwt", "the key id in the JWKS")
	if err := fs.Parse(args); err != nil || *dir == "" || *jwks == "" || *kid == "" || fs.NArg() != 0 {
		return errors.New("usage: eacpctl dev-client-key --dir <dir> --jwks <file> [--kid <kid>]")
	}
	keyFile, certFile := filepath.Join(*dir, "key.pem"), filepath.Join(*dir, "cert.pem")
	present := 0
	for _, f := range []string{keyFile, certFile, *jwks} {
		if _, err := os.Stat(f); err == nil {
			present++
		}
	}
	switch present {
	case 3:
		fmt.Fprintf(out, "development client key already in %s\n", *dir)
		return nil
	case 0:
	default:
		return fmt.Errorf("dev-client-key: a partial key set exists (%s, %s, %s); remove it and retry", keyFile, certFile, *jwks)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return err
	}
	now := time.Now()
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "eacp-dev-pkjwt"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(1, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(der)
	b64 := base64.RawURLEncoding.EncodeToString
	set, err := json.MarshalIndent(map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "kid": *kid, "use": "sig", "alg": "PS256",
		"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()), "x5t#S256": b64(sum[:]),
	}}}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*jwks), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(*jwks, append(set, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "development client key ready in %s; JWKS in %s\n", *dir, *jwks)
	return nil
}
