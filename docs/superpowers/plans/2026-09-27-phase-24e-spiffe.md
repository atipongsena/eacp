# Phase 24e — SPIFFE JWT-SVIDs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The execution worker can authenticate a connector binding with a SPIFFE JWT-SVID fetched from the SPIRE agent's Workload API, as a direct Bearer (`value_spiffe`) or as an OAuth client assertion (`client_assertion_spiffe`), proven on minikube against a real SPIRE.

**Architecture:** A lazily created go-spiffe `workloadapi.Client` in `internal/worker/spiffe.go` fetches SVIDs per audience and caches them; a `spiffeValue` is a fourth kind of `secretEntry`, and the OAuth provider gains an SVID assertion source. Fake ERP verifies SVIDs against SPIRE's JWT bundle, the chart mounts the Workload API socket into the worker through the SPIFFE CSI driver, and a dev-only SPIRE runs on the e2e cluster.

**Tech Stack:** Go 1.27, `github.com/spiffe/go-spiffe/v2` v2.8.2 (workloadapi, svid/jwtsvid, spiffeid, proto/spiffe/workload), gRPC, Helm 4.3.0, SPIRE 1.15.3, spiffe-csi-driver 0.2.13, csi-node-driver-registrar v2.18.0, minikube.

**Spec:** `docs/superpowers/specs/2026-09-27-spiffe-jwt-svid-design.md` (read it with this plan).

## Global Constraints

- ADR-019 Rev 1.4; the spec's §3.1 field names are fixed: `spiffe.endpoint`, `spiffe.spiffe_id`, `value_spiffe.audience`, `oauth2.client_assertion_spiffe.audience`.
- `endpoint`: `unix://` + absolute path only; `tcp://<loopback IP literal>:<port>` only with `AllowPlainTokenURL()` (development and test). `spiffe_id` ≤ 2048 bytes, valid per `spiffeid.FromString`. Audience: 1–256 printable ASCII, no spaces (regexp `^[\x21-\x7e]{1,256}$`).
- Fetch timeout 10 s (`tokenRequestTimeout`); SVID token 1 B–16 KiB (`maxAssertion`), printable ASCII without spaces (`tokenPattern`); minimum usable life 10 s.
- Failure classes exactly: `spiffe_unavailable`, `spiffe_denied`, `spiffe_invalid`, `spiffe_expiring` (value path); the OAuth path reports `assertion_expired` for an SVID with < 10 s left.
- Back-off: 1 s doubling to 60 s (`minMintBackoff`, `maxMintBackoff`), counted from the end of the failed fetch.
- Redaction: each SVID until `exp` + 24 h (`redactAfterExpiry`). Never log an SVID; log audience, class, remaining lifetime.
- The worker never contacts the agent at load and never verifies an SVID's signature.
- Fake ERP env: `EACP_FAKEERP_OAUTH_SPIFFE_{CLIENT_ID,ISSUER,AUDIENCE,SUBJECT,BUNDLE_FILE}` (all or none), `EACP_FAKEERP_SPIFFE_{AUDIENCE,SUBJECT,BUNDLE_FILE}` (all or none). Direct-bearer principal `spiffe:<sub>`; audit field `svid_sha256`.
- Demo: tenant `00000000-0000-4000-8000-0000000000a8` ("umbrella"), refs `fakeerp-spiffe` (audience `fakeerp-api`) and `fakeerp-spiffe-oauth` (client `eacp-worker-spiffe`, audience `fakeerp-token`, token_url `http://fakeerp:8090/oauth/token`), worker ID `spiffe://eacp.test/ns/eacp/sa/eacp-worker`, SPIRE `jwt_issuer` `https://spire.eacp.test`, entry `-jwtSVIDTTL 3600`.
- AGENTS.md rules: tests first, `go test -race`, never weaken a test, commit as the user only with no Co-Authored-By trailer.

## Review Focus

1. **Many callers while the agent is down:** one fetch per audience per failure, queued callers share its class (Task 3 test `TestQueuedCallersShareAFailedFetch`).
2. **Two bindings with the same audience:** they share the cached SVID (one fetch) but back off independently (Task 4 test `TestBindingsSharingAnAudienceFetchOnce`).
3. **An SVID for the wrong audience or subject, or with whitespace / oversize:** `spiffe_invalid`, nothing sent (Task 3 test `TestAResponseMustBeTheWorkersSVIDForTheAudience`).
4. **The agent's stale cached copy with seconds left:** `spiffe_expiring` with back-off on the value path, `assertion_expired` on the OAuth path; never sent (Tasks 4 and 5).
5. **`Rejected` with an SVID that is no longer current:** drops nothing (Task 4 test `TestRejectedDropsOnlyTheCurrentSVID`).

---

### Task 1: go-spiffe dependency and the fake Workload API (`internal/spiffetest`)

**Files:**
- Modify: `go.mod`, `go.sum`
- Create: `internal/spiffetest/spiffetest.go`, `internal/spiffetest/spiffetest_test.go`

**Interfaces:**
- Produces (tests only, like `internal/jwttest`):
  - `type Agent struct{ Signer *jwttest.Signer; ... }`
  - `func New(t testing.TB) *Agent` — serves `workload.SpiffeWorkloadAPIServer` on `127.0.0.1:0`, stops on `t.Cleanup`.
  - `func (a *Agent) Addr() string` — `"tcp://127.0.0.1:<port>"`.
  - `func (a *Agent) Handle(f func(req *workload.JWTSVIDRequest) (string, error))` — scripts FetchJWTSVID; a returned error is sent as-is (use `status.Error(codes.X, …)`); the string is the SVID.
  - `func (a *Agent) SVID(sub string, aud []string, ttl time.Duration) string` — RS256 via `Signer`, claims `sub`, `aud`, `exp` = now+ttl, `iat` = now.
  - `func (a *Agent) Fetches(audience string) int` — FetchJWTSVID calls whose first audience is `audience`.
  - Default handler: an SVID for `req.SpiffeId` and `req.Audience` living 1 h.
  - Calls without metadata `workload.spiffe.io: true` get `codes.InvalidArgument` and are not counted.

- [ ] **Step 1:** `go get github.com/spiffe/go-spiffe/v2@v2.8.2 && go mod tidy` (after Step 2's file imports it). Confirm `go.mod` lists go-spiffe v2.8.2 and grpc stays ≥ v1.83.2.
- [ ] **Step 2: Write the failing test** `TestTheFakeAgentServesGoSpiffe` in `spiffetest_test.go`: `a := New(t)`; `c, _ := workloadapi.New(ctx, workloadapi.WithAddr(a.Addr()))`; `svid, err := c.FetchJWTSVID(ctx, jwtsvid.Params{Audience: "aud-1", Subject: spiffeid.RequireFromString("spiffe://eacp.test/w")})` → `err == nil`, `svid.ID.String() == "spiffe://eacp.test/w"`, `a.Fetches("aud-1") == 1`; with `a.Handle` returning `status.Error(codes.PermissionDenied, "no identity")`, `status.Code(err) == codes.PermissionDenied`. `TestTheFakeAgentRequiresTheHeader`: a raw `workload.NewSpiffeWorkloadAPIClient` call without metadata → `codes.InvalidArgument`, `Fetches == 0`.
- [ ] **Step 3:** Run `go test ./internal/spiffetest/` → FAIL (package missing).
- [ ] **Step 4:** Implement `spiffetest.go` (embed `workload.UnimplementedSpiffeWorkloadAPIServer`; read metadata with `metadata.FromIncomingContext`).
- [ ] **Step 5:** `go test -race ./internal/spiffetest/` → PASS; `go vet ./...` clean.
- [ ] **Step 6: Commit** `test(spiffetest): a fake SPIFFE Workload API for the worker's tests` (go.mod, go.sum, internal/spiffetest).

### Task 2: The `spiffe` block and the two credential forms at load

**Files:**
- Create: `internal/worker/spiffe.go`, `internal/worker/spiffe_test.go`
- Modify: `internal/worker/secrets.go` (file struct, kinds count, `secretEntry.spiffe`), `internal/worker/oauth2.go` (`oauthEntry.ClientAssertionSPIFFE`, kinds count)

**Interfaces:**
- Produces:
  - `type spiffeEntry struct{ Endpoint string `json:"endpoint"`; SPIFFEID string `json:"spiffe_id"` }`
  - `type spiffeAudience struct{ Audience string `json:"audience"` }` with `func (a spiffeAudience) validate() error`
  - `type spiffeClient struct{ endpoint string; id spiffeid.ID; now func() time.Time; redact *logging.SecretSet; log *slog.Logger; ... }`
  - `func newSpiffeClient(e spiffeEntry, c loadConfig) (*spiffeClient, error)` — validation only; no dial.
  - `loadConfig.spiffe *spiffeClient`; `secretEntry.spiffe *spiffeValue` (type defined in Task 4; here a struct with `client`, `audience`, `binding`, `now`, `log` fields is enough).
  - `oauthEntry.ClientAssertionSPIFFE *spiffeAudience `json:"client_assertion_spiffe"``; `oauthProvider.spiffe *spiffeClient`, `oauthProvider.spiffeAudience string`.

- [ ] **Step 1: Write the failing tests** in `spiffe_test.go` (write secrets files with `t.TempDir()`, as `vaultsecrets_test.go` does):
  - `TestSPIFFEBlockLoads`: `{"spiffe": {"endpoint": "unix:///spiffe-workload-api/spire-agent.sock", "spiffe_id": "spiffe://eacp.test/ns/eacp/sa/eacp-worker"}, "secrets": [value_spiffe entry, oauth2 entry with client_assertion_spiffe]}` loads; `Bindings()` has both; nothing is dialled (the endpoint does not exist).
  - `TestSPIFFEBlockFailsClosed` (table; each rejects the whole file and the error contains no audience/ID value): endpoint `unix://host/x`, `unix:relative`, `unix:///x?q=1`, `unix:///x#f`, `http://x`, `tcp://127.0.0.1:1` without `AllowPlainTokenURL()`, `tcp://10.0.0.1:1` with it, `tcp://localhost:1` with it (not an IP literal), `tcp://127.0.0.1` (no port); spiffe_id `""`, `spiffe://`, `https://x`, 2049 bytes; audience `""`, `"a b"`, 257 bytes; `value_spiffe` without the block; `client_assertion_spiffe` without the block; `value_spiffe` together with `value`; `client_assertion_spiffe` together with `client_secret`; unknown member in `spiffe`.
  - `TestTCPEndpointOnlyInDevelopment`: `tcp://127.0.0.1:8081` and `tcp://[::1]:8081` load with `AllowPlainTokenURL()`.
- [ ] **Step 2:** `go test ./internal/worker -run 'SPIFFE|TCPEndpoint'` → FAIL.
- [ ] **Step 3:** Implement: `newSpiffeClient` (parse with `url.Parse`; unix: scheme `unix`, empty host/user/query/fragment/opaque, absolute path; tcp: `c.allowPlain`, `netip.ParseAddrPort(u.Host)` loopback, empty path); `LoadSecrets` reads `file.Spiffe *spiffeEntry`, counts `ValueSPIFFE *spiffeAudience` as a fourth static kind (error text "exactly one of value, value_file, value_vault, value_spiffe and oauth2 is required"); `newOAuthProvider` counts `ClientAssertionSPIFFE` as a sixth kind and needs `c.spiffe`.
- [ ] **Step 4:** Run the tests → PASS; `go test -race ./internal/worker` → PASS (24a–24d tests unchanged).
- [ ] **Step 5: Commit** `feat(worker): the spiffe block, value_spiffe and client_assertion_spiffe in the secrets file`.

### Task 3: Fetching and caching SVIDs (`spiffeClient.svid`)

**Files:** Modify `internal/worker/spiffe.go`, `internal/worker/spiffe_test.go`

**Interfaces:**
- Produces:
  - `func (s *spiffeClient) svid(ctx context.Context, audience string, minLife time.Duration) (Secret, time.Time, string)` — the cached SVID for `audience` while `now + minLife` is before its `exp`; otherwise one fetch (per-audience lock, 10 s timeout) whose SVID is returned whatever its remaining life (callers judge it). Returns the SVID, its `exp` and a class (`""` on success).
  - `func (s *spiffeClient) drop(audience string, svid Secret)` — forgets the cached SVID if it is `svid`.
- Behaviour: `Params{Audience: audience, Subject: s.id}`; class by `status.Code(err)` — `PermissionDenied` → `spiffe_denied`; a go-spiffe parse error (not a gRPC status: `status.FromError` reports `ok == false` and the error does not come from the transport) or `sub != s.id` or token not `tokenPattern` / > `maxAssertion` → `spiffe_invalid`; everything else (including `workloadapi.New` failing) → `spiffe_unavailable`. Queued callers behind a failed fetch get its class without fetching (the `attempts` counter pattern of `vaultClient.value`). Each SVID received goes to `redact.Add(v, exp+redactAfterExpiry)`. Logs: `"spiffe svid fetched"` (audience, expires_in) / `"spiffe svid fetch failed"` (audience, class).
- Note: go-spiffe checks `exp` against the real clock, so tests sign SVIDs relative to `time.Now()` and give the store a clock near it (`WithClock(func() time.Time { return time.Now().Add(offset) })`).

- [ ] **Step 1: Write the failing tests** (each builds a `spiffeClient` pointed at `spiffetest.New(t).Addr()` with `AllowPlainTokenURL()`):
  - `TestAnSVIDIsFetchedOnceAndCached`: two `svid(ctx, "a", 30s)` calls → same SVID, `Fetches("a") == 1`; `svid(ctx, "b", …)` → a second fetch.
  - `TestAShortRemainingLifeFetchesAgain`: agent SVID ttl 40 s; `svid(…, 30s)` fetches; with the clock 15 s ahead, `svid(…, 30s)` fetches again.
  - `TestAResponseMustBeTheWorkersSVIDForTheAudience`: handler returns an SVID with another `sub` → `spiffe_invalid`; with `aud` lacking the audience → `spiffe_invalid`; with a space inside the token → `spiffe_invalid`; `codes.PermissionDenied` → `spiffe_denied`; `codes.Unavailable` → `spiffe_unavailable`; a stopped agent → `spiffe_unavailable`.
  - `TestQueuedCallersShareAFailedFetch`: handler blocks until released and returns `Unavailable`; 20 goroutines call `svid`; after release all get `spiffe_unavailable` and `Fetches == 1` (`-race`).
  - `TestEverySVIDIsRedactedUntilADayAfterItExpires`: with a `logging.SecretSet`, a fetched SVID is redacted; at `exp + 24h + 1s` it no longer is.
  - `TestNothingContactsTheAgentAtLoad`: loading a file whose endpoint points at the fake agent leaves `Fetches == 0`.
- [ ] **Step 2:** Run → FAIL. **Step 3:** Implement. **Step 4:** `go test -race ./internal/worker -run 'SVID|Agent|Queued'` → PASS.
- [ ] **Step 5: Commit** `feat(worker): fetch JWT-SVIDs from the Workload API per audience, cached while they outlive the call`.

### Task 4: `value_spiffe` bindings in the secret store

**Files:** Modify `internal/worker/spiffe.go`, `internal/worker/secrets.go`, `internal/worker/spiffe_test.go`

**Interfaces:**
- Produces `type spiffeValue struct{ client *spiffeClient; audience string; binding Binding; now func() time.Time; log *slog.Logger; mu sync.Mutex; current, previous Secret; currentExp, previousExp time.Time; lifetime time.Duration; backoff time.Duration; backoffUntil time.Time }` with:
  - `func (v *spiffeValue) credential(ctx context.Context, validFor time.Duration) (Secret, error)`
  - `func (v *spiffeValue) available(now time.Time) bool`
  - `func (v *spiffeValue) rejected(s Secret)`
  - `func (v *spiffeValue) live(now time.Time) []string`
- `SecretStore.Credential`, `Available`, `Rejected`, `Values` gain the `e.spiffe` case (Credential passes `validFor`).
- Rules (spec §3.3): during back-off → `ErrCredentialUnavailable` without a fetch; remembered `lifetime` ≤ `validFor` and no cached SVID serving the call → `ErrCredentialTooShort` without a fetch; a fetch class → back-off (grown once when concurrent callers share a failure, as `vaultValue.credential`), `ErrCredentialUnavailable`; a fetched SVID with < 10 s left → class `spiffe_expiring`, back-off; ≥ 10 s but ≤ `validFor` → keep it, `lifetime` = exp − fetch start, `ErrCredentialTooShort`, no back-off; otherwise serve it (the one replaced becomes `previous` until its `exp`).

- [ ] **Step 1: Write the failing tests** (store loaded from a file with the fake agent's tcp endpoint):
  - `TestAValueSPIFFEBindingServesAnSVIDForItsAudience`: `Credential(ctx, tenant, "erp-spiffe", "http://erp.internal:8443/v1", 33*time.Second)` returns the SVID; its `aud` is the binding's audience; `Resolve` for another host → `ErrNoCredential`.
  - `TestAnAgentFailureWithholdsTheBinding`: `Unavailable` → `ErrCredentialUnavailable`, `Available()` lacks the binding for 1 s, then 2 s after a second failure; a success resets it.
  - `TestAnSVIDShorterThanTheCallIsTooShort`: agent ttl 60 s, `validFor` 90 s → `ErrCredentialTooShort`, binding still in `Available()`; a 20 s call is served from the same SVID without a fetch; a second 90 s call returns `ErrCredentialTooShort` without a fetch.
  - `TestAnSVIDAboutToExpireIsNeverSent`: agent ttl 5 s → `ErrCredentialUnavailable`, back-off, class `spiffe_expiring` in the log.
  - `TestBindingsSharingAnAudienceFetchOnce`: two bindings, same audience → one fetch; a failure backs off both bindings only after each asks (independent back-offs).
  - `TestRejectedDropsOnlyTheCurrentSVID`: `Rejected` with an old SVID → next `Credential` is served from cache (no fetch); with the current SVID → next `Credential` fetches.
  - `TestValuesHoldTheSVIDsUntilTheyExpire`: `Values()` contains the current SVID and, after a refetch, the previous one until its `exp`.
- [ ] **Step 2:** Run → FAIL. **Step 3:** Implement. **Step 4:** `go test -race ./internal/worker` → PASS.
- [ ] **Step 5: Commit** `feat(worker): value_spiffe - a JWT-SVID for the connector's audience as its credential`.

### Task 5: `client_assertion_spiffe` in the OAuth provider

**Files:** Modify `internal/worker/oauth2.go`, `internal/worker/spiffe_test.go` (or a new `internal/worker/spiffeoauth_test.go`)

**Interfaces:**
- Consumes `spiffeClient.svid` (Task 3).
- In `oauthProvider.request`: when `p.spiffe != nil`, `a, exp, class := p.spiffe.svid(ctx, p.spiffeAudience, tokenRequestTimeout)`; a class is a failed mint with that class; `exp` before `now + tokenRequestTimeout` → `assertion_expired`; otherwise it is the assertion (kept as `p.assertion`/`p.assertionExp`, sent exactly as §3a). `rejected` also calls `p.spiffe.drop(p.spiffeAudience, p.assertion)`.

- [ ] **Step 1: Write the failing tests** with an `httptest` token endpoint (as `oauth_test.go` in this package does):
  - `TestASPIFFEAssertionAuthenticatesTheMint`: the form has `client_id`, `client_assertion_type` jwt-bearer and `client_assertion` equal to an SVID whose `aud` is the configured audience; no `Authorization` header; the token is returned.
  - `TestEveryMintFetchesAFreshAssertionOnlyWhenNeeded`: two mints within the SVID's life use one fetch; a mint after the SVID has < 10 s left fetches again.
  - `TestASPIFFEFailureIsAFailedMint`: `PermissionDenied` → `ErrCredentialUnavailable`, no token request made, binding out of `Available()` for 1 s; agent SVID with 5 s left → no token request, class `assertion_expired`.
  - `TestTheSVIDAssertionIsRedactedAndScrubbed`: the assertion is in the redaction set and in `Values()` until its `exp`.
- [ ] **Step 2:** Run → FAIL. **Step 3:** Implement. **Step 4:** `go test -race ./internal/worker` → PASS.
- [ ] **Step 5: Commit** `feat(worker): client_assertion_spiffe - a JWT-SVID as the OAuth client assertion`.

### Task 6: Fake ERP verifies JWT-SVIDs

**Files:**
- Create: `internal/fakeerp/spiffe.go`, `internal/fakeerp/spiffe_test.go`
- Modify: `internal/fakeerp/erp.go` (`Options`, `principal`, `privileged`, `begin`, `issue`, `audit.SVIDSHA256`), `cmd/fakeerp/main.go`, `cmd/fakeerp/main_test.go`

**Interfaces:**
- Produces:
  - `func ParseSPIFFEBundle(raw []byte) ([]PublicKey, error)` — keys with `"use": "jwt-svid"` and a `kid`, parsed like `ParseKeySet` (RSA, P-256); none → error.
  - `type SPIFFEClient struct{ ClientID, Issuer, Audience, Subject string; Keys []PublicKey }`, `func (c *SPIFFEClient) verifyAssertion(form url.Values, now time.Time) bool` — alg RS256 or ES256, key by `kid`, `iss`, `sub` exact, `aud` has Audience, `timely(now, 0)`.
  - `type SPIFFEBearer struct{ Audience, Subject string; Keys []PublicKey }`, `func (b *SPIFFEBearer) verify(token string, now time.Time) bool` — same checks without `iss`.
  - `Options.SPIFFEClient *SPIFFEClient`, `Options.SPIFFEBearer *SPIFFEBearer`; the token endpoint is served when `SPIFFEClient != nil` too; client ids must differ from Federated/KeyClient ids; `principal` returns `"spiffe:" + Subject` for a verified SVID and `begin` records `SVIDSHA256`; `privileged` accepts the `spiffe:` prefix.
  - `cmd/fakeerp`: `loadSPIFFEClient(getenv)`, `loadSPIFFEBearer(getenv)` (all-or-none, unreadable/invalid bundle fails startup).

- [ ] **Step 1: Write the failing tests** (ES256 test key via `ecdsa.GenerateKey(elliptic.P256(), …)` and a small signer in the test; RS256 via `jwttest`; a bundle JSON with one `x509-svid` key and one `jwt-svid` key):
  - `TestParseSPIFFEBundleKeepsOnlyJWTAuthorities`, `TestABundleWithoutJWTAuthoritiesIsRefused`.
  - `TestASPIFFEAssertionMintsATokenForTheSPIFFEClient`: ES256 and RS256 SVIDs mint; audit `token_issued`, principal `oauth:eacp-worker-spiffe`, `assertion_sha256` set.
  - `TestTheSPIFFEClientRefusesBadSVIDs` (table): unknown `kid`, `alg` HS256/none/PS256, bad signature, wrong `iss`, wrong `sub`, missing audience, expired beyond 30 s skew, `nbf` in the future → 401 `invalid_client`.
  - `TestAnSVIDBearerAuthorisesTheERPAPI`: POST `/v1/execute` with the SVID as Bearer → 200/201 as with the static token; audit principal `spiffe:spiffe://eacp.test/ns/eacp/sa/eacp-worker`, `svid_sha256` = hex SHA-256 of the SVID, the SVID itself absent from the log file.
  - `TestAnSVIDBearerMustMatch` (table as above minus `iss`) → 401 `unauthorized`.
  - `cmd/fakeerp`: `TestSPIFFESettingsGoTogether` (partial env → error), `TestSPIFFESettingsLoad`.
- [ ] **Step 2:** Run `go test ./internal/fakeerp ./cmd/fakeerp` → FAIL. **Step 3:** Implement. **Step 4:** `go test -race ./internal/fakeerp ./cmd/fakeerp` → PASS.
- [ ] **Step 5: Commit** `feat(fakeerp): verify JWT-SVIDs - a SPIFFE OAuth client and SVID bearers`.

### Task 7: Worker-loop integration (`internal/worker/spiffe_integration_test.go`)

**Files:** Create `internal/worker/spiffe_integration_test.go`

**Interfaces:** Consumes Tasks 1–6 and the harness in `jit_integration_test.go`: `newJITFile(t, erpFor func(tokenURL string) fakeerp.Options, fileFor func(tokenURL, host string) string, timeoutMS int) *jitEnv` (one binding, tenant A's `erp-jit` for `host`), `v.submit("")`, `v.run(n)`, `v.get(id)`, `v.audit()`, `v.assertNotPersisted(values...)` (actions, attempts, audit events, outbox and the worker log). All three tests need `EACP_TEST_ADMIN_DSN`; the fake agent's SVIDs carry `iss` `https://spire.test` (custom `Handle`), and the file's `spiffe` block uses the agent's tcp address.

- [ ] **Step 1: Write the tests:**
  - `TestTheWorkerExecutesWithAnSVID`: Fake ERP `SPIFFEBearer{Audience: "fakeerp-api", Subject: id, Keys: the agent signer's key}`; binding `value_spiffe` `{"audience":"fakeerp-api"}`; one purchase → `SUCCEEDED`, the ERP audit's execute principal is `spiffe:<id>` with `svid_sha256`; `assertNotPersisted(svid)` for every SVID the agent issued.
  - `TestTheWorkerMintsWithAnSVIDAssertion`: Fake ERP `SPIFFEClient{ClientID: "eacp-worker-spiffe", Issuer: "https://spire.test", Audience: "fakeerp-token", Subject: id, …}`; binding `oauth2` with `client_assertion_spiffe` `{"audience":"fakeerp-token"}`; one purchase → `SUCCEEDED`, principal `oauth:eacp-worker-spiffe`; `assertNotPersisted` for the SVIDs and the issued tokens.
  - `TestAnAgentRefusalWithholdsWork`: as the first, with the agent answering `PermissionDenied`: `v.run(0)` after the first failed claim (the binding is out of `Available()`), the action stays `QUEUED` with `attempt_count` 0; after restoring the handler and advancing `v.clock` past the back-off, the action `SUCCEEDED`s.
- [ ] **Step 2:** `go test -race ./internal/worker -run 'SVID|AgentRefusal' -count=1` with the DSN → PASS (the implementation exists; if it fails, fix the implementation, not the test).
- [ ] **Step 3: Commit** `test(worker): the worker buys through SVID bindings and withholds work while the agent refuses`.

### Task 8: Helm chart `worker.spiffe`

**Files:** Modify `deployments/helm/eacp/values.yaml`, `templates/worker.yaml`, `templates/validate.yaml`, `test/helm/identity_test.go`, `test/helm/workloads_test.go`

- [ ] **Step 1: Write the failing tests** (`EACP_HELM_REQUIRED=1 go test ./test/helm`):
  - `TestTheSPIFFESocketIsTheWorkersOwn`: with `--set worker.spiffe.enabled=true`, only the worker has a volume `spiffe-workload-api` with `csi: {driver: csi.spiffe.io, readOnly: true}` and a read-only mount at `/spiffe-workload-api`; `--set worker.spiffe.csiDriver=csi.example.com` changes the driver; off by default (no such volume anywhere).
  - `workloads_test.go` validation table: `"worker.spiffe.enabled=true,worker.spiffe.csiDriver=": "spiffe.csiDriver"`.
- [ ] **Step 2:** Run → FAIL. **Step 3:** Implement (`worker.spiffe: {enabled: false, csiDriver: csi.spiffe.io}` with an ADR-019 Rev 1.4 comment; `validate.yaml` fails `"worker.spiffe.csiDriver must be set when worker.spiffe is enabled"`). **Step 4:** Run → PASS.
- [ ] **Step 5: Commit** `feat(helm): worker.spiffe mounts the SPIFFE Workload API into the worker alone`.

### Task 9: SPIRE on the e2e cluster

**Files:**
- Create: `deployments/k8s/dev/spire.yaml`, `deployments/k8s/connector-secrets.spiffe.json`
- Modify: `deployments/k8s/dev/namespaces.yaml` (namespace `spire`, `pod-security.kubernetes.io/enforce: privileged`), `deployments/k8s/dev/fakes.yaml` (Fake ERP env and the `spiffe-bundle.json` ConfigMap item), `deployments/k8s/e2e-values.yaml` (`worker.spiffe.enabled: true`), `scripts/k8s-e2e.sh`

**Details** (take the manifests' shape from SPIRE's Kubernetes quickstart and the spiffe-csi example for the pinned versions; verify every server/agent config key against `doc/spire_server.md` and `doc/spire_agent.md` at v1.15.3):
- `spire.yaml`: ServiceAccounts, ClusterRoles (server: `tokenreviews` create, `nodes`/`pods` get; agent: `pods`, `nodes`, `nodes/proxy` get), server ConfigMap (`trust_domain = "eacp.test"`, `data_dir`, SQLite datastore, `jwt_issuer = "https://spire.eacp.test"`, `default_jwt_svid_ttl = "5m"`, NodeAttestor `k8s_psat` with `clusters = {"eacp-e2e" = {service_account_allow_list = ["spire:spire-agent"]}}`, KeyManager `memory`), server StatefulSet + Service (8081), agent ConfigMap (`server_address = "spire-server.spire.svc"`, `socket_path = "/run/spire/agent-sockets/spire-agent.sock"`, `insecure_bootstrap = true` — dev only, stated in a comment, WorkloadAttestor `k8s` with `skip_kubelet_verification = true`), agent DaemonSet (`hostPID: true`, `hostNetwork: true`, `dnsPolicy: ClusterFirstWithHostNet`, projected token audience `spire-server`, hostPath `/run/spire/agent-sockets`), CSI driver DaemonSet (driver + node-driver-registrar, `workload-api-socket-dir` = the agent's hostPath) and `CSIDriver csi.spiffe.io` (`podInfoOnMount: true`, `volumeLifecycleModes: [Ephemeral]`, `attachRequired: false`). Every image is pinned by tag and `imagePullPolicy: Never` (loaded by the script).
- `connector-secrets.spiffe.json`: the `spiffe` block (`unix:///spiffe-workload-api/spire-agent.sock`, the worker ID) and the two tenant-`…00a8` bindings (host `fakeerp:8090`).
- `k8s-e2e.sh`: load the four SPIRE images; apply `namespaces.yaml` and `spire.yaml` first; `rollout status` server and agent and CSI DaemonSets; `spire-server entry create` (node alias `spiffe://eacp.test/k8s-nodes` with `-node -selector k8s_psat:cluster:eacp-e2e`; worker `-parentID spiffe://eacp.test/k8s-nodes -spiffeID spiffe://eacp.test/ns/eacp/sa/eacp-worker -selector k8s:ns:eacp -selector k8s:sa:eacp-worker -jwtSVIDTTL 3600`), idempotent (skip when `entry show -spiffeID` finds it); `bundle show -format spiffe` into the `fakeerp-federation` ConfigMap as `spiffe-bundle.json`; merge the spiffe JSON's `spiffe` block and secrets into the worker's secrets (the Python merge step).
- Fake ERP env in `fakes.yaml`: `EACP_FAKEERP_OAUTH_SPIFFE_*` (client `eacp-worker-spiffe`, issuer `https://spire.eacp.test`, audience `fakeerp-token`, the worker ID, `/run/config/fakeerp-federation/spiffe-bundle.json`) and `EACP_FAKEERP_SPIFFE_*` (audience `fakeerp-api`).

- [ ] **Step 1:** Write the files; `EACP_HELM_REQUIRED=1 go test ./test/helm` still PASS (e2e values render).
- [ ] **Step 2:** `KEEP=1 TESTS=NONE HELM=… bash scripts/k8s-e2e.sh` → installed; `kubectl -n eacp exec deploy/eacp-worker -- ls /spiffe-workload-api` is not possible (distroless), so check the worker's log line `worker ready … bindings: 11` and `kubectl -n spire exec spire-server-0 -- /opt/spire/bin/spire-server entry show` lists the worker entry; `kubectl get csidriver csi.spiffe.io`.
- [ ] **Step 3: Commit** `feat(deploy): a development SPIRE on the e2e cluster, bundle and entries from k8s-e2e.sh`.

### Task 10: `TestSPIFFEDemo`

**Files:** Create `test/demo/spiffe_test.go`; modify `scripts/k8s-e2e.sh` (add `TestSPIFFEDemo` to the default `TESTS`)

- [ ] **Step 1: Write the test** following `vault_test.go` and `federation_test.go`: skip unless `EACP_DEMO_PLATFORM=k8s`; tenant `tenantS = "00000000-0000-4000-8000-0000000000a8"`, "umbrella"; `d.secretRef = "fakeerp-spiffe"`, `d.extra = [][2]string{{"erp-oauth", "fakeerp-spiffe-oauth"}}`; steps S0 (tenant, cast, allow policy), S1 (register; `erp.create_po` → principal `spiffe:spiffe://eacp.test/ns/eacp/sa/eacp-worker` with `svid_sha256` set; `erp-oauth.create_po` → `oauth:eacp-worker-spiffe`, a `token_issued` audit with `assertion_sha256`), S2 (responses, `d.p.logs()`, `pg_dump`: `containsIssued` for issued tokens, `containsAssertion(text, svidAndAssertionHashes, workerSPIFFEID)` → 0). Add `SVIDSHA256 string json:"svid_sha256"` to the demo's ERP audit entry type.
- [ ] **Step 2:** Run on the kept cluster: `EACP_DEMO=1 EACP_DEMO_PLATFORM=k8s … go test -count=1 -run TestSPIFFEDemo ./test/demo` → PASS.
- [ ] **Step 3: Commit** `test(demo): the SPIFFE demo on Kubernetes - purchases with a JWT-SVID and a SPIFFE-authenticated OAuth client`.

### Task 11: Documentation and full verification

**Files:** `docs/adr/ADR-019-credential-custody.md` (Rev 1.4: header, §2 forms, new §3d, §4, §5, §7 proof list, unresolved assumptions), `docs/MASTER_PLAN.md` §96 status, `AGENTS.md` (status line; the credential rule gains `value_spiffe`/`client_assertion_spiffe`), `README.md`, `docs/KUBERNETES.md` (`worker.spiffe`, SPIRE in e2e), `docs/DEMO.md` (`TestSPIFFEDemo`), `research/REFERENCES.md` (go-spiffe v2.8.2 and SPIRE 1.15.3 notes from spec §2), `docs/INVARIANTS.md` only if a custody invariant gains a test.

- [ ] **Step 1:** Write the docs (facts only from the spec and the code as built).
- [ ] **Step 2:** `go vet ./... && EACP_TEST_ADMIN_DSN=… go test -race -count=1 -timeout 30m ./...` → all ok; `EACP_HELM_REQUIRED=1 go test ./test/helm` → ok; `(cd internal/ui && node --test jstest/*.test.mjs)` → ok.
- [ ] **Step 3:** Compose: `docker compose up -d --build`, `EACP_COMPOSE_TEST=1 go test -count=1 ./test/security/` → ok (compose unchanged); `DEMO=J scripts/demo.sh` → ok.
- [ ] **Step 4:** A fresh `KEEP=0 HELM=… bash scripts/k8s-e2e.sh` (all default demos including `TestSPIFFEDemo`) → ok.
- [ ] **Step 5: Commit** `docs: ADR-019 Rev 1.4 - SPIFFE JWT-SVIDs; Phase 24e delivered`.
- [ ] **Step 6:** Final whole-branch review (one reviewer, most capable model), fix findings in a separate commit, re-run the affected tests.
