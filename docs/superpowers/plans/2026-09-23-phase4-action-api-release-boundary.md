# Phase 4 Action API and Atomic Release Boundary Implementation Plan

> **For agentic workers:** Implement the tasks in order with the Superpowers test-driven-development and verification-before-completion workflows. Each task includes a failing test, a narrow implementation, and a fresh check.

**Goal:** Deliver Slice A Phase 4 (MASTER_PLAN §78): the Action API, the pre-dispatch part of the ADR-004 state machine, idempotent submission, static admission, the ADR-005 release boundary, the budget hook, journaled transitions and the outbox. No worker, lease or dispatch (Phase 5).

**Architecture:** `migrations/00005_actions.sql` owns the `actions` and `outbox_events` tables and every transition guard. `internal/action` runs submission, governance evaluation, release and cancellation as short tenant transactions; the PDP is only called with no transaction open. PostgreSQL triggers re-check every guard, attribute the actor, append the hash-chained audit event and write the outbox row, so raw SQL as `eacp_app` obeys the same state machine. A sweeper in `controlplane-api` retries evaluation, releases approved actions and expires overdue ones.

**Tech Stack:** Go 1.27, PostgreSQL 18, pgx v5, goose, Go standard library. No new dependency.

**Spec:** `docs/MASTER_PLAN.md` §15, §18, §26, §62, §65, §78, §103; ADR-002 §4–§6; ADR-004 T1–T15; ADR-005 §2, §5, §5a, §6.

## Decisions (conservative, recorded in the ADRs)

1. **Actor context.** A transaction has exactly one actor: a principal (`app.actor_id`), an agent version (`app.agent_version_id`, set only after API-key authentication) or a named system component (`app.system_actor = 'sweeper'`). `eacp.actor()` is unchanged and still refuses non-principals, so registry and policy writes stay principal-only. The Phase 3 evidence, request, grant and governance-audit functions are replaced to accept the action actor context and to journal the correct actor kind.
2. **Subject.** The binding subject is the `subject` of an enabled human principal of the tenant, asserted by the authenticated agent. It is not proof of user consent (a later OBO ADR). An unknown, disabled or non-human subject denies the action (`subject_invalid`) so the attempt is auditable. SoD still excludes the subject from approving.
3. **Canonical payload storage.** Input and enforced payloads are stored as RFC 8785 text in `json` columns (verbatim), never `jsonb`, so a later digest recomputation is byte-exact.
4. **Side-effect class.** Governance input `side_effect_class` is the active contract's sorted side effects joined by `,` (e.g. `FINANCIAL,IRREVERSIBLE_WRITE`). Exact match only.
5. **Revalidation freshness.** T10 requires decision evidence recorded in the release transaction, the locked policy pointer equal to it, and the contract used for the evaluation equal to the locked active contract; otherwise R0 is repeated.
6. **Escalate at release without a usable grant** (policy changed, or a contract change made an allowed action escalate): T11 re-approval. A grant that expired before release expires the action (ADR-005 default), via the request.
7. **Approval-driven transitions cascade in PostgreSQL:** grant issued → T6, request denied → T7, request expired → T8/T13. Votes lock the action row before the request, as does every other action transaction (lock order: action → approval rows → registry → audit chain head).
8. **Admission** counts `QUEUED` actions, per tenant and globally, at submission; a replayed idempotency key is never rejected by admission. Counts are advisory under concurrency (overshoot bounded by concurrent submissions); admission is a capacity control, not a safety invariant.
9. **Cross-tenant scans** (global admission count, sweeper tenant list) use narrow `SECURITY DEFINER` functions and a `SELECT` policy that applies only to the schema owner role. `eacp_app` keeps strict tenant RLS.
10. **Outbox.** A trigger inserts one `action.queued` row (only `action_id`, plus `traceparent` from `app.traceparent`) in the release transaction. Publishing is Phase 5.
11. **Cancel** follows the table exactly: T9, T13, T15 only (not `RECEIVED`). Allowed for the submitting agent, the subject principal or an `operator`. Cancel never consults the PDP. Outstanding approval state is voided in the same transaction.

## Tasks

### Task 1: Migration 00005 schema and guards (raw SQL tests first)

Files: `migrations/00005_actions.sql`, `internal/action/schema_test.go`, `internal/storage/storage.go` (SetAgent, SetSystem, SetTraceparent), `internal/registry/registrytest` (action fixtures).

- [x] Tests: insert requires matching agent context and derives agent/subject/tool; idempotency uniqueness; RLS and no DELETE; illegal and terminal transitions rejected; T3/T10 reject mismatched or stale evidence and missing grant consumption; immutable identity; audit actor kinds; outbox on QUEUED; vote cascades; cancel actor rules.
- [x] Update Phase 3 fixtures to create real actions now that evidence, requests and grants reference `actions`.
- [x] Implement; run focused and race tests.

### Task 2: Engine — submission, evaluation, idempotency, admission

Files: `internal/action/action.go`, `internal/action/engine.go`, `internal/action/engine_test.go`, `internal/governance` (export `Canonicalize`).

- [x] Tests: allow/warn/transform/deny/escalate; capability and subject denials; PDP error and incomplete decision stay `RECEIVED` (alert); digest mismatch denies (alert); replay and 409; concurrent same key; admission 429 without an action.
- [x] Implement.

### Task 3: Release boundary

- [x] Tests: approve then release consumes once (N parallel releases); transform-then-approve binds the enforced payload; policy v2 escalate → new request, old voided; v2 allow → released, old voided; activation between R0 and R1; activation waits for R1; registry drift → DENIED; injected failure inside R1 leaves the grant unconsumed; expired grant → EXPIRED; evidence reconstruction.
- [x] Implement with retry on 40001/40P01 and bounded R0 repeats.

### Task 4: Sweeper and cancel

- [x] Tests: T2a recovery, T5/T8/T13/T15 expiry, release after votes, cancel from each allowed state while the PDP is down, votes rejected after cancel.
- [x] Implement `Sweeper.RunOnce` and wire the loop into `controlplane-api`.

### Task 5: HTTP API

Files: `internal/api/actions.go`, `internal/api/actions_test.go`, `internal/api/api.go`, `internal/config`, `cmd/controlplane-api`, `internal/service`.

- [x] Tests: agent-only submission, missing/invalid Idempotency-Key, 202/200/409/429/503 contracts, `?wait=` bounds, GET visibility (own agent, operator, auditor), cancel.
- [x] Implement.

### Task 6: Docs, review, validation

- [x] Update ADR-002/004/005, MASTER_PLAN §78, AGENTS.md, README; write `docs/reviews/2026-09-23-phase4-code-review.md`.
- [x] `gofmt -l`, `go vet ./...`, `go test -race -count=1 ./...` with the DSN; compose build, `/readyz`, compose security tests. Self-review with mutation checks (the owner dropped the Codex review).
- [x] Commit and stop; do not begin Phase 5.
