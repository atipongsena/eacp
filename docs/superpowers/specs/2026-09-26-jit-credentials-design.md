# Phase 24a — JIT credentials: the CredentialProvider and an OAuth 2.0 broker (design)

Date: 2026-09-26 · Status: approved by the owner in chat ("yes, write the spec and dev until finished phase")
Scope: MASTER_PLAN §96 (Phase 24), first sub-phase. ADR: **ADR-019 Credential Custody, Rev 1.0** (new; the
number is reserved by MASTER_PLAN §74). 24b/24c (Vault, SPIFFE/SPIRE, cloud workload identity) follow later.

## 1. Intent

Today the execution worker sends a long-lived static secret to each enterprise system. §96 asks to replace it
with short-lived credentials minted just in time, without changing custody (ADR-001: only the worker holds
anything; agents, the API, PostgreSQL, logs and responses never see a credential).

24a adds the `CredentialProvider` seam and one real provider: OAuth 2.0 client credentials (RFC 6749 §4.4)
against an enterprise token endpoint — the pattern behind Entra ID, Okta, Keycloak and most custom brokers.

Success:

- an action executes with a bearer token minted from the token endpoint, never with a static secret;
- a token is reused only while it stays valid for the whole call, and a fresh one is minted otherwise;
- when the token endpoint fails, nothing is dispatched, the lease is released, and the worker stops claiming
  that binding's work until a back-off passes (no lease churn, no hammering of the endpoint);
- no minted token or client secret appears in logs, API responses, the database or connector results;
- the static provider behaves exactly as before (every existing test and demo passes unchanged).

## 2. Principles (non-negotiable)

1. **Custody unchanged.** Providers run only inside `execution-worker` (`config.Options.AllowConnectorSecrets`).
   No API, table or message learns a credential or a provider's configuration beyond the existing
   `secret_ref`.
2. **Fail closed.** An invalid provider entry rejects the whole secrets file at startup. A token response
   that is not a short-lived Bearer token is refused. A missing credential means no dispatch.
3. **No persistence.** Minted tokens live in worker memory only, until they expire.
4. **Host binding stays.** A credential is still bound to one connector host (ADR-003 §4). The token endpoint
   is a second, separately bound host: the client secret is sent only to it.
5. **No new dependency.** Standard library HTTP; no OAuth library, no downloads.
6. **Execution semantics unchanged (ADR-004).** A rejected token at the target is classified exactly as
   today; the provider only drops its cached token so the next attempt mints a new one.

## 3. Design

### 3.1 The seam (`internal/worker/credentials.go`)

```go
// Credentials resolves connector credentials from the worker's providers.
type Credentials struct { /* bindings → provider, redaction set, clock */ }

// Resolve returns a credential for tenant's secret reference at endpoint that
// stays valid for at least validFor. ErrNoCredential: no binding for this
// tenant, reference and exact host. ErrCredentialUnavailable: the provider
// could not produce one now (transient).
func (c *Credentials) Resolve(ctx context.Context, tenant uuid.UUID, ref, endpoint string,
	validFor time.Duration) (Secret, error)

// Available lists the bindings the worker can serve now (no values): every
// static binding, and each OAuth binding that is not backing off.
func (c *Credentials) Available() []Binding

// Bindings lists every configured binding (no values), for startup logs.
func (c *Credentials) Bindings() []Binding

// Values returns every live credential value (static secrets, client
// secrets, unexpired minted tokens) for scrubbing connector results.
func (c *Credentials) Values() []string

// Rejected tells the provider the target refused this credential (the
// connector's error class "unauthorized"); an OAuth provider drops its cached
// token. It never changes how the attempt is classified.
func (c *Credentials) Rejected(tenant uuid.UUID, ref string, s Secret)
```

A provider is internal:

```go
type provider interface {
	credential(ctx context.Context, validFor time.Duration) (Secret, error)
	available(now time.Time) bool
	rejected(s Secret)
}
```

`SecretStore` and `LoadSecrets` are replaced by `Credentials` and `LoadCredentials(path, opts)`; `Secret` keeps
its redacting formatters. The worker, reconciler and scanner call `Resolve` with `validFor`:

| Caller | validFor |
|---|---|
| worker `execute` | the contract's call timeout (`timeout_ms`, capped as `eacp.call_timeout`: default 30 s, max 300 s) + 30 s skew |
| reconciler lookup | its lookup timeout + 30 s |
| MCP scanner | its scan timeout + 30 s |

Claim queries (`Claimable`, `Reconcilable`, `ScansDue`) receive `Available()` instead of all bindings, so a
worker whose token endpoint is failing claims none of that binding's work.

On `ErrCredentialUnavailable` (or `ErrNoCredential`) the worker releases the lease before the dispatch intent,
with reason `credential unavailable` — no call, no circuit change (the target was never contacted). The
reconciler and scanner give their lease back the same way they do today when they cannot serve.

### 3.2 Secrets file

The same file (`EACP_CONNECTOR_SECRETS_FILE`) keeps its shape; each entry has exactly one of `value`,
`value_file` or `oauth2`:

```json
{"secrets": [
  {"tenant_id": "…", "secret_ref": "erp", "host": "erp.internal:8443", "value_file": "/run/secrets/erp"},
  {"tenant_id": "…", "secret_ref": "erp-jit", "host": "erp.internal:8443",
   "oauth2": {"token_url": "https://idp.internal/oauth2/token", "client_id": "eacp-worker",
              "client_secret_file": "/run/secrets/idp" | "client_secret": "…",
              "scope": "erp.purchase", "resource": "https://erp.internal"}}
]}
```

Validation (any failure rejects the whole file; errors never contain a value):

- `token_url`: absolute URL, no user info, no fragment; scheme `https`, or `http` only when `EACP_ENV` is
  `development` or `test`; its host is lowercase `host[:port]` as for `host`.
- `client_id`: 1–256 printable ASCII characters without spaces or `:` (it goes into Basic auth).
- exactly one of `client_secret` and `client_secret_file`; the secret is 1–4096 bytes.
- `scope` (optional): RFC 6749 scope tokens separated by single spaces, ≤ 1024 bytes; `resource` (optional,
  RFC 8707): an absolute URL without fragment.
- `(tenant_id, secret_ref)` stays unique across all entries.

### 3.3 The OAuth 2.0 provider (`internal/worker/oauth2.go`)

Token request: `POST token_url`, `Content-Type: application/x-www-form-urlencoded`, body
`grant_type=client_credentials[&scope=…][&resource=…]`, client authentication `client_secret_basic`
(RFC 6749 §2.3.1: form-urlencoded id and secret in Basic auth). Its own `http.Client`: 10 s timeout, **no
redirects** (a redirect could carry the client secret to another host), response ≤ 64 KiB.

Response (RFC 6749 §5.1), all required:

- HTTP 200 with a JSON object;
- `access_token`: 1–8192 bytes, printable ASCII without spaces;
- `token_type`: `Bearer` (case-insensitive, RFC 6750);
- `expires_in`: an integer from 1 to 3600. A token without an expiry or living longer than an hour is not
  short-lived and is refused.

Anything else — an error response (§5.2), a transport error, a malformed body — is a failed mint.

Cache and back-off (per binding, mutex-guarded):

- The provider keeps the latest token with `expiry = time of request + expires_in` (measured from before the
  request, so network time never extends it).
- `credential(validFor)` returns the cached token when `expiry - now ≥ validFor`; otherwise it mints. One mint
  runs at a time per binding; concurrent callers wait for it (and share its result).
- A token whose lifetime is shorter than `validFor` is returned as unavailable: it cannot cover the call.
- A failed mint sets `backoffUntil = now + d`, with `d` doubling from 1 s to at most 60 s; success resets it.
  While backing off, `available` is false and `credential` fails at once without a request.
- `rejected(s)` drops the cached token if it is still `s`.

### 3.4 Redaction

`internal/logging` gains a concurrency-safe `SecretSet`: `Add(value, until time.Time)`, `AddPermanent(value)`
and the values the redactor scrubs. `logging.New` takes it, so a logger built at startup redacts values added
later. `service.Deps` owns one set; `RedactSecrets` adds permanent values (existing behaviour). The worker
adds every client secret permanently and every minted token until its expiry + 24 h; the set is capped at
10 000 temporary values (oldest dropped first). The scrubbers `scrub`/`scrubLookup` use `Credentials.Values()`.

The provider logs mints and failures with the binding (tenant, ref, token host), `expires_in` and the error
class only — never a token, a secret or a response body.

### 3.5 Fake ERP token endpoint

`internal/fakeerp` gains an optional OAuth client (`EACP_FAKEERP_OAUTH_CLIENT_ID`,
`EACP_FAKEERP_OAUTH_CLIENT_SECRET_FILE`, `EACP_FAKEERP_OAUTH_TTL` default `300s`, 1 s–3600 s):

- `POST /oauth/token` implements §3.3's contract (Basic auth, `grant_type=client_credentials`); it issues a
  random 32-byte opaque token and remembers only its SHA-256 and expiry in memory; wrong credentials get
  `401 {"error":"invalid_client"}`, another grant `400 {"error":"unsupported_grant_type"}`.
- The privileged routes accept the static token (principal `execution-worker`, unchanged) or an unexpired
  minted token (principal `oauth:<client_id>`); an expired or unknown token is `unauthorized` as today.
- Issuance is audited (route `/oauth/token`, outcome `token_issued`, principal `oauth:<client_id>`, the
  token's SHA-256 and expiry), never the token. Tokens are random 32 bytes, base64url without padding (43
  characters).
- Test hooks: `fakeerp.Options` lets tests shorten the TTL, count issuances and make the endpoint fail.

Compose: Fake ERP gets a dev client (`deployments/docker/secrets/fakeerp-oauth-client.dev`, prepared like the
other dev tokens) and TTL `60s`; the dev connector-secrets manifest gains tenant
`00000000-0000-4000-8000-0000000000a4` (`secret_ref` `fakeerp-jit`, host `fakeerp:8090`, `oauth2` with
`token_url` `http://fakeerp:8090/oauth/token`). The Kubernetes dev manifests and e2e script get the same.

### 3.6 Proof

- `internal/worker` unit tests with an `httptest` token server: minting, reuse, refresh when the remaining life
  is shorter than `validFor`, single-flight under concurrency, every refused response (§3.3), no redirects,
  back-off and `available`, `rejected`, file validation (§3.2), redaction of minted tokens by a logger built
  before the mint, and the scrubbers.
- `internal/worker` PostgreSQL integration tests with the real HTTP connector and an in-process Fake ERP with
  OAuth: an action succeeds with principal `oauth:<client>` and one PO; a burst reuses one token; a TTL
  shorter than the call's `validFor` never dispatches; a failing token endpoint dispatches nothing, releases
  the lease and stops claims until the back-off passes, then the action succeeds; the reconciler resolves an
  unknown outcome with a minted token; no token appears in the database (`pg_dump`-equivalent query over
  text columns), the logs or the action API.
- `test/demo` `TestJITDemo` (compose; Kubernetes too): a tenant bound through `oauth2` buys through the Fake
  ERP; the ERP audit shows only `oauth:` principals for its operations and token issuance; the secret scan
  covers the client secret and every token the ERP issued (read from a test-only audit of hashes).
- `test/security` (compose): the agent cannot reach `/oauth/token`.

### 3.7 Documentation

ADR-019 Rev 1.0 (custody as built since Slice A, plus providers), MASTER_PLAN §96 status, AGENTS.md (status, a
rule line), README, `docs/KUBERNETES.md` (the token endpoint belongs in `worker.connectorEgress`; inline
`client_secret` since the chart mounts one file), `docs/DEMO.md`.

## 4. Out of scope

Vault, SPIFFE/SPIRE, Azure/AWS workload identity, `private_key_jwt` and mTLS client authentication, token
exchange (RFC 8693), per-attempt credential evidence in PostgreSQL, rotating static secrets without a
restart, a separate broker process.

## 5. Unresolved assumptions (conservative choices, recorded in ADR-019)

| Assumption | Choice |
|---|---|
| Where providers run | Inside the worker only; a broker sidecar is deferred. |
| Client authentication | `client_secret_basic` only; `private_key_jwt` deferred. |
| Token lifetime | Required `expires_in`, at most 3600 s; longer or missing is refused. |
| Reuse | Only while the token outlives the whole call (`validFor`); never persisted. |
| Token endpoint failure | No dispatch, lease released, binding withheld from claims with 1–60 s back-off. |
| Target rejects a token | Classified as today (ADR-004); the cached token is dropped. |
| Redirects from the token endpoint | Refused. |
| Plain `http` token endpoint | Only in `development`/`test`. |
| Credential evidence in PostgreSQL | None in 24a; logs carry binding and expiry, never values. |
| Minted tokens in the redactor | Kept until expiry + 24 h, at most 10 000. |
