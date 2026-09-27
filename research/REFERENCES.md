# Upstream references (verified)

Facts here were checked against the released artifacts, not the docs alone
(MASTER_PLAN §107: no invented APIs). Re-verify on every version bump.

## Phase 25a — A2A 1.0 and the a2a-go reference implementation (2026-09-27)

Checked against the module source (`go mod download`) of `github.com/a2aproject/a2a-go/v2` **v2.6.0** (`a2a/core.go`, `a2a/agent.go`, `a2a/errors.go`, `a2a/svcparams.go`, `a2asrv/jsonrpc.go`, `a2asrv/handler.go`, `a2asrv/agentcard.go`, `internal/taskexec`) and by running its server in `internal/connector/a2a/interop_test.go`. ADR-030 relies on these.

### Pinned module

- `github.com/a2aproject/a2a-go/v2` **v2.6.0**. Test code imports it (interop only); no EACP binary links it.

### Protocol facts used

- **Card.** Served at `/.well-known/agent-card.json` (`a2asrv.WellKnownAgentCardPath`). `supportedInterfaces` lists `{url, protocolBinding, protocolVersion}`; the JSON-RPC binding is `JSONRPC` and the version `1.0`. Skills are `{id, name, description, tags, examples, …}`.
- **JSON-RPC.** JSON-RPC 2.0 over HTTP POST, one method per request: `SendMessage`, `GetTask`, `CancelTask` (also `SendStreamingMessage`, `SubscribeToTask`, `ListTasks`, push-notification methods and `GetExtendedAgentCard`, unused). The client sends `A2A-Version: 1.0`.
- **Results.** `SendMessage` returns `{"task": …}` or `{"message": …}`; `GetTask` and `CancelTask` return the Task itself. An error is HTTP 200 with `error {code, message}`: `-32001` TaskNotFound, `-32002` TaskNotCancelable, `-32601` MethodNotFound, `-32009` VersionNotSupported.
- **Tasks.** `{id, contextId, status {state, message, timestamp}, artifacts, history, metadata}`. States are `TASK_STATE_SUBMITTED`, `_WORKING`, `_COMPLETED`, `_FAILED`, `_CANCELED`, `_REJECTED`, `_INPUT_REQUIRED`, `_AUTH_REQUIRED`. Messages have `messageId`, `role` (`ROLE_USER`, `ROLE_AGENT`) and flattened parts (`{"text": …}` or `{"data": …, "mediaType": …}`).
- **Blocking.** Without `configuration.returnImmediately`, the server holds a non-streaming `SendMessage` reply until execution ends, interrupting early only on `AUTH_REQUIRED`. With it true, the reply carries the first task event. EACP sets it (interop finding).
- **No deduplication.** The server does not deduplicate by `messageId` (a TODO in `internal/taskexec/local_manager.go`: "handle idempotency once spec establishes the key"), and `ListTasks` filters only by context, state and status time: a task whose reply was lost cannot be found by message id.

### Server API used (interop tests)

- `a2asrv.NewHandler(executor)`, `a2asrv.NewJSONRPCHandler(handler)`, `a2asrv.NewStaticAgentCardHandler(card)`.
- Events: `a2a.NewSubmittedTask`, `a2a.NewStatusUpdateEvent`, `a2a.NewArtifactEvent`; interfaces with `a2a.NewAgentInterface(url, a2a.TransportProtocolJSONRPC)`.

## Phase 24g — AWS STS web identity and SigV4 (2026-09-27)

Checked against the module source (`go mod download`) of `github.com/aws/aws-sdk-go-v2/service/sts` **v1.51.1** (`serializers.go`, `deserializers.go`, `endpoints.go`) and `github.com/aws/aws-sdk-go-v2` **v1.47.1** (`aws/signer/v4`, `aws/credentials.go`). ADR-019 Rev 1.6 relies on these.

### Pinned modules

- `github.com/aws/aws-sdk-go-v2` **v1.47.1** is a direct dependency for the SigV4 signer only; it adds `github.com/aws/smithy-go` **v1.28.1** (indirect). The STS service module is not imported: the worker speaks its query protocol with the standard library.

### STS

- `AssumeRoleWithWebIdentity` is sent **unsigned**: `POST` `application/x-www-form-urlencoded`, form `Action=AssumeRoleWithWebIdentity`, `Version=2011-06-15`, `RoleArn`, `RoleSessionName`, `WebIdentityToken`, optional `DurationSeconds` (and `Policy`, `PolicyArns`, `ProviderId`, unused here).
- The reply is XML `AssumeRoleWithWebIdentityResponse` → `AssumeRoleWithWebIdentityResult` → `Credentials` {`AccessKeyId`, `SecretAccessKey`, `SessionToken`, `Expiration` (ISO 8601)}, beside `AssumedRoleUser` {`Arn`, `AssumedRoleId`}, `SubjectFromWebIdentityToken`, `Audience` and `Provider`. Errors are XML `ErrorResponse` → `Error` {`Type`, `Code`, `Message`}; a web identity token that fails validation is `InvalidIdentityToken` with HTTP 400.
- The SDK's default endpoint is regional: `https://sts.<region>.amazonaws.com`, `.amazonaws.com.cn` in the `aws-cn` partition.

### SigV4 signer

- `v4.NewSigner().SignHTTP(ctx, aws.Credentials{AccessKeyID, SecretAccessKey, SessionToken}, req, payloadHash, service, region, signingTime)`: `payloadHash` is the hex SHA-256 of the body. It sets `X-Amz-Date` (`20060102T150405Z`), `X-Amz-Security-Token` when a session token is given, and `Authorization: AWS4-HMAC-SHA256 Credential=<id>/<yyyymmdd>/<region>/<service>/aws4_request, SignedHeaders=…, Signature=<64 hex>`.
- It signs every header present except `Authorization`, `User-Agent`, `X-Amzn-Trace-Id`, `Expect` and `Transfer-Encoding`, plus `host` and `content-length` when positive. It strips a default port from the host (`SanitizeHostForHeader`) and sets `req.Host`; a verifier must rebuild the request with the host it received.

## Phase 24f — RFC 8693 token exchange and GCP impersonation (2026-09-27)

Checked against RFC 8693 (rfc-editor.org) and Google's own client, `golang.org/x/oauth2` **v0.36.0** (module source: `google/internal/stsexchange`, `google/internal/impersonate`, `google/externalaccount/basecredentials.go`). No new dependency: the worker speaks both protocols with the standard library. ADR-019 Rev 1.5 builds on these facts.

- **RFC 8693 §2.1** request: `grant_type=urn:ietf:params:oauth:grant-type:token-exchange`; `subject_token` and `subject_token_type` REQUIRED; `resource`, `audience`, `scope`, `requested_token_type`, `actor_token`/`actor_token_type` OPTIONAL. Client authentication is optional (the normal OAuth 2.0 mechanisms). **§2.2.1** response: `access_token`, `issued_token_type` and `token_type` REQUIRED, `expires_in` RECOMMENDED. **§3** token types `urn:ietf:params:oauth:token-type:{access_token,refresh_token,id_token,saml1,saml2,jwt}`.
- **GCP STS** (`stsexchange.ExchangeToken`): `POST https://sts.googleapis.com/v1/token`, `application/x-www-form-urlencoded`, form `audience`, `grant_type`, `requested_token_type=urn:ietf:params:oauth:token-type:access_token`, `subject_token_type`, `subject_token`, `scope` (space-joined) and optional `options` (JSON). The response is JSON `access_token`, `issued_token_type`, `token_type`, `expires_in` (an integer). Google spells one subject type `urn:ietf:params:oauth:token-type:id-token` (a hyphen, unlike RFC 8693's `id_token`); others it lists are `…:jwt`, `…:saml2` and `urn:ietf:params:aws:token-type:aws4_request`.
- **Scopes when impersonating** (`externalaccount`): the STS request asks for `https://www.googleapis.com/auth/cloud-platform`; the caller's scopes go to the impersonation request.
- **Impersonation** (`impersonate.ImpersonateTokenSource`): `POST https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/<email>:generateAccessToken`, `Authorization: Bearer <federated token>`, `Content-Type: application/json`, body `{"delegates"?: [...], "lifetime": "<n>s", "scope": [...]}` (lifetime default `3600s`). The response is `{"accessToken", "expireTime"}`, `expireTime` in RFC 3339.

## Phase 24e — go-spiffe and SPIRE (2026-09-27)

Checked against the module source (`go mod download github.com/spiffe/go-spiffe/v2@v2.8.2`), the SPIRE v1.15.3 source (`doc/spire_server.md`, `pkg/agent/manager/manager.go`, `pkg/common/rotationutil`), the spiffe-csi v0.2.13 example (`example/config`) and a run on minikube (`scripts/k8s-e2e.sh`). ADR-019 Rev 1.4 relies on these.

### Pinned artifacts

- `github.com/spiffe/go-spiffe/v2` **v2.8.2** (adds go-jose v4 and, on Windows, go-winio); `google.golang.org/grpc` becomes a direct dependency.
- Images: `ghcr.io/spiffe/spire-server:1.15.3`, `ghcr.io/spiffe/spire-agent:1.15.3`, `ghcr.io/spiffe/spiffe-csi-driver:0.2.13`, `registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.18.0` (development only).

### go-spiffe API used

- `workloadapi.New(ctx, workloadapi.WithAddr(addr))` dials without blocking, so creating the client contacts nothing; `Close()` releases it.
- `(*Client).FetchJWTSVID(ctx, jwtsvid.Params{Audience, Subject})` sends `JWTSVIDRequest{SpiffeId, Audience}` with the `workload.spiffe.io: true` metadata header and parses the first SVID with `jwtsvid.ParseInsecure`: the signature is **not** verified; it requires a SPIFFE-ID `sub`, an `exp` not passed on the **real** clock, the audience in `aud`, an RS/ES/PS 256–512 algorithm and `typ` absent, `JWT` or `JOSE`. `SVID.Marshal()` returns the token.
- Agent errors are gRPC statuses: `Unavailable` when unreachable, `PermissionDenied` when no registration entry matches the caller.
- Addresses: `unix:///abs/path` or `tcp://ip:port` on Linux; only `tcp://` or `npipe:` on Windows (the tests' fake agent listens on loopback TCP). `workload.SpiffeWorkloadAPIServer` (`proto/spiffe/workload`) is public, so a test can serve the API.

### SPIRE facts used

- Server: `jwt_issuer` sets `iss` (absent by default), `default_jwt_svid_ttl` defaults to 5m, `jwt_key_type` defaults to the CA key type (`ec-p256`, so ES256). `entry create -jwtSVIDTTL <seconds>` sets one entry's TTL.
- The agent caches a JWT-SVID per identity and audience and hands the cached one out until it is at about half its lifetime (±10 % jitter); if its server is unreachable it may return an older **unexpired** SVID. A returned SVID can have much less than its TTL left.
- `bundle show -format spiffe` prints a JWKS whose keys carry `use` `x509-svid` or `jwt-svid`; the JWT authorities have a `kid`.
- `entry show -spiffeID <id>` prints `Found 0 entries` when none matches (the e2e script creates entries idempotently with it).
- The SPIFFE CSI driver places the agent's socket in an inline `csi` volume (`csi.spiffe.io`, `Ephemeral` lifecycle), which the `restricted` Pod Security Standard allows.

## Phase 18 — OTLP/HTTP JSON and the OpenTelemetry GenAI conventions (2026-09-25)

Checked against the OTLP specification (opentelemetry.io/docs/specs/otlp, **1.11.0**) and the GenAI semantic-conventions repository (`open-telemetry/semantic-conventions-genai`, commit `8ffdf568e1b4391a99adb081db16e8102e36918e`, 2026-09-22, status **Development**). `internal/finops/otlp_test.go` and `internal/api/finops_test.go` pin what EACP relies on.

### OTLP/HTTP facts used

- The default trace path is `/v1/traces`. EACP serves it under the agent API as `POST /v1/agent/otlp/v1/traces`, so an exporter's endpoint is `<api>/v1/agent/otlp`.
- JSON requests use `Content-Type: application/json`. Keys are lowerCamelCase, `traceId`/`spanId` are **case-insensitive** hex strings, enums are integers, and 64-bit integers are decimal strings. EACP also accepts a JSON integer, as the protobuf JSON mapping does. Receivers **must** ignore unknown fields.
- Servers **must** support `gzip` (`Content-Encoding: gzip`) as well as no compression. EACP bounds the body at 4 MiB both before and after decompression.
- Full success is `200` with `partialSuccess` unset. Partial success is `200` with `partialSuccess.rejectedSpans` (an int64 string) and `errorMessage`. Undecodable data is `400`. Throttling is `429` or `503`. JSON responses use `Content-Type: application/json`.
- Accepting binary Protobuf and JSON on the same port is a **SHOULD**. EACP accepts JSON only and answers `415` for anything else (ADR-025 §1), a documented deviation.

### GenAI attributes used

- `gen_ai.operation.name` is required. Well-known values include `chat`, `generate_content`, `text_completion`, `embeddings`, `invoke_agent`, `invoke_workflow`, `plan`, `execute_tool` and `fetch_response`. `fetch_response` SHOULD NOT report usage.
- Also used: `gen_ai.provider.name`, `gen_ai.request.model` and `gen_ai.response.model`. `gen_ai.system` is not in this registry.
- `gen_ai.usage.input_tokens` "SHOULD include all types of input tokens, including cached tokens". `gen_ai.usage.output_tokens` and `gen_ai.usage.cache_read.input_tokens` are the other usage attributes. All are integers.
- Agent-level spans (`invoke_agent`, …) may carry usage aggregated over their child calls. EACP stores them as not billable, so usage is never counted twice (ADR-025 §2).

## Phase 14 — MCP specification and the official Go SDK (2026-09-24)

Checked against the specification repository (`modelcontextprotocol/modelcontextprotocol`, `main`: `schema/2026-07-28/schema.ts` and `docs/specification/2026-07-28`, plus `2025-11-25` for the legacy transport), against the SDK module source (`go mod download`), and by running the SDK's server in `internal/connector/mcp/interop_test.go`.

### Pinned artifacts

- Specification revision **2026-07-28** (modern). Legacy revisions accepted: **2025-11-25**, **2025-06-18**, **2025-03-26**.
- `github.com/modelcontextprotocol/go-sdk` **v1.8.0**, package `mcp`. Test code imports it (interop only); no EACP binary links it. It declares the same four revisions (`shared.go`: `protocolVersion20260728` … `protocolVersion20250326`).

### Protocol facts used

- **Modern requests are stateless.** Each request carries `params._meta` with `io.modelcontextprotocol/protocolVersion`, `io.modelcontextprotocol/clientCapabilities` and optionally `io.modelcontextprotocol/clientInfo`. `server/discover` returns the supported versions, capabilities and server info.
- **Headers (Streamable HTTP).** `MCP-Protocol-Version` on every request, and `Mcp-Method` mirroring the JSON-RPC method on all requests. A mismatch is `400` with `HeaderMismatch`.
- **Error codes.** `-32020` HeaderMismatch, `-32021` MissingRequiredClientCapability, `-32022` UnsupportedProtocolVersion. `-32020` to `-32099` are reserved for MCP.
- **Legacy detection.** A server that predates 2026-07-28 answers a modern request with `400`, or `404`/`405` from an HTTP+SSE server. The client then uses `initialize`, keeps `Mcp-Session-Id`, sends `notifications/initialized` and ends the session with `DELETE`. 2025-06-18 did not define the `MCP-Protocol-Version` header, so servers of that revision may ignore it.
- **Results.** `resultType` other than `complete` means the server needs more input. An absent `resultType` is `complete`.
- **Tool definitions.** `inputSchema` is an object with `type: "object"`, and any JSON Schema 2020-12 keyword may appear. `title`, `icons` and `annotations.title` are display metadata. Annotation defaults: `readOnlyHint` false, `destructiveHint` true, `idempotentHint` false, `openWorldHint` true. The spec calls them hints, not guarantees.
- **`x-mcp-header`.** A property may mirror an argument into an `Mcp-Param-{name}` header. The value must be a non-empty HTTP token (no control characters), case-insensitively unique within the `inputSchema`, and may only annotate an `integer`, `string` or `boolean` property (not `number`) reached from the root through `properties` keys alone (no `items`, composition, conditional keywords or `$ref`). A client on Streamable HTTP **must** exclude a tool that violates this from `tools/list`; EACP records it as rejected (`invalid_x_mcp_header`).

### SDK API used (interop tests)

- `mcp.NewServer(&mcp.Implementation{...}, &mcp.ServerOptions{SupportedProtocolVersions: ...})` and `server.AddTool(&mcp.Tool{...}, handler)`.
- `mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server, &mcp.StreamableHTTPOptions{Stateless, JSONResponse})`. Stateless mode answers GET and DELETE with `405`.
- **Observed:** the stateful handler does not serve 2026-07-28 (`server.go`: it does not keep modern session state on a transport that cannot surface it), so EACP falls back to `2025-11-25` there. The interop test pins this.

## Phase 10 — NATS JetStream Go client and server (2026-09-24)

Checked against the module sources (`go mod download`) and by running the
embedded server in `internal/messaging` tests and the `nats:2.15.0-alpine`
image in compose.

### Pinned artifacts

- `github.com/nats-io/nats.go` **v1.54.0** (h1:vsXoOxjHp/GmPUN+EcI7uOf/uB+iAP+kEsAFNQN0yzA=), package `jetstream`.
- `github.com/nats-io/nats-server/v2` **v2.15.0** (h1:M99yf0y05rTr46/qc/Is6ZAowI58Ryp2SjufLCUeVJc=). It is imported by test code only (`internal/messaging/natstest`), so no EACP binary links it.
- Compose image `nats:2.15.0-alpine` (`nats-server: v2.15.0`, entrypoint `docker-entrypoint.sh`, runs as root).

### API used

- **Client construction.** `jetstream.New(*nats.Conn)`, then `JetStream.PublishMsg(ctx, *nats.Msg, ...PublishOpt) (*PubAck, error)`.
  - The publish is synchronous and waits for the stream's ack.
  - `PubAck.Duplicate` is true when the stream's duplicate window dropped the message.
- **Publish options.**
  - `jetstream.WithMsgID(id)` sets the `Nats-Msg-Id` header (`jetstream.MsgIDHeader`).
  - `WithRetryAttempts(0)` disables the client's retry on "no responders". The default is 2 retries, 250 ms apart.
  - `nats.NewMsg(subject)` initializes `Header`. `nats.Header.Get`/`Set` are **case-sensitive**, so the relay and the consumer both use `traceparent`.
- **Streams and consumers.** `CreateOrUpdateStream` / `CreateOrUpdateConsumer` are idempotent.
  - `StreamConfig.Duplicates` is the dedup window; the server default is 2 minutes.
  - Retention: `WorkQueuePolicy` removes a message once it is ACKed, and `LimitsPolicy` keeps it by age and size. `Discard: DiscardOld` drops the oldest at a limit.
  - Consumer: `ConsumerConfig{Durable, FilterSubject, AckPolicy: AckExplicitPolicy, AckWait, MaxDeliver}`.
- **Consuming.** `JetStream.Consumer(ctx, stream, name)` binds to an existing consumer and returns `ErrConsumerNotFound` otherwise.
  - `Consumer.Fetch(n, FetchContext(ctx))` makes one pull request. A context deadline sets the request's expiry.
  - `MessageBatch.Messages()` is always closed, and `Error()` reports the failure.
  - `Msg.Ack`, `NakWithDelay`, `TermWithReason` (servers >= 2.10.4) all publish to the message's reply subject, `$JS.ACK.<stream>.<consumer>...`.
- **Dashboards.** `JetStream.OrderedConsumer(ctx, stream, OrderedConsumerConfig{FilterSubjects})`, and `FetchNoWait(n)` for a read that doesn't wait.
- **Connection options.**
  - `nats.RetryOnFailedConnect(true)` makes `Connect` succeed while the server is down.
  - `MaxReconnects(-1)` retries forever.
  - `ReconnectBufSize(-1)` disables the reconnect buffer, so publishes fail at once while disconnected rather than being flushed later.
- **Embedded test server.** `server.NewServer(&server.Options{Host, Port: -1, JetStream: true, StoreDir, NoLog, NoSigs})`, then `Start()` and `ReadyForConnections(d)`.
  - `Addr()` gives the chosen port.
  - `Shutdown()` + `WaitForShutdown()`.
- **Permissions the worker needs to bind, pull and ACK** (checked in compose):
  - `$JS.API.CONSUMER.INFO.<stream>.<consumer>`
  - `$JS.API.CONSUMER.MSG.NEXT.<stream>.<consumer>`
  - `$JS.ACK.<stream>.<consumer>.>`
  - subscribe on `_INBOX.>`

  A publish outside its permissions gets `-ERR 'Permissions Violation for Publish to "<subject>"'`.

## Phase 16 kill semantics checked against upstream (2026-09-24)

Microsoft's [Go kill-switch source at commit `98a7773`](https://github.com/microsoft/agent-governance-toolkit/blob/98a777328af9ad4dc76bc76803071cf5a615811b/agent-governance-golang/packages/agentmesh/kill_switch.go) defines scoped `Activate` and `Clear`, an event history, scopes `global`, `agent`, `capability`, and reason codes `policy_violation`, `security_incident`, `operator_request`, `error_budget_exhausted`. EACP uses those reason-code strings and maps agent/capability to registry agent/tool IDs. It does not import this Go module or use its process-local registry as execution authority; the pinned `agt-policies` sidecar remains the governance PDP (ADR-002, ADR-016). This source check does not change any dependency pin.

## Phase 9 spike — AGT / ACS Python API (2026-09-24)

### Pinned artifacts

| Artifact | Version | Source | SHA-256 |
|---|---|---|---|
| `agt-policies` (AGT 5.0 policy layer, package `agt`) | 5.0.0 | PyPI wheel | `02afdc295ab7f0aff4f40346f3da31101ee4c635a4df33aee45d03035d784bfc` |
| `agent-control-specification` (ACS Python SDK + Rust core) | 0.3.1b1 | PyPI wheel `cp311-abi3-manylinux_2_28_x86_64` (a pre-release: `pip index --pre`) | `a0eea57016fc4cf3c620d92b81ad8c7b1376aa2865991a561ea6a427cb1451be` |
| OPA CLI (the ACS Rego dispatcher shells out to it) | v1.20.2 | GitHub release `opa_linux_amd64_static` | `69da5179ee403d10fa11bab6cfb4ffb0d23dba5f9b682fa977db772a1da5670f` |
| `rfc8785` (JCS for the sidecar's own digests) | 0.1.4 | PyPI wheel | pinned by hash in `sidecars/agt-pdp/requirements.txt` |

There's no Windows wheel, so `pip` on a Windows host falls back to the sdist
(`560a717b…`). That sdist's `Cargo.lock` is stale: `cargo --locked` refuses it.
The sidecar therefore installs the upstream Linux wheel by hash. Its `glibc`
floor is 2.28; Debian bookworm has 2.36.

The meta package `agent-governance-toolkit` is still 4.1.0 and does **not** contain
ACS. In 4.1.0 the v4 adapters reach ACS only through
`agent_os.integrations._v5_runtime_bridge`, which imports `agt.policies.runtime`
from `agt-policies` 5.0.0. `agent-governance-toolkit-core` 5.0.0 also exists.
EACP depends only on `agt-policies` 5.0.0 and ACS 0.3.1b1.

### API verified by running it (image `sidecars/agt-pdp`, Python 3.12)

- `agt.policies.runtime.AgtRuntime(manifest_path)` loads an ACS manifest.
  `AgtRuntime.control` is the ACS `agent_control_specification.AgentControl`.
- `await AgentControl.evaluate_intervention_point(ip, snapshot, mode)` returns
  `InterventionPointResult(verdict, transformed_policy_target,
  transformed_policy_target_applied, policy_input, input_identity,
  enforced_identity)`. It does **not** consult an approval resolver; only
  `AgentControl.enforce()` / `run()` and `AgtRuntime.evaluate_intervention_point`
  in `enforce` mode do. With no resolver those turn `escalate` into a block
  (`deny`), so the sidecar must never call them.
- `Verdict(decision, reason, message, transform, evidence, result_labels)`.
  Decisions: `allow | deny | warn | escalate | transform`.
- A `transform` is a **single-path** replacement `{path, value}` rooted at
  `$policy_target`. The path `$policy_target` itself is accepted, and it replaces
  the whole target. In `enforce` mode the core applies it
  (`transformed_policy_target`). In `evaluate_only` mode the core validates it
  but does not apply it.
- A transform is forbidden on every other decision. An `escalate` verdict never
  carries or applies a transformed target.
- Verdict evidence is `{artefact?: str, verification_pointers?: {str: str}}`,
  capped at 4 KiB serialized. It is propagated verbatim.
- Every runtime failure (OPA missing or timing out, invalid policy output, limits)
  becomes `deny` with a reserved `runtime_error:*` reason. A policy may not emit
  that prefix itself.
- Identity: `action_identity = "sha256:" + hex(SHA-256(serde_json with keys sorted
  recursively))` over the **whole ACS policy input**
  (`intervention_point`, `policy_target`, `snapshot`, `annotations`, `tool`).
  This is **not RFC 8785**: numbers and key order follow serde_json, and keys are
  sorted by UTF-8 bytes, not by UTF-16 code units. It also covers a different
  document from EACP's binding.
  - ACS 0.3.1b1's pyo3 binding emits only the legacy `action_identity`, which
    is the enforced identity. The Python SDK copies it into both
    `input_identity` and `enforced_identity`, so the two are always equal in
    Python.
- Rego policies run through the bundled OPA dispatcher:
  `opa eval --format json --stdin-input --bundle <dir> <query>`.
  - The executable comes from `ACS_OPA_PATH` (default `opa` on `PATH`).
  - The timeout comes from `ACS_OPA_TIMEOUT_MS` (default 5 s).
  - The manifest declares it as `policies.<id>: {type: rego, bundle: <dir>,
    query: data.<pkg>.verdict}`. A bundle directory needs no `.manifest`.
- Snapshot: `pre_tool_call` with `policy_target: "$snap.tool_call.args"` works
  without `tool_name_from` or a `tools:` section.

### Consequences recorded in ADR-002 Rev 2.4

1. EACP still computes its own JCS digests. The sidecar computes the same
   digests with `rfc8785`, and EACP compares them (ADR-002 §4). The ACS
   identity is kept as provider evidence only.
2. EACP's multi-key `set` becomes one ACS transform of `$policy_target`: the
   original object, minus the replaced keys, united with `set`. The replacement
   is shallow; `object.union` alone would merge nested objects.
3. `escalate` with `set` can't be expressed in ACS. The AGT provider refuses such
   a bundle (`policy_unsupported`), and the action fails closed.
4. `runtime_error:*` is an outage, not a policy decision. The sidecar reports
   it as a transient failure (503), so the action stays `RECEIVED`
   (ADR-002 §6). It is never recorded as a terminal `DENIED`.
5. `escalate` carries no quorum or TTL. The Rego adapter puts the matched rule id
   in `evidence.verification_pointers.eacp_rule_id`. The sidecar then takes the
   approval requirement from that rule, after checking that the rule's
   verdict equals the ACS decision.
