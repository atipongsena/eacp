# Phase 24g — AWS STS and SigV4 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An `aws` binding mints temporary AWS keys through `AssumeRoleWithWebIdentity` and the HTTP connector signs its calls with SigV4; the five 24f minors are fixed.

**Architecture:** The `aws` entry builds the existing minting provider (`oauthProvider`) in a new mode (`p.aws`), reusing 24f's subject token and every provider rule. `worker.Secret` gains AWS keys and `Authorize(req, body)`, which the HTTP connector calls last. Fake ERP gains an STS and a SigV4 verifier built on the same pinned signer.

**Tech Stack:** Go standard library (`encoding/xml`, `net/http`), `github.com/aws/aws-sdk-go-v2` v1.47.1 (`aws`, `aws/signer/v4`) with `github.com/aws/smithy-go` v1.28.1.

**Spec:** `docs/superpowers/specs/2026-09-27-aws-sts-design.md`

## Global Constraints

- Pin `github.com/aws/aws-sdk-go-v2 v1.47.1` only (no `service/sts`, no `config`, no `credentials` module).
- Every provider rule of ADR-019 holds: nothing contacted at load, whole-file rejection, errors without values, 10 s per request, no redirects, 64 KiB responses, back-off 1 s → 60 s, one-hour use cap, redaction until real expiry + 24 h.
- Classes: `http_<code>`, `transport`, `response_unreadable`, `sts_invalid`, the subject token's `assertion_*`/`spiffe_*`; connector `credential_signing`; scanner `unsupported_credential`.
- Log `grant` `aws_web_identity` and `access_key_id`; never the secret key, session token or subject token.
- Fake ERP principal `aws:arn:aws:sts::<account>:assumed-role/<role name>/<session>`; demo tenant `00000000-0000-4000-8000-0000000000aa` (Soylent), role `arn:aws:iam::000000000000:role/eacp-erp`, region `us-east-1`, service `execute-api`, STS `http://fakeerp:8090/aws/sts`.
- Commit as the user only; no Co-Authored-By trailer. Tests with `-race`; PostgreSQL tests with `EACP_TEST_ADMIN_DSN=postgres://postgres:postgres@127.0.0.1:55432/postgres?sslmode=disable`.

## Review Focus

1. **A header set after signing** (the connector's tenant, content type or idempotency key) breaks the signature at the target — `Authorize` runs last; Task 4's connector test verifies every sent header is in `SignedHeaders` and the signature re-verifies.
2. **A lookup (GET, no body)** must sign the empty payload hash — Task 4's connector lookup test verifies it.
3. **An `Expiration` with fractional seconds or without a zone** — fractional accepted, zone-less refused (`sts_invalid`); Task 3.
4. **A session token with `/`, `+`, `=`** (base64) is valid — Task 3 uses one.
5. **The provider's test clock far from real time** must not skew the signature — `Authorize` signs with the real clock; Task 4 test signs from a store whose clock is two hours off and still verifies.

---

### Task 1: The 24f minors

**Files:** Modify `internal/worker/oauth2.go`, `internal/worker/exchange.go`, `internal/fakeerp/exchange.go`, `internal/fakeerp/erp.go`; tests in `internal/worker/exchangemint_test.go`, `internal/worker/exchange_test.go`, `internal/worker/impersonate_test.go`, `internal/fakeerp/exchange_test.go`.

- [ ] **Step 1: Write failing tests**
  - `TestTheSubjectTokenIsReadAfterClientAuthentication`: an exchange with `client_secret_vault` against a fake Vault whose read takes longer than the time left on a subject file (fake clock: subject with 15 s left; the Vault handler advances the store clock by 10 s) → the mint sends a subject read after the Vault read (request made, subject not expired: `assertion_expired` never logged when the file is rotated during the Vault read — write a fresh subject into the file inside the Vault handler and assert the STS received the fresh one).
  - `TestExchangeClientAuthentication` gains cases `client_secret_vault`, `private_key_jwt` (assertion `aud` is the STS URL) and `client_assertion_spiffe`.
  - `TestAShortSPIFFESubjectIsRefused`: the agent issues a subject SVID with 5 s left → `assertion_expired`, no STS request.
  - `TestInvalidTokenExchangeEntriesRejectTheWholeFile` gains `"percent in the account"` (`…/serviceAccounts/a%2Fb@x.y:generateAccessToken`).
  - `TestATooShortLogNamesTheGrant`: an exchange whose token lives 60 s, asked for 2 minutes → the error log has `"grant":"token_exchange"`.
  - Fake ERP `TestAnExchangeAuditKeepsTheClient` (Basic-authenticated exchange → audit `client` `eacp-worker`, principal `sts:…`) and `TestAMissingSubjectIsNotDigested` (no `subject_token` → `subject_sha256` empty); `TestImpersonationRefusesBadRequests` gains a `%` account in `Impersonation.Accounts` rejected by `valid()`.
- [ ] **Step 2:** `go test -race -count=1 ./internal/worker ./internal/fakeerp -run 'Subject|ClientAuthentication|TooShortLog|Invalid|AuditKeeps|MissingSubject|Impersonation'` → the new cases FAIL.
- [ ] **Step 3: Implement**: acquire the subject token after client authentication in `request()` and set `subject_token` then; drop `%` from `impersonationPath` and Fake ERP's `accountPattern`; the too-short log uses `mintAttrs()` plus `lifetime`, `needed`; Fake ERP adds `Client string json:"client,omitempty"` to the audit (the Basic or assertion client of an exchange) and digests only a non-empty subject.
- [ ] **Step 4:** the same command → PASS; `go test -race -count=1 ./internal/worker ./internal/fakeerp` → ok.
- [ ] **Step 5: Commit** `fix: the 24f follow-ups - subject after client auth, no percent in accounts, tests and audit`.

### Task 2: `aws` entries

**Files:** Create `internal/worker/aws.go`, `internal/worker/aws_test.go`; modify `internal/worker/secrets.go` (entry field, exclusivity message, wiring into `e.oauth`), `internal/worker/oauth2.go` (provider fields, shared HTTP client constructor).

**Interfaces — Produces:**
- `type awsEntry struct { RoleARN string "role_arn"; RoleSessionName string "role_session_name"; Region, Service string; STSEndpoint string "sts_endpoint"; DurationSeconds int "duration_seconds"; SubjectToken *subjectTokenEntry "subject_token" }`
- `type awsConfig struct { roleARN, sessionName, region, service string; duration time.Duration }`
- `func newAWSProvider(i int, e awsEntry, b Binding, c loadConfig) (*oauthProvider, error)` — sets `tokenURL` (the STS endpoint), `exchange` (subject only, via a shared `validateSubject(s *subjectTokenEntry, c loadConfig) (*exchangeConfig, error)` extracted from `validateExchange`) and `aws`.
- `func (p *oauthProvider) grant() string` returns `aws_web_identity` when `p.aws != nil`.

- [ ] **Step 1: Write failing tests** `TestAnAWSEntryLoads` (defaults: endpoint `https://sts.eu-west-1.amazonaws.com`, `https://sts.cn-north-1.amazonaws.com.cn` for `aws-cn`, session `eacp-worker`, duration 3600; nothing contacted — a closed port as endpoint) and `TestInvalidAWSEntriesRejectTheWholeFile` (table: missing/bad role ARN, other partition, bad session name, bad region, bad service, 899 and 3601 duration, http endpoint without dev, endpoint with query/user info/uppercase host, no subject, both subject sources, spiffe subject without `spiffe` block, `aws` together with `oauth2` or `value`; each error names no value and is not a JSON decoder error).
- [ ] **Step 2:** `go test -count=1 ./internal/worker -run 'AWSEntr'` → FAIL.
- [ ] **Step 3: Implement** the validation of spec §3.1 with regexps `^arn:(aws|aws-cn|aws-us-gov):iam::[0-9]{12}:role/[\w+=,.@/-]+$` (length 20–2048), `^[\w+=,.@-]{2,64}$`, `^[a-z]{2}(-[a-z]+)+-[0-9]+$` (≤ 32), `^[a-z0-9-]{1,64}$`; endpoint rules as `token_url` (a path allowed).
- [ ] **Step 4:** → PASS; whole package ok.
- [ ] **Step 5: Commit** `feat(worker): aws entries - role, region, service, STS endpoint and subject token`.

### Task 3: The AWS mint

**Files:** Modify `internal/worker/aws.go`, `internal/worker/oauth2.go` (`request()` dispatches to `assumeRole`; `redactUntil` and `live()` cover every value of a credential), `internal/worker/secrets.go` (`Secret` gains `aws *awsKeys`; `func (s Secret) values() []string`); test `internal/worker/awsmint_test.go`.

**Interfaces — Produces:** `type awsKeys struct{ accessKeyID, sessionToken, region, service string }` (the secret key is `Secret.v`); `func (p *oauthProvider) assumeRole(ctx context.Context) (Secret, time.Time, time.Time, time.Duration, string)`.

- [ ] **Step 1: Write failing tests** (a fake STS `httptest` server answering spec §2's XML):
  - `TestAnAssumeRoleSendsTheSubjectToken`: form exactly `Action`, `Version=2011-06-15`, `RoleArn`, `RoleSessionName`, `WebIdentityToken`, `DurationSeconds=3600`; no `Authorization`; the credential's `Reveal()` is the secret key; both subject sources.
  - `TestEveryInvalidSTSReplyIsRefused`: 400 → `http_400`; not XML, wrong root, missing each field, key id `abc`, secret with a space, 8193-byte session token, `Expiration` in the past, 13 h ahead, `2026-09-27T10:00:00` (no zone) → `sts_invalid`; a closed connection → `transport`; a 307 is not followed; each withholds the binding (`Available()` empty).
  - `TestAWSKeysAreUsedForAtMostAnHour`: `Expiration` 12 h ahead → a 61-minute call is `ErrCredentialTooShort`; the values stay in `Values()` after 2 h.
  - `TestAWSKeysAreRedactedAndScrubbed`: the secret key and a session token `IQoJb3JpZ2luX2VjEJr//+=`-style value are in the redaction set and `Values()`; a fractional `Expiration` (`…T10:00:00.123Z`) is accepted; the mint log has `"grant":"aws_web_identity"` and `"access_key_id":"ASIA…"` and neither secret.
  - `TestARejectedAWSCredentialIsDropped`: `Rejected` with the credential → the next call mints again.
- [ ] **Step 2:** `go test -race -count=1 ./internal/worker -run 'AssumeRole|STSReply|AWSKeys|RejectedAWS'` → FAIL.
- [ ] **Step 3: Implement** spec §3.2 steps 1–4: `encoding/xml` into `AssumeRoleWithWebIdentityResponse>AssumeRoleWithWebIdentityResult>Credentials`; `^[A-Z0-9]{16,128}$`; `tokenPattern` with the size limits; `time.Parse(time.RFC3339, …)`; lifetime `min(exp-start, maxTokenUse)`; redact and scrub `values()`.
- [ ] **Step 4:** → PASS; whole package ok.
- [ ] **Step 5: Commit** `feat(worker): AssumeRoleWithWebIdentity mints temporary AWS keys`.

### Task 4: Signing

**Files:** `go.mod`/`go.sum` (`go get github.com/aws/aws-sdk-go-v2@v1.47.1`); create `internal/worker/sigv4.go`, `internal/worker/sigv4_test.go`; modify `internal/connector/http.go` (+ `http_test.go`), `internal/worker/worker.go` and `reconciler.go` (scrub `secret.values()`), `internal/worker/scanner.go` (+ test), `internal/connector/mcp/client.go`.

**Interfaces — Produces:** `func (s Secret) Authorize(req *http.Request, body []byte) error`; `func (s Secret) SignsRequests() bool`; exported test helper in package `worker` for other packages' tests: `func AWSTestSecret(accessKeyID, secretKey, sessionToken, region, service string) Secret` (documented test-only, like `worker.NewSecret` if it exists — reuse the existing constructor pattern).

- [ ] **Step 1: Write failing tests**:
  - `TestABearerSecretAuthorizes` (header exactly `Bearer <v>`).
  - `TestAnAWSSecretSignsSigV4`: re-signing a copy with `v4.NewSigner()` at the sent `X-Amz-Date` gives the same `Authorization`; `X-Amz-Security-Token` is the session token; the credential scope names the region and service; every header set before `Authorize` is in `SignedHeaders`; a store clock two hours off does not change `X-Amz-Date` from real time (±5 s).
  - Connector `TestExecuteSignsAnAWSCredential` and `TestLookupSignsAnAWSCredential` (a test server verifies with the signer; the lookup's payload hash is that of the empty body; `Idempotency-Key` and `X-EACP-Tenant-ID` are signed); `TestAnExternalReferenceWithTheSessionTokenIsRefused`.
  - Scanner `TestAScanWithAnAWSCredentialIsRecordedUnsupported` (class `unsupported_credential`, `Discover` never called); MCP `Discover` refuses a signing secret (`no_credential`).
- [ ] **Step 2:** `go test -race -count=1 ./internal/worker ./internal/connector/... -run 'Authoriz|Signs|SignsAn|Unsupported|SessionToken'` → FAIL (build or assertion).
- [ ] **Step 3: Implement** spec §3.3; `Authorize` computes `hex(sha256(body))` and calls `SignHTTP(req.Context(), aws.Credentials{…}, req, hash, service, region, time.Now().UTC())`.
- [ ] **Step 4:** → PASS; `go test -race -count=1 ./internal/worker ./internal/connector/...` ok.
- [ ] **Step 5: Commit** `feat: SigV4 - Secret.Authorize signs AWS credentials; the HTTP connector signs execute and lookup`.

### Task 5: Fake ERP STS and SigV4

**Files:** Create `internal/fakeerp/aws.go`, `internal/fakeerp/aws_test.go`; modify `internal/fakeerp/erp.go` (options, validation, route `POST /aws/sts`, the SigV4 wrapper, `principal`, `privileged` for `aws:`, audit fields `aws_access_key_id`), `cmd/fakeerp/main.go` (+ `main_test.go` `TestAWSSettingsComeTogether`).

**Interfaces — Produces:** `type AWS struct { RoleARN, Region, Service string; Subjects []ExchangeSubject }`; `Options.AWS *AWS`; `func loadAWS(getenv func(string) string) (*fakeerp.AWS, error)`.

- [ ] **Step 1: Write failing tests** `TestAssumeRoleIssuesKeysForTheSubject` (both subject kinds; XML parses; key id `ASIA` + 16; principal of a signed execute `aws:arn:aws:sts::000000000000:assumed-role/eacp-erp/<session>`; keys survive a restart), `TestAssumeRoleRefusesBadRequests` (wrong action, version, role, session name, duration 899/3601, bad token → XML error codes of spec §3.4), `TestSigV4IsVerified` (accepts a request signed with the worker's `Authorize`; refuses an unknown or expired key id, wrong session token, region, service, a date 6 min old, an altered body, an altered signed header), `TestAWSOptionsFailClosed`.
- [ ] **Step 2:** `go test -race -count=1 ./internal/fakeerp ./cmd/fakeerp -run 'AssumeRole|SigV4|AWS'` → FAIL.
- [ ] **Step 3: Implement** spec §3.4 (secret key derivation with the static credential; durable log entries like tokens).
- [ ] **Step 4:** → PASS; both packages ok.
- [ ] **Step 5: Commit** `feat(fakeerp): AssumeRoleWithWebIdentity and SigV4 verification`.

### Task 6: The worker buys with AWS keys

**Files:** Create `internal/worker/aws_integration_test.go`.

- [ ] **Step 1: Write** `TestTheWorkerExecutesWithAWSKeysFromAFileSubject` and `TestTheWorkerExecutesWithAWSKeysFromAnSVID` (`newJITFile`, `newSVIDAgent`; principal `aws:…/<session>`; the subject, secret key and session token not persisted: collect from `v.secrets.Values()`).
- [ ] **Step 2:** PostgreSQL run → PASS; mutate (`Authorize` sends Bearer) → FAIL; revert.
- [ ] **Step 3: Commit** `test(worker): the worker buys with AWS keys`.

### Task 7: The cluster and `TestAWSDemo`

**Files:** Create `deployments/k8s/connector-secrets.aws.json`, `test/demo/aws_test.go`; modify `deployments/k8s/dev/fakes.yaml`, `scripts/k8s-e2e.sh` (merge; `TestAWSDemo` in the default TESTS), `test/demo/demo_test.go` (`AWSAccessKeyID` in `erpEntry`).

- [ ] **Step 1: Write** the files of spec §3.5; the demo's leak scan re-derives each secret key from its audited key id with the dev static credential (`deployments/docker/secrets`) and hashes 43-character windows for session tokens.
- [ ] **Step 2:** Helm tests; `bash -n`; a `KEEP=1 TESTS=NONE` cluster; `TestAWSDemo` → PASS.
- [ ] **Step 3: Commit** `test(demo): the AWS demo on Kubernetes`.

### Task 8: Documentation and verification

- [ ] **Step 1:** ADR-019 Rev 1.6 §3f and surrounding sections; MASTER_PLAN §96 (Phase 24 complete); AGENTS.md; README; KUBERNETES.md; DEMO.md; INVARIANTS.md; REFERENCES.md (the pinned modules and verified facts).
- [ ] **Step 2:** `go vet ./... && go test -race -count=1 -timeout 30m ./...` with PostgreSQL; Helm; node UI; invariants.
- [ ] **Step 3:** compose security tests and `DEMO=J scripts/demo.sh`.
- [ ] **Step 4:** a fresh `scripts/k8s-e2e.sh` (profile deleted first) → every demo PASS.
- [ ] **Step 5: Commit** `docs: ADR-019 Rev 1.6 - AWS STS and SigV4; Phase 24 complete`.
- [ ] **Step 6:** final whole-branch review (one reviewer, most capable model), one fix pass, suite green.
