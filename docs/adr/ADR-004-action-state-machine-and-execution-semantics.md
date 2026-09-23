# ADR-004: Action State Machine and Execution Semantics

- **Status:** Accepted
- **Date:** 2026-09-23
- **Phase 0 gate:** yes (hard gate)
- **Related:** MASTER_PLAN §18, §19, §20, §22, §23, §29, §31, §103; ADR-001, ADR-005; detail ADRs 007–010 and 013 must conform to this ADR
- **Review input:** `docs/reviews/2026-09-23-master-plan-review.md` (C4, H2, X4; Codex: H2 is a Phase 0 gate, C4 needs connector contracts)

## Context

The action state machine is the correctness boundary for approvals, retries, leases, reconciliation, kill and audit. Revision 1 had four problems:

- No state for asynchronous approval.
- Unclear ordering around `ADMITTED`, `QUEUED` and `LEASED`.
- No crash-recovery rule for an action that was `EXECUTING`.
- Fencing that protected **database commits** but not **external side effects**. A stale worker could make a second external call and create a second Purchase Order.

This ADR is the single normative definition of action states, transitions, crash recovery and execution semantics.

## Principles

1. **Conservative by default.** Whenever an outcome can't be proven, the system assumes the side effect **may have happened**.
2. **Postgres is the only authority.** Every transition is a compare-and-set in Postgres (`WHERE id = $1 AND state = $expected [AND lease_generation = $g]`), in the same transaction as its journal entry. Queues and messages are hints only.
3. **One clock.** Lease and expiry comparisons use the database's `now()`, never a worker's clock.
4. **Terminal states are immutable.**
5. **Every transition is journaled** (hash-chained, append-only) with actor, reason, and state from and to.

## States

| State | Meaning | Terminal |
|---|---|---|
| `RECEIVED` | The request is authenticated, passed the capability check, holds its idempotency key and is persisted. It hasn't been evaluated yet. | |
| `PENDING_APPROVAL` | The governance verdict was `escalate`. An approval request is bound to `enforced_digest` + `policy_version`. | |
| `AUTHORIZED` | The action has an allow/warn/transform verdict, or an approval grant, and is awaiting the release boundary. | |
| `QUEUED` | Passed the release boundary (ADR-005) and is executable. | |
| `LEASED` | Claimed by a worker (`lease_generation` incremented). **No dispatch intent has been recorded, so no external call can have happened.** | |
| `EXECUTING` | **The dispatch intent is durably recorded. An external call may have happened.** | |
| `RETRY_WAIT` | Waiting for backoff before re-queue. Only reachable when a retry is proven safe. | |
| `UNKNOWN_OUTCOME` | The external effect is unknown. It must be reconciled before anything else. | |
| `RECONCILING` | Held by a reconciler lease. Evidence is being gathered. | |
| `NEEDS_HUMAN_RESOLUTION` | Automated evidence isn't enough. An authenticated operator must decide. | |
| `SUCCEEDED` | Definitive success, with evidence. | ✔ |
| `FAILED` | Definitively not executed (or no-effect), with no further retry. | ✔ |
| `DENIED` | Rejected by the capability check, governance, an approver, or revalidation. | ✔ |
| `CANCELLED` | Cancelled **before** any dispatch intent. | ✔ |
| `EXPIRED` | Passed `not_after`, or the approval expired, before any dispatch intent. | ✔ |

Rev 1 names that were removed: `CREATED` is now `RECEIVED`; `ADMITTED` is now admission at submission (a rejected request gets 429 and no action is created); `PREPARING` is folded into `LEASED`; `DEAD_LETTER` is a message/inbox concept (an action whose retries run out is `FAILED(retry_exhausted)`).

## Transition table

Actors: **API** (control-plane request handler), **GOV** (governance evaluator), **REL** (release boundary, ADR-005), **W** (execution worker holding lease generation *g*), **SWP** (sweeper), **REC** (reconciler), **APR** (authenticated approver), **OPR** (authenticated operator), **REQ** (requesting agent or subject).

| # | From | To | Trigger | Guard (all must hold, else no transition) | Actor |
|---|---|---|---|---|---|
| T1 | — | `RECEIVED` | Submission | Agent authenticated. AgentVersion `ACTIVE`. Tool in allowlist **and** has a valid connector contract. Admission limit not exceeded. Idempotency key new (a same-`input_digest` duplicate returns the existing action; a different digest returns 409). | API |
| T2 | `RECEIVED` | `DENIED` | The capability or contract check fails, **or** verdict `deny`, **or** PDP unavailable/incomplete/digest mismatch (ADR-002) | — | API / GOV |
| T3 | `RECEIVED` | `AUTHORIZED` | Verdict `allow`, `warn` or `transform` | Decision evidence persisted (complete). Enforced payload persisted. | GOV |
| T4 | `RECEIVED` | `PENDING_APPROVAL` | Verdict `escalate` | Approval request created, bound to tenant + action + `enforced_digest` + `policy_version` | GOV |
| T5 | `RECEIVED` | `EXPIRED` | `not_after` passed before evaluation | — | SWP |
| T6 | `PENDING_APPROVAL` | `AUTHORIZED` | Quorum of eligible approve votes | Grant created (ADR-005). Separation of duties satisfied. Request not expired. | APR |
| T7 | `PENDING_APPROVAL` | `DENIED` | Any eligible deny vote (short-circuit) | — | APR |
| T8 | `PENDING_APPROVAL` | `EXPIRED` | Approval request `expires_at` or `not_after` passed | — | SWP |
| T9 | `PENDING_APPROVAL` | `CANCELLED` | Cancel request | Actor is REQ or OPR | REQ / OPR |
| T10 | `AUTHORIZED` | `QUEUED` | Release boundary succeeds | See ADR-005: revalidation under the **current** policy version, digest unchanged, grant consumed if required, AgentVersion `ACTIVE`, tool still allowed, `not_after` not passed | REL |
| T11 | `AUTHORIZED` | `PENDING_APPROVAL` | Revalidation under a **new** policy version returns `escalate` | The old grant is voided (never consumed). A new request is bound to the new version. | REL |
| T12 | `AUTHORIZED` | `DENIED` | Revalidation `deny`, AgentVersion not `ACTIVE`, or tool removed from the allowlist | — | REL |
| — | `AUTHORIZED` | `AUTHORIZED` | Revalidation **unavailable** | No transition. Retried later (ADR-002 §6). | REL |
| T13 | `AUTHORIZED` | `EXPIRED` / `CANCELLED` | `not_after` passed / cancel request | — | SWP / REQ / OPR |
| T14 | `QUEUED` | `LEASED` | Worker claim (`FOR UPDATE SKIP LOCKED`) | `lease_generation := lease_generation + 1`, `leased_until` set | W |
| T15 | `QUEUED` | `EXPIRED` / `CANCELLED` | `not_after` passed / cancel request | — | SWP / REQ / OPR |
| T16 | `LEASED` | `EXECUTING` | **Fenced dispatch intent** | `lease_generation = g`. `leased_until > now() + call_budget`. AgentVersion `ACTIVE`. No cancel requested. (Slice C: kill epoch unchanged.) An `action_attempts` row is inserted in the **same transaction**. | W(g) |
| T17 | `LEASED` | `QUEUED` | Lease expired, or worker releases voluntarily | No dispatch intent exists | SWP / W(g) |
| T18 | `LEASED` | `CANCELLED` / `EXPIRED` | Cancel requested / `not_after` passed | No dispatch intent exists; fenced | W(g) / SWP |
| T19 | `EXECUTING` | `SUCCEEDED` | Definitive success (response includes an external reference) | `lease_generation = g` | W(g) |
| T20 | `EXECUTING` | `RETRY_WAIT` | Definitive **no-effect** error (listed in the connector contract) | Retry policy allows it. Retry budget remains. `lease_generation = g`. | W(g) |
| T21 | `EXECUTING` | `FAILED` | Definitive no-effect error | Not retryable, or budget exhausted. `lease_generation = g`. | W(g) |
| T22 | `EXECUTING` | `UNKNOWN_OUTCOME` | Ambiguous result: timeout or reset after send, a 5xx not certified as no-effect, or a cancel/kill during the call | `lease_generation = g` | W(g) |
| T23 | `EXECUTING` | `UNKNOWN_OUTCOME` | **Lease expired while EXECUTING** (crash, pause, partition) | The connector is **not** READ_ONLY or natively idempotent | SWP |
| T24 | `EXECUTING` | `RETRY_WAIT` | Lease expired while EXECUTING | The connector **is** READ_ONLY or natively idempotent. The retry reuses the **same operation key**. | SWP |
| T25 | `RETRY_WAIT` | `QUEUED` | Backoff elapsed | Policy version unchanged since the release boundary | SWP |
| T26 | `RETRY_WAIT` | `AUTHORIZED` | Backoff elapsed | Policy version **changed**, so the action must pass the release boundary again (T10–T12) | SWP |
| T27 | `RETRY_WAIT` | `FAILED` | Retry budget (attempts, elapsed, cost) exhausted | — | SWP |
| T28 | `UNKNOWN_OUTCOME` | `RECONCILING` | Reconciler claims (reconciler lease, generation incremented) | Connector reconciliation `lookup` supported | REC |
| T29 | `UNKNOWN_OUTCOME` | `NEEDS_HUMAN_RESOLUTION` | Connector proof standard `NONE`, or no lookup | — | REC / SWP |
| T30 | `RECONCILING` | `SUCCEEDED` | **Positive evidence**: an external record carrying this action's operation key | Fenced by reconciler generation | REC |
| T31 | `RECONCILING` | `RETRY_WAIT` | **Authoritative negative evidence** (see Proof standard) | Retry policy allows it. Same operation key. | REC |
| T32 | `RECONCILING` | `FAILED` | Authoritative negative evidence | Retry not allowed or budget exhausted | REC |
| T33 | `RECONCILING` | `UNKNOWN_OUTCOME` | "Not found" under `BEST_EFFORT`, a lookup error, or reconciler lease lost | `reconcile_attempts := reconcile_attempts + 1`, with backoff | REC / SWP |
| T34 | `RECONCILING` / `UNKNOWN_OUTCOME` | `NEEDS_HUMAN_RESOLUTION` | Conflict (for example multiple records, or a mismatched amount) **or** reconcile attempts or time exhausted | — | REC / SWP |
| T35 | `NEEDS_HUMAN_RESOLUTION` | `SUCCEEDED` | Operator resolves as succeeded | Reason and external evidence reference are required | OPR |
| T36 | `NEEDS_HUMAN_RESOLUTION` | `FAILED` | Operator resolves as not executed | Reason and evidence are required | OPR |
| T37 | `NEEDS_HUMAN_RESOLUTION` | `RETRY_WAIT` | Operator authorises a retry | Reason required. Same operation key. **High-risk actions need a second, distinct operator** (two-person rule). | OPR (+OPR) |

**Anything not listed is forbidden.** In particular:
- `EXECUTING → QUEUED` doesn't exist. A re-dispatch happens only via `RETRY_WAIT` under T20, T24, T31 or T37.
- No cancel or expiry exists from `EXECUTING`, `UNKNOWN_OUTCOME`, `RECONCILING` or `NEEDS_HUMAN_RESOLUTION`. A cancel request after dispatch intent is **recorded**, the in-flight call is cancelled if possible, and the action goes to `UNKNOWN_OUTCOME` (T22).

## Execution semantics

### Operation identity

- `operation_key = "eacp:{tenant_id}:{action_id}"`. It's stable for the action's whole lifetime, across attempts, retries and human-authorised retries.
- The connector passes it to the target according to its contract: as a native idempotency key, embedded as a correlation reference, or neither (at-most-once).

### Fenced dispatch protocol

1. **Dispatch intent (T16)** is its own committed transaction **before** any external call. If it affects 0 rows, the worker **must not** call the external system.
2. The call deadline is `< leased_until − safety_margin`. The worker heartbeats (extending `leased_until`, fenced by `g`) while the call is in flight.
3. The fencing generation is also passed to targets that support conditional writes.
4. Result commits (T19–T22) are fenced by `g`.
5. If a stale worker's result commit fails, it appends a **late-result evidence** journal entry (no state change). Reconcilers treat it as evidence, but it's never sufficient alone for `FAILED`.

### Definitive vs ambiguous results

- **Definitive success**: a success response that includes an external reference.
- **Definitive no-effect**: an error class the connector contract certifies as no-effect (for example a specific validation 4xx, or connection refused before any bytes were sent).
- **Everything else is ambiguous → `UNKNOWN_OUTCOME`.** That includes timeouts, resets after send, uncertified 5xx and 429 after send, worker crashes, and cancel or kill during the call.

### Reconciliation proof standard (connector contract)

| Proof standard | Positive → `SUCCEEDED` | Negative → retry or `FAILED` |
|---|---|---|
| `AUTHORITATIVE` | A record found by operation key | Allowed **only** when the lookup reads a strongly consistent source **and** the contract certifies "absent means never happened" |
| `BEST_EFFORT` | A record found by operation key | **Never.** "Not found" means `STILL_UNKNOWN` (T33) |
| `NONE` | Not applicable | Not applicable. Goes straight to `NEEDS_HUMAN_RESOLUTION` (T29) |

### Retry rules (summary)

| Side effect / idempotency | After definitive no-effect | After lease loss in `EXECUTING` | After `UNKNOWN_OUTCOME` |
|---|---|---|---|
| READ_ONLY | Retry per policy | Retry (T24) | Retry per policy |
| Natively idempotent (same key) | Retry per policy | Retry with the same key (T24) | Reconcile first. Retry with the same key after authoritative negative evidence. |
| Irreversible / non-idempotent | Retry per policy | **Never.** → `UNKNOWN_OUTCOME` (T23) | Only after **authoritative** negative evidence (T31) **or** human authorisation (T37) |
| Unknown class | Treated as irreversible and non-idempotent | | |

### Crash recovery

| Crash while… | Recovery |
|---|---|
| `RECEIVED` | Evaluation is pure, so the sweeper re-runs governance. Or T5. |
| `PENDING_APPROVAL`, `AUTHORIZED`, `QUEUED`, `RETRY_WAIT` | Durable in Postgres. Resume. |
| `LEASED` | T17 → `QUEUED`. Safe, because no dispatch intent exists. |
| `EXECUTING` | T23 or T24. Never a blind re-dispatch. |
| `RECONCILING` | Reconciler lease expires, then T33 → `UNKNOWN_OUTCOME`. |
| Release boundary mid-transaction | Rolled back atomically. The grant isn't consumed and the action stays `AUTHORIZED`. |
| Stale worker wakes after reclaim | Its commits fail on the generation check. It may only append late-result evidence. |

## Consequences

**Positive**
- Every Slice A invariant (§103 #1, 2, 4–8, 10–19) maps to a specific guard in this table.
- There's no path from an unproven outcome to an automatic duplicate of an irreversible effect.
- Ambiguity becomes an explicit, visible queue (`NEEDS_HUMAN_RESOLUTION`) instead of a silent guess.

**Negative / costs**
- More actions land in `NEEDS_HUMAN_RESOLUTION` for connectors without `AUTHORITATIVE` lookup. That's the intended price of safety, and it's visible in metrics (`needs_human_resolution`).
- An extra transaction per dispatch (the dispatch intent) adds latency.
- Connector authors must declare a contract (ADR-013) before a tool can be executed at all.

## Unresolved assumptions and conservative defaults

| Unresolved | Conservative default |
|---|---|
| Whether a given target's error is truly no-effect | Treated as ambiguous unless listed in the certified contract |
| Whether a lookup source is strongly consistent | Treated as `BEST_EFFORT` unless certified |
| Default reconcile attempts and time limit | Finite and configurable. Exhaustion goes to `NEEDS_HUMAN_RESOLUTION`, never `FAILED`. |
| Whether a retry after a long wait needs new governance | Yes, if the policy version changed (T26) |
| Whether cancel during `EXECUTING` means failure | No. It goes to `UNKNOWN_OUTCOME` (T22). |

## Verification

- **Property-based tests** over a model of this table:
  - No path from `DENIED`, `CANCELLED` or `EXPIRED` to `EXECUTING`.
  - No path from `EXECUTING` to `EXECUTING` for a non-idempotent connector without passing `RECONCILING` with authoritative evidence, or an operator resolution.
  - Every non-terminal state has an exit (no deadlock state).
  - Terminal states are absorbing.
- **Concurrency tests:**
  - Two workers race to claim, so only one gets generation *g*.
  - The stale worker's dispatch-intent and result commits fail.
  - A reclaim during `EXECUTING` never dispatches a non-idempotent connector.
- **Fake ERP flagship tests:**
  - Execute then drop the response → reconcile → `SUCCEEDED`, with exactly one ERP record.
  - Delayed visibility under `BEST_EFFORT` → no retry.
  - Worker killed mid-dispatch → exactly one ERP record.
- **Every** transition row T1–T37 has at least one test that exercises it, and at least one test showing its guard rejecting.
