# Phase 24d — a HashiCorp Vault KV v2 credential source (design)

Date: 2026-09-27 · Status: approved by the owner in chat ("yes, write the spec and dev until finished phase")
Scope: MASTER_PLAN §96 (Phase 24), fourth sub-phase. ADR: **ADR-019 Credential Custody, Rev 1.3**.
Follows 24a (the provider seam, OAuth client credentials), 24b (workload identity federation) and 24c
(private_key_jwt). Owner choices: KV v2 as the secret source (not dynamic leases), Kubernetes and AppRole
login, HashiCorp Vault (pinned `hashicorp/vault:2.1.1`) for the demos, and approach A (a third source for
every credential field).

## 1. Intent

Until now every credential the worker uses sits in its connector-secrets file (inline or in a mounted file),
so rotating one means rewriting that file and restarting the worker. Enterprises keep such secrets in Vault.
After 24d the file can say *where* a credential lives in Vault KV v2 instead of holding it; the worker logs in
to Vault with its own identity, reads the value when it needs it, and picks up a rotation without a restart.

Success:

- a static connector token, an OAuth client secret and a private_key_jwt key and certificate can each come
  from Vault KV v2;
- the worker logs in with Kubernetes auth (its own projected service-account token, audience `vault`) or
  AppRole, and holds no long-lived Vault secret on Kubernetes;
- a rotated value is used within one refresh interval, at once when the target rejects the old one, without a
  restart;
- Vault being down, sealed, forbidding or missing a value never stops the worker from starting and never fails
  an action: that binding's work waits (`QUEUED`) and resumes;
- no Vault token and no Vault-held value appears in logs, API responses, the database or connector results;
  only the worker talks to Vault;
- the demo runs on compose (AppRole) and Kubernetes (Kubernetes auth); 24a–24c bindings behave exactly as
  before.

## 2. Verified upstream API (developer.hashicorp.com, 2026-09)

- All routes are under `/v1/`. The token goes in `X-Vault-Token`; `X-Vault-Namespace` selects a namespace.
  Errors are `{"errors": [...]}`; 403 = forbidden (bad or expired token, or policy), 404 = invalid path or no
  permission to see it, 503 = sealed, down or overloaded.
- AppRole login: `POST /v1/auth/<mount>/login` with `{"role_id", "secret_id"}`; response
  `auth.client_token`, `auth.lease_duration` (seconds), `auth.renewable`.
- Kubernetes login: `POST /v1/auth/<mount>/login` with `{"role", "jwt"}`; same `auth` object. In-cluster
  Vault reviews the token with its own service account (`system:auth-delegator`); a role binds
  `bound_service_account_names`, `bound_service_account_namespaces` and `audience`.
- KV v2 read: `GET /v1/<mount>/data/<path>` → `data.data` (the key/value map) and `data.metadata`
  (`version`, `created_time`, `deletion_time` ("" unless soft-deleted), `destroyed`).
- Latest release: 2.1.1.

## 3. Design

### 3.1 Secrets file

A top-level `vault` object configures one Vault client per worker:

```json
{"vault": {"address": "https://vault.internal:8200", "namespace": "eacp", "ca_file": "/run/secrets/vault-ca.pem",
           "kv_mount": "secret", "refresh_seconds": 300,
           "auth": {"kubernetes": {"mount": "kubernetes", "role": "eacp-worker",
                                   "jwt_file": "/run/secrets/eacp-vault-identity/token"}}},
 "secrets": [...]}
```

- `address`: an absolute URL with a lowercase host and no user info, query or fragment; `https`, or `http`
  only in development and test (as `token_url`). `namespace` optional (1–256 printable characters).
  `ca_file` optional (one or more PEM certificates, added to a pool used instead of the system roots).
  `kv_mount` default `secret`. `refresh_seconds` 30–3600, default 300.
- `auth` has exactly one of:
  - `kubernetes`: `role` (required), `mount` (default `kubernetes`), `jwt_file` (required; read at load
    and at every login);
  - `approle`: `mount` (default `approle`), exactly one of `role_id` / `role_id_file` and exactly one of
    `secret_id` / `secret_id_file` (files read at load and at every login).

Every credential field gains a Vault form, and each field takes exactly one of its forms:

| Field | Forms |
|---|---|
| a static credential | `value` · `value_file` · `value_vault` (alongside `oauth2`, exactly one of the four) |
| `oauth2` client secret | `client_secret` · `client_secret_file` · `client_secret_vault` (still exclusive with `client_assertion_file` and `private_key_jwt`) |
| `private_key_jwt` key | `key` · `key_file` · `key_vault` |
| `private_key_jwt` certificate | `certificate` · `certificate_file` · `certificate_vault` (optional) |

A Vault reference is `{"path": "eacp/tenant-a/erp", "key": "token", "mount": "secret"}`: `path` is 1–256
characters of segments `[A-Za-z0-9._-]+` joined by `/` (no `.` or `..` segment, no leading or trailing `/`),
`key` is 1–128 characters `[A-Za-z0-9._-]`, `mount` optional (default `kv_mount`, same rules as a segment).

Load-time validation (any failure rejects the whole file; errors never contain a value): the shapes above; a
`_vault` field without a `vault` block; a `vault` block whose login files are unreadable or empty. The worker
**never contacts Vault at load**: the worker starts with Vault down.

### 3.2 The Vault client (`internal/worker/vault.go`)

- Its own `http.Client`: 10 s timeout, no redirects (`ErrUseLastResponse`; a redirect is a failure), TLS with
  `ca_file`'s pool when set, responses read to at most 64 KiB.
- **Login** is lazy and one at a time. It re-reads the JWT or secret_id (and role_id) file, posts to
  `/v1/auth/<mount>/login`, and requires HTTP 200 with a non-empty `auth.client_token` (printable ASCII, at
  most 1024 bytes) and an integer `auth.lease_duration` of at least 1. The token is used until the request
  time plus `min(lease_duration, 3600) * 2/3`, then the next request logs in again. It lives in memory only.
- **Read** of a path: `GET /v1/<mount>/data/<path>` with `X-Vault-Token` (and `X-Vault-Namespace`). The
  whole `data.data` map is cached per path for `refresh_seconds`, measured from before the request; one read
  per path at a time; callers of the same path share it.
- A 403 drops the Vault token (the next attempt, after the back-off, logs in again). Nothing is retried
  within one `Credential` call or mint.
- Failure classes: `vault_login_unreadable` (a login file cannot be read or is empty), `vault_login` (the
  login request failed or its answer is unusable), `vault_forbidden` (403 on a read), `vault_read` (any other
  failed read: transport, 5xx, non-JSON), `vault_missing` (404, `data.data` null, a non-empty
  `deletion_time`, `destroyed`, or the key absent), `vault_invalid` (the key's value is not a JSON string, or
  it fails the rules of the field it feeds).

### 3.3 Using a Vault value

- **Static credential (`value_vault`).** `Credential` returns the cached value while it is fresh, else reads
  the path. The value must be 1–4096 bytes. A failure makes the binding unavailable with the 1 s–60 s
  back-off (as a failed mint): `Credential` answers `ErrCredentialUnavailable` without a request during it, and
  `Available()` omits the binding. A stale value is never served: past its refresh deadline, a failed read
  means no credential.
- **OAuth client secret (`client_secret_vault`)** and **private_key_jwt key/certificate (`key_vault`,
  `certificate_vault`).** Read inside each mint, before the token request; a failure is a failed mint with the
  Vault class. A key or certificate from Vault is parsed with the 24c rules each time its value changes (the
  signer is rebuilt; an invalid PEM is `vault_invalid`); the certificate's validity is checked at every mint
  (24c). A failed mint also drops the provider's cached Vault values, so a rotated client secret is read at the
  next mint.
- **Rejected.** When a call ends `unauthorized`, a static Vault binding drops its cached path, so the next
  call re-reads it (a rotation takes effect at once). An OAuth binding drops its token as before.
- **Redaction.** Every value read from Vault is added to the redaction set permanently (and, for a PEM, each
  base64 line of 16 or more characters, as 24c). The Vault token is added until its lease end plus 24 h.
- **Values().** A static Vault binding contributes its current value and, until one refresh interval after a
  rotation, its previous value, so a value just rotated out is still scrubbed from connector results. Neither
  the Vault token nor a private key ever enters `Values()` (they are never sent to a connector).
- **Logging.** Logins and reads are logged with the Vault host, path, mount and class only — never a token,
  value or the login files' contents.

### 3.4 Kubernetes identity for Vault (Helm chart)

`worker.vaultIdentity` — `enabled` (default `false`), `audience` (default `vault`; 1–256 characters without
whitespace), `expirationSeconds` (600–86 400, default 3600). When enabled, the worker pod alone gets a second
projected service-account token, read-only at `/run/secrets/eacp-vault-identity/token`, under the worker's own
ServiceAccount `<release>-worker`. It is separate from 24b's `workloadIdentity` token so that neither relying
party (the ERP's IdP, Vault) can replay a token meant for the other. Egress to Vault is declared through the
existing `worker.connectorEgress`.

### 3.5 Demo

- Tenant `00000000-0000-4000-8000-0000000000a7` (slug `wayne`), two bindings:
  - `fakeerp-vault`: a static credential `value_vault` = `{"path": "eacp/fakeerp", "key": "token"}` (the
    Fake ERP's static token);
  - `fakeerp-vault-oauth`: the 24a OAuth client `eacp-worker` with `client_secret_vault` =
    `{"path": "eacp/fakeerp-oauth", "key": "client_secret"}`.
- The dev manifest's `vault` block: `address` `http://vault:8200`, `refresh_seconds` 30.
- **Compose:** a `vault` service (`hashicorp/vault:2.1.1`, `server -dev`, a dev-only root token, in memory) on a
  new internal network `vault` joined only by `vault`, `vault-init` and `execution-worker`. `vault-init` (the
  same image, one-shot) enables AppRole, writes a policy that can only read `secret/data/eacp/*`, creates role
  `eacp-worker` with that policy, writes both KV values from the dev secret files, and writes `role_id` and
  `secret_id` into the `vault_approle` volume, mounted read-only into the worker only at
  `/run/secrets/eacp-vault-approle`. The worker depends on `vault-init` having completed.
- **Kubernetes:** `deployments/k8s/dev/vault.yaml` — a dev-mode Vault Deployment and Service in `eacp-deps`
  (its own ServiceAccount with `system:auth-delegator`), and an init Job that enables Kubernetes auth
  (`kubernetes_host` = the in-cluster API), creates role `eacp-worker` bound to service account `eacp-worker`
  in namespace `eacp` with audience `vault`, and writes the KV values. A NetworkPolicy admits only the worker
  and the init Job. The e2e values enable `worker.vaultIdentity` and add Vault to `worker.connectorEgress`;
  `scripts/k8s-e2e.sh` replaces the merged manifest's `vault` block with Kubernetes auth
  (`deployments/k8s/connector-secrets.vault.json`).
- `test/demo` `TestVaultDemo` (both platforms):
  - V0: bootstrap tenant Wayne and an allow policy.
  - V1: purchases through both bindings succeed, one PO each, as principal `execution-worker` (the ERP's static
    token) and `oauth:eacp-worker`.
  - V2: the demo soft-deletes the static token's latest KV version (`vault kv delete`, run in the Vault
    container as its admin): after the refresh interval a new purchase waits in `QUEUED` with no attempt;
    `vault kv undelete` restores it and the purchase succeeds.
  - V3: the scan of API responses, service logs and a `pg_dump` finds no issued token, no Vault token
    (`hvs.`-prefixed) and neither Vault-held value.
- `test/security` (compose): the agent cannot reach Vault; only the worker mounts `vault_approle`; only
  `vault`, `vault-init` and the worker are on the `vault` network; the worker loads 8 bindings.

### 3.6 Proof (tests first)

- `internal/worker` (`vault_test.go`, an in-process fake Vault): AppRole and Kubernetes login bodies and paths,
  the namespace header, a login file re-read at every login, token reuse and re-login at two thirds of the
  (capped) lease, a 403 dropping the token, one login and one read per path under concurrency; KV reads,
  caching and refresh, each failure class with back-off and no stale value, `Rejected` forcing a re-read, a
  rotation observed within one refresh, the previous value in `Values()` until the next refresh; the client
  secret and private_key_jwt key from Vault (a rotated key rebuilds the signer; a bad PEM is `vault_invalid`);
  no redirect followed; no token or value in logs or errors; `TestInvalidVaultEntriesRejectTheWholeFile`.
- `internal/worker` PostgreSQL (`jit_integration_test.go`): `TestTheWorkerExecutesWithAVaultCredential` — the
  real HTTP connector, Fake ERP and a fake Vault; two actions succeed; the Vault value is rotated and the ERP
  rejects the old one, and the next action succeeds with the new one; no Vault token or value in the database
  or logs.
- `test/helm` (`identity_test.go`): `vaultIdentity` projects its own token into the worker only; bad values
  are refused at render time.
- `test/security`, `test/demo` as above; the minikube e2e.

### 3.7 Documentation

ADR-019 Rev 1.3, MASTER_PLAN §96, AGENTS.md, README, `docs/KUBERNETES.md` (`vaultIdentity`, egress),
`docs/DEMO.md`, `docs/INVARIANTS.md` (invariant 11 gains the integration test).

## 4. Out of scope

Dynamic secrets and leases, Vault Agent, response wrapping, KV v1, renewing or revoking the Vault token
(the worker logs in again instead), Vault Enterprise features beyond the namespace header, OpenBao testing.

## 5. Unresolved assumptions (conservative choices, recorded in ADR-019)

| Assumption | Choice |
|---|---|
| Vault down at start | The worker starts; bindings that need Vault back off until it answers. |
| Stale values | Never served past `refresh_seconds`; a failed read means no credential. |
| Refresh interval | 300 s by default, 30–3600 s. |
| Vault token lifetime | Used for two thirds of `min(lease_duration, 3600)`, then a new login; never renewed or persisted. |
| A 403 from Vault | Drops the Vault token; the binding backs off; the next attempt logs in again. |
| Rotation on rejection | A static binding re-reads Vault after the target answers `unauthorized`. |
| Soft-deleted or destroyed version | Treated as missing: no credential. |
| Two tokens on Kubernetes | Separate audiences for the ERP's IdP and Vault; neither can replay the other's. |
| Vault in the demo | Dev mode, in memory, a dev-only root token known to Vault and its init job only. |
