# Phase 6 Connector and Fake ERP Implementation Plan

> **For agentic workers:** Implement this plan task by task with Superpowers TDD and verification. The source requirements are `docs/MASTER_PLAN.md` §80 and ADR-001/004.

**Goal:** Dispatch real HTTP connector calls through the fenced worker and provide a credential-protected Fake ERP with operation-key lookup and the Phase 6 failure modes.

**Architecture:** The worker retains ownership of the dispatch intent, lease, credential lookup, and result classification. A new HTTP connector speaks a small JSON protocol to the target and implements execute and lookup. Fake ERP implements that protocol with a durable operation log so lookup can report a committed effect after a process restart. The worker registers HTTP at startup.

**Tech stack:** Go 1.27 standard library, existing pgx/PostgreSQL integration harness, Docker Compose.

**Spec:** `docs/MASTER_PLAN.md` §19–21, §31, §80; `docs/adr/ADR-001-product-boundary-and-enforcement-point.md`; `docs/adr/ADR-004-action-state-machine-and-execution-semantics.md`.

## Global constraints

- No dependency or schema migration is needed.
- Keep the enforced payload intact inside the HTTP request envelope.
- Never follow redirects while sending a connector credential.
- A success requires an external reference; only contract-certified error classes are definitive no-effect; all other results are ambiguous.
- The operation key stays stable across attempts. `Lookup` reports evidence; Phase 7 owns reconciliation transitions.
- Fake ERP rejects unauthenticated privileged calls and associates accepted calls with the worker principal.
- Run PostgreSQL integration tests with `EACP_TEST_ADMIN_DSN` and `-race`.

## Review focus

- A redirect must not forward the credential to another host. The HTTP connector test will assert zero requests at the redirected target.
- A response lost after an ERP commit must remain ambiguous while lookup finds the record. The HTTP/Fake ERP test will cover this.
- A missing record under delayed visibility must not become proof of no effect. The lookup test will report absent evidence while the worker remains `UNKNOWN_OUTCOME`.
- A credential or target-controlled response body must not enter an action row, attempt, journal, or log. The integration test will use a canary.
- A worker or ERP restart must not cause a second ERP record for the same native operation key. The ERP persistence test will reopen its data file.

## Task 1: HTTP protocol and connector

**Files:** `internal/worker/connector.go`, `internal/connector/http.go`, `internal/connector/http_test.go`.

**Interfaces:** Extend `worker.Connector` with `Lookup(context.Context, worker.LookupCall) worker.LookupResult`; provide `connector.NewHTTP()`.

- [x] Write request/response tests with `httptest.Server` for native and correlation operation keys, preserved enforced payload, bearer credential, success reference, certified no-effect, 429/5xx/timeout ambiguity, lookup found/absent/unknown, and redirect refusal.
- [x] Run the focused test and confirm the missing implementation fails.
- [x] Implement the minimal HTTP client and response bounds, then run the focused test with `-race`.

## Task 2: Fake ERP

**Files:** `internal/fakeerp/erp.go`, `internal/fakeerp/erp_test.go`, `cmd/fakeerp/main.go`.

**Interfaces:** `fakeerp.New(token, dataPath string) (http.Handler, error)`; POST `/v1/execute`, GET `/v1/operations/{key}`, GET `/v1/audit`.

- [x] Write tests for rejected calls, operation-key deduplication and lookup, restart persistence, audit principal, and every §80 failure mode.
- [x] Run the focused test and confirm the missing implementation fails.
- [x] Implement the handler and durable append log, then run the focused test with `-race`.

## Task 3: Worker wiring, integration, and documentation

**Files:** `cmd/execution-worker/main.go`, `internal/worker/worker_test.go`, `docker-compose.yml` and development secret wiring after approval, ADR-004, MASTER_PLAN status, Phase 6 review.

- [x] Write a PostgreSQL integration test using the real HTTP connector and Fake ERP. Assert successful effects, ambiguous after-effect responses, no duplicate dispatch, stable operation key, and no secret persistence.
- [x] Run the test and confirm it fails because HTTP is not wired.
- [x] Register HTTP in the worker. Add approved Compose credential and durable-file mounts. Run the integration test with `-race`.
- [x] Update the ADR and phase status; run `go vet ./...`, `go test -race ./...` with PostgreSQL, Compose security tests, and review the final diff.
