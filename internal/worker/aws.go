package worker

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// AWS STS web identity (ADR-019 §3f): an "aws" binding trades the worker's
// identity token for temporary AWS keys (AssumeRoleWithWebIdentity) and the
// HTTP connector signs its calls with them (SigV4).

const (
	defaultAWSSession  = "eacp-worker"
	defaultAWSDuration = time.Hour
)

var (
	roleARNPattern     = regexp.MustCompile(`^arn:(aws|aws-cn|aws-us-gov):iam::[0-9]{12}:role/[\w+=,.@/-]+$`)
	sessionNamePattern = regexp.MustCompile(`^[\w+=,.@-]{2,64}$`)
	regionPattern      = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`)
	servicePattern     = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
)

// awsEntry is the "aws" object of a secrets file entry.
type awsEntry struct {
	RoleARN         string             `json:"role_arn"`
	RoleSessionName string             `json:"role_session_name"`
	Region          string             `json:"region"`
	Service         string             `json:"service"`
	STSEndpoint     string             `json:"sts_endpoint"`
	DurationSeconds int                `json:"duration_seconds"`
	SubjectToken    *subjectTokenEntry `json:"subject_token"`
}

// awsConfig is a validated aws binding.
type awsConfig struct {
	roleARN, sessionName, region, service string
	duration                              time.Duration
}

// newAWSProvider validates entry i and returns the minting provider in its
// aws mode. Its errors never repeat a value from the entry.
func newAWSProvider(i int, e awsEntry, b Binding, c loadConfig) (*oauthProvider, error) {
	bad := func(what string) error { return fmt.Errorf("worker: secret %d: aws %s", i, what) }
	if len(e.RoleARN) < 20 || len(e.RoleARN) > 2048 || !roleARNPattern.MatchString(e.RoleARN) {
		return nil, bad("role_arn must be an IAM role ARN in the aws, aws-cn or aws-us-gov partition")
	}
	a := &awsConfig{roleARN: e.RoleARN, sessionName: defaultAWSSession, region: e.Region, service: e.Service,
		duration: defaultAWSDuration}
	if e.RoleSessionName != "" {
		if !sessionNamePattern.MatchString(e.RoleSessionName) {
			return nil, bad("role_session_name must be 2-64 characters of letters, digits and +=,.@_-")
		}
		a.sessionName = e.RoleSessionName
	}
	if len(e.Region) > 32 || !regionPattern.MatchString(e.Region) {
		return nil, bad("region must be an AWS region such as eu-west-1")
	}
	if !servicePattern.MatchString(e.Service) {
		return nil, bad("service must be a signing name of lowercase letters, digits and dashes")
	}
	if e.DurationSeconds != 0 {
		if e.DurationSeconds < 900 || e.DurationSeconds > 3600 {
			return nil, bad("duration_seconds must be 900-3600")
		}
		a.duration = time.Duration(e.DurationSeconds) * time.Second
	}
	endpoint := e.STSEndpoint
	if endpoint == "" {
		endpoint = "https://sts." + e.Region + ".amazonaws.com"
		if strings.HasPrefix(e.RoleARN, "arn:aws-cn:") {
			endpoint += ".cn"
		}
	}
	u, err := url.Parse(endpoint)
	if err != nil || !u.IsAbs() || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		strings.Contains(endpoint, "#") || !hostPattern.MatchString(u.Host) {
		return nil, bad("sts_endpoint must be an absolute URL with a lowercase host and no user info, query or fragment")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && c.allowPlain) {
		return nil, bad("sts_endpoint must be https (http only in development or test)")
	}
	x, err := validateSubject(e.SubjectToken, c)
	if err != nil {
		return nil, bad(err.Error())
	}
	return &oauthProvider{binding: b, tokenURL: u.String(), exchange: x, aws: a,
		now: c.now, redact: c.redact, log: c.log, client: newTokenClient()}, nil
}

// awsKeys are the rest of a temporary AWS credential; its secret access key
// is the Secret's value.
type awsKeys struct {
	accessKeyID, sessionToken, region, service string
}

const (
	maxAWSSecretKey    = 1024
	maxAWSSessionToken = 8192
	maxAWSKeyLifetime  = 12 * time.Hour // AWS's longest role session
)

var accessKeyIDPattern = regexp.MustCompile(`^[A-Z0-9]{16,128}$`)

// assumeRole trades the subject token for temporary keys
// (AssumeRoleWithWebIdentity, unsigned) and returns them as a signing Secret
// with the provider's use-until, real expiry and lifetime, or a failure
// class, never a response body or secret.
func (p *oauthProvider) assumeRole(ctx context.Context) (Secret, time.Time, time.Time, time.Duration, string) {
	subject, class := p.subjectToken(ctx)
	if class != "" {
		return Secret{}, time.Time{}, time.Time{}, 0, class
	}
	a := p.aws
	form := url.Values{"Action": {"AssumeRoleWithWebIdentity"}, "Version": {"2011-06-15"}, "RoleArn": {a.roleARN},
		"RoleSessionName": {a.sessionName}, "WebIdentityToken": {subject.v},
		"DurationSeconds": {strconv.Itoa(int(a.duration / time.Second))}}
	ctx, cancel := context.WithTimeout(ctx, tokenRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Secret{}, time.Time{}, time.Time{}, 0, "request"
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/xml")
	start := p.now()
	resp, err := p.client.Do(req) // never follows a redirect
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
	var r struct {
		XMLName     xml.Name `xml:"AssumeRoleWithWebIdentityResponse"`
		Credentials struct {
			AccessKeyID     string `xml:"AccessKeyId"`
			SecretAccessKey string `xml:"SecretAccessKey"`
			SessionToken    string `xml:"SessionToken"`
			Expiration      string `xml:"Expiration"`
		} `xml:"AssumeRoleWithWebIdentityResult>Credentials"`
	}
	if xml.Unmarshal(body, &r) != nil {
		return Secret{}, time.Time{}, time.Time{}, 0, "sts_invalid"
	}
	c := r.Credentials
	if !accessKeyIDPattern.MatchString(c.AccessKeyID) ||
		len(c.SecretAccessKey) > maxAWSSecretKey || !tokenPattern.MatchString(c.SecretAccessKey) ||
		len(c.SessionToken) > maxAWSSessionToken || !tokenPattern.MatchString(c.SessionToken) {
		return Secret{}, time.Time{}, time.Time{}, 0, "sts_invalid"
	}
	exp, err := time.Parse(time.RFC3339, c.Expiration)
	if err != nil || !exp.After(start) || exp.After(start.Add(maxAWSKeyLifetime)) {
		return Secret{}, time.Time{}, time.Time{}, 0, "sts_invalid"
	}
	lifetime := min(exp.Sub(start), maxTokenUse)
	return Secret{v: c.SecretAccessKey, aws: &awsKeys{accessKeyID: c.AccessKeyID, sessionToken: c.SessionToken,
		region: a.region, service: a.service}}, start.Add(lifetime), exp, lifetime, ""
}
