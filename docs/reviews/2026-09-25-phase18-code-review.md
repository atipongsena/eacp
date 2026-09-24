# Phase 18 Agent FinOps Review

Date: 2026-09-25
Scope: Slice C Phase 18 (MASTER_PLAN §3.1, §45–§48, §55 and §92; ADR-025 Rev 1.0; ADR-012; §103 invariants 3, 8 and 17).
Review: a self-review against ADR-025, ADR-012 and the OTLP and GenAI upstream notes, plus mutation checks of the FinOps tests.

## Invariants stated before the code

1. **Observation, never authority.** Nothing FinOps records or computes blocks, releases or changes an action. Only ADR-012 hard limits block (invariant 3 is untouched).
2. **Usage is the key's.** An OTel usage record's agent and version come from the agent key that sent it, never from the span. No principal, system component or mixed transaction can record one.
3. **PostgreSQL prices.** Cost is computed by a trigger from the price in effect at `observed_at`. It rounds up, never undercharges on cached tokens, and is NULL (never zero) without a price. A caller's cost is ignored.
4. **No rewriting history.** Usage and prices are insert-only. A price takes effect no earlier than its insertion. A span must be observed within the last 7 days, and a retried span is not counted twice.
5. **No double counting.** Agent-level spans are not billable. Per agent, unit and UTC day, effective LLM spend is the greater of the reported and billed sums.
6. **Alerts are evidence.** Only the `finops` system actor raises an alert, once per kind, subject, unit and period. An operator or admin acknowledges it once, with a reason. Both are journaled.
7. **Isolation.** All four tables are tenant rows under forced RLS. The one cross-tenant path is the reviewed `eacp.finops_tenants()` hint, which returns tenant ids only.

## Implemented

- **ADR-025** (new; Accepted Rev 1.0) and `research/REFERENCES.md` notes for OTLP/HTTP JSON (spec 1.11.0) and the GenAI conventions (commit `8ffdf568`).
- **Migration 00018** adds four tables, `eacp.usage_cost`, the spend functions and the evaluator.
  - Tables: `eacp.model_prices`, `eacp.usage_records`, `eacp.budget_soft_limits` and `eacp.finops_alerts`, each with its guard trigger.
  - Journaling: `finops_row_audit` journals prices, billing lines and soft limits with the row's own reason; `finops_alerts_audit` journals alerts.
  - `eacp.usage_cost` prices a record.
  - Spend functions: `finops_llm_daily`, `finops_agent_spend` and `finops_account_spend` (the account one uses a recursive subtree).
  - `eacp.finops_evaluate()` raises the alerts.
  - The SECURITY DEFINER hint `eacp.finops_tenants()`.
- **`internal/finops`** covers the OTLP JSON parser and the service. The service handles prices, billing import, span ingest with a savepoint per span, usage, soft limits, alerts and acknowledgement, and the evaluator (`Evaluate`, `EvaluateAll`, `Run`). It also has chargeback by agent, team or account, and the dashboard.
- **`internal/api`**:
  - `POST /v1/agent/otlp/v1/traces`: agent key; JSON only (`415` otherwise); gzip; 4 MiB before and after decompression; `partialSuccess`.
  - Nine routes under `/v1/finops/`.
- **`eacpctl finops`** and **`EACP_FINOPS_INTERVAL`** (default 1m, 10s to 1h). controlplane-api runs the evaluator.

## Design decisions made during the phase

- **Seeding a past price.** A forward-only rate card cannot price usage observed before it existed. Only the schema owner, the bootstrap path every registry table already allows, may backdate `effective_from`. An admin cannot.
- **Response model first.** The span's `gen_ai.response.model` is the model that served the call, so it is priced before `gen_ai.request.model`.
- **Missing provider.** A span without a provider is recorded under `unknown` rather than rejected, so its usage stays visible (and alerted as unpriced) rather than lost.
- **Spans are not journaled one by one.** They are high-volume, key-bound, immutable and idempotent observations. Billing lines, which an admin asserts, are journaled.
- **Reason in the journal.** The generic `audit_row_change` records the table operation as the reason. The FinOps rows journal their own reason, so an operator reading the chain sees why a price or soft limit changed.

## Findings from the self-review (fixed)

1. **Unpriced test fixtures.** The first schema run priced nothing: every fixture price started at its insertion, after the spans it should price. This led to the owner-seeding decision above. `TestPriceInEffectAtObservationAndNoRetroactivePrices` now shows both sides: a span a minute before an admin's price keeps the old price, and a span after it takes the new one.
2. **The journal lacked the reason.** `TestSoftLimitsAreAdminOnlyAndJournaled` claimed journaling but did not read the journal. It now checks each entry's action, actor and reason, which drove `finops_row_audit`.
3. **Mixed actors.** No test bound a principal (or a system component) together with an agent key. The guard's checks for those were therefore unpinned. `TestOnlyTheRightActorMayRecordUsage` now covers both.
4. **The anomaly factor was unpinned.** The only anomaly case had a tiny baseline, so any factor passed. The test now has an agent at exactly 3× its mean (flagged) and one at 2.5× (not flagged).
5. **Default chargeback period.** It ends at request time, so a span observed a second in the future is outside it. This is by design (the window accepts clocks up to 5 minutes ahead), and the API test observes at the current time.
6. **Four more gaps, from the first mutation run** (33 of 40 killed):
   - Rounding up was indistinguishable from rounding for a half: the test now adds 0.4 µ (rounds up to 0.000001, never down to zero).
   - Billing with a principal and an agent bound together was untested.
   - Held tool spend did not exclude settled reservations: the chargeback world now releases a second reservation by cancelling its action.
   - Zero spend was never evaluated: a free (zero-price) agent with a history must not be flagged.

## Mutation checks

Each mutation of migration 00018 was applied alone, and `internal/finops` was run. After the fixes above, 37 of 40 were killed:

- **Usage guard:**
  - the principal, system and retired-version checks;
  - both window bounds;
  - agent from the key;
  - the billable list (both ways);
  - clearing a caller's cost.
- **Pricing:**
  - rounding up;
  - the cache rule;
  - the price in effect and the latest price;
  - forward-only prices;
  - admin only.
- **Billing:** admin only, agent refused, and cost required.
- **Soft limits:** admin only, and `set_by`.
- **Alerts:**
  - raised only by `finops`;
  - acknowledged once, by an operator or admin;
  - the alert and row journals, with their reasons.
- **Spend:**
  - greater of the two sources;
  - billable only;
  - held means ACTIVE;
  - the account subtree.
- **Evaluator:**
  - both soft-limit ratios;
  - the anomaly factor (both ways);
  - the baseline age;
  - zero spend;
  - unpriced usage billable only;
  - the tenant hint.

Survivors (equivalent, defense in depth):

- **"An acknowledgement changes nothing else"** and **"`acknowledged_by` is the actor".** `eacp_app` may update only `ack_reason` and `acknowledged_at` (column grants). No application write can reach the other columns, and the test's attempt fails with `42501` either way.
- **The evaluator's actor check.** Every alert it could write passes the `finops_alerts` insert guard, which refuses any actor but `finops` (`42501`), so a wrong caller raises nothing. The function check fails earlier and more clearly.

## Verification

- `go vet ./...` and `go test -race -count=1 ./...` with `EACP_TEST_ADMIN_DSN`: every package passes, PostgreSQL tests included.
- The compose stack migrates to 00018. `/readyz` is ok, and the evaluator runs every minute without errors.
- `EACP_COMPOSE_TEST=1 go test ./test/security/` passes. `scripts/demo.sh` passes (82 s).

## Not done (by design)

- **Actual tool cost.** A connector still commits the estimate (ADR-012 §5).
- **Run budgets.** Actions have no authenticated run id.
- **Forecasts.**
- **A NATS alert signal.**
- **A `finops` role.** `admin` manages prices, billing and soft limits.
- **An LLM gateway.**
