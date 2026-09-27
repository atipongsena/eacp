# ADR-025: Agent FinOps: cost ingest, chargeback, soft budgets and alerts

Status: Accepted (Rev 1.0, 2026-09-25). Scope: Phase 18 (MASTER_PLAN §3.1, §45–§48, §55 and §92).
Related: ADR-012 (hard budget reservation), ADR-003 (registry, agent keys), ADR-014 (signals), ADR-024 (fleet view).

## Context

MASTER_PLAN §92 asks for:
- cost telemetry ingest;
- hierarchical budgets;
- chargeback;
- a spend dashboard;
- anomaly alerts.

EACP is not in the LLM path (§3.1), so LLM cost can only be ingested (§45): OpenTelemetry GenAI spans from agent runtimes, and provider billing exports.

Tool actions already have hard, reserved budgets in an escrowed account tree (ADR-012). Nothing yet observes LLM usage, prices it, aggregates spend, or warns before a hard limit bites.

Agents are untrusted (§67). An agent's usage report is an observation, not proof of what it spent.

**Upstream verified** (`research/REFERENCES.md`):
- **OTLP/HTTP JSON** (OTLP specification):
  - path `/v1/traces`, `Content-Type: application/json`;
  - lowerCamelCase keys, 64-bit integers as decimal strings, hex trace and span ids;
  - `200` with an unset `partialSuccess`, or a `partialSuccess` carrying `rejectedSpans` and `errorMessage`.
- **GenAI semantic conventions**, `open-telemetry/semantic-conventions-genai` at commit `8ffdf568`, status **Development**:
  - attributes `gen_ai.operation.name`, `gen_ai.provider.name`, `gen_ai.request.model`, `gen_ai.response.model`, `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens` and `gen_ai.usage.cache_read.input_tokens`;
  - `input_tokens` "SHOULD include all types of input tokens, including cached tokens";
  - agent-level operations (`invoke_agent`, …) carry usage attributes too.

## Decision

1. **Two usage sources, both observations.** `eacp.usage_records` is an insert-only tenant table with two sources:
   - **`otel`:** an agent runtime exports OTLP/HTTP JSON to `POST /v1/agent/otlp/v1/traces` with its agent key.
     - The record takes the agent and version from the key, never from span attributes. An agent reports only its own usage.
     - Only spans with `gen_ai.operation.name` and at least one `gen_ai.usage.*_tokens` attribute are kept. Others are accepted and ignored.
     - A malformed usage span is rejected and counted in `partialSuccess`.
     - `(trace_id, span_id)` is unique, so an exporter's retry never counts twice.
     - `observed_at` (the span end) must be within the last 7 days and at most 5 minutes ahead, so an agent cannot rewrite old periods.
     - Protobuf encoding is refused with `415` (the spec makes accepting both a SHOULD). `gzip` is accepted (a MUST). An export is at most 4 MiB before and after decompression, and at most 1000 usage spans.
     - The model is `gen_ai.response.model` (the model that served the call), else `gen_ai.request.model`. A span without `gen_ai.provider.name` is recorded under provider `unknown` and stays unpriced unless an admin prices it.
     - Each span is its own savepoint. A span PostgreSQL refuses (for example, outside the window) is counted in `partialSuccess` and the rest are kept. A key whose version is `RETIRED` or `REVOKED` records nothing (`403`). A `SUSPENDED` version still reports, since its calls in flight still cost.
     - OTel records are not journaled one by one. They are high-volume observations, bound to the key, immutable and idempotent.
   - **`provider_billing`:** an `admin` imports billing lines `{external_id, agent_id, provider, model, tokens, cost, unit, observed_at}`. `external_id` is unique, and the amount is the provider's.
     - An import (at most 10 000 lines) records all its lines or none. A line whose `external_id` was already imported is counted as a duplicate: the first import stands.
     - `observed_at` must be within the last 400 days and at most 5 minutes ahead.
     - Each line is journaled with the admin and `billing import <external_id>`.
2. **Billable operations.** Only inference operations are billable: `chat`, `generate_content`, `text_completion` and `embeddings`. Agent-level spans are kept with `billable = false` and shown as unbilled tokens, never summed. Summing an `invoke_agent` total with its child calls would double-count.
3. **Prices live in PostgreSQL.**
   - `eacp.model_prices` is an insert-only rate card: provider, model, unit, and price per million input, cached-input and output tokens. An `admin` adds it, and it is journaled.
   - A price takes effect no earlier than its insertion, so no past period is repriced. Only the schema owner (bootstrap, as for every registry table) may seed a rate card with an earlier `effective_from`.
   - Prices and soft limits are journaled with their actor and the row's own reason.
   - A trigger computes each OTel record's cost, never Go. It uses the price in effect at `observed_at`: `(input − cache_read) × input + cache_read × cached + output × output`, divided by 10⁶ and **rounded up** to six decimals.
   - `cache_read > input` is only consistent if the instrumentation's `input_tokens` excludes cached tokens, so the record is then priced as `input × input + cache_read × cached`. That is the higher of the two readings.
   - With no price, the cost is NULL (**unpriced**). It is shown and alerted, never counted as zero.
4. **Effective LLM spend.** Both sources may describe the same calls. So per agent, unit and UTC day, effective LLM spend is the **greater** of the reported and billed sums. It never double-counts and never takes the smaller.
5. **Tool spend** comes from ADR-012 reservations:
   - **committed:** `COMMITTED` amounts, by `settled_at`;
   - **held:** `ACTIVE` reservations, including unknown outcomes, by `created_at`.
   Chargeback reports held spend separately. Soft budgets count it: until an outcome is known, spend is assumed.
6. **Hierarchy and chargeback.** The ADR-012 account tree is the cost-centre hierarchy (organization → … → agent leaf, per unit).
   - `GET /v1/finops/chargeback` groups a period's spend by agent, team (the owning group or principal) or account.
   - Account grouping rolls each agent leaf up to every ancestor with a recursive CTE, so account groups overlap: each ancestor includes its subtree. Spend in a unit where the agent has no leaf, and the tokens of an agent with no leaf at all, appear as `unassigned`.
   - Tokens are the reported (OTel) tokens. A billing line's tokens are shown only with its model line, so the two sources never add up.
   - Each group shows committed and held tool spend, reported, billed and effective LLM spend, tokens, unpriced and unbilled tokens, and lines per connector and per provider/model.
7. **Soft budgets are observations.**
   - `eacp.budget_soft_limits` gives an account a monthly (UTC) soft limit. One `admin` sets or changes it, with a reason, and it is journaled.
   - It never blocks anything; only ADR-012 hard limits do. Setting the limit to `null` clears it and keeps the row, so the journal shows the change.
   - Month-to-date spend covers every agent leaf of the account's subtree in its unit: committed plus held tool spend, plus effective LLM spend.
8. **Alerts** are rows of `eacp.finops_alerts`. The `finops` system actor (`storage.SetSystem`) creates them through `eacp.finops_evaluate()`. Each is unique per kind, subject and period, so re-evaluation never repeats one.

   | Kind | Subject | When |
   |---|---|---|
   | `soft_limit_warning` | account | month-to-date ≥ 80 % of the soft limit |
   | `soft_limit_exceeded` | account | month-to-date ≥ 100 % |
   | `spend_anomaly` | agent, unit | Last complete hour ≥ 3 × the mean hourly spend of the previous 168 hours, above zero. The agent must have spend older than 24 hours, so a new agent has no baseline to break. |
   | `unpriced_usage` | agent | billable usage with no price today |

   - Anomaly spend is reported LLM cost plus reservations (not released) made in the hour. Billing exports are usually daily, so the hourly rule doesn't use them.
   - An `operator` or `admin` acknowledges an alert once, with a reason.
   - Alert creation and acknowledgement are journaled.
   - The controlplane-api runs the evaluator every `EACP_FINOPS_INTERVAL` (default 1 minute), per tenant listed by the reviewed `SECURITY DEFINER` hint `eacp.finops_tenants()`.
9. **Dashboard.** `GET /v1/finops/dashboard` returns, per unit:
   - spend today and month to date;
   - hard blocks today (actions `DENIED` for `budget_exceeded`, `budget_account_missing` or `budget_cost_invalid`);
   - open alerts by kind;
   - unpriced tokens today;
   - the top agents month to date.

   Readers are `admin`, `operator` and `auditor`, as for `GET /v1/budgets`.
10. **API.** Agent key: `POST /v1/agent/otlp/v1/traces`. Readers: `GET /v1/finops/dashboard`, `chargeback?group_by=agent|team|account&from&to` (default: the UTC month to date, at most 400 days), `usage`, `prices`, `soft-limits`, `alerts?open=true`. `admin`: `POST /v1/finops/prices`, `POST /v1/finops/billing` and `PUT /v1/finops/soft-limits/{account}`. `operator` or `admin`: `POST /v1/finops/alerts/{id}/ack`. `eacpctl finops …` wraps them all.

## Not decided here

- **Actual tool cost.** A connector still commits the estimate (ADR-012 §5). Reporting an actual needs a connector contract change.
- **Run-level budgets.** Actions have no authenticated run identity (ADR-016).
- **Forecasts.**
- **A NATS signal for alerts.** The API shows alerts. A `finops.alert` outbox topic would follow ADR-014 later.
- **A new `finops` role.** `admin` manages prices, billing imports and soft limits.
- **An LLM gateway** that would reserve per call (§45). Decided by ADR-031 (Phase 25b).

## Amendment (Phase 25b, ADR-031)

`eacp.usage_records` accepts source `gateway`, one row per settled gateway call with known usage (`llm_call_id`, unique), priced in PostgreSQL from the price pinned at admission. The dashboard's reported spend, chargeback, spend anomalies and release metrics count `otel` and `gateway` together, so an agent that also emits OTel spans for a gateway call is over-counted in reported spend, never under-counted. The gateway's reservations are hard budgets (ADR-012), not soft limits; FinOps still never blocks.

## Verification

- `internal/finops/schema_test.go`, raw SQL as `eacp_app`:
  - agent binding and time window;
  - idempotent spans;
  - cost computed in PostgreSQL with rounding up, the cache rule and unpriced records;
  - insert-only tables;
  - forward-only prices;
  - role checks;
  - alert creation only by `finops`, and a single acknowledgement;
  - the evaluator's thresholds, anomaly baseline and idempotence;
  - audit.
- `internal/finops/*_test.go`:
  - the OTLP JSON parser (int64 strings and numbers, partial success, `415`);
  - chargeback by agent, team and account rollup;
  - effective spend as the greater source;
  - the dashboard;
  - tenant isolation.
- API, CLI, and the RLS catalog of the new tables and functions.
