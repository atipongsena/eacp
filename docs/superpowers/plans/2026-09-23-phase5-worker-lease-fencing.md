# Phase 5 Worker, Lease, Fencing and Dispatch Intent Implementation Plan

> **For agentic workers:** Implement the tasks in order, test first. Each task gets a failing test, a narrow implementation and a fresh `-race` run.

**Goal:** Deliver Slice A Phase 5 (MASTER_PLAN §79). That covers:
- Claiming `QUEUED` actions from PostgreSQL (`FOR UPDATE SKIP LOCKED`, FIFO).
- Heartbeats, and lease expiry and reclaim (§22).
- Generation fencing on every worker write (§23).
- The fenced dispatch intent before any external call (§23.1).
- Late-result evidence.
- Credential custody in the worker only.

The work implements ADR-004 T14, T16–T27 and the cancel requests in `EXECUTING`/`RETRY_WAIT`. Reconciliation and human resolution (T28–T37, T29a) belong to Phase 7. The HTTP connector and Fake ERP behaviours belong to Phase 6.

**Architecture:**
- `migrations/00006_execution.sql` adds lease, attempt and cancel-request columns to `eacp.actions`. It also adds an `eacp.action_attempts` table, and extends `actions_guard` with every Phase 5 move.
- `internal/worker` holds:
  - The fenced store operations: claim, heartbeat, dispatch intent, result, release.
  - The `Connector` interface, which Phase 6 implements for HTTP.
  - The tenant-namespaced, host-bound secret store.
  - The worker loop.
- The `controlplane-api` sweeper gains lease reclaim (T17, T18, T23, T24) and retry scheduling (T25–T27).

## Decisions (conservative, recorded in ADR-004 Rev 2.3)

1. **Worker actor.** `app.system_actor = 'worker'` together with `app.worker_id` and `app.lease_generation`. Every worker write names its generation, so a stale worker is rejected by PostgreSQL itself, not only by the `WHERE lease_generation = $g` clause. The worker component performs only execution moves. It never makes governance moves, and it never cancels.
2. **The claim is a hint plus CAS.** A narrow `SECURITY DEFINER` function lists the oldest `QUEUED` actions. It orders them by the time they entered `QUEUED`, and returns only actions whose connector protocol the worker implements and whose `(tenant, secret_ref, endpoint host)` it holds a credential for. The worker then claims each action in its tenant transaction with `FOR UPDATE SKIP LOCKED` and the state CAS. A worker never claims an action it can't execute, so nothing loops at the head of the queue.
3. **Dispatch intent (T16).** T16 runs in one transaction with the registry rows read `FOR SHARE`. The DB requires all of:
   - The lease is valid.
   - `attempt_count` increases by one.
   - No cancel has been requested, and `not_after` hasn't passed.
   - The capability check passes.
   - The subject is an enabled human.
   - The pinned contract is still the tool's active contract and isn't revoked.
   - The policy pointer still equals the pinned bundle and version.
   - The new `leased_until` exceeds `now()` plus the call timeout plus 1 second.

   The `action_attempts` row is inserted by the actions trigger in the same transaction, so a dispatch intent without an attempt is impossible. The call timeout is the contract's `timeout_ms` (default 30 s, capped at 5 min), and it bounds the call deadline stored on the attempt.
4. **Drift at T16.**
   - A different policy pointer means T16a: back to `AUTHORIZED`, and through the release boundary again.
   - A capability denial, a superseded or revoked pinned contract, or a disabled subject means T16b: `DENIED(revoked_before_dispatch: <reason>)`.
   - An enforced payload whose recomputed digest doesn't match the stored `enforced_digest` also means T16b (`enforced_digest_mismatch`), and raises a security alert.
5. **Results are fenced.**
   - The worker locks the action, completes its attempt row (outcome `succeeded`, `no_effect` or `ambiguous`), and in the same transaction moves the action (T19–T22a).
   - `succeeded` needs an external reference.
   - `no_effect` needs an error class listed in the pinned contract's `no_effect_errors`. Otherwise the worker classifies the result as `ambiguous`.
   - A deferred check requires every completed, non-late attempt to have moved its action out of `EXECUTING`.
6. **Late results.** An attempt completed after its action left `EXECUTING` at that generation is marked `late` and journaled as `action.late_result`, with no state change. It is evidence only, never sufficient for `FAILED`.
7. **T24 narrowed.** In Slice A, lease loss while `EXECUTING` retries (T24) only for a `READ_ONLY` contract that isn't revoked. A natively idempotent contract goes to `UNKNOWN_OUTCOME` (T23) and is reconciled first (Phase 7). EACP can't verify that the target honours the key across its deduplication window. As a result, every attempt before a `RETRY_WAIT` or re-queue either definitively had no effect or was a read. That keeps `CANCELLED`, `EXPIRED` and `FAILED` truthful for re-queued actions.
8. **Cancellation after release.**
   - `QUEUED` → T15.
   - `LEASED` → T18: a direct `CANCELLED` by the agent, the subject or an operator. The row lock and the state CAS order it against T16; T16 then fails and nothing is dispatched.
   - `EXECUTING` and `RETRY_WAIT` record a cancel request without a state change:
     - `EXECUTING`: the worker's heartbeat sees the request and cancels the call. The result becomes T22 unless a definitive result arrives, and a definitive no-effect then becomes T21, never a retry.
     - `RETRY_WAIT`: the sweeper applies T27 `FAILED(cancelled)`.
   - `CANCELLED` is never set after a dispatch intent.
9. **Retry scheduling.**
   - T20 and T22a set `next_attempt_at` to a time in (now, now + 1 h].
   - The sweeper's T25 re-queues only under the pinned policy version. T26 sends the action back to `AUTHORIZED` when the policy changed.
   - T27 fails the action when attempts are exhausted, `not_after` has passed, or a cancel was requested. There is no expiry edge from `RETRY_WAIT`, so T27 carries the `not_after` case (reason `expired`).
10. **Heartbeat.** A heartbeat extends a live lease only (`OLD.leased_until > now()`), because an expired lease belongs to the sweeper. It is not a transition and isn't journaled. It returns the cancel request.
11. **Admission** (Phase 4) now counts every released, unfinished action: `QUEUED`, `LEASED`, `EXECUTING` and `RETRY_WAIT`. Otherwise workers draining the queue would disable the limit.
12. **Credential custody.**
    - Only the execution worker may be configured with `EACP_CONNECTOR_SECRETS_FILE`. Every other service refuses to start with it.
    - Secrets are keyed by `(tenant_id, secret_ref)` and bound to an endpoint host. The worker refuses to hand a secret to an endpoint with a different host.
    - Secret values are never logged, stored or journaled, and never marshalled. Their String, GoString, LogValue and JSON forms are redacted, and the values are registered with the log redactor.

## Tasks

### Task 1: Migration 00006 (raw SQL tests first)
- [x] Tests:
  - T14 generation increment and the lease bound.
  - A heartbeat on an expired lease is rejected.
  - A stale generation is rejected for T16, T19 and the heartbeat.
  - The T16 attempt row is created, with the call deadline.
  - T16 drift: T16a and T16b.
  - Result guards (T19–T22a) against the outcome and the contract's no-effect errors.
  - The deferred result check.
  - Late results are journaled.
  - Sweeper moves T17, T18, T23, T24 and T25–T27.
  - Cancel requests.
  - The worker can't make governance moves or cancel.
  - RLS on attempts.
- [x] Implement; the Down migration restores the 00005 functions verbatim.

### Task 2: `internal/worker` store, connector, secrets
- [x] Tests:
  - Secret file validation.
  - Host binding.
  - Redaction.
  - The `AllowConnectorSecrets` config gate.
- [x] Tests for the store operations against PostgreSQL:
  - Claim FIFO and SKIP LOCKED.
  - A lease race: N workers, one winner.
  - A stale commit is rejected.

### Task 3: Worker loop
- [x] Tests with fake connectors:
  - Success.
  - No-effect with retry.
  - Ambiguous → `UNKNOWN_OUTCOME`.
  - A read-only ambiguous result retries.
  - Cancel during `EXECUTING`.
  - Drift → T16a and T16b.
  - A stale worker never dispatches a second time.
  - A late result is recorded.
  - A secret canary never appears in logs, rows or the journal.

### Task 4: Sweeper and API
- [x] Tests:
  - Lease reclaim: T17, T18, T23, T24.
  - Retry scheduling: T25, T26, T27.
  - A released action re-releases after T16a.
  - Cancel in `LEASED`, `EXECUTING` and `RETRY_WAIT`.
  - The View exposes lease and attempt facts.
  - Admission counts in-flight actions.

### Task 5: Wiring
- [x] `cmd/execution-worker` runs the loop. Phase 5 registers no protocol, so it claims nothing until Phase 6 adds HTTP.
- [x] Add the worker config.
- [x] Mount secrets only into the worker in compose.
- [x] Add a compose test that only the worker carries a secrets configuration.

### Task 6: Docs, validation, commit
- [x] Update ADR-004 Rev 2.3, ADR-003 (the secret host binding is delivered), MASTER_PLAN §79, AGENTS.md, README and the review doc.
- [x] Run the mutation checks and the full `-race` suite, then build and test the compose stack.
- [x] Commit, then stop.
