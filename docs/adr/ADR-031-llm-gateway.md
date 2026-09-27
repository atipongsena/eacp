# ADR-031: The LLM gateway

Status: Accepted (Rev 1.0, 2026-09-27). Scope: Phase 25b (MASTER_PLAN §97, §45).
Related: ADR-001 §2 (ingress over the shared core; who holds secrets), ADR-002 (the PDP), ADR-003 (registry, allowlists), ADR-005 §5a (no transaction open across the PDP), ADR-012 (hard budgets), ADR-016 (kill states and epochs), ADR-018 (canary cohorts), ADR-019 (credential providers), ADR-025 (FinOps, the rate card), ADR-026 (Governance-as-Code), ADR-029 (replicas without a leader).

## Context

Before 25b an agent called its LLM provider directly with a provider key, and EACP learned the cost afterwards from OTel spans or billing imports (ADR-025). Nothing stopped an agent from using an unapproved model, overspending, or calling a model an operator had killed. ADR-025 left "an LLM gateway that would reserve per call" undecided.

The owner chose (2026-09-27):

- govern and meter calls, with no content inspection;
- the Anthropic Messages and OpenAI Chat Completions wire shapes, streaming passed through;
- a new service that alone holds provider keys;
- registry models plus a per-version model allowlist;
- a ledger of LLM calls that follows the shared core's rules.

Wire facts were verified against the official Go SDKs, `github.com/anthropics/anthropic-sdk-go` v1.75.0 and `github.com/openai/openai-go/v3` v3.66.0 (`research/REFERENCES.md`), which the interoperability tests use unchanged.

## Decision

### 1. A new ingress, `llm-gateway`

- A new binary, `cmd/llm-gateway` (`internal/llmgateway`, default `:8083`). It is built on `internal/service`: fail-closed startup, the `eacp_app` role check, `/healthz`, `/readyz` and draining.
- It serves two routes, both `POST`, each with one JSON object of at most `EACP_LLM_MAX_REQUEST_BYTES` (default 4 MiB, at most 32 MiB):
  - `/v1/messages` (Anthropic). The key comes in `x-api-key` or a Bearer header.
  - `/v1/chat/completions` (OpenAI). The key comes in a Bearer header.
  - Anything else is 404.
- An agent points its unmodified SDK at the gateway and uses its **EACP agent key** as the API key. The agent never holds a provider key.
- **ADR-001 §2 amendment.** The gateway is the second process that holds secrets, and it holds only LLM provider credentials.
  - `config.Options.AllowProviderSecrets` marks it. It requires `EACP_LLM_SECRETS_FILE` and refuses `EACP_CONNECTOR_SECRETS_FILE`; every other service refuses `EACP_LLM_SECRETS_FILE`.
  - The file uses the worker's manifest format and providers: a static value, OAuth 2.0, Vault or SPIFFE.
  - An `aws` entry fails the load (`worker.RefuseSigningCredentials()`), and any signing credential is refused again at request time. The gateway sends Bearer or API-key credentials only.
  - Every value, and every minted token, joins the redaction set.
- On compose the gateway joins `agents`, `core` and a new internal network `llm`, which holds the development provider `fakellm`. Agents have no route to `llm`.
  - The gateway uses the local PDP there. The AGT sidecar stays reachable from the API alone. A deployment that gives the gateway the sidecar needs the `pdp` network and a client certificate.

### 2. Registry: models and model allowlists

- **`eacp.llm_models`.** A tenant's declared models: `name` (what agents send), `provider` (`anthropic` | `openai`), `base_url`, `upstream_model`, `secret_ref`, `max_output_tokens` and `timeout_ms`.
  - Only a `registry_editor` inserts one. A model is immutable and journaled (`llm_models.insert`), with RLS as in 00001.
  - Because the table grants no UPDATE, `llm_admit` reads it without `FOR SHARE`.
- **`eacp.agent_allowlists.model_ids`.** Guarded like `tool_ids`: sorted, de-duplicated and tenant-local. Activating an allowlist still needs a second person.
  - The API allowlist body takes `"models": [names]`.
  - A bundle (ADR-026) never names models in 25b. It carries the active allowlist's models into any allowlist it proposes.
- **Kill scope `model`** (ADR-016). `eacp.set_kill` validates it against the tenant's models. Actions carry no model, so the scope stops LLM calls only.
  - The platform-authority scopes (global, run) remain unsupported.
- **Prices** come from `eacp.model_prices` by (`provider`, `upstream_model`), the effective row at admission. Migration 00024 adds a nullable `cache_write_per_mtok`; the rate card stays forward-only.

### 3. The ledger, `eacp.llm_calls`

- One row per request that authenticated as an agent. It records the agent and version, the model, the subject, stream, trace id, request bytes, the output cap, and the decision (id, bundle, version, verdict, input digest).
- It also records the pinned `price_id`, the state (`DENIED` | `ADMITTED` | `SETTLED`), a denial code, the outcome, the provider status, the tokens (input, cache read, cache write, output), the cost, the committed amount, the gateway id and the deadline.
- It **never** holds a prompt, a response, a header or a key.
- `eacp_app` cannot write the ledger: `llm_admit`, `llm_settle` and `llm_sweep` are SECURITY DEFINER functions (search path pinned, every statement tenant-filtered), and the ledger, LLM reservation and gateway usage guards require `eacp.llm_ledger_context(call)`: the transaction-local gate `eacp.llm_call` names the row **and** the statement runs as the schema owner. The setting alone, which any transaction can set, opens nothing (`TestTheLedgerCannotBeWrittenBySettingItsGate`).
  - The agent inserts through `eacp.llm_admit` (`storage.SetAgent`).
  - The system actor `llm_gateway` settles once through `eacp.llm_settle`.
  - The system actor `llm_sweeper` settles `abandoned`, only after the deadline, through `eacp.llm_sweep`.
  - Nothing deletes a row. `eacp.actor_context()` is not widened, and the audit trigger names the two system actors itself.
- Audit events `llm.denied`, `llm.admitted` and `llm.settled` are appended in the same transaction, last.
- The reservation points at the call (`budget_reservations.llm_call_id`, unique). There is no `reservation_id` on the call, which avoids a circular foreign key.

### 4. Hard budgets for LLM calls (ADR-012 amendment)

- `eacp.budget_reservations` gains `llm_call_id`; `action_id` and `contract_id` become nullable, and a CHECK requires exactly one subject.
- The guard keeps every action rule. Its LLM branch runs before `actor_context()`: a call's reservation is inserted only inside `llm_admit`, on the agent's leaf account in the price's unit, for exactly the estimate. It is committed or released only by `llm_settle` or `llm_sweep`, and the committed amount never exceeds the amount reserved.
- Account counters, fold and escrow are unchanged.
- The estimate is computed in PostgreSQL:

  ```text
  request_bytes × max(input, cached input, cache write price) / 10⁶
    + max_output_tokens × output price / 10⁶
  ```

  Request bytes bound input tokens from above **only for inline input**. Two rules keep the hard limit binding:
  - **Unbounded inputs are refused** (400 `unbounded_input`, before the PDP, nothing recorded). The check reads the request's structure, never its content: message and system content blocks and the tools' types, never tool schemas or arguments.
    - Anthropic: a content `source` other than `base64`, `text` or `content` (URLs, files), any `file_id`, a tool whose `type` is not `custom` (server tools and Anthropic-defined tools), `mcp_servers` and `container`.
    - OpenAI: an `image_url` that is not a `data:` URL, a `file` by `file_id`, an earlier answer's `audio`, a tool that is not `function` or `custom`, `web_search_options`, `audio`, `prediction`, `modalities` other than text, and a `service_tier` other than `auto`, `default` or `flex`.
    - Test: `TestUnboundedInputsAreRefused`.
  - **An overrun counts against the next admission.** A call can still cost more than its reservation: a provider-side price above the rate card (long-context pricing, a project's default tier), or fixed per-request tokens on a tiny request. The reservation commits only what it holds, and the call's `cost_amount − committed_amount` is real spend. `llm_admit` adds every such excess of the agent in the price's unit to the estimate it checks against the room. One overrun can exceed the limit once; after it the agent spends nothing more until an admin raises the limit (two-person). Test: `TestAnOverrunCountsAgainstTheNextAdmission`.
- An agent with no leaf account in the price's unit runs unreserved, and its cost is still recorded.

### 5. The request flow

1. **Authenticate** with `identity.Authenticate`; the key must be an agent key, or the gateway answers 401.
2. **Parse and validate.** The body must be exactly one JSON object: no duplicate keys, depth ≤ 128, valid UTF-8, and a `model` of 1–256 characters.
   - Anthropic: an integer `max_tokens` ≥ 1.
   - OpenAI: `n` absent or 1; at most one of `max_completion_tokens`/`max_tokens`, an integer ≥ 1; `stream_options`, if present, an object.
   - Otherwise 400.
3. **Decide** with the PDP, no transaction open (ADR-005 §5a).
   - Operation `llm.generate`, target the model name, tool `llm:<provider>`.
   - The subject is `EACP-Subject`, else `agent:<id>`.
   - The payload is `{model, stream, max_output_tokens, request_bytes}`, never content.
   - `allow` and `warn` proceed.
   - `deny` denies with its first reason when that is a code (`^[a-z][a-z0-9_]{0,63}$`), else `policy_denied`.
   - `escalate` is `approval_unsupported`. A `transform`, or an allow whose enforced payload differs from the input, is `transform_unsupported`: the gateway never rewrites a request.
   - No active policy, or an unreachable PDP, is 503 and records nothing.
4. **Admit** in one transaction as the agent (`eacp.llm_admit`), after reading the tenant kill epoch.
   - Denials, in order: `agent_version_not_active`, `subject_invalid`, `unknown_model`, `wrong_provider`, `model_not_in_allowlist`, the release denial (`eacp.release_denial`, ADR-018), the policy code, `killed` (tenant, team, agent, version, model), `max_tokens_over_cap`, `model_unpriced`, `budget_exceeded`.
   - A denial is a `DENIED` row and a provider-shaped 403 whose message is `eacp: <code>`.
   - An admitted call gets its reservation and `deadline = now() + timeout + 60 s`.
5. **Check the kill epoch again**, then take the credential (`SecretStore.Credential`, the model's timeout plus `worker.CredentialSkew`).
   - A kill that landed during admission settles `killed` with nothing sent.
   - A missing or signing credential is 503 `credential_unavailable`, settled `provider_error`, with nothing sent.
6. **Forward** to `base_url` + the route, with `model` replaced by `upstream_model`.
   - Credentials: Anthropic `x-api-key`, OpenAI Bearer.
   - For OpenAI, the gateway sets `max_completion_tokens` to the model's cap when the request states none, and forces `stream_options.include_usage = true` on a stream, keeping the agent's other keys.
   - Only `Content-Type`, `Accept`, `anthropic-version`, `anthropic-beta` and W3C trace context are forwarded.
   - No proxy, no redirects (a 3xx is relayed without `Location` as `provider_error`), the model's timeout, and **at most one provider request per admitted call**. The SDK may retry a 5xx; that is a new call.
7. **Relay** the status, `Content-Type`, request id and body.
   - A JSON body is at most 16 MiB.
   - An SSE stream is relayed and flushed event by event, at most 1 MiB per event.
   - Usage is read without keeping content: Anthropic `message_start` then the cumulative `message_delta`; OpenAI the final chunk's `usage`.
   - Every `EACP_LLM_KILL_POLL` (default 2 s, 500 ms–30 s) a watcher compares the kill epoch and re-checks the call's scopes. A killed call is cut: the upstream is cancelled and a provider-shaped error event ends the client stream.
   - A failed poll is logged and the call continues.
8. **Settle** in one transaction as `llm_gateway` (`eacp.llm_settle`), even after the client left.
   - `succeeded`: a 2xx with usage. PostgreSQL prices it from the pinned price and commits `min(cost, reservation)`.
   - `provider_error`: a non-2xx. Released, cost 0.
   - `usage_unknown`: a 2xx without readable usage, a cut stream, a transport error after sending, or a timeout. The full reservation is committed and the cost is NULL.
   - `killed`: as `usage_unknown`.
   - Known usage also writes `eacp.usage_records` (source `gateway`, `llm_call_id`), so FinOps dashboards, chargeback, soft limits, spend anomalies and release metrics see gateway spend (ADR-025 amendment).
   - A provider 401 tells the secret store, so an OAuth token is re-minted.
9. **Sweep.** Every `EACP_LLM_SWEEP_INTERVAL` (default 30 s), `eacp.llm_sweep()` as `llm_sweeper` settles overdue `ADMITTED` calls as `abandoned` and commits their reservation.
   - It uses `FOR UPDATE SKIP LOCKED`, at most 1 000 rows a run, and compare-and-set, so replicas share it without a lock (ADR-029).
   - `eacp.llm_sweep_tenants()` (SECURITY DEFINER) lists the tenants with overdue calls.

### 6. API, CLI and demo

- API:
  - `POST /v1/llm-models` (`registry_editor`) and `GET /v1/llm-models`.
  - `GET /v1/llm-calls` and `GET /v1/llm-calls/{id}` (`admin`, `operator`, `auditor`; filters agent, model, state, from, to and limit ≤ 1 000).
  - `POST /v1/killswitch` accepts scope `model`.
- CLI: `eacpctl llm-model register|list`, `eacpctl llm-calls list|show` and `eacpctl kill --scope model`.
- `fakellm` (`:8093`, compose network `llm` only) serves both shapes behind its own key, with fixed answers and exact usage. Scenarios are picked by the last user message (`error_429`, `error_500`, `no_usage`, `cut`, `slow`). It keeps a durable, content-free JSONL audit.
- `TestLLMGatewayDemo` (`DEMO=L`, tenant Hooli-AI) demonstrates metering, the allowlist, a free provider error, the budget denial, a mid-stream kill and the secret scan (docs/DEMO.md).

## Invariants

- **Invariant 3 (hard budget).** Committed LLM spend never exceeds its reservation, and a reservation never exceeds the limit. Tests: `TestReservationGuardForLLMCalls`, `TestAdmitReservesTheEstimate`, `TestTheGatewayMetersAndLimitsSpend`.
- **Invariant 11 (secrets).** No provider key or agent key appears in any row, journal, outbox or log, and no agent holds a provider key. Tests: `TestNothingSecretIsPersistedByTheGateway`, `TestOnlyTheGatewayHoldsProviderSecrets`, `TestProviderSecretsOnlyInTheGateway`.
- Nothing reaches a provider without an `ADMITTED` row, and a killed call is cut within one poll (`TestKillScopesBindLLMCalls`, `TestAKillDuringAdmissionStopsTheCallBeforeTheProvider`, `TestKillCutsAStream`, `TestAKillBeforeTheUpstreamCallCuts`).

## Out of scope

- Inspecting or transforming prompts and responses.
- Bedrock, Vertex AI and Azure routes; SigV4 providers.
- Embeddings, the Responses API, legacy completions, batch, files and count-tokens.
- Tools the gateway executes; caching; retries or fallback between models.
- Models in Governance-as-Code bundles; the operator console; the Helm chart (the gateway ships on compose; the chart gains it later).
- NATS wake-ups for kills (the gateway polls).
- De-duplicating gateway usage against an agent's own OTel spans for the same call.

## Unresolved assumptions

| Assumption | Choice | Cost if wrong |
|---|---|---|
| Providers do not bill a request answered with an error status | Released at cost 0 | Failed calls undercounted |
| Usage of a cut, killed, timed-out or abandoned call | Unknown: commit the full reservation, record no usage row | Overcharged budget, never undercharged |
| Input tokens of an admitted request | Bounded by request bytes once unbounded inputs are refused; the provider's usage for the cost | One call can exceed its reservation (and once, the limit); the overrun then blocks further spend |
| The refused shapes list | Covers the providers' fetched, stored, server-run and premium-priced features known on 2026-09-27 | A new provider feature that fetches input or prices higher is admitted until listed; the overrun rule still stops the agent after one such call |
| Cache writes without a cache-write price | Cost NULL (unpriced, ADR-025); the full reservation is committed | As above |
| OpenAI `cached_tokens` | Part of `prompt_tokens`: input = prompt − cached, cache read = cached | Mispriced cached input |
| A canary candidate's LLM calls | Need an `EACP-Subject` inside the cohort, else denied `canary_cohort`; not exercised by a gateway test | A cohort bug surfaces only in review |
| `anthropic-beta` features | Forwarded; usage still bounds the cost | A beta that bills outside `usage` |
| An agent that also emits OTel spans for gateway calls | Both count as reported spend | Reported spend over-counted, never under-counted |
| A failed kill poll during a call | Logged; the call continues until a good poll | A kill during a database outage waits for recovery |
