package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"eacp/internal/logging"
)

const (
	tokenRequestTimeout = 10 * time.Second
	maxTokenResponse    = 64 << 10
	maxTokenLifetime    = 86400 // seconds; an IdP may issue tokens living longer than an hour
	maxTokenUse         = time.Hour
	maxAccessToken      = 8192
	minMintBackoff      = time.Second
	maxMintBackoff      = time.Minute
	// minted tokens stay in the redaction set this long after they expire,
	// for late log lines.
	redactAfterExpiry = 24 * time.Hour
	// replaced tokens kept for scrubbing until their real expiry, per binding
	maxRetired = 16
)

var (
	clientIDPattern = regexp.MustCompile(`^[\x21-\x39\x3b-\x7e]{1,256}$`) // printable, no space or ':'
	scopePattern    = regexp.MustCompile(`^[\x21\x23-\x5b\x5d-\x7e]+( [\x21\x23-\x5b\x5d-\x7e]+)*$`)
	tokenPattern    = regexp.MustCompile(`^[\x21-\x7e]+$`) // at most maxAccessToken bytes
)

// oauthEntry is the "oauth2" object of a secrets file entry.
type oauthEntry struct {
	TokenURL         string  `json:"token_url"`
	ClientID         string  `json:"client_id"`
	ClientSecret     *string `json:"client_secret"`
	ClientSecretFile *string `json:"client_secret_file"`
	// The client secret read from Vault at each mint (ADR-019 §3c).
	ClientSecretVault *vaultRef `json:"client_secret_vault"`
	// A platform-issued JWT (RFC 7523), re-read at every mint: workload
	// identity federation, with no client secret at all.
	ClientAssertionFile *string `json:"client_assertion_file"`
	// A JWT-SVID for this audience from the Workload API, fetched at each
	// mint (ADR-019 §3d).
	ClientAssertionSPIFFE *spiffeAudience `json:"client_assertion_spiffe"`
	// An assertion the worker signs with its own key (private_key_jwt).
	PrivateKeyJWT *privateKeyJWTEntry `json:"private_key_jwt"`
	Scope         string              `json:"scope"`
	Resource      string              `json:"resource"`
	// Token exchange (RFC 8693, ADR-019 §3e): absent or client_credentials,
	// or token_exchange with a subject token and optional impersonation.
	Grant            string             `json:"grant"`
	SubjectToken     *subjectTokenEntry `json:"subject_token"`
	SubjectTokenType string             `json:"subject_token_type"`
	Audience         string             `json:"audience"`
	Impersonate      *impersonateEntry  `json:"impersonate"`
}

type heldToken struct {
	token  Secret
	expiry time.Time
}

// oauthProvider mints OAuth 2.0 client-credentials tokens (RFC 6749 §4.4)
// for one binding and keeps the latest in memory until it expires. It never
// persists a token (ADR-019).
type oauthProvider struct {
	binding            Binding
	tokenURL, clientID string
	clientSecret       Secret // empty when the client authenticates with an assertion
	assertionFile      string
	signer             *assertionSigner // private_key_jwt
	vault              *vaultClient     // with secretRef or pk: values read at each mint
	secretRef          *vaultRef        // client_secret_vault
	pk                 *vaultSigner     // private_key_jwt with a key or certificate in Vault
	spiffe             *spiffeClient    // client_assertion_spiffe: an SVID for spiffeAudience at each mint
	spiffeAudience     string
	scope, resource    string
	exchange           *exchangeConfig // token_exchange; nil for client credentials
	publicClient       bool            // token_exchange with client_id alone: sent in the form
	client             *http.Client
	now                func() time.Time
	redact             *logging.SecretSet
	log                *slog.Logger

	mint sync.Mutex // one mint at a time; callers waiting on it reuse its token

	mu           sync.Mutex
	token        Secret
	expiry       time.Time     // the token is used until then: at most maxTokenUse after its request
	realExpiry   time.Time     // it authorises at the target until then, so it is scrubbed until then
	lifetime     time.Duration // usable lifetime of the last token minted: a longer call cannot be served
	retired      []heldToken   // replaced or dropped tokens that still authorise, newest last
	assertion    Secret        // the client assertion last read, kept for scrubbing until assertionExp
	assertionExp time.Time
	subject      Secret // the subject token last exchanged (§3e), kept for scrubbing until subjectExp
	subjectExp   time.Time
	federated    Secret // the last federated token, kept for scrubbing until federatedExp
	federatedExp time.Time
	backoff      time.Duration
	backoffUntil time.Time
}

// newOAuthProvider validates entry i. Its errors never repeat a value from
// the entry.
func newOAuthProvider(i int, e oauthEntry, b Binding, c loadConfig) (*oauthProvider, error) {
	bad := func(what string) error { return fmt.Errorf("worker: secret %d: oauth2 %s", i, what) }
	u, err := url.Parse(e.TokenURL)
	if err != nil || !u.IsAbs() || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		strings.Contains(e.TokenURL, "#") || !hostPattern.MatchString(u.Host) {
		return nil, bad("token_url must be an absolute URL with a lowercase host and no user info, query or fragment")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && c.allowPlain) {
		return nil, bad("token_url must be https (http only in development or test)")
	}
	x, err := validateExchange(e, c)
	if err != nil {
		return nil, bad(err.Error())
	}
	if !clientIDPattern.MatchString(e.ClientID) && !(x != nil && e.ClientID == "") {
		return nil, bad("client_id must be 1-256 printable characters without spaces or ':'")
	}
	kinds := 0
	for _, set := range []bool{e.ClientSecret != nil, e.ClientSecretFile != nil, e.ClientSecretVault != nil,
		e.ClientAssertionFile != nil, e.ClientAssertionSPIFFE != nil, e.PrivateKeyJWT != nil} {
		if set {
			kinds++
		}
	}
	switch {
	case x != nil && kinds == 0: // no client authentication, or a public client
	case x != nil && e.ClientID == "":
		return nil, bad("client authentication needs a client_id")
	case kinds != 1:
		return nil, bad("needs exactly one of client_secret, client_secret_file, client_secret_vault, client_assertion_file, client_assertion_spiffe and private_key_jwt")
	}
	var secret, assertionFile string
	var assertion Secret
	var assertionExp time.Time
	var signer *assertionSigner
	var secretRef *vaultRef
	var pk *vaultSigner
	var sc *spiffeClient
	switch {
	case kinds == 0: // token_exchange without client authentication
	case e.ClientAssertionSPIFFE != nil:
		if c.spiffe == nil {
			return nil, bad("client_assertion_spiffe needs the file's spiffe object")
		}
		if err := e.ClientAssertionSPIFFE.validate(); err != nil {
			return nil, bad(err.Error())
		}
		sc = c.spiffe
	case e.PrivateKeyJWT != nil && (e.PrivateKeyJWT.KeyVault != nil || e.PrivateKeyJWT.CertificateVault != nil):
		var err error
		if pk, err = newVaultSigner(*e.PrivateKeyJWT, c); err != nil {
			return nil, bad(err.Error())
		}
	case e.ClientSecretVault != nil:
		if c.vault == nil {
			return nil, bad("client_secret_vault needs the file's vault object")
		}
		r, err := e.ClientSecretVault.validate(c.vault.kvMount)
		if err != nil {
			return nil, bad(err.Error())
		}
		secretRef = &r
	case e.PrivateKeyJWT != nil:
		var err error
		if signer, err = newAssertionSigner(*e.PrivateKeyJWT); err != nil {
			return nil, bad(err.Error())
		}
	case e.ClientSecret != nil:
		secret = *e.ClientSecret
	case e.ClientSecretFile != nil:
		raw, err := os.ReadFile(*e.ClientSecretFile)
		if err != nil {
			return nil, bad("client_secret_file cannot be read")
		}
		secret = strings.TrimRight(string(raw), "\r\n")
	default:
		// Only the file's shape is checked here: an expired assertion is
		// accepted, because the kubelet may be about to rotate it.
		assertionFile = *e.ClientAssertionFile
		var class string
		if assertion, assertionExp, class = readAssertion(assertionFile, time.Time{}); class != "" {
			return nil, bad("client_assertion_file must hold a compact JWS with a numeric exp (" + class + ")")
		}
	}
	if kinds > 0 && assertionFile == "" && signer == nil && pk == nil && secretRef == nil && sc == nil && (secret == "" || len(secret) > maxSecret) {
		return nil, bad(fmt.Sprintf("client secret must be 1-%d bytes", maxSecret))
	}
	if e.Scope != "" && (len(e.Scope) > 1024 || !scopePattern.MatchString(e.Scope)) {
		return nil, bad("scope must be scope tokens separated by single spaces")
	}
	if e.Resource != "" {
		r, err := url.Parse(e.Resource)
		if err != nil || !r.IsAbs() || r.Fragment != "" || strings.Contains(e.Resource, "#") || len(e.Resource) > 2048 {
			return nil, bad("resource must be an absolute URL without a fragment")
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	p := &oauthProvider{
		binding: b, tokenURL: u.String(), clientID: e.ClientID, clientSecret: Secret{secret},
		assertionFile: assertionFile, assertion: assertion, assertionExp: assertionExp, signer: signer,
		vault: c.vault, secretRef: secretRef, pk: pk, spiffe: sc,
		scope: e.Scope, resource: e.Resource, exchange: x, publicClient: x != nil && kinds == 0 && e.ClientID != "", now: c.now, redact: c.redact, log: c.log,
		client: &http.Client{Timeout: tokenRequestTimeout, Transport: transport,
			// A redirect could carry the client secret or assertion to another host.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
	if sc != nil {
		p.spiffeAudience = e.ClientAssertionSPIFFE.Audience
	}
	p.redactUntil(assertion, assertionExp)
	return p, nil
}

// cached returns the held token when it outlives validFor.
func (p *oauthProvider) cached(validFor time.Duration) (Secret, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.token.v != "" && p.now().Add(validFor).Before(p.expiry) {
		return p.token, true
	}
	return Secret{}, false
}

// available reports whether the provider is outside a back-off.
func (p *oauthProvider) available(now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !now.Before(p.backoffUntil)
}

// tooShort reports whether the last token minted lived no longer than
// validFor, so minting again cannot serve the call.
func (p *oauthProvider) tooShort(validFor time.Duration) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lifetime > 0 && validFor >= p.lifetime
}

// credential returns a token valid for at least validFor, minting one when
// the held token would expire sooner. It fails with ErrCredentialUnavailable
// during a back-off and when a mint fails, and with ErrCredentialTooShort
// when the IdP's tokens live shorter than the call: that is not an outage,
// so it neither backs off nor mints again for such a call.
func (p *oauthProvider) credential(ctx context.Context, validFor time.Duration) (Secret, error) {
	if s, ok := p.cached(validFor); ok {
		return s, nil
	}
	p.mint.Lock()
	defer p.mint.Unlock()
	if s, ok := p.cached(validFor); ok {
		return s, nil
	}
	if p.tooShort(validFor) {
		return Secret{}, ErrCredentialTooShort
	}
	if !p.available(p.now()) {
		return Secret{}, ErrCredentialUnavailable
	}
	tok, expiry, realExpiry, lifetime, class := p.request(ctx)
	if class == "" {
		p.redactUntil(tok, realExpiry)
	} else {
		// A rotated client secret or key is read at the next mint. Not under
		// p.mu: a path lock may wait on Vault, and Available and Values need p.mu.
		p.dropVault()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if class == "" {
		p.retire()
		p.token, p.expiry, p.realExpiry, p.lifetime = tok, expiry, realExpiry, lifetime
		p.backoff, p.backoffUntil = 0, time.Time{}
		if !now.Add(validFor).Before(expiry) {
			// Kept for shorter calls; this one cannot be served.
			p.log.ErrorContext(ctx, "credential lifetime shorter than the call", "tenant", p.binding.TenantID.String(),
				"ref", p.binding.Ref, "host", hostOf(p.tokenURL), "lifetime", lifetime, "needed", validFor)
			return Secret{}, ErrCredentialTooShort
		}
	}
	if class != "" {
		p.backoff = min(max(2*p.backoff, minMintBackoff), maxMintBackoff)
		p.backoffUntil = now.Add(p.backoff)
		p.log.WarnContext(ctx, "credential mint failed", append(p.mintAttrs(), "class", class, "retry_after", p.backoff)...)
		return Secret{}, ErrCredentialUnavailable
	}
	p.log.InfoContext(ctx, "credential minted", append(p.mintAttrs(), "expires_in", expiry.Sub(now).Round(time.Second))...)
	return tok, nil
}

// mintAttrs are the log attributes of a mint: the binding, the hosts it
// contacts, the grant and whether it impersonated. Never a token.
func (p *oauthProvider) mintAttrs() []any {
	attrs := []any{"tenant", p.binding.TenantID.String(), "ref", p.binding.Ref, "host", hostOf(p.tokenURL),
		"grant", p.grant(), "impersonated", p.impersonates()}
	if p.impersonates() {
		attrs = append(attrs, "impersonation_host", hostOf(p.exchange.impersonate.url))
	}
	return attrs
}

// grant names the binding's OAuth grant for the log.
func (p *oauthProvider) grant() string {
	if p.exchange != nil {
		return "token_exchange"
	}
	return "client_credentials"
}

// impersonates reports whether a mint ends with service-account impersonation.
func (p *oauthProvider) impersonates() bool {
	return p.exchange != nil && p.exchange.impersonate != nil
}

func (p *oauthProvider) redactUntil(tok Secret, expiry time.Time) {
	if p.redact != nil && tok.v != "" {
		p.redact.Add(tok.v, expiry.Add(redactAfterExpiry))
	}
}

// request performs one token request. It returns the token, the time it is
// used until (at most maxTokenUse after the request), its real expiry and its
// usable lifetime, or a failure class, never a response body or secret. Both
// expiries are measured from before the request.
func (p *oauthProvider) request(ctx context.Context) (Secret, time.Time, time.Time, time.Duration, string) {
	form := url.Values{"grant_type": {"client_credentials"}}
	if p.exchange != nil {
		// RFC 8693: the worker's own identity token is the subject (§3e).
		subject, class := p.subjectToken(ctx)
		if class != "" {
			return Secret{}, time.Time{}, time.Time{}, 0, class
		}
		form = url.Values{"grant_type": {grantTokenExchange}, "subject_token": {subject.v},
			"subject_token_type": {p.exchange.subjectType}, "requested_token_type": {tokenTypeAccess}}
		if p.exchange.audience != "" {
			form.Set("audience", p.exchange.audience)
		}
	}
	if p.scope != "" {
		form.Set("scope", p.scope)
	}
	if p.resource != "" {
		form.Set("resource", p.resource)
	}
	secret := p.clientSecret
	if p.secretRef != nil {
		s, class := p.vault.value(ctx, *p.secretRef)
		if class == "" && len(s.v) > maxSecret {
			class = "vault_invalid"
		}
		if class != "" {
			return Secret{}, time.Time{}, time.Time{}, 0, class
		}
		p.mu.Lock()
		p.clientSecret = s
		p.mu.Unlock()
		secret = s
	}
	signer := p.signer
	if p.pk != nil {
		var class string
		if signer, class = p.pk.current(ctx); class != "" {
			return Secret{}, time.Time{}, time.Time{}, 0, class
		}
	}
	if p.assertionFile != "" || signer != nil || p.spiffe != nil {
		var assertion Secret
		var exp time.Time
		var class string
		if p.spiffe != nil {
			// A JWT-SVID from the Workload API (§3d); it must outlive the request.
			assertion, exp, class = p.spiffe.svid(ctx, p.spiffeAudience, tokenRequestTimeout)
			if class == "" && exp.Before(p.now().Add(tokenRequestTimeout)) {
				class = "assertion_expired"
			}
		} else {
			assertion, exp, class = p.clientAssertion(p.now(), signer)
		}
		if class != "" {
			return Secret{}, time.Time{}, time.Time{}, 0, class
		}
		p.redactUntil(assertion, exp)
		p.mu.Lock()
		p.assertion, p.assertionExp = assertion, exp
		p.mu.Unlock()
		// RFC 7523 §2.2, with client_id as Entra ID requires; no Basic auth.
		form.Set("client_id", p.clientID)
		form.Set("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer")
		form.Set("client_assertion", assertion.v)
	}
	if p.publicClient {
		form.Set("client_id", p.clientID) // RFC 6749 §3.2.1: a public client identifies itself in the form
	}
	reqCtx, cancel := context.WithTimeout(ctx, tokenRequestTimeout) // the impersonation hop gets its own
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, p.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Secret{}, time.Time{}, time.Time{}, 0, "request"
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	switch {
	case form.Has("client_assertion"):
	case p.exchange != nil && p.clientID == "": // an exchange without client authentication
	case p.publicClient: // client_id is in the form
	default:
		// RFC 6749 §2.3.1: the id and secret are form-urlencoded before Basic auth.
		req.SetBasicAuth(url.QueryEscape(p.clientID), url.QueryEscape(secret.v))
	}
	start := p.now()
	resp, err := p.client.Do(req)
	if err != nil {
		return Secret{}, time.Time{}, time.Time{}, 0, "transport"
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponse+1))
	if err != nil || len(body) > maxTokenResponse {
		return Secret{}, time.Time{}, time.Time{}, 0, "response_unreadable"
	}
	if resp.StatusCode != http.StatusOK {
		return Secret{}, time.Time{}, time.Time{}, 0, fmt.Sprintf("http_%d", resp.StatusCode)
	}
	var tr struct {
		AccessToken     string          `json:"access_token"`
		TokenType       string          `json:"token_type"`
		ExpiresIn       json.RawMessage `json:"expires_in"` // a bare JSON integer
		IssuedTokenType string          `json:"issued_token_type"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return Secret{}, time.Time{}, time.Time{}, 0, "invalid_json"
	}
	if len(tr.AccessToken) > maxAccessToken || !tokenPattern.MatchString(tr.AccessToken) {
		return Secret{}, time.Time{}, time.Time{}, 0, "invalid_access_token"
	}
	if !strings.EqualFold(tr.TokenType, "Bearer") {
		return Secret{}, time.Time{}, time.Time{}, 0, "not_bearer"
	}
	n, err := strconv.ParseInt(string(tr.ExpiresIn), 10, 64)
	if err != nil || n < 1 || n > maxTokenLifetime {
		return Secret{}, time.Time{}, time.Time{}, 0, "invalid_expires_in"
	}
	if p.exchange != nil && tr.IssuedTokenType != tokenTypeAccess {
		// RFC 8693 §2.2.1 requires it; a JWT or refresh token is never sent as a Bearer.
		return Secret{}, time.Time{}, time.Time{}, 0, "wrong_token_type"
	}
	if p.exchange != nil && p.exchange.impersonate != nil {
		return p.impersonate(ctx, Secret{tr.AccessToken}, start.Add(time.Duration(n)*time.Second))
	}
	lifetime := min(time.Duration(n)*time.Second, maxTokenUse)
	return Secret{tr.AccessToken}, start.Add(lifetime), start.Add(time.Duration(n) * time.Second), lifetime, ""
}

// clientAssertion returns the client assertion of this mint and its expiry,
// or a failure class: the platform-issued file, read again because the
// platform rotates it (§3a), or a fresh one signed with the worker's key
// (§3b).
func (p *oauthProvider) clientAssertion(now time.Time, signer *assertionSigner) (Secret, time.Time, string) {
	if signer == nil {
		return readAssertion(p.assertionFile, now)
	}
	if !signer.validAt(now) {
		return Secret{}, time.Time{}, "certificate_not_valid"
	}
	a, exp, err := signer.sign(p.clientID, p.tokenURL, now)
	if err != nil {
		return Secret{}, time.Time{}, "assertion_signing"
	}
	return a, exp, ""
}

// dropVault forgets the provider's cached Vault paths.
func (p *oauthProvider) dropVault() {
	if p.secretRef != nil {
		p.vault.drop(*p.secretRef)
	}
	if p.pk != nil {
		p.pk.drop()
	}
}

// rejected drops the held token when it is s.
func (p *oauthProvider) rejected(s Secret) {
	p.mu.Lock()
	if s.v == "" || p.token.v != s.v {
		p.mu.Unlock()
		return
	}
	p.retire()
	p.token, p.expiry, p.realExpiry = Secret{}, time.Time{}, time.Time{}
	assertion := p.assertion
	p.mu.Unlock()
	if p.spiffe != nil {
		// The next mint fetches a new assertion. Not under p.mu: the audience
		// lock may wait on a fetch.
		p.spiffe.drop(p.spiffeAudience, assertion)
	}
}

// retire keeps the held token for scrubbing until its real expiry. The
// caller holds p.mu.
func (p *oauthProvider) retire() {
	if p.token.v == "" {
		return
	}
	p.retired = append(p.retired, heldToken{p.token, p.realExpiry})
	if len(p.retired) > maxRetired {
		p.retired = p.retired[len(p.retired)-maxRetired:]
	}
}

// live returns the client secret, the last client assertion until it
// expires, and every token, held or retired, until its real expiry: a token
// authorises at the target until then.
func (p *oauthProvider) live() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	var out []string
	if p.clientSecret.v != "" {
		out = append(out, p.clientSecret.v)
	}
	if p.assertion.v != "" && now.Before(p.assertionExp) {
		out = append(out, p.assertion.v)
	}
	if p.subject.v != "" && now.Before(p.subjectExp) {
		out = append(out, p.subject.v)
	}
	if p.federated.v != "" && now.Before(p.federatedExp) {
		out = append(out, p.federated.v)
	}
	if p.token.v != "" && now.Before(p.realExpiry) {
		out = append(out, p.token.v)
	}
	kept := p.retired[:0]
	for _, h := range p.retired {
		if now.Before(h.expiry) {
			kept = append(kept, h)
			out = append(out, h.token.v)
		}
	}
	p.retired = kept
	return out
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}
