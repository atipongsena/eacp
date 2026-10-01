[English](THREAT_MODEL.md) | [ไทย](THREAT_MODEL.th.md)

# Threat model

This is the threat model that [MASTER_PLAN](../MASTER_PLAN.md) §68 asks for. It lists what EACP protects, where its
trust boundaries are, the threats at each boundary with the control that answers each one, and what remains
outside its reach. Every control named here exists in the code: each row names the ADR that decides it and a test
that enforces it. [INVARIANTS.md](../INVARIANTS.md) lists the full set of tests behind each guarantee.

## Starting assumptions

EACP trusts none of its inputs (MASTER_PLAN §67). The agent is untrusted. So are the model's output, tool
metadata, MCP servers, remote A2A agents, queue messages and the responses of external APIs. A control that only
works when the agent behaves is not a control.

## Assets

| Asset | Where it lives | Why it matters |
|---|---|---|
| Connector credentials | The execution worker's memory, loaded from its secrets file, Vault or a JIT provider ([ADR-019](../adr/ADR-019-credential-custody.md)) | They grant privileged access to enterprise systems. |
| Model provider keys | The LLM gateway's memory ([ADR-031](../adr/ADR-031-llm-gateway.md)) | They spend money and reach the provider. |
| Enterprise system state | The target systems | The effects of actions: orders, payments, records. |
| Approvals and grants | PostgreSQL ([ADR-005](../adr/ADR-005-approval-ownership-and-atomic-execution-boundary.md)) | A human decision that authorises one exact action. |
| The audit journal | PostgreSQL, hash-chained per tenant ([ADR-003](../adr/ADR-003-agent-registry-identity-and-capability.md)) | The evidence of what happened, by whom, and why. |
| Tenant data | PostgreSQL, under Row-Level Security | Registry, actions, payloads, budgets and costs of each tenant. |
| Kept tool output | PostgreSQL, for at most a day per opted-in contract ([ADR-034](../adr/ADR-034-result-channel.md)) | Untrusted remote content that may be personal data; only the calling agent may read it. |
| API keys | Only their hashes, in PostgreSQL ([`internal/identity`](../../internal/identity)) | They authenticate agents and people. |

## Trust boundaries

| # | Boundary | Crossed by |
|---|---|---|
| B1 | Agent → API and LLM gateway | Every request an agent makes, with its own EACP key |
| B2 | API → policy decision point | Governance questions and their decisions |
| B3 | Execution worker → target systems | Connector calls, MCP scans, A2A delegation, reconciliation lookups |
| B4 | Services → PostgreSQL | Every state change and read |
| B5 | Services → NATS | Work hints and kill signals |
| B6 | People → API and console | Operators, approvers, registry editors and approvers, admins |
| B7 | LLM gateway → model provider | Model calls |
| B8 | Source and build → release | Dependencies, CI and release artifacts |

The compose stack ([`docker-compose.yml`](../../docker-compose.yml)) and the Helm chart make B1, B3 and B7 network
boundaries: agents cannot reach target systems, PostgreSQL, the PDP, NATS or the model provider
([ARCHITECTURE.md](../ARCHITECTURE.md)).

## Threats and controls

Each table uses STRIDE: **S**poofing, **T**ampering, **R**epudiation, **I**nformation disclosure, **D**enial of
service and **E**levation of privilege. Tests are named as package and function.

### B1: agent to API and gateway

| STRIDE | Threat | Control | Decided by | Enforced by |
|---|---|---|---|---|
| S | Agent impersonation | Each agent authenticates with its own key, which EACP stores only as a hash | [ADR-003](../adr/ADR-003-agent-registry-identity-and-capability.md) | `internal/identity` `TestAuthenticateAgent`, `TestAuthenticationFailures` |
| T | Parameter substitution: the executed payload differs from the approved one | The enforced payload is bound by digest to its decision and grant; a mismatch denies and raises an alert | [ADR-005](../adr/ADR-005-approval-ownership-and-atomic-execution-boundary.md) | `internal/action` `TestDigestMismatchDeniesAndAlerts`; `internal/worker` `TestTamperedEnforcedPayloadIsDeniedWithAnAlert` |
| T | Duplicate execution from retried submissions | One idempotency key gives one action; a different body under the same key is refused | [ADR-004](../adr/ADR-004-action-state-machine-and-execution-semantics.md) | `internal/action` `TestConcurrentSubmissionsWithOneKeyCreateOneAction`; `internal/worker` `TestDuplicateSubmissionsProduceOneEffect` |
| R | An agent denies having requested an action | Every action is bound to an authenticated agent and version, and every transition is journaled | [ADR-003](../adr/ADR-003-agent-registry-identity-and-capability.md), [ADR-004](../adr/ADR-004-action-state-machine-and-execution-semantics.md) | `internal/action` `TestActionInsertRequiresMatchingAgentAndDerivesIdentity`, `TestActionTransitionsAreJournaledWithActorKinds` |
| I | Cross-tenant access | Row-Level Security on every tenant table; tenant-scoped unique keys | [ADR-003](../adr/ADR-003-agent-registry-identity-and-capability.md), MASTER_PLAN §69 | `internal/storage` `TestTenantSeesOnlyItsOwnRows`, `TestMissingTenantContextSeesNothing` |
| D | Queue flooding | Admission limits answer 429 without creating an action; bounded pending and connector queues | [ADR-022](../adr/ADR-022-backpressure-bulkheads-circuit-breakers-retry-budgets.md) | `internal/api` `TestAdmissionLimitIs429`; `internal/action` `TestAdmissionLimitRejectsWithoutCreatingAnAction` |
| D | Denial of wallet | Hard budgets are reserved in PostgreSQL at release; LLM calls reserve PostgreSQL's estimate before anything is sent | [ADR-012](../adr/ADR-012-budget-reservation.md), [ADR-031](../adr/ADR-031-llm-gateway.md) | `internal/action` `TestConcurrentReleasesNeverOversubscribeAHardBudget`; `internal/llm` `TestAdmitReservesTheEstimate` |
| E | Privilege escalation or a confused deputy: an agent uses a tool it was not given | The tool must be on the `ACTIVE` version's allowlist with an active certified contract, checked at submission, at release and again at dispatch | [ADR-003](../adr/ADR-003-agent-registry-identity-and-capability.md), [ADR-005](../adr/ADR-005-approval-ownership-and-atomic-execution-boundary.md) | `internal/registry` `TestCheckCapabilityDeniesSuspendedRevokedAndDrifted`; `internal/worker` `TestDispatchIntentRefusesDriftAndRevocation` |
| E | Control-plane bypass: the agent calls the target directly | Agents hold no enterprise credential and have no network route to targets | [ADR-001](../adr/ADR-001-product-boundary-and-enforcement-point.md) | `test/security` `TestAgentCannotReachFakeERP`, `TestOnlyTheWorkerHoldsConnectorSecrets`, `TestFakeERPRejectsUnauthenticatedPrivilegedCall` |
| E | Prompt injection makes the agent request harmful actions | EACP does not read prompts. It bounds what any request can do: allowlist, policy, approvals, budgets and kill switches apply to every action, whatever made the agent ask | [ADR-001](../adr/ADR-001-product-boundary-and-enforcement-point.md) | The tests of the controls above; see residual risks |

### B2: API to the policy decision point

| STRIDE | Threat | Control | Decided by | Enforced by |
|---|---|---|---|---|
| S | A forged governance decision | The sidecar is reached over mutual TLS with a dedicated CA; only the API and the sidecar hold its keys; agents have no route to it | [ADR-002](../adr/ADR-002-agt-integration-sidecar-pdp.md) | `test/security` `TestPDPRefusesAClientWithoutCertificate`, `TestAgentCannotReachThePDP`, `TestOnlyTheAPIAndTheSidecarHoldThePDPPKI` |
| T | A replayed or incomplete decision | Decision evidence is stored with the input and enforced digests; incomplete evidence fails closed | [ADR-002](../adr/ADR-002-agt-integration-sidecar-pdp.md) | `internal/governance` `TestRecordDecisionPersistsBoundedProviderEvidence`, `TestEvaluateCheckedFailsClosedOnProviderErrorAndIncompleteEvidence` |
| T | A stale approval after the policy changed | The release revalidates under the current policy version; a grant is not consumable after a policy change | [ADR-005](../adr/ADR-005-approval-ownership-and-atomic-execution-boundary.md) | `internal/action` `TestPolicyChangeAfterGrant`; `internal/approval` `TestGrantCannotBeConsumedTwiceOrAfterPolicyChange` |
| D | The PDP is down | Fail closed: the action stays `RECEIVED` and is not executable; cancellation, reconciliation reads and containment never need the PDP | [ADR-004](../adr/ADR-004-action-state-machine-and-execution-semantics.md) T2a | `internal/action` `TestGovernanceOutageKeepsActionReceivedUntilResubmission`, `TestCancelBeforeDispatchNeverConsultsGovernance` |

### B3: execution worker to target systems

| STRIDE | Threat | Control | Decided by | Enforced by |
|---|---|---|---|---|
| S | A caller other than EACP performs privileged calls | In a conforming deployment the target accepts privileged calls only from the worker's credential; the Fake ERP enforces this | [ADR-001](../adr/ADR-001-product-boundary-and-enforcement-point.md) §3a | `test/security` `TestFakeERPRejectsUnauthenticatedPrivilegedCall` |
| T | Tool poisoning or an MCP rug pull: a tool's definition changes after certification | Tools are discovered, never declared; PostgreSQL fingerprints each definition; a high-risk change or a missing certified tool quarantines it, and only another approver releases it | [ADR-023](../adr/ADR-023-mcp-registry-and-tool-fingerprint.md) | `internal/worker` `TestScannerDiscoversToolsAndQuarantinesDrift`; `internal/fleet` `TestQuarantineAndReleaseByAnotherApprover`; `internal/registry` `TestMissingCertifiedToolIsQuarantined` |
| T | A remote A2A agent changes its card after certification | The card is part of the delegate's definition; a change quarantines it | [ADR-030](../adr/ADR-030-a2a-delegation.md) | `internal/registry` `TestA2ADefinitionChangeQuarantinesACertifiedDelegate` |
| T | Stale-worker duplicate dispatch | Leases are fenced by generation in the database; a worker that lost its lease cannot write or dispatch | [ADR-004](../adr/ADR-004-action-state-machine-and-execution-semantics.md) | `internal/worker` `TestLeaseRaceHasExactlyOneWinner`, `TestStaleWorkerBeforeIntentNeverDispatches` |
| T | Unknown outcome mishandled as failure or success | An ambiguous result becomes `UNKNOWN_OUTCOME` and is reconciled, never retried blindly | [ADR-004](../adr/ADR-004-action-state-machine-and-execution-semantics.md) | `internal/worker` `TestLostResponseIsReconciledToSuccessWithOneRecord`, `TestDelayedVisibilityIsNeverRetried` |
| T | False-negative reconciliation: "not found" leads to a retry and a duplicate | Negative evidence counts only under an `AUTHORITATIVE` contract after every call has settled; otherwise a person decides | [ADR-004](../adr/ADR-004-action-state-machine-and-execution-semantics.md) Rev 2.5 | `internal/worker` `TestNegativeEvidenceNeedsAnAuthoritativeContract`, `TestDecideAppliesTheProofStandard` |
| R | No record of what the target did | Attempts, external references and reconciliation checks are part of the evidence | [ADR-004](../adr/ADR-004-action-state-machine-and-execution-semantics.md) | `internal/worker` `TestEvidenceReconstructsTheWholeActionFromItsID` |
| I | Credential theft, or a connector echoing its secret | Credentials live only in the worker, are never stored or journaled, are redacted from logs, and connector-returned fields that contain one are dropped | [ADR-001](../adr/ADR-001-product-boundary-and-enforcement-point.md), [ADR-019](../adr/ADR-019-credential-custody.md) | `internal/worker` `TestSecretCanaryNeverLeaks`; `internal/logging` `TestRegisteredSecretValuesAreRedactedInMessageAndAttrs` |
| I | Tool output reaches another agent, a person, a log or the journal, or keeps a credential | Output is kept only for an opted-in contract's success, readable only by an `ACTIVE` version of the calling agent through PostgreSQL, withheld when it holds a worker credential, bounded, expired and cleared; never logged, journaled or messaged | [ADR-034](../adr/ADR-034-result-channel.md) | `internal/worker` `TestOnlyTheActionsAgentReadsTheResult`, `TestResultContentIsNotSelectable`, `TestACredentialInTheOutputIsWithheld`, `TestASuccessKeepsItsOutputForTheAgent` |
| D | Retry storm or a failing connector starves others | Retry budgets bound attempts, cost and time; circuit breakers and bulkheads isolate connectors | [ADR-022](../adr/ADR-022-backpressure-bulkheads-circuit-breakers-retry-budgets.md) | `internal/action` `TestRetryBudgetBoundsRetryCost`; `internal/worker` `TestConnectorFailureDoesNotStarveUnrelatedConnectors` |
| E | A killed agent keeps acting | Kill states are checked in PostgreSQL at claim and dispatch and polled during the call; a kill during a call leaves the outcome unknown, never retried | [ADR-016](../adr/ADR-016-distributed-kill-switch.md) | `internal/worker` `TestRawDispatchIntentIsDatabaseFencedByKill`, `TestKillDuringCallForcesUnknownOutcome` |

### B4: services to PostgreSQL

| STRIDE | Threat | Control | Decided by | Enforced by |
|---|---|---|---|---|
| S | A service runs with a role that bypasses Row-Level Security | Services refuse to start unless they connect as a role without `BYPASSRLS` | [ADR-003](../adr/ADR-003-agent-registry-identity-and-capability.md) | `internal/service` `TestStartRefusesRoleThatCanBypassRLS` |
| T | Audit manipulation | The journal is append-only and hash-chained per tenant, appended in the same transaction as the change; clients cannot set chain fields; verification detects tampering | [ADR-003](../adr/ADR-003-agent-registry-identity-and-capability.md) §8 | `internal/audit` `TestVerifyDetectsTampering`, `TestClientCannotForgeChainFields`; `internal/registry` `TestRawSQLChangesAreAuditedByTheDatabase` |
| T | Approval replay: one grant releases two actions | A grant is consumed at most once, under a row lock | [ADR-005](../adr/ADR-005-approval-ownership-and-atomic-execution-boundary.md) | `internal/approval` `TestConcurrentConsumeSucceedsOnce`; `internal/action` `TestApprovalThenParallelReleasesConsumeTheGrantOnce` |
| T | Budget race | Reservations are made under the leaf account's lock; limits are escrowed from parents | [ADR-012](../adr/ADR-012-budget-reservation.md) | `internal/action` `TestConcurrentReleasesNeverOversubscribeAHardBudget`; `internal/budget` `TestEscrowBoundsChildrenByTheirParent` |
| I | A new table without tenant isolation | Every table, policy and `SECURITY DEFINER` function is reviewed in a catalog test | MASTER_PLAN §69 | `internal/storage` `TestEveryTableFollowsTheRLSConventionAndCrossTenantPathsAreReviewed` |

### B5: services to NATS

| STRIDE | Threat | Control | Decided by | Enforced by |
|---|---|---|---|---|
| S | A forged or replayed message causes an action | Messages carry signals only; nothing is claimed, executed, cancelled or decided from a message; each role has its own NATS user and permissions | [ADR-014](../adr/ADR-014-postgresql-authority-nats-signals.md) | `test/security` `TestWorkerCredentialCannotPublishHintsOrEvents`, `TestAgentCannotReachNATS` |
| I | Sensitive data in messages | Messages carry ids, states and epochs, never a reason, payload or secret | [ADR-014](../adr/ADR-014-postgresql-authority-nats-signals.md) | `internal/messaging` `TestTransitionsWriteDashboardEvents`, `TestOutboxRowsComeOnlyFromTheActionTriggers` |
| D | NATS is lost | Polling stays on; the outbox keeps rows until NATS recovers | [ADR-014](../adr/ADR-014-postgresql-authority-nats-signals.md) | `internal/messaging` `TestWithoutNATSTheWorkerStillExecutes`, `TestNATSOutageLeavesRowsUnpublishedUntilItRecovers` |

### B6: people to API and console

| STRIDE | Threat | Control | Decided by | Enforced by |
|---|---|---|---|---|
| E | Self-approval, or an approver from another tenant | Separation of duties in PostgreSQL: the subject, the owner, the owner's group and enabling actors cannot approve; approvers are tenant-scoped | [ADR-005](../adr/ADR-005-approval-ownership-and-atomic-execution-boundary.md) | `internal/approval` `TestSelfAndCrossTenantApprovalRejected`, `TestOwnerAndEnablingActorsCannotApprove` |
| E | A malicious operator or admin | Two-person rules: registry activations by a second person, raising a budget limit, clearing a kill, retrying after a human resolution, approving a change set; every privileged action is journaled with actor and reason | [ADR-003](../adr/ADR-003-agent-registry-identity-and-capability.md), [ADR-012](../adr/ADR-012-budget-reservation.md), [ADR-016](../adr/ADR-016-distributed-kill-switch.md), [ADR-026](../adr/ADR-026-governance-as-code.md) | `internal/api` `TestKillAPIRequiresOperatorAndSecondOperatorToResume`; `internal/worker` `TestHumanResolutionIsSeparatedJournaledAndTwoPersonForRetry` |
| I | The console leaks a key or runs injected content | The key stays in the tab's memory; a strict CSP; no HTML sinks, browser storage, cookies or other origins | [ADR-028](../adr/ADR-028-operator-console.md) | `internal/ui` `TestConsoleUsesNoDangerousSinks` |
| R | An operator denies a resolution or a kill | Kills and resolutions are journaled with actor and reason | [ADR-016](../adr/ADR-016-distributed-kill-switch.md) | `internal/worker` `TestKillAndResumeAreJournaledWithActorAndReason` |

### B7: gateway to model provider

| STRIDE | Threat | Control | Decided by | Enforced by |
|---|---|---|---|---|
| E | An agent uses a model it was not given | PostgreSQL admission checks the model allowlist before anything is sent | [ADR-031](../adr/ADR-031-llm-gateway.md) | `internal/llm` `TestStoreDenialIsReturnedNotAnError` |
| I | Prompts, responses or keys end up in storage or logs | The gateway stores metadata only and never logs a prompt, response, header or key | [ADR-031](../adr/ADR-031-llm-gateway.md) | `internal/llmgateway` `TestNothingSecretIsPersistedByTheGateway`, `TestNothingSecretIsLogged` |
| D | Denial of wallet through model calls | A budget reservation at admission; unknown usage charges the full reservation; a kill cuts a running stream | [ADR-031](../adr/ADR-031-llm-gateway.md), [ADR-016](../adr/ADR-016-distributed-kill-switch.md) | `internal/llmgateway` `TestTheGatewayMetersAndLimitsSpend`, `TestKillCutsAStream` |

### B8: supply chain

| STRIDE | Threat | Control | Decided by | Enforced by |
|---|---|---|---|---|
| T | A compromised GitHub Action | Every action is pinned to a full commit SHA | [RELEASING.md](../RELEASING.md) | `test/opensource` `TestWorkflowActionsArePinned` |
| T | A vulnerable Go dependency | `govulncheck` in CI; Dependabot updates | [RELEASING.md](../RELEASING.md) | [`scripts/ci/vuln.sh`](../../scripts/ci/vuln.sh) |
| T | An unreviewed change to the governance engine | The AGT, ACS and OPA versions are pinned; a bump needs spike notes and a green conformance run | [ADR-002](../adr/ADR-002-agt-integration-sidecar-pdp.md) | `integrations/governance/microsoftagt` `TestHealthChecksTheVersionPins`; the sidecar's conformance suite |
| T | A tampered release archive | Every release publishes `SHA256SUMS` | [RELEASING.md](../RELEASING.md) | [`scripts/ci/release-binaries.sh`](../../scripts/ci/release-binaries.sh) |

## Assumptions

- **A conforming deployment** ([ADR-001](../adr/ADR-001-product-boundary-and-enforcement-point.md) §3a): target
  systems accept privileged calls only from EACP's worker identities, agents have no network route to targets, and
  an agent's EACP key grants nothing at a target. EACP cannot prove the first condition for an arbitrary system.
- **A trusted database administrator.** PostgreSQL is the authority. A superuser, or anyone who can change its data
  files, can change anything, including the journal (verification would show the chain breaking, but only after
  the fact).
- **A trusted host for the worker and the gateway.** Whoever controls their process memory holds the credentials.
- **Honest time.** Leases, expiries and grants use the database clock.

## Residual risks

- **Prompt injection is bounded, not prevented.** An agent that has been manipulated can still request any action
  its allowlist, policy, approvals and budget permit. Narrow allowlists and approvals for sensitive operations are
  what limit the damage.
- **A compromised worker holds live credentials.** The database still fences what it may record, but it could use
  its credentials outside EACP until they are revoked. JIT credentials with short lifetimes
  ([ADR-019](../adr/ADR-019-credential-custody.md)) narrow this window.
- **Bypass is not detected.** In a deployment that is not conforming, EACP cannot see calls made around it. Reading
  target audit logs for non-EACP principals is future work (ADR-001 §3a).
- **Personal data in payloads.** Action payloads are stored as submitted, under Row-Level Security. EACP does not
  classify or redact personal data in them.
- **Kept tool output is plain data at rest.** A contract that opts into the result channel keeps its successes'
  output for up to a day, protected by Row-Level Security, column privileges and PostgreSQL's own at-rest
  protection, not by application-level encryption ([ADR-034](../adr/ADR-034-result-channel.md)).
- **Two colluding people.** Two-person rules stop one person, not two who agree.
- **A global kill scope does not exist yet.** It waits for platform authority
  ([ADR-016](../adr/ADR-016-distributed-kill-switch.md)). A tenant-wide kill is available, and since Rev 1.1 a kill
  of one Studio run, whose actions PostgreSQL binds to it.

## Coverage of MASTER_PLAN §68

| Threat in §68 | Section |
|---|---|
| Prompt injection | B1 |
| Tool poisoning, MCP rug pull | B3 |
| Agent impersonation | B1 |
| Confused deputy, privilege escalation | B1 |
| Approval replay | B4 |
| Parameter substitution | B1 |
| Duplicate execution | B1, B3 |
| Credential theft | B3 |
| Cross-tenant access | B1, B4 |
| Budget race | B4 |
| Denial of wallet | B1, B7 |
| Retry storm | B3 |
| Queue flooding | B1 |
| Compromised worker | Residual risks |
| Compromised connector | B3 |
| Audit manipulation | B4 |
| Supply-chain compromise | B8 |
| Unknown-outcome mishandling | B3 |
| Control-plane bypass | B1, assumptions |
| Stale-worker duplicate dispatch | B3 |
| False-negative reconciliation | B3 |
| Self-approval, cross-tenant approver | B6 |
| A stale approval after a policy change | B2 |
| Forged or replayed governance decision | B2 |
| Malicious operator or admin | B6 |
| Sensitive data in traces and audit | B3, B5, B7, residual risks |
