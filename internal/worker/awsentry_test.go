package worker

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"eacp/internal/jwttest"
)

var awsTenant = uuid.MustParse("00000000-0000-4000-8000-0000000000aa")

// awsSubject writes a projected-token-like JWT and returns its path.
func awsSubject(t *testing.T) string {
	t.Helper()
	now := time.Now()
	return writeFile(t, "subject", jwttest.New(t).Sign(map[string]any{"sub": "system:serviceaccount:eacp:eacp-worker",
		"aud": "sts.amazonaws.com", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}))
}

// awsFile is a secrets file with one aws binding "erp" whose aws object is
// body; spiffe, when set, is the file's spiffe object.
func awsFile(t *testing.T, spiffe, body string) string {
	t.Helper()
	if spiffe != "" {
		spiffe = `"spiffe":` + spiffe + `,`
	}
	return writeFile(t, "secrets.json", fmt.Sprintf(`{%s"secrets":[{"tenant_id":%q,"secret_ref":"erp",
		"host":"abc123.execute-api.eu-west-1.amazonaws.com:443","aws":{%s}}]}`, spiffe, awsTenant, body))
}

func TestAnAWSEntryLoads(t *testing.T) {
	subject := fmt.Sprintf(`"subject_token":{"file":%q}`, awsSubject(t))
	for name, c := range map[string]struct {
		body, endpoint, session string
		duration                time.Duration
	}{
		"defaults": {`"role_arn":"arn:aws:iam::123456789012:role/eacp-worker","region":"eu-west-1","service":"execute-api",` + subject,
			"https://sts.eu-west-1.amazonaws.com", "eacp-worker", time.Hour},
		"china": {`"role_arn":"arn:aws-cn:iam::123456789012:role/eacp","region":"cn-north-1","service":"execute-api",` + subject,
			"https://sts.cn-north-1.amazonaws.com.cn", "eacp-worker", time.Hour},
		"gov cloud": {`"role_arn":"arn:aws-us-gov:iam::123456789012:role/path/to/eacp","region":"us-gov-west-1","service":"s3",` + subject,
			"https://sts.us-gov-west-1.amazonaws.com", "eacp-worker", time.Hour},
		"explicit": {`"role_arn":"arn:aws:iam::123456789012:role/eacp","role_session_name":"eacp+worker@1","region":"us-east-1",` +
			`"service":"lambda","sts_endpoint":"http://127.0.0.1:9/aws/sts","duration_seconds":900,` + subject,
			"http://127.0.0.1:9/aws/sts", "eacp+worker@1", 15 * time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			s, err := LoadSecrets(awsFile(t, "", c.body), AllowPlainTokenURL())
			if err != nil {
				t.Fatal(err)
			}
			p := s.m[secretKey{awsTenant, "erp"}].oauth
			if p == nil || p.aws == nil || p.tokenURL != c.endpoint || p.aws.sessionName != c.session || p.aws.duration != c.duration {
				t.Fatalf("provider %+v", p)
			}
			if p.grant() != "aws_web_identity" || len(s.Available()) != 1 {
				t.Fatalf("grant %q, available %v", p.grant(), s.Available())
			}
		})
	}
}

func TestInvalidAWSEntriesRejectTheWholeFile(t *testing.T) {
	subject := fmt.Sprintf(`"subject_token":{"file":%q}`, awsSubject(t))
	role := `"role_arn":"arn:aws:iam::123456789012:role/eacp"`
	rest := `,"region":"eu-west-1","service":"execute-api",` + subject
	good := role + rest
	for name, body := range map[string]string{
		"no role":             strings.TrimPrefix(rest, ","),
		"not a role arn":      `"role_arn":"arn:aws:iam::123456789012:user/eacp"` + rest,
		"short account":       `"role_arn":"arn:aws:iam::1234:role/eacp"` + rest,
		"other partition":     `"role_arn":"arn:aws-iso:iam::123456789012:role/eacp"` + rest,
		"space in role":       `"role_arn":"arn:aws:iam::123456789012:role/ea cp"` + rest,
		"session too short":   good + `,"role_session_name":"e"`,
		"session bad char":    good + `,"role_session_name":"eacp worker"`,
		"session too long":    good + `,"role_session_name":"` + strings.Repeat("e", 65) + `"`,
		"no region":           role + `,"service":"execute-api",` + subject,
		"bad region":          role + `,"region":"EU-WEST-1","service":"execute-api",` + subject,
		"no service":          role + `,"region":"eu-west-1",` + subject,
		"bad service":         role + `,"region":"eu-west-1","service":"Execute API",` + subject,
		"duration too short":  good + `,"duration_seconds":899`,
		"duration too long":   good + `,"duration_seconds":3601`,
		"plain endpoint":      good + `,"sts_endpoint":"http://sts.example"`,
		"endpoint query":      good + `,"sts_endpoint":"https://sts.example/?a=b"`,
		"endpoint user":       good + `,"sts_endpoint":"https://u:p@sts.example"`,
		"endpoint upper host": good + `,"sts_endpoint":"https://STS.example"`,
		"endpoint relative":   good + `,"sts_endpoint":"/sts"`,
		"no subject":          role + `,"region":"eu-west-1","service":"execute-api"`,
		"both subjects":       role + `,"region":"eu-west-1","service":"execute-api","subject_token":{"file":"x","spiffe":{"audience":"a"}}`,
		"spiffe no block":     role + `,"region":"eu-west-1","service":"execute-api","subject_token":{"spiffe":{"audience":"sts.amazonaws.com"}}`,
		"unreadable subject":  role + `,"region":"eu-west-1","service":"execute-api","subject_token":{"file":"/nonexistent/token"}`,
	} {
		_, err := LoadSecrets(awsFile(t, "", body)) // not a development environment
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), "expected shape") {
			t.Errorf("%s: refused as an unknown field, not by validation: %v", name, err)
		}
		if strings.Contains(err.Error(), "123456789012") || strings.Contains(err.Error(), "eacp worker") {
			t.Errorf("%s: the error repeats a value: %v", name, err)
		}
	}
	both := writeFile(t, "secrets.json", fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"erp","host":"erp.example",
		"value":"v","aws":{%s}}]}`, awsTenant, good))
	if _, err := LoadSecrets(both); err == nil || !strings.Contains(err.Error(), "aws") {
		t.Fatalf("aws beside value: %v", err)
	}
}
