# ADR-004: Action State Machine and Execution Semantics

- **Status:** Accepted — Rev 2.6 (Phase 8 hardening: T5a, 2026-09-23; Rev 2.5 Phase 7 reconciliation and human resolution; Rev 2.4 Phase 6 HTTP connector and Fake ERP protocol; Rev 2.3 Phase 5 worker, lease and fencing; Rev 2.2 Phase 4 pre-dispatch; Rev 2.1 amended after Codex adversarial review)
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
6. **Pinned, immutable facts.** At the release boundary an action pins `policy_version` and `connector_contract_version`. Both are immutable, insert-only versions. Later decisions (dispatch, sweeper, reconciler) use the **pinned** facts. They may only become **more** conservative if the pinned version has since been **revoked**. Revocation is a monotonic flag, and it can never make an action less restricted.
7. **Registry reads that guard a transition are row-locked.** The transition reads the agent version, allowlist entry, contract version and tenant policy pointer `FOR SHARE`. Operator changes to those rows take `FOR UPDATE`. That serialises every guard against every concurrent change, with no `SERIALIZABLE` needed.

## States

| State | Meaning | Terminal |
|---|---|---|
| `RECEIVED` | The request is authenticated, admitted, holds its idempotency key and is persisted. Capability, contract and governance haven't been evaluated yet (T2/T2a/T3/T4). | |
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
| T1 | — | `RECEIVED` | Submission | Agent authenticated (unauthenticated → 401, no action). Admission limit not exceeded (→ 429, no action; metric and log only). Idempotency key new (a same-`input_digest` duplicate returns the existing action; a different digest → 409). **Capability and contract are checked in T2/T3, not here**, so that a denial leaves an auditable action. | API |
| T2 | `RECEIVED` | `DENIED` | **Deterministic** rejection: AgentVersion not `ACTIVE`, tool not in allowlist, no active certified contract (reason `capability` / `contract`), verdict `deny`, or `digest_mismatch` (a security signal, which also raises an alert) | — | API / GOV |
| T2a | `RECEIVED` | `RECEIVED` | **Transient** governance failure: PDP unavailable, timeout, or incomplete decision | No transition. The API returns **503 retryable** with `action_id`. A resubmit with the **same** `Idempotency-Key` returns this action and triggers re-evaluation, and the sweeper also re-evaluates. Evaluation is pure, so this is safe. Nothing is executable in `RECEIVED` (fail closed). T5 applies at `not_after`. | API / GOV / SWP |
| T3 | `RECEIVED` | `AUTHORIZED` | Verdict `allow`, `warn` or `transform` | Decision evidence persisted (complete). Enforced payload persisted. | GOV |
| T4 | `RECEIVED` | `PENDING_APPROVAL` | Verdict `escalate` | Approval request created, bound to tenant + action + `enforced_digest` + `policy_version` | GOV |
| T5 | `RECEIVED` | `EXPIRED` | `not_after` passed before evaluation | — | SWP |
| T5a | `RECEIVED` | `CANCELLED` | Cancel request, for example while governance is unavailable (Rev 2.6) | Reason required. No decision, grant or dispatch exists yet. A decision that arrives later finds the action no longer `RECEIVED` and records nothing. | REQ / OPR |
| T6 | `PENDING_APPROVAL` | `AUTHORIZED` | Quorum of eligible approve votes | Grant created (ADR-005). Separation of duties satisfied. Request not expired. | APR |
| T7 | `PENDING_APPROVAL` | `DENIED` | Any eligible deny vote (short-circuit) | — | APR |
| T8 | `PENDING_APPROVAL` | `EXPIRED` | Approval request `expires_at` or `not_after` passed | — | SWP |
| T9 | `PENDING_APPROVAL` | `CANCELLED` | Cancel request | Actor is REQ or OPR | REQ / OPR |
| T10 | `AUTHORIZED` | `QUEUED` | Release boundary succeeds | See ADR-005: revalidation under the **current** policy version (tenant policy pointer locked `FOR SHARE`), digest unchanged, grant consumed if required, AgentVersion `ACTIVE`, tool still allowed, an active certified contract exists, `not_after` not passed. **Pins** `policy_version` and `connector_contract_version` on the action. | REL |
| T11 | `AUTHORIZED` | `PENDING_APPROVAL` | Revalidation under a **new** policy version returns `escalate` | The old grant is voided (never consumed). A new request is bound to the new version. | REL |
| T12 | `AUTHORIZED` | `DENIED` | Revalidation `deny`, `digest_changed`, AgentVersion not `ACTIVE`, tool removed from the allowlist, or no active certified contract | — | REL |
| — | `AUTHORIZED` | `AUTHORIZED` | Revalidation **unavailable** | No transition. Retried later (ADR-002 §6). | REL |
| T13 | `AUTHORIZED` | `EXPIRED` / `CANCELLED` | `not_after` passed / cancel request | — | SWP / REQ / OPR |
| T14 | `QUEUED` | `LEASED` | Worker claim (`FOR UPDATE SKIP LOCKED`) | `lease_generation := lease_generation + 1`, `leased_until` set | W |
| T15 | `QUEUED` | `EXPIRED` / `CANCELLED` | `not_after` passed / cancel request | — | SWP / REQ / OPR |
| T16 | `LEASED` | `EXECUTING` | **Fenced dispatch intent** | One transaction, with registry rows read `FOR SHARE` (principle 7). `lease_generation = g`. `leased_until > now() + call_budget`. AgentVersion `ACTIVE`. Tool **still in the allowlist**. The pinned contract version is **still active and not revoked**. The tenant policy pointer **equals the pinned `policy_version`**. No cancel requested. (Slice C: kill epoch unchanged.) An `action_attempts` row is inserted in the **same transaction**. | W(g) |
| T16a | `LEASED` | `AUTHORIZED` | Policy pointer ≠ pinned `policy_version` at T16 | Policy drift before dispatch means the action must pass the release boundary again (T10–T12). The lease is released and no dispatch intent is written. | W(g) |
| T16b | `LEASED` | `DENIED` | At T16: AgentVersion not `ACTIVE`, tool removed from the allowlist, or the pinned contract revoked or superseded | Reason `revoked_before_dispatch`. No dispatch intent is written. | W(g) |
| T17 | `LEASED` | `QUEUED` | Lease expired, or worker releases voluntarily | No dispatch intent exists | SWP / W(g) |
| T18 | `LEASED` | `CANCELLED` / `EXPIRED` | Cancel requested / `not_after` passed | No dispatch intent exists; fenced | W(g) / SWP |
| T19 | `EXECUTING` | `SUCCEEDED` | Definitive success (response includes an external reference) | `lease_generation = g` | W(g) |
| T20 | `EXECUTING` | `RETRY_WAIT` | Definitive **no-effect** error (listed in the connector contract) | Retry policy allows it. Retry budget remains. `lease_generation = g`. | W(g) |
| T21 | `EXECUTING` | `FAILED` | Definitive no-effect error | Not retryable, or budget exhausted. `lease_generation = g`. | W(g) |
| T22 | `EXECUTING` | `UNKNOWN_OUTCOME` | Ambiguous result: timeout or reset after send, a 5xx not certified as no-effect, or a cancel/kill during the call | `lease_generation = g` | W(g) |
| T22a | `EXECUTING` | `RETRY_WAIT` | Ambiguous result | The pinned contract is **READ_ONLY** and not revoked. Retrying a read is safe by definition. | W(g) |
| T23 | `EXECUTING` | `UNKNOWN_OUTCOME` | **Lease expired while EXECUTING** (crash, pause, partition) | The pinned contract is **not** READ_ONLY or natively idempotent, **or** it has been revoked since release | SWP |
| T24 | `EXECUTING` | `RETRY_WAIT` | Lease expired while EXECUTING | The pinned contract **is** READ_ONLY or natively idempotent **and is not revoked**. The retry reuses the **same operation key**. **Slice A: READ_ONLY only** (Rev 2.3). | SWP |
| T25 | `RETRY_WAIT` | `QUEUED` | Backoff elapsed | Policy version unchanged since the release boundary | SWP |
| T26 | `RETRY_WAIT` | `AUTHORIZED` | Backoff elapsed | Policy version **changed**, so the action must pass the release boundary again (T10–T12) | SWP |
| T27 | `RETRY_WAIT` | `FAILED` | Retry budget (attempts, elapsed, cost) exhausted | — | SWP |
| T28 | `UNKNOWN_OUTCOME` | `RECONCILING` | Reconciler claims (reconciler lease, generation incremented) | Connector reconciliation `lookup` supported | REC |
| T29 | `UNKNOWN_OUTCOME` | `NEEDS_HUMAN_RESOLUTION` | Pinned contract proof standard `NONE`, no lookup, or the contract has been revoked | Not READ_ONLY. **Every call has settled** (Rev 2.5). | REC / SWP (**Slice A: SWP**, Rev 2.5) |
| T29a | `UNKNOWN_OUTCOME` | `RETRY_WAIT` | — | The pinned contract is **READ_ONLY** and not revoked. No lookup is needed. | SWP |
| T30 | `RECONCILING` | `SUCCEEDED` | **Positive evidence**: an external record carrying this action's operation key | Fenced by reconciler generation | REC |
| T31 | `RECONCILING` | `RETRY_WAIT` | **Authoritative negative evidence** (see Proof standard) | Retry policy allows it. Same operation key. | REC |
| T32 | `RECONCILING` | `FAILED` | Authoritative negative evidence | Retry not allowed or budget exhausted | REC |
| T33 | `RECONCILING` | `UNKNOWN_OUTCOME` | "Not found" under `BEST_EFFORT`, a lookup error, or reconciler lease lost | `reconcile_attempts := reconcile_attempts + 1`, with backoff | REC / SWP |
| T34 | `RECONCILING` / `UNKNOWN_OUTCOME` | `NEEDS_HUMAN_RESOLUTION` | Conflict (for example multiple records, or a mismatched amount) **or** reconcile attempts or time exhausted | From `UNKNOWN_OUTCOME`: every call has settled (Rev 2.5) | REC / SWP |
| T35 | `NEEDS_HUMAN_RESOLUTION` | `SUCCEEDED` | Operator resolves as succeeded | Reason and external evidence reference are required | OPR |
| T36 | `NEEDS_HUMAN_RESOLUTION` | `FAILED` | Operator resolves as not executed | Reason and evidence are required | OPR |
| T37 | `NEEDS_HUMAN_RESOLUTION` | `RETRY_WAIT` | Operator authorises a retry | Reason required. Same operation key. **High-risk actions need a second, distinct operator** (two-person rule). **Slice A: every retry** (Rev 2.5). | OPR (+OPR) |

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
| Natively idempotent (same key) | Retry per policy | Retry with the same key (T24). **Slice A: `UNKNOWN_OUTCOME` (T23), reconcile first** (Rev 2.3). | Reconcile first. Retry with the same key after authoritative negative evidence. |
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

## Phase 4 implementation (Rev 2.2)

Slice A Phase 4 implements T1–T13 and T15 in `migrations/00005_actions.sql` and `internal/action`. Workers, leases and dispatch (T14, T16 onwards) are Phase 5. The rules below record where the implementation had to choose; each is the conservative reading of the table above.

**Enforcement.** `eacp.actions` carries every Phase 4 column of this ADR, with `operation_key` generated as `eacp:{tenant_id}:{id}`. A `BEFORE` trigger re-checks every row of the table for every write, including raw SQL as `eacp_app`: it rejects any transition not listed, any change to a terminal action, and any change to a column the move doesn't own. It also records the actor, and stamps `state_changed_at` with the database clock. An `AFTER` trigger journals every insert and transition (hash-chained, same transaction, last statement) without digests or payloads.

**Actors.** Every transaction binds exactly one actor:
- An agent version (`app.agent_version_id`, set only after API-key authentication). This is API, GOV and REL when the agent's own request drives the action.
- A principal (`app.actor_id`). APR drives T6/T7 through the vote cascade; REQ and OPR can cancel.
- A named system component (`app.system_actor`, only `sweeper`). This is SWP, and GOV/REL when the sweeper drives the action.

An agent can only act on its own agent's actions. Principals may perform only T6–T8 and cancellation, and the system never cancels.

**T1.** An agent key is required. `Idempotency-Key` (1–255 visible ASCII characters) is unique per (tenant, agent). A replay with the same `input_digest` returns the existing action and is never rejected by admission; a different digest is 409. Admission counts `QUEUED` actions (Rev 2.3: every released, unfinished action), per tenant and globally (defaults 1000 and 10000, configurable, never unbounded). An action over a limit is 429 with `Retry-After`, and no action is created. The counts are advisory under concurrency: concurrent submissions may overshoot by their number. That is acceptable because admission controls capacity, not safety. `not_after` is at most 24 hours ahead (default 1 hour), by the database clock.

**T2.** Deterministic denials also cover an asserted subject that isn't an enabled human principal of the tenant (`subject_invalid`), so the attempt stays auditable.

**T2a.** The action stays `RECEIVED` on any of these outcomes:
- A PDP error or timeout (`EACP_PDP_TIMEOUT`, default 5s).
- A malformed decision, which also raises the alert `governance.malformed_decision`.
- No active policy for the tenant.
- Inputs that changed during evaluation three times in a row.

The API answers 503 with `Retry-After` and `action_id`.

**T6–T8 cascade from the approval rows** in the same transaction. Issuing a grant authorizes the action (T6). A denied request denies it (T7). An expired request expires it: T8 from `PENDING_APPROVAL`, T13 from `AUTHORIZED`. Every transaction that touches an action locks the action row first, including a vote (lock order: action → approval rows → registry `FOR SHARE` → audit chain head).

**T11 is generalised.** It applies whenever revalidation returns `escalate` and no usable grant exists for the current enforced digest and policy version. That covers a new policy version, and also a contract change that turns an allowed action into an escalated one. The old request is voided and a new one is bound to the fresh decision.

**T13 includes approval expiry.** A grant that expired before release is never consumed. The request expires, and so does the action. This is the ADR-005 default.

**Terminal states void approval state.** An action that becomes `DENIED`, `CANCELLED` or `EXPIRED` voids its live requests and expires its unconsumed grants in the same transaction.

**Cancellation (T9, T13, T15) is limited to three states.** It is allowed only from `PENDING_APPROVAL`, `AUTHORIZED` and `QUEUED`. A `RECEIVED` action can't be cancelled; it can only be decided or expire. Cancellation requires a reason. The submitting agent, the subject principal or an `operator` may cancel, and cancellation never consults the PDP.

**HTTP.**
- `POST /v1/actions` (agent keys only) answers:
  - 200 for a terminal action and 202 for one still in progress, with the action in the body.
  - 409 for an idempotency conflict, 429 at admission, and 503 on T2a.
- `GET /v1/actions/{id}` is open to the action's own agent, operators and auditors. Another agent's action is 404.
- `POST /v1/actions/{id}/cancel` is open to agents and principals, and the database decides.
- Both `POST /v1/actions` and `GET /v1/actions/{id}` accept `?wait=` (at most 60s). It polls until the action is terminal or the wait ends. An agent's own wait also drives the release of its approved action.

**Outbox.** The release transaction inserts one `action.queued` outbox row. It carries only `action_id` and the request's W3C `traceparent`. The row is a hint, never an authority (principle 2). Phase 5 workers poll PostgreSQL directly, so publishing the outbox is not needed for correctness. Phase 10 publishes it to NATS JetStream as work hints and dashboard events (ADR-014). A hint only wakes the worker's claim loop, and polling stays on.

**Sweeper.** The sweeper runs in `controlplane-api` every `EACP_ACTION_SWEEP_INTERVAL` (default 1s). In each pass it:
- Expires overdue actions (T5, T13, T15) and lapsed approvals (T8, T13).
- Re-evaluates `RECEIVED` actions (T2a).
- Releases `AUTHORIZED` ones (T10–T12).

Each step is an ordinary engine transition, so racing an API request on the same action is harmless.

## Phase 5 implementation (Rev 2.3)

Slice A Phase 5 implements T14 and T16–T27, and cancel requests in `EXECUTING` and `RETRY_WAIT`. The code is in `migrations/00006_execution.sql`, `internal/worker` and the sweeper in `internal/action`. Reconciliation and human resolution (T28–T37, T29a) are Phase 7, and the HTTP connector is Phase 6. The rules below record where the implementation had to choose; each is the conservative reading of the table above.

**The worker actor.** A worker transaction binds `app.system_actor = 'worker'`, `app.worker_id` and `app.lease_generation` (`storage.SetWorker`). Every lease-holder move checks all three against the row (`eacp.assert_lease_holder`). A stale worker is therefore rejected by PostgreSQL itself, not only by a `WHERE lease_generation = $g` clause the worker might omit. The worker makes only execution moves: it never makes a governance move, and it never cancels. Only the sweeper makes T23–T27 and the lapsed-lease T17. Heartbeats, dispatch intents and results belong only to the lease holder.

**Claim (T14).** A narrow `SECURITY DEFINER` function (`eacp.claimable_actions`) lists the oldest `QUEUED` actions across tenants, in the order they entered `QUEUED`. It returns only actions whose connector protocol the worker implements, and whose `(tenant, secret_ref, endpoint host)` it holds a credential for. So a worker never claims an action it can't execute, and nothing loops at the head of the queue. The list is a hint. The worker then claims each action in its own tenant transaction with `FOR UPDATE SKIP LOCKED` and a state CAS. The guard requires `lease_generation + 1`, the claiming worker's id and generation, and a lease of at most 10 minutes.

**Phase 12 amendment (ADR-011).** The claim hint now orders eligible work by PostgreSQL tenant/team turns and priority aging, and omits capacity-saturated groups. T14 enforces connector capacity under a transaction advisory lock and advances scheduler state before the audit append. The worker requests a fresh hint after each successful claim. The other T14 fencing rules above still apply.

**Phase 13 amendment (ADR-022).** T14 is also refused while the connector's shared circuit is open or the connector is disabled, and T16 reads that circuit row `FOR SHARE` and is refused likewise; the worker releases such a lease instead (T17). Every edge that grants a retry (T20, T22a, T25, T26, T31, T37) now requires the contract's whole retry budget, which adds a retry time and a retry cost to `max_attempts`; T27 and T32 accept any exhausted limit. The worker's default backoff is jittered. The rest of this section still applies.

**Heartbeat.** A heartbeat extends a live lease only (`OLD.leased_until > now()`), because an expired lease belongs to the sweeper. It changes nothing but the lease, and it is neither a transition nor journaled. It returns the cancel request. The worker heartbeats every lease/3 while a call is in flight, and cancels the call if the lease is lost or a cancel is requested.

**Dispatch intent (T16).** T16 is its own committed transaction before any external call, with registry rows read `FOR SHARE`. The database requires all of:
- A live lease held at the named generation.
- `attempt_count + 1`.
- No cancel request, and `not_after` not passed.
- No drift (`eacp.dispatch_drift`): the capability check passes, the subject is an enabled human, the pinned contract is still the tool's active contract and not revoked, and the policy pointer still equals the pinned bundle and version.
- A new `leased_until` beyond `now()` + the call timeout + 1s, and at most 10 minutes ahead.

The call timeout is the contract's `timeout_ms`: default 30s, capped at 5 minutes. The `action_attempts` row (attempt number, generation, worker, operation key, pinned contract, enforced digest, call deadline) is inserted by the action trigger in the same transaction. A dispatch intent without an attempt, or an attempt without a dispatch intent, is impossible. Every attempt reuses the action's `operation_key`.

**Drift at T16.**
- A moved policy pointer is T16a: back to `AUTHORIZED`, then through the release boundary again.
- Any other drift is T16b: `DENIED` with reason `revoked_before_dispatch: <reason>`.
- The worker recomputes the enforced digest (JCS, RFC 8785) from the stored payloads before the dispatch intent. On a mismatch it denies (T16b, `enforced_digest_mismatch`) and logs the security alert `worker.enforced_digest_mismatch`. This is invariant 14 enforced at the last point before the call. The database accepts that reason only from the lease holder.

**Results (T19–T22a) are fenced.** In one transaction the worker locks the action, completes its attempt row, and moves the action. The attempt outcome is `succeeded`, `no_effect` or `ambiguous`; an attempt completes once, and only by its own worker and generation. The guard requires the matching outcome to have been recorded in the same transaction. A deferred constraint trigger rejects a completed, non-late attempt whose action is still `EXECUTING`.

The worker classifies results conservatively:
- `succeeded` requires a non-empty external reference.
- `no_effect` requires an error class in the pinned contract's `no_effect_errors`. The database checks this as well.
- Everything else, including a connector panic, is `ambiguous`.

Before storing a result, the worker drops any connector-returned field that contains a loaded credential. A success that loses its reference this way becomes ambiguous.

The moves:
- A definitive no-effect retries (T20) while attempts remain, no cancel is requested and `not_after` hasn't passed. Otherwise it is `FAILED` (T21).
- An ambiguous result retries (T22a) only for an unrevoked `READ_ONLY` contract under the same conditions. Otherwise it is `UNKNOWN_OUTCOME` (T22).
- A retry is scheduled within (now, now + 1 hour], with exponential backoff (1s doubling, at most 5 minutes).

**Late results.** If the worker's attempt completes after its action left `EXECUTING` at that generation, the attempt is marked `late` and journaled as `action.late_result`. There is no state change. It is evidence for Phase 7 reconciliation only, never sufficient for `FAILED`.

**Lease expiry (sweeper).**
- `LEASED` past `leased_until` → T17 `QUEUED`. That is safe because no dispatch intent exists; the attempt count is unchanged and the generation is kept for the next claim.
- `EXECUTING` past `leased_until` → T23 `UNKNOWN_OUTCOME`.
- **T24 is narrowed to unrevoked `READ_ONLY` contracts in Slice A.** Its retry is due immediately, because the lapsed lease was its backoff.

A natively idempotent contract goes to T23 and is reconciled first (Phase 7). EACP can't verify that a target honours the key across its deduplication window. With this narrowing, every attempt before a `RETRY_WAIT` or re-queue either definitively had no effect or was a read. That keeps `FAILED` truthful for re-queued actions.

**Retry scheduling (sweeper).**
- T25 re-queues a due retry only under the pinned policy version.
- T26 returns it to `AUTHORIZED` when the policy moved, and the same sweep then drives the release boundary.
- T27 fails the action when attempts are exhausted, `not_after` passed (reason `expired`, since there is no expiry edge from `RETRY_WAIT`), or a cancel was requested (reason `cancelled`).

**Cancellation after release.**
- `LEASED` → T18: a direct `CANCELLED`. The row lock and state CAS order it against T16, so the holder's dispatch intent then fails and nothing is dispatched.
- `EXECUTING` and `RETRY_WAIT`: the requester or an operator records a cancel request (`cancel_requested_at`, reason required, once only; a repeat is 409). It is journaled as `action.cancel_requested` without a state change. In `EXECUTING` the worker's heartbeat cancels the call. The result then becomes T22 `UNKNOWN_OUTCOME` (reason `cancelled during the call`), unless a definitive result arrives: a success is `SUCCEEDED`, and a no-effect is T21 `FAILED`, never a retry. In `RETRY_WAIT` the sweeper applies T27.
- `CANCELLED` is never set after a dispatch intent.
- `POST /v1/actions/{id}/cancel` answers 202 with `cancel_requested_at` for a recorded request.

**Admission.** Admission now counts every released, unfinished action: `QUEUED`, `LEASED`, `EXECUTING` and `RETRY_WAIT`. Otherwise workers draining the queue would disable the limit.

**Credential custody (ADR-001 §3).**
- Only `execution-worker` accepts `EACP_CONNECTOR_SECRETS_FILE` (`config.Options.AllowConnectorSecrets`). Any other service refuses to start with it set, and compose mounts the file into the worker only.
- Secrets are keyed by `(tenant_id, secret_ref)` and bound to one endpoint `host:port`. The worker refuses to hand a secret to any other host, or to an endpoint URL with userinfo. The file is validated strictly and fails closed.
- Secret values are redacted in every string, JSON and log form, and registered with the service's log redactor.
- Values are never stored, and never journaled or put in the outbox. A test with a canary secret checks the rows, attempts, journal, outbox and logs.
- Phase 5 registers no connector protocol, so a deployed worker claims nothing until Phase 6 adds HTTP.

**Worker shutdown.** On shutdown the worker stops claiming. A leased action without a dispatch intent is released (T17). A call already in flight completes within its deadline and records its result.

### Phase 6 HTTP connector and Fake ERP (Rev 2.4)

**HTTP wire protocol.** After T16 commits, the connector sends `POST <endpoint>/v1/execute` with the enforced payload as the `payload` member of a JSON envelope and the registered `connector.tool` as `tool`. It sends the worker-held credential as a bearer token and the tenant UUID in `X-EACP-Tenant-ID`. A native contract puts the stable operation key in its declared idempotency header; a correlation-only contract puts the key in its declared envelope field. A `none` contract sends no key. The HTTP connector rejects malformed operation keys and unsafe header names before sending. It never follows redirects or uses environment proxies. It removes Go's automatic request replay capability from POSTs, even when an `Idempotency-Key` header is present: only the worker's fenced state machine may decide whether another attempt is allowed.

The response is bounded to 16 KiB. A 2xx response needs an external reference to be definitive success. A non-2xx response can be definitive no-effect only if its `error_class` appears in the pinned contract and it carries no external reference. Connection refusal before sending has its own certifiable class. Timeouts, resets, 429, uncertified 5xx, malformed responses, and contradictory result fields are ambiguous. The worker repeats these checks after scrubbing secret-bearing fields.

`Lookup` calls `GET <endpoint>/v1/operations/{operation_key}` with the worker credential. A 200 with an external reference is positive evidence. A 404 is absence evidence only when its bounded JSON body has `error_class: not_found` and no reference; a generic router 404 is unknown. `LookupAbsent` is **not** a no-effect decision. Phase 7 must apply the pinned proof standard before a negative lookup permits any transition or retry.

**Fake ERP proof boundary.** Fake ERP requires the worker bearer credential on its privileged API and records the derived principal, never the token, in its audit log. It appends and syncs an effect and its audit entry before responding, reloads that log on restart, and stops serving privileged operations after an uncertain log write. A registered connector's `create_po` tool has immediate lookup visibility and can be certified `AUTHORITATIVE` when its contract matches the deployment. Its `create_po_eventual` tool permits delayed visibility and must be certified `BEST_EFFORT`; its 404 is never authoritative negative evidence. Native operation keys deduplicate. The same key on a correlation-only call may produce multiple records, in which case lookup reports a conflict. Neither the connector nor Fake ERP claims exactly-once execution.

## Phase 7 implementation (Rev 2.5)

Phase 7 implements T28–T37 and T29a in `migrations/00007_reconciliation.sql`, `internal/worker/reconciler.go`, `internal/action/sweeper.go` and `internal/action/resolution.go`. As in earlier phases, each rule lives in the `eacp.actions_guard` trigger, which applies to raw SQL too, and is tested as `eacp_app` (`internal/worker/reconcile_schema_test.go`).

**Settle rule.** An attempt's call may still take effect until its `call_deadline`. The outcome of an action is **settled** at `max(call_deadline) + greatest(call_timeout, 1 s)` over all its attempts (`eacp.outcome_settled_at`). The database refuses, before that time:
- negative evidence (T31, T32);
- handing the action to a human (T29, and T34 from `UNKNOWN_OUTCOME`). A late result is then already part of the evidence an operator sees.

On entering `UNKNOWN_OUTCOME` from `EXECUTING` (T22, T23), `next_reconcile_at` is set to the settle time or now, whichever is later, and `reconcile_attempts` is reset. A found record (T30) needs no settling: positive evidence is final.

**Reconciler lease.** The reconciler runs inside `execution-worker`, which is the only service holding connector secrets. It reuses the action's lease columns:
- T28 claims an action that is due, with `FOR UPDATE SKIP LOCKED`, and increments `lease_generation`. The transaction's actor is `storage.SetReconciler` (component `reconciler`, with a reconciler id and a generation).
- `eacp.assert_lease_holder` expects the `reconciler` component in `RECONCILING`. A stale worker or a stale reconciler is fenced out by the same generation check.
- There is no reconciler heartbeat. A lookup's timeout is the smaller of the contract's call budget and half the lease.
- A lapsed reconciler lease is returned by the sweeper (T33, reason `reconciler lease expired`), counted as an attempt and backed off.

Candidates come from the SECURITY DEFINER hint `eacp.reconcilable_actions`, like the worker's claim hint. The claim itself re-checks everything under RLS.

**Evidence.** Each lookup writes one immutable `eacp.reconciliation_checks` row per generation (`found`, `absent`, `unknown` or `conflict`), in the same transaction as the move it justifies. The guard trigger stamps the reconciler, the time, the pinned proof standard and the contract version, and UPDATE is revoked. T30–T34 from `RECONCILING` each require the check recorded in their own transaction. The HTTP connector reports a conflict (`LookupConflict`) only for a 409 whose body is `{"error_class":"conflict"}` with no reference; anything else is unknown. The worker drops a found reference that contains a connector secret.

**Decisions** (`decide`, and enforced again by the trigger):

| Lookup | Condition | Move |
|---|---|---|
| found | No attempt reported a different reference | T30 `SUCCEEDED` with the found reference |
| found | An attempt (late ones included) reported a different reference | T34 conflict |
| conflict | — | T34 |
| absent | Proof standard not `AUTHORITATIVE` | T33 still unknown (T34 once exhausted) |
| absent | An attempt reported success | T34 conflict: a reported success is never overruled by absence |
| absent | Not settled | T33 |
| absent | `AUTHORITATIVE`, settled, and a retry is possible (no cancel, not expired, attempts left) | T31 `RETRY_WAIT` with the same operation key |
| absent | `AUTHORITATIVE`, settled, and no retry is possible | T32 `FAILED` |
| unknown | — | T33 (T34 once exhausted) |

T31 and T32 are mutually exclusive in the database: T32 applies **only** when a retry is not possible. An authoritatively absent effect with a retry left is therefore retried, never failed.

**Bounds.** The reconciler gives up after `EACP_RECONCILE_MAX_ATTEMPTS` lookups (default 10, range 1–50), and after `EACP_RECONCILE_MAX_AGE` since the outcome became unknown (default 1 hour, at most 24 hours). Backoff between lookups is capped at one hour. `reconcile_attempts` is **not** capped in the database. A sweeper T33 after a lapsed lease could otherwise strand an action. The age backstop in the sweeper bounds it instead.

**Sweeper routing.** The sweeper does no lookups. For an `UNKNOWN_OUTCOME` action:
- an unrevoked READ_ONLY contract is retried at once (T29a);
- once the outcome has settled, a revoked contract, or one with lookup or proof standard `none`, goes to a human (T29);
- once settled, an action older than the maximum age goes to a human as well (T34).

T29 means "no usable lookup". The trigger turns an `UNKNOWN_OUTCOME → NEEDS_HUMAN_RESOLUTION` move into T34 when a usable lookup exists. The reconciler never performs T29.

**Human resolution (§20.3).** Operators resolve through `eacp.action_resolutions`: POST `/v1/actions/{id}/resolutions`, then `/confirm` or `/withdraw`, or `eacpctl action resolve|confirm|withdraw`.
- Every resolution needs the `operator` role. The resolver may not be the action's subject, the agent's owner, or a member of the owning group. A reason is always required.
- `succeeded` needs evidence and the external reference, and applies at once (T35).
- `failed` needs evidence and applies at once (T36).
- `retry` is only **proposed**. A **second, distinct operator** applies it (T37); either operator may withdraw it. Every retry is two-person in Slice A, because a retry dispatches the effect again.
- The database allows one open proposal per action. Proposals are voided when the action leaves `NEEDS_HUMAN_RESOLUTION`.
- Applying a resolution moves the action in the same transaction (`action_resolutions_apply`). Resolutions and their decisions are journaled as `action.resolution`.

The queue (`GET /v1/actions?state=`) and the evidence (`GET /v1/actions/{id}/evidence`, which returns attempts including late results, checks and resolutions) are available to operators and auditors.

**Verification.** The Fake ERP flagship tests in `internal/worker/reconcile_integration_test.go` run the real HTTP connector, worker, sweeper and reconciler:
- lost responses (reset, timeout, 5xx after effect) → `SUCCEEDED` with one ERP record;
- delayed visibility under `BEST_EFFORT` → no retry → a human, or `SUCCEEDED` once visible;
- a worker killed after the dispatch intent → T23 → found, or authoritative absence → one retry with the same key;
- authoritative absence → `FAILED` only once no retry remains;
- a duplicate correlation key → conflict → a human;
- no credential in checks or the journal.

Nothing here claims exactly-once execution: the claim is at most one effect per operation key where the target deduplicates, and otherwise an explicit human decision.

## Phase 8 hardening (Rev 2.6)

**T5a: cancelling a `RECEIVED` action.** An action stays `RECEIVED` while governance is unavailable (T2a). Up to Rev 2.5 no edge left `RECEIVED` except evaluation and expiry. During an outage the requester therefore could not withdraw an action; it could only wait for `not_after`. That contradicts MASTER_PLAN §103 invariant 18: governance failure must never block cancellation.

T5a lets the same actors as T9 cancel it: the submitting agent, the subject, or an operator, with a reason, and never the system. It is the most conservative choice available:
- a `RECEIVED` action has no decision, grant or dispatch intent, so cancelling it can only prevent execution;
- evaluation re-reads the action under its row lock after the PDP call, so a decision that arrives after the cancel changes nothing and records no evidence.

Migration 00008 adds the edge to `eacp.actions_guard`. Tests: `TestReceivedActionCanBeCancelledByTheRequesterOrAnOperator` (raw SQL), `TestCancelWinsOverAnInFlightDecision` (a cancel during a PDP call) and `TestGovernanceOutageFailsClosedWithoutBlockingSafety`.

**Evidence reconstruction (invariants 10 and 17).** `GET /v1/actions/{id}/evidence` (operators and auditors) and `eacpctl action evidence` return everything recorded about one action, read in one snapshot:
- the action;
- every governance decision: verdict, policy version, reasons, input and enforced digests;
- approval requests with their votes and the one-time grant, including what consumed it;
- attempts, including late results;
- reconciliation checks;
- operator resolutions;
- every journal entry about these records.

The tenant's journal chain is verified in the same snapshot. A broken chain is reported in the evidence (`chain.verified = false`), not as an error, so the evidence stays readable when it matters most. The verification reads the whole tenant journal; that cost is accepted for Slice A.

**Supervision.** Services fail closed at startup: for example, they exit when the database is unreachable or unsafe. They rely on their supervisor to start them again. Compose runs `controlplane-api`, `execution-worker` and `fakeerp` with `restart: on-failure`; production deployments need the equivalent. The demo found this: restarting the worker together with PostgreSQL made the worker exit, and nothing restarted it.

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
| Default reconcile attempts and time limit | Finite and configurable (Rev 2.5: 10 lookups, 1 hour). Exhaustion goes to `NEEDS_HUMAN_RESOLUTION`, never `FAILED`. |
| Which operator resolutions are high risk (T37) | All of them: every retry needs a second operator, and every resolution is separated from the subject and the agent's owners (Rev 2.5) |
| When a lookup's absence is final while a call may still be in flight | Only after every call has settled (Rev 2.5 settle rule) |
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
- **Every** transition row (T1–T37, including the lettered rows) has at least one test that exercises it, and at least one test showing its guard rejecting.
- **Registry drift between `QUEUED` and T16** (Rev 2.1): tool removed from the allowlist, AgentVersion suspended, contract revoked or superseded, or policy pointer moved. Each must prevent dispatch (T16a/T16b), including when the change commits concurrently with the dispatch-intent transaction.
- **Transient PDP outage at submission** → `RECEIVED` + 503. Resubmit with the same `Idempotency-Key` after recovery → evaluated normally, and never stuck in `DENIED`.
- **Crash after T16 commits, before the external call starts** → T23 → reconcile. Under `BEST_EFFORT` → no retry → `NEEDS_HUMAN_RESOLUTION`. Under `AUTHORITATIVE` → negative evidence → retry with the same operation key.
- **Contract revoked while `EXECUTING` / `UNKNOWN_OUTCOME`** → the conservative path (T23/T29), never T24/T29a.
- **Model tests include external mutations** (registry, policy and contract changes interleaved with transitions), not just transitions in isolation.
