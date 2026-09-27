package fakeerp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// AWS STS AssumeRoleWithWebIdentity and SigV4-signed ERP calls for the demo
// and tests (ADR-019 §3f).

const (
	stsVersion      = "2011-06-15"
	stsNamespace    = "https://sts.amazonaws.com/doc/2011-06-15/"
	maxSigV4Body    = 64 << 10
	defaultDuration = 3600
)

var (
	awsRolePattern    = regexp.MustCompile(`^arn:(aws|aws-cn|aws-us-gov):iam::([0-9]{12}):role/(?:[\w+=,.@-]+/)*([\w+=,.@-]+)$`)
	awsRegionPattern  = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`)
	awsServicePattern = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
	awsSessionPattern = regexp.MustCompile(`^[\w+=,.@-]{2,64}$`)
)

// AWS is an STS: POST /aws/sts with AssumeRoleWithWebIdentity issues
// temporary keys for RoleARN to a web identity token that verifies against
// one of Subjects. The keys sign ERP calls with SigV4 for Region and Service.
type AWS struct {
	RoleARN, Region, Service string
	Subjects                 []ExchangeSubject
}

func (a *AWS) valid() error {
	if !awsRolePattern.MatchString(a.RoleARN) || !awsRegionPattern.MatchString(a.Region) ||
		!awsServicePattern.MatchString(a.Service) || len(a.Subjects) == 0 {
		return errors.New("fakeerp: AWS needs a role ARN, a region, a service and a subject")
	}
	for _, s := range a.Subjects {
		if s.Audience == "" || s.Subject == "" || len(s.Keys) == 0 {
			return errors.New("fakeerp: an AWS subject needs an audience, a subject and keys")
		}
	}
	return nil
}

// assumedRole is the ARN of a session of the role.
func (a *AWS) assumedRole(session string) string {
	m := awsRolePattern.FindStringSubmatch(a.RoleARN)
	return "arn:" + m[1] + ":sts::" + m[2] + ":assumed-role/" + m[3] + "/" + session
}

// awsKey is what the ERP keeps of issued keys: never the secret key or the
// session token, only the session token's SHA-256. The secret key is derived
// from the static credential and the key id.
type awsKey struct {
	principal, sessionSHA256 string
	expires                  time.Time
}

func (e *ERP) awsSecret(keyID string) string {
	m := hmac.New(sha256.New, []byte(e.token))
	m.Write([]byte("aws-secret:" + keyID))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

type stsError struct {
	XMLName xml.Name `xml:"ErrorResponse"`
	Xmlns   string   `xml:"xmlns,attr"`
	Error   struct {
		Type, Code, Message string
	}
}

type stsCredentials struct {
	AccessKeyID     string `xml:"AccessKeyId"`
	SecretAccessKey string
	SessionToken    string
	Expiration      string
}

type stsReply struct {
	XMLName xml.Name `xml:"AssumeRoleWithWebIdentityResponse"`
	Xmlns   string   `xml:"xmlns,attr"`
	Result  struct {
		SubjectFromWebIdentityToken string
		AssumedRoleUser             struct{ Arn, AssumedRoleId string }
		Credentials                 stsCredentials
	} `xml:"AssumeRoleWithWebIdentityResult"`
}

func writeXML(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(v)
}

// assumeRole is AssumeRoleWithWebIdentity. It never says which check of the
// web identity token failed.
func (e *ERP) assumeRole(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	formErr := r.ParseForm()
	f := r.PostForm
	e.mu.Lock()
	if e.failed {
		e.mu.Unlock()
		errorJSON(w, 503, "operation_log_unavailable")
		return
	}
	now := time.Now().UTC()
	ev := event{Audit: audit{At: now, Principal: "unauthenticated", Method: r.Method, Path: "/aws/sts"}}
	token, session := f.Get("WebIdentityToken"), f.Get("RoleSessionName")
	if token != "" {
		ev.Audit.SubjectSHA256 = subjectDigest(token)
	}
	a := e.oauth.AWS
	duration, durErr := defaultDuration, error(nil)
	if d := f.Get("DurationSeconds"); d != "" {
		duration, durErr = strconv.Atoi(d)
	}
	status, code, sub := 0, "", ""
	switch {
	case formErr != nil:
		status, code = 400, "MalformedInput"
	case f.Get("Action") != "AssumeRoleWithWebIdentity":
		status, code = 400, "InvalidAction"
	case f.Get("Version") != stsVersion:
		status, code = 400, "InvalidParameterValue"
	case token == "" || session == "" || f.Get("RoleArn") == "":
		status, code = 400, "MissingParameter"
	case !awsSessionPattern.MatchString(session) || durErr != nil || duration < 900 || duration > 3600:
		status, code = 400, "InvalidParameterValue"
	case f.Get("RoleArn") != a.RoleARN:
		status, code = 403, "AccessDenied"
	default:
		for _, s := range a.Subjects {
			if verifySVID(token, s.Keys, s.Issuer, s.Subject, s.Audience, now) {
				sub = s.Subject
				break
			}
		}
		if sub == "" {
			status, code = 400, "InvalidIdentityToken"
		}
	}
	var reply any
	if status != 0 {
		ev.Audit.Outcome = code
		x := stsError{Xmlns: stsNamespace}
		x.Error.Type, x.Error.Code, x.Error.Message = "Sender", code, code
		reply = x
	} else {
		keyID, sessionToken, ok := newAWSKeys()
		if !ok {
			e.mu.Unlock()
			errorJSON(w, 503, "token_unavailable")
			return
		}
		expires := now.Add(time.Duration(duration) * time.Second)
		arn := a.assumedRole(session)
		sum := sha256.Sum256([]byte(sessionToken))
		ev.Audit.Principal, ev.Audit.Outcome, ev.Audit.AWSAccessKeyID = "aws:"+arn, "keys_issued", keyID
		ev.Audit.TokenSHA256, ev.Audit.ExpiresAt = hex.EncodeToString(sum[:]), &expires
		x := stsReply{Xmlns: stsNamespace}
		x.Result.SubjectFromWebIdentityToken = sub
		x.Result.AssumedRoleUser.Arn, x.Result.AssumedRoleUser.AssumedRoleId = arn, "AROAEACPFAKEERP00001:"+session
		x.Result.Credentials = stsCredentials{AccessKeyID: keyID, SecretAccessKey: e.awsSecret(keyID),
			SessionToken: sessionToken, Expiration: expires.Format(time.RFC3339)}
		status, reply = 200, x
		w.Header().Set("Cache-Control", "no-store")
	}
	logged := e.log(ev)
	e.mu.Unlock()
	if !logged {
		errorJSON(w, 503, "audit_unavailable")
		return
	}
	writeXML(w, status, reply)
}

// newAWSKeys returns a temporary access key id (ASIA and 16 base32
// characters) and a random session token.
func newAWSKeys() (string, string, bool) {
	id, session := make([]byte, 10), make([]byte, 32)
	if _, err := rand.Read(id); err != nil {
		return "", "", false
	}
	if _, err := rand.Read(session); err != nil {
		return "", "", false
	}
	return "ASIA" + base32.StdEncoding.EncodeToString(id), base64.RawURLEncoding.EncodeToString(session), true
}

type sigV4CallerKey struct{}

type sigV4Caller struct{ principal, keyID string }

// verifyingSigV4 authenticates SigV4-signed requests before next sees them:
// a verified request carries its principal and key id in its context. An
// unverified one reaches next unauthenticated.
func (e *ERP) verifyingSigV4(next http.Handler) http.Handler {
	a := e.oauth.AWS
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), sigV4Label) {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxSigV4Body+1))
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), r.Body))
		if err != nil || len(body) > maxSigV4Body {
			next.ServeHTTP(w, r)
			return
		}
		now := time.Now()
		var principal string
		keyID, err := VerifySigV4(r, body, a.Region, a.Service, now, func(id string) (SigV4Key, bool) {
			e.mu.Lock()
			k, ok := e.awsKeys[id]
			e.mu.Unlock()
			session := r.Header.Get("X-Amz-Security-Token")
			sum := sha256.Sum256([]byte(session))
			if !ok || !now.Before(k.expires) ||
				subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(k.sessionSHA256)) != 1 {
				return SigV4Key{}, false
			}
			principal = k.principal
			return SigV4Key{SecretKey: e.awsSecret(id), SessionToken: session}, true
		})
		if err == nil {
			r = r.WithContext(context.WithValue(r.Context(), sigV4CallerKey{}, sigV4Caller{principal, keyID}))
		}
		next.ServeHTTP(w, r)
	})
}

// sigV4CallerOf returns the verified SigV4 caller of r, if any.
func sigV4CallerOf(r *http.Request) (sigV4Caller, bool) {
	c, ok := r.Context().Value(sigV4CallerKey{}).(sigV4Caller)
	return c, ok
}
