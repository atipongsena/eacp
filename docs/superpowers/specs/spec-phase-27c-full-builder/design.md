# Phase 27c: implementation design and ADR amendments

Date: 2026-10-01. Status: accepted and delivered; implementation and deployment verification passed.
Program: [section 8](../2026-09-29-agent-studio-program-design.md).
Contract: [SPEC.md](SPEC.md). All of Phase 27c is one owner-facing delivery.

## 1. Current behavior and required amendments

Verified at HEAD `a0dbc0b` on `phase-27a-thin-slice`:

| Existing boundary | Phase 27c proposal |
|---|---|
| Migration 00027 accepts schema 1, tools and a final response; Studio allowlists contain tools only | Schema 2 also validates model steps and a forward graph; PostgreSQL computes the exact model/tool union |
| Migration 00028 records contiguous tool action indices | V2 owns a cursor and records visited nodes, one-use LLM intents and branch choices |
| Migration 00030 binds Studio actions to a running run | V2 additionally requires the current chosen node and refuses preview actions |
| `llm_admit` admits arbitrary calls by an ACTIVE allowlisted agent, with no Studio binding | Studio callers require an intent; ordinary agents keep their existing admission path |
| ADR-031 prohibits stored prompts/responses and keeps only ledger metadata | A Studio-only exception retains a bounded, validated JSON output separately; prompts and provider envelopes remain unstored |
| Gateway kill checks do not include a Studio run | A bound Studio call honors `run`, including a run that already failed `killed` after the kill is cleared |
| Helm packages the API, worker, PDP and optional runtime, without a gateway | Optional gateway packaging and narrowly permitted runtime-to-gateway egress |

When authorized, append ADR-033 Rev 1.4 for these rules; append the Studio output exception to ADR-031, builder behavior to ADR-028 and gateway packaging to ADR-029. Do not mark these delivered before validation. Keep the department gate recorded as not met.

## 2. Definition v2

Keep schema 1 and its semantics. Schema 2 retains `kind`, string `inputs`, `steps` and `limits`. At most 65,536 definition bytes, 10 inputs and 20 nodes; identifiers and timeout (10–3,600 seconds) keep their existing limits. Every node has an immutable unique `id`.

| Kind | Fields and successor |
|---|---|
| `tool_call` | Existing tool fields plus `next`, the id of a later node |
| `llm` | `model` (registered tenant model name), `instruction`, `input` (JSON template), `max_output_tokens`, `output_schema`, `next` |
| `branch` | `condition: {left, operator, right}`, `then`, `else`; both destinations are later nodes |
| `respond` | `text`; terminal, with no successor |

The first array element is the entry. Every node must be reachable through some path from it. Every edge points to a strictly higher array index and every path ends in `respond`. Reject implicit jumps, loops, duplicate ids, unreachable nodes, unknown fields and unknown operators.

References retain `{{inputs.NAME}}` and `{{steps.ID.output.PATH}}`. A referenced output node must dominate its consumer: every path from entry to that consumer visits that node first. `branch` and `respond` have no output. Validate placeholders only in declared template fields, never model/tool names, targets, destinations, schema keys or operators. Unknown or missing values at execution fail `result_unavailable`.

Branch operands are JSON scalars or an exact placeholder. Operators: `eq`, `ne`, `lt`, `le`, `gt`, `ge`. Equality compares values of the same JSON scalar type; ordering accepts numbers only, preserving JSON number precision. Missing values, null operands or incompatible types fail `branch_invalid`; no truthiness, string coercion, expressions or scripts. A model supplies data only: the definition fixes the destinations and all external capabilities.

LLM instructions are at most 4,096 characters and render only declared placeholders. `input` is an object. `max_output_tokens` is an integer within the registered model's cap. `limits.max_output_tokens` bounds the sum of all declared LLM node caps, conservatively including both branches. `limits.timeout_seconds` remains mandatory. Registry model rows are immutable; PostgreSQL resolves each model name to its tenant-local id and computes the sorted unique `model_ids`. Tool capability derivation remains compatible with `studio_definition_capability`.

Use a closed output-schema subset: object, array, string, number, integer and boolean; `properties`, `required`, `additionalProperties: false`, `items`, finite scalar `enum`, `minLength`, `maxLength`, `minimum`, `maximum`, `minItems` and `maxItems`. Object properties are declared, arrays/strings have explicit upper bounds, nesting is at most 4 and there are at most 20 properties per object. Reject all other keys, `$ref`, `$dynamicRef`, `$id`, formats and combinators. The root is an object. Output must be one complete JSON value with unique keys and no trailing data, at most 65,536 bytes. Both providers use local post-response validation; no assumption about a provider's structured-output feature.

V2 model runs require an existing leaf budget account for the agent in each called model price's unit. Authors cannot select another agent's budget. Missing/unpriced model or missing account blocks the call with a named reason; budgets and rates are provisioned through existing admin/FinOps paths, never by Studio. Ordinary agents keep ADR-031's existing budget behavior.

For version saves, add an expected latest-version id to the author API. Lock the agent and compare it before any write. A stale save returns `studio_version_stale` (409); the form keeps its unsaved inputs and offers Save as copy, which uses the existing clone/save path and grants no capability. Initial creation has no expected id. Existing v1 runtime and stored versions require no data rewrite.

## 3. Run progress and external work

Add v2 progress alongside the legacy step record rather than reinterpreting action ids. A metadata-only node ledger records run id, definition index/id, kind, state, chosen successor, action/call/intent ids and timestamps. Content is in a separate restricted table. A run's server-owned current node moves only through fenced Studio functions.

`tool_call` keeps `Idempotency-Key: studio:<run>:<definition-index>`, its requester subject and the normal Action API. V2 insertion also proves that the node is current and its branch path was selected. The runtime can replay an unrecorded submission under the same key, wait on a recorded action and read retained results through ADR-034. Completion checks the actual action belongs to the node and has succeeded before advancing. A refusal or unknown outcome stops the run with the existing named reason; it never proceeds to another node.

`branch` is computed by the trusted interpreter under the lease, records its boolean choice and successor atomically, and has no external effect. PostgreSQL validates that the successor is one of this current branch's two declared targets; the runtime is not a new permission authority. Recorded branch choices are reused on recovery. `respond` can finish only at the current terminal node after all visited external work succeeded; unchosen branches need not execute. The answer remains requester-only for one hour.

Do not weaken v1 guards to accept arbitrary non-contiguous indices. V1 uses the existing path; V2 uses the explicit cursor functions. Kill checks precede any cursor advance or terminal success and terminal `killed` wins over a concurrent completion.

## 4. LLM intent and gateway integration

1. Under the runtime principal's live lease, `studio_llm_begin` creates or reads the single intent for the current LLM node. It records only metadata: run, node, version, requester, model, provider, cap, runtime id and lease generation. Re-fence an unconsumed intent on takeover; never re-fence a consumed one into permission to resend.
2. Render the instruction/input in memory. Send one non-streaming request to the existing OpenAI Chat Completions or Anthropic Messages route using the derived agent key, `EACP-Subject`, `EACP-Studio-Intent`, `EACP-Studio-Runtime` and `EACP-Studio-Generation` headers. These identifiers carry the lease fence and are never forwarded upstream. The same intent id survives takeover; the runtime id and generation distinguish a stale request from the current holder. No tools, functions, tool choice, external input URLs or provider-side agents are allowed. Do not retry this gateway request on an ambiguous response.
3. Before any provider call, PostgreSQL admission proves this is a Studio run's current LLM intent, the run/version/requester match, the lease is live, the model/provider/cap match and the mode permits it. Studio keys cannot call the gateway without a valid intent. The gateway evaluates the existing metadata-only PDP with no transaction open.
4. Admission locks the run, node/intent, registry rows, kill advisory lock and budget leaf in that order, then binds the intent and a unique `llm_calls` row in the same transaction. Consume denied admissions too; a duplicate intent can report metadata but never return permission to forward. Budget reservation and audit remain in this transaction, audit last. Ordinary action transactions retain their existing action-first lock order; no transaction holds a run lock while acquiring an action lock.
5. The gateway sends at most one provider request for the admitted row with no redirect or rewind, using only its provider credential. Strip Studio headers at the provider boundary. Extend `llm_call_killed` to honor the bound run and permanent `killed` containment; existing model and agent kills remain effective before and during the call.
6. For a successful non-streaming response, extract the single textual JSON answer (reject refusals, tool blocks, multiple choices and unexpected content), validate against the immutable node schema and scan for credential content using the existing redaction/credential controls. A content or schema failure still settles spend correctly and supplies a named Studio failure; it never advances.
7. Atomically settle known usage and record a valid typed output for the bound run/node. This transaction locks run/node before the LLM call, reservation and audit head; ordinary settlement keeps its established path. A run that was killed/finished or lost eligibility cannot receive content. Late provider usage still settles without advancing the run. The runtime reads through its live fenced lease and advances only after the ledger proves successful settlement and valid output.
8. Recovery reads recorded branch choices, action results and typed LLM results. If an intent was consumed, poll that original call; never resend. A settled unknown/abandoned/invalid call fails closed. An unconsumed intent can be re-fenced and admitted once. Gateway crashes after admission but before forwarding may lose a call; the sweeper settles it abandoned, and safety takes precedence over availability.

New run failure reasons: `branch_invalid`, `llm_denied`, `llm_failed`, `llm_unknown`, `llm_invalid_output`, `llm_output_unavailable`, `llm_output_contains_credential`, `budget_pending`. Preserve existing credential/version/deadline/action/killed reasons. Errors must never include provider content, rendered inputs or a key.

## 5. Typed Studio output and retention

Propose a separate tenant-scoped output table keyed by run/node and bound to one LLM call. `eacp_app` cannot select its content or write it directly. SECURITY DEFINER functions check the gateway actor for recording and the live runtime lease for reading; RLS follows the foundation convention, fixed `search_path`, PUBLIC revoked, and all new entries enter the catalogue.

Only the extracted validated JSON is stored, at most 65,536 bytes. PostgreSQL computes the digest, byte count and expiry. Output is available through the run deadline while the run is live; terminal transitions clear it immediately, and the sweeper clears overdue content. Metadata may remain as evidence. No content audit trigger, output in claim responses without the required fence, operator content view or content in messages/logs. Run-private preview samples follow the same content access and cleanup rules. Inputs and final answers retain their existing redacted handling.

This is an explicit narrow exception to ADR-031's current prohibition on stored responses. Do not reuse the action result table: its action/attempt/contract checks remain unchanged.

## 6. Builder, tests and templates

Keep the embedded ES-module form architecture and shared semantic tokens; no dependency/build step. Provide Basics, Inputs, Steps, Test and Review sections with per-kind editors, fixed successor pickers and a model picker from the existing registry model route. Invalid form structure remains visible until corrected; server refusals show their codes/detail safely. Capability review names all tools, risk/side effects, all models, token caps, timeout and budget prerequisite. The approver and key queue also show derived models.

Draft tests render locally using supplied tool/model sample outputs. They create no run or external call. For real model tests, an owner saves and obtains capability/key approval, then starts a preview of that immutable version with tool samples. The preview uses normal model governance/budgets, may execute branch/respond and never submits a tool action. PostgreSQL binds `preview` as an immutable run mode and rejects every action from it, even if runtime code is bypassed. Only the owner starts preview runs; Hub access grants ordinary runs only. Expired or missing samples stop the preview. Every server write uses `confirm.js`.

This amends program section 7's draft real-LLM test requirement: an unapproved draft does not obtain real model access. The UI explains how to request approval for a real model test.

Templates: preserve leave balance; add leave request triage (read balance, structured model advice, fixed eligible/ineligible branches) and procurement request triage (a structured eligibility decision and fixed branches to human-review responses; it deliberately creates no purchase or commitment). No model-selected action and no connector/tool declared by a template. Use an explicit configuration form to select existing tenant tools/models; templates carry no key, active allowlist or listing. Static page definitions and demo fixtures remain equal, tested in node. Hub clones/templates preserve existing two-person listing rules.

Every visible literal has its Thai catalogue entry. Add served files to `consoleFiles`; preserve ROUTES, CSP, sink bans, memory-only credentials and `?lang=`. Generate screenshots only through `scripts/screenshots.sh` and inspect them for layout and secret absence.

## 7. Deployment and demo

Runtime gains a non-secret gateway origin, used only for LLM definitions. It still refuses DB, connector/provider secrets and a missing/short master. No ambient proxy, redirect, arbitrary provider URL or user-supplied destination. Compose uses the existing `agents` network; provider connectivity stays gateway-only.

Helm adds an optional gateway Deployment/Service, off by default, with its own tokenless ServiceAccount, app DB Secret by name, provider Secret by name, PDP mTLS references, draining health lifecycle and the existing DNS options. Validate dangerous values at render time. NetworkPolicies permit runtime-to-API/gateway, gateway-to-DB/PDP/configured provider peers and required DNS; neither grants runtime access to those downstream peers. PostgreSQL/NATS remain external.

Development Kubernetes resources add Fake LLM, Studio master/runtime-key fixtures and the Studio demo's discovered tool. Bootstrap through the public API; keys are never echoed. Do not print secret values or put them in tracked resources. Preserve the demo's ignored-key-file convention; confirm cleanup targets are inside the demo workspace. Extend `scripts/k8s-e2e.sh` to run Studio/27c after chart render tests, with actual enforced NetworkPolicies.

Compose/Kubernetes scenarios: approval and key readiness; both deterministic paths; model not allowed; budget denial; invalid JSON/schema; an unchosen tool never called; preview cannot dispatch; interrupted runtime resumes without another model admission/provider call; mid-call run/model kills; permanent containment after clear; revoked key; content/key canaries absent from journal/outbox/logs/screenshots. A running fake provider never substitutes for these assertions.

## 8. Delivery and verification gates

Implementation sequence (work packages, not separately delivered phases): database definition/cursor/capabilities; Studio admission and typed output; interpreter/recovery; form/test/templates; deployment/demo; paired docs and complete validation. Write failing tests before each implementation and retain the test map for every affected invariant.

Run narrow PostgreSQL tests as `eacp_app` with `-race`, including cross-tenant, forged settings, expired/replaced lease, consumed intent, stale save, wrong node/model/subject/cap, invalid graphs and forbidden direct content access. Add meaningful concurrency tests for kill versus admission/settlement, takeover versus old request and simultaneous claims. Check migration rollback restores the latest preceding function definitions without editing earlier migrations.

Fresh final checks, sequentially where Docker/PostgreSQL isolation requires it:

1. `bash scripts/ci/lint.sh`.
2. With the isolated test PostgreSQL DSN and no full/demo stack competing: `go vet ./...` then `EACP_UI_NODE_REQUIRED=1 go test -race -timeout 45m ./...`.
3. `DEMO=S bash scripts/demo.sh` including the new full-builder scenarios.
4. Full compose Studio stack, `bash scripts/ci/examples.sh` and `EACP_COMPOSE_TEST=1 go test -count=1 ./test/security/`.
5. `EACP_HELM_REQUIRED=1 go test ./test/helm` and the full required `scripts/k8s-e2e.sh` run including Studio/27c.
6. UI node suites, generated screenshots and visual inspection; catalogue, invariant and paired-document guards. Stage new docs before guards that inspect tracked files.
7. Review final diff and Git status as the implementer; commit as the owner's configured identity without attribution; do not push or start Phase 29.

Do not run a command that could download a tool/module/image without permission. Prefer installed tools and cached dependencies/images; inspect availability first. If permission is withheld, report that gate as unverified rather than passed. Stop after three distinct unsuccessful attempts at the same problem or an out-of-scope cause.

## 9. Permission boundary

The owner selected the entire Phase 27c and Fast path drafting, then approved sections 2–7, including migrations, admission/preview authorization, development infrastructure and ADR amendments, on 2026-10-01. The owner subsequently approved the connected node UI and the required minikube/Calico downloads. Dependency upgrades, external Git writes and production deployment remain outside this scope.

## 10. Owner-requested connected node UI

The owner extended Phase 27c on 2026-10-01 to an editable node-based graph. `UX-DESIGN.md` and `EXPERIENCE.md` specify deterministic forward layers, labelled True/False edges, click/drag port connections, a selected-node inspector and read-only saved graphs. This replaces the earlier canvas exclusion only for this bounded editor. No arbitrary node position persistence, execution authority, dependency or API change is introduced. Schema-v1 remains an explicit sequential form.

The owner granted the proposed migration, authorization and development infrastructure scope on 2026-10-01. The separately required minikube/Calico downloads were also authorized on 2026-10-01.
