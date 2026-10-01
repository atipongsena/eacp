# Phase 28: the `run` kill scope (design)

Date: 2026-10-01 · Status: the owner chose this phase after 27b; built from the program spec (section 8, row 28)
ADR: ADR-016 Rev 1.1 · Program: [Agent Studio program](2026-09-29-agent-studio-program-design.md)

## 1. Why

ADR-016 rejected the `run` scope because no action carried an authenticated run binding: accepting a kill that
could match no action would be false containment. A Studio run (ADR-033 Rev 1.2) is such a binding, but today it is
recorded only after the action exists (`eacp.studio_run_step`), so an action could reach T14/T16 before its run is
known. This phase makes PostgreSQL bind every Studio action to its run when the action is inserted, and adds the
scope.

## 2. What it does

1. **The binding.** `eacp.actions.studio_run_id` is set by a `BEFORE INSERT` trigger and never changes. An action of
   a Studio version must name a run in its idempotency key, `studio:<run>:<index>`, and PostgreSQL checks that the
   run is `RUNNING`, of the same version, requested by the action's subject, and that step `<index>` is a
   `tool_call` of the action's tool. Anything else is refused (`42501`), so a Studio key cannot act outside a run.
   Every other action has no run.
2. **The scope.** `eacp.set_kill` accepts `run` for a Studio run of the tenant (any state: a finished run may still
   have a queued action). `eacp.action_killed` matches `run` on `studio_run_id`, so T14, T16, the claim filter, the
   pre-call check and the in-call poll all honour it with no change in Go. Clearing it is two-person as for every
   scope. Every action of a run that failed `killed` stays stopped after any resume, so a cleared kill never releases
   a step nobody is waiting for.
3. **The run stops.** A run is killed when a `tenant`, `team` (the agent's owner group, which a Studio agent does not
   have today, as for its actions), `agent`, `agent_version` or `run` kill matches it (`eacp.studio_run_killed`).
   PostgreSQL then:
   - refuses to start a run (`55000`, HTTP 409);
   - fails a due run `killed` at claim instead of leasing it;
   - fails a held run `killed` at its heartbeat, which now returns `active`, `replaced` or `killed`.
   The runtime stops a killed run and finishes nothing (PostgreSQL already did). Resuming the scope restarts nothing.
4. **Surfaces.** The heartbeat route adds `"killed"`; eacpctl and the console's kill form list `run`; the Studio page
   words `killed`; a run kill incident's affected snapshot is the run's version.

## 3. Narrower choices (conservative)

| Question | Choice |
|---|---|
| A Studio action without a valid run | Refused at insert; a Studio key never acts outside a run |
| Which kills stop a run | `tenant`, `team`, `agent`, `agent_version`, `run`; `tool` and `connector` stop its actions only |
| An in-flight call | Unchanged: the tenant epoch moved, so it settles `UNKNOWN_OUTCOME` (ADR-016) |
| A killed run's queued action | Stays queued until it expires, and stays stopped even after the kill is cleared |
| `global` | Still rejected: it needs platform authority, not run bindings |

## 4. Tests (failing first, `-race`)

- Raw SQL (`internal/studio/kill_schema_test.go`): the binding (wrong key, version, subject, tool, step, a run not
  `RUNNING` are refused; the run is set and immutable; a plain agent's action has none); `set_kill` accepts a run
  and refuses a foreign one; `eacp.action_killed`, which T14, T16 and the worker call (`internal/worker/kill_test.go`
  covers that path), matches an action of a killed run and not of another run, and still matches after the resume
  of a run that failed `killed`; start, claim and heartbeat under each run-matching scope; two-person resume.
- Runtime (`internal/studioruntime`): a killed heartbeat stops the run without a finish.
- Demo (`TestStudioDemo` S7): with the tool killed, a run's step waits; a run kill fails it; a second operator clears
  both kills and the step is never sent.
- API, eacpctl, console and Studio page tests for the new value. No SECURITY DEFINER function is new (the
  heartbeat keeps its name), so `rls_catalog_test.go` is unchanged and still passes.
