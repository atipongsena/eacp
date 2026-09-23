# Enterprise Agent Control Plane (EACP)

EACP is a control plane for running many AI agents in an enterprise. Agents may be built with any framework, but **privileged actions against enterprise systems go through EACP**. There they are governed, approved, executed safely and audited.

- **Governance:** Microsoft Agent Governance Toolkit / ACS, connected through a sidecar PDP ([ADR-002](docs/adr/ADR-002-agt-integration-sidecar-pdp.md))
- **Execution:** a Go execution fabric with fenced dispatch, explicit `UNKNOWN_OUTCOME` handling and reconciliation ([ADR-004](docs/adr/ADR-004-action-state-machine-and-execution-semantics.md))
- **State:** PostgreSQL is the single source of truth, with tenant isolation enforced by Row-Level Security

> **Status: early development.** Slice A · Phase 1 (Platform Foundation) is complete. There's no Action API yet.
> See the [Master Plan](docs/MASTER_PLAN.md) and the [ADRs](docs/adr/).

## Slice A goal

> Privileged agent actions cannot bypass the control plane, approvals are durable, retries cannot casually duplicate irreversible side effects, and ambiguous execution outcomes are handled explicitly rather than guessed.

This claim is scoped to conforming deployments; see [ADR-001 §3a](docs/adr/ADR-001-product-boundary-and-enforcement-point.md).

## What exists today (Phase 1)

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

## Quick start

```bash
docker compose up -d --build
curl localhost:8080/readyz
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
