# ADR-034: The result channel

Status: Accepted (Rev 1.0, 2026-09-30). Scope: Phase 26b.
Related: ADR-001 (agents never hold credentials), ADR-003 §4 (contracts), ADR-004 (states, fencing, evidence), ADR-029 (replicas), ADR-030 (A2A delegation), ADR-032 (MCP `tools/call`).

## Context

No connector returned a tool's output. HTTP returned `external_reference`, A2A the task id, and MCP (ADR-032) `mcp:sha256:<digest of the result>`. An agent that reads data (a leave balance, a purchase order) could learn only that the call succeeded. The Agent Studio program (27a, its thin slice) needs a workflow step to read a result. The program spec (row 26b) asks for "a governed way for the calling agent to read a tool's output (bounded, retained for a limited time, never in logs or the audit journal), for HTTP, MCP and A2A alike".

Tool output is untrusted remote content and may be personal data. Keeping it is a new kind of data at rest in EACP.

**How this was decided (owner, 2026-09-30).** The owner asked for Phase 26b to run to the end without stopping at the design gates. Every choice below is therefore the most conservative option, and each is listed under Unresolved assumptions for the owner's review.

## Decision

### 1. Opt-in per contract

`eacp.tool_contracts.result_retention_seconds` (60 to 86 400; NULL keeps nothing, as before). A contract is immutable, so turning the channel on for a tool is a new contract version and a second person's activation. Governance-as-Code bundles carry the field, and drift compares it.

### 2. Only a success, recorded in its own transaction

A connector sets `worker.Result.Output` only with `Succeeded`, and `classify` drops it from every other outcome. `Store.Complete` calls `eacp.action_result_record(action, output, withheld)` after the attempt update and before the action moves to `SUCCEEDED`, and only when that is the state it is about to write. PostgreSQL checks everything: the actor is the worker holding the lease at the current generation, the action is `EXECUTING`, this generation's attempt is `succeeded` and not late, one of output or withheld reason is given, and the pinned contract has a retention (without one it stores nothing and returns false). It computes `sha256`, `bytes`, `agent_id`, `contract_id` and `expires_at`. Nothing is kept for an ambiguous, no-effect, late or kill-interrupted attempt, nor for an action reconciled or resolved by a human.

### 3. What is kept

The output as RFC 8785 JSON text (canonicalized by the worker; `IS JSON` and at most 65 536 bytes in PostgreSQL). It is withheld, with the reason kept instead, when it:

- contains a credential the worker holds, raw, canonical or JSON-escaped (`contains_credential`; checked before the size);
- is over 65 536 bytes (`too_large`);
- or does not canonicalize (`invalid_output`).

The action still succeeds. The connectors' outputs are:

- **HTTP:** the execute response's optional `result`. The response cap rises from 16 KiB to 128 KiB.
- **MCP:** the whole `CallToolResult`, so the stored `sha256` equals the reference's digest.
- **A2A:** `{"artifacts": …}` for a completed task, `{"parts": …}` for a direct reply.

### 4. Who reads it

Only an `ACTIVE` version of the action's agent reads the content, through `eacp.action_result(action)` (`SECURITY DEFINER`). A principal, a system actor, a worker, another agent and a suspended or quarantined version are refused or see nothing; an expired or pruned result is not served. `eacp_app` has `SELECT` on every column of `eacp.action_results` except `output`, and no write privilege. The API route is `GET /v1/actions/{id}/result`, for agents only:

- **200:** `{action_id, output, sha256, bytes, expires_at}`, with `Cache-Control: no-store`.
- **409:** `result_withheld` with its `reason`.
- **404:** `result_not_available` for everything else.

Operators and auditors see the metadata in `GET /v1/actions/{id}/evidence` (`result`: withheld, sha256, bytes, created, expires, pruned), never the content.

### 5. How long

A read after `expires_at` returns nothing. The sweeper's second pass lists tenants through `eacp.action_result_tenants()` and clears expired content with `eacp.action_results_prune(batch)` as the `sweeper` system actor: `output` becomes NULL and `pruned_at` is set. The metadata row stays as evidence. Replicas race harmlessly (`FOR UPDATE SKIP LOCKED`, only real clears are counted), and no loop lock is involved.

### 6. Never logged, journaled or messaged

No log line, audit event, outbox row or NATS message carries output. `eacp.action_results` has no audit trigger. The journal records the action's `SUCCEEDED` transition as before; the result row's digest is the evidence of what was kept.

## Invariants

1. **Only the lease holder's success is kept.** A stale generation, another worker, a non-success, a late attempt and an action that left `EXECUTING` are refused by PostgreSQL.
2. **Only the calling agent reads content.** `eacp_app` cannot select `output`; the read function admits only an `ACTIVE` version of the action's agent.
3. **No credential is kept.** An output containing any value the worker holds is withheld.
4. **Output reaches nothing else.** A canary in a kept output is absent from the worker log, the actions, their attempts, the journal and the outbox.
5. **Bounded in size and time.** At most 65 536 bytes; not served after its expiry; the content is cleared by the sweeper.

## Consequences

- A workflow step can read a tool's result, so the 27a thin slice can build on it.
- EACP now holds untrusted, possibly personal data at rest for at most a day per contract. PostgreSQL's own at-rest protection is the only encryption.
- A tool whose contract predates this ADR keeps nothing until a new contract version turns the channel on.
- The HTTP connector reads responses up to 128 KiB.

## Unresolved assumptions

| Assumption | Conservative choice |
|---|---|
| Whether output is kept for every tool | No: opt-in per contract, which is two-person |
| Who may read content | Only an `ACTIVE` version of the action's agent; no human through EACP (operators see metadata) |
| The same agent or the same version | The same agent, as for `GET /v1/actions/{id}`, but only an `ACTIVE` version |
| Which outcomes keep output | Only a success moving to `SUCCEEDED` in the same transaction |
| Size | 65 536 bytes of canonical JSON; a larger output is withheld, and the action still succeeds |
| Retention | 60 s to 24 h per contract; metadata stays after pruning |
| Output containing a credential | Withheld, never stored or scrubbed in place |
| Whether a read consumes the result | No: an agent may read it again until it expires (a lost response must not lose the result) |
| Journaling | Not journaled; the result row's digest is the evidence |
| Encryption beyond PostgreSQL | None in 26b |

## Out of scope

Human reads of content, the console showing results, per-field redaction (DLP), output larger than 64 KiB or streamed, application-level encryption, output for reconciled or human-resolved actions, and results of LLM gateway calls (ADR-031 keeps none).

## Verification

- Raw SQL as `eacp_app` (`internal/worker/result_schema_test.go`): `TestResultRetentionIsBoundedAndImmutable`, `TestResultContentIsNotSelectable`, `TestResultsCannotBeWrittenDirectly`, `TestOnlyTheLeaseHolderRecordsASuccess`, `TestOneResultPerAction`, `TestNoRetentionStoresNothing`, `TestRecordComputesTheDigest`, `TestOnlyTheActionsAgentReadsTheResult`, `TestPruneNeedsTheSweeper`; `internal/storage` `TestEveryTableFollowsTheRLSConventionAndCrossTenantPathsAreReviewed` reviews the table's owner scan and the four functions.
- The worker: `TestPrepareOutput`, `TestClassifyKeepsOutputOnlyForASuccess`, and through the real worker and store `TestASuccessKeepsItsOutputForTheAgent`, `TestNothingIsKeptWithoutRetention`, `TestAnAmbiguousResultKeepsNoOutput`, `TestAKilledSuccessKeepsNoOutput`, `TestACredentialInTheOutputIsWithheld`, `TestAnOversizedOutputIsWithheldAndTheActionSucceeds`, `TestAReadOnlyRetryKeepsTheSucceedingAttemptsOutput`.
- Connectors: `TestHTTPReturnsASuccessesResult`, `TestASuccessReturnsItsResultAsOutput` (MCP; the digest equals the reference), `TestExecuteNeverLeaksOutput` (only a success's `Output` carries output), `TestASuccessReturnsItsArtifactsAsOutput` (A2A).
- The API: `TestTheCallingAgentReadsItsResult`, `TestAWithheldResultSaysWhy`; the sweeper: `TestTheSweeperClearsExpiredResultsOnce`; registry and bundles: `TestContractCarriesResultRetention`, `TestAResultRetentionIsAppliedAndDoesNotDrift`.
- `test/demo` `TestSliceCDemo`: po-assistant reads the MCP tool's output, whose digest is the reference; an auditor is refused; the evidence shows metadata only.
- Everything runs with `-race`; the PostgreSQL suites run with `EACP_TEST_ADMIN_DSN` set.
