package worker

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/atipongsena/eacp/internal/logging"
)

// The Vault client (ADR-019 §3c): the worker logs in to Vault with its own
// identity and reads credentials from KV v2 when it needs them.
const (
	vaultTimeout        = 10 * time.Second
	maxVaultResponse    = 64 << 10
	maxVaultToken       = 1024
	maxVaultTokenUse    = time.Hour // a Vault token is used for two thirds of min(lease, this)
	defaultVaultRefresh = 300
)

var (
	vaultSegment     = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	vaultKeyPattern  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	vaultNamePattern = regexp.MustCompile(`^[\x21-\x7e]{1,256}$`)
)

// vaultEntry is the secrets file's top-level "vault" object.
type vaultEntry struct {
	Address        string         `json:"address"`
	Namespace      string         `json:"namespace"`
	CAFile         string         `json:"ca_file"`
	KVMount        string         `json:"kv_mount"`
	RefreshSeconds int            `json:"refresh_seconds"`
	Auth           vaultAuthEntry `json:"auth"`
}

type vaultAuthEntry struct {
	Kubernetes *vaultKubernetesAuth `json:"kubernetes"`
	AppRole    *vaultAppRoleAuth    `json:"approle"`
}

type vaultKubernetesAuth struct {
	Mount   string `json:"mount"`
	Role    string `json:"role"`
	JWTFile string `json:"jwt_file"`
}

type vaultAppRoleAuth struct {
	Mount        string  `json:"mount"`
	RoleID       *string `json:"role_id"`
	RoleIDFile   *string `json:"role_id_file"`
	SecretID     *string `json:"secret_id"`
	SecretIDFile *string `json:"secret_id_file"`
}

// vaultRef names one value in Vault KV v2: a key of the secret at path.
type vaultRef struct {
	Mount string `json:"mount"`
	Path  string `json:"path"`
	Key   string `json:"key"`
}

func validSegment(s string) bool { return vaultSegment.MatchString(s) && s != "." && s != ".." }

// validate fills the default mount and checks every part.
func (r vaultRef) validate(defaultMount string) (vaultRef, error) {
	if r.Mount == "" {
		r.Mount = defaultMount
	}
	if !validSegment(r.Mount) {
		return r, errors.New("vault mount must be one path segment of [A-Za-z0-9._-]")
	}
	if len(r.Path) == 0 || len(r.Path) > 256 {
		return r, errors.New("vault path must be 1-256 characters")
	}
	for _, seg := range strings.Split(r.Path, "/") {
		if !validSegment(seg) {
			return r, errors.New("vault path must be segments of [A-Za-z0-9._-] joined by '/'")
		}
	}
	if !vaultKeyPattern.MatchString(r.Key) {
		return r, errors.New("vault key must be 1-128 characters of [A-Za-z0-9._-]")
	}
	return r, nil
}

func (r vaultRef) api() string { return r.Mount + "/data/" + r.Path }

// vaultClient logs in to Vault and reads KV v2 paths, caching each path's
// data for the refresh interval. Tokens and values live in memory only.
type vaultClient struct {
	base, host, namespace, kvMount string
	refreshEvery                   time.Duration
	loginMount                     string
	kubernetes                     *vaultKubernetesAuth
	appRole                        *vaultAppRoleAuth
	client                         *http.Client
	now                            func() time.Time
	redact                         *logging.SecretSet
	log                            *slog.Logger

	login sync.Mutex // one login at a time

	mu         sync.Mutex
	token      string
	tokenUntil time.Time
	paths      map[string]*vaultPath
}

type vaultPath struct {
	mu         sync.Mutex // one read per path at a time
	data       map[string]json.RawMessage
	freshUntil time.Time
	attempts   atomic.Uint64 // completed reads, so a queued caller sees one that ended while it waited
	failed     string        // the class of the last read when it failed
}

// newVaultClient validates e and checks that its login files are readable.
// It never contacts Vault: the worker starts while Vault is down.
func newVaultClient(e vaultEntry, c loadConfig) (*vaultClient, error) {
	u, err := url.Parse(e.Address)
	if err != nil || !u.IsAbs() || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		strings.Contains(e.Address, "#") || !hostPattern.MatchString(u.Host) || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("vault address must be an absolute URL with a lowercase host and no user info, path, query or fragment")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && c.allowPlain) {
		return nil, errors.New("vault address must be https (http only in development or test)")
	}
	if e.Namespace != "" && !vaultNamePattern.MatchString(e.Namespace) {
		return nil, errors.New("vault namespace must be 1-256 printable characters without spaces")
	}
	v := &vaultClient{base: u.Scheme + "://" + u.Host, host: u.Host, namespace: e.Namespace, kvMount: e.KVMount,
		now: c.now, redact: c.redact, log: c.log, paths: map[string]*vaultPath{}}
	if v.kvMount == "" {
		v.kvMount = "secret"
	}
	if !validSegment(v.kvMount) {
		return nil, errors.New("vault kv_mount must be one path segment of [A-Za-z0-9._-]")
	}
	refresh := e.RefreshSeconds
	if refresh == 0 {
		refresh = defaultVaultRefresh
	}
	if refresh < 30 || refresh > 3600 {
		return nil, errors.New("vault refresh_seconds must be 30-3600")
	}
	v.refreshEvery = time.Duration(refresh) * time.Second
	switch a := e.Auth; {
	case (a.Kubernetes == nil) == (a.AppRole == nil):
		return nil, errors.New("vault auth needs exactly one of kubernetes and approle")
	case a.Kubernetes != nil:
		k := *a.Kubernetes
		if k.Mount == "" {
			k.Mount = "kubernetes"
		}
		if !vaultNamePattern.MatchString(k.Role) || k.JWTFile == "" {
			return nil, errors.New("vault kubernetes auth needs a role and a jwt_file")
		}
		if _, ok := readLoginFile(k.JWTFile); !ok {
			return nil, errors.New("vault kubernetes jwt_file cannot be read or is empty")
		}
		v.kubernetes, v.loginMount = &k, k.Mount
	default:
		r := *a.AppRole
		if r.Mount == "" {
			r.Mount = "approle"
		}
		for _, pair := range []struct {
			inline, file *string
			name         string
		}{{r.RoleID, r.RoleIDFile, "role_id"}, {r.SecretID, r.SecretIDFile, "secret_id"}} {
			if (pair.inline == nil) == (pair.file == nil) {
				return nil, errors.New("vault approle needs exactly one of " + pair.name + " and " + pair.name + "_file")
			}
			if pair.inline != nil && *pair.inline == "" {
				return nil, errors.New("vault approle " + pair.name + " is empty")
			}
			if pair.file != nil {
				if _, ok := readLoginFile(*pair.file); !ok {
					return nil, errors.New("vault approle " + pair.name + "_file cannot be read or is empty")
				}
			}
		}
		v.appRole, v.loginMount = &r, r.Mount
	}
	if !validSegment(v.loginMount) {
		return nil, errors.New("vault auth mount must be one path segment of [A-Za-z0-9._-]")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if e.CAFile != "" {
		pem, err := os.ReadFile(e.CAFile)
		pool := x509.NewCertPool()
		if err != nil || !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("vault ca_file must hold PEM certificates")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	v.client = &http.Client{Timeout: vaultTimeout, Transport: transport,
		// A redirect could carry the Vault token to another host.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return v, nil
}

// readLoginFile returns a login file's content without trailing newlines.
func readLoginFile(path string) (string, bool) {
	b, err := os.ReadFile(path)
	s := strings.TrimRight(string(b), "\r\n")
	return s, err == nil && s != "" && len(s) <= maxAssertion
}

func (v *vaultClient) refresh() time.Duration { return v.refreshEvery }

// value returns the value of r and "" or, on failure, a failure class.
func (v *vaultClient) value(ctx context.Context, r vaultRef) (Secret, string) {
	r = v.withMount(r)
	p := v.path(r)
	seen := p.attempts.Load()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.data == nil && p.failed != "" && p.attempts.Load() != seen {
		// A read that failed while this caller waited answers it too:
		// callers queued on a failing Vault make no request each.
		return Secret{}, p.failed
	}
	if p.data == nil || !v.now().Before(p.freshUntil) {
		started := v.now()
		data, class := v.read(ctx, r)
		p.failed = class
		p.attempts.Add(1)
		if class != "" {
			p.data = nil
			v.log.WarnContext(ctx, "vault read failed", "vault", v.host, "mount", r.Mount, "path", r.Path, "class", class)
			return Secret{}, class
		}
		p.data, p.freshUntil = data, started.Add(v.refreshEvery)
	}
	raw, ok := p.data[r.Key]
	if !ok {
		return Secret{}, "vault_missing"
	}
	var s string
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte(`"`)) || json.Unmarshal(raw, &s) != nil || s == "" {
		return Secret{}, "vault_invalid"
	}
	if v.redact != nil {
		v.redact.AddPermanent(s)
	}
	return Secret{v: s}, ""
}

// drop forgets r's cached path: the next value reads Vault.
func (v *vaultClient) drop(r vaultRef) {
	p := v.path(v.withMount(r))
	p.mu.Lock()
	p.data = nil
	p.mu.Unlock()
}

// withMount gives r the client's kv_mount when it names none.
func (v *vaultClient) withMount(r vaultRef) vaultRef {
	if r.Mount == "" {
		r.Mount = v.kvMount
	}
	return r
}

func (v *vaultClient) path(r vaultRef) *vaultPath {
	v.mu.Lock()
	defer v.mu.Unlock()
	p := v.paths[r.api()]
	if p == nil {
		p = &vaultPath{}
		v.paths[r.api()] = p
	}
	return p
}

// read fetches r's path from KV v2.
func (v *vaultClient) read(ctx context.Context, r vaultRef) (map[string]json.RawMessage, string) {
	token, class := v.tokenFor(ctx)
	if class != "" {
		return nil, class
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.base+"/v1/"+r.api(), nil)
	if err != nil {
		return nil, "vault_read"
	}
	req.Header.Set("X-Vault-Token", token)
	status, body, ok := v.do(req)
	switch {
	case !ok:
		return nil, "vault_read"
	case status == http.StatusForbidden:
		v.dropToken(token)
		return nil, "vault_forbidden"
	case status == http.StatusNotFound:
		return nil, "vault_missing"
	case status != http.StatusOK:
		return nil, "vault_read"
	}
	var out struct {
		Data *struct {
			Data     map[string]json.RawMessage `json:"data"`
			Metadata struct {
				DeletionTime string `json:"deletion_time"`
				Destroyed    bool   `json:"destroyed"`
			} `json:"metadata"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &out) != nil {
		return nil, "vault_read"
	}
	if out.Data == nil || out.Data.Data == nil || out.Data.Metadata.DeletionTime != "" || out.Data.Metadata.Destroyed {
		return nil, "vault_missing"
	}
	return out.Data.Data, ""
}

// tokenFor returns a Vault token, logging in when none is usable.
func (v *vaultClient) tokenFor(ctx context.Context) (string, string) {
	v.login.Lock()
	defer v.login.Unlock()
	v.mu.Lock()
	if v.token != "" && v.now().Before(v.tokenUntil) {
		t := v.token
		v.mu.Unlock()
		return t, ""
	}
	v.mu.Unlock()
	body := map[string]string{}
	if k := v.kubernetes; k != nil {
		jwt, ok := readLoginFile(k.JWTFile)
		if !ok {
			return "", "vault_login_unreadable"
		}
		body["role"], body["jwt"] = k.Role, jwt
	} else {
		for name, pair := range map[string][2]*string{"role_id": {v.appRole.RoleID, v.appRole.RoleIDFile},
			"secret_id": {v.appRole.SecretID, v.appRole.SecretIDFile}} {
			if pair[0] != nil {
				body[name] = *pair[0]
				continue
			}
			s, ok := readLoginFile(*pair[1])
			if !ok {
				return "", "vault_login_unreadable"
			}
			body[name] = s
		}
	}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.base+"/v1/auth/"+v.loginMount+"/login", bytes.NewReader(payload))
	if err != nil {
		return "", "vault_login"
	}
	req.Header.Set("Content-Type", "application/json")
	started := v.now()
	status, raw, ok := v.do(req)
	if !ok || status != http.StatusOK {
		v.log.WarnContext(ctx, "vault login failed", "vault", v.host, "mount", v.loginMount, "status", status)
		return "", "vault_login"
	}
	var out struct {
		Auth *struct {
			ClientToken   string          `json:"client_token"`
			LeaseDuration json.RawMessage `json:"lease_duration"`
		} `json:"auth"`
	}
	if json.Unmarshal(raw, &out) != nil || out.Auth == nil || !printableToken(out.Auth.ClientToken) {
		return "", "vault_login"
	}
	lease, err := strconv.ParseInt(string(out.Auth.LeaseDuration), 10, 64)
	if err != nil || lease < 1 {
		return "", "vault_login"
	}
	leaseEnd := started.Add(time.Duration(lease) * time.Second)
	use := min(time.Duration(lease)*time.Second, maxVaultTokenUse) * 2 / 3
	if v.redact != nil {
		v.redact.Add(out.Auth.ClientToken, leaseEnd.Add(24*time.Hour))
	}
	v.mu.Lock()
	v.token, v.tokenUntil = out.Auth.ClientToken, started.Add(use)
	v.mu.Unlock()
	v.log.InfoContext(ctx, "vault login", "vault", v.host, "mount", v.loginMount, "lease_seconds", lease)
	return out.Auth.ClientToken, ""
}

func printableToken(s string) bool {
	if s == "" || len(s) > maxVaultToken {
		return false
	}
	for _, r := range s {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

// dropToken forgets token (a 403): the next request logs in again.
func (v *vaultClient) dropToken(token string) {
	v.mu.Lock()
	if v.token == token {
		v.token, v.tokenUntil = "", time.Time{}
	}
	v.mu.Unlock()
}

// do sends req with the namespace header and reads at most
// maxVaultResponse bytes of the answer.
func (v *vaultClient) do(req *http.Request) (int, []byte, bool) {
	if v.namespace != "" {
		req.Header.Set("X-Vault-Namespace", v.namespace)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return 0, nil, false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxVaultResponse+1))
	if err != nil || len(body) > maxVaultResponse {
		return resp.StatusCode, nil, false
	}
	return resp.StatusCode, body, true
}

// vaultValue is a static connector credential read from Vault (value_vault).
// A failed read withholds the binding with the mint back-off; a stale value
// is never served (ADR-019 §3c).
type vaultValue struct {
	client  *vaultClient
	ref     vaultRef
	binding Binding
	now     func() time.Time
	log     *slog.Logger

	mu            sync.Mutex
	current       Secret
	previous      Secret // the value just rotated out, scrubbed until previousUntil
	previousUntil time.Time
	backoff       time.Duration
	backoffUntil  time.Time
}

func (v *vaultValue) credential(ctx context.Context) (Secret, error) {
	v.mu.Lock()
	waiting := v.now().Before(v.backoffUntil)
	v.mu.Unlock()
	if waiting {
		return Secret{}, ErrCredentialUnavailable
	}
	s, class := v.client.value(ctx, v.ref)
	if class == "" && len(s.v) > maxSecret {
		class = "vault_invalid"
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now() // after the read: a read that failed slowly still backs off
	if class != "" {
		if now.Before(v.backoffUntil) {
			return Secret{}, ErrCredentialUnavailable // another caller's failure already set the back-off
		}
		v.backoff = min(max(2*v.backoff, minMintBackoff), maxMintBackoff)
		v.backoffUntil = now.Add(v.backoff)
		v.log.WarnContext(ctx, "vault credential unavailable", "tenant", v.binding.TenantID.String(),
			"ref", v.binding.Ref, "class", class, "retry_after", v.backoff)
		return Secret{}, ErrCredentialUnavailable
	}
	if s.v != v.current.v {
		if v.current.v != "" {
			v.previous, v.previousUntil = v.current, now.Add(v.client.refresh())
		}
		v.current = s
	}
	v.backoff, v.backoffUntil = 0, time.Time{}
	return s, nil
}

func (v *vaultValue) available(now time.Time) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return !now.Before(v.backoffUntil)
}

// rejected drops the cached path when the target refused the current value,
// so the next call reads a rotation at once.
func (v *vaultValue) rejected(s Secret) {
	v.mu.Lock()
	current := v.current.v
	v.mu.Unlock()
	if s.v != "" && s.v == current {
		v.client.drop(v.ref)
	}
}

// live returns the values to scrub: the current one and, for one refresh
// interval after a rotation, the previous one.
func (v *vaultValue) live(now time.Time) []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []string
	if v.current.v != "" {
		out = append(out, v.current.v)
	}
	if v.previous.v != "" && now.Before(v.previousUntil) {
		out = append(out, v.previous.v)
	}
	return out
}
