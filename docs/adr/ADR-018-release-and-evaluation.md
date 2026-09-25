# ADR-018: Agent releases: evaluation, replay, shadow, canary and rollback

Status: Accepted (Rev 1.0, 2026-09-25). Scope: Slice C Phase 19 (MASTER_PLAN §49–§53, §93 and §101).
Related: ADR-003 §2 (version lifecycle, one `ACTIVE` version), ADR-004 (T2/T10/T16), ADR-005 §5a (no PDP call inside a transaction), ADR-012 (budgets), ADR-016 (kills), ADR-024 (fleet operations), ADR-025 (FinOps).

## Context

MASTER_PLAN §49 puts a pipeline in front of production: evaluations, replay, shadow, canary, then production. §51 says replay is research-grade: LLM agents are not deterministic, so replay answers from recorded tool responses and compares distributions, not exact results. §52 says a shadow must be non-destructive **by structure**, not by a flag. §53 steps a canary through 1, 5, 25, 50 and 100 %, and rolls it back automatically when the SLO drops, policy violations increase, unknown outcomes rise, or cost or latency spikes.

ADR-003 allows at most one `ACTIVE` version per agent, with a partial unique index, and leaves canaries with two active versions to their own ADR. This is that ADR.

EACP is not the agent's router. An agent runtime calls EACP with a key bound to one version (ADR-003 §3). EACP cannot choose which version handles a request; it can only decide what each version may do.

## Decision

1. **A release is a PostgreSQL row with a fixed plan.** `eacp.agent_releases` moves one agent from its `ACTIVE` version (the *stable*, read by the database at opening) to a newer *candidate*:

   ```text
   EVALUATING ──► SHADOW ──► CANARY (step 1 … n) ──► PROMOTED
        └────────────┴────────────┴──────────────────► ROLLED_BACK
   ```

   - A `registry_editor` or `registry_approver` opens it with a reason. The candidate is a `REGISTERED` or `SUSPENDED` version of the same agent. One release per agent is open at a time.
   - The plan is fixed at opening:
     - required evaluation suites (at least one);
     - replay and shadow gates (minimum cases, minimum agreement);
     - canary steps in basis points (strictly increasing, the last is 10 000; default 100, 500, 2 500, 5 000, 10 000);
     - the minimum candidate actions per step (default 20);
     - the guardrails of §6.
   - The table is a tenant table under forced RLS. Its guard trigger allows only the transitions below, sets every `*_by` and `*_at` column, and journals each change in the same transaction.

2. **Evaluation is recorded evidence.** `eacp.agent_release_evaluations` is insert-only. Each row has a suite, a score, a threshold, a SHA-256 dataset digest and an evidence reference. PostgreSQL computes `passed = score >= threshold`. A `registry_editor` or `registry_approver` records results while the release is `EVALUATING` or `SHADOW`. The **evaluation gate** holds when the latest result of every required suite passed. EACP does not run evaluations; a result is an attestation by the principal who recorded it, and the approver who acts on it must be someone else (§5).

3. **Replay and shadow observe; they never execute.** The candidate's runtime records what it *would* do in `eacp.agent_release_observations`, with the candidate's agent key:
   - **Replay** (`EVALUATING` only): the reference is a terminal action of another version of the same agent. When the candidate proposes the reference's tool and payload, the response is the reference's recorded outcome (its final state, its external reference and its last attempt's classification). That is EACP's record/replay connector. Otherwise the response is a miss. Nothing else is answered, because EACP stores no response body (ADR-004).
   - **Shadow** (`SHADOW` only): the reference is an action of the stable version submitted since the shadow began. The candidate proposes its action for the same request (the same subject), and gets no response.
   - For each observation, PostgreSQL computes:
     - the candidate's capability denial against its own allowlist and the tool rules of `eacp.action_capability_denial`, which now share `eacp.allowlist_tool_denial`;
     - the reference's outcome: its governance verdict, or `denied:<reason>` when it was denied before governance;
     - `tool_match`, `outcome_match` and `payload_match` (canonical payload text);
     - `agrees = tool_match AND outcome_match AND` no candidate capability denial.
   - The control plane calls the PDP for the candidate's proposal with no transaction open, only when the capability check passes. It records the verdict and the policy version, which must still be current at insert. The agent never supplies a verdict.
   - Each reference is used at most once per release and kind. A gate compares **rates** (the share of agreeing observations), never single results.

   **Shadow is non-destructive by structure:**
   - Until `CANARY` the candidate is not `ACTIVE`, so T2, T10 and T16 deny every real action it submits (`agent_version_not_active`). The candidate can become `ACTIVE` only by the release's canary transition (§4).
   - An observation lives in its own insert-only table with no state machine. No worker, lease, dispatch intent, outbox row, budget reservation or connector secret is involved. The worker dispatches only `eacp.actions` rows after a T16 intent (ADR-004).
   - The API process holds no connector secrets (ADR-001).

   So a shadow has no path to a system of record, whatever flag a runtime sets.

4. **Canary is a cohort that PostgreSQL enforces.**
   - Entering `CANARY` activates the candidate in the same transaction, through the version guard. The approver must meet ADR-003's grant rules: a `registry_approver` who is neither the version's creator nor the author of its allowlist.
   - The partial unique index `agent_versions_one_active` is replaced by `agent_versions_release_guard`. It serialises activations per agent (the version-numbering advisory lock) and allows a second `ACTIVE` version only when that version is the candidate of this agent's release in `CANARY`, next to that release's stable version. It raises `23505` as the index did.
   - The canary serves a deterministic cohort of subjects: `eacp.release_bucket(release, subject principal)` is the first 32 bits of SHA-256 over the release id and the subject, modulo 10 000. A candidate's action is allowed only when its subject's bucket is below the current step (`eacp.release_denial`); otherwise it is `DENIED canary_cohort`. A candidate of an open release that is not in `CANARY` is `DENIED release_not_in_canary`.
   - The check runs at T2 (the engine), as a backstop on every move into `PENDING_APPROVAL`, `AUTHORIZED` or `QUEUED` (the `actions_release_guard` trigger), and at dispatch intent (`eacp.dispatch_drift`, T16b). Steps only grow, so a subject admitted at one step stays admitted.
   - The stable version is not restricted. A runtime asks `GET /v1/agent/release/route?subject=` which version serves a subject. Routing is the runtime's job; admission is EACP's.

5. **Separation of duties.**
   - Every forward move (`SHADOW`, `CANARY`, each step, `PROMOTED`) is made by a `registry_approver` who:
     - did not open the release;
     - recorded none of its evaluations;
     - is neither the candidate's creator nor the author of its active allowlist.
   - Rollback is containment: any `operator` or `registry_approver`, from any open state, with a reason.
   - Promotion suspends the stable version in the same transaction.
   - Rollback suspends the candidate if it is `ACTIVE`.
   - Each version change goes through the version guard and is journaled.

6. **Guardrails compare the candidate with the stable version over the current step.** `eacp.release_canary_report` reads, for each version, the actions created since the step began. It computes:
   - **policy violations:** the `DENIED` share of actions;
   - **failures:** `FAILED / (SUCCEEDED + FAILED)`;
   - **unknown outcomes:** the share of dispatched actions that are or were in an unknown outcome (a reconciliation check, an ambiguous attempt, or `UNKNOWN_OUTCOME`, `RECONCILING` or `NEEDS_HUMAN_RESOLUTION`);
   - **latency:** p95 of completed attempts, from dispatch to completion;
   - **cost per action**, per unit: budget reservations that were not released, plus priced OTel LLM usage of the version (ADR-025). Unpriced usage is not counted.

   A breach is a candidate rate above the stable rate plus the plan's tolerance (default +0.05 denials, +0.05 failures, +0.01 unknown outcomes), a p95 latency above 1.5× the stable's, or a cost per action above 1.5× the stable's in any unit. A stable rate with no data counts as zero. A unit where the stable has no cost but the candidate does is a breach. The report is *sufficient* once the candidate has the plan's minimum number of actions in the step. A step or a promotion needs a sufficient report with no breach, checked in the same transaction.

7. **Automatic rollback only withdraws.**
   - `eacp.release_evaluate()` runs as the `release` system actor, from the control plane every `EACP_RELEASE_INTERVAL` (default 30 s, from 10 s to 1 h). It rolls back each `CANARY` release whose sufficient report has a breach, recording the breaches.
   - The release guard recomputes the report; the system actor cannot roll back without a breach, and cannot make any other move.
   - The version guard gains one narrow system branch: the `release` actor may move a version `ACTIVE → SUSPENDED` only when the version is the candidate of a release rolled back by the system in the same transaction. The journal shows the system as the actor.
   - Promotion and every step stay human (§5). EACP never grants capability automatically.

8. **Interplay.**
   - Kills (ADR-016) apply to both versions as usual; a release never creates or clears one.
   - Fleet operations (ADR-024) still pass through the version guard and `agent_versions_release_guard`. Pausing contains both versions. A fleet rollback or resume that would leave a non-canary second `ACTIVE` version is refused.
   - The fleet view reports the stable version as the active version, and the canary version and its release separately.
   - Budgets and FinOps count both versions under the agent.

## Interfaces

| Route | Caller | Purpose |
|---|---|---|
| `POST /v1/releases` | `registry_editor`, `registry_approver` | Open a release with its plan |
| `GET /v1/releases`, `GET /v1/releases/{id}` | registry roles, `operator`, `auditor` | List; detail with evaluations, failing suites, replay and shadow summaries and the canary report |
| `POST /v1/releases/{id}/evaluations` | `registry_editor`, `registry_approver` | Record an evaluation result |
| `POST /v1/releases/{id}/advance` | `registry_approver` | Move one stage forward. The body names the state (and, in `CANARY`, the step) the approver reviewed; a release that has moved since is a `409` |
| `POST /v1/releases/{id}/rollback` | `operator`, `registry_approver` | Roll back, with a reason |
| `POST /v1/agent/release/observations` | the candidate's agent key | Record a replay or shadow proposal. `503 governance_unavailable` records nothing |
| `GET /v1/agent/release/route?subject=` | an agent key of the agent | The version that serves a subject (advisory) |

`eacpctl release list|show|open|evaluation|advance|rollback` wraps the principal routes.

## Unresolved assumptions

| Assumption | Conservative choice |
|---|---|
| Who may attest an evaluation result | Registry roles record it; an approver who recorded none of the release's results must act on it (§5). EACP does not verify the evaluation itself. |
| Statistical significance of guardrails | Rates over a minimum sample with fixed tolerances. No significance test; small samples block progress instead of passing it. |
| A stable version with no traffic in a step | Its rates count as zero, and any candidate cost in a unit the stable has none of is a breach. Latency is compared only when both have completed attempts. |
| Cohort key | The action's subject principal. Unknown subjects are outside every cohort. |
| Automatic rollback | Only on a sufficient report with a breach. It suspends the candidate; nothing is promoted or advanced automatically. |
| Replay responses | Only EACP's recorded classification of the reference action. Response bodies are not stored (ADR-004), so replay cannot judge result quality. |
| Rolling back before `CANARY` | Recorded as `ROLLED_BACK`. The candidate never became `ACTIVE`, so nothing is suspended. |

## Not decided here

- Running evaluations or replay harnesses inside EACP. EACP records and gates; a harness drives the candidate's runtime.
- Traffic mirroring. The runtime or a proxy sends the shadow request; EACP pairs it with the stable action.
- Statistical tests (for example, sequential tests) instead of fixed tolerances.
- Canary by weight without a subject key.

## Verification

- `internal/release/schema_test.go` checks every rule as raw SQL against `eacp_app`:
  - release opening and the fixed plan;
  - the evaluation gate, and who may record a result;
  - replay and shadow observations and their computed fields;
  - the structural shadow claim: an observation creates no action, outbox row, attempt or reservation, and a candidate's real submission is denied;
  - the second `ACTIVE` version and the cohort at T2, T10 and T16;
  - separation of duties;
  - the guardrails, automatic rollback by the system actor and its limits;
  - promotion and rollback;
  - the journal.
- `internal/release/rules_test.go` pins, one case each, the single conditions that the mutation run of migration 00019 showed untested (docs/reviews/2026-09-25-phase19-code-review.md).
- `internal/release` service tests cover observations with the PDP, routing and the evaluator. `internal/api` tests cover the routes and roles. `internal/fleet` tests cover the fleet view with a canary.
- `TestFleetDriftAgreesWithTheCapabilityCheck` stays green with the shared tool rules.
