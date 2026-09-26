# Enterprise Agent Control Plane (EACP)

EACP is a control plane for running many AI agents in an enterprise. Agents may be built with any framework, but **privileged actions against enterprise systems go through EACP**. There they are governed, approved, executed safely and audited.

- **Governance:** Microsoft Agent Governance Toolkit / ACS, connected through a sidecar PDP ([ADR-002](docs/adr/ADR-002-agt-integration-sidecar-pdp.md))
- **Execution:** a Go execution fabric with fenced dispatch, explicit `UNKNOWN_OUTCOME` handling and reconciliation ([ADR-004](docs/adr/ADR-004-action-state-machine-and-execution-semantics.md))
- **State:** PostgreSQL is the single source of truth, with tenant isolation enforced by Row-Level Security

> **Status: early development.** Slice A (Phases 1–8) is complete: platform foundation; registry, identity and capability; local governance and durable approvals; the Action API and atomic release boundary; workers, leases, fencing and the dispatch intent; the HTTP connector and Fake ERP; `UNKNOWN_OUTCOME`, reconciliation and human resolution; hardening and the [demo](docs/DEMO.md). Every Slice A invariant has passing tests ([map](docs/INVARIANTS.md)). Slice B is complete:
- Phase 9 adds the Microsoft AGT/ACS sidecar PDP ([ADR-002](docs/adr/ADR-002-agt-integration-sidecar-pdp.md) Rev 2.4).
- Phase 10 adds NATS JetStream work hints and dashboard events ([ADR-014](docs/adr/ADR-014-postgresql-authority-nats-signals.md)).
- Phase 11 adds hard budget reservation ([ADR-012](docs/adr/ADR-012-budget-reservation.md)).
- Phase 12 adds PostgreSQL fair claim scheduling and connector capacity ([ADR-011](docs/adr/ADR-011-scheduler-fairness.md)).
- Phase 13 adds backpressure, bulkheads, circuit breakers and retry budgets ([ADR-022](docs/adr/ADR-022-backpressure-bulkheads-circuit-breakers-retry-budgets.md)).

Slice C has begun. Phase 14 adds the MCP registry: tool discovery, fingerprints, definition history, contract invalidation and quarantine ([ADR-023](docs/adr/ADR-023-mcp-registry-and-tool-fingerprint.md)). Phase 15 adds tenant-scoped dependency evidence and conservative blast-radius queries ([ADR-015](docs/adr/ADR-015-dependency-graph.md)). Phase 16 adds PostgreSQL-fenced execution kills for scopes bound to actions ([ADR-016](docs/adr/ADR-016-distributed-kill-switch.md)). Phase 17 adds fleet operations and the fleet view ([ADR-024](docs/adr/ADR-024-fleet-operations.md)). Phase 18 adds Agent FinOps: LLM cost ingest, chargeback, soft budgets, a spend dashboard and alerts ([ADR-025](docs/adr/ADR-025-agent-finops.md)). Phase 19 adds agent releases: evaluation evidence, replay, a structurally non-destructive shadow, a PostgreSQL-enforced canary cohort, promotion and rollback ([ADR-018](docs/adr/ADR-018-release-and-evaluation.md)). The [Slice C demo](docs/DEMO.md#slice-c-demo) triggers MCP drift, shows the blast radius, kills the affected agent version and shows the trace and audit evidence.
> See the [Master Plan](docs/MASTER_PLAN.md) and the [ADRs](docs/adr/).

## Slice A goal

> Privileged agent actions cannot bypass the control plane, approvals are durable, retries cannot casually duplicate irreversible side effects, and ambiguous execution outcomes are handled explicitly rather than guessed.

This claim is scoped to conforming deployments; see [ADR-001 §3a](docs/adr/ADR-001-product-boundary-and-enforcement-point.md).

## What exists today (Slice A, Phases 1–8)

| Capability | Evidence |
|---|---|
| Services: `controlplane-api`, `execution-worker`, `fakeerp`, `eacpctl` | `docker compose up -d --build` |
| Fail-closed startup: refuses a DB role that can bypass RLS | `internal/service` tests; container exits 1 with a superuser DSN |
| Row-Level Security convention: a missing tenant context sees no rows | `internal/storage` tests (mutation-checked) |
| Schema migrations (goose, embedded, advisory-locked) | `eacpctl migrate up\|status` |
| Health and readiness (database, role safety, schema version) | `GET /healthz`, `GET /readyz` |
| Secret redaction in logs | `internal/logging`, `internal/service` tests |
| OpenTelemetry tracing with W3C propagation | `internal/telemetry` tests |
| Graceful shutdown | `internal/httpserver` tests |
| Network isolation: the agent can't reach the ERP or the DB | `test/security` (mutation-checked) |
| Registry: principals, two-person role grants, groups, agents, versions, allowlists, connectors, tools, contracts ([ADR-003](docs/adr/ADR-003-agent-registry-identity-and-capability.md)) | `internal/registry` (schema tests run raw SQL as the app role; triggers mutation-checked) |
| Two-person rules enforced by PostgreSQL: grants, credentials, allowlist/contract/version activation, quarantine release | `internal/registry/schema_test.go` |
| API keys: bring your own key, hash-only, bound to a principal or one agent version, 90-day maximum | `internal/identity` |
| Capability check: tool must be in the ACTIVE version's allowlist with an active, unrevoked, fingerprint-matching contract | `registry.CheckCapability`, `POST /v1/agent/capability-check` |
| Hash-chained, append-only audit journal written in the same transaction as each change | `internal/audit` (tamper tests) |
| Local governance provider with five verdicts, immutable policy versions and two-person activation | `internal/governance`; `POST /v1/policies`, `POST /v1/policies/{id}/activate`, `GET /v1/policies/current` |
| JCS + SHA-256 input and enforced digests; immutable decision evidence | `internal/governance/digest.go`, `migrations/00004_governance_approvals.sql` |
| Durable approval requests, human votes, quorum, separation of duties, expiry and one-time action-bound grants | `internal/approval`; `GET /v1/approvals`, `GET /v1/approvals/{id}`, `POST /v1/approvals/{id}/votes` |
| Action API: idempotent submission (409 on a different digest), static admission (429), governance with fail-closed 503, `?wait=` | `internal/action`, `internal/api/actions.go`; `POST /v1/actions`, `GET /v1/actions/{id}` |
| ADR-004 pre-dispatch state machine (T1–T13, T15) enforced by PostgreSQL triggers for raw SQL too | `migrations/00005_actions.sql`, `internal/action/schema_test.go` (mutation-checked) |
| Atomic release boundary: fresh revalidation, one-time grant consumption checked at commit, pinned policy and contract, journal and outbox in one transaction | `internal/action` release tests (parallel releases, activation races, injected failures) |
| Sweeper: outage recovery, release after approval, expiry; cancel that never consults the PDP | `internal/action/sweeper.go`; `POST /v1/actions/{id}/cancel` |
| Worker claim (`FOR UPDATE SKIP LOCKED`, fair order from Phase 12), heartbeats and lease generations; PostgreSQL rejects any write by a stale worker | `migrations/00006_execution.sql`, `migrations/00012_scheduler.sql`, `internal/worker/schema_test.go` (mutation-checked) |
| Fenced dispatch intent before any call, with drift re-check (T16a/T16b) and an attempt row per dispatch; fenced results and late-result evidence | `internal/worker` (lease race, stale worker never dispatches twice) |
| Lease reclaim, retries by contract, cancel requests while executing | `internal/action/sweeper.go`, `internal/action/execution_test.go` |
| Worker connector credentials are tenant-namespaced and host-bound; agents receive none | `internal/worker/secrets.go`, `test/security` |
| HTTP connector and credential-protected Fake ERP with a durable operation-key lookup and failure scenarios | `internal/connector`, `internal/fakeerp`, `internal/worker/http_integration_test.go` |
| Fenced reconciler: lookup under the pinned proof standard; only authoritative, settled absence permits a retry with the same key or `FAILED`; conflicts and exhaustion go to a human | `migrations/00007_reconciliation.sql`, `internal/worker/reconciler.go`, `internal/worker/reconcile_schema_test.go` |
| Fake ERP flagship tests: lost response → one record; delayed visibility → no retry; killed worker → no blind re-dispatch | `internal/worker/reconcile_integration_test.go` |
| Operator resolution with separation of duties and a two-person retry; queue and evidence for operators and auditors | `internal/action/resolution.go`; `GET /v1/actions?state=`, `GET /v1/actions/{id}/evidence`, `POST /v1/actions/{id}/resolutions`; `eacpctl action` |
| Evidence reconstruction from an `action_id`: decisions, approvals with votes and grant, attempts, checks, resolutions and journal, with chain verification | `internal/action/evidence.go`, `internal/worker/evidence_integration_test.go` |
| Tenant isolation: RLS catalog test with reviewed cross-tenant paths; every table swept after a full flow | `internal/storage/rls_catalog_test.go`, `internal/worker/isolation_integration_test.go` |
| Chaos: connection storms under live loops, restarted services, killed worker, duplicate submissions; governance outages block no cancellation | `internal/worker/chaos_integration_test.go` |
| Slice A invariant map, checked against MASTER_PLAN §103 | `docs/INVARIANTS.md`, `test/invariants` |
| Slice A and Slice C demos on an isolated stack (§111) | `scripts/demo.sh`, [docs/DEMO.md](docs/DEMO.md) |

## Slice B (Phase 9): the AGT sidecar PDP

| Capability | Evidence |
|---|---|
| Sidecar PDP wrapping the pinned AGT 5.0.0 policy layer, the ACS 0.3.1b1 engine and OPA 1.20.2; stateless, never resolves an approval | `sidecars/agt-pdp`, [research/REFERENCES.md](research/REFERENCES.md) |
| Go client implementing `GovernanceProvider`: loopback or mutual TLS 1.3 only, version pins per decision, strict responses, clock-skew bound | `integrations/governance/microsoftagt` |
| Conformance: one reference set with digests; the local provider, the wire protocol and the sidecar (through ACS and OPA, in its image build) must all reproduce it | `test/conformance/governance_reference.json`, `internal/governance/conformance_test.go`, `sidecars/agt-pdp/tests` |
| Provider evidence (ACS identity, rule, adapter digest, engine versions) stored and journaled with each decision | `migrations/00009_provider_evidence.sql`, `GET /v1/actions/{id}/evidence` |
| Compose runs `controlplane-api` with `EACP_GOVERNANCE_PROVIDER=microsoft-agt` on an internal `pdp` network; only the API reaches the sidecar | `test/security/agt_pdp_test.go` |

## Slice B (Phase 10): NATS JetStream signals

| Capability | Evidence |
|---|---|
| The outbox relay in `controlplane-api` publishes after the transaction commits. It locks rows with SKIP LOCKED and uses the row id as `Nats-Msg-Id` and the row's `traceparent`. A row is marked published only after the PubAck; delivery is at least once. | `internal/messaging/relay.go`, `internal/messaging/relay_test.go` |
| Work hints carry only `action_id` and wake the worker's PostgreSQL claim loop. Polling stays on, so without NATS the system is slower, not wrong. | `internal/messaging/hints.go`, `TestHintedWorkerExecutesLongBeforeItsPollInterval`, `TestWithoutNATSTheWorkerStillExecutes` |
| Inbox dedup (§63). A tenant table under RLS, written only by the `inbox` messaging actor. A duplicate is ACKed and not processed again. | `migrations/00010_messaging.sql`, `TestHintWakesOnceAndADuplicateIsAckedWithoutWaking` |
| Dashboard event stream (`eacp.events.<tenant>.action.transition`). Each event carries only ids, states and a time, never a reason or a payload. | `TestTransitionsWriteDashboardEvents`, `TestDashboardEventsArrivePerTenant` |
| Compose runs NATS on an internal `bus` network with one user per role. The worker's user cannot publish, and the agent has no route. | `deployments/docker/nats/nats.conf`, `test/security/nats_test.go` |

## Slice B (Phase 11): hard budget reservation

| Capability | Evidence |
|---|---|
| A connector contract declares what a call costs: a unit, a fixed cost and a payload amount field. PostgreSQL computes the cost from the enforced payload, which the decision's digest binds. | `migrations/00011_budget.sql` (`eacp.action_cost`), `TestContractsDeclareACostOrNone` |
| The release transaction reserves the cost on the agent's budget leaf before any journal write and before a grant is consumed. A budget that can't take it denies the action (`budget_exceeded`); no account or an invalid cost also denies. T10 of a budgeted action requires the reservation. | `eacp.budget_reserve`, `TestReservationsAreMadeOnlyByTheReleaseForTheActionsCost`, `TestABudgetDenialNeverSpendsTheApproval`, `TestBudgetFailuresDenyClosed` |
| No oversubscription (§103 invariant 3): 100 concurrent releases on a leaf that fits 37 reserve exactly 37. `CHECK (allocated + reserved + committed <= hard_limit)` is the backstop. | `TestConcurrentReleasesNeverOversubscribeAHardBudget` (logs p50/p99) |
| The action's own state change settles its reservation: success commits, no effect releases, an unknown outcome holds it until reconciled or resolved, and expiry at `not_after` (the TTL) releases it. Settling never locks the account. | `TestSettlementFollowsTheOutcome`, `TestAnExpiredReleaseGivesItsBudgetBack`, `TestSettlementNeverWaitsForTheAccount` |
| Account trees with escrow: a child's limit is carved from its parent, so a reservation locks only its leaf. Lowering a limit takes one admin; raising it takes two. | `TestEscrowBoundsChildrenByTheirParent`, `TestRaisingALimitIsTwoPersonAndLoweringIsNot`, `TestReleasesSettlementsAndLimitChangesDoNotDeadlock` |
| Budget API: `POST /v1/budgets`, `GET /v1/budgets[/{id}]`, `POST /v1/budgets/{id}/limit`, `POST /v1/budget-limit-changes/{id}/approve\|reject`. Action evidence shows the reservation. | `internal/api/budget.go`, `TestBudgetsThroughTheAPI` |

## Slice B (Phase 12): fair scheduler

| Capability | Evidence |
|---|---|
| PostgreSQL chooses tenant/team weighted turns; teams pin their group weight and contract priority at release, with aging and deadline promotion within a team. NATS remains a wake-up hint. | [ADR-011](docs/adr/ADR-011-scheduler-fairness.md), `TestSchedulerServesTenantWithSmallerBacklog`, `TestSchedulerUsesPinnedTeamWeight`, `TestSchedulerPriorityAndAging` |
| T14 serializes and enforces `max_inflight` for a connector or named group, including raw SQL, concurrent claims and stale transaction snapshots. | `TestConnectorCapacityIsEnforcedForRawClaims`, `TestConnectorCapacitySerializesConcurrentClaims`, `TestConnectorCapacityRejectsStaleRepeatableReadClaim`, `TestNamedCapacityGroupSpansConnectorsAndStateIsTenantIsolated` |
| The 10,000:100:100 benchmark gives each small team 10 of the first 30 claims; the measured 30-claim run was about 1.4 seconds with `-race` on the development machine. | `BenchmarkSchedulerFairness` |

## Slice B (Phase 13): backpressure, bulkheads, circuit breakers and retry budgets

| Capability | Evidence |
|---|---|
| Admission answers 429 with a `scope` and creates nothing: global and tenant queues, a tenant's unreleased actions (`EACP_ACTION_MAX_PENDING_PER_TENANT`), and a connector group's queue (contract `max_queued`). A full connector queue still admits other connectors. | [ADR-022](docs/adr/ADR-022-backpressure-bulkheads-circuit-breakers-retry-budgets.md) §1, `TestAdmissionBoundsPendingAndConnectorQueues`, `TestAdmissionLimitIs429` |
| No connector failure starves another pool (§103 invariant 9): a worker holds at most `EACP_WORKER_GROUP_CONCURRENCY` actions per capacity group, and `max_inflight` bounds the group across workers. | `TestConnectorFailureDoesNotStarveUnrelatedConnectors` |
| A breaker per worker and connector opens after `EACP_WORKER_BREAKER_FAILURES` failures, probes once after its cooldown and doubles the cooldown on each consecutive trip. It also opens the connector's shared circuit, journaled; while that is open, PostgreSQL refuses the claim (T14) and the dispatch intent (T16). Operators disable and enable a connector (`eacpctl connector disable\|enable`). | `TestBreakerOpensProbesAndCloses`, `TestLocalBreakerHoldsWithoutTheSharedCircuit`, `TestOpenCircuitRefusesClaimAndDispatch`, `TestDisableSerializesWithAStaleDispatchIntent`, `TestCircuitChangesAreGuardedAndJournaled` |
| Retries back off exponentially with jitter and stay within the contract's retry budget: attempts, `retry_max_elapsed_ms` since the first dispatch, and `retry_max_cost`. PostgreSQL answers the budget question for the worker, sweeper, reconciler and operator retries alike. | `TestDefaultBackoffIsJitteredWithinBounds`, `TestRetryBudgetBoundsElapsedTime`, `TestRetryBudgetBoundsRetryCost` |

## Slice C (Phase 14): MCP registry and tool fingerprint

| Capability | Evidence |
|---|---|
| An MCP server is a connector with protocol `mcp`; its tools are discovered, never declared. The execution worker's scanner lists them over Streamable HTTP (2026-07-28, falling back to the initialize-based 2025 revisions; JSON and SSE) with the worker-held, host-bound credential, every `EACP_MCP_SCAN_INTERVAL`, and bounds pages, tools and bytes. | [ADR-023](docs/adr/ADR-023-mcp-registry-and-tool-fingerprint.md) §1–§2, `internal/connector/mcp` (interop with the official MCP Go SDK), `TestScannerDiscoversToolsAndQuarantinesDrift` |
| Scans are fenced by a scan lease (worker id, generation, expiry) in PostgreSQL; a stale scanner records nothing, and a failed scan changes no tool. | `TestScanLeaseFencesStaleScanners`, `TestStaleScannerCannotRecord`, `TestScannerRecordsFailuresWithoutChangingTools` |
| PostgreSQL fingerprints each canonical (RFC 8785) definition, keeps the history, and classifies a change: display-only (`title`, `icons`) is low risk, anything else high. Server annotations are untrusted hints that can only tighten. | `TestScanRecordsDefinitionsWithDatabaseFingerprints`, `TestDefinitionChangesAreClassifiedAndInvalidateContracts`, `TestMCPContractRules` |
| A contract pins the reviewed definition. A high-risk change, or a certified tool that disappears, quarantines the tool and its contract stops matching; an approved action is denied at release. Quarantine is containment (operator or registry approver); release needs a second registry approver and never recertifies. | `TestMCPDefinitionDriftBeforeReleaseDenies`, `TestMissingCertifiedToolIsQuarantined`, `TestToolQuarantineRules` |
| Operators see tools, definition history and scans, request a rescan, and quarantine or release a tool. | `GET /v1/connectors/{id}/tools`, `GET /v1/connectors/{id}/mcp[/scans]`, `POST /v1/connectors/{id}/mcp/scan`, `GET /v1/tools/{id}[/definitions]`, `POST /v1/tools/{id}/quarantine\|release`; `eacpctl connector tools\|mcp\|scans\|scan`, `eacpctl tool` |

Executing MCP tools (`tools/call`) is not part of Phase 14: no worker serves protocol `mcp`, so an action on an MCP tool is never dispatched.

## Slice C (Phase 15): dependency graph and blast radius

| Capability | Evidence |
|---|---|
| Active allowlists give Agent→Tool and Agent→MCP paths; tools belong to MCP servers. Editors record Agent→Model, Agent→MCP, Agent→Agent and Tool→System observations with source, expiry and confidence. The database guards and audits every write. | [ADR-015](docs/adr/ADR-015-dependency-graph.md), `TestDependencyWritesAreGuardedAndTenantScoped` |
| PostgreSQL recursive CTEs return confirmed and possible affected agent versions. Stale and unknown evidence widens the possible set; coverage is explicitly `observed_only`. | `TestBlastRadiusTraversesRegistryAndDeclaredDependencies`, `TestBlastRadiusWidensForUnknownOrStaleEvidence` |
| Operators and auditors query `GET /v1/dependencies/blast-radius?kind=mcp&id=<uuid>` or `eacpctl dependency blast-radius mcp <uuid>`. Editors record with `POST /v1/dependencies` and revoke with `POST /v1/dependencies/{id}/revoke`. For model/system targets, use `name=<tenant-local-name>` in place of `id`. | `TestDependencyAPIRecordsAndReportsBlastRadius`, `TestDependencyBlastRadiusCommand` |

The report counts recent **actions**, not workflow runs; workflow identity is not yet in the registry. Graph evidence does not change execution authorization or trigger containment.

## Slice C (Phase 16): distributed execution kills

Operators use `POST /v1/killswitch` with `{"scope":"agent_version","target_id":"<uuid>","killed":true,"reason_code":"security_incident","reason":"<incident note>"}`. `reason_code` defaults to `operator_request` and accepts the four [AGT reason codes](docs/adr/ADR-016-distributed-kill-switch.md). A different operator resumes the scope with `killed:false`. Operators and auditors list states with `GET /v1/killswitch`. The CLI provides `eacpctl kill activate|resume <scope> <uuid> --reason <text> [--code <reason-code>]` and `eacpctl kill list`.

The enforceable scopes are `tenant`, `team`, `agent`, `agent_version`, `action`, `connector` and `tool`. PostgreSQL rejects a killed scope at claim and dispatch intent; workers check again before the external call and during it. NATS only wakes that check. A kill during execution records `UNKNOWN_OUTCOME` for reconciliation because cancellation does not undo an external effect. `global`, `run` and `model` are rejected until EACP has platform operator authority and authenticated run/model action bindings; see ADR-016.

## Slice C (Phase 17): fleet operations

`GET /v1/fleet/health` is the fleet dashboard, and `GET /v1/fleet/agents` lists agents. Both take `environment`, `risk_class`, `owner_group_id`, `health` and `window` filters. Each agent shows its active version, owner status, matching kills, allowlisted tools the database would not execute, open circuits, and open and recent actions. The resulting health is `ok`, `degraded` or `contained`, with reasons. This is an observation only: it never grants or blocks anything.

`POST /v1/fleet/operations` applies one operation atomically: every version changes, or none does. A dry run returns the plan without changing anything:

```json
{"kind":"pause","selector":{"environment":"production","tool":"erp.post"},"reason":"ERP incident 42","dry_run":true}
```

- `pause` suspends the active version. Queued and new actions are then denied; use a kill (Phase 16) to hold work instead.
- `quarantine` quarantines every live version of the selected agents.
- `resume` and `release` undo exactly the versions a given `source_operation_id` changed.
- `rollback` activates an older `SUSPENDED` version of one agent.

Operators may pause and quarantine. Only a `registry_approver` may resume, release or roll back, under the same separation of duties as a single activation. The CLI is `eacpctl fleet status|list|operation|pause|quarantine|resume|release|rollback`.

## Phase 18: Agent FinOps

EACP is not in the LLM path, so it ingests LLM cost. An agent runtime points its OTLP/HTTP trace exporter at `<api>/v1/agent/otlp` with its agent key, as JSON (`OTEL_EXPORTER_OTLP_PROTOCOL=http/json`), gzip or not. Spans carrying OpenTelemetry GenAI usage (`gen_ai.operation.name` plus `gen_ai.usage.*_tokens`) are recorded against the key's agent and priced in PostgreSQL. Other spans are ignored.

- **Rate card.** An admin adds prices per million tokens with `POST /v1/finops/prices` (or `eacpctl finops price add`). A price applies from now on, never to the past. Usage with no price stays unpriced and raises an `unpriced_usage` alert; it is never counted as zero.
- **Billing.** An admin imports provider billing lines with `POST /v1/finops/billing` (or `eacpctl finops billing import <file.json>`). Per agent and day, the greater of the reported and billed amounts counts.
- **Chargeback.** `GET /v1/finops/chargeback?group_by=agent|team|account&from=…&to=…` combines tool spend from hard budgets (committed and held) with LLM spend.
- **Soft limits and alerts.** An admin sets a monthly soft limit on a budget account with `PUT /v1/finops/soft-limits/{account}`. The evaluator (every `EACP_FINOPS_INTERVAL`, default 1m) raises alerts at 80 % and 100 % of it. It also raises alerts on hourly spend anomalies and on unpriced usage. Operators acknowledge alerts with `POST /v1/finops/alerts/{id}/ack`.
- **Dashboard.** `GET /v1/finops/dashboard` shows spend today and month to date, the top agents, today's hard budget blocks and open alerts.

Soft limits and alerts never block anything; only hard budgets do.

## Phase 19: Release & Evaluation

A release moves an agent from its `ACTIVE` (stable) version to a candidate: `EVALUATING → SHADOW → CANARY → PROMOTED`, or `ROLLED_BACK`.

- **Open.** A registry editor opens a release with `POST /v1/releases` (or `eacpctl release open`), naming the required evaluation suites, the replay and shadow gates, the canary steps in basis points and the guardrail tolerances.
- **Evaluate.** Registry roles record suite results with a dataset digest and an evidence reference (`POST /v1/releases/{id}/evaluations`). EACP gates on them; it does not run them.
- **Replay and shadow.** The candidate's runtime posts what it would do for a reference action to `POST /v1/agent/release/observations` with the candidate's key. The PDP decides the proposal. PostgreSQL pairs it with the reference and computes agreement. A replay answers only with EACP's recorded outcome of the reference. The candidate is not `ACTIVE` before the canary, so none of this can execute anything.
- **Canary.** A second `registry_approver` advances the release (`POST /v1/releases/{id}/advance` with the state they reviewed). The candidate becomes `ACTIVE` beside the stable version and serves only subjects whose bucket is inside the current step; others are `DENIED canary_cohort`. A runtime asks `GET /v1/agent/release/route?subject=` which version to use. Each step and the promotion need enough candidate actions and no guardrail breach against the stable version (denials, failures, unknown outcomes, p95 latency, cost per action).
- **Rollback.** An operator or approver rolls back with `POST /v1/releases/{id}/rollback`. Every `EACP_RELEASE_INTERVAL` (default 30s) the control plane also rolls back any canary whose report breaches a guardrail. Promotion is never automatic.

## Phase 22: Agent SOC and the operator console

- **Incidents.** Every `EACP_INCIDENT_INTERVAL` (default 15s) the control plane opens incidents from existing signals: MCP drift, kills, open circuits, unknown outcomes, canary rollbacks and FinOps overspend, each with its blast radius. Operators acknowledge, assign, note, link and resolve them (`/v1/incidents`, `eacpctl incident`); a critical incident is resolved by a second person. `GET /v1/soc/summary` (`eacpctl soc summary`) serves the SOC counters.
- **Console.** Open `http://localhost:8080/ui/` and sign in with a principal API key. The key stays in the tab's memory only; a reload or 30 minutes without activity signs you out. The console shows the overview, incidents, security (kills, circuits, quarantined tools), fleet, approvals, execution, inventory, dependencies and cost, and runs the incident lifecycle and containment through the same API, behind a confirm dialog. It holds no authority of its own (ADR-028). Set `EACP_UI=off` to disable it.

## Phase 23: running several replicas and Kubernetes

Run as many `controlplane-api` and `execution-worker` replicas as you like against one database: there is no leader, and PostgreSQL arbitrates every background loop (ADR-029). The incident, FinOps and release evaluators skip a tenant another replica is evaluating, the sweeper tolerates actions another replica moved first, and each worker needs a unique `EACP_WORKER_ID` (the host name by default). Set `EACP_SHUTDOWN_DELAY` (0s–60s, default 0s) behind a load balancer: on SIGTERM the service fails `/readyz`, closes kept-alive connections after each response, stops its loops and keeps serving for that long before shutting down. `internal/worker` `TestReplicasShareTheWorkAndSurviveLosingOne` runs three API loop sets and three workers and stops one of each mid-run.

On Kubernetes, install the Helm chart `deployments/helm/eacp` (Phase 23b, ADR-029 Rev 1.1): hardened, replicated Deployments for the API, the worker and the PDP, the compose network boundary as NetworkPolicies, Secrets referenced by name only, migrations as a hook, disruption budgets and optional CPU autoscaling. PostgreSQL and NATS stay external. See [docs/KUBERNETES.md](docs/KUBERNETES.md); `bash scripts/k8s-e2e.sh` proves it on a 2-node minikube cluster.

The worker registers the Phase 6 HTTP connector and runs the Phase 7 reconciler. Fake ERP requires a credential for privileged calls and keeps its operation log in a durable Compose volume.

## Quick start

```bash
python3 deployments/docker/secrets/prepare_fakeerp_token.py
docker compose up -d --build
curl localhost:8080/readyz
```

The stack's governance decisions come from the AGT sidecar (`agt-pdp`). To use the in-process local provider instead, set `EACP_GOVERNANCE_PROVIDER=local` on `controlplane-api`.

The preparation command copies the existing local-development ERP and MCP tokens into Git-ignored files for the Fake ERP and Fake MCP secret mounts. Run it again if the worker's local-development secret changes. This is a demo credential; production deployments supply their own secrets.

Bootstrap a tenant (each admin generates their own key; only the hash is registered):

```bash
export EACP_DATABASE_URL="postgres://eacp_owner:eacp_owner_dev@127.0.0.1:55432/eacp?sslmode=disable"
T=$(uuidgen)
eacpctl key generate --kind principal --tenant $T      # run once per admin, keep the key
eacpctl tenant create --id $T --slug acme --name Acme   --admin name=alice,subject=alice@acme.test,credential=<id>,hash=<hex>   --admin name=bob,subject=bob@acme.test,credential=<id>,hash=<hex>
EACP_API_KEY=<alice's key> eacpctl api GET /v1/me
```

Tests:

```bash
docker compose up -d postgres
export EACP_TEST_ADMIN_DSN="postgres://postgres:postgres@127.0.0.1:55432/postgres?sslmode=disable"
go test -race ./...
EACP_COMPOSE_TEST=1 go test -count=1 ./test/security/
docker build -f sidecars/agt-pdp/Dockerfile --target test .   # sidecar + conformance through ACS/OPA
```

Demo (an isolated stack on ports 18080 and 55433, removed afterwards):

```bash
scripts/demo.sh
```

Compose credentials are local-development defaults only.

## Contributing

Read [AGENTS.md](AGENTS.md) first.
