# Changelog

All notable changes to EACP are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). Before 1.0.0, a minor version may change APIs and
schemas; each change says so.

## [Unreleased]

## [0.1.0]

The first release. Every guarantee below is enforced by tests listed in [docs/INVARIANTS.md](docs/INVARIANTS.md);
the design decisions are in [docs/adr/](docs/adr/).

### Slice A: the governed action path (Phases 1–8)

- Platform foundation: PostgreSQL with Row-Level Security from the first migration, services that refuse a role able
  to bypass it, secret redaction, OpenTelemetry, health and graceful shutdown.
- Registry, identity and capability: principals, two-person role grants, agents, versions, allowlists, connectors,
  tools and contracts, all enforced by PostgreSQL triggers; bring-your-own API keys stored as hashes.
- Governance and durable approvals: a local policy provider, immutable policy versions, JCS digests, votes with
  separation of duties, and one-time grants bound to the exact enforced payload.
- The Action API and the atomic release boundary: idempotent submission, admission control, and a release that
  revalidates, consumes the grant, journals and enqueues in one transaction.
- Workers with leases, fencing generations and a dispatch intent committed before any external call.
- The HTTP connector and a credential-protected Fake ERP.
- `UNKNOWN_OUTCOME`, reconciliation under a pinned proof standard, and human resolution.
- A hash-chained, append-only audit journal and evidence reconstruction from an action id.

### Slice B: operating at scale (Phases 9–13)

- The Microsoft AGT/ACS policy engine as a sidecar PDP over mutual TLS, pinned and checked by a shared conformance set.
- NATS JetStream work hints and events; PostgreSQL stays the only authority.
- Hard budget reservation with escrowed hierarchies and two-person limit raises.
- Fair claim scheduling, connector capacity, backpressure, bulkheads, circuit breakers and retry budgets.

### Slice C: control of the fleet (Phases 14–22)

- The MCP registry: tool discovery, fingerprints, definition history, contract invalidation and quarantine.
- Dependency evidence and conservative blast-radius queries.
- Distributed kill switches fenced in PostgreSQL.
- Fleet operations as audited lifecycle transitions.
- Agent FinOps: LLM cost ingest, rate cards, chargeback, soft limits and alerts.
- Releases with evaluation evidence, replay, shadow, a PostgreSQL-enforced canary cohort and rollback.
- Governance-as-Code bundles planned into two-person change sets, with drift detection.
- Incidents, the Agent SOC read model and the operator console.

### Platform (Phases 23–25)

- High availability inside the binaries and a Helm chart with network policies.
- Just-in-time connector credentials: OAuth 2.0 client credentials, workload identity federation, `private_key_jwt`,
  Vault KV v2, SPIFFE JWT-SVIDs, OAuth token exchange with GCP impersonation, and AWS STS with SigV4 signing.
- Governed outbound A2A delegation.
- The LLM gateway (Anthropic Messages and OpenAI Chat Completions) with admission, budgets and kill switches.

### Project

- A reproducible load benchmark ([docs/BENCHMARKS.md](docs/BENCHMARKS.md)).
- Apache-2.0 license, CI, examples, and documentation in English and Thai.

[Unreleased]: https://github.com/atipongsena/eacp/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/atipongsena/eacp/releases/tag/v0.1.0
