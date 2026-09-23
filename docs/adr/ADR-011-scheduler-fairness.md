# ADR-011: PostgreSQL fair scheduler

Status: Accepted (Rev 1.0, 2026-09-24). Scope: Slice B Phase 12.

## Decision

PostgreSQL chooses the claim order. NATS remains a wake-up hint. A claim is
still the fenced T14 transition under the action row lock; no message grants
execution authority.

The scheduler uses unit-cost weighted deficit round robin at two levels:
tenant, then team. Each eligible tenant gets one claim per turn. A team's
quantum is its `groups.schedule_weight` (1–10), pinned on an action at T10.
Agents owned by a group share that team; directly owned agents each form a
weight-one team. A team's turn advances only after its quantum is spent.
Inactive teams retain no credit beyond the current quantum. Global sequence
numbers order turns; gaps from rolled-back claims are harmless. A successful
T14 advances the turn in the same transaction, before its audit append.

Within a team, the pinned contract priority (0–9) is increased by one for
each five minutes spent QUEUED, capped at nine. Within five minutes of the
action deadline it reaches priority nine immediately. Earliest `not_after`,
then queue time and action id break ties. Contract priority and group weight
are fixed on their registry rows; a new contract or a newly configured group
affects subsequent releases. Existing queued rows from earlier migrations
have weight one and priority zero.

The existing `max_inflight` contract field is enforced for a connector or
its named `concurrency_group`. Every T14 takes a transaction advisory lock
for that tenant/group and counts LEASED and EXECUTING actions. The effective
cap is the smallest declared cap among the candidate and currently active
contracts in the group. A NULL cap is unbounded when no other member sets a
cap. Expired leases still count until the sweeper moves them: capacity must
not assume an external call has stopped. The SQL hint also hides a full
group, but the trigger is the authoritative check, including for raw SQL.
Every successful T14 also writes its tenant turn row, so a stale
`REPEATABLE READ` snapshot conflicts with a later claim and cannot commit an
over-capacity lease.

## Consequences and limits

- The read-only candidate function returns only tenant and action ids across
  RLS. The worker re-queries after each successful claim so its next pick
  reflects the committed turn. Concurrent workers may have stale hints and
  produce a bounded short-term deviation; the database capacity guard still
  prevents overclaiming.
- Team weight is set when an admin creates a group via `schedule_weight`.
  Contract priority is set by a registry editor in a proposed contract and
  takes effect only after the existing two-person activation. Both default
  to the conservative value (weight one, priority zero).
- This limits active leases in a connector group. It does not promise a
  bounded count of uncertain external effects; UNKNOWN_OUTCOME and
  reconciliation retain the ADR-004 semantics. Phase 13 owns broader
  bulkheads, worker/global limits and backpressure.
- The claim hint performs capacity checks over active rows. The active
  lease index and bounded candidate list keep it practical for the initial
  release; benchmark results are recorded with Phase 12 validation.

## Unresolved assumptions

| Question | Conservative choice |
|---|---|
| Work unit cost across tools | One claim consumes one unit; do not infer cost from payload or budget. |
| Tenant weights | Equal weight one until an explicit administrative policy is designed. |
| Stale hint under concurrent workers | Recheck capacity and action state under T14; never treat a hint as authority. |

## Verification

`internal/worker/scheduler_test.go` covers a large-team backlog against a
small team, weighted turns, priority and aging, raw SQL capacity enforcement,
concurrent claims and a stale `REPEATABLE READ` claim.
`internal/storage/rls_catalog_test.go` reviews the new tables' RLS policies.
Run the PostgreSQL integration suite with `-race`.
