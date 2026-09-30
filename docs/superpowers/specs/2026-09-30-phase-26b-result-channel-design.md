# Phase 26b: the result channel (design)

Status: Accepted 2026-09-30; implemented as ADR-034 (the owner asked for the phase to run to the end without stopping, so every
choice below is the most conservative option and is recorded in ADR-034 for review afterwards).

## 1. Why

No connector returns a tool's output today. HTTP returns `external_reference`, A2A the task id, and MCP (Phase 26a)
`mcp:sha256:<digest>`. A workflow step that reads data ("how many leave days does this person have?"), and the
27a thin slice whose template reads one, needs the output. The program spec (section 8, row 26b) asks for "a governed
way for the calling agent to read a tool's output (bounded, retained for a limited time, never in logs or the audit
journal), for HTTP, MCP and A2A alike".

## 2. What the owner said, and what is assumed

Said (program spec, ADR-032 split): bounded; retained for a limited time; readable only by the calling agent; never in
logs or the audit journal; HTTP, MCP and A2A alike; before 27a.

Assumed (conservative, each recorded in ADR-034):

1. **Opt-in per contract.** A contract carries `result_retention_seconds` (60 to 86 400). Without it nothing is kept,
   which is today's behaviour. Turning it on is a new contract version, so it passes the registry's two-person
   activation: exposing output is a governed decision about that tool.
2. **Only a success.** Output is kept only for an attempt the worker records as `succeeded` and that moves the action
   to `SUCCEEDED` in the same transaction. Nothing is kept for an ambiguous, no-effect, late, kill-interrupted,
   reconciled or human-resolved outcome: that content is either untrusted or never seen.
3. **Only the calling agent reads content.** An authenticated version of the action's agent (the same rule as
   `GET /v1/actions/{id}` for agents). No principal reads the content through EACP: operators, auditors and admins
   see metadata (size, digest, expiry, withheld reason). PostgreSQL enforces this: `eacp_app` has no `SELECT` on the
   `output` column, and the content is read only through a `SECURITY DEFINER` function that checks the agent.
4. **Bounded.** At most 65 536 bytes of RFC 8785 JSON. A larger output is withheld (`too_large`); the action still
   succeeds.
5. **No credential ever.** An output containing any value the worker holds as a secret is withheld
   (`contains_credential`), like `scrub` today.
6. **Limited time.** Reads stop at `expires_at`. The sweeper then clears the content (`output` NULL, `pruned_at`); the
   metadata row stays as evidence.
7. **Never logged, journaled or messaged.** No log line, audit event, outbox row or NATS message carries the output.
   The action's `SUCCEEDED` transition is journaled as before; the result row carries the digest as evidence.

## 3. Design

### 3.1 Schema (migration 00026)

- `eacp.tool_contracts.result_retention_seconds integer NULL CHECK (BETWEEN 60 AND 86400)`, immutable like the rest
  of a contract.
- `eacp.action_results` (tenant-scoped, RLS convention, `FORCE`): `action_id` (PK with tenant), `agent_id`,
  `contract_id`, `lease_generation`, `output text` (RFC 8785 JSON, `IS JSON`, `octet_length <= 65536`), `withheld`
  (`too_large`, `contains_credential`, `invalid_output`), `sha256` and `bytes` computed by PostgreSQL, `created_at`,
  `expires_at`, `pruned_at`. Exactly one of `output`/`withheld` is set until pruning; after pruning neither.
- `eacp_app`: no INSERT/UPDATE/DELETE; `SELECT` on every column except `output`.
- `eacp.action_result_record(action_id, output text, withheld text) RETURNS boolean`, `SECURITY DEFINER`: the actor
  must be the worker holding the action's lease (`eacp.assert_lease_holder`); the action must be `EXECUTING` under that
  generation with this generation's attempt recorded `succeeded` and not late; the pinned contract must have a
  retention (otherwise nothing is stored and it returns false). PostgreSQL computes `sha256`, `bytes`, `agent_id`,
  `contract_id` and `expires_at`.
- `eacp.action_result(action_id) RETURNS TABLE (output, sha256, bytes, expires_at, withheld)`, `SECURITY DEFINER`:
  the actor must be an agent (`eacp.actor_context()` kind `agent`) of the action's agent; an expired or pruned row, or
  another agent's, returns no row.
- `eacp.action_results_prune(batch)` for the `sweeper` system actor, and `eacp.action_result_tenants()` listing the
  tenants with content past its expiry.

### 3.2 Worker

- `worker.Result.Output json.RawMessage`: set by a connector only with `Succeeded`; `classify` keeps it only for a
  success, `scrub` turns a credential-bearing output into `withheld = contains_credential`.
- `Job.ResultRetention` (from the pinned contract): the worker prepares output only when it is set.
- `Store.Complete` calls `eacp.action_result_record` after the attempt update and before the action moves to
  `SUCCEEDED`, only when that is the state it is about to write. The output is canonicalized (RFC 8785) in Go; a value
  that does not canonicalize is `invalid_output`; over 65 536 bytes is `too_large`.

### 3.3 Connectors

- HTTP: the execute response may carry `result` (any JSON value) beside `external_reference`. The response cap rises
  from 16 KiB to 128 KiB.
- MCP: the whole `CallToolResult` object, so the stored digest equals the reference `mcp:sha256:<hex>`.
- A2A: `{"artifacts": [...]}` for a completed task, `{"parts": [...]}` for a direct message reply.

### 3.4 API

- `GET /v1/actions/{id}/result` (agents only; `Cache-Control: no-store`): 200 `{action_id, output, sha256, bytes,
  expires_at}`; 409 `result_withheld` with `reason`; 404 `result_not_available` for everything else (no result,
  expired, pruned, not a success, another agent's action).
- `GET /v1/actions/{id}/evidence` gains `result` metadata (never content).
- Contracts in the registry API and Governance-as-Code bundles carry `result_retention_seconds`; drift compares it.

### 3.5 Sweeper

A second pass per sweep: tenants from `eacp.action_result_tenants()`, then `eacp.action_results_prune(batch)` in each.
Replicas race harmlessly (row locks, `pruned_at IS NULL`); only real clears are counted.

## 4. Tests (failing first)

- Raw SQL as `eacp_app`: no `SELECT output`; no direct writes; record refuses a stale generation, a non-worker, a
  non-success, a late attempt, a second row; a contract without retention stores nothing; read refuses a principal,
  another agent and an expired row; prune needs the sweeper; retention bounds.
- Worker integration (HTTP via fake ERP, MCP via the fake server, A2A via its fake): a success's output is readable by
  the agent and equals what the tool returned; an ambiguous outcome and a kill-interrupted success keep nothing; a
  credential in the output is withheld; an oversized output is withheld and the action still succeeds; without
  retention nothing is kept; nothing of the output reaches the worker log.
- API: the route's status codes; a principal gets 403/404; evidence shows metadata only.
- Sweeper: an expired result is cleared once across two replicas.
- Slice C demo: the agent reads the MCP tool's output.

## 5. Out of scope

Human reads of content, the console showing results, encryption beyond PostgreSQL's own at-rest protection, per-field
redaction (DLP), streaming or blobs larger than 64 KiB, and output for reconciled or human-resolved actions.
