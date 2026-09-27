package fakeerp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Token exchange (RFC 8693) and GCP-style service-account impersonation for
// the demo and tests (ADR-019 §3e).

const (
	grantTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"
	tokenTypeAccess    = "urn:ietf:params:oauth:token-type:access_token"
)

var (
	subjectTypes = map[string]bool{"urn:ietf:params:oauth:token-type:jwt": true,
		"urn:ietf:params:oauth:token-type:id_token": true, "urn:ietf:params:oauth:token-type:id-token": true}
	accountPattern  = regexp.MustCompile(`^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+$`)
	lifetimePattern = regexp.MustCompile(`^[1-9][0-9]{0,3}s$`)
)

// TokenExchange is an STS: POST /oauth/token with the token-exchange grant
// issues an access token for a subject token that verifies against one of
// Subjects, when the request names Audience. Client authentication is
// optional, as at GCP's STS; a request that presents a credential must
// authenticate with it.
type TokenExchange struct {
	Audience string
	Subjects []ExchangeSubject
}

// ExchangeSubject is a trusted subject token: RS256 or ES256 by the key its
// kid names, exactly Issuer (when set) and Subject, with Audience in aud.
type ExchangeSubject struct {
	Issuer, Audience, Subject string
	Keys                      []PublicKey
}

func (x *TokenExchange) valid() error {
	if x.Audience == "" || len(x.Subjects) == 0 {
		return errors.New("fakeerp: a token exchange needs an audience and a subject")
	}
	for _, s := range x.Subjects {
		if s.Audience == "" || s.Subject == "" || len(s.Keys) == 0 {
			return errors.New("fakeerp: an exchange subject needs an audience, a subject and keys")
		}
	}
	return nil
}

// subject checks an exchange request and returns the subject's sub, or the
// OAuth error code to answer with. It never says which check failed.
func (x *TokenExchange) subject(form url.Values, now time.Time) (string, string) {
	if !subjectTypes[form.Get("subject_token_type")] || form.Get("subject_token") == "" {
		return "", "invalid_request"
	}
	if t := form.Get("requested_token_type"); t != "" && t != tokenTypeAccess {
		return "", "invalid_request"
	}
	if form.Get("audience") != x.Audience {
		return "", "invalid_target"
	}
	token := form.Get("subject_token")
	for _, s := range x.Subjects {
		if verifySVID(token, s.Keys, s.Issuer, s.Subject, s.Audience, now) {
			return s.Subject, ""
		}
	}
	return "", "invalid_grant"
}

// Impersonation serves generateAccessToken for Accounts to callers holding a
// token the exchange issued.
type Impersonation struct {
	Accounts []string
}

func (i *Impersonation) valid() error {
	if len(i.Accounts) == 0 {
		return errors.New("fakeerp: impersonation needs a service account")
	}
	for _, a := range i.Accounts {
		if len(a) > 254 || !accountPattern.MatchString(a) {
			return errors.New("fakeerp: a service account must be an email address")
		}
	}
	return nil
}

// generateAccessToken is POST /v1/projects/-/serviceAccounts/{email}:generateAccessToken.
func (e *ERP) generateAccessToken(w http.ResponseWriter, r *http.Request) {
	raw, readErr := io.ReadAll(io.LimitReader(r.Body, (16<<10)+1))
	e.mu.Lock()
	if e.failed {
		e.mu.Unlock()
		errorJSON(w, 503, "operation_log_unavailable")
		return
	}
	now := time.Now().UTC()
	ev := event{Audit: audit{At: now, Principal: e.principal(r), Method: r.Method,
		Path: "/v1/projects/-/serviceAccounts/{account}:generateAccessToken"}}
	account, verb := strings.CutSuffix(r.PathValue("account"), ":generateAccessToken")
	var body struct {
		Scope    []string `json:"scope"`
		Lifetime string   `json:"lifetime"`
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var lifetime time.Duration
	status, reply := 0, map[string]any(nil)
	switch {
	case !strings.HasPrefix(ev.Audit.Principal, "sts:"):
		ev.Audit.Outcome, status, reply = "unauthorized", 401, map[string]any{"error": map[string]any{"code": 401}}
	case !verb || !slices.Contains(e.oauth.Impersonation.Accounts, account):
		ev.Audit.Outcome, status, reply = "permission_denied", 403, map[string]any{"error": map[string]any{"code": 403}}
	case readErr != nil || len(raw) > 16<<10 || dec.Decode(&body) != nil || len(body.Scope) == 0 || len(body.Scope) > 32 ||
		slices.Contains(body.Scope, "") || !lifetimePattern.MatchString(body.Lifetime):
		ev.Audit.Outcome, status, reply = "invalid_argument", 400, map[string]any{"error": map[string]any{"code": 400}}
	default:
		n, _ := strconv.Atoi(strings.TrimSuffix(body.Lifetime, "s"))
		if lifetime = time.Duration(n) * time.Second; lifetime > time.Hour {
			ev.Audit.Outcome, status, reply = "invalid_argument", 400, map[string]any{"error": map[string]any{"code": 400}}
		}
	}
	if status == 0 {
		token, ok := e.newToken(&ev, "sa:"+account, now, lifetime)
		if !ok {
			e.mu.Unlock()
			errorJSON(w, 503, "token_unavailable")
			return
		}
		status, reply = 200, map[string]any{"accessToken": token, "expireTime": now.Add(lifetime).Format(time.RFC3339)}
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

// subjectDigest is the SHA-256 of an exchanged subject token, for the audit.
func subjectDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
