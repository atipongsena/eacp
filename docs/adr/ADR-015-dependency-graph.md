# ADR-015: Dependency graph and conservative blast radius

Status: Accepted (Rev 1.0, 2026-09-24). Scope: Slice C Phase 15 (MASTER_PLAN §35–§37, §89).
Related: ADR-003 (agent ownership, allowlists and contracts), ADR-023 (MCP discovery), ADR-001 (credentials stay in the worker).

## Context

An MCP drift or compromised system needs a fast answer to which agent versions may be affected. Before Phase 15, the registry knew agent allowlists, connectors and tools, but did not record model use, delegation or the systems behind tools. An unobserved relationship must not be interpreted as evidence that an agent is safe.

## Decision

1. PostgreSQL stores the graph. `agent_versions.active_allowlist_id` and `agent_allowlists.tool_ids` provide current Agent→Tool and Agent→MCP relationships; `tools.connector_id` provides MCP→Tool. No second copy of those edges is maintained. New, tenant-scoped `dependency_edges` record Agent→Model, Agent→MCP, Agent→Agent, and Tool→System observations. Model and system targets use stable, tenant-local names; the other targets use UUIDs validated against the tenant's registry. These observations **never grant execution capability**.
2. Each observed edge has a source, observed time, expiry and confidence (`high`, `medium`, `low`, `unknown`). A `registry_editor` inserts immutable evidence or revokes it with a reason. PostgreSQL validates roles, tenant-local endpoints, shapes and provenance, and journals both changes in the same transaction. Unknown confidence permits an unknown target. Self-delegation is rejected; a cycle across different agents is permitted and traversal terminates.
3. A tenant-scoped, read-only REPEATABLE READ transaction runs PostgreSQL recursive CTEs. A currently observed, unexpired, high-confidence path yields a **confirmed** agent version. An expired, future-dated, lower-confidence or unknown edge makes its source and every upstream agent **possible**, even when its last reported target differs from the queried target. This deliberately overreports: stale evidence cannot make the reported radius narrower. Revoked edges contribute no path. The response's `coverage: observed_only` states that an absent declaration is not proof of no dependency.
4. The operator/auditor API returns confirmed and possible agent versions, owning groups as teams (a principal owner appears separately), data sensitivity from active contracts, and the number of production actions created in the past 24 hours. That action count is not a count of workflow runs. The current registry has no workflow entity, so the Phase 15 query does not invent an affected-workflow count. `eacpctl dependency blast-radius` exposes the same query.
5. Graph reads are evidence for an operator's containment decision. They do not disable connectors, kill agents, approve actions or change the action state machine. Phase 16 owns distributed kill semantics.

## Invariants and limits

- Every new table follows tenant RLS; an edge endpoint from another tenant is rejected by the database even if a caller supplies its UUID.
- Only a principal with `registry_editor` can alter dependency evidence. The creator and revoker come from `eacp.actor()`, never caller input. The journal is appended in the same transaction.
- The query uses current allowlist pointers. A proposed, inactive allowlist does not create a confirmed path.
- An MCP server's scan result is not a dependency grant. A tool may be quarantined while its graph history still matters to blast radius.
- Models, systems and delegation are observational: EACP does not yet enforce or automatically discover them. Because no source can attest inventory completeness in Phase 15, `observed_only` must remain visible to operators. If a caller needs a provable upper bound over undeclared relationships, this phase cannot provide it.

## Unresolved assumptions

| Assumption | Conservative choice |
|---|---|
| What a stale or unknown edge can reach | Treat every upstream agent as possibly affected by any queried target. This can overreport across kinds. |
| Whether a missing model, system or delegation declaration proves absence | No. Report only observed paths and label coverage `observed_only`. |
| What counts as a production run | Do not claim a run count. Report `recent_actions_24h` from committed production actions. |
| How to identify a model or system | Tenant-local name supplied by the registry editor; no external discovery claim. |

## Verification

`internal/registry/dependency_test.go` exercises graph traversal, cycles, uncertainty, raw-SQL guards, audit and tenant isolation. `internal/api/dependency_test.go` exercises roles, record, query and revocation. `cmd/eacpctl/dependency_test.go` checks CLI requests. `internal/storage/rls_catalog_test.go` includes the new table in its convention check.
