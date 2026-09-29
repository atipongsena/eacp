# ADR-023: MCP registry, tool fingerprint and certification

Status: Accepted (Rev 1.0, 2026-09-24). Scope: Slice C Phase 14 (MASTER_PLAN §31, §33, §34, §67, §88).
Related: ADR-001 (only workers reach connectors and hold their secrets), ADR-003 §4 (connectors, tools, contracts, fingerprint), ADR-004 (T10/T16 re-check the capability), ADR-022 (connector circuits), ADR-030 (A2A agents reuse this machinery).

## Context

MASTER_PLAN §67 treats MCP servers and tool metadata as **untrusted**. §31 Rev 2 makes the connector contract an operator declaration bound to a tool fingerprint: when the fingerprint changes, the contract is invalid until the tool is recertified. §33 asks for a fingerprint over the tool schema, metadata and security attributes (canonical form, SHA-256) that is compared on every reconnect: a low-risk change is audited, and a high-risk change quarantines the tool or requires recertification. §68 lists **tool poisoning** and **MCP rug pull**: a server that shows one definition while a tool is certified, then serves a different one.

Before Phase 14, every connector spoke EACP's HTTP connector protocol, tools were declared by a `registry_editor`, and the fingerprint (ADR-003 §4) covered only the connector's identity (protocol, endpoint, secret reference, names).

## Decision

### 1. MCP servers are connectors

- `eacp.connectors.protocol` accepts `mcp`: a Streamable HTTP MCP endpoint. Everything else about a connector is unchanged: it is immutable, and its `secret_ref` names a bearer token that only the execution worker holds, bound to the endpoint's host (ADR-001 §3, ADR-003 §4).
- Each MCP connector gets exactly one `eacp.mcp_servers` row, created with it, which holds its scan schedule, the operator's rescan request and the scan lease.
- **Tools of an MCP connector are discovered, never declared.** A `registry_editor` cannot insert a tool on an MCP connector, and the scanner cannot insert a tool on any other connector.
- An MCP tool keeps the server's exact name in `remote_name`. Its EACP name (used in `connector.tool` references) equals `remote_name` when that is already a valid tool slug. Otherwise it is derived: lowercase, runs of other characters replaced by `_`, cut to 54 characters, followed by `-` and the first 8 hex digits of the SHA-256 of `remote_name`. The database derives it, so every scanner derives the same name.

**Discovered protocols (Phase 25a, ADR-030).** `a2a` connectors are discovered the same way: they get an `eacp.mcp_servers` row, the same scan lease and `eacp.mcp_record_scan`, and one discovered tool, `delegate`, whose definition embeds the remote agent's Agent Card. The scanner picks a discoverer by protocol; §3–§7 (fingerprint, risk, certification, quarantine) apply unchanged. The table names still say `mcp`.

**Out of scope for Phase 14: calling MCP tools (`tools/call`).** No worker serves protocol `mcp` yet, so the claim hint never offers an action on an MCP tool, and such an action expires unclaimed (fail safe). Execution needs the MCP connector contract (ADR-013) and a pre-dispatch definition check; it is a later phase (ADR-032 makes the worker call a certified MCP tool). The stdio transport, `subscriptions/listen` and OAuth authorization flows are out of scope as well: EACP scans remote servers with a worker-held bearer token.

### 2. Discovery (the scanner in `execution-worker`)

Discovery contacts an untrusted server with a worker-held secret, so it runs in the execution worker, which alone has the network path and the secret.

- **Protocol.** The client implements the MCP Streamable HTTP transport (spec in `research/REFERENCES.md`). It prefers the modern revision `2026-07-28` (stateless: every request carries `_meta` with the protocol version and client capabilities, and the `MCP-Protocol-Version` and `Mcp-Method` headers). A `400`, `404` or `405` whose body is not a recognized modern JSON-RPC error means a legacy server: the client falls back to an `initialize` handshake offering `2025-11-25`. It accepts the legacy revisions `2025-11-25`, `2025-06-18` and `2025-03-26`, and records the version it used. It follows no redirects, uses no proxy, and never sends the token anywhere but the connector's endpoint.
- **Listing.** `tools/list`, following `nextCursor`. A scan is complete only when every page arrived. Limits: 50 pages, 500 tools, 4 MiB of responses, 64 KiB per canonical definition, and a timeout (`EACP_MCP_SCAN_TIMEOUT`, 30 s).
- **Rejected tools.** A definition is rejected, and listed with its reason in the scan record, when its name is not 1–128 characters of `[A-Za-z0-9_.-]`, its name repeats, its `inputSchema` is not an object with `type: "object"`, its JSON is not I-JSON (so it has no RFC 8785 form, or contains U+0000), it is too large, or an `x-mcp-header` annotation breaks the transport's rules (which require a client to drop such a tool). A rejected tool is treated as not listed.
- **Schedule.** A connector is due every `EACP_MCP_SCAN_INTERVAL` (15 minutes), after a failed scan with exponential backoff capped at the interval, and immediately after an operator requests a rescan (`operator` or `registry_editor`).
- **Lease and fencing.** A scanner claims a due server by taking its scan lease: the lease generation increases by one, and the lease expires after twice the scan timeout. The scanner then calls the server with no transaction open, and records the result in one transaction bound with `storage.SetScanner` (worker id and lease generation). The database refuses the record unless that worker still holds that generation of an unexpired lease, so a stale scanner cannot commit. `scanner` is a messaging-like actor: `eacp.actor_context()` rejects it, so it can never change an action.
- Every scan, successful or not, is recorded in `eacp.mcp_scans`. A failed scan changes no tool.

### 3. Fingerprint (§33)

- **Canonical definition.** The tool object exactly as listed, minus the display-only fields `title`, `icons` and `annotations.title`, in RFC 8785 (JCS) form. Everything else is in it, including `description` (what a model reads, so the tool-poisoning vector), `inputSchema`, `outputSchema`, the behaviour annotations, `_meta` and any field a later revision adds. An unknown field is therefore treated as security-relevant.
- **Display metadata.** The removed fields, also in JCS form.
- **Computed by PostgreSQL.** The scanner stores both texts. The database computes their SHA-256 digests, checks that the definition parses as a JSON object whose `name` equals the tool's `remote_name` and whose `inputSchema` is an object, and never trusts a digest from the client.
- **Tool identity.** For an MCP tool, `eacp.tool_fingerprint` (ADR-003 §4) becomes a `v2` hash over the connector identity, the tool's names and the fingerprint of its **current** definition. It is NULL while the tool has no definition. For an HTTP tool it is the unchanged `v1` hash, so no existing contract changes.

### 4. Schema tracking and risk of a change

`eacp.tool_definitions` is an insert-only history per tool. The scanner inserts a row only when the canonical definition or the display metadata differs from the tool's current one, and the database classifies the change against the previous row:

| Risk | When | Effect |
|---|---|---|
| `initial` | the tool's first definition | none |
| `low` | only the display metadata changed | journaled only |
| `high` | the canonical definition changed | the contract no longer matches (§6); a certified tool is quarantined (§7) |

The database also records which top-level fields changed (`inputSchema`, `description`, `annotations`, …) and points `tools.definition_id` at the new row. When a certified tool is no longer listed (or is rejected) by a complete listing, `tools.missing_since` is set and the tool is quarantined too. A tool that reappears with the same definition gets no new row and stays quarantined.

### 5. Risk metadata

Each definition stores its behaviour annotations with the defaults the specification gives: `readOnlyHint` false, `destructiveHint` true, `idempotentHint` false, `openWorldHint` true (a read-only tool is recorded as non-destructive and idempotent). These are **self-described and untrusted** (§31 Rev 2), so they can only make a contract stricter:

- A contract may declare an MCP tool `READ_ONLY` only if its certified definition says `readOnlyHint: true`.
- An MCP contract must declare `reconciliation_lookup = none` (and so `proof_standard = none`), because MCP defines no lookup by operation key. An unknown outcome of an MCP tool goes to a human (ADR-004).

### 6. Certification and contract invalidation (§34)

- A contract for an MCP tool pins `definition_id`: the definition the proposer reviewed. It must be the tool's current definition when the contract is inserted, or the insert fails (55000) and the proposer reviews the new definition. The contract's fingerprint is then computed over that definition.
- Activating a contract (two-person, ADR-003 §4) is refused unless its fingerprint matches the tool's current fingerprint.
- After a high-risk change, the active contract's fingerprint no longer matches, so every capability check denies with `contract_fingerprint_mismatch`: submission, approval request, release (T10) and dispatch intent (T16 through `eacp.dispatch_drift`). Recertifying means proposing a new contract for the current definition and activating it (two people).
- The lifecycle of §34 maps onto these rows: DISCOVERED (a tool with a definition and no contract), SCANNED/VALIDATED (a complete scan accepted the definition), CERTIFIED (a contract pins it), ACTIVE (the contract is active and matches), QUARANTINED (§7) and REVOKED (the contract is revoked).

### 7. Quarantine

- `tools.quarantined_at` blocks the tool: `CheckCapability` and `eacp.action_capability_denial` deny with `tool_quarantined`. Every guarded action transition that checks the capability (submission, T2–T4, T10, T16) therefore refuses it. An action already dispatched is not affected: it finishes or is reconciled (ADR-004).
- The scanner quarantines a certified tool (one with an active contract) on a high-risk change or when it disappears. An `operator` or `registry_approver` may quarantine any tool (one person, with a reason: containment is fast).
- Releasing needs a `registry_approver` who is not the principal who quarantined it, and a reason. Releasing never re-certifies: a tool whose contract no longer matches stays blocked until a new contract is activated. A rug pull that flips back to the certified definition leaves the tool quarantined, so a human decides.
- Every quarantine, release, definition and scan is journaled in the tenant's audit chain.

### 8. Lock order and trust boundary

- A scan record locks the `mcp_servers` row, then the connector's tool rows (`FOR UPDATE`), then the audit chain head. It never locks an action, so the action lock order (AGENTS.md) is unchanged. A capability check reads the tool row `FOR SHARE`, so a definition change and a check serialize.
- The scanner canonicalizes; PostgreSQL hashes and classifies. A scanner bug that produced different canonical text for the same definition would cause a spurious high-risk change: it errs towards quarantine, never towards trust.
- As in ADR-003 §8, the triggers stop application bugs, not a holder of a stolen `eacp_app` credential.

## Consequences

- A changed MCP tool stops being executable as soon as its new definition is recorded, whatever path requests it. No component has to remember to check.
- A certified tool that flaps (disappears during a server deploy) is quarantined and needs a human to release it. This is deliberate: availability is traded for a human review.
- Approval requests are not re-checked for quarantine (the approval guard only checks the contract), but the release and the dispatch are. A human may therefore approve an action that can never be released.
- Discovery depends on server-reported data. EACP never trusts it for authorization: a definition only ever restricts.

## Unresolved assumptions

| Assumption | Conservative choice |
|---|---|
| Which tool fields are display-only | Only `title`, `icons` and `annotations.title`; unknown fields are fingerprinted |
| Whether a description-only change is high risk | Yes: descriptions are what a model reads (tool poisoning) |
| Whether a disappeared tool is high risk | Yes for a certified tool: it is quarantined |
| Trusting server annotations | Never for authorization; they can only tighten a contract |
| MCP reconciliation | No lookup is certified for MCP tools until a convention exists |
| Legacy servers | Supported back to `2025-03-26` through `initialize`; the deprecated HTTP+SSE transport is not |

## Verification

- `internal/registry/mcp_schema_test.go`: raw SQL as `eacp_app` for every trigger rule: discovered-only tools, scan lease fencing, DB-computed fingerprints, risk classification, contract pinning and activation, READ_ONLY and lookup rules, quarantine and release, `tool_quarantined` and `contract_fingerprint_mismatch` denials, the flip-back rug pull, and the audit events.
- `internal/connector/mcp`: the client against a modern and a legacy fake server, the official Go SDK server (interop), the canonical form, the rejection rules and the limits.
- `internal/worker/scanner_test.go`: discovery end to end, drift that invalidates and quarantines, the stale-scanner fence, and operator rescans.
- `internal/registry`, `internal/api`, `cmd/eacpctl`: the operator surface.
