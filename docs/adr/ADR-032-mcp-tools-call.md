# ADR-032: Governed MCP `tools/call`

Status: Accepted (Rev 1.0, 2026-09-29). Scope: Phase 26a.
Related: ADR-001 (only workers reach connectors and hold their secrets), ADR-003 §4 (connectors, tools, contracts), ADR-004 (unknown outcomes, the dispatch intent), ADR-019 (credential providers), ADR-023 (discovered tools, fingerprints, quarantine), ADR-030 (A2A delegation, the closest pattern).

## Context

An agent may already be granted an MCP tool, and the scanner already certifies it (ADR-023), but no worker serves protocol `mcp`: an action on an MCP tool expires unclaimed (ADR-023 §1). Phase 26a makes the execution worker **call** a certified MCP tool with the guarantees of every other connector call: allowlist, policy, approval, budget, kill switch, circuit, dispatch intent, fencing, at most one send, and a human for any outcome nobody can prove.

The Model Context Protocol (revision `2026-07-28`, with a legacy `initialize` fallback; `research/REFERENCES.md`) shapes this decision in four ways:

- **No idempotency key and no lookup.** `tools/call` carries neither, so a resent call may run the tool twice, and a call whose reply was lost cannot be found (ADR-023 §5).
- **Every annotation is an untrusted hint.** `readOnlyHint` comes from the server. A hint never permits a retry.
- **The definition can change under a certified integration.** T16 compares a contract with the registry's current definition, which is as fresh as the last scan (up to 15 minutes). A server that changes a tool between scans would otherwise be called with an unreviewed definition (the rug pull of ADR-023 §3).
- **The reply is untrusted content.** Tool output, `structuredContent` and error text come from a remote server. Nothing in EACP returns or stores tool output today (HTTP and A2A return only `external_reference`).

**Split (owner, 2026-09-29).** Phase 26 is two phases. Phase 26a (this ADR) returns what A2A returns: a reference, never the tool's output. Phase 26b is a separate ADR for a governed result channel (bounded, retained for a limited time, readable only by the calling agent) that serves HTTP, MCP and A2A alike. Storing output is exactly what 26b decides.

## Decision

The section numbers S3.1 to S3.7 match the approved design (`docs/superpowers/specs/2026-09-29-phase-26a-mcp-tools-call-design.md`, section 3).

### S3.1 The worker serves `mcp`

`mcp.Client` implements `worker.Connector`. `Execute` runs one call (S3.3). `Lookup` always answers `unknown`: MCP has no lookup by operation key (ADR-023 §5), so the reconciler never claims an MCP action. `cmd/execution-worker` registers the client under `mcp` beside `http` and `a2a`. The stdio transport, OAuth authorization flows, `subscriptions/listen`, sampling, elicitation and roots stay out of scope, as in ADR-023.

### S3.2 Contract rules for an MCP tool (PostgreSQL)

The contract trigger already pins `definition_id`, requires `reconciliation_lookup = none` and allows `READ_ONLY` only when the certified definition says `readOnlyHint: true`. Phase 26a adds, for an `mcp` connector's tool:

- `idempotency_mode = none` and `max_attempts = 1`, **even for `READ_ONLY`**. The protocol offers no idempotency key and every annotation is an untrusted hint, so a hint never permits a retry. A lost read goes to a human; a later revision may relax this for `READ_ONLY` with its own ADR.
- `no_effect_errors` only from the classes of S3.4 that the worker reports before or instead of any tool run: `connection_refused_before_send`, `unauthorized`, `invalid_payload`, `definition_changed`, `tool_missing`, `unsupported_header_mirroring`, `definition_unverified`, and `mcp_rpc_<code>` for the codes 32700, 32600, 32601 and 32602. The class `mcp_tool_error` may be certified too, as a per-contract human choice, like `a2a_rejected` (ADR-030 §3).

Existing certified MCP contracts (none can have executed) are unaffected: the rules apply to new contracts, and an old contract with `max_attempts > 1` is refused at dispatch by the worker (`invalid_contract`, nothing sent). HTTP and A2A contracts are untouched.

### S3.3 One call

After the dispatch intent (T16), the kill check and the credential rules of every connector, `mcp.Client.Execute` does the following:

1. **Validates** the enforced payload: a JSON object (the tool's `arguments`). Otherwise nothing is sent and the result is `NoEffect` `invalid_payload`. The worker does not validate against `inputSchema`: the server does, and a validation error is a protocol error (`-32602`), a certifiable no-effect class.
2. **Opens a session** exactly as discovery does (modern probe, else legacy `initialize`), with the worker-held Bearer. An AWS-signing credential is refused (`unsupported_credential`), as for discovery.
3. **Checks the definition before it calls** (S3.5). Any mismatch means no `tools/call` is sent.
4. **Sends `tools/call` once**, with `name` = the tool's `remote_name` and `arguments` = the enforced payload, plus the modern `_meta` and headers (`Mcp-Method`, and on the modern revision only `Mcp-Name` = the tool's remote name). The transport never replays the POST, follows no redirect and uses no proxy. A certified `inputSchema` that contains `x-mcp-header` is refused with `unsupported_header_mirroring` (nothing sent, checked before the session opens): the value encoding of `Mcp-Param-{name}` is not specified in the verified specification page (`research/REFERENCES.md`, Phase 26a), so header mirroring stays out of scope.
5. **Classifies** the reply (S3.4) and returns a `worker.Result`.

Kill and cancellation: the call runs under the worker's context, which the heartbeat cancels on a lost lease, a cancellation request or a kill. A cancelled call that was already sent is `Ambiguous` `timeout` or `mcp_interrupted`; EACP sends no `notifications/cancelled` in 26a.

### S3.4 Classification

| What happened | Outcome | Class / reference |
|---|---|---|
| `tools/call` result, `isError` false, `structuredContent` (if any) valid against the certified `outputSchema` | `Succeeded` | reference `mcp:sha256:<hex>`, the SHA-256 of the result in RFC 8785 form (a digest, never content) |
| result with `isError: true` | `Ambiguous` | `mcp_tool_error` (`NoEffect` only when the contract certifies it) |
| `structuredContent` that breaks the certified `outputSchema` | `Ambiguous` | `mcp_output_invalid` |
| `resultType: "input_required"` (or any type but `complete`) | `Ambiguous` | `mcp_input_required`; the worker never answers it and never loops |
| JSON-RPC error 32700, 32600, 32601, 32602 | `NoEffect` | `mcp_rpc_<abs(code)>` (`Ambiguous` unless certified) |
| any other JSON-RPC error | `Ambiguous` | `mcp_rpc_<abs(code)>` |
| HTTP 401 or 403 (at any stage before `tools/call` is sent, or in reply to it) | `NoEffect` | `unauthorized` (the worker drops the credential) |
| connection refused (at any stage before `tools/call` is sent, or in reply to it) | `NoEffect` | `connection_refused_before_send` |
| any other failure while probing, initializing or listing (before `tools/call` is sent) | `NoEffect` | `definition_unverified` |
| definition or tool differs from the certified one (S3.5) | `NoEffect` | `definition_changed` or `tool_missing` |
| the `tools/call` was sent and no valid reply came (timeout, reset, malformed, oversize) | `Ambiguous` | `timeout`, `transport_error`, `invalid_response`, `response_too_large` |

Two rules refine the table (controller decisions, 2026-09-29):

- **Before `tools/call` is sent.** A refused connection or an HTTP 401 or 403 at any stage before the call (the probe, `initialize`, the listing) reports `connection_refused_before_send` or `unauthorized`. Any other failure while probing, initializing or listing (a timeout, a reset, a malformed or oversize reply, an HTTP error) reports `definition_unverified`. Nothing was called, so both are `NoEffect` and certifiable, and the action fails closed.
- **`timeout`.** The `timeout` row applies only to a `tools/call` that was sent and got no reply. A deadline that expires while probing, initializing or listing is `definition_unverified`, never `timeout`.

`Ambiguous` goes to `UNKNOWN_OUTCOME` and to a human once it settles (ADR-004). A `Succeeded` result needs an external reference, so a success always carries the digest. The digest proves that a result arrived and lets a human compare it with the tool's own log; it reveals nothing about the content.

### S3.5 The definition check before every call

The worker checks the server itself, in the same session it will call:

- It lists tools (`tools/list`, every page, the ADR-023 limits) and canonicalizes the tool named `remote_name` with the same code the scanner uses (`canonicalTool`).
- It compares that canonical text, **byte for byte**, with the certified canonical definition, which the worker reads with the job (`tool_definitions.definition` of the contract's `definition_id`). No digest is compared, so no hash is computed in Go. An empty certified definition is refused as `definition_unverified`.
- Not equal: `NoEffect` `definition_changed`. Not listed, or listed but rejected by the scanner's rules: `NoEffect` `tool_missing`. Nothing was called, so both are certifiable. The worker logs a security alert (`worker.mcp_definition_changed`); the next scan records the change and quarantines the tool (ADR-023 §7).
- A listing that cannot complete (a page fails, a limit is hit, the connection fails) is a failed check. Nothing was called, so it is `NoEffect` `definition_unverified` (certifiable), or `connection_refused_before_send` or `unauthorized` under the rules of S3.4, and the action fails closed.

Cost: one full listing per call (at most 500 tools and 4 MiB). This is the conservative choice; a short-lived cache is a later optimisation that needs its own reasoning about the window it opens.

Residual risk: the server may change the tool between the listing and the call. The human who approved the action and the two people who certified the definition bound the damage; nothing in the protocol closes the window.

### S3.6 What is stored

Only ids, states, counts and the digest. The tool's output, its `content`, its `structuredContent` and any error text from the server are untrusted and never stored, journaled or logged (ADR-030 §6). The credential is redacted from every log line and refused in a reference.

### S3.7 Changes outside `internal/connector/mcp`

- `worker.Call` gains the tool's `RemoteName` and the certified `Definition` (canonical text), read by `Store.Load` through the contract's `definition_id`; both are empty for other protocols.
- `cmd/execution-worker/main.go` registers `mcp`. The reconciler needs no change (a `none` lookup is never listed).
- `internal/fakemcp` and `internal/connector/mcp/mcptest` gain `tools/call` (scenarios: success, `isError`, bad output, `input_required`, a protocol error, a changed definition, a hang) and a durable, content-free call log for the demo and the interoperability test.
- Documents: the `AGENTS.md` rule "No worker serves protocol `mcp` yet" becomes the ADR-032 rule; `docs/FEATURES(.th).md` and `docs/DEMO(.th).md` where they say MCP tools cannot be called. ADR-023's "Out of scope for Phase 14" paragraph points here.

## Invariants

Each becomes a test first.

1. **At most one send.** No path sends `tools/call` twice for one attempt, and no MCP contract allows a second attempt.
2. **No call on an unverified definition.** If the server's current definition is not byte-equal to the certified one, or cannot be verified, no `tools/call` is sent.
3. **Output never leaves the worker.** No log line, journal entry, database column or error string contains any part of a tool's output or error text (a canary string in the fake server's reply is searched for everywhere).
4. **Credentials.** The Bearer goes only to the connector's endpoint; an AWS-signing credential is refused; a rejected token is dropped; redirects and proxies are refused.
5. **A human settles anything unproven.** `isError`, invalid output, `input_required`, transport failures after the send and unknown JSON-RPC errors all reach `UNKNOWN_OUTCOME`; only the certified no-effect classes are `NoEffect`.
6. **The contract rules hold in PostgreSQL**, tested with raw SQL as `eacp_app`: `idempotency_mode = none`, `max_attempts = 1`, and the certifiable classes.

## Consequences

- An MCP tool call is governed like any other external effect: allowlist, policy, approval, budget, kill, circuit and evidence apply unchanged.
- No MCP call is ever retried, not even a `READ_ONLY` one. A lost call needs a human once it settles.
- A tool that changes between scans is never called. The cost is one full listing per call and a failed check when the server cannot list.
- The calling agent sees a digest, not the tool's output. A workflow that needs the result waits for Phase 26b.
- A tool whose schema uses `x-mcp-header` cannot be called until its encoding is verified against the specification.

## Unresolved assumptions

| Assumption | Conservative choice |
|---|---|
| `READ_ONLY` retries | None in 26a: `max_attempts = 1` for every MCP contract. A later ADR may allow them for a two-person certified `READ_ONLY` contract |
| `notifications/cancelled` | Not sent. A sent call that is cut stays `Ambiguous`; whether the server stops is unknown either way |
| Faster drift response | A refused call logs an alert and waits for the next scan (at most 15 minutes). A worker-requested rescan would need an actor the scanner lease does not give it; it is a follow-up |
| The external reference | `mcp:sha256:<hex>` of the canonical result. The alternative, `mcp:<action id>`, proves nothing about the result |
| ADR number | ADR-032 is the next free number on `main` (ADR-031 is the LLM gateway) |
| Whether a tool annotation can be trusted | No: every annotation is an untrusted hint and never permits a retry |
| Whether a server deduplicates a resent call | No: a call is sent at most once, never retried after a send |
| The `x-mcp-header` value encoding | Unknown (the specification page does not state it): such a tool is refused with `unsupported_header_mirroring` |
| Remote output | Untrusted and not persisted |

## Out of scope

Returning or storing output (Phase 26b), stdio, OAuth flows, sampling, elicitation, roots, subscriptions, progress notifications, cancellation notifications, retries of any kind, a definition cache, header mirroring (`x-mcp-header`) until its encoding is verified, per-argument policy and non-JSON content types.

## Verification

- `internal/registry/mcp_schema_test.go` (raw SQL as `eacp_app`): `TestMCPContractsAreAtMostOnce` (each refused and each accepted case; an HTTP or A2A contract is untouched) beside `TestMCPContractRules`.
- `internal/connector/mcp`: `TestExecuteClassifiesEveryReply` (every row of S3.4 against `mcptest`), `TestExecuteRefusesAPayloadThatIsNotAnObject`, `TestExecuteRefusesAnUnsafeContract`, `TestExecuteRefusesASigningCredential`, `TestExecuteWorksOverSSE`, `TestExecuteWorksOnALegacyServer`, `TestExecuteNeverLeaksOutput`, `TestExecuteSendsOnceAndNeverFollowsARedirect`; the definition check: `TestAChangedDefinitionIsNeverCalled`, `TestAMissingOrRejectedToolIsNeverCalled`, `TestAnUnverifiableListingIsNeverCalled`, `TestTheSameDefinitionInAnotherKeyOrderIsNotDrift`, `TestAToolOnALaterPageIsFound`, `TestAnEmptyCertifiedDefinitionIsRefused`, `TestAToolWithHeaderMirroringIsRefused`.
- `internal/worker`: `TestLoadCarriesTheCertifiedDefinition` (`Store.Load`), and through the real worker, connector and fake server `TestTheWorkerCallsAnMCPTool`, `TestAChangedDefinitionBetweenScansIsNeverCalled`, `TestAnMCPToolErrorIsUnknownUntilCertified`, `TestAnMCPActionIsNeverRetried`, `TestAKillDuringAnMCPCallIsUnknown`, `TestNothingOfTheOutputIsPersisted`, `TestAWorkerWithoutMCPLeavesTheActionQueued`.
- Interoperability with the official Go SDK server (`modelcontextprotocol/go-sdk`, already a dependency): `TestExecuteCallsAToolOfTheOfficialSDKServer`.
- `internal/fakemcp` (`tools/call`, a content-free call log), `test/security` (the agent cannot reach the MCP server; only the worker holds its token) and `test/demo` `TestSliceCDemo` (`DEMO=C scripts/demo.sh`) extended with one approved MCP call and one rug pull refused.
- Everything runs with `-race`; the PostgreSQL suites run with `EACP_TEST_ADMIN_DSN` set (skipped tests are not passes).
