# Phase 24c — private_key_jwt client authentication (design)

Date: 2026-09-26 · Status: approved by the owner in chat ("yes and dev until finish phase")
Scope: MASTER_PLAN §96 (Phase 24), third sub-phase. ADR: **ADR-019 Credential Custody, Rev 1.2**.
Follows 24a (the provider seam, OAuth 2.0 client credentials) and 24b (workload identity federation).
Token exchange (RFC 8693), SPIFFE, Vault and HSM/KMS keys come later.

## 1. Intent

24b removed the shared client secret where a platform issues the worker an identity token (Kubernetes).
Elsewhere — VMs, compose, on-premises hosts — there is no platform issuer. `private_key_jwt` (OpenID Connect
Core §9, RFC 7523 §2.2) gives the same property there: the worker holds an asymmetric private key, the IdP
holds only the public half (a certificate or a JWKS), and every token request carries a fresh, short-lived
assertion the worker signs. Nothing the IdP stores can authenticate as the worker.

Success:

- an action executes with a token minted by presenting an assertion the worker signed, with no client secret;
- every token request carries a new assertion with a unique `jti`, valid for five minutes;
- the Entra ID certificate shape (PS256 with `x5t#S256`) and the Okta/Keycloak JWKS shape (RS256/ES256 with
  `kid`) are both produced correctly;
- neither the private key, an assertion nor a token appears in logs, API responses, the database or connector
  results; only the worker holds the key;
- the demo runs on compose and Kubernetes;
- 24a and 24b bindings behave exactly as before.

## 2. Verified upstream requirements

- **Entra ID certificate credentials** (learn.microsoft.com, `certificate-credentials`, 2026-06): header
  `alg` PS256 (PSS padding), `typ` JWT, `x5t#S256` = base64url SHA-256 thumbprint of the certificate's DER;
  claims `aud` = the token endpoint URL, `iss` = `sub` = the client id, `jti` unique, `nbf`, `iat`, `exp`
  ("keep it short — 5–10 minutes after `nbf` at most"). Sent as `client_assertion_type` jwt-bearer +
  `client_assertion` (the 24b request shape).
- **Okta** private_key_jwt (developer.okta.com, client-auth guide): RS256/384/512 and ES256/384/512; `aud` = the
  full URL of the endpoint; `iss` = `sub` = client id; `exp` required and at most one hour ahead; `jti` optional
  but single-use when present; `iat` must be in the past; the public key is registered in the client's JWKS.
- RFC 7518: ES256 signatures are the raw 64-byte `R ‖ S`; PS256 uses a salt as long as the hash.

## 3. Design

### 3.1 Secrets file

An `oauth2` object takes exactly one of `client_secret`, `client_secret_file`, `client_assertion_file` and the
new `private_key_jwt` object:

```json
"oauth2": {"token_url": "https://login.microsoftonline.com/<tenant>/oauth2/v2.0/token",
           "client_id": "<application id>", "scope": "api://erp/.default",
           "private_key_jwt": {"alg": "PS256",
                               "key_file": "/run/secrets/idp/key.pem"   | "key": "-----BEGIN PRIVATE KEY-----\n…",
                               "certificate_file": "/run/secrets/idp/cert.pem" | "certificate": "…",
                               "key_id": "…"}}
```

Validation at load (any failure rejects the whole file; errors never contain a value):

- `alg` is one of `RS256`, `PS256` (RSA key of at least 2048 bits) and `ES256` (ECDSA P-256 key).
- Exactly one of `key_file` and `key`: PEM with exactly one block, type `PRIVATE KEY` (PKCS #8),
  `RSA PRIVATE KEY` (PKCS #1) or `EC PRIVATE KEY` (SEC 1); encrypted keys are refused; the key type must match
  `alg`. At most 16 KiB.
- At most one of `certificate_file` and `certificate`: one PEM `CERTIFICATE` block whose public key equals the
  private key's, valid now (not expired, not yet valid). It adds `x5t#S256` to the header.
- `key_id` (optional, 1–256 printable ASCII characters without spaces) becomes the header `kid`.
- The key and certificate are read once, at load. Rotating them means restarting the worker, like 24a's
  static secrets. Inline `key`/`certificate` exist because the Helm chart mounts only the secrets file.

### 3.2 The assertion

For every token request the worker signs:

- header `{"alg": <alg>, "typ": "JWT"}` plus `"kid"` when `key_id` is set and `"x5t#S256"` when a
  certificate is set;
- claims `{"iss": client_id, "sub": client_id, "aud": token_url, "jti": <128 random bits, base64url>,
  "iat": now, "nbf": now, "exp": now + 300}` (whole seconds).

It is sent exactly like 24b's assertion (`client_id`, `client_assertion_type`, `client_assertion`; no Basic
auth). Each assertion is added to the redaction set until `exp` + 24 h and is in `Values()` until `exp`; the
PEM text of the key is added to the redaction set permanently and is never in `Values()` (it is never sent to
a connector). A signing error is a failed mint (class `assertion_signing`), with the 1–60 s back-off.

Implementation: 24b's per-mint `readAssertion` and the new signer become two sources of one
`clientAssertion(now) (Secret, time.Time, string)` step inside `request`; the rest of the provider is shared.

### 3.3 Fake ERP relying party (demo and tests)

A third optional client, `fakeerp.KeyClient{ClientID, Audience string; Keys []PublicKey}`, where
`fakeerp.ParseKeySet([]byte) ([]PublicKey, error)` reads a JWKS of RSA and EC P-256 keys, each with its `kid`
and/or `x5t#S256`. Environment: `EACP_FAKEERP_OAUTH_KEY_CLIENT_ID`, `EACP_FAKEERP_OAUTH_KEY_AUDIENCE`,
`EACP_FAKEERP_OAUTH_KEY_JWKS_FILE` (all or none).

The token endpoint picks the client by `client_id` among the assertion clients (federated and key). A key-client
assertion must:

- use `alg` RS256, PS256 or ES256, with a key of the matching type (RSA for RS/PS, EC P-256 for ES);
- name a known key by `kid`, else by `x5t#S256`;
- verify;
- have `iss` = `sub` = the client id and `aud` = the configured audience (a string, or an array containing it);
- have `exp` in the future and at most one hour ahead, `nbf` and `iat` not in the future (30 s skew);
- carry a `jti` never seen before. Accepted `jti`s are recorded in the issuance audit (`assertion_jti`) and
  rebuilt from the durable log at startup, so a restart cannot reopen a replay.

Claim and header parsing becomes strict for both assertion clients: exact member names, and NumericDate values
that are JSON numbers, never strings (two 24b review minors).

### 3.4 Development keys

`eacpctl dev-client-key --dir <dir> --jwks <file> [--kid <id>]` (EACP_ENV development or test only, like
`pdp-dev-certs`) writes `key.pem` (RSA 2048, PKCS #8), `cert.pem` (self-signed, one year) into `<dir>` and the
public JWKS (`kty`, `kid`, `use` sig, `alg` PS256, `n`, `e`, `x5t#S256`) to `<file>`. It keeps a complete
existing set and refuses a partial one.

### 3.5 Demo

- Tenant `00000000-0000-4000-8000-0000000000a6`, `secret_ref` `fakeerp-pkjwt`, Fake ERP client
  `eacp-worker-pkjwt`, audience `http://fakeerp:8090/oauth/token`, `alg` PS256 with a certificate (the Entra
  shape).
- **Compose:** a one-shot `client-key` (the eacp image, `network_mode: none`) runs `eacpctl dev-client-key`
  into two volumes: `client_key` (key and certificate), mounted read-only into `execution-worker` only at
  `/run/secrets/eacp-client-key`, and `client_jwks` (the JWKS), mounted read-only into `fakeerp` only. The dev
  connector-secrets manifest gains the tenant's entry with `key_file` and `certificate_file` there.
- **Kubernetes:** `scripts/k8s-e2e.sh` runs `eacpctl dev-client-key` locally; the secrets merge replaces any
  `key_file`/`certificate_file` under `/run/secrets/eacp-client-key/` with the inline PEM; the JWKS goes into the
  `fakeerp-federation` ConfigMap (key `client-jwks.json`).
- `test/demo` `TestPrivateKeyJWTDemo` (both platforms): three purchases, one PO each, as principal
  `oauth:eacp-worker-pkjwt`; every issuance has a distinct `assertion_jti`; the scan of API responses, service
  logs and a `pg_dump` finds no issued token, no `PRIVATE KEY` PEM and no JWT whose payload's `iss` is
  `eacp-worker-pkjwt`.
- `test/security` (compose): only the worker mounts `client_key`; only Fake ERP mounts `client_jwks`; the worker
  loads 6 bindings.

### 3.6 Proof (tests first)

- `internal/worker` (`privatekeyjwt_test.go`): for each `alg`, an IdP double verifies the signature with the
  public key (PSS salt = hash length, ES256 raw R‖S), checks the header (`typ`, `kid`, `x5t#S256` equal to the
  certificate's SHA-256) and claims (`iss`, `sub`, `aud` = token URL, `exp - iat` = 300, `nbf` = `iat`), and sees
  a new `jti` on every mint; no Basic auth; the key PEM is in the redaction set and not in `Values()`; the
  assertion is in both. `TestInvalidOAuthEntriesRejectTheWholeFile` gains: unknown `alg`, `alg`/key mismatch,
  RSA 1024, encrypted PEM, two PEM blocks, a key and a `key_file` together, a certificate for another key, an
  expired certificate, `private_key_jwt` beside `client_secret`.
- `internal/worker` PostgreSQL (`jit_integration_test.go`): `TestTheWorkerExecutesWithPrivateKeyJWT` — an
  in-process Fake ERP with a key client; two actions succeed as `oauth:<id>`; no key, assertion or token in the
  database or logs.
- `internal/fakeerp` (`keyclient_test.go`): issuance for RS256/PS256/ES256 by `kid` and by `x5t#S256`; refusal of
  each: `alg` none/HS256, an RSA key used with ES256, unknown key, bad signature, `iss` ≠ `sub`, wrong `aud`,
  expired, `exp` more than an hour ahead, future `nbf`/`iat`, missing `jti`, a replayed `jti`, a replay after a
  restart, a quoted `exp`, a claim spelled `ISS`; `ParseKeySet`.
- `cmd/eacpctl`: `dev-client-key` only in development/test, the files and the JWKS thumbprint match the
  certificate, an existing set is kept.
- `test/security`, `test/demo` as above; the minikube e2e.

### 3.7 Documentation

ADR-019 Rev 1.2, MASTER_PLAN §96, AGENTS.md, README, `docs/KUBERNETES.md` (inline key), `docs/DEMO.md`,
`docs/INVARIANTS.md`.

## 4. Out of scope

RFC 8693 token exchange, SPIFFE, Vault, keys in an HSM/KMS or a cloud key vault, re-reading a key without a
restart, RS384/RS512/ES384/ES512/EdDSA, `x5c` in the header.

## 5. Unresolved assumptions (conservative choices, recorded in ADR-019)

| Assumption | Choice |
|---|---|
| Assertion lifetime | 300 s (Entra advises 5–10 min; Okta allows at most 1 h). |
| Audience | Always the token URL; no override. |
| `jti` | 128 random bits per request, never reused. |
| Key rotation | Restart the worker; the key is read once. |
| Certificate validity | An expired or not-yet-valid certificate rejects the secrets file at load. |
| Algorithms | RS256, PS256, ES256 only; the key type must match. |
| The key in logs | Its PEM is redacted permanently; it never enters `Values()`. |
| Fake ERP replay record | Kept for the life of the log (demo scale). |
