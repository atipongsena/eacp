# Phase 3 Governance and Approval Review

Date: 2026-09-23
Scope: Slice A Phase 3, migration 00004, `internal/governance`, `internal/approval`, and the policy/approval API routes.

## Delivered

- A provider-neutral decision type, strict local rule evaluator with all five verdicts, and independently checked JCS + SHA-256 input and enforced action bindings.
- Immutable, tenant-scoped policy versions; a single current-version pointer; distinct author and activator; decision evidence; and audited backfill of policy pointers for existing tenants.
- Durable approval requests, votes and grants. PostgreSQL triggers enforce the active policy, human approver role, request-time and current owner-group separation of duties, enabling-change separation of duties, quorum, deny short-circuit, expiry, revocation and one-time consumption. Go exposes request creation and consumption within a caller-owned transaction for Phase 4.
- Policy administration and eligible approval queue/detail/vote API routes. Detail exposes the enforced payload and digest the human would approve.

## Review findings and fixes

1. A shortened request expiry initially left an issued grant consumable. A raw-SQL regression test reproduced it; the grant trigger now checks both current request expiry and old/new grant expiry.
2. Revoking the active policy initially left votes and grants usable. Vote and consume now lock and verify the bundle; the queue hides revoked-policy requests.
3. Raw SQL initially accepted unknown policy fields that the Go evaluator rejected. The database validator now enforces the local bundle's nested field contract.
4. PostgreSQL `jsonb` expands some exponent-form numbers on read, which could make a stored policy fail Go's I-JSON validation. Both validators now reject replacement values outside the interoperable numeric range.
5. Existing tenants initially got a policy pointer before its audit trigger existed. Backfill now runs after trigger creation in each tenant context, and an upgrade test verifies the audit event.
6. An external provider could return an approval quorum or TTL outside the database's allowed range. `EvaluateChecked` now rejects those decisions before evidence persistence.

## Validation

- `go vet ./...` and `go test -race -count=1 ./...` with `EACP_TEST_ADMIN_DSN` set: passed, including PostgreSQL integration tests.
- Migration up/down/up round trip, raw SQL role/RLS tests, concurrency tests and the governance API tests: passed as part of the full suite.
- `docker compose up -d --build`, `GET /readyz`, and `EACP_COMPOSE_TEST=1 go test -count=1 ./test/security/`: passed.
- `gofmt -l cmd internal migrations` and `git diff --check`: no output.

## Phase boundary and remaining risks

- Phase 4 must add the tenant-scoped action FK and Action API, make grant consumption and queue transition one transaction, wire PDP error/alert handling into action state, and verify the release and dispatch-intent race against policy activation. Phase 3 alone cannot execute an action.
- The approval queue is limited to the oldest 100 eligible requests; pagination and operator views are later work.
- The SQL guards protect application mistakes within the `eacp_app` trust boundary. A compromised application-role credential can set transaction tenant and actor context (ADR-003); deployment credential isolation remains essential.
- Rejected vote transactions leave no durable audit event because the transaction rolls back. Successful changes are hash-chained and attributed in the same transaction. Failed-attempt security logging is separate work.
- The local provider is not an AGT/ACS conformance implementation. The Phase 9 sidecar and reference-policy parity suite remain open.
