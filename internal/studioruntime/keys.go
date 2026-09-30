// Package studioruntime is agent-runtime (ADR-033, Phase 27a-2): it runs
// approved Agent Studio agents. It claims runs through the API, acts as each
// run's agent version with a key derived from its master secret, and sends
// every tool call through the ordinary action path. It holds no database
// connection, no connector secret and no provider key, and it never approves
// anything: a registry_approver approves every key it proposes.
package studioruntime

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/identity"
)

// masterBytes is the shortest master secret the runtime accepts.
const masterBytes = 32

var masterVersion = regexp.MustCompile(`^v[0-9]{1,4}$`)

// Master is the Studio master secret and its version. Keys derived from it
// are never stored: only their hashes reach EACP.
type Master struct {
	version string
	secret  []byte
}

// LoadMaster reads the master secret at path (surrounding whitespace is
// trimmed) under version. It refuses a secret shorter than 32 bytes.
func LoadMaster(path, version string) (*Master, error) {
	if !masterVersion.MatchString(version) {
		return nil, errors.New("studioruntime: a master version is v followed by 1 to 4 digits")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("studioruntime: read the master secret: %w", err)
	}
	secret := bytes.TrimSpace(raw)
	if len(secret) < masterBytes {
		return nil, fmt.Errorf("studioruntime: the master secret must be at least %d bytes", masterBytes)
	}
	return &Master{version: version, secret: secret}, nil
}

// Version is the master's version, recorded beside each derived key.
func (m *Master) Version() string { return m.version }

// Redactions returns the forms of the master a log line could carry.
func (m *Master) Redactions() []string { return []string{string(m.secret)} }

// Key derives credential's agent key in tenant (ADR-033 §2) and returns it
// with the hash to store:
//
//	secret = HMAC-SHA-256(master, "eacp-studio-ak-" + version + tenant + credential)
func (m *Master) Key(tenant, credential uuid.UUID) (string, []byte) {
	mac := hmac.New(sha256.New, m.secret)
	mac.Write([]byte("eacp-studio-ak-" + m.version + tenant.String() + credential.String()))
	key, hash, err := identity.KeyFromSecret(identity.KindAgent, tenant, credential, mac.Sum(nil))
	if err != nil {
		// Only nil ids fail, and PostgreSQL never names one.
		panic(fmt.Sprintf("studioruntime: derive a key: %v", err))
	}
	return key, hash
}

// LoadKeys reads the runtime's own principal keys, one per line and one per
// tenant (blank lines and lines starting with # are skipped).
func LoadKeys(path string) (map[uuid.UUID]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("studioruntime: read the key file: %w", err)
	}
	defer f.Close()
	keys := map[uuid.UUID]string{}
	s := bufio.NewScanner(f)
	for n := 1; s.Scan(); n++ {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p, err := identity.ParseKey(line)
		if err != nil || p.Kind != identity.KindPrincipal {
			return nil, fmt.Errorf("studioruntime: key file line %d is not a principal key", n)
		}
		if _, dup := keys[p.TenantID]; dup {
			return nil, fmt.Errorf("studioruntime: key file line %d: a second key for tenant %s", n, p.TenantID)
		}
		keys[p.TenantID] = line
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("studioruntime: read the key file: %w", err)
	}
	if len(keys) == 0 {
		return nil, errors.New("studioruntime: the key file holds no key")
	}
	return keys, nil
}
