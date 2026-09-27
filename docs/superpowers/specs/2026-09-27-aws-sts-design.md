# Phase 24g — AWS STS web identity and SigV4 signing (design)

Date: 2026-09-27 · Status: approved by the owner in chat (approach A; section 1 approved; "yes, continue and dev
until finish phase")
Scope: MASTER_PLAN §96 (Phase 24), the last cloud identity, and the five deferred minors of 24f. ADR: **ADR-019
Credential Custody, Rev 1.6**. Owner choices: `AssumeRoleWithWebIdentity` plus SigV4 signing in the HTTP
connector; the STS call written with the standard library under the provider rules; the pinned
`aws-sdk-go-v2` SigV4 signer for signing and for Fake ERP's verification; the 24f minors folded in.

## 1. Intent

AWS issues no Bearer tokens. A workload trades its identity token for temporary keys
(`AssumeRoleWithWebIdentity`) and signs every request with them (SigV4): API Gateway with IAM
authorisation, Lambda function URLs, AWS service APIs. After 24g a binding can do both with no long-lived
secret, and Phase 24's list (SPIFFE, cloud identities, Vault) is complete.

Success:

- an `aws` binding mints temporary keys from the worker's projected token or JWT-SVID, and the HTTP connector
  signs execute and lookup calls with SigV4 for the binding's region and service;
- the subject token goes only to STS, the secret key never leaves the worker, the session token only rides on
  signed requests to the bound host;
- every provider rule still applies (one mint at a time, reuse while it outlives the call, one-hour use cap,
  too-short, rejection, back-off, withholding from claims, redaction until expiry plus 24 h, scrubbing);
- no subject token, secret key or session token appears in logs, responses, the database or connector results;
- the minikube demo buys through both subject sources; compose and every 24a–24f binding behave as before;
- the five 24f minors are fixed.

## 2. Verified upstream API

- **STS** (`github.com/aws/aws-sdk-go-v2/service/sts` v1.51.1, `serializers.go`, `deserializers.go`):
  `AssumeRoleWithWebIdentity` is an unsigned query-protocol call: `POST` form
  `Action=AssumeRoleWithWebIdentity`, `Version=2011-06-15`, `RoleArn`, `RoleSessionName`, `WebIdentityToken`,
  optional `DurationSeconds`. The XML reply is `AssumeRoleWithWebIdentityResponse` →
  `AssumeRoleWithWebIdentityResult` → `Credentials` {`AccessKeyId`, `SecretAccessKey`, `SessionToken`,
  `Expiration` (ISO 8601 date-time)}, with `AssumedRoleUser` {`Arn`, `AssumedRoleId`} beside it. The SDK's
  default endpoint is regional: `https://sts.<region>.amazonaws.com` (`.amazonaws.com.cn` in `aws-cn`).
- **SigV4** (`github.com/aws/aws-sdk-go-v2` v1.47.1, `aws/signer/v4`, depends only on `smithy-go` v1.28.1):
  `v4.NewSigner().SignHTTP(ctx, aws.Credentials{AccessKeyID, SecretAccessKey, SessionToken}, req, payloadHash,
  service, region, signingTime)` sets `X-Amz-Date`, `X-Amz-Security-Token` (when a session token is given) and
  `Authorization: AWS4-HMAC-SHA256 Credential=<id>/<date>/<region>/<service>/aws4_request,
  SignedHeaders=…, Signature=…`. It signs every header present except `Authorization`, `User-Agent`,
  `X-Amzn-Trace-Id`, `Expect` and `Transfer-Encoding`, plus `host` and the content length.

## 3. Design

### 3.1 Secrets file

```json
{"tenant_id": "…", "secret_ref": "erp-aws", "host": "abc123.execute-api.eu-west-1.amazonaws.com:443",
 "aws": {"role_arn": "arn:aws:iam::123456789012:role/eacp-worker",
         "role_session_name": "eacp-worker",
         "region": "eu-west-1", "service": "execute-api",
         "sts_endpoint": "https://sts.eu-west-1.amazonaws.com",
         "duration_seconds": 3600,
         "subject_token": {"file": "/run/secrets/eacp-identity/token"}}}
```

- `aws` is a sixth credential form, exclusive with `value`, `value_file`, `value_vault`, `value_spiffe` and
  `oauth2`. Any failure rejects the whole file; the worker contacts nothing at load; errors never repeat a
  value.
- `role_arn`: `arn:<partition>:iam::<12 digits>:role/<path and name>`, partition `aws`, `aws-cn` or
  `aws-us-gov`, 20–2048 characters, the name part `[\w+=,.@/-]`.
- `role_session_name`: 2–64 characters of `[\w+=,.@-]`, default `eacp-worker`.
- `region`: `^[a-z]{2}(-[a-z]+)+-[0-9]+$`, at most 32 characters. `service`: `^[a-z0-9-]{1,64}$`.
- `sts_endpoint`: optional; default `https://sts.<region>.amazonaws.com` (`.amazonaws.com.cn` for `aws-cn`).
  Absolute, lowercase host, no user info, query or fragment; a path is allowed; `https` (`http` only in
  development and test).
- `duration_seconds`: 900–3600, default 3600 (AWS allows up to the role's maximum; the keys are used for at
  most an hour anyway).
- `subject_token`: 24f's `file` or `spiffe` object with its rules (a `spiffe` subject needs the `spiffe`
  block).

### 3.2 A mint

The `aws` binding is a mode of the existing minting provider (`oauthProvider`): one mint at a time, the cache,
the one-hour cap, too-short, back-off (1 s doubling to 60 s), `Available()`, `Rejected()`, redaction and
`Values()` apply unchanged.

1. **Subject token**: 24f's `subjectToken` (classes `assertion_*`, `spiffe_*`).
2. **STS**: `POST sts_endpoint`, `Content-Type: application/x-www-form-urlencoded`, `Accept: text/xml`, the
   form of §2 with `DurationSeconds`; no client authentication; 10 s timeout, no redirects, at most 64 KiB
   read. HTTP 200 required (else `http_<code>`); a transport error is `transport`.
3. **The reply**: XML as in §2 (namespace ignored). `AccessKeyId` `^[A-Z0-9]{16,128}$`; `SecretAccessKey` and
   `SessionToken` printable ASCII without spaces, 1–1024 and 1–8192 bytes; `Expiration` RFC 3339, after the
   request started and at most 12 h later. Anything else is `sts_invalid`.
4. **The credential** is a `Secret` whose value is the secret access key and which carries the key id, the
   session token, the region and the service. It is used for at most an hour after the request, never for a
   call it could expire during; the secret key and session token are redacted until the real expiry plus
   24 h and kept in `Values()` until then (held and retired alike). The key id is not a secret: the mint log
   names it (`access_key_id`) so an operator can match CloudTrail, with `grant` `aws_web_identity`, the STS
   host and the lifetime.

### 3.3 Signing

- `worker.Secret` gains `Authorize(req *http.Request, body []byte) error`: a Bearer credential sets
  `Authorization: Bearer <value>` as before; an AWS credential signs with the pinned signer, the payload hash
  being the hex SHA-256 of `body` (of nothing for a GET), at the current time. It is called after every other
  header is set, so they are signed. `SignsRequests()` reports an AWS credential.
- The HTTP connector's execute and lookup call `Authorize` instead of setting the header. A signing error
  (never expected) is `Ambiguous` with class `credential_signing` before anything is sent, as other
  request-construction failures are.
- Scrubbing uses every value of the credential sent (`values()`: the secret key and the session token), and
  the connector's external-reference check refuses a reference containing either.
- MCP servers are not signed: the scanner records a scan with an AWS credential as failed with class
  `unsupported_credential` (as it records `no_credential`), and the MCP client refuses one too.

### 3.4 Fake ERP

- **STS** (`Options.AWS`): `POST /aws/sts` with `Action=AssumeRoleWithWebIdentity` and `Version=2011-06-15`.
  `RoleArn` must be the configured role; `RoleSessionName` 2–64 of `[\w+=,.@-]`; `DurationSeconds` absent or
  900–3600; `WebIdentityToken` must verify against a configured subject (24f's `ExchangeSubject`: RS256/ES256
  by `kid`, issuer, exact `sub`, audience). It issues a key id `ASIA` + 16 random base32 characters, a secret
  key derived as base64url(HMAC-SHA256(the ERP's static credential, "aws-secret:" + key id)) (nothing new to
  keep secret) and a random 32-byte session token, and replies with the XML of §2 (`AssumedRoleUser.Arn`
  `arn:aws:sts::<account>:assumed-role/<role name>/<session>`). The durable log keeps the key id, the session
  token's SHA-256, the principal `aws:<assumed-role ARN>` and the expiry; the audit records the subject
  token's SHA-256 and the key id, never a token or key. Errors are XML `ErrorResponse` with 400
  (`InvalidParameterValue`, `MissingParameter`) or 403 (`InvalidIdentityToken`, `AccessDenied`).
- **SigV4 on the ERP API**: a request whose `Authorization` starts with `AWS4-HMAC-SHA256 ` is verified by a
  wrapper that reads the body (at most 64 KiB) and restores it: the key id is known and unexpired, the
  `X-Amz-Security-Token` hashes to its session token, the credential scope's region and service are the
  configured ones, `X-Amz-Date` is within 5 minutes, and re-signing a request rebuilt from exactly the signed
  headers, the host, the content length and the body with the derived secret key at `X-Amz-Date` gives the
  same `Authorization` (constant-time). The principal is `aws:<assumed-role ARN>`; the audit records the key
  id (`aws_access_key_id`). `aws:` principals are privileged like the others.
- Env: `EACP_FAKEERP_AWS_ROLE_ARN`, `EACP_FAKEERP_AWS_REGION`, `EACP_FAKEERP_AWS_SERVICE` (all or none) and
  subjects `EACP_FAKEERP_AWS_K8S_{ISSUER,AUDIENCE,SUBJECT,JWKS_FILE}` and
  `EACP_FAKEERP_AWS_SPIFFE_{ISSUER,AUDIENCE,SUBJECT,BUNDLE_FILE}` (each all or none); the role needs a
  subject and a subject needs the role. Key lifetime is `DurationSeconds` (default 3600).

### 3.5 Kubernetes and the demo

- `deployments/k8s/connector-secrets.aws.json` binds tenant `…00aa` (demo "Soylent"): `fakeerp-aws` (file
  subject: the projected token, audience `fakeerp`; session `eacp-worker-k8s`) and `fakeerp-aws-spiffe`
  (SPIFFE subject, audience `sts.amazonaws.com`; session `eacp-worker-spiffe`), both role
  `arn:aws:iam::000000000000:role/eacp-erp`, region `us-east-1`, service `execute-api`, STS
  `http://fakeerp:8090/aws/sts`. `scripts/k8s-e2e.sh` merges it; `fakes.yaml` configures Fake ERP.
- `TestAWSDemo` (Kubernetes only): tenant, cast, policy; a purchase through each binding as
  `aws:arn:aws:sts::000000000000:assumed-role/eacp-erp/eacp-worker-k8s` and `…/eacp-worker-spiffe`, one PO
  each, every execute signed (audited key id); no subject token, secret key or session token in responses,
  logs or a database dump (secret keys are re-derived from the audited key ids; session tokens found by
  hashing 43-character windows).
- Compose is unchanged.

### 3.6 The 24f minors

1. The subject token is acquired after client authentication, just before the request is built, so its 10 s
   margin covers the request.
2. `%` is removed from the impersonation email class (worker and Fake ERP).
3. Tests for `client_secret_vault`, `private_key_jwt` and `client_assertion_spiffe` inside an exchange, and for
   a SPIFFE subject issued with less than 10 s left.
4. The too-short log carries the mint attributes (grant, impersonated, hosts).
5. Fake ERP's exchange audit keeps the authenticated client (`client` field) and records no subject digest when
   `subject_token` is missing.

### 3.7 Proof (tests first)

- `internal/worker`: load validation for every `aws` field, exclusivity, defaults (endpoint by partition,
  session name, duration), no contact at load; the STS request shape for both subject sources; every reply
  refusal; the credential's values, redaction, `Values()` and the one-hour cap; `Authorize` for Bearer and
  SigV4 (signature verified by re-signing, session token header, payload hash, other headers signed); the
  scanner's `unsupported_credential`; the 24f minors; PostgreSQL integration buying through both subject
  sources against Fake ERP with nothing persisted.
- `internal/connector`: execute and lookup sign an AWS credential and send the Bearer otherwise.
- `internal/fakeerp`: STS issuance and every refusal; SigV4 acceptance and refusal of a wrong key id, expired
  key, wrong session token, region, service, stale date, altered body or header, unsigned header added; env
  settings fail closed.
- minikube: `TestAWSDemo` plus every existing demo in a fresh `scripts/k8s-e2e.sh` run.

### 3.8 Documentation

ADR-019 Rev 1.6 (§3f and the surrounding sections), MASTER_PLAN §96 (Phase 24 complete), AGENTS.md, README,
docs/KUBERNETES.md (the STS host in `worker.connectorEgress`), docs/DEMO.md, docs/INVARIANTS.md (the integration
tests join invariant 11), research/REFERENCES.md (the pinned modules).

## 4. Out of scope

SigV4 for MCP servers; SigV4a; presigned URLs; `AssumeRole` chaining; `ProviderId` (OAuth 2.0 access tokens
from Amazon/Facebook/Google); session policies and tags; IAM Roles Anywhere; EC2/ECS metadata credentials;
global STS endpoint by default.

## 5. Unresolved assumptions (conservative choices, recorded in ADR-019)

- Keys are used for at most an hour and the requested duration is capped at 3600 s.
- The regional STS endpoint is the default (the SDK's default; the global endpoint is a single region).
- The access key id is logged and audited: AWS treats it as an identifier, not a secret.
- Signing time is the real clock, never the provider's test clock: the target judges it.
- An MCP server bound to an AWS credential is a recorded, classified scan failure rather than a silent skip.
