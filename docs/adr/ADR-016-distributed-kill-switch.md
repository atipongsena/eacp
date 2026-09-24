# ADR-016: PostgreSQL authority for distributed execution kills

Status: Accepted (Rev 1.0, 2026-09-24). Scope: Slice C Phase 16 (MASTER_PLAN §39–§40, §90).
Related: ADR-004 (T14/T16/T22), ADR-014 (signals), ADR-002 (PDP independence), ADR-015 (observed dependencies).

## Context

Agent suspension and connector disablement stop some new dispatches, but an operator needs scoped, durable containment across workers. A NATS message can be delayed or lost. A worker can be between its lease and T16, or already inside an external call. Cancellation cannot prove that an external effect was rolled back.

Microsoft AGT's Go kill switch distinguishes activation, clear, scope, reason code and history. It is process-local. The [upstream source at the inspected commit](https://github.com/microsoft/agent-governance-toolkit/blob/98a777328af9ad4dc76bc76803071cf5a615811b/agent-governance-golang/packages/agentmesh/kill_switch.go) defines `global`, `agent` and `capability` scopes and four reason codes. EACP's pinned `agt-policies` sidecar is a PDP, so an in-memory AGT registry cannot serve as distributed execution authority.

## Decision

1. `eacp.kill_states` stores one row per tenant, scope and target. `killed` is the active flag; `epoch` increments on every activation or clear. `eacp.kill_tenant_epochs` increments on every scoped change in the tenant. Both follow tenant RLS. The only application write is the `SECURITY DEFINER` function `eacp.set_kill`, which requires a bound human `operator`, validates the tenant-local target and reason code, and appends a hash-chained audit event as its last statement. A different operator must clear an active kill. Direct table writes by `eacp_app` are revoked.
2. EACP maps AGT `agent` to the registry agent UUID, and AGT `capability` to the registry tool UUID. Tenant, owner team, agent version, action and connector are narrower EACP scopes. The four verified AGT reason codes are `policy_violation`, `security_incident`, `operator_request` and `error_budget_exhausted`; an operator supplies a separate required message. The default code is `operator_request`. The audit journal records both code and message. The NATS signal carries neither.
3. T14 and T16 take the tenant kill advisory transaction lock after the action row and other established locks, then check matching active scopes in PostgreSQL. The operator mutation takes the same lock before insertion or update, including when no scope row existed before. A kill that commits before T16 therefore prevents its intent from committing; no external call follows. The claim hint filters killed actions for efficiency, but its answer grants nothing. The T14/T16 triggers remain authoritative for raw SQL.
4. The dispatch intent pins the tenant kill epoch. After T16 and before the connector call, the worker checks PostgreSQL again. During a call, it polls PostgreSQL every second by default; the poll only reads, and the lease heartbeat keeps its own slower schedule. A `kill.changed` transactional outbox event is relayed through JetStream; a worker's NATS subscription broadcasts a wake-up to every local in-flight call. The message only prompts the database check, and polling remains on when NATS is absent. Completion serializes with the operator mutation on the same lock. If the tenant epoch changed since dispatch, the result is recorded as ambiguous and the action moves to `UNKNOWN_OUTCOME`, even if the connector returned a claimed definitive result or the kill was quickly cleared. This can overreport unknown outcomes after an unrelated tenant kill; it cannot falsely prove safety.
5. Reconciliation, cancellation and human resolution retain their existing paths and do not call the governance PDP. Kill never rewrites a completed effect or authorizes a retry. `UNKNOWN_OUTCOME` follows ADR-004's proof rules.

## Scope and limits

The current action has authoritative tenant, team (pinned owner group), agent, agent version, action, connector and tool identities. Those seven scopes are enforceable now. A team kill matches the group pinned when the action was released.

`global` is platform-wide in AGT and in MASTER_PLAN §39. EACP's principals and operator role are tenant-scoped; treating a tenant operator as a platform operator would break tenant isolation. A `tenant` kill is explicitly tenant-wide, not a claim of global authority. `run` and `model` are also rejected: actions have no authenticated run or model binding, and ADR-015's observed model edges cannot prove absence. Accepting either request while it could match no action would create false containment. These scopes need an approved platform identity and binding design before exposure.

The worker can request cancellation of a cooperative connector call. A target may already have committed an effect, so an executing kill always enters the unknown-outcome evidence path. A kill activated after a terminal action does not change that action's state.

## Verification

`internal/worker/kill_test.go` covers seven scopes, raw T16, pre-call and in-call kills, epoch changes (including a kill resumed during the call, and an earlier kill that must not taint a later dispatch), a read-only kill poll, role and two-person clear, foreign targets and direct-write refusal. `internal/messaging/relay_test.go` verifies the minimal transactional signal, and `test/security/nats_test.go` that the worker's NATS credential may subscribe to kill signals and no other event. `internal/api/kill_test.go` and `cmd/eacpctl/kill_test.go` cover operator controls. `internal/worker/isolation_integration_test.go` covers both new tables under tenant RLS. `internal/storage/rls_catalog_test.go` reviews the new policies and SECURITY DEFINER function.
