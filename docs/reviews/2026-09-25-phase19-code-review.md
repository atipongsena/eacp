# Phase 19 Release & Evaluation Review

Date: 2026-09-25
Scope: Slice C Phase 19 (MASTER_PLAN §49–§53, §93 and §101; ADR-018 Rev 1.0; ADR-003 §2; ADR-004 T2/T10/T16; ADR-005 §5a; §103 invariants 7, 8, 17 and 19).
Review: a self-review against ADR-018, ADR-003 and ADR-004, plus mutation checks of migration 00019.

## Invariants stated before the code

1. **Shadow cannot execute, by structure.** A candidate that is not in `CANARY` is not `ACTIVE`, so T2, T10 and T16 deny its real actions. An observation is an insert-only row with no state machine, worker, lease, dispatch intent, outbox row, reservation or secret.
2. **Two `ACTIVE` versions only in a canary.** Outside a release in `CANARY`, an agent has at most one `ACTIVE` version, under concurrency. The second one is that release's candidate, next to that release's stable version.
3. **The cohort is PostgreSQL's.** A candidate acts only for subjects whose bucket is below the current step: checked by the engine at T2, by a trigger on every move toward execution, and at T16. The stable version is not restricted.
4. **Every forward move is a second person's.** The approver did not open the release or record its evaluations, and (for the canary and after) did not create the candidate or author its allowlist.
5. **Gates are evidence, computed in PostgreSQL.** The latest result of each required suite must pass. Replay and shadow gates compare agreement rates over a minimum number of cases. Each step and the promotion need a sufficient canary report with no breach, recomputed in the same transaction.
6. **Automatic rollback only withdraws.** Only the `release` system actor rolls back automatically, only a breached canary with a sufficient report, and it can suspend only that release's candidate in the same transaction. Nothing is promoted or advanced automatically.
7. **The agent never supplies an outcome.** The PDP decides a proposal with no transaction open; PostgreSQL computes the capability denial, the reference outcome, every match, the agreement and the replay response.
8. **Journal and isolation.** Every release change, evaluation and observation is journaled in its transaction. All three tables are tenant rows under forced RLS; the one cross-tenant path is the reviewed `eacp.release_tenants()` hint.

## Implemented

- **ADR-018** (new; Accepted Rev 1.0). ADR-003 points to it for the second `ACTIVE` version.
- **Migration 00019**:
  - Tables: `eacp.agent_releases` (update limited to `state`, `canary_bp` and `change_reason`), `eacp.agent_release_evaluations` and `eacp.agent_release_observations` (insert-only), each with a guard and an audit trigger.
  - `agent_versions_release_guard` replaces the partial unique index `agent_versions_one_active`.
  - The version guard gains the narrow `release` system branch.
  - `actions_release_guard` and `eacp.dispatch_drift` enforce the cohort.
  - Functions: `release_bucket`, `release_denial`, `release_route`, the metrics, breaches and report functions, the gates and `release_evaluate()`.
  - `eacp.allowlist_tool_denial` is now shared by `action_capability_denial` and the observation guard.
  - The SECURITY DEFINER hint `eacp.release_tenants()`.
- **`internal/action`**: the engine denies an out-of-cohort candidate at T2 with a reason (`canary_cohort` or `release_not_in_canary`), instead of tripping the database backstop.
- **`internal/release`**: open, list, get (with the evaluation, replay, shadow and canary evidence), record evaluation, advance (naming the reviewed state and step), roll back, observe (PDP outside any transaction; `ErrGovernanceUnavailable` records nothing), route, and the evaluator (`Evaluate`, `EvaluateAll`, `Run`).
- **`internal/api`**: six principal routes under `/v1/releases` and two agent routes under `/v1/agent/release/`.
- **`internal/fleet`**: during a canary the view shows one row per agent, with the stable version as the active version and the canary (release, version, step) beside it. Kills of the candidate and its drift count for the agent.
- **`eacpctl release`** and **`EACP_RELEASE_INTERVAL`** (default 30s, 10s to 1h). controlplane-api runs the evaluator with the configured PDP.

## Design decisions made during the phase

- **EACP admits; the runtime routes.** EACP cannot choose which version handles a request, so the canary is a cohort EACP enforces, plus an advisory route.
- **Advance names what was reviewed.** `POST /advance` carries `from` (and `from_canary_bp`), so an approver who reviewed step 1 cannot unknowingly approve step 2 after a concurrent move.
- **Replay responds only with EACP's own record.** EACP stores no response bodies (ADR-004), so replay is limited to the reference's final state, external reference and last attempt classification. ADR-018 records this limit.
- **Keys of non-`ACTIVE` versions authenticate** (ADR-003 §5 gates actions on state, not authentication). That is what lets an `EVALUATING` candidate record observations, while every real action it submits is denied.

## Findings from the self-review (fixed)

1. **Engine backstop instead of a denial.** The first cohort test got a `409` from the database trigger for an out-of-cohort submission. The engine now checks `eacp.release_denial` in `inputs()`, so the action is `DENIED canary_cohort` with evidence; the trigger stays as the backstop.
2. **The fleet view double-counted an agent.** Its `LEFT JOIN` on `ACTIVE` versions produced two rows during a canary. It now joins the stable version and reports the canary separately, deduplicating drift across both versions.
3. **Down migration order.** The functions that take a release row type had to be dropped before the tables.

## Mutation checks

96 mutations of migration 00019's Up section were each applied alone. Each was run against `internal/release`, then the `Canary|Release` tests of `internal/action`, `internal/fleet` and `internal/api`.

**First run: 60 of 96 killed.** The 36 survivors showed that the first tests exercised the happy path of each rule but seldom the single condition that makes it fail. `internal/release/rules_test.go` adds one case per gap:

- **Opening:** duplicate suites; a candidate without an allowlist.
- **Evaluations:** a recorder named in the insert is overridden by the actor; a score equal to its threshold passes.
- **Move shapes:** a shadow with a step; a rollback that changes the step; an editor's rollback before the canary. In `CANARY` the version guard also refuses an editor, which had hidden the role check.
- **Separation of duties:**
  - an approver who opened the release;
  - a candidate created by an approver who also holds `registry_editor`;
  - an allowlist authored by the approver attempting a step.
- **Canary entry and steps:**
  - each gate alone: the shadow gate, a suite failing again in `SHADOW`, and a suspended stable version;
  - a step with a suspended candidate.
- **The canary pair:**
  - a third version beside the candidate;
  - the candidate beside a third version;
  - the stable version reactivated beside the candidate, so that its row is newer. This also pins `release_route` to the stable version, independent of row order.
- **Automatic rollback:**
  - a direct system rollback of a healthy canary;
  - a direct system rollback of an insufficient canary;
  - a system rollback of a `SHADOW` release whose refused submissions look like a breach;
  - a system suspension of a candidate after an earlier release (not this transaction's) was rolled back by an operator;
  - a quarantine in the evaluator's own transaction.
- **Observations:**
  - the candidate's own action as a reference;
  - a shadow reference from a third version;
  - another allowed tool with the same verdict (no agreement);
  - an agent-supplied replay response (discarded);
  - a proposal denied by capability paired with a reference denied the same way (no agreement; reference outcome `denied:tool_not_in_allowlist`).
- **Gates and cohorts:**
  - one agreeing replay case of two required;
  - a subject whose bucket equals the step (outside);
  - an action queued under a rolled-back release, then checked at dispatch under a later release of the same candidate whose cohort excludes its subject. This is a real path: `dispatch_drift` returns `canary_cohort`.
- **Metrics:**
  - the failure denominator at its boundary;
  - an `UNKNOWN_OUTCOME` action without an ambiguous attempt;
  - a released reservation, which is not counted as cost.

**After the fixes: 94 of 96 killed.** Two survivors are equivalent to the unmutated code:

- `breaches evaluator only`: `eacp_app` has no UPDATE grant on `breaches`, so the grant refuses the write before the trigger's check runs.
- `denial not in canary`: a candidate of a release that is not in `CANARY` is never `ACTIVE` (`agent_versions_release_guard`), so the capability check denies it (`agent_version_not_active`) before `release_denial` is reached.

Both checks stay as defence in depth.

## Verification

- `go vet ./...` and `go test -race ./...` with `EACP_TEST_ADMIN_DSN`: green (the first full run failed `TestAnotherTenantSeesAndChangesNothingAfterAFullFlow` because the three release tables had no tenant A rows. Its flow now opens a release, records an evaluation and records a replay observation).
- The compose stack rebuilt with migration 00019: `/readyz` is ok, controlplane-api logs `release_interval` and runs the evaluator without errors, and `EACP_COMPOSE_TEST=1 go test ./test/security/` passes.
- New tests: `internal/release` (27 schema tests as raw SQL against `eacp_app`, 14 of them in `schema_test.go` and 13 in `rules_test.go`; 3 service tests), `internal/action` (`TestCanaryCandidateIsDeniedOutsideItsCohort`), `internal/api` (`TestReleaseAPI`), `internal/fleet` (`TestFleetViewShowsACanaryBesideTheStableVersion`), `cmd/eacpctl` (`TestReleaseCommands`) and `internal/config`.
- `internal/storage/rls_catalog_test.go` lists the three tables, the `owner_scan` policy and `eacp.release_tenants()`.
