# Phase 24b — Workload identity federation (design)

Date: 2026-09-26 · Status: approved by the owner in chat ("yes, write the spec and dev until finished phase")
Scope: MASTER_PLAN §96 (Phase 24), second sub-phase. ADR: **ADR-019 Credential Custody, Rev 1.1**.
Follows 24a (the provider seam and the OAuth 2.0 client-credentials provider). 24c (SPIFFE JWT-SVIDs,
`private_key_jwt`, token exchange, AWS/GCP STS) follows later.

## 1. Intent

After 24a a JIT binding still holds one long-lived secret: the OAuth client secret. Workload identity
federation removes it. The worker proves who it is with a short-lived JWT that its platform issues and
rotates — a Kubernetes projected service-account token, which is also what Azure Workload Identity injects —
and the enterprise IdP exchanges that JWT for an access token (RFC 7523 client assertion, the Microsoft Entra
"federated credential" request).

Success:

- an action executes with an access token that was minted by presenting a platform-issued assertion, with no
  client secret anywhere in the worker's configuration;
- the worker re-reads the assertion file for every mint, so the kubelet's rotation is picked up;
- an unreadable, malformed or expiring assertion dispatches nothing and backs the binding off exactly like a
  failed mint in 24a;
- no assertion, token or secret appears in logs, API responses, the database or connector results;
- on Kubernetes the demo buys through a token minted with the cluster's real service-account issuer;
- 24a's client-secret bindings behave exactly as before, except that an IdP may now issue tokens living up to
  24 h (§3.3).

## 2. Principles (unchanged from 24a)

Custody stays in the worker; fail closed; nothing persisted; host binding; standard library only; execution
semantics unchanged (ADR-004). The worker never verifies an assertion's signature: it is an opaque input that
only the IdP judges. It parses only the `exp` claim, to know when the assertion stops being usable and how long
to redact it.

## 3. Design

### 3.1 Secrets file

An `oauth2` object takes exactly one of `client_secret`, `client_secret_file` and the new
`client_assertion_file`:

```json
{"tenant_id": "…", "secret_ref": "erp-wif", "host": "erp.internal:8443",
 "oauth2": {"token_url": "https://login.microsoftonline.com/<tenant>/oauth2/v2.0/token",
            "client_id": "<application id>",
            "client_assertion_file": "/run/secrets/eacp-identity/token",
            "scope": "api://erp/.default"}}
```

Validation at load (any failure rejects the whole file; errors never contain a value):

- the file is readable, 1 byte–16 KiB after trimming a trailing newline;
- it is a compact JWS: three non-empty base64url segments (no padding), the first two decoding to JSON objects;
- the payload has a numeric `exp`. An already expired assertion is accepted at load (the kubelet may be
  about to rotate it); it fails at mint time.

The assertion read at load is redacted like one read at a mint (§3.4).

### 3.2 The token request with an assertion

RFC 7523 §2.2 and the Entra client-credentials "federated credential" case (verified against
learn.microsoft.com, `v2-oauth2-client-creds-grant-flow`, 2026-06):

```
POST token_url
Content-Type: application/x-www-form-urlencoded

grant_type=client_credentials&client_id=<id>
&client_assertion_type=urn:ietf:params:oauth:client-assertion-type:jwt-bearer
&client_assertion=<jwt>[&scope=…][&resource=…]
```

No `Authorization` header (a client uses one authentication method, RFC 6749 §2.3). Everything else — its own
HTTP client, 10 s timeout, no redirects, 64 KiB response limit, the response contract, single-flight, back-off —
is 24a's.

Per mint, before the request, the provider re-reads the file (the kubelet rotates it at 80 % of its TTL, and
the Kubernetes docs leave reloading to the application). A mint fails, with the 1–60 s back-off, when:

| Class | Cause |
|---|---|
| `assertion_unreadable` | the file cannot be read, or is empty or over 16 KiB |
| `assertion_invalid` | not a compact JWS with a JSON payload and a numeric `exp` |
| `assertion_expired` | `exp` is less than 10 s (the token request timeout) after now |

### 3.3 Token lifetime (ADR-019 change)

Entra issues access tokens with a random default lifetime of 60–90 minutes (learn.microsoft.com,
`access-tokens`, "Token lifetime"). 24a refused `expires_in` above 3600, so real Entra tokens would fail about
half the time. Rev 1.1:

- `expires_in` is accepted from 1 to 86 400 (still required, still a bare JSON integer);
- the worker **uses** a token for at most 3600 s: its usable expiry is `start + min(expires_in, 3600)`, and the
  "lifetime" that decides `ErrCredentialTooShort` is that capped value;
- redaction and scrubbing (`Values()`) keep the token until its real expiry (+ 24 h for redaction), because it
  still authorises at the target until then.

This never holds a token for longer than 24a did; it only stops refusing tokens the IdP issues for longer.

### 3.4 Redaction

Each assertion read at mint time is added to the `SecretSet` until its `exp` + 24 h (the same bound of 10 000
temporary values), and `Values()` includes the assertion last read while it is unexpired, so the scrubbers
remove it from connector results. The assertion is a credential: it can be exchanged for tokens until it
expires. Mint logs gain nothing: binding, token host, lifetime or failure class only.

### 3.5 Kubernetes chart

- The worker gets its own ServiceAccount, `<release>-worker` (`automountServiceAccountToken: false`), so a
  federated subject `system:serviceaccount:<namespace>:<release>-worker` names only the worker. The API, PDP
  and migrations keep `<release>`.
- New values:

  ```yaml
  worker:
    workloadIdentity:
      enabled: false
      audience: ""            # the IdP's expected audience, e.g. api://AzureADTokenExchange
      expirationSeconds: 3600 # 600-86400
  ```

  When enabled, the worker pod (and only it) gets a projected volume
  `serviceAccountToken {audience, expirationSeconds, path: token}` mounted read-only at
  `/run/secrets/eacp-identity`; a secrets-file entry names `/run/secrets/eacp-identity/token` as its
  `client_assertion_file`.
- `validate.yaml` refuses: enabled without an audience; an audience with whitespace or over 256 characters;
  `expirationSeconds` that is not an integer from 600 to 86 400 (the Kubernetes minimum is 600).
- Render tests: the worker alone projects a token and uses `<release>-worker`; disabled renders no projected
  token; the refusals.
- `docs/KUBERNETES.md`: Entra (AKS or any cluster whose issuer Entra can reach) — a federated identity credential
  with the cluster's issuer, subject `system:serviceaccount:<ns>:<release>-worker` and audience
  `api://AzureADTokenExchange`; the token endpoint goes into `worker.connectorEgress`. The Azure Workload
  Identity webhook is not required, because the chart projects the token itself.

### 3.6 Fake ERP as a federated relying party (demo and tests)

`fakeerp.Options` gains an optional federated client:

```go
type Federated struct {
	ClientID string
	Issuer   string                     // exact iss
	Audience string                     // must be one of aud
	Subject  string                     // exact sub
	Keys     map[string]*rsa.PublicKey  // by kid, from a JWKS
}
```

and `fakeerp.ParseJWKS([]byte)` reads RSA keys (`kty` `RSA`, `kid`, `n`, `e`; other keys ignored; at least one
RSA key required). Environment (`cmd/fakeerp`): `EACP_FAKEERP_OAUTH_FEDERATED_CLIENT_ID`, `_ISSUER`,
`_AUDIENCE`, `_SUBJECT`, `_JWKS_FILE`, all or none. The token endpoint is served when either client is
configured.

`POST /oauth/token` chooses the client by the authentication method:

- both Basic auth and `client_assertion` → `400 invalid_request`;
- Basic auth → 24a's client-secret path;
- `client_assertion` → `client_assertion_type` must be the jwt-bearer URN, `client_id` must equal the
  federated client's id, and the assertion must verify: header `alg` `RS256` (anything else refused) with a
  known `kid`, a valid PKCS #1 v1.5 SHA-256 signature, `iss` and `sub` exactly, `aud` (a string or an array)
  containing the audience, `exp` in the future and `nbf`/`iat` not in the future, each with 30 s skew.
  Failure → `401 invalid_client`, audited with outcome `invalid_client` and no assertion;
- neither → `401 invalid_client`.

On success the token is issued as in 24a (principal `oauth:<client_id>`), and the issuance audit also records
`assertion_sha256`, never the assertion.

### 3.7 Kubernetes demo (the only end-to-end run with a real issuer)

- `scripts/k8s-e2e.sh` reads the cluster's issuer and JWKS (`kubectl get --raw /.well-known/openid-configuration`
  and `/openid/v1/jwks`) into ConfigMap `eacp-deps/fakeerp-federation` (`issuer`, `jwks.json`). Fake ERP reads
  the issuer through `configMapKeyRef` and mounts the JWKS; client id `eacp-worker-wif`, audience `fakeerp`,
  subject `system:serviceaccount:eacp:eacp-worker`.
- `deployments/k8s/e2e-values.yaml` enables `worker.workloadIdentity` (audience `fakeerp`,
  `expirationSeconds: 600`).
- The worker's secrets file on Kubernetes is the dev file plus `deployments/k8s/connector-secrets.federated.json`
  (tenant `00000000-0000-4000-8000-0000000000a5`, `secret_ref` `fakeerp-wif`, `client_assertion_file`
  `/run/secrets/eacp-identity/token`), merged by the script. Compose never sees that entry: its worker has no
  assertion file and would refuse to start.
- `test/demo` `TestFederatedJITDemo` (skips unless `EACP_DEMO_PLATFORM=k8s`): tenant `…00a5` buys three times;
  every operation's ERP principal is `oauth:eacp-worker-wif`; the issuance audit shows assertion hashes; the scan
  of API responses, service logs and a `pg_dump` finds no issued token (24a's 43-character window hashing) and no
  JWT whose SHA-256 is an audited assertion or whose payload's `sub` is the worker's service account.

Compose keeps 24a's client-secret tenant and gets no federation demo (no platform issuer exists there).

### 3.8 Proof (tests first)

- `internal/worker` unit tests (`federation_test.go`, httptest IdP, RSA-signed test assertions written to a
  temporary file):
  - the request carries `client_id`, the jwt-bearer type and the file's assertion, and no Basic auth;
  - rotating the file between mints sends the new assertion;
  - each of `assertion_unreadable`, `assertion_invalid`, `assertion_expired` fails the mint, backs off and
    withholds the binding, and makes no request;
  - an invalid assertion file at load rejects the whole secrets file; so does naming a secret and an
    assertion together;
  - the assertion is redacted by a logger built before the mint and appears in `Values()` until it expires;
  - the lifetime cap: `expires_in` 5400 is accepted, the token is reused only up to 3600 s after its request,
    `ErrCredentialTooShort` uses the capped lifetime, and `Values()` keeps it until the real expiry;
    `expires_in` 86 401 is refused.
- `internal/worker` PostgreSQL test (`jit_integration_test.go`): an action succeeds through the real HTTP
  connector and an in-process Fake ERP with a federated client, principal `oauth:<id>`; no assertion or token in
  the database or logs.
- `internal/fakeerp` (`federation_test.go`): issuance with a valid assertion; refusal for each of wrong `alg`,
  unknown `kid`, bad signature, wrong `iss`, `sub`, `aud`, `client_id`, `client_assertion_type`, expired `exp`,
  future `nbf`; two authentication methods; the audit carries the assertion hash and never the assertion;
  `ParseJWKS`; `cmd/fakeerp` all-or-none settings.
- `test/helm`: §3.5.
- `test/demo` `TestFederatedJITDemo` on minikube through `scripts/k8s-e2e.sh`.

### 3.9 Documentation

ADR-019 Rev 1.1 (§3.2 client assertions, §3.3 lifetime, §3.4 redaction, the chart and Fake ERP, proof,
assumptions), MASTER_PLAN §96 status, AGENTS.md (status and the credential rule line), README,
`docs/KUBERNETES.md` (§3.5), `docs/DEMO.md` (the federated demo on Kubernetes), `docs/INVARIANTS.md` if a new
test becomes an invariant's proof.

## 4. Out of scope

SPIFFE/SPIRE JWT-SVIDs, `private_key_jwt` with a worker-held key, RFC 8693 token exchange, AWS STS
`AssumeRoleWithWebIdentity` and GCP STS (different APIs, and AWS returns signing keys, not a bearer token),
a live JWKS fetch in Fake ERP, a federation demo on compose.

## 5. Unresolved assumptions (conservative choices, recorded in ADR-019)

| Assumption | Choice |
|---|---|
| Who validates the assertion | The IdP only; the worker reads `exp` and nothing else. |
| When the assertion is read | At every mint; never cached between mints. |
| An assertion close to expiry | Refused when under 10 s remain (a failed mint, back-off). |
| Tokens living longer than an hour | Accepted up to 24 h, used for at most 1 h, redacted until their real expiry + 24 h. |
| Which service account the worker federates as | Its own (`<release>-worker`); the others cannot obtain its identity through the chart. |
| Projected token lifetime | 600–86 400 s, default 3600 (as Azure Workload Identity). |
| Assertion replay at Fake ERP | Not checked (Entra accepts the same assertion until it expires). |
| Fake ERP's keys | A JWKS file captured at install; key rotation needs a reinstall (demo only). |
