# Phase 17 Fleet Operations Review

Date: 2026-09-25
Scope: Slice C Phase 17 (MASTER_PLAN §38, §65–§66 and §91; ADR-024 Rev 1.0; ADR-003 §2; §103 invariants 8, 17 and 19).
Review: a self-review against ADR-024, ADR-003 and ADR-016, plus mutation checks of the fleet tests.

## Invariants stated before the code

1. **No new authority.** A fleet operation is a set of ADR-003 lifecycle transitions. Each one passes the existing version guard, so an operation can't grant what the actor couldn't grant version by version. Pause and quarantine are containment by one person. Resume, release and rollback need a `registry_approver` under the activation, allowlist-author and quarantiner rules.
2. **Each kind makes only its own transition.** A kind never borrows another legal transition. For example, a "pause" can't release a quarantined version, and a "release" can't retire one.
3. **Atomic and evidenced.** An operation commits all its transitions or none. PostgreSQL records each target with the `from` state it read, binds targets to the transaction and actor that created the operation, and journals the operation (actor, reason, selector, targets) last.
4. **Undo only what was done.** Resume and release change only versions their source pause or quarantine changed, and still hold that state.
5. **A rollback is one agent, backwards.** It suspends the ACTIVE version, if there is one, and activates exactly one older SUSPENDED version of the same agent.
6. **Explicit selection.** An empty selector selects nothing, and the whole tenant needs `all`. Named agents must exist. Every operation is bounded at 500 versions.
7. **The view observes; it never decides.** It reads one tenant snapshot and triggers nothing. Its drift rule agrees with the capability check.
8. **Isolation.** Both tables are insert-only tenant rows under forced RLS, and a target in another tenant is not found.

## Implemented

- **ADR-024** (new; Accepted Rev 1.0).
- **Migration 00017.** It adds:
  - `eacp.fleet_operations` (kind, selector, reason, source, actor, time, creating transaction id) and `eacp.fleet_operation_targets` (version, agent, ordinal, `from_state`, `to_state`), both insert-only.
  - `fleet_operations_guard`: principal, early role check, reason, and the source-kind rules.
  - `fleet_operation_targets_guard`: the target row *is* the transition. It locks the version, checks the kind's pair, the undo-source rule and the rollback rules, then updates the version with `fleet <kind> <id>: <reason>`.
  - The deferred constraint trigger `fleet_operations_commit`: at least one target, exactly one activation for a rollback, and the `fleet.<kind>` audit event.
- **`internal/fleet`.** `Apply` validates the request, plans the targets (locking versions in id order, with a rollback suspending first) and inserts the operation. A dry run shares the same transaction and rolls it back. There are also `Operation`, `Agents` and `Health`, the read-only view.
- **`internal/api`** adds `GET /v1/fleet/agents`, `GET /v1/fleet/health`, `POST /v1/fleet/operations` and `GET /v1/fleet/operations/{id}`. **`eacpctl`** adds `fleet status|list|operation|pause|quarantine|resume|release|rollback`.

## Design decisions made during the phase

- **Pause is suspension, not a hold.** A fleet pause uses the existing `SUSPENDED` state. Queued and new actions are therefore denied (T2/T16b), and calls in flight continue. `TestFleetPauseDeniesQueuedAndNewWork` shows this end to end through the worker. Holding work or interrupting calls is the kill switch's job (ADR-016). A second hold state would have created two sources of truth for whether an agent may act.
- **The view repeats the tool rules.** `eacp.action_capability_denial` takes `FOR SHARE` locks, which a read-only transaction cannot. The view evaluates the same four tool conditions in the same order without locks, and `TestFleetDriftAgreesWithTheCapabilityCheck` compares the two for every reason the application can produce.
- **No canary count.** ADR-003 allows one ACTIVE version, so the dashboard reports none rather than a fabricated zero-cost field.

## Findings from the self-review (fixed)

1. **A kind could borrow another legal transition.** The first tests only tried pairs the version guard also refuses, so dropping the kind's `from` check (pause) or `to` check (release) went unnoticed. With those checks gone, a registry approver's "pause" of a quarantined version would have released it, and a "release" could retire or revoke. `TestKindCannotBorrowAnotherLegalTransition` covers both.
2. **The one-agent rollback rule was masked.** The spanning test failed on the "suspend first" rule before reaching the agent check. `TestRollbackCannotSpanTwoAgents` now uses a second agent with no ACTIVE version, where the agent check is the only barrier.
3. **The default rollback target was not pinned.** Picking any SUSPENDED version (a newer one included) was refused by the database, so the error kind hid the Go bug. `TestRollbackIgnoresNewerSuspendedVersions` shows that a correct rollback succeeds when a newer SUSPENDED version exists.
4. **Actor rebinding.** `storage.SetActor` may be called twice in one transaction. `TestTargetsMustBeAddedByTheOperationsActor` shows that another principal still can't extend the operation.
5. **Kill matching in the view** was tested only for the `agent` scope. `TestFleetViewMatchesEveryKillScope` covers tenant, team, agent, agent version, tool and connector, and checks that a bystander is matched only by the tenant kill.
6. **Resume after a replacement.** `TestResumeSkipsAgentsWithAnotherActiveVersion` shows that an agent that activated another version is skipped, and the rest are resumed.

## Mutation checks

Each mutation was applied alone, and `internal/fleet` was run. 25 of 26 were killed:

- **Migration:**
  - transaction and actor binding;
  - the pause and release pairs;
  - the undo-source, source-kind and no-source rules;
  - the early role check;
  - one agent and older-only for a rollback;
  - non-empty and one-activation at commit;
  - the operation's audit event.
- **Go:**
  - the environment and tool filters;
  - named agents must exist;
  - `all` is exclusive;
  - dry run;
  - the default rollback target;
  - the "another version is ACTIVE" skip.
- **View:**
  - contained on kill;
  - team and connector kill matching;
  - the drift rule order;
  - removed group members don't count as owners.

Survivor (equivalent): the rollback rule "suspends first". Only one version can be ACTIVE, so no reachable state puts a suspension after an activation in the same rollback. The unique index and the primary key already refuse every such sequence. The rule stays as defense in depth.

## Not done (by design)

- **Canary, upgrade and evaluation gates** belong to ADR-018 (Release & Evaluation).
- **Deployment.** EACP does not deploy agent code. A rollback changes which version is authorized, and the runtime must use that version's credential (ADR-003 §5).
- **The Slice C demo** (MASTER_PLAN §111): MCP drift → blast radius → kill the affected version → trace and audit evidence. Every capability exists, but the compose demo has no MCP server yet. The demo is a separate piece of work.
