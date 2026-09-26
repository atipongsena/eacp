# Phase 23a — High availability inside the binaries (design)

Date: 2026-09-26 · Status: approved by the owner (design in chat: "yes, write the spec and dev until finished phase")
Scope: MASTER_PLAN §95 (Phase 23). Phase 23 is split: **23a** (this spec) makes and proves the binaries safe to
run as N replicas; **23b** is the Helm chart, NetworkPolicies, disruption budgets, autoscaling and a real
minikube run (a later spec). ADR: **ADR-029** (new, next free number; §74 assigns none).

## 1. Intent

§95 asks for multiple API nodes, multiple scheduler nodes, worker autoscaling, pod disruption handling and
"leader election where required". Before any Kubernetes object exists, the binaries must be correct, cheap and
graceful when several copies run at once and when copies come and go.

What already holds (verified in the code, 2026-09-26):

- Every authority is a PostgreSQL row, so replicas share no memory that decides anything.
- The incident and FinOps evaluators insert with `ON CONFLICT DO NOTHING`; the release evaluator locks each
  `CANARY` release `FOR UPDATE`. Two concurrent evaluations of one tenant cannot double an incident, an alert
  or a rollback.
- The sweeper re-reads each action `FOR UPDATE` in its own transaction and moves it by compare-and-set.
- The outbox relay publishes with `FOR UPDATE SKIP LOCKED` (`TestConcurrentRelaysPublishEachRowOnce`).
- Workers claim through the fair scheduler in `eacp.claimable_actions`/T14; every write is fenced by the lease
  generation; `max_inflight` is the cluster-wide bulkhead (ADR-022 §2). The worker id defaults to the host
  name, which is the pod name in Kubernetes.
- On SIGTERM a worker stops claiming, lets in-flight calls finish (bounded by their call deadline) and
  releases undispatched leases. A process killed mid-call leaves `EXECUTING` → lease expiry → `UNKNOWN_OUTCOME`
  → reconciler (ADR-004).

What is missing:

1. **Duplicate work grows with N.** Every API replica runs every evaluator for every tenant on the same
   interval. The evaluations are correct but serialise on each tenant's audit chain head (the Phase 22a
   deferred minor), and N−1 of them do nothing useful.
2. **The sweeper's counters over-count** when another replica moved the action first: `Expired`,
   `Reclaimed`, `Retried` and `Escalated` are incremented even when the locked re-check found nothing to do.
3. **Shutdown drops requests in Kubernetes.** The listener closes as soon as SIGTERM arrives, before the
   endpoint controller has removed the pod from its Service, so a rolling update or drain fails a few
   in-flight connections.
4. **No proof.** Nothing runs several API loop sets and several workers together over one database and
   checks the outcome, including a replica leaving mid-run.

Success: the loops of N replicas do each tenant's evaluation once per interval, the sweeper's counters are
exact, a draining pod reports not-ready while it keeps serving for a configurable delay, and a multi-replica
test settles every action with one ERP effect per operation key while replicas stop mid-run.

## 2. Principles (non-negotiable)

1. **PostgreSQL stays the only authority.** No leader decides anything. The new lock only avoids duplicate
   work; every outcome is identical with or without it, and a test proves that for each evaluator.
2. **No Kubernetes dependency in the binaries.** No client-go, no Lease objects, no API-server access. The
   same binaries run under compose, systemd or Kubernetes.
3. **Pooler-safe.** Only transaction-scoped locks (`pg_try_advisory_xact_lock`); nothing depends on a
   session outliving a transaction.
4. **Defaults keep today's behaviour.** `EACP_SHUTDOWN_DELAY` defaults to `0s`; compose is unchanged.
5. **No schema change.** No migration, table, role or SECURITY DEFINER function.
6. **Delivery terms per MASTER_PLAN §21.** "Effectively-once where reconcilable"; never "exactly-once".

## 3. Design

### 3.1 Per-tenant loop lock (approach A: no leader)

`internal/storage` gains:

```go
// TryLoopLock takes the transaction-scoped advisory lock of background loop
// `loop` for the transaction's tenant (set by InTenantTx) without waiting.
// It returns false when another transaction holds it.
func TryLoopLock(ctx context.Context, tx pgx.Tx, loop string) (bool, error)
```

SQL: `SELECT pg_try_advisory_xact_lock(hashtextextended('eacp.loop:' || $1 || ':' || eacp.current_tenant_id()::text, 0))`.
`loop` must match `^[a-z][a-z0-9_]{0,31}$` (else an error, never a lock).

`incident.Service.Evaluate`, `finops.Service.Evaluate` and `release.Service.Evaluate` call it right after
`storage.SetSystem`, with loop names `incident`, `finops` and `release`. When it returns false the
evaluation returns `(0, nil)` and changes nothing: another replica is evaluating that tenant now, and the
next interval picks up anything it missed. The lock is released at commit or rollback, so a crashed replica
never holds it.

Key space: the prefix `eacp.loop:` is distinct from `eacp.kill:` and from the release guard's
`<tenant>/<agent>` key. A 64-bit hash collision could at worst make a try-lock fail (one skipped pass) or
make a blocking kill-lock wait for one evaluation; recorded in ADR-029.

### 3.2 Sweeper and pruner: row locks only

No loop lock: each sweeper step is already its own transaction that locks the action row and re-checks.
The sweeper's per-step transaction closures record whether they moved a row, and only a move increments
`Expired`, `Reclaimed`, `Retried` or `Escalated`. `Advanced` keeps counting advance attempts (its doc comment
says so). The pruner's `DELETE`s are idempotent and need nothing.

### 3.3 Drain on shutdown

New setting `EACP_SHUTDOWN_DELAY` (Go duration, default `0s`, `0s`–`60s`; anything else fails startup, like
every setting). `service.Deps.Serve` changes the shutdown sequence to:

1. The signal context ends.
2. `/readyz` answers 503 with the check `draining` failing (it is the first readiness check, and exists even
   for services without a database). `/healthz` keeps answering 200.
3. Background loops are cancelled at once: the worker stops claiming (in-flight calls still finish as today),
   API loops stop (other replicas carry on).
4. The listener keeps serving for `EACP_SHUTDOWN_DELAY`.
5. The existing graceful HTTP shutdown runs, bounded by `EACP_SHUTDOWN_TIMEOUT`.
6. `stop` waits for the background loops, then closes the pool and telemetry (unchanged).

A listener failure before the signal still returns at once (no delay).

### 3.4 Multi-replica proof (`internal/worker`, PostgreSQL-gated)

`TestReplicasShareTheWorkAndSurviveLosingOne` in `internal/worker/ha_integration_test.go`, on the existing
`erpEnv` harness (real Fake ERP over HTTP, real HTTP connector, local PDP):

- Three **API replicas** (each its own `pgxpool` on the app DSN, its own `action.Engine`) run the sweeper,
  the incident, FinOps and release evaluators and the outbox pruner at short intervals.
- Three **worker replicas** (each its own pool, id `w-ha-<n>`) run the worker and the reconciler.
- 30 actions across the Fake ERP scenarios (success, execute-then-timeout, reset, 5xx-after-effect, slow)
  are submitted through the API replicas in turn. Before the replicas start, an operator kills an unrelated
  idle agent, so every incident evaluator has one standing signal.
- After a third of the submissions, API replica 1 and worker replica 1 are stopped (context cancelled, as
  SIGTERM does).
- Assertions:
  - every action is terminal and `SUCCEEDED` with the expected external reference;
  - the Fake ERP holds one record per operation key (effectively-once where reconcilable);
  - one `kill` incident for the one kill occurrence, although three evaluators saw it;
  - no replica logged an `evaluation failed` or `sweep failed` error;
  - `audit.Verify` succeeds for the tenant;
  - at least two worker ids executed actions (the work was shared).

Unit-level tests per package (all RED first):

- `storage`: `TestTryLoopLockIsPerLoopAndTenant` (a held lock blocks the same loop and tenant only; released
  at commit), `TestTryLoopLockRejectsBadNames`.
- `incident`, `finops`, `release`: `Test…EvaluationSkipsATenantAnotherReplicaHolds` (lock held by an open
  transaction → `(0, nil)` and nothing written; after it ends the next evaluation does the work) and
  `TestConcurrent…EvaluationsActOnce` (two evaluations of one tenant in parallel, with the lock bypassed by
  calling the SQL function directly in two transactions, and again through `Evaluate`: one result either
  way, no error).
- `action`: `TestTwoSweepersMoveEachActionOnceAndCountIt` (both sweepers select the same overdue actions
  while a blocker holds their rows; after release, the actions are `EXPIRED` once and the two `Expired`
  counters sum to the number of actions).
- `config`: `TestShutdownDelaySetting` (default 0, bounds, garbage).
- `service`: `TestDrainReportsNotReadyAndKeepsServing` (after cancel: `/readyz` 503 `draining`, `/healthz`
  200, requests still served during the delay, background context already cancelled, Serve returns only
  after the delay), `TestNoDelayShutsDownAtOnce`.

### 3.5 Documentation

- `docs/adr/ADR-029-high-availability.md` (Rev 1.0, 23a; 23b extends it) with the unresolved-assumptions
  table, and its row in `docs/adr/README.md`.
- MASTER_PLAN §95 status line; AGENTS.md status and one rule line (loop locks save work only; no leader;
  new background loops take `storage.TryLoopLock` or prove row-lock arbitration); README Phase 23 section;
  `docs/INVARIANTS.md` only if an [A] invariant gains a test (invariants 1, 5, 12 gain the HA test).

## 4. Out of scope (23b or later)

Helm chart, Kubernetes manifests, NetworkPolicies, PodDisruptionBudgets, autoscaling metrics or KEDA,
cert-manager, PostgreSQL HA (managed/external), a NATS cluster, and any Kubernetes Lease-based election.

## 5. Unresolved assumptions (conservative choices, recorded in ADR-029)

| Assumption | Choice |
|---|---|
| Leader election | None. PostgreSQL arbitrates; per-tenant try-locks only save work. |
| Lock scope | Transaction-scoped, per loop and tenant; never session-scoped (pooler-safe, crash-safe). |
| A skipped tenant | Waits for the next interval (at most one evaluator interval late). |
| Sweeper duplicates | Allowed: row locks decide; only counters change. |
| Shutdown delay default | `0s` (compose unchanged); the 23b chart sets it. Upper bound 60 s. |
| Background loops on SIGTERM | Stop at once, before the listener drains; other replicas continue. |
| Hash collision of lock keys | Accepted (2⁻⁶⁴): worst case one skipped pass or one short wait. |
