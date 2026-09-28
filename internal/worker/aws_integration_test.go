package worker_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/atipongsena/eacp/internal/fakeerp"
	"github.com/atipongsena/eacp/internal/jwttest"
	"github.com/atipongsena/eacp/internal/storage/pgtest"
)

const (
	awsRoleARN = "arn:aws:iam::000000000000:role/eacp-erp"
	awsAssumed = "aws:arn:aws:sts::000000000000:assumed-role/eacp-erp/"
)

// awsERP is a Fake ERP whose STS trusts subject for role eacp-erp and whose
// API takes SigV4 for us-east-1 and execute-api.
func awsERP(subject fakeerp.ExchangeSubject) func(string) fakeerp.Options {
	return func(string) fakeerp.Options {
		return fakeerp.Options{AWS: &fakeerp.AWS{RoleARN: awsRoleARN, Region: "us-east-1", Service: "execute-api",
			Subjects: []fakeerp.ExchangeSubject{subject}}}
	}
}

// awsBinding is a secrets file whose "erp-jit" binding assumes the role at
// the Fake ERP's STS with subjectToken; spiffe, when set, is its spiffe object.
func awsBinding(spiffe, session, subjectToken string) func(tokenURL, host string) string {
	return func(_, host string) string {
		if spiffe != "" {
			spiffe = `"spiffe":` + spiffe + `,`
		}
		return fmt.Sprintf(`{%s"secrets":[{"tenant_id":%q,"secret_ref":"erp-jit","host":%q,"aws":{"role_arn":%q,
			"role_session_name":%q,"region":"us-east-1","service":"execute-api","sts_endpoint":"http://%s/aws/sts",
			"subject_token":%s}}]}`, spiffe, pgtest.TenantA, host, awsRoleARN, session, host, subjectToken)
	}
}

// TestTheWorkerExecutesWithAWSKeysFromAFileSubject: the binding holds no
// credential; the worker assumes a role with its platform token (a file) and
// signs the execute with the temporary keys (ADR-019 Rev 1.6).
func TestTheWorkerExecutesWithAWSKeysFromAFileSubject(t *testing.T) {
	signer := jwttest.New(t)
	file := subjectFile(t, signer, time.Hour)
	v := newJITFile(t, awsERP(fakeerp.ExchangeSubject{Issuer: "https://issuer.test", Audience: "idp", Subject: stsSubject,
		Keys: []fakeerp.PublicKey{{KID: signer.KID, Key: &signer.Key.PublicKey}}}),
		awsBinding("", "eacp-worker-file", fmt.Sprintf(`{"file":%q}`, file)), 100)
	view := v.submit("")
	v.run(1)
	if got := v.get(view.ID); got.State != "SUCCEEDED" || got.AttemptCount != 1 {
		t.Fatalf("action = %+v", got)
	}
	if got := v.executePrincipals(); fmt.Sprint(got) != fmt.Sprint([]any{awsAssumed + "eacp-worker-file"}) {
		t.Fatalf("execute principals = %v, want one signed call", got)
	}
	// The live values: the subject (redacted while it lives), the secret key and the session token.
	values := v.secrets.Values()
	if len(values) != 3 || !slices.Contains(values, readFile(t, file)) {
		t.Fatalf("%d values, want the subject, the secret key and the session token", len(values))
	}
	v.assertNotPersisted(values...)
}

// TestTheWorkerExecutesWithAWSKeysFromAnSVID: the web identity is the
// worker's JWT-SVID for sts.amazonaws.com.
func TestTheWorkerExecutesWithAWSKeysFromAnSVID(t *testing.T) {
	a := newSVIDAgent(t)
	v := newJITFile(t, awsERP(fakeerp.ExchangeSubject{Issuer: "https://spire.test", Audience: "sts.amazonaws.com",
		Subject: spiffeWorker, Keys: a.keys()}),
		awsBinding(fmt.Sprintf(`{"endpoint":%q,"spiffe_id":%q}`, a.Addr(), spiffeWorker), "eacp-worker-spiffe",
			`{"spiffe":{"audience":"sts.amazonaws.com"}}`), 100)
	view := v.submit("")
	v.run(1)
	if got := v.get(view.ID); got.State != "SUCCEEDED" || got.AttemptCount != 1 {
		t.Fatalf("action = %+v", got)
	}
	if got := v.executePrincipals(); fmt.Sprint(got) != fmt.Sprint([]any{awsAssumed + "eacp-worker-spiffe"}) {
		t.Fatalf("execute principals = %v, want one signed call", got)
	}
	values := v.secrets.Values()
	if svids := a.svids(); len(values) != 3 || len(svids) != 1 || !slices.Contains(values, svids[0]) {
		t.Fatalf("%d values, want the SVID, the secret key and the session token", len(values))
	}
	v.assertNotPersisted(values...)
}
