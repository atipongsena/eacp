# Phase 24f — OAuth 2.0 token exchange (RFC 8693) and GCP impersonation (design)

Date: 2026-09-27 · Status: approved by the owner in chat (approach 1; sections 1 and 2 approved; "yes, continue
dev untill finish phase")
Scope: MASTER_PLAN §96 (Phase 24), sixth sub-phase: cloud identities. ADR: **ADR-019 Credential Custody, Rev 1.5**.
Follows 24a (client credentials), 24b (workload identity federation), 24c (private_key_jwt), 24d (Vault KV v2)
and 24e (SPIFFE JWT-SVIDs). Owner choices: RFC 8693 token exchange; subject tokens from a file **and** from the
SPIFFE Workload API; client authentication at the STS optional (none, or any existing form); GCP service-account
impersonation as an optional second hop; approach 1 (a `grant` of the existing `oauth2` provider).

## 1. Intent

Clouds and enterprise IdPs issue access tokens in exchange for a workload's own identity token: GCP Workload
Identity Federation (STS `v1/token`, then optionally IAM Credentials `generateAccessToken`), Keycloak and Okta
token exchange. After 24f an `oauth2` binding can trade the worker's projected service-account token or its
JWT-SVID for a Bearer access token, optionally impersonating a GCP service account, with no long-lived secret.

Success:

- `"grant": "token_exchange"` mints a Bearer token through an RFC 8693 exchange; with `impersonate`, the final
  token is a service account's access token;
- the subject token is re-read (file) or fetched (SPIFFE) at every mint and goes only to the token endpoint;
  the federated token goes only to the impersonation endpoint; the final token only to the connector;
- every 24a rule still applies to the final token (reuse while it outlives the call, one-hour cap, too-short,
  rejection, back-off, withholding from claims, redaction), and every failure withholds only that binding;
- no subject, federated or final token appears in logs, responses, the database or connector results;
- a minikube demo buys through a file-subject exchange and a SPIFFE-subject exchange with impersonation;
  compose and every 24a–24e binding behave exactly as before.

## 2. Verified upstream API

- **RFC 8693** (rfc-editor.org): request `grant_type=urn:ietf:params:oauth:grant-type:token-exchange`,
  `subject_token` and `subject_token_type` REQUIRED; `resource`, `audience`, `scope`, `requested_token_type`
  OPTIONAL. Response `access_token`, `issued_token_type`, `token_type` REQUIRED, `expires_in` RECOMMENDED.
  Token types `urn:ietf:params:oauth:token-type:{access_token,refresh_token,id_token,saml1,saml2,jwt}`. Client
  authentication is optional ("the normal mechanisms provided by OAuth 2.0").
- **Google's client, `golang.org/x/oauth2` v0.36.0** (module source: `google/internal/stsexchange`,
  `google/internal/impersonate`, `google/externalaccount/basecredentials.go`):
  - STS: `POST https://sts.googleapis.com/v1/token`, `Content-Type: application/x-www-form-urlencoded`, form
    `audience`, `grant_type`, `requested_token_type=urn:ietf:params:oauth:token-type:access_token`,
    `subject_token_type`, `subject_token`, `scope` (space-joined), optional `options` (JSON). Response JSON
    `access_token`, `issued_token_type`, `token_type`, `expires_in` (integer). Subject token types Google
    lists: `…:jwt`, `…:id-token` (hyphen), `…:saml2`, `urn:ietf:params:aws:token-type:aws4_request`.
  - When impersonating, the STS request's scope is `https://www.googleapis.com/auth/cloud-platform` and the
    caller's scopes go to the second hop.
  - Impersonation: `POST https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/<email>:generateAccessToken`,
    `Authorization: Bearer <federated token>`, `Content-Type: application/json`, body
    `{"delegates"?, "lifetime": "<n>s", "scope": [...]}` (lifetime default `3600s`). Response
    `{"accessToken", "expireTime"}` with `expireTime` in RFC 3339.

## 3. Design

### 3.1 Secrets file

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

- `grant`: absent or `client_credentials` (unchanged behaviour) or `token_exchange`. Any other value rejects
  the file. `subject_token`, `subject_token_type`, `audience` and `impersonate` are refused without
  `token_exchange`.
- `subject_token` (required for `token_exchange`): exactly one of `file` (a path; at load only its shape is
  checked, as `client_assertion_file`) or `spiffe` (`{"audience": …}`, 24e's audience rules; needs the file's
  `spiffe` block).
- `subject_token_type`: default `urn:ietf:params:oauth:token-type:jwt`; also `…:id_token` or `…:id-token`.
- `audience`: optional, 1–1024 printable ASCII characters without spaces. `scope` and `resource` keep their
  rules.
- Client authentication for `token_exchange`: none (no `client_id` and no credential form), a public client
  (`client_id` alone, sent in the form body), or `client_id` with exactly one existing form. For
  `client_credentials` the rules are unchanged.
- `impersonate` (optional): `url` absolute, `https` (`http` only in development and test), lowercase host, no
  user info, query or fragment, path exactly `/v1/projects/-/serviceAccounts/<email>:generateAccessToken`
  where `<email>` is 3–254 characters of `[A-Za-z0-9._%+-@]` with one `@`; `scope` 1–32 scope tokens (each as
  24a's scope tokens); `lifetime_seconds` 300–3600, default 3600.
- The worker contacts nothing at load; errors never contain a value.

### 3.2 A mint

One mint at a time per binding, as 24a; the steps below replace 24a's single request.

1. **Subject token.** `file`: read with 24b's `readAssertion` (classes `assertion_unreadable`,
   `assertion_invalid`, `assertion_expired` with less than 10 s left). `spiffe`: 24e's `svid(ctx, audience,
   10 s)` (classes `spiffe_*`; less than 10 s left is `assertion_expired`). It is redacted until its `exp`
   plus 24 h and kept for `Values()` until its `exp`.
2. **Client authentication** as configured: none; `client_id` in the form; Basic with the client secret; or a
   client assertion (RFC 7523) with `client_id`. A Vault-held secret or key is read inside the mint (24d).
3. **Exchange.** `POST token_url`, form: `grant_type`, `subject_token`, `subject_token_type`,
   `requested_token_type=urn:ietf:params:oauth:token-type:access_token`, and `audience`, `scope`, `resource`
   when set. Response: 24a's checks (HTTP 200, at most 64 KiB, JSON, `access_token` printable ≤ 8192 B,
   `token_type` Bearer case-insensitively, `expires_in` an integer 1–86 400), and `issued_token_type` equal to
   `urn:ietf:params:oauth:token-type:access_token` (else `wrong_token_type`).
4. **Impersonation** (when set). `POST impersonate.url`, `Authorization: Bearer <federated token>`,
   `Content-Type: application/json`, body `{"scope": [...], "lifetime": "<n>s"}`; 10 s timeout, no redirects,
   response at most 64 KiB. Response: HTTP 200 (else `impersonation_http_<code>`), JSON with a printable
   `accessToken` of at most 8192 bytes and an `expireTime` that parses as RFC 3339, is after the request
   started and at most 86 400 s later (else `impersonation_invalid`); a transport error is `transport`. The
   federated token is redacted until its expiry plus 24 h, kept for `Values()` until its expiry, never cached
   for a later mint and never sent anywhere else.
5. **The final token** (the exchange result, or the impersonated token) is used exactly as 24a's: until at
   most one hour after the request, too-short for a call it could expire during, dropped when rejected,
   redacted until its real expiry plus 24 h.

A failed step fails the mint with its class: 24a's back-off (1 s doubling to 60 s), withheld from claims, no
dispatch, no retry within the mint. Logs carry the binding, the host(s), the grant, whether it impersonated
and the lifetime, never a token.

### 3.3 Fake ERP

- **Token exchange** (`Options.Exchange`): `POST /oauth/token` with the token-exchange grant. Client
  authentication is optional; when a request carries Basic or an assertion it must authenticate as today.
  `subject_token_type` must be one of the three JWT types, `requested_token_type` absent or `…:access_token`,
  `audience` exactly the configured audience. The subject token must verify against one configured subject
  (RS256 or ES256 by `kid` from its keys, exact `iss` when set, exact `sub`, the audience in `aud`, `exp`/`nbf`
  with 30 s skew). It issues a token (TTL as other tokens) whose principal is `sts:<sub>`, answers with
  `issued_token_type`, and audits the subject token's SHA-256 (`subject_sha256`), never the token.
  Env: `EACP_FAKEERP_STS_AUDIENCE`, and subjects `EACP_FAKEERP_STS_K8S_{ISSUER,AUDIENCE,SUBJECT,JWKS_FILE}` and
  `EACP_FAKEERP_STS_SPIFFE_{ISSUER,AUDIENCE,SUBJECT,BUNDLE_FILE}`; each group all or none; the audience needs
  a subject.
- **Impersonation** (`Options.Impersonation`, env `EACP_FAKEERP_IMPERSONATE_ACCOUNTS`, a comma-separated
  list): `POST /v1/projects/-/serviceAccounts/{email}:generateAccessToken` with a Bearer token issued by the
  exchange (principal `sts:…`), a JSON body with 1–32 scopes and a lifetime `^\d+s$` of 1–3600 s. It issues a
  token for that lifetime, principal `sa:<email>`, and answers `{"accessToken", "expireTime"}`.
- `sts:` and `sa:` tokens authorise the ERP API like the other tokens.

### 3.4 Kubernetes and the demo

- `deployments/k8s/connector-secrets.exchange.json` binds tenant `…00a9` (demo "tyrell"):
  `fakeerp-sts` (file subject: the worker's projected token, audience `fakeerp`; STS audience `fakeerp-sts`; no
  client authentication) and `fakeerp-sts-sa` (SPIFFE subject, audience `fakeerp-sts`; impersonating
  `eacp-erp@eacp-demo.iam.gserviceaccount.com` at `http://fakeerp:8090`). `scripts/k8s-e2e.sh` merges it and
  configures Fake ERP (`fakes.yaml`) with the cluster's issuer and JWKS and the SPIRE bundle it already has.
- `TestTokenExchangeDemo` (Kubernetes only): tenant, cast and policy; a purchase through each binding as
  `sts:system:serviceaccount:eacp:eacp-worker` and `sa:eacp-erp@eacp-demo.iam.gserviceaccount.com`, one PO
  each; the ERP audit shows the exchanges with subject digests; no subject token, SVID, federated or final
  token in responses, logs or a database dump.
- Compose is unchanged: it has no platform issuer.

### 3.5 Proof (tests first)

- `internal/worker`: load validation (every field, grant exclusivity, client-authentication cases, impersonate
  URL shape, dev-only http, no contact at load); the exchange request shape for each client-authentication
  case and both subject sources; every response refusal including `wrong_token_type`; impersonation request
  shape, every refusal, the one-hour cap from `expireTime`; redaction and `Values()` for the subject, federated
  and final tokens; a failed step backing off; PostgreSQL integration buying through both kinds of binding
  against Fake ERP with nothing persisted.
- `internal/fakeerp`: exchange accepts a good subject for each kind and refuses a wrong audience, subject type,
  requested type, issuer, subject, key, signature or expiry, and a bad client credential; impersonation accepts
  an `sts:` token and refuses others, unknown accounts, bad scopes and lifetimes; env settings fail closed.
- minikube: `TestTokenExchangeDemo` plus every existing k8s demo in a fresh `scripts/k8s-e2e.sh` run.

### 3.6 Documentation

ADR-019 Rev 1.5 (§3e and the surrounding sections), MASTER_PLAN §96, AGENTS.md, README, docs/KUBERNETES.md,
docs/DEMO.md, docs/INVARIANTS.md (the integration tests join invariant 11), research/REFERENCES.md.

## 4. Out of scope

AWS STS and SigV4; `actor_token` (delegation); `delegates` in impersonation; refresh tokens; caching the
federated token across mints; GCP workforce pools (`options.userProject`); requesting token types other than
access tokens; STS `options`.

## 5. Unresolved assumptions (conservative choices, recorded in ADR-019)

- `issued_token_type` is required and must be an access token, although GCP's docs and some IdPs treat it
  loosely: a JWT or refresh token is never sent as a Bearer.
- `expires_in` stays required for the exchange (RFC 8693 only recommends it): a token without a lifetime is
  never cached or sent.
- The requested impersonation lifetime is capped at 3600 s (Google's default maximum); the final token is used
  for at most an hour anyway.
- Two hops per mint (no federated-token cache): simpler, and mints are at most hourly per binding.
- An impersonation URL must have Google's path shape, so a mistyped URL fails at load rather than sending a
  federated token to the wrong place.
