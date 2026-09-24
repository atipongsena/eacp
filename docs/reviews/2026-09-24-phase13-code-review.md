# Phase 13 Backpressure, Bulkheads, Circuit Breakers and Retry Budgets Review

Date: 2026-09-24
Scope: Slice B Phase 13 (MASTER_PLAN §87, with §26–§30; ADR-022 Rev 1.0; §103 invariant 9 [B], and invariants 6, 8 and 17).
Review: a self-review against ADR-022, ADR-004 and the invariants below, plus mutation checks of the new tests.

## Invariants stated before the code

1. **Withhold, never decide.** Admission, bulkheads and breakers only refuse or delay work. None of them changes an outcome, moves an action that has a dispatch intent, or adds a dispatch.
2. **Invariant 9.** A failing or hanging connector cannot occupy every slot of a worker, every worker in the fleet, or the whole admission budget of a tenant.
3. **PostgreSQL decides.** An open circuit is refused by the database at T14 and T16, including raw SQL. Once an operator's disable commits, no dispatch intent for that connector commits after it.
4. **One answer to "may this retry?".** Every retry edge (T20, T22a, T25, T26, T31, T37) and every retry decision in Go asks `eacp.retry_budget_exhausted`. An exhausted budget always has a way to `FAILED` (T21, T27, T32), and T31/T32 stay mutually exclusive.
5. **Tenant isolation and audit.** Circuits are tenant rows under forced RLS; the only cross-tenant read is the reviewed claim hint. Every circuit change is journaled with its actor and a reason.

## Implemented

- **ADR-022** (new; Accepted Rev 1.0). ADR-004 gains a Phase 13 amendment; ADR-011's forward reference now points to ADR-022.
- **Migration 00013.**
  - `tool_contracts`: `max_queued`, `retry_max_elapsed_ms`, `retry_max_cost` (needs `cost_unit`).
  - `eacp.retry_budget_exhausted` (attempts, elapsed since the first dispatch intent, retry cost). `eacp.actions_guard` is re-declared from 00008 with only its four retry predicates changed; `eacp.assert_retry_possible` uses the budget too.
  - `eacp.connector_queue_full` for admission.
  - `eacp.connector_circuits`: one row per connector, created by a trigger on `eacp.connectors` and backfilled; its guard lets a worker only open it (forward, at most 10 minutes) and an operator only disable or enable it; `zz_audit` journals every change.
  - T14 (`eacp.actions_capacity_before`) refuses an open circuit with 53300; T16 (`eacp.actions_circuit_before`) reads the row `FOR SHARE` and refuses with 55000.
  - `eacp.claimable_actions(protocols, bindings, limit, skip)` omits open circuits and the worker's skip list.
- **`internal/action`.** `AdmissionError{Scope}` (matches `ErrAdmission`); admission adds the `pending` and `connector` scopes. The sweeper's T27 names the exhausted limit.
- **`internal/worker`.** The breaker (`breaker.go`), per-group bulkhead accounting and skip list, `Claim` returns the capacity group, `Intent` releases (T17) while the circuit is open, `Complete` and the reconciler ask the database for the retry budget, `TripCircuit`, and jittered default backoff for worker and reconciler.
- **`internal/registry`, `internal/api`, `eacpctl`.** Contract fields; `ConnectorCircuit` / `SetConnectorDisabled`; `GET /v1/connectors/{id}/circuit`, `POST …/circuit/disable|enable`; `eacpctl connector circuit|disable|enable`; 429 bodies carry `scope`.
- **Config.** `EACP_ACTION_MAX_PENDING_PER_TENANT`, `EACP_WORKER_GROUP_CONCURRENCY`, `EACP_WORKER_BREAKER_FAILURES`, `EACP_WORKER_BREAKER_COOLDOWN`.

## Findings from the self-review (fixed)

1. **Advisory lock replaced by a row lock.** The first design serialized T16 with a circuit change through a transaction advisory lock. A `REPEATABLE READ` T16 could then still read a stale circuit after waiting for the lock. Creating the row with the connector lets T16 read it `FOR SHARE` (ADR-004 principle 7): a disable waits for an in-flight dispatch intent, and a stale snapshot fails to serialize.
2. **Probe slot freed by the wrong call.** In `HALF_OPEN`, a call admitted before the trip that ended neutral cleared the probe flag and allowed a second concurrent probe. `start` now reports whether a call is the probe, and only the probe's own end frees the slot.
3. **Mutation found a masked lock.** Dropping `FOR SHARE` did not fail the first `REPEATABLE READ` test, because the tenant's audit-chain head already serializes that case. The test now also checks that an uncommitted dispatch intent holds the circuit row (`FOR UPDATE NOWAIT` → 55P03), which is what protects `READ COMMITTED`.
4. **Local breaker masked by the shared circuit.** Disabling the worker's own breaker check went unnoticed while the shared circuit was open. `TestLocalBreakerHoldsWithoutTheSharedCircuit` clears the shared circuit first.

## Mutation checks

Each mutation was applied alone and the named test run; all were killed.

| Mutation | Killed by |
|---|---|
| worker bulkhead never full | `TestConnectorFailureDoesNotStarveUnrelatedConnectors` |
| no shared trip written | `TestConnectorFailureDoesNotStarveUnrelatedConnectors` |
| worker's breaker never withholds a held lease | `TestLocalBreakerHoldsWithoutTheSharedCircuit` |
| skip list ignores open breakers | `TestLocalBreakerHoldsWithoutTheSharedCircuit` |
| T16 ignores the circuit | `TestOpenCircuitRefusesClaimAndDispatch` |
| T14 ignores the circuit | `TestOpenCircuitRefusesClaimAndDispatch` |
| claim hint ignores circuits | `TestOpenCircuitRefusesClaimAndDispatch` |
| T16 reads the circuit without `FOR SHARE` | `TestDisableSerializesWithAStaleDispatchIntent` |
| a worker may move `open_until` earlier | `TestCircuitChangesAreGuardedAndJournaled` |
| enabling keeps an automatic trip | `TestCircuitChangesAreGuardedAndJournaled` |
| no elapsed-time budget | `TestRetryBudgetBoundsElapsedTime` |
| no retry-cost budget | `TestRetryBudgetBoundsRetryCost` |
| connector queue never full | `TestAdmissionBoundsPendingAndConnectorQueues` |
| no pending limit | `TestAdmissionBoundsPendingAndConnectorQueues` |

## Accepted limits

- Admission counts stay advisory under concurrency (ADR-004 Rev 2.2); T14 capacity and the T14/T16 circuit checks are authoritative.
- There is no cluster-wide half-open state. After the shared circuit expires every worker may call again; the recovery burst is bounded by *failures × workers*, and a still-failing target re-trips with a doubled cooldown.
- A half-open worker may claim more than one action on that connector in one pass; all but the probe are released before their dispatch intent (T17). This costs claims, not safety.
- Reconciliation lookups are not gated by the circuit (ADR-022 §3; invariant 18).
- `retry_max_cost` uses the contract's estimated cost per call; actual cost is FinOps (Phase 18).
