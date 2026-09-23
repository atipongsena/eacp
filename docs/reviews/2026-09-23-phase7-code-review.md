# Phase 7 UNKNOWN_OUTCOME, Reconciliation and Human Resolution Review

Date: 2026-09-23
Scope: Slice A Phase 7 (MASTER_PLAN §81; ADR-004 T28–T37, T29a; Rev 2.5).
Review: self-review against ADR-004 and MASTER_PLAN §19–§20, with raw-SQL tests of every new guard as `eacp_app`.

## Implemented

- **Migration 00007** extends the `eacp.actions_guard` state machine with T28–T37 and T29a. It adds `reconcile_attempts`, `next_reconcile_at` and `outcome_unknown_at`, and extends the lease constraints to `RECONCILING`.
- **Reconciliation checks.** `eacp.reconciliation_checks` is immutable, with one row per reconciler lease generation. Each row is written by the lease holder in the same transaction as the move it justifies, and stamps the pinned proof standard and contract version.
- **Operator resolutions.** `eacp.action_resolutions` holds proposed, applied, withdrawn and voided resolutions. An applied resolution moves the action in the same transaction and is journaled as `action.resolution`.
- **Reconciler.** `worker.Reconciler` runs inside `execution-worker`, the only service with connector secrets. It claims due actions under a fenced lease generation (`storage.SetReconciler`) and calls `Lookup`. It then decides under the pinned proof standard:
  - found, with no contradicting reported success → `SUCCEEDED`;
  - a conflict, or a reported success contradicted by the lookup → a human;
  - `BEST_EFFORT` absence → still unknown;
  - `AUTHORITATIVE` absence, once every call has settled → a retry with the same operation key, or `FAILED` only when no retry is possible;
  - exhaustion → a human.
- **HTTP connector.** Its lookup reports a conflict for a 409 `{"error_class":"conflict"}` with no reference.
- **Sweeper.** It returns lapsed reconciler leases (T33) and retries unknown reads (T29a). Once an outcome has settled, it hands contracts without usable proof to a human (T29), and does the same for unknown outcomes older than `EACP_RECONCILE_MAX_AGE` (T34).
- **API and CLI.** `GET /v1/actions?state=` and `GET /v1/actions/{id}/evidence` are for operators and auditors. `POST /v1/actions/{id}/resolutions`, `.../{rid}/confirm` and `.../{rid}/withdraw` are for operators. `eacpctl action list|get|evidence|resolve|confirm|withdraw` wraps them.
- **Configuration.** `EACP_RECONCILE_MAX_ATTEMPTS` (default 10, range 1–50) and `EACP_RECONCILE_MAX_AGE` (default 1 h, at most 24 h).

## Review findings and fixes

1. **A human could be asked to resolve an outcome that had not settled.** The sweeper escalated contracts without proof as soon as they became unknown. An operator could then resolve `FAILED` while the call could still commit. The database now refuses T29, and T34 from `UNKNOWN_OUTCOME`, before `eacp.outcome_settled_at`, and the sweeper waits for the same time. Raw-SQL tests show both moves rejected before settling and accepted after it. The action and API tests wait for the settle time.
2. **Absence must never overrule a reported success.** A late or current attempt that reported success, with authoritative absence, now goes to a human as a conflict, not to a retry. A found record whose reference differs from a reported one is also a conflict. The database enforces this separately: T31 and T32 are refused when any attempt reported success.
3. **T32 must not fail an action that could be retried safely.** T31 and T32 are mutually exclusive in the trigger. `FAILED` applies only when a retry is impossible: a cancel was requested, the action expired, or no attempts remain.
4. **A database cap on `reconcile_attempts` could strand an action.** A sweeper T33 after a lapsed reconciler lease would violate the cap and never commit. The cap was removed. The reconciler's attempt limit and the sweeper's age backstop bound reconciliation instead.
5. **An out-of-range backoff could strand an action in `RECONCILING`.** A configured backoff of zero, a negative value or more than an hour produced a schedule the trigger refuses. The reconciler now clamps it to between 100 ms and 1 h. A regression test covers −1 s, 0 and 3 h.
6. **Reading agents `FOR SHARE` in the separation-of-duties check failed for `eacp_app`.** Agent rows are immutable, so the check reads them without the lock.
7. **Every resolution is separated from the subject, the agent's owner and the owner group, and every retry needs two operators.** ADR-004 required a second operator only for high-risk retries. Slice A has no risk-tiered resolution policy, so all retries are treated as high risk: this is the most conservative choice, and it is recorded in ADR-004 Rev 2.5.
8. **A Phase 5 test assumed an unknown outcome without proof stays `UNKNOWN_OUTCOME`.** Under Phase 7 the same sweep hands it to a human (T29). The test now checks T29, the journaled `UNKNOWN_OUTCOME` transition, no re-dispatch, and that a late result changes nothing.
9. **Fake ERP's directory fsync fails on Windows** ("Access is denied"). It is now build-tagged and skipped there. This was committed separately (`7b25dd3`).

## Validation

- `go vet ./...` and `gofmt -l .` produced no output.
- With `EACP_TEST_ADMIN_DSN` set, `go test -race -count=1 ./...` passed, including every PostgreSQL integration test.
- The flagship Fake ERP tests (`internal/worker/reconcile_integration_test.go`) use real HTTP, the worker, the sweeper and the reconciler:
  - execute-then-reset, execute-then-timeout and 5xx-after-effect → `SUCCEEDED`, with exactly one ERP record and no second dispatch;
  - delayed visibility under `BEST_EFFORT` → no retry → `NEEDS_HUMAN_RESOLUTION`, or `SUCCEEDED` once the record becomes visible;
  - a worker killed after the dispatch intent (T23), both after the effect committed (found) and before any call (authoritative absence → one retry with the same key → one record);
  - a 429 with authoritative absence → `RETRY_WAIT`, then `FAILED` once attempts run out;
  - a duplicate correlation key → conflict → a human;
  - no connector credential in checks or the journal.
- The flagship tests were run three times in a row without a failure.
- `docker compose up -d --build` passed, and `GET /readyz` reported ok with the schema at version 7. `EACP_COMPOSE_TEST=1 go test -count=1 ./test/security/` passed (8 tests).
- Operational note: `docker compose up` before `prepare_fakeerp_token.py` makes Docker create a directory in place of the token file, and Fake ERP then exits. Remove the directory and run the documented preparation step.

## Phase boundary

Phase 8 (Slice A hardening and the demo) has not started. Nothing in Phase 7 claims exactly-once execution. The guarantee is at most one effect per operation key where the target deduplicates or where authoritative evidence was obtained, and otherwise an explicit, separated human decision.
