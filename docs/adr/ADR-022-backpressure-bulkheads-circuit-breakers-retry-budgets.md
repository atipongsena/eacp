# ADR-022: Backpressure, bulkheads, circuit breakers and retry budgets

Status: Accepted (Rev 1.0, 2026-09-24). Scope: Slice B Phase 13 (MASTER_PLAN §26–§30, §87).
Related: ADR-004 (transition table, retry rules), ADR-011 (claim order and `max_inflight`), ADR-012 (cost), ADR-014 (NATS is a hint).

## Context

Slice A admitted work against two static counts, and Phase 12 capped the active leases of a connector group. Nothing yet stopped one failing connector from occupying a worker's every slot, stopped a fleet of workers from calling a target that is down, or bounded a retry by anything but its attempt count. MASTER_PLAN §103 invariant 9 [B] requires that **a connector failure cannot starve unrelated connector pools**.

None of these controls may weaken the ADR-004 safety rules. They decide *when* and *whether* work is admitted, claimed or dispatched. They never decide that an effect did or did not happen, and they never add a dispatch.

## Decision

### 1. Admission (T1, `controlplane-api`)

A new submission is rejected with **429**, `Retry-After` and `{"error":"admission_limit","scope":…}`, and no action is created, when any of these is reached:

| Scope | Counts | Limit |
|---|---|---|
| `global` | released, unfinished actions (`QUEUED` … `RETRY_WAIT`) in every tenant | `EACP_ACTION_MAX_QUEUED_GLOBAL` (10000) |
| `tenant` | released, unfinished actions of the tenant | `EACP_ACTION_MAX_QUEUED_PER_TENANT` (1000) |
| `pending` | unreleased, unfinished actions of the tenant (`RECEIVED`, `PENDING_APPROVAL`, `AUTHORIZED`) | `EACP_ACTION_MAX_PENDING_PER_TENANT` (1000) — new |
| `connector` | released, unfinished actions in the tool's capacity group (its `concurrency_group`, else its connector) | the smallest `max_queued` among the tool's active contract and the pinned contracts of those actions — new, optional |

The `pending` scope closes a gap: before Phase 13, actions waiting for governance or approval were not counted at all, so a PDP outage let a tenant queue without bound. The `connector` scope is backpressure per pool: a full SAP queue refuses SAP submissions and still admits e-mail.

A replay with the same idempotency key and input digest is never rejected by admission. The counts remain **advisory under concurrency**, as in ADR-004 Rev 2.2: concurrent submissions may overshoot a limit by their number. Admission controls capacity, not safety.

### 2. Worker limits and bulkheads (`execution-worker`)

- `EACP_WORKER_CONCURRENCY` (4) bounds a worker's in-flight actions, as before.
- `EACP_WORKER_GROUP_CONCURRENCY` bounds one worker's in-flight actions **per tenant and capacity group**. It defaults to half the concurrency (at least 1) and may not exceed it. A connector whose calls hang therefore holds at most that many of the worker's slots, and the rest serve other pools.
- The worker passes the groups whose bulkhead is full, and the connectors whose local breaker is open, to the claim hint as a skip list (`eacp.claimable_actions(protocols, bindings, limit, skip)`). The claim transaction returns the claimed action's connector and capacity group, so the skip list is current before the next hint.
- `max_inflight` (ADR-011) stays the cluster-wide bulkhead of a group, enforced under T14.

### 3. Circuit breakers

**Per worker.** Each worker keeps a breaker per tenant and connector: `CLOSED` → `OPEN` after `EACP_WORKER_BREAKER_FAILURES` (5) consecutive failures → `HALF_OPEN` after the cooldown → one probe call → `CLOSED` on success, or `OPEN` again. The cooldown starts at `EACP_WORKER_BREAKER_COOLDOWN` (30 s) and doubles with each consecutive trip, up to 10 minutes. An open breaker removes the connector from this worker's claims, and a lease claimed before the breaker opened is released before its dispatch intent (T17).

A **failure** is an ambiguous result, or a certified no-effect of class `connection_refused_before_send`. A success, or any other certified no-effect (the target answered deterministically), is a success. A call the worker cancelled itself (lease lost, cancel requested) counts as neither.

**Shared flag.** When a worker's breaker opens it writes `eacp.connector_circuits.open_until` for that connector (at most 10 minutes ahead, never moved earlier by a worker), journaled as `connector.circuit_opened`. An `operator` may **disable** a connector's dispatch until they enable it again, and **enabling** also clears an automatic trip. Both are journaled (`connector.circuit_disabled`, `connector.circuit_enabled`) with a required reason. While a connector is disabled or `open_until` lies ahead:

- the claim hint omits it;
- T14 refuses the claim (SQLSTATE 53300, like exhausted capacity);
- T16 refuses the dispatch intent (55000). The worker checks first and releases the lease instead (T17, reason `connector circuit open`).

Every connector has exactly one circuit row, created closed with the connector (and backfilled by migration 00013); nobody can insert or delete one. T16 reads it `FOR SHARE`, and a circuit change updates it, so the two serialize (ADR-004 principle 7): once an operator's disable commits, no dispatch intent for that connector commits after it, and a `REPEATABLE READ` dispatch that raced the change fails to serialize instead of missing it. A connector without its row is refused. The lock order becomes action → registry `FOR SHARE` → connector circuit `FOR SHARE` → budget leaf → audit chain head. A circuit change locks only its row and the audit chain head, and nothing locks a circuit row before a registry row, so the new lock adds no cycle.

After `open_until` passes, every worker may claim again; a worker whose own breaker is `HALF_OPEN` sends one probe, others resume normally, and if the target is still down they trip it again with a longer cooldown. There is no cluster-wide half-open state: the breaker limits load, and bounding the recovery burst to *failures × workers* calls is enough.

The breaker only withholds work. It never moves an action that has a dispatch intent, never changes an outcome, and does not gate reconciliation lookups (they are bounded by the reconciler's own backoff, and a reconciliation read must not be blocked by containment of new dispatch, §103 invariant 18).

### 4. Backoff with jitter

The worker's default retry backoff is still exponential (1 s doubling, at most 5 minutes) but now uses equal jitter: a delay *d* becomes a uniform value in [*d*/2, *d*]. The reconciler's backoff is jittered the same way. The database still requires every retry within the next hour. Jitter spreads retries of actions that failed together, so they do not return together.

### 5. Retry budget (§30)

A contract bounds retries by three limits, all checked in PostgreSQL through `eacp.retry_budget_exhausted(action, contract)`:

| Limit | Contract column | Exhausted when |
|---|---|---|
| attempts | `max_attempts` (existing) | `attempt_count >= max_attempts` |
| elapsed time | `retry_max_elapsed_ms` (new, 1 s – 24 h, optional) | now is at least the first dispatch intent plus the limit |
| retry cost | `retry_max_cost` (new, optional; needs `cost_unit`) | `attempt_count × action_cost` exceeds the limit, i.e. the next retry would take the cost of retries past it; an action whose cost cannot be computed is exhausted |

Every edge that grants a retry now uses the function: T20, T22a and T31 into `RETRY_WAIT`, T25/T26 out of it, and the operator retry T37 (`eacp.assert_retry_possible`). T27 and T32 accept any exhausted limit, so an exhausted budget always has a way to `FAILED`, and T31/T32 stay mutually exclusive. The worker, sweeper and reconciler ask the database the same question instead of recomputing it. The reason names the limit, for example `retry budget exhausted: elapsed`.

Retry cost is the cost of calls beyond the first, in the contract's budget unit. The ADR-012 reservation covers one execution of the action; a retry does not reserve again (ADR-012 §5). `retry_max_cost` bounds what retries may spend on top of that, for targets that charge per call.

A human-authorised retry (T37) is also bounded: once the budget is exhausted, the operator resolves the action as failed instead. That is the conservative choice, because a retry dispatches the effect again.

## Consequences

- Invariant 9 holds at three layers: admission per pool, the worker bulkhead per pool, and the circuit per connector.
- A worker needs one more write when its breaker trips, and a T16 takes one more row lock (shared).
- The counts in §1 are advisory. The capacity checks of T14 (ADR-011) and the circuit checks of T14/T16 are authoritative.
- Operators gain a per-connector dispatch switch before the Phase 16 kill switch. It stops new dispatch only; in-flight calls finish or become `UNKNOWN_OUTCOME` as before.

## Unresolved assumptions

| Question | Conservative choice |
|---|---|
| Whether a certified no-effect means the target is healthy | Yes, except `connection_refused_before_send`; the target answered. |
| Whether an operator retry may exceed the retry budget | No; resolve it as failed. |
| Whether retry cost should be the actual cost | Not known before FinOps (Phase 18); the contract's estimate per call is used. |
| Cluster-wide half-open probing | Not attempted; each worker probes once, and a failed recovery re-trips with a doubled cooldown. |
| Whether a worker may disable a connector | No; a worker only opens a circuit for at most 10 minutes. Disabling is an operator decision. |

## Verification

- `internal/worker` `TestConnectorFailureDoesNotStarveUnrelatedConnectors`: a hanging connector holds only its bulkhead, other connectors' actions succeed meanwhile, its failures trip the breaker, the shared circuit stops a second worker, and enabling it resumes dispatch.
- `internal/worker` `TestLocalBreakerHoldsWithoutTheSharedCircuit`: the per-worker breaker withholds claims and releases a held lease even after an operator clears the shared circuit.
- `internal/worker` `TestOpenCircuitRefusesClaimAndDispatch`, `TestCircuitChangesAreGuardedAndJournaled`: raw SQL as `eacp_app`.
- `internal/worker` `TestRetryBudgetBoundsElapsedTime`, `internal/action` `TestRetryBudgetBoundsRetryCost`: every retry edge honours the budget; T27 fails an exhausted one.
- `internal/worker` breaker and backoff unit tests; `internal/action` `TestAdmissionBoundsPendingAndConnectorQueues`; `internal/registry` `TestBackpressureSettingsAndConnectorCircuit`; `internal/api` `TestConnectorCircuitAPI` and the 429 scope; `cmd/eacpctl` `TestConnectorCircuitCommands`.
- Operators use `GET /v1/connectors/{id}/circuit`, `POST …/circuit/disable|enable` (reason required) or `eacpctl connector circuit|disable|enable`.
