# Phase 8 Slice A Hardening and Demo Review

Date: 2026-09-23
Scope: Slice A Phase 8 (MASTER_PLAN §82; §103 [A] invariants; §110–111 demo; ADR-004 Rev 2.6).
Review: self-review against MASTER_PLAN §82, §103 and §111, with mutation checks of the new isolation tests.

## Implemented

- **Invariant map.** `docs/INVARIANTS.md` maps every §103 [A] invariant (1, 2, 4–8, 10–19) to the tests that prove it. `test/invariants` reads the [A] list from MASTER_PLAN §103 itself. It fails if an invariant has no section, a section has no test, or a named test does not exist in its package.
- **Evidence reconstruction (invariants 10 and 17).** `GET /v1/actions/{id}/evidence` and `eacpctl action evidence` return, in one snapshot:
  - the action;
  - every governance decision;
  - approval requests with votes and the consumed grant;
  - attempts (including late results), reconciliation checks and resolutions;
  - every journal entry about these records.

  The tenant's hash chain is verified in the same snapshot, and tampering is reported in the evidence.
- **Tenant isolation (invariant 8):**
  - A catalog test covers the RLS convention on all 25 tables, the reviewed owner-only policies, and the SECURITY DEFINER functions (their allowlist, pinned `search_path` and EXECUTE grants).
  - A behavioural sweep after a full flow populates every table and shows that tenant B and a session without a tenant see and change none of tenant A's rows.
  - The action API answers 404 across tenants.
- **Chaos:**
  - Repeated termination of every application connection while the worker, reconciler and sweeper loops run and work keeps arriving (about 100 terminations per run).
  - A restart of every process with an approval half-voted, an approved action waiting, and a call cut off after its dispatch intent.
  - Duplicate submissions, concurrent and repeated, with ERP effects counted.
- **Governance outage (invariant 18).** With the PDP down, a new action fails closed, while cancellation, reconciliation and containment still work.
- **Demo (§111).** `scripts/demo.sh` runs `test/demo` against an isolated compose project (`eacp-demo`, ports 18080 and 55433, its own volumes) and removes it afterwards. The demo:
  - bootstraps a tenant with `eacpctl`, and people, keys, the registry and the policy through the API with two-person approvals;
  - shows the agent has no route or credential to the ERP;
  - denies a tool outside the allowlist, and rejects self-approval;
  - restarts the API, the worker and PostgreSQL with an approval pending;
  - kills the worker mid-call, submits duplicates, and times out after the ERP commits;
  - leads a delayed-visibility action to human resolution;
  - reconstructs one action from its id;
  - scans API responses, logs and a database dump for the ERP credential.

## Review findings and fixes

1. **A governance outage blocked cancellation (invariant 18).** An action stays `RECEIVED` while the PDP is unavailable, and ADR-004 had no edge from `RECEIVED` to `CANCELLED`. The requester could only wait for expiry.
   - Migration 00008 adds **T5a**, for the same actors as T9, with a reason required.
   - Evaluation re-checks the state under the row lock after the PDP call, so a cancel that lands during a decision wins and no evidence is recorded (`TestCancelWinsOverAnInFlightDecision`).
   - This is recorded in ADR-004 Rev 2.6.
   - `TestIllegalAndTerminalTransitionsAreRejected` listed this edge as illegal. It now lists other edges that remain illegal, and T5a has its own raw-SQL test.
2. **A service that failed closed at startup stayed down.** In the demo, restarting the worker together with PostgreSQL made the worker exit: its startup database ping failed, which is correct fail-closed behaviour. Compose had no restart policy, so it never came back and a queued action waited forever. `controlplane-api`, `execution-worker` and `fakeerp` now run with `restart: on-failure`. ADR-004 Rev 2.6 records that deployments must supervise these services.
3. **Evidence reconstruction had no supported interface.** The existing test joined the evidence in raw SQL. The evidence API now returns the whole chain. A superuser who edits a journal entry afterwards makes `chain.verified` false in the evidence instead of failing the request.
4. **The first isolation sweep was partly vacuous.** Three tables (credentials, groups, group memberships) had no tenant A rows, so the sweep proved nothing for them. The flow now populates every table, and the test fails if any table is empty. A mutation (`CREATE POLICY leak ON eacp.actions USING (true)`) is caught.
5. **The first chaos test finished its work before the storm began.** Submissions now arrive throughout the connection storm, and a client retries a submission with the same idempotency key.
6. **Fake ERP could not hide a record long enough to exhaust reconciliation.** The visibility delay limit of this test double is raised from 5 seconds to 10 minutes, with a test at the boundary.

## Validation

- `gofmt -l .`, `go vet ./...` and `git diff --check` produced no output.
- `go test -race -count=1 ./...` with `EACP_TEST_ADMIN_DSN` passed, including every PostgreSQL integration test and `test/invariants`.
- The chaos test ran three times in a row without a failure.
- `docker compose up -d --build` migrated the development database to version 8, and `GET /readyz` is ok. `EACP_COMPOSE_TEST=1 go test -count=1 ./test/security/` passed (8 tests).
- `scripts/demo.sh` passed twice (about 55 seconds per run once the images were built) and removed the demo project afterwards.

## Known limitations

- The evidence endpoint verifies the tenant's whole journal and finds entries by scanning it: O(journal size) per request. That is accepted for Slice A; an indexed subject column is the natural next step.
- `test/security` and `test/demo` need the compose stack (`EACP_COMPOSE_TEST=1`, `EACP_DEMO=1`) and are skipped by a plain `go test ./...`. `test/invariants` checks that the tests exist; the suite and the demo check that they pass.
- The demo credentials, tenant id and ERP token are local-development values.

## Phase boundary

Slice A is complete. Slice B (Phase 9, the AGT sidecar PDP) has not started. Nothing claims exactly-once execution. The claims are "idempotent where supported", "effectively-once where reconcilable" and "at-most-once when retry is unsafe" (MASTER_PLAN §21).
