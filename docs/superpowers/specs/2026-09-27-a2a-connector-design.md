# Phase 25a — Governed A2A delegation (design)

Date: 2026-09-27 · Status: approved by the owner in chat (25a = A2A first, the LLM Gateway is 25b; outbound A2A
as a connector; approach A: EACP's own A2A client with Agent Card discovery; design section 1 approved: one
`delegate` tool per remote agent certified over the whole card; "yes, continue and dev until finish phase").
Scope: MASTER_PLAN §97 (A2A: "support governed remote Agent execution … authority stays governed, execution
remains observable"). ADR: **ADR-030 A2A delegation** (new). Builds on ADR-001, ADR-003, ADR-004, ADR-019,
ADR-022, ADR-023 and ADR-027.

## 1. Intent

A governed agent delegates a task to a remote agent that speaks A2A. The delegation is an EACP action like any
other: the agent's allowlist, policy, approval, budget, kill switch, circuit and lease apply before anything is
sent; only the execution worker holds the credential for the remote agent and reaches it; the remote task's id,
state and outcome are recorded; and what the remote agent says it can do (its Agent Card) is discovered,
fingerprinted and certified by two people, and a change quarantines the delegation.

Success:

- a registry editor registers an `a2a` connector; the worker's scanner discovers its `delegate` tool from the
  Agent Card; two people certify a contract for it; an allowlisted agent's delegation runs once and ends
  `SUCCEEDED` with the remote task id as its external reference;
- a remote task that does not complete (fails, asks for input, outlives the call) ends `UNKNOWN_OUTCOME` with
  the remote task id recorded for the human who resolves it, and nothing is re-sent automatically;
- a changed Agent Card (a new, changed or removed skill, a new description or auth requirement) quarantines a
  certified `delegate`; a card that points the worker at another host is refused;
- the remote agent's credential never appears in responses, logs, the database or the scan record;
- the client interoperates with the reference A2A server (a2a-go v2.6.0); every existing connector behaves as
  before; the compose demo shows it end to end.

## 2. Verified upstream protocol (A2A 1.0)

Checked against `github.com/a2aproject/a2a-go/v2` **v2.6.0** (2026-09-25; `a2a/core.go`, `a2a/agent.go`,
`a2a/svcparams.go`, `a2aclient/jsonrpc.go`, `a2aclient/agentcard/resolver.go`, `internal/jsonrpc/jsonrpc.go`),
which implements protocol version `1.0` (`a2a.Version`) with a v0.3 compatibility layer.

- **Agent Card** at `GET <origin>/.well-known/agent-card.json`. Members: `name`, `description`, `version`,
  `supportedInterfaces` (`[{url, protocolBinding, protocolVersion, tenant?}]`), `capabilities`
  (`streaming`, `pushNotifications`, `extendedAgentCard?`, `extensions?`), `defaultInputModes`,
  `defaultOutputModes`, `skills` (`[{id, name, description, tags, examples?, inputModes?, outputModes?,
  securityRequirements?}]`), `provider?` (`organization`, `url`), `securitySchemes?`, `securityRequirements?`,
  `signatures?`, `iconUrl?`, `documentationUrl?`.
- **JSON-RPC 2.0** over HTTP POST, string `id`, methods `SendMessage`, `GetTask`, `CancelTask` (and others
  unused here: `SendStreamingMessage`, `ListTasks`, `SubscribeToTask`, push-notification config,
  `GetExtendedAgentCard`). The protocol version travels as the `A2A-Version` service parameter (an HTTP
  header on the JSON-RPC binding). A non-200 HTTP status is an error.
- `SendMessage` params: `{message, configuration?, metadata?}`; `message`: `{messageId, role: "ROLE_USER",
  parts, contextId?, taskId?, metadata?}`; a part is flattened: `{"text": …}`, `{"data": …}` (with optional
  `mediaType`), `{"url": …}` or `{"raw": …}`; `configuration.returnImmediately` false means the server answers
  when the task reaches a terminal or interrupted state (it may still answer earlier). The result is exactly one
  of `{"task": Task}` or `{"message": Message}`.
- `Task`: `{id, contextId, status: {state, message?, timestamp?}, artifacts?, history?, metadata?}`; states
  `TASK_STATE_SUBMITTED`, `_WORKING`, `_COMPLETED`, `_FAILED`, `_CANCELED`, `_REJECTED`, `_INPUT_REQUIRED`,
  `_AUTH_REQUIRED`. `GetTask` params `{id, historyLength?}` returns a Task; `CancelTask` params `{id}`.
- **No idempotency key.** The reference server leaves deduplication by `messageId` as a TODO "once spec
  establishes the key", and `ListTasks` filters by `contextId` and state only. A resent message may start a
  second task, and a task whose reply was lost cannot be found.

## 3. Design

### 3.1 Registry: an `a2a` connector and its `delegate` tool

- `eacp.connectors.protocol` accepts `a2a`. The endpoint is the remote agent's **JSON-RPC 1.0 interface URL**
  (absolute; `https`, `http` only in development and test; no user info, query or fragment); its `secret_ref`
  names a credential only the worker holds, bound to the endpoint's host (ADR-019 providers all apply,
  including SigV4). Like an `mcp` connector it gets one scan row (the existing `eacp.mcp_servers`, which now
  holds every discovered connector).
- **A2A has no skill selection**: a message goes to the agent, which decides what to do. The governed
  capability is therefore delegation to the agent, and each `a2a` connector has exactly **one discovered tool,
  `delegate`** (`remote_name` `delegate`). An agent's allowlist grants `<connector>.delegate`. It is
  discovered, never declared (ADR-023 §1): a registry editor cannot insert it, and the scanner inserts nothing
  else.
- The tool's **canonical definition** is `{"name": "delegate", "inputSchema": <EACP's payload schema, §3.3>,
  "agentCard": <the card minus display fields>}` in RFC 8785 form; display metadata is `iconUrl` and
  `documentationUrl`. Everything else in the card is security-relevant: skills (a model or a reviewer reads
  their descriptions and examples), the description, provider, version, capabilities, interfaces, security
  schemes and requirements, default modes, extensions, signatures and any unknown member. PostgreSQL hashes and
  classifies exactly as for MCP (ADR-023 §3–§4): `initial`, `low` (display only) or `high`, which invalidates
  the contract and quarantines a certified `delegate`.
- A definition has no behaviour annotations, so ADR-023 §5's defaults apply (not read-only, destructive, not
  idempotent, open world): a `delegate` contract can never be `READ_ONLY`.

### 3.2 Discovery (the worker's scanner)

- The scanner serves both discovered protocols with one lease and one record function (ADR-023 §2 unchanged):
  the claim offers `mcp` and `a2a` scan rows, and a per-protocol `Discoverer` does the listing.
- The A2A discoverer fetches `<scheme>://<host[:port]>/.well-known/agent-card.json` of the connector endpoint
  with the worker-held credential (`Secret.Authorize` with an empty body: a Bearer or SigV4), `Accept:
  application/json`, no redirects, no proxy, at most 256 KiB, the scan timeout. HTTP 200 is required
  (`http_<code>`; 401/403 is `unauthorized`); a transport error is `transport`.
- The card must be an I-JSON object (no duplicate members, no U+0000; else `card_invalid`) with a non-empty
  `name` and `skills` an array of at most 200 objects each with a non-empty string `id` unique in the card
  (else `card_invalid`). Among `supportedInterfaces` there must be one with `protocolBinding` `JSONRPC` and
  `protocolVersion` `1.0` whose `url` equals the connector endpoint exactly (after the same URL normalisation
  the registry applies); none with that binding and version is `no_supported_interface`, one on another URL is
  `interface_mismatch`. So the worker only ever sends a task to the endpoint the registry approved.
- A successful scan records protocol version `1.0`, server info `{name, version}` from the card (display only)
  and the one `delegate` definition. A failed scan changes nothing (ADR-023 §2). Unlike MCP, a card is fetched,
  not paginated, and there are no rejected tools: a bad card fails the scan.

### 3.3 Contracts

A `delegate` contract (ADR-003 §4, two-person activation, pinned `definition_id` as ADR-023 §6) must declare:

- `idempotency_mode = none`, so `max_attempts = 1` (the existing rule for a non-read-only, non-idempotent
  contract): **a delegation is sent at most once.**
- `reconciliation_lookup = none` (so consistency and proof `none`), exactly as MCP (ADR-023 §5): A2A offers no
  lookup that could prove a lost task absent. An unknown outcome goes to a human after it settles (ADR-004).
- `no_effect_errors` may certify only classes the connector can report before or instead of starting work:
  `a2a_rejected`, `connection_refused_before_send`, `unauthorized` and `a2a_rpc_<code>` (a JSON-RPC error
  reply). Anything else is refused by the guard.

The payload schema (in the definition, and checked by the connector before sending): an object with
`text` (a string of 1–65 536 characters) and/or `data` (a JSON object), nothing else, at least one of them.

### 3.4 Execution (the `a2a` connector in the worker)

The worker serves protocol `a2a` with `internal/connector/a2a`. After the dispatch intent (T16), with the
lease, kill and circuit rules of every connector:

1. **Validate** the enforced payload against §3.3 (else `NoEffect` `invalid_payload`: nothing was sent).
2. **Send once.** `POST` the endpoint, `Content-Type: application/json`, `Accept: application/json`,
   `A2A-Version: 1.0`, the W3C trace headers, and the credential through `Secret.Authorize` (last, so SigV4
   signs every header). Body: `{"jsonrpc": "2.0", "id": "1", "method": "SendMessage", "params": {"message":
   {"messageId": <action id>, "role": "ROLE_USER", "parts": [<text part>, <data part with mediaType
   application/json>], "metadata": {"eacp": {"operation_key": <key>}}}, "configuration": {"returnImmediately":
   false}}}`. No redirects, no proxy, at most 1 MiB per response.
3. **Follow the task** while it is `SUBMITTED` or `WORKING` and the call's context is alive: `GetTask` every
   second, doubling to at most 5 s, each reply checked like the first. The call's budget is the contract's
   `timeout_ms` (ADR-004), inside the lease, as for every connector.
4. **Classify** (every class matches the attempt's error-class pattern):

| What happened | Outcome | Class / reference |
|---|---|---|
| `{"message": …}` reply | `Succeeded` | reference `message:<messageId>` |
| task `COMPLETED` | `Succeeded` | reference = task id |
| task `REJECTED` | `NoEffect` | `a2a_rejected` (Ambiguous unless certified) |
| task `FAILED` / `CANCELED` | `Ambiguous` | `a2a_failed` / `a2a_canceled` |
| task `INPUT_REQUIRED` / `AUTH_REQUIRED` | `Ambiguous` | `a2a_input_required` / `a2a_auth_required` |
| still `SUBMITTED`/`WORKING` when the context ends | `Ambiguous` | `a2a_interrupted` |
| JSON-RPC error reply to `SendMessage` | `NoEffect` | `a2a_rpc_<abs(code)>` (Ambiguous unless certified) |
| HTTP 401/403 to `SendMessage` | `NoEffect` | `unauthorized` (the worker drops the credential) |
| connection refused before sending | `NoEffect` | `connection_refused_before_send` |
| anything else (other status, transport error after send, malformed or oversized reply, an id or reference that breaks the limits) | `Ambiguous` | `transport_error`, `invalid_response` … |

   A task id or message id must be 1–256 characters of `[A-Za-z0-9._:-]` (else `invalid_response`), so it is
   safe to store and log.
5. **Contain.** When the context ends while a known task is `SUBMITTED`, `WORKING`, `INPUT_REQUIRED` or
   `AUTH_REQUIRED` (deadline, kill, cancellation or lost lease), the connector sends one `CancelTask` for it with
   its own 2 s budget, best effort, and logs whether it was accepted. The outcome stays `Ambiguous`: a cancel
   request is not proof that nothing happened. EACP never answers an input or auth request on the agent's
   behalf.

`Lookup` always reports `unknown` (the contract never asks for one).

**The remote reference.** An ambiguous or no-effect attempt may not carry an external reference (00006). The
attempt gains `remote_reference` (text, 1–512 characters, NULL unless the outcome is `ambiguous` or
`no_effect`), set to the remote task id when one is known. It appears in the action's evidence, so the human who
resolves an `a2a` unknown outcome can look the task up at the remote agent. It is never used to decide
anything.

### 3.5 Observability

- The log line for each delegation names the connector host, the action id, the task id, the final
  state, the number of `GetTask` polls, the number of artifacts and the SHA-256 of their canonical JSON; never a
  part's content, the payload or the credential.
- Task ids, states and classes are not secrets. Artifact and message content from the remote agent is
  untrusted and is never stored, journaled or logged in 25a (the agent sees the external reference, not the
  output; returning outputs is out of scope).
- The existing scan, definition, quarantine and incident journal entries cover discovery; a quarantined
  `delegate` opens the same incident an MCP quarantine does (ADR-027's signal generalised to discovered
  connectors).

### 3.6 Governance-as-Code and the console

- A bundle may declare an `a2a` connector; like `mcp`, it never declares its tools (ADR-026), and prune never
  touches a discovered tool.
- The console lists `a2a` connectors and their `delegate` tool with the existing connector, scan and quarantine
  views; no new route.

### 3.7 Fake A2A agent, compose and demo

- `cmd/fakea2a` + `internal/fakea2a`: an A2A 1.0 JSON-RPC server for tests and the demo. It verifies a bearer
  token (only its SHA-256 is configured), serves its card from a file (so a demo can replace it), and keeps a
  durable log of every message and task (message id, task id, state, SHA-256 of the message), which an audit
  endpoint returns with the token. A message's `data.scenario` picks the behaviour: none → `COMPLETED` at once
  with an artifact; `working` → `WORKING` until `delay_ms`, then `COMPLETED`; `input_required`; `fail`;
  `reject`; `hang` (never finishes; `CancelTask` cancels it).
- Compose: `fakea2a` on the worker-only `erp` network; its token in the worker's connector-secret manifest
  (tenant Slice A demo's sibling `…00ab`); `test/security` proves the agent cannot reach it.
- `TestA2ADemo` (`DEMO=D scripts/demo.sh`, compose): register the connector; the scanner discovers `delegate`;
  two people certify it (`no_effect_errors: [a2a_rejected]`); an allowlisted agent delegates: (1) a task that
  completes → `SUCCEEDED`, reference = the fake agent's task id, exactly one task in its log with `messageId` =
  the action id; (2) `input_required` → `UNKNOWN_OUTCOME` with `remote_reference` in the evidence and a
  `CancelTask` in the fake agent's log; (3) `reject` → `FAILED` (certified no effect); (4) the card gains a skill
  → a rescan quarantines `delegate` and the next delegation is `DENIED` (`tool_quarantined`); (5) no token in
  responses, logs or a database dump.

## 4. Proof (tests first)

- `internal/connector/a2a`: card validation (every refusal and the host binding), the canonical definition and
  display split, the request shape (headers, body, `messageId` = action id, SigV4 signs), each classification
  row of §3.4, polling with back-off, `CancelTask` on interruption, payload validation, response limits, no
  redirects; **interoperability with the a2a-go v2.6.0 reference server** (card resolution, `SendMessage`,
  `GetTask`, `CancelTask`; a test-only dependency like the MCP SDK).
- PostgreSQL: the `a2a` protocol and its scan row; the `delegate` tool discovered, never declared; the contract
  guard (idempotency, lookup, certified classes, pinned definition); a high-risk card change quarantines;
  `remote_reference` rules (raw SQL as `eacp_app`); the incident signal; the RLS catalog.
- `internal/worker`: the scanner dispatches by protocol; an integration test runs a delegation through the real
  worker, connector and fake agent (success, interrupted with cancel, rejected) with nothing secret persisted.
- `internal/bundle`: `a2a` connectors accepted, their tools never declared.
- `test/security`, `test/demo` (`TestA2ADemo`), `test/invariants` (invariant 11 gains the A2A integration test).

## 5. Out of scope

Inbound A2A (an A2A ingress into EACP); streaming (`SendStreamingMessage`, `SubscribeToTask`); push
notifications; the REST and gRPC bindings; the v0.3 wire format; `GetExtendedAgentCard`; verifying card JWS
signatures (they are part of the certified definition, not trusted); multi-turn conversations (`contextId`
reuse, answering `INPUT_REQUIRED`); returning remote outputs to the agent; reconciling by task id; per-skill
governance; Kubernetes demo coverage; the LLM Gateway (25b).

## 6. Unresolved assumptions (conservative choices, recorded in ADR-030)

- A delegation is sent at most once: A2A has no idempotency key, so no retry after a send, ever.
- An unfinished, failed or interrupted remote task is an unknown outcome for a human, never a failure EACP
  assumes had no effect; only `REJECTED` and pre-work errors can be certified as no effect.
- The certified unit is the whole Agent Card: any change but the icon or documentation URL is high risk.
- The worker only talks to the certified interface URL, which must equal the connector endpoint.
- Interruption sends one best-effort `CancelTask`; it never changes the outcome.
- Remote content is untrusted and not persisted; only ids, states, counts and digests are.
