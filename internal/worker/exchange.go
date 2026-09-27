package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// OAuth 2.0 token exchange (RFC 8693) and GCP service-account impersonation
// (ADR-019 §3e): an oauth2 binding with "grant": "token_exchange" trades the
// worker's own identity token for an access token.

const (
	grantTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"
	tokenTypeAccess    = "urn:ietf:params:oauth:token-type:access_token"
	tokenTypeJWT       = "urn:ietf:params:oauth:token-type:jwt"
	// defaultImpersonation is the lifetime asked of generateAccessToken:
	// Google's default and the one-hour use cap agree.
	defaultImpersonation = time.Hour
	maxImpersonateScopes = 32
)

var (
	// subjectTokenTypes are the JWT types a subject token may be declared as:
	// RFC 8693's jwt and id_token, and the id-token spelling Google's client uses.
	subjectTokenTypes = map[string]bool{tokenTypeJWT: true,
		"urn:ietf:params:oauth:token-type:id_token": true, "urn:ietf:params:oauth:token-type:id-token": true}
	exchangeAudience = regexp.MustCompile(`^[\x21-\x7e]+$`) // at most 1024 bytes
	// impersonationPath is IAM Credentials' generateAccessToken for one
	// service account.
	impersonationPath = regexp.MustCompile(`^/v1/projects/-/serviceAccounts/([A-Za-z0-9._+-]+@[A-Za-z0-9.-]+):generateAccessToken$`)
	scopeToken        = regexp.MustCompile(`^[\x21\x23-\x5b\x5d-\x7e]+$`) // at most 1024 bytes
)

// subjectTokenEntry is the "subject_token" object: exactly one source.
type subjectTokenEntry struct {
	File   *string         `json:"file"`
	SPIFFE *spiffeAudience `json:"spiffe"`
}

// impersonateEntry is the "impersonate" object.
type impersonateEntry struct {
	URL             string   `json:"url"`
	Scope           []string `json:"scope"`
	LifetimeSeconds int      `json:"lifetime_seconds"`
}

// exchangeConfig is a validated token-exchange grant.
type exchangeConfig struct {
	subjectFile   string // re-read at every mint
	subjectSPIFFE string // or the JWT-SVID for this audience
	spiffe        *spiffeClient
	subjectType   string
	audience      string
	impersonate   *impersonation
}

type impersonation struct {
	url      string
	scope    []string
	lifetime time.Duration
}

// validateExchange checks the token-exchange fields of e. Its errors never
// repeat a value from the entry.
func validateExchange(e oauthEntry, c loadConfig) (*exchangeConfig, error) {
	switch e.Grant {
	case "", "client_credentials":
		if e.SubjectToken != nil || e.SubjectTokenType != "" || e.Audience != "" || e.Impersonate != nil {
			return nil, errors.New("subject_token, subject_token_type, audience and impersonate need grant token_exchange")
		}
		return nil, nil
	case "token_exchange":
	default:
		return nil, errors.New("grant must be client_credentials or token_exchange")
	}
	s := e.SubjectToken
	if s == nil || (s.File == nil) == (s.SPIFFE == nil) {
		return nil, errors.New("token_exchange needs a subject_token with exactly one of file and spiffe")
	}
	x := &exchangeConfig{subjectType: tokenTypeJWT, audience: e.Audience}
	if s.File != nil {
		// Only the file's shape is checked: the kubelet may be about to rotate it.
		if _, _, class := readAssertion(*s.File, time.Time{}); class != "" {
			return nil, errors.New("subject_token file must hold a compact JWS with a numeric exp (" + class + ")")
		}
		x.subjectFile = *s.File
	} else {
		if c.spiffe == nil {
			return nil, errors.New("a spiffe subject_token needs the file's spiffe object")
		}
		if err := s.SPIFFE.validate(); err != nil {
			return nil, err
		}
		x.subjectSPIFFE, x.spiffe = s.SPIFFE.Audience, c.spiffe
	}
	if e.SubjectTokenType != "" {
		if !subjectTokenTypes[e.SubjectTokenType] {
			return nil, errors.New("subject_token_type must be the jwt, id_token or id-token token type")
		}
		x.subjectType = e.SubjectTokenType
	}
	if e.Audience != "" && (len(e.Audience) > 1024 || !exchangeAudience.MatchString(e.Audience)) {
		return nil, errors.New("audience must be 1-1024 printable characters without spaces")
	}
	if e.Impersonate != nil {
		imp, err := validateImpersonation(*e.Impersonate, c)
		if err != nil {
			return nil, err
		}
		x.impersonate = imp
	}
	return x, nil
}

func validateImpersonation(e impersonateEntry, c loadConfig) (*impersonation, error) {
	u, err := url.Parse(e.URL)
	if err != nil || !u.IsAbs() || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		strings.Contains(e.URL, "#") || !hostPattern.MatchString(u.Host) {
		return nil, errors.New("impersonate url must be an absolute URL with a lowercase host and no user info, query or fragment")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && c.allowPlain) {
		return nil, errors.New("impersonate url must be https (http only in development or test)")
	}
	m := impersonationPath.FindStringSubmatch(u.EscapedPath())
	if m == nil || len(m[1]) < 3 || len(m[1]) > 254 {
		return nil, errors.New("impersonate url must be /v1/projects/-/serviceAccounts/<email>:generateAccessToken")
	}
	if len(e.Scope) == 0 || len(e.Scope) > maxImpersonateScopes {
		return nil, errors.New("impersonate scope must list 1-32 scopes")
	}
	for _, s := range e.Scope {
		if len(s) > 1024 || !scopeToken.MatchString(s) {
			return nil, errors.New("impersonate scope must be scope tokens")
		}
	}
	lifetime := defaultImpersonation
	if e.LifetimeSeconds != 0 {
		if e.LifetimeSeconds < 300 || e.LifetimeSeconds > 3600 {
			return nil, errors.New("impersonate lifetime_seconds must be 300-3600")
		}
		lifetime = time.Duration(e.LifetimeSeconds) * time.Second
	}
	return &impersonation{url: u.String(), scope: append([]string(nil), e.Scope...), lifetime: lifetime}, nil
}

// subjectToken returns this mint's subject token: the file read again
// (the platform rotates it) or the JWT-SVID for the subject audience. It
// must outlive the request; it is redacted until its exp and kept for
// scrubbing until then.
func (p *oauthProvider) subjectToken(ctx context.Context) (Secret, string) {
	var s Secret
	var exp time.Time
	var class string
	if x := p.exchange; x.spiffe != nil {
		s, exp, class = x.spiffe.svid(ctx, x.subjectSPIFFE, tokenRequestTimeout)
		if class == "" && exp.Before(p.now().Add(tokenRequestTimeout)) {
			class = "assertion_expired"
		}
	} else {
		s, exp, class = readAssertion(x.subjectFile, p.now())
	}
	if class != "" {
		return Secret{}, class
	}
	p.redactUntil(s, exp)
	p.mu.Lock()
	p.subject, p.subjectExp = s, exp
	p.mu.Unlock()
	return s, ""
}

// impersonate trades the federated token for a service account's access
// token (IAM Credentials generateAccessToken). The federated token goes only
// to the impersonation endpoint and is never cached for a later mint; it is
// redacted and kept for scrubbing until federatedExp. It returns 24a's
// token, use-until, real expiry and lifetime, or a failure class.
func (p *oauthProvider) impersonate(ctx context.Context, federated Secret, federatedExp time.Time) (Secret, time.Time, time.Time, time.Duration, string) {
	p.redactUntil(federated, federatedExp)
	p.mu.Lock()
	p.federated, p.federatedExp = federated, federatedExp
	p.mu.Unlock()
	imp := p.exchange.impersonate
	body, err := json.Marshal(map[string]any{"scope": imp.scope, "lifetime": fmt.Sprintf("%ds", int(imp.lifetime/time.Second))})
	if err != nil {
		return Secret{}, time.Time{}, time.Time{}, 0, "request"
	}
	ctx, cancel := context.WithTimeout(ctx, tokenRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, imp.url, bytes.NewReader(body))
	if err != nil {
		return Secret{}, time.Time{}, time.Time{}, 0, "request"
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+federated.v)
	start := p.now()
	resp, err := p.client.Do(req) // never follows a redirect
	if err != nil {
		return Secret{}, time.Time{}, time.Time{}, 0, "transport"
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponse+1))
	if resp.StatusCode != http.StatusOK {
		return Secret{}, time.Time{}, time.Time{}, 0, fmt.Sprintf("impersonation_http_%d", resp.StatusCode)
	}
	var r struct {
		AccessToken string `json:"accessToken"`
		ExpireTime  string `json:"expireTime"`
	}
	if err != nil || len(raw) > maxTokenResponse || json.Unmarshal(raw, &r) != nil ||
		len(r.AccessToken) > maxAccessToken || !tokenPattern.MatchString(r.AccessToken) {
		return Secret{}, time.Time{}, time.Time{}, 0, "impersonation_invalid"
	}
	exp, err := time.Parse(time.RFC3339, r.ExpireTime)
	if err != nil || !exp.After(start) || exp.After(start.Add(maxTokenLifetime*time.Second)) {
		return Secret{}, time.Time{}, time.Time{}, 0, "impersonation_invalid"
	}
	lifetime := min(exp.Sub(start), maxTokenUse)
	return Secret{r.AccessToken}, start.Add(lifetime), exp, lifetime, ""
}
