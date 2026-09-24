# AGENTS.md — working in the EACP repository

Instructions for AI coding agents (Claude Code, Codex) and humans.

## Source of truth

1. `docs/adr/` — normative decisions. **An ADR wins over the master plan.**
   Phase 0 gates: ADR-001, ADR-002, ADR-004 (hard gate) and ADR-005.
2. `docs/MASTER_PLAN.md` (Revision 2) — scope, slices and phases.
3. `docs/reviews/` — why things are the way they are.

Current status: **Slice A complete, Phases 1–8; Slice B complete, Phases 9–13. Slice C: Phases 14 (MCP Registry, ADR-023) and 15 (Dependency Graph, ADR-015) complete. Phase 16 (Distributed Kill Switch, ADR-016) is complete for seven action-bound scopes (global, run and model await platform authority and authenticated action bindings). Phase 17 (Fleet Operations, ADR-024) is complete. Phase 18 (Agent FinOps, ADR-025) is complete.**

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
- The AGT sidecar (`sidecars/agt-pdp`) and its Go client pin AGT policies 5.0.0, ACS 0.3.1b1 and OPA 1.20.2 (`microsoftagt.Pinned`, `sidecars/agt-pdp/requirements.txt`, its Dockerfile). A bump needs the spike notes in `research/REFERENCES.md` and a green conformance run. The sidecar never resolves an approval, and every sidecar failure is transient (the action stays `RECEIVED`). Change `test/conformance/governance_reference.json` only with `-update`, then rebuild the sidecar's test stage.
- NATS carries signals, never authority (ADR-014/016). Work hints wake the claim loop; kill signals wake each in-flight PostgreSQL check. Polling stays on. Nothing is claimed, executed, cancelled or decided from a message. Outbox rows come from action or kill-state triggers; messages carry ids, states and epochs, never a reason, action payload or secret. `outbox` and `inbox` are messaging actors: `storage.SetSystem` binds them, and `eacp.actor_context()` rejects them. A consumer with effects records `Nats-Msg-Id` in `eacp.inbox_messages` in the same transaction.
- Hard budgets (ADR-012) are enforced in PostgreSQL. The release reserves with `eacp.budget_reserve` after the action, approval rows and registry are locked, and before any audited write or grant consumption; T10 of a budgeted action requires the reservation. Only the agent's leaf account is locked (child limits are escrowed from their parent). Settlement follows the action's state and never locks an account; the next reservation or limit change folds it. Account counters change only from the budget triggers. Raising a limit is two-person.
- Every transaction that changes an action locks the action row **first** (action → approval rows → registry `FOR SHARE` → connector circuit `FOR SHARE` (T16) → tenant kill advisory lock (T14/T16/completion) → budget leaf → audit chain head). Never call the PDP with a transaction open (ADR-005 §5a).
- MCP tools are discovered, never declared (ADR-023). Only the execution worker's scanner talks to an MCP server, with the worker-held secret. A scan transaction binds `storage.SetScanner` (worker id and scan-lease generation) and writes only through `eacp.mcp_record_scan`; `eacp.actor_context()` rejects the scanner. PostgreSQL computes every fingerprint, digest and risk; never send them from Go. An MCP contract pins the tool's current `definition_id`. A high-risk change or a missing certified tool quarantines the tool; release is a second registry approver and never recertifies. No worker serves protocol `mcp` yet: do not add `tools/call` without an ADR.
- Backpressure and breakers only withhold work (ADR-022). A connector's circuit row is created with the connector; only a worker opens it (at most 10 minutes, never earlier) and only an `operator` disables or enables it. T14 and T16 refuse an open circuit in PostgreSQL. Ask `eacp.retry_budget_exhausted` whether a retry is allowed; never compare `attempt_count` with `max_attempts` in Go.
- Dependency edges are tenant-scoped, immutable observations with source, expiry and confidence (ADR-015); only a `registry_editor` records or revokes them. Existing allowlists and connector/tool rows supply the capability edges. Blast radius runs in a tenant read-only snapshot with recursive CTEs; stale or unknown evidence widens possible impact. Its `observed_only` coverage never proves an undeclared dependency absent. The graph never grants access or triggers containment.
- FinOps observes and never blocks (ADR-025). OTel usage is recorded only with the agent's own key; PostgreSQL binds the agent and computes every cost from the forward-only rate card (never send a cost from Go). Unpriced usage stays NULL, never zero. Alerts are raised only by the `finops` system actor through `eacp.finops_evaluate()` and acknowledged once by an operator or admin.
- Kill states and epochs are authoritative in PostgreSQL (ADR-016). `eacp.set_kill` checks an operator and tenant-local target, journals the change and requires a second operator to clear it. T14/T16 use the tenant kill advisory lock and reject active scopes. After intent, worker checks again before the call, polls during the call, and treats an epoch change as `UNKNOWN_OUTCOME`. Global, run and model scopes require authoritative identity bindings; reject them until those exist.
- Fleet operations are ADR-003 lifecycle transitions, never a new authority (ADR-024). Insert a `eacp.fleet_operation_targets` row to make a transition. The version guard still authorizes it; the target guard reads the `from` state, checks the kind's pair and binds the operation to its creating transaction and actor. Never update `agent_versions` for a fleet operation from Go. Resume and release undo only their source operation's targets. The fleet view is read-only and repeats `eacp.action_capability_denial`'s tool rules without locks; keep `TestFleetDriftAgreesWithTheCapabilityCheck` green when either changes.

## Commands

```bash
docker compose up -d postgres                        # test database on 127.0.0.1:55432
export EACP_TEST_ADMIN_DSN="postgres://postgres:postgres@127.0.0.1:55432/postgres?sslmode=disable"
go vet ./... && go test -race ./...                  # unit + PostgreSQL integration tests

docker compose up -d --build                         # full stack
curl localhost:8080/readyz
EACP_COMPOSE_TEST=1 go test -count=1 ./test/security/   # network-isolation tests (stack must be running)
scripts/demo.sh                                      # Slice A demo on an isolated stack (docs/DEMO.md)
docker build -f sidecars/agt-pdp/Dockerfile --target test .   # AGT sidecar suites + conformance through ACS/OPA
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
                     allowlists, connectors, tools, contracts, dependency evidence and blast radius; CheckCapability
internal/registry/registrytest  bootstrapped fixture for tests
internal/governance  local PDP, JCS digests, policy versions and decision evidence
internal/governance/conformance  the ADR-002 reference set loader and checker
internal/approval    approval request, vote and one-time grant transactions
internal/action      Action API engine: submission, evaluation, release boundary, cancel, sweeper,
                     operator resolution, evidence
internal/worker      claim, heartbeat, fenced dispatch intent and results, host-bound secrets, worker loop,
                     reconciler (lookup under the pinned proof standard), MCP scanner (fenced scan lease)
internal/messaging   outbox relay and pruner, inbox, the worker's work-hint consumer (NATS JetStream)
internal/messaging/natstest  embedded JetStream server for tests (never skips)
internal/budget      budget accounts and limit changes (two-person raises, escrow)
internal/kill        operator API for PostgreSQL execution kills (ADR-016)
internal/fleet       fleet operations (atomic lifecycle transitions) and the read-only fleet view (ADR-024)
internal/finops      OTLP GenAI usage ingest, rate card, billing import, chargeback, soft limits, dashboard, alert evaluator (ADR-025)
internal/connector   HTTP connector (execute, lookup)
internal/connector/mcp  MCP discovery client (Streamable HTTP, modern and legacy revisions); mcptest fake server
internal/fakeerp     credential-protected Fake ERP with a durable operation log
internal/api         HTTP API (/v1/...) for registry, policies, approvals and actions
integrations/governance/microsoftagt  Go client of the AGT sidecar PDP (mTLS, version pins), dev PKI
sidecars/agt-pdp     Python sidecar: AGT policy layer + ACS engine + OPA, Rego adapter, conformance tests
research/            verified upstream API notes (REFERENCES.md)
migrations/          goose SQL, embedded
test/security        docker-compose end-to-end security tests
test/invariants      checks docs/INVARIANTS.md against MASTER_PLAN §103 and the tests
test/conformance     governance reference set shared by Go and the sidecar
test/demo            the Slice A demo (EACP_DEMO=1, scripts/demo.sh)
deployments/docker   Dockerfile, postgres bootstrap, NATS config (per-role users)
deployments/demo     compose override for the isolated demo project
```
