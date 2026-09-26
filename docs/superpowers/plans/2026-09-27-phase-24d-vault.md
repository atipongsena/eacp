# Phase 24d — Vault KV v2 credential source Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let every worker credential field (static value, OAuth client secret, private_key_jwt key and
certificate) come from HashiCorp Vault KV v2, read with the worker's own Vault login (Kubernetes or AppRole),
refreshed and rotated without a restart.

**Architecture:** A per-worker `vaultClient` (login, token cache, KV v2 reads cached per path) in
`internal/worker/vault.go`. The secrets loader resolves each `_vault` field to a `vaultRef`; static bindings
get a `vaultValue` entry kind; the OAuth provider reads its client secret, key and certificate from the client
inside each mint. Compose adds a dev-mode Vault with an AppRole init job; the chart adds a second projected
token (`worker.vaultIdentity`); minikube runs Vault with Kubernetes auth.

**Tech Stack:** Go 1.27 standard library only (net/http, encoding/json, crypto/x509), Helm chart, docker
compose, `hashicorp/vault:2.1.1`.

**Spec:** `docs/superpowers/specs/2026-09-27-vault-provider-design.md`

## Global Constraints

- ADR-019 Rev 1.3; only `execution-worker` talks to Vault; the worker never contacts Vault at load.
- Vault client: 10 s timeout, no redirects, responses at most 64 KiB, `X-Vault-Token`, optional
  `X-Vault-Namespace`, `https` (or `http` only with `AllowPlainTokenURL`, i.e. development/test).
- `refresh_seconds` 30–3600, default 300; token used for `min(lease_duration, 3600) * 2/3`; lease_duration ≥ 1.
- Failure classes exactly: `vault_login_unreadable`, `vault_login`, `vault_forbidden`, `vault_read`,
  `vault_missing`, `vault_invalid`.
- Back-off 1 s doubling to 60 s (the existing OAuth numbers); no stale value past its refresh deadline.
- Never log, store, journal or return a Vault token or value; errors name fields, never values.
- Demo tenant `00000000-0000-4000-8000-0000000000a7` (slug `wayne`); bindings `fakeerp-vault`
  (`eacp/fakeerp#token`) and `fakeerp-vault-oauth` (`eacp/fakeerp-oauth#client_secret`, client `eacp-worker`).
- Compose worker loads 8 bindings. Commits as the user only, no Co-Authored-By trailer.

## Review Focus

1. Vault answers a redirect (a standby forwarding to the active node on another host) — failure, the token is
   never sent to the other host (Task 1 test).
2. A Vault role with a long TTL (768 h) — the token is still re-logged within 40 min (Task 1 test).
3. Many actions at once when a path's cache expired — one KV read, one login (Task 1 test).
4. A reference path with a `..` segment or a leading `/` — the file is rejected (Task 2 test).
5. Vault unreachable when the worker starts — `LoadSecrets` succeeds and the binding backs off (Task 2 test).

---

### Task 1: The Vault client

**Files:**
- Create: `internal/worker/vault.go`, `internal/worker/vault_test.go`

**Interfaces:**
- Produces:
  - `type vaultEntry struct { Address, Namespace, CAFile, KVMount string; RefreshSeconds int; Auth vaultAuthEntry }`
    with JSON names `address`, `namespace`, `ca_file`, `kv_mount`, `refresh_seconds`, `auth`;
    `vaultAuthEntry{Kubernetes *struct{Mount, Role, JWTFile string}; AppRole *struct{Mount string; RoleID, RoleIDFile, SecretID, SecretIDFile *string}}`
    (`kubernetes` {`mount`,`role`,`jwt_file`}, `approle` {`mount`,`role_id`,`role_id_file`,`secret_id`,`secret_id_file`}).
  - `type vaultRef struct { Mount, Path, Key string }` (JSON `mount`, `path`, `key`), `func (r vaultRef) validate(defaultMount string) (vaultRef, error)`.
  - `func newVaultClient(e vaultEntry, c loadConfig) (*vaultClient, error)` — validates, reads the login
    files once (load-time check), contacts nothing.
  - `func (v *vaultClient) value(ctx context.Context, r vaultRef) (Secret, string)` — the value and a failure
    class ("" on success); caches `data.data` per mount+path for the refresh interval.
  - `func (v *vaultClient) drop(r vaultRef)` — forget the cached path.
  - `func (v *vaultClient) refresh() time.Duration`.
- Consumes: `loadConfig` (`allowPlain`, `redact`, `now`, `log`), `Secret`, `hostPattern` (secrets.go).

- [ ] **Step 1: Write the failing tests** in `vault_test.go` with a `fakeVault` httptest server (counts logins
  and reads per path, records request paths, bodies and headers, and can answer any status per route):
  - `TestVaultAppRoleLogin` — `POST /v1/auth/approle/login` body `{"role_id","secret_id"}` from files; the
    read carries `X-Vault-Token` and `X-Vault-Namespace: eacp`; `GET /v1/secret/data/eacp/erp` returns the key.
  - `TestVaultKubernetesLoginRereadsTheJWT` — `POST /v1/auth/kubernetes/login` `{"role":"eacp-worker","jwt":…}`;
    the JWT file is rewritten between two logins and the second login sends the new one.
  - `TestVaultTokenIsReusedThenRenewedByLogin` — lease 90 s: reads at +0 and +59 s share one login, a read at
    +61 s logs in again; lease 2 764 800 s: a read at +40 min + 1 s logs in again (cap 3600 × 2/3).
  - `TestVaultCachesAPathForTheRefreshInterval` — two keys of one path, reads at +0 and +29 s → one KV read;
    at +31 s (refresh 30) → a second read; a rotated value is returned after it.
  - `TestVaultFailureClasses` — table: unreadable secret_id file → `vault_login_unreadable`; login 400 →
    `vault_login`; login without client_token or lease 0 → `vault_login`; read 403 → `vault_forbidden` and the
    next call logs in again; read 503 and non-JSON → `vault_read`; 404, `data.data` null, non-empty
    `deletion_time`, `destroyed: true`, key absent → `vault_missing`; value `42` → `vault_invalid`.
  - `TestVaultNeverFollowsARedirect` — the read answers 307 to a second server; the second server gets no
    request; class `vault_read`.
  - `TestVaultOneLoginAndOneReadUnderConcurrency` — 20 goroutines on an expired cache → 1 login, 1 read.
  - `TestVaultNeverLogsATokenOrValue` — a canary token and value appear in no log line or class.
- [ ] **Step 2: Run** `go test ./internal/worker -count=1 -run TestVault`. Expected: FAIL (undefined).
- [ ] **Step 3: Implement** `vault.go` per spec §3.2. The Vault token is added to `c.redact` until its lease
  end + 24 h (`AddUntil` as the OAuth tokens), every value read permanently. Logins and reads log host, mount,
  path and class only.
- [ ] **Step 4: Run** Step 2's command with `-race`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(worker): a Vault client - AppRole and Kubernetes login, KV v2 reads cached per path`

### Task 2: Static credentials from Vault

**Files:**
- Modify: `internal/worker/secrets.go` (file shape, `value_vault`, entry kind, Credential/Available/Rejected/Values)
- Create: `internal/worker/vaultsecrets_test.go`

**Interfaces:**
- Consumes: Task 1's `vaultEntry`, `vaultRef`, `newVaultClient`, `value`, `drop`.
- Produces: `type vaultValue` (per-binding: ref, current and previous value, back-off) and
  `secretEntry.vault *vaultValue`; the file's top-level `vault` object; `loadConfig.vault *vaultClient` set
  while loading so Task 3 can use it.

- [ ] **Step 1: Write the failing tests:**
  - `TestAStaticCredentialComesFromVault` — `Credential` returns the Vault value; `Values()` has it.
  - `TestAVaultFailureBacksTheBindingOff` — read 503: `ErrCredentialUnavailable`, `Available()` omits the
    binding, no further request until 1 s passes, then 2 s; success resets.
  - `TestAStaleVaultValueIsNeverServed` — after the refresh deadline with Vault answering 503 → unavailable.
  - `TestARejectedVaultCredentialIsReadAgain` — `Rejected` with the current value → next `Credential` reads
    Vault at once (before the refresh deadline) and returns the rotated value; the previous value stays in
    `Values()` until one refresh interval after the rotation, then leaves.
  - `TestTheWorkerStartsWithVaultDown` — address of a closed port: `LoadSecrets` succeeds; `Credential` →
    `ErrCredentialUnavailable`.
  - `TestInvalidVaultEntriesRejectTheWholeFile` — `value_vault` without `vault`; `value` + `value_vault`;
    path `a/../b`, `/a`, `a/`, `a//b`, 257 characters; key `a b`; bad `mount`; refresh 29 and 3601; `ftp://`
    address; `http://` without `AllowPlainTokenURL`; address with user info or query; both and neither auth
    method; kubernetes without role or jwt_file; approle with `role_id` and `role_id_file`; unreadable or empty
    login file; unknown member. No error repeats a canary.
- [ ] **Step 2: Run** `go test ./internal/worker -count=1 -run 'Vault'`. Expected: the new tests FAIL.
- [ ] **Step 3: Implement.** `exactly one of value, value_file, value_vault and oauth2`. Back-off reuses the
  OAuth numbers (1 s doubling, 60 s cap). A value must be 1–4096 bytes (else `vault_invalid`). Log a failed read
  as the OAuth provider logs a failed mint (binding, class).
- [ ] **Step 4: Run** Step 2's command with `-race`, then `go test ./internal/worker -count=1 -run 'OAuth|Secret|Assertion|PrivateKey'`.
  Expected: PASS.
- [ ] **Step 5: Commit** `feat(worker): a static connector credential may come from Vault KV v2`

### Task 3: OAuth client secrets and private_key_jwt keys from Vault

**Files:**
- Modify: `internal/worker/oauth2.go` (`ClientSecretVault`), `internal/worker/privatekeyjwt.go`
  (`KeyVault`, `CertificateVault`), extend `internal/worker/vaultsecrets_test.go`

**Interfaces:**
- Consumes: Task 2's `loadConfig.vault`; 24c's `newAssertionSigner(privateKeyJWTEntry)`.
- Produces: `oauthEntry.ClientSecretVault *vaultRef` (`client_secret_vault`),
  `privateKeyJWTEntry.KeyVault` / `CertificateVault *vaultRef` (`key_vault`, `certificate_vault`).

- [ ] **Step 1: Write the failing tests:**
  - `TestAnOAuthClientSecretComesFromVault` — the token request's Basic auth carries the Vault value; a
    rotated client secret (IdP answers 401 to the old one) is read again at the next mint and the mint succeeds.
  - `TestAPrivateKeyComesFromVault` — assertions verify with the Vault key; rotating the key and certificate in
    Vault changes the signature key and `x5t#S256` after the refresh; a bad PEM from Vault fails the mint with
    `vault_invalid` and backs off; the key PEM is redacted and never in `Values()`.
  - `TestInvalidVaultEntriesRejectTheWholeFile` gains: `client_secret` + `client_secret_vault`;
    `key` + `key_vault`; `certificate_file` + `certificate_vault`.
- [ ] **Step 2: Run** `go test ./internal/worker -count=1 -run 'Vault'`. Expected: FAIL.
- [ ] **Step 3: Implement** per spec §3.3: the provider reads Vault inside `request` before the token
  request; the signer is rebuilt when the key or certificate text changes (inline form into
  `newAssertionSigner`); a failed mint drops the provider's cached Vault paths.
- [ ] **Step 4: Run** Step 2's command with `-race`, plus the 24a–24c unit tests
  (`-run 'OAuth|Assertion|PrivateKey|Federat|Token'`). Expected: PASS.
- [ ] **Step 5: Commit** `feat(worker): OAuth client secrets and private_key_jwt keys may come from Vault`

### Task 4: PostgreSQL integration test

**Files:** Modify `internal/worker/jit_integration_test.go`.

- [ ] **Step 1: Write** `TestTheWorkerExecutesWithAVaultCredential`. Generalise `newJITFor` so the test can
  give the whole binding body (a static `value_vault` entry plus a top-level `vault` block) instead of the
  `oauth2` fields. An in-process fake Vault (AppRole, `refresh_seconds` 30 on the test clock) first holds a
  stale token the Fake ERP refuses: action 1 ends its attempt `unauthorized` (AttemptCount 1, not SUCCEEDED).
  Vault is then updated to `jitStatic`; without advancing the clock past the refresh, action 2 re-reads Vault
  (the rejection dropped the cache) and SUCCEEDS as principal `execution-worker`; action 3 reuses the cached
  value (no further KV read). Finally `assertNotPersisted(vaultToken, staleValue, jitStatic)`.
- [ ] **Step 2: Run** `go test ./internal/worker -race -count=1 -run 'VaultCredential|MintedToken'` with
  `EACP_TEST_ADMIN_DSN`. Expected: PASS (the behaviour exists from Tasks 1–3); mutation check: make
  `Rejected` not drop the cache and see it fail, then restore.
- [ ] **Step 3: Commit** `test(worker): an action executes with a credential read from Vault, and after its rotation`

### Task 5: Helm chart `worker.vaultIdentity`

**Files:** Modify `deployments/helm/eacp/values.yaml`, `templates/worker.yaml`, `templates/validate.yaml`;
`test/helm/identity_test.go`.

- [ ] **Step 1: Write** `TestTheVaultIdentityIsTheWorkersOwn` (projected token audience `vault`, path
  `token`, mounted read-only at `/run/secrets/eacp-vault-identity` in the worker only; absent by default; works
  with and without `workloadIdentity`) and extend the refused-values table (empty audience, whitespace,
  expirationSeconds 599 and 86 401).
- [ ] **Step 2: Run** `EACP_HELM_REQUIRED=1 go test ./test/helm -count=1 -run Identity`. Expected: FAIL.
- [ ] **Step 3: Implement** mirroring `workloadIdentity` (volume `vault-identity`).
- [ ] **Step 4: Run** `EACP_HELM_REQUIRED=1 go test ./test/helm -count=1`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(deploy): an optional projected token for the worker's Vault login`

### Task 6: Compose Vault, security tests and the demo

**Files:** Modify `docker-compose.yml`, `deployments/docker/secrets/connector-secrets.dev.json`,
`test/security/network_isolation_test.go`, `test/demo/platform_test.go` (a `vault(args...)` exec),
`scripts/demo.sh`; Create `deployments/docker/vault/init.sh`, `test/demo/vault_test.go`.

- [ ] **Step 1: Write** `TestVaultDemo` (spec §3.5 V0–V3; the Vault-token scan matches `hvs.` followed by
  base64url characters) and the security tests `TestAgentCannotReachVault`,
  `TestTheVaultAppRoleIsMountedOnlyIntoTheWorker`, `TestOnlyTheWorkerSharesTheVaultNetwork`; bump
  `"bindings":8`.
- [ ] **Step 2: Wire compose** per spec §3.5: `vault` (`server -dev -dev-root-token-id=dev-only-vault-root-token
  -dev-listen-address=0.0.0.0:8200`, `IPC_LOCK` not needed in dev mode, `SKIP_SETCAP=true`), `vault-init`
  (runs `init.sh` with `VAULT_ADDR`, `VAULT_TOKEN`, reads the ERP token and OAuth client secret dev files,
  writes `role_id`/`secret_id` to `/approle`), volume `vault_approle`, network `vault` (internal). The dev
  manifest gets the `vault` block (AppRole files at `/run/secrets/eacp-vault-approle/{role_id,secret_id}`,
  refresh 30) and the two tenant `…a7` bindings. `DEMO=J` also runs `TestVaultDemo`.
- [ ] **Step 3: Run** `docker compose up -d --build`, `EACP_COMPOSE_TEST=1 go test -count=1 ./test/security/`,
  `DEMO=J bash scripts/demo.sh`. Expected: all PASS.
- [ ] **Step 4: Commit** `test(demo): a Vault demo on compose - AppRole login, credentials read from KV v2`

### Task 7: Kubernetes Vault and the e2e run

**Files:** Create `deployments/k8s/dev/vault.yaml`, `deployments/k8s/connector-secrets.vault.json`; Modify
`deployments/k8s/e2e-values.yaml` (`vaultIdentity`, Vault egress), `scripts/k8s-e2e.sh` (load the Vault
image, wait for the init Job, merge the `vault` block, TESTS default adds `TestVaultDemo`).

- [ ] **Step 1: Wire** per spec §3.5 (Kubernetes); the init Job waits for Vault, enables `kubernetes` auth
  with `kubernetes_host=https://$KUBERNETES_SERVICE_HOST:$KUBERNETES_SERVICE_PORT`, creates role `eacp-worker`
  (`bound_service_account_names=eacp-worker`, `bound_service_account_namespaces=eacp`, `audience=vault`,
  `token_policies=eacp-worker`, `token_ttl=1h`) and writes both KV values from a dev Secret.
- [ ] **Step 2: Run** `bash scripts/k8s-e2e.sh` (fresh profile). Expected: `TestVaultDemo`, the JIT demos
  and the disruption run PASS (`TestSliceADemo` step 7 is the known pre-existing flake; record, do not weaken).
- [ ] **Step 3: Commit** `test(demo): the Vault demo on Kubernetes with Kubernetes auth`

### Task 8: Documentation and final verification

- [ ] **Step 1:** ADR-019 Rev 1.3 (§3c Vault, §6 Fake ERP unchanged, proof, assumptions), MASTER_PLAN §96,
  AGENTS.md, README, `docs/KUBERNETES.md`, `docs/DEMO.md`, `docs/INVARIANTS.md`.
- [ ] **Step 2:** `go vet ./... && go test -race ./...` (DSN, `EACP_HELM_REQUIRED=1`) with nothing else running.
- [ ] **Step 3:** Fresh reviewer on the whole branch; fix Critical/Important with RED→GREEN tests.
- [ ] **Step 4: Commit** `docs: ADR-019 Rev 1.3 - Vault KV v2 credential source; Phase 24d delivered`
