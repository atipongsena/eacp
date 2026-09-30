# Changelog

All notable changes to EACP are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). Before 1.0.0, a minor version may change APIs and
schemas; each change says so.

## [Unreleased]

- Agent Studio's rules (Phase 27a-1, ADR-033): new roles `studio_author` (a human role) and `studio_runtime` (held alone by a service principal). An author saves an agent definition (`POST /v1/studio/agents`, `POST /v1/studio/agents/{id}/versions`); PostgreSQL validates it, derives its capability and creates the agent, a `REGISTERED` version and an allowlist of exactly those tools. A registry approver who is not the author approves or rejects it once (`/v1/studio/versions/{id}/approve`, `/reject`), and approval retires the previous version. `GET /v1/studio/agents`, `/versions/{id}` and `/requests` show each version's stage and who acts next. The runtime may propose an agent key only for an approved Studio version. Migration 00027 adds `eacp.studio_agents`, `eacp.studio_versions`, `eacp.studio_save_marks`, `eacp.studio_save` and `eacp.studio_decide`, and a Studio branch in the role, agent, version, allowlist and credential guards. Bundles accept the two roles.
- The result channel (Phase 26b, ADR-034): a contract may keep a successful call's output for the agent that made it (`result_retention_seconds`, 60 s to a day, opt-in and two-person). HTTP returns the execute response's `result`, MCP the whole `CallToolResult` and A2A the task's artifacts. Only an `ACTIVE` version of the calling agent reads it (`GET /v1/actions/{id}/result`); operators see its size and digest in the evidence. Outputs over 64 KiB or containing a worker-held credential are withheld; the sweeper clears expired content. Nothing is logged or journaled. Migration 00026 adds `eacp.action_results` and the contract column; the HTTP connector reads responses up to 128 KiB.
- MCP `tools/call` (Phase 26a, ADR-032): the execution worker calls a certified MCP tool at most once per action, after checking that the server's current definition is byte-equal to the certified one (`definition_changed`, `tool_missing`, `definition_unverified`; nothing is sent otherwise). Tools using `x-mcp-header` are refused. A success's reference is a digest of the result; the tool's output is never stored, journaled or logged. MCP contracts are single-attempt (migration 00025). The Slice C demo calls a tool and refuses a rug pull.
- Operator console: a redesigned interface (semantic light and dark tokens checked for WCAG AA contrast, grouped navigation, KPI cards, banners, empty states) and Thai alongside English, chosen with `?lang=` and never stored (ADR-028 Rev 1.1). No API route, table or migration changes.

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
