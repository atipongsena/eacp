# Invariants and their tests

Phase 9 (Slice B) adds the AGT sidecar PDP. Its tests join invariants 10, 14 and 18. The sidecar's own suites, including the conformance reference set run through ACS and OPA, run in `docker build -f sidecars/agt-pdp/Dockerfile --target test .`.

Phase 10 (Slice B) adds NATS JetStream for work hints and dashboard events (ADR-014). Its tests join invariants 4 and 8. They run against a JetStream server embedded in the test process, so they never skip for want of NATS.

Phase 11 (Slice B) adds hard budget reservation (ADR-012). It maps the first [B] invariant, 3, and its tests join invariants 8 and 17.

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
- `internal/action` TestAnExpiredReleaseGivesItsBudgetBack — the reservation TTL: expiry at `not_after` releases it
- `internal/action` TestABudgetDenialNeverSpendsTheApproval — the budget is checked before the grant is consumed
- `internal/action` TestBudgetFailuresDenyClosed — no account or an invalid cost denies

## 4 [A] Duplicate messages, submissions or reclaims do not duplicate external effects

- `internal/action` TestConcurrentSubmissionsWithOneKeyCreateOneAction — one idempotency key, one action
- `internal/worker` TestDuplicateSubmissionsProduceOneEffect — concurrent and repeated submissions, one ERP record
- `internal/worker` TestWorkerKilledWhileExecutingIsReconciledNotRedispatched — a reclaim reconciles and never re-dispatches blindly
- `internal/worker` TestLoopsSurviveRepeatedDatabaseConnectionLoss — connection storms, one ERP record per operation key
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

## 7 [A] Sensitive action governance is revalidated before execution

- `internal/action` TestReleaseRequiresFreshEvidenceAndConsumedGrant — release needs fresh evidence recorded in its transaction
- `internal/action` TestPolicyChangeAfterGrant — a new policy version re-evaluates and voids the grant
- `internal/action` TestRegistryDriftBeforeReleaseDenies — registry drift denies at release
- `internal/worker` TestDispatchIntentRefusesDriftAndRevocation — drift after release blocks the dispatch intent
- `internal/worker` TestDriftBeforeDispatchNeverCalls — no external call after drift

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

## 18 [A] Governance failure fails closed, but never blocks cancellation, reconciliation reads or containment

- `internal/action` TestGovernanceOutageKeepsActionReceivedUntilResubmission — an outage leaves the action RECEIVED
- `internal/governance` TestEvaluateCheckedFailsClosedOnProviderErrorAndIncompleteEvidence — incomplete decisions fail closed
- `internal/api` TestGovernanceOutageIs503WithTheAction — the API answers 503 with the action
- `internal/action` TestCancelBeforeDispatchNeverConsultsGovernance — cancellation never calls the PDP
- `internal/action` TestReceivedActionCanBeCancelledByTheRequesterOrAnOperator — T5a in raw SQL
- `internal/action` TestCancelWinsOverAnInFlightDecision — a cancel during a PDP call wins
- `internal/worker` TestGovernanceOutageFailsClosedWithoutBlockingSafety — cancellation, reconciliation and containment during an outage
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
