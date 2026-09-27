# Phase 25b — The LLM Gateway Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Agents call Anthropic Messages and OpenAI Chat Completions through an EACP `llm-gateway` that decides every call in PostgreSQL (allowlisted model, kill, hard budget), forwards it with a provider key only the gateway holds, and settles the actual cost from the rate card.

**Architecture:** A new service (`cmd/llm-gateway`, `internal/llmgateway`) over the shared core. Migration 00024 adds registry models, `model_ids` on allowlists, the `model` kill scope, the `eacp.llm_calls` ledger with `eacp.llm_admit` / `llm_settle` / `llm_sweep`, and budget reservations for LLM calls. `internal/llm` is the Go store for those functions; the gateway depends on it through a small interface, so its HTTP core is unit-tested with fakes.

**Tech Stack:** Go stdlib (net/http, encoding/json, bufio SSE), PostgreSQL (goose 00024), `governance.EvaluateChecked`, `worker.SecretStore`; `github.com/anthropics/anthropic-sdk-go` v1.75.0 and `github.com/openai/openai-go/v3` v3.66.0 as **test-only** interop dependencies.

**Spec:** `docs/superpowers/specs/2026-09-27-llm-gateway-design.md` (ADR-031 records it).

## Global Constraints

- Routes: `POST /v1/messages` (Anthropic; key in `x-api-key` or `Authorization: Bearer`) and `POST /v1/chat/completions` (OpenAI; `Authorization: Bearer`); anything else 404. Default address `:8083`.
- Request body ≤ `EACP_LLM_MAX_REQUEST_BYTES` (default 4 MiB, max 32 MiB); non-streamed response ≤ 16 MiB; one SSE event ≤ 1 MiB.
- `EACP_LLM_SECRETS_FILE` only in `llm-gateway` (`config.Options.AllowProviderSecrets`); every other service refuses it; the gateway refuses `EACP_CONNECTOR_SECRETS_FILE`; a signing (`aws`) credential is `credential_unavailable`.
- Upstream headers: Anthropic `x-api-key` + the agent's `anthropic-version` / `anthropic-beta`; OpenAI `Authorization: Bearer`; plus `Content-Type`, `Accept`, `traceparent`/`tracestate` only. No redirects, no proxy, the model's `timeout_ms`.
- Body rewrite: `model` → `upstream_model`; OpenAI without a cap gets `max_completion_tokens` = model cap; an OpenAI stream gets `stream_options.include_usage = true`. Nothing else changes.
- Denial codes, in admit order: `agent_version_not_active`, `subject_invalid`, `unknown_model`, `wrong_provider`, `model_not_in_allowlist`, release denial (`release_not_in_canary`, `canary_cohort`), the PDP's (`policy_denied` or its reason, `approval_unsupported`, `transform_unsupported`), `killed`, `max_tokens_over_cap`, `model_unpriced`, `budget_exceeded`. Denials are HTTP 403 with a provider-shaped error; PDP or PostgreSQL unavailable is 503; bad input 400; bad key 401.
- Outcomes: `succeeded`, `provider_error`, `usage_unknown`, `killed`, `abandoned`. Reservation estimate = `request_bytes × max(input, cached input, cache write) / 10⁶ + max_output_tokens × output / 10⁶`; commit `min(cost, reservation)` on success, 0 on `provider_error`, the full reservation otherwise.
- PDP binding: operation `llm.generate`, target and resource the model name, tool `llm:<provider>`, schema version `1`, side-effect class `LLM_GENERATION`, subject `EACP-Subject` or `agent:<agent id>`, payload `{"model","stream","max_output_tokens","request_bytes"}`.
- Never store, journal or log a prompt, response, provider key or agent key. PostgreSQL computes every cost.
- Commit as the user only; no Co-Authored-By trailer. Tests first; `-race`; PostgreSQL tests with `EACP_TEST_ADMIN_DSN`.

## Review Focus

1. **A client that disconnects mid-stream** — the gateway cancels upstream and settles `usage_unknown` at the full reservation; Task 5 tests it.
2. **A provider stream without usage** (OpenAI server ignoring `include_usage`, an Anthropic stream missing `message_delta`) — `usage_unknown`, never cost 0; Task 5 tests both shapes.
3. **Two replicas sweeping the same overdue call** — settled once (`abandoned`), counted once; Task 3 tests it.
4. **An agent sending its own `stream_options`, `max_tokens` and `max_completion_tokens` together, `n: 2`, or duplicate JSON keys** — 400 `invalid_request`, nothing admitted; Task 5 tests each.
5. **A kill set after admission but before the upstream request** — the call is cut before or at the first poll and settled `killed`; Task 8 tests it.

---

### Task 1: Migration 00024 (registry part) — models, model allowlists, the `model` kill scope

**Files:** Create `migrations/00024_llm_gateway.sql`, `internal/registry/llm_schema_test.go`; modify `internal/registry/registrytest/registrytest.go` (`LLMModel` helper), `internal/storage/rls_catalog_test.go`.

**Interfaces — Produces:** `eacp.llm_models`; `eacp.agent_allowlists.model_ids`; kill scope `model`; `eacp.model_prices.cache_write_per_mtok`; `func (f *Fixture) LLMModel(t *testing.T, name, provider, baseURL, upstream string) uuid.UUID` (secret_ref `llm`, cap 4096, timeout 600000).

- [ ] **Step 1: Write failing raw-SQL tests** (as `eacp_app`, like `a2a_schema_test.go`): `TestLLMModelsAreDeclaredByEditorsAndImmutable` (erin inserts; alice/otto refused 42501; UPDATE/DELETE refused; name pattern, provider enum, base_url with credentials/query refused 23514; audit event `llm_model.created`); `TestAllowlistModelIDs` (sorted/de-duplicated; another tenant's model or a random uuid → 23503; default `{}`); `TestKillScopeModel` (otto kills a tenant model → epoch bumps; unknown target → 23503); `TestModelPricesCacheWrite` (nullable, ≥ 0).
- [ ] **Step 2:** `go test -race -run 'LLMModel|AllowlistModel|KillScopeModel|CacheWrite' ./internal/registry` → FAIL (relation does not exist).
- [ ] **Step 3: Write** the Up/Down: table + RLS per 00001 + guard trigger + audit; redefine `eacp.agent_allowlists_guard` (adds the `model_ids` rules, keeps every tool rule), `kill_states` scope CHECK and `eacp.set_kill` (adds `WHEN 'model'`); Down restores the prior bodies verbatim. Add the table and functions to `rls_catalog_test.go`.
- [ ] **Step 4:** the Step 2 command plus `./internal/storage -run RLS` → PASS.
- [ ] **Step 5: Commit** `feat(registry): LLM models, model allowlists and the model kill scope`.

### Task 2: Migration 00024 (ledger part) — `llm_calls`, reservations, admit/settle/sweep

**Files:** Modify `migrations/00024_llm_gateway.sql` (append); create `internal/llm/schema_test.go`; modify `internal/storage/rls_catalog_test.go`.

**Interfaces — Produces (SQL):**
- `eacp.llm_calls` (spec §3.3), states `DENIED|ADMITTED|SETTLED`.
- `eacp.llm_admit(p jsonb) RETURNS jsonb` — input keys `model_name, provider, subject, stream, request_bytes, max_output_tokens (NULL = model cap), trace_id, gateway_id, decision {id, bundle_id, version, verdict, input_digest, denial}`; runs as the agent (`storage.SetAgent`); returns `{"call_id", "denial"}` or `{"call_id", "model_id", "upstream_model", "base_url", "secret_ref", "timeout_ms", "max_output_tokens", "deadline"}`.
- `eacp.llm_settle(p_call uuid, p_outcome text, p_status int, p_input bigint, p_cache_read bigint, p_cache_write bigint, p_output bigint, p_usage_known boolean) RETURNS jsonb` — system actor `llm_gateway` only; returns `{"cost_amount","cost_unit","committed_amount"}`.
- `eacp.llm_sweep() RETURNS integer` — system actor `llm_sweeper`; `eacp.llm_sweep_tenants() RETURNS SETOF uuid` (SECURITY DEFINER, tenants with overdue `ADMITTED` calls).
- `eacp.llm_call_killed(p_call uuid) RETURNS boolean`; `eacp.kill_epoch() RETURNS bigint` if absent (tenant epoch, 0 when none).
- `eacp.budget_reservations.llm_call_id`; `usage_records.source` gains `gateway` with `llm_call_id`.

- [ ] **Step 1: Write failing tests** in `internal/llm/schema_test.go` using `registrytest` (agent, active version, allowlist with a model, a price, a USD leaf account): `TestAdmitDeniesInOrder` (one sub-test per denial code, each leaving a `DENIED` row with that code and an `llm.denied` audit event, no reservation); `TestAdmitReservesTheEstimate` (reservation = spec formula to the micro-unit; `budget_accounts.reserved` rises; `llm.admitted` audited last); `TestAdmitWithoutALeafAccountRunsUnreserved`; `TestSettleOutcomes` (succeeded commits min(cost, amount) and writes one `usage_records` row source `gateway` with PostgreSQL's cost; a cost above the reservation commits the reservation and keeps the full cost on the call; provider_error releases 0; usage_unknown/killed commit the full reservation and write no usage row; cache writes without a cache-write price → cost NULL, full commit); `TestSettleIsOnceAndOnlyByTheGateway` (agent/principal refused 42501; second settle 55000); `TestSweepSettlesOverdueCallsOnce` (two concurrent transactions → one row moved, `abandoned`); `TestLedgerNeverChanges` (UPDATE of columns outside settle, DELETE refused); `TestKillScopesBindLLMCalls` (tenant, team, agent, agent_version, model → `killed`; `llm_call_killed` true after admission when a scope is killed); `TestReservationGuardForLLMCalls` (direct insert by agent outside `llm_admit` refused; action reservations unchanged — run the existing budget tests).
- [ ] **Step 2:** `go test -race ./internal/llm` → FAIL.
- [ ] **Step 3: Write** the SQL (lock order: version/model `FOR SHARE` → `pg_advisory_xact_lock(hashtextextended('eacp.kill:'||tenant,0))` → `budget_lock_account` leaf → insert call → reservation → audit last). `llm_admit` uses `eacp.release_denial(version, subject_principal)` and resolves the subject like the actions guard (`principals.subject`, human, enabled). Down drops everything and restores `budget_reservations` and `usage_records` constraints.
- [ ] **Step 4:** `go test -race ./internal/llm ./internal/budget ./internal/finops ./internal/storage` → PASS.
- [ ] **Step 5: Commit** `feat(llm): the LLM-call ledger - admit, settle and sweep in PostgreSQL`.

### Task 3: `internal/llm` — the Go store

**Files:** Create `internal/llm/llm.go`, `internal/llm/llm_test.go`.

**Interfaces — Produces:**
```go
type Decision struct { ID, BundleID uuid.UUID; Version int; Verdict string; InputDigest [32]byte; Denial string }
type AdmitRequest struct { AgentVersionID uuid.UUID; ModelName, Provider, Subject, TraceID, GatewayID string; Stream bool; RequestBytes, MaxOutputTokens int64; Decision Decision }
type Model struct { ID uuid.UUID; UpstreamModel, BaseURL, SecretRef string; Timeout time.Duration; MaxOutputTokens int64 }
type Admission struct { CallID uuid.UUID; Denial string; Model Model; Deadline time.Time }
type Usage struct { Input, CacheRead, CacheWrite, Output int64; Known bool }
type Settlement struct { Outcome string; ProviderStatus int; Usage Usage }
type Store struct{ /* pool */ }
func New(pool *pgxpool.Pool) *Store
func (s *Store) Admit(ctx context.Context, tenant uuid.UUID, r AdmitRequest) (Admission, error)
func (s *Store) Settle(ctx context.Context, tenant, call uuid.UUID, st Settlement) error
func (s *Store) Killed(ctx context.Context, tenant, call uuid.UUID) (bool, error)
func (s *Store) KillEpoch(ctx context.Context, tenant uuid.UUID) (int64, error)
func (s *Store) SweepAll(ctx context.Context) (int, error) // llm_sweep_tenants, then llm_sweep per tenant
func (s *Store) List(ctx context.Context, tenant uuid.UUID, f Filter) ([]Call, error); func (s *Store) Get(ctx context.Context, tenant, id uuid.UUID) (Call, error)
```
Outcome constants `OutcomeSucceeded = "succeeded"` etc.

- [ ] **Step 1: Write failing tests** `TestStoreAdmitSettleRoundTrip`, `TestStoreDenialIsReturnedNotAnError`, `TestStoreSweepAll`, `TestStoreListFiltersAndTenantIsolation`.
- [ ] **Step 2:** `go test -race ./internal/llm` → FAIL (undefined).
- [ ] **Step 3: Implement** with `storage.InTenantTx` + `SetAgent` (admit), `SetSystem("llm_gateway")` (settle), `SetSystem("llm_sweeper")` (sweep).
- [ ] **Step 4:** → PASS. **Step 5: Commit** `feat(llm): the Go store for the ledger`.

### Task 4: Registry, API and CLI

**Files:** Modify `internal/registry/tx.go`, `internal/registry/agents.go` (+ `llm_models.go`), `internal/bundle/execute.go`, `internal/api/api.go` (+ `llm.go`), `cmd/eacpctl/*`; tests beside each.

**Interfaces — Produces:** `func (t Tx) ProposeAllowlist(ctx, versionID uuid.UUID, tools, models []string) (uuid.UUID, error)` (models by name; unknown → `ErrNotFound`); `Service.ProposeAllowlist(ctx, a, versionID, tools, models)`; `func (t Tx) RegisterLLMModel(ctx, m LLMModelInput) (uuid.UUID, error)`; routes `POST/GET /v1/llm-models`, `GET /v1/llm-calls`, `GET /v1/llm-calls/{id}`; allowlist JSON `"models"`; eacpctl `llm-model register|list`, `llm-calls list`, allowlist `--models`.

- [ ] **Step 1: Write failing tests**: `TestProposeAllowlistWithModels` (registry); `TestBundleKeepsModelGrants` (bundle: an agent with an active allowlist holding a model; a bundle that changes its tools creates an allowlist whose `model_ids` equal the old ones); API `TestLLMModelRoutes`, `TestLLMCallsAreReadOnlyForReaders` (buyer agent key 403, auditor 200); eacpctl `TestLLMModelCommands`.
- [ ] **Step 2:** run → FAIL. **Step 3: Implement.** **Step 4:** `go test -race ./internal/registry ./internal/bundle ./internal/api ./cmd/eacpctl ./internal/ui` → PASS.
- [ ] **Step 5: Commit** `feat(api): LLM models, model allowlists and the call ledger`.

### Task 5: `internal/llmgateway` — the HTTP core

**Files:** Create `internal/llmgateway/{gateway.go,request.go,anthropic.go,openai.go,sse.go,errors.go}` and `*_test.go`.

**Interfaces — Produces:**
```go
type Authenticator func(ctx context.Context, key string) (identity.Caller, error)
type Ledger interface { Admit(ctx, tenant uuid.UUID, r llm.AdmitRequest) (llm.Admission, error); Settle(ctx, tenant, call uuid.UUID, s llm.Settlement) error; Killed(ctx, tenant, call uuid.UUID) (bool, error); KillEpoch(ctx, tenant uuid.UUID) (int64, error) }
type Policies interface { CurrentPolicy(ctx, tenant uuid.UUID) (governance.Policy, error) }
type Options struct { ID string; Auth Authenticator; Ledger Ledger; Policies Policies; PDP governance.GovernanceProvider; Secrets *worker.SecretStore; AgentRisk func(ctx, tenant, agent uuid.UUID) (string, error); MaxRequestBytes int64; KillPoll time.Duration; Log *slog.Logger; HTTP *http.Client }
func New(o Options) (*Gateway, error); func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request)
```
Usage parsing: Anthropic `message_start.message.usage` + cumulative `message_delta.usage` (`input_tokens`, `cache_read_input_tokens`, `cache_creation_input_tokens`, `output_tokens`); OpenAI `usage.prompt_tokens`, `completion_tokens`, `prompt_tokens_details.cached_tokens` (input = prompt − cached).

- [ ] **Step 1: Write failing tests** (fake Ledger/PDP/Auth, `httptest` provider): `TestRoutesAndAuth` (404, 401, principal key 401); `TestRequestValidation` (Review Focus 4 cases → 400, nothing admitted); `TestDecisionBinding` (exact binding and payload; escalate/transform/deny → 403 with codes; PDP error → 503, nothing admitted); `TestDenialIsProviderShaped` (Anthropic `{"type":"error","error":{"type":"permission_error","message":"eacp: <code>"}}`, OpenAI `{"error":{"message":"eacp: <code>","type":"permission_denied","code":"<code>"}}`); `TestForwardRewritesOnlyWhatItMust` (model, cap, include_usage; headers allowlist; key only upstream); `TestNonStreamSettlesExactUsage` (both shapes); `TestStreamRelaysAndSettles` (events arrive in order and flushed; usage from events); `TestStreamWithoutUsageIsUnknown` (Review Focus 2); `TestClientDisconnectCancelsUpstream` (Review Focus 1); `TestProviderErrorIsReleased` (429/500 → `provider_error`, body relayed); `TestKillCutsAStream` (Ledger epoch moves, `Killed` true → stream closed within 2 polls, `killed`); `TestLimits` (request > max → 413, response > 16 MiB and event > 1 MiB → `usage_unknown`); `TestNoRedirectNoProxy`; `TestSigningCredentialIsRefused`; `TestNothingSecretIsLogged` (keys and prompt text absent from logs).
- [ ] **Step 2:** `go test -race ./internal/llmgateway` → FAIL. **Step 3: Implement.** **Step 4:** → PASS.
- [ ] **Step 5: Commit** `feat(llmgateway): govern, forward, relay and settle LLM calls`.

### Task 6: Interoperability with the official SDKs

**Files:** Create `internal/llmgateway/interop_test.go`; modify `go.mod`/`go.sum` (test-only requires).

- [ ] **Step 1: Write** `TestAnthropicSDKThroughTheGateway` and `TestOpenAISDKThroughTheGateway`: each official client (`option.WithBaseURL(gateway)`, `option.WithAPIKey(<eacp agent key>)`) sends a normal and a streamed request; the fake provider asserts the rewritten request; the SDK parses the reply and stream; a denied model surfaces as the SDK's API error with status 403 and is **not retried** (fake counts one request).
- [ ] **Step 2:** run → FAIL until wired (or reveals a wire mismatch — fix the gateway, ledger a ruling). **Step 3/4:** PASS.
- [ ] **Step 5: Commit** `test(llmgateway): interoperability with the Anthropic and OpenAI Go SDKs`.

### Task 7: `fakellm` — the development provider

**Files:** Create `internal/fakellm/fakellm.go`, `fakellm_test.go`, `cmd/fakellm/main.go`, `main_test.go`.

**Interfaces — Produces:** `func New(key, dataFile string) (http.Handler, error)`; env `EACP_FAKELLM_KEY_FILE`, `EACP_FAKELLM_DATA_FILE`; `:8093`; `GET /v1/audit` (keyed). Scenarios from the last user message text: `stream` is the request's own flag; `error_429`, `error_500`, `no_usage`, `cut`, `slow`. Fixed usage: input = 10 + len(prompt bytes)/4, output = 20, cache read 0.

- [ ] **Step 1: Write failing tests** `TestFakeLLMServesBothShapes` (non-stream + stream, exact usage), `TestFakeLLMScenarios`, `TestFakeLLMNeedsItsKey` (Anthropic `x-api-key`, OpenAI Bearer), `TestFakeLLMAuditHasNoContent`, `TestFakeLLMLogSurvivesARestart`.
- [ ] **Step 2–4:** FAIL → implement → PASS. **Step 5: Commit** `feat(fakellm): a durable development LLM provider`.

### Task 8: The `llm-gateway` service

**Files:** Create `cmd/llm-gateway/main.go`, `internal/llmgateway/integration_test.go`; modify `internal/config/config.go` (+ test), `deployments/docker/Dockerfile`.

**Interfaces — Consumes:** Tasks 3, 5, 7. **Produces:** `config.Options.AllowProviderSecrets`, `Config.LLMSecretsFile`, `LLMKillPoll`, `LLMSweepInterval`, `LLMMaxRequestBytes`, `LLMID`.

- [ ] **Step 1: Write failing tests**: `TestProviderSecretsOnlyInTheGateway` (config: every other service refuses `EACP_LLM_SECRETS_FILE`; the gateway refuses `EACP_CONNECTOR_SECRETS_FILE` and requires its file); integration with PostgreSQL + `fakellm` + real gateway: `TestTheGatewayMetersAndLimitsSpend` (exact cost, reservation released/committed, `budget_exceeded` at the limit), `TestAKillBeforeTheUpstreamCallCuts` (Review Focus 5), `TestTheSweeperSettlesAnAbandonedCall`, `TestNothingSecretIsPersistedByTheGateway` (provider key and agent key in no row, journal, outbox or log; prompt text in none).
- [ ] **Step 2–4:** FAIL → implement (`main` wires service startup, secrets with the redaction set, the sweeper loop every `EACP_LLM_SWEEP_INTERVAL`, draining) → PASS with `-race` and PostgreSQL.
- [ ] **Step 5: Commit** `feat(llm-gateway): the gateway service`.

### Task 9: Compose, isolation and the demo

**Files:** Modify `docker-compose.yml` (`llm-gateway` on `agents`+`core`+`llm`; `fakellm` on `llm`; network `llm` internal; secret `fakellm_key`; volume `fakellm_data`), `deployments/docker/secrets/` (`llm-secrets.dev.json`: tenant `00000000-0000-4000-8000-0000000000ac`, secret_ref `fakellm`, host `fakellm:8093`, value `dev-only-fakellm-key`; `prepare_fakeerp_token.py` writes `fakellm-key.dev`), `.gitignore`, `scripts/demo.sh` (`L` → `TestLLMGatewayDemo`; default `ACDJL`), `test/security/*`, `test/demo/platform_test.go`; create `test/demo/llm_test.go`.

- [ ] **Step 1: Write** `TestLLMGatewayDemo` (spec §3.7 L0–L6) and security tests `TestAgentCannotReachFakeLLM`, `TestLLMNetworkCanReachFakeLLM`, `TestOnlyTheGatewayHoldsProviderSecrets`, `TestFakeLLMRejectsUnauthenticatedCalls`, `TestAgentReachesTheGateway`.
- [ ] **Step 2:** `docker compose up -d --build`; `EACP_COMPOSE_TEST=1 go test -count=1 ./test/security` PASS; `DEMO=L scripts/demo.sh` PASS.
- [ ] **Step 3: Commit** `test(demo): the LLM gateway demo on compose`.

### Task 10: Documentation, verification, review

- [ ] **Step 1:** ADR-031 (new) and `docs/adr/README.md`; ADR-001 §2 (the gateway is the second secret holder, provider keys only); ADR-016 (scope `model`); ADR-012 and ADR-025 (LLM reservations, source `gateway`); MASTER_PLAN §97 (25b delivered) and §45; AGENTS.md (status, an LLM-gateway rule, layout, commands); README; docs/DEMO.md (L); docs/INVARIANTS.md (invariants 3 and 11 gain tests); research/REFERENCES.md (Anthropic and OpenAI facts, SDK versions).
- [ ] **Step 2:** `go vet ./... && go test -race -count=1 ./...` (PostgreSQL, Helm, node); `test/invariants`.
- [ ] **Step 3:** compose security tests; `DEMO=ACDJL scripts/demo.sh`.
- [ ] **Step 4: Commit** `docs: ADR-031 - the LLM gateway; Phase 25b complete`.
- [ ] **Step 5:** final whole-branch review (one reviewer, most capable model), one fix pass, suite green.
