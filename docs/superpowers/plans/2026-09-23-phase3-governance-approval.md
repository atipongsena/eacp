# Phase 3 Governance and Approval Implementation Plan

> **For agentic workers:** Implement the tasks in order with the Superpowers test-driven-development and verification-before-completion workflows. Each task includes a failing test, a narrow implementation, and a fresh check.

**Goal:** Deliver the Slice A Phase 3 governance provider, local policy evaluation, decision evidence, and durable action-bound approvals without starting the Phase 4 Action API.

**Outcome:** Phase 3 implementation and review are recorded in `docs/reviews/2026-09-23-phase3-code-review.md`. Checklist lines below preserve the original test-first plan; any Action API, alert emission, or release-boundary scenario belongs to Phase 4.

**Architecture:** The pure Go provider evaluates an immutable versioned policy bundle and returns a complete decision. EACP computes both RFC 8785 digests independently. PostgreSQL owns policy activation, approval eligibility, state transitions, one-time grant consumption, RLS, and audit through triggers. Go services expose typed operations and HTTP routes but do not replace the database guards.

**Tech Stack:** Go 1.27, PostgreSQL 18, pgx v5, goose migrations, Go standard library. No new dependency.

**Spec:** `docs/MASTER_PLAN.md` §77; `docs/adr/ADR-002-agt-integration-sidecar-pdp.md`; `docs/adr/ADR-003-agent-registry-identity-and-capability.md`; `docs/adr/ADR-004-action-state-machine-and-execution-semantics.md`; `docs/adr/ADR-005-approval-ownership-and-atomic-execution-boundary.md`.

## Global Constraints

- The migration is `migrations/00004_governance_approvals.sql`; committed migrations stay untouched.
- Every tenant table has ENABLE and FORCE RLS, a `tenant_isolation` policy, tenant-scoped references, and no DELETE grant.
- The app role is `eacp_app`; actor identity comes from `storage.SetActor`; all privileged mutations are audited in the same transaction.
- Policy versions and decision evidence are insert-only. Policy activation is two-person and moves monotonically to newer versions.
- The request records the union basis for group-owner SoD at creation; the vote also checks current membership and role under row locks.
- Grants bind tenant, action, enforced digest, and policy version; consumption is conditional and can succeed once only.
- There is no `actions` table until Phase 4. Phase 3 stores `action_id` as a tenant-scoped future reference; Phase 4 adds the FK and atomic release transaction.
- Any malformed or unavailable governance decision leaves a side-effecting action unable to execute.

## Review Focus

- Duplicate JSON keys or invalid Unicode must fail digest creation rather than silently change what is approved.
- JSON number serialization and UTF-16 key sorting must agree with RFC 8785 vectors.
- A policy with no matching rule must deny; a malformed policy cannot activate.
- A vote racing with a role or group-membership change must use a serialized, current snapshot.
- A grant cannot be consumed after expiry, digest substitution, or policy activation, including through raw SQL.

---

### Task 1: Canonical action binding and two digests

**Files:** Create `internal/governance/digest.go`, `internal/governance/digest_test.go`.

**Interface:** `DigestBinding(Binding) ([32]byte, error)` and `Digests(Binding, json.RawMessage) (input, enforced [32]byte, error)`. Binding fields follow ADR-005 §2.

- [ ] Write literal RFC 8785 canonicalization vectors, parameter substitution, transform binding, duplicate-key, invalid-Unicode, and unsafe-number tests.
- [ ] Run `go test ./internal/governance -run 'TestDigest|TestCanonical' -count=1`; confirm the new tests fail because the implementation is absent.
- [ ] Implement strict I-JSON parsing, canonical UTF-16 sorting and ECMAScript-compatible primitive serialization, then SHA-256 over the complete binding.
- [ ] Run the focused tests and `go test -race ./internal/governance`.

### Task 2: Governance interface and deterministic local provider

**Files:** Create `internal/governance/governance.go`, `internal/governance/local.go`, `internal/governance/local_test.go`.

**Interface:** `GovernanceProvider.Evaluate(context.Context, GovernanceRequest) (GovernanceDecision, error)` as ADR-002 §1; `LocalProvider` evaluates a versioned bundle supplied in the request.

- [ ] Write tests for all five verdicts, first matching rule, default deny, deterministic transform plus escalation, invalid policy, and provider error failing closed.
- [ ] Run the focused tests and confirm they fail for the missing provider.
- [ ] Implement a versioned JSON rule format with exact context predicates, constant JSON payload replacements, and strict validation. No external calls or approval state inside the provider.
- [ ] Run focused and race tests.

### Task 3: Policy versions, pointer and decision evidence

**Files:** Create `migrations/00004_governance_approvals.sql`, `internal/governance/schema_test.go`, `internal/governance/store.go`.

**Interface:** Policy insert and activation, current-policy read under `FOR SHARE`, immutable decision evidence append.

- [ ] Write raw-SQL schema tests as `eacp_app` for two-person activation, immutable versions, monotonic pointer, RLS, policy drift and evidence completeness.
- [ ] Run the focused PostgreSQL tests with `EACP_TEST_ADMIN_DSN`; confirm missing-table failures.
- [ ] Add the new migration with tenant RLS, composite references, actor/role triggers, column UPDATE grants and row-change audit triggers. New tenants get a pointer row; old tenants are backfilled.
- [ ] Add the Go store methods using tenant transactions and DB error mapping; run focused and race tests.

### Task 4: Approval request, votes and grants

**Files:** Extend `migrations/00004_governance_approvals.sql`; create `internal/approval/approval.go`, `internal/approval/schema_test.go`, `internal/approval/service_test.go`; extend `internal/registry/registrytest/registrytest.go`.

**Interface:** `CreateRequest`, `Vote`, `Consume` and read methods. `Consume` takes a caller transaction so Phase 4 can put it inside the release transaction.

- [ ] Add three human `approver` principals to the fixture through the owner bootstrap path.
- [ ] Write raw-SQL tests for self/owner/enabling-change SoD, tenant isolation, role revocation, creation-time plus vote-time membership union, quorum, deny short-circuit, expiry and duplicate votes.
- [ ] Write tests for a second consume, digest substitution and stale policy version, including concurrent consume and pointer activation serialization.
- [ ] Run the focused tests and confirm missing-table failures.
- [ ] Implement request/vote/grant tables and DB triggers so direct SQL obeys every guard; implement the Go transaction methods; run focused and race tests.

### Task 5: HTTP and evidence integration

**Files:** Modify `internal/api/api.go` and `internal/api/api_test.go`; add focused handler files if the existing file grows unwieldy.

**Interface:** Policy create/activate/read for admins; approval queue/detail/vote for authenticated human approvers. Internal governance evaluation persists required evidence. No Action API yet.

- [ ] Write API tests for authenticated role checks, enforced payload and digest shown to the approver, votes, cross-tenant access and PDP errors.
- [ ] Run the focused tests and confirm failure for absent routes.
- [ ] Add routes and handlers that call the services; DB rechecks authorization; keep errors safe for the caller.
- [ ] Run the focused and race tests.

### Task 6: Documentation, review and final validation

**Files:** Update `AGENTS.md`, `README.md`, `docs/MASTER_PLAN.md` §77, `docs/adr/ADR-002-agt-integration-sidecar-pdp.md`, `docs/adr/ADR-005-approval-ownership-and-atomic-execution-boundary.md`, and `docs/adr/README.md`; add a Phase 3 review note.

- [ ] Record the local bundle format, policy activation and approval defaults in the ADRs, and state the DB credential trust boundary without enlarging Phase 3.
- [ ] Run trigger mutation checks, review `git diff`, and run `gofmt -l .`, `go vet ./...`, `go test -race -count=1 ./...` with the PostgreSQL DSN.
- [ ] Build the compose stack, run `/readyz` and `EACP_COMPOSE_TEST=1 go test -count=1 ./test/security/`.
- [ ] Stop after Phase 3; report what changed, exact validation, assumptions and remaining risks. Do not begin Phase 4.
