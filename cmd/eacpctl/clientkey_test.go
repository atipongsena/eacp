package main

import (
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var devEnv = map[string]string{"EACP_ENV": "development"}

func TestDevClientKeyOnlyInDevelopment(t *testing.T) {
	dir := t.TempDir()
	for _, envName := range []string{"", "staging", "production"} {
		if _, err := runWith(t, map[string]string{"EACP_ENV": envName}, "dev-client-key", "--dir", dir,
			"--jwks", filepath.Join(dir, "jwks.json")); err == nil || !strings.Contains(err.Error(), "refused") {
			t.Errorf("EACP_ENV=%q: err = %v, want refusal", envName, err)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("a refused run wrote %d files", len(entries))
	}
	for name, args := range map[string][]string{
		"no --dir":   {"--jwks", filepath.Join(dir, "jwks.json")},
		"no --jwks":  {"--dir", dir},
		"extra args": {"--dir", dir, "--jwks", filepath.Join(dir, "jwks.json"), "x"},
		"empty kid":  {"--dir", dir, "--jwks", filepath.Join(dir, "jwks.json"), "--kid", ""},
	} {
		if _, err := runWith(t, devEnv, append([]string{"dev-client-key"}, args...)...); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDevClientKeyWritesAMatchingSet(t *testing.T) {
	dir := t.TempDir()
	keys, jwks := filepath.Join(dir, "key"), filepath.Join(dir, "public", "jwks.json")
	out, err := runWith(t, devEnv, "dev-client-key", "--dir", keys, "--jwks", jwks, "--kid", "k1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "development") {
		t.Errorf("output = %q", out)
	}
	keyPEM, _ := os.ReadFile(filepath.Join(keys, "key.pem"))
	certPEM, _ := os.ReadFile(filepath.Join(keys, "cert.pem"))
	kb, _ := pem.Decode(keyPEM)
	cb, _ := pem.Decode(certPEM)
	if kb == nil || kb.Type != "PRIVATE KEY" || cb == nil || cb.Type != "CERTIFICATE" {
		t.Fatalf("key %v, certificate %v", kb, cb)
	}
	priv, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	rsaPriv, ok := priv.(*rsa.PrivateKey)
	if !ok || rsaPriv.N.BitLen() != 2048 {
		t.Fatalf("key = %T", priv)
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pub, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok || !pub.Equal(&rsaPriv.PublicKey) || cert.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		t.Fatal("the certificate does not hold the key's public half for signing")
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(filepath.Join(keys, "key.pem")); st.Mode().Perm() != 0o600 {
			t.Fatalf("key.pem mode = %v", st.Mode().Perm())
		}
	}
	raw, _ := os.ReadFile(jwks)
	if strings.Contains(string(raw), `"d"`) || strings.Contains(string(raw), "PRIVATE") {
		t.Fatal("the JWKS holds private material")
	}
	var set struct {
		Keys []map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(raw, &set); err != nil || len(set.Keys) != 1 {
		t.Fatalf("jwks = %s (%v)", raw, err)
	}
	k := set.Keys[0]
	n, _ := base64.RawURLEncoding.DecodeString(k["n"])
	e, _ := base64.RawURLEncoding.DecodeString(k["e"])
	sum := sha256.Sum256(cert.Raw)
	if k["kty"] != "RSA" || k["kid"] != "k1" || k["use"] != "sig" || k["alg"] != "PS256" ||
		new(big.Int).SetBytes(n).Cmp(pub.N) != 0 || int(new(big.Int).SetBytes(e).Int64()) != pub.E ||
		k["x5t#S256"] != base64.RawURLEncoding.EncodeToString(sum[:]) {
		t.Fatalf("jwks key = %v", k)
	}
	// A second run keeps the complete set.
	if _, err := runWith(t, devEnv, "dev-client-key", "--dir", keys, "--jwks", jwks, "--kid", "k1"); err != nil {
		t.Fatal(err)
	}
	for path, before := range map[string][]byte{filepath.Join(keys, "key.pem"): keyPEM,
		filepath.Join(keys, "cert.pem"): certPEM, jwks: raw} {
		if after, _ := os.ReadFile(path); string(after) != string(before) {
			t.Errorf("%s changed on the second run", path)
		}
	}
}

func TestDevClientKeyRefusesAPartialSet(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runWith(t, devEnv, "dev-client-key", "--dir", dir, "--jwks", filepath.Join(dir, "jwks.json")); err == nil ||
		!strings.Contains(err.Error(), "partial") {
		t.Fatalf("err = %v, want a partial-set refusal", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "key.pem")); string(got) != "x" {
		t.Fatal("a partial set was overwritten")
	}
}
