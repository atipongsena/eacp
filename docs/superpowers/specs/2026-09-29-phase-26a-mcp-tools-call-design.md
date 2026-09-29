# Phase 26a: MCP `tools/call` (design)

Status: draft for the owner's review, 2026-09-29. It becomes ADR-032 when accepted.
Parent: [the Agent Studio program design](2026-09-29-agent-studio-program-design.md), section 8.
Inputs: ADR-023 (MCP registry), ADR-030 (A2A delegation, the closest pattern), ADR-004 (unknown outcomes),
`research/STUDIO_REFERENCES.md` section 2 (verified facts about `tools/call`).

## 1. Intent

An agent may already be granted an MCP tool, and the scanner already certifies it, but no worker serves protocol
`mcp`: an action on an MCP tool expires unclaimed (ADR-023 section 1). Phase 26a makes the execution worker **call**
a certified MCP tool, with the same guarantees as every other connector call: allowlist, policy, approval, budget,
kill switch, circuit, dispatch intent, fencing, at most one send, and a human for any outcome nobody can prove.

**Split decision (owner, 2026-09-29).** Phase 26 is two phases. **26a** (this document) returns what A2A returns:
a reference, never the tool's output. **26b** is a separate ADR for a governed *result channel* (bounded, retained
for a limited time, readable only by the calling agent) that serves HTTP, MCP and A2A alike. 26b runs before 27a,
because Studio's `tool_call` step and the leave-balance template need a result (a gap found while writing this
document; the program design is amended).

## 2. What already exists (so 26a stays small)

- The scanner discovers tools, computes fingerprints in PostgreSQL, classifies changes and quarantines a certified
  tool on a high-risk change. A contract pins `definition_id`. T16 already denies a call whose contract no longer
  matches (`eacp.dispatch_drift`) and a quarantined tool.
- The worker serves the protocols in its `Connectors` map and claims only actions on those connectors
  (`cmd/execution-worker/main.go`, `worker.Store.Claimable`). Serving `mcp` is a map entry plus an `Execute`.
- `internal/connector/mcp` already speaks Streamable HTTP for discovery: the modern revision `2026-07-28`, a legacy
  `initialize` fallback, no redirects, no proxy, response budgets, a Bearer only (`SignsRequests()` is refused).
- Nothing in EACP returns tool output today (HTTP and A2A return only `external_reference`), and nothing stores it.

## 3. Decisions

### 3.1 The worker serves `mcp`

`mcp.Client` implements `worker.Connector`. `Execute` runs one call (section 3.3); `Lookup` always answers
`unknown` (MCP has no lookup by operation key; ADR-023 section 5), so the reconciler never claims an MCP action.
`cmd/execution-worker` registers the client under `mcp` beside `http` and `a2a`. The stdio transport, OAuth
authorization flows, `subscriptions/listen`, sampling, elicitation and roots stay out of scope, as in ADR-023.

### 3.2 Contract rules for an MCP tool (new PostgreSQL rules)

The contract trigger (migration 00014, `tool_contracts`) already pins `definition_id`, requires `reconciliation_lookup
= none` and allows `READ_ONLY` only when the certified definition says `readOnlyHint: true`. 26a adds:

- `idempotency_mode = none` and `max_attempts = 1`, **even for `READ_ONLY`**. The protocol offers no idempotency key
  and every annotation is an untrusted hint, so a hint never permits a retry. A lost read goes to a human; a later
  revision may relax this for `READ_ONLY` with its own ADR.
- `no_effect_errors` only from the classes in section 3.4 that the worker reports before or instead of any tool
  run: `connection_refused_before_send`, `unauthorized`, `invalid_payload`, `definition_changed`, `tool_missing`,
  `unsupported_header_mirroring`, `definition_unverified`, and `mcp_rpc_<code>` for the codes 32700, 32600, 32601 and 32602. The class
  `mcp_tool_error` (section 3.4) may be certified too, as a per-contract human choice, like `a2a_rejected`.

Existing certified MCP contracts (none can have executed) are unaffected: the rules apply to new contracts, and an
old contract with `max_attempts > 1` is refused at dispatch by the worker (`invalid_contract`, nothing sent).

### 3.3 One call

After the dispatch intent (T16), the kill check and the credential rules of every connector, `mcp.Client.Execute`:

1. **Validates** the enforced payload: a JSON object (the tool's `arguments`). Otherwise nothing is sent and the
   result is `NoEffect` `invalid_payload`. The worker does not validate against `inputSchema`: the server does, and
   a validation error is a protocol error (`-32602`), which is a certifiable no-effect class.
2. **Opens a session** exactly as discovery does (modern probe, else legacy `initialize`), with the worker-held
   Bearer. An AWS-signing credential is refused (`unsupported_credential`), as for discovery.
3. **Checks the definition before it calls** (section 3.5). Any mismatch means no `tools/call` is sent.
4. **Sends `tools/call` once**, with `name` = the tool's `remote_name` and `arguments` = the enforced payload, plus
   the modern `_meta` and headers. The transport never replays the POST, follows no redirect and uses no proxy. A
   certified `inputSchema` that contains `x-mcp-header` is refused with `unsupported_header_mirroring` (nothing
   sent): the header-value encoding is not in the verified notes, and the plan's first task verifies it against the
   specification source before that refusal is lifted.
5. **Classifies** the reply (section 3.4) and returns a `worker.Result`.

Kill and cancellation: the call runs under the worker's context, which the heartbeat cancels on a lost lease, a
cancellation request or a kill. A cancelled call that was already sent is `Ambiguous` `timeout` or
`mcp_interrupted`; EACP sends no `notifications/cancelled` in 26a (open point 2).

### 3.4 Classification

| What happened | Outcome | Class / reference |
|---|---|---|
| `tools/call` result, `isError` false, `structuredContent` (if any) valid against the certified `outputSchema` | `Succeeded` | reference `mcp:sha256:<hex>`, the SHA-256 of the result in RFC 8785 form (a digest, never content) |
| result with `isError: true` | `Ambiguous` | `mcp_tool_error` (`NoEffect` only when the contract certifies it) |
| `structuredContent` that breaks the certified `outputSchema` | `Ambiguous` | `mcp_output_invalid` |
| `resultType: "input_required"` (or any type but `complete`) | `Ambiguous` | `mcp_input_required`; the worker never answers it and never loops |
| JSON-RPC error 32700, 32600, 32601, 32602 | `NoEffect` | `mcp_rpc_<abs(code)>` (`Ambiguous` unless certified) |
| any other JSON-RPC error | `Ambiguous` | `mcp_rpc_<abs(code)>` |
| HTTP 401 or 403 | `NoEffect` | `unauthorized` (the worker drops the credential) |
| connection refused | `NoEffect` | `connection_refused_before_send` |
| definition or tool differs from the certified one (3.5) | `NoEffect` | `definition_changed` or `tool_missing` |
| the request was sent and no valid reply came (timeout, reset, malformed, oversize) | `Ambiguous` | `timeout`, `transport_error`, `invalid_response`, `response_too_large` |

`Ambiguous` goes to `UNKNOWN_OUTCOME` and to a human once it settles (ADR-004). A `Succeeded` result needs an
external reference, so a success always carries the digest. The digest proves that a result arrived and lets a human
compare it with the tool's own log; it reveals nothing about the content.

### 3.5 The definition check before every call

T16 compares the contract with the registry's current definition, which is as fresh as the last scan (up to 15
minutes). A server that changes a tool between scans would be called with an unreviewed definition (the rug pull of
ADR-023). So the worker checks the server itself, in the same session it will call:

- It lists tools (`tools/list`, every page, the ADR-023 limits) and canonicalizes the tool named `remote_name` with
  the same code the scanner uses (`canonicalTool`).
- It compares that canonical text, **byte for byte**, with the certified canonical definition, which the worker
  reads with the job (`tool_definitions.definition` of the contract's `definition_id`). No digest is compared, so no
  hash is computed in Go.
- Not equal: `NoEffect` `definition_changed`. Not listed, or listed but rejected by the scanner's rules:
  `NoEffect` `tool_missing`. Nothing was called, so both are certifiable. The worker logs a security alert
  (`worker.mcp_definition_changed`); the next scan records the change and quarantines the tool (ADR-023 section 7).
- A listing that cannot complete (a page fails, a limit is hit) is a failed check. Nothing was called, so it is
  `NoEffect` `definition_unverified` (certifiable), and the action fails closed.

Cost: one full listing per call (at most 500 tools and 4 MiB). This is the conservative choice; a short-lived cache
is a later optimisation that needs its own reasoning about the window it opens.

Residual risk, stated plainly: the server may change the tool between the listing and the call. The human who
approved the action and the two people who certified the definition bound the damage; nothing in the protocol
closes the window.

### 3.6 What is stored

Only ids, states, counts and the digest. The tool's output, its `content`, its `structuredContent` and any error
text from the server are untrusted and never stored, journaled or logged (ADR-030 section 6). The credential is
redacted from every log line and refused in a reference. Storing output is exactly what 26b decides.

### 3.7 Changes outside `internal/connector/mcp`

- `worker.Call` gains the tool's `RemoteName` and the certified `Definition` (canonical text), read by `Store.Load`
  through the contract's `definition_id`; both are empty for other protocols.
- `cmd/execution-worker/main.go` registers `mcp`; the reconciler needs no change (a `none` lookup is never listed).
- `internal/fakemcp` and `internal/connector/mcp/mcptest` gain `tools/call` (scenarios: success, `isError`, bad
  output, `input_required`, a protocol error, a changed definition, a hang) and a durable call log for the
  demo and the interoperability test.
- `docs`: ADR-032; the `AGENTS.md` rule "No worker serves protocol `mcp` yet" becomes the ADR-032 rule;
  `docs/FEATURES(.th).md` and `docs/DEMO(.th).md` where they say MCP tools cannot be called.

## 4. Invariants (each becomes a test first)

1. **At most one send.** No path sends `tools/call` twice for one attempt, and no MCP contract allows a second
   attempt.
2. **No call on an unverified definition.** If the server's current definition is not byte-equal to the certified
   one, or cannot be verified, no `tools/call` is sent.
3. **Output never leaves the worker.** No log line, journal entry, database column or error string contains any
   part of a tool's output or error text (a canary string in the fake server's reply is searched for everywhere).
4. **Credentials.** The Bearer goes only to the connector's endpoint; an AWS-signing credential is refused; a
   rejected token is dropped; redirects and proxies are refused.
5. **A human settles anything unproven.** `isError`, invalid output, `input_required`, transport failures after the
   send and unknown JSON-RPC errors all reach `UNKNOWN_OUTCOME`; only the certified no-effect classes are
   `NoEffect`.
6. **The contract rules hold in PostgreSQL**, tested with raw SQL as `eacp_app`: `idempotency_mode = none`,
   `max_attempts = 1`, and the certifiable classes.

## 5. Verification plan

- **PostgreSQL, raw SQL as `eacp_app`:** the new contract rules (each refused and each accepted case), and that an
  A2A or HTTP contract is untouched.
- **`internal/connector/mcp`:** every row of the classification table against `mcptest`; the definition check
  (changed description, changed schema, missing tool, rejected tool, a failing page, a hang); the modern and the
  legacy path; the no-replay, no-redirect and credential rules; the output canary.
- **`internal/worker`:** the real worker, connector and fake server end to end (`TestTheWorkerCallsAnMCPTool`,
  `TestAChangedDefinitionIsNeverCalled`, `TestAnMCPErrorIsUnknownUntilCertified`, `TestNothingOfTheOutputIsPersisted`),
  the kill and the cancellation during a call, and a stale lease.
- **Interoperability:** the official Go SDK server (`modelcontextprotocol/go-sdk` v1.8.0, already a dependency) for
  a real `tools/call`, as discovery does today.
- **Compose:** `test/security` (the agent cannot reach the MCP server; only the worker holds its token) and the Slice
  C demo (`DEMO=C`) extended with one MCP call, approved and executed, and one rug pull refused.
- Run everything with `-race`; run the PostgreSQL suites with `EACP_TEST_ADMIN_DSN` set (skipped tests are not
  passes).

## 6. Out of scope

Returning or storing output (26b), stdio, OAuth flows, sampling, elicitation, roots, subscriptions, progress
notifications, cancellation notifications, retries of any kind, a definition cache, header mirroring
(`x-mcp-header`) until its encoding is verified, per-argument policy and non-JSON content types.

## 7. Open points (each has a conservative default; the owner may overrule)

1. **`READ_ONLY` retries.** Default: none in 26a (`max_attempts = 1` for every MCP contract). A later ADR may allow
   them for a two-person certified `READ_ONLY` contract.
2. **`notifications/cancelled`.** Default: not sent. A sent call that is cut stays `Ambiguous`; whether the server
   stops is unknown either way.
3. **Faster drift response.** Default: a refused call logs an alert and waits for the next scan (at most 15
   minutes). A worker-requested rescan would need an actor the scanner lease does not give it; it is a follow-up.
4. **The digest as external reference.** Default: `mcp:sha256:<hex>` of the canonical result. The alternative,
   `mcp:<action id>`, proves nothing about the result.
5. **ADR number.** ADR-032 is the next free number on `main` (ADR-031 is the LLM gateway).
