[English](FEATURES.md) | [ไทย](FEATURES.th.md)

# Features

This page lists what EACP does today, phase by phase, with the evidence behind each capability: the tests that
enforce it, the API routes and commands that expose it, and the ADR that decides it. The [README](../README.md)
explains the ideas; this page is the inventory. [INVARIANTS.md](INVARIANTS.md) maps each guarantee of MASTER_PLAN
§103 to its tests.

## At a glance

| Phase | What it adds | Decision |
|---|---|---|
| 1–8 (Slice A) | Registry, identity and capability; governance and durable approvals; the Action API and atomic release boundary; leases, fencing and the dispatch intent; the HTTP connector and Fake ERP; `UNKNOWN_OUTCOME`, reconciliation and human resolution; hardening and the demo | [ADR-001](adr/ADR-001-product-boundary-and-enforcement-point.md), [ADR-003](adr/ADR-003-agent-registry-identity-and-capability.md), [ADR-004](adr/ADR-004-action-state-machine-and-execution-semantics.md), [ADR-005](adr/ADR-005-approval-ownership-and-atomic-execution-boundary.md) |
| 9 | The AGT sidecar PDP | [ADR-002](adr/ADR-002-agt-integration-sidecar-pdp.md) |
| 10 | NATS JetStream signals | [ADR-014](adr/ADR-014-postgresql-authority-nats-signals.md) |
| 11 | Hard budget reservation | [ADR-012](adr/ADR-012-budget-reservation.md) |
| 12 | The fair scheduler | [ADR-011](adr/ADR-011-scheduler-fairness.md) |
| 13 | Backpressure, bulkheads, circuit breakers and retry budgets | [ADR-022](adr/ADR-022-backpressure-bulkheads-circuit-breakers-retry-budgets.md) |
| 14 | The MCP registry and tool fingerprints | [ADR-023](adr/ADR-023-mcp-registry-and-tool-fingerprint.md) |
| 15 | The dependency graph and blast radius | [ADR-015](adr/ADR-015-dependency-graph.md) |
| 16 | Distributed execution kills | [ADR-016](adr/ADR-016-distributed-kill-switch.md) |
| 17 | Fleet operations | [ADR-024](adr/ADR-024-fleet-operations.md) |
| 18 | Agent FinOps | [ADR-025](adr/ADR-025-agent-finops.md) |
| 19 | Release and evaluation | [ADR-018](adr/ADR-018-release-and-evaluation.md) |
| 20–21 | Governance-as-Code | [ADR-026](adr/ADR-026-governance-as-code.md) |
| 22 | Incidents, the Agent SOC and the operator console | [ADR-027](adr/ADR-027-incidents-and-agent-soc.md), [ADR-028](adr/ADR-028-operator-console.md) |
| 23 | High availability and Kubernetes | [ADR-029](adr/ADR-029-high-availability.md) |
| 24a–24g | Just-in-time credentials | [ADR-019](adr/ADR-019-credential-custody.md) |
| 25a | Governed A2A delegation | [ADR-030](adr/ADR-030-a2a-delegation.md) |
| 25b | The LLM gateway | [ADR-031](adr/ADR-031-llm-gateway.md) |
| §104 | The load benchmark | [BENCHMARKS.md](BENCHMARKS.md) |

## Slice A (Phases 1–8): the foundation and the action path

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
| Registry: principals, two-person role grants, groups, agents, versions, allowlists, connectors, tools, contracts ([ADR-003](adr/ADR-003-agent-registry-identity-and-capability.md)) | `internal/registry` (schema tests run raw SQL as the app role; triggers mutation-checked) |
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
| Slice A, Slice C, A2A, JIT credential and LLM gateway demos on an isolated stack (§111, §96) | `scripts/demo.sh`, [DEMO.md](DEMO.md) |

## Slice B (Phase 9): the AGT sidecar PDP

| Capability | Evidence |
|---|---|
| Sidecar PDP wrapping the pinned AGT 5.0.0 policy layer, the ACS 0.3.1b1 engine and OPA 1.20.2; stateless, never resolves an approval | `sidecars/agt-pdp`, [research/REFERENCES.md](../research/REFERENCES.md) |
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
| PostgreSQL chooses tenant/team weighted turns; teams pin their group weight and contract priority at release, with aging and deadline promotion within a team. NATS remains a wake-up hint. | [ADR-011](adr/ADR-011-scheduler-fairness.md), `TestSchedulerServesTenantWithSmallerBacklog`, `TestSchedulerUsesPinnedTeamWeight`, `TestSchedulerPriorityAndAging` |
| T14 serializes and enforces `max_inflight` for a connector or named group, including raw SQL, concurrent claims and stale transaction snapshots. | `TestConnectorCapacityIsEnforcedForRawClaims`, `TestConnectorCapacitySerializesConcurrentClaims`, `TestConnectorCapacityRejectsStaleRepeatableReadClaim`, `TestNamedCapacityGroupSpansConnectorsAndStateIsTenantIsolated` |
| With backlogs of 10,000, 100 and 100 actions, each small team gets 10 of the first 30 claims. | `BenchmarkSchedulerFairness` |

## Slice B (Phase 13): backpressure, bulkheads, circuit breakers and retry budgets

| Capability | Evidence |
|---|---|
| Admission answers 429 with a `scope` and creates nothing: global and tenant queues, a tenant's unreleased actions (`EACP_ACTION_MAX_PENDING_PER_TENANT`), and a connector group's queue (contract `max_queued`). A full connector queue still admits other connectors. | [ADR-022](adr/ADR-022-backpressure-bulkheads-circuit-breakers-retry-budgets.md) §1, `TestAdmissionBoundsPendingAndConnectorQueues`, `TestAdmissionLimitIs429` |
| No connector failure starves another pool (§103 invariant 9): a worker holds at most `EACP_WORKER_GROUP_CONCURRENCY` actions per capacity group, and `max_inflight` bounds the group across workers. | `TestConnectorFailureDoesNotStarveUnrelatedConnectors` |
| A breaker per worker and connector opens after `EACP_WORKER_BREAKER_FAILURES` failures, probes once after its cooldown and doubles the cooldown on each consecutive trip. It also opens the connector's shared circuit, journaled; while that is open, PostgreSQL refuses the claim (T14) and the dispatch intent (T16). Operators disable and enable a connector (`eacpctl connector disable\|enable`). | `TestBreakerOpensProbesAndCloses`, `TestLocalBreakerHoldsWithoutTheSharedCircuit`, `TestOpenCircuitRefusesClaimAndDispatch`, `TestDisableSerializesWithAStaleDispatchIntent`, `TestCircuitChangesAreGuardedAndJournaled` |
| Retries back off exponentially with jitter and stay within the contract's retry budget: attempts, `retry_max_elapsed_ms` since the first dispatch, and `retry_max_cost`. PostgreSQL answers the budget question for the worker, sweeper, reconciler and operator retries alike. | `TestDefaultBackoffIsJitteredWithinBounds`, `TestRetryBudgetBoundsElapsedTime`, `TestRetryBudgetBoundsRetryCost` |

## Slice C (Phase 14): MCP registry and tool fingerprint

| Capability | Evidence |
|---|---|
| An MCP server is a connector with protocol `mcp`; its tools are discovered, never declared. The execution worker's scanner lists them over Streamable HTTP (2026-07-28, falling back to the initialize-based 2025 revisions; JSON and SSE) with the worker-held, host-bound credential, every `EACP_MCP_SCAN_INTERVAL`, and bounds pages, tools and bytes. | [ADR-023](adr/ADR-023-mcp-registry-and-tool-fingerprint.md) §1–§2, `internal/connector/mcp` (interop with the official MCP Go SDK), `TestScannerDiscoversToolsAndQuarantinesDrift` |
| Scans are fenced by a scan lease (worker id, generation, expiry) in PostgreSQL; a stale scanner records nothing, and a failed scan changes no tool. | `TestScanLeaseFencesStaleScanners`, `TestStaleScannerCannotRecord`, `TestScannerRecordsFailuresWithoutChangingTools` |
| PostgreSQL fingerprints each canonical (RFC 8785) definition, keeps the history, and classifies a change: display-only (`title`, `icons`) is low risk, anything else high. Server annotations are untrusted hints that can only tighten. | `TestScanRecordsDefinitionsWithDatabaseFingerprints`, `TestDefinitionChangesAreClassifiedAndInvalidateContracts`, `TestMCPContractRules` |
| A contract pins the reviewed definition. A high-risk change, or a certified tool that disappears, quarantines the tool and its contract stops matching; an approved action is denied at release. Quarantine is containment (operator or registry approver); release needs a second registry approver and never recertifies. | `TestMCPDefinitionDriftBeforeReleaseDenies`, `TestMissingCertifiedToolIsQuarantined`, `TestToolQuarantineRules` |
| Operators see tools, definition history and scans, request a rescan, and quarantine or release a tool. | `GET /v1/connectors/{id}/tools`, `GET /v1/connectors/{id}/mcp[/scans]`, `POST /v1/connectors/{id}/mcp/scan`, `GET /v1/tools/{id}[/definitions]`, `POST /v1/tools/{id}/quarantine\|release`; `eacpctl connector tools\|mcp\|scans\|scan`, `eacpctl tool` |

Calling MCP tools (`tools/call`) is Phase 26a ([ADR-032](adr/ADR-032-mcp-tools-call.md)): the execution worker sends at most one `tools/call` per action, after listing the server's tools and finding the tool's definition byte-equal to the certified one (`definition_changed`, `tool_missing` and `definition_unverified` send nothing). Tools that use `x-mcp-header` are refused (`unsupported_header_mirroring`). A success's reference is `mcp:sha256:<digest of the result>`; the tool's output is never journaled or logged, and it is kept for the calling agent only when the contract opts in (Phase 26b below). An `isError` result, invalid output, `input_required` and a transport failure after the send are `UNKNOWN_OUTCOME` unless the class is certified. Evidence: `TestTheWorkerCallsAnMCPTool`, `TestAChangedDefinitionBetweenScansIsNeverCalled`, `TestAnMCPActionIsNeverRetried`, `TestNothingOfTheOutputIsPersisted`, `TestAToolWithHeaderMirroringIsRefused`.

## Slice C (Phase 15): dependency graph and blast radius

| Capability | Evidence |
|---|---|
| Active allowlists give Agent→Tool and Agent→MCP paths; tools belong to MCP servers. Editors record Agent→Model, Agent→MCP, Agent→Agent and Tool→System observations with source, expiry and confidence. The database guards and audits every write. | [ADR-015](adr/ADR-015-dependency-graph.md), `TestDependencyWritesAreGuardedAndTenantScoped` |
| PostgreSQL recursive CTEs return confirmed and possible affected agent versions. Stale and unknown evidence widens the possible set; coverage is explicitly `observed_only`. | `TestBlastRadiusTraversesRegistryAndDeclaredDependencies`, `TestBlastRadiusWidensForUnknownOrStaleEvidence` |
| Operators and auditors query `GET /v1/dependencies/blast-radius?kind=mcp&id=<uuid>` or `eacpctl dependency blast-radius mcp <uuid>`. Editors record with `POST /v1/dependencies` and revoke with `POST /v1/dependencies/{id}/revoke`. For model/system targets, use `name=<tenant-local-name>` in place of `id`. | `TestDependencyAPIRecordsAndReportsBlastRadius`, `TestDependencyBlastRadiusCommand` |

The report counts recent **actions**, not workflow runs; workflow identity is not yet in the registry. Graph evidence does not change execution authorization or trigger containment.

## Slice C (Phase 16): distributed execution kills

Operators use `POST /v1/killswitch` with `{"scope":"agent_version","target_id":"<uuid>","killed":true,"reason_code":"security_incident","reason":"<incident note>"}`. `reason_code` defaults to `operator_request` and accepts the four [AGT reason codes](adr/ADR-016-distributed-kill-switch.md). A different operator resumes the scope with `killed:false`. Operators and auditors list states with `GET /v1/killswitch`. The CLI provides `eacpctl kill activate|resume <scope> <uuid> --reason <text> [--code <reason-code>]` and `eacpctl kill list`.

The enforceable scopes are `tenant`, `team`, `agent`, `agent_version`, `action`, `connector`, `tool` and, since Phase 25b, `model`, which names a tenant LLM model and stops LLM calls only. PostgreSQL rejects a killed scope at claim and dispatch intent; workers check again before the external call and during it. NATS only wakes that check. A kill during execution records `UNKNOWN_OUTCOME` for reconciliation because cancellation does not undo an external effect. `global` and `run` are rejected until EACP has platform operator authority and authenticated run action bindings; see ADR-016.

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

Model calls that go through the LLM gateway (Phase 25b) are metered and priced by the gateway. For calls made outside it, EACP ingests the cost: an agent runtime points its OTLP/HTTP trace exporter at `<api>/v1/agent/otlp` with its agent key, as JSON (`OTEL_EXPORTER_OTLP_PROTOCOL=http/json`), gzip or not. Spans carrying OpenTelemetry GenAI usage (`gen_ai.operation.name` plus `gen_ai.usage.*_tokens`) are recorded against the key's agent and priced in PostgreSQL. Other spans are ignored.

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

## Phases 20–21: Governance-as-Code

A platform team can keep the governed registry in Git ([ADR-026](adr/ADR-026-governance-as-code.md)). A bundle is a
directory with `eacp.yml` and `resources/*.yml`: connectors, tools and contracts, agents with their versions and
allowlists (Phase 20), and principals, role grants, groups, policies, budgets and prices (Phase 21). Targets bind a
bundle to an API and a tenant, and variables are resolved by `eacpctl` before one JSON document is sent. A bundle
never carries a secret value.

- **Plan.** `eacpctl bundle plan` (or `POST /v1/change-sets` with `dry_run`) compares the bundle with the registry in
  one snapshot and answers with ordered steps, each in the `submit` or the `approve` stage. PostgreSQL computes the
  digests of the desired state and of the registry it read.
- **Deploy and approve.** `eacpctl bundle deploy` records the change set and submits it: the submit stage runs as the
  submitter through the same registry transactions as the API. A second person approves it
  (`eacpctl bundle approve`), and the approve stage activates proposals and moves versions. A change set whose
  registry moved on is stale and runs nothing.
- **Nothing is deleted.** With `--prune`, what the bundle no longer declares is retired or revoked through an existing
  lifecycle move; a version is never activated beside another `ACTIVE` one (use a release), containment is never
  undone and MCP tools are never declared.
- **Drift.** `eacpctl bundle drift` compares the registry with the bundle's last applied change set and reports each
  address as `in_sync`, `modified`, `missing` or `unmanaged_reference`. It never repairs anything.

[Example 03](../examples/03-governance-as-code/README.md) runs the whole cycle.

## Phase 22: Agent SOC and the operator console

- **Incidents.** Every `EACP_INCIDENT_INTERVAL` (default 15s) the control plane opens incidents from existing signals: MCP drift, kills, open circuits, unknown outcomes, canary rollbacks and FinOps overspend, each with its blast radius. Operators acknowledge, assign, note, link and resolve them (`/v1/incidents`, `eacpctl incident`); a critical incident is resolved by a second person. `GET /v1/soc/summary` (`eacpctl soc summary`) serves the SOC counters.
- **Console.** Open `http://localhost:8080/ui/` and sign in with a principal API key. The key stays in the tab's memory only; a reload or 30 minutes without activity signs you out. The console shows the overview, incidents, security (kills, circuits, quarantined tools), fleet, approvals, execution, inventory, dependencies and cost, and runs the incident lifecycle and containment through the same API, behind a confirm dialog. It holds no authority of its own (ADR-028). Set `EACP_UI=off` to disable it.

## Phase 23: running several replicas and Kubernetes

Run as many `controlplane-api` and `execution-worker` replicas as you like against one database: there is no leader, and PostgreSQL arbitrates every background loop (ADR-029). The incident, FinOps and release evaluators skip a tenant another replica is evaluating, the sweeper tolerates actions another replica moved first, and each worker needs a unique `EACP_WORKER_ID` (the host name by default). Set `EACP_SHUTDOWN_DELAY` (0s–60s, default 0s) behind a load balancer: on SIGTERM the service fails `/readyz`, closes kept-alive connections after each response, stops its loops and keeps serving for that long before shutting down. `internal/worker` `TestReplicasShareTheWorkAndSurviveLosingOne` runs three API loop sets and three workers and stops one of each mid-run.

On Kubernetes, install the Helm chart `deployments/helm/eacp` (Phase 23b, ADR-029 Rev 1.1): hardened, replicated Deployments for the API, the worker and the PDP, the compose network boundary as NetworkPolicies, Secrets referenced by name only, migrations as a hook, disruption budgets and optional CPU autoscaling. PostgreSQL and NATS stay external. See [docs/KUBERNETES.md](KUBERNETES.md); `bash scripts/k8s-e2e.sh` proves it on a 2-node minikube cluster.

## Phase 24a: just-in-time credentials

A connector credential no longer has to be a static secret (ADR-019). An entry of the worker's connector-secrets file may name an OAuth 2.0 client-credentials provider instead of a `value`:

```json
{"tenant_id": "…", "secret_ref": "erp-jit", "host": "erp.internal:8443",
 "oauth2": {"token_url": "https://idp.internal/oauth2/token", "client_id": "eacp-worker",
            "client_secret_file": "/run/secrets/idp", "scope": "erp.purchase"}}
```

The worker mints a Bearer token (it must expire within an hour) just before a call, reuses it only while it outlives the whole call plus 30 s, and never stores it. If the token endpoint fails, nothing is dispatched and the worker stops claiming that binding's work during a 1–60 s back-off. Tokens and client secrets are redacted from every log. Only the worker holds any of this; agents, the API and PostgreSQL never do. `DEMO=J scripts/demo.sh` runs the JIT demo.

## Phase 24b: workload identity federation

The OAuth provider can authenticate without any client secret (ADR-019 Rev 1.1). Name a JWT that the platform issues and rotates instead, such as the Kubernetes service-account token the chart projects into the worker:

```json
{"tenant_id": "…", "secret_ref": "erp-wif", "host": "erp.internal:8443",
 "oauth2": {"token_url": "https://login.microsoftonline.com/<tenant>/oauth2/v2.0/token",
            "client_id": "<application id>", "client_assertion_file": "/run/secrets/eacp-identity/token",
            "scope": "api://erp/.default"}}
```

The worker reads the file at every mint and sends it as an RFC 7523 client assertion, the request Entra ID documents for a federated credential. An unreadable, malformed or expiring assertion backs the binding off like a failing token endpoint. Tokens may live up to a day (Entra ID issues 60–90 minutes) but are used for at most an hour. With `worker.workloadIdentity.enabled`, the Helm chart projects the token into the worker pod only, under the worker's own ServiceAccount; see [docs/KUBERNETES.md](KUBERNETES.md). `scripts/k8s-e2e.sh` runs `TestFederatedJITDemo`, which buys with tokens minted against the cluster's real service-account issuer.

## Phase 24c: private_key_jwt

Where no platform issues the worker an identity (VMs, compose, on-premises hosts), the worker can sign its own client assertions with a private key (ADR-019 Rev 1.2). The IdP registers only the public half, as a certificate (Entra ID) or a JWKS (Okta, Keycloak):

```json
{"tenant_id": "…", "secret_ref": "erp-pkjwt", "host": "erp.internal:8443",
 "oauth2": {"token_url": "https://login.microsoftonline.com/<tenant>/oauth2/v2.0/token",
            "client_id": "<application id>", "scope": "api://erp/.default",
            "private_key_jwt": {"alg": "PS256", "key_file": "/run/secrets/idp/key.pem",
                                "certificate_file": "/run/secrets/idp/cert.pem"}}}
```

Every token request carries a new assertion, valid for five minutes, with a unique `jti` (RS256, ES256, or PS256 with the certificate's `x5t#S256` as Entra ID expects). The key and certificate are read once at startup and checked there, except the certificate's validity period: an expired certificate fails that binding's mints (with back-off) rather than the whole worker. The key is redacted from logs and never sent to a connector. On Kubernetes, put the PEM inline (`key`, `certificate`) in the worker's secrets file. `eacpctl dev-client-key` writes a development key, certificate and JWKS; `DEMO=J scripts/demo.sh` also runs `TestPrivateKeyJWTDemo`.

## Phase 24d: credentials from Vault

Any credential in the worker's secrets file can live in HashiCorp Vault KV v2 instead (ADR-019 Rev 1.3): a static token (`value_vault`), an OAuth client secret (`client_secret_vault`) or a `private_key_jwt` key and certificate (`key_vault`, `certificate_vault`). The worker logs in with its own identity and reads each value when it needs it:

```json
{"vault": {"address": "https://vault.internal:8200", "refresh_seconds": 300,
           "auth": {"kubernetes": {"role": "eacp-worker", "jwt_file": "/run/secrets/eacp-vault-identity/token"}}},
 "secrets": [{"tenant_id": "…", "secret_ref": "erp", "host": "erp.internal:8443",
              "value_vault": {"path": "eacp/erp", "key": "token"}}]}
```

On Kubernetes, `worker.vaultIdentity.enabled` projects a token with audience `vault` into the worker pod only; elsewhere use `"approle": {"role_id_file": …, "secret_id_file": …}`. The worker starts with Vault down and never contacts it at load. Values are cached for `refresh_seconds` (30–3600) and never served stale: a rotation in Vault takes effect within one interval, or at once after the target rejects the old value, without a restart. A Vault failure holds only the bindings that need it in `QUEUED`, with the usual back-off. Vault tokens and values are redacted from every log and never stored. `DEMO=J scripts/demo.sh` also runs `TestVaultDemo` against a dev-mode Vault with AppRole; `scripts/k8s-e2e.sh` runs it with Kubernetes auth.

## Phase 24e: SPIFFE JWT-SVIDs

Where SPIRE attests the worker, a binding can hold no secret at all (ADR-019 Rev 1.4): the worker asks the SPIRE agent for its JWT-SVID for the audience the binding names, and presents it straight to a SPIFFE-aware target as the Bearer (`value_spiffe`) or as the client assertion of an OAuth mint (`client_assertion_spiffe`):

```json
{"spiffe": {"endpoint": "unix:///spiffe-workload-api/spire-agent.sock",
            "spiffe_id": "spiffe://example.org/ns/eacp/sa/eacp-worker"},
 "secrets": [{"tenant_id": "…", "secret_ref": "erp", "host": "erp.internal:8443",
              "value_spiffe": {"audience": "erp-api"}}]}
```

On Kubernetes, `worker.spiffe.enabled` mounts the Workload API socket into the worker pod only, through the SPIFFE CSI driver. The worker starts with the agent down and never contacts it at load; it refuses an SVID for any identity but `spiffe_id`, never sends one that could expire during the call, and withholds only that binding's work with the usual back-off while the agent is unreachable or has no entry for it. Give the worker's registration entry a JWT-SVID TTL of at least 2.5 × (the longest call budget + 30 s), since the agent hands out a cached SVID until about half its life. Deleting the entry revokes nothing already issued: the worker may keep using its SVID for up to the full TTL, so contain with a kill. SVIDs are redacted from every log and never stored. `scripts/k8s-e2e.sh` installs a development SPIRE and runs `TestSPIFFEDemo`.

## Phase 24f: token exchange

A binding can trade the worker's own identity for an access token (ADR-019 Rev 1.5): an RFC 8693 token exchange of its projected service-account token or its JWT-SVID at an STS, optionally followed by GCP service-account impersonation. This is GCP Workload Identity Federation, and Keycloak or Okta token exchange:

```json
{"tenant_id": "…", "secret_ref": "gcs", "host": "storage.googleapis.com:443",
 "oauth2": {"grant": "token_exchange", "token_url": "https://sts.googleapis.com/v1/token",
            "audience": "//iam.googleapis.com/projects/123/locations/global/workloadIdentityPools/eacp/providers/k8s",
            "scope": "https://www.googleapis.com/auth/cloud-platform",
            "subject_token": {"file": "/run/secrets/eacp-identity/token"},
            "impersonate": {"url": "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/eacp@p.iam.gserviceaccount.com:generateAccessToken",
                            "scope": ["https://www.googleapis.com/auth/devstorage.read_only"]}}}
```

The subject token is read again (or fetched from the SPIRE agent) at every mint and goes only to the STS; client authentication there is optional. The exchange must return an access token (`issued_token_type`). With `impersonate`, the federated token goes only to the impersonation endpoint and is never cached; the service account's token is the one the connector receives. The final token follows every Phase 24a rule, and any failed step withholds only that binding with the usual back-off. Subject and federated tokens are redacted from every log and never stored. `scripts/k8s-e2e.sh` runs `TestTokenExchangeDemo` against Fake ERP's STS.

## Phase 24g: AWS STS and SigV4

AWS issues no Bearer tokens, so an `aws` binding (ADR-019 Rev 1.6) assumes an IAM role with the worker's projected token or JWT-SVID (`AssumeRoleWithWebIdentity`) and signs every call to the connector with the temporary keys (SigV4), as API Gateway with IAM authorisation, Lambda function URLs and AWS APIs require:

```json
{"tenant_id": "…", "secret_ref": "erp-aws", "host": "abc123.execute-api.eu-west-1.amazonaws.com:443",
 "aws": {"role_arn": "arn:aws:iam::123456789012:role/eacp-worker", "region": "eu-west-1", "service": "execute-api",
         "subject_token": {"file": "/run/secrets/eacp-identity/token"}}}
```

The STS endpoint defaults to the regional one; keys are requested for at most an hour and used for at most an hour. The HTTP connector signs execute and lookup with the pinned `aws-sdk-go-v2` signer after every other header is set. The secret key never leaves the worker and the session token rides only on signed calls to the bound host; both are redacted from every log and never stored, while the access key id is logged so it can be matched in CloudTrail. MCP servers are not signed. `scripts/k8s-e2e.sh` runs `TestAWSDemo` against Fake ERP's AWS STS. This completes Phase 24.

## Phase 25a: governed A2A delegation

An agent can hand work to a remote agent that speaks [A2A 1.0](https://a2a-protocol.org) without holding its credential or reaching it ([ADR-030](adr/ADR-030-a2a-delegation.md)). Register the remote agent as a connector whose endpoint is its JSON-RPC interface URL:

```bash
eacpctl connector register --name procurement --protocol a2a --endpoint https://agents.example.com/a2a --secret-ref procurement-agent
```

The worker's scanner reads the Agent Card (`/.well-known/agent-card.json`) with the worker-held credential and discovers one tool, `delegate`, certified over the whole card: a new skill or any other change except the icon and documentation URLs quarantines it. A `delegate` contract has no idempotency and no lookup, so each delegation is sent once. The worker sends `SendMessage` (the action id is the `messageId`), follows the task with `GetTask`, and cancels a task it stops following (the call ran out of time, was killed, or the remote agent asked for input). Such an outcome is unknown and goes to a human, with the remote task id in the action's evidence. Only ids, states and digests are kept, never the remote agent's output. `DEMO=D scripts/demo.sh` runs the A2A demo.

## Phase 25b: the LLM gateway

An agent can call a model through EACP with its unmodified Anthropic or OpenAI SDK, using its own EACP agent key as the API key ([ADR-031](adr/ADR-031-llm-gateway.md)). Point the SDK's base URL at `llm-gateway` (`:8083`); it serves `POST /v1/messages` and `POST /v1/chat/completions`, streamed or not. Declare each model and the provider key's name, then add it to an agent version's allowlist (`"models": ["sonnet"]` beside `"tools"`; a second person activates it):

```bash
eacpctl llm-model register --name sonnet --provider anthropic --base-url https://api.anthropic.com \
  --upstream claude-sonnet-4-5 --secret-ref anthropic --max-output-tokens 64000
```

Every call is decided in PostgreSQL before it is sent: the model must be on the allowlist, no kill scope may match (an operator can now kill a `model`), and a hard budget reservation must fit at the rate card's price. The gateway alone holds provider keys (`EACP_LLM_SECRETS_FILE`, the worker's manifest format), cuts a killed call within seconds, and settles the provider's reported usage at PostgreSQL's price. `eacpctl llm-calls list` shows the ledger: tokens, cost and outcome, never content. `DEMO=L scripts/demo.sh` runs the LLM gateway demo.

The worker registers the Phase 6 HTTP connector and runs the Phase 7 reconciler. Fake ERP requires a credential for privileged calls and keeps its operation log in a durable Compose volume.

## Phase 26b: the result channel

A contract may keep a successful call's output for the agent that made it, for 60 seconds to a day (`result_retention_seconds`; [ADR-034](adr/ADR-034-result-channel.md)). Turning it on is a new contract version, so a second person activates it. HTTP returns the execute response's `result`, MCP the whole `CallToolResult` (its digest is the reference) and A2A the task's artifacts.

| Capability | Evidence |
|---|---|
| Only a success is kept, recorded by the lease holder in the transaction that moves the action to `SUCCEEDED`; PostgreSQL computes the digest, size and expiry | `migrations/00026_result_channel.sql`, `TestOnlyTheLeaseHolderRecordsASuccess`, `TestASuccessKeepsItsOutputForTheAgent`, `TestAKilledSuccessKeepsNoOutput` |
| Only an `ACTIVE` version of the calling agent reads the content (`GET /v1/actions/{id}/result`); operators see its size and digest in the evidence | `TestOnlyTheActionsAgentReadsTheResult`, `TestResultContentIsNotSelectable`, `TestTheCallingAgentReadsItsResult` |
| Bounded: RFC 8785 JSON up to 64 KiB; an output with a worker-held credential or over the limit is withheld and the action still succeeds | `TestPrepareOutput`, `TestACredentialInTheOutputIsWithheld`, `TestAnOversizedOutputIsWithheldAndTheActionSucceeds`, `TestAWithheldResultSaysWhy` |
| Time-limited: not served after it expires, and the sweeper clears the content once | `TestTheSweeperClearsExpiredResultsOnce`, `TestPruneNeedsTheSweeper` |
| Never in a log, the journal or a message | `TestASuccessKeepsItsOutputForTheAgent` |

## Phase 27a-1: Agent Studio's rules

An employee with the `studio_author` role saves an agent definition: inputs, `tool_call` steps and a closing `respond` step ([ADR-033](adr/ADR-033-agent-studio-and-runtime-credentials.md)). PostgreSQL validates it, computes its digest and the tools it needs, and creates a registry agent the employee owns, a `REGISTERED` version and an allowlist holding exactly those tools. A `registry_approver` who is not the author approves or rejects the request, and approval replaces the agent's active version. The runtime that will run Studio agents (Phase 27a-2) holds only `studio_runtime`.

| Capability | Evidence |
|---|---|
| The definition is checked in PostgreSQL: schema version 1, at most 10 string inputs, 1 to 20 steps, known tools, placeholders only for declared inputs and earlier steps, no value that looks like a secret, at most 64 KiB | `migrations/00027_studio.sql`, `TestTheDefinitionRules` |
| The allowlist is exactly the derived capability; nobody widens it or activates it outside a decision | `TestTheCapabilityIsTheSortedDistinctTools`, `TestTheRegistryCannotWidenAStudioAgent` |
| An author writes registry rows only inside a Studio save, only for their own agent and only in their department | `TestAStudioAuthorWritesNoRegistryRowDirectly`, `TestOnlyAStudioAuthorInTheDepartmentSaves`, `TestOnlyTheOwnerAddsAVersion` |
| A second person decides, once; approval retires the previous version; a waiting version only | `TestTheOwnerAndNonApproversCannotDecide`, `TestApprovalActivatesTheVersionAndReplacesThePreviousOne`, `TestRejectionRetiresTheVersion`, `TestOnlyAWaitingVersionIsApproved` |
| `studio_runtime` is held alone by a service principal and proposes agent keys only for approved Studio versions | `TestStudioRuntimeIsHeldAlone`, `TestStudioRolesKeepTheirPrincipalKind`, `TestTheRuntimeProposesCredentialsOnlyForApprovedStudioVersions` |
| The API (`/v1/studio/...`) shows each version's stage and who acts next; the approver's queue describes the tools in plain words | `TestStudioAuthorsSaveAndApproversDecide` |

## Phase 27a-2: `agent-runtime`, runs and derived keys

A member of an agent's department runs an approved Studio agent with its inputs (`POST /v1/studio/agents/{id}/runs`). The new `agent-runtime` service claims the run through the API, acts as the agent's version with a key it derives from a master secret only it holds, and sends each `tool_call` through the ordinary action path with the employee as the subject ([ADR-033](adr/ADR-033-agent-studio-and-runtime-credentials.md) Rev 1.2). Policy, approvals, budgets and kill scopes apply as to any agent. The employee reads the answer for one hour. The runtime proposes each version's key, and its successor 30 days before expiry, and a `registry_approver` approves it; an operator can revoke every Studio key at once.

| Capability | Evidence |
|---|---|
| Without a Hub listing only the owner starts a run (Phase 27b); its inputs are exactly the declared ones | `TestARunIsStartedByItsOwnerWithItsInputs`, `TestStudioRunsThroughTheAPI` |
| One runtime holds a run at a time; only the lease holder moves it | `TestOnlyTheLeaseHolderMovesARun`, `TestTwoRuntimesNeverShareARun`, `TestRuntimeRoutesAreForTheRuntimeOnly` |
| Each step is the run's own action, under `studio:<run>:<index>`; after a crash the same action is resumed, never sent twice | `TestAStepIsTheRunsOwnAction`, `TestACrashBeforeTheStepRecordResubmitsTheSameAction`, `TestARunEndToEnd` |
| A run fails closed with a named reason: a missing, expired or revoked key, a denied step, a replaced version, the deadline | `TestARunFailsClosedWithoutAKey`, `TestARevokedKeyFailsTheRun`, `TestADeniedStepFailsTheRun`, `TestAReplacedVersionStopsTheRun`, `TestAStepAwaitingApprovalFailsAtTheDeadline`, `TestTheSweeperExpiresStudioRuns` |
| Placeholders take the declared inputs and earlier outputs from the result channel; a missing value is never sent | `TestRenderSubstitutesInputsAndOutputs` |
| The answer is read only by the requester, for one hour; inputs and answer are never journaled | `TestOnlyTheRequesterReadsTheAnswerBeforeItExpires`, `TestRunsAreJournaledWithoutInputsOrAnswer` |
| Keys are derived from a master only the runtime holds; no key or master appears in a response, log, row, journal or message | `TestKeyFromSecretAuthenticatesUnchanged`, `TestOnlyTheRuntimeHoldsTheStudioMaster`, `TestTheRuntimeRefusesAWeakMaster`, `TestNoKeyOrMasterLeaks` |
| The runtime proposes keys and successors and never approves; an operator revokes every Studio key; an expiring key opens an incident | `TestRotationProposesASuccessorAndNeverApproves`, `TestKeysAreDueBeforeTheyExpire`, `TestTheBulkRevocationRevokesOnlyStudioKeys`, `TestOperatorsRevokeEveryStudioKey`, `TestAnExpiringStudioKeyOpensAnIncident` |

## Phase 27a-3a: packaging `agent-runtime` and the Studio demo

`agent-runtime` ships in the image and the release binaries. In compose it runs under the profile `studio` on the `agents` network only; in Helm, `studio.enabled` adds it with its own ServiceAccount and NetworkPolicy and two Secrets by name ([docs/KUBERNETES.md](KUBERNETES.md)). `DEMO=S scripts/demo.sh` takes an HR leave-balance agent from its template through approval to a run ([docs/DEMO.md](DEMO.md#agent-studio-demo)).

| Capability | Evidence |
|---|---|
| The runtime reaches only the API and holds no database URL, connector or provider secret; only it mounts the Studio master | `TestTheRuntimeReachesOnlyTheAPI`, `TestOnlyTheRuntimeHoldsTheStudioMaster` (compose), `TestTheRuntimePodIsHardened`, `TestTheRuntimeReachesOnlyTheAPIInTheCluster` (Helm) |
| The chart refuses a runtime without its Secrets or with a secret, database URL or chart-set variable in `studio.env` | `TestTheRuntimeNeedsItsSecrets`, `TestRuntimeEnvIsValidated`, `TestTheRuntimeIsOffByDefault` |
| Fake MCP answers only the tools it lists; `get_leave_balance` returns structured content | `TestAToolOutsideTheListIsRefused`, `TestLeaveBalanceAnswersStructuredContent`, `TestLeaveBalanceErrIsAToolError` |
| From a template to an answer, with a second approver, department-only runs, fail-closed keys and no secret or answer leaked | `TestStudioDemo` |

## Phase 27a-3b: the Agent Studio page

`/studio/` is a second page built from the console's modules and rules ([ADR-028](adr/ADR-028-operator-console.md) Rev 1.2). An employee starts from the leave-balance template, saves an agent into one of their groups, sees in plain words where it is and who acts next, and runs it; a registry approver decides requests and the runtime's keys in the same page. English and Thai ([user guide](USER_GUIDE.md#for-employees-agent-studio)).

| Capability | Evidence |
|---|---|
| The page is the console's files under the console's rules; each page serves only its own HTML and signs in on its own | `TestServesTheStudioWithItsHeaders`, `TestEachPageServesOnlyItsOwnHTML`, `TestIndexLoadsOnlyTheConsole`, `TestConsoleUsesNoDangerousSinks`, `TestEveryConsoleCallIsARealRoute` |
| The template is the demo's fixture, and the form builds exactly the server's definition | `the leave-balance template is the demo fixture, value for value`, `the form round-trips the template to the same definition`, `the template form saves the fixture definition into the author’s department` (`jstest`) |
| Every status and failure reason reads in plain words, in English and Thai; an unknown one is shown as sent | `every status has a sentence that says who acts next; an unknown one is shown as sent`, `every failure reason the runtime names has its own sentence; an unknown one is shown as sent`, `TestEveryTranslatedTextHasAThaiEntry` |
| An approver never decides their own agent; approving a request or a key goes through a confirmation with a reason | `an approver’s own agent is not offered for their decision`, `an approver approves someone else’s agent with a reason, and a runtime key` (`jstest`) |
| `/v1/me` lists only the caller's active groups | `TestMeListsTheCallersGroups` |

## Phase 27b: the Agent Hub

An agent's owner proposes it to the Hub for their department or the whole organisation; a lead of the department, or an admin or registry approver for the organisation, publishes it ([ADR-033](adr/ADR-033-agent-studio-and-runtime-credentials.md) Rev 1.3). Everyone it reaches finds it, runs it as themselves and can copy it; the copy carries no permission. PostgreSQL decides every step ([user guide](USER_GUIDE.md#share-it-in-the-hub)). The trial with departments (the gate) did not take place and is recorded as not met.

| Capability | Evidence |
|---|---|
| A department lead is set only when an admin adds the membership, and only for a human; it never changes afterwards | `TestALeadIsSetOnlyWhenAnAdminAddsTheMembership`, `TestALeadIsAddedThroughTheAPIAndShownInMe` |
| Only the owner proposes, only an `ACTIVE`, approved version, one proposal at a time | `TestOnlyTheOwnerProposesAnActiveApprovedVersion` |
| Tiered approval: a lead of the agent's department for the department, an admin or registry approver for the organisation, an admin for a template; nobody decides their own | `TestEachScopeHasItsApprover`, `TestTheTemplateTagNeedsAnAdmin` |
| A listing publishes only a version that is still `ACTIVE`; deprecating and withdrawing only narrow | `TestApprovalNeedsTheVersionStillActive`, `TestDeprecateAndWithdrawOnlyNarrow` |
| PostgreSQL shows each listing to its audience only, and runs only through a visible listing | `TestTheHubShowsEachListingToItsAudience`, `TestOthersRunOnlyThroughAVisibleListing` |
| A copy is a new, unapproved agent with no allowlist, key or listing | `TestACloneCarriesNoPermission` |
| Listings and proposals are written only through their functions, and journaled | `TestTheHubIsWrittenOnlyThroughItsFunctions` |
| The API and the page: search, run, copy, propose and decide | `TestTheHubThroughTheAPI`, `the owner proposes a ready agent to the Hub and sees who decides it`, `a lead decides a Hub proposal, and never loads the approvers’ queue` (`jstest`) |
| An HR lead publishes to HR, a colleague runs it, a finance author's copy starts unapproved | `TestStudioDemo` |

## Benchmarks

`scripts/bench.sh` measures the whole stack under open-loop load on an isolated compose stack (MASTER_PLAN §104):
the action path with the local PDP and through the AGT sidecar, from 100 to 10,000 registered agents, and the LLM
gateway against a fake provider. It records throughput, latency percentiles per stage, errors, duplicates and
resource use. The latest run is in [BENCHMARKS.md](BENCHMARKS.md), where every table is generated from committed raw
results; its figures come from one development machine and are not a production capacity claim.
