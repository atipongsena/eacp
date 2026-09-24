# Phase 14 MCP Registry and Tool Fingerprint Review

Date: 2026-09-24
Scope: Slice C Phase 14 (MASTER_PLAN §88, with §31–§33; ADR-023 Rev 1.0; ADR-003 Rev 1.3 amendment; §103 invariants 1, 7, 8, 17 and 19).
Review: a self-review against ADR-023, ADR-003 and the invariants below, plus mutation checks of the new tests.

## Invariants stated before the code

1. **Discovered, not declared.** A tool of an MCP connector exists only because a scanner saw it. No principal can create one, and its current definition moves only when a scan records one.
2. **PostgreSQL decides trust.** The database computes every fingerprint, display digest and risk class, and applies annotations only as tightening hints. The scanner reports what the server said and nothing more.
3. **Fenced scans.** A scan commits only under the scan lease it claimed: the same worker, the same generation, before expiry. A stale or foreign scanner changes nothing, and a failed scan changes no tool.
4. **A contract certifies what was reviewed.** An MCP contract pins the definition its proposer reviewed, which must be current. A behavioural change (or the disappearance of a certified tool) quarantines the tool and the contract stops matching, so a pending approval cannot release it. Lifting a quarantine never recertifies.
5. **Containment is easy, release is two-person.** Quarantine is single-person containment (operator or registry approver); release needs a registry approver other than the quarantiner.
6. **Isolation and audit.** The new tables are tenant rows under forced RLS; the only cross-tenant read is a reviewed SECURITY DEFINER hint returning ids. Every scan, recorded definition, rescan request and quarantine change is journaled with its actor and reason.
7. **The scanner never becomes an action actor, and agents never see MCP credentials.** `eacp.actor_context()` rejects it, and the token stays in the worker, bound to its tenant, secret reference and host.

## Implemented

- **ADR-023** (new; Accepted Rev 1.0). ADR-003 Rev 1.3 amends §4 (v2 fingerprint for MCP tools, updatable tool columns, `tool_quarantined`).
- **Migration 00014.**
  - `connectors.protocol` allows `mcp`; `eacp.mcp_servers` (scan schedule and lease, one row per MCP connector, created by trigger), `eacp.mcp_scans` and `eacp.tool_definitions` (insert-only); `tools` gains `remote_name`, `origin`, `definition_id`, `missing_since` and quarantine fields; `tool_contracts.definition_id`.
  - `eacp.registry_actor()` recognizes the `scanner` actor bound by `storage.SetScanner`; `eacp.assert_scan_lease` fences every scanner write.
  - `eacp.tool_fingerprint` v2 for MCP tools; `tools_guard`, `tool_contracts_guard`, `action_capability_denial` and `audit_row_change` re-declared from 00003/00005 with the MCP rules.
  - `eacp.mcp_record_scan` sequences a whole scan; `eacp.mcp_scans_due` is the scanner's cross-tenant hint.
- **`internal/connector/mcp`.** The discovery client (modern 2026-07-28 with fallback to the initialize-based revisions; JSON and SSE; page, tool and byte limits; no redirects or proxy), the canonical definition and rejection rules, and `mcptest`, a fake server.
- **`internal/worker`.** `Scanner` (`scanner.go`) and its store (`scan_store.go`); the execution worker runs it with `EACP_MCP_SCAN_INTERVAL` and `EACP_MCP_SCAN_TIMEOUT`.
- **`internal/registry`, `internal/api`, `eacpctl`.** `ListTools`, `GetTool`, `ToolDefinitions`, `QuarantineTool`, `ReleaseTool`, `MCPServer`, `RequestScan`, `MCPScans`, `Contract.DefinitionID`; the matching `/v1/connectors/{id}/tools|mcp|mcp/scans|mcp/scan` and `/v1/tools/{id}[/definitions|/quarantine|/release]` routes; `eacpctl connector tools|mcp|scans|scan` and `eacpctl tool`.

## Findings from the self-review (fixed)

1. **Stateful SDK servers are legacy.** The interop test first expected the official SDK's stateful handler to negotiate 2026-07-28. It advertises only the 2025 revisions on that transport, so the client's fallback to `2025-11-25` is correct; the test now pins that.
2. **Lease checks masked by the release.** Removing any one of the worker, generation or expiry checks from `eacp.assert_scan_lease` went unnoticed: `mcp_record_scan` ends by releasing the lease, and `mcp_servers_guard` repeats the checks there. But a scanner session could also write `tools` or `tool_definitions` directly, where `assert_scan_lease` is the only fence. `TestScanLeaseFencesStaleScanners` now tries direct writes from a foreign worker, from a restarted worker at an old generation, and after expiry.
3. **No action-level drift test.** The capability check was tested, but nothing drove an approved action through a drifted tool. `TestMCPDefinitionDriftBeforeReleaseDenies` shows the release is denied (`tool_quarantined`), the grant stays unconsumed, the request is voided, and a later submission after release of the quarantine is denied with `contract_fingerprint_mismatch`.
4. **Isolation sweep.** The full-flow isolation test failed on the three new tables; the flow now includes a real scan.

## Mutation checks

Each mutation was applied alone and the relevant package's MCP tests run (`internal/registry`, `internal/worker`, `internal/connector/mcp`). 31 of 32 were killed:

- **Capability:** the quarantine check dropped from `eacp.action_capability_denial` or from `CheckCapability`.
- **Scan lease:** the worker, generation or expiry check dropped from `eacp.assert_scan_lease` (after finding 2); a claim over a live lease; the 10-minute lease cap; the scan hint ignoring the worker's secret bindings; a failed scan treated as a complete listing.
- **Classification:** a behavioural change classified low; a missing `readOnlyHint` treated as true.
- **Quarantine:** no quarantine on drift or on disappearance; the scanner quarantining without drift; quarantine without the containment role or a reason; release without a registry approver, or by the quarantiner.
- **Contracts:** the definition pin, the read-only-hint rule, the lookup rule, the HTTP no-pin rule, the activation fingerprint check, and the v2 fingerprint ignoring the definition.
- **Discovery:** a principal declaring an MCP tool; a principal or the lease-holding scanner moving `definition_id` (after the test added for it — see below).
- **Client:** the `x-mcp-header` check; `title` or `annotations.title` kept in the fingerprinted definition; no legacy fallback; `2025-06-18` not accepted.

Survivor (equivalent): dropping the scanner's "shutdown: do not record" check. `RecordScan` runs with the same cancelled context, so its transaction cannot begin and nothing is recorded either way; the check only avoids a spurious error log.

The `definition_id` guard first survived: nothing tested that a principal cannot repoint a tool's current definition, although `eacp_app` has the column grant. `TestMCPToolsAreDiscoveredNotDeclared` now tries it as three principals and as the scanner holding the lease.

## Not done (by design)

- Calling MCP tools (`tools/call`): no worker serves protocol `mcp`, so the claim hint never offers such an action and it expires unclaimed. Execution needs an MCP connector contract and a pre-dispatch definition check (ADR-023 §1).
- stdio, `subscriptions/listen` and OAuth flows (ADR-023 §1).
