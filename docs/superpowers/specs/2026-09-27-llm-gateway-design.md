# Phase 25b — The LLM Gateway (design)

Date: 2026-09-27 · Status: approved by the owner in chat. Choices: govern and meter calls with no content inspection;
Anthropic Messages and OpenAI Chat Completions; pass-through SSE streaming; a new `llm-gateway` service that alone
holds provider keys; registry models plus a per-version model allowlist; approach B (an LLM-call ledger that follows
the shared core's rules); design section 1 approved; "yes, continue and dev until finish phase".
Scope: MASTER_PLAN §97 (LLM Gateway: a new ingress over the shared core, §3.1), ADR-001 §2, ADR-025 "not decided:
an LLM gateway that would reserve per call", ADR-016 (the `model` kill scope). ADR: **ADR-031 LLM gateway** (new).
Builds on ADR-001, ADR-002, ADR-003, ADR-005 §5a, ADR-012, ADR-016, ADR-018, ADR-019, ADR-025, ADR-026 and ADR-029.

## 1. Intent

Today an agent calls its LLM provider directly with a provider key, and EACP only learns the cost afterwards from
OTel spans or billing imports (ADR-025): nothing can stop an agent from using an unapproved model, overspending, or
calling a model an operator has killed. 25b puts EACP in the LLM path as an optional ingress. An agent points its
existing Anthropic or OpenAI SDK at the gateway, with its EACP agent key as the API key. The gateway decides every call
in PostgreSQL (capability, kill, budget), forwards it with a provider key only the gateway holds, and settles the
actual cost. The shared core does not change shape: the gateway is a client of the same registry, policy, kill,
budget, rate card and audit journal.

Success: an agent can call only the models its active version allows; a killed model or agent stops at the next call
and within seconds mid-stream; a hard budget can never be exceeded by LLM spend; every call is priced from the rate
card in PostgreSQL; no provider key reaches an agent, the API, a row, a journal or a log; and prompts and responses are
never stored.

## 2. Out of scope (recorded in ADR-031)

Content inspection or transformation of prompts and responses; Bedrock, Vertex AI and Azure-specific routes (Azure
OpenAI's `api-key` header, deployment paths); embeddings, the OpenAI Responses API, legacy completions, batch, files
and count-tokens endpoints; tools executed by the gateway; response caching; retries or fallback between models; models
in Governance-as-Code bundles; the operator console; the Helm chart (25b ships on compose; the chart gains the gateway
later); NATS kill wake-ups for the gateway (it polls); de-duplication between gateway usage and an agent's own OTel
spans for the same call.

## 3. Design

### 3.1 The service

- New binary `cmd/llm-gateway` (package `internal/llmgateway`), default address `:8083`, built on `internal/service`
  (fail-closed startup, `eacp_app` role check, `/healthz`, `/readyz`, draining with `EACP_SHUTDOWN_DELAY`).
- Routes, both `POST` with a JSON body of at most `EACP_LLM_MAX_REQUEST_BYTES` (default 4 MiB, at most 32 MiB):
  - `/v1/messages` — Anthropic Messages. The agent's EACP key arrives in `x-api-key` (the Anthropic SDK's header) or
    `Authorization: Bearer`.
  - `/v1/chat/completions` — OpenAI Chat Completions. The key arrives in `Authorization: Bearer`.
  - Anything else is 404.
- Configuration (`config.Options.AllowProviderSecrets`, set only by `llm-gateway`): `EACP_LLM_SECRETS_FILE` (required;
  the worker's connector-secrets manifest format and providers: static `value`, `oauth2`, Vault and SPIFFE; an `aws`
  entry is refused at load for this service), `EACP_LLM_KILL_POLL` (default 2s, 500ms–30s), `EACP_LLM_SWEEP_INTERVAL`
  (default 30s), `EACP_LLM_ID` (default the host name). Every other service refuses to start with
  `EACP_LLM_SECRETS_FILE` set, and `llm-gateway` refuses `EACP_CONNECTOR_SECRETS_FILE`.
- Only `llm-gateway` reaches providers. On compose it joins `agents` (agents reach it), `core` (PostgreSQL) and a new
  internal network `llm`, which holds the development provider `fakellm`. Agents have no route to `llm`.

### 3.2 Registry: models and the model allowlist (migration 00024)

- `eacp.llm_models`: `id`, `tenant_id`, `name` (the name agents send as `model`; `^[a-z0-9][a-z0-9._-]{0,62}$`, unique
  per tenant), `provider` (`anthropic` | `openai`), `base_url` (http(s), no credentials, query or fragment; the gateway
  appends `/v1/messages` or `/v1/chat/completions`), `upstream_model` (1–256 characters, no control characters),
  `secret_ref` (as for connectors), `max_output_tokens` (1–1 000 000), `timeout_ms` (1 000–3 600 000, default 600 000),
  `created_by`, `created_at`. Inserted by a `registry_editor` only; immutable; audited. RLS as in 00001.
- `eacp.agent_allowlists.model_ids uuid[] NOT NULL DEFAULT '{}'`. The allowlist guard sorts and de-duplicates them and
  requires every id to be a model of the tenant, exactly as for `tool_ids`; activating an allowlist is unchanged (a
  second person). `registry.Tx` gains the models; a bundle (ADR-026) never names models in 25b and carries the active
  allowlist's `model_ids` into any allowlist it creates.
- The kill switch gains scope `model` (a tenant model id). `eacp.set_kill` validates it as it does other targets.
  Actions carry no model, so the scope stops LLM calls only.
- Prices come from `eacp.model_prices` by (`provider`, `upstream_model`), the effective row at admission. Migration
  00024 adds `cache_write_per_mtok` (nullable) to the rate card, forward-only as before.

### 3.3 The ledger: `eacp.llm_calls`

One row per request that authenticated as an agent:
`id`, `tenant_id`, `agent_id`, `agent_version_id`, `model_id` (NULL if the name was unknown), `model_name`,
`subject_principal_id`, `stream`, `trace_id`, `request_bytes`, `max_output_tokens`, the decision (`decision_id`,
`policy_bundle_id`, `policy_version`, `verdict`, `input_digest`), `price_id`, `reservation_id`, `state`
(`DENIED` | `ADMITTED` | `SETTLED`), `denial` (a code), `outcome` (`succeeded` | `provider_error` | `usage_unknown` |
`killed` | `abandoned`), `provider_status`, `input_tokens`, `cache_read_tokens`, `cache_write_tokens`,
`output_tokens`, `cost_amount`, `cost_unit`, `committed_amount`, `gateway_id`, `created_at`, `deadline`,
`settled_at`. Never any prompt, response, header or key.

Triggers guard it: rows are inserted only through `eacp.llm_admit` by the authenticated agent (`storage.SetAgent`),
move `ADMITTED → SETTLED` once through `eacp.llm_settle` (system actor `llm_gateway`) or `eacp.llm_sweep` (system
actor `llm_sweeper`), and are never deleted. PostgreSQL computes the cost. Audit events `llm.denied`, `llm.admitted`
and `llm.settled` are appended in the same transactions, last.

### 3.4 Budget reservations for LLM calls

`eacp.budget_reservations` gains `llm_call_id`; `action_id` and `contract_id` become nullable, with exactly one of
`action_id` (with its contract) or `llm_call_id` set. The guard keeps every action rule and adds the LLM branch: a
reservation for a call is inserted only by that call's agent inside `llm_admit`, on the agent's leaf account in the
price's unit, for exactly the estimate; it is committed or released only by `llm_settle` or `llm_sweep`, with
`committed_amount ≤ amount`. The account counters, fold and audit triggers are unchanged. Hard budgets therefore bind
LLM spend exactly as tool spend (ADR-012).

The estimate, computed in PostgreSQL: `request_bytes × max(input, cached input, cache write price) / 10⁶ +
max_output_tokens × output price / 10⁶` (per-million-token prices). Request bytes bound the input tokens from above:
every BPE token of text is at least one byte, and an image's tokens are far fewer than its encoded bytes.

### 3.5 The request flow

1. **Authenticate** the key with `identity.Authenticate`; it must be an agent key (401 otherwise). Parse the body as
   one JSON object without duplicate keys; read `model`, `stream`, the output cap and (OpenAI) `n`.
2. **Validate** the shape (400 `invalid_request`): `model` a string; Anthropic `max_tokens` an integer ≥ 1; OpenAI
   `n` absent or 1, `max_completion_tokens`/`max_tokens` (at most one) an integer ≥ 1.
3. **Decide** with the PDP, no transaction open (ADR-005 §5a): the binding is tenant, agent, version, subject (the
   `EACP-Subject` header, else `agent:<agent id>`), operation `llm.generate`, target and resource the model name, tool
   `llm:<provider>`, schema version `1`, and payload `{"model", "stream", "max_output_tokens", "request_bytes"}` —
   never content. `allow` and `warn` proceed; `deny` denies with the policy's reason; `escalate` (no synchronous
   approval) and `transform` (the gateway never rewrites content) deny as `approval_unsupported` and
   `transform_unsupported`. An unavailable PDP is 503 (retryable) and records nothing.
4. **Admit** in one transaction, `SELECT eacp.llm_admit(...)` as the agent. In lock order: agent version and model
   `FOR SHARE` → the tenant kill advisory lock → the budget leaf → the call row and audit. Denials, in order:
   `agent_version_not_active`, `subject_invalid` (an `EACP-Subject` that is not an enabled human principal's
   `subject`, resolved as `eacp.actions` resolves it), `unknown_model`, `wrong_provider` (a model of the other shape), `model_not_in_allowlist`,
   the release denial (`eacp.release_denial` with the subject principal; ADR-018), `policy_denied` / the PDP code,
   `killed` (scopes tenant, team — the agent's owner group — agent, agent version, model), `max_tokens_over_cap`,
   `model_unpriced`, `budget_exceeded`. A denial inserts a `DENIED` row and its audit event and returns the code; the
   agent receives 403 with a provider-shaped error. An admitted call gets its reservation (if the agent has a leaf
   account in the price's unit; otherwise it runs unreserved and its cost is recorded) and `deadline = now() +
   timeout_ms + 60 s`.
5. **Forward** to `base_url` + the route with the model's credential (`SecretStore.Credential` with the model's
   timeout plus `worker.CredentialSkew`): Anthropic `x-api-key: <secret>` and the agent's `anthropic-version` and
   `anthropic-beta` headers; OpenAI `Authorization: Bearer <secret>`. The body is the agent's object with `model`
   replaced by `upstream_model`; for OpenAI without an output cap the gateway sets `max_completion_tokens` to the
   model's cap, and for an OpenAI stream it sets `stream_options.include_usage = true`. No other header is forwarded
   except `Content-Type`, `Accept` and W3C trace context. No redirects, no proxy, the model's timeout, and at most
   16 MiB for a non-streamed body.
6. **Relay** the provider's status, `Content-Type`, `request-id`/`x-request-id` and body. A stream is relayed event by
   event (at most 1 MiB per event) and flushed. While relaying, the gateway reads usage without keeping content:
   Anthropic `message_start.message.usage` then each `message_delta.usage` (cumulative); OpenAI the final chunk's
   `usage`. Every `EACP_LLM_KILL_POLL` it compares the tenant kill epoch and, if it moved, re-checks the call's scopes;
   a killed call is cut (upstream cancelled, client stream closed).
7. **Settle** in one transaction as `llm_gateway`, `SELECT eacp.llm_settle(call, outcome, status, tokens...)`:
   - `succeeded` — a 2xx with usage: PostgreSQL prices it from the pinned price and commits `min(cost, reservation)`
     (a cost above the reservation is recorded in full on the call and in FinOps; the budget commits the reservation).
   - `provider_error` — a non-2xx with no usage: released, cost 0.
   - `usage_unknown` — a 2xx whose usage is missing or unreadable, a stream cut early, the client gone, or the timeout:
     the full reservation is committed; cost NULL.
   - `killed` — cut by a kill: as `usage_unknown`.
   A known usage also writes `eacp.usage_records` (source `gateway`, `llm_call_id`), so FinOps dashboards, chargeback
   and soft limits see gateway spend.
8. **Sweep**: every `EACP_LLM_SWEEP_INTERVAL`, `eacp.llm_sweep()` as `llm_sweeper` settles calls still `ADMITTED`
   after their deadline as `abandoned`, committing the full reservation. It moves rows with `FOR UPDATE SKIP LOCKED`
   and compare-and-set, so replicas share it without a leader (ADR-029).

### 3.6 API and CLI

- `POST /v1/llm-models` (`registry_editor`), `GET /v1/llm-models` (readers). Allowlists accept `"models": [names]`
  beside `"tools"`, and GET shows them.
- `GET /v1/llm-calls?agent=&model=&state=&from=&to=&limit=` and `GET /v1/llm-calls/{id}` (`admin`, `operator`,
  `auditor`).
- `POST /v1/kills` accepts scope `model`.
- `eacpctl llm-model register|list`, `eacpctl llm-calls list`, and `--models` on the allowlist command.

### 3.7 Development provider and demo

- `internal/fakellm` + `cmd/fakellm` (`:8093`, on `llm` only): speaks both shapes, requires its own key (only the
  SHA-256 is kept), answers with fixed text and exact usage, and picks a scenario from the last user message:
  `stream`, `error_429`, `error_500`, `no_usage`, `cut` (drops the stream after two events), `slow` (an event a second
  for 60 s). It logs method, model, stream flag and token counts to a durable JSONL audit, never content.
- `TestLLMGatewayDemo` (`DEMO=L`, compose), tenant `…00ac` "Hooli-AI" (a new tenant id):
  - L0 bootstrap, policy allowing `llm.generate`.
  - L1 register models `sonnet` (anthropic) and `gpt` (openai) against `fakellm`, prices, an agent with a USD leaf
    account of 0.05, an allowlist with `sonnet` only.
  - L2 a Messages call and a streamed Messages call succeed; the ledger shows exact tokens and PostgreSQL's cost; the
    budget committed equals the cost.
  - L3 `gpt` is denied `model_not_in_allowlist`; an `error_429` is `provider_error` and costs nothing.
  - L4 a large `max_tokens` is denied `budget_exceeded`.
  - L5 a `slow` stream is cut within seconds when an operator kills model `sonnet`; the next call is denied `killed`.
  - L6 no provider key or agent key in API responses, logs or a database dump.

## 4. Error handling and invariants

- Fail closed: PDP down → 503; PostgreSQL down → 503; missing credential → 503 `credential_unavailable` and the call is
  settled `provider_error` with nothing sent; nothing reaches a provider without an `ADMITTED` row.
- At most one provider request per admitted call; the gateway never retries (the SDK may retry a 5xx, which is a new
  call).
- Invariant 11 extends: no provider key in any row, journal, outbox or log, and no agent holds one.
- Hard budget (invariant 3): committed LLM spend never exceeds a reservation, and reservations never exceed the limit.

## 5. Unresolved assumptions (conservative choices, recorded in ADR-031)

| Assumption | Choice |
|---|---|
| Providers do not bill a request answered with an error status | Released at cost 0; cost if wrong: undercounted failed calls |
| Usage of a cut, killed, timed-out or abandoned call | Unknown: commit the full reservation, record no usage row |
| Tokens of the input | Bounded by request bytes for the reservation; the provider's usage for the cost |
| Cache writes without a cache-write price | The cost is NULL (unpriced, ADR-025) and the full reservation is committed |
| OpenAI `cached_tokens` | Part of `prompt_tokens`: input = prompt − cached, cache read = cached |
| A canary candidate's LLM calls | Need `EACP-Subject` inside the cohort; otherwise denied `canary_cohort` |
| `anthropic-beta` features | Forwarded; usage still bounds the cost |

## 6. Testing

- Raw SQL as `eacp_app` (`internal/llm/schema_test.go` or the registry package): models guard, allowlist model ids,
  kill scope `model`, every `llm_admit` denial in order, reservation rules, settle outcomes and costs, the sweeper,
  RLS; new tables and functions in `rls_catalog_test.go`.
- `internal/llmgateway`: validation, header handling, body rewriting, the SSE relay and usage parsers for both shapes,
  kill cuts, timeouts, limits, no redirects, secrets never leave the provider host; interoperability with the official
  `github.com/anthropics/anthropic-sdk-go` v1.75.0 and `github.com/openai/openai-go/v3` v3.66.0 clients (test-only)
  pointed at the gateway, streamed and not.
- Integration through PostgreSQL, the gateway and `fakellm`; a secret canary search (invariant 11).
- `test/security`: the agent cannot reach `fakellm`; only `llm-gateway` mounts provider secrets; the API and worker
  refuse `EACP_LLM_SECRETS_FILE`. `test/demo` `TestLLMGatewayDemo`.
