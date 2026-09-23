# Phase 5 Worker, Lease, Fencing and Dispatch Intent Review

Date: 2026-09-23
Scope: Slice A Phase 5 (MASTER_PLAN §79). This covers:
- Migration 00006.
- `internal/worker` (store, secrets, connector interface, worker loop).
- Sweeper lease reclaim and retry scheduling in `internal/action`.
- Cancel requests after release.
- Config, service and compose wiring for `execution-worker`.

## Delivered

- **Lease and attempt schema.** `eacp.actions` gains lease generation, worker, lease expiry, heartbeat, attempt count, dispatch intent, retry schedule, external reference and cancel-request columns, with CHECK constraints tying each to its states. `eacp.action_attempts` is tenant-scoped under RLS: one row per dispatch intent, unique per (action, generation), insert only through T16, and completion once only by its own worker and generation.
- **The worker actor.** `storage.SetWorker` binds `app.system_actor = 'worker'`, the worker id and the lease generation. `eacp.assert_lease_holder` checks all three on every heartbeat, dispatch intent, result and voluntary release. A stale worker is rejected by PostgreSQL, including through raw SQL as `eacp_app`.
- **Claim (T14).** `eacp.claimable_actions` is a FIFO hint filtered by protocol and credential. The claim itself is `FOR UPDATE SKIP LOCKED` plus a state CAS that takes the next generation.
- **Heartbeat.** A heartbeat extends only a live lease. It is not journaled, and it returns the cancel request.
- **Dispatch intent (T16).** It is its own committed transaction before any call. It re-checks drift (T16a/T16b), requires the lease to outlive the call's deadline, and inserts the attempt row in the same transaction. The worker also recomputes the enforced digest (JCS) and denies on a mismatch, raising an alert.
- **Fenced results (T19–T22a).** They are committed with the attempt's outcome in one transaction, with a deferred check that a completed attempt moved its action. Classification is conservative: a success needs a reference, and a no-effect needs a certified error class. Anything else, including a panic, is ambiguous. Late results are marked `late` and journaled as `action.late_result`, with no state change.
- **The sweeper.**
  - Lease reclaim: T17 re-queues an undispatched lease, T23 moves a lapsed write to `UNKNOWN_OUTCOME`, and T24 retries a lapsed read. T24 is narrowed to READ_ONLY in Slice A.
  - Retry scheduling: T25 under the pinned policy, T26 back to `AUTHORIZED` when the policy moved, and T27 on exhaustion, expiry or cancel.
- **Cancellation after release.** `LEASED` is cancelled directly (T18). `EXECUTING` and `RETRY_WAIT` record a cancel request (202). The heartbeat cancels an in-flight call, and `CANCELLED` is never set after a dispatch intent.
- **Admission** counts every released, unfinished action.
- **Credential custody.**
  - Secrets are keyed by (tenant, secret_ref), bound to one `host:port`, strictly validated, and redacted in every format.
  - Only `execution-worker` accepts `EACP_CONNECTOR_SECRETS_FILE`; every other service refuses to start.
  - Compose mounts the file into the worker only, and loaded values are registered with the log redactor.
- **`cmd/execution-worker`** runs the loop as a tracked background task. No protocol is registered until Phase 6.

## Review findings and fixes

1. **Secret echoed by a connector.** The canary test showed that a connector echoing its credential in an error class would store it in `action_attempts` and the journal, because the class matched the permitted character set. The worker now drops any connector-returned field that contains a loaded secret. A success whose reference is dropped becomes ambiguous.
2. **Actor check for agents.** `comp NOT IN (...)` evaluated to NULL for agent actors and let an agent pass the T17/T18 component check. It now uses `COALESCE(comp, '')`. Raw-SQL tests cover agents attempting worker and sweeper moves.
3. **Formatter leak.** `fmt.Sprintf("%+v", store)` could print the secret map's unexported values. `SecretStore` now implements `fmt.Formatter`.
4. **Goose parsing.** A comment line ending in `;` outside a statement block split the Up section, and PL/pgSQL `IF` stopped at a `CASE ... THEN` inside its condition. The comment was reworded, and the outcome is computed into a variable first.
5. **Retry-bound test.** A schema test scheduled a retry 1 hour ahead, which is outside the database's bound (now, now + 1 hour]. It now uses 30 minutes.
6. **Worker loop could hang at shutdown.** In `Run`, each finished job sent on the buffered `freed` channel, and the main loop drains it only when idle. Under sustained load more than `Concurrency` completions could accumulate, blocking goroutines that `wg.Wait()` then waited on forever at shutdown. The send is now non-blocking.
7. **Down-migration drift was only checked by hand.** A new test, `TestEveryDownMigrationRestoresThePreviousSchema`, compares the full catalog after `up N → down N` with version N−1. It covers functions (with ACLs), columns, constraints, indexes, triggers, policies, RLS flags and grants. It covers migrations 2–6. A mutated function restore in 00006's Down was detected.
8. **Mutation check: claim generations.** A self-consistent claim that skipped a generation, or reused the previous one after a release, went undetected. New raw-SQL cases show both are refused.
9. **Mutation check: T14 and T16 after `not_after`, and T16 on a lapsed lease.** A lease can lapse, or an action can expire, between claim and dispatch intent. New tests (using the schema owner to shorten `not_after`) show the claim and the dispatch intent are refused.
10. **Mutation check: late-result forgery.** The only test of another worker completing an attempt also moved the action, which the action guard rejected anyway. After the action has left `EXECUTING`, a completion needs no transition. A new test shows that another worker, or the same worker at another generation, can't write late evidence into an attempt.
11. **Mutation check: sweeper-only moves.** For a worker, T23 and T24 fall through to result moves and fail. But without the sweeper-only rule, an agent could re-queue or fail its own due retry (T25, T27). A new test covers the agent.
12. **Mutation check: an ambiguous multi-attempt write.** Every write contract in the classification test had one attempt, so removing the READ_ONLY condition from the worker's T22a choice went undetected. The database would have refused T22a, but the rolled-back transaction would also have lost the attempt's outcome. A new test shows an ambiguous idempotent write (three attempts) goes straight to `UNKNOWN_OUTCOME` with the outcome recorded.
13. **Mutation check: shutdown before intent.** A new test shows a worker that is stopping releases its undispatched lease (T17) instead of dispatching.

## Mutation checks

Each mutation removed one guard and was run against the relevant packages: 32 in the first pass, then 11 re-runs.

- **Detected in the first pass (21):**
  - The lease-holder worker id and generation.
  - The T16 drift and lease-outlives-call checks.
  - Heartbeat on a lapsed lease.
  - Result outcome matching.
  - T22a, T23 and T24 guards.
  - T25 not due.
  - Attempt insert-only-by-intent and complete-once.
  - Certified no-effect, and the deferred result check.
  - The system never recording a cancel request in `LEASED`.
  - Secret scrubbing.
  - "A success needs a reference".
  - The heartbeat's cancellation.
  - "A cancel request stops retries".
  - Sweeper T24 read-only.
  - Admission counting in-flight actions.
- **False kills, re-run with valid mutations and then detected (3):**
  - T25/T26 policy: a multi-line `RAISE` left a syntax error.
  - The worker's digest check and the certified-class check: removing the call left an unused import.
- **Survivors fixed with new tests, then detected (7):** findings 8–13 above. These cover the claim generation, the T16 lease, T16 `not_after`, the attempt's own worker, sweeper-only moves, the ambiguous write retry and shutdown. The re-run also added a new T14 `not_after` mutation, which was detected.
- **Equivalent (2):**
  - "Only a worker performs %": for a non-worker, `actor_context()` has no worker, so `assert_lease_holder` and T14's worker check already refuse every such move.
  - The cancel-request half of the T16 check: a cancel request can't exist in `LEASED`, because `RETRY_WAIT` with a request only fails (T27). Both checks are kept as defence in depth.

Finding 6 (the blocking `freed` send) is a scheduling race that tests can't trigger deterministically. `TestRunLoopStopsUnderLoad` covers the loop's stop under many completions.

## Validation

- `gofmt -l .`, `go vet ./...` and `git diff --check` produced no output.
- `go test -race -count=1 ./...` with `EACP_TEST_ADMIN_DSN` passed. That includes the PostgreSQL integration tests, the migration round trip and the new down-migration catalog check for 2–6.
- `docker compose up -d --build`:
  - `GET /readyz` reports database, role safety and schema version as ok.
  - `execution-worker` logs `worker ready` with `"bindings":1`, `"protocols":0` and a redacted secrets path.
  - `controlplane-api` refuses to start with `EACP_CONNECTOR_SECRETS_FILE` set.
- `EACP_COMPOSE_TEST=1 go test -count=1 ./test/security/` passed, including `TestOnlyTheWorkerHoldsConnectorSecrets`.

## Phase boundary and remaining risks

- **No connector protocol is registered.** The deployed worker claims nothing until Phase 6 adds the HTTP connector and the Fake ERP. The loop, fencing and custody are exercised end to end with fake connectors in `internal/worker/worker_test.go`.
- **`UNKNOWN_OUTCOME` is a holding state until Phase 7.** Reconciliation (T28–T34) and human resolution (T35–T37) are not implemented, so ambiguous writes stay there visibly. Nothing retries them automatically.
- **T24 is narrowed to READ_ONLY.** Natively idempotent contracts that lose their lease go to `UNKNOWN_OUTCOME`. This is conservative; Phase 7 reconciliation will decide their retries.
- **Outbox publishing** is still open. Workers poll PostgreSQL, which is the authority, so the outbox is only a latency hint.
- **The claim hint scans across tenants** through a `SECURITY DEFINER` function that returns only (tenant, action) identifiers. It reads nothing else, and the claim itself runs under the action's tenant RLS.
- **Clock.** Lease and retry times use the database clock throughout. The worker's call deadline uses its own clock, but the lease is sized to outlive that deadline plus the lease duration, so drift within that margin cannot cause a second dispatch: a lapsed lease after a dispatch intent goes to `UNKNOWN_OUTCOME`, never to a re-dispatch of a write.
- This phase makes no exactly-once claim. For a non-idempotent write it guarantees at most one dispatch per action by EACP (MASTER_PLAN §21), and any ambiguity is surfaced as `UNKNOWN_OUTCOME`.
