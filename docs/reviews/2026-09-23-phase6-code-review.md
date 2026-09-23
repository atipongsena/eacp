# Phase 6 Connector Framework and Fake ERP Review

Date: 2026-09-23
Scope: Slice A Phase 6 (MASTER_PLAN §80).

## Implemented

- The execution worker registers an HTTP connector. After the existing fenced dispatch intent commits, it sends the enforced payload with the stable operation key under the pinned native or correlation-only contract. Its bounded responses require a reference for success and a certified class with no reference for definitive no-effect. Timeouts, resets, malformed responses, uncertified errors and contradictory fields remain ambiguous.
- The HTTP client refuses redirects and environment proxies, and disables Go's automatic POST replay on reused connections. Lookup reports found, absent or unknown evidence; it makes no reconciliation transition.
- Fake ERP requires a worker bearer credential for privileged calls. Its operation and audit log is synced before a committed effect is acknowledged and loaded on restart. An uncertain log write halts privileged operations. A native operation key deduplicates; correlation-only duplicates produce a lookup conflict. The `create_po` tool has immediate lookup visibility, while `create_po_eventual` can delay visibility.
- The Fake ERP can simulate the Phase 6 success and failure modes: validation failure before effect, rate limit, 5xx before or after effect, outage, slow response, timeout, connection reset and delayed visibility. Its audit entries identify the worker principal without storing the credential.
- A PostgreSQL integration test sends real HTTP requests from the worker to Fake ERP. It verifies success, a committed effect with lost response becoming `UNKNOWN_OUTCOME`, positive lookup evidence, no automatic redispatch, and no credential in action, attempt, audit or outbox rows.

## Review findings and fixes

1. Go can automatically replay a POST with `Idempotency-Key` and `GetBody` after a reused connection fails. The connector now clears `GetBody`; a regression test proves it sends no second POST after a response is lost.
2. A generic HTTP 404 could otherwise be mistaken for authoritative absence. Lookup now requires a bounded JSON `not_found` response with no reference; a router 404 is unknown.
3. A certified no-effect result with an external reference is contradictory. The worker now classifies it as ambiguous, including when secret scrubbing removes the reference.
4. Fake ERP audit logging originally accepted untrusted path text, which could contain a credential. It now records only route templates and validated identifiers.
5. A write or sync failure in the Fake ERP operation log makes effect status uncertain. The handler now stops serving privileged operations until restart and fails closed on a corrupt log.
6. A correlation-only duplicate with one delayed record could look like a single visible success. Lookup now reports a conflict whenever the durable log holds multiple effects for the key, including before delayed records become visible.

## Validation

- `go test -race -count=1 ./...` with `EACP_TEST_ADMIN_DSN` passed, including PostgreSQL integration tests.
- `go vet ./...`, `gofmt -l` on changed Go files, and `git diff --check` produced no output.
- `docker compose up -d --build` passed. `GET /readyz` reported database, role safety and schema version as ok. The worker and Fake ERP run with a shared demo credential supplied through Compose secret mounts, while only the worker receives the connector-secret manifest. The Fake ERP log uses a durable volume.
- `go test -c -o /tmp/eacp-phase6-security.test ./test/security` followed by `EACP_COMPOSE_TEST=1 /tmp/eacp-phase6-security.test -test.v` passed. It covers network isolation, mount custody and unauthenticated privileged calls. A check of worker and ERP logs found no demo credential.
- The demo ERP token is generated locally from the existing development manifest. Git ignores the generated file, and `.dockerignore` excludes the secrets directory from image builds.

## Phase boundary

Phase 7 owns reconciliation and human resolution. A positive lookup in Phase 6 supplies evidence but does not move an `UNKNOWN_OUTCOME` action to `SUCCEEDED`; absence never triggers an automatic retry. The worker makes no exactly-once claim.
