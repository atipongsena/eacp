# Phase 27c Full Builder Implementation Plan

> **For agentic workers:** Use `superpowers:executing-plans` and implement task by task in this session. The owner requested one complete Phase 27c delivery and no Codex reviewer; do not dispatch reviewers or start Phase 29.

**Goal:** Deliver the full governed Studio builder, deterministic branches, model steps, templates and compose/Kubernetes proof.

**Architecture:** Keep the API-only leased runtime and PostgreSQL authority. Add schema-v2 graphs and server-owned progress, bind each Studio LLM step to one admission and retain only its validated typed output under the live lease. The browser remains a confirmed-write client; provider credentials remain gateway-only.

**Tech Stack:** Existing pinned Go 1.27, PostgreSQL, pgx, jsonschema-go, embedded ES modules, node test runner, Docker Compose and Helm. No new dependency.

**Spec:** [SPEC.md](../specs/spec-phase-27c-full-builder/SPEC.md), [design.md](../specs/spec-phase-27c-full-builder/design.md) and [architecture spine](../specs/spec-phase-27c-full-builder/ARCHITECTURE-SPINE.md).

**Status:** Phase 27c complete on 2026-10-01. The owner accepted the complete scope, connected node UI and required minikube/Calico downloads. Full race, UI, compose/security, Helm and the complete live Kubernetes gate passed. Stop before Phase 29. See [verification evidence](../specs/spec-phase-27c-full-builder/VERIFICATION.md).

## Global constraints

- Preserve immutable schema-v1 definitions and their existing runtime path.
- V2: 65,536 bytes, at most 20 forward-only reachable nodes, at most 10 string inputs, timeout 10–3,600 seconds. Every path ends in a response; earlier output references must dominate their consumer.
- Model output: one unique-key JSON object, at most 65,536 bytes, closed schema subset, no I/O or references. All LLM steps are non-streaming, tool-free, at most one admitted provider request each.
- Fixed typed branch operators: `eq`, `ne`, `lt`, `le`, `gt`, `ge`; no coercion or model-chosen destination. Preserve number precision.
- PostgreSQL derives the complete model/tool capability union, owns cursor/lease/kill/budget authority and refuses preview tool actions.
- Live-lease-only typed-output reads; clear content at terminal state/deadline; metadata-only audit. Do not store prompts, provider envelopes, headers or credentials.
- TDD, raw SQL as `eacp_app`, `-race`, RLS catalogue, invariant map, EN/TH user-doc pairs, literal translations, safe DOM and confirmed writes.
- Existing `graft/` is user-owned. Commit only scoped files under the owner's identity, with no attribution. No push, download or production deployment without separate permission.

## Review focus

- An old replica sends a request after takeover: the old intent fence is refused and exactly one current admission can forward (Tasks 2–3).
- Kill races settlement: usage settles, output cannot advance the killed run, and clearing the kill never revives it (Task 3).
- A branch skips an output producer: definition validation refuses any later reference to that producer on the other path (Task 1).
- A provider answer contains fences, tool blocks, duplicate JSON keys, large integers or credential content: reject it without storing/logging content and still settle spend (Tasks 3–4).
- An author edits a saved version in two tabs or tests an unapproved definition: stale save preserves form values; draft tests use samples and preview cannot reach tools (Tasks 1 and 5).

## Task 1: Schema-v2 definitions and derived capabilities

**Files:** Create `migrations/00031_studio_builder.sql` and `internal/studio/builder_schema_test.go`; modify `internal/studio/studio.go`, `internal/api/studio.go`, `internal/storage/rls_catalog_test.go` and relevant API tests.

**Interfaces:** Keep `eacp.studio_definition_capability(text) RETURNS text[]` for tool capability. Add `eacp.studio_definition_models(text) RETURNS uuid[]`. Add `eacp.studio_save_checked(uuid,text,text,text,uuid,text,uuid) RETURNS uuid`, with final argument expected latest version; preserve the existing six-argument v1 entry point. Expose `models` beside existing tool capability in Studio version/request/key views. Saved `studio_versions.model_capability` is the derived immutable `uuid[]`.

- [x] Write `TestStudioV2DerivesExactlyToolsAndModels`, `TestStudioV2RejectsInvalidGraphs`, `TestStudioV2OutputReferencesMustDominate`, `TestStudioV2SchemaIsClosed`, `TestStudioV2CapsTokens` and `TestStudioCheckedSaveRefusesAStaleVersion`. Assert cross-tenant model/tool rejection, both branches in the union, unknown fields/version/operators, backward/unreachable edges, null/missing fields, forbidden placeholders, duplicate ids and distinct-person approval.
- [x] Run `go test -race -count=1 ./internal/studio -run 'TestStudioV2|TestStudioCheckedSave'` with the isolated PostgreSQL DSN and confirm failures identify absent rules.
- [x] Implement database validation, closed output-schema validation, dominance and exact capability guards. Add expected-version input to new-version requests and lock/check the agent before writes. Preserve v1 behavior and refuse extra model ids supplied through raw registry writes.
- [x] Run the same tests plus `go test -race -count=1 ./internal/api ./internal/storage ./internal/studio`. Check migration Down restores the latest prior definitions exactly. Catalogue every new definer function, table and policy.
- [x] Append ADR-033's proposed/delivery rules without claiming this task completes the phase; stage only scoped files and commit with the owner's configured identity.

## Task 2: Fenced v2 progress, preview and one-use intents

**Files:** Add follow-on migrations 00032–00034; preserve earlier committed migrations; create `internal/studio/progress.go`, `internal/studio/progress_schema_test.go` and `internal/api/studio_progress_test.go`; modify `internal/studio/runs.go`, `internal/api/studio.go` and the RLS catalogue.

**Interfaces:** Add `eacp.studio_node_begin(uuid,text,bigint,integer) RETURNS jsonb` and `eacp.studio_node_complete(uuid,text,bigint,integer,jsonb) RETURNS void`; completion names only action id or fixed branch choice, never arbitrary content. Add `eacp.studio_llm_begin(uuid,text,bigint,integer) RETURNS jsonb` with `{intent_id, call_id, state}`. Add `eacp.studio_preview_start(uuid,jsonb,jsonb) RETURNS uuid` for an approved owned version and tool samples. New runtime routes: `POST /v1/studio/runtime/runs/{id}/nodes/begin`, `/nodes/complete`, `/llm/begin`; owner preview route: `POST /v1/studio/versions/{id}/previews`. Bodies include existing `studio.Lease` plus `index` and kind-specific metadata. Claims add `schema_version`, `current_index`, `mode` and metadata-only `nodes`.

- [x] Write `TestStudioCursorRefusesUnchosenOrPrematureSteps`, `TestStudioIntentIsUniqueAndFenced`, `TestStudioTakeoverCannotResendAConsumedIntent`, `TestStudioPreviewCannotCreateAnAction` and `TestStudioV1StillRecordsContiguousActions`. Assert wrong requester/version, stale/live leases, terminal runs, simultaneous begin and preview owner checks.
- [x] Run those tests with `go test -race -count=1 ./internal/studio -run 'TestStudioCursor|TestStudioIntent|TestStudioTakeover|TestStudioPreview|TestStudioV1'`; observe the missing cursor/intent/preview rules.
- [x] Implement tenant-scoped metadata node/intent tables, immutable run mode, cursor updates and fenced functions. V2 action insertion checks current chosen node and ordinary mode before dispatch eligibility. Keep action transactions action-first and avoid run-to-action lock acquisition. Read v1 claims/steps through their existing path.
- [x] Run the narrow tests, API authorization tests, existing Studio run/kill tests and catalogue checks; use actual database concurrency rather than a mocked ledger.
- [x] Stage only scoped files and commit the independently testable progress rules under the owner's identity.

## Task 3: Studio gateway admission, run kills and atomic typed output

**Files:** Add follow-on migrations 00032–00034; preserve earlier committed migrations; create `internal/llm/studio_test.go`, `internal/llmgateway/studio.go`, `internal/llmgateway/studio_test.go` and `internal/studio/output_schema_test.go`; modify `internal/llm/llm.go`, `internal/llmgateway/gateway.go`, `internal/llmgateway/request.go`, `internal/llmgateway/api.go`, gateway integration tests and the RLS catalogue.

**Interfaces:** Add `llm.AdmitRequest.StudioIntentID uuid.UUID`; extend `llm.Admission` with optional `Studio *StudioBinding` containing run id, index and immutable output schema. Add `llm.Settlement.StudioOutput json.RawMessage` and `StudioFailure string`. Store settlement selects the bound Studio path from database state, not caller-supplied run ids. Add `eacp.studio_llm_settle(uuid,text,integer,bigint,bigint,bigint,bigint,boolean,jsonb,text) RETURNS jsonb`, mirroring existing settlement arguments plus typed output/failure. Add `eacp.studio_node_output(uuid,text,bigint,integer) RETURNS jsonb`; runtime output route `POST /v1/studio/runtime/runs/{id}/nodes/output` includes `Lease,index`, returns private `{output,state,failure}` with no content in operator views.

- [x] Write `TestStudioAdmissionRequiresItsCurrentIntent`, `TestStudioIntentAdmitsAtMostOnce`, `TestStudioLLMRequiresALeafBudget`, `TestStudioRunKillCutsItsModelCall`, `TestKilledStudioCallCannotPublishOutput`, `TestStudioOutputIsAtomicWithSettlement`, `TestStudioOutputIsPrivateAndPruned` and `TestOrdinaryLLMAdmissionIsUnchanged`. Test wrong header/subject/model/provider/cap, missing intent, preview tool attempts, forged transaction settings and cross-tenant/fence reads.
- [x] Run `go test -race -count=1 ./internal/llm ./internal/llmgateway ./internal/studio -run 'TestStudio|TestKilledStudio|TestOrdinaryLLM'` with PostgreSQL; observe the missing admission/run-kill/output behavior.
- [x] Enforce `EACP-Studio-Intent` at admission for every Studio key, reject tools/streaming, strip Studio headers upstream and bind one call transactionally. Add run-kill lookup and permanent containment. Extract/validate JSON from both provider shapes without storing raw response; reject credential content and malformed answers. Keep budgets and uncertain-usage settlement unchanged.
- [x] Implement atomic settlement/output recording and live-fence-only reads/pruning. Lock Studio run/node before call/reservation/audit in Studio settlement; no opposite call-to-run acquisition path. A late result can settle spend but cannot advance an ineligible run. Output cleanup emits no content audit.
- [x] Run these packages with `-race`, including real gateway/provider/database tests for takeover and settlement/kill races. Assert provider request count is one and content/key canaries are absent from storage metadata, logs, journal and outbox.
- [x] Amend ADR-031's narrowly scoped exception and run-kill semantics, update ADR-033 and the invariant map, then commit only scoped changes under the owner's identity.

## Task 4: Interpreter, precise branches and crash recovery

**Files:** Create `internal/studioruntime/graph.go`, `branch.go`, `llm.go` and associated tests; modify `runner.go`, `render.go`, `client.go`, `internal/config/config.go`, config tests and `cmd/agent-runtime/main.go`.

**Interfaces:** Add `studioruntime.Options.Gateway string` and non-secret `EACP_RUNTIME_LLM_URL`; v1 uses API only. Add `evaluateBranch(left any, operator string, right any) (bool,error)` preserving `json.Number` precision. `execution.stepsV2(context.Context) (answer,reason string,err error)` follows server progress; `execution.llmNode(context.Context,int,step,Env) (any,string,error)` uses Task 2 intents and Task 3 outputs. Keep existing `execution.steps` dispatching v1/v2 without refactoring unrelated execution.

- [x] Write `TestRuntimeV2ChoosesOnlyTheFixedBranch`, `TestBranchPreservesJSONNumberPrecision`, `TestRuntimeResumesWithTheSameLLMCall`, `TestRuntimeDoesNotRetryAnAmbiguousLLMRequest`, `TestRuntimeWaitsForDurableLLMOutput`, `TestRuntimeStopsOnInvalidModelOutput`, `TestRuntimePreviewUsesSamplesOnlyForTools` and config origin/secret refusal tests. Include both providers, shutdown before/after admission, missing branch values, expired outputs, version replacement and run kill.
- [x] Run `go test -race -count=1 ./internal/studioruntime ./internal/config -run 'TestRuntimeV2|TestBranch|TestRuntimeResumes|TestRuntimeDoesNotRetry|TestRuntimeWaits|TestRuntimeStops|TestRuntimePreview|TestRuntimeLLM'` and observe failures before implementation.
- [x] Implement v2 traversal, metadata-only progress calls, typed branch comparison and single-send gateway call. On consumption recovery poll the same call/output; never request fresh admission. Validate/render input in memory and use named failure reasons without logging content.
- [x] Run complete runtime/config tests with PostgreSQL and `-race`, plus identity and gateway integration checks; assert v1 tests remain green.
- [x] Commit the tested interpreter/config change as the owner.

## Task 5: Form builder, preview and template parity

**Files:** Modify `internal/ui/static/studio/definition.js`, `form.js`, `templates.js`, `requests.js`, `run.js`, `status.js`, `api.js`, `messages.th.js`, `internal/ui/ui_test.go` and existing Studio node tests; create focused `studio/preview.js` and `jstest/studio-builder.test.mjs`, plus fixtures under `test/demo/testdata/`.

**Interfaces:** `toDefinition(form)` emits schema v2 for new full-builder forms while `fromDefinition` preserves known v1 definitions; explicit kind editors never coerce unknown kinds into a tool. Add `previewDefinition(definition,inputs,samples)` for sample-only local tests, reusing graph/branch semantics through test fixtures. Add literal routes `studio.preview`, `studio.node.begin`, `studio.node.complete`, `studio.llm.begin`, `studio.node.output`; keep runtime-only routes out of page calls. Form sends `expected_version_id` on saves; stale refusal preserves state and exposes Save as copy through existing save/clone authority.

- [x] Write node tests for all kind round trips, fixed successor pickers, model capability union, unknown kinds, missing inputs/samples, branch precision/dominance fixtures, stale-save retention, preview write confirmation and English/Thai parity. Assert templates equal demo fixtures and no key/output enters storage or URL.
- [x] Run `node --test internal/ui/jstest/studio-builder.test.mjs` and targeted Studio tests; observe failures before adding editors/preview/templates.
- [x] Implement Basics/Inputs/Steps/Test/Review sections, model picker, sample-only draft preview and approval-required real preview. Show all models/token caps/budget prerequisites beside tools in review/approver/key queues; leave server authority unchanged. Add translations in the same change and every served file to `consoleFiles`.
- [x] Run `EACP_UI_NODE_REQUIRED=1 go test -race -count=1 ./internal/ui` and all node suites; inspect the running UI later in Task 7. No screenshots by hand.
- [x] Commit the tested page/template changes as the owner.

## Task 6: Compose/Helm packaging and full demo

**Files:** Modify `docker-compose.yml`, `deployments/helm/eacp/values.yaml`, chart helpers/validation/networkpolicy/serviceaccount templates, runtime template, `test/helm/studio_test.go`, `test/security/studio_test.go`, `test/demo/studio_test.go`, `scripts/demo.sh` and `scripts/k8s-e2e.sh`; create gateway template/Helm tests, `test/demo/studio_builder_test.go` and development-only Kubernetes fake/provider/Studio resources.

**Interfaces:** Chart `llmGateway.enabled` defaults false; gateway has a named provider Secret and explicitly configured provider peers, its own tokenless ServiceAccount and app DB/PDP references. Runtime's LLM origin points to this Service only when enabled. The full Studio demo exercises the same fixture definitions on compose and `EACP_DEMO_PLATFORM=k8s`; no protocol or permission is invented by demo bootstrapping.

- [x] Write chart tests for safe gateway rendering, named-only secrets, startup refusals, no runtime DB/provider/connector egress and DNS/PDP/provider rules. Write security assertions for runtime-to-gateway reachability and downstream refusal. Write full-builder demo assertions for both branches, invalid output, preview tool refusal, budget/model denial, one-call recovery and run/model containment.
- [x] Run `EACP_HELM_REQUIRED=1 go test -count=1 ./test/helm` using installed pinned Helm; observe failures before chart changes. Unit demo fixtures run before stack execution.
- [x] Implement optional gateway chart and narrow networking, non-secret compose runtime origin and development-only fake LLM/Studio bootstrapping. Keep existing secret custody, DNS settings, draining behavior and public API-only examples. Inspect cached tools/images first; request download permission separately if required.
- [x] Run fresh Helm tests, `DEMO=S bash scripts/demo.sh`, full compose examples/security, and the full required Kubernetes end-to-end script with the new Studio tests. Run them sequentially and stop long-lived demo resources according to their cleanup rules; do not weaken a failing isolation assertion.
- [x] Amend ADR-029 packaging and commit scoped infrastructure/demo changes as the owner after successful checks; the complete live Kubernetes gate subsequently passed.


- [x] Fresh compose demo, full stack, examples twice, full security and Helm passed; the complete live Kubernetes gate subsequently passed after authorized downloads and two test-harness corrections, without weakening isolation or one-send assertions.

## Task 7: Documents, screenshots and final validation

**Files:** Update ADR-033 Rev 1.4, ADR-031, ADR-028, ADR-029, program spec, MASTER_PLAN, AGENTS.md status, `docs/INVARIANTS.md` and affected EN/TH README/FEATURES/USER_GUIDE/ARCHITECTURE/THREAT_MODEL/DEMO/KUBERNETES documents. Generate scoped screenshots under `docs/images/` through `scripts/screenshots.sh` only.

- [x] Update documents to describe the delivered contract and limitations, including approval-required real preview, conservative loss after admission, bounded output retention and the unfulfilled department gate. Change both languages with matching structural elements; stage new docs before tracked-file guards.
- [x] With the full Studio compose stack, run the screenshot script and inspect generated images for layout and absence of keys/content canaries. Update only relevant generated images.
- [x] Stop competing stacks; bring up isolated test PostgreSQL using cached images. Run `bash scripts/ci/lint.sh`, `go vet ./...`, then `EACP_UI_NODE_REQUIRED=1 go test -race -timeout 45m ./...` with the test DSN. Report skips accurately. Rerun an environment-failed socket/database package alone only when evidence identifies that cause.
- [x] Run fresh invariants/catalogue/opensource/translation guards and any demo/security/Helm/Kubernetes check justified by changes made since its prior run. Do not repeat unaffected expensive checks merely to add output.
- [x] Inspect `git diff --check`, final diff, scoped staged files and status as the implementer. Confirm Git owner identity before committing and omit every attribution trailer. Preserve `graft/` and do not push.
- [x] Report the entire Phase 27c result, exact validation and remaining unverified gates. Mark completion only if every required gate passed. Stop; Phase 29 requires a new owner request.

## Owner-directed UI extension: connected node builder

- [x] Record BMAD UX design and experience within the accepted Phase 27c specification.
- [x] Observe red graph/inspector tests, then implement forward click/drag connections and deterministic branch-aware layout with safe bounded DOM geometry, selected-node editing, read-only saved graphs and Thai literals. Preserve v1 and server authority.
- [x] Run fresh complete UI race suite and existing node suites, including DOM, routes, translations and semantic tokens.
- [x] Rebuild the embedded UI, regenerate and inspect screenshots; update paired user guidance and the final phase evidence.
