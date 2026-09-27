# ADR-019: Credential custody — providers and just-in-time credentials

Status: Accepted (Rev 1.5, 2026-09-27). Phases 24a (Rev 1.0), 24b (Rev 1.1), 24c (Rev 1.2), 24d (Rev 1.3),
24e (Rev 1.4) and 24f (Rev 1.5).
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

Rev 1.4 (Phase 24e) gives the worker an identity that does not depend on its platform: a SPIFFE JWT-SVID.
Rev 1.1's projected token has one issuer (the cluster) and one fixed audience per mounted token; the SPIRE
agent issues a JWT-SVID for whatever audience the worker names, when it asks. A binding may now present the
worker's JWT-SVID straight to a SPIFFE-aware target as its Bearer credential, or as the client assertion of
an OAuth mint, and hold no secret at all (§3d).

Rev 1.5 (Phase 24f) lets a binding trade those identities for a token instead of authenticating a client with
them. Clouds and enterprise IdPs issue access tokens in exchange for a workload's own identity token: GCP
Workload Identity Federation (an STS, then optionally a service account's token from IAM Credentials),
Keycloak and Okta token exchange. An `oauth2` binding may now mint through an RFC 8693 token exchange of the
worker's projected token or JWT-SVID, optionally impersonating a GCP service account, with no long-lived
secret (§3e).

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
exactly one of `value`, `value_file` (a static credential, as before) or `oauth2` (a provider); Rev 1.3 adds
`value_vault` and Rev 1.4 `value_spiffe`:

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
  before the request, with one read per path at a time; callers queued behind a read that fails get its
  failure without a request of their own. A 403 drops the Vault token. Nothing is retried within one call or
  mint.
- **Failure classes:** `vault_login_unreadable`, `vault_login`, `vault_forbidden` (403), `vault_read`
  (transport, 5xx, non-JSON), `vault_missing` (404, no data, a soft-deleted or destroyed version, or the key
  absent) and `vault_invalid` (not a string, or not valid for the field it feeds).
- **A static credential** is the cached value while it is fresh, else a new read; it must be 1–4096 bytes. A
  failure makes the binding unavailable with §5's back-off (1 s doubling to 60 s, counted from the end of the
  failed read, and grown once however many callers shared it), omitted from `Available()`.
  A stale value is never served: past its refresh deadline a failed read means no credential. When the target
  answers `unauthorized`, the binding drops its cached path, so a rotation takes effect at the next call.
- **A client secret, key or certificate** from Vault is read inside each mint, before the token request; a
  failure is a failed mint with the Vault class. A key or certificate is parsed with §3b's rules whenever its
  text changes (the signer is rebuilt; a bad PEM is `vault_invalid`). A failed mint drops the provider's cached
  Vault paths (outside the provider's lock, which `Available()` and `Values()` need), so a rotated client secret
  is read at the next mint.
- **Redaction and scrubbing:** every value read from Vault is redacted permanently, once (`AddPermanent` ignores
  a value it already holds, so the set does not grow with use) (a PEM also line by line,
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

### 3d. SPIFFE JWT-SVIDs (Rev 1.4)

A top-level `spiffe` object configures one Workload API client per worker (go-spiffe v2.8.2):

```json
{"spiffe": {"endpoint": "unix:///spiffe-workload-api/spire-agent.sock",
            "spiffe_id": "spiffe://eacp.test/ns/eacp/sa/eacp-worker"},
 "secrets": [
   {"tenant_id": "…", "secret_ref": "erp-spiffe", "host": "erp.internal:8443",
    "value_spiffe": {"audience": "erp-api"}},
   {"tenant_id": "…", "secret_ref": "erp-spiffe-oauth", "host": "erp.internal:8443",
    "oauth2": {"token_url": "https://idp.internal/token", "client_id": "eacp-worker",
               "client_assertion_spiffe": {"audience": "api://AzureADTokenExchange"}}}]}
```

- **At load** (any failure rejects the whole file; errors never contain a value): `endpoint` is `unix://` with
  an absolute path and nothing else, or, only when `EACP_ENV` is `development` or `test`, `tcp://` to a
  loopback IP literal with a port (the Workload API authenticates its caller by process, which a TCP listener
  cannot do). `spiffe_id` is required: a SPIFFE ID of at most 2048 bytes with a path. `value_spiffe` is a
  fourth static-credential form (exclusive with `value`, `value_file`, `value_vault` and `oauth2`);
  `client_assertion_spiffe` a sixth client-authentication form (exclusive with `client_secret`,
  `client_secret_file`, `client_secret_vault`, `client_assertion_file` and `private_key_jwt`). An audience is
  1–256 printable ASCII characters without spaces. A `_spiffe` field needs the `spiffe` block. **The worker
  never contacts the agent at load**, so it starts with the agent down.
- **The client** (`internal/worker/spiffe.go`): the go-spiffe client is created at the first fetch, and again
  at the next fetch if creating it failed. A fetch asks for the file's `spiffe_id` as the request's subject
  and one audience, with a 10 s timeout. Per audience the latest SVID is cached and answers while it outlives
  the caller's minimum; otherwise there is one fetch per audience at a time, and callers queued behind a fetch
  that fails get its failure without a fetch of their own. Beyond go-spiffe's own checks (a SPIFFE-ID `sub`,
  the audience, `exp` against the real clock, the algorithm) the worker requires `sub` to equal `spiffe_id`
  (a registration mistake never lends it another identity), a token of at most 16 KiB of printable ASCII
  without spaces, and an `exp` after the fetch started. It never verifies the signature: only the relying
  party judges it. Fetches are logged with the audience and the class or remaining lifetime only.
- **Failure classes:** `spiffe_unavailable` (the client cannot be created, transport errors, `Unavailable`,
  `DeadlineExceeded` and any other gRPC status), `spiffe_denied` (`PermissionDenied`: no registration entry
  matches the worker), `spiffe_invalid` (no SVID, a parse failure, the wrong `sub`, a bad token shape) and
  `spiffe_expiring` (a `value_spiffe` SVID with less than 10 s left, e.g. the agent's cached copy while its
  server is unreachable).
- **`value_spiffe`**: `Credential(…, validFor)` asks for an SVID living at least `validFor`. A failure makes
  the binding unavailable with §5's back-off (1 s doubling to 60 s, counted from the end of the failed fetch
  and grown once however many callers shared it); `Available()` omits it. A fetched SVID that lives at least
  10 s but not beyond the call is `ErrCredentialTooShort`: kept for shorter calls, no back-off, and an equally
  long call fails at once while that SVID lives; once it has expired the next call fetches again, so a stale
  copy never withholds the binding for longer than its own life. `Rejected` (the target answered
  `unauthorized`) drops the cached SVID so the next call fetches again (the agent may return the same one);
  the attempt's outcome is never reclassified. `Values()` holds the current SVID until its `exp` and the one it
  replaced until that one's `exp`.
- **`client_assertion_spiffe`**: each mint asks for an SVID living at least the token request timeout (10 s)
  and sends it exactly as §3a sends an assertion (RFC 7523 §2.2, `client_id`, no `Authorization` header). A
  failure is a failed mint with the `spiffe_*` class (§3's back-off, withheld from claims); an SVID with less
  than 10 s left is `assertion_expired`. A rejected token also drops the SVID. Tokens are cached, capped at an
  hour and redacted exactly as before.
- **The SVID's TTL.** The SPIRE agent hands out a cached SVID until it is at about half its life (±10 %), so
  an SVID may arrive with 0.4 of its TTL left. Operators give the worker's registration entry a JWT-SVID TTL
  of at least 2.5 × (the longest call budget + `CredentialSkew`), 825 s for the 300 s maximum budget; the
  demo uses 3600 s.
- **Kubernetes** (ADR-029, the chart): `worker.spiffe` (`enabled` default false, `csiDriver` default
  `csi.spiffe.io`) adds an inline, read-only `csi` volume to the worker pod alone, mounted at
  `/spiffe-workload-api`, where the SPIFFE CSI driver places the agent's socket. A `csi` volume is allowed by
  the `restricted` Pod Security Standard; a hostPath socket would not be. No egress rule is needed.

X509-SVIDs and mTLS to connectors, SPIFFE federation between trust domains, verifying SVIDs in the worker and
watching the Workload API (the worker fetches on demand) are out of scope.

### 3e. Token exchange (Rev 1.5)

The `oauth2` provider gains a grant; everything §3 says about the token it returns still applies:

```json
{"tenant_id": "…", "secret_ref": "gcs", "host": "storage.googleapis.com:443",
 "oauth2": {"grant": "token_exchange",
            "token_url": "https://sts.googleapis.com/v1/token",
            "audience": "//iam.googleapis.com/projects/123/locations/global/workloadIdentityPools/eacp/providers/k8s",
            "scope": "https://www.googleapis.com/auth/cloud-platform",
            "subject_token": {"file": "/run/secrets/eacp-identity/token"},
            "impersonate": {"url": "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/eacp@p.iam.gserviceaccount.com:generateAccessToken",
                            "scope": ["https://www.googleapis.com/auth/devstorage.read_only"],
                            "lifetime_seconds": 3600}}}
```

- **At load** (any failure rejects the whole file; the worker contacts nothing): `grant` is absent or
  `client_credentials` (unchanged) or `token_exchange`; any other value is an error, and `subject_token`,
  `subject_token_type`, `audience` and `impersonate` are refused without `token_exchange`. `subject_token` is
  exactly one of `file` (a path whose content is checked for shape only, as `client_assertion_file`) or
  `spiffe` (`{"audience": …}`, §3d's audience rules, needs the `spiffe` block). `subject_token_type` defaults
  to `urn:ietf:params:oauth:token-type:jwt`; `…:id_token` and Google's `…:id-token` are also accepted.
  `audience` is 1–1024 printable ASCII characters without spaces; `scope` and `resource` keep §3's rules.
  Client authentication at the STS is optional (RFC 8693 and GCP's STS need none): no `client_id` and no
  form, a public client (`client_id` alone, sent in the form body), or `client_id` with exactly one of §3's
  forms. `impersonate.url` is absolute, `https` (`http` only in development and test), with a lowercase host
  and no user info, query or fragment, and its path is exactly
  `/v1/projects/-/serviceAccounts/<email>:generateAccessToken` (`<email>` 3–254 characters), so a mistyped URL
  fails at load rather than receiving a federated token. `impersonate.scope` lists 1–32 scope tokens;
  `lifetime_seconds` is 300–3600, default 3600.
- **A mint** (one at a time per binding, as §3):
  1. *The subject token.* A `file` is read again at every mint with §3a's rules (`assertion_unreadable`,
     `assertion_invalid`, `assertion_expired` with less than 10 s left). A `spiffe` subject is §3d's SVID for
     its audience, living at least the request timeout (`spiffe_*`; less than 10 s left is
     `assertion_expired`); a cached SVID that outlives the exchange is reused. It is redacted until its `exp`
     plus 24 h and kept in `Values()` until its `exp`.
  2. *The exchange.* `POST token_url` with `grant_type=urn:ietf:params:oauth:grant-type:token-exchange`,
     `subject_token`, `subject_token_type`, `requested_token_type=urn:ietf:params:oauth:token-type:access_token`
     and `audience`, `scope` and `resource` when set, plus the configured client authentication (a Vault-held
     secret or key is read inside the mint, §3c). The response passes §3's checks, and `issued_token_type` must
     be `urn:ietf:params:oauth:token-type:access_token` (else `wrong_token_type`): a JWT or refresh token is
     never sent as a Bearer. `expires_in` stays required although RFC 8693 only recommends it.
  3. *Impersonation* (when set). `POST impersonate.url` with `Authorization: Bearer <federated token>`, JSON
     `{"scope": [...], "lifetime": "<n>s"}`, its own 10 s timeout, no redirects and a response of at most
     64 KiB. HTTP 200 is required (else `impersonation_http_<code>`); the JSON needs a printable `accessToken`
     of at most 8192 bytes and an RFC 3339 `expireTime` after the request started and at most 86 400 s later
     (else `impersonation_invalid`); a transport error is `transport`. The federated token goes nowhere else,
     is never cached for a later mint, and is redacted until its expiry plus 24 h and kept in `Values()` until
     its expiry.
  4. *The final token* (the exchange's, or the impersonated one) is §3's token: used for at most an hour,
     never for a call it could expire during, dropped when the target rejects it, redacted until its real
     expiry plus 24 h.

  Each token goes to one place only: the subject token to the STS, the federated token to the impersonation
  endpoint, the final token to the connector. A failed step fails the mint with its class: §3's back-off,
  withheld from claims, no dispatch and no retry within the mint. The mint's log names the binding, the token
  endpoint's host, the grant, whether it impersonated (and then the impersonation host, `impersonation_host`)
  and the lifetime or failure class, never a token.
- **Kubernetes.** Nothing new in the chart: a file subject is the worker's projected token (§3a's
  `worker.workloadIdentity`), a SPIFFE subject comes through §3d's socket. The impersonation endpoint's host
  belongs in `worker.connectorEgress` beside the token endpoint's.

AWS STS and SigV4, `actor_token` (delegation), `delegates`, refresh tokens, caching the federated token
across mints, GCP workforce pools and STS `options` are out of scope.

### 4. Redaction

`logging.SecretSet` holds the values a logger redacts and may grow after the logger is built
(`logging.NewWithSet`). `service.Deps` owns one set (`Redaction()`); `RedactSecrets` adds permanent values.
The worker adds each client secret permanently, each client assertion until its `exp` plus 24 h and each minted
token until its real expiry plus 24 h, keeping at most 10 000 temporary values (the oldest go first). Rev 1.3
adds every Vault-held value permanently and each Vault token until its lease end plus 24 h. Rev 1.4 adds each
JWT-SVID received until its `exp` plus 24 h. Rev 1.5 adds each subject token until its `exp` plus 24 h and
each federated token until its expiry plus 24 h.

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

Rev 1.4: `fakeerp.ParseSPIFFEBundle` reads the `jwt-svid` keys (RSA or P-256, with a `kid`) of a SPIFFE
bundle. With `EACP_FAKEERP_OAUTH_SPIFFE_CLIENT_ID`, `_ISSUER`, `_AUDIENCE`, `_SUBJECT` and `_BUNDLE_FILE`
(all or none) the token endpoint accepts an RFC 7523 assertion that is a JWT-SVID: RS256 or ES256 by `kid`,
exactly that `iss` and `sub`, the audience in `aud`. With `EACP_FAKEERP_SPIFFE_AUDIENCE`, `_SUBJECT` and
`_BUNDLE_FILE` (all or none) the ERP API accepts a JWT-SVID as its Bearer, verified the same way without an
issuer; its principal is `spiffe:<sub>` and the audit records the SVID's SHA-256 (`svid_sha256`), never the
SVID. A JWT-SVID is a bearer token: it can be replayed until `exp`, and its audience limits where. On
Kubernetes only, a DEVELOPMENT-ONLY SPIRE 1.15.3 (`deployments/k8s/dev/spire.yaml`: one server with
SQLite on an emptyDir, trust domain `eacp.test`, `jwt_issuer` `https://spire.eacp.test`, `k8s_psat` node
attestation; an agent per node; the SPIFFE CSI driver) issues the worker
`spiffe://eacp.test/ns/eacp/sa/eacp-worker` (selectors `k8s:ns:eacp`, `k8s:sa:eacp-worker`, JWT-SVID TTL
3600 s). The e2e script snapshots the bundle into Fake ERP's `fakeerp-federation` ConfigMap and binds tenant
`…00a8` `fakeerp-spiffe` (audience `fakeerp-api`) and `fakeerp-spiffe-oauth` (client `eacp-worker-spiffe`,
audience `fakeerp-token`) (`deployments/k8s/connector-secrets.spiffe.json`). Compose has no SPIRE.

Rev 1.5: with `EACP_FAKEERP_STS_AUDIENCE` and at least one subject group, `EACP_FAKEERP_STS_K8S_{ISSUER,
AUDIENCE,SUBJECT,JWKS_FILE}` or `EACP_FAKEERP_STS_SPIFFE_{ISSUER,AUDIENCE,SUBJECT,BUNDLE_FILE}` (each all or
none), the token endpoint also serves the token-exchange grant. Client authentication is optional; a request
that presents Basic or an assertion must authenticate with it. `subject_token_type` must be one of the three
JWT types, `requested_token_type` absent or an access token, and `audience` exactly the configured one
(`invalid_request`, `invalid_target`). The subject token must verify against a configured subject (RS256 or
ES256 by `kid`, the issuer, the exact `sub`, the audience in `aud`, `exp` and `nbf` with 30 s skew), else
`invalid_grant`. The token issued has principal `sts:<sub>`, the reply carries `issued_token_type`, and the
audit records the subject token's SHA-256 (`subject_sha256`), never the token. With
`EACP_FAKEERP_IMPERSONATE_ACCOUNTS` (a comma-separated list; needs the exchange) Fake ERP serves
`POST /v1/projects/-/serviceAccounts/{email}:generateAccessToken`: a caller that is not an `sts:` token gets
401, an unknown account 403, a body other than 1–32 non-empty scopes and a lifetime of 1–3600 s 400; it issues
a token of that lifetime with principal `sa:<email>` and answers `{"accessToken", "expireTime"}`. `sts:` and
`sa:` tokens authorise the ERP API like the others. On Kubernetes the e2e script configures both subjects
from the cluster's issuer and JWKS and the SPIRE bundle it already captures, and binds tenant `…00a9`
`fakeerp-sts` (the projected token, audience `fakeerp`, exchanged at audience `fakeerp-sts` with no client
authentication) and `fakeerp-sts-sa` (the JWT-SVID for `fakeerp-sts`, then impersonating
`eacp-erp@eacp-demo.iam.gserviceaccount.com`) (`deployments/k8s/connector-secrets.exchange.json`). Compose
has no platform issuer and is unchanged.

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
  `TestInvalidOAuthVaultEntriesRejectTheWholeFile`; `vaultreview_test.go`:
  `TestRepeatedVaultReadsKeepTheRedactionSetConstant`, `TestConcurrentCallersShareAFailedVaultRead`,
  `TestASlowVaultFailureStillBacksOff`, `TestAFailedMintDoesNotBlockAvailability`; `internal/logging`
  `TestAddingAPermanentValueAgainKeepsOneEntry`; PostgreSQL (`vault_integration_test.go`):
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

- Rev 1.4, `internal/spiffetest`: a fake Workload API over loopback TCP that refuses a call without the
  `workload.spiffe.io` header (`TestTheFakeAgentRequiresTheHeader`). `internal/worker` (`spiffe_test.go`):
  `TestSPIFFEBlockLoads` (no contact at load), `TestSPIFFEBlockFailsClosed` (every field and exclusivity
  rejecting the whole file), `TestTCPEndpointOnlyInDevelopment`; `spiffefetch_test.go`:
  `TestAnSVIDIsFetchedOnceAndCached` (one fetch under concurrency), `TestAShortRemainingLifeFetchesAgain`,
  `TestAResponseMustBeTheWorkersSVIDForTheAudience`, `TestQueuedCallersShareAFailedFetch`,
  `TestEverySVIDIsRedacted`; `spiffevalue_test.go`: `TestAnAgentFailureWithholdsTheBinding` (each class, the
  back-off and `Available()`), `TestAnSVIDShorterThanTheCallIsTooShort`, `TestATooShortSVIDRecoversWhenItExpires`,
  `TestAShortSharedSVIDDoesNotWedgeTheSecondBinding`, `TestAnSVIDAboutToExpireIsNeverSent`,
  `TestBindingsSharingAnAudienceFetchOnce`, `TestRejectedDropsOnlyTheCurrentSVID`,
  `TestValuesHoldTheSVIDsUntilTheyExpire`; `spiffeoauth_test.go`: `TestASPIFFEAssertionAuthenticatesTheMint`,
  `TestEveryMintFetchesAnAssertionOnlyWhenNeeded`, `TestASPIFFEFailureIsAFailedMint`,
  `TestTheSVIDAssertionIsRedactedAndScrubbed`; PostgreSQL (`spiffe_integration_test.go`):
  `TestTheWorkerExecutesWithAnSVID`, `TestTheWorkerMintsWithAnSVIDAssertion` (the real HTTP connector and Fake
  ERP, no SVID or token in the database or logs) and `TestAnAgentRefusalWithholdsWork` (nothing dispatched and
  the agent not asked again during the back-off).
- Rev 1.4, `internal/fakeerp` (`spiffe_test.go`): `TestParseSPIFFEBundleKeepsOnlyJWTAuthorities`, the SPIFFE
  client and the SVID bearer accepting a good SVID and refusing a wrong `kid`, algorithm, signature, issuer,
  subject, audience or an expired one (`TestTheSPIFFEClientRefusesBadSVIDs`, `TestAnSVIDBearerMustMatch`),
  `TestSPIFFEOptionsFailClosed`; `cmd/fakeerp` `TestSPIFFESettingsComeTogether`; `test/helm` `TestTheSPIFFESocketIsTheWorkersOwn`
  and the `csiDriver` refusal; `test/demo` `TestSPIFFEDemo` on minikube against a real SPIRE: purchases as
  `spiffe:spiffe://eacp.test/ns/eacp/sa/eacp-worker` (with an SVID digest) and `oauth:eacp-worker-spiffe`, and
  no SVID, JWT naming the worker's SPIFFE ID or issued token in responses, logs or a database dump.
- Rev 1.5, `internal/worker` (`exchange_test.go`): `TestATokenExchangeEntryLoads` and
  `TestInvalidTokenExchangeEntriesRejectTheWholeFile` (every field, grant exclusivity, client authentication,
  the impersonation URL, dev-only `http`); `exchangemint_test.go`: `TestAnExchangeSendsTheSubjectToken`,
  `TestExchangeClientAuthentication` (public client, client secret, client assertion),
  `TestAnSVIDSubjectIsFetchedPerMint`, `TestTheSubjectTokenMustOutliveTheExchange`,
  `TestEveryInvalidExchangeResponseIsRefused` (including `wrong_token_type`),
  `TestExchangeTokensAreRedactedAndScrubbed`; `impersonate_test.go`: `TestImpersonationTradesTheFederatedToken`,
  `TestEachTokenGoesOnlyToItsHop`, `TestEveryInvalidImpersonationResponseIsRefused`,
  `TestAnImpersonatedTokenIsUsedForAtMostAnHour`, `TestTheImpersonationEndpointMayNotRedirect`,
  `TestTheFederatedTokenIsRedactedAndScrubbed`, `TestAMintLogNamesTheGrant`; PostgreSQL
  (`exchange_integration_test.go`): `TestTheWorkerExecutesWithAnExchangedToken` and
  `TestTheWorkerExecutesWithAnImpersonatedToken` (the real HTTP connector and Fake ERP, principals `sts:…` and
  `sa:…`, no subject, federated or final token in the database or logs).
- Rev 1.5, `internal/fakeerp` (`exchange_test.go`): `TestAnExchangeIssuesATokenForTheSubject` (both subject
  kinds), `TestTheExchangeRefusesBadRequests`, `TestImpersonationIssuesAServiceAccountToken`,
  `TestImpersonationRefusesBadRequests`, `TestExchangeOptionsFailClosed`; `cmd/fakeerp`
  `TestExchangeSettingsComeTogether`; `test/demo` `TestTokenExchangeDemo` on minikube: purchases as
  `sts:system:serviceaccount:eacp:eacp-worker` and `sa:eacp-erp@eacp-demo.iam.gserviceaccount.com`, both
  subjects exchanged with their digests audited, and no subject token or issued token in responses, logs or a
  database dump.

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
- With SPIFFE (Rev 1.4) the worker holds no secret for the binding on any platform SPIRE attests; its
  identity is its registration entry. Deleting the entry stops new SVIDs, but the SVIDs already issued stay
  valid and the worker keeps sending its cached one while it outlives the call: revocation takes up to the
  full JWT-SVID TTL. Use a kill (ADR-016) or disable the connector's circuit for immediate containment.
- With token exchange (Rev 1.5) the worker holds no secret for the binding either: the STS trusts the
  platform's or SPIRE's issuer, and an impersonated service account's permissions stay in the cloud's IAM.
  Revoking the trust (or the impersonation grant) stops new tokens; a token already issued lives for at most
  its lifetime and is used for at most an hour.
- AWS STS, Vault dynamic secrets and HSM/KMS-held keys (later phases) implement the same seam: a provider that returns a credential valid for `validFor`, reports
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
| The worker's SPIFFE identity | `spiffe_id` is required and must equal the SVID's `sub`; the worker never takes whatever identity the agent offers first (Rev 1.4). |
| Workload API endpoint | `unix://` with an absolute path; `tcp://` loopback only in development and test (Rev 1.4). |
| Unexpected Workload API status | `spiffe_unavailable`: withhold and retry, never a permanent refusal (Rev 1.4). |
| The agent's cached SVID | Used only while it outlives the call; never past its `exp`; less than 10 s left is `spiffe_expiring` (Rev 1.4). |
| Who validates a JWT-SVID | The relying party only; the worker checks its shape, `sub`, audience and `exp` (Rev 1.4). |
| SVID bearer replay at Fake ERP | Accepted until `exp` (a JWT-SVID is a bearer token); the audience limits where (Rev 1.4). |
| SPIRE in the demo | Kubernetes only, one server with SQLite on an emptyDir pinned to the control-plane node; the bundle is a snapshot taken at install (Rev 1.4). |
| `issued_token_type` | Required and must be an access token, although some IdPs treat it loosely: a JWT or refresh token is never sent as a Bearer (Rev 1.5). |
| `expires_in` of an exchange | Required (RFC 8693 only recommends it): a token without a lifetime is never cached or sent (Rev 1.5). |
| Client authentication at an STS | Optional; when configured, exactly one of §3's forms (Rev 1.5). |
| Impersonation lifetime | Requested 300–3600 s (Google's default maximum); the final token is used for at most an hour anyway (Rev 1.5). |
| The federated token | Two hops per mint, never cached for a later mint; sent only to the impersonation endpoint (Rev 1.5). |
| The impersonation URL | Must have Google's path shape, so a mistyped URL fails at load rather than receiving a federated token (Rev 1.5). |
