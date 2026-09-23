# Enterprise Agent Control Plane (EACP)

EACP is a control plane for running many AI agents in an enterprise. Agents may be built with any framework, but **privileged actions against enterprise systems go through EACP**. There they are governed, approved, executed safely and audited.

- **Governance:** Microsoft Agent Governance Toolkit / ACS, connected through a sidecar PDP ([ADR-002](docs/adr/ADR-002-agt-integration-sidecar-pdp.md))
- **Execution:** a Go execution fabric with fenced dispatch, explicit `UNKNOWN_OUTCOME` handling and reconciliation ([ADR-004](docs/adr/ADR-004-action-state-machine-and-execution-semantics.md))
- **State:** PostgreSQL is the single source of truth, with tenant isolation enforced by Row-Level Security

> **Status: early development.** Slice A · Phases 1–7 are complete: platform foundation; registry, identity and capability; local governance and durable approvals; the Action API and atomic release boundary; workers, leases, fencing and the dispatch intent; the HTTP connector and Fake ERP; `UNKNOWN_OUTCOME`, reconciliation and human resolution. Slice A hardening and the demo are Phase 8.
> See the [Master Plan](docs/MASTER_PLAN.md) and the [ADRs](docs/adr/).

## Slice A goal

> Privileged agent actions cannot bypass the control plane, approvals are durable, retries cannot casually duplicate irreversible side effects, and ambiguous execution outcomes are handled explicitly rather than guessed.

This claim is scoped to conforming deployments; see [ADR-001 §3a](docs/adr/ADR-001-product-boundary-and-enforcement-point.md).

## What exists today (Phases 1–7)

| Capability | Evidence |
|---|---|
| Services: `controlplane-api`, `execution-worker`, `fakeerp`, `eacpctl` | `docker compose up -d --build` |
| Fail-closed startup: refuses a DB role that can bypass RLS | `internal/service` tests; container exits 1 with a superuser DSN |
| Row-Level Security convention: a missing tenant context sees no rows | `internal/storage` tests (mutation-checked) |
| Schema migrations (goose, embedded, advisory-locked) | `eacpctl migrate up\|status` |
| Health and readiness (database, role safety, schema version) | `GET /healthz`, `GET /readyz` |
| Secret redaction in logs | `internal/logging`, `internal/service` tests |
| OpenTelemetry tracing with W3C propagation | `internal/telemetry` tests |
| Graceful shutdown | `internal/httpserver` tests |
| Network isolation: the agent can't reach the ERP or the DB | `test/security` (mutation-checked) |
| Registry: principals, two-person role grants, groups, agents, versions, allowlists, connectors, tools, contracts ([ADR-003](docs/adr/ADR-003-agent-registry-identity-and-capability.md)) | `internal/registry` (schema tests run raw SQL as the app role; triggers mutation-checked) |
| Two-person rules enforced by PostgreSQL: grants, credentials, allowlist/contract/version activation, quarantine release | `internal/registry/schema_test.go` |
| API keys: bring your own key, hash-only, bound to a principal or one agent version, 90-day maximum | `internal/identity` |
| Capability check: tool must be in the ACTIVE version's allowlist with an active, unrevoked, fingerprint-matching contract | `registry.CheckCapability`, `POST /v1/agent/capability-check` |
| Hash-chained, append-only audit journal written in the same transaction as each change | `internal/audit` (tamper tests) |
| Local governance provider with five verdicts, immutable policy versions and two-person activation | `internal/governance`; `POST /v1/policies`, `POST /v1/policies/{id}/activate`, `GET /v1/policies/current` |
| JCS + SHA-256 input and enforced digests; immutable decision evidence | `internal/governance/digest.go`, `migrations/00004_governance_approvals.sql` |
| Durable approval requests, human votes, quorum, separation of duties, expiry and one-time action-bound grants | `internal/approval`; `GET /v1/approvals`, `GET /v1/approvals/{id}`, `POST /v1/approvals/{id}/votes` |
| Action API: idempotent submission (409 on a different digest), static admission (429), governance with fail-closed 503, `?wait=` | `internal/action`, `internal/api/actions.go`; `POST /v1/actions`, `GET /v1/actions/{id}` |
| ADR-004 pre-dispatch state machine (T1–T13, T15) enforced by PostgreSQL triggers for raw SQL too | `migrations/00005_actions.sql`, `internal/action/schema_test.go` (mutation-checked) |
| Atomic release boundary: fresh revalidation, one-time grant consumption checked at commit, pinned policy and contract, journal and outbox in one transaction | `internal/action` release tests (parallel releases, activation races, injected failures) |
| Sweeper: outage recovery, release after approval, expiry; cancel that never consults the PDP | `internal/action/sweeper.go`; `POST /v1/actions/{id}/cancel` |
| Worker claim (`FOR UPDATE SKIP LOCKED`, FIFO), heartbeats and lease generations; PostgreSQL rejects any write by a stale worker | `migrations/00006_execution.sql`, `internal/worker/schema_test.go` (mutation-checked) |
| Fenced dispatch intent before any call, with drift re-check (T16a/T16b) and an attempt row per dispatch; fenced results and late-result evidence | `internal/worker` (lease race, stale worker never dispatches twice) |
| Lease reclaim, retries by contract, cancel requests while executing | `internal/action/sweeper.go`, `internal/action/execution_test.go` |
| Worker connector credentials are tenant-namespaced and host-bound; agents receive none | `internal/worker/secrets.go`, `test/security` |
| HTTP connector and credential-protected Fake ERP with a durable operation-key lookup and failure scenarios | `internal/connector`, `internal/fakeerp`, `internal/worker/http_integration_test.go` |
| Fenced reconciler: lookup under the pinned proof standard; only authoritative, settled absence permits a retry with the same key or `FAILED`; conflicts and exhaustion go to a human | `migrations/00007_reconciliation.sql`, `internal/worker/reconciler.go`, `internal/worker/reconcile_schema_test.go` |
| Fake ERP flagship tests: lost response → one record; delayed visibility → no retry; killed worker → no blind re-dispatch | `internal/worker/reconcile_integration_test.go` |
| Operator resolution with separation of duties and a two-person retry; queue and evidence for operators and auditors | `internal/action/resolution.go`; `GET /v1/actions?state=`, `GET /v1/actions/{id}/evidence`, `POST /v1/actions/{id}/resolutions`; `eacpctl action` |

The worker registers the Phase 6 HTTP connector and runs the Phase 7 reconciler. Fake ERP requires a credential for privileged calls and keeps its operation log in a durable Compose volume.

## Quick start

```bash
python3 deployments/docker/secrets/prepare_fakeerp_token.py
docker compose up -d --build
curl localhost:8080/readyz
```

The preparation command copies the existing local-development ERP token into a Git-ignored file for the Fake ERP secret mount. Run it again if the worker's local-development secret changes. This is a demo credential; production deployments supply their own secrets.

Bootstrap a tenant (each admin generates their own key; only the hash is registered):

```bash
export EACP_DATABASE_URL="postgres://eacp_owner:eacp_owner_dev@127.0.0.1:55432/eacp?sslmode=disable"
T=$(uuidgen)
eacpctl key generate --kind principal --tenant $T      # run once per admin, keep the key
eacpctl tenant create --id $T --slug acme --name Acme   --admin name=alice,subject=alice@acme.test,credential=<id>,hash=<hex>   --admin name=bob,subject=bob@acme.test,credential=<id>,hash=<hex>
EACP_API_KEY=<alice's key> eacpctl api GET /v1/me
```

Tests:

```bash
docker compose up -d postgres
export EACP_TEST_ADMIN_DSN="postgres://postgres:postgres@127.0.0.1:55432/postgres?sslmode=disable"
go test -race ./...
EACP_COMPOSE_TEST=1 go test -count=1 ./test/security/
```

Compose credentials are local-development defaults only.

## Contributing

Read [AGENTS.md](AGENTS.md) first.
