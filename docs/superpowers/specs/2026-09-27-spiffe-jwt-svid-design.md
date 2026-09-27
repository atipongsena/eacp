# Phase 24e — SPIFFE JWT-SVIDs as connector credentials (design)

Date: 2026-09-27 · Status: approved by the owner in chat ("yes, write the spec and continue dev until finished phase")
Scope: MASTER_PLAN §96 (Phase 24), fifth sub-phase. ADR: **ADR-019 Credential Custody, Rev 1.4**.
Follows 24a (the provider seam, OAuth client credentials), 24b (workload identity federation), 24c
(private_key_jwt) and 24d (Vault KV v2). Owner choices: both JWT-SVID uses (OAuth client assertion and a
direct bearer to a SPIFFE-aware connector; no X509-SVID mTLS), the real-SPIRE proof on minikube only, and
approach 1 (go-spiffe's Workload API client inside the worker).

## 1. Intent

24b let the worker authenticate to an IdP with a Kubernetes projected token: one issuer (the cluster), one
fixed audience per mounted token, Kubernetes only. SPIFFE gives a workload an identity that does not depend
on the platform, and the SPIRE agent issues a JWT-SVID **for whatever audience the workload names, when it
asks**. After 24e a connector binding can say "authenticate as the worker's SPIFFE identity, for this
audience", either to an IdP that trusts SPIRE's issuer or straight to a service that validates JWT-SVIDs.

Success:

- a static connector credential can be a JWT-SVID for the connector's own audience (`value_spiffe`), sent as
  the Bearer and refreshed before it could expire during a call;
- an OAuth binding can authenticate with a JWT-SVID as its RFC 7523 client assertion
  (`client_assertion_spiffe`), fetched at every mint, holding no client secret;
- the worker receives only the identity the file names (`spiffe_id`); anything else fails closed;
- the SPIRE agent being down, not yet attesting the worker, or answering wrongly never stops the worker from
  starting and never fails an action: that binding's work waits (`QUEUED`) and resumes;
- no JWT-SVID appears in logs, API responses, the database or connector results; only the worker talks to the
  Workload API, through a socket mounted into its pod alone;
- a demo on minikube buys through both kinds of binding against a real SPIRE; compose and every 24a–24d
  binding behave exactly as before.

## 2. Verified upstream API

- **go-spiffe v2.8.2** (`github.com/spiffe/go-spiffe/v2`, read from the module source):
  - `workloadapi.New(ctx, workloadapi.WithAddr(addr))` returns a `*Client`; it dials with
    `grpc.DialContext` without blocking, so creating it contacts nothing. `Close()` releases it.
  - `(*Client).FetchJWTSVID(ctx, jwtsvid.Params{Audience, ExtraAudiences, Subject spiffeid.ID})` sends
    `JWTSVIDRequest{SpiffeId, Audience}` with the `workload.spiffe.io: true` metadata header and parses the
    first SVID with `jwtsvid.ParseInsecure(token, audience)`: signature **not** verified; requires a
    SPIFFE-ID `sub`, an `exp`, one of the audiences in `aud`, not expired against the **real** clock, an
    allowed JWS algorithm (RS/ES/PS 256–512) and `typ` absent, `JWT` or `JOSE`. Returns `SVID{ID, Audience,
    Expiry, Claims}`; `Marshal()` returns the token.
  - Errors from the agent are gRPC statuses (`status.Code(err)`): `Unavailable` when it cannot be reached,
    `PermissionDenied` when no registration entry matches the caller.
  - Addresses: on Linux `unix:///abs/path` or `tcp://ip:port`; on Windows only `tcp://` or `npipe:`. The
    generated server interface `workload.SpiffeWorkloadAPIServer` (`proto/spiffe/workload`) is public, so tests
    can serve a fake agent.
  - Dependencies added to `go.mod`: go-spiffe v2.8.2, go-jose v4, go-winio (Windows only); gRPC is already in
    the module graph.
- **SPIRE v1.15.3** (`doc/spire_server.md`, `pkg/agent/manager/manager.go`, `pkg/common/rotationutil`):
  - Server config `default_jwt_svid_ttl` (default 5m), `jwt_key_type` (default the CA key type, `ec-p256`,
    so JWT-SVIDs are ES256), `jwt_issuer` (the `iss` claim; absent by default).
  - `spire-server entry create ... -jwtSVIDTTL <seconds>` sets one entry's JWT-SVID TTL.
  - The agent caches a JWT-SVID per identity and audience and hands the cached one out until it is at about
    half its lifetime (± 10 % jitter); if the server cannot be reached it may return an older **unexpired**
    cached SVID. A returned SVID can therefore have much less than its TTL left.
  - `spire-server bundle show -format spiffe` prints the trust bundle as a SPIFFE bundle: a JWKS whose keys
    carry `use` `x509-svid` or `jwt-svid`; JWT authorities have a `kid`.
- **Images** (all present in their registries): `ghcr.io/spiffe/spire-server:1.15.3`,
  `ghcr.io/spiffe/spire-agent:1.15.3`, `ghcr.io/spiffe/spiffe-csi-driver:0.2.13`,
  `registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.18.0`.

## 3. Design

### 3.1 Secrets file

A top-level `spiffe` object configures one Workload API client per worker:

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

- `endpoint`: `unix://` with an absolute path and nothing else (no host, user info, query or fragment); or,
  only when `EACP_ENV` is `development` or `test`, `tcp://` to a loopback IP literal with a port (the Windows
  tests' fake agent). Any other form rejects the whole file.
- `spiffe_id`: required; a valid SPIFFE ID (`spiffeid.FromString`) of at most 2048 bytes. It is sent as the
  request's `Subject`, and an SVID whose `sub` differs is refused (`spiffe_invalid`): a registration mistake
  never lends the worker another identity.
- `value_spiffe` is a fourth static-credential form beside `value`, `value_file` and `value_vault` (still
  exclusive with `oauth2`). `client_assertion_spiffe` is a sixth client-authentication form, exclusive with
  `client_secret`, `client_secret_file`, `client_secret_vault`, `client_assertion_file` and `private_key_jwt`.
- An audience is 1–256 printable ASCII characters without spaces. A `_spiffe` field without the `spiffe` block
  rejects the file. **The worker never contacts the agent at load.**

### 3.2 The Workload API client (`internal/worker/spiffe.go`)

- One `spiffeClient` per secrets file; the go-spiffe `Client` is created at the first fetch, never at load
  (creation that fails is retried at the next fetch; gRPC reconnects an existing client by itself).
- `svid(ctx, audience, minLife)` returns a JWT-SVID for `audience` that lives at least `minLife`, or a failure
  class. Per audience it caches the latest SVID; the cache answers while `now + minLife` is before its `exp`.
  Otherwise it fetches: **one fetch per audience at a time** with a 10 s timeout; callers queued behind a fetch
  that fails get its class without a fetch of their own (as 24d's Vault reads).
- A response is accepted only when, beyond go-spiffe's checks: `sub` equals `spiffe_id`; the token is 1 B–16 KiB
  of printable ASCII without spaces (it is sent as a header or form value); `exp` is after the fetch started.
  The worker never verifies the signature: only the relying party judges it.
- **Failure classes:** `spiffe_unavailable` (the client cannot be created, transport errors, `Unavailable`,
  `DeadlineExceeded`, any other gRPC status), `spiffe_denied` (`PermissionDenied`: no entry matches the
  worker), `spiffe_invalid` (no SVID, a parse failure, the wrong `sub`, a bad token shape),
  `spiffe_expiring` (a `value_spiffe` SVID with less than 10 s left, e.g. the agent's cached copy while its
  server is unreachable).
- Every SVID received is added to the redaction set until its `exp` plus 24 h (ADR-019 §4). Fetches are logged
  with the audience, class or remaining lifetime only.

### 3.3 Using an SVID

- **`value_spiffe`** (a `spiffeValue` in `secretEntry`): `Credential(…, validFor)` asks for an SVID living at
  least `validFor`.
  - A failure makes the binding unavailable with §5's back-off (1 s doubling to 60 s, counted from the end of
    the failed fetch, grown once however many callers shared it); `Available()` omits it.
  - A fetched SVID that lives at least 10 s but less than `validFor` is 24a's `ErrCredentialTooShort`: kept for
    shorter calls, no back-off. It is remembered like a token's lifetime, so an equally long call fails at once
    until a later fetch returns a longer-lived SVID.
  - `Rejected` (the target answered `unauthorized`) drops the cached SVID; the next call fetches again (the agent
    may return the same one; the attempt's outcome is never reclassified).
  - `Values()` holds the latest SVID until its `exp`, and the one it replaced until that one's `exp`.
- **`client_assertion_spiffe`**: each mint asks for an SVID living at least the token request timeout (10 s)
  and sends it exactly as §3a sends an assertion (RFC 7523 §2.2, `client_id`, no `Authorization` header). A
  failure is a failed mint with the `spiffe_*` class (§3's 1–60 s back-off, withheld from claims); an SVID
  living less than 10 s is `assertion_expired`. The assertion is kept for `Values()` until its `exp`, as §3a.
  Tokens are cached, capped at an hour and redacted exactly as before.
- Operators give the worker's entry a JWT-SVID TTL of at least twice the longest call budget plus
  `CredentialSkew` (the agent may hand out an SVID at half its life); the demo uses 3600 s.

### 3.4 Kubernetes (Helm chart)

`worker.spiffe` (`enabled` default false, `csiDriver` default `csi.spiffe.io`) adds an inline, read-only
`csi` volume `spiffe-workload-api` to the worker pod only, mounted read-only at `/spiffe-workload-api`
(the SPIFFE CSI driver places the agent's socket there as `spire-agent.sock`). `csi` volumes are allowed by
the `restricted` Pod Security Standard; a hostPath socket would not be. No egress rule is needed (a local
socket). `validate.yaml` refuses an empty `csiDriver` when enabled. The secrets file names the socket.

### 3.5 Fake ERP

- `ParseSPIFFEBundle(raw)`: the `jwt-svid` keys of a SPIFFE bundle (RSA or P-256, with a `kid`); a bundle
  without one is an error.
- **SPIFFE OAuth client** (`EACP_FAKEERP_OAUTH_SPIFFE_{CLIENT_ID,ISSUER,AUDIENCE,SUBJECT,BUNDLE_FILE}`): 24b's
  RFC 7523 check, but RS256 or ES256 against the bundle's keys by `kid`, with exactly `iss` = issuer,
  `sub` = subject and the audience in `aud`. All five or none.
- **Direct JWT-SVID bearer on the ERP API** (`EACP_FAKEERP_SPIFFE_{AUDIENCE,SUBJECT,BUNDLE_FILE}`): a Bearer
  that verifies against the bundle (RS256/ES256, `kid`), with exactly that `sub`, the audience in `aud` and
  a current `exp`/`nbf` (30 s skew), authorises like the static credential. Its principal is
  `spiffe:<sub>`; the audit records the SVID's SHA-256, never the SVID. A JWT-SVID is a bearer token: it can
  be replayed until `exp`, and its audience limits where. All three or none.

### 3.6 SPIRE on minikube (development only)

- `deployments/k8s/dev/spire.yaml`, namespace `spire` (Pod Security `privileged`, pinned to nothing):
  SPIRE server (StatefulSet, SQLite on an emptyDir, trust domain `eacp.test`, `k8s_psat` node attestor for
  cluster `eacp-e2e`, `jwt_issuer` `https://spire.eacp.test`, `default_jwt_svid_ttl` 5m); agent DaemonSet
  (`k8s_psat`, the `k8s` workload attestor with `hostPID`, socket in a hostPath directory the CSI driver
  shares); SPIFFE CSI driver DaemonSet with the node-driver-registrar and its `CSIDriver` object.
- `scripts/k8s-e2e.sh` loads the four images, applies `spire.yaml` before the other dev manifests, waits for
  the server and agents, creates a node-alias entry (selector `k8s_psat:cluster:eacp-e2e`) and the worker's
  entry (`spiffe://eacp.test/ns/eacp/sa/eacp-worker`, selectors `k8s:ns:eacp`, `k8s:sa:eacp-worker`,
  `-jwtSVIDTTL 3600`), and adds the bundle (`bundle show -format spiffe`) to ConfigMap `fakeerp-federation` as
  `spiffe-bundle.json`. It merges `deployments/k8s/connector-secrets.spiffe.json` (the `spiffe` block and two
  bindings) into the worker's secrets and sets `worker.spiffe.enabled` in the e2e values. The bundle is a
  snapshot: the demo does not follow SPIRE key rotation (default CA TTL 24 h).
- Compose is unchanged: its worker has no `spiffe` block.

### 3.7 Demo

`TestSPIFFEDemo` (Kubernetes only, `EACP_DEMO_PLATFORM=k8s`), tenant `…00a8` "umbrella", connector bindings
`fakeerp-spiffe` (audience `fakeerp-api`) and `fakeerp-spiffe-oauth` (client `eacp-worker-spiffe`, audience
`fakeerp-token`):

1. tenant, cast, allow-ERP policy and the procurement agent with both connectors;
2. a purchase through the direct binding: `SUCCEEDED`, one PO, the ERP audit's principal is
   `spiffe:spiffe://eacp.test/ns/eacp/sa/eacp-worker` with an SVID digest;
3. a purchase through the OAuth binding: `SUCCEEDED`, one PO, a `token_issued` audit to the SPIFFE client and
   a purchase by the minted token;
4. no SVID or minted token in API responses, the service logs or the database: 24b's scan
   (`containsAssertion` with the SVID and assertion digests from the ERP audit, and any JWT whose `sub` is the
   worker's SPIFFE ID; `containsIssued` for the minted tokens).

A withheld purchase is not shown: the worker caches its SVID up to its TTL, so removing the registration entry
would not stop the next purchase in a demo's time. The Go tests cover every failure path.

### 3.8 Proof (tests first)

- `internal/worker`: a fake Workload API (`workload.SpiffeWorkloadAPIServer` over loopback TCP, rejecting calls
  without the `workload.spiffe.io` header, scripted per audience: SVIDs signed by `internal/jwttest`, gRPC
  errors, delays). Load validation (every field and exclusivity, dev-only tcp, no contact at load); caching and
  one fetch per audience (`-race`); the wrong `sub`; each failure class with back-off and `Available()`;
  too-short; `Rejected`; redaction and `Values()`; the OAuth assertion path; a worker-loop integration test
  that buys through both bindings against Fake ERP.
- `internal/fakeerp`: `ParseSPIFFEBundle`; the SPIFFE OAuth client and the direct bearer accept a good SVID and
  refuse a wrong `kid`, algorithm, signature, issuer, subject, audience or an expired one; env options fail
  closed.
- `test/helm`: the CSI volume only on the worker, read-only, off by default, driver validation.
- minikube: `TestSPIFFEDemo` plus the existing k8s demos in a fresh `scripts/k8s-e2e.sh` run.

### 3.9 Documentation

ADR-019 Rev 1.4 (§3d, redaction, failure handling, proof, unresolved assumptions), MASTER_PLAN §96 status,
AGENTS.md (status line and the credential rule), README, docs/KUBERNETES.md (`worker.spiffe`, SPIRE on the
e2e cluster), docs/DEMO.md (`TestSPIFFEDemo`), docs/INVARIANTS.md if a custody test is added to the map,
`research/REFERENCES.md` (go-spiffe and SPIRE notes).

## 4. Out of scope

X509-SVIDs and mTLS to connectors; SPIFFE federation between trust domains; verifying SVIDs in the worker;
watching the Workload API (the worker fetches on demand); the SPIRE Controller Manager; SPIRE on compose;
token exchange, cloud STS, Vault dynamic secrets and HSM/KMS keys (later 24 sub-phases).

## 5. Unresolved assumptions (conservative choices, recorded in ADR-019)

- `spiffe_id` is required rather than taking whatever identity the agent offers first.
- tcp endpoints are refused outside development and test: the Workload API authenticates callers by process,
  which a TCP listener cannot do.
- Any unexpected gRPC status is `spiffe_unavailable` (withhold and retry) rather than a permanent refusal.
- An SVID is never used past its `exp` and never for a call it could expire during; the agent's cached copy
  after a server outage is used only while it still outlives the call.
