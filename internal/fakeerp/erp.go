// Package fakeerp provides the credential-protected Slice A demo target.
// Its append log retains effects and audit evidence across process restarts.
package fakeerp

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type record struct {
	OperationKey      string    `json:"operation_key"`
	ExternalReference string    `json:"external_reference"`
	VisibleAt         time.Time `json:"visible_at"`
}

type audit struct {
	At           time.Time `json:"at"`
	Principal    string    `json:"principal"`
	TenantID     string    `json:"tenant_id,omitempty"`
	Method       string    `json:"method"`
	Path         string    `json:"path"`
	OperationKey string    `json:"operation_key,omitempty"`
	Outcome      string    `json:"outcome"`
	// A token issuance records the token's SHA-256 and expiry, never the token.
	TokenSHA256 string     `json:"token_sha256,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	// A federated issuance records the assertion's SHA-256, never the assertion.
	AssertionSHA256 string `json:"assertion_sha256,omitempty"`
	// A private_key_jwt issuance records the assertion's jti: it is single-use.
	AssertionJTI string `json:"assertion_jti,omitempty"`
	// A call authorised by a JWT-SVID records its SHA-256, never the SVID.
	SVIDSHA256 string `json:"svid_sha256,omitempty"`
	// A token exchange records the subject token's SHA-256, never the token,
	// and the client that authenticated it, if any.
	SubjectSHA256 string `json:"subject_sha256,omitempty"`
	Client        string `json:"client,omitempty"`
	// Issued or signing AWS keys record the access key id, never the secret
	// key; an issuance records the session token's SHA-256 as TokenSHA256.
	AWSAccessKeyID string `json:"aws_access_key_id,omitempty"`
}

type event struct {
	Audit  audit   `json:"audit"`
	Record *record `json:"record,omitempty"`
}

type ERP struct {
	mu      sync.Mutex
	token   string
	oauth   Options
	path    string
	failed  bool
	records map[string][]record
	audit   []audit
	tokens  map[string]issued // SHA-256 of each minted token
	jtis    map[string]bool   // private_key_jwt assertions already used
	awsKeys map[string]awsKey // issued AWS keys by access key id
}

type issued struct {
	principal string
	expires   time.Time
}

// Options configures the optional OAuth 2.0 client-credentials token
// endpoint (RFC 6749 §4.4). With a client, POST /oauth/token issues
// short-lived bearer tokens that authorise like the static credential until
// they expire (ADR-019).
type Options struct {
	OAuthClientID     string
	OAuthClientSecret string
	TokenTTL          time.Duration  // default 300 s; 1 s to 1 h
	Federated         *Federated     // optional: a client authenticated by platform-issued assertions
	KeyClient         *KeyClient     // optional: a client authenticated by assertions it signs itself
	SPIFFEClient      *SPIFFEClient  // optional: a client authenticated by JWT-SVID assertions
	SPIFFEBearer      *SPIFFEBearer  // optional: JWT-SVIDs authorise the ERP API
	Exchange          *TokenExchange // optional: RFC 8693 token exchange at the token endpoint
	Impersonation     *Impersonation // optional: generateAccessToken for exchanged tokens
	AWS               *AWS           // optional: AssumeRoleWithWebIdentity and SigV4-signed calls
}

const defaultTokenTTL = 300 * time.Second

var connectorName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// New loads a durable Fake ERP operation log. A corrupt log fails startup
// closed, because an empty map could falsely say an operation never ran.
func New(token, dataPath string) (http.Handler, error) {
	return NewWithOptions(token, dataPath, Options{})
}

// NewWithOptions is New with an optional OAuth client.
func NewWithOptions(token, dataPath string, o Options) (http.Handler, error) {
	if token == "" || dataPath == "" {
		return nil, errors.New("fakeerp: credential and data path are required")
	}
	if (o.OAuthClientID == "") != (o.OAuthClientSecret == "") {
		return nil, errors.New("fakeerp: an OAuth client needs both an id and a secret")
	}
	if o.KeyClient != nil {
		if err := o.KeyClient.valid(); err != nil {
			return nil, err
		}
		if o.Federated != nil && o.Federated.ClientID == o.KeyClient.ClientID {
			return nil, errors.New("fakeerp: the federated and key clients need different ids")
		}
	}
	if o.Federated != nil {
		if err := o.Federated.valid(); err != nil {
			return nil, err
		}
	}
	if o.SPIFFEClient != nil {
		if err := o.SPIFFEClient.valid(); err != nil {
			return nil, err
		}
		if (o.Federated != nil && o.Federated.ClientID == o.SPIFFEClient.ClientID) ||
			(o.KeyClient != nil && o.KeyClient.ClientID == o.SPIFFEClient.ClientID) {
			return nil, errors.New("fakeerp: the SPIFFE client needs its own id")
		}
	}
	if o.SPIFFEBearer != nil {
		if err := o.SPIFFEBearer.valid(); err != nil {
			return nil, err
		}
	}
	if o.Exchange != nil {
		if err := o.Exchange.valid(); err != nil {
			return nil, err
		}
	}
	if o.Impersonation != nil {
		if o.Exchange == nil {
			return nil, errors.New("fakeerp: impersonation needs the token exchange")
		}
		if err := o.Impersonation.valid(); err != nil {
			return nil, err
		}
	}
	if o.AWS != nil {
		if err := o.AWS.valid(); err != nil {
			return nil, err
		}
	}
	if o.TokenTTL == 0 {
		o.TokenTTL = defaultTokenTTL
	}
	if o.TokenTTL < time.Second || o.TokenTTL > time.Hour {
		return nil, errors.New("fakeerp: the token TTL must be 1s-1h")
	}
	f, err := os.OpenFile(dataPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("fakeerp: open operation log: %w", err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return nil, fmt.Errorf("fakeerp: sync operation log: %w", err)
	}
	if err := syncDir(filepath.Dir(dataPath)); err != nil {
		return nil, err
	}
	e := &ERP{token: token, oauth: o, path: dataPath, records: map[string][]record{}, tokens: map[string]issued{},
		jtis: map[string]bool{}, awsKeys: map[string]awsKey{}}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		var ev event
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil || ev.Audit.At.IsZero() {
			return nil, errors.New("fakeerp: corrupt operation log")
		}
		e.apply(ev)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("fakeerp: read operation log: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/execute", e.execute)
	mux.HandleFunc("GET /v1/operations/{key}", e.lookup)
	mux.HandleFunc("GET /v1/audit", e.listAudit)
	if o.OAuthClientID != "" || o.Federated != nil || o.KeyClient != nil || o.SPIFFEClient != nil || o.Exchange != nil {
		mux.HandleFunc("POST /oauth/token", e.issue)
	}
	if o.Impersonation != nil {
		mux.HandleFunc("POST /v1/projects/-/serviceAccounts/{account}", e.generateAccessToken)
	}
	if o.AWS != nil {
		mux.HandleFunc("POST /aws/sts", e.assumeRole)
		return e.verifyingSigV4(mux), nil
	}
	return mux, nil
}

func (e *ERP) apply(ev event) {
	e.audit = append(e.audit, ev.Audit)
	if ev.Audit.Outcome == "token_issued" && ev.Audit.ExpiresAt != nil {
		e.tokens[ev.Audit.TokenSHA256] = issued{ev.Audit.Principal, *ev.Audit.ExpiresAt}
		if ev.Audit.AssertionJTI != "" {
			e.jtis[ev.Audit.AssertionJTI] = true // single use, across restarts
		}
	}
	if ev.Audit.Outcome == "keys_issued" && ev.Audit.ExpiresAt != nil {
		e.awsKeys[ev.Audit.AWSAccessKeyID] = awsKey{ev.Audit.Principal, ev.Audit.TokenSHA256, *ev.Audit.ExpiresAt}
	}
	if ev.Record != nil {
		e.records[ev.Record.OperationKey] = append(e.records[ev.Record.OperationKey], *ev.Record)
	}
}

// append writes and syncs before applying an effect in memory or replying.
// A failed write never becomes a definitive no-effect response.
func (e *ERP) append(ev event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(e.path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	e.apply(ev)
	return nil
}

func (e *ERP) principal(r *http.Request) string {
	if c, ok := sigV4CallerOf(r); ok {
		return c.principal
	}
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return "unauthenticated"
	}
	got := strings.TrimPrefix(header, "Bearer ")
	if subtle.ConstantTimeCompare([]byte(got), []byte(e.token)) == 1 {
		return "execution-worker"
	}
	sum := sha256.Sum256([]byte(got))
	if t, ok := e.tokens[hex.EncodeToString(sum[:])]; ok && time.Now().Before(t.expires) {
		return t.principal
	}
	if b := e.oauth.SPIFFEBearer; b != nil && b.verify(got, time.Now()) {
		return "spiffe:" + b.Subject
	}
	return "unauthenticated"
}

// privileged reports whether principal may execute, look up and read the
// audit: the static credential or an unexpired minted token.
func privileged(principal string) bool {
	return principal == "execution-worker" || strings.HasPrefix(principal, "oauth:") || strings.HasPrefix(principal, "spiffe:") ||
		strings.HasPrefix(principal, "sts:") || strings.HasPrefix(principal, "sa:") || strings.HasPrefix(principal, "aws:")
}

// issue is an OAuth 2.0 client-credentials token endpoint (RFC 6749 §4.4).
// A client authenticates with client_secret_basic or, when federated, with
// an RFC 7523 client assertion; one method per request. It keeps only each
// token's SHA-256 and expiry (and an assertion's SHA-256), in the durable
// log, so tokens survive a restart like every effect.
func (e *ERP) issue(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10) // a 16 KiB assertion, form-encoded
	formErr := r.ParseForm()
	hasBasic := r.Header.Get("Authorization") != ""
	hasAssertion := r.PostForm.Has("client_assertion") || r.PostForm.Has("client_assertion_type")
	exchange := e.oauth.Exchange != nil && r.PostForm.Get("grant_type") == grantTokenExchange
	// RFC 8693 leaves client authentication optional: an exchange without a
	// credential is a public request, one with a credential must authenticate.
	public := exchange && !hasBasic && !hasAssertion
	e.mu.Lock()
	if e.failed {
		e.mu.Unlock()
		errorJSON(w, 503, "operation_log_unavailable")
		return
	}
	now := time.Now().UTC()
	ev := event{Audit: audit{At: now, Principal: "unauthenticated", Method: r.Method, Path: "/oauth/token"}}
	status, reply := 0, map[string]any(nil)
	id, authenticated := "", false
	switch {
	case formErr != nil || (hasBasic && hasAssertion):
		ev.Audit.Outcome, status, reply = "invalid_request", 400, map[string]any{"error": "invalid_request"}
	case public:
	case hasAssertion:
		// The assertion client is chosen by client_id.
		if f := e.oauth.Federated; f != nil && f.verifyAssertion(r.PostForm, now) {
			id, authenticated = f.ClientID, true
		}
		if k := e.oauth.KeyClient; k != nil && !authenticated {
			if jti, ok := k.verifyAssertion(r.PostForm, now); ok && !e.jtis[jti] {
				id, authenticated, ev.Audit.AssertionJTI = k.ClientID, true, jti
			}
		}
		if c := e.oauth.SPIFFEClient; c != nil && !authenticated && c.verifyAssertion(r.PostForm, now) {
			id, authenticated = c.ClientID, true
		}
		if authenticated {
			sum := sha256.Sum256([]byte(r.PostForm.Get("client_assertion")))
			ev.Audit.AssertionSHA256 = hex.EncodeToString(sum[:])
		}
	default:
		basicID, secret, ok := r.BasicAuth()
		basicID, idErr := url.QueryUnescape(basicID)
		secret, secretErr := url.QueryUnescape(secret)
		if ok && idErr == nil && secretErr == nil && e.oauth.OAuthClientID != "" &&
			subtle.ConstantTimeCompare([]byte(basicID), []byte(e.oauth.OAuthClientID)) == 1 &&
			subtle.ConstantTimeCompare([]byte(secret), []byte(e.oauth.OAuthClientSecret)) == 1 {
			id, authenticated = basicID, true
		} else {
			w.Header().Set("WWW-Authenticate", `Basic realm="fakeerp"`)
		}
	}
	switch {
	case status != 0:
	case !authenticated && !public:
		ev.Audit.Outcome, status, reply = "invalid_client", 401, map[string]any{"error": "invalid_client"}
	case exchange:
		sub, code := e.oauth.Exchange.subject(r.PostForm, now)
		ev.Audit.Client = id
		if s := r.PostForm.Get("subject_token"); s != "" {
			ev.Audit.SubjectSHA256 = subjectDigest(s)
		}
		if code != "" {
			ev.Audit.Outcome, status, reply = code, 400, map[string]any{"error": code}
			break
		}
		token, ok := e.newToken(&ev, "sts:"+sub, now, e.oauth.TokenTTL)
		if !ok {
			e.mu.Unlock()
			errorJSON(w, 503, "token_unavailable")
			return
		}
		status, reply = 200, map[string]any{"access_token": token, "issued_token_type": tokenTypeAccess,
			"token_type": "Bearer", "expires_in": int(e.oauth.TokenTTL / time.Second)}
		w.Header().Set("Cache-Control", "no-store")
	case r.PostForm.Get("grant_type") != "client_credentials":
		ev.Audit.Principal = "oauth:" + id
		ev.Audit.Outcome, status, reply = "unsupported_grant_type", 400, map[string]any{"error": "unsupported_grant_type"}
	default:
		token, ok := e.newToken(&ev, "oauth:"+id, now, e.oauth.TokenTTL)
		if !ok {
			e.mu.Unlock()
			errorJSON(w, 503, "token_unavailable")
			return
		}
		status, reply = 200, map[string]any{"access_token": token, "token_type": "Bearer",
			"expires_in": int(e.oauth.TokenTTL / time.Second)}
		w.Header().Set("Cache-Control", "no-store")
	}
	logged := e.log(ev)
	e.mu.Unlock()
	if !logged {
		errorJSON(w, 503, "audit_unavailable")
		return
	}
	writeJSON(w, status, reply)
}

// newToken mints a random bearer token for principal living ttl and records
// its SHA-256 and expiry in ev, never the token.
func (e *ERP) newToken(ev *event, principal string, now time.Time, ttl time.Duration) (string, bool) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", false
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	expires := now.Add(ttl)
	ev.Audit.Principal = principal
	ev.Audit.Outcome, ev.Audit.TokenSHA256, ev.Audit.ExpiresAt = "token_issued", hex.EncodeToString(sum[:]), &expires
	return token, true
}

func (e *ERP) begin(r *http.Request, key string) audit {
	path := r.URL.Path
	if strings.HasPrefix(path, "/v1/operations/") {
		path = "/v1/operations/{key}"
	}
	tenant := r.Header.Get("X-EACP-Tenant-ID")
	if _, err := uuid.Parse(tenant); err != nil {
		tenant = ""
	}
	if !validKey(key, tenant) {
		key = ""
	}
	a := audit{At: time.Now().UTC(), Principal: e.principal(r), TenantID: tenant,
		Method: r.Method, Path: path, OperationKey: key}
	if strings.HasPrefix(a.Principal, "spiffe:") {
		sum := sha256.Sum256([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
		a.SVIDSHA256 = hex.EncodeToString(sum[:])
	}
	if c, ok := sigV4CallerOf(r); ok {
		a.AWSAccessKeyID = c.keyID
	}
	return a
}

func (e *ERP) log(ev event) bool {
	if e.failed {
		return false
	}
	if err := e.append(ev); err != nil {
		e.failed = true
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func errorJSON(w http.ResponseWriter, status int, class string) {
	writeJSON(w, status, map[string]string{"error_class": class})
}

func validKey(key, tenant string) bool {
	parts := strings.Split(key, ":")
	if len(parts) != 3 || parts[0] != "eacp" || parts[1] != tenant {
		return false
	}
	_, err1 := uuid.Parse(parts[1])
	_, err2 := uuid.Parse(parts[2])
	return err1 == nil && err2 == nil
}

func (e *ERP) execute(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	if e.failed {
		e.mu.Unlock()
		errorJSON(w, 503, "operation_log_unavailable")
		return
	}
	if !privileged(e.principal(r)) {
		ev := event{Audit: e.begin(r, "")}
		ev.Audit.Outcome = "unauthorized"
		ok := e.log(ev)
		e.mu.Unlock()
		if !ok {
			errorJSON(w, 503, "audit_unavailable")
			return
		}
		errorJSON(w, 401, "unauthorized")
		return
	}
	var request map[string]json.RawMessage
	b, err := io.ReadAll(io.LimitReader(r.Body, (64<<10)+1))
	if err != nil || len(b) > 64<<10 || json.Unmarshal(b, &request) != nil {
		e.reject(w, r, "", 422, "validation")
		return
	}
	var tool string
	if json.Unmarshal(request["tool"], &tool) != nil {
		e.reject(w, r, "", 422, "validation")
		return
	}
	connector, operation, validTool := strings.Cut(tool, ".")
	if !validTool || !connectorName.MatchString(connector) || (operation != "create_po" && operation != "create_po_eventual") {
		e.reject(w, r, "", 422, "validation")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	native := key != ""
	if ref, ok := request["external_reference"]; ok {
		var correlation string
		if json.Unmarshal(ref, &correlation) != nil || (native && correlation != key) {
			e.reject(w, r, key, 422, "validation")
			return
		}
		if !native {
			key = correlation
		}
	}
	if !validKey(key, r.Header.Get("X-EACP-Tenant-ID")) {
		e.reject(w, r, key, 422, "validation")
		return
	}
	var payload struct {
		Scenario          string `json:"scenario"`
		DelayMS           int    `json:"delay_ms"`
		VisibilityDelayMS int    `json:"visibility_delay_ms"`
	}
	if json.Unmarshal(request["payload"], &payload) != nil || payload.DelayMS < 0 || payload.DelayMS > 5000 ||
		payload.VisibilityDelayMS < 0 || payload.VisibilityDelayMS > 600000 {
		e.reject(w, r, key, 422, "validation")
		return
	}
	if operation == "create_po" && (payload.Scenario == "delayed_visibility" || payload.VisibilityDelayMS != 0) {
		e.reject(w, r, key, 422, "validation")
		return
	}
	if native && len(e.records[key]) > 0 {
		rec := e.records[key][0]
		ev := event{Audit: e.begin(r, key)}
		ev.Audit.Outcome = "deduplicated"
		ok := e.log(ev)
		e.mu.Unlock()
		if !ok {
			errorJSON(w, 503, "audit_unavailable")
			return
		}
		writeJSON(w, 200, map[string]string{"external_reference": rec.ExternalReference})
		return
	}
	switch payload.Scenario {
	case "fail_before_execute":
		e.reject(w, r, key, 422, "validation")
		return
	case "rate_limit":
		e.reject(w, r, key, 429, "rate_limited")
		return
	case "5xx_before_effect":
		e.reject(w, r, key, 503, "fakeerp_5xx_before_effect")
		return
	case "outage":
		e.reject(w, r, key, 503, "fakeerp_outage_no_effect")
		return
	case "", "5xx_after_effect", "slow_response", "delayed_visibility", "execute_then_timeout", "execute_then_reset":
	default:
		e.reject(w, r, key, 422, "validation")
		return
	}
	parts := strings.Split(key, ":")
	ref := "PO-" + parts[2]
	if n := len(e.records[key]); n > 0 {
		ref += "-" + strconv.Itoa(n+1)
	}
	rec := record{OperationKey: key, ExternalReference: ref, VisibleAt: time.Now().UTC()}
	if operation == "create_po_eventual" && payload.VisibilityDelayMS > 0 {
		rec.VisibleAt = rec.VisibleAt.Add(time.Duration(payload.VisibilityDelayMS) * time.Millisecond)
	}
	ev := event{Audit: e.begin(r, key), Record: &rec}
	ev.Audit.Outcome = "effect_committed"
	ok := e.log(ev)
	e.mu.Unlock()
	if !ok {
		errorJSON(w, 503, "commit_ambiguous")
		return
	}
	switch payload.Scenario {
	case "5xx_after_effect":
		errorJSON(w, 503, "fakeerp_5xx_after_effect")
	case "execute_then_reset":
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, err := hj.Hijack()
			if err == nil {
				conn.Close()
				return
			}
		}
		errorJSON(w, 503, "response_lost")
	case "slow_response", "execute_then_timeout":
		d := time.Duration(payload.DelayMS) * time.Millisecond
		if d == 0 {
			d = 2 * time.Second
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(d):
		}
		writeJSON(w, 200, map[string]string{"external_reference": ref})
	default:
		writeJSON(w, 200, map[string]string{"external_reference": ref})
	}
}

// reject requires the ERP lock and releases it before writing the response.
func (e *ERP) reject(w http.ResponseWriter, r *http.Request, key string, status int, class string) {
	ev := event{Audit: e.begin(r, key)}
	ev.Audit.Outcome = class
	ok := e.log(ev)
	e.mu.Unlock()
	if !ok {
		errorJSON(w, 503, "audit_unavailable")
		return
	}
	errorJSON(w, status, class)
}

func (e *ERP) lookup(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	e.mu.Lock()
	if e.failed {
		e.mu.Unlock()
		errorJSON(w, 503, "operation_log_unavailable")
		return
	}
	if !privileged(e.principal(r)) {
		e.reject(w, r, key, 401, "unauthorized")
		return
	}
	if !validKey(key, r.Header.Get("X-EACP-Tenant-ID")) {
		e.reject(w, r, key, 422, "validation")
		return
	}
	records := e.records[key]
	ev := event{Audit: e.begin(r, key)}
	ev.Audit.Outcome = "lookup"
	ok := e.log(ev)
	e.mu.Unlock()
	if !ok {
		errorJSON(w, 503, "audit_unavailable")
		return
	}
	// A duplicate correlation key is conflicting evidence even when one
	// effect is still hidden by the eventual-visibility scenario.
	if len(records) > 1 {
		errorJSON(w, 409, "conflict")
		return
	}
	var visible []record
	for _, rec := range records {
		if !time.Now().Before(rec.VisibleAt) {
			visible = append(visible, rec)
		}
	}
	switch len(visible) {
	case 0:
		errorJSON(w, 404, "not_found")
	case 1:
		writeJSON(w, 200, map[string]string{"external_reference": visible[0].ExternalReference})
	default:
		errorJSON(w, 409, "conflict")
	}
}

func (e *ERP) listAudit(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	if e.failed {
		e.mu.Unlock()
		errorJSON(w, 503, "operation_log_unavailable")
		return
	}
	if !privileged(e.principal(r)) {
		e.reject(w, r, "", 401, "unauthorized")
		return
	}
	ev := event{Audit: e.begin(r, "")}
	ev.Audit.Outcome = "audit_read"
	ok := e.log(ev)
	entries := append([]audit(nil), e.audit...)
	e.mu.Unlock()
	if !ok {
		errorJSON(w, 503, "audit_unavailable")
		return
	}
	writeJSON(w, 200, entries)
}
