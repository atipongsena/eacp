# Phase 27a-1 Studio rules Implementation Plan

> **For agentic workers:** executed inline (native) by the owner's request ("dev until finished phase"). Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Studio authors save agent definitions that PostgreSQL validates and turns into a registry agent, a
`REGISTERED` version and an allowlist; a second person approves or rejects through the existing guards; the runtime's
credential branch exists.

**Architecture:** Migration 00027 adds two roles, a private save marker, two Studio tables and three `SECURITY DEFINER`
functions (`studio_definition_capability`, `studio_save`, `studio_decide`). The registry guards gain a branch that
accepts `studio_author` only while `eacp.in_studio_save()` is true. Go adds `internal/studio` (a thin store) and
`/v1/studio/...` routes.

**Tech Stack:** Go 1.26, pgx v5, PostgreSQL 18 (plpgsql, `IS JSON`, jsonb).

**Spec:** `docs/superpowers/specs/2026-09-30-phase-27a-1-studio-rules-design.md`; ADR-033.

## Global Constraints

- Every rule in PostgreSQL, tested with raw SQL as `eacp_app`; SQLSTATE 23514 for definition rules, 42501 for roles,
  55000 for state.
- `SECURITY DEFINER` functions pin `search_path = pg_catalog, pg_temp`; EXECUTE only for `eacp_app` (and the owner);
  every one is listed in `internal/storage/rls_catalog_test.go`.
- New tables follow the RLS convention and get `zz_audit`; `eacp_app` has SELECT only.
- The isolation flow (`TestAnotherTenantSeesAndChangesNothingAfterAFullFlow`) writes tenant-A rows to both tables.
- Commit as the user only (no co-author trailer).

## Review Focus

- A `studio_author` calling `studio_save` and then inserting directly in the same transaction: the marker is gone
  after the function returns (test: direct insert after a save in one transaction is refused).
- A definition written with `{{` escapes: placeholder checks run on parsed values, not on raw text.
- Two concurrent grants that would break `studio_runtime` exclusivity: serialised by a per-principal advisory lock.
- Approving a version whose agent has an open release: refused with 55000.
- A version quarantined by an operator before the decision: approval refused (the version must be `REGISTERED`).

---

### Task 1: Roles (migration 00027 part 1)

**Files:** Create `migrations/00027_studio.sql`; Test `internal/studio/schema_test.go`.

- [ ] Failing tests: `TestStudioRolesAreGrantedByTwoAdmins`, `TestStudioRuntimeIsExclusive` (both orders, pending grants
  count), `TestStudioRuntimeIsForServicePrincipalsOnly`, `TestStudioAuthorIsHumanOnly`.
- [ ] Extend the `role_grants` role check; `role_grants_guard` allows a service principal `auditor` or
  `studio_runtime`, refuses `studio_runtime` to humans, enforces exclusivity under
  `pg_advisory_xact_lock(hashtextextended('role/' || principal, 0))`.

### Task 2: Definition, tables, save

- [ ] Failing tests: the definition matrix (one case per spec 3.2 rule plus the example), digest and capability;
  saving refusals (role, department, owner, direct inserts inside and after a save); allowlist equals capability.
- [ ] `eacp.studio_save_marks`, `eacp.in_studio_save()`, `eacp.studio_definition_capability(text) RETURNS text[]`,
  `eacp.studio_agents`, `eacp.studio_versions`, `eacp.studio_save(...)`, guard branches in `agents_guard`,
  `agent_versions_guard` (INSERT) and `agent_allowlists_guard`.

### Task 3: Decisions and the credential branch

- [ ] Failing tests: owner cannot decide, non-approver cannot, approval activates and retires the previous
  `ACTIVE` version, rejection retires, decided once, journaled, open release refused; `studio_runtime` proposes only
  for an approved Studio version, never approves.
- [ ] `eacp.studio_decide(uuid, boolean, text)`; `credentials_guard` branch.

### Task 4: Go store and API

**Files:** Create `internal/studio/studio.go`, `internal/api/studio.go`, `internal/api/studio_test.go`.

- [ ] Failing API tests for every route, status stage and error code.
- [ ] `studio.Service` with `Save`, `Decide`, `Agents`, `Version`, `Requests`; JCS canonicalisation of the definition
  before it is sent; status computed in SQL from the rows.

### Task 5: Catalogue, isolation, bundle, docs

- [ ] `rls_catalog_test.go`, the isolation flow, `bundle` role names, INVARIANTS (19, 17, 8), AGENTS.md rule,
  FEATURES pair, CHANGELOG, MASTER_PLAN, ADR-033 Rev 1.1 note, the program spec row.
- [ ] Full suite `go vet ./... && go test -race -timeout 45m ./...` with `EACP_TEST_ADMIN_DSN`.
