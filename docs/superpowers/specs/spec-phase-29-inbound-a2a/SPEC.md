# Phase 29 — inbound A2A

Status: complete on 2026-10-02 after owner approval and phase verification; see [evidence](VERIFICATION.md).

## Why

Remote agents need to delegate a governed enterprise action to EACP using the A2A protocol. Inbound transport must preserve the existing action authority rather than create a task runner beside it.

## Capabilities

| ID | Intent | Success |
| --- | --- | --- |
| CAP-01 | Discover EACP's governed-action skill | A static Agent Card advertises A2A 1.0 JSON-RPC, JSON data input/output and existing EACP Bearer agent authentication, without tenant registry contents. |
| CAP-02 | Delegate a single action | `SendMessage` accepts one user data part containing ordinary action fields, submits through `action.Engine`, and returns the same action UUID as the task ID. Replays reuse the action; changed authority or payload under the same message ID conflicts. |
| CAP-03 | Follow or cancel a task | `GetTask` and `CancelTask` use the caller's existing agent ownership checks. Unknown outcomes remain working. Cancellation is reported only when PostgreSQL has recorded `CANCELLED`. |
| CAP-04 | Read a successful result | Completed tasks expose retained output only through the existing caller-only result channel. Unavailable output does not turn an already successful action into a failure. No input/history is returned. |

## Constraints

- Routes are disabled unless `EACP_A2A_PUBLIC_URL` is configured. The advertised URL is explicit, ends in `/a2a`, and has no credentials, query or fragment. HTTPS is required outside development/test.
- Discovery: `GET /.well-known/agent-card.json`. Authenticated RPC: `POST /a2a`, JSON-RPC 2.0 with A2A protocol version 1.0.
- Only an approved, unexpired, unrevoked existing EACP agent key authenticates RPC. The key determines tenant, agent and version; supplied tenant selectors are refused. Existing PostgreSQL lifecycle/capability/PDP/approval/budget/kill rules remain authoritative.
- The sole input data object contains `subject`, `operation`, `target`, `tool`, `tool_schema_version`, `resource`, `payload`. Lifetime is fixed at one hour, so a replay cannot extend a deadline or silently change a lifetime. Subject resolution uses the ordinary action rules.
- Message IDs have the existing bounded safe identifier form (1–256 ASCII identifier characters); the adapter uses `a2a:` plus SHA-256 of that ID as the core idempotency key. Deduplication is scoped to tenant and agent by the existing database unique constraint. No exactly-once claim.
- Requests are bounded to 1 MiB and validated as I-JSON before decoding. Unknown fields and ambiguous part unions are refused. `returnImmediately` must be true; nonzero history, push configuration, task/context references, extensions and non-JSON output modes are refused. Opaque metadata is ignored, never persisted or returned.
- Tasks are projections of action rows. No new table, function, policy, migration, task store, network listener, service, secret or database role. Production uses existing dependencies only. Tests use the existing pinned `a2a-go/v2 v2.6.0` client and add an explicit indirect requirement for the already-selected, checksum-pinned `golang.org/x/mod v0.41.0`. The owner approved the test dependencies/download; `go mod tidy` left `go.sum` unchanged.
- Safe task metadata contains action ID/state only. Error replies use fixed messages and never serialize SQL, PDP errors, submitted content or headers. Result artifacts are private, bounded and never logged.
- Initial admission/PDP failures distinguish requests with no committed action from requests whose action already exists. A persisted nonterminal action is returned as a task even when governance is temporarily unavailable.

## Non-goals

Free-text planning, selecting tools from a prompt, Studio workflow execution, foreign identities or inbound OAuth federation, streaming, push notifications, REST/gRPC/v0.3, extended cards, list/search tasks, multi-turn conversations and histories. Existing outbound A2A remains unchanged.

## Success signal

A pinned upstream client discovers EACP, submits an action, polls the same task, and reads its retained result. Live demos show normal approval and exactly one recorded external effect for a replay, denied capability/kill cases, private task ownership and conservative cancellation. Fresh lint, vet, full PostgreSQL race suite, demos, compose security/examples, and Helm/live Kubernetes checks for deployment changes pass. Paired docs and invariant maps agree with the delivered scope. Owner-only local commits are recorded, followed by a report and stop.

## Assumptions and authorization

The action-oriented skill is the conservative Phase 29 interpretation of “accept delegations from remote agents”; it does not accept arbitrary chat. The owner approved the optional environment setting, existing-auth integration, development deployment settings and reference-client test dependencies/download. No schema change. Kill containment withholds claim/dispatch rather than inventing a denied final state; the task remains working while the queued action is stopped.
