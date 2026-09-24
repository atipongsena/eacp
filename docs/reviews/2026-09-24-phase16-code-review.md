# Phase 16 Distributed Kill Switch Review

Date: 2026-09-24
Scope: Slice C Phase 16 (MASTER_PLAN §39–§40 and §90; ADR-016 Rev 1.0; §103 invariants 1, 8, 12, 17 and 18).
Review: a self-review against ADR-016, ADR-004 and ADR-014, a compose run of the NATS permission tests, and mutation checks of the kill tests.

## Invariants stated before the code

1. **PostgreSQL is the authority.** Kill state and epochs live only in PostgreSQL. A NATS message wakes a database check; it never claims, cancels or decides anything.
2. **Fenced before dispatch.** A kill that commits before T16 prevents the dispatch intent from committing, so no external call follows. T14/T16 check in the triggers, so raw SQL is fenced too.
3. **Serialized with completion.** Completion and `eacp.set_kill` take the same tenant advisory lock. A result can't be classified against a kill that is still uncommitted.
4. **Unknown, never proven.** A kill or any tenant epoch change during execution records `UNKNOWN_OUTCOME` for reconciliation. That holds even if the connector claimed success or the scope was already resumed. Cancellation never proves an effect absent.
5. **Containment is one operator; resume is two.** Only an `operator` activates. A different operator resumes. Every change is journaled with actor, AGT reason code and message.
6. **No false containment.** Only scopes with an authoritative action binding are accepted. `global`, `run` and `model` are rejected.
7. **Isolation.** Both tables are tenant rows under forced RLS. The only write path is the reviewed `SECURITY DEFINER` `eacp.set_kill`, and targets must be tenant-local.

## Implemented

- **ADR-016** (new; Accepted Rev 1.0), with the upstream AGT kill-switch source check in `research/REFERENCES.md`.
- **Migration 00016.** `eacp.kill_states` and `eacp.kill_tenant_epochs`; `eacp.set_kill`; `eacp.action_killed`; the `actions_kill_before` trigger (T14/T16 check, dispatch epoch pin); the `kill.changed` outbox topic (the outbox guard now ties each topic to its source row); `claimable_actions` leaves killed actions out.
- **`internal/worker`.** `Intent` returns `Killed` (T17 back to `QUEUED`). `CheckKill` runs before the call and on every kill poll or signal. `Complete` serializes on the kill lock and records `kill_interrupted` as ambiguous. `Worker.NotifyKill` wakes every in-flight check.
- **`internal/messaging`**, **`cmd/execution-worker`**: the relay publishes `kill.changed` on `EACP_EVENTS`, and the worker subscribes to `eacp.events.*.kill.changed`.
- **`internal/kill`, `internal/api`, `eacpctl`.** `POST|GET /v1/killswitch`, `eacpctl kill activate|resume|list`.

## Findings from the self-review (fixed)

1. **The worker's NATS credential could not hear kill signals.** `test/security/nats_test.go` expected the `worker` user to subscribe to `eacp.events.*.kill.changed`, but `deployments/docker/nats/nats.conf` still allowed only `_INBOX.>`. On the compose stack, the worker's subscription was refused, so signals never arrived (polling still contained the call). The worker user may now subscribe to that subject and nothing else under `eacp.events`. The test was confirmed to fail on the old config and pass on the new one.
2. **The kill poll wrote the action row every second.** Every poll tick and signal ran the lease-extending `Heartbeat` `UPDATE`, which is one write per in-flight call per second. The poll and the signal now run the read-only `CheckKill`; the lease keeps its `Lease/3` tick. `TestKillPollDoesNotWriteTheActionRow` checks that the row's `xmin` does not change across polls.
3. **Two latency tests did not isolate what they tested.** With the default 5-second lease, the heartbeat (which also reports kills) fired at ~1.7s, inside the deadlines of `TestKillDuringCallForcesUnknownOutcome` and `TestKillSignalWakesEveryInFlightDatabaseCheck`. So removing the poll or the signal wake-up went unnoticed. Both tests now use a 30-second lease.
4. **Nothing tested the dispatch epoch pin.** Without it, every call in a tenant after any earlier kill would become `UNKNOWN_OUTCOME`. `TestEarlierKillDoesNotTaintALaterDispatch` covers it. `TestKillResumedDuringCallStillLeavesOutcomeUnknown` covers the other side: a kill resumed during the call.

## Mutation checks

Each mutation was applied alone and the kill tests of `internal/worker` were run. All 15 were killed (after the fixes above):

- **Migration:** the T14/T16 trigger check; the two-person resume; the operator role; the tenant-local target; `set_kill`'s advisory lock; the `connector` and `team` scope matches; the claim hint filter; the dispatch epoch pin.
- **Worker:** `Complete`'s advisory lock; `Complete` ignoring the epoch; `Intent`'s `Killed` decision; the pre-call check; the signal wake-up; the kill poll.

## Accepted as is

- **One kill lock per tenant.** T14, T16 and completion take the tenant's kill advisory lock exclusively. These transactions already serialize on the tenant's audit chain head, so the extra contention is small. A shared lock for checkers (exclusive for `set_kill`) would be a later optimization.
- **Down migration with pending kill signals.** Like every earlier Down, 00016 changes the schema only. If `kill.changed` outbox rows remain, re-adding the action FK fails and the rollback aborts. It fails closed and deletes nothing.
- **Overreporting.** Any tenant epoch change during a call makes that call unknown, even for an unrelated scope (ADR-016 §4). This is conservative by design.

## Not done (by design)

- `global`, `run` and `model` scopes (ADR-016 "Scope and limits"): they need platform operator authority and authenticated run/model bindings on actions.
