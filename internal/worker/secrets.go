package worker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// ErrNoCredential: the worker holds no credential for this tenant, secret
// reference and endpoint host. The action is never dispatched without one.
var ErrNoCredential = errors.New("worker: no credential for this connector endpoint")

const redacted = "[REDACTED]"

// Secret is a connector credential (ADR-001 §3). Its value never prints,
// logs or marshals; only Reveal returns it, for the connector call itself.
type Secret struct{ v string }

// Reveal returns the secret value. Call it only to authenticate a request.
func (s Secret) Reveal() string { return s.v }

func (Secret) String() string               { return redacted }
func (Secret) GoString() string             { return redacted }
func (Secret) LogValue() slog.Value         { return slog.StringValue(redacted) }
func (Secret) MarshalJSON() ([]byte, error) { return json.Marshal(redacted) }
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }
func (Secret) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(redacted)) }
func (s *SecretStore) Format(f fmt.State, _ rune) {
	fmt.Fprintf(f, "SecretStore(%d secrets)", len(s.m))
}

// Binding is one credential the worker holds: a tenant's secret reference,
// bound to the only endpoint host it may be sent to (ADR-003 §4).
type Binding struct {
	TenantID uuid.UUID `json:"tenant_id"`
	Ref      string    `json:"secret_ref"`
	Host     string    `json:"host"`
}

type secretKey struct {
	tenant uuid.UUID
	ref    string
}

type secretEntry struct {
	host   string
	secret Secret
}

// SecretStore holds the worker's connector credentials, keyed by tenant and
// secret reference so a tenant can never name another tenant's secret.
type SecretStore struct {
	m map[secretKey]secretEntry
}

var (
	refPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,127}$`) // as eacp.connectors.secret_ref
	hostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,252})(:[0-9]{1,5})?$`)
)

const maxSecret = 4096

// LoadSecrets reads the secrets file at path:
//
//	{"secrets": [{"tenant_id": "...", "secret_ref": "erp", "host": "erp.internal:8443",
//	              "value": "..." | "value_file": "/run/secrets/erp"}]}
//
// Any invalid entry rejects the whole file (fail closed). Errors never
// include secret values.
func LoadSecrets(path string) (*SecretStore, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("worker: read secrets file: %w", err)
	}
	var file struct {
		Secrets []struct {
			TenantID  string  `json:"tenant_id"`
			Ref       string  `json:"secret_ref"`
			Host      string  `json:"host"`
			Value     *string `json:"value"`
			ValueFile *string `json:"value_file"`
		} `json:"secrets"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		return nil, errors.New("worker: secrets file is not valid JSON of the expected shape")
	}
	if len(file.Secrets) == 0 {
		return nil, errors.New("worker: secrets file lists no secrets")
	}
	store := &SecretStore{m: map[secretKey]secretEntry{}}
	for i, e := range file.Secrets {
		tenant, err := uuid.Parse(e.TenantID)
		if err != nil || tenant == uuid.Nil {
			return nil, fmt.Errorf("worker: secret %d: tenant_id must be a UUID", i)
		}
		if !refPattern.MatchString(e.Ref) {
			return nil, fmt.Errorf("worker: secret %d: invalid secret_ref", i)
		}
		if !hostPattern.MatchString(e.Host) {
			return nil, fmt.Errorf("worker: secret %d: host must be a lowercase host[:port]", i)
		}
		if (e.Value == nil) == (e.ValueFile == nil) {
			return nil, fmt.Errorf("worker: secret %d: exactly one of value and value_file is required", i)
		}
		var value string
		if e.Value != nil {
			value = *e.Value
		} else {
			b, err := os.ReadFile(*e.ValueFile)
			if err != nil {
				return nil, fmt.Errorf("worker: secret %d: value_file cannot be read", i)
			}
			value = strings.TrimRight(string(b), "\r\n")
		}
		if value == "" || len(value) > maxSecret {
			return nil, fmt.Errorf("worker: secret %d: value must be 1-%d bytes", i, maxSecret)
		}
		k := secretKey{tenant, e.Ref}
		if _, dup := store.m[k]; dup {
			return nil, fmt.Errorf("worker: secret %d: duplicate tenant and secret_ref", i)
		}
		store.m[k] = secretEntry{host: e.Host, secret: Secret{value}}
	}
	return store, nil
}

// endpointHost returns the lowercase host[:port] of an http(s) endpoint, or
// "" when it has none or carries user information.
func endpointHost(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Host)
}

// Resolve returns the credential of tenant's secret reference for endpoint.
// It fails with ErrNoCredential unless the secret exists and is bound to the
// endpoint's exact host and port.
func (s *SecretStore) Resolve(tenant uuid.UUID, ref, endpoint string) (Secret, error) {
	if s == nil {
		return Secret{}, ErrNoCredential
	}
	e, ok := s.m[secretKey{tenant, ref}]
	if !ok || e.host != endpointHost(endpoint) {
		return Secret{}, ErrNoCredential
	}
	return e.secret, nil
}

// Bindings lists the held credentials without values, ordered by tenant,
// then reference.
func (s *SecretStore) Bindings() []Binding {
	if s == nil {
		return nil
	}
	out := make([]Binding, 0, len(s.m))
	for k, e := range s.m {
		out = append(out, Binding{TenantID: k.tenant, Ref: k.ref, Host: e.host})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TenantID != out[j].TenantID {
			return out[i].TenantID.String() < out[j].TenantID.String()
		}
		return out[i].Ref < out[j].Ref
	})
	return out
}

// Values returns every secret value, for registration with the log
// redactor (internal/logging) only.
func (s *SecretStore) Values() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.m))
	for _, e := range s.m {
		out = append(out, e.secret.v)
	}
	return out
}
