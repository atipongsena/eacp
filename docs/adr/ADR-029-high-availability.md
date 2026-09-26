# ADR-029: High availability — replicas without a leader

Status: Accepted (Rev 1.0, 2026-09-26). Scope: Phase 23a (MASTER_PLAN §95). Phase 23b (Helm, NetworkPolicies,
disruption budgets, autoscaling, a real cluster run) extends this ADR.
Related: ADR-004 (leases, fencing, unknown outcomes), ADR-011 (scheduler), ADR-014 (PostgreSQL is the authority,
NATS carries signals), ADR-022 (bulkheads, circuits), ADR-025/-027/-018 (the FinOps, incident and release
evaluators).

## Context

§95 asks for multiple API nodes, multiple scheduler nodes, worker autoscaling, pod disruption handling and
"leader election where required". Every EACP authority is already a PostgreSQL row:

- The incident and FinOps evaluators insert with `ON CONFLICT DO NOTHING`; the release evaluator locks each
  `CANARY` release `FOR UPDATE`.
- The sweeper re-reads each action `FOR UPDATE` in its own transaction and moves it by compare-and-set.
- The outbox relay publishes with `FOR UPDATE SKIP LOCKED`.
- Workers claim through the fair scheduler in PostgreSQL (T14), every worker write is fenced by the lease
  generation, and `max_inflight` is the cluster-wide bulkhead (ADR-022 §2). The worker id defaults to the host
  name (the pod name in Kubernetes).
- On SIGTERM a worker stops claiming, finishes in-flight calls and releases undispatched leases; a process
  killed mid-call leaves an expiring lease that ends in `UNKNOWN_OUTCOME` and reconciliation (ADR-004).

So no leader is required for correctness. What was missing: every API replica ran every evaluator for every
tenant (N−1 of them wasted, all queueing on the tenant's audit chain head); the sweeper failed a tenant's pass
when another replica had moved an action it had selected, and over-counted its moves; the listener closed on
SIGTERM before a load balancer stopped routing to the pod; and nothing proved the whole under several replicas.

## Decision

### 1. No leader; a per-tenant loop lock only saves work

- There is no leader election: no Kubernetes Lease, no client-go, no session-held lock. The binaries run the
  same under compose, systemd or Kubernetes.
- `storage.TryLoopLock(ctx, tx, loop)` runs
  `pg_try_advisory_xact_lock(hashtextextended('eacp.loop:' || loop || ':' || tenant, 0))` in the tenant
  transaction. Loop names match `^[a-z][a-z0-9_]{0,31}$`; outside a tenant transaction it errors.
- The incident, FinOps and release evaluators take it (`incident`, `finops`, `release`) right after binding
  their system actor. When another replica holds it, the evaluation returns `(0, nil)` and writes nothing; the
  next interval picks up whatever it missed.
- The lock is transaction-scoped: it ends at commit or rollback, a crashed replica never holds it, and it works
  behind a transaction-mode pooler.
- It decides nothing. Each evaluator keeps a test showing the outcome is the same when evaluations run in
  parallel without it (`TestConcurrent…EvaluationsActOnce`, `TestConcurrentReleaseEvaluationsRollBackOnce`).

### 2. Loops that move rows rely on row locks

The sweeper and the outbox pruner take no loop lock. Each sweeper step is its own transaction that locks the
action and re-checks it; when another replica moved it first the step does nothing.

- The re-check treats a lease or retry time cleared by that move as "not due"
  (`COALESCE(leased_until <= now(), false)`, the same for `next_attempt_at`). Before, the NULL failed the
  tenant's whole pass.
- `Stats.Expired`, `Reclaimed`, `Retried` and `Escalated` count only the actions the pass moved; `Advanced`
  counts attempts.

### 3. Drain on shutdown

`service.Deps.ServeOn` runs this sequence when the signal context ends:

1. `/readyz` fails with the check `draining` (it exists for every service, with or without a database);
   `/healthz` stays 200. Keep-alives are switched off, so every response now says `Connection: close` and
   clients move to other pods before the shutdown closes their connections.
2. Background loops are cancelled at once (the worker stops claiming; in-flight calls still finish).
3. The listener keeps serving for `EACP_SHUTDOWN_DELAY` (default `0s`, `0s`–`60s`).
4. The graceful HTTP shutdown waits up to `EACP_SHUTDOWN_TIMEOUT`; `stop` then waits for the loops and closes
   the pool.

A listener failure before any signal returns at once.

### 4. Proof

`internal/worker` `TestReplicasShareTheWorkAndSurviveLosingOne` runs three API loop sets (sweeper, the three
evaluators, the outbox pruner) and three workers (worker and reconciler), each on its own pool, over one
database, a real Fake ERP and the real HTTP connector. One API replica and one worker stop a third of the way
through 30 submissions across the Fake ERP failure scenarios. Every action ends `SUCCEEDED` with one ERP record
per operation key (effectively-once where reconcilable, MASTER_PLAN §21), a standing kill opens one incident
although three evaluators saw it, no loop logs an error, more than one worker executed actions, and the audit
chain verifies. The stopped worker must have made attempts before it stopped, so losing it is tested, not assumed. Its workers' breakers are set out of reach: most scenarios fail at the ERP on purpose, and the
breaker is ADR-022's subject.

The test found the sweeper's NULL re-check (§2); `internal/action` `TestTwoSweepersMoveEachActionOnceAndCountIt`,
`TestTwoSweepersReclaimEachLapsedLeaseOnce` and `TestTwoSweepersRetryEachActionOnce` pin it with two sweepers
that both select the same actions before either moves them.

## Consequences

- Scaling the API or the workers needs no configuration beyond a unique worker id (the default host name).
- Each tenant's evaluation runs once per interval across replicas; a skipped tenant waits at most one interval.
- Compose behaviour is unchanged (delay `0s`). In Kubernetes (23b), `terminationGracePeriodSeconds` must exceed
  `EACP_SHUTDOWN_DELAY` + `EACP_SHUTDOWN_TIMEOUT` + the longest connector call timeout, or a SIGKILL turns an
  in-flight call into an unknown outcome (still safe, but slower to settle).
- Evaluator and sweeper intervals are per replica, so N replicas look at the database N times per interval;
  the loop lock makes the extra looks cheap, not absent.
- The sweeper's advance step (T2a retries and releases after approval) calls the PDP with no transaction
  open (ADR-005 §5a), so no transaction-scoped lock can cover it: every replica may call the PDP for the same
  pending action each interval, and PDP load grows with the number of API replicas, most during a PDP outage.
  The PDP is stateless and the release is still decided once, under the action's row lock; 23b sizes the PDP
  for it.

## Unresolved assumptions (conservative choices)

| Assumption | Choice |
|---|---|
| Leader election | None. PostgreSQL arbitrates; per-tenant try-locks only save work. |
| Lock scope | Transaction-scoped, per loop and tenant; never session-scoped (pooler-safe, crash-safe). |
| A skipped tenant | Waits for the next interval (at most one evaluator interval late). |
| Sweeper duplicates | Allowed: row locks decide; only counters change. |
| Shutdown delay default | `0s` (compose unchanged); the 23b chart sets it. Upper bound 60 s. |
| Background loops on SIGTERM | Stop at once, before the listener drains; other replicas continue. |
| Hash collision of lock keys | Accepted (2⁻⁶⁴): worst case one skipped pass, or a blocking advisory lock (kill, scheduler capacity group, release guard) waiting for one evaluation. |
