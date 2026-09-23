# ADR-005: Approval Ownership and the Atomic Execution Boundary

- **Status:** Accepted — Rev 2.3 (Phase 4 release boundary, 2026-09-23; Rev 2.2 Phase 3 approval-store contract)
- **Date:** 2026-09-23
- **Phase 0 gate:** yes
- **Related:** MASTER_PLAN §13, §14, §14.1, §14.2, §15, §16, §57, §103 (inv. 2, 7, 10, 14–18); ADR-002, ADR-004
- **Review input:** `docs/reviews/2026-09-23-master-plan-review.md` (C2, C3, X1, X2, X3)

## Context

Revision 1 said EACP should *"not duplicate governance engine state"* and should *"consume one-time approval"* inside the atomic boundary. Those two statements can't both hold:

- ACS is **stateless**. The host supplies the full snapshot on every call, and the AGT Go SDK has no approval-chain execution layer (issue #3083). There's no AGT-side approval state to avoid duplicating.
- Consuming an approval **in the same Postgres transaction** as queueing the action is only possible if the approval lives in EACP's database. Otherwise "consume approval" and "queue action" become a distributed transaction. A crash between them either burns an approval with no action, or leaves an action queued with a reusable approval, which violates invariant 2.

Revision 1 also bound the approval digest to the request as submitted. When a policy returns `transform`, the payload that actually executes is different. The approver could consent to one payload while another one runs.

The cross-review added three more gaps:
- Approvals can go stale when policy changes (X2).
- Approver eligibility and separation of duties weren't modelled (X3).
- Decision evidence wasn't persisted (X1).

## Decision

### 1. AGT decides; EACP stores and consumes

- The `GovernanceProvider` (ADR-002) returns **verdicts** only.
- EACP owns **all approval state**: requests, votes and grants. It also owns **decision evidence**, all in Postgres, under tenant RLS (ADR-021).
- Approval state is **durable**. No EACP process holds approval state in memory, so a restart of any process loses nothing (invariant 16).

### 2. Two digests; everything downstream binds to the enforced one

```text
input_binding    = {tenant, agent, agent_version, subject, operation, target,
                    tool, tool_schema_version, resource, payload}
input_digest     = SHA-256(JCS(input_binding))           -- request identity
enforced_binding = input_binding with payload := enforced_payload
enforced_digest  = SHA-256(JCS(enforced_binding))        -- what executes
```

- **Idempotency** conflict detection uses `input_digest`. It's about the client request: the same key and same digest returns the existing action, and a different digest returns 409.
- **Approval, revalidation and execution** use `enforced_digest`.
- The approver UI and API show the **enforced payload** and `enforced_digest`.
- Workers execute **only** the enforced payload persisted on the action row. They never use anything the agent sends later (invariant 14).

### 3. Approval data model

```text
approval_requests
  id, tenant_id, action_id,
  enforced_digest, policy_bundle_id, policy_version,
  required_quorum, eligible_roles[], risk_class,
  state (PENDING | GRANTED | DENIED | EXPIRED | VOIDED),
  expires_at, created_at

approval_votes
  id, tenant_id, request_id, approver_principal_id,
  decision (APPROVE | DENY), reason,
  authorization_basis (role, role_grant_id, policy_bundle_id, policy_version),
  snapshot_hash, voted_at
  UNIQUE (request_id, approver_principal_id)

approval_grants
  id, tenant_id, request_id, action_id,
  enforced_digest, policy_version,
  expires_at, consumed_at NULL, consumed_by_action_id NULL
  UNIQUE (request_id)
```

### 4. Approver eligibility and separation of duties (conservative)

Eligibility is evaluated **inside the vote transaction** against EACP's canonical `principals`, `role_grants` and `group_memberships` tables. Those rows are read `FOR SHARE`, so a concurrent role or membership change serialises with the vote. The vote stores a hash of the effective-principal snapshot it was evaluated against.

A vote counts only if **all** of these hold. Otherwise the transaction rejects it and stores no vote. A rejected SQL statement cannot append an audit event in the same rolled-back transaction; API security logging for failed attempts is separate from the durable change journal.

- The approver is an authenticated **human principal** in the **same tenant**. Service principals and agents can't vote.
- Votes cast through delegation count as the **effective (delegating) human principal**, and every rule below applies to that principal.
- The approver holds one of `eligible_roles` from the decision's approval requirement.
- The approver is **not** the requesting subject.
- The approver is **not** the agent's owner. If the owner is a group or team, the approver is **not a member** of it, resolved through `group_memberships`.
- **If the owner's membership can't be resolved** (for example an unknown group or a missing membership sync), **the vote is rejected** (fail closed).
- **Enabling-change SoD:** the approver did **not** author or activate the `policy_version`, allowlist entry, or `connector_contract_version` the action depends on.
- The approver hasn't already voted on this request.
- The request is `PENDING` and not expired.

Outcomes:
- **Any eligible `DENY`** → the request is `DENIED` and the action is `DENIED` (ADR-004 T7).
- **Quorum of eligible `APPROVE`** → a grant is created and the action is `AUTHORIZED` (T6).

### 5. Policy versions and the policy pointer (Rev 2.1)

- `policy_bundles(tenant_id, version, content, authored_by, created_at)` is **insert-only**, and versions are immutable.
- `tenant_policy_pointer(tenant_id PRIMARY KEY, current_version, activated_by, activated_at)` holds **one row per tenant**.
- **Activation** is a privileged two-person operation: the author and the activator must differ. It's `UPDATE tenant_policy_pointer ... WHERE tenant_id = $t`, which takes the row lock.
- **Every** transition guarded by policy version reads the pointer with `SELECT ... FOR SHARE`: the release boundary (R1) and dispatch intent (ADR-004 T16). `FOR SHARE` conflicts with the activation's `UPDATE`, so activation and guarded transitions serialise. Under READ COMMITTED, a transaction that waited on the lock re-reads the committed pointer, sees the new version and aborts (R1) or re-routes (T16a). That closes the R0/R1 time-of-check window.
- The same pattern (immutable versions plus a locked "active" pointer, with two-person activation) applies to **allowlists** and **connector contracts**. The release boundary pins `connector_contract_version` on the action.

### 5a. The release boundary (the atomic execution boundary)

The PDP is **never** called while a DB transaction is open.

```text
Step R0 (outside tx): revalidate
    decision' = Evaluate(current snapshot)
    fail / timeout / incomplete        → no change; retry later (ADR-002 §6)
    decision'.verdict = deny           → go to R1 with intent DENY
    decision'.enforced_digest ≠ action.enforced_digest
                                       → go to R1 with intent DENY (reason digest_changed)
    decision'.policy_version ≠ action.policy_version
        and verdict = escalate         → go to R1 with intent RE_APPROVE
    otherwise                          → go to R1 with intent RELEASE

Step R1 (ONE transaction, READ COMMITTED, row locks):
    SELECT action FOR UPDATE WHERE id=$a AND state='AUTHORIZED'
        → 0 rows: abort (someone else transitioned it)
    CHECK now() < action.not_after                      else → EXPIRED
    SELECT current_version FROM tenant_policy_pointer
     WHERE tenant_id = $t FOR SHARE
    CHECK current_version == decision'.policy_version   else → abort, retry R0
    CHECK agent_version.state == 'ACTIVE'  (FOR SHARE)
          AND tool ∈ active allowlist      (FOR SHARE)
          AND active certified connector contract exists (FOR SHARE)
                                                        else → DENIED
    IF intent = DENY        → UPDATE action → DENIED
    IF intent = RE_APPROVE  → void old request/grant; insert new approval_request
                              bound to new policy_version → PENDING_APPROVAL
    IF intent = RELEASE:
        IF decision'.policy_version ≠ action.policy_version:
            -- policy changed and now allows: outstanding approval state is void
            UPDATE approval_requests SET state='VOIDED'
             WHERE action_id=$a AND state IN ('PENDING','GRANTED')
            UPDATE approval_grants SET expires_at = now()
             WHERE action_id=$a AND consumed_at IS NULL
        IF the action requires approval under decision':
            UPDATE approval_grants
               SET consumed_at = now(), consumed_by_action_id = $a
             WHERE action_id = $a
               AND enforced_digest = $enforced_digest
               AND policy_version  = $policy_version
               AND consumed_at IS NULL
               AND expires_at > now()
            → must affect exactly 1 row, else ROLLBACK (no release)
        reserve budget                (Slice B, ADR-012: eacp.budget_reserve, which runs
                                       before any audited write and before the grant is consumed)
        UPDATE action SET state='QUEUED', released_at=now(),
               release_decision_id = decision'.decision_id,
               policy_version = decision'.policy_version,          -- pinned
               connector_contract_version = <active contract ver>  -- pinned
         WHERE id=$a AND state='AUTHORIZED'
        INSERT decision evidence (decision')
        INSERT audit journal events (hash-chained)
        INSERT outbox event (with traceparent)
COMMIT
```

Guarantees:
- **Either everything happens or nothing does.** A failed check rolls back the grant consumption too, so an approval is never burned without a queued action.
- **At most one consumption per grant**, because consumption is a conditional `UPDATE ... WHERE consumed_at IS NULL`, and grants are unique per request (invariant 2 and 15).
- **A grant is valid only for the policy version it was issued under.** A policy change forces re-evaluation. A new `escalate` needs a **new** approval (X2, the conservative choice). A new `allow` **voids** the old request and grant in the same transaction, so no stale grant survives.
- **No time-of-check gap against policy activation**, because both sides lock the tenant policy pointer row (§5).
- **Nothing is executable before COMMIT.** Workers claim only `QUEUED` rows (ADR-004 T14).

What the release boundary does **not** guarantee: freedom from duplicate *external* effects. That's the job of fenced dispatch, operation identity and reconciliation (ADR-004).

### 6. Decision evidence is required

Every evaluation that leads to a transition (submission and revalidation) persists: provider, provider instance, policy bundle ID and version, verdict, reasons, both digests, decision ID and time. A transition whose decision evidence can't be persisted doesn't happen (invariant 10).

### 7. Privileged operator actions

Human resolution (ADR-004 T35–T37), approval votes, policy bundle changes, allowlist changes and connector contract changes all go through the same pattern:
- An authenticated principal with a role.
- A mandatory reason.
- A hash-chained audit event.
- A two-person rule for high-risk operations (§57).

### 8. Phase 3 implementation boundary and defaults

Migration `00004_governance_approvals.sql` owns policy versions, the per-tenant pointer, decision evidence, requests, votes and grants. `internal/approval` exposes request creation and grant consumption as caller-transaction operations so Phase 4 can put them inside the action transition. There is no `actions` table or Action API in Phase 3; `action_id` is an immutable future reference and Phase 4 must add the tenant-scoped action FK and atomic release transaction.

Only the EACP-managed `approver` role is eligible in Slice A. A request captures the owner-group membership at creation and the enabling policy, allowlist and contract actors. A vote checks both that snapshot and current membership, role, principal state, policy version and policy revocation. The current policy and unexpired request are checked again at grant consumption. A `DENY` vote immediately ends the request; a distinct-person quorum creates one grant. Phase 4 maps those states to action transitions.

The approval TTL is explicit in the policy, capped at 24 hours, and request expiry cannot exceed the action's `not_after` or the evaluation time plus TTL. Expiry can only be shortened. Direct SQL under `eacp_app` is subject to the same triggers, tenant RLS and audit as the Go service. The database guards protect application mistakes; a party holding a usable `eacp_app` credential can set tenant and actor transaction context, so database credentials remain a trusted service boundary under ADR-003. Phase 4 must treat the release transaction as the final enforcement point.

### 9. Phase 4 release boundary (Rev 2.3)

Migration `00005_actions.sql` adds `eacp.actions` and the tenant-scoped action foreign keys from decision evidence, requests and grants. It also replaces four Phase 3 functions so that governance writes accept the action actor context: an agent version or the `sweeper` system component, besides principals. The rules:

- **Evidence.** Evidence is recorded only for an action in `RECEIVED` or `AUTHORIZED`, by that action's own agent or the system, never by a principal. Its `input_digest` must equal the action's.
- **Requests.** A request derives its agent version, tool, requesting subject and `not_after` from the action. Only one live request exists per action. Principals can't create requests. A request can be `VOIDED` only when its action is terminal, or when the policy, allowlist or contract it was bound to is no longer current.
- **Grant consumption.** Only the agent or the system can consume a grant, and only while the request's allowlist and contract are still current. A deferred constraint trigger checks at commit that every consumed grant's action is `QUEUED` with `released_at = consumed_at`. So invariant 2 holds in PostgreSQL even for raw SQL, not only through `internal/action`.

The release follows §5a with these implementation choices:

- **R0.** R0 evaluates with no transaction open. The snapshot records the policy bundle and version, the active contract, and the active allowlist. R1 locks the action `FOR UPDATE`, then the pointer and registry rows `FOR SHARE`, and compares them with the snapshot.
  - If anything differs, R1 does nothing and R0 is repeated.
  - After three repeats the release counts as unavailable, and the action stays `AUTHORIZED`.
- **T10.** The trigger requires:
  - Evidence recorded in the same transaction (`recorded_at = now()`).
  - An enforced digest and payload that are unchanged.
  - For `escalate`, a grant for this digest and policy version consumed in this transaction. Otherwise, no live request.
  - An enabled human subject, a passing capability check, and the pinned contract equal to the active one.

  The release decision is the action's new `decision_evidence_id`. It is inserted before the `UPDATE`, because the guard reads it.
- **`escalate` with no usable grant → T11.** This covers a new policy version, and also a contract change that turns an allowed action into an escalated one. A grant that expired before release instead expires the request, and with it the action (T13, the default in the table below).
- **Allow after a policy change** voids the outstanding request and grants in the release transaction, then queues.
- **Budget.** Since Phase 11, R1 reserves the action's cost on its agent's budget leaf ([ADR-012](ADR-012-budget-reservation.md) §4). The reservation runs after the action, approval rows and registry are locked, and before any audited write or grant consumption. A budget that can't take the cost denies the action (T12, `budget_exceeded`); the grant stays unconsumed and is voided with the action. The Slice A no-op hook (`action.Budget`) is gone.
- **Retries.** Serialization failures and deadlocks roll the whole transaction back and are retried.

Cross-tenant work never loosens tenant RLS for `eacp_app`. Two narrow `SECURITY DEFINER` functions do it instead, and only they read across tenants:
- `global_queued_count()` returns a count, for global admission.
- `tenants_with_open_actions()` returns tenant ids, for the sweeper.

They read through a `SELECT` policy that applies only to the schema owner.

## Consequences

**Positive**
- Invariant 2 is enforced by a single SQL predicate, and it's testable under concurrency.
- Approvals survive restarts and are fully auditable, including who could approve and why.
- A transform can't cause an approver to consent to a different payload from the one that executes.
- Policy changes can't be sidestepped by approvals issued earlier.

**Negative / costs**
- EACP maintains approval logic that AGT (Python) partly provides. This is accepted because ACS is stateless and the logic has to live next to the transaction. Upstreaming a PostgreSQL ApprovalStore is noted in §109.
- Policy-version binding may force re-approval after a policy update, even for harmless changes. That's the conservative trade-off. A future ADR may add explicit "compatible policy version" declarations.
- Two governance evaluations per approved action (submission and release).

## Unresolved assumptions and conservative defaults

| Unresolved | Conservative default |
|---|---|
| Whether a policy update should invalidate outstanding grants | Yes, always (§5 R1). |
| Owner-group membership can't be resolved | Reject the vote (fail closed, §4). Slice A models `group_memberships` in EACP. External IdP sync comes later, and until it exists only EACP-managed memberships count. |
| Whether READ COMMITTED is enough | Yes, because every guarded transition and every conflicting operator change locks the same pointer and registry rows (§5). `SERIALIZABLE` isn't required. Concurrency tests (below) are the proof. |
| Default grant lifetime | Short and configurable, capped by the action's `not_after`. Expiry means the grant is unusable and the action goes to `EXPIRED` if it wasn't released. |
| Whether `warn` should require acknowledgement | No approval is required, but `warn` is persisted in evidence and surfaced to operators. |
| Whether a retry after `RETRY_WAIT` needs a new approval | Only if the policy version changed and re-evaluation returns `escalate` (ADR-004 T26 → T11). |

## Verification

- **Concurrency:** N parallel releases of the same approved action → exactly one `QUEUED` transition and exactly one grant consumption.
- **Security:**
  - Replaying a consumed grant fails.
  - A grant for digest D can't release an action with digest D′ (parameter substitution).
  - Transform-then-approve → the worker receives the enforced payload.
  - Self-approval and owner-approval are rejected.
  - A cross-tenant vote is rejected by RLS.
- **Policy change:** a grant issued under v1, with policy moved to v2 and still escalating → `PENDING_APPROVAL` with a new request, and the old grant `VOIDED`. Moved to v2 that **allows** → released, and the old request and grant are voided.
- **Activation race:** policy activation committed concurrently with R1 (both orders, with injected delays) under READ COMMITTED → an action is never released under a version other than the one committed at release time.
- **RLS with the real application role** (no `BYPASSRLS`): a missing or wrong `SET LOCAL app.tenant_id` → zero rows, never cross-tenant rows.
- **SoD:** a team-member vote on a team-owned agent is rejected. A vote with unresolved membership is rejected. A delegated vote is judged as the delegator. The author or activator of the policy the action relies on can't approve it. A self-authored policy can't be self-activated.
- **Crash injection:**
  - Kill the process inside R1 → after restart the grant is unconsumed and the action is `AUTHORIZED`.
  - Restart all processes while `PENDING_APPROVAL` → the request and votes are intact.
- **Evidence reconstruction:** from any `action_id`, reconstruct submission decision → votes → grant → release decision → execution → outcome.

## Alternatives considered

| Option | Why not chosen |
|---|---|
| Keep approval state in AGT (Python) | ACS is stateless. It would create a distributed transaction with the action queue and break invariant 2 under crashes. |
| Consume the approval when it's granted rather than at release | The approval could be "spent" for an action that never gets queued (expiry, policy change), and it breaks the exactly-once link between a grant and an execution claim. |
| Bind approval to `input_digest` | An approver could consent to something other than what executes after a `transform`. |
| Call the PDP inside the transaction | Holds row locks across a network call, which risks contention and lock timeouts. Revalidating outside the transaction plus a policy-version check inside it gives the same safety. |
