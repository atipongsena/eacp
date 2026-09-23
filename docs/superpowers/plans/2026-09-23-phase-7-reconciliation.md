# Phase 7 UNKNOWN_OUTCOME, Reconciliation and Human Resolution Plan

> Source requirements: `docs/MASTER_PLAN.md` §18–21, §57, §81, §103; ADR-004 T28–T37 and T29a.

**Goal:** Resolve every `UNKNOWN_OUTCOME` with evidence, never with a guess. A reconciler holding a fenced lease looks the operation key up under the pinned contract's proof standard. Anything it cannot prove goes to `NEEDS_HUMAN_RESOLUTION`, and only authenticated operators can resolve it.

## Invariants (each has a test)

1. **Only proof moves an unknown outcome.** `RECONCILING → SUCCEEDED` (T30) needs a recorded `found` check with an external reference in the same transaction. A late worker success with a different reference is a conflict.
2. **"Not found" never authorizes a retry or `FAILED` unless the contract is `AUTHORITATIVE`** (invariant 13). The database enforces this for T31/T32:
   - an `absent` check;
   - an unrevoked `authoritative` contract with a strongly consistent lookup by operation key;
   - no attempt that reported success, late or not;
   - every attempt's call deadline has settled.
3. **`BEST_EFFORT` absence and lookup errors mean still unknown** (T33). This increments `reconcile_attempts` with backoff and never re-dispatches.
4. **The reconciler lease is fenced like a worker lease.** The claim takes the next `lease_generation` (T28). Every reconciler write names the reconciler and its generation, so a stale reconciler, a stale worker or anyone else is rejected by PostgreSQL. A lapsed reconciler lease is T33, applied by the sweeper.
5. **Exhaustion and missing proof go to a human, never to `FAILED`.**
   - T29: proof `none`, no lookup, or a revoked contract.
   - T34: a conflict, attempts or time exhausted.
   - T29a: an unrevoked READ_ONLY contract is retried instead.
6. **Human resolution is a privileged, journaled, two-person-for-retry operation** (T35–T37). An operator must record a reason; `SUCCEEDED` also needs evidence and an external reference, and `FAILED` needs evidence. The operator can't be the action's subject, the agent's owner or a member of the owner group. A retry needs a second, distinct operator to confirm it. A retry also needs attempts left, a live `not_after` and no cancel request, and it reuses the same operation key.
7. **`NEEDS_HUMAN_RESOLUTION` has no automated exit.** The sweeper, reconciler, worker and agents cannot move it.
8. **Credentials stay in the execution worker.** The reconciler runs in `execution-worker`. Only the reconciler performs lookups, with the same host-bound secrets.

## Tasks

1. Migration `00007_reconciliation.sql`:
   - the `reconciler` actor;
   - reconcile columns;
   - the `reconciliation_checks` and `action_resolutions` tables;
   - guards for T28–T37 and T29a;
   - the `reconcilable_actions` claim hint;
   - a verbatim Down.

   Raw-SQL schema tests as `eacp_app` come first.
2. `storage.SetReconciler`; `LookupConflict` in the HTTP connector (409 `conflict`).
3. `worker.Reconciler`:
   - claim, lookup inside the lease, a pure `decide` function, and a fenced commit of the check and transition;
   - backoff and attempt/age limits;
   - wiring in `cmd/execution-worker`.
4. Sweeper: T29, T29a, T33 on a lapsed reconciler lease, and T34 as the age backstop.
5. Engine and API:
   - `GET /v1/actions?state=`, `GET /v1/actions/{id}/evidence`;
   - `POST /v1/actions/{id}/resolutions`, `.../{rid}/confirm`, `.../{rid}/withdraw`;
   - `eacpctl action list|evidence|resolve|confirm|withdraw`.
6. Flagship tests with real HTTP and Fake ERP:
   - lost response → `SUCCEEDED`;
   - delayed visibility → no retry, then `NEEDS_HUMAN_RESOLUTION` or a later `SUCCEEDED`;
   - worker killed in `EXECUTING` → no re-dispatch, reconciled; exactly one ERP record;
   - every §19 cause → `UNKNOWN_OUTCOME`.
7. Documentation: ADR-004 Rev 2.5, MASTER_PLAN §81 status, AGENTS.md, README, and the Phase 7 review.
