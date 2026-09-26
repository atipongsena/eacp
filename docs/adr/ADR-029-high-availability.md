# ADR-029: High availability — replicas without a leader

Status: Accepted (Rev 1.2, 2026-09-26). Rev 1.0: Phase 23a, the binaries (§1–§4). Rev 1.1: Phase 23b, the
Kubernetes deployment (§5). Rev 1.2: the PDP sidecar drains too (§3, §5), and the Go services resend a lost
DNS query after 1 s (§5). Scope: MASTER_PLAN §95.
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

The AGT sidecar PDP (`sidecars/agt-pdp`, Rev 1.2) drains the same way on SIGTERM (`Server.drain`): for
`AGT_PDP_SHUTDOWN_DELAY` (default `0s`, `0s`–`60s`, whole seconds) it keeps accepting and answering and every
response says `Connection: close`; then it serves the connections already in its listen backlog, closes the
listener, closes idle kept-alive connections from its side, and waits up to `AGT_PDP_SHUTDOWN_TIMEOUT` (default
`15s`) for the decisions it accepted. Until Rev 1.2 it closed its listener within half a second of SIGTERM.
Kubernetes removes a terminating pod's endpoint while it signals the pod, and kube-proxy on each node takes a
moment longer to stop routing new connections there, so an API call to a stopping PDP was refused
(`connection refused`) or, once the pod's network was gone, hung until `EACP_PDP_TIMEOUT`. The action stayed
`RECEIVED` and the API answered `503 governance_unavailable` — fail closed, but a rollout or a drain was not
free of errors, whatever the PodDisruptionBudgets allowed. The Go client still never retries (ADR-002).

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

### 5. Kubernetes (Rev 1.1, Phase 23b)

The Helm chart `deployments/helm/eacp` (guide: `docs/KUBERNETES.md`) deploys the binaries of §1–§4 unchanged.

- **Shape.** Deployments for the API, the worker and the PDP sidecar (2 replicas each), Services for the API
  and the PDP, a migrate Job as a `pre-install,pre-upgrade` hook with the owner DSN, and one ServiceAccount with
  no token. No RBAC object, no operator, no cluster API access. PostgreSQL and NATS are external.
- **Secret custody.** Values only name Secrets with fixed keys; the chart renders no secret value. The worker
  alone mounts the connector secrets, the API alone the PDP client key, the PDP alone its server key, and the
  migrate Job alone the owner DSN (ADR-001).
- **Hardening.** Every pod runs as 65532, non-root, with a read-only root filesystem, no privilege escalation,
  all capabilities dropped, `RuntimeDefault` seccomp, a `/tmp` emptyDir and no service links. The release
  namespace enforces the `restricted` Pod Security Standard.
- **Environment.** The chart sets `EACP_ENV` from `environment` (default `production`), so NATS must be
  `tls://` outside development and test (ADR-014 §6); optional `nats.caSecret` and `database.caSecret` mount a
  private CA. The binaries alone default to `development`.
- **Fail closed at render time.** `helm template` stops with a message for a missing Secret name; a connector
  secret file, database URL, credential-bearing URL, non-string value or chart-set variable in any `env`
  (Kubernetes keeps the last duplicate, so an override would win); an unknown environment; a shutdown delay or
  timeout that is not whole seconds in range, or a `worker.maxCallSeconds` that is not a whole number ≥ 1;
  replicas below 1; a short grace period; an unknown governance provider; empty PostgreSQL or NATS peers; or no
  connector egress without an explicit opt-out.
- **NetworkPolicies** repeat the compose networks: default deny in the namespace, DNS egress, API ingress on
  8080 only, the PDP reachable only from the API, the worker the only pod with egress to enterprise systems
  (`worker.connectorEgress`), and the migrate Job reaching only PostgreSQL. The dev dependencies protect
  themselves the same way, so every target decides who reaches it.
- **Availability.** PodDisruptionBudgets `maxUnavailable: 1`; rolling updates `maxUnavailable: 0`,
  `maxSurge: 1`; pods spread across nodes (`ScheduleAnyway`). `EACP_SHUTDOWN_DELAY` 10 s and
  `EACP_SHUTDOWN_TIMEOUT` 15 s, and the same values as `AGT_PDP_SHUTDOWN_DELAY` and `AGT_PDP_SHUTDOWN_TIMEOUT`
  for the PDP (Rev 1.2); the chart refuses a worker grace period below delay + timeout +
  `worker.maxCallSeconds` and an API or PDP grace below delay + timeout. `worker.maxCallSeconds` defaults to
  300, the cap PostgreSQL puts on a call (`eacp.call_timeout`), so the default worker grace (330 s) covers every call.
- **DNS (Rev 1.2).** The API and the worker run with the pod DNS options `timeout:1` and `attempts:3`. The API
  resolves the PDP Service on every new connection, and Go's resolver resends a lost query only after the
  resolv.conf timeout, 5 s unless set: the whole `EACP_PDP_TIMEOUT`. DNS queries are lost while pods start and
  stop on a node (a probe resolving the PDP every 50 ms during the disruption run lost one or two queries per
  run, each beside pod churn on its node; why the node loses them was not traced further). With the options a
  lost query costs 1 s. Nothing is sent twice: the resend is a DNS query, before any byte of the call.
- **Autoscaling.** CPU HPAs for the API and the worker exist but are off by default. Queue-depth scaling is
  deferred: it needs a new dependency (KEDA or a metrics adapter) and a read-only queue metric.
- **Proof.** `test/helm` renders the chart and checks each rule above, including every refused value
  (`EACP_HELM_REQUIRED=1` fails instead of skipping without Helm). `scripts/k8s-e2e.sh` installs it on a 2-node
  minikube cluster with Calico and runs the Slice A demo unchanged and `TestKubernetesDisruption`. The API and
  the PDP roll onto one node, the workers scale to 3 and that node drains; 30 purchases, each on a new
  connection, are all accepted on the first attempt and end `SUCCEEDED` with one ERP record each; the API and
  PDP Services never lose their last ready endpoint (an EndpointSlice watch); a `/healthz` probe through the
  Service every 100 ms never fails; the stand-in agent reaches the API and nothing else; every pod is non-root
  with a read-only root filesystem. With the PodDisruptionBudgets deleted the same run fails: the drain evicts
  both PDP pods at once and a purchase gets `503 governance_unavailable`. The PodDisruptionBudgets were not
  enough: on Rev 1.1 the run failed in most attempts the same way, one purchase at a time, while a single PDP
  pod stopped. Captures of API and PDP logs, EndpointSlices, each node's kube-proxy rules and conntrack showed
  the API dialling the PDP Service within a second of a PDP's SIGTERM, kube-proxy still sending it to that
  pod, and the pod's listener already closed. `sidecars/agt-pdp` `tests.test_main` reproduces it without a
  cluster (a SIGTERMed sidecar refused connections at once); `tests.test_server.DrainTest` pins the drain, and
  `test/helm` `TestEveryServerDrainsOnShutdown` the chart's variables. With the drain the run still failed now
  and then: a traced API call spent its 5 s without starting to connect, which is where Go resolves the name,
  and in a container a DNS server that drops the first query of each name makes Go's lookup of `eacp-pdp` take
  5.01 s with Kubernetes' default resolv.conf and 1 s with `timeout:1 attempts:3`. `test/helm`
  `TestGoServicesResendALostDNSQueryAfterOneSecond` pins the options.

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
| Shutdown delay default | `0s` (compose unchanged); the chart sets 10 s. Upper bound 60 s. |
| Background loops on SIGTERM | Stop at once, before the listener drains; other replicas continue. |
| Hash collision of lock keys | Accepted (2⁻⁶⁴): worst case one skipped pass, or a blocking advisory lock (kill, scheduler capacity group, release guard) waiting for one evaluation. |
| PostgreSQL and NATS in the chart (Rev 1.1) | No: external services; dev-only manifests for the e2e. |
| Secrets (Rev 1.1) | Referenced by name only; never rendered from values. |
| API ingress default (Rev 1.1) | Any source, port 8080 only; narrowing is per deployment. |
| Worker egress default (Rev 1.1) | None; the chart refuses to render without an allow-list or an explicit opt-out. |
| Autoscaling (Rev 1.1) | CPU HPAs, off by default; queue-depth scaling deferred. |
| PDB (Rev 1.1) | `maxUnavailable: 1` for api, worker and pdp. |
| Shutdown timing (Rev 1.1) | Delay 10 s, timeout 15 s; worker grace ≥ delay + timeout + longest call (default 300 s, PostgreSQL's cap). |
| PDP shutdown (Rev 1.2) | The sidecar drains in-process with the chart's delay and timeout (no `preStop` hook, so compose and Kubernetes run the same code); PDP grace ≥ delay + timeout (default 30 s). |
| Retrying a PDP call (Rev 1.2) | Not added: the client stays single-shot, and the drain removes the cause. A refused call still fails closed. |
| DNS options (Rev 1.2) | `timeout:1`, `attempts:3` for the API and the worker, fixed in the chart; the PDP resolves nothing. A cluster with NodeLocal DNSCache keeps them harmlessly. A dead DNS now fails a lookup in 3 s instead of 10 s. |
| Environment (Rev 1.1) | The chart sets `EACP_ENV=production` unless told otherwise; env entries never override a chart-set variable. |
| PDP probes (Rev 1.1) | TCP connects, since its HTTP health needs a client certificate; each logs an `SSLEOFError` line. |
| Namespace (Rev 1.1) | Dedicated, `restricted` Pod Security Standard; the default deny covers the whole namespace. |
| Slice C on Kubernetes (Rev 1.1) | Not run (a file copy into a distroless pod); compose keeps it. |
