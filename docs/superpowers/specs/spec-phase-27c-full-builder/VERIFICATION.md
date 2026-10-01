# Phase 27c verification evidence

Status: Phase 27c complete; full race, UI, local/compose, Helm and live Kubernetes verification passed. The department trial gate remains not met and the owner chose the fallback.

## Completed checks

Local/compose commands used installed tools, pinned cached dependencies and local development resources. The owner subsequently authorized the minikube/Calico downloads needed for live Kubernetes verification. Nothing was pushed.

| Check | Result |
|---|---|
| Tasks 1–4 raw PostgreSQL/API/runtime/gateway tests with `-race` | Passed before each corresponding implementation commit; red-first evidence recorded in `.memlog.md` |
| `EACP_UI_NODE_REQUIRED=1 go test -race -count=1 ./internal/ui` | Passed after the last UI-only ID fix (2.497s), including graph/inspector, unique generated ids after removal, exact JSON, v1 preservation, routes, sink restrictions, translations and semantic tokens |
| Real Chrome node interaction | Passed against the rebuilt final API: geometry, both branch arms, selection, retained draft edits, click connections, zoom, 760px layout and unique ids after removal |
| `EACP_HELM_REQUIRED=1 go test -count=1 ./test/helm` | Passed, 16.977s |
| `KEEP=1 GOFLAGS=-race bash scripts/k8s-e2e.sh` | Passed, all ten required live scenarios, 448.205s package; Studio 128.33s |
| `DEMO=S bash scripts/demo.sh` | Passed, full Studio/27c scenario on isolated compose (128.464s package) |
| `docker compose --profile studio up -d --build --wait` | Passed |
| `bash scripts/ci/examples.sh` | Passed, all three examples twice |
| `EACP_COMPOSE_TEST=1 go test -count=1 ./test/security/` | Passed, 157.882s |
| `SCREENSHOTS=studio bash scripts/screenshots.sh` | Passed; all six generated Studio images inspected, no key shown |
| `bash scripts/ci/lint.sh` | Passed after final UI/screenshot-helper edits |
| `go vet ./...` | Passed |
| Invariant and paired-document guards | Passed; new documents staged before tracked-file guards |

## Full suite

Passed: `go vet ./...` followed by `EACP_UI_NODE_REQUIRED=1 go test -race -timeout 45m ./...`, with the isolated PostgreSQL DSN on port 55433 and `EACP_HELM_REQUIRED=1`. The development stack was stopped without removing data. The first run exposed the existing exhaustive tenant-isolation fixture missing rows in the new v2 node/results tables. The fixture now populates both through real Studio save/approval, fenced admission and typed settlement, preserving all isolation assertions. Its focused race run passed (6.290s); the complete command then passed, rerunning worker (459.865s) and the final UI (2.642s), and reusing successful unchanged-package results from the first run. No test or table was excluded.

Full-suite demo/security packages require their environment flags for live stacks; those gates were separately run and passed above. Helm was required in the full suite. No PostgreSQL integration tests were skipped for a missing DSN.

After that full pass, a focused UI regression exposed duplicate generated ids when adding after removal. The shared id generator now selects an unused default; the complete UI race suite, lint and real Chrome interaction passed again. No shared core or schema changed after the full pass. The development stack was restored and the isolated test PostgreSQL stopped without removing its data.

## Kubernetes gate

After the live gate and completion-document updates, a fresh `go vet ./...` and `EACP_UI_NODE_REQUIRED=1 EACP_HELM_REQUIRED=1 go test -race -timeout 45m ./...` command passed (exit 0), with the isolated test DSN and all competing stacks stopped. Successful unchanged-package cache entries were reused; UI (2.614s), demo package (1.645s; live scenarios separately flagged), invariants (1.525s) and opensource/translation guards (6.173s) ran again. The test PostgreSQL was stopped afterwards and the original Studio compose stack restored.

The owner authorized the required downloads on 2026-10-01. The first full `KEEP=1 bash scripts/k8s-e2e.sh` run created two Kubernetes v1.35.1 nodes with Calico v3.31.3. All existing required scenarios passed, but Studio exposed a test-harness failure: the host gateway port-forward selected a pod that the earlier disruption scenario drained. Kubernetes ends that forwarding session when its pod terminates. The model/runtime flow and preview had passed inside the cluster before the manual host request failed.

The shared forwarding helper now selects a current pod for each explicit host operation, closes it afterwards and still sends each model request once, without replay. The second full race run passed recovery and containment but exposed BusyBox nslookup returning an error for unrelated search suffixes of the short PDP service name, despite a valid service answer. The strict probe now uses fully qualified service names; every denied target must still resolve successfully before TCP refusal counts. The same probe passed independently against Calico before the final full run. Narrow demo package compile/race checks (live scenarios separately flagged), vet and lint passed after these helper changes. No production authority, schema or networking policy changed to make a check pass.

The final fresh-profile `KEEP=1 GOFLAGS=-race bash scripts/k8s-e2e.sh` passed all ten required scenarios in one command (exit 0, 448.205s): AWS, Slice A, token exchange, federated JIT, JIT, Kubernetes disruption, private_key_jwt, SPIFFE, full Studio and Vault. Studio passed in 128.33s, including both model providers and branch arms, missing-budget and malformed-output refusal, sampled-tool preview with no action, immutable-intent 403 and consumed-intent 409, one-call lease recovery, permanent in-flight run/model containment, cleared typed outputs, private-content/key canaries and a verified 380-event audit chain.

Both Kubernetes v1.35.1 nodes were Ready after disruption, and both Calico v3.31.3 pods were Running. The runtime stand-in reached API/gateway; PostgreSQL, NATS, PDP, HR and provider names resolved but their direct TCP connections were refused. All 30 disruption purchases succeeded with one recorded PO each. The owned test profile was deleted after validation; the original compose data was preserved.

## Remaining behavior and assumptions

- Node positions are deterministic rather than freely movable or persisted. Connections and inspector changes are unsaved local form state until confirmed Save.
- Draft tests use samples only. Real preview requires the owner's approved immutable version, approved key, allowlist, prices and leaf budget, and never dispatches a tool action.
- Procurement triage returns human-review instructions, creating no purchase. Templates grant no capability or credential.
- An admitted model request is not retried after an ambiguous response. Recovery reads the same durable call or fails closed.
- Phase 29, production deployment and external Git writes remain outside this task.
