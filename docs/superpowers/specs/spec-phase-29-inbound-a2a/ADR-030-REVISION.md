# Proposed ADR-030 Rev 1.1 — inbound A2A

Status: accepted by the owner on 2026-10-02; incorporated into `docs/adr/ADR-030-a2a-delegation.md` Rev 1.1. This companion preserves the design draft; the normative ADR records the final delivery, including working-state kill containment and Go's selected test-dependency version. All outbound rules remain unchanged.

## Decision

Inbound A2A is an optional transport over the existing action engine in controlplane-api. It is not a new task execution authority and does not depend on Studio. The existing API listener serves a static public Agent Card and agent-authenticated JSON-RPC endpoint only when an explicit public URL is configured.

Remote callers supply an existing approved EACP agent key. Tenant, agent and version come exclusively from authentication. Principal keys cannot execute through A2A. PostgreSQL still validates subject, active version, allowlist, registry state, governance decision, approvals, release, hard budget and kill scopes. The adapter does not decide or bypass any of those rules.

## Protocol contract

Version 1.0, binding JSONRPC, methods `SendMessage`, `GetTask`, `CancelTask`. The JSON-RPC envelope uses `jsonrpc: "2.0"`, a non-null string or number ID, and a single request rather than a batch or notification. The verified wire source is the existing pinned `a2a-go/v2 v2.6.0` module.

`SendMessage` requires a new `ROLE_USER` message, a bounded identifier `messageId`, exactly one JSON data part, and `configuration.returnImmediately: true`. The data is an ordinary action submission without lifetime controls. The adapter fixes lifetime at one hour. It refuses free text, files, task/context references, extensions, histories, push configuration, supplied tenant selection and output modes other than `application/json`. Opaque metadata is ignored and never persisted or returned.

Example params (no key):

```json
{
  "message": {
    "messageId": "delegation-1",
    "role": "ROLE_USER",
    "parts": [{
      "mediaType": "application/json",
      "data": {
        "subject": "requester@example.test",
        "operation": "purchase",
        "target": "erp",
        "tool": "erp.purchase",
        "tool_schema_version": "1",
        "resource": "po",
        "payload": {"amount": 100, "currency": "THB"}
      }
    }]
  },
  "configuration": {"returnImmediately": true}
}
```

The core idempotency key is `a2a:` followed by the SHA-256 of `messageId`. Existing PostgreSQL idempotency is tenant/agent scoped and compares the canonical authority/payload digest. A changed request or version under that ID conflicts, never starts a new action. A replay cannot extend its deadline. There is no exactly-once guarantee for external effects.

## Task projection and results

The task and context IDs are the existing action UUID. `SendMessage` returns `result.task`; `GetTask` and `CancelTask` return a task directly. The action engine enforces agent ownership. Unknown and unauthorized task IDs are indistinguishable task-not-found errors. Task metadata contains only the action ID and a known state; no input payload, reason text, external reference, credentials, request metadata or history.

| Action state | A2A task state |
| --- | --- |
| RECEIVED | TASK_STATE_SUBMITTED |
| PENDING_APPROVAL, AUTHORIZED, QUEUED, LEASED, EXECUTING, RETRY_WAIT | TASK_STATE_WORKING |
| UNKNOWN_OUTCOME, NEEDS_HUMAN_RESOLUTION, any unrecognized state | TASK_STATE_WORKING |
| SUCCEEDED | TASK_STATE_COMPLETED |
| DENIED | TASK_STATE_REJECTED |
| FAILED, EXPIRED | TASK_STATE_FAILED |
| CANCELLED | TASK_STATE_CANCELED |

Cancellation uses the ordinary core cancellation request. A successful canceled task requires the actual `CANCELLED` state. After dispatch, a cancellation request is not evidence of no effect: reply task-not-cancelable and leave the recorded request for the worker. Repeated cancellation of an already canceled owned task returns that canceled task. Human resolution remains through existing operator APIs.

Only completed tasks may carry an artifact, fetched by `Engine.Result` as the authenticated agent. ADR-034's active-version, contract retention, credential withholding, output limit and expiry rules remain authoritative. Unavailable/withheld/expired output yields no artifact, without falsifying the successful action. Results are never logged, audited or stored again by the adapter.

## Failure and deployment behavior

Bodies are bounded and checked as I-JSON before closed-struct decoding. Fixed JSON-RPC errors never include raw request content, SQL, PDP errors or headers. Admission failure before an action commits creates no task. Temporary governance failure after commitment returns the persisted submitted/working task; the core sweeper or an identical SendMessage replay can advance it safely.

`EACP_A2A_PUBLIC_URL` is blank by default. A configured URL must be absolute, have exact path `/a2a`, and contain no user info, query or fragment; HTTPS is required outside development/test. Discovery never derives its address from a request Host or forwarding header. Compose and development Helm advertise explicit API endpoints. No new port, service, secret, egress, database object or role.

## Scope and verification

This revision adds inbound structured action delegation only. Streaming, push, REST/gRPC/v0.3, extended cards, listing tasks, foreign identity federation, multi-turn conversations and Studio execution remain deferred. ADR-034 has separately delivered outbound result return; the earlier outbound result-channel exclusion is historical.

Tests must prove real engine/database use, same-action replay, conflict without duplicate execution, identity isolation, ordinary approval/budget/kill enforcement, conservative cancellation/uncertainty, private retained output and no content logging. Full reference-client interoperability requires approval for the SDK's pinned test-only indirect dependencies. Live compose and Kubernetes gates exercise the published endpoint and existing worker boundary.
