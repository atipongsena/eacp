package microsoftagt_test

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"eacp/integrations/governance/microsoftagt"
)

func readCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		t.Fatalf("%s: no PEM", path)
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestWriteDevPKIIssuesServerAndClientCertificatesOnce(t *testing.T) {
	dir := t.TempDir()
	if err := microsoftagt.WriteDevPKI(dir, []string{"agt-pdp", "127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	ca := readCert(t, filepath.Join(dir, "ca.pem"))
	server := readCert(t, filepath.Join(dir, "server.pem"))
	client := readCert(t, filepath.Join(dir, "client.pem"))
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if _, err := server.Verify(x509.VerifyOptions{Roots: roots, DNSName: "agt-pdp",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatalf("server certificate: %v", err)
	}
	if _, err := server.Verify(x509.VerifyOptions{Roots: roots, DNSName: "127.0.0.1"}); err != nil {
		t.Fatalf("server certificate IP SAN: %v", err)
	}
	if _, err := client.Verify(x509.VerifyOptions{Roots: roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("client certificate: %v", err)
	}
	if _, err := client.Verify(x509.VerifyOptions{Roots: roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err == nil {
		t.Fatal("the client certificate can act as a server")
	}
	if _, err := os.Stat(filepath.Join(dir, "ca-key.pem")); !os.IsNotExist(err) {
		t.Fatalf("the CA key was kept: %v", err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if err := microsoftagt.WriteDevPKI(dir, []string{"agt-pdp"}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if !bytes.Equal(before, after) {
		t.Fatal("a second run replaced the existing PKI")
	}
}
