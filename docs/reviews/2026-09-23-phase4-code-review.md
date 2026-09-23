# Phase 4 Action API and Atomic Release Boundary Review

Date: 2026-09-23
Scope: Slice A Phase 4 (MASTER_PLAN §78). This covers:
- Migration 00005.
- `internal/action` (engine, release boundary, sweeper).
- The `/v1/actions` routes.
- The Phase 3 functions replaced for the action actor context.
- Config and service wiring in `controlplane-api`.

## Delivered

- **`eacp.actions`** with ADR-004 T1–T13 and T15, and `eacp.outbox_events`. Both are tenant-scoped under RLS, with no `DELETE` and column-level `UPDATE` grants. A `BEFORE` trigger enforces the transition table, the move-specific mutable columns, the actor rules and the database clock, so raw SQL as `eacp_app` obeys the same state machine as Go. An `AFTER` trigger journals every insert and transition in the same transaction.
- **A single actor per transaction:** a principal, an authenticated agent version, or the `sweeper` system component (`eacp.actor_context()`). `eacp.actor()` stays principal-only. An agent can only act on its own agent's actions. Principals can only drive approval outcomes and cancellation.
- **Idempotent submission.**
  - Keys are unique per (tenant, agent); a replay returns the existing action and a different digest is 409.
  - Concurrent submissions with the same key create one action.
  - Static admission on `QUEUED` counts, per tenant and global, returns 429 and creates nothing.
- **Governance evaluation with no transaction open**, including a missing policy:
  - An outage or a malformed decision leaves the action `RECEIVED`, with 503, `action_id` and `Retry-After`. It is never `DENIED`.
  - A digest mismatch denies the action and raises an alert.
  - The subject and capability are checked, and denials stay auditable.
- **The ADR-005 release boundary:**
  - R0 runs outside any transaction. R1 locks the action, then reads the pointer and registry `FOR SHARE`, compares the snapshot, and repeats R0 up to three times.
  - T10 requires fresh evidence, a grant consumed in the same transaction for `escalate`, no live approval state for `allow`, a passing capability check, and the pinned active contract.
  - A deferred constraint trigger checks at commit that no grant is consumed without its action becoming `QUEUED`.
  - The budget hook, the journal and the outbox row are all in R1.
- **Approval cascades** (T6, T7, T8/T13). Votes lock the action first. A terminal action voids its outstanding approval state.
- **The sweeper:** T2a recovery, releasing approved actions, and expiry. It runs as a tracked background task, and `stop` waits for it before closing the pool. Cancellation never consults the PDP.
- **The API:** `POST /v1/actions`, `GET /v1/actions/{id}` and `POST /v1/actions/{id}/cancel`, with `?wait=` up to 60s and 200/202 for terminal and in-progress actions.
- **Config:** `EACP_ACTION_MAX_QUEUED_PER_TENANT`, `EACP_ACTION_MAX_QUEUED_GLOBAL`, `EACP_ACTION_SWEEP_INTERVAL` and `EACP_PDP_TIMEOUT`, all validated so they are never unbounded or zero.

## Review findings and fixes

1. **Down migration.** The Down section failed with `2BP01` because `assert_governance_evidence(decision_evidence, actions)` depends on the `actions` row type. It is now dropped before the table. A textual check confirmed that the four Phase 3 functions restored by Down match their 00004 definitions exactly.
2. **Mutation check: stale revalidation.** Removing the "evidence recorded in this transaction" guard at T10 went undetected. A new raw-SQL test shows that release evidence committed in an earlier transaction cannot release.
3. **Mutation check: live approval state.** Removing the "no live approval state on an allow release" guard went undetected. A new test shows that a v1 granted request blocks an allow release under v2 until it is voided.
4. **Mutation check: pinned contract.** Removing the "pin this tool's active contract" guard went undetected. A new test shows that a release pinning another tool's contract is rejected.
5. **PDP polling during a wait.** During a PDP outage, an agent's `?wait=` on an `AUTHORIZED` action attempted a release every 100ms, making 9 PDP calls in one second. After one failed attempt the wait now only reads, and retries are left to the sweeper. A test covers this, and it fails without the fix.
6. **Implementation-time issues fixed before review:**
   - An ambiguous `kind` variable in `actions_guard`.
   - The generated `operation_key` appearing NULL in `BEFORE` triggers.
   - A parameter name that collided with a keyword.
   - `audit.Verify` requiring a REPEATABLE READ transaction in tests.
7. **ADR-004 wording.** The T2a text claimed every T2a cause raises the malformed-decision alert. Only a malformed decision does, and the ADR now says so.

## Mutation checks

Each mutation removed one guard and was run against the relevant package. All 13 distinct mutations were detected. One more mutation matched its pattern twice, so it was rewritten to target only the T10 capability guard.

The SQL guards removed were:
- The fresh T10 revalidation.
- The deferred release check.
- Escalate-release grant consumption.
- Voiding before an allow release.
- The T10 capability check.
- The active-contract pin.
- "The system never cancels".
- "Principals don't drive governance".
- The outbox insert on `QUEUED`.
- Terminal voiding of approval state.

The Go checks removed were:
- The idempotency digest comparison.
- Tenant admission.
- Agent read visibility.
- The API principal role check.

## Validation

- `gofmt -l cmd internal migrations`, `go vet ./...` and `git diff --check` produced no output.
- `go test -race -count=1 ./...` with `EACP_TEST_ADMIN_DSN` passed. That includes the PostgreSQL integration tests, the migration up/down round trip and the eacpctl migrations.
- `docker compose up -d --build`:
  - `GET /readyz` reports database, role safety and schema version as ok.
  - The sweeper runs as `eacp_app` without errors.
  - `docker compose stop` exits 0 with "http stopped cleanly".
- `EACP_COMPOSE_TEST=1 go test -count=1 ./test/security/` passed.

## Phase boundary and remaining risks

- `QUEUED` is released but not executed. Phase 5 must:
  - Claim with `FOR UPDATE SKIP LOCKED`.
  - Extend the move table from T14 onwards under the same trigger discipline.
  - Publish the outbox (the rows are hints only).
  - Keep the lock order: action first.
- Admission counts are advisory under concurrency, so concurrent submissions can overshoot. This is a capacity control, not a safety invariant.
- The subject is asserted by the agent and checked only for existence and state. It is not proof of the user's consent. On-behalf-of proof is a later ADR.
- Agent, principal and system contexts are transaction settings under the `eacp_app` trust boundary (ADR-003 §8). A holder of the database credential can set them. The API sets the agent context only after key authentication.
- `?wait=` returns at the deadline for actions that are not yet terminal. In Phase 4, `QUEUED` is not terminal, so a wait on a released action runs to its deadline.
- Alerts are structured log records. Metrics and routing arrive with the telemetry work (§105).
- An independent external review (Codex) was dropped at the owner's request. This review is the implementer's own, backed by the mutation checks above.
