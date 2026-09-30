# Phase 26b Result Channel Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the calling agent read a successful tool call's output (HTTP, MCP, A2A), bounded, time-limited, opt-in per contract, never logged or journaled.

**Architecture:** A new tenant table `eacp.action_results` written only by the lease-holding worker inside `Store.Complete` through a `SECURITY DEFINER` function, read only by the action's agent through another; `eacp_app` cannot select the content column. Connectors return `Result.Output`; the sweeper clears expired content.

**Tech Stack:** Go 1.25, pgx v5, PostgreSQL 18, goose migrations.

**Spec:** `docs/superpowers/specs/2026-09-30-phase-26b-result-channel-design.md`

## Global Constraints

- `result_retention_seconds` NULL (off) or 60..86400; immutable (only `revoked_at`, `revoke_reason` are updatable).
- Output is RFC 8785 JSON text, at most 65 536 bytes; withheld reasons exactly `too_large`, `contains_credential`, `invalid_output`.
- Output kept only when the action moves to `SUCCEEDED` in that `Complete` transaction.
- Content readable only by an authenticated version of the action's agent; principals see metadata only.
- No log line, audit event, outbox row or NATS message carries output. `eacp.action_results` gets no `zz_audit` trigger.
- PostgreSQL computes `sha256`, `bytes`, `agent_id`, `contract_id`, `expires_at`.
- TDD, `go test -race`, PostgreSQL tests with `EACP_TEST_ADMIN_DSN` (skipped is not passed); full suite with `-timeout 45m`.

## Review Focus

- An MCP result whose stored digest must equal the `mcp:sha256:` reference: Task 4 asserts `sha256` == reference hex.
- A success turned `kill_interrupted` inside `Complete`: Task 3 asserts no row.
- A READ_ONLY retry where attempt 1 was ambiguous and attempt 2 succeeds: Task 3 asserts one row, from attempt 2.
- An agent version of another agent in the same tenant: Task 1 raw SQL and Task 5 API assert no content.
- Output containing a minted OAuth token (a value only in `secret.values()`): Task 3 asserts `contains_credential`.

---

### Task 1: Migration 00026 and its raw-SQL guards

**Files:**
- Create: `migrations/00026_result_channel.sql`
- Create: `internal/action/result_schema_test.go`
- Modify: `internal/storage/rls_catalog_test.go`

**Interfaces:**
- Produces: `eacp.tool_contracts.result_retention_seconds`; table `eacp.action_results(tenant_id, action_id, agent_id, contract_id, lease_generation, output, withheld, sha256, bytes, created_at, expires_at, pruned_at)`; `eacp.action_result_record(p_action uuid, p_output text, p_withheld text) RETURNS boolean`; `eacp.action_result(p_action uuid) RETURNS TABLE(output text, sha256 text, bytes integer, expires_at timestamptz, withheld text)`; `eacp.action_results_prune(p_batch integer) RETURNS integer`; `eacp.action_result_tenants() RETURNS SETOF uuid`.

- [ ] Step 1: tests (raw SQL as `eacp_app`, fixture from existing action schema tests): `TestResultRetentionIsBoundedAndImmutable` (59 and 86401 fail 23514; UPDATE fails 42501), `TestResultContentIsNotSelectable` (`SELECT output` → 42501; metadata columns select), `TestResultsCannotBeWrittenDirectly`, `TestOnlyTheLeaseHolderRecordsASuccess` (non-worker, wrong generation → 42501; attempt not `succeeded` or late, action not EXECUTING → 55000; second call → 23505), `TestNoRetentionStoresNothing` (returns false, no row), `TestRecordComputesTheDigest` (sha256 = encode(sha256(output)), bytes, expires_at = created_at + retention), `TestOnlyTheActionsAgentReadsTheResult` (principal → 42501; other agent → no row; expired → no row), `TestPruneNeedsTheSweeper` (non-sweeper 42501; clears output only after expiry, sets pruned_at, counts).
- [ ] Step 2: run, expect failures (function/column missing).
- [ ] Step 3: write the migration: column + CHECK; table with RLS convention (`ENABLE`, `FORCE`, `tenant_isolation`), FK `(tenant_id, action_id)` → actions, CHECKs (`output IS JSON`, `octet_length(output) <= 65536`, withheld enum, `num_nonnulls(output, withheld) = 1 OR pruned_at IS NOT NULL`); `REVOKE ALL` then `GRANT SELECT (every column except output)` to `eacp_app`; the four functions `SECURITY DEFINER SET search_path = pg_catalog, eacp`, `REVOKE ALL ... FROM PUBLIC`, `GRANT EXECUTE ... TO eacp_app`. Record uses `eacp.actor_context()` + `eacp.assert_lease_holder`. Read uses actor kind `agent` and joins `eacp.agent_versions` for the agent id. Prune requires `eacp.current_system_actor() = 'sweeper'`, `FOR UPDATE SKIP LOCKED`. Down drops all.
- [ ] Step 4: add the table and functions to `rls_catalog_test.go`; run the package tests, expect PASS.
- [ ] Step 5: commit `feat(db): the result channel table and its guards (ADR-034)`.

### Task 2: Contracts carry the retention

**Files:** `internal/registry/connectors.go`, `internal/registry/tx.go`, `internal/bundle/state.go`, their tests.

**Interfaces:** `registry.Contract.ResultRetentionSeconds int \`json:"result_retention_seconds,omitempty"\``.

- [ ] Step 1: tests `TestContractCarriesResultRetention` (registry round trip via API JSON) and a bundle drift test that a changed retention is a change.
- [ ] Step 2: fail; Step 3: insert `nullInt(c.ResultRetentionSeconds)`, bundle state selects `COALESCE(ct.result_retention_seconds, 0)`; Step 4: pass; Step 5: commit `feat(registry): contracts carry a result retention`.

### Task 3: The worker keeps a success's output

**Files:** `internal/worker/connector.go`, `internal/worker/worker.go`, `internal/worker/store.go`, create `internal/worker/result_test.go`, `internal/worker/result_integration_test.go`.

**Interfaces:**
- `Result.Output json.RawMessage` (connectors set it only with `Succeeded`).
- `Job.ResultRetention int` (0 = off).
- `func prepareOutput(out json.RawMessage, secrets []string) (text string, withheld string)` in `worker.go`.
- `Store.Complete` records via `eacp.action_result_record` when `c.State == "SUCCEEDED"` and `r.Output != nil || withheld != ""`, after the attempt UPDATE and before the actions UPDATE; the prepared pair travels in `Result` as unexported fields set by the worker.

- [ ] Step 1: unit tests for `prepareOutput` (canonicalizes; >65536 → too_large; invalid → invalid_output; contains secret → contains_credential) and `classify` dropping Output unless success. Integration tests with a scripted connector: `TestASuccessKeepsItsOutputForTheAgent`, `TestNothingIsKeptWithoutRetention`, `TestAnAmbiguousResultKeepsNoOutput`, `TestAKilledSuccessKeepsNoOutput`, `TestACredentialInTheOutputIsWithheld` (minted value), `TestAnOversizedOutputIsWithheldAndTheActionSucceeds`, `TestAReadOnlyRetryKeepsTheSucceedingAttemptsOutput`, `TestTheOutputNeverReachesTheLog`.
- [ ] Step 2: fail; Step 3: implement; Step 4: pass with `-race`; Step 5: commit `feat(worker): keep a successful call's output for the agent (ADR-034)`.

### Task 4: Connectors return output

**Files:** `internal/connector/http.go`, `internal/connector/mcp/execute.go`, `internal/connector/a2a/execute.go`, `internal/fakeerp/erp.go`, tests.

- [ ] Step 1: tests: HTTP `result` passes through on success and is dropped on failure, a 100 KiB response is read; MCP output equals the result object and its sha256 equals the reference; A2A `{"artifacts":...}` for a completed task and `{"parts":...}` for a message; fake ERP create_po returns `result` `{"external_reference", "status":"created"}`.
- [ ] Step 2: fail; Step 3: implement (`maxResponseBytes = 128 << 10`); Step 4: pass; Step 5: commit `feat(connector): HTTP, MCP and A2A return a success's output`.

### Task 5: The API reads it

**Files:** `internal/action/result.go` (new), `internal/action/evidence.go`, `internal/api/actions.go`, `internal/api/api.go`, tests.

**Interfaces:** `func (e *Engine) Result(ctx, a Actor, id uuid.UUID) (ResultView, error)`; `ResultView{ActionID, Output json.RawMessage, SHA256, Bytes, ExpiresAt}`; errors `ErrResultWithheld{Reason}`, `ErrNotFound`; `Evidence.Result *ResultMeta`.

- [ ] Step 1: tests: `GET /v1/actions/{id}/result` 200 body and `Cache-Control: no-store`; 409 `result_withheld` with reason; 404 `result_not_available` for no row, another agent and expiry; a principal key is refused; evidence carries metadata without `output`.
- [ ] Step 2–5: implement, pass, commit `feat(api): the calling agent reads its result (ADR-034)`.

### Task 6: The sweeper clears expired content

**Files:** `internal/action/sweeper.go`, test.

- [ ] Step 1: `TestTheSweeperClearsExpiredResultsOnce` (two sweepers concurrently; `Stats.Pruned` sums to the expired count; unexpired kept).
- [ ] Step 2–5: implement the pass after the tenant loop; commit `feat(action): the sweeper clears expired results`.

### Task 7: Demo and documents

**Files:** `test/demo/slice_c_test.go`, `docs/adr/ADR-034-result-channel.md`, `docs/adr/README.md`, ADR-032/ADR-030 notes, `AGENTS.md`, `docs/INVARIANTS.md`, README/FEATURES/DEMO/THREAT_MODEL pairs, `CHANGELOG.md`, `docs/MASTER_PLAN.md`, program spec row 26b.

- [ ] Step 1: the Slice C demo's MCP contract sets a retention and the agent reads the tool's output (digest equals the reference).
- [ ] Step 2: docs; INVARIANTS: result tests join 1, 8 and 11.
- [ ] Step 3: full suite `go vet ./... && go test -race -timeout 45m ./...`, the demo, the opensource guards; commit `docs: Phase 26b, the result channel (ADR-034)`.
