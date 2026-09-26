# Phase 24c — private_key_jwt Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A JIT binding authenticates to its token endpoint with an assertion the worker signs with its own
private key (OIDC `private_key_jwt`), demonstrated on compose and Kubernetes.

**Architecture:** A signer (`internal/worker/privatekeyjwt.go`) becomes the second source of 24b's client
assertion inside `oauthProvider.request`. Fake ERP gains a key client with JWKS lookup by `kid`/`x5t#S256`,
strict claim parsing and a durable `jti` replay record. `eacpctl dev-client-key` makes the dev key, certificate
and JWKS.

**Tech Stack:** Go 1.27 standard library (crypto/rsa, crypto/ecdsa, crypto/x509, encoding/pem), docker compose,
Helm/minikube.

**Spec:** `docs/superpowers/specs/2026-09-26-private-key-jwt-design.md`

## Global Constraints

- No new Go dependency.
- `alg` ∈ {RS256, PS256, ES256}; RSA ≥ 2048 bits; ES256 = P-256; key type must match `alg`.
- PEM: exactly one block; `PRIVATE KEY` | `RSA PRIVATE KEY` | `EC PRIVATE KEY`; encrypted refused; ≤ 16 KiB.
- Certificate: one `CERTIFICATE` block, same public key, valid now; header `x5t#S256` = base64url(SHA-256(DER)).
- Assertion: header `alg`, `typ` JWT, optional `kid`, optional `x5t#S256`; claims `iss`=`sub`=client_id,
  `aud`=token_url, `jti` 128 random bits base64url, `iat`=`nbf`=now, `exp`=now+300.
- PS256 salt length = hash length; ES256 signature raw 64-byte R‖S.
- Failure class `assertion_signing`; redaction: key PEM permanent (not in `Values()`), assertion until exp+24 h,
  in `Values()` until exp.
- Fake ERP key client env: `EACP_FAKEERP_OAUTH_KEY_CLIENT_ID`, `_KEY_AUDIENCE`, `_KEY_JWKS_FILE`; `exp` ≤ now+1 h;
  `jti` required, single-use, audited as `assertion_jti` and rebuilt at startup; skew 30 s.
- Demo: tenant `00000000-0000-4000-8000-0000000000a6`, `secret_ref` `fakeerp-pkjwt`, client `eacp-worker-pkjwt`,
  audience `http://fakeerp:8090/oauth/token`, PS256 with a certificate; compose volumes `client_key` (worker
  only, `/run/secrets/eacp-client-key`) and `client_jwks` (Fake ERP only).
- Commits carry no AI attribution.

## Review Focus

1. A PEM file with Windows line endings or a trailing blank line — still one block, accepted (Task 1 test writes CRLF PEM).
2. A PKCS #8 file holding an Ed25519 key — refused as an unsupported key type, not a panic (Task 1 case).
3. Two mints in the same second — distinct `jti`s (Task 1 test mints twice without advancing the clock).
4. A JWKS entry with both `kid` and `x5t#S256` where the assertion carries only `x5t#S256` (the Entra shape) — found (Task 2 test).
5. Fake ERP restarted between two uses of one assertion — the replay is still refused (Task 2 test).

---

### Task 1: The worker signs its client assertion

**Files:**
- Create: `internal/worker/privatekeyjwt.go`
- Modify: `internal/worker/oauth2.go` (`oauthEntry.PrivateKeyJWT`, exactly-one-of-four, `request` uses one assertion step)
- Test: `internal/worker/privatekeyjwt_test.go`, `internal/worker/secrets_test.go`

**Interfaces:**
- Produces:
  - `type privateKeyJWTEntry struct { Alg string; KeyFile, Key, CertificateFile, Certificate *string; KeyID string }`
    (JSON `alg`, `key_file`, `key`, `certificate_file`, `certificate`, `key_id`)
  - `type assertionSigner struct { alg string; key crypto.Signer; kid, x5t string; pem string }`
  - `func newAssertionSigner(e privateKeyJWTEntry) (*assertionSigner, error)` — errors name the field, never a value
  - `func (s *assertionSigner) sign(clientID, audience string, now time.Time) (Secret, time.Time, error)` — the
    assertion and its `exp`
  - `oauthProvider.clientAssertion(now time.Time) (Secret, time.Time, string)` — file (24b) or signer; class on failure

- [ ] **Step 1: Write the failing tests** (`privatekeyjwt_test.go`, package `worker_test`): helpers generate
  keys (`rsa.GenerateKey` 2048, `ecdsa.GenerateKey` P-256), PEM-encode them (PKCS #8 via `x509.MarshalPKCS8PrivateKey`),
  make a self-signed certificate with `x509.CreateCertificate`, and a `pkjwtStore(t, tokenURL, c, pkjwtJSON)`.
  - `TestPrivateKeyJWTSignsEachAlgorithm` (subtests RS256, PS256, ES256): after a mint, the IdP double's form has
    `client_id`, the jwt-bearer type and an assertion; no Basic auth; the test verifies the signature with the
    public key (`rsa.VerifyPKCS1v15`, `rsa.VerifyPSS` with `PSSSaltLengthEqualsHash`, `ecdsa.Verify` on the
    split 32+32 bytes); header `typ` = JWT and `alg` as set; claims `iss` = `sub` = the client id, `aud` = the
    token URL, `nbf` = `iat` = clock now, `exp` = `iat` + 300, `jti` decodes from base64url to 16 bytes.
  - `TestTheHeaderNamesTheKey`: with `key_id` → `kid`; with a certificate → `x5t#S256` = base64url of
    `sha256.Sum256(cert.Raw)`; with neither → no `kid`, no `x5t#S256`.
  - `TestEveryMintHasANewJTI`: two mints without advancing the clock (drop the token with `Rejected` between) →
    different `jti`s and different assertions.
  - `TestAPrivateKeyIsRedactedButNeverAValue`: with `WithRedaction(set)` the key PEM is in `set.Values()` and not
    in `s.Values()`; after a mint the assertion is in both; after `c.add(6*time.Minute)` it leaves `s.Values()`.
  - `TestAnInlineKeyAndACRLFPEMLoad`: `key` inline (PEM with `\r\n` line endings) loads and signs.
  - In `TestInvalidOAuthEntriesRejectTheWholeFile` add `private_key_jwt` cases: unknown `alg` "HS256"; ES256 with
    an RSA key; RS256 with a P-256 key; RSA 1024; an Ed25519 PKCS #8 key; an encrypted PEM (header
    `Proc-Type: 4,ENCRYPTED`); two PEM blocks; `key` and `key_file` together; neither; a certificate of another
    key; an expired certificate; `private_key_jwt` with `client_secret`; `key_id` with a space. No error may
    contain PEM text.

- [ ] **Step 2: Run** `go test ./internal/worker -race -count=1 -run 'PrivateKey|TheHeaderNames|NewJTI|InlineKey|InvalidOAuthEntries'`.
  Expected: FAIL (unknown field `private_key_jwt`).

- [ ] **Step 3: Implement.** Parse PEM with `pem.Decode` after normalising `\r\n`; reject any leftover non-space
  data (a second block); `x509.IsEncryptedPEMBlock` or a `Proc-Type` header → refuse; type-switch the parsed key
  to `*rsa.PrivateKey` / `*ecdsa.PrivateKey` (curve P-256). Sign: SHA-256 digest of `header.payload`;
  RS256 `rsa.SignPKCS1v15`; PS256 `rsa.SignPSS` with `PSSSaltLengthEqualsHash`; ES256 `ecdsa.Sign` → `r`,`s` each
  `FillBytes` into 32 bytes. `jti`: 16 bytes from `crypto/rand`. Header and claims marshalled with
  `encoding/json` from ordered structs. In `request`, replace 24b's inline `readAssertion` block with
  `p.clientAssertion(now)`; the signer path redacts and records the assertion exactly as the file path does.
  `live()` unchanged (it already reports the last assertion until its exp). Add the key PEM to the redaction set
  permanently in `LoadSecrets` (where client secrets are added).

- [ ] **Step 4: Run** the Step 2 command, then `go test ./internal/worker -race -count=1 -run 'OAuth|Token|Mint|Credential|Secret|Assertion|LongLived|TheCap|PrivateKey'`.
  Expected: PASS.

- [ ] **Step 5: Commit** `feat(worker): sign client assertions with the worker's own key (private_key_jwt)`

### Task 2: Fake ERP key client, strict claims and jti replay

**Files:**
- Create: `internal/fakeerp/keyclient.go`
- Modify: `internal/fakeerp/federation.go` (strict decoding shared by both clients), `internal/fakeerp/erp.go`
  (`Options.KeyClient`, dispatch by `client_id`, audit `assertion_jti`, replay set rebuilt in `apply`),
  `cmd/fakeerp/main.go` (`loadKeyClient`), `cmd/fakeerp/main_test.go`
- Test: `internal/fakeerp/keyclient_test.go`, `internal/fakeerp/federation_test.go` (two strictness cases)

**Interfaces:**
- Produces:
  - `type PublicKey struct { KID, X5TS256 string; Key crypto.PublicKey }`
  - `func ParseKeySet(raw []byte) ([]PublicKey, error)` — RSA and EC P-256 keys with a `kid` or `x5t#S256`;
    other keys ignored; none → error
  - `type KeyClient struct { ClientID, Audience string; Keys []PublicKey }`, `Options.KeyClient *KeyClient`
  - audit field `AssertionJTI string \`json:"assertion_jti,omitempty"\``
  - `func loadKeyClient(getenv func(string) string) (*fakeerp.KeyClient, error)`

- [ ] **Step 1: Write the failing tests** (`keyclient_test.go`): a local signer helper (RS256/PS256/ES256 with a
  header map) — do not extend `jwttest`, whose `SignWith` is RS256-only; build `keyERP(t, dir)` over a fixed data
  path so a test can restart it.
  - `TestAKeyClientAssertionMintsAToken` (subtests RS256 by `kid`, PS256 by `x5t#S256` only, ES256 by `kid`):
    200; the token executes; the audit has `assertion_jti` and principal `oauth:eacp-worker-pkjwt`.
  - `TestKeyClientAssertionsAreRefused` table → 401 `invalid_client`: `alg` none, `alg` HS256, ES256 header over an
    RSA key's `kid`, unknown `kid`/thumbprint, a signature by another key, `iss` ≠ `sub`, `aud` other, `exp` past,
    `exp` now+2 h, `nbf` now+2 min, `iat` now+2 min, no `jti`, `exp` as the string `"…"`, a claim spelled `ISS`.
  - `TestAJTIIsSingleUse`: the same assertion twice → 200 then 401; after rebuilding the ERP on the same log
    (restart), a third use → 401; a new assertion → 200.
  - `TestParseKeySet`: RSA+EC keys load with their ids; a key with neither `kid` nor `x5t#S256` is ignored; an
    EC P-384 key is ignored; `{}` → error.
  - In `federation_test.go` add to `TestInvalidAssertionsAreRefused`: `"exp as a string"` and `"ISS instead of iss"`.
  - `cmd/fakeerp` `TestKeyClientSettingsComeTogether` (none → nil; partial → error; all with a JWKS file → client).

- [ ] **Step 2: Run** `go test ./internal/fakeerp ./cmd/fakeerp -race -count=1`. Expected: FAIL (undefined).

- [ ] **Step 3: Implement.** One strict decoder for header and claims: `map[string]json.RawMessage`, exact keys,
  NumericDate = raw value not starting with `"`, parsed with `json.Number.Int64`. Dispatch: an assertion request
  is verified by the client whose `ClientID` equals the form's `client_id` (federated or key). Key-client checks
  in the spec order (§3.3); verify RS256 `rsa.VerifyPKCS1v15`, PS256 `rsa.VerifyPSS(…PSSSaltLengthEqualsHash)`,
  ES256 split R‖S + `ecdsa.Verify`. Replay: `e.jtis map[string]bool` filled in `apply` from issuance audits with
  `AssertionJTI`, checked under `e.mu` before issuing.

- [ ] **Step 4: Run** the Step 2 command. Expected: PASS (24a/24b Fake ERP tests unchanged and green).

- [ ] **Step 5: Commit** `feat(fakeerp): a private_key_jwt client with JWKS lookup, strict claims and single-use jti`

### Task 3: PostgreSQL integration — an action executes with private_key_jwt

**Files:** Modify `internal/worker/jit_integration_test.go`

- [ ] **Step 1: Write** `TestTheWorkerExecutesWithPrivateKeyJWT` with `newJITWith`: Fake ERP `KeyClient`
  (ClientID `eacp-pkjwt`, Audience = the httptest token URL, keys from a generated PS256 key + certificate JWKS);
  the worker entry's `private_key_jwt` uses inline `key` and `certificate`. Because `newJITWith` builds the token
  URL itself, give it the audience through a closure: change its `client string` parameter to
  `client func(tokenURL string) string`, and adjust the two existing callers. Two actions succeed with one
  attempt each as `oauth:eacp-pkjwt`; `v.assertNotPersisted(keyPEM, tokens...)`.
- [ ] **Step 2: Run** with `EACP_TEST_ADMIN_DSN`: `go test ./internal/worker -race -count=1 -run 'PrivateKeyJWT|WorkloadIdentity|MintedToken'`.
  Expected: PASS; then temporarily sign with a key the ERP does not know and confirm it FAILS (restore).
- [ ] **Step 3: Commit** `test(worker): an action executes with a token minted by a private_key_jwt assertion`

### Task 4: eacpctl dev-client-key

**Files:** Create `cmd/eacpctl/clientkey.go`, `cmd/eacpctl/clientkey_test.go`; Modify `cmd/eacpctl/main.go` (usage + dispatch)

**Interfaces:**
- Produces: `func runDevClientKey(args []string, getenv func(string) string, out io.Writer) error`; files
  `<dir>/key.pem` (PKCS #8, 0600), `<dir>/cert.pem`, and `<jwks>` (0644); `--kid` default `eacp-dev-pkjwt`.

- [ ] **Step 1: Write** `TestDevClientKeyOnlyInDevelopment` (refused for "", staging, production),
  `TestDevClientKeyWritesAMatchingSet` (the JWKS key's `n`/`e` equal the certificate's public key, its `x5t#S256`
  equals base64url SHA-256 of the certificate DER, `alg` PS256, `kid` as given; a second run keeps the files
  byte-identical), `TestDevClientKeyRefusesAPartialSet` (only `key.pem` present → error).
- [ ] **Step 2: Run** `go test ./cmd/eacpctl -count=1 -run DevClientKey`. Expected: FAIL.
- [ ] **Step 3: Implement.** RSA 2048; certificate CN `eacp-dev-pkjwt`, one year, `KeyUsageDigitalSignature`.
- [ ] **Step 4: Run** the Step 2 command. Expected: PASS.
- [ ] **Step 5: Commit** `feat(eacpctl): dev-client-key writes a development key, certificate and JWKS`

### Task 5: Compose wiring, security tests and the demo

**Files:**
- Modify: `docker-compose.yml` (one-shot `client-key`, volumes `client_key`, `client_jwks`; worker and fakeerp
  mounts, env, `depends_on`), `deployments/docker/secrets/connector-secrets.dev.json` (tenant `…a6` entry),
  `test/security/network_isolation_test.go` (`"bindings":6`; `TestTheClientKeyIsMountedOnlyIntoTheWorker`)
- Create: `test/demo/privatekeyjwt_test.go`; Modify `test/demo/demo_test.go` (`erpEntry.AssertionJTI`)

- [ ] **Step 1: Write** `TestPrivateKeyJWTDemo` (steps P0–P3 as the spec §3.5; slug `stark`), helper
  `containsSignedAssertion(text, iss string) int` (JWT-shaped matches whose payload `iss` equals it) with a local
  unit test `TestContainsSignedAssertionFindsAJWT`; and the security test (worker mounts `client_key` at
  `/run/secrets/eacp-client-key`; fakeerp mounts `client_jwks` and not `client_key`; api, agent, postgres, fakemcp
  mount neither).
- [ ] **Step 2: Wire compose** per the spec §3.5.
- [ ] **Step 3: Run** `docker compose up -d --build`, `EACP_COMPOSE_TEST=1 go test -count=1 ./test/security/`,
  then `DEMO=J scripts/demo.sh` extended so `DEMO=J` also runs `TestPrivateKeyJWTDemo` (edit `scripts/demo.sh`'s
  J selection). Expected: all PASS.
- [ ] **Step 4: Commit** `test(demo): a private_key_jwt demo on compose - the key never leaves the worker`

### Task 6: Kubernetes wiring and the e2e run

**Files:** Modify `scripts/k8s-e2e.sh` (generate keys with `go run ./cmd/eacpctl dev-client-key`; inline
`key_file`/`certificate_file` under `/run/secrets/eacp-client-key/` in the merged secrets; add `client-jwks.json`
to ConfigMap `fakeerp-federation`; default TESTS adds `TestPrivateKeyJWTDemo`), `deployments/k8s/dev/fakes.yaml`
(key-client env, JWKS item).

- [ ] **Step 1: Wire** as above.
- [ ] **Step 2: Run** `bash scripts/k8s-e2e.sh` (fresh profile). Expected: Slice A, disruption, JIT, federated JIT
  and private_key_jwt demos PASS.
- [ ] **Step 3: Commit** `test(demo): the private_key_jwt demo on Kubernetes with an inline key`

### Task 7: Documentation and final verification

- [ ] **Step 1:** ADR-019 Rev 1.2 (§3b private_key_jwt, Fake ERP key client, proof, assumptions), MASTER_PLAN
  §96, AGENTS.md, README, `docs/KUBERNETES.md`, `docs/DEMO.md`, `docs/INVARIANTS.md` (the integration test).
- [ ] **Step 2:** `go vet ./... && go test -race ./...` (DSN, `EACP_HELM_REQUIRED=1`), run with nothing else loading
  the machine.
- [ ] **Step 3:** Fresh reviewer on the whole branch; fix Critical/Important with RED→GREEN tests.
- [ ] **Step 4: Commit** `docs: ADR-019 Rev 1.2 - private_key_jwt; Phase 24c delivered`
