package worker

import (
	"fmt"
	"net/url"
	"regexp"
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
