# Invariants and their tests

Phase 9 (Slice B) adds the AGT sidecar PDP. Its tests join invariants 10, 14 and 18. The sidecar's own suites, including the conformance reference set run through ACS and OPA, run in `docker build -f sidecars/agt-pdp/Dockerfile --target test .`.

Phase 10 (Slice B) adds NATS JetStream for work hints and dashboard events (ADR-014). Its tests join invariants 4 and 8. They run against a JetStream server embedded in the test process, so they never skip for want of NATS.

Phase 11 (Slice B) adds hard budget reservation (ADR-012). It maps the first [B] invariant, 3, and its tests join invariants 8 and 17.

Phase 13 (Slice B) adds backpressure, bulkheads, circuit breakers and retry budgets (ADR-022). It maps the second [B] invariant, 9, and its tests join invariants 6, 8 and 17.

Phase 14 (Slice C) adds the MCP registry: discovery, database-computed tool fingerprints, definition history, risk metadata, contract invalidation and quarantine (ADR-023). §103 tags no invariant [C]. Its tests join invariants 1, 7, 8, 17 and 19, and the full-flow isolation test now also covers the MCP tables. `internal/connector/mcp` checks the discovery client against the official MCP Go SDK (modern and legacy revisions, JSON and SSE).

Phase 15 (Slice C) adds tenant-scoped dependency evidence and conservative blast-radius queries (ADR-015). Its database guards, audit, RLS and read API join invariants 8 and 17. `internal/registry/dependency_test.go` also checks current allowlist paths, delegation cycles, and widened possible impact for stale or unknown evidence.

Phase 16 (Slice C) adds PostgreSQL-authoritative scoped execution kills (ADR-016). Raw T14/T16 checks, in-flight cancellation and ambiguous outcome evidence join invariants 1, 8, 12, 17 and 18. `internal/worker/kill_test.go` also checks monotonic epochs, two-person clear and tenant isolation; `internal/messaging/relay_test.go` checks the transactional signal.

Phase 17 (Slice C) adds fleet operations and the fleet view (ADR-024). An operation is a set of ADR-003 lifecycle transitions that PostgreSQL makes atomically under the version guard. Its tests join invariants 8, 17 and 19. `internal/fleet/schema_test.go` also checks every rule in raw SQL; `internal/fleet/fleet_test.go` checks selection, dry runs, atomicity, rollback and the observed health reasons.

Phase 18 (Slice C) adds Agent FinOps (ADR-025): LLM usage ingest over OTLP/HTTP JSON and billing imports, a forward-only rate card priced in PostgreSQL, chargeback, soft limits, a spend dashboard and alerts. Nothing it adds blocks an action; only invariant 3's hard budgets do. Its tests join invariants 8 and 17. `internal/finops/schema_test.go` also checks key-bound attribution, the observation window, idempotent spans, rounding up and the cache rule, and the evaluator's thresholds and baseline; `internal/finops/service_test.go` checks effective spend as the greater source per day and the account rollup.

Phase 20 adds Governance-as-Code (ADR-026): bundles planned into change sets, two-person submit and approve through the registry triggers, and read-only drift. It grants nothing a registry write through the API could not. Its tests join invariants 8 and 17. `internal/bundle/schema_test.go` also checks every change-set rule in raw SQL; `internal/bundle/plan_test.go` checks the ordered diff, releases, containment, imports and prune; `internal/bundle/service_test.go` checks staleness, atomic stages and drift.

Phase 21 extends it to identity, the tenant policy, budgets and prices (ADR-026 Rev 1.1). Every step writes through the domain Tx types, so the existing triggers decide it, and no change set leaves fewer than two admins. `internal/bundle/schema_phase21_test.go` checks the new kinds, the policy owner, the admin floor at commit and the digest rows in raw SQL; `plan_identity_test.go` and `plan_governance_test.go` check the ordered diff; `service_governance_test.go` checks two-admin application, the admin floor, staleness and drift.

Phase 22a adds incidents and the Agent SOC read model (ADR-027). An incident observes and never decides. Its tests join invariant 17. `internal/incident/schema_test.go` also checks every guard in raw SQL (automatic incidents only from the `incident` system actor, manual ones by operators, the one-move lifecycle, two-person critical resolution, an insert-only timeline, tenant isolation). `evaluate_test.go` checks one incident per signal occurrence, operator containment opening nothing, and the affected snapshot agreeing with `registry.BlastRadius`.

Phase 23a adds high availability without a leader (ADR-029). `internal/worker` TestReplicasShareTheWorkAndSurviveLosingOne (three API loop sets, three workers, one of each stopped mid-run) joins invariants 4 and 16, and `internal/action` TestTwoSweepersReclaimEachLapsedLeaseOnce joins invariant 4. Phase 23b (the Helm chart, ADR-029 Rev 1.1) adds `test/demo` TestKubernetesDisruption on a 2-node minikube cluster (scripts/k8s-e2e.sh); like the demos it needs a running stack, so it is not in the map.

Phase 12 (Slice B) adds fair tenant/team claim order, priority aging and connector capacity (ADR-011). `internal/worker/scheduler_test.go` checks bounded service for small teams and tenants, weight, priority, raw and concurrent capacity claims, and scheduler-state tenant isolation. `BenchmarkSchedulerFairness` covers the 10,000:100:100 backlog.

MASTER_PLAN §82 sets the Slice A exit criterion: every invariant in §103 tagged [A] has an automated test that passes. This page maps each one to the tests that prove it.

`test/invariants` checks the map mechanically. It reads the invariants and their tags from MASTER_PLAN §103, requires a section here for each [A] invariant, allows one for a [B] invariant once its phase has landed, requires each section's tag to match §103, and requires that every test named in a section exists in the named package. `go test -race ./...` with `EACP_TEST_ADMIN_DSN` runs them all. The `test/security` tests also need `EACP_COMPOSE_TEST=1` and the compose stack, and `test/demo` needs `EACP_DEMO=1` (see [DEMO.md](DEMO.md)).

Format: one `## <n> [A]` (or `[B]`) section per invariant, and one list item per test (`` `package` TestName — what it shows ``).

## 1 [A] A stale worker cannot commit, nor cause a second dispatch

- `internal/worker` TestLeaseRaceHasExactlyOneWinner — two claims: one generation, one winner
- `internal/worker` TestResultCommitsAreFencedAndNeedTheAttemptOutcome — the database rejects a result from another generation
- `internal/worker` TestStaleWorkerBeforeIntentNeverDispatches — a reclaimed worker makes no call
- `internal/worker` TestStaleWorkerAfterIntentIsUnknownOutcomeAndLate — a stale result is only late evidence
- `internal/worker` TestHeartbeatExtendsOnlyTheHoldersLiveLease — only the holder extends its lease
- `internal/worker` TestStillUnknownBacksOffAndStaleReconcilersAreFenced — stale reconcilers are fenced out too
- `internal/worker` TestStaleScannerCannotRecord — a scanner whose scan lease was taken over records nothing
- `internal/worker` TestRawDispatchIntentIsDatabaseFencedByKill — raw T16 cannot commit an attempt under a killed scope
- `internal/registry` TestScanLeaseFencesStaleScanners — the database fences scan records by worker, generation and expiry

## 2 [A] One-time approval cannot release two execution claims

- `internal/action` TestApprovalThenParallelReleasesConsumeTheGrantOnce — parallel releases consume one grant once
- `internal/approval` TestConcurrentConsumeSucceedsOnce — concurrent consumption succeeds once
- `internal/approval` TestConsumeBindsDigestAndCanSucceedOnce — a grant binds its digest and is consumed once
- `internal/approval` TestGrantCannotBeConsumedTwiceOrAfterPolicyChange — no second consumption, and none after a policy change

## 3 [B] Hard budgets cannot oversubscribe

- `internal/action` TestConcurrentReleasesNeverOversubscribeAHardBudget — 100 concurrent releases on one leaf that fits 37: exactly 37 reserved, 63 denied `budget_exceeded`; p50/p99 release latency upper bounds logged
- `internal/budget` TestReserveReportsWhyItCannotReserve — the account CHECK refuses even a correctly priced reservation made by hand
- `internal/budget` TestReservationsAreMadeOnlyByTheReleaseForTheActionsCost — T10 requires the reservation; only the release actor reserves, and only the exact cost
- `internal/budget` TestEscrowBoundsChildrenByTheirParent — a child's limit is escrowed from its parent, so the tree never exceeds its root
- `internal/budget` TestRaisingALimitIsTwoPersonAndLoweringIsNot — a raise needs a second admin and is refused once the limit moved
- `internal/budget` TestReleasesSettlementsAndLimitChangesDoNotDeadlock — releases, settlements and limit changes under load: no deadlock, counters match the rows
- `internal/budget` TestSettlementNeverWaitsForTheAccount — settling touches only the reservation, never the account row
- `internal/budget` TestALeafReservationDoesNotWaitForItsParent — a reservation locks only its leaf
- `internal/action` TestSettlementFollowsTheOutcome — success commits, no effect releases, an unknown outcome holds until a human resolves it
- `internal/bundle` TestABudgetIsCreatedAndRaisedInOneChangeSet — a bundle's limit increase is proposed by one admin, escrowed from its parent and applied by a second
- `internal/action` TestAnExpiredReleaseGivesItsBudgetBack — the reservation TTL: expiry at `not_after` releases it
- `internal/action` TestABudgetDenialNeverSpendsTheApproval — the budget is checked before the grant is consumed
- `internal/action` TestBudgetFailuresDenyClosed — no account or an invalid cost denies

## 4 [A] Duplicate messages, submissions or reclaims do not duplicate external effects

- `internal/action` TestConcurrentSubmissionsWithOneKeyCreateOneAction — one idempotency key, one action
- `internal/worker` TestDuplicateSubmissionsProduceOneEffect — concurrent and repeated submissions, one ERP record
- `internal/worker` TestWorkerKilledWhileExecutingIsReconciledNotRedispatched — a reclaim reconciles and never re-dispatches blindly
- `internal/worker` TestLoopsSurviveRepeatedDatabaseConnectionLoss — connection storms, one ERP record per operation key
- `internal/worker` TestReplicasShareTheWorkAndSurviveLosingOne — three replicas of every loop, one ERP record per operation key
- `internal/action` TestTwoSweepersReclaimEachLapsedLeaseOnce — two sweepers race on the same lapsed leases: each is reclaimed once
- `internal/worker` TestWorkerExecutesAQueuedActionOnce — workers claim from PostgreSQL, not from queue messages
- `internal/connector` TestHTTPExecuteDoesNotReplayAfterAReusedConnectionLosesResponse — the HTTP client never replays a POST
- `internal/messaging` TestRepublishedRowIsDeduplicatedByTheStream — a republished outbox row is dropped by the stream's duplicate window
- `internal/messaging` TestHintWakesOnceAndADuplicateIsAckedWithoutWaking — a duplicate hint past that window is ACKed through the inbox, not processed
- `internal/messaging` TestHintedWorkerExecutesLongBeforeItsPollInterval — a hint only wakes the claim loop: one fenced claim, one call
- `internal/messaging` TestWithoutNATSTheWorkerStillExecutes — without NATS the worker polls; the late hint claims nothing

## 5 [A] Timeout after dispatch does not automatically mean failure

- `internal/connector` TestHTTPClassifiesOnlyCertifiedNoEffectAndTreatsLostResponsesAsAmbiguous — timeouts and resets are ambiguous
- `internal/worker` TestResultsAreClassifiedAndRetriedPerContract — ambiguous results become UNKNOWN_OUTCOME
- `internal/worker` TestLostResponseIsReconciledToSuccessWithOneRecord — a lost response is reconciled to SUCCEEDED

## 6 [A] An irreversible non-idempotent action is never blindly retried

- `internal/worker` TestStaleWorkerAfterIntentIsUnknownOutcomeAndLate — lease loss during a call → UNKNOWN_OUTCOME, no re-dispatch
- `internal/worker` TestDelayedVisibilityIsNeverRetried — "not found" under BEST_EFFORT is never a retry
- `internal/worker` TestNegativeEvidenceNeedsAnAuthoritativeContract — the database refuses retry without authoritative proof
- `internal/worker` TestDecideAppliesTheProofStandard — the reconciler's decision table
- `internal/action` TestSweeperReclaimsLapsedLeases — only READ_ONLY actions are retried after lease loss
- `internal/worker` TestRetryBudgetBoundsElapsedTime — no retry is granted or re-queued once the retry time budget has passed; the action fails instead
- `internal/action` TestRetryBudgetBoundsRetryCost — the cost of retries bounds how many are dispatched

## 7 [A] Sensitive action governance is revalidated before execution

- `internal/action` TestReleaseRequiresFreshEvidenceAndConsumedGrant — release needs fresh evidence recorded in its transaction
- `internal/action` TestPolicyChangeAfterGrant — a new policy version re-evaluates and voids the grant
- `internal/action` TestRegistryDriftBeforeReleaseDenies — registry drift denies at release
- `internal/worker` TestDispatchIntentRefusesDriftAndRevocation — drift after release blocks the dispatch intent
- `internal/worker` TestDriftBeforeDispatchNeverCalls — no external call after drift
- `internal/action` TestMCPDefinitionDriftBeforeReleaseDenies — a changed MCP tool definition denies an approved action at release, and lifting the quarantine does not recertify it
- `internal/registry` TestDefinitionChangesAreClassifiedAndInvalidateContracts — PostgreSQL classifies definition changes; a behavioural change quarantines the tool and its contract stops matching
- `internal/worker` TestScannerDiscoversToolsAndQuarantinesDrift — a scan that sees a poisoned description quarantines the certified tool

## 8 [A] Tenant isolation cannot be bypassed

- `internal/storage` TestEveryTableFollowsTheRLSConventionAndCrossTenantPathsAreReviewed — every table forces RLS; cross-tenant paths are pinned and reviewed
- `internal/storage` TestTenantSeesOnlyItsOwnRows — the RLS convention
- `internal/storage` TestMissingTenantContextSeesNothing — no tenant context, no rows
- `internal/service` TestStartRefusesRoleThatCanBypassRLS — services refuse a role that bypasses RLS
- `internal/worker` TestAnotherTenantSeesAndChangesNothingAfterAFullFlow — after a full flow, another tenant reads and changes nothing in any table
- `internal/api` TestActionsOfOtherTenantsAreNotFound — the action API answers 404 across tenants
- `internal/api` TestOtherTenantsResourcesAreNotFound — the registry API answers 404 across tenants
- `internal/registry` TestCrossTenantReferencesAreRejected — rows cannot reference another tenant's rows
- `internal/action` TestCrossTenantScansExposeOnlyCountsAndTenantIDs — the SECURITY DEFINER hints expose ids and counts only
- `internal/messaging` TestInboxIsGuardedAndTenantIsolated — inbox records are tenant rows under RLS, written only by the inbox actor
- `internal/messaging` TestDashboardEventsArrivePerTenant — dashboard events carry the tenant in the subject; another tenant's filter sees none
- `internal/budget` TestBudgetRowsAreTenantIsolated — budget accounts, reservations and limit changes are tenant rows under RLS
- `internal/worker` TestCircuitChangesAreGuardedAndJournaled — connector circuits are tenant rows under RLS
- `internal/worker` TestScannerScansOnlyServersItHoldsSecretsFor — the cross-tenant scan hint yields only servers whose tenant-bound secret the worker holds
- `internal/registry` TestDependencyWritesAreGuardedAndTenantScoped — a dependency cannot refer across tenants; its table follows RLS
- `internal/worker` TestKillRejectsDirectWriteAndForeignTarget — the kill API rejects a foreign target and direct table writes
- `internal/fleet` TestFleetOperationsAreTenantScoped — a fleet operation selects, changes and shows only its tenant's agents
- `internal/fleet` TestFleetTargetMustBeATenantVersion — a raw target naming another tenant's version is refused
- `internal/finops` TestAlertsThroughTheServiceAndTheEvaluatorAcrossTenants — the cross-tenant evaluator hint yields tenant ids only; each tenant sees and acknowledges only its own alerts
- `internal/api` TestFinOpsOfOtherTenantsAreNotFound — the FinOps API shows another tenant no spend, and its agents, accounts and alerts are not found
- `internal/bundle` TestChangeSetsAreTenantIsolated — change sets, bundles and steps are tenant rows under RLS
- `internal/api` TestChangeSetsOfOtherTenantsAreNotFound — the change-set and drift API answers 404 across tenants

## 9 [B] Connector failure cannot starve unrelated connector pools

- `internal/worker` TestConnectorFailureDoesNotStarveUnrelatedConnectors — a hanging connector holds only its worker bulkhead while another connector's actions succeed; its failures open the breaker and the shared circuit, which also stops a second worker until an operator enables the connector
- `internal/worker` TestLocalBreakerHoldsWithoutTheSharedCircuit — a worker's own breaker withholds claims and releases a held lease even after the shared circuit is cleared
- `internal/worker` TestOpenCircuitRefusesClaimAndDispatch — an open or disabled circuit refuses T14 and T16 in raw SQL and hides only that connector from the claim hint
- `internal/worker` TestDisableSerializesWithAStaleDispatchIntent — a dispatch intent holds the circuit row; none commits past a disable
- `internal/worker` TestConnectorCapacitySerializesConcurrentClaims — a connector group's `max_inflight` is the cluster-wide bulkhead
- `internal/action` TestAdmissionBoundsPendingAndConnectorQueues — a full connector queue refuses that connector (429, scope `connector`) and admits another; unreleased actions are bounded per tenant
- `internal/worker` TestBreakerOpensProbesAndCloses — the breaker's states, single probe and escalating cooldown

## 10 [A] Audit and evidence references remain reconstructible end-to-end

- `internal/worker` TestEvidenceReconstructsTheWholeActionFromItsID — governance → approval → execution → reconciliation → outcome, with the verified journal
- `integrations/governance/microsoftagt` TestEngineWithTheAGTProvider — an AGT decision keeps its provider evidence in the evidence report and the journal
- `internal/governance` TestRecordDecisionPersistsBoundedProviderEvidence — provider evidence is stored with the decision and bounded in raw SQL
- `internal/action` TestEvidenceIsReconstructibleFromTheAction — decisions, votes and grants join by action_id
- `internal/api` TestOperatorsResolveActionsOverTheAPI — evidence over the API, with a verified chain

## 11 [A] Agents never hold credentials for privileged systems

- `test/security` TestAgentCannotReachFakeERP — no network path from the agent to the ERP (compose)
- `test/security` TestAgentCannotReachPostgres — no network path from the agent to the database (compose)
- `test/security` TestOnlyTheWorkerHoldsConnectorSecrets — only the worker mounts connector secrets (compose)
- `test/security` TestFakeERPRejectsUnauthenticatedPrivilegedCall — the ERP refuses calls without the credential (compose)
- `internal/config` TestConnectorSecretsOnlyInTheWorker — every other service refuses a connector-secrets file
- `internal/worker` TestSecretCanaryNeverLeaks — a canary secret appears in no row, journal, outbox or log
- `internal/fakeerp` TestFakeERPRejectsUnauthenticatedPrivilegedCalls — privileged ERP calls need the worker credential

## 12 [A] After a dispatch intent, re-dispatch only when READ_ONLY or natively idempotent, after authoritative absence, or after a human resolution, with the same operation key

- `internal/worker` TestWorkerKilledWhileExecutingIsReconciledNotRedispatched — authoritative absence → one retry with the same key
- `internal/worker` TestStaleWorkerAfterIntentIsUnknownOutcomeAndLate — nothing else re-dispatches
- `internal/worker` TestHumanResolutionIsSeparatedJournaledAndTwoPersonForRetry — a human retry needs two operators and keeps the key
- `internal/worker` TestRestartedServicesFinishEveryPendingAction — an interrupted call is reconciled after a restart, not re-dispatched
- `internal/worker` TestCompletionSerializesWithKillAndKeepsOutcomeUnknown — completion cannot turn a racing kill into a proven success
- `internal/worker` TestKillResumedDuringCallStillLeavesOutcomeUnknown — a kill resumed during the call still leaves the outcome unknown

## 13 [A] "Not found" alone never authorizes the retry of an irreversible or non-idempotent action

- `internal/worker` TestNegativeEvidenceNeedsAnAuthoritativeContract — BEST_EFFORT absence cannot retry, in raw SQL
- `internal/worker` TestDecideAppliesTheProofStandard — absence without authority is still unknown
- `internal/worker` TestDelayedVisibilityIsNeverRetried — the Fake ERP flagship
- `internal/worker` TestAuthoritativeAbsenceFailsOnlyWhenNoRetryRemains — authoritative absence, settled, with the same key

## 14 [A] An action executes only its enforced payload, bound by digest to its decision and grant

- `internal/worker` TestTamperedEnforcedPayloadIsDeniedWithAnAlert — a changed payload is denied before dispatch
- `internal/governance` TestDigestsBindTheEnforcedPayload — digests bind the enforced payload
- `internal/action` TestTransformThenApproveBindsTheEnforcedPayload — approvals bind the transformed payload
- `internal/action` TestDigestMismatchDeniesAndAlerts — a digest mismatch denies and alerts
- `internal/connector` TestHTTPExecutePreservesEnforcedPayloadAndNativeKey — the connector sends exactly the enforced payload
- `internal/governance` TestConformanceReference — the ADR-002 reference set pins the verdicts, enforced payloads and digests; the AGT sidecar's image build must reproduce them through ACS
- `integrations/governance/microsoftagt` TestReferenceSetRoundTripsTheWireProtocol — the AGT client carries every decision and digest of the reference set losslessly

## 15 [A] An approval grant is bound, expires, is consumed at most once, and is not granted by the subject or the owner

- `internal/approval` TestConsumeBindsDigestAndCanSucceedOnce — binding and single consumption
- `internal/approval` TestOwnerAndEnablingActorsCannotApprove — owners and enabling actors cannot approve
- `internal/approval` TestSelfAndCrossTenantApprovalRejected — no self-approval, no cross-tenant approval
- `internal/approval` TestOwnerGroupMembershipAtRequestOrVoteBlocksApproval — owner-group members cannot approve
- `internal/approval` TestGrantCannotBeConsumedAfterRequestExpiryIsShortened — expiry is enforced at consumption
- `internal/action` TestExpiredGrantExpiresTheAction — an expired grant expires the action

## 16 [A] Approval and execution state survive a restart of any EACP process

- `internal/worker` TestRestartedServicesFinishEveryPendingAction — fresh processes finish half-voted, approved and interrupted actions
- `internal/worker` TestLoopsSurviveRepeatedDatabaseConnectionLoss — dropped database connections lose nothing
- `internal/worker` TestReplicasShareTheWorkAndSurviveLosingOne — an API replica and a worker stop mid-run; the others finish every action
- `internal/action` TestSweeperRecoversActionsLeftByAnOutage — work left by a stopped process is resumed
- `internal/approval` TestVoteServiceReturnsDurableState — votes are durable when acknowledged
- `test/demo` TestSliceADemo — restarts the API, worker and PostgreSQL with an approval pending (compose)

## 17 [A] Every state transition and privileged operator action is journaled, hash-chained, with actor and reason

- `internal/action` TestActionTransitionsAreJournaledWithActorKinds — transitions with actor kinds
- `internal/registry` TestRawSQLChangesAreAuditedByTheDatabase — the database journals even raw SQL changes
- `internal/audit` TestVerifyDetectsTampering — edits, deletions and reordering break the chain
- `internal/audit` TestConcurrentAppendsStayGapless — the chain has no gaps under concurrency
- `internal/worker` TestHumanResolutionIsSeparatedJournaledAndTwoPersonForRetry — operator resolutions are journaled
- `internal/worker` TestEvidenceReconstructsTheWholeActionFromItsID — tampering is reported by the evidence chain
- `internal/budget` TestRaisingALimitIsTwoPersonAndLoweringIsNot — every limit proposal, application and rejection is journaled
- `internal/action` TestSettlementFollowsTheOutcome — a reservation and its settlement are journaled with their actors
- `internal/bundle` TestAnIdentityBundleIsAppliedByTwoAdmins — a bundle's grant is proposed by the submitter and approved by a second admin; the change set's moves are journaled
- `internal/worker` TestCircuitChangesAreGuardedAndJournaled — every circuit trip, disable and enable is journaled with its actor and reason
- `internal/registry` TestScanActivityIsJournaled — every scan, recorded definition, rescan request and quarantine is journaled with its actor and reason
- `internal/registry` TestDependencyWritesAreGuardedAndTenantScoped — dependency evidence insertion and revocation are journaled with the editor actor
- `internal/worker` TestKillAndResumeAreJournaledWithActorAndReason — both operator changes are audited with reason and emitted to the outbox
- `internal/fleet` TestPauseTargetRecordsDatabaseStateAndIsJournaled — each transition and the operation (actor, reason, targets) are journaled, the operation last
- `internal/finops` TestSoftLimitsAreAdminOnlyAndJournaled — soft limits, prices and billing imports are journaled with their actor and the row's reason
- `internal/finops` TestAlertsAreRaisedByFinOpsAndAcknowledgedOnce — an alert's creation (the finops system actor) and its single acknowledgement are journaled
- `internal/bundle` TestClosedChangeSetsAreTerminalAndRejectionNeedsAReason — every change-set move is journaled with its actor, a rejection with its reason
- `internal/incident` TestTheIncidentLifecycle — every incident event (opened, acknowledged, resolved) is journaled with its actor and reason
- `internal/bundle` TestApprovalIsASecondPersonAgainstTheSealedDigest — plan, submission and approval are journaled; the approver is a second person

## 18 [A] Governance failure fails closed, but never blocks cancellation, reconciliation reads or containment

- `internal/action` TestGovernanceOutageKeepsActionReceivedUntilResubmission — an outage leaves the action RECEIVED
- `internal/governance` TestEvaluateCheckedFailsClosedOnProviderErrorAndIncompleteEvidence — incomplete decisions fail closed
- `internal/api` TestGovernanceOutageIs503WithTheAction — the API answers 503 with the action
- `internal/action` TestCancelBeforeDispatchNeverConsultsGovernance — cancellation never calls the PDP
- `internal/action` TestReceivedActionCanBeCancelledByTheRequesterOrAnOperator — T5a in raw SQL
- `internal/action` TestCancelWinsOverAnInFlightDecision — a cancel during a PDP call wins
- `internal/worker` TestGovernanceOutageFailsClosedWithoutBlockingSafety — cancellation, reconciliation and containment during an outage
- `internal/worker` TestKillDuringCallForcesUnknownOutcome — kill containment is database-authoritative and the call is reconciled as unknown
- `integrations/governance/microsoftagt` TestFailuresAreTransientAndBadResponsesAreMalformed — every AGT sidecar failure, version mismatch or bad response is transient
- `integrations/governance/microsoftagt` TestEngineWithTheAGTProvider — an AGT outage keeps the action RECEIVED, and cancel never calls the sidecar
- `cmd/controlplane-api` TestGovernanceProviderSelection — an unreachable PDP does not stop the API from starting; a mismatched one does
- `test/demo` TestSliceADemo — stopping the AGT sidecar: 503 and RECEIVED, cancel works, the sweeper resumes (compose)

## 19 [A] Every executed action is attributable to an authenticated agent and an ACTIVE version whose allowlist includes the tool

- `internal/identity` TestAuthenticateAgent — agent keys bind one agent version
- `internal/action` TestActionInsertRequiresMatchingAgentAndDerivesIdentity — the database derives the identity
- `internal/action` TestCapabilityAndSubjectDenialsLeaveAnAuditableAction — capability denials before governance
- `internal/registry` TestCheckCapabilityDeniesSuspendedRevokedAndDrifted — inactive versions and removed tools are denied
- `internal/worker` TestDispatchIntentRefusesDriftAndRevocation — rechecked at the dispatch intent
- `internal/api` TestAgentKeysCannotUseOperatorRoutesAndViceVersa — agent and operator keys are separate
- `internal/registry` TestToolQuarantineRules — a quarantined tool is denied (`tool_quarantined`) in Go and in PostgreSQL
- `internal/fleet` TestFleetResumeKeepsVersionSeparationOfDuties — a fleet resume cannot grant what a single activation could not
- `internal/worker` TestFleetPauseDeniesQueuedAndNewWork — a paused version's queued and new actions are denied; nothing reaches the connector
