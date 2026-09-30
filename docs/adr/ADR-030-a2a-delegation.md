# ADR-030: Governed A2A delegation

Status: Accepted (Rev 1.0, 2026-09-27). Scope: Phase 25a (MASTER_PLAN §97).
Related: ADR-001 (only workers reach connectors and hold their secrets), ADR-003 §4 (connectors, tools, contracts), ADR-004 (unknown outcomes, the dispatch intent), ADR-019 (credential providers, SigV4), ADR-023 (discovered tools, fingerprints, quarantine), ADR-027 (incidents).

## Context

Agents increasingly hand work to other agents. The Agent2Agent protocol (A2A 1.0, verified against the reference implementation `github.com/a2aproject/a2a-go/v2` v2.6.0; `research/REFERENCES.md`) lets a client send a message to a remote agent, which may answer directly or start a **task** that the client follows. Delegating to a remote agent is an external effect like any connector call: it needs a certified capability, a credential only the worker holds, an at-most-once send, and a human for an outcome nobody can prove.

A2A differs from EACP's HTTP connector protocol in three ways that shape this decision:

- **No skill selection.** A message goes to the agent; the agent decides which of its skills to use. EACP cannot govern per skill.
- **No idempotency key and no lookup.** The reference server leaves deduplication by `messageId` as a TODO, and `ListTasks` filters only by context, state and status time. A resent message may start a second task, and a task whose reply was lost cannot be found.
- **The card is self-description.** The Agent Card lists the agent's skills, examples and security requirements; a model or a reviewer reads them. Like an MCP tool definition (ADR-023 §3), it can change under a certified integration.

## Decision

### 1. An A2A agent is a connector with one discovered tool, `delegate`

- `eacp.connectors.protocol` accepts `a2a`. The endpoint is the remote agent's **JSON-RPC 1.0 interface URL**; its `secret_ref` names a credential only the worker holds, bound to the endpoint's host. Every ADR-019 provider applies, SigV4 included.
- An `a2a` connector gets a scan row in `eacp.mcp_servers`, which now holds every discovered connector, and is scanned by the same lease, fence and record function as an MCP server (ADR-023 §2).
- It has exactly **one discovered tool, `delegate`**. A registry editor cannot declare it; the scanner inserts no other tool and records exactly one per successful scan, with no rejected tools. An agent's allowlist grants `<connector>.delegate`.
- The tool's canonical definition is `{"name":"delegate","inputSchema":<payload schema>,"agentCard":<the card minus iconUrl and documentationUrl>}` in RFC 8785 form; its display metadata is those two URLs. Everything else in the card is certified, unknown members included. PostgreSQL hashes and classifies exactly as for MCP: `initial`, `low` (display only) or `high`. A high-risk change quarantines a certified `delegate` (ADR-023 §7), and ADR-027's scanner-quarantine signal opens the incident. Its kind and title still read `mcp_drift` and "MCP tool …".
- The tool fingerprint is ADR-023's `v2` hash. A delegate definition carries no behaviour annotations, and a delegation is never certified `READ_ONLY`.

### 2. Discovery

- The worker's scanner picks a discoverer by the connector's protocol. The A2A discoverer fetches `<scheme>://<host>/.well-known/agent-card.json` of the endpoint's origin with the worker-held credential (`Secret.Authorize` with an empty body), `Accept: application/json`, no redirects, no proxy, at most 256 KiB, within the scan timeout.
- It requires HTTP 200 (`http_<code>`; 401 or 403 is `unauthorized`); a transport failure is `transport`. The card must be an I-JSON object without U+0000, with a non-empty `name` and `skills` an array of at most 200 objects, each with a non-empty `id` unique in the card (else `card_invalid`). The canonical definition must fit in 64 KiB (`too_large`).
- Among `supportedInterfaces` there must be one with `protocolBinding` `JSONRPC` and `protocolVersion` `1.0` whose `url` **equals the connector endpoint byte for byte**. If no interface has that binding and version, the scan fails with `no_supported_interface`, and a v0.3-only card fails the same way. If such an interface names another URL, it fails with `interface_mismatch`. The worker only ever sends a task to the URL the registry approved.
- A successful scan records protocol version `1.0` (the scan table accepts `1.0` for `a2a` connectors and a dated revision for `mcp` ones), server info `{name, version}` from the card for display, and the one definition.
- MCP servers still refuse a signing credential (`unsupported_credential`). An A2A agent behind AWS IAM is scanned and called with SigV4.

### 3. Contracts

A `delegate` contract pins the tool's current `definition_id` (ADR-023 §6). The guard also requires the following:

- `idempotency_mode = none`, so the existing rule makes `max_attempts = 1`: **a delegation is sent at most once.**
- `reconciliation_lookup = none`: no lookup can prove a lost task absent, so an unknown outcome goes to a human once it settles (ADR-004).
- `no_effect_errors` only from the classes the connector reports before or instead of any work: `a2a_rejected`, `connection_refused_before_send`, `unauthorized`, `invalid_payload`, and `a2a_rpc_<code>` for the JSON-RPC errors the protocol layer returns before any task exists: 32700, 32600, 32601, 32602 (parse, request, method, params) and A2A's 32004, 32005, 32008, 32009 (unsupported operation, content type, required extension, version). An internal (-32603), server-defined (-32000 to -32099) or application error may follow real work and is never certifiable.
- Never `READ_ONLY`.

The payload is an object with `text` (1–65 536 characters) and/or `data` (a JSON object), nothing else and at least one of them.

### 4. Execution

After the dispatch intent (T16), with the lease, kill, circuit and credential rules of every connector, the `a2a` connector does the following:

1. **Validates** the enforced payload. If it is invalid, nothing is sent and the result is `NoEffect` `invalid_payload`.
2. **Sends once.** It POSTs a JSON-RPC `SendMessage` to the endpoint with `A2A-Version: 1.0` and the W3C trace headers. The credential comes through `Secret.Authorize` last, so SigV4 signs every header. The message's `messageId` is the action id, its parts are the text and a `data` part (`application/json`), and its metadata carries the operation key. `configuration.returnImmediately` is **true**: the reference server otherwise holds the reply until the executor finishes, leaving EACP without a task id to record or cancel (interop finding, Rev 1.0). The transport never replays the POST, follows no redirect, uses no proxy and reads at most 1 MiB per reply.
3. **Follows the task** while it is `SUBMITTED` or `WORKING` and the call's context is alive. It calls `GetTask` every second, doubling to at most 5 s, within the contract's `timeout_ms`. A reply must be JSON-RPC 2.0 with the request's id and carry the same task id.
4. **Classifies** the result:

| What happened | Outcome | Class / reference |
|---|---|---|
| `{"message": …}` reply | `Succeeded` | reference `message:<messageId>` |
| task `COMPLETED` | `Succeeded` | reference = task id |
| task `REJECTED`, never reported `WORKING` | `NoEffect` | `a2a_rejected` (`Ambiguous` unless certified) |
| task `REJECTED` after the agent reported it `WORKING` | `Ambiguous` | `a2a_rejected_after_work` |
| task `FAILED` / `CANCELED` | `Ambiguous` | `a2a_failed` / `a2a_canceled` |
| task `INPUT_REQUIRED` / `AUTH_REQUIRED` | `Ambiguous` | `a2a_input_required` / `a2a_auth_required` |
| still running when the context ends | `Ambiguous` | `a2a_interrupted` |
| no reply to `SendMessage` before the deadline | `Ambiguous` | `timeout` |
| JSON-RPC protocol-layer error reply to `SendMessage` (the codes in §3) | `NoEffect` | `a2a_rpc_<abs(code)>` (`Ambiguous` unless certified) |
| any other JSON-RPC error reply to `SendMessage` | `Ambiguous` | `a2a_rpc_<abs(code)>` |
| HTTP 401/403 to `SendMessage` | `NoEffect` | `unauthorized` (the worker drops the credential) |
| connection refused | `NoEffect` | `connection_refused_before_send` |
| anything else | `Ambiguous` | `http_<code>`, `transport_error`, `invalid_response`, … |

   A task or message id must be 1–256 characters of `[A-Za-z0-9._:-]` and must not contain the credential; otherwise the result is `invalid_response`.
5. **Contains the task.** The connector sends one `CancelTask` with its own 2 s budget, best effort, logging whether it was accepted. It does so for a task it stops following: interrupted (deadline, kill, cancellation, lost lease), waiting for input or authentication, in an unknown state, or answered for another id. The outcome stays `Ambiguous`: a cancel request proves nothing. EACP never answers an input or auth request on the agent's behalf.

`Lookup` always reports `unknown`.

### 5. The remote reference

An ambiguous or no-effect attempt carries no external reference (ADR-004). `eacp.action_attempts.remote_reference` holds the remote task id: at most 512 characters of `[A-Za-z0-9._:-]`, set only with the attempt's outcome by its worker, only when that outcome is `ambiguous` or `no_effect`, and never changed afterwards. The worker drops a malformed one, and one that contains a credential. It is kept when a kill replaces the result, and when a kill turns a success into an unknown outcome the success's reference (the task id) becomes the remote reference. The action's evidence shows it, so the human who settles an unknown outcome can look the task up at the remote agent. It never decides anything.

### 6. What is stored

Only ids, states, counts and digests. The delegation log line names the host, action id, task id, final state (a state EACP does not know is logged as `unknown`), `GetTask` polls, artifact count and the SHA-256 of the artifacts' canonical JSON. Message and artifact content from the remote agent is untrusted and never stored, journaled or logged; the calling agent sees the external reference, not the output.

> **Phase 26b (ADR-034):** a completed task's artifacts, or a direct reply's parts, are returned as a success's output. They are kept for the calling agent only when the contract has a `result_retention_seconds`, and are still never journaled or logged.

## Consequences

- A delegation is governed like any other external effect: allowlist, policy, approval, budget, kill, circuit and evidence all apply unchanged.
- Governance is per remote agent, not per skill. An agent allowed to delegate may ask for any skill the certified card lists; a new skill is a high-risk change that quarantines the delegate until two people certify the new card.
- Every unfinished delegation needs a human: A2A offers no way to prove a lost or interrupted task had no effect.
- A card that flaps (a deploy that serves an older card for a moment) quarantines a certified delegate, and a human releases it (ADR-023 §7).

## Unresolved assumptions

| Assumption | Conservative choice |
|---|---|
| Whether a resent message is deduplicated | No: a delegation is sent at most once, never retried after a send |
| What an unfinished, failed or interrupted task did | Unknown, for a human; only `REJECTED` and pre-work errors may be certified as no effect |
| Whether a `REJECTED` task did any work | Trusted only if the agent never reported it `WORKING`; an agent that works without saying so defeats this, which is why certifying `a2a_rejected` is a human's choice per contract |
| Which card members are display-only | Only `iconUrl` and `documentationUrl`; unknown members are certified |
| Where the worker may send a task | Only the certified interface URL, which must equal the connector endpoint exactly |
| Whether a cancel stopped the work | Never assumed; one best-effort `CancelTask`, the outcome unchanged |
| Card signatures (JWS) | Part of the certified definition, not verified |
| Remote output | Untrusted and not persisted |
| The incident title for a quarantined delegate | ADR-027's scanner-quarantine signal unchanged (`mcp_drift`, "MCP tool …") |

Out of scope: inbound A2A, streaming (`SendStreamingMessage`, `SubscribeToTask`), push notifications, the REST and gRPC bindings, the v0.3 wire format, `GetExtendedAgentCard`, multi-turn conversations, returning remote output to the agent, reconciling by task id, per-skill governance and Kubernetes demo coverage.

## Verification

- `internal/registry/a2a_schema_test.go` (raw SQL as `eacp_app`): the `a2a` scan row, `delegate` discovered and never declared, exactly one delegate per scan, protocol versions per connector, a card change that quarantines, the contract rules, the `v2` fingerprint. `internal/worker/remote_reference_schema_test.go`: the `remote_reference` rules.
- `internal/connector/a2a`: card validation and the host binding, the canonical definition, the request shape (SigV4 included), every classification row, polling, containment, payload validation, response limits, no redirects, the credential never leaving the host; interoperability with the a2a-go v2.6.0 reference server (card, `SendMessage`, `GetTask`, `CancelTask`).
- `internal/worker`: the scanner by protocol; `TestTheWorkerDelegatesToAnA2AAgent`, `TestAnUncertifiedRejectionIsUnknown` and `TestNothingSecretIsPersistedByADelegation` through the real worker, connector and fake agent.
- `internal/bundle`: `a2a` connectors, tools never declared, contracts pinned.
- `test/security` (the agent cannot reach `fakea2a`; only it mounts its verifier) and `test/demo` `TestA2ADemo` (`DEMO=D scripts/demo.sh`).
