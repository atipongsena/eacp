// Package identity authenticates callers of EACP: agent runtimes (bound to one
// AgentVersion) and principals (humans or services with roles). ADR-003 §5.
package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Kind distinguishes agent keys from principal keys.
type Kind string

const (
	KindAgent     Kind = "ak"
	KindPrincipal Kind = "pk"
)

const secretBytes = 32

var (
	secretEncoding = base64.RawURLEncoding.Strict()
	// ErrMalformedKey is returned for any key that does not have the exact
	// canonical format. Callers must not reveal which part was wrong.
	ErrMalformedKey = errors.New("identity: malformed api key")
)

// ParsedKey is a syntactically valid key. The secret is kept unexported and
// is never formatted.
type ParsedKey struct {
	Kind         Kind
	TenantID     uuid.UUID
	CredentialID uuid.UUID
	secret       []byte
}

// NewKey generates a key for a credential and returns the key (shown to the
// caller once) and the hash to store. Only SHA-256(secret) is stored: the
// secret is 256 random bits, so a slow KDF adds nothing.
func NewKey(kind Kind, tenantID, credentialID uuid.UUID) (key string, hash []byte, err error) {
	if kind != KindAgent && kind != KindPrincipal {
		return "", nil, fmt.Errorf("identity: unknown key kind %q", kind)
	}
	if tenantID == uuid.Nil || credentialID == uuid.Nil {
		return "", nil, errors.New("identity: nil tenant or credential id")
	}
	secret := make([]byte, secretBytes)
	if _, err := rand.Read(secret); err != nil {
		return "", nil, fmt.Errorf("identity: generate secret: %w", err)
	}
	key = strings.Join([]string{
		"eacp", string(kind), hexID(tenantID), hexID(credentialID), secretEncoding.EncodeToString(secret),
	}, "_")
	sum := sha256.Sum256(secret)
	return key, sum[:], nil
}

// ParseKey validates the canonical key format:
//
//	eacp_<ak|pk>_<tenant uuid, 32 lowercase hex>_<credential uuid, 32 lowercase hex>_<base64url secret>
//
// The tenant id lets the server set the Row-Level Security context before
// looking the credential up.
func ParseKey(key string) (ParsedKey, error) {
	// The secret alphabet contains '_', so split into at most five fields.
	parts := strings.SplitN(key, "_", 5)
	if len(parts) != 5 || parts[0] != "eacp" {
		return ParsedKey{}, ErrMalformedKey
	}
	kind := Kind(parts[1])
	if kind != KindAgent && kind != KindPrincipal {
		return ParsedKey{}, ErrMalformedKey
	}
	tenant, ok := parseHexID(parts[2])
	if !ok {
		return ParsedKey{}, ErrMalformedKey
	}
	cred, ok := parseHexID(parts[3])
	if !ok {
		return ParsedKey{}, ErrMalformedKey
	}
	secret, err := secretEncoding.DecodeString(parts[4])
	if err != nil || len(secret) != secretBytes {
		return ParsedKey{}, ErrMalformedKey
	}
	return ParsedKey{Kind: kind, TenantID: tenant, CredentialID: cred, secret: secret}, nil
}

// Matches reports, in constant time, whether the key's secret hashes to the
// stored hash.
func (p ParsedKey) Matches(stored []byte) bool {
	sum := sha256.Sum256(p.secret)
	return len(stored) == len(sum) && subtle.ConstantTimeCompare(sum[:], stored) == 1
}

// String identifies the key without its secret.
func (p ParsedKey) String() string {
	return fmt.Sprintf("eacp_%s_%s_%s_[REDACTED]", p.Kind, hexID(p.TenantID), hexID(p.CredentialID))
}

// GoString keeps %#v from printing the secret bytes.
func (p ParsedKey) GoString() string { return p.String() }

func hexID(id uuid.UUID) string { return hex.EncodeToString(id[:]) }

func parseHexID(s string) (uuid.UUID, bool) {
	if len(s) != 32 || strings.ToLower(s) != s {
		return uuid.Nil, false
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return uuid.Nil, false
	}
	id, err := uuid.FromBytes(b)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, false
	}
	return id, true
}
