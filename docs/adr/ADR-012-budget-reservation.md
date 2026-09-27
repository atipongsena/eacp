# ADR-012: Hard Budget Reservation

- **Status:** Accepted — Rev 1.0 (Phase 11, 2026-09-24)
- **Date:** 2026-09-24
- **Phase 0 gate:** no (Slice B)
- **Related:** MASTER_PLAN §14–§16, §45–§47, §57, §69, §85, §103 (inv. 3 [B], 8, 17); ADR-003 §4 (contracts), ADR-004 (states), ADR-005 §5a (release boundary)

## Context

MASTER_PLAN §47 asks for strict reservation: **reserve** an estimate, execute, **commit** the actual, **release** the remainder. Two actions asking for $7 each from $10 must not both proceed. §85 (Phase 11) makes reserve, commit and release atomic, and requires that 100 concurrent reservations never oversubscribe a hard budget. It also covers lock ordering, escrow, a reservation TTL, and the p99 under contention.

ADR-005 §5a reserved a place for the budget in the release transaction R1 (`action.Budget`, a no-op in Slice A).

## Decision

### 1. What an action costs: the connector contract

The operator-declared, two-person-activated connector contract (ADR-003 §4) gains a cost declaration:

| Column | Meaning |
|---|---|
| `cost_unit` | `^[A-Z][A-Z0-9_]{0,15}$`, e.g. `THB` or `TOOL_CALLS`. NULL means the tool is **not budgeted**. |
| `cost_fixed` | A non-negative per-call cost (default 0). |
| `cost_amount_field` | Optional. A top-level field of the **enforced payload** whose JSON number is added to the cost. |
| `cost_unit_field` | Optional. A top-level field of the enforced payload whose string must equal `cost_unit`, e.g. `currency`. |

- **Cost.** An action's cost is `cost_fixed + payload[cost_amount_field]`. It is computed in PostgreSQL (`eacp.action_cost`) from the enforced payload, which the governance decision and any grant bind by digest (inv. 14). An agent can't make an action cheaper without changing the digest.
- **Limits.** A budgeted contract needs `cost_fixed > 0` or an amount field. An amount must be a finite JSON number in `[0, 10^15)` with at most six decimal places, matching the exact `numeric(21,6)` reservation and account columns. More precise amounts fail closed as `budget_cost_invalid`; rounding could under-reserve the budget.
- **Contract identity.** The contract version pins the cost rule. Changing it is a new two-person-activated contract version.

### 2. Budget accounts and escrow (§46)

`eacp.budget_accounts` is a tenant table under RLS. Each account has:
- `name` and `unit`, both immutable;
- an optional `parent_id` (same tenant, same unit);
- an optional `agent_id`, which makes it an **agent leaf**: at most one per agent and unit, and it never has children;
- counters: `hard_limit`, `allocated`, `reserved` and `committed`.

`CHECK (allocated + reserved + committed <= hard_limit)`, with every value ≥ 0, is the hard guarantee. No code path can store an oversubscribed account.

**Escrow.** A child's `hard_limit` is carved out of its parent when it is set: the parent's `allocated` grows by the child's limit. So:
- the tree's spend is bounded by the root's limit, by induction;
- a reservation locks **only its leaf row**, never an ancestor.

Accounts are created with `hard_limit = 0`, so creating one grants no capacity.

### 3. Limit changes are privileged (§57)

`eacp.budget_limit_changes` records every change. Each has a mandatory reason, an actor, and a hash-chained journal entry.

- **Lowering** a limit, or keeping it, tightens control. One `admin` applies it at once.
- **Raising** a limit loosens control, so it is **two-person**. One admin proposes it; a different admin applies it.
  - The proposal records `old_limit`. Approval fails (55000) if the limit changed in between, so the approver always approves "from X to Y".
  - An account has at most one open proposal. Any admin may reject it, with a reason.
- **Application** locks the parent first, then the account (root → leaf), and moves the parent's `allocated` by the difference. A raise the parent can't cover, or a cut below what is allocated, reserved and committed, fails on the CHECK.

### 4. Reserve inside the release boundary (ADR-005 §5a, R1)

The engine calls `eacp.budget_reserve(action, contract)` in R1:
- after locking the action, its approval rows and the registry (`FOR SHARE`);
- **before any audited write**, including decision evidence and voiding or consuming approvals.

The function returns one of:

| Result | Meaning | Engine |
|---|---|---|
| `unbudgeted` | The pinned contract has no `cost_unit` | Release as before |
| `reserved` | A reservation of the cost now exists (an existing one for the same account and amount is reused) | Consume any grant, then T10 → `QUEUED` |
| `exceeded` | The leaf can't take the cost now | T12 → `DENIED` (`budget_exceeded`) |
| `no_account` | The agent has no leaf account in this unit | T12 → `DENIED` (`budget_account_missing`) |
| `invalid_cost` | The amount field is missing, not a number, negative or too large, or the unit field is wrong | T12 → `DENIED` (`budget_cost_invalid`) |

- **Denial.** Any budget denial is terminal and fails closed: "A gets $7, B is denied" (§47). The release decision is recorded as evidence, and the reason names the budget. Because the budget is checked **before** a grant is consumed, a denied action never burns an approval.
- **The leaf lock.** The function locks the agent leaf `FOR UPDATE`. Two actions reserving from one leaf serialize on that row, and nothing else.
- **Backstop.** A separate `BEFORE UPDATE` trigger on `actions` requires that a T10 of a budgeted action has an `ACTIVE` reservation. The reservation must match the action's agent leaf, unit and computed cost. So a raw-SQL release can't skip the reservation.

### 5. Commit and release: settlement never locks the account

An `AFTER UPDATE` trigger (`actions_budget`) settles the action's reservation in the **same transaction** as the state change:

| New state | Reservation |
|---|---|
| `SUCCEEDED` (T19, T30, T35) | `COMMITTED`, with `committed_amount = amount` |
| `FAILED`, `CANCELLED`, `EXPIRED`, `DENIED` | `RELEASED` (no effect happened) |
| `UNKNOWN_OUTCOME`, `RECONCILING`, `NEEDS_HUMAN_RESOLUTION`, `RETRY_WAIT` and the pre-terminal states | Held (§47: kept until reconciled, conservatively) |

- **Settlement leaves the account row alone.** It changes only the action's own reservation row. The account's `reserved` still includes a settled reservation until the next reservation or limit change **folds** it in under the leaf lock: `reserved -= amount`, `committed += committed_amount`. Until then, usage is over-counted, never under-counted. That errs on the safe side (inv. 3).
- **Why.** The tenant's audit chain head is a row lock held to commit. Resolution and reconciliation transactions append to the journal before they update the action. If settlement locked the leaf there, a release, which locks leaf then head, and a resolution, which locks head then leaf, could deadlock. With folding, the lock order is always:

  ```text
  action → approval rows → registry (FOR SHARE) → budget leaf (reserve / fold only) → audit chain head
  ```

  Limit changes lock parent → leaf → audit head and never touch actions.
- **Retries.** A retry keeps its reservation (T25), and a re-release reuses it (T26 → T10). If a new contract version reprices the action, the old reservation is released and a new one reserved in the same transaction.
- **Actual cost.** "Commit actual" commits the estimate. No connector reports an actual cost in Slice B; that is FinOps (Phase 18). The schema already separates `amount` from `committed_amount`.

### 6. Reservation TTL (§47)

Every reservation has `expires_at` = the action's `not_after`.
- A reservation is created and settled only inside action transitions. A crashed flow therefore can't strand one: either the release committed, and the action holds it, or it rolled back.
- An action that never dispatches is expired by the sweeper at `not_after` (T13, T15, T18), and the expiry releases its reservation in the same transaction.
- After a dispatch intent, the reservation outlives `expires_at` on purpose, until the outcome is known.

### 7. Journal and visibility

Account inserts, limit changes and limit applications go to the hash-chained journal, through the registry's row audit trigger, with the principal actor. Reservation events go through the action trigger, with the action's actor:
- `budget.reserved` (amount, unit, account);
- `budget.committed`;
- `budget.released` (with the reason).

The API is:
- `POST /v1/budgets` and `POST /v1/budgets/{id}/limit` (admin);
- `POST /v1/budget-limit-changes/{id}/approve|reject` (a second admin);
- `GET /v1/budgets` (admin, operator, auditor), which shows limit, allocated, active reservations, committed and available;
- action evidence, which shows the reservation.

## Amendment (Phase 25b, ADR-031): LLM calls

`eacp.budget_reservations` also holds reservations for LLM calls: `llm_call_id` is set instead of `action_id` and `contract_id` (a CHECK requires exactly one subject). Only `eacp.llm_admit`, as the call's agent, inserts one, on the agent's leaf account in the price's unit and for exactly PostgreSQL's estimate; only `eacp.llm_settle` (system actor `llm_gateway`) or `eacp.llm_sweep` (`llm_sweeper`) commits or releases it, never above the amount. Counters, fold and escrow are unchanged, so the hard limit binds LLM spend as it binds tool spend. An unknown usage commits the full reservation.

## Consequences

- A budgeted tool can't run without an account. Activating a costed contract is a deliberate, two-person act.
- A denial is final, and the agent resubmits under a new idempotency key. Waiting for capacity was rejected: it would re-evaluate every waiting action on each sweep, loading the PDP, for little gain.
- An approved action can still be denied for budget at release. The approver's decision isn't burned, and the denial is journaled.
- Contention is per leaf. Different agents never contend; one agent's concurrent releases serialize on its leaf for the length of R1 after the reservation.

## Unresolved assumptions and conservative defaults

| Assumption | Default chosen |
|---|---|
| Deny or wait when the budget is short | Deny (`budget_exceeded`, terminal) |
| A costed tool without an account | Deny (`budget_account_missing`) |
| The cost of a no-effect failure | Released in full |
| The cost of an unknown outcome | Held until reconciled or resolved by a human |
| The actual cost | The estimate (no connector reports actuals yet) |
| Who changes limits | `admin`: one person lowers, two raise |
| A budget check before asking approvers | None; the release is authoritative. A pre-check could be added later as a hint |
| Soft budgets and alerts | Out of scope; §47 computes them asynchronously |

## Verification

- `internal/budget` (raw SQL as `eacp_app`, and the Go service):
  - the account, escrow and limit-change guards;
  - the two-person raise;
  - the fold;
  - an escrowed child can't exceed its parent;
  - a leaf reservation doesn't wait on a locked parent;
  - no deadlock under mixed limit changes and reservations.
- `internal/action`:
  - 100 concurrent releases against one leaf reserve exactly `floor(limit / cost)` and deny the rest, and never oversubscribe (inv. 3). The p50/p99 from the start of R1 through the final response read are logged as conservative upper bounds on release latency;
  - a budget denial doesn't consume a grant;
  - a raw-SQL release without a reservation is refused.
- `internal/worker`:
  - `SUCCEEDED` commits the reservation;
  - a no-effect `FAILED` releases it;
  - `UNKNOWN_OUTCOME` holds it through reconciliation;
  - human resolution commits or releases it;
  - an expired `QUEUED` action releases it.
- `internal/storage`: the RLS catalog reviews the new tables; cross-tenant accounts are refused.
- `test/demo`: step 13 fires 100 concurrent purchases against one THB budget. Exactly the affordable number execute, the ERP holds exactly that many POs, and the account ends at its limit.
