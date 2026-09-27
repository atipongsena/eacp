# Phase 24f — OAuth 2.0 Token Exchange Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An `oauth2` binding with `"grant": "token_exchange"` trades the worker's projected token or JWT-SVID for a Bearer access token (RFC 8693), optionally impersonating a GCP service account, with every 24a custody rule intact.

**Architecture:** A `grant` branch inside the existing `oauthProvider` (`internal/worker/oauth2.go`): the mint builds the subject token, the exchange form and optional client authentication, then an optional impersonation hop (new `internal/worker/exchange.go`); the result feeds 24a's cache unchanged. Fake ERP gains an exchange grant and an impersonation endpoint; minikube proves both subject sources.

**Tech Stack:** Go 1.27 standard library (net/http, encoding/json), existing `internal/worker`, `internal/fakeerp`, `internal/spiffetest`, `internal/jwttest`; Helm/minikube for the demo.

**Spec:** `docs/superpowers/specs/2026-09-27-token-exchange-design.md`

## Global Constraints

- Every rule in AGENTS.md applies: tests first, `-race`, never weaken a test or fail-closed behaviour, never log/store/journal a secret, only the worker holds connector secrets, commit as the user without a Co-Authored-By trailer.
- `grant` ∈ {absent, `client_credentials`, `token_exchange`}; `requested_token_type` is always `urn:ietf:params:oauth:token-type:access_token`.
- `subject_token_type` ∈ {`urn:ietf:params:oauth:token-type:jwt` (default), `…:id_token`, `…:id-token`}.
- `audience` 1–1024 printable ASCII without spaces; impersonation `scope` 1–32 scope tokens; `lifetime_seconds` 300–3600 (default 3600).
- Impersonation path exactly `/v1/projects/-/serviceAccounts/<email>:generateAccessToken`; `https` only, `http` in development and test.
- New failure classes: `wrong_token_type`, `impersonation_http_<code>`, `impersonation_invalid`; subject classes reuse `assertion_*` and `spiffe_*`.
- Subject, federated and final tokens: redacted until expiry + 24 h; in `Values()` while they live; never logged.
- Demo tenant `00000000-0000-4000-8000-0000000000a9`, slug `tyrell`; bindings `fakeerp-sts`, `fakeerp-sts-sa`; STS audience `fakeerp-sts`; account `eacp-erp@eacp-demo.iam.gserviceaccount.com`.

## Review Focus

1. **A subject token that expires between read and exchange** (a projected token near rotation): refused with `assertion_expired` before any request; covered by Task 2 `TestTheSubjectTokenMustOutliveTheExchange`.
2. **An STS that returns a JWT or refresh token as `issued_token_type`, or omits it**: `wrong_token_type`, never sent as a Bearer; Task 2 `TestEveryInvalidExchangeResponseIsRefused`.
3. **An impersonation `expireTime` in the past, unparsable, or days ahead**: `impersonation_invalid`; Task 3 `TestEveryInvalidImpersonationResponseIsRefused`.
4. **Where each token goes**: the subject token only to the STS, the federated token only to the impersonation host, never in the connector call; Task 3 `TestEachTokenGoesOnlyToItsHop`.
5. **A client-credentials entry with the new fields, or an exchange entry with a secret but no client_id**: rejected at load; Task 1 `TestInvalidTokenExchangeEntriesRejectTheWholeFile`.

---

### Task 1: Load and validate token-exchange entries

**Files:** Modify `internal/worker/oauth2.go` (entry fields, `newOAuthProvider`); create `internal/worker/exchange.go` (types and validation); test `internal/worker/exchange_test.go` (package `worker_test`).

**Interfaces:**
- Produces: `oauthEntry` fields `Grant string` (`grant`), `SubjectToken *subjectTokenEntry` (`subject_token`: `File *string` `file`, `SPIFFE *spiffeAudience` `spiffe`), `SubjectTokenType string` (`subject_token_type`), `Audience string` (`audience`), `Impersonate *impersonateEntry` (`impersonate`: `URL string`, `Scope []string`, `LifetimeSeconds int` `lifetime_seconds`). `oauthProvider` gains `exchange *exchangeConfig{subjectFile string; subjectSPIFFE string; subjectType, audience string; impersonate *impersonation{url string; scope []string; lifetime time.Duration}}` (nil for client credentials) and `publicClient bool` (client_id in the form, no secret).

- [ ] **Step 1: Write `TestATokenExchangeEntryLoads`** — file subject + audience + impersonate (URL on `http://127.0.0.1:<port>` with `AllowPlainTokenURL`), no client auth → loads; a public-client variant (`client_id` only) and a `client_secret` variant load; a `spiffe` subject variant loads with a `spiffe` block (fake agent from `spiffetest`) and never contacts it (`Fetches == 0`).
- [ ] **Step 2: Write `TestInvalidTokenExchangeEntriesRejectTheWholeFile`** — a table, each `LoadSecrets` error, none echoing a value: unknown `grant`; `subject_token` / `audience` / `subject_token_type` / `impersonate` without `token_exchange`; exchange without `subject_token`; both `file` and `spiffe`; neither; `spiffe` subject without the `spiffe` block; bad `subject_token_type`; `audience` with a space or 1025 chars; client secret without `client_id`; two client-auth forms; impersonate URL with query, user info, wrong path, missing `@`, `http` without dev; empty/33 scopes; a scope with a space; lifetime 299 or 3601; `client_credentials` still requiring a form.
- [ ] **Step 3:** `go test -race -count=1 ./internal/worker -run 'TokenExchangeEntr'` → FAIL (unknown fields accepted / fields missing).
- [ ] **Step 4: Implement** the fields and validation (`validateExchange(e oauthEntry, c loadConfig) (*exchangeConfig, error)` in `exchange.go`; `newOAuthProvider` makes client auth optional only for `token_exchange`). Until Task 2, a token-exchange mint fails with class `exchange_unimplemented` (fail closed).
- [ ] **Step 5:** same command → PASS; `go test -race -count=1 ./internal/worker -run 'OAuth|Assertion|SPIFFE|PrivateKey|Vault'` → PASS (no regression).
- [ ] **Step 6: Commit** `feat(worker): token_exchange entries - subject token, audience and impersonation in the secrets file`.

### Task 2: The exchange request and response

**Files:** Modify `internal/worker/oauth2.go` (`request` branches on `p.exchange`), `internal/worker/exchange.go` (`exchangeForm`, response check); test `internal/worker/exchange_test.go`.

**Interfaces:**
- Consumes: Task 1's `exchangeConfig`, 24e's `spiffeClient.svid`, 24b's `readAssertion`.
- Produces: `(*oauthProvider).subjectToken(ctx) (Secret, time.Time, string)`; the exchange response decoder returns 24a's `(Secret, expiry, realExpiry, lifetime, class)`; `p.subject Secret`, `p.subjectExp time.Time` kept for `live()`.

- [ ] **Step 1: Write the tests** (an `sts` helper like `idp` that records the form, Basic auth and path):
  - `TestAnExchangeSendsTheSubjectToken` — form has `grant_type=urn:ietf:params:oauth:grant-type:token-exchange`, `subject_token` = file contents, `subject_token_type` default jwt, `requested_token_type=…:access_token`, `audience`, `scope`; no `Authorization`, no `client_id`; the returned credential is the STS's `access_token`.
  - `TestExchangeClientAuthentication` — public client: `client_id` in form, no Basic; `client_secret`: Basic with url-escaped id/secret; `client_assertion_file`: `client_assertion` + type + `client_id`, no Basic.
  - `TestAnSVIDSubjectIsFetchedPerMint` — `spiffe` subject: `subject_token` is the agent's SVID for the audience; a second mint (after the token is dropped) fetches again.
  - `TestTheSubjectTokenMustOutliveTheExchange` — file JWT with 5 s left → `ErrCredentialUnavailable`, class `assertion_expired` logged, STS never called.
  - `TestEveryInvalidExchangeResponseIsRefused` — `issued_token_type` missing, `…:jwt`, `…:refresh_token` → `wrong_token_type`; plus 24a's refusals (non-200, not Bearer, missing `expires_in`); each backs off and withholds the binding.
  - `TestExchangeTokensAreRedactedAndScrubbed` — subject and access token in the logger's redaction set and in `Values()` while they live.
- [ ] **Step 2:** `go test -race -count=1 ./internal/worker -run 'Exchange|SVIDSubject|SubjectToken'` → FAIL (`exchange_unimplemented`).
- [ ] **Step 3: Implement** the subject read (file: `readAssertion(path, now)`; SPIFFE: `svid(ctx, aud, tokenRequestTimeout)` and the 10 s check), the form, the client-auth branches (Basic only when a secret is configured; assertion forms unchanged; `client_id` alone for a public client), and the `issued_token_type` check after 24a's checks; redact the subject until `exp+24h`.
- [ ] **Step 4:** same command → PASS; full `go test -race -count=1 ./internal/worker -run 'OAuth|Assertion|SPIFFE|PrivateKey|Vault|Exchange'` → PASS.
- [ ] **Step 5: Commit** `feat(worker): token_exchange mints - RFC 8693 request, optional client authentication, issued_token_type`.

### Task 3: GCP service-account impersonation

**Files:** Modify `internal/worker/exchange.go` (`(*oauthProvider).impersonate(ctx, federated Secret, federatedExp time.Time) (Secret, time.Time, time.Time, time.Duration, string)`), `oauth2.go` (call it after a successful exchange; keep `p.federated`, `p.federatedExp` for `live()`); test `internal/worker/impersonate_test.go`.

- [ ] **Step 1: Write the tests** (one httptest server with `/sts` and `/v1/projects/-/serviceAccounts/…:generateAccessToken` routes):
  - `TestImpersonationTradesTheFederatedToken` — second request is POST JSON `{"scope":[...],"lifetime":"3600s"}` with `Authorization: Bearer <STS token>`; the credential is `accessToken`; realExpiry = `expireTime`.
  - `TestEachTokenGoesOnlyToItsHop` — the STS request carries no Bearer; the impersonation request carries no `subject_token`; the returned credential is neither the subject nor the federated token.
  - `TestEveryInvalidImpersonationResponseIsRefused` — 403 → `impersonation_http_403`; non-JSON, empty/space `accessToken`, 8193-byte token, `expireTime` unparsable, in the past, > 86 400 s ahead → `impersonation_invalid`; server closed → `transport`; each backs off.
  - `TestAnImpersonatedTokenIsUsedForAtMostAnHour` — an `expireTime` 12 h ahead is accepted, used until +1 h (a 61-minute call is too short) and kept in `Values()` until +12 h.
  - `TestTheImpersonationEndpointMayNotRedirect` — a 307 is `impersonation_http_307`, the target never hit.
  - `TestTheFederatedTokenIsRedactedAndScrubbed` — in the redaction set, in `Values()` until its expiry, not after.
- [ ] **Step 2:** `go test -race -count=1 ./internal/worker -run 'Impersonat|EachTokenGoesOnlyToItsHop|FederatedToken'` → FAIL.
- [ ] **Step 3: Implement** the hop with `p.client` (no redirects, 10 s), a 64 KiB limit, `time.Parse(time.RFC3339, …)`, lifetime `min(expire-start, maxTokenUse)`.
- [ ] **Step 4:** same command → PASS; `go test -race -count=1 ./internal/worker -run 'OAuth|Exchange|Impersonat'` → PASS.
- [ ] **Step 5: Commit** `feat(worker): impersonate a GCP service account with the federated token`.

### Task 4: Fake ERP token exchange and impersonation

**Files:** Create `internal/fakeerp/exchange.go`; modify `internal/fakeerp/erp.go` (Options, routing in `issue`, `principal`/`privileged`, audit `SubjectSHA256` json `subject_sha256`), `cmd/fakeerp/main.go` (env); tests `internal/fakeerp/exchange_test.go`, `cmd/fakeerp/main_test.go`.

**Interfaces:**
- Produces: `fakeerp.TokenExchange{Audience string; Subjects []ExchangeSubject}`, `fakeerp.ExchangeSubject{Issuer, Audience, Subject string; Keys []PublicKey}`, `fakeerp.Impersonation{Accounts []string}`; `Options.Exchange *TokenExchange`, `Options.Impersonation *Impersonation`. Principals `sts:<sub>`, `sa:<email>`.

- [ ] **Step 1: Write the tests**: `TestAnExchangeIssuesATokenForTheSubject` (RS256 k8s-style and ES256 SVID-style subjects; response has `issued_token_type`; audit `subject_sha256`; the token executes as `sts:<sub>`), `TestTheExchangeRefusesBadRequests` (wrong audience, subject type, requested type, issuer, sub, kid, signature, expired, bad Basic credential → 400/401, no token), `TestImpersonationIssuesAServiceAccountToken` (with an `sts:` token → `accessToken`, `expireTime` = now+lifetime, executes as `sa:<email>`), `TestImpersonationRefusesBadRequests` (no/static/`oauth:` token, unknown account, 0/33 scopes, lifetime `0s`/`3601s`/`1h`), `TestExchangeOptionsFailClosed`; in `cmd/fakeerp`: `TestExchangeSettingsComeTogether`.
- [ ] **Step 2:** `go test -race -count=1 ./internal/fakeerp ./cmd/fakeerp` → FAIL.
- [ ] **Step 3: Implement** (reuse `verifySVID` for subjects; the exchange branch in `issue` before the client-credentials grant check; route `POST /v1/projects/-/serviceAccounts/{account}` splitting `:generateAccessToken`; tokens in `e.tokens` with their principal).
- [ ] **Step 4:** same command → PASS.
- [ ] **Step 5: Commit** `feat(fakeerp): RFC 8693 token exchange and a generateAccessToken endpoint`.

### Task 5: The worker buys through exchanged tokens

**Files:** Create `internal/worker/exchange_integration_test.go` (package `worker_test`).

**Interfaces:** Consumes Task 4's Fake ERP options, 24b/24e helpers (`newJITFile`, `assertNotPersisted`, `newSVIDAgent`).

- [ ] **Step 1: Write** `TestTheWorkerExecutesWithAnExchangedToken` (file subject signed by a `jwttest.Signer` the Fake ERP trusts; execute principal `sts:<sub>`; subject and token not persisted) and `TestTheWorkerExecutesWithAnImpersonatedToken` (SPIFFE subject + impersonation; principal `sa:<email>`; SVID, federated and final tokens not persisted).
- [ ] **Step 2:** `EACP_TEST_ADMIN_DSN=… go test -race -count=1 ./internal/worker -run 'ExchangedToken|ImpersonatedToken'` → PASS; prove each can fail by mutating (e.g. skip the impersonation hop → principal assertion fails), then revert.
- [ ] **Step 3: Commit** `test(worker): the worker buys with exchanged and impersonated tokens`.

### Task 6: The exchange on the e2e cluster

**Files:** Create `deployments/k8s/connector-secrets.exchange.json`; modify `deployments/k8s/dev/fakes.yaml` (STS and impersonation env; the k8s subject uses the existing `issuer` and `jwks.json`), `scripts/k8s-e2e.sh` (merge the exchange bindings; `TestTokenExchangeDemo` in the default TESTS).

- [ ] **Step 1:** Write the files; `EACP_HELM_REQUIRED=1 go test ./test/helm` (PATH with `.tools`) → PASS; `bash -n scripts/k8s-e2e.sh`.
- [ ] **Step 2: Commit** `feat(deploy): token-exchange bindings and Fake ERP STS on the e2e cluster`.

### Task 7: `TestTokenExchangeDemo`

**Files:** Create `test/demo/exchange_test.go`; modify `test/demo/demo_test.go` (`SubjectSHA256` in `erpEntry`).

- [ ] **Step 1: Write** the demo (k8s only): X0 tenant/cast/policy; X1 `erp` (`fakeerp-sts`) and `erp-oauth` (`fakeerp-sts-sa`) purchases → principals `sts:system:serviceaccount:eacp:eacp-worker` and `sa:eacp-erp@eacp-demo.iam.gserviceaccount.com`, exchanges audited with `subject_sha256`; X2 leak scan: `containsIssued` for every issued token, `containsAssertion` for subject digests and JWTs naming either subject.
- [ ] **Step 2:** on a cluster from Task 6 (`KEEP=1 TESTS=NONE`), `EACP_DEMO=1 EACP_DEMO_PLATFORM=k8s … go test -run TestTokenExchangeDemo ./test/demo` → PASS.
- [ ] **Step 3: Commit** `test(demo): the token-exchange demo on Kubernetes`.

### Task 8: Documentation and verification

**Files:** ADR-019 (Rev 1.5 §3e, §2, §4, §5, §6, §7, consequences, assumptions), MASTER_PLAN §96, AGENTS.md, README, docs/KUBERNETES.md, docs/DEMO.md, docs/INVARIANTS.md (Task 5 tests join invariant 11), research/REFERENCES.md.

- [ ] **Step 1:** Write the docs from the spec and the code as built.
- [ ] **Step 2:** `go vet ./... && EACP_TEST_ADMIN_DSN=… go test -race -count=1 -timeout 30m ./...` → all ok; Helm tests; node UI tests; `go test ./test/invariants`.
- [ ] **Step 3:** compose `EACP_COMPOSE_TEST=1 go test ./test/security` and `DEMO=J scripts/demo.sh` → ok (compose unchanged).
- [ ] **Step 4:** a fresh `KEEP=0 scripts/k8s-e2e.sh` (profile deleted first) → all demos PASS.
- [ ] **Step 5: Commit** `docs: ADR-019 Rev 1.5 - token exchange; Phase 24f delivered`.
- [ ] **Step 6:** Final whole-branch review (one reviewer, most capable model), one fix pass, suite green.
