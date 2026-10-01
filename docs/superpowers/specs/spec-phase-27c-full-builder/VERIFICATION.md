# Phase 27c verification evidence

Status: implemented; all local/compose validation passed; live Kubernetes verification pending. Do not mark Phase 27c complete while the Kubernetes gate is unverified. The department trial gate remains not met and the owner chose the fallback.

## Completed checks

All commands used installed tools, pinned cached dependencies and local development resources. Nothing was pushed or downloaded.

| Check | Result |
|---|---|
| Tasks 1–4 raw PostgreSQL/API/runtime/gateway tests with `-race` | Passed before each corresponding implementation commit; red-first evidence recorded in `.memlog.md` |
| `EACP_UI_NODE_REQUIRED=1 go test -race -count=1 ./internal/ui` | Passed after the last UI-only ID fix (2.497s), including graph/inspector, unique generated ids after removal, exact JSON, v1 preservation, routes, sink restrictions, translations and semantic tokens |
| Real Chrome node interaction | Passed against the rebuilt final API: geometry, both branch arms, selection, retained draft edits, click connections, zoom, 760px layout and unique ids after removal |
| `EACP_HELM_REQUIRED=1 go test -count=1 ./test/helm` | Passed, 16.977s |
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

`scripts/k8s-e2e.sh` has not run. A fresh two-node minikube profile with enforced Calico policies is required; Calico is absent from the inspected cache. The owner's handoff requires asking before downloads. Permission for the required minikube/Calico files and images has been requested and is pending. Helm rendering alone does not verify live networking, disruption, provider custody or the full Studio path in Kubernetes.

## Remaining behavior and assumptions

- Node positions are deterministic rather than freely movable or persisted. Connections and inspector changes are unsaved local form state until confirmed Save.
- Draft tests use samples only. Real preview requires the owner's approved immutable version, approved key, allowlist, prices and leaf budget, and never dispatches a tool action.
- Procurement triage returns human-review instructions, creating no purchase. Templates grant no capability or credential.
- An admitted model request is not retried after an ambiguous response. Recovery reads the same durable call or fails closed.
- Phase 29, production deployment and external Git writes remain outside this task.
