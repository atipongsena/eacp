# Phase 24b — Workload Identity Federation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A JIT connector binding mints its access tokens by presenting a platform-issued JWT (RFC 7523 client
assertion) instead of a client secret, proven end to end on Kubernetes with the cluster's real issuer.

**Architecture:** The 24a `oauthProvider` gains a third client-authentication source, `client_assertion_file`,
re-read at every mint. Tokens may live up to 24 h but are used for at most 1 h. Fake ERP becomes a federated
relying party (RS256 against a JWKS). The chart gives the worker its own ServiceAccount and a projected token.

**Tech Stack:** Go 1.27 standard library (crypto/rsa, crypto/sha256, encoding/base64), Helm v4.3.0, minikube.

**Spec:** `docs/superpowers/specs/2026-09-26-workload-identity-federation-design.md`

## Global Constraints

- No new Go dependency; no OAuth or JWT library.
- The worker never verifies an assertion's signature; it parses only `exp`.
- Assertion file: 1 byte–16 KiB after trimming trailing `\r\n`; compact JWS (3 non-empty base64url segments,
  no padding, header and payload JSON objects); numeric `exp`.
- Mint failure classes: `assertion_unreadable`, `assertion_invalid`, `assertion_expired` (under 10 s left).
- jwt-bearer URN: `urn:ietf:params:oauth:client-assertion-type:jwt-bearer`.
- `expires_in` accepted 1–86 400; usable lifetime `min(expires_in, 3600)`; redaction and `Values()` until the
  real expiry (+ 24 h for redaction).
- Fake ERP accepts only `RS256`; skew 30 s for `exp`, `nbf`, `iat`.
- Chart: worker SA `<release>-worker`; projected token at `/run/secrets/eacp-identity/token`;
  `expirationSeconds` integer 600–86 400, default 3600; audience required when enabled, no whitespace, ≤ 256.
- Demo: tenant `00000000-0000-4000-8000-0000000000a5`, `secret_ref` `fakeerp-wif`, client id `eacp-worker-wif`,
  audience `fakeerp`, subject `system:serviceaccount:eacp:eacp-worker`.
- Errors never repeat a value from the secrets file. Commits carry no AI attribution.

## Review Focus

1. An assertion file replaced atomically by the kubelet (symlink swap `..data`) between two mints — the next mint
   must send the new content (Task 2 rotation test writes a new file in place; the read is `os.ReadFile` each time).
2. An assertion file with a trailing newline or CRLF — trimmed, not sent with it (Task 2 test writes `jwt+"\r\n"`).
3. An IdP returning `expires_in` 3599 with a call budget of 3600 s + skew — `ErrCredentialTooShort`, no back-off
   (Task 1 test with `validFor` above the capped lifetime).
4. A JWKS with an EC key beside RSA keys, or a JWT whose `aud` is a string rather than an array — the EC key is
   ignored and a string `aud` is accepted (Task 3 tests).
5. The helm chart rendered with workload identity disabled — no projected token and no identity mount, but still
   the worker's own ServiceAccount (Task 5 test).

---

### Task 1: Token lifetime — accept up to 24 h, use at most 1 h

**Files:**
- Modify: `internal/worker/oauth2.go` (`maxTokenLifetime`, `request`, `credential`, `live`, provider fields)
- Test: `internal/worker/oauth2_test.go`

**Interfaces:**
- Produces: `oauthProvider` keeps `expiry` (usable, capped) and `realExpiry` (for `live()` and redaction).
  `request` returns `(tok Secret, usable time.Time, real time.Time, lifetime time.Duration, class string)`
  where `lifetime = min(expires_in, 3600 s)`.

- [ ] **Step 1: Write the failing tests** in `oauth2_test.go`:
  - change the `"too long lived"` case of `TestEveryInvalidTokenResponseIsRefused` to `expires_in: 86401`;
  - `TestALongLivedTokenIsUsedForAtMostAnHour`: IdP answers `expires_in: 5400`; `Credential(validFor: time.Minute)`
    succeeds (1 mint); after `c.add(58*time.Minute)` a second call reuses it (1 mint); after
    `c.add(2*time.Minute)` (60 min from mint) a call mints again (2 mints). The first token stays in
    `s.Values()` at 60 min (its real expiry is 90 min) and is gone after `c.add(31*time.Minute)`.
  - `TestTheCapDecidesWhetherATokenIsTooShort`: `expires_in: 5400`, `Credential(validFor: 3700*time.Second)`
    returns `ErrCredentialTooShort` and `s.Available()` still lists the binding; a second call with the same
    `validFor` makes no new request.
  - `TestALongLivedTokenIsRedactedUntilItsRealExpiry`: with `WithRedaction(set)`, the redaction set holds the
    token (check through `set.Values()`); the `until` itself is covered by logging's tests.

- [ ] **Step 2: Run** `go test ./internal/worker -run 'TestEveryInvalidTokenResponseIsRefused|LongLived|TheCapDecides' -race`
  Expected: FAIL (5400 refused as `invalid_expires_in`).

- [ ] **Step 3: Implement.** `maxTokenLifetime = 86400`, new `const maxTokenUse = time.Hour`. In `request`,
  `real = start + expires_in`, `lifetime = min(expires_in, maxTokenUse)`, `usable = start + lifetime`.
  `credential` stores `p.expiry = usable`, `p.realExpiry = real`, `p.lifetime = lifetime`; `redactUntil(tok, real)`;
  `live()` includes the token while `now < realExpiry`; `rejected` clears both expiries. The mint log's
  `expires_in` shows the usable remaining time.

- [ ] **Step 4: Run** the Step 2 command, then `go test ./internal/worker -run 'OAuth|Token|Mint|Credential' -race`.
  Expected: PASS.

- [ ] **Step 5: Commit** `feat(worker): accept tokens living up to a day but use each for at most an hour`

### Task 2: The worker authenticates with a client assertion file

**Files:**
- Create: `internal/jwttest/jwttest.go` (test helper package: RSA signer and JWKS)
- Create: `internal/worker/assertion.go` (reading and checking an assertion)
- Modify: `internal/worker/oauth2.go` (entry field, validation, request form, per-mint read, `live`)
- Test: `internal/worker/federation_test.go`, `internal/worker/secrets_test.go`

**Interfaces:**
- Produces (`internal/jwttest`, imported only by tests):
  - `type Signer struct{ Key *rsa.PrivateKey; KID string }`
  - `func New(t testing.TB) *Signer` (2048-bit key, KID `"test-key"`)
  - `func (s *Signer) Sign(claims map[string]any) string` — RS256 compact JWS, header `{"alg":"RS256","kid":KID,"typ":"JWT"}`
  - `func (s *Signer) SignWith(header, claims map[string]any) string` — any header, signed RS256 (for bad-`alg` tests)
  - `func (s *Signer) JWKS() []byte` — `{"keys":[{"kty":"RSA","kid":…,"use":"sig","alg":"RS256","n":…,"e":…}]}`
- Produces (`internal/worker`):
  - `oauthEntry.ClientAssertionFile *string` (`json:"client_assertion_file"`)
  - `func readAssertion(path string, now time.Time) (Secret, time.Time, string)` — the assertion, its `exp`,
    or a failure class (`assertion_unreadable` | `assertion_invalid` | `assertion_expired`, the last when
    `exp` is less than `tokenRequestTimeout` after `now`); `const maxAssertion = 16 << 10`.

- [ ] **Step 1: Write `internal/jwttest`** (it is a helper, not product code; no test of its own beyond use).

- [ ] **Step 2: Write the failing tests** in `federation_test.go` (package `worker_test`, reusing `newIDP`,
  `clock`, `secretsFile`, `tenant`, `canary`, `erpEndpoint`); helper
  `assertionStore(t, tokenURL string, c *clock, file string, opts ...worker.LoadOption) *worker.SecretStore`
  with `"client_id":"eacp-wif"` and `"client_assertion_file":file`; helper `writeAssertion(t, path, jwt string)`.
  - `TestAnAssertionAuthenticatesTheTokenRequest`: form has `grant_type=client_credentials`,
    `client_id=eacp-wif`, `client_assertion_type` = the URN, `client_assertion` = the file's JWT, `scope` when
    set; `p.last()` auth is `["",""]` (no Basic auth).
  - `TestEveryMintReadsTheCurrentAssertion`: mint, overwrite the file with a second JWT (`writeAssertion`),
    `s.Rejected` the token, mint again; the second request carries the second JWT.
  - `TestTheAssertionIsTrimmed`: file content `jwt+"\r\n"` → sent value equals `jwt`.
  - `TestAnUnusableAssertionFailsTheMintAndBacksOff`, subtests `unreadable` (file removed after load), `invalid`
    (content `"not.a.jwt"` written after load), `expired` (`exp` = now + 5 s): `Credential` returns
    `ErrCredentialUnavailable`, `p.mints.Load() == 0`, `len(s.Available()) == 0`; the store's log (via
    `WithLogger` on a buffer) contains `"class":"assertion_<kind>"`.
  - `TestAssertionsAreRedactedAndScrubbed`: with `WithRedaction(set)`, after a mint `set.Values()` and
    `s.Values()` contain the JWT; after `c.add` past its `exp`, `s.Values()` no longer does.
  - In `secrets_test.go` `TestInvalidOAuthEntriesRejectTheWholeFile`, add cases: `"secret and assertion"`
    (`client_secret` + `client_assertion_file`), `"unreadable assertion"` (`/absent/file`), and cases built from
    temp files: empty, 16 KiB + 1 bytes, `"a.b"`, a JWT whose payload is `[]`, a JWT without `exp`, a JWT whose
    `exp` is a string. Each error must not contain the file's content.
  - Add `TestAnExpiredAssertionIsAcceptedAtLoad`: an assertion with `exp` in the past loads without error.

- [ ] **Step 3: Run** `go test ./internal/worker -run 'Assertion|InvalidOAuthEntries' -race`. Expected: FAIL
  (unknown field `client_assertion_file`).

- [ ] **Step 4: Implement.**
  - `newOAuthProvider`: exactly one of `client_secret`, `client_secret_file`, `client_assertion_file`
    (message `needs exactly one of client_secret, client_secret_file and client_assertion_file`); for an
    assertion call `readAssertion(path, time.Time{})` and refuse on `assertion_unreadable`/`assertion_invalid`
    (never on `assertion_expired`); redact the value read until `exp + redactAfterExpiry`. Keep `clientSecret`
    empty for assertion bindings and do not `AddPermanent("")` (the set ignores empty values already).
  - `readAssertion`: `os.ReadFile`, trim `\r\n`, length check, split on `.` into exactly 3 non-empty parts,
    `base64.RawURLEncoding` decode parts 0 and 1, each must unmarshal into `map[string]json.RawMessage`,
    payload `exp` must parse as a JSON number (`json.Number` → `Int64`, or float without fraction).
  - `request`: with an assertion, call `readAssertion(p.assertionFile, p.now())` first; on a class, return it
    (no request). Form adds `client_id`, `client_assertion_type`, `client_assertion`; no `SetBasicAuth`.
    Redact the assertion until its `exp + 24 h`; remember it (`p.assertion`, `p.assertionExp`) for `live()`.
  - `live()`: client secret only when non-empty; the last assertion while `now < assertionExp`.

- [ ] **Step 5: Run** the Step 3 command, then `go test ./internal/worker -race -run 'OAuth|Token|Mint|Credential|Secret|Assertion'`.
  Expected: PASS.

- [ ] **Step 6: Commit** `feat(worker): authenticate token requests with a platform-issued client assertion (RFC 7523)`

### Task 3: Fake ERP accepts federated client assertions

**Files:**
- Create: `internal/fakeerp/federation.go` (`Federated`, `ParseJWKS`, `verifyAssertion`)
- Modify: `internal/fakeerp/erp.go` (`Options.Federated`, route registration, `issue` dispatch, audit field)
- Modify: `cmd/fakeerp/main.go` (`loadFederated`), `cmd/fakeerp/main_test.go`
- Test: `internal/fakeerp/federation_test.go`

**Interfaces:**
- Consumes: `internal/jwttest` (Task 2).
- Produces:
  - `type Federated struct { ClientID, Issuer, Audience, Subject string; Keys map[string]*rsa.PublicKey }`
  - `Options.Federated *Federated`
  - `func ParseJWKS(raw []byte) (map[string]*rsa.PublicKey, error)` — RSA keys with non-empty `kid`, `n`, `e`;
    other `kty` ignored; error when none.
  - audit field `AssertionSHA256 string \`json:"assertion_sha256,omitempty"\``
  - `func loadFederated(getenv func(string) string) (*fakeerp.Federated, error)` in `cmd/fakeerp`

- [ ] **Step 1: Write the failing tests** (`federation_test.go`, package `fakeerp_test`):
  helper `fedERP(t) (*httptest.Server, *jwttest.Signer)` with `Federated{ClientID:"eacp-wif", Issuer:"https://issuer.test",
  Audience:"fakeerp", Subject:"system:serviceaccount:eacp:eacp-worker", Keys: ParseJWKS(signer.JWKS())}` and no
  secret client; helper `assert(t, srv, form url.Values, basic bool) (int, map[string]any)`; `good(s)` claims
  `{iss, sub, aud:["fakeerp"], exp: now+600, iat: now, nbf: now}`.
  - `TestAFederatedAssertionMintsAToken`: 200, `token_type` Bearer; the token executes (`executeWith` == 200);
    audit shows principal `oauth:eacp-wif`, outcome `token_issued`, `assertion_sha256` = hex SHA-256 of the
    JWT, and the raw audit text does not contain the JWT.
  - `TestAStringAudienceIsAccepted`: `aud: "fakeerp"` → 200.
  - `TestInvalidAssertionsAreRefused` table → 401 `invalid_client`, audit outcome `invalid_client`, no JWT in
    the audit: alg `HS256` (via `SignWith`), alg `none`, unknown `kid`, signature from another `jwttest.New`
    key, `iss` other, `sub` other, `aud` `["other"]`, `exp` now-60, `nbf` now+120, `iat` now+120, wrong
    `client_id`, wrong `client_assertion_type`, malformed JWT `"a.b.c"`.
  - `TestTwoAuthenticationMethodsAreRefused`: Basic auth plus `client_assertion` → 400 `invalid_request`.
  - `TestParseJWKSIgnoresOtherKeysAndNeedsAnRSAKey`: an EC key beside the RSA key → 1 key; only EC → error;
    `{}` → error.
  - `TestFederatedOptionsFailClosed`: empty field in `Federated` or nil `Keys` → `NewWithOptions` error.
  - `cmd/fakeerp` `TestFederatedSettingsComeTogether`: none set → nil; some set → error; all set with a JWKS
    file → a `Federated`; unreadable JWKS file → error.

- [ ] **Step 2: Run** `go test ./internal/fakeerp ./cmd/fakeerp -race`. Expected: FAIL (undefined `Federated`).

- [ ] **Step 3: Implement.** Route `/oauth/token` when `OAuthClientID != "" || Federated != nil`. `issue`:
  `hasBasic := r.Header.Get("Authorization") != ""`; `hasAssertion := r.PostForm.Get("client_assertion") != ""`;
  both → 400 `invalid_request`; assertion → `verifyAssertion(jwt, form, now)`; basic → existing path (only when a
  secret client exists, else 401); neither → 401. `verifyAssertion` checks, in order: `client_assertion_type`,
  `client_id`, 3 segments, header `alg == "RS256"`, known `kid`, `rsa.VerifyPKCS1v15(key, crypto.SHA256,
  sha256(seg0+"."+seg1), sig)`, `iss`, `sub`, `aud` (string or array), `exp > now-30s`, `nbf <= now+30s` if
  present, `iat <= now+30s` if present. The token endpoint's body limit rises from 4 KiB to 32 KiB (a 16 KiB
  assertion, form-encoded, must fit). Env names: `EACP_FAKEERP_OAUTH_FEDERATED_CLIENT_ID`, `_ISSUER`, `_AUDIENCE`,
  `_SUBJECT`, `_JWKS_FILE`.

- [ ] **Step 4: Run** the Step 2 command. Expected: PASS (and 24a's `oauth_test.go` unchanged and green).

- [ ] **Step 5: Commit** `feat(fakeerp): a federated OAuth client that verifies RS256 assertions against a JWKS`

### Task 4: PostgreSQL integration — an action executes through federation

**Files:**
- Modify: `internal/worker/jit_integration_test.go`

**Interfaces:**
- Consumes: Tasks 2 and 3.

- [ ] **Step 1: Write the failing test** `TestTheWorkerExecutesThroughWorkloadIdentityFederation`: an in-process
  Fake ERP with only `Federated{ClientID:"eacp-wif", …}` (JWKS from a `jwttest.Signer`); a connector
  `secret_ref 'erp-wif'`; the secrets file entry uses `client_assertion_file` (a temp file holding a signed JWT
  with `exp` now + 10 min); two actions succeed with `AttemptCount == 1`; every ERP execute has principal
  `oauth:eacp-wif`; the JWT and the minted tokens are absent from `actions`, `action_attempts`,
  `audit_events`, `outbox_events` and the worker log (reuse the queries of `TestTheWorkerExecutesWithAMintedToken`,
  factored into `func (v *jitEnv) assertNotPersisted(values ...string)`). Refactor `newJIT` into
  `newJITWith(t, erpOptions fakeerp.Options, oauthJSON string, timeoutMS int)` so both tests share it.

- [ ] **Step 2: Run** with `EACP_TEST_ADMIN_DSN` set: `go test ./internal/worker -race -run 'WorkloadIdentity|MintedToken'`.
  Expected: the new test passes only after Tasks 2–3 (it is written after them, so confirm it fails when the
  assertion file is replaced by an expired JWT — then restore).

- [ ] **Step 3: Commit** `test(worker): an action executes with a token minted through workload identity federation`

### Task 5: Helm — the worker's own ServiceAccount and a projected token

**Files:**
- Modify: `deployments/helm/eacp/templates/serviceaccount.yaml`, `_helpers.tpl` (`eacp.podSecurity` takes
  `"sa"` as a name), `api.yaml`, `pdp.yaml`, `worker.yaml`, `validate.yaml`, `values.yaml`
- Modify: `deployments/k8s/e2e-values.yaml` (`worker.workloadIdentity: {enabled: true, audience: fakeerp, expirationSeconds: 600}`)
- Test: `test/helm/identity_test.go`, `test/helm/workloads_test.go` (`TestDangerousValuesAreRefused` cases)

- [ ] **Step 1: Write the failing tests** (`identity_test.go`):
  - `TestTheWorkerHasItsOwnServiceAccount`: a ServiceAccount `eacp-worker` exists with
    `automountServiceAccountToken: false`; the worker Deployment uses it; api and pdp use `eacp`.
  - `TestOnlyTheWorkerProjectsAnIdentityToken`: with e2e values, the worker has a projected volume source
    `serviceAccountToken {audience: fakeerp, expirationSeconds: 600, path: token}` mounted read-only at
    `/run/secrets/eacp-identity`; no other workload has a `serviceAccountToken` source.
  - `TestWorkloadIdentityOffProjectsNothing`: `--set worker.workloadIdentity.enabled=false` → no
    `serviceAccountToken` anywhere and no `/run/secrets/eacp-identity` mount; the worker still uses `eacp-worker`.
  - `TestDangerousValuesAreRefused` additions: `worker.workloadIdentity.audience=` → `workloadIdentity.audience`;
    `worker.workloadIdentity.audience=a b` → `workloadIdentity.audience`;
    `worker.workloadIdentity.expirationSeconds=599` and `=86401` and `=1.5` → `workloadIdentity.expirationSeconds`.

- [ ] **Step 2: Run** `EACP_HELM_REQUIRED=1 go test ./test/helm -race`. Expected: FAIL.

- [ ] **Step 3: Implement** the templates and values (default `enabled: false`, `audience: ""`,
  `expirationSeconds: 3600`). Keep `TestSecretsReachOnlyTheirPods` unchanged (a projected token is not a Secret).

- [ ] **Step 4: Run** the Step 2 command and `helm lint --strict` via `TestChartLintsStrictly`. Expected: PASS.

- [ ] **Step 5: Commit** `feat(deploy): the worker gets its own ServiceAccount and an optional projected identity token`

### Task 6: Kubernetes demo with the cluster's issuer

**Files:**
- Create: `deployments/k8s/connector-secrets.federated.json`
- Modify: `deployments/k8s/dev/fakes.yaml` (Fake ERP federated env from ConfigMap `fakeerp-federation`, JWKS mount)
- Modify: `scripts/k8s-e2e.sh` (issuer + JWKS ConfigMap before `k apply -f deployments/k8s/dev/`; merged
  connector-secrets file; default `TESTS` adds `TestFederatedJITDemo`)
- Create: `test/demo/federation_test.go`

- [ ] **Step 1: Write** `TestFederatedJITDemo` (skips with `t.Skip` unless `d.p.name() == "k8s"`): tenant
  `…00a5` (`tenantWithCast("initech", "Initech", …)` as `TestJITDemo`), `d.secretRef = "fakeerp-wif"`; F0
  bootstrap + policy; F1 three purchases, one PO each; F2 every operation principal `oauth:eacp-worker-wif`,
  collect `token_sha256` and `assertion_sha256` of issuances (extend the demo's ERP audit entry struct with
  `AssertionSHA256`); F3 scan responses, `d.p.logs()` and `pg_dump`: `containsIssued` for tokens, plus
  `containsAssertion(text, hashes, subject)` — every match of
  `[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+` whose SHA-256 is an audited assertion, or whose decoded
  payload has `sub` = `system:serviceaccount:eacp:eacp-worker`. Unit-test `containsAssertion` locally
  (`TestContainsAssertionFindsAJWT`).

- [ ] **Step 2: Wire the script and manifests**: `k get --raw /.well-known/openid-configuration` → issuer
  (python json), `k get --raw /openid/v1/jwks` → file; `k -n eacp-deps create configmap fakeerp-federation
  --from-literal=issuer=… --from-file=jwks.json=…`; merge `connector-secrets.dev.json` + the federated file with
  python into `$work/connector-secrets.json` for `eacp-connector-secrets`.

- [ ] **Step 3: Run** `go test ./test/demo -run 'ContainsAssertion|ContainsIssued'`, then
  `bash scripts/k8s-e2e.sh` (full: Slice A, disruption, JIT, federated JIT). Expected: all PASS.

- [ ] **Step 4: Commit** `test(demo): a federated JIT demo on Kubernetes - tokens minted with the cluster's service-account issuer`

### Task 7: Documentation and final verification

**Files:**
- Modify: `docs/adr/ADR-019-credential-custody.md` (Rev 1.1), `docs/adr/README.md`, `docs/MASTER_PLAN.md` §96,
  `AGENTS.md` (status line, the credential rule), `README.md`, `docs/KUBERNETES.md`, `docs/DEMO.md`

- [ ] **Step 1: Write the docs** per spec §3.9.
- [ ] **Step 2: Run** `go vet ./... && go test -race ./...` with `EACP_TEST_ADMIN_DSN` and `EACP_HELM_REQUIRED=1`;
  `docker compose up -d --build` then `EACP_COMPOSE_TEST=1 go test -count=1 ./test/security/` and
  `scripts/demo.sh` with `DEMO=J` (24a unchanged on compose). Expected: all green.
- [ ] **Step 3: Self-review the branch diff** (a fresh reviewer subagent on the whole branch), fix findings, commit.
- [ ] **Step 4: Commit** `docs: ADR-019 Rev 1.1 - workload identity federation; Phase 24b delivered`
