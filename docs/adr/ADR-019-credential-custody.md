# ADR-019: Credential custody — providers and just-in-time credentials

Status: Accepted (Rev 1.3, 2026-09-27). Phases 24a (Rev 1.0), 24b (Rev 1.1), 24c (Rev 1.2) and 24d (Rev 1.3).
Scope: MASTER_PLAN §96.
Related: ADR-001 (the product boundary: agents never hold enterprise credentials), ADR-003 §4 (a credential is
bound to one connector host), ADR-004 (execution semantics, unknown outcomes and reconciliation), ADR-022
(circuits and backpressure withhold work), ADR-023 (the MCP scanner), ADR-029 (the worker runs as several
replicas).

## Context

Since Slice A only `execution-worker` holds connector credentials:

- it loads them from `EACP_CONNECTOR_SECRETS_FILE`, keyed by tenant and `secret_ref`;
- each credential is bound to the exact `host[:port]` of its connector;
- values are redacted from logs, scrubbed from connector results, and never stored, journaled or returned;
- every other service refuses the file (`config.Options.AllowConnectorSecrets`).

Those credentials were static: a long-lived secret per target. §96 asks for short-lived credentials minted
just in time, then Vault, SPIFFE and cloud workload identity. This revision adds the provider seam and its
first real provider, OAuth 2.0 client credentials (RFC 6749 §4.4), the pattern behind Entra ID, Okta, Keycloak
and most enterprise token brokers. Custody does not change.

Rev 1.1 (Phase 24b) removes the last long-lived secret of such a binding: workload identity federation. The
worker authenticates to the token endpoint with a JWT its platform issues and rotates (a Kubernetes projected
service-account token, which Azure Workload Identity also uses) instead of a client secret (§3a). It also
accepts tokens that live longer than an hour, as Entra ID issues them, but uses each for at most an hour
(§3).

Rev 1.2 (Phase 24c) brings the same property where no platform issues the worker an identity (VMs, compose,
on-premises hosts): `private_key_jwt` (OpenID Connect Core §9, RFC 7523 §2.2). The worker holds an asymmetric
private key, the IdP holds only its public half, and every token request carries a fresh, short-lived
assertion the worker signs (§3b). Nothing the IdP stores can authenticate as the worker.

Rev 1.3 (Phase 24d) moves the credentials themselves out of the secrets file. Enterprises keep them in
HashiCorp Vault; rotating a value in the file meant rewriting it and restarting the worker. Now the file may
say *where* a credential lives in Vault KV v2 instead of holding it: the worker logs in to Vault with its own
identity (Kubernetes auth with its projected token, or AppRole), reads the value when it needs it and picks up
a rotation without a restart (§3c).

## Decision

### 1. Custody (unchanged, now written down)

- Providers run only inside `execution-worker`. No API, table, message or log learns a credential or a
  provider's configuration; the registry knows only the connector's `secret_ref`.
- A credential is bound to one connector host. The OAuth token endpoint is a second, separately configured
  host; the client secret is sent only to it.
- An invalid entry rejects the whole secrets file at startup (fail closed). Errors never contain a value.
- Nothing is dispatched without a credential.

### 2. The provider seam

`worker.SecretStore` (loaded by `worker.LoadSecrets(path, opts...)`) keeps its name and file. Each entry has
exactly one of `value`, `value_file` (a static credential, as before) or `oauth2` (a provider):

- `Credential(ctx, tenant, ref, endpoint, validFor)` returns a credential that stays valid for at least
  `validFor`. `ErrNoCredential`: no binding for this tenant, reference and exact host.
  `ErrCredentialUnavailable`: the provider cannot produce one now. `Resolve` is `Credential` with `validFor` 0.
- `validFor` is the caller's call budget plus `worker.CredentialSkew` (30 s): the worker uses
  `eacp.call_timeout` of the pinned contract (read with the job, at most 300 s), the reconciler its lookup
  budget, the MCP scanner its scan timeout. A token that could expire during the call is never sent.
- `Available()` lists the bindings the worker can serve now. Claims (`Claimable`, `Reconcilable`, `ScansDue`)
  use it, so a binding whose provider is backing off gets no work: nothing is claimed only to be released.
- `Rejected(tenant, ref, secret)`: when a call ends with error class `unauthorized`, the worker (and the MCP
  scanner) tell the provider, which drops the token so the next attempt mints another. The attempt's outcome
  is classified exactly as before (ADR-004).
- `Values()` returns every live value (static secrets, client secrets, unexpired tokens). The scrubbers use it
  together with the value actually sent, which the cache may already have replaced or dropped.

### 3. The OAuth 2.0 client-credentials provider

Entry: `token_url` (absolute, lowercase host, no user info, query or fragment; `https`, or `http` only when
`EACP_ENV` is `development` or `test`), `client_id` (1–256 printable characters without spaces or `:`),
exactly one of `client_secret`, `client_secret_file` (1–4096 bytes), `client_assertion_file` (§3a) and
`private_key_jwt` (§3b),
optional `scope` (RFC 6749 scope tokens, ≤ 1024 bytes) and `resource` (RFC 8707, an absolute URL without
fragment).

Token request: `POST token_url`, form `grant_type=client_credentials[&scope][&resource]`, client
authentication `client_secret_basic` with the id and secret form-urlencoded (RFC 6749 §2.3.1). Its own HTTP
client: 10 s timeout, **no redirects** (a redirect could carry the client secret elsewhere), response at most
64 KiB.

Only this response is a token: HTTP 200, a JSON object, `access_token` of 1–8192 printable ASCII characters
without spaces, `token_type` `Bearer` (case-insensitive, RFC 6750) and `expires_in` a bare JSON integer from 1
to 86 400. Anything else is a failed mint.

A token is **used** for at most an hour: its usable expiry is the request time plus `min(expires_in, 3600)`,
and that capped lifetime decides whether a call can be served. Entra ID gives access tokens a random default
lifetime of 60–90 minutes, so Rev 1.0's refusal of `expires_in` above 3600 would have refused its tokens about
half the time (Rev 1.1). The token still authorises at the target until its real expiry, so it stays in
`Values()` (for scrubbing) until then, even after a newer token replaced it (up to 16 per binding), and in the
redaction set until a day later.

A token that lives shorter than the call's `validFor` is not a failed mint: the IdP works, the call is simply
longer than its tokens. The token is kept for shorter calls, the provider remembers the lifetime and answers
`ErrCredentialTooShort` (an `ErrCredentialUnavailable`) for this and any call as long, at once and without
minting again, until a later mint shows a longer lifetime. The binding does not back off.

- The expiry is measured from before the request, so network time never extends a token.
- A cached token is reused only while `now + validFor` is before its expiry. Tokens live in worker memory only.
- One mint runs at a time per binding; callers waiting for it reuse its token.
- A failed mint backs the binding off for 1 s, doubling to at most 60 s; a success resets it. During the
  back-off `Credential` fails at once without a request and `Available()` omits the binding.
- Mints and failures are logged with tenant, reference, token host, lifetime or failure class only.

### 3a. Workload identity federation (Rev 1.1)

With `client_assertion_file`, the client holds no secret. The file holds a JWT that the platform issues and
rotates, such as a Kubernetes projected service-account token (the kubelet rotates it at 80 % of its lifetime,
and the application must reload it). The token request is RFC 7523 §2.2, in the shape Entra ID documents for a
federated credential:

```
grant_type=client_credentials&client_id=<id>
&client_assertion_type=urn:ietf:params:oauth:client-assertion-type:jwt-bearer
&client_assertion=<jwt>[&scope][&resource]
```

with no `Authorization` header (one client authentication method per request).

- **At load** the file must be readable, 1 byte–16 KiB after a trailing newline is trimmed, and a compact JWS
  (three non-empty unpadded base64url segments, a JSON object header and payload) whose payload has a
  numeric `exp`; otherwise the whole secrets file is rejected. An expired assertion is accepted at load: the
  kubelet may be about to rotate it.
- **At every mint** the file is read again. An unreadable, empty or oversized file (`assertion_unreadable`),
  a malformed one (`assertion_invalid`), or one with less than 10 s (the token request timeout) left
  (`assertion_expired`) is a failed mint: no request, the 1–60 s back-off, and the binding withheld from
  claims.
- The worker never checks the assertion's signature, issuer or audience: only the IdP judges it. It reads
  `exp` only to know when the assertion is unusable and how long to redact it.
- The assertion is a credential (it can be exchanged for tokens until it expires): each value read is added to
  the redaction set until its `exp` plus 24 h, and the last one read is in `Values()` until its `exp`.

**Kubernetes.** The Helm chart gives the worker its own ServiceAccount, `<release>-worker`, so a federated
subject `system:serviceaccount:<namespace>:<release>-worker` names only the worker. With
`worker.workloadIdentity.enabled`, it projects a service-account token (`audience`, `expirationSeconds`
600–86 400, default 3600) into the worker pod alone, read-only at `/run/secrets/eacp-identity/token`; the
secrets-file entry names that path. For Entra ID, register a federated identity credential with the cluster's
issuer, that subject and the audience `api://AzureADTokenExchange`; the Azure Workload Identity webhook is
not needed.

### 3b. private_key_jwt (Rev 1.2)

With a `private_key_jwt` object the client holds no shared secret: it signs its own assertion, in the shapes
Entra ID (certificate credentials) and Okta/Keycloak (a client JWKS) document.

```json
"private_key_jwt": {"alg": "PS256",
                    "key_file": "/run/secrets/idp/key.pem"         | "key": "-----BEGIN PRIVATE KEY-----\n…",
                    "certificate_file": "/run/secrets/idp/cert.pem" | "certificate": "…",
                    "key_id": "…"}
```

- **At load** (any failure rejects the whole secrets file; errors never contain a value): `alg` is `RS256`,
  `PS256` (an RSA key of at least 2048 bits) or `ES256` (an ECDSA P-256 key). Exactly one of `key_file` and
  `key`: one unencrypted PEM block, at most 16 KiB, of type `PRIVATE KEY` (PKCS #8), `RSA PRIVATE KEY` or
  `EC PRIVATE KEY`, whose key type matches `alg`. At most one of `certificate_file` and `certificate`: one
  `CERTIFICATE` block for the same public key (an empty value is refused, not taken as absent); it adds
  `x5t#S256` (the base64url SHA-256 of its DER) to the header. `key_id` (1–256 printable ASCII characters
  without spaces) adds `kid`. A certificate outside its validity period is accepted at load, like an expired
  platform assertion (§3a): a clock passing `NotAfter` must not stop every other binding at the worker's next
  start. The key and
  certificate are read once; rotating them means restarting the worker, like a static secret. Inline `key` and
  `certificate` exist because the Helm chart mounts only the secrets file.
- **At every mint** the worker signs header `{"alg", "typ": "JWT"[, "kid"][, "x5t#S256"]}` and claims
  `{"iss" = "sub" = client_id, "aud" = token_url, "jti" (128 random bits), "iat" = "nbf" = now, "exp" = now +
  300}`, PSS with a salt as long as the hash for PS256 and the raw 64-byte `R‖S` for ES256 (RFC 7518). It is
  sent exactly like §3a's assertion, with no `Authorization` header. A certificate outside its validity
  period (`certificate_not_valid`) or a signing error (`assertion_signing`) is a failed mint: no request, the
  1–60 s back-off, and the binding withheld from claims until the certificate is valid.
- Each assertion is redacted until its `exp` plus 24 h, and the latest is in `Values()` until its `exp`. The
  key is never sent to a connector, so it is never in `Values()`; its PEM text, and each of its base64 lines
  of 16 or more characters, are redacted permanently (a log line may wrap a PEM).

### 3c. Vault KV v2 as a credential source (Rev 1.3)

A top-level `vault` object configures one Vault client per worker, and every credential field gains a third
form beside the inline value and the file:

```json
{"vault": {"address": "https://vault.internal:8200", "namespace": "eacp", "ca_file": "/run/secrets/vault-ca.pem",
           "kv_mount": "secret", "refresh_seconds": 300,
           "auth": {"kubernetes": {"role": "eacp-worker", "jwt_file": "/run/secrets/eacp-vault-identity/token"}}},
 "secrets": [{"tenant_id": "…", "secret_ref": "erp", "host": "erp.internal:8443",
              "value_vault": {"path": "eacp/erp", "key": "token"}}]}
```

| Field | Forms (exactly one) |
|---|---|
| a static credential | `value` · `value_file` · `value_vault` (or `oauth2`) |
| `oauth2` client secret | `client_secret` · `client_secret_file` · `client_secret_vault` (still exclusive with `client_assertion_file` and `private_key_jwt`) |
| `private_key_jwt` key | `key` · `key_file` · `key_vault` |
| `private_key_jwt` certificate (optional) | `certificate` · `certificate_file` · `certificate_vault` |

- **At load** (any failure rejects the whole file; errors never contain a value): `address` is an absolute
  URL with a lowercase host and no user info, query or fragment, `https` or, in development and test only,
  `http`; `namespace` 1–256 printable characters; `ca_file` one or more PEM certificates, used instead of the
  system roots; `kv_mount` defaults to `secret`; `refresh_seconds` 30–3600, default 300. `auth` has exactly
  one of `kubernetes` (`role`, `jwt_file`, `mount` default `kubernetes`) and `approle` (`mount` default
  `approle`, one of `role_id`/`role_id_file` and one of `secret_id`/`secret_id_file`); the login files must be
  readable and non-empty. A reference is `{"path", "key"[, "mount"]}`: `path` 1–256 characters of
  `[A-Za-z0-9._-]+` segments joined by `/` (no `.` or `..`, no leading or trailing `/`), `key` 1–128
  characters `[A-Za-z0-9._-]`. A `_vault` field needs the `vault` block. **The worker never contacts Vault at
  load**, so it starts with Vault down.
- **The client** (`internal/worker/vault.go`): its own HTTP client with a 10 s timeout, no redirects (a
  redirect is a failure) and responses read to at most 64 KiB. It logs in lazily, one login at a time,
  re-reading the JWT or AppRole files at every login (`POST /v1/auth/<mount>/login`); it requires HTTP 200,
  a printable `auth.client_token` of at most 1024 bytes and an integer `auth.lease_duration` of at least 1,
  and uses the token until the request time plus two thirds of `min(lease_duration, 3600)`. The token is
  never renewed, persisted or logged. A read is `GET /v1/<mount>/data/<path>` with `X-Vault-Token` (and
  `X-Vault-Namespace`); the whole `data.data` map is cached per path for `refresh_seconds`, measured from
  before the request, with one read per path at a time. A 403 drops the Vault token. Nothing is retried
  within one call or mint.
- **Failure classes:** `vault_login_unreadable`, `vault_login`, `vault_forbidden` (403), `vault_read`
  (transport, 5xx, non-JSON), `vault_missing` (404, no data, a soft-deleted or destroyed version, or the key
  absent) and `vault_invalid` (not a string, or not valid for the field it feeds).
- **A static credential** is the cached value while it is fresh, else a new read; it must be 1–4096 bytes. A
  failure makes the binding unavailable with §5's back-off (1 s doubling to 60 s), omitted from `Available()`.
  A stale value is never served: past its refresh deadline a failed read means no credential. When the target
  answers `unauthorized`, the binding drops its cached path, so a rotation takes effect at the next call.
- **A client secret, key or certificate** from Vault is read inside each mint, before the token request; a
  failure is a failed mint with the Vault class. A key or certificate is parsed with §3b's rules whenever its
  text changes (the signer is rebuilt; a bad PEM is `vault_invalid`). A failed mint drops the provider's cached
  Vault paths, so a rotated client secret is read at the next mint.
- **Redaction and scrubbing:** every value read from Vault is redacted permanently (a PEM also line by line,
  as §3b); the Vault token until its lease end plus 24 h. A static Vault binding's `Values()` holds its
  current value and, for one refresh interval after a rotation, the previous one. Neither the Vault token nor
  a private key ever enters `Values()`. Logins and reads are logged with the host, mount, path and class only.
- **Kubernetes identity** (ADR-029, the chart): `worker.vaultIdentity` (`enabled`, `audience` default
  `vault`, `expirationSeconds` 600–86 400, default 3600) projects a second token into the worker pod alone,
  read-only at `/run/secrets/eacp-vault-identity/token`, under the worker's own ServiceAccount. It is separate
  from §3a's token, so neither relying party can replay a token meant for the other. Egress to Vault is
  declared in `worker.connectorEgress`.

KV v2 only: dynamic secrets and leases, Vault Agent, response wrapping, KV v1 and renewing or revoking the
Vault token are out of scope; the worker logs in again instead of renewing.

### 4. Redaction

`logging.SecretSet` holds the values a logger redacts and may grow after the logger is built
(`logging.NewWithSet`). `service.Deps` owns one set (`Redaction()`); `RedactSecrets` adds permanent values.
The worker adds each client secret permanently, each client assertion until its `exp` plus 24 h and each minted
token until its real expiry plus 24 h, keeping at most 10 000 temporary values (the oldest go first). Rev 1.3
adds every Vault-held value permanently and each Vault token until its lease end plus 24 h.

### 5. Failure handling

- **Worker.** Without a credential the lease is released before the dispatch intent, with reason
  `credential unavailable` (or `worker cannot serve this connector` for a missing binding). No call is made,
  no attempt is recorded and the circuit is unchanged: the target was never contacted. For
  `ErrCredentialTooShort` the reason is `credential lifetime shorter than the call`, and that worker leaves the
  action alone for a minute while it keeps serving the binding's other actions.
- **Reconciler.** Nothing is looked up and no check is recorded; the lease lapses and the action waits for its
  next reconciliation (T33). An unavailable credential never counts as evidence.
- **MCP scanner.** The scan is not recorded (not even as failed); its lease expires and it is retried.

### 6. Fake ERP token endpoint (demo and tests)

With `EACP_FAKEERP_OAUTH_CLIENT_ID`, `EACP_FAKEERP_OAUTH_CLIENT_SECRET_FILE` and `EACP_FAKEERP_OAUTH_TTL`
(default 300 s, 1 s–1 h), Fake ERP serves `POST /oauth/token` under §3's contract. It issues random 32-byte
base64url tokens and keeps only their SHA-256 and expiry, in its durable operation log, so tokens survive a
restart like every effect. An unexpired token authorises as principal `oauth:<client_id>`; issuance and
refusals are audited with the hash and expiry, never the token. Compose and the Kubernetes dev manifests bind
tenant `…00a4`, `secret_ref` `fakeerp-jit`, to it; the TTL is 300 s because a 60 s token could never outlive
the default 30 s call plus the 30 s skew.

Rev 1.1: with `EACP_FAKEERP_OAUTH_FEDERATED_CLIENT_ID`, `_ISSUER`, `_AUDIENCE`, `_SUBJECT` and `_JWKS_FILE`
(all or none), Fake ERP also serves a federated client. It verifies the assertion with the standard library:
header `alg` `RS256` only and a known `kid`, the signature, `iss` and `sub` exactly, `aud` (a string or an
array) containing the audience, and `exp`, `nbf` and `iat` with 30 s skew; a request using Basic auth and an
assertion together is refused (`400 invalid_request`), a failed check is `401 invalid_client`. A token issued
this way records the assertion's SHA-256 in the audit, never the assertion. On Kubernetes the e2e script reads
the cluster's real issuer and JWKS into Fake ERP's configuration and binds tenant `…00a5`, `secret_ref`
`fakeerp-wif`, to the worker's projected token (`deployments/k8s/connector-secrets.federated.json`). Compose
has no platform issuer and keeps the Rev 1.0 client-secret tenant only.

Rev 1.2: with `EACP_FAKEERP_OAUTH_KEY_CLIENT_ID`, `_KEY_AUDIENCE` (the token URL as the worker names it) and
`_KEY_JWKS_FILE` (all or none), Fake ERP serves a key client with a different id from the federated one. It
reads the JWKS's RSA and EC P-256 keys that carry a `kid` or `x5t#S256` and ignores other or malformed keys
(RFC 7517 §5). An assertion must use `RS256`, `PS256` or `ES256` with a key of the matching type, found by
`kid` and else by `x5t#S256`; verify; have `iss` = `sub` = the client id and the audience; `exp` in the future
and at most an hour ahead (as Okta), `nbf` and `iat` not in the future (30 s skew); and a `jti` never seen.
Accepted `jti`s are recorded in the issuance audit (`assertion_jti`) and rebuilt from the durable log at start,
so a restart cannot reopen a replay. Header and claim parsing is strict for both assertion clients: exact
member names, and NumericDates that are JSON numbers, never strings. Compose and Kubernetes bind tenant
`…00a6`, `secret_ref` `fakeerp-pkjwt`, to it with a PS256 key and certificate from `eacpctl dev-client-key`
(development and test only): on compose the key reaches the worker alone through the `client_key` volume, and
Fake ERP gets only the JWKS; on Kubernetes the e2e script inlines the key into the worker's secrets file.

Rev 1.3: a DEVELOPMENT-ONLY Vault (`hashicorp/vault:2.1.1` in dev mode: in memory, a fixed dev root token)
holds tenant `…00a7`'s two credentials: `fakeerp-vault` (`value_vault` `eacp/fakeerp#token`, the ERP's static
token) and `fakeerp-vault-oauth` (the Rev 1.0 client with `client_secret_vault` `eacp/fakeerp-oauth#client_secret`).
Its init writes both values from the dev secret files and a policy that can only read `secret/data/eacp/*`. On
compose, Vault is on an internal `vault` network joined by Vault, its one-shot init and the worker; the init
enables AppRole and writes `role_id` and `secret_id` into the `vault_approle` volume, mounted read-only into
the worker alone. On Kubernetes (`deployments/k8s/dev/vault.yaml`), Vault reviews tokens with its own service
account (`system:auth-delegator`); its init Job enables Kubernetes auth with role `eacp-worker` bound to
service account `eacp-worker` in namespace `eacp` and audience `vault`, and a NetworkPolicy admits only the
worker and the Job. The e2e script replaces the merged manifest's `vault` block with Kubernetes auth
(`deployments/k8s/connector-secrets.vault.json`).

### 7. Proof

- `internal/logging`: `TestALoggerRedactsASecretAddedAfterItWasBuilt`, `TestTemporarySecretsExpireAndAreBounded`.
- `internal/worker` unit tests (`oauth2_test.go`): reuse while the token outlives the call, a fresh mint when
  it would not, every refused response, no redirects, one mint under concurrency, back-off and its one-minute
  cap, dropping a rejected token, redaction, host binding; `TestInvalidOAuthEntriesRejectTheWholeFile`.
- `internal/worker` PostgreSQL tests (`jit_integration_test.go`) with the real HTTP connector and Fake ERP:
  three calls with one minted token and no token or client secret in the database or logs; a token shorter
  than the call never dispatches, and a long call does not hold up short calls on the same binding
  (`TestALongCallDoesNotHoldUpShortCallsOnTheSameBinding`); the token sent is scrubbed even after the cache
  rotated (`TestTheTokenSentIsScrubbedEvenAfterTheCacheRotates`); a failing token endpoint withholds claims until the back-off passes; a
  refused token is replaced; the reconciler looks up with a minted token and records nothing while the
  credential is unavailable; `TestAScanWithoutACredentialIsNotRecorded`.
- `internal/fakeerp`: issuance, expiry, refused clients and grants, tokens across a restart.
- `test/demo` `TestJITDemo` (compose and Kubernetes): purchases as `oauth:eacp-worker`, one PO each; the client
  secret and every issued token (found by hashing every 43-character window of base64url text) are absent
  from API responses, service logs and a database dump.
- `test/security`: the agent cannot reach the token endpoint; only Fake ERP mounts the OAuth client secret;
  an unauthenticated token request gets 401.
- Rev 1.1, `internal/worker` (`federation_test.go`): the assertion request's shape without Basic auth
  (`TestAnAssertionAuthenticatesTheTokenRequest`), a rotated file read at the next mint
  (`TestEveryMintReadsTheCurrentAssertion`), trimming, each unusable assertion backs off without a request
  (`TestAnUnusableAssertionFailsTheMintAndBacksOff`), redaction and scrubbing, and every invalid assertion
  file rejecting the whole secrets file; `oauth2_test.go`: `TestALongLivedTokenIsUsedForAtMostAnHour`,
  `TestTheCapDecidesWhetherATokenIsTooShort`; `jit_integration_test.go`:
  `TestTheWorkerExecutesThroughWorkloadIdentityFederation` (real HTTP connector, Fake ERP with a federated
  client, no assertion or token in the database or logs).
- Rev 1.1, `internal/fakeerp` (`federation_test.go`): issuance with a valid assertion and every refusal;
  `test/helm` (`identity_test.go`): the worker's own ServiceAccount and the projected token in the worker only;
  `test/demo` `TestFederatedJITDemo` on minikube with the cluster's issuer: purchases as
  `oauth:eacp-worker-wif`, and no issued token and no JWT of the worker's service account in responses, logs
  or a database dump.
- Rev 1.2, `internal/worker` (`privatekeyjwt_test.go`): each algorithm's signature verified with the public
  key, the header naming the key, a new `jti` on every mint, the key redacted and never in `Values()`, inline and
  CRLF PEM, every invalid entry rejecting the whole file, and a certificate outside its validity failing the
  mint without a request (`TestACertificateOutsideItsValidityFailsTheMintNotTheFile`); `jit_integration_test.go`:
  `TestTheWorkerExecutesWithPrivateKeyJWT` (real HTTP connector, Fake ERP with a key client, no key or token in
  the database or logs).
- Rev 1.2, `internal/fakeerp` (`keyclient_test.go`): issuance by `kid` and `x5t#S256` for each algorithm, every
  refusal, a `jti` refused a second time and after a restart, `ParseKeySet`; `cmd/eacpctl`: `dev-client-key`;
  `test/security`: only the worker mounts the key, only Fake ERP the JWKS; `test/demo` `TestPrivateKeyJWTDemo`
  (compose and Kubernetes): purchases as `oauth:eacp-worker-pkjwt`, a distinct `jti` per issuance, and no
  token, private key or assertion in responses, logs or a database dump.
- Rev 1.3, `internal/worker` with an in-process fake Vault (`vault_test.go`): `TestVaultAppRoleLogin` (with the namespace header),
  `TestVaultKubernetesLoginRereadsTheJWT` (the file read at every login),
  `TestVaultTokenIsReusedThenRenewedByLogin` (two thirds of the capped lease),
  `TestVaultCachesAPathForTheRefreshInterval`, `TestVaultFailureClasses` (each class; a 403 drops the token), `TestVaultNeverFollowsARedirect`,
  `TestVaultOneLoginAndOneReadUnderConcurrency`, `TestVaultNeverLogsATokenOrValue`; `vaultsecrets_test.go`:
  `TestAStaticCredentialComesFromVault` (a rotation within one refresh, the previous value scrubbed until the
  next), `TestAVaultFailureBacksTheBindingOff`, `TestAStaleVaultValueIsNeverServed`,
  `TestARejectedVaultCredentialIsReadAgain`, `TestTheWorkerStartsWithVaultDown`,
  `TestInvalidVaultEntriesRejectTheWholeFile`; `vaultoauth_test.go`: `TestAnOAuthClientSecretComesFromVault`,
  `TestAPrivateKeyComesFromVault` (a rotated key rebuilds the signer, a bad PEM backs off),
  `TestInvalidOAuthVaultEntriesRejectTheWholeFile`; PostgreSQL (`vault_integration_test.go`):
  `TestTheWorkerExecutesWithAVaultCredential` (the real HTTP connector and Fake ERP; a stale value is refused,
  the rotated one is read at once and used, exactly two KV reads, no Vault token or value in the database or
  logs).
- Rev 1.3, `test/helm`: `TestTheVaultIdentityIsTheWorkersOwn` and the render-time refusals; `test/security`:
  `TestAgentCannotReachVault`, `TestTheVaultAppRoleIsMountedOnlyIntoTheWorker`,
  `TestOnlyTheWorkerSharesTheVaultNetwork`; `test/demo` `TestVaultDemo` (compose with AppRole, Kubernetes with
  Kubernetes auth): purchases through both bindings as `execution-worker` and `oauth:eacp-worker`; a value
  soft-deleted in Vault withholds the next purchase (`QUEUED`, no attempt, no PO) after one refresh interval,
  and restoring it lets the purchase succeed; no Vault token (`hvs.`), Vault-held value or issued token in
  responses, logs or a database dump.

## Consequences

- A connector moves to just-in-time credentials by changing its secrets-file entry; the registry, policies
  and contracts do not change.
- A token endpoint outage stops that binding's work without churning leases or tripping the target's circuit.
  Work waits in `QUEUED`; it is never failed for a missing credential.
- Each worker replica mints its own tokens (ADR-029: replicas share nothing that decides).
- With workload identity federation (Rev 1.1) the worker holds no long-lived secret for the binding: the
  platform rotates the assertion, and revoking the federated credential at the IdP stops new tokens.
- With `private_key_jwt` (Rev 1.2) the IdP holds only a public key: a leak of its client registration
  cannot authenticate as the worker, and the worker's key never leaves it.
- With Vault (Rev 1.3) a credential is rotated in Vault, not in the worker's file, and needs no restart; on
  Kubernetes the worker's only Vault secret is a projected token the platform rotates. A Vault outage longer
  than the refresh interval withholds the affected bindings' work, exactly like a token endpoint outage.
- SPIFFE JWT-SVIDs, token exchange (RFC 8693), AWS and GCP STS, Vault dynamic secrets and HSM/KMS-held keys
  (later phases) implement the same seam: a provider that returns a credential valid for `validFor`, reports
  availability and drops a rejected value.

## Unresolved assumptions (conservative choices)

| Assumption | Choice |
|---|---|
| Where providers run | Inside the worker only; a broker sidecar is deferred. |
| Client authentication | `client_secret_basic`, a platform assertion (Rev 1.1) or `private_key_jwt` (Rev 1.2); mTLS is deferred. |
| Token lifetime | `expires_in` is required, 1–86 400 s; a token is used for at most 3600 s and scrubbed and redacted until its real expiry (Rev 1.1; Rev 1.0 refused anything above 3600 s). |
| Reuse | Only while the token outlives the whole call (`validFor`); never persisted. |
| A token shorter than the call | Unavailable for that call only: no back-off, no new mint for calls as long, the action deferred by that worker for a minute, logged at error level with both durations. |
| Reconciling while the credential is unavailable | Nothing is looked up; the lapsed lease still counts as a reconciliation attempt (T33), so a long IdP outage can hand an unknown outcome to a human sooner (T34). |
| Token endpoint failure | No dispatch, lease released, binding withheld from claims with a 1–60 s back-off. |
| The target rejects a token | Classified as before (ADR-004); the worker and scanner drop the token. The reconciler's lookup carries no error class, so a refused token is reused for lookups until it expires. |
| Redirects from the token endpoint | Refused. |
| Plain `http` token endpoint | Only when `EACP_ENV` is `development` or `test`. |
| Credential evidence in PostgreSQL | None; logs carry the binding and lifetime, never a value. |
| Minted tokens in the redactor | Kept until expiry + 24 h, at most 10 000. |
| Who validates a client assertion | The IdP only; the worker reads `exp` and nothing else (Rev 1.1). |
| When the assertion is read | At every mint; never reused from an earlier read. |
| An assertion close to expiry | Refused with less than 10 s left: a failed mint and back-off. |
| Which service account the worker federates as | Its own, `<release>-worker`; no other chart workload can obtain its token. |
| Projected token lifetime | 600–86 400 s, default 3600 s (as Azure Workload Identity). |
| Assertion replay at Fake ERP | Not checked for the federated client (Entra ID accepts the same platform assertion until it expires); the key client accepts each `jti` once (Rev 1.2). |
| Fake ERP's keys | A JWKS captured at install; key rotation needs a reinstall (demo only). |
| Signed assertion lifetime | 300 s (Entra ID advises 5–10 minutes; Okta allows at most an hour) (Rev 1.2). |
| Assertion audience | Always the token URL; no override. |
| `jti` | 128 random bits per request, never reused. |
| Key rotation | Restart the worker; the key and certificate are read once. |
| Certificate validity | Checked at every mint: outside its validity period the binding's mints fail (`certificate_not_valid`) and back off; the secrets file still loads, so other bindings keep working. |
| Signing algorithms | RS256, PS256 and ES256 only; the key type must match. |
| The key in logs | Its PEM and each of its base64 lines of 16 or more characters are redacted permanently; it never enters `Values()`. |
| Fake ERP's `jti` record | Kept for the life of its log (demo scale). |
| Vault down at start | The worker starts; bindings that need Vault back off until it answers (Rev 1.3). |
| Stale Vault values | Never served past `refresh_seconds`; a failed read means no credential. |
| Refresh interval | 300 s by default, 30–3600 s. |
| Vault token lifetime | Used for two thirds of `min(lease_duration, 3600)`, then a new login; never renewed or persisted. |
| A 403 from Vault | Drops the Vault token; the binding backs off; the next attempt logs in again. |
| Rotation on rejection | A static Vault binding re-reads Vault after the target answers `unauthorized`; an OAuth binding re-reads its client secret after a failed mint. |
| Soft-deleted or destroyed version | Treated as missing: no credential. |
| Two tokens on Kubernetes | Separate audiences for the ERP's IdP and Vault; neither can replay the other's. |
| Vault in the demo | Dev mode, in memory, a dev-only root token known to Vault, its init and the demo's CLI only; on compose the AppRole `secret_id` has no TTL or use limit, because the worker reuses it at every login. |
