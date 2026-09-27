package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/proto/spiffe/workload"

	"eacp/internal/logging"
	"eacp/internal/spiffetest"
)

const (
	awsEndpoint = "https://abc123.execute-api.eu-west-1.amazonaws.com:443/prod/po"
	awsCanary   = "aws-canary-7f3e"
	awsKeyID    = "ASIAEACPTESTKEY00001"
	// a session token as AWS spells them: base64 with '/', '+' and '='
	awsSession = "IQoJb3JpZ2luX2VjEJr//+aws-canary-7f3e-session=="
)

// stsReply is AssumeRoleWithWebIdentity's XML answer.
func stsReply(keyID, secret, session, expiration string) string {
	return fmt.Sprintf(`<AssumeRoleWithWebIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">
  <AssumeRoleWithWebIdentityResult>
    <SubjectFromWebIdentityToken>system:serviceaccount:eacp:eacp-worker</SubjectFromWebIdentityToken>
    <AssumedRoleUser><Arn>arn:aws:sts::123456789012:assumed-role/eacp/eacp-worker</Arn><AssumedRoleId>AROAEACP:eacp-worker</AssumedRoleId></AssumedRoleUser>
    <Credentials><AccessKeyId>%s</AccessKeyId><SecretAccessKey>%s</SecretAccessKey><SessionToken>%s</SessionToken><Expiration>%s</Expiration></Credentials>
  </AssumeRoleWithWebIdentityResult>
  <ResponseMetadata><RequestId>ad4156e9-bce1-11e2-82e6-6b6efEXAMPLE</RequestId></ResponseMetadata>
</AssumeRoleWithWebIdentityResponse>`, keyID, secret, session, expiration)
}

// fakeSTS answers AssumeRoleWithWebIdentity as the test scripts it.
type fakeSTS struct {
	srv    *httptest.Server
	mu     sync.Mutex
	form   url.Values
	header http.Header
	calls  int
	answer func(n int) (int, string)
}

func newFakeSTS(t *testing.T) *fakeSTS {
	t.Helper()
	f := &fakeSTS{answer: func(n int) (int, string) {
		return 200, stsReply(awsKeyID, fmt.Sprintf("secret-%d-%s", n, awsCanary), awsSession,
			time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/aws/sts" {
			w.WriteHeader(http.StatusTeapot) // a redirect target
			return
		}
		_ = r.ParseForm()
		f.mu.Lock()
		f.calls++
		f.form, f.header = r.PostForm, r.Header.Clone()
		n, answer := f.calls, f.answer
		f.mu.Unlock()
		status, body := answer(n)
		if status == http.StatusTemporaryRedirect {
			w.Header().Set("Location", "/elsewhere")
		}
		w.Header().Set("Content-Type", "text/xml")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSTS) set(answer func(n int) (int, string)) {
	f.mu.Lock()
	f.answer = answer
	f.mu.Unlock()
}

// awsStore loads one aws binding against f on clock c; subject is the
// subject_token object and spiffe the file's spiffe object, if any.
func awsStore(t *testing.T, f *fakeSTS, c *vclock, spiffe, subject string, opts ...LoadOption) *SecretStore {
	t.Helper()
	body := fmt.Sprintf(`"role_arn":"arn:aws:iam::123456789012:role/eacp","region":"eu-west-1","service":"execute-api",`+
		`"sts_endpoint":%q,"subject_token":%s`, f.srv.URL+"/aws/sts", subject)
	s, err := LoadSecrets(awsFile(t, spiffe, body), append([]LoadOption{AllowPlainTokenURL(), WithClock(c.now)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func fileSubjectOf(path string) string { return fmt.Sprintf(`{"file":%q}`, path) }

func TestAnAssumeRoleSendsTheSubjectToken(t *testing.T) {
	a := spiffetest.New(t)
	var svid string
	a.Handle(func(r *workload.JWTSVIDRequest) (string, error) {
		svid = a.SVID(r.SpiffeId, r.Audience, time.Hour)
		return svid, nil
	})
	file := awsSubject(t)
	spiffe := fmt.Sprintf(`{"endpoint":%q,"spiffe_id":%q}`, a.Addr(), "spiffe://eacp.test/ns/eacp/sa/eacp-worker")
	for name, c := range map[string]struct {
		spiffe, subject string
		want            func() string
	}{
		"file":   {"", fileSubjectOf(file), func() string { return strings.TrimSpace(readText(t, file)) }},
		"spiffe": {spiffe, `{"spiffe":{"audience":"sts.amazonaws.com"}}`, func() string { return svid }},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeSTS(t)
			s := awsStore(t, f, &vclock{t: time.Now()}, c.spiffe, c.subject)
			tok, err := s.Credential(context.Background(), awsTenant, "erp", awsEndpoint, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			want := url.Values{"Action": {"AssumeRoleWithWebIdentity"}, "Version": {"2011-06-15"},
				"RoleArn": {"arn:aws:iam::123456789012:role/eacp"}, "RoleSessionName": {"eacp-worker"},
				"WebIdentityToken": {c.want()}, "DurationSeconds": {"3600"}}
			if fmt.Sprint(f.form) != fmt.Sprint(want) {
				t.Fatalf("form %v, want %v", f.form, want)
			}
			if f.header.Get("Authorization") != "" || !strings.HasPrefix(f.header.Get("Content-Type"), "application/x-www-form-urlencoded") {
				t.Fatalf("headers %v", f.header)
			}
			if tok.Reveal() != "secret-1-"+awsCanary || !tok.SignsRequests() {
				t.Fatal("the credential is not the secret key of a signing credential")
			}
		})
	}
}

func TestEveryInvalidSTSReplyIsRefused(t *testing.T) {
	at := func(d time.Duration) string { return time.Now().Add(d).UTC().Format(time.RFC3339) }
	type stsCase struct {
		status      int
		body, class string
	}
	invalid := func(keyID, secret, session, exp string) stsCase {
		return stsCase{200, stsReply(keyID, secret, session, exp), "sts_invalid"}
	}
	file := awsSubject(t)
	for name, c := range map[string]stsCase{
		"http 400":          {400, `<ErrorResponse><Error><Code>InvalidIdentityToken</Code></Error></ErrorResponse>`, "http_400"},
		"http 403":          {403, `<ErrorResponse/>`, "http_403"},
		"a redirect":        {307, ``, "http_307"},
		"not xml":           {200, `{"Credentials":{}}`, "sts_invalid"},
		"wrong root":        {200, strings.ReplaceAll(stsReply(awsKeyID, "s", "t", at(time.Hour)), "AssumeRoleWithWebIdentityResponse", "AssumeRoleResponse"), "sts_invalid"},
		"no key id":         invalid("", "s", "t", at(time.Hour)),
		"short key id":      invalid("abc", "s", "t", at(time.Hour)),
		"no secret":         invalid(awsKeyID, "", "t", at(time.Hour)),
		"secret with space": invalid(awsKeyID, "a b", "t", at(time.Hour)),
		"no session token":  invalid(awsKeyID, "s", "", at(time.Hour)),
		"huge session":      invalid(awsKeyID, "s", strings.Repeat("a", 8193), at(time.Hour)),
		"expired":           invalid(awsKeyID, "s", "t", at(-time.Minute)),
		"13 hours ahead":    invalid(awsKeyID, "s", "t", at(13*time.Hour)),
		"no zone":           invalid(awsKeyID, "s", "t", time.Now().Add(time.Hour).UTC().Format("2006-01-02T15:04:05")),
		"no expiration":     invalid(awsKeyID, "s", "t", ""),
		"too large":         {200, stsReply(awsKeyID, "s", "t", at(time.Hour)) + strings.Repeat(" ", 64<<10), "response_unreadable"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeSTS(t)
			f.set(func(int) (int, string) { return c.status, c.body })
			buf := &lockedBuffer{}
			s := awsStore(t, f, &vclock{t: time.Now()}, "", fileSubjectOf(file), WithLogger(slog.New(slog.NewJSONHandler(buf, nil))))
			if _, err := s.Credential(context.Background(), awsTenant, "erp", awsEndpoint, time.Second); !errors.Is(err, ErrCredentialUnavailable) {
				t.Fatalf("err = %v", err)
			}
			if !strings.Contains(buf.String(), `"class":"`+c.class+`"`) || len(s.Available()) != 0 {
				t.Fatalf("not withheld with %s: %s", c.class, buf.String())
			}
		})
	}
	f := newFakeSTS(t)
	buf := &lockedBuffer{}
	s := awsStore(t, f, &vclock{t: time.Now()}, "", fileSubjectOf(file), WithLogger(slog.New(slog.NewJSONHandler(buf, nil))))
	f.set(func(int) (int, string) { f.srv.CloseClientConnections(); return 200, "" })
	if _, err := s.Credential(context.Background(), awsTenant, "erp", awsEndpoint, time.Second); !errors.Is(err, ErrCredentialUnavailable) ||
		!strings.Contains(buf.String(), `"class":"transport"`) {
		t.Fatalf("a broken connection: %v %s", err, buf.String())
	}
}

func TestAWSKeysAreUsedForAtMostAnHour(t *testing.T) {
	f := newFakeSTS(t)
	c := &vclock{t: time.Now()}
	// The longest role session, measured from the worker's clock: wall time
	// could cross a second boundary and land past the 12-hour bound.
	f.set(func(int) (int, string) {
		return 200, stsReply(awsKeyID, "long-"+awsCanary, awsSession, c.now().Add(12*time.Hour).UTC().Format(time.RFC3339))
	})
	s := awsStore(t, f, c, "", fileSubjectOf(awsSubject(t)))
	if _, err := s.Credential(context.Background(), awsTenant, "erp", awsEndpoint, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Credential(context.Background(), awsTenant, "erp", awsEndpoint, 61*time.Minute); !errors.Is(err, ErrCredentialTooShort) {
		t.Fatalf("a 61-minute call: %v", err)
	}
	c.add(2 * time.Hour)
	if v := s.Values(); !slices.Contains(v, "long-"+awsCanary) || !slices.Contains(v, awsSession) {
		t.Fatal("the keys left Values before their real expiry")
	}
}

func TestAWSKeysAreRedactedAndScrubbed(t *testing.T) {
	f := newFakeSTS(t)
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second).Add(123 * time.Millisecond)
	f.set(func(int) (int, string) {
		return 200, stsReply(awsKeyID, "k-"+awsCanary, awsSession, exp.Format(time.RFC3339Nano))
	})
	set := logging.NewSecretSet()
	buf := &lockedBuffer{}
	c := &vclock{t: time.Now()}
	file := awsSubject(t)
	s := awsStore(t, f, c, "", fileSubjectOf(file), WithRedaction(set), WithLogger(slog.New(slog.NewJSONHandler(buf, nil))))
	tok, err := s.Credential(context.Background(), awsTenant, "erp", awsEndpoint, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	redacted, live := set.Values(), s.Values()
	for _, v := range []string{"k-" + awsCanary, awsSession, strings.TrimSpace(readText(t, file))} {
		if !slices.Contains(redacted, v) || !slices.Contains(live, v) {
			t.Errorf("a value is missing from the redaction set or Values: %.12s…", v)
		}
	}
	if !slices.Equal(tok.values(), []string{"k-" + awsCanary, awsSession}) {
		t.Fatal("the credential's values are not its secret key and session token")
	}
	log := buf.String()
	if !strings.Contains(log, `"grant":"aws_web_identity"`) || !strings.Contains(log, `"access_key_id":"`+awsKeyID+`"`) ||
		strings.Contains(log, awsCanary) {
		t.Fatalf("mint log: %s", log)
	}
	c.add(time.Hour + time.Second)
	if slices.Contains(s.Values(), awsSession) {
		t.Fatal("the session token stayed in Values after it expired")
	}
}

func TestARejectedAWSCredentialIsDropped(t *testing.T) {
	f := newFakeSTS(t)
	s := awsStore(t, f, &vclock{t: time.Now()}, "", fileSubjectOf(awsSubject(t)))
	tok, err := s.Credential(context.Background(), awsTenant, "erp", awsEndpoint, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	s.Rejected(awsTenant, "erp", tok)
	next, err := s.Credential(context.Background(), awsTenant, "erp", awsEndpoint, time.Minute)
	if err != nil || next.Reveal() == tok.Reveal() {
		t.Fatalf("a rejected credential was reused: %v", err)
	}
}

func readText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
