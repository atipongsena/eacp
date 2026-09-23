# AGENTS.md — working in the EACP repository

Instructions for AI coding agents (Claude Code, Codex) and humans.

## Source of truth

1. `docs/adr/` — normative decisions. **An ADR wins over the master plan.**
   Phase 0 gates: ADR-001, ADR-002, ADR-004 (hard gate) and ADR-005.
2. `docs/MASTER_PLAN.md` (Revision 2) — scope, slices and phases.
3. `docs/reviews/` — why things are the way they are.

Current status: **Slice A complete, Phases 1–8** (Platform Foundation; Registry, Identity & Capability; Governance & Approval per ADR-002/005; Action API & Atomic Boundary per ADR-004/005; Worker, Lease, Fencing & Dispatch Intent per ADR-004 Rev 2.3; Connector Framework & Fake ERP per ADR-004 Rev 2.4; UNKNOWN_OUTCOME, Reconciliation & Human Resolution per ADR-004 Rev 2.5; Hardening & Demo per ADR-004 Rev 2.6). Slice B (Phase 9) has not started.

## Rules (MASTER_PLAN §106, §107)

- Per phase: read the plan and the relevant ADR → inspect the code → state the invariants → **write failing tests first** → implement → run tests **with `-race`** → update the docs.
- **Do not start the next phase automatically.** Stop and report back after each phase.
- Never invent an upstream API. Verify it against the module source or the docs first.
- Never disable or weaken a test, or fail-closed behaviour, to make CI green.
- Never claim exactly-once. Use the terms in MASTER_PLAN §21.
- Where an assumption is unresolved, choose the option that's **most conservative for correctness and safety**, and record it in the ADR.
- Every tenant-scoped table follows the RLS convention in `migrations/00001_foundation.sql`.
- Services must use the `eacp_app` role. They refuse to start with a role that can bypass RLS.
- Agents never receive enterprise credentials (ADR-001). Connector secrets live only in the execution worker.
- Registry rules live in PostgreSQL triggers (ADR-003 §8). Every privileged write runs in a transaction with `storage.SetActor`; the triggers take the actor from there and fill every `*_by`/`*_at` column. Test each rule with raw SQL as `eacp_app` (see `internal/registry/schema_test.go`), not only through Go.
- Append the audit event in the **same** transaction as the change, as its last statement (`registry.Service.change` does this).
- Action transactions bind exactly one actor: `storage.SetActor` (principal), `storage.SetAgent` (an authenticated agent version), `storage.SetSystem` (a named component such as `sweeper`) or `storage.SetWorker` (a worker id and its lease generation). `eacp.actor()` stays principal-only; action guards use `eacp.actor_context()` (migrations 00005, 00006).
- Every worker write is fenced by the database: lease-holder moves check the worker id and generation set by `storage.SetWorker` (`eacp.assert_lease_holder`). A dispatch intent (T16) commits before any external call, and nothing is dispatched without one.
- Only `execution-worker` may be configured with connector secrets (`config.Options.AllowConnectorSecrets`). Never log, store or journal a secret value; the worker drops connector-returned fields that contain one.
- A reconciler transaction binds `storage.SetReconciler` (a reconciler id and its lease generation). Negative evidence proves nothing unless the pinned contract is AUTHORITATIVE and every call has settled; a human sees an unknown outcome only once it has settled (ADR-004 Rev 2.5).
- Every [A] invariant of MASTER_PLAN §103 keeps at least one passing test listed in `docs/INVARIANTS.md` (`test/invariants` enforces the map). A new table, policy or SECURITY DEFINER function must be reviewed and added to `internal/storage/rls_catalog_test.go`.
- Every transaction that changes an action locks the action row **first** (action → approval rows → registry `FOR SHARE` → audit chain head). Never call the PDP with a transaction open (ADR-005 §5a).

## Commands

```bash
docker compose up -d postgres                        # test database on 127.0.0.1:55432
export EACP_TEST_ADMIN_DSN="postgres://postgres:postgres@127.0.0.1:55432/postgres?sslmode=disable"
go vet ./... && go test -race ./...                  # unit + PostgreSQL integration tests

docker compose up -d --build                         # full stack
curl localhost:8080/readyz
EACP_COMPOSE_TEST=1 go test -count=1 ./test/security/   # network-isolation tests (stack must be running)
scripts/demo.sh                                      # Slice A demo on an isolated stack (docs/DEMO.md)
```

Without `EACP_TEST_ADMIN_DSN`, the PostgreSQL integration tests are **skipped, not passed**. Always run them before reporting a phase complete.

On Git Bash for Windows, prefix `docker compose run ... /binary` with `MSYS_NO_PATHCONV=1`.

## Layout

```text
cmd/                 controlplane-api, execution-worker, fakeerp, eacpctl
internal/config      env configuration (EACP_*)
internal/logging     slog with secret redaction
internal/telemetry   OpenTelemetry + W3C propagation
internal/health      /healthz, /readyz
internal/httpserver  graceful shutdown
internal/service     shared startup (fail closed)
internal/storage     pgx pool, InTenantTx, SetActor, role and schema checks, migrations
internal/audit       hash-chained, append-only audit journal (Append, Verify)
internal/identity    API keys (bring your own key) and Authenticate
internal/registry    principals, roles, groups, credentials, agents, versions,
                     allowlists, connectors, tools, contracts; CheckCapability
internal/registry/registrytest  bootstrapped fixture for tests
internal/governance  local PDP, JCS digests, policy versions and decision evidence
internal/approval    approval request, vote and one-time grant transactions
internal/action      Action API engine: submission, evaluation, release boundary, cancel, sweeper,
                     operator resolution, evidence
internal/worker      claim, heartbeat, fenced dispatch intent and results, host-bound secrets, worker loop,
                     reconciler (lookup under the pinned proof standard)
internal/connector   HTTP connector (execute, lookup)
internal/fakeerp     credential-protected Fake ERP with a durable operation log
internal/api         HTTP API (/v1/...) for registry, policies, approvals and actions
migrations/          goose SQL, embedded
test/security        docker-compose end-to-end security tests
test/invariants      checks docs/INVARIANTS.md against MASTER_PLAN §103 and the tests
test/demo            the Slice A demo (EACP_DEMO=1, scripts/demo.sh)
deployments/docker   Dockerfile, postgres bootstrap
deployments/demo     compose override for the isolated demo project
```
