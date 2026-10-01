# Phase 29 verification

Status: complete on 2026-10-02. Implementation commit: `9927c9f`; completion evidence and the final fixture correction are committed separately under the owner identity.

## Delivered behavior

ADR-030 Rev 1.1 adds opt-in A2A 1.0 JSON-RPC discovery and `SendMessage`, `GetTask`, `CancelTask` on the existing API. An existing approved EACP agent key binds the caller. Tasks project ordinary action rows; PostgreSQL remains authoritative for capability, lifecycle, governance, approval, budget, kill, dispatch and cancellation. Retained artifacts use ADR-034's private result channel.

No schema, production dependency, task store, service, role or credential type was added. Reference-client tests use the existing pinned Go SDK v2.6.0 and an explicit indirect x/mod v0.41.0 requirement already selected and checksum-pinned by the existing graph; `go.sum` is unchanged. Generated development connector bindings remain ignored, and the tracked secret manifest is unchanged.

## Evidence

| Command | Result |
| --- | --- |
| `go test -race -timeout 8m ./internal/api` with isolated PostgreSQL | Passed, 187.801s; real engine/database and actual pinned SDK interop. |
| `go test -race -count=1 ./internal/config ./test/opensource ./test/invariants` | Passed: config 1.357s, opensource 7.624s, invariants 1.512s. |
| `EACP_HELM_REQUIRED=1 go test -race -count=1 ./test/helm` | Passed, 19.205s. |
| `DEMO=IDS GOFLAGS=-race bash scripts/demo.sh` | Passed, 148.942s: outbound A2A 14.40s, full Studio 119.23s, inbound A2A 13.60s. Isolated stack removed. |
| `bash scripts/ci/lint.sh` | Passed after implementation commit, exit 0. |
| `go vet ./...` and `EACP_UI_NODE_REQUIRED=1 EACP_HELM_REQUIRED=1 go test -race -timeout 45m ./...` | Passed, exit 0, with isolated PostgreSQL on port 55433 and every competing stack stopped. API 206.005s, config 1.291s, runtime 89.538s, UI 4.359s, Helm 28.186s, opensource 11.142s. Successful unchanged-package cache entries were reused. |
| `KEEP=1 GOFLAGS=-race bash scripts/k8s-e2e.sh` | Passed, exit 0, all 11 scenarios in 496.116s: AWS, Slice A, token exchange, federated JIT, JIT, disruption, private_key_jwt, SPIFFE, full Studio, Vault, inbound A2A. Studio 135.67s; inbound 25.58s. |
| `docker compose --profile studio up -d --build --wait` | Passed, exit 0; original development volumes preserved. API `/readyz` returned 200. |
| `EACP_COMPOSE_TEST=1 go test -race -count=1 -timeout 10m ./test/security/` | Passed, 159.407s after the generated-manifest fixture correction. |
| `bash scripts/ci/examples.sh` | Passed, exit 0; examples 01, 02 and 03 each passed twice. |

The live inbound test proves replay after API restart produces one recorded purchase order, normal two-person approval, changed-message conflict, capability denial, principal/foreign-agent refusal, a caller-only retained MCP artifact, conservative cancellation, kill containment and the existing audit/provenance evidence checks. One observed external effect in this scenario is not an exactly-once delivery claim.

The admin DSN was set throughout the full suite; PostgreSQL tests were not skipped for a missing DSN. Demo/security packages need their separate live-stack flags; the default full-suite run alone does not verify those live gates. Test PostgreSQL was stopped only after the successful command completed.

After the final security fixture correction and completion-document updates, fresh lint, `go vet ./...` and the full race command passed again (exit 0) with the isolated admin DSN, UI/Helm required and every competing stack stopped. Successful unchanged-package cache entries were reused; opensource/translation guards (7.163s), invariants (1.543s) and the default security package (1.315s, live flag disabled) ran again. The complete live security result above is the evidence for actual compose checks. Final evidence-text updates are followed by fresh documentation/invariant guards before the completion commit. The isolated test PostgreSQL is stopped and the original Studio compose stack restored with its data preserved.

The live gate used two Kubernetes v1.35.1 nodes with Calico. Both nodes were Ready and both Calico pods Running after the complete disruption run. All 30 disruption purchases had one recorded PO each. Studio verified API/gateway reachability and refused direct database, NATS, PDP, HR and provider access after resolving every target. The inbound tenant's 126-event audit chain verified; its high-value action retained the two votes, one consumed grant and the normal dispatch journal. The owned test profile was removed after validation.

## Debugging and verification boundaries

Tests failed before the API option, configuration field and chart wiring existed. Further URL tests failed on empty fragments and invalid ports before the guards were implemented. The API credential fixture was corrected to use the operator actor required by the existing guard. Kill tests assert withheld claim/dispatch and a working queued task, matching ADR-016; admission was never changed to invent a denial.

An initial full API run was interrupted by premature shutdown of its test PostgreSQL; the fresh rerun above passed. The first live inbound run used a policy rule name inconsistent with the existing evidence helper, corrected to `high-value`. The next combined demo exposed Studio's global Fake HR call-count assertion after inbound used the same server. The new inbound test runs last, preserving the strict existing assertion. The full Helm regression exposed a staging fixture retaining the development-only HTTP URL; that fixture now uses HTTPS without changing its environment assertions or HTTP-refusal checks.

The initial compose security run exposed its positive worker-binding check still reading the original manifest, while compose now mounts the ignored generated manifest with the inbound tenant bindings. The check now reads the mounted source and still requires the exact binding count. Its focused race run passed in 7.083s. Every mount, network and leakage assertion remains intact; the fresh complete live suite passed after this fixture correction in 159.407s.

## Scope and cleanup

The approved interpretation accepts one structured governed action. It excludes free-text planning, Studio workflow delegation, foreign identities, streaming, push, histories and multi-turn conversations. Routes remain disabled by default. No UI change required screenshots. External Git writes and production deployment are outside this phase; commits are local under the owner identity, without attribution trailers.
