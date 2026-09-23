package microsoftagt

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// DevPKIFiles are the files WriteDevPKI creates. ca.pem is written last, so
// its presence marks a complete PKI.
var DevPKIFiles = []string{"server.pem", "server-key.pem", "client.pem", "client-key.pem", "ca.pem"}

// WriteDevPKI creates a DEVELOPMENT-ONLY mutual-TLS PKI for the AGT sidecar
// in dir: a throwaway CA (its key is discarded), a server certificate for
// names (DNS names or IP addresses) and a client certificate. It keeps an
// existing complete PKI and refuses a partial one. Production deployments
// issue these certificates from their own CA.
func WriteDevPKI(dir string, names []string) error {
	present := 0
	for _, f := range DevPKIFiles {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			present++
		}
	}
	switch {
	case present == len(DevPKIFiles):
		return nil
	case present != 0:
		return fmt.Errorf("microsoftagt: %s holds an incomplete PKI; remove it and retry", dir)
	case len(names) == 0:
		return errors.New("microsoftagt: the server certificate needs at least one name")
	}
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	caTmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "EACP development PDP CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(1, 0, 0),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true, MaxPathLenZero: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return err
	}
	issue := func(cn string, usage x509.ExtKeyUsage, names []string) (cert, key []byte, err error) {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, nil, err
		}
		tmpl := &x509.Certificate{
			SerialNumber: serial(), Subject: pkix.Name{CommonName: cn},
			NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(1, 0, 0),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
		}
		for _, n := range names {
			if ip := net.ParseIP(n); ip != nil {
				tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
			} else {
				tmpl.DNSNames = append(tmpl.DNSNames, n)
			}
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &k.PublicKey, caKey)
		if err != nil {
			return nil, nil, err
		}
		kd, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			return nil, nil, err
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
			pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}), nil
	}
	serverCert, serverKey, err := issue("agt-pdp", x509.ExtKeyUsageServerAuth, names)
	if err != nil {
		return err
	}
	clientCert, clientKey, err := issue("controlplane-api", x509.ExtKeyUsageClientAuth, nil)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, f := range []struct {
		name string
		data []byte
		mode os.FileMode
	}{
		{"server.pem", serverCert, 0o644}, {"server-key.pem", serverKey, 0o600},
		{"client.pem", clientCert, 0o644}, {"client-key.pem", clientKey, 0o600},
		{"ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o644},
	} {
		if err := writeFile(filepath.Join(dir, f.name), f.data, f.mode); err != nil {
			return err
		}
	}
	return nil
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		panic(err)
	}
	return n
}

// writeFile writes through a temporary file and a rename, so a crash never
// leaves a truncated certificate or key behind.
func writeFile(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".pki-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
