# Phase 8 — Slice A Hardening & Demo (MASTER_PLAN §82, §103, §110–111)

**Goal:** prove the Slice A goal statement (§110) with automated tests and a runnable demo. Every [A] invariant in §103 has at least one passing automated test, and the matrix is checked mechanically.

**Exit criterion (§82):** every §103 invariant tagged [A] (1, 2, 4–8, 10–19) maps to passing automated tests.

## Invariants this phase must demonstrate

1. **Bypass (§3.2, inv. 11):**
   - The agent runtime has no route to Fake ERP or PostgreSQL and holds no ERP credential.
   - A tool outside the allowlist is `DENIED` before governance.
   - No connector secret appears in API responses, service logs or the database.
2. **Tenant isolation (inv. 8):**
   - Every tenant-scoped table enables and forces RLS with the `tenant_isolation` policy.
   - Only reviewed owner-only policies and SECURITY DEFINER functions cross tenants.
   - After a full flow, another tenant (or no tenant) sees and changes nothing.
   - The action API returns 404 or an empty list across tenants.
3. **Chaos (inv. 4, 5, 12, 16):**
   - Dropped database connections while the worker, reconciler and sweeper run lose no action and duplicate no effect.
   - A killed worker leads to `UNKNOWN_OUTCOME` and reconciliation.
   - Duplicate submissions produce one action and one ERP effect.
   - Restarting the API, the worker or PostgreSQL loses no pending approval or in-flight action.
4. **Fail closed without blocking (inv. 18):** with the PDP down, new actions stay `RECEIVED`, while cancellation, reconciliation and containment still work.
5. **Evidence reconstruction (inv. 10, 17):** from an `action_id` alone, an auditor reconstructs the whole chain over the API, and the journal chain verifies in the same snapshot:
   - governance decisions (verdict, policy version, digests);
   - the approval request, votes and the grant consumed by this action;
   - attempts and reconciliation checks;
   - operator resolutions;
   - every journal entry.

## Tasks

1. Evidence reconstruction: extend `action.Evidence` with the action, decisions, approvals (votes, grant) and journal entries with chain verification. Covered by an end-to-end test (escalate → approve → release → lost response → reconcile) and `eacpctl action evidence`.
2. Tenant isolation: a catalog test of the RLS convention, the pinned owner-only policies and the SECURITY DEFINER allowlist. Plus a behavioural sweep across every tenant table after a full flow, and cross-tenant action API tests.
3. Chaos: a connection-loss test with live loops, an end-to-end duplicate-submission test, and a restart-durability test (fresh pools and services continue pending work).
4. Invariant 18: a PDP-outage test with cancellation, reconciliation and containment.
5. Invariant matrix `docs/INVARIANTS.md`, plus `test/invariants`, which fails if an [A] invariant has no test or a referenced test does not exist.
6. Demo:
   - an isolated compose project (`deployments/demo/compose.demo.yml`, project `eacp-demo`);
   - `test/demo` (`EACP_DEMO=1`), which drives the §111 script over the real API, restarts services and PostgreSQL, kills the worker mid-call, counts ERP records, and scans for leaked secrets;
   - `scripts/demo.sh` and `docs/DEMO.md`.
7. Docs: MASTER_PLAN §82 status, AGENTS.md, README, and the review `docs/reviews/2026-09-23-phase8-code-review.md`.
