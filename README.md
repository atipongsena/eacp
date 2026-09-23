# Enterprise Agent Control Plane (EACP)

EACP is a control plane for running many AI agents in an enterprise. Agents may be built with any framework, but **privileged actions against enterprise systems go through EACP**. There they are governed, approved, executed safely and audited.

- **Governance:** Microsoft Agent Governance Toolkit / ACS, connected through a sidecar PDP ([ADR-002](docs/adr/ADR-002-agt-integration-sidecar-pdp.md))
- **Execution:** a Go execution fabric with fenced dispatch, explicit `UNKNOWN_OUTCOME` handling and reconciliation ([ADR-004](docs/adr/ADR-004-action-state-machine-and-execution-semantics.md))
- **State:** PostgreSQL is the single source of truth, with tenant isolation enforced by Row-Level Security

> **Status: early development.** Slice A · Phases 1–3 are complete: platform foundation; registry, identity and capability; local governance and durable approvals. The Action API and atomic release boundary are Phase 4.
> See the [Master Plan](docs/MASTER_PLAN.md) and the [ADRs](docs/adr/).

## Slice A goal

> Privileged agent actions cannot bypass the control plane, approvals are durable, retries cannot casually duplicate irreversible side effects, and ambiguous execution outcomes are handled explicitly rather than guessed.

This claim is scoped to conforming deployments; see [ADR-001 §3a](docs/adr/ADR-001-product-boundary-and-enforcement-point.md).

## What exists today (Phases 1–3)

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

Approval request creation and grant consumption are internal transaction operations for Phase 4's Action API. The approval API exposes only the eligible queue, enforced payload and voting; it does not make an action executable by itself.

## Quick start

```bash
docker compose up -d --build
curl localhost:8080/readyz
```

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
