package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"eacp/internal/logging"
)

// ErrNoCredential: the worker holds no credential for this tenant, secret
// reference and endpoint host. The action is never dispatched without one.
var ErrNoCredential = errors.New("worker: no credential for this connector endpoint")

// ErrCredentialUnavailable: the binding exists but its provider cannot
// produce a credential that outlives the call now (a failed or backing-off
// token mint). Nothing is dispatched; the work waits (ADR-019).
var ErrCredentialUnavailable = errors.New("worker: credential unavailable")

// ErrCredentialTooShort (an ErrCredentialUnavailable): the provider works,
// but its credentials live shorter than this call needs. Shorter calls on
// the same binding are still served.
var ErrCredentialTooShort = fmt.Errorf("%w: its lifetime is shorter than the call", ErrCredentialUnavailable)

// CredentialSkew is added to every call budget when asking for a
// credential, so a token never expires during a call.
const CredentialSkew = 30 * time.Second

// LoadOption configures LoadSecrets.
type LoadOption func(*loadConfig)

type loadConfig struct {
	allowPlain bool
	noSigning  bool
	redact     *logging.SecretSet
	now        func() time.Time
	log        *slog.Logger
	vault      *vaultClient  // the file's Vault client, while loading
	spiffe     *spiffeClient // the file's Workload API client, while loading
}

// AllowPlainTokenURL lets an oauth2 token_url use http. The worker passes it
// only in development and test.
func AllowPlainTokenURL() LoadOption { return func(c *loadConfig) { c.allowPlain = true } }

// RefuseSigningCredentials rejects a file with an aws entry: a service that
// only sends Bearer or API-key credentials (the LLM gateway, ADR-031) cannot
// sign requests.
func RefuseSigningCredentials() LoadOption { return func(c *loadConfig) { c.noSigning = true } }

// WithRedaction adds every client secret and minted token to set.
func WithRedaction(set *logging.SecretSet) LoadOption { return func(c *loadConfig) { c.redact = set } }

// WithClock replaces the clock providers use for expiry and back-off.
func WithClock(now func() time.Time) LoadOption { return func(c *loadConfig) { c.now = now } }

// WithLogger sets the logger that records mints (never their values).
func WithLogger(log *slog.Logger) LoadOption { return func(c *loadConfig) { c.log = log } }

const redacted = "[REDACTED]"

// Secret is a connector credential (ADR-001 §3). Its value never prints,
// logs or marshals; only Reveal returns it, for the connector call itself.
type Secret struct {
	v   string
	aws *awsKeys // temporary AWS keys (ADR-019 §3f): v is the secret access key
}

// SignsRequests reports an AWS credential, which signs a request (SigV4)
// rather than riding on it as a Bearer.
func (s Secret) SignsRequests() bool { return s.aws != nil }

// values returns every secret part of s: what must be redacted and scrubbed.
func (s Secret) values() []string {
	switch {
	case s.aws != nil:
		return []string{s.v, s.aws.sessionToken}
	case s.v != "":
		return []string{s.v}
	}
	return nil
}

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
	secret Secret         // a static credential, or
	oauth  *oauthProvider // tokens minted just in time, or
	vault  *vaultValue    // a static credential read from Vault, or
	spiffe *spiffeValue   // a JWT-SVID for the binding's audience
}

// SecretStore holds the worker's connector credentials, keyed by tenant and
// secret reference so a tenant can never name another tenant's secret.
type SecretStore struct {
	m   map[secretKey]secretEntry
	now func() time.Time
}

var (
	refPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,127}$`) // as eacp.connectors.secret_ref
	hostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,252})(:[0-9]{1,5})?$`)
)

const maxSecret = 4096

// LoadSecrets reads the secrets file at path:
//
//	{"vault": {"address": "https://vault:8200", "auth": {"kubernetes" | "approle": {...}}, ...},  (optional)
//	 "spiffe": {"endpoint": "unix:///spiffe-workload-api/spire-agent.sock", "spiffe_id": "spiffe://…"},  (optional)
//	 "secrets": [{"tenant_id": "...", "secret_ref": "erp", "host": "erp.internal:8443",
//	              "value": "..." | "value_file": "/run/secrets/erp" |
//	              "value_vault": {"path": "eacp/erp", "key": "token"} |
//	              "value_spiffe": {"audience": "erp-api"} |
//	              "oauth2": {"token_url": "https://idp/token", "client_id": "...",
//	                         "client_secret": "..." | "client_secret_file": "...",
//	                         "scope": "...", "resource": "https://..."}}]}
//
// Any invalid entry rejects the whole file (fail closed). Errors never
// include secret values.
func LoadSecrets(path string, opts ...LoadOption) (*SecretStore, error) {
	c := loadConfig{now: time.Now, log: slog.New(slog.DiscardHandler)}
	for _, o := range opts {
		o(&c)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("worker: read secrets file: %w", err)
	}
	var file struct {
		Vault   *vaultEntry  `json:"vault"`
		Spiffe  *spiffeEntry `json:"spiffe"`
		Secrets []struct {
			TenantID    string          `json:"tenant_id"`
			Ref         string          `json:"secret_ref"`
			Host        string          `json:"host"`
			Value       *string         `json:"value"`
			ValueFile   *string         `json:"value_file"`
			ValueVault  *vaultRef       `json:"value_vault"`
			ValueSPIFFE *spiffeAudience `json:"value_spiffe"`
			OAuth2      *oauthEntry     `json:"oauth2"`
			AWS         *awsEntry       `json:"aws"`
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
	if file.Vault != nil {
		v, err := newVaultClient(*file.Vault, c)
		if err != nil {
			return nil, fmt.Errorf("worker: %w", err)
		}
		c.vault = v
	}
	if file.Spiffe != nil {
		sc, err := newSpiffeClient(*file.Spiffe, c)
		if err != nil {
			return nil, fmt.Errorf("worker: %w", err)
		}
		c.spiffe = sc
	}
	store := &SecretStore{m: map[secretKey]secretEntry{}, now: c.now}
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
		k := secretKey{tenant, e.Ref}
		if _, dup := store.m[k]; dup {
			return nil, fmt.Errorf("worker: secret %d: duplicate tenant and secret_ref", i)
		}
		kinds := 0
		for _, set := range []bool{e.Value != nil, e.ValueFile != nil, e.ValueVault != nil, e.ValueSPIFFE != nil,
			e.OAuth2 != nil, e.AWS != nil} {
			if set {
				kinds++
			}
		}
		if kinds != 1 {
			return nil, fmt.Errorf("worker: secret %d: exactly one of value, value_file, value_vault, value_spiffe, oauth2 and aws is required", i)
		}
		if e.ValueSPIFFE != nil {
			if c.spiffe == nil {
				return nil, fmt.Errorf("worker: secret %d: value_spiffe needs the file's spiffe object", i)
			}
			if err := e.ValueSPIFFE.validate(); err != nil {
				return nil, fmt.Errorf("worker: secret %d: %w", i, err)
			}
			store.m[k] = secretEntry{host: e.Host, spiffe: &spiffeValue{client: c.spiffe, audience: e.ValueSPIFFE.Audience,
				binding: Binding{TenantID: tenant, Ref: e.Ref, Host: e.Host}, now: c.now, log: c.log}}
			continue
		}
		if e.ValueVault != nil {
			if c.vault == nil {
				return nil, fmt.Errorf("worker: secret %d: value_vault needs the file's vault object", i)
			}
			ref, err := e.ValueVault.validate(c.vault.kvMount)
			if err != nil {
				return nil, fmt.Errorf("worker: secret %d: %w", i, err)
			}
			store.m[k] = secretEntry{host: e.Host, vault: &vaultValue{client: c.vault, ref: ref,
				binding: Binding{TenantID: tenant, Ref: e.Ref, Host: e.Host}, now: c.now, log: c.log}}
			continue
		}
		if e.AWS != nil {
			if c.noSigning {
				return nil, fmt.Errorf("worker: secret %d: aws credentials sign requests and are not accepted here", i)
			}
			p, err := newAWSProvider(i, *e.AWS, Binding{TenantID: tenant, Ref: e.Ref, Host: e.Host}, c)
			if err != nil {
				return nil, err
			}
			store.m[k] = secretEntry{host: e.Host, oauth: p}
			continue
		}
		if e.OAuth2 != nil {
			p, err := newOAuthProvider(i, *e.OAuth2, Binding{TenantID: tenant, Ref: e.Ref, Host: e.Host}, c)
			if err != nil {
				return nil, err
			}
			if c.redact != nil {
				c.redact.AddPermanent(p.clientSecret.v)
				if p.signer != nil {
					redactPEM(c.redact, p.signer.pem)
				}
			}
			store.m[k] = secretEntry{host: e.Host, oauth: p}
			continue
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
		store.m[k] = secretEntry{host: e.Host, secret: Secret{v: value}}
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

// Credential returns the credential of tenant's secret reference for
// endpoint, valid for at least validFor (ADR-019). It fails with
// ErrNoCredential unless the secret exists and is bound to the endpoint's
// exact host and port, and with ErrCredentialUnavailable when its provider
// cannot produce one now.
func (s *SecretStore) Credential(ctx context.Context, tenant uuid.UUID, ref, endpoint string,
	validFor time.Duration) (Secret, error) {
	if s == nil {
		return Secret{}, ErrNoCredential
	}
	e, ok := s.m[secretKey{tenant, ref}]
	if !ok || e.host != endpointHost(endpoint) {
		return Secret{}, ErrNoCredential
	}
	if e.oauth != nil {
		return e.oauth.credential(ctx, validFor)
	}
	if e.vault != nil {
		return e.vault.credential(ctx)
	}
	if e.spiffe != nil {
		return e.spiffe.credential(ctx, validFor)
	}
	return e.secret, nil
}

// Resolve is Credential for a credential that only needs to be valid now.
func (s *SecretStore) Resolve(tenant uuid.UUID, ref, endpoint string) (Secret, error) {
	return s.Credential(context.Background(), tenant, ref, endpoint, 0)
}

// Available lists the bindings the worker can serve now, as Bindings does:
// every static one and each OAuth binding outside a mint back-off. Claims
// use it so a failing token endpoint withholds work instead of churning it.
func (s *SecretStore) Available() []Binding {
	var out []Binding
	for _, b := range s.Bindings() {
		e := s.m[secretKey{b.TenantID, b.Ref}]
		switch {
		case e.oauth != nil && !e.oauth.available(s.now()):
		case e.vault != nil && !e.vault.available(s.now()):
		case e.spiffe != nil && !e.spiffe.available(s.now()):
		default:
			out = append(out, b)
		}
	}
	return out
}

// Rejected tells the provider the target refused secret (error class
// "unauthorized"); an OAuth provider drops it so the next attempt mints a
// new one. The attempt's outcome is unchanged.
func (s *SecretStore) Rejected(tenant uuid.UUID, ref string, secret Secret) {
	if s == nil {
		return
	}
	e, ok := s.m[secretKey{tenant, ref}]
	switch {
	case ok && e.oauth != nil:
		e.oauth.rejected(secret)
	case ok && e.vault != nil:
		e.vault.rejected(secret)
	case ok && e.spiffe != nil:
		e.spiffe.rejected(secret)
	}
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

// Values returns every live credential value (static secrets, client
// secrets and unexpired minted tokens), for the log redactor and for
// scrubbing connector results only.
func (s *SecretStore) Values() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.m))
	for _, e := range s.m {
		switch {
		case e.oauth != nil:
			out = append(out, e.oauth.live()...)
		case e.vault != nil:
			out = append(out, e.vault.live(s.now())...)
		case e.spiffe != nil:
			out = append(out, e.spiffe.live(s.now())...)
		default:
			out = append(out, e.secret.v)
		}
	}
	return out
}
